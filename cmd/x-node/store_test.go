package main

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testAddr(t *testing.T, seed byte) net.IP {
	t.Helper()
	raw := make([]byte, ed25519.SeedSize)
	raw[0] = seed
	priv := ed25519.NewKeyFromSeed(raw)
	pub := priv.Public().(ed25519.PublicKey)
	ip, err := addrForKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return ip
}

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg := defaultConfig()
	cfg.ListenAddress = "202:24ae:c219:8a4a:de94:eae2:b97e:ac87"
	cfg.StateDir = t.TempDir()
	cfg.MaxClients = 10
	return cfg
}

func TestStoreInviteRegistration(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	first := testAddr(t, 1)
	second := testAddr(t, 2)

	token, _, err := store.NewInvite("test", time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	client, existed, err := store.Register(token, Client{Address: first.String(), DeviceName: "phone"}, cfg.MaxClients)
	if err != nil {
		t.Fatal(err)
	}
	if existed {
		t.Fatal("first registration must create a new client")
	}
	if client.Label != "test 1" {
		t.Fatalf("label = %q, want test 1 (the note and the use number)", client.Label)
	}
	if !store.Allowed(first) {
		t.Fatal("registered address is not allowed")
	}

	// A repeated registration with the same address must be idempotent and
	// must not burn another use of the invite.
	if _, existed, err := store.Register(token, Client{Address: first.String()}, cfg.MaxClients); err != nil || !existed {
		t.Fatalf("repeat registration: existed=%v err=%v", existed, err)
	}
	if _, _, err := store.Register(token, Client{Address: second.String(), DeviceName: "laptop"}, cfg.MaxClients); err != nil {
		t.Fatalf("second use: %v", err)
	}
	if client, _ := store.Get(second); client.Label != "test 2" {
		t.Fatalf("second label = %q, want test 2", client.Label)
	}
	if !store.Allowed(second) {
		t.Fatal("second registered address is not allowed")
	}

	third := testAddr(t, 3)
	if _, _, err := store.Register(token, Client{Address: third.String()}, cfg.MaxClients); !errors.Is(err, errInviteInvalid) {
		t.Fatalf("third use error = %v, want errInviteInvalid", err)
	}
}

// The note typed when creating an invite names the client that redeems it.
// A multi-use invite numbers the devices, so an operator who writes "Family"
// gets "Family 1" and "Family 2" instead of three identical rows.
func TestStoreInviteNoteBecomesClientLabel(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}

	single, _, err := store.NewInvite("Home router", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	client, _, err := store.Register(single, Client{
		Address:    testAddr(t, 40).String(),
		DeviceName: "phone",
	}, cfg.MaxClients)
	if err != nil {
		t.Fatal(err)
	}
	if client.Label != "Home router" {
		t.Fatalf("single-use label = %q, want the note", client.Label)
	}

	// Without a note the device name still names the client.
	plain, _, err := store.NewInvite("", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	client, _, err = store.Register(plain, Client{
		Address:    testAddr(t, 41).String(),
		DeviceName: "laptop",
	}, cfg.MaxClients)
	if err != nil {
		t.Fatal(err)
	}
	if client.Label != "laptop" {
		t.Fatalf("label = %q, want the device name", client.Label)
	}

	// A multi-use invite numbers every registration.
	multi, _, err := store.NewInvite("Family", time.Hour, 3)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		client, _, err = store.Register(multi, Client{
			Address: testAddr(t, byte(42+index)).String(),
		}, cfg.MaxClients)
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("Family %d", index+1)
		if client.Label != want {
			t.Fatalf("label %d = %q, want %q", index, client.Label, want)
		}
	}
}

func TestStoreExpiredInvite(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.NewInvite("short", time.Millisecond, 1)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, _, err := store.Register(token, Client{Address: testAddr(t, 4).String()}, cfg.MaxClients); !errors.Is(err, errInviteExpired) {
		t.Fatalf("error = %v, want errInviteExpired", err)
	}
}

func TestStoreAllowlistFull(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxClients = 1
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.NewInvite("", time.Hour, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Register(token, Client{Address: testAddr(t, 5).String()}, cfg.MaxClients); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Register(token, Client{Address: testAddr(t, 6).String()}, cfg.MaxClients); !errors.Is(err, errAllowlistFull) {
		t.Fatalf("error = %v, want errAllowlistFull", err)
	}
}

func TestStoreManualAllowlist(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	ip := testAddr(t, 7)
	client, existed, err := store.AddClient(ip, "router")
	if err != nil || existed {
		t.Fatalf("add: existed=%v err=%v", existed, err)
	}
	if client.Label != "router" || !store.Allowed(ip) {
		t.Fatal("manual client was not stored")
	}
	removed, err := store.RemoveClient(ip)
	if err != nil || !removed {
		t.Fatalf("remove: removed=%v err=%v", removed, err)
	}
	if store.Allowed(ip) {
		t.Fatal("removed client is still allowed")
	}
}

func TestStoreSuspension(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	ip := testAddr(t, 21)
	if _, _, err := store.AddClient(ip, "phone"); err != nil {
		t.Fatal(err)
	}
	client, err := store.SetSuspended(ip, true)
	if err != nil {
		t.Fatal(err)
	}
	if !client.Suspended || client.SuspendedAt.IsZero() {
		t.Fatalf("suspended client = %+v", client)
	}
	// A suspension is temporary: the entry stays in the allowlist, so the
	// client keeps its counters and token.
	if !store.Allowed(ip) {
		t.Fatal("a suspended client was removed from the allowlist")
	}
	client, err = store.SetSuspended(ip, false)
	if err != nil {
		t.Fatal(err)
	}
	if client.Suspended || !client.SuspendedAt.IsZero() {
		t.Fatalf("resumed client = %+v", client)
	}
	if _, err := store.SetSuspended(testAddr(t, 22), true); !errors.Is(err, errNotAllowed) {
		t.Fatalf("unknown client error = %v, want errNotAllowed", err)
	}
}

func TestStorePicksUpCLIChanges(t *testing.T) {
	cfg := testConfig(t)
	service, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	cli, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}

	// An invite created by the CLI must be usable without a service restart.
	token, _, err := cli.NewInvite("cli", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	ip := testAddr(t, 9)
	if _, _, err := service.Register(token, Client{Address: ip.String()}, cfg.MaxClients); err != nil {
		t.Fatalf("register with a CLI invite: %v", err)
	}

	// A manual allowlist change must be visible to the running service too.
	manual := testAddr(t, 10)
	if _, _, err := cli.AddClient(manual, "manual"); err != nil {
		t.Fatal(err)
	}
	if !service.Allowed(manual) {
		t.Fatal("service did not pick up a manual allowlist entry")
	}
	if _, err := cli.RemoveClient(manual); err != nil {
		t.Fatal(err)
	}
	if service.Allowed(manual) {
		t.Fatal("service still allows a client removed by the CLI")
	}
}

func TestStorePersistsAcrossRestart(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	ip := testAddr(t, 8)
	if _, _, err := store.AddClient(ip, "persisted"); err != nil {
		t.Fatal(err)
	}
	again, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Allowed(ip) {
		t.Fatal("allowlist was not restored from disk")
	}
	if client, ok := again.Get(ip); !ok || client.Label != "persisted" {
		t.Fatalf("restored client = %+v, ok=%v", client, ok)
	}
}

func TestStoreTokenLifecycle(t *testing.T) {
	cfg := testConfig(t)
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	ip := testAddr(t, 30)
	other := testAddr(t, 31)
	if _, _, err := store.AddClient(ip, "phone"); err != nil {
		t.Fatal(err)
	}
	if store.TokenValid(ip, "") || store.TokenValid(ip, "guess") {
		t.Fatal("an address without a token accepted a guess")
	}

	token, err := store.IssueToken(ip)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("IssueToken returned an empty token")
	}
	if !store.TokenValid(ip, token) {
		t.Fatal("the issued token is not valid")
	}
	if store.TokenValid(other, token) {
		t.Fatal("a token worked for another address")
	}

	// The allowlist must carry only the hash: a leaked state file must not
	// hand out a working credential.
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, "allowlist.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("the token was written to the allowlist in clear")
	}
	if !strings.Contains(string(raw), "tokenHash") {
		t.Fatal("the allowlist does not carry the token hash")
	}

	// A rotation invalidates the previous credential.
	next, err := store.IssueToken(ip)
	if err != nil {
		t.Fatal(err)
	}
	if next == token || store.TokenValid(ip, token) {
		t.Fatal("the previous token survived a rotation")
	}
	if !store.TokenValid(ip, next) {
		t.Fatal("the rotated token is not valid")
	}

	if _, err := store.IssueToken(testAddr(t, 32)); !errors.Is(err, errNotAllowed) {
		t.Fatalf("issuing a token to a stranger = %v, want errNotAllowed", err)
	}
}

// The CLI is a separate process: a token issued there must be accepted by the
// running service without a restart, and the other way round.
func TestStoreTokenVisibleAcrossProcesses(t *testing.T) {
	cfg := testConfig(t)
	service, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	cli, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	ip := testAddr(t, 33)
	if _, _, err := service.AddClient(ip, "phone"); err != nil {
		t.Fatal(err)
	}
	token, err := cli.IssueToken(ip)
	if err != nil {
		t.Fatal(err)
	}
	if !service.TokenValid(ip, token) {
		t.Fatal("the service did not pick up a token issued by the CLI")
	}
}
