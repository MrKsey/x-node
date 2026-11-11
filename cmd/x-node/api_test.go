package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAPIRegisterAndStatus(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIServer(cfg, store, nil, newLogger("error"))
	source := testAddr(t, 9)
	token, _, err := store.NewInvite("api", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(registerRequest{
		Invite:     token,
		Address:    source.String(),
		DeviceName: "phone",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/register", bytes.NewReader(body))
	req.RemoteAddr = "[" + source.String() + "]:40000"
	rec := httptest.NewRecorder()
	api.handleRegister(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register status = %d, body %s", rec.Code, rec.Body.String())
	}
	var registered registerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	if !registered.OK || !registered.Allowlisted {
		t.Fatalf("unexpected response %+v", registered)
	}
	if registered.Token == "" {
		t.Fatal("registration did not return an API token")
	}
	if !store.TokenValid(source, registered.Token) {
		t.Fatal("the returned token is not valid for the registered address")
	}
	if registered.Auth != apiAuthRequired {
		t.Fatalf("auth mode = %q, want required", registered.Auth)
	}
	if !store.Allowed(source) {
		t.Fatal("the source address was not added to the allowlist")
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	statusReq.RemoteAddr = "[" + source.String() + "]:40000"
	statusReq.Header.Set("Authorization", "Bearer "+registered.Token)
	statusRec := httptest.NewRecorder()
	api.handleStatus(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status code = %d", statusRec.Code)
	}
	var status statusResponse
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Allowlisted == nil || !*status.Allowlisted {
		t.Fatal("status does not report the client as allowlisted")
	}
}

// registerAt performs one /v1/register request from the given source and
// decodes the answer.
func registerAt(t *testing.T, api *apiServer, source net.IP, invite, device string) registerResponse {
	t.Helper()
	body, _ := json.Marshal(registerRequest{
		Invite:     invite,
		Address:    source.String(),
		DeviceName: device,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/register", bytes.NewReader(body))
	req.RemoteAddr = "[" + source.String() + "]:40000"
	rec := httptest.NewRecorder()
	api.handleRegister(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register status = %d, body %s", rec.Code, rec.Body.String())
	}
	var response registerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode register: %v (body %s)", err, rec.Body.String())
	}
	return response
}

// The app registers on every connect to refresh its device name and version.
// That call used to rotate the API token, so the credential in the profile
// changed on every reconnect and a token issued by the operator in the
// dashboard was overwritten at once.
func TestAPIRegistrationKeepsTheIssuedToken(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIServer(cfg, store, nil, newLogger("error"))
	source := testAddr(t, 13)
	invite, _, err := store.NewInvite("", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}

	first := registerAt(t, api, source, invite, "phone")
	if first.Token == "" {
		t.Fatal("the first registration did not issue a token")
	}
	if !store.TokenValid(source, first.Token) {
		t.Fatal("the issued token is not valid")
	}

	// The operator presses New token in the dashboard.
	manual, err := store.IssueToken(source)
	if err != nil {
		t.Fatal(err)
	}

	// The app reconnects and registers again with an empty invite.
	again := registerAt(t, api, source, "", "phone")
	if again.Token != "" {
		t.Fatalf("re-registration returned a token %q, want none", again.Token)
	}
	if again.Registered {
		t.Fatal("re-registration reported a new client")
	}
	if !store.TokenValid(source, manual) {
		t.Fatal("re-registration replaced the token issued by the operator")
	}
	if store.TokenValid(source, first.Token) {
		t.Fatal("the first token survived the manual rotation")
	}

	// A client added by hand in the dashboard has no token yet, so the next
	// registration is what gives it one.
	manualClient := testAddr(t, 14)
	if _, _, err := store.AddClient(manualClient, "laptop"); err != nil {
		t.Fatal(err)
	}
	issued := registerAt(t, api, manualClient, "", "laptop")
	if issued.Token == "" {
		t.Fatal("a client without a token did not get one at registration")
	}
	if !store.TokenValid(manualClient, issued.Token) {
		t.Fatal("the token issued to a client without one is not valid")
	}
}

func TestAPIRejectsWrongAddress(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIServer(cfg, store, nil, newLogger("error"))
	source := testAddr(t, 10)
	other := testAddr(t, 11)
	token, _, err := store.NewInvite("", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(registerRequest{Invite: token, Address: other.String()})
	req := httptest.NewRequest(http.MethodPost, "/v1/register", bytes.NewReader(body))
	req.RemoteAddr = "[" + source.String() + "]:40000"
	rec := httptest.NewRecorder()
	api.handleRegister(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if store.Allowed(other) {
		t.Fatal("the claimed address must not be registered")
	}
}

func TestAPIRejectsNonYggdrasilSource(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIServer(cfg, store, nil, newLogger("error"))
	req := httptest.NewRequest(http.MethodPost, "/v1/register", bytes.NewReader([]byte(`{}`)))
	req.RemoteAddr = "203.0.113.5:40000"
	rec := httptest.NewRecorder()
	api.handleRegister(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAPIStatusReportsLoadAndRefusals(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxConnsPerClient = 128
	cfg.MaxUDPAssociations = 64
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	tracker := newRuntimeTracker(cfg, newLogger("error"))
	api := newAPIServer(cfg, store, tracker, newLogger("error"))
	source := testAddr(t, 13)
	if _, _, err := store.AddClient(source, "phone"); err != nil {
		t.Fatal(err)
	}
	tracker.open(source, "1.1.1.1:443")
	tracker.open(source, "1.1.1.1:443")
	tracker.refused(source)
	tracker.udpOpen(source)
	token, err := store.IssueToken(source)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.RemoteAddr = "[" + source.String() + "]:40000"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	api.handleStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	var status statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Connections != 2 {
		t.Fatalf("connections = %d, want 2", status.Connections)
	}
	if status.Refused != 1 {
		t.Fatalf("refused = %d, want 1", status.Refused)
	}
	if status.UDPAssociations != 1 {
		t.Fatalf("udp associations = %d, want 1", status.UDPAssociations)
	}
	if status.MaxConns != 128 || status.MaxUDPAssociations != 64 {
		t.Fatalf("limits = %d/%d, want 128/64", status.MaxConns, status.MaxUDPAssociations)
	}
	tracker.shutdown()
}

func TestAPIRegistrationDisabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.Registration = false
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIServer(cfg, store, nil, newLogger("error"))
	source := testAddr(t, 12)
	token, _, err := store.NewInvite("", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(registerRequest{Invite: token, Address: source.String()})
	req := httptest.NewRequest(http.MethodPost, "/v1/register", bytes.NewReader(body))
	req.RemoteAddr = "[" + source.String() + "]:40000"
	rec := httptest.NewRecorder()
	api.handleRegister(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// A health check from the exit itself, or a probe from a stranger, reaches the
// shared listener as an ordinary HTTP request. Neither may create a row in
// `x-node clients`: the row would look like an open client that was never
// registered, which is what `doctor` used to leave behind.
func TestHTTPOnSharedPortDoesNotTrackStrangers(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	tracker := newRuntimeTracker(cfg, newLogger("error"))
	defer tracker.shutdown()
	server := newSocksServer(cfg, store, tracker, newLogger("error"), nil)

	server.trackSource(testAddr(t, 21))
	if rows := tracker.snapshotLocked().Clients; len(rows) != 0 {
		t.Fatalf("a stranger was recorded as a client: %+v", rows)
	}

	client := testAddr(t, 22)
	if _, _, err := store.AddClient(client, "phone"); err != nil {
		t.Fatal(err)
	}
	server.trackSource(client)
	rows := tracker.snapshotLocked().Clients
	if len(rows) != 1 || rows[0].Address != client.String() {
		t.Fatalf("an allowlisted client was not recorded: %+v", rows)
	}
}

// statusOf performs one /v1/status request and decodes the answer.
func statusOf(t *testing.T, api *apiServer, source net.IP, token string) (int, statusResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.RemoteAddr = "[" + source.String() + "]:40000"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	api.handleStatus(rec, req)
	var status statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v (body %s)", err, rec.Body.String())
	}
	return rec.Code, status
}

// Without a token the status must say only that a token is required. Nothing
// about the exit, its clients or their limits is published.
func TestAPIStatusWithoutTokenIsMinimal(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxConnsPerClient = 128
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIServer(cfg, store, nil, newLogger("error"))
	source := testAddr(t, 31)
	if _, _, err := store.AddClient(source, "phone"); err != nil {
		t.Fatal(err)
	}

	code, status := statusOf(t, api, source, "")
	if code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", code)
	}
	if !status.OK || status.Authenticated {
		t.Fatalf("ok=%v authenticated=%v, want true/false", status.OK, status.Authenticated)
	}
	if status.Auth != apiAuthRequired {
		t.Fatalf("auth = %q, want required", status.Auth)
	}
	if status.Allowlisted != nil || status.Open {
		t.Fatalf("allowlist leaked without a token: allowlisted=%v open=%v", status.Allowlisted, status.Open)
	}
	if status.Address != "" || status.Label != "" || status.MaxConns != 0 {
		t.Fatalf("private fields leaked without a token: %+v", status)
	}
	if status.ExitName != "" || status.Version != "" || status.Capabilities != nil {
		t.Fatalf("exit details leaked without a token: %+v", status)
	}
	if status.Connections != 0 || status.Refused != 0 || status.UDPAssociations != 0 {
		t.Fatalf("counters leaked without a token: %+v", status)
	}
}

func TestAPIStatusWithTokenIsFull(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxConnsPerClient = 128
	cfg.MaxUDPAssociations = 64
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	tracker := newRuntimeTracker(cfg, newLogger("error"))
	defer tracker.shutdown()
	api := newAPIServer(cfg, store, tracker, newLogger("error"))
	source := testAddr(t, 32)
	if _, _, err := store.AddClient(source, "phone"); err != nil {
		t.Fatal(err)
	}
	tracker.open(source, "1.1.1.1:443")
	tracker.refused(source)
	token, err := store.IssueToken(source)
	if err != nil {
		t.Fatal(err)
	}

	code, status := statusOf(t, api, source, token)
	if code != http.StatusOK {
		t.Fatalf("status code = %d", code)
	}
	if !status.Authenticated {
		t.Fatal("a valid token was not accepted")
	}
	if status.Address != source.String() || status.Label != "phone" {
		t.Fatalf("address/label = %q/%q", status.Address, status.Label)
	}
	if status.MaxConns != 128 || status.MaxUDPAssociations != 64 {
		t.Fatalf("limits = %d/%d", status.MaxConns, status.MaxUDPAssociations)
	}
	if status.Connections != 1 || status.Refused != 1 {
		t.Fatalf("counters = %d/%d", status.Connections, status.Refused)
	}
}

func TestAPIStatusRejectsWrongToken(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIServer(cfg, store, nil, newLogger("error"))
	source := testAddr(t, 33)
	if _, _, err := store.AddClient(source, "phone"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.IssueToken(source); err != nil {
		t.Fatal(err)
	}

	_, status := statusOf(t, api, source, "not-the-token")
	if status.Authenticated {
		t.Fatal("a wrong token was accepted")
	}
	if status.Address != "" || status.Label != "" {
		t.Fatalf("a wrong token revealed private fields: %+v", status)
	}
}

// A token is bound to the address it was issued to: another node that somehow
// learned it must not read the counters of its owner.
func TestAPIStatusTokenIsBoundToAddress(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIServer(cfg, store, nil, newLogger("error"))
	owner := testAddr(t, 34)
	other := testAddr(t, 35)
	for _, ip := range []net.IP{owner, other} {
		if _, _, err := store.AddClient(ip, "phone"); err != nil {
			t.Fatal(err)
		}
	}
	token, err := store.IssueToken(owner)
	if err != nil {
		t.Fatal(err)
	}

	_, status := statusOf(t, api, other, token)
	if status.Authenticated {
		t.Fatal("a token worked from another address")
	}
}

func tokenOf(t *testing.T, api *apiServer, source net.IP) (int, tokenResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/token", nil)
	req.RemoteAddr = "[" + source.String() + "]:40000"
	rec := httptest.NewRecorder()
	api.handleToken(rec, req)
	var response tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode token: %v (body %s)", err, rec.Body.String())
	}
	return rec.Code, response
}

func TestAPITokenEndpointIssuesToken(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIServer(cfg, store, nil, newLogger("error"))
	source := testAddr(t, 36)
	if _, _, err := store.AddClient(source, "phone"); err != nil {
		t.Fatal(err)
	}

	code, response := tokenOf(t, api, source)
	if code != http.StatusOK || !response.OK || response.Token == "" {
		t.Fatalf("token response = %d %+v", code, response)
	}
	if !store.TokenValid(source, response.Token) {
		t.Fatal("the issued token is not valid")
	}

	// A second issue rotates the credential: the old one must stop working.
	first := response.Token
	_, second := tokenOf(t, api, source)
	if second.Token == "" || second.Token == first {
		t.Fatalf("the token was not rotated: %q -> %q", first, second.Token)
	}
	if store.TokenValid(source, first) {
		t.Fatal("the previous token still works after a rotation")
	}
}

func TestAPITokenEndpointRejectsStranger(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIServer(cfg, store, nil, newLogger("error"))
	code, response := tokenOf(t, api, testAddr(t, 37))
	if code != http.StatusForbidden || response.Token != "" {
		t.Fatalf("a stranger got a token: %d %+v", code, response)
	}
}

func TestAPITokenEndpointRequiresPost(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIServer(cfg, store, nil, newLogger("error"))
	req := httptest.NewRequest(http.MethodGet, "/v1/token", nil)
	req.RemoteAddr = "[" + testAddr(t, 38).String() + "]:40000"
	rec := httptest.NewRecorder()
	api.handleToken(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/token = %d, want 405", rec.Code)
	}
}

// Optional mode is the migration window: the full status stays public, but a
// token is still accepted and reported as valid.
func TestAPIStatusOptionalModeStaysPublic(t *testing.T) {
	cfg := testConfig(t)
	cfg.APIToken = apiAuthOptional
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIServer(cfg, store, nil, newLogger("error"))
	source := testAddr(t, 39)
	if _, _, err := store.AddClient(source, "phone"); err != nil {
		t.Fatal(err)
	}

	_, status := statusOf(t, api, source, "")
	if !status.Authenticated || status.Address != source.String() {
		t.Fatalf("optional mode did not stay public: %+v", status)
	}
	if status.Auth != apiAuthOptional {
		t.Fatalf("auth = %q, want optional", status.Auth)
	}
}

// Open mode has no allowlist to bind a token to; the client must be told that
// no token is needed instead of retrying on every poll.
func TestAPITokenEndpointInOpenMode(t *testing.T) {
	cfg := testConfig(t)
	cfg.AllowAll = true
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIServer(cfg, store, nil, newLogger("error"))
	code, response := tokenOf(t, api, testAddr(t, 40))
	if code != http.StatusOK || !response.Open || response.Token != "" {
		t.Fatalf("open mode token response = %d %+v", code, response)
	}
	if cfg.tokenRequired() {
		t.Fatal("open mode must not require a token")
	}
}

func TestAPIRateLimitPerSource(t *testing.T) {
	cfg := testConfig(t)
	cfg.APIRateLimit = 2
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIServer(cfg, store, nil, newLogger("error"))
	handler := api.handler()
	source := testAddr(t, 41)
	if _, _, err := store.AddClient(source, "phone"); err != nil {
		t.Fatal(err)
	}

	call := func(ip net.IP) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
		req.RemoteAddr = "[" + ip.String() + "]:40000"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < 2; i++ {
		if code := call(source); code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, code)
		}
	}
	if code := call(source); code != http.StatusTooManyRequests {
		t.Fatalf("the third request = %d, want 429", code)
	}
	if code := call(testAddr(t, 42)); code != http.StatusOK {
		t.Fatalf("another source was limited too: %d", code)
	}
}

// The IPv6 capability is a property of the host: an exit with IPv6 turned off
// must tell the client so instead of advertising a path it will not carry.
func TestAPICapabilitiesReportsIPv6Off(t *testing.T) {
	cfg := testConfig(t)
	cfg.IPv6 = ipv6Off
	api := newAPIServer(cfg, nil, nil, newLogger("error"))
	caps := api.capabilities()
	if caps.IPv6 {
		t.Fatal("IPv6 was advertised with the mode off")
	}
	if caps.IPv6State != "off" {
		t.Fatalf("ipv6State = %q, want off", caps.IPv6State)
	}
}

// Without a monitor (a unit test, an old wiring) the state is "none", not a
// silent true: a missing probe must not promise an IPv6 path.
func TestAPICapabilitiesWithoutMonitor(t *testing.T) {
	cfg := testConfig(t)
	cfg.IPv6 = ipv6Auto
	api := newAPIServer(cfg, nil, nil, newLogger("error"))
	caps := api.capabilities()
	if caps.IPv6 {
		t.Fatal("IPv6 was advertised without a monitor")
	}
	if caps.IPv6State != ipv6StateNone {
		t.Fatalf("ipv6State = %q, want %q", caps.IPv6State, ipv6StateNone)
	}
}
