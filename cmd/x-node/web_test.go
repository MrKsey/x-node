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
	"os"
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
		map[string]any{"maxConnsPerClient": 42, "logLevel": "warn"}, true)
	if code != http.StatusOK {
		t.Fatalf("settings = %d %v", code, payload)
	}
	if got := server.live.get().MaxConnsPerClient; got != 42 {
		t.Fatalf("maxConnsPerClient = %d", got)
	}
	merged, err := loadSettingsOverlay(cfg)
	if err != nil {
		t.Fatalf("overlay was not written: %v", err)
	}
	if merged.MaxConnsPerClient != 42 || merged.LogLevel != "warn" {
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
