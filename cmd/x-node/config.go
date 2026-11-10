package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the on-disk configuration of the exit daemon. The installer writes
// it, the service only reads it. Every duration is a Go duration string so the
// file stays readable ("24h", "15s").
type Config struct {
	Name              string `json:"name"`
	ListenAddress     string `json:"listenAddress"`
	SocksPort         int    `json:"socksPort"`
	APIPort           int    `json:"apiPort"`
	AllowAll          bool   `json:"allowAll"`
	Registration      bool   `json:"registration"`
	InviteTTL         string `json:"inviteTTL"`
	InviteUses        int    `json:"inviteUses"`
	MaxClients        int    `json:"maxClients"`
	MaxConnsPerClient int    `json:"maxConnsPerClient"`
	MaxConnsTotal     int    `json:"maxConnsTotal"`
	DialTimeout       string `json:"dialTimeout"`
	IdleTimeout       string `json:"idleTimeout"`
	BlockedPorts      []int  `json:"blockedPorts"`
	LogLevel          string `json:"logLevel"`
	StateDir          string `json:"stateDir"`

	// UDPEnabled turns on the SOCKS5 UDP ASSOCIATE relay. With it off the
	// daemon answers UDP ASSOCIATE with "command not supported", so a client
	// knows to keep its traffic on TCP instead of leaking it around the exit.
	UDPEnabled bool `json:"udpEnabled"`
	// UDPIdleTimeout closes a UDP association that carried no traffic for this
	// long. UDP has no close of its own, so this is the only way a relay socket
	// is released when the client disappears without closing its control link.
	UDPIdleTimeout string `json:"udpIdleTimeout"`
	// MaxUDPAssociations caps the number of simultaneous UDP associations. A
	// client that opens many control connections is additionally limited by
	// MaxConnsPerClient and MaxConnsTotal.
	MaxUDPAssociations int `json:"maxUDPAssociations"`

	// APIToken controls the bearer-token authentication of the HTTP API:
	//
	//   "required" — every endpoint except /v1/health needs the token of the
	//                client; /v1/status without one answers with the minimal
	//                public payload. This is the default.
	//   "optional" — tokens are accepted but not demanded. Only useful while
	//                an old client is being migrated.
	//   "off"      — no authentication at all, the pre-1.3 behaviour.
	APIToken string `json:"apiToken"`
	// APIRateLimit is how many API requests one source address may make per
	// minute. It covers every endpoint, including the cheap status poll, so a
	// single node cannot flood the service. 0 disables the limiter; the
	// registration and token endpoints keep their own stricter limits.
	APIRateLimit int `json:"apiRateLimit"`

	// Peers is the seed list of public Yggdrasil peers. The supervisor starts
	// from it and keeps [PeerCount] links alive from then on; the live list is
	// stored in the state directory, so replacing a peer survives a restart.
	// Inbound peers (the clients of the exit node) are never part of it.
	Peers []string `json:"peers,omitempty"`

	// ManualPeers are links the operator added by hand. They are kept
	// connected on top of [PeerCount], are never replaced or trimmed by the
	// supervisor and are removed only from the web interface or by editing
	// this list. Use it for a private peer that must stay pinned.
	ManualPeers []string `json:"manualPeers,omitempty"`

	// PeerCount is how many public peers the node keeps connected. The number
	// counts outbound links only: clients that dial the exit node are extra and
	// are never dropped or replaced by the supervisor.
	PeerCount int `json:"peerCount"`
	// PeerAutoReplace enables the replacement loop: a public peer that stays
	// down is swapped for a probed one, and a much faster candidate replaces
	// the slowest link. With it off the list is only topped up when the count
	// changes.
	PeerAutoReplace bool `json:"peerAutoReplace"`
	// PeerDeadTimeout is how long a public peer may stay down before it is
	// replaced.
	PeerDeadTimeout string `json:"peerDeadTimeout"`
	// PeerCheckInterval is how often the live links are inspected. The service
	// also re-reads this file on every check, so peer settings apply without a
	// restart.
	PeerCheckInterval string `json:"peerCheckInterval"`
	// PeerRefreshInterval is how often the public peer list is downloaded and
	// probed to look for better links.
	PeerRefreshInterval string `json:"peerRefreshInterval"`
	// PeerImprovementPercent is the margin by which a candidate must be faster
	// than the slowest link before it is swapped in. It stops the supervisor
	// from chasing noise.
	PeerImprovementPercent int `json:"peerImprovementPercent"`
	// AdminSocket is the Yggdrasil admin endpoint, e.g.
	// unix:///var/run/yggdrasil/yggdrasil.sock. The supervisor adds and
	// removes peers through it, so the node never restarts.
	AdminSocket string `json:"adminSocket"`
	// PeerProbePath is the x-node-peerprobe binary used to find live peers.
	PeerProbePath string `json:"peerProbePath"`

	// PeerURI is the public peering address of this node, e.g.
	// tls://203.0.113.10:13338?key=<public key>. It is printed in the invite
	// QR code so the app can add the exit node as a pinned peer. Empty when the
	// node accepts no incoming peering connections.
	PeerURI string `json:"peerURI,omitempty"`

	// IPv6 selects how outbound connections use IPv6:
	//
	//   "auto"   — use IPv6 when this host has a global IPv6 address and a
	//              default route; fall back to IPv4 otherwise. The default.
	//   "prefer" — always try IPv6 first, then IPv4.
	//   "off"    — never open an IPv6 connection, even if the host has one.
	IPv6 string `json:"ipv6"`

	// DomainResolution accepts SOCKS5 requests with ATYP=domain. The exit then
	// resolves the name itself, applies the destination policy to every result
	// and connects to an approved address. With it off such requests are
	// refused with "address type not supported", which is the pre-1.4
	// behaviour.
	DomainResolution bool `json:"domainResolution"`

	// Web is the built-in web interface. It is served from the same binary.
	Web WebConfig `json:"web"`

	// DNS is the optional Alfis-based DNS server for the clients of this exit.
	// It runs as a separate systemd unit next to the daemon, because binding
	// port 53 needs a capability the hardened x-node service does not have.
	DNS DNSConfig `json:"dns"`

	// Quotas holds the defaults applied to a client that has no explicit
	// limit. Zero means "no limit".
	Quotas QuotaDefaults `json:"quotas"`

	// Usage controls the traffic accounting that feeds the quotas and the web
	// interface.
	Usage UsageConfig `json:"usage"`
}

// WebConfig describes the built-in web interface.
type WebConfig struct {
	// Enabled turns the interface on. The installer enables it by default.
	Enabled bool `json:"enabled"`
	// Listen selects the bind address:
	//   "all"       — every address (default)
	//   "yggdrasil" — only the Yggdrasil address of this node
	//   "loopback"  — 127.0.0.1 and ::1, for a reverse proxy such as nginx
	Listen string `json:"listen"`
	// Address overrides Listen with an explicit IP address.
	Address string `json:"address,omitempty"`
	Port    int    `json:"port"`
	// Path is the URL prefix, e.g. /a1b2c3d4e5f6g7h8/web. It is not a secret
	// on its own: the login page protects the interface.
	Path string `json:"path"`
	// PublicURL is the address a browser should open, when the interface is
	// reachable through a reverse proxy or a public name. webURLs prints it
	// first, so "x-node web url" and the dashboard show the real address
	// instead of 127.0.0.1:8443. Empty means "derive it from the listener".
	PublicURL string `json:"publicURL,omitempty"`
	// TLS selects the transport:
	//   "auto" — HTTPS when a certificate is configured or can be created,
	//            plain HTTP otherwise (the default, behind nginx)
	//   "self" — always HTTPS, create a self-signed certificate when missing
	//   "off"  — plain HTTP
	TLS string `json:"tls"`
	// CertFile and KeyFile are the certificate pair. When they are empty the
	// daemon keeps them in the state directory.
	CertFile string `json:"certFile,omitempty"`
	KeyFile  string `json:"keyFile,omitempty"`
	// SessionTTL is how long a login stays valid.
	SessionTTL string `json:"sessionTTL"`
	// MaxFailedLogins is how many wrong passwords one address may try inside
	// Lockout before it is blocked. 0 disables the protection.
	MaxFailedLogins int    `json:"maxFailedLogins"`
	Lockout         string `json:"lockout"`
	// TrustedProxies lists the addresses whose X-Forwarded-For header is
	// believed. The installer adds the loopback addresses for nginx.
	TrustedProxies []string `json:"trustedProxies"`
	// Compact keeps the interface 25% narrower on a desktop screen. On a phone
	// the layout already fills the width, so the switch has no effect there.
	Compact bool `json:"compact"`
}

// DNSConfig describes the Alfis DNS server that is offered to the clients of
// this exit node. The server runs as "x-node-dns.service" and listens on the
// Yggdrasil address of the host, so a client reaches it through the same tunnel
// it already uses for the exit.
type DNSConfig struct {
	// Enabled turns the DNS server on. A fresh installation enables it by
	// default; the installer asks once and the dashboard can change it later.
	Enabled bool `json:"enabled"`

	// Listen is the full host:port the resolver binds to, e.g.
	// "[200:...]:53". Empty means "the Yggdrasil address on port 53".
	Listen string `json:"listen,omitempty"`
	Port   int    `json:"port"`

	// WebListen is the address of the Alfis web interface. The panel is opened
	// through the dashboard proxy, so the default is loopback on WebPort: the
	// Yggdrasil address may not be assigned yet when the resolver starts.
	// Alfis speaks plain HTTP; the proxy adds TLS.
	WebListen string `json:"webListen,omitempty"`
	WebPort   int    `json:"webPort"`
	// WebPassword protects the Alfis web interface. Alfis refuses to start the
	// interface without one; the installer and the dashboard generate it.
	WebPassword string `json:"webPassword,omitempty"`

	// Forwarders are the upstream resolvers Alfis uses for ordinary names.
	Forwarders []string `json:"forwarders,omitempty"`
	// CacheMemoryLimitMB caps the resolver cache. 0 means unlimited.
	CacheMemoryLimitMB int `json:"cacheMemoryLimitMB,omitempty"`
}

// QuotaDefaults are the limits of a client without its own quota entry.
type QuotaDefaults struct {
	// MonthlyBytes and DailyBytes cap the traffic of one client. 0 = no limit.
	MonthlyBytes int64 `json:"monthlyBytes"`
	DailyBytes   int64 `json:"dailyBytes"`
	// DownKbps and UpKbps throttle one client. 0 = no limit.
	DownKbps int `json:"downKbps"`
	UpKbps   int `json:"upKbps"`
}

// UsageConfig controls traffic accounting.
type UsageConfig struct {
	// Enabled turns accounting on. Without it quotas cannot be enforced.
	Enabled bool `json:"enabled"`
	// Timezone is the zone whose midnight and month start reset the counters,
	// e.g. "UTC" or "Europe/Moscow".
	Timezone string `json:"timezone"`
}

const (
	defaultConfigPath = "/etc/x-node/config.json"
	defaultStateDir   = "/var/lib/x-node"

	apiAuthRequired = "required"
	apiAuthOptional = "optional"
	apiAuthOff      = "off"

	ipv6Auto   = "auto"
	ipv6Prefer = "prefer"
	ipv6Off    = "off"

	webListenAll       = "all"
	webListenYggdrasil = "yggdrasil"
	webListenLoopback  = "loopback"

	webTLSAuto = "auto"
	webTLSSelf = "self"
	webTLSOff  = "off"
)

// defaultWebPath is only a fallback for a hand-written configuration: the
// installer always generates a random path.
const defaultWebPath = "/x-node/web"

// apiAuthMode normalises the apiToken value. An unknown value is reported as
// required, so a typo in the config never opens the API by accident.
func (c Config) apiAuthMode() string {
	switch strings.ToLower(strings.TrimSpace(c.APIToken)) {
	case apiAuthOff:
		return apiAuthOff
	case apiAuthOptional:
		return apiAuthOptional
	default:
		return apiAuthRequired
	}
}

// tokenRequired reports whether /v1/status without a token must be answered
// with the minimal payload.
func (c Config) tokenRequired() bool {
	return c.apiAuthMode() == apiAuthRequired && !c.AllowAll
}

func validAPIAuth(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case apiAuthRequired, apiAuthOptional, apiAuthOff:
		return true
	}
	return false
}

func defaultConfig() Config {
	return Config{
		Name:                   "X-Node",
		SocksPort:              1080,
		APIPort:                0,
		Registration:           true,
		InviteTTL:              "24h",
		InviteUses:             1,
		MaxClients:             64,
		MaxConnsPerClient:      64,
		MaxConnsTotal:          512,
		DialTimeout:            "15s",
		IdleTimeout:            "5m",
		BlockedPorts:           []int{25},
		LogLevel:               "info",
		StateDir:               defaultStateDir,
		UDPEnabled:             true,
		UDPIdleTimeout:         "2m",
		MaxUDPAssociations:     64,
		APIToken:               apiAuthRequired,
		APIRateLimit:           120,
		PeerCount:              3,
		PeerAutoReplace:        true,
		PeerDeadTimeout:        "90s",
		PeerCheckInterval:      "30s",
		PeerRefreshInterval:    "6h",
		PeerImprovementPercent: 25,
		AdminSocket:            defaultAdminSocket,
		PeerProbePath:          defaultPeerProbePath,
		IPv6:                   ipv6Auto,
		DomainResolution:       true,
		Web: WebConfig{
			Enabled:         true,
			Listen:          webListenAll,
			Port:            443,
			Path:            defaultWebPath,
			TLS:             webTLSAuto,
			SessionTTL:      "12h",
			MaxFailedLogins: 5,
			Lockout:         "15m",
			TrustedProxies:  []string{"127.0.0.1", "::1"},
		},
		DNS: DNSConfig{
			Enabled:            true,
			Port:               53,
			WebPort:            8082,
			Forwarders:         []string{"94.140.14.14:53", "94.140.15.15:53"},
			CacheMemoryLimitMB: 100,
		},
		Usage: UsageConfig{
			Enabled:  true,
			Timezone: "UTC",
		},
	}
}

// loadEffectiveConfig reads the file configuration and applies the overlay
// written by the web interface. The daemon and the peer supervisor use it, so a
// setting changed in the dashboard applies without root and without a restart.
func loadEffectiveConfig(path string) (Config, error) {
	cfg, err := loadConfig(path)
	if err != nil {
		return cfg, err
	}
	return loadSettingsOverlay(cfg)
}

// configStamp identifies the base file and the overlay, so the supervisor
// notices a change in either of them.
func configStamp(path string) string {
	overlay := ""
	if cfg, err := loadConfig(path); err == nil {
		overlay = cfg.statePath(settingsFileName)
	}
	return fileStampString(path) + "|" + fileStampString(overlay)
}

func fileStampString(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}

// configPath resolves the -config flag, the environment and the default path.
func configPath(flagValue string) string {
	if strings.TrimSpace(flagValue) != "" {
		return flagValue
	}
	if env := os.Getenv("X_NODE_CONFIG"); strings.TrimSpace(env) != "" {
		return env
	}
	return defaultConfigPath
}

// loadConfig reads the file and fills every missing field with its default.
func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	d := defaultConfig()
	if c.Name == "" {
		c.Name = d.Name
	}
	if c.SocksPort == 0 {
		c.SocksPort = d.SocksPort
	}
	// APIPort is deliberately not defaulted here: 0 is the default and a
	// valid value that means "serve /v1/... on the SOCKS5 listener". A missing
	// field keeps the default from defaultConfig because the file is decoded
	// over it.
	if c.InviteTTL == "" {
		c.InviteTTL = d.InviteTTL
	}
	if c.InviteUses <= 0 {
		c.InviteUses = d.InviteUses
	}
	if c.MaxClients <= 0 {
		c.MaxClients = d.MaxClients
	}
	if c.MaxConnsPerClient <= 0 {
		c.MaxConnsPerClient = d.MaxConnsPerClient
	}
	if c.MaxConnsTotal <= 0 {
		c.MaxConnsTotal = d.MaxConnsTotal
	}
	if c.DialTimeout == "" {
		c.DialTimeout = d.DialTimeout
	}
	if c.IdleTimeout == "" {
		c.IdleTimeout = d.IdleTimeout
	}
	if c.BlockedPorts == nil {
		c.BlockedPorts = d.BlockedPorts
	}
	if c.LogLevel == "" {
		c.LogLevel = d.LogLevel
	}
	if c.StateDir == "" {
		c.StateDir = d.StateDir
	}
	// UDPEnabled is deliberately not defaulted here: false is a valid value
	// that turns the relay off. A missing field keeps the default from
	// defaultConfig because the file is decoded over it.
	if c.UDPIdleTimeout == "" {
		c.UDPIdleTimeout = d.UDPIdleTimeout
	}
	if c.MaxUDPAssociations <= 0 {
		c.MaxUDPAssociations = d.MaxUDPAssociations
	}
	if strings.TrimSpace(c.APIToken) == "" {
		c.APIToken = d.APIToken
	}
	// APIRateLimit is deliberately not defaulted here: 0 is a valid value
	// that turns the general limiter off. A missing field keeps the default
	// from defaultConfig because the file is decoded over it.
	if c.PeerCount == 0 {
		c.PeerCount = d.PeerCount
	}
	if c.PeerDeadTimeout == "" {
		c.PeerDeadTimeout = d.PeerDeadTimeout
	}
	if c.PeerCheckInterval == "" {
		c.PeerCheckInterval = d.PeerCheckInterval
	}
	if c.PeerRefreshInterval == "" {
		c.PeerRefreshInterval = d.PeerRefreshInterval
	}
	if c.PeerImprovementPercent == 0 {
		c.PeerImprovementPercent = d.PeerImprovementPercent
	}
	if c.AdminSocket == "" {
		c.AdminSocket = d.AdminSocket
	}
	if c.PeerProbePath == "" {
		c.PeerProbePath = d.PeerProbePath
	}
	if c.IPv6 == "" {
		c.IPv6 = d.IPv6
	}
	if c.Web.Listen == "" {
		c.Web.Listen = d.Web.Listen
	}
	if c.Web.Port == 0 {
		c.Web.Port = d.Web.Port
	}
	if c.Web.Path == "" {
		c.Web.Path = d.Web.Path
	}
	if c.Web.TLS == "" {
		c.Web.TLS = d.Web.TLS
	}
	if c.Web.SessionTTL == "" {
		c.Web.SessionTTL = d.Web.SessionTTL
	}
	if c.Web.Lockout == "" {
		c.Web.Lockout = d.Web.Lockout
	}
	if c.Web.TrustedProxies == nil {
		c.Web.TrustedProxies = d.Web.TrustedProxies
	}
	if c.DNS.Port == 0 {
		c.DNS.Port = d.DNS.Port
	}
	if c.DNS.WebPort == 0 {
		c.DNS.WebPort = d.DNS.WebPort
	}
	if c.DNS.Forwarders == nil {
		c.DNS.Forwarders = d.DNS.Forwarders
	}
	if c.DNS.CacheMemoryLimitMB == 0 {
		c.DNS.CacheMemoryLimitMB = d.DNS.CacheMemoryLimitMB
	}
	if strings.TrimSpace(c.DNS.Listen) == "" {
		c.DNS.Listen = net.JoinHostPort(c.ListenAddress, strconv.Itoa(c.DNS.Port))
	}
	if strings.TrimSpace(c.DNS.WebListen) == "" ||
		c.DNS.WebListen == net.JoinHostPort(c.ListenAddress, strconv.Itoa(c.DNS.WebPort)) {
		// The old default bound the panel to the Yggdrasil address. The panel
		// is proxied through the dashboard now, so loopback is enough and does
		// not depend on the tunnel interface being up at boot.
		c.DNS.WebListen = net.JoinHostPort("127.0.0.1", strconv.Itoa(c.DNS.WebPort))
	}
	if c.Usage.Timezone == "" {
		c.Usage.Timezone = d.Usage.Timezone
	}
	c.Web.Path = normalizeWebPath(c.Web.Path)
}

func (c *Config) validate() error {
	if !isYggAddress(net.ParseIP(strings.TrimSpace(c.ListenAddress))) {
		return fmt.Errorf("listenAddress %q must be a Yggdrasil node address (200::/7)", c.ListenAddress)
	}
	if c.SocksPort < 1 || c.SocksPort > 65535 {
		return fmt.Errorf("socksPort %d is out of range", c.SocksPort)
	}
	if c.APIPort < 0 || c.APIPort > 65535 {
		return fmt.Errorf("apiPort %d is out of range (0 = share the SOCKS5 port)", c.APIPort)
	}
	if c.APIPort != 0 && c.SocksPort == c.APIPort {
		return fmt.Errorf("socksPort and apiPort must differ (use apiPort 0 to share the port)")
	}
	if _, err := c.inviteTTL(); err != nil {
		return err
	}
	if _, err := c.dialTimeout(); err != nil {
		return err
	}
	if _, err := c.idleTimeout(); err != nil {
		return err
	}
	if _, err := c.udpIdleTimeout(); err != nil {
		return err
	}
	if c.MaxUDPAssociations < 1 || c.MaxUDPAssociations > 4096 {
		return fmt.Errorf("maxUDPAssociations %d is out of range (1..4096)", c.MaxUDPAssociations)
	}
	if !validAPIAuth(c.APIToken) {
		return fmt.Errorf("apiToken %q must be required, optional or off", c.APIToken)
	}
	if c.APIRateLimit < 0 || c.APIRateLimit > 6000 {
		return fmt.Errorf("apiRateLimit %d is out of range (0..6000, 0 disables the limiter)", c.APIRateLimit)
	}
	if _, err := c.peerPort(); err != nil {
		return err
	}
	if c.PeerCount < 1 || c.PeerCount > 50 {
		return fmt.Errorf("peerCount %d is out of range (1..50)", c.PeerCount)
	}
	if _, err := c.peerDeadTimeout(); err != nil {
		return err
	}
	if _, err := c.peerCheckInterval(); err != nil {
		return err
	}
	if _, err := c.peerRefreshInterval(); err != nil {
		return err
	}
	if c.PeerImprovementPercent < 0 || c.PeerImprovementPercent > 90 {
		return fmt.Errorf("peerImprovementPercent %d is out of range (0..90)", c.PeerImprovementPercent)
	}
	if _, _, err := adminTarget(c.AdminSocket); err != nil {
		return err
	}
	for _, raw := range c.Peers {
		if err := validatePeerURI(raw); err != nil {
			return err
		}
	}
	for _, raw := range c.ManualPeers {
		if err := validatePeerURI(raw); err != nil {
			return fmt.Errorf("manualPeers: %w", err)
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.IPv6)) {
	case ipv6Auto, ipv6Prefer, ipv6Off:
	default:
		return fmt.Errorf("ipv6 %q must be auto, prefer or off", c.IPv6)
	}
	switch strings.ToLower(strings.TrimSpace(c.Web.Listen)) {
	case webListenAll, webListenYggdrasil, webListenLoopback:
	default:
		return fmt.Errorf("web.listen %q must be all, yggdrasil or loopback", c.Web.Listen)
	}
	if strings.TrimSpace(c.Web.Address) != "" && net.ParseIP(strings.TrimSpace(c.Web.Address)) == nil {
		return fmt.Errorf("web.address %q is not an IP address", c.Web.Address)
	}
	if c.Web.Port < 1 || c.Web.Port > 65535 {
		return fmt.Errorf("web.port %d is out of range", c.Web.Port)
	}
	if !validWebPath(c.Web.Path) {
		return fmt.Errorf("web.path %q must be a URL path such as /<random>/web", c.Web.Path)
	}
	if raw := strings.TrimSpace(c.Web.PublicURL); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("web.publicURL %q must be an http:// or https:// URL", c.Web.PublicURL)
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.Web.TLS)) {
	case webTLSAuto, webTLSSelf, webTLSOff:
	default:
		return fmt.Errorf("web.tls %q must be auto, self or off", c.Web.TLS)
	}
	if _, err := c.webSessionTTL(); err != nil {
		return err
	}
	if _, err := c.webLockout(); err != nil {
		return err
	}
	if c.Web.MaxFailedLogins < 0 || c.Web.MaxFailedLogins > 1000 {
		return fmt.Errorf("web.maxFailedLogins %d is out of range (0..1000, 0 disables)", c.Web.MaxFailedLogins)
	}
	for _, proxy := range c.Web.TrustedProxies {
		if net.ParseIP(strings.TrimSpace(proxy)) == nil {
			return fmt.Errorf("web.trustedProxies entry %q is not an IP address", proxy)
		}
	}
	if c.Quotas.MonthlyBytes < 0 || c.Quotas.DailyBytes < 0 {
		return fmt.Errorf("quotas must not be negative")
	}
	if c.Quotas.DownKbps < 0 || c.Quotas.UpKbps < 0 {
		return fmt.Errorf("quotas must not be negative")
	}
	if _, err := c.usageLocation(); err != nil {
		return err
	}
	if err := c.validateDNS(); err != nil {
		return err
	}
	return nil
}

// validateDNS checks the two listeners of the Alfis process. The forwarders
// may be plain host:port pairs or DoH URLs, so Alfis is the authority on them;
// only the listeners that x-node prints and links to are checked here.
func (c *Config) validateDNS() error {
	// The raw fields may be empty: the effective address is derived from
	// listenAddress and the port. Validating the derived value catches both a
	// hand-written address and a broken fallback.
	for name, value := range map[string]string{
		"dns.listen":    c.dnsListen(),
		"dns.webListen": c.dnsWebListen(),
	} {
		host, port, err := net.SplitHostPort(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("%s %q is not host:port: %w", name, value, err)
		}
		if strings.TrimSpace(host) != "" && net.ParseIP(host) == nil {
			return fmt.Errorf("%s %q must use an IP address", name, value)
		}
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return fmt.Errorf("%s %q has no valid port", name, value)
		}
	}
	if c.DNS.Port < 1 || c.DNS.Port > 65535 {
		return fmt.Errorf("dns.port %d is out of range", c.DNS.Port)
	}
	if c.DNS.WebPort < 1 || c.DNS.WebPort > 65535 {
		return fmt.Errorf("dns.webPort %d is out of range", c.DNS.WebPort)
	}
	if c.DNS.CacheMemoryLimitMB < 0 {
		return fmt.Errorf("dns.cacheMemoryLimitMB must not be negative")
	}
	return nil
}

// normalizeWebPath makes a hand-written path usable: a leading slash is added
// and a trailing slash is dropped.
func normalizeWebPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultWebPath
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	return strings.TrimRight(value, "/")
}

// validWebPath accepts one to four path segments of letters, digits, dash and
// underscore. The installer uses /<random>/web; a hand-written path may be
// simpler, but no dots, no percent-encoding and no query string.
func validWebPath(value string) bool {
	if !strings.HasPrefix(value, "/") || len(value) > 200 {
		return false
	}
	segments := strings.Split(strings.TrimPrefix(value, "/"), "/")
	if len(segments) > 4 {
		return false
	}
	for _, segment := range segments {
		if segment == "" || len(segment) > 64 {
			return false
		}
		for _, char := range segment {
			switch {
			case char >= 'a' && char <= 'z':
			case char >= 'A' && char <= 'Z':
			case char >= '0' && char <= '9':
			case char == '-' || char == '_':
			default:
				return false
			}
		}
	}
	return true
}

func (c Config) ipv6Mode() string {
	return strings.ToLower(strings.TrimSpace(c.IPv6))
}

func (c Config) webTLSMode() string {
	return strings.ToLower(strings.TrimSpace(c.Web.TLS))
}

// webBindHost turns web.listen into the host passed to net.Listen.
func (c Config) webBindHost() string {
	if address := strings.TrimSpace(c.Web.Address); address != "" {
		return address
	}
	switch strings.ToLower(strings.TrimSpace(c.Web.Listen)) {
	case webListenYggdrasil:
		return c.ListenAddress
	case webListenLoopback:
		return "127.0.0.1"
	default:
		return ""
	}
}

// dnsListen is the host:port the Alfis resolver binds to.
func (c Config) dnsListen() string {
	if value := strings.TrimSpace(c.DNS.Listen); value != "" {
		return value
	}
	return net.JoinHostPort(c.ListenAddress, strconv.Itoa(c.DNS.Port))
}

// dnsWebListen is the host:port of the Alfis web interface. The panel is
// reached through the dashboard proxy, so the fallback is loopback: binding to
// the Yggdrasil address would expose the panel directly and fail while the
// tunnel interface is not up yet.
func (c Config) dnsWebListen() string {
	if value := strings.TrimSpace(c.DNS.WebListen); value != "" {
		return value
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(c.DNS.WebPort))
}

// dnsWebPath is the URL prefix of the Alfis panel inside the X-Node web
// server. It is the sibling of the dashboard path, so /<random>/web becomes
// /<random>/dns: the panel then lives on the same TLS endpoint and port as the
// dashboard instead of needing a second public port.
func (c Config) dnsWebPath() string {
	prefix := c.Web.Path
	if prefix == "" {
		prefix = defaultWebPath
	}
	if index := strings.LastIndex(prefix, "/"); index > 0 {
		return prefix[:index] + "/dns"
	}
	return "/dns"
}

// dnsWebTarget is the host:port the proxy dials. A wildcard listener is
// reached through loopback, because 0.0.0.0 and :: are not dialable addresses.
func (c Config) dnsWebTarget() string {
	host, port, err := net.SplitHostPort(c.dnsWebListen())
	if err != nil {
		return ""
	}
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	return net.JoinHostPort(host, port)
}

// dnsPanelURL is the address a browser opens for the Alfis panel: the
// dashboard endpoint with the DNS path. The Alfis listener itself is on
// loopback and is only reached through this proxy.
func (c Config) dnsPanelURL() string {
	urls := webURLs(c)
	if len(urls) == 0 {
		return ""
	}
	base := urls[0]
	if path := strings.TrimSpace(c.Web.Path); path != "" {
		base = strings.TrimSuffix(base, path)
	}
	return strings.TrimSuffix(base, "/") + c.dnsWebPath() + "/"
}

// dnsServerAddress is the address published to Y-Node in the capabilities
// block. The client always queries port 53; DNSPort carries a non-default port
// for a client that understands it.
func (c Config) dnsServerAddress() string {
	if !c.DNS.Enabled {
		return ""
	}
	return c.ListenAddress
}

func (c Config) dnsServerPort() int {
	if !c.DNS.Enabled {
		return 0
	}
	return c.DNS.Port
}

// alfisConfig renders the TOML document of the resolver. Alfis runs its own
// Yggdrasil node (the Alfis network), so it has a separate peering listener and
// its own key store; only the DNS and web listeners point at this host's
// Yggdrasil address.
func (c Config) alfisConfig() string {
	forwarders := c.DNS.Forwarders
	if len(forwarders) == 0 {
		forwarders = defaultConfig().DNS.Forwarders
	}
	cache := c.DNS.CacheMemoryLimitMB
	if cache <= 0 {
		cache = defaultConfig().DNS.CacheMemoryLimitMB
	}
	var out strings.Builder
	out.WriteString("# Written by X-Node. Settings -> DNS server regenerates this file.\n")
	out.WriteString("origin = \"0000001D2A77D63477172678502E51DE7F346061FF7EB188A2445ECA3FC0780E\"\n")
	out.WriteString("key_files = []\n")
	out.WriteString("check_blocks = 8\n")
	out.WriteString("dark_theme = true\n\n")
	out.WriteString("[net]\n")
	out.WriteString("peers = [\"peer-v4.alfis.name:4244\", \"peer-v6.alfis.name:4244\"]\n")
	out.WriteString("listen = \"127.0.0.1:4244\"\n")
	out.WriteString("public = false\n")
	out.WriteString("yggdrasil_only = false\n\n")
	out.WriteString("[dns]\n")
	fmt.Fprintf(&out, "listen = %s\n", strconv.Quote(c.dnsListen()))
	out.WriteString("threads = 10\n")
	fmt.Fprintf(&out, "forwarders = %s\n", tomlStringList(forwarders))
	out.WriteString("bootstraps = [\"9.9.9.9:53\", \"94.140.14.14:53\"]\n")
	out.WriteString("hosts = []\n")
	out.WriteString("enable_0x20 = true\n")
	fmt.Fprintf(&out, "cache_memory_limit_mb = %d\n\n", cache)
	out.WriteString("[mining]\n")
	out.WriteString("threads = 0\n")
	out.WriteString("lower = true\n\n")
	out.WriteString("[webui]\n")
	fmt.Fprintf(&out, "listen = %s\n", strconv.Quote(c.dnsWebListen()))
	fmt.Fprintf(&out, "password = %s\n", strconv.Quote(c.DNS.WebPassword))
	return out.String()
}

func tomlStringList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, strconv.Quote(value))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// webSessionTTL turns the configured lifetime into a duration. A zero value,
// "0" and "unlimited" mean the session never expires: the cookie carries no
// expiry and the signer never rejects it.
func (c Config) webSessionTTL() (time.Duration, error) {
	raw := strings.TrimSpace(c.Web.SessionTTL)
	switch strings.ToLower(raw) {
	case "0", "unlimited":
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("web.sessionTTL %q is not a duration; use 0 or unlimited for a session that never expires", c.Web.SessionTTL)
	}
	return d, nil
}

func (c Config) webLockout() (time.Duration, error) {
	d, err := time.ParseDuration(c.Web.Lockout)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("web.lockout %q is not a positive duration", c.Web.Lockout)
	}
	return d, nil
}

func (c Config) usageLocation() (*time.Location, error) {
	name := strings.TrimSpace(c.Usage.Timezone)
	if name == "" {
		name = "UTC"
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("usage.timezone %q: %w", name, err)
	}
	return location, nil
}

// peerSchemes are the transports Yggdrasil can dial.
var peerSchemes = map[string]bool{
	"tls":  true,
	"tcp":  true,
	"quic": true,
	"ws":   true,
	"wss":  true,
}

// peerPort validates PeerURI and returns the port it points at, or 0 when no
// peering address is configured.
func (c Config) peerPort() (int, error) {
	raw := strings.TrimSpace(c.PeerURI)
	if raw == "" {
		return 0, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return 0, fmt.Errorf("peerURI %q is not a URL: %w", c.PeerURI, err)
	}
	if !peerSchemes[strings.ToLower(u.Scheme)] {
		return 0, fmt.Errorf("peerURI %q uses an unsupported transport", c.PeerURI)
	}
	if strings.TrimSpace(u.Hostname()) == "" {
		return 0, fmt.Errorf("peerURI %q has no host", c.PeerURI)
	}
	port, err := net.LookupPort("tcp", u.Port())
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("peerURI %q has no valid port", c.PeerURI)
	}
	return port, nil
}

func (c Config) inviteTTL() (time.Duration, error) {
	d, err := time.ParseDuration(c.InviteTTL)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("inviteTTL %q is not a positive duration", c.InviteTTL)
	}
	return d, nil
}

func (c Config) dialTimeout() (time.Duration, error) {
	d, err := time.ParseDuration(c.DialTimeout)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("dialTimeout %q is not a positive duration", c.DialTimeout)
	}
	return d, nil
}

func (c Config) idleTimeout() (time.Duration, error) {
	d, err := time.ParseDuration(c.IdleTimeout)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("idleTimeout %q is not a positive duration", c.IdleTimeout)
	}
	return d, nil
}

func (c Config) udpIdleTimeout() (time.Duration, error) {
	d, err := time.ParseDuration(c.UDPIdleTimeout)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("udpIdleTimeout %q is not a positive duration", c.UDPIdleTimeout)
	}
	return d, nil
}

func (c Config) peerDeadTimeout() (time.Duration, error) {
	d, err := time.ParseDuration(c.PeerDeadTimeout)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("peerDeadTimeout %q is not a positive duration", c.PeerDeadTimeout)
	}
	return d, nil
}

func (c Config) peerCheckInterval() (time.Duration, error) {
	d, err := time.ParseDuration(c.PeerCheckInterval)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("peerCheckInterval %q is not a positive duration", c.PeerCheckInterval)
	}
	return d, nil
}

func (c Config) peerRefreshInterval() (time.Duration, error) {
	d, err := time.ParseDuration(c.PeerRefreshInterval)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("peerRefreshInterval %q is not a positive duration", c.PeerRefreshInterval)
	}
	return d, nil
}

// validatePeerURI checks one entry of the public peer list. The options
// (?key=..., priority, ...) are left to Yggdrasil: only the transport, the host
// and the port are checked here.
func validatePeerURI(raw string) error {
	value := strings.TrimSpace(raw)
	if value == "" {
		return fmt.Errorf("peer URI is empty")
	}
	u, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("peer URI %q is not a URL: %w", raw, err)
	}
	if !peerSchemes[strings.ToLower(u.Scheme)] {
		return fmt.Errorf("peer URI %q uses an unsupported transport", raw)
	}
	if strings.TrimSpace(u.Hostname()) == "" {
		return fmt.Errorf("peer URI %q has no host", raw)
	}
	if port := u.Port(); port != "" {
		if number, err := net.LookupPort("tcp", port); err != nil || number < 1 || number > 65535 {
			return fmt.Errorf("peer URI %q has no valid port", raw)
		}
	}
	return nil
}

// apiShared reports whether the HTTP API shares the SOCKS5 listener.
func (c Config) apiShared() bool { return c.APIPort == 0 }

// apiPort is the TCP port the HTTP API is reachable on.
func (c Config) apiPort() int {
	if c.apiShared() {
		return c.SocksPort
	}
	return c.APIPort
}

// statePath returns a file inside the state directory.
func (c Config) statePath(name string) string {
	return filepath.Join(c.StateDir, name)
}
