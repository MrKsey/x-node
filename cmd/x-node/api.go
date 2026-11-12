package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// daemonVersion is stamped by build.sh (-ldflags -X main.daemonVersion=...).
// The default is what a plain "go build" reports.
var daemonVersion = "1.7.18"

type apiServer struct {
	cfg     Config
	live    *settings
	store   *Store
	tracker *runtimeTracker
	quotas  *quotaStore
	usage   *usageStore
	family  *ipv6Monitor
	log     *logger

	// registerLimit guards /v1/register: an invite is a secret, so one source
	// may not guess many of them. tokenLimit does the same for /v1/token.
	registerLimit *rateLimiter
	tokenLimit    *rateLimiter
	// generalLimit is the per-source budget of all API requests together. A
	// connected client polls /v1/status every few seconds; the budget is high
	// enough for that and low enough to stop a flood.
	generalLimit *rateLimiter
}

// capabilities is the part of the status that is safe to publish to anyone who
// can reach the API. It tells an old client what the exit can do without
// revealing anything about its clients.
type capabilities struct {
	UDP          bool `json:"udp"`
	IPv6         bool `json:"ipv6"`
	Registration bool `json:"registration"`
	Token        bool `json:"token"`

	// DNS tells the app that this exit also runs a resolver for its clients.
	// DNSAddress is the Yggdrasil address to query; DNSPort is normally 53 and
	// is sent separately because the app stores DNS servers as bare addresses.
	DNS        bool   `json:"dns"`
	DNSAddress string `json:"dnsAddress,omitempty"`
	DNSPort    int    `json:"dnsPort,omitempty"`

	// IPv6State explains the IPv6 flag: "ok", "none" (no global address),
	// "broken" (an address the provider does not route) or "off" (disabled in
	// the settings). An old client ignores it; a new one stops sending IPv6
	// destinations to an exit that cannot reach them.
	IPv6State string `json:"ipv6State,omitempty"`
}

type registerRequest struct {
	Invite     string `json:"invite"`
	Address    string `json:"address"`
	PublicKey  string `json:"publicKey"`
	DeviceName string `json:"deviceName"`
	AppVersion string `json:"appVersion"`
}

type registerResponse struct {
	OK           bool         `json:"ok"`
	Allowlisted  bool         `json:"allowlisted"`
	Registered   bool         `json:"registered"`
	ExitName     string       `json:"exitName"`
	Address      string       `json:"address"`
	Label        string       `json:"label"`
	Version      string       `json:"version"`
	Auth         string       `json:"auth"`
	Capabilities capabilities `json:"capabilities"`

	// Token is the API credential of this client. It is shown once, here; the
	// server keeps only its hash. Empty in open mode, where no token is needed,
	// and when the client already has one: a repeat registration must not
	// replace a working credential.
	Token string `json:"token,omitempty"`
}

type statusResponse struct {
	OK            bool   `json:"ok"`
	Authenticated bool   `json:"authenticated"`
	Auth          string `json:"auth"`

	// Everything below is published only to an authenticated caller. Without
	// a token the answer is just ok/authenticated/auth: a stranger learns
	// nothing about the exit, its clients, their limits or their addresses.
	Allowlisted  *bool         `json:"allowlisted,omitempty"`
	Open         bool          `json:"open,omitempty"`
	Suspended    bool          `json:"suspended,omitempty"`
	ExitName     string        `json:"exitName,omitempty"`
	Version      string        `json:"version,omitempty"`
	Capabilities *capabilities `json:"capabilities,omitempty"`
	Address      string        `json:"address,omitempty"`
	Label        string        `json:"label,omitempty"`
	MaxConns     int           `json:"maxConnsPerClient,omitempty"`

	// Live counters of this client. They let the app show "27 of 128" and
	// "12 refused" instead of guessing why public traffic stopped going
	// through the exit.
	Connections        int `json:"connections,omitempty"`
	Refused            int `json:"refused,omitempty"`
	LimitRefused       int `json:"limitRefused"`
	QuotaRefused       int `json:"quotaRefused,omitempty"`
	SuspendedRefused   int `json:"suspendedRefused,omitempty"`
	UDPAssociations    int `json:"udpAssociations,omitempty"`
	MaxUDPAssociations int `json:"maxUdpAssociations,omitempty"`

	// Quota is the effective limit of this client and how much of it is used.
	// The app shows it, so a client knows that a quota applies before it hits
	// one, and why a new connection was refused after it did.
	Quota *statusQuota `json:"quota,omitempty"`
}

// statusQuota is the quota state of the calling client.
type statusQuota struct {
	MonthlyBytes int64  `json:"monthlyBytes"`
	DailyBytes   int64  `json:"dailyBytes"`
	DownKbps     int    `json:"downKbps"`
	UpKbps       int    `json:"upKbps"`
	DayUsed      uint64 `json:"dayUsed"`
	MonthUsed    uint64 `json:"monthUsed"`
	Exhausted    bool   `json:"exhausted"`
	Reason       string `json:"reason,omitempty"`
}

type tokenResponse struct {
	OK       bool   `json:"ok"`
	Token    string `json:"token,omitempty"`
	Open     bool   `json:"open,omitempty"`
	ExitName string `json:"exitName"`
	Version  string `json:"version"`
}

type errorResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

func newAPIServer(cfg Config, store *Store, tracker *runtimeTracker, log *logger) *apiServer {
	api := &apiServer{
		cfg:           cfg,
		store:         store,
		tracker:       tracker,
		log:           log,
		registerLimit: newRateLimiter(10, 10*time.Minute),
		tokenLimit:    newRateLimiter(5, 10*time.Minute),
	}
	if cfg.APIRateLimit > 0 {
		api.generalLimit = newRateLimiter(cfg.APIRateLimit, time.Minute)
	}
	return api
}

// config returns the configuration in effect. The web interface can switch
// open mode, registration and limits without a restart.
func (a *apiServer) config() Config {
	if a.live != nil {
		return a.live.get()
	}
	return a.cfg
}

func (a *apiServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/register", a.guard(a.handleRegister, a.registerLimit))
	mux.HandleFunc("/v1/status", a.guard(a.handleStatus, a.generalLimit))
	mux.HandleFunc("/v1/token", a.guard(a.handleToken, a.tokenLimit))
	mux.HandleFunc("/v1/health", a.guard(a.handleHealth, a.generalLimit))
	return mux
}

func (a *apiServer) listenAndServe(ctx context.Context) error {
	if a.config().apiShared() {
		// The SOCKS5 listener serves the same mux; there is nothing to bind.
		<-ctx.Done()
		return nil
	}
	addr := net.JoinHostPort(a.config().ListenAddress, strconv.Itoa(a.config().APIPort))
	listener, err := listenOnAssignedAddress(ctx, addr, a.log, "registration API")
	if err != nil {
		if ctx.Err() != nil {
			// The daemon is stopping while the address is still missing.
			return nil
		}
		return fmt.Errorf("listen API on %s: %w", addr, err)
	}
	server := &http.Server{
		Handler:           a.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	a.log.info("registration API listening on %s (auth %s)", addr, a.config().apiAuthMode())
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method_not_allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"name":    a.config().Name,
		"version": daemonVersion,
	})
}

func (a *apiServer) capabilities() capabilities {
	caps := capabilities{
		UDP:          a.config().UDPEnabled,
		Registration: a.config().Registration,
		Token:        true,
		DNS:          a.config().DNS.Enabled,
		DNSAddress:   a.config().dnsServerAddress(),
		DNSPort:      a.config().dnsServerPort(),
	}
	// IPv6 is a property of the host, not of the build: the exit may have an
	// address the provider does not route, or no address at all. Reporting a
	// flat true made every client send IPv6 destinations that could only time
	// out.
	if a.config().ipv6Mode() == ipv6Off {
		caps.IPv6State = "off"
		return caps
	}
	available, state, _, _, _ := a.family.status()
	caps.IPv6 = available
	caps.IPv6State = state
	return caps
}

// handleStatus answers with the live counters of the caller. Without a valid
// token the answer carries only ok, authenticated and auth: nothing about the
// exit, its clients or their limits. The app sees that a token is required and
// fetches one; a stranger sees nothing.
func (a *apiServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method_not_allowed"})
		return
	}
	source := remoteIP(r)
	if !isYggAddress(source) {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "not_yggdrasil"})
		return
	}
	mode := a.config().apiAuthMode()
	// "optional" and "off" keep the pre-1.3 behaviour; open mode has no tokens
	// to check in the first place.
	authenticated := mode != apiAuthRequired || a.config().AllowAll ||
		a.store.TokenValid(source, bearerToken(r))
	response := statusResponse{
		OK:            true,
		Authenticated: authenticated,
		Auth:          mode,
	}
	if !authenticated {
		writeJSON(w, http.StatusOK, response)
		return
	}
	allowlisted := a.config().AllowAll || a.store.Allowed(source)
	if allowlisted {
		a.store.Touch(source)
		if a.tracker != nil {
			a.tracker.touch(source)
		}
	}
	caps := a.capabilities()
	client, _ := a.store.Get(source)
	live := a.tracker.live(source)
	response.Allowlisted = &allowlisted
	response.Open = a.config().AllowAll
	response.ExitName = a.config().Name
	response.Version = daemonVersion
	response.Capabilities = &caps
	response.Address = source.String()
	response.Label = client.Label
	response.Suspended = client.Suspended
	response.MaxConns = a.config().MaxConnsPerClient
	response.Connections = live.Connections
	response.Refused = live.Refused
	response.QuotaRefused = live.QuotaRefused
	response.SuspendedRefused = live.SuspendedRefused
	// A suspension and an exhausted quota are refusals, but neither is a
	// connection limit. Reporting them here made the app warn about a limit
	// that never existed.
	response.LimitRefused = live.Refused - live.QuotaRefused - live.SuspendedRefused
	if response.LimitRefused < 0 {
		response.LimitRefused = 0
	}
	response.UDPAssociations = live.UDPAssociations
	response.MaxUDPAssociations = a.config().MaxUDPAssociations
	if a.quotas != nil && a.usage != nil {
		state := buildQuotaState(a.quotas, a.usage, source)
		response.Quota = &statusQuota{
			MonthlyBytes: state.MonthlyBytes,
			DailyBytes:   state.DailyBytes,
			DownKbps:     state.DownKbps,
			UpKbps:       state.UpKbps,
			DayUsed:      state.DayUsed,
			MonthUsed:    state.MonthUsed,
			Exhausted:    state.Exhausted,
			Reason:       state.Reason,
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// handleToken issues a new API token to an allowlisted caller. The Yggdrasil
// source address is the credential here: it is derived from the node key and
// cannot be spoofed, so a reinstalled app or a profile from before 1.3 can get
// back in without a new invite. The previous token stops working at once.
func (a *apiServer) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method_not_allowed"})
		return
	}
	source := remoteIP(r)
	if !isYggAddress(source) {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "not_yggdrasil"})
		return
	}
	if a.config().AllowAll {
		// Open mode needs no credential; telling the client so keeps it from
		// asking again on every status poll.
		writeJSON(w, http.StatusOK, tokenResponse{
			OK:       true,
			Open:     true,
			ExitName: a.config().Name,
			Version:  daemonVersion,
		})
		return
	}
	if !a.store.Allowed(source) {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "not_allowed"})
		return
	}
	token, err := a.store.IssueToken(source)
	if err != nil {
		a.log.error("could not issue an API token to %s: %v", source, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "token_failed"})
		return
	}
	a.log.info("api token issued to %s", source)
	writeJSON(w, http.StatusOK, tokenResponse{
		OK:       true,
		Token:    token,
		ExitName: a.config().Name,
		Version:  daemonVersion,
	})
}

func (a *apiServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method_not_allowed"})
		return
	}
	source := remoteIP(r)
	if !isYggAddress(source) {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "not_yggdrasil"})
		return
	}
	if !a.registerLimit.allow(source.String()) {
		writeJSON(w, http.StatusTooManyRequests, errorResponse{Error: "rate_limited"})
		return
	}
	if !a.config().Registration {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "registration_disabled"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req registerRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "bad_request", Message: err.Error()})
		return
	}

	// The source address is authenticated by Yggdrasil. A body that claims a
	// different address is either a broken client or an attempt to register
	// somebody else; both are refused.
	if strings.TrimSpace(req.Address) != "" {
		claimed := net.ParseIP(strings.TrimSpace(req.Address))
		if claimed == nil || !claimed.Equal(source) {
			writeJSON(w, http.StatusForbidden, errorResponse{Error: "address_mismatch"})
			return
		}
	}
	if strings.TrimSpace(req.PublicKey) != "" {
		raw, err := hex.DecodeString(strings.TrimSpace(req.PublicKey))
		if err != nil || len(raw) != ed25519.PublicKeySize {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "bad_public_key"})
			return
		}
		derived, err := addrForKey(ed25519.PublicKey(raw))
		if err != nil || !derived.Equal(source) {
			writeJSON(w, http.StatusForbidden, errorResponse{Error: "key_mismatch"})
			return
		}
	}

	if a.config().AllowAll {
		writeJSON(w, http.StatusOK, registerResponse{
			OK:           true,
			Allowlisted:  true,
			Registered:   false,
			ExitName:     a.config().Name,
			Address:      source.String(),
			Version:      daemonVersion,
			Auth:         a.config().apiAuthMode(),
			Capabilities: a.capabilities(),
		})
		return
	}

	client, existed, err := a.store.Register(req.Invite, Client{
		Address:    source.String(),
		PublicKey:  strings.TrimSpace(req.PublicKey),
		DeviceName: strings.TrimSpace(req.DeviceName),
		AppVersion: strings.TrimSpace(req.AppVersion),
	}, a.config().MaxClients)
	if err != nil {
		code := http.StatusForbidden
		name := "invite_invalid"
		switch {
		case errors.Is(err, errInviteExpired):
			code, name = http.StatusGone, "invite_expired"
		case errors.Is(err, errInviteUsed):
			code, name = http.StatusGone, "invite_used"
		case errors.Is(err, errAllowlistFull):
			code, name = http.StatusConflict, "allowlist_full"
		}
		a.log.warn("registration from %s refused: %s (%s)", source, name, err)
		writeJSON(w, code, errorResponse{Error: name, Message: err.Error()})
		return
	}

	// A token is issued only to a client that has none. The app registers on
	// every connect to refresh its device name and version, and rotating the
	// credential there made the token in the profile change on every reconnect
	// and overwrote one the operator had issued by hand. Rotation stays where
	// it belongs: POST /v1/token and the dashboard's New token button.
	var token string
	if client.TokenHash == "" {
		issued, err := a.store.IssueToken(source)
		if err != nil {
			a.log.warn("registration from %s: could not issue an API token: %v", source, err)
		} else {
			token = issued
		}
	}
	if existed {
		a.log.info("registration from %s: already allowed (%s), api token kept", source, client.Label)
	} else if token != "" {
		a.log.info("registration from %s: allowed as %q, api token issued", source, client.Label)
	} else {
		a.log.info("registration from %s: allowed as %q", source, client.Label)
	}
	writeJSON(w, http.StatusOK, registerResponse{
		OK:           true,
		Allowlisted:  true,
		Registered:   !existed,
		ExitName:     a.config().Name,
		Address:      source.String(),
		Label:        client.Label,
		Version:      daemonVersion,
		Auth:         a.config().apiAuthMode(),
		Capabilities: a.capabilities(),
		Token:        token,
	})
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

// bearerToken extracts the API token from the Authorization header. The token
// is a bearer credential: whoever holds it may read the counters of the client
// it belongs to, and it is bound to one Yggdrasil address.
func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// statusRecorder remembers the status code so the audit line can report it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(data)
}

// guard applies the per-source rate limit and writes one audit line per
// request. The Yggdrasil source check stays in the handlers: a direct handler
// test must not depend on the middleware.
func (a *apiServer) guard(next http.HandlerFunc, limiter *rateLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		source := remoteIP(r)
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		if limiter != nil && !limiter.allow(source.String()) {
			writeJSON(rec, http.StatusTooManyRequests, errorResponse{Error: "rate_limited"})
			a.audit(r, source, rec.status, started)
			return
		}
		next(rec, r)
		a.audit(r, source, rec.status, started)
	}
}

// audit writes one line per API request. Successful reads are debug: the app
// polls /v1/status every few seconds, and an info line for each poll would
// bury everything else. Refusals, registrations and token issues are info.
func (a *apiServer) audit(r *http.Request, source net.IP, status int, started time.Time) {
	// "api token" is the Authorization header, not the invite in the body:
	// /v1/register authenticates with the invite and receives its token in the
	// answer, so "none" there is expected and does not mean a refused request.
	token := "none"
	if bearerToken(r) != "" {
		token = "sent"
	}
	line := fmt.Sprintf("%s %s from %s: %d (api token: %s, %s)",
		r.Method, r.URL.Path, source, status, token,
		time.Since(started).Round(time.Millisecond))
	if status >= http.StatusBadRequest || r.Method != http.MethodGet {
		a.log.info("api: %s", line)
		return
	}
	a.log.debug("api: %s", line)
}

func remoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return net.ParseIP(r.RemoteAddr)
	}
	return net.ParseIP(host)
}

// rateLimiter is a small sliding-window limiter keyed by client address. It
// only exists for the registration endpoint, which is the one unauthenticated
// entry point.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	events map[string][]time.Time
	lastGC time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:  limit,
		window: window,
		events: make(map[string][]time.Time),
		lastGC: time.Now(),
	}
}

func (l *rateLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastGC) > l.window {
		for k, list := range l.events {
			if len(list) == 0 || now.Sub(list[len(list)-1]) > l.window {
				delete(l.events, k)
			}
		}
		l.lastGC = now
	}
	list := l.events[key]
	cut := 0
	for cut < len(list) && now.Sub(list[cut]) > l.window {
		cut++
	}
	list = list[cut:]
	if len(list) >= l.limit {
		l.events[key] = list
		return false
	}
	l.events[key] = append(list, now)
	return true
}
