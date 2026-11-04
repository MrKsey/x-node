package main

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"
)

// Web interface authentication.
//
// The password is stored as PBKDF2-HMAC-SHA256 with a per-installation salt and
// 600 000 iterations (the OWASP recommendation for this construction). The
// session is a signed cookie: the payload carries the user name and the expiry,
// the signature is HMAC-SHA256 with a key generated once and kept in web.json,
// so sessions survive a service restart. The cookie is HttpOnly, SameSite=Strict
// and Secure when the interface is served over HTTPS.

const (
	webCredentialsFileName = "web.json"
	webPasswordIterations  = 600_000
	webPasswordSaltBytes   = 16
	webPasswordKeyBytes    = 32
	webSessionKeyBytes     = 32
	webCookieName          = "x_node_session"
)

// webCredentials is the content of web.json in the state directory.
type webCredentials struct {
	Version      int       `json:"version"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"passwordHash"`
	SessionKey   string    `json:"sessionKey"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (c *webCredentials) clone() webCredentials {
	copied := *c
	return copied
}

func loadWebCredentials(path string) (webCredentials, error) {
	var creds webCredentials
	raw, err := os.ReadFile(path)
	if err != nil {
		return creds, err
	}
	if err := json.Unmarshal(raw, &creds); err != nil {
		return creds, err
	}
	if creds.Username == "" {
		creds.Username = "admin"
	}
	return creds, nil
}

func saveWebCredentials(path string, creds webCredentials) error {
	creds.Version = 1
	creds.UpdatedAt = time.Now().UTC()
	return writeJSONAtomic(path, creds)
}

// newWebCredentials creates the first credential set: the given (or generated)
// password and a fresh session key.
func newWebCredentials(username, password string) (webCredentials, error) {
	if strings.TrimSpace(username) == "" {
		username = "admin"
	}
	hash, err := hashWebPassword(password)
	if err != nil {
		return webCredentials{}, err
	}
	key := make([]byte, webSessionKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return webCredentials{}, err
	}
	return webCredentials{
		Version:      1,
		Username:     username,
		PasswordHash: hash,
		SessionKey:   base64.RawStdEncoding.EncodeToString(key),
		UpdatedAt:    time.Now().UTC(),
	}, nil
}

// hashWebPassword returns "pbkdf2-sha256$<iterations>$<salt>$<key>".
func hashWebPassword(password string) (string, error) {
	salt := make([]byte, webPasswordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, webPasswordIterations, webPasswordKeyBytes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s",
		webPasswordIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// verifyWebPassword checks a password against the stored hash in constant time.
func verifyWebPassword(stored, password string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	var iterations int
	if _, err := fmt.Sscanf(parts[1], "%d", &iterations); err != nil || iterations < 1000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(key, expected) == 1
}

// generatePassword returns a 20-character password with upper and lower case
// letters, digits and a few punctuation marks. Characters that look alike
// (0/O, 1/l/I) are left out so the password can be typed from a screenshot.
func generatePassword() (string, error) {
	const (
		lower   = "abcdefghijkmnopqrstuvwxyz"
		upper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"
		digits  = "23456789"
		symbols = "!@#$%^&*-_=+?"
		all     = lower + upper + digits + symbols
	)
	length := 20
	out := make([]byte, 0, length)
	// One character from every class first, so the result always satisfies a
	// "must contain" rule.
	for _, class := range []string{lower, upper, digits, symbols} {
		index, err := randIndex(len(class))
		if err != nil {
			return "", err
		}
		out = append(out, class[index])
	}
	for len(out) < length {
		index, err := randIndex(len(all))
		if err != nil {
			return "", err
		}
		out = append(out, all[index])
	}
	// Shuffle so the guaranteed characters are not the first four.
	for i := len(out) - 1; i > 0; i-- {
		j, err := randIndex(i + 1)
		if err != nil {
			return "", err
		}
		out[i], out[j] = out[j], out[i]
	}
	return string(out), nil
}

func randIndex(limit int) (int, error) {
	value, err := rand.Int(rand.Reader, big.NewInt(int64(limit)))
	if err != nil {
		return 0, err
	}
	return int(value.Int64()), nil
}

// webSession is the payload of the signed cookie.
type webSession struct {
	User    string `json:"u"`
	Expires int64  `json:"e"`
}

// sessionSigner creates and verifies session cookies.
type sessionSigner struct {
	key []byte
	ttl time.Duration
}

func newSessionSigner(creds webCredentials, ttl time.Duration) (*sessionSigner, error) {
	key, err := base64.RawStdEncoding.DecodeString(creds.SessionKey)
	if err != nil || len(key) < 16 {
		return nil, fmt.Errorf("web.json has no usable sessionKey")
	}
	return &sessionSigner{key: key, ttl: ttl}, nil
}

func (s *sessionSigner) sign(session webSession) (string, error) {
	payload, err := json.Marshal(session)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(encoded))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encoded + "." + signature, nil
}

func (s *sessionSigner) verify(value string) (webSession, bool) {
	var session webSession
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return session, false
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(parts[0]))
	expected := mac.Sum(nil)
	actual, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || subtle.ConstantTimeCompare(expected, actual) != 1 {
		return session, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return session, false
	}
	if err := json.Unmarshal(payload, &session); err != nil {
		return session, false
	}
	if session.Expires < time.Now().Unix() {
		return session, false
	}
	return session, true
}

// loginGuard blocks an address after too many wrong passwords. The delay grows
// linearly with the number of blocks, so a bot that keeps trying waits longer
// and longer, while a user who mistyped twice is not locked out for long.
type loginGuard struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	attempts map[string]*loginState
}

type loginState struct {
	failures int
	first    time.Time
	blocked  time.Time
	blocks   int
}

func newLoginGuard(max int, window time.Duration) *loginGuard {
	return &loginGuard{
		max:      max,
		window:   window,
		attempts: make(map[string]*loginState),
	}
}

// blockedUntil reports whether the address is currently blocked.
func (g *loginGuard) blockedUntil(address string) (time.Time, bool) {
	if g == nil || g.max <= 0 {
		return time.Time{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	state, ok := g.attempts[address]
	if !ok {
		return time.Time{}, false
	}
	if time.Now().Before(state.blocked) {
		return state.blocked, true
	}
	return time.Time{}, false
}

// fail records a wrong password and returns the moment the address is blocked,
// or the zero time when it is not.
func (g *loginGuard) fail(address string) time.Time {
	if g == nil || g.max <= 0 {
		return time.Time{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	state, ok := g.attempts[address]
	if !ok || now.Sub(state.first) > g.window {
		state = &loginState{first: now}
		g.attempts[address] = state
	}
	state.failures++
	if state.failures < g.max {
		return time.Time{}
	}
	state.blocks++
	delay := g.window * time.Duration(state.blocks)
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	state.blocked = now.Add(delay)
	state.failures = 0
	state.first = now
	return state.blocked
}

// success clears the failure counter of an address.
func (g *loginGuard) success(address string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	delete(g.attempts, address)
	g.mu.Unlock()
}

// cleanup drops entries that are long past their window.
func (g *loginGuard) cleanup() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	for address, state := range g.attempts {
		if now.Sub(state.first) > g.window && now.After(state.blocked) {
			delete(g.attempts, address)
		}
	}
}

// randomWebPath builds the URL prefix: /<16 random characters>/web.
func randomWebPath() (string, error) {
	const alphabet = "abcdefghijkmnopqrstuvwxyz23456789"
	raw := make([]byte, 16)
	for i := range raw {
		index, err := randIndex(len(alphabet))
		if err != nil {
			return "", err
		}
		raw[i] = alphabet[index]
	}
	return "/" + string(raw) + "/web", nil
}

// webScheme is the transport the interface is reachable with.
func webScheme(cfg Config) string {
	switch cfg.webTLSMode() {
	case webTLSSelf:
		return "https"
	case webTLSAuto:
		if cfg.Web.CertFile != "" || fileExists(cfg.statePath("tls/web.crt")) {
			return "https"
		}
	}
	return "http"
}

// webURLs lists the addresses the interface is reachable on. It is used by the
// installer, `x-node web url` and the console output of the service. A
// configured public URL comes first: behind nginx the listener is on
// 127.0.0.1, which is not the address an operator can open.
func webURLs(cfg Config) []string {
	var out []string
	add := func(url string) {
		if url == "" {
			return
		}
		for _, existing := range out {
			if existing == url {
				return
			}
		}
		out = append(out, url)
	}
	if public := strings.TrimSpace(cfg.Web.PublicURL); public != "" {
		add(public)
	}
	scheme := webScheme(cfg)
	path := cfg.Web.Path
	port := cfg.Web.Port
	suffix := ""
	if !(scheme == "https" && port == 443) && !(scheme == "http" && port == 80) {
		suffix = fmt.Sprintf(":%d", port)
	}
	addHost := func(host string) {
		if host == "" {
			return
		}
		add(fmt.Sprintf("%s://%s%s%s", scheme, host, suffix, path))
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Web.Listen)) {
	case webListenYggdrasil:
		addHost("[" + cfg.ListenAddress + "]")
	case webListenLoopback:
		addHost("127.0.0.1")
	default:
		addHost("[" + cfg.ListenAddress + "]")
		if hostname, err := os.Hostname(); err == nil && hostname != "" {
			addHost(hostname)
		}
	}
	return out
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// fingerprintShort is a stable identifier of a certificate, for the console.
func fingerprintShort(certPath string) string {
	raw, err := os.ReadFile(certPath)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:4])
}
