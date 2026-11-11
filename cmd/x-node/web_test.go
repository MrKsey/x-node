package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// newTestWebServer builds a web server with a known password and its own state
// directory. The Yggdrasil listener is never started: the tests talk to the
// handler directly.
func newTestWebServer(t *testing.T) (*webServer, Config) {
	t.Helper()
	cfg := testConfig(t)
	cfg.Web.Enabled = true
	cfg.Web.Path = "/test/web"
	cfg.Web.TLS = webTLSOff
	log := newLogger("error")
	store, err := openStore(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := openUsage(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	quotas, err := openQuotas(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	live := newSettings(cfg)
	server, err := newWebServer(cfg, cfg.statePath("config.json"), live, store, nil, usage, quotas, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := newWebCredentials("admin", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveWebCredentials(server.credsPath, creds); err != nil {
		t.Fatal(err)
	}
	if err := server.useCredentials(creds); err != nil {
		t.Fatal(err)
	}
	server.guard = newLoginGuard(3, time.Minute)
	return server, cfg
}

func testClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 5 * time.Second}
}

func doJSON(t *testing.T, client *http.Client, method, url string, body any, csrf bool) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if csrf {
		request.Header.Set("X-Requested-With", "x-node")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	out := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return response.StatusCode, out
}

func TestWebLoginAndSession(t *testing.T) {
	server, _ := newTestWebServer(t)
	ts := httptest.NewServer(server)
	defer ts.Close()
	client := testClient(t)

	code, payload := doJSON(t, client, http.MethodGet, ts.URL+"/test/web/api/session", nil, false)
	if code != http.StatusOK || payload["authenticated"] != false {
		t.Fatalf("session before login = %d %v", code, payload)
	}

	code, _ = doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/login",
		map[string]string{"username": "admin", "password": "wrong"}, true)
	if code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", code)
	}

	code, payload = doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/login",
		map[string]string{"username": "admin", "password": "correct horse battery staple"}, true)
	if code != http.StatusOK {
		t.Fatalf("login = %d %v", code, payload)
	}

	code, payload = doJSON(t, client, http.MethodGet, ts.URL+"/test/web/api/session", nil, false)
	if code != http.StatusOK || payload["authenticated"] != true {
		t.Fatalf("session after login = %d %v", code, payload)
	}

	code, _ = doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/logout", nil, true)
	if code != http.StatusOK {
		t.Fatalf("logout = %d", code)
	}
	code, payload = doJSON(t, client, http.MethodGet, ts.URL+"/test/web/api/session", nil, false)
	if payload["authenticated"] != false {
		t.Fatalf("session after logout = %v", payload)
	}
}

func TestWebUnlimitedSession(t *testing.T) {
	server, _ := newTestWebServer(t)
	server.setSessionTTL(0)
	ts := httptest.NewServer(server)
	defer ts.Close()

	request, err := http.NewRequest(http.MethodPost, ts.URL+"/test/web/api/login",
		strings.NewReader(`{"username":"admin","password":"correct horse battery staple"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Requested-With", "x-node")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", response.StatusCode)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d, want one", len(cookies))
	}
	cookie := cookies[0]
	// The cookie has to survive a browser restart: a session cookie (MaxAge 0)
	// would be dropped on close, which is exactly the "password again" the
	// setting is meant to stop.
	if cookie.MaxAge < 300*24*60*60 {
		t.Fatalf("max age = %d, want a long-lived cookie", cookie.MaxAge)
	}
	_, signer := server.credentials()
	session, ok := signer.verify(cookie.Value)
	if !ok {
		t.Fatal("the unlimited session was rejected")
	}
	if session.Expires != 0 {
		t.Fatalf("expires = %d, want no expiry", session.Expires)
	}

	// The cookie still authenticates the API.
	parsed, err := url.Parse(ts.URL + "/test/web/")
	if err != nil {
		t.Fatal(err)
	}
	client := testClient(t)
	client.Jar.SetCookies(parsed, []*http.Cookie{cookie})
	code, payload := doJSON(t, client, http.MethodGet, ts.URL+"/test/web/api/session", nil, false)
	if code != http.StatusOK || payload["authenticated"] != true {
		t.Fatalf("session = %d %v", code, payload)
	}

	// Switching back to a real lifetime governs the sessions again: a cookie
	// signed without an expiry is no longer accepted.
	server.setSessionTTL(12 * time.Hour)
	_, signer = server.credentials()
	if _, ok := signer.verify(cookie.Value); ok {
		t.Fatal("a cookie without an expiry must be rejected once a lifetime is set")
	}
}

func TestWebPasswordChangeWithoutRestart(t *testing.T) {
	server, _ := newTestWebServer(t)
	ts := httptest.NewServer(server)
	defer ts.Close()
	client := testClient(t)

	// "x-node web password" is a separate process: it only rewrites web.json.
	creds, err := loadWebCredentials(server.credsPath)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hashWebPassword("a brand new password")
	if err != nil {
		t.Fatal(err)
	}
	creds.PasswordHash = hash
	if err := saveWebCredentials(server.credsPath, creds); err != nil {
		t.Fatal(err)
	}
	// A coarse file system timestamp would hide the change from the daemon.
	next := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(server.credsPath, next, next); err != nil {
		t.Fatal(err)
	}

	code, _ := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/login",
		map[string]string{"username": "admin", "password": "correct horse battery staple"}, true)
	if code != http.StatusUnauthorized {
		t.Fatalf("old password after the change = %d", code)
	}
	code, payload := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/login",
		map[string]string{"username": "admin", "password": "a brand new password"}, true)
	if code != http.StatusOK {
		t.Fatalf("new password without a restart = %d %v", code, payload)
	}
}

func TestWebRejectsMissingCSRFHeader(t *testing.T) {
	server, _ := newTestWebServer(t)
	ts := httptest.NewServer(server)
	defer ts.Close()
	client := testClient(t)

	if code, _ := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/login",
		map[string]string{"username": "admin", "password": "correct horse battery staple"}, true); code != http.StatusOK {
		t.Fatalf("login = %d", code)
	}
	code, _ := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/clients",
		map[string]string{"address": testAddr(t, 9).String()}, false)
	if code != http.StatusForbidden {
		t.Fatalf("mutation without the header = %d", code)
	}
}

func TestWebClientAndQuota(t *testing.T) {
	server, _ := newTestWebServer(t)
	ts := httptest.NewServer(server)
	defer ts.Close()
	client := testClient(t)

	if code, _ := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/login",
		map[string]string{"username": "admin", "password": "correct horse battery staple"}, true); code != http.StatusOK {
		t.Fatalf("login failed")
	}
	address := testAddr(t, 7).String()
	code, payload := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/clients",
		map[string]string{"address": address, "label": "test phone"}, true)
	if code != http.StatusOK {
		t.Fatalf("add client = %d %v", code, payload)
	}

	code, payload = doJSON(t, client, http.MethodGet, ts.URL+"/test/web/api/clients", nil, false)
	if code != http.StatusOK {
		t.Fatalf("list clients = %d", code)
	}
	clients, _ := payload["clients"].([]any)
	if len(clients) != 1 {
		t.Fatalf("clients = %v", payload)
	}

	code, _ = doJSON(t, client, http.MethodPut, ts.URL+"/test/web/api/quotas/"+address,
		map[string]any{"monthlyBytes": 1 << 30, "dailyBytes": 1 << 28, "downKbps": 2000, "upKbps": 1000}, true)
	if code != http.StatusOK {
		t.Fatalf("set quota = %d", code)
	}
	code, payload = doJSON(t, client, http.MethodGet, ts.URL+"/test/web/api/quotas", nil, false)
	if code != http.StatusOK {
		t.Fatalf("list quotas = %d", code)
	}
	states, _ := payload["clients"].([]any)
	if len(states) != 1 {
		t.Fatalf("quotas = %v", payload)
	}
	state, _ := states[0].(map[string]any)
	if state["monthlyBytes"].(float64) != float64(1<<30) || state["downKbps"].(float64) != 2000 {
		t.Fatalf("quota = %v", state)
	}

	code, _ = doJSON(t, client, http.MethodDelete, ts.URL+"/test/web/api/quotas/"+address, nil, true)
	if code != http.StatusOK {
		t.Fatalf("reset quota = %d", code)
	}
}

func TestWebClientLink(t *testing.T) {
	server, cfg := newTestWebServer(t)
	ts := httptest.NewServer(server)
	defer ts.Close()
	client := testClient(t)

	if code, _ := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/login",
		map[string]string{"username": "admin", "password": "correct horse battery staple"}, true); code != http.StatusOK {
		t.Fatalf("login failed")
	}
	address := testAddr(t, 9).String()
	if code, payload := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/clients",
		map[string]string{"address": address, "label": "tablet"}, true); code != http.StatusOK {
		t.Fatalf("add client = %d %v", code, payload)
	}

	code, payload := doJSON(t, client, http.MethodGet,
		ts.URL+"/test/web/api/clients/"+address+"/link", nil, false)
	if code != http.StatusOK {
		t.Fatalf("client link = %d %v", code, payload)
	}
	link, _ := payload["link"].(string)
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("link %q: %v", link, err)
	}
	query := parsed.Query()
	if query.Get("a") != cfg.ListenAddress || query.Get("i") != "" {
		t.Fatalf("link = %q", link)
	}
	qr, _ := payload["qr"].(string)
	if !strings.HasPrefix(qr, "data:image/png;base64,") {
		t.Fatalf("qr = %q", qr)
	}
	if payload["label"] != "tablet" {
		t.Fatalf("label = %v", payload["label"])
	}

	code, _ = doJSON(t, client, http.MethodGet,
		ts.URL+"/test/web/api/clients/"+testAddr(t, 10).String()+"/link", nil, false)
	if code != http.StatusNotFound {
		t.Fatalf("link of an unknown client = %d", code)
	}
}

// Removing a client must take it out of the allowlist, close what it has open
// and drop its live row. The row used to survive in the runtime document and
// the dashboard kept listing the deleted client as an "open" one.
func TestWebClientRemove(t *testing.T) {
	server, cfg := newTestWebServer(t)
	server.conns = newConnectionRegistry()
	server.tracker = newRuntimeTracker(cfg, newLogger("error"))
	ts := httptest.NewServer(server)
	defer ts.Close()
	client := testClient(t)

	if code, _ := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/login",
		map[string]string{"username": "admin", "password": "correct horse battery staple"}, true); code != http.StatusOK {
		t.Fatalf("login failed")
	}
	address := testAddr(t, 9).String()
	if code, payload := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/clients",
		map[string]string{"address": address}, true); code != http.StatusOK {
		t.Fatalf("add client = %d %v", code, payload)
	}
	ip := net.ParseIP(address)
	server.tracker.touch(ip)

	local, remote := net.Pipe()
	defer remote.Close()
	server.conns.track(ip, local)

	code, payload := doJSON(t, client, http.MethodDelete,
		ts.URL+"/test/web/api/clients/"+address, nil, true)
	if code != http.StatusOK || payload["closed"].(float64) != 1 {
		t.Fatalf("remove = %d %v", code, payload)
	}
	_ = remote.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := remote.Read(make([]byte, 1)); err == nil {
		t.Fatal("the connection of a removed client is still open")
	}
	if _, ok := server.store.Get(ip); ok {
		t.Fatal("the client is still in the allowlist")
	}
	if live := server.tracker.live(ip); live.Connections != 0 || !live.LastSeenAt.IsZero() {
		t.Fatalf("the live row survived: %+v", live)
	}

	// The list is built from the store and the runtime document, so the row
	// must be gone from the answer even before the next service restart.
	_, payload = doJSON(t, client, http.MethodGet, ts.URL+"/test/web/api/clients", nil, false)
	if clients, _ := payload["clients"].([]any); len(clients) != 0 {
		t.Fatalf("clients after remove = %v", clients)
	}

	if code, _ := doJSON(t, client, http.MethodDelete,
		ts.URL+"/test/web/api/clients/"+address, nil, true); code != http.StatusNotFound {
		t.Fatalf("removing an unknown client = %d, want 404", code)
	}
}

func TestWebClientSuspensionAndDisconnect(t *testing.T) {
	server, _ := newTestWebServer(t)
	server.conns = newConnectionRegistry()
	ts := httptest.NewServer(server)
	defer ts.Close()
	client := testClient(t)

	if code, _ := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/login",
		map[string]string{"username": "admin", "password": "correct horse battery staple"}, true); code != http.StatusOK {
		t.Fatalf("login failed")
	}
	address := testAddr(t, 8).String()
	if code, payload := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/clients",
		map[string]string{"address": address}, true); code != http.StatusOK {
		t.Fatalf("add client = %d %v", code, payload)
	}
	ip := net.ParseIP(address)

	// A tracked socket stands in for a live tunnel; Disconnect must close it.
	local, remote := net.Pipe()
	defer remote.Close()
	release := server.conns.track(ip, local)
	defer release()
	code, payload := doJSON(t, client, http.MethodPost,
		ts.URL+"/test/web/api/clients/"+address+"/disconnect", nil, true)
	if code != http.StatusOK || payload["closed"].(float64) != 1 {
		t.Fatalf("disconnect = %d %v", code, payload)
	}
	_ = remote.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := remote.Read(make([]byte, 1)); err == nil {
		t.Fatal("the tracked connection is still open")
	}

	code, payload = doJSON(t, client, http.MethodPost,
		ts.URL+"/test/web/api/clients/"+address+"/suspend",
		map[string]any{"suspended": true}, true)
	if code != http.StatusOK {
		t.Fatalf("suspend = %d %v", code, payload)
	}
	stored, ok := server.store.Get(ip)
	if !ok || !stored.Suspended || stored.SuspendedAt.IsZero() {
		t.Fatalf("stored suspension = %+v", stored)
	}
	_, payload = doJSON(t, client, http.MethodGet, ts.URL+"/test/web/api/clients", nil, false)
	clients, _ := payload["clients"].([]any)
	row, _ := clients[0].(map[string]any)
	if row["suspended"] != true || row["state"] != "suspended" {
		t.Fatalf("suspended client row = %v", row)
	}

	code, payload = doJSON(t, client, http.MethodPost,
		ts.URL+"/test/web/api/clients/"+address+"/suspend",
		map[string]any{"suspended": false}, true)
	if code != http.StatusOK {
		t.Fatalf("resume = %d %v", code, payload)
	}
	stored, _ = server.store.Get(ip)
	if stored.Suspended || !stored.SuspendedAt.IsZero() {
		t.Fatalf("resumed client = %+v", stored)
	}
}

// The Clients page applies the default quota to a client without its own
// entry. A stale explicit entry used to override the defaults forever, even
// after the client was removed and registered again, which is why setting a
// speed limit in Default quota looked like it did nothing.
func TestWebDefaultQuotaAndRemoveClearsExplicit(t *testing.T) {
	server, _ := newTestWebServer(t)
	ts := httptest.NewServer(server)
	defer ts.Close()
	client := testClient(t)

	if code, _ := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/login",
		map[string]string{"username": "admin", "password": "correct horse battery staple"}, true); code != http.StatusOK {
		t.Fatalf("login failed")
	}
	code, payload := doJSON(t, client, http.MethodPut, ts.URL+"/test/web/api/settings",
		map[string]any{"quotas": map[string]any{
			"monthlyBytes": 0, "dailyBytes": 0, "downKbps": 20000, "upKbps": 20000,
		}}, true)
	if code != http.StatusOK {
		t.Fatalf("settings = %d %v", code, payload)
	}

	address := testAddr(t, 31).String()
	add := func() {
		t.Helper()
		if code, payload := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/clients",
			map[string]string{"address": address}, true); code != http.StatusOK {
			t.Fatalf("add client = %d %v", code, payload)
		}
	}
	quotaOf := func() map[string]any {
		t.Helper()
		_, payload := doJSON(t, client, http.MethodGet, ts.URL+"/test/web/api/clients", nil, false)
		clients, _ := payload["clients"].([]any)
		if len(clients) != 1 {
			t.Fatalf("clients = %v", clients)
		}
		row := clients[0].(map[string]any)
		quota, _ := row["quota"].(map[string]any)
		return quota
	}

	add()
	if quota := quotaOf(); quota["downKbps"].(float64) != 20000 || quota["upKbps"].(float64) != 20000 {
		t.Fatalf("defaults did not reach a new client: %v", quota)
	}

	// An explicit entry, even an all-zero one, wins over the defaults. This is
	// what the client editor used to write when only the label was changed.
	if code, payload := doJSON(t, client, http.MethodPut, ts.URL+"/test/web/api/quotas/"+address,
		map[string]any{"monthlyBytes": 0, "dailyBytes": 0, "downKbps": 0, "upKbps": 0}, true); code != http.StatusOK {
		t.Fatalf("quota = %d %v", code, payload)
	}
	if quota := quotaOf(); quota["downKbps"].(float64) != 0 {
		t.Fatalf("an explicit quota must win: %v", quota)
	}

	// Removing the client drops that entry with it.
	if code, payload := doJSON(t, client, http.MethodDelete,
		ts.URL+"/test/web/api/clients/"+address, nil, true); code != http.StatusOK {
		t.Fatalf("remove = %d %v", code, payload)
	}
	add()
	if quota := quotaOf(); quota["downKbps"].(float64) != 20000 || quota["upKbps"].(float64) != 20000 {
		t.Fatalf("a re-added client inherited the old explicit quota: %v", quota)
	}
}

func TestWebSettingsPatch(t *testing.T) {
	server, cfg := newTestWebServer(t)
	ts := httptest.NewServer(server)
	defer ts.Close()
	client := testClient(t)

	if code, _ := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/login",
		map[string]string{"username": "admin", "password": "correct horse battery staple"}, true); code != http.StatusOK {
		t.Fatalf("login failed")
	}
	code, payload := doJSON(t, client, http.MethodPut, ts.URL+"/test/web/api/settings",
		map[string]any{"maxConnsPerClient": 42, "logLevel": "warn",
			"web": map[string]any{"compact": true}}, true)
	if code != http.StatusOK {
		t.Fatalf("settings = %d %v", code, payload)
	}
	if got := server.live.get().MaxConnsPerClient; got != 42 {
		t.Fatalf("maxConnsPerClient = %d", got)
	}
	if !server.live.get().Web.Compact {
		t.Fatal("the compact layout was not saved")
	}
	merged, err := loadSettingsOverlay(cfg)
	if err != nil {
		t.Fatalf("overlay was not written: %v", err)
	}
	if merged.MaxConnsPerClient != 42 || merged.LogLevel != "warn" || !merged.Web.Compact {
		t.Fatalf("overlay = %+v", merged)
	}
}

func TestWebLoginLockout(t *testing.T) {
	server, _ := newTestWebServer(t)
	ts := httptest.NewServer(server)
	defer ts.Close()
	client := testClient(t)

	for i := 0; i < 3; i++ {
		code, _ := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/login",
			map[string]string{"username": "admin", "password": "nope"}, true)
		if code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d", i, code)
		}
	}
	code, payload := doJSON(t, client, http.MethodPost, ts.URL+"/test/web/api/login",
		map[string]string{"username": "admin", "password": "correct horse battery staple"}, true)
	if code != http.StatusTooManyRequests {
		t.Fatalf("blocked attempt = %d %v", code, payload)
	}
}

func TestWebStaticAssets(t *testing.T) {
	server, _ := newTestWebServer(t)
	ts := httptest.NewServer(server)
	defer ts.Close()

	response, err := http.Get(ts.URL + "/test/web/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "X-Node") {
		t.Fatalf("index = %d", response.StatusCode)
	}
	response, err = http.Get(ts.URL + "/test/web/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("css = %d", response.StatusCode)
	}
}

// TestWebSelfSignedCertificate checks the certificate the interface creates
// when no real one is configured: it must cover the Yggdrasil address and the
// loopback, and a second start must reuse it instead of generating a new one.
func TestWebSelfSignedCertificate(t *testing.T) {
	cfg := testConfig(t)
	cfg.Web.TLS = webTLSSelf
	certFile, keyFile := webCertPaths(cfg)

	if err := ensureSelfSignedCertificate(certFile, keyFile, cfg.ListenAddress); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	want := net.ParseIP(cfg.ListenAddress)
	found := false
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(want) {
			found = true
		}
	}
	if !found {
		t.Fatalf("certificate IPs = %v, want %s", leaf.IPAddresses, cfg.ListenAddress)
	}
	if leaf.NotAfter.Before(time.Now().Add(365 * 24 * time.Hour)) {
		t.Fatalf("certificate expires too soon: %s", leaf.NotAfter)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("extended key usage = %v", leaf.ExtKeyUsage)
	}

	before, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	// The service restarts often; a new certificate on every start would teach
	// the browser a new exception each time.
	server := &webServer{log: newLogger("error")}
	if _, err := server.certificate(cfg); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the certificate was regenerated although it exists")
	}
}

func TestWebURLsFollowTheListener(t *testing.T) {
	cfg := testConfig(t)
	cfg.Web.TLS = webTLSOff
	cfg.Web.Port = 8443
	cfg.Web.Path = "/abc/web"

	cfg.Web.Listen = webListenYggdrasil
	urls := webURLs(cfg)
	if len(urls) != 1 || urls[0] != "http://["+cfg.ListenAddress+"]:8443/abc/web" {
		t.Fatalf("yggdrasil urls = %v", urls)
	}

	cfg.Web.Listen = webListenLoopback
	urls = webURLs(cfg)
	if len(urls) != 1 || urls[0] != "http://127.0.0.1:8443/abc/web" {
		t.Fatalf("loopback urls = %v", urls)
	}

	// With a certificate in the state directory "auto" switches to HTTPS, and
	// the default port disappears from the URL.
	cfg.Web.TLS = webTLSAuto
	cfg.Web.Listen = webListenYggdrasil
	cfg.Web.Port = 443
	certFile, keyFile := webCertPaths(cfg)
	if err := ensureSelfSignedCertificate(certFile, keyFile, cfg.ListenAddress); err != nil {
		t.Fatal(err)
	}
	if scheme := webScheme(cfg); scheme != "https" {
		t.Fatalf("scheme = %q, want https", scheme)
	}
	urls = webURLs(cfg)
	if len(urls) != 1 || urls[0] != "https://["+cfg.ListenAddress+"]/abc/web" {
		t.Fatalf("https urls = %v", urls)
	}
}

func TestDNSWebPathIsTheSiblingOfTheDashboard(t *testing.T) {
	cases := map[string]string{
		"/rdnoytarahru4psv/web": "/rdnoytarahru4psv/dns",
		"/x-node/web":           "/x-node/dns",
		"/web":                  "/dns",
		"":                      "/x-node/dns",
	}
	for path, want := range cases {
		cfg := Config{}
		cfg.Web.Path = path
		if got := cfg.dnsWebPath(); got != want {
			t.Errorf("dnsWebPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestDNSWebTargetDialsAWildcardThroughLoopback(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:8082":    "127.0.0.1:8082",
		"[::]:8082":       "[::1]:8082",
		"127.0.0.1:8082":  "127.0.0.1:8082",
		"[200::1:2]:8082": "[200::1:2]:8082",
	}
	for listen, want := range cases {
		cfg := Config{}
		cfg.DNS.WebListen = listen
		if got := cfg.dnsWebTarget(); got != want {
			t.Errorf("dnsWebTarget(%q) = %q, want %q", listen, got, want)
		}
	}
	cfg := Config{}
	cfg.DNS.WebListen = "not-a-host-port"
	if got := cfg.dnsWebTarget(); got != "" {
		t.Fatalf("an unusable listener returned %q", got)
	}
}

func TestDNSWebListenDefaultsToLoopback(t *testing.T) {
	cfg := Config{ListenAddress: "200::1", DNS: DNSConfig{WebPort: 8082}}
	if got := cfg.dnsWebListen(); got != "127.0.0.1:8082" {
		t.Fatalf("dnsWebListen() = %q", got)
	}
	// The old default bound the panel to the Yggdrasil address; an upgrade
	// moves it to loopback.
	cfg.DNS.WebListen = "[200::1]:8082"
	cfg.applyDefaults()
	if got := cfg.DNS.WebListen; got != "127.0.0.1:8082" {
		t.Fatalf("migrated webListen = %q", got)
	}
	// An explicit listener is kept.
	cfg.DNS.WebListen = "0.0.0.0:8082"
	cfg.applyDefaults()
	if got := cfg.DNS.WebListen; got != "0.0.0.0:8082" {
		t.Fatalf("explicit webListen = %q", got)
	}
}

func TestDNSPanelURLUsesTheDashboardEndpoint(t *testing.T) {
	cfg := Config{ListenAddress: "200::1"}
	cfg.Web.Path = "/abc/web"
	cfg.Web.Port = 443
	cfg.Web.TLS = webTLSSelf
	cfg.Web.Listen = webListenYggdrasil
	cfg.DNS.WebPort = 8082
	if got := cfg.dnsPanelURL(); got != "https://[200::1]/abc/dns/" {
		t.Fatalf("dnsPanelURL() = %q", got)
	}
	// A public URL typed with a trailing slash must not repeat the dashboard
	// path: /abc/web/ + /abc/dns/ used to become /abc/web/abc/dns/.
	cfg.Web.PublicURL = "https://example.org/abc/web/"
	if got := cfg.dnsPanelURL(); got != "https://example.org/abc/dns/" {
		t.Fatalf("dnsPanelURL() with a trailing slash = %q", got)
	}
	cfg.Web.PublicURL = "https://example.org/abc/web"
	if got := cfg.dnsPanelURL(); got != "https://example.org/abc/dns/" {
		t.Fatalf("dnsPanelURL() without a trailing slash = %q", got)
	}
	// A public URL on a bare origin has no dashboard path to strip.
	cfg.Web.PublicURL = "https://example.org/"
	if got := cfg.dnsPanelURL(); got != "https://example.org/abc/dns/" {
		t.Fatalf("dnsPanelURL() on a bare origin = %q", got)
	}
}

func TestRewriteAlfisResponsePrefixesAbsoluteURLs(t *testing.T) {
	html := `<link href="/styles.css"><script src="/scripts.js"></script>`
	response := &http.Response{
		Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}},
		Body:   io.NopCloser(strings.NewReader(html)),
	}
	if err := rewriteAlfisResponse(response, "/abc/dns"); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `href="/abc/dns/styles.css"`) {
		t.Fatalf("stylesheet path was not rewritten: %s", body)
	}
	if !strings.Contains(string(body), `src="/abc/dns/scripts.js"`) {
		t.Fatalf("script path was not rewritten: %s", body)
	}
	if response.ContentLength != int64(len(body)) {
		t.Fatalf("content length = %d, want %d", response.ContentLength, len(body))
	}

	script := `fetch('/api/status'); new EventSource("/api/events");`
	response = &http.Response{
		Header: http.Header{"Content-Type": {"application/javascript; charset=utf-8"}},
		Body:   io.NopCloser(strings.NewReader(script)),
	}
	if err := rewriteAlfisResponse(response, "/abc/dns"); err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	if !strings.Contains(string(body), `'/abc/dns/api/status'`) {
		t.Fatalf("single-quoted API path was not rewritten: %s", body)
	}
	if !strings.Contains(string(body), `"/abc/dns/api/events"`) {
		t.Fatalf("double-quoted API path was not rewritten: %s", body)
	}
}

func TestRewriteAlfisResponseLeavesTheEventStreamAlone(t *testing.T) {
	stream := "data: {\"ok\":true}\n\n"
	response := &http.Response{
		Header: http.Header{"Content-Type": {"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(stream)),
	}
	if err := rewriteAlfisResponse(response, "/abc/dns"); err != nil {
		t.Fatal(err)
	}
	// The body is not read for an event stream: reading it would block until
	// the connection closes.
	body, _ := io.ReadAll(response.Body)
	if string(body) != stream {
		t.Fatalf("event stream changed: %q", body)
	}
}

func TestDNSProxyServesThePanelUnderBothPrefixes(t *testing.T) {
	panel := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = rw.Write([]byte(`<link href="/styles.css"><script src="/scripts.js"></script>`))
		case "/api/status":
			rw.Header().Set("Content-Type", "application/json")
			_, _ = rw.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(rw, r)
		}
	}))
	defer panel.Close()

	// The panel is offered only when the installed binary is a webui build.
	// The probe reads the file, so a marker file is enough for the test.
	dir := t.TempDir()
	name := "alfis"
	if runtime.GOOS == "windows" {
		// LookPath on Windows only accepts names from PATHEXT.
		name = "alfis.exe"
	}
	binary := filepath.Join(dir, name)
	if err := os.WriteFile(binary, []byte("alfis_session"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	server, cfg := newTestWebServer(t)
	cfg.Web.Path = "/abc/web"
	cfg.DNS.Enabled = true
	cfg.DNS.WebListen = strings.TrimPrefix(panel.URL, "http://")
	server.live.set(cfg)

	for _, path := range []string{"/abc/dns/", "/abc/web/dns/"} {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", path, recorder.Code, recorder.Body.String())
		}
		// Assets keep the prefix the browser used, so both paths stay usable.
		base := strings.TrimSuffix(path, "/")
		if body := recorder.Body.String(); !strings.Contains(body, `href="`+base+`/styles.css"`) {
			t.Fatalf("%s was not rewritten: %s", path, body)
		}
	}

	// An API call reaches the panel without the prefix.
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/abc/dns/api/status", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"ok":true}` {
		t.Fatalf("api = %d %s", recorder.Code, recorder.Body.String())
	}
}
