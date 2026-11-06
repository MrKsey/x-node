package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MrKsey/x-node/internal/webui"
)

// The web interface is served by the same binary. It lives under a random path
// (/<random>/web) so a scanner does not find it, but the real protection is the
// password: the path is not a secret and is printed by the installer.
//
// Authentication is a signed cookie. The password is checked with PBKDF2, and a
// per-address guard blocks an address after too many failures. Mutating
// requests need the X-Requested-With header, which a cross-origin form cannot
// set, so a CSRF token is not needed on top of SameSite=Strict.

type webServer struct {
	live    *settings
	store   *Store
	tracker *runtimeTracker
	usage   *usageStore
	quotas  *quotaStore
	family  *ipv6Monitor
	log     *logger
	history *trafficHistory

	credsPath  string
	sessionTTL time.Duration
	credsMu    sync.RWMutex
	creds      webCredentials
	signer     *sessionSigner
	credsMod   time.Time
	credsSize  int64
	guard      *loginGuard
	assets     fs.FS
	startedAt  time.Time
	// conns is the live connection registry of the SOCKS server. It lets the
	// web interface close every connection of one client.
	conns *connectionRegistry
	// supervisor is the peer supervisor of the daemon. It is set by main and
	// used only to list the probed candidates the operator can pin by hand.
	supervisor *peerSupervisor
	// configPath is the file the service was started with. It is compared with
	// the start time to report a configuration change that needs a restart.
	configPath string
	// The update endpoint caches the GitHub answer so a dashboard reload does
	// not spend the anonymous rate limit. updateRunning covers the helper call,
	// which outlives the HTTP request that started it.
	updateMu        sync.Mutex
	updateCache     *updateInfo
	updateCheckedAt time.Time
	updateRunning   bool
	updateError     string
}

func newWebServer(cfg Config, configPath string, live *settings, store *Store, tracker *runtimeTracker,
	usage *usageStore, quotas *quotaStore, family *ipv6Monitor, log *logger) (*webServer, error) {

	server := &webServer{
		live:       live,
		store:      store,
		tracker:    tracker,
		usage:      usage,
		quotas:     quotas,
		family:     family,
		log:        log,
		history:    newTrafficHistory(usage),
		credsPath:  cfg.statePath(webCredentialsFileName),
		assets:     webui.Assets(),
		startedAt:  time.Now(),
		configPath: configPath,
	}

	creds, err := loadWebCredentials(server.credsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", server.credsPath, err)
		}
		password, err := generatePassword()
		if err != nil {
			return nil, err
		}
		creds, err = newWebCredentials("admin", password)
		if err != nil {
			return nil, err
		}
		if err := saveWebCredentials(server.credsPath, creds); err != nil {
			return nil, err
		}
		// The password is printed once, here, exactly like the installer does.
		log.warn("web interface credentials were missing; login admin / %s", password)
	}

	sessionTTL, err := cfg.webSessionTTL()
	if err != nil {
		return nil, err
	}
	server.sessionTTL = sessionTTL
	if err := server.useCredentials(creds); err != nil {
		return nil, err
	}

	lockout, err := cfg.webLockout()
	if err != nil {
		return nil, err
	}
	server.guard = newLoginGuard(cfg.Web.MaxFailedLogins, lockout)
	return server, nil
}

// useCredentials installs a credential set and the signer built from its session
// key. It also records the state of the file on disk, so a later change is
// noticed by reloadCredentials.
func (w *webServer) useCredentials(creds webCredentials) error {
	signer, err := newSessionSigner(creds, w.sessionTTL)
	if err != nil {
		return err
	}
	var modTime time.Time
	var size int64
	if info, err := os.Stat(w.credsPath); err == nil {
		modTime, size = info.ModTime(), info.Size()
	}
	w.credsMu.Lock()
	w.creds = creds
	w.signer = signer
	w.credsMod = modTime
	w.credsSize = size
	w.credsMu.Unlock()
	return nil
}

// credentials returns the credentials and the signer together, so a request
// cannot mix a password hash from one file version with a session key from
// another.
func (w *webServer) credentials() (webCredentials, *sessionSigner) {
	w.credsMu.RLock()
	defer w.credsMu.RUnlock()
	return w.creds, w.signer
}

// reloadCredentials rereads web.json when the file changed on disk. "x-node web
// password" is a separate process, so without this the new password would only
// work after a service restart.
func (w *webServer) reloadCredentials() {
	info, err := os.Stat(w.credsPath)
	if err != nil {
		return
	}
	w.credsMu.RLock()
	unchanged := info.ModTime().Equal(w.credsMod) && info.Size() == w.credsSize
	w.credsMu.RUnlock()
	if unchanged {
		return
	}
	creds, err := loadWebCredentials(w.credsPath)
	if err != nil {
		w.log.warn("web: cannot reload %s: %v", w.credsPath, err)
		return
	}
	if err := w.useCredentials(creds); err != nil {
		w.log.warn("web: %s is not usable: %v", w.credsPath, err)
		return
	}
	w.log.info("web: credentials reloaded from %s", w.credsPath)
}

// ServeHTTP routes every request below the configured prefix.
func (w *webServer) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	cfg := w.live.get()
	prefix := cfg.Web.Path
	if prefix == "" {
		prefix = defaultWebPath
	}
	setWebSecurityHeaders(rw, r)

	if r.URL.Path == prefix {
		// A trailing slash makes the relative static/ links resolve correctly.
		http.Redirect(rw, r, prefix+"/", http.StatusMovedPermanently)
		return
	}
	if !strings.HasPrefix(r.URL.Path, prefix+"/") {
		http.NotFound(rw, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix+"/")

	switch {
	case rest == "" || rest == "index.html":
		w.serveIndex(rw, r)
	case strings.HasPrefix(rest, "static/"):
		w.serveStatic(rw, r, strings.TrimPrefix(rest, "static/"))
	case strings.HasPrefix(rest, "api/"):
		w.serveAPI(rw, r, strings.TrimPrefix(rest, "api/"))
	default:
		http.NotFound(rw, r)
	}
}

func setWebSecurityHeaders(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	rw.Header().Set("X-Frame-Options", "DENY")
	rw.Header().Set("Referrer-Policy", "no-referrer")
	rw.Header().Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
			"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	rw.Header().Set("Cache-Control", "no-store")
}

func (w *webServer) serveIndex(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, err := fs.ReadFile(w.assets, "index.html")
	if err != nil {
		http.Error(rw, "index.html is missing", http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	_, _ = rw.Write(raw)
}

func (w *webServer) serveStatic(rw http.ResponseWriter, r *http.Request, name string) {
	if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		http.NotFound(rw, r)
		return
	}
	raw, err := fs.ReadFile(w.assets, name)
	if err != nil {
		http.NotFound(rw, r)
		return
	}
	switch filepath.Ext(name) {
	case ".css":
		rw.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js":
		rw.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case ".svg":
		rw.Header().Set("Content-Type", "image/svg+xml")
	default:
		rw.Header().Set("Content-Type", "application/octet-stream")
	}
	rw.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = rw.Write(raw)
}

/* ---------- API ---------- */

func (w *webServer) serveAPI(rw http.ResponseWriter, r *http.Request, route string) {
	route = strings.TrimSuffix(route, "/")

	if route == "login" {
		w.handleLogin(rw, r)
		return
	}
	if route == "session" {
		w.handleSession(rw, r)
		return
	}
	if !w.authenticated(r) {
		writeJSON(rw, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	if route == "logout" {
		w.handleLogout(rw, r)
		return
	}
	// Every mutating call must carry the header a cross-origin form cannot set.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if r.Header.Get("X-Requested-With") != "x-node" {
			writeJSON(rw, http.StatusForbidden, errorResponse{Error: "bad_request"})
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
			writeJSON(rw, http.StatusForbidden, errorResponse{Error: "bad_origin"})
			return
		}
	}

	switch {
	case route == "status" && r.Method == http.MethodGet:
		w.handleStatus(rw, r)
	case route == "clients" && r.Method == http.MethodGet:
		w.handleClients(rw, r)
	case route == "clients" && r.Method == http.MethodPost:
		w.handleClientAdd(rw, r)
	case strings.HasPrefix(route, "clients/") && strings.HasSuffix(route, "/token") && r.Method == http.MethodPost:
		w.handleClientToken(rw, r, strings.TrimSuffix(strings.TrimPrefix(route, "clients/"), "/token"))
	case strings.HasPrefix(route, "clients/") && strings.HasSuffix(route, "/disconnect") && r.Method == http.MethodPost:
		w.handleClientDisconnect(rw, r, strings.TrimSuffix(strings.TrimPrefix(route, "clients/"), "/disconnect"))
	case strings.HasPrefix(route, "clients/") && strings.HasSuffix(route, "/suspend") && r.Method == http.MethodPost:
		w.handleClientSuspend(rw, r, strings.TrimSuffix(strings.TrimPrefix(route, "clients/"), "/suspend"))
	case strings.HasPrefix(route, "clients/") && r.Method == http.MethodPut:
		w.handleClientUpdate(rw, r, strings.TrimPrefix(route, "clients/"))
	case strings.HasPrefix(route, "clients/") && r.Method == http.MethodDelete:
		w.handleClientRemove(rw, r, strings.TrimPrefix(route, "clients/"))
	case route == "invites" && r.Method == http.MethodGet:
		w.handleInvites(rw, r)
	case route == "invites" && r.Method == http.MethodPost:
		w.handleInviteCreate(rw, r)
	case strings.HasPrefix(route, "invites/") && r.Method == http.MethodDelete:
		w.handleInviteDelete(rw, r, strings.TrimPrefix(route, "invites/"))
	case route == "quotas" && r.Method == http.MethodGet:
		w.handleQuotas(rw, r)
	case strings.HasPrefix(route, "quotas/") && r.Method == http.MethodPut:
		w.handleQuotaSet(rw, r, strings.TrimPrefix(route, "quotas/"))
	case strings.HasPrefix(route, "quotas/") && r.Method == http.MethodDelete:
		w.handleQuotaReset(rw, r, strings.TrimPrefix(route, "quotas/"))
	case route == "settings" && r.Method == http.MethodGet:
		w.handleSettingsGet(rw, r)
	case route == "settings" && r.Method == http.MethodPut:
		w.handleSettingsPut(rw, r)
	case route == "password" && r.Method == http.MethodPost:
		w.handlePassword(rw, r)
	case route == "restart" && r.Method == http.MethodPost:
		w.handleRestart(rw, r)
	case route == "update" && r.Method == http.MethodGet:
		w.handleUpdateGet(rw, r)
	case route == "update" && r.Method == http.MethodPost:
		w.handleUpdatePost(rw, r)
	case route == "config" && r.Method == http.MethodGet:
		w.handleConfigGet(rw, r)
	case route == "config" && r.Method == http.MethodPut:
		w.handleConfigPut(rw, r)
	case route == "peers/manual" && r.Method == http.MethodPost:
		w.handleManualPeers(rw, r)
	case route == "peers" && r.Method == http.MethodGet:
		w.handlePeers(rw, r)
	case route == "traffic" && r.Method == http.MethodGet:
		w.handleTraffic(rw, r)
	case route == "peers" && r.Method == http.MethodPost:
		w.handlePeersUpdate(rw, r)
	case route == "logs" && r.Method == http.MethodGet:
		w.handleLogs(rw, r)
	default:
		writeJSON(rw, http.StatusNotFound, errorResponse{Error: "not_found"})
	}
}

func sameOrigin(origin, host string) bool {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(origin, "https://"), "/")
	trimmed = strings.TrimSuffix(strings.TrimPrefix(trimmed, "http://"), "/")
	return strings.EqualFold(trimmed, host)
}

/* ---------- authentication ---------- */

func (w *webServer) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(webCookieName)
	if err != nil {
		return false
	}
	creds, signer := w.credentials()
	session, ok := signer.verify(cookie.Value)
	return ok && session.User == creds.Username
}

func (w *webServer) clientIP(r *http.Request) string {
	remote := r.RemoteAddr
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	cfg := w.live.get()
	trusted := false
	for _, proxy := range cfg.Web.TrustedProxies {
		if strings.TrimSpace(proxy) == host {
			trusted = true
			break
		}
	}
	if trusted {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			parts := strings.Split(forwarded, ",")
			candidate := strings.TrimSpace(parts[0])
			if net.ParseIP(candidate) != nil {
				return candidate
			}
		}
	}
	return host
}

func (w *webServer) handleSession(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(rw, http.StatusMethodNotAllowed, errorResponse{Error: "method_not_allowed"})
		return
	}
	if !w.authenticated(r) {
		writeJSON(rw, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	creds, _ := w.credentials()
	writeJSON(rw, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          creds.Username,
	})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (w *webServer) handleLogin(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(rw, http.StatusMethodNotAllowed, errorResponse{Error: "method_not_allowed"})
		return
	}
	// Pick up a password changed by "x-node web password" without a restart.
	w.reloadCredentials()
	address := w.clientIP(r)
	if until, blocked := w.guard.blockedUntil(address); blocked {
		writeJSON(rw, http.StatusTooManyRequests, errorResponse{
			Error:   "locked",
			Message: "too many failed attempts; try again after " + until.Format(time.RFC3339),
		})
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 4<<10)
	var request loginRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_request"})
		return
	}
	// A fixed delay makes a guessing loop slower without annoying a user who
	// mistyped a password once.
	time.Sleep(400 * time.Millisecond)
	creds, signer := w.credentials()
	userOK := strings.EqualFold(strings.TrimSpace(request.Username), creds.Username)
	passwordOK := verifyWebPassword(creds.PasswordHash, request.Password)
	if !userOK || !passwordOK {
		if until := w.guard.fail(address); !until.IsZero() {
			w.log.warn("web: %s is blocked until %s after repeated failures", address, until.Format(time.RFC3339))
		}
		w.log.info("web: failed login from %s", address)
		writeJSON(rw, http.StatusUnauthorized, errorResponse{Error: "bad_credentials", Message: "Wrong username or password"})
		return
	}
	w.guard.success(address)
	session := webSession{User: creds.Username, Expires: time.Now().Add(signer.ttl).Unix()}
	value, err := signer.sign(session)
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "session_failed"})
		return
	}
	cfg := w.live.get()
	http.SetCookie(rw, &http.Cookie{
		Name:     webCookieName,
		Value:    value,
		Path:     cfg.Web.Path + "/",
		MaxAge:   int(signer.ttl.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
	w.log.info("web: %s signed in", address)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "user": creds.Username})
}

func (w *webServer) handleLogout(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(rw, http.StatusMethodNotAllowed, errorResponse{Error: "method_not_allowed"})
		return
	}
	cfg := w.live.get()
	http.SetCookie(rw, &http.Cookie{
		Name:     webCookieName,
		Value:    "",
		Path:     cfg.Web.Path + "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

/* ---------- status ---------- */

type webStatus struct {
	Name              string        `json:"name"`
	Version           string        `json:"version"`
	Address           string        `json:"address"`
	UptimeSeconds     int64         `json:"uptimeSeconds"`
	SocksPort         int           `json:"socksPort"`
	APIPort           int           `json:"apiPort"`
	APIAuth           string        `json:"apiAuth"`
	AllowAll          bool          `json:"allowAll"`
	Registration      bool          `json:"registration"`
	UDPEnabled        bool          `json:"udpEnabled"`
	MaxUDPAssoc       int           `json:"maxUdpAssociations"`
	IPv6Mode          string        `json:"ipv6Mode"`
	IPv6Available     bool          `json:"ipv6Available"`
	IPv6Detail        string        `json:"ipv6Detail"`
	DomainResolution  bool          `json:"domainResolution"`
	WebPath           string        `json:"webPath"`
	WebURLs           []string      `json:"webURLs"`
	WebLogin          string        `json:"webLogin"`
	PendingRestart    bool          `json:"pendingRestart"`
	RestartSupported  bool          `json:"restartSupported"`
	Clients           int           `json:"clients"`
	MaxClients        int           `json:"maxClients"`
	MaxConnsPerClient int           `json:"maxConnsPerClient"`
	MaxConnsTotal     int           `json:"maxConnsTotal"`
	ActiveConnections int           `json:"activeConnections"`
	ActiveUDP         int           `json:"activeUdp"`
	TodayRx           uint64        `json:"todayRx"`
	TodayTx           uint64        `json:"todayTx"`
	MonthRx           uint64        `json:"monthRx"`
	MonthTx           uint64        `json:"monthTx"`
	TotalRx           uint64        `json:"totalRx"`
	TotalTx           uint64        `json:"totalTx"`
	PeersUp           int           `json:"peersUp"`
	PeersManaged      int           `json:"peersManaged"`
	PeersManual       int           `json:"peersManual"`
	PeersInbound      int           `json:"peersInbound"`
	PeerURI           string        `json:"peerURI"`
	Quotas            QuotaDefaults `json:"quotas"`
	UsageEnabled      bool          `json:"usageEnabled"`
	UsageTimezone     string        `json:"usageTimezone"`
	LogLevel          string        `json:"logLevel"`
	StateDir          string        `json:"stateDir"`
	ConfigPath        string        `json:"configPath"`
}

func (w *webServer) handleStatus(rw http.ResponseWriter, r *http.Request) {
	cfg := w.live.get()
	status := webStatus{
		Name:              cfg.Name,
		Version:           daemonVersion,
		Address:           cfg.ListenAddress,
		UptimeSeconds:     int64(time.Since(w.startedAt).Seconds()),
		SocksPort:         cfg.SocksPort,
		APIPort:           cfg.apiPort(),
		APIAuth:           cfg.apiAuthMode(),
		AllowAll:          cfg.AllowAll,
		Registration:      cfg.Registration,
		UDPEnabled:        cfg.UDPEnabled,
		MaxUDPAssoc:       cfg.MaxUDPAssociations,
		IPv6Mode:          cfg.ipv6Mode(),
		DomainResolution:  cfg.DomainResolution,
		WebPath:           cfg.Web.Path,
		WebURLs:           webURLs(cfg),
		WebLogin:          currentWebUser(w.credsPath),
		PendingRestart:    pendingSettings(cfg, w.configPath),
		RestartSupported:  requestHelperInstalled(),
		Clients:           len(w.store.List()),
		MaxClients:        cfg.MaxClients,
		MaxConnsPerClient: cfg.MaxConnsPerClient,
		MaxConnsTotal:     cfg.MaxConnsTotal,
		PeerURI:           cfg.PeerURI,
		Quotas:            cfg.Quotas,
		UsageEnabled:      cfg.Usage.Enabled,
		UsageTimezone:     cfg.Usage.Timezone,
		LogLevel:          cfg.LogLevel,
		StateDir:          cfg.StateDir,
		ConfigPath:        w.configPath,
	}
	if w.family != nil {
		status.IPv6Available, status.IPv6Detail = w.family.status()
	}
	if doc, err := readRuntime(cfg.statePath(runtimeFileName)); err == nil && runtimeFresh(doc, time.Now()) {
		status.ActiveConnections = doc.TotalConnections
		status.ActiveUDP = doc.TotalUDPAssociations
	}
	for _, row := range w.usage.rows() {
		status.TodayRx += row.DayRx
		status.TodayTx += row.DayTx
		status.MonthRx += row.MonthRx
		status.MonthTx += row.MonthTx
		status.TotalRx += row.Rx
		status.TotalTx += row.Tx
	}
	managed, manual, up, inbound := peerSummary(cfg)
	status.PeersManaged = managed
	status.PeersManual = manual
	status.PeersUp = up
	status.PeersInbound = inbound
	writeJSON(rw, http.StatusOK, status)
}

// peerSummary reads the managed peer list and asks the admin socket which of
// them are up. A failure to reach the socket is not an error for the dashboard.
// Inbound links are clients of the exit node, not public peers, so they are
// counted separately and never mixed into the managed total. Manual peers are
// counted on their own: they are extra links the supervisor never trims.
func peerSummary(cfg Config) (managed int, manual int, up int, inbound int) {
	list := cleanPeerList(cfg.Peers)
	if state, err := readPeersState(cfg.statePath(peersStateFile)); err == nil && len(state.Managed) > 0 {
		list = cleanPeerList(state.Managed)
	}
	managed = len(list)
	manualList := cleanPeerList(cfg.ManualPeers)
	manual = len(manualList)
	wanted := make(map[string]bool, len(list)+len(manualList))
	for _, uri := range list {
		wanted[peerKeyOfURI(uri)] = true
	}
	for _, uri := range manualList {
		wanted[peerKeyOfURI(uri)] = true
	}
	live, err := newAdminSocket(cfg.AdminSocket, 2*time.Second).Peers()
	if err != nil {
		return managed, manual, 0, 0
	}
	for _, peer := range live {
		if peer.Inbound {
			inbound++
			continue
		}
		if peer.Up && wanted[peerKeyOfURI(peer.URI)] {
			up++
		}
	}
	return managed, manual, up, inbound
}

/* ---------- clients ---------- */

type webClient struct {
	Address         string     `json:"address"`
	Label           string     `json:"label"`
	Source          string     `json:"source"`
	State           string     `json:"state"`
	Suspended       bool       `json:"suspended"`
	RegisteredAt    time.Time  `json:"registeredAt"`
	LastSeenAt      time.Time  `json:"lastSeenAt"`
	Connections     int        `json:"connections"`
	UDPAssociations int        `json:"udpAssociations"`
	Refused         int        `json:"refused"`
	QuotaRefused    int        `json:"quotaRefused"`
	HasToken        bool       `json:"hasToken"`
	DeviceName      string     `json:"deviceName,omitempty"`
	AppVersion      string     `json:"appVersion,omitempty"`
	UDPDatagramsIn  uint64     `json:"udpDatagramsIn"`
	UDPDatagramsOut uint64     `json:"udpDatagramsOut"`
	UDPBytesIn      uint64     `json:"udpBytesIn"`
	UDPBytesOut     uint64     `json:"udpBytesOut"`
	Destinations    []string   `json:"destinations,omitempty"`
	DayUsed         uint64     `json:"dayUsed"`
	MonthUsed       uint64     `json:"monthUsed"`
	Quota           quotaState `json:"quota"`
}

func (w *webServer) handleClients(rw http.ResponseWriter, r *http.Request) {
	cfg := w.live.get()
	doc, _ := readRuntime(cfg.statePath(runtimeFileName))
	rows := mergeClients(w.store.List(), doc, time.Now())
	out := make([]webClient, 0, len(rows))
	for _, row := range rows {
		ip := net.ParseIP(row.Address)
		day, month := w.usage.used(ip)
		client := webClient{
			Address:         row.Address,
			Label:           row.Label,
			Source:          row.Source,
			State:           row.State,
			RegisteredAt:    row.RegisteredAt,
			LastSeenAt:      row.LastSeenAt,
			Connections:     row.Connections,
			UDPAssociations: row.UDPAssociations,
			Refused:         row.Refused,
			QuotaRefused:    row.QuotaRefused,
			UDPDatagramsIn:  row.UDPDatagramsIn,
			UDPDatagramsOut: row.UDPDatagramsOut,
			UDPBytesIn:      row.UDPBytesIn,
			UDPBytesOut:     row.UDPBytesOut,
			Destinations:    row.Destinations,
			DayUsed:         day,
			MonthUsed:       month,
		}
		if stored, ok := w.store.Get(ip); ok {
			client.HasToken = stored.TokenHash != ""
			client.DeviceName = stored.DeviceName
			client.AppVersion = stored.AppVersion
			client.Suspended = stored.Suspended
			if stored.Suspended {
				client.State = "suspended"
			}
		}
		if ip != nil {
			client.Quota = buildQuotaState(w.quotas, w.usage, ip)
		}
		out = append(out, client)
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "clients": out})
}

type clientRequest struct {
	Address string `json:"address"`
	Label   string `json:"label"`
}

func (w *webServer) handleClientAdd(rw http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(rw, r.Body, 4<<10)
	var request clientRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_request"})
		return
	}
	ip := net.ParseIP(strings.TrimSpace(request.Address))
	if ip == nil || !isYggAddress(ip) {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_address", Message: "Not a Yggdrasil address"})
		return
	}
	client, existed, err := w.store.AddClient(ip, strings.TrimSpace(request.Label))
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "store_failed", Message: err.Error()})
		return
	}
	w.log.info("web: client %s %s", client.Address, map[bool]string{true: "updated", false: "added"}[existed])
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "client": client, "existed": existed})
}

func (w *webServer) handleClientUpdate(rw http.ResponseWriter, r *http.Request, address string) {
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil || !isYggAddress(ip) {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_address"})
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 4<<10)
	var request clientRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_request"})
		return
	}
	client, err := w.store.UpdateClient(ip, request.Label)
	if err != nil {
		if errors.Is(err, errNotAllowed) {
			writeJSON(rw, http.StatusNotFound, errorResponse{Error: "not_found"})
			return
		}
		writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "store_failed", Message: err.Error()})
		return
	}
	w.log.info("web: client %s updated", ip)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "client": client})
}

func (w *webServer) handleClientRemove(rw http.ResponseWriter, r *http.Request, address string) {
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_address"})
		return
	}
	removed, err := w.store.RemoveClient(ip)
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "store_failed", Message: err.Error()})
		return
	}
	if !removed {
		writeJSON(rw, http.StatusNotFound, errorResponse{Error: "not_found"})
		return
	}
	w.log.info("web: client %s removed", ip)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

func (w *webServer) handleClientToken(rw http.ResponseWriter, r *http.Request, address string) {
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil || !isYggAddress(ip) {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_address"})
		return
	}
	token, err := w.store.IssueToken(ip)
	if err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "token_failed", Message: err.Error()})
		return
	}
	w.log.info("web: api token issued to %s", ip)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "token": token})
}

// handleClientDisconnect closes every TCP and UDP connection of one client.
// The allowlist entry and the quota stay untouched: the client may reconnect
// at once, which is what makes this different from a suspension.
func (w *webServer) handleClientDisconnect(rw http.ResponseWriter, r *http.Request, address string) {
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil || !isYggAddress(ip) {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_address"})
		return
	}
	closed := 0
	if w.conns != nil {
		closed = w.conns.closeClient(ip)
	}
	w.log.info("web: closed %d connection(s) of %s", closed, ip)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "closed": closed})
}

type suspendRequest struct {
	Suspended bool `json:"suspended"`
}

// handleClientSuspend blocks or unblocks one allowlisted client. A suspension
// is a temporary state: the entry, its traffic counters and its token survive,
// and the operator can lift it with the same call. Suspending closes the
// connections that are open at that moment.
func (w *webServer) handleClientSuspend(rw http.ResponseWriter, r *http.Request, address string) {
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil || !isYggAddress(ip) {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_address"})
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 4<<10)
	var request suspendRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_request"})
		return
	}
	client, err := w.store.SetSuspended(ip, request.Suspended)
	if err != nil {
		if errors.Is(err, errNotAllowed) {
			writeJSON(rw, http.StatusNotFound, errorResponse{Error: "not_found"})
			return
		}
		writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "store_failed", Message: err.Error()})
		return
	}
	closed := 0
	if request.Suspended && w.conns != nil {
		closed = w.conns.closeClient(ip)
	}
	state := "resumed"
	if request.Suspended {
		state = "suspended"
	}
	w.log.info("web: client %s %s (%d connection(s) closed)", ip, state, closed)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "client": client, "closed": closed})
}

/* ---------- invites ---------- */

type inviteRequest struct {
	Note string `json:"note"`
	Uses int    `json:"uses"`
	TTL  string `json:"ttl"`
}

// webInvite is one invite as the dashboard shows it. The token is stored next
// to its hash, so the link and the QR code can be shown again; the state
// directory is readable only by the service user and root.
type webInvite struct {
	Hash      string    `json:"hash"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Uses      int       `json:"uses"`
	Used      int       `json:"used"`
	Token     string    `json:"token,omitempty"`
	Link      string    `json:"link,omitempty"`
	QR        string    `json:"qr,omitempty"`
}

func (w *webServer) handleInvites(rw http.ResponseWriter, r *http.Request) {
	cfg := w.live.get()
	invites := w.store.ListInvites()
	out := make([]webInvite, 0, len(invites))
	for _, invite := range invites {
		row := webInvite{
			Hash:      invite.Hash,
			Note:      invite.Note,
			CreatedAt: invite.CreatedAt,
			ExpiresAt: invite.ExpiresAt,
			Uses:      invite.Uses,
			Used:      invite.Used,
			Token:     invite.Token,
		}
		if invite.Token != "" {
			row.Link = exitLink(cfg, invite.Token)
			if png, err := qrPNG(row.Link, 320); err == nil {
				row.QR = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
			}
		}
		out = append(out, row)
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "invites": out})
}

func (w *webServer) handleInviteCreate(rw http.ResponseWriter, r *http.Request) {
	cfg := w.live.get()
	r.Body = http.MaxBytesReader(rw, r.Body, 4<<10)
	var request inviteRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_request"})
		return
	}
	ttl, err := cfg.inviteTTL()
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "bad_config", Message: err.Error()})
		return
	}
	if strings.TrimSpace(request.TTL) != "" {
		ttl, err = time.ParseDuration(strings.TrimSpace(request.TTL))
		if err != nil || ttl <= 0 {
			writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_ttl"})
			return
		}
	}
	uses := request.Uses
	if uses <= 0 {
		uses = cfg.InviteUses
	}
	if uses > 100 {
		uses = 100
	}
	token, invite, err := w.store.NewInvite(strings.TrimSpace(request.Note), ttl, uses)
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "invite_failed", Message: err.Error()})
		return
	}
	w.log.info("web: invite created (%d use(s), expires %s)", invite.Uses, invite.ExpiresAt.Format(time.RFC3339))
	link := exitLink(cfg, token)
	qr := ""
	if png, err := qrPNG(link, 320); err == nil {
		qr = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	}
	writeJSON(rw, http.StatusOK, map[string]any{
		"ok":        true,
		"invite":    token,
		"link":      link,
		"qr":        qr,
		"expiresAt": invite.ExpiresAt,
		"uses":      invite.Uses,
		"note":      invite.Note,
	})
}

func (w *webServer) handleInviteDelete(rw http.ResponseWriter, r *http.Request, hash string) {
	removed, err := w.store.DeleteInvite(strings.TrimSpace(hash))
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "store_failed", Message: err.Error()})
		return
	}
	if !removed {
		writeJSON(rw, http.StatusNotFound, errorResponse{Error: "not_found"})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

/* ---------- quotas ---------- */

func (w *webServer) handleQuotas(rw http.ResponseWriter, r *http.Request) {
	states := quotaStates(w.quotas, w.usage, w.store.List())
	writeJSON(rw, http.StatusOK, map[string]any{
		"ok":       true,
		"defaults": w.quotas.Defaults(),
		"clients":  states,
	})
}

type quotaRequest struct {
	MonthlyBytes int64 `json:"monthlyBytes"`
	DailyBytes   int64 `json:"dailyBytes"`
	DownKbps     int   `json:"downKbps"`
	UpKbps       int   `json:"upKbps"`
}

func (w *webServer) handleQuotaSet(rw http.ResponseWriter, r *http.Request, address string) {
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil || !isYggAddress(ip) {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_address"})
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 4<<10)
	var request quotaRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_request"})
		return
	}
	quota := Quota{
		Address:      ip.String(),
		MonthlyBytes: request.MonthlyBytes,
		DailyBytes:   request.DailyBytes,
		DownKbps:     request.DownKbps,
		UpKbps:       request.UpKbps,
	}
	if err := w.quotas.Set(quota); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_quota", Message: err.Error()})
		return
	}
	w.log.info("web: quota updated for %s", ip)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

func (w *webServer) handleQuotaReset(rw http.ResponseWriter, r *http.Request, address string) {
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_address"})
		return
	}
	w.quotas.Reset(ip.String(), false)
	w.usage.reset(ip.String(), false)
	w.log.info("web: quota and traffic reset for %s", ip)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

/* ---------- settings ---------- */

type webSettingsView struct {
	Name               string        `json:"name"`
	ListenAddress      string        `json:"listenAddress"`
	SocksPort          int           `json:"socksPort"`
	APIPort            int           `json:"apiPort"`
	AllowAll           bool          `json:"allowAll"`
	Registration       bool          `json:"registration"`
	InviteTTL          string        `json:"inviteTTL"`
	InviteUses         int           `json:"inviteUses"`
	MaxClients         int           `json:"maxClients"`
	MaxConnsPerClient  int           `json:"maxConnsPerClient"`
	MaxConnsTotal      int           `json:"maxConnsTotal"`
	DialTimeout        string        `json:"dialTimeout"`
	IdleTimeout        string        `json:"idleTimeout"`
	BlockedPorts       []int         `json:"blockedPorts"`
	LogLevel           string        `json:"logLevel"`
	UDPEnabled         bool          `json:"udpEnabled"`
	UDPIdleTimeout     string        `json:"udpIdleTimeout"`
	MaxUDPAssociations int           `json:"maxUDPAssociations"`
	IPv6               string        `json:"ipv6"`
	DomainResolution   bool          `json:"domainResolution"`
	PeerCount          int           `json:"peerCount"`
	PeerAutoReplace    bool          `json:"peerAutoReplace"`
	Quotas             QuotaDefaults `json:"quotas"`
	Usage              UsageConfig   `json:"usage"`
	Web                WebConfig     `json:"web"`
	PendingRestart     bool          `json:"pendingRestart"`
}

func (w *webServer) handleSettingsGet(rw http.ResponseWriter, r *http.Request) {
	cfg := w.live.get()
	writeJSON(rw, http.StatusOK, webSettingsView{
		Name:               cfg.Name,
		ListenAddress:      cfg.ListenAddress,
		SocksPort:          cfg.SocksPort,
		APIPort:            cfg.APIPort,
		AllowAll:           cfg.AllowAll,
		Registration:       cfg.Registration,
		InviteTTL:          cfg.InviteTTL,
		InviteUses:         cfg.InviteUses,
		MaxClients:         cfg.MaxClients,
		MaxConnsPerClient:  cfg.MaxConnsPerClient,
		MaxConnsTotal:      cfg.MaxConnsTotal,
		DialTimeout:        cfg.DialTimeout,
		IdleTimeout:        cfg.IdleTimeout,
		BlockedPorts:       cfg.BlockedPorts,
		LogLevel:           cfg.LogLevel,
		UDPEnabled:         cfg.UDPEnabled,
		UDPIdleTimeout:     cfg.UDPIdleTimeout,
		MaxUDPAssociations: cfg.MaxUDPAssociations,
		IPv6:               cfg.IPv6,
		DomainResolution:   cfg.DomainResolution,
		PeerCount:          cfg.PeerCount,
		PeerAutoReplace:    cfg.PeerAutoReplace,
		Quotas:             cfg.Quotas,
		Usage:              cfg.Usage,
		Web:                cfg.Web,
		PendingRestart:     pendingSettings(cfg, w.configPath),
	})
}

// settingsPatch uses pointers so a field that is absent from the request keeps
// its current value. The web interface sends only the fields it shows.
type settingsPatch struct {
	Name               *string        `json:"name"`
	AllowAll           *bool          `json:"allowAll"`
	Registration       *bool          `json:"registration"`
	InviteTTL          *string        `json:"inviteTTL"`
	InviteUses         *int           `json:"inviteUses"`
	MaxClients         *int           `json:"maxClients"`
	MaxConnsPerClient  *int           `json:"maxConnsPerClient"`
	MaxConnsTotal      *int           `json:"maxConnsTotal"`
	DialTimeout        *string        `json:"dialTimeout"`
	IdleTimeout        *string        `json:"idleTimeout"`
	BlockedPorts       *[]int         `json:"blockedPorts"`
	LogLevel           *string        `json:"logLevel"`
	UDPEnabled         *bool          `json:"udpEnabled"`
	UDPIdleTimeout     *string        `json:"udpIdleTimeout"`
	MaxUDPAssociations *int           `json:"maxUDPAssociations"`
	IPv6               *string        `json:"ipv6"`
	DomainResolution   *bool          `json:"domainResolution"`
	PeerCount          *int           `json:"peerCount"`
	PeerAutoReplace    *bool          `json:"peerAutoReplace"`
	Quotas             *QuotaDefaults `json:"quotas"`
	Usage              *UsageConfig   `json:"usage"`
	Web                *webPatch      `json:"web"`
}

type webPatch struct {
	Path            *string `json:"path"`
	PublicURL       *string `json:"publicURL"`
	SessionTTL      *string `json:"sessionTTL"`
	MaxFailedLogins *int    `json:"maxFailedLogins"`
	Lockout         *string `json:"lockout"`
	Compact         *bool   `json:"compact"`
}

func (w *webServer) handleSettingsPut(rw http.ResponseWriter, r *http.Request) {
	before := w.live.get()
	r.Body = http.MaxBytesReader(rw, r.Body, 32<<10)
	var patch settingsPatch
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patch); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_request", Message: err.Error()})
		return
	}

	next := before
	if patch.Name != nil {
		next.Name = strings.TrimSpace(*patch.Name)
		if next.Name == "" {
			writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_name", Message: "The node name must not be empty"})
			return
		}
	}
	if patch.AllowAll != nil {
		next.AllowAll = *patch.AllowAll
	}
	if patch.Registration != nil {
		next.Registration = *patch.Registration
	}
	if patch.InviteTTL != nil {
		next.InviteTTL = strings.TrimSpace(*patch.InviteTTL)
	}
	if patch.InviteUses != nil {
		next.InviteUses = *patch.InviteUses
	}
	if patch.MaxClients != nil {
		next.MaxClients = *patch.MaxClients
	}
	if patch.MaxConnsPerClient != nil {
		next.MaxConnsPerClient = *patch.MaxConnsPerClient
	}
	if patch.MaxConnsTotal != nil {
		next.MaxConnsTotal = *patch.MaxConnsTotal
	}
	if patch.DialTimeout != nil {
		next.DialTimeout = strings.TrimSpace(*patch.DialTimeout)
	}
	if patch.IdleTimeout != nil {
		next.IdleTimeout = strings.TrimSpace(*patch.IdleTimeout)
	}
	if patch.BlockedPorts != nil {
		next.BlockedPorts = *patch.BlockedPorts
	}
	if patch.LogLevel != nil {
		next.LogLevel = strings.TrimSpace(*patch.LogLevel)
	}
	if patch.UDPEnabled != nil {
		next.UDPEnabled = *patch.UDPEnabled
	}
	if patch.UDPIdleTimeout != nil {
		next.UDPIdleTimeout = strings.TrimSpace(*patch.UDPIdleTimeout)
	}
	if patch.MaxUDPAssociations != nil {
		next.MaxUDPAssociations = *patch.MaxUDPAssociations
	}
	if patch.IPv6 != nil {
		next.IPv6 = strings.TrimSpace(*patch.IPv6)
	}
	if patch.DomainResolution != nil {
		next.DomainResolution = *patch.DomainResolution
	}
	if patch.PeerCount != nil {
		next.PeerCount = *patch.PeerCount
	}
	if patch.PeerAutoReplace != nil {
		next.PeerAutoReplace = *patch.PeerAutoReplace
	}
	if patch.Quotas != nil {
		next.Quotas = *patch.Quotas
	}
	if patch.Usage != nil {
		next.Usage = *patch.Usage
	}
	if patch.Web != nil {
		if patch.Web.Path != nil {
			next.Web.Path = normalizeWebPath(*patch.Web.Path)
		}
		if patch.Web.PublicURL != nil {
			next.Web.PublicURL = strings.TrimSpace(*patch.Web.PublicURL)
		}
		if patch.Web.SessionTTL != nil {
			next.Web.SessionTTL = strings.TrimSpace(*patch.Web.SessionTTL)
		}
		if patch.Web.MaxFailedLogins != nil {
			next.Web.MaxFailedLogins = *patch.Web.MaxFailedLogins
		}
		if patch.Web.Lockout != nil {
			next.Web.Lockout = strings.TrimSpace(*patch.Web.Lockout)
		}
		if patch.Web.Compact != nil {
			next.Web.Compact = *patch.Web.Compact
		}
	}

	next.applyDefaults()
	if err := next.validate(); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_settings", Message: err.Error()})
		return
	}
	if err := saveSettingsOverlay(next); err != nil {
		writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "save_failed", Message: err.Error()})
		return
	}
	w.live.set(next)
	w.quotas.UpdateDefaults(next.Quotas)
	w.log.setLevel(next.LogLevel)

	restartRequired := next.SocksPort != before.SocksPort ||
		next.APIPort != before.APIPort ||
		next.ListenAddress != before.ListenAddress ||
		next.Web.Port != before.Web.Port ||
		next.Web.Listen != before.Web.Listen ||
		next.Web.Address != before.Web.Address ||
		next.Web.TLS != before.Web.TLS ||
		next.Web.Enabled != before.Web.Enabled

	w.log.info("web: settings updated (restart required: %v)", restartRequired)
	writeJSON(rw, http.StatusOK, map[string]any{
		"ok":              true,
		"restartRequired": restartRequired,
		"webPath":         next.Web.Path,
	})
}

/* ---------- peers and logs ---------- */

type webPeer struct {
	URI           string  `json:"uri"`
	Up            bool    `json:"up"`
	Inbound       bool    `json:"inbound"`
	Managed       bool    `json:"managed"`
	Manual        bool    `json:"manual"`
	Address       string  `json:"address"`
	Key           string  `json:"key"`
	Port          int     `json:"port"`
	Priority      int     `json:"priority"`
	Cost          int     `json:"cost"`
	BytesRecv     uint64  `json:"bytesRecv"`
	BytesSent     uint64  `json:"bytesSent"`
	UptimeSeconds float64 `json:"uptimeSeconds"`
	LatencyMs     float64 `json:"latencyMs"`
}

func (w *webServer) handlePeers(rw http.ResponseWriter, r *http.Request) {
	if w.supervisor != nil {
		// The picker needs candidates; the supervisor does not download the
		// catalogue while the managed list is full, so ask once here.
		w.supervisor.ProbeIfEmpty()
	}
	cfg := w.live.get()
	list := cleanPeerList(cfg.Peers)
	if state, err := readPeersState(cfg.statePath(peersStateFile)); err == nil && len(state.Managed) > 0 {
		list = cleanPeerList(state.Managed)
	}
	wanted := make(map[string]bool, len(list))
	for _, uri := range list {
		wanted[peerKeyOfURI(uri)] = true
	}
	manualList := cleanPeerList(cfg.ManualPeers)
	manualKeys := peerKeySet(manualList)
	socket := newAdminSocket(cfg.AdminSocket, 3*time.Second)
	live, err := socket.Peers()
	self, selfErr := socket.Self()
	rows := make([]webPeer, 0, len(live))
	up, inbound := 0, 0
	for _, peer := range live {
		row := webPeer{
			URI:           peer.URI,
			Up:            peer.Up,
			Inbound:       peer.Inbound,
			Managed:       wanted[peerKeyOfURI(peer.URI)],
			Manual:        manualKeys[peerKeyOfURI(peer.URI)],
			Address:       peer.Address,
			Key:           peer.Key,
			Port:          peer.Port,
			Priority:      peer.Priority,
			Cost:          peer.Cost,
			BytesRecv:     peer.BytesRecv,
			BytesSent:     peer.BytesSent,
			UptimeSeconds: peer.Uptime,
		}
		if peer.Latency > 0 {
			row.LatencyMs = float64(peer.Latency) / float64(time.Millisecond)
		}
		if peer.Inbound {
			inbound++
		} else if peer.Up {
			up++
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Inbound != rows[j].Inbound {
			return !rows[i].Inbound
		}
		if rows[i].Up != rows[j].Up {
			return rows[i].Up
		}
		return rows[i].LatencyMs < rows[j].LatencyMs
	})
	writeJSON(rw, http.StatusOK, map[string]any{
		"ok":             err == nil,
		"adminReachable": err == nil,
		"peerCount":      cfg.PeerCount,
		"autoReplace":    cfg.PeerAutoReplace,
		"managed":        len(list),
		"manualPeers":    manualList,
		"up":             up,
		"inbound":        inbound,
		"peers":          rows,
		"self":           self,
		"selfOk":         selfErr == nil,
		"candidates":     w.peerCandidates(manualList),
		"probing":        w.supervisor != nil && w.supervisor.Probing(),
	})
}

// peerCandidates is the probed list the operator can pin by hand. Peers that
// are already configured (automatic or manual) are left out: adding them again
// would only duplicate a link.
func (w *webServer) peerCandidates(manual []string) []probeResult {
	if w.supervisor == nil {
		return []probeResult{}
	}
	used := peerKeySet(manual)
	out := make([]probeResult, 0, 16)
	for _, candidate := range w.supervisor.Candidates() {
		if used[peerKeyOfURI(candidate.URI)] {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

type peersPatch struct {
	Count       *int  `json:"count"`
	AutoReplace *bool `json:"autoReplace"`
	Refresh     bool  `json:"refresh"`
}

func (w *webServer) handlePeersUpdate(rw http.ResponseWriter, r *http.Request) {
	cfg := w.live.get()
	r.Body = http.MaxBytesReader(rw, r.Body, 4<<10)
	var patch peersPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_request"})
		return
	}
	next := cfg
	if patch.Count != nil {
		if *patch.Count < 1 || *patch.Count > 50 {
			writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_count", Message: "peer count is 1..50"})
			return
		}
		next.PeerCount = *patch.Count
	}
	if patch.AutoReplace != nil {
		next.PeerAutoReplace = *patch.AutoReplace
	}
	if err := saveSettingsOverlay(next); err != nil {
		writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "save_failed", Message: err.Error()})
		return
	}
	w.live.set(next)
	if patch.Refresh {
		path := cfg.statePath(peerRefreshFile)
		if err := os.WriteFile(path, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "refresh_failed", Message: err.Error()})
			return
		}
		if w.supervisor != nil {
			w.supervisor.Notify()
		}
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

func (w *webServer) handleLogs(rw http.ResponseWriter, r *http.Request) {
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= logRingSize {
			limit = parsed
		}
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "entries": w.log.recent(limit)})
}

/* ---------- listener ---------- */

func (w *webServer) listenAndServe(ctx context.Context) error {
	cfg := w.live.get()
	if !cfg.Web.Enabled {
		w.log.info("web interface is disabled")
		<-ctx.Done()
		return nil
	}
	addr := net.JoinHostPort(cfg.webBindHost(), strconv.Itoa(cfg.Web.Port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen web interface on %s: %w", addr, err)
	}

	useTLS := false
	switch cfg.webTLSMode() {
	case webTLSSelf:
		useTLS = true
	case webTLSAuto:
		certFile, keyFile := webCertPaths(cfg)
		useTLS = fileExists(certFile) && fileExists(keyFile)
	}
	if useTLS {
		cert, err := w.certificate(cfg)
		if err != nil {
			_ = listener.Close()
			return fmt.Errorf("web interface TLS: %w", err)
		}
		listener = tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		})
	}

	server := &http.Server{
		Handler:           w,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	w.log.info("web interface listening on %s (%s)", addr, scheme)
	for _, url := range webURLs(cfg) {
		w.log.info("web interface: %s", url)
	}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func webCertPaths(cfg Config) (string, string) {
	certFile := strings.TrimSpace(cfg.Web.CertFile)
	keyFile := strings.TrimSpace(cfg.Web.KeyFile)
	if certFile == "" {
		certFile = cfg.statePath("tls/web.crt")
	}
	if keyFile == "" {
		keyFile = cfg.statePath("tls/web.key")
	}
	return certFile, keyFile
}

func (w *webServer) certificate(cfg Config) (tls.Certificate, error) {
	certFile, keyFile := webCertPaths(cfg)
	if fileExists(certFile) && fileExists(keyFile) {
		return tls.LoadX509KeyPair(certFile, keyFile)
	}
	if cfg.webTLSMode() == webTLSOff {
		return tls.Certificate{}, errors.New("no certificate configured")
	}
	if err := ensureSelfSignedCertificate(certFile, keyFile, cfg.ListenAddress); err != nil {
		return tls.Certificate{}, err
	}
	w.log.info("web interface: created a self-signed certificate at %s", certFile)
	return tls.LoadX509KeyPair(certFile, keyFile)
}

// ensureSelfSignedCertificate creates a certificate for the addresses the
// interface is reachable on. It is only used when no real certificate is
// configured; the README explains how to switch to Let's Encrypt.
func ensureSelfSignedCertificate(certFile, keyFile, yggAddress string) error {
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	notBefore := time.Now().Add(-time.Hour)
	notAfter := notBefore.Add(10 * 365 * 24 * time.Hour)

	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hostname},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	if hostname != "" {
		template.DNSNames = append(template.DNSNames, hostname)
	}
	if ip := net.ParseIP(yggAddress); ip != nil {
		template.IPAddresses = append(template.IPAddresses, ip)
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return err
	}
	return nil
}

// copyLimited reads a small body, used by the handlers that need a raw payload.
func copyLimited(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}
