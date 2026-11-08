package main

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestConfigPeerURI(t *testing.T) {
	cfg := testConfig(t)
	cfg.PeerURI = "tls://203.0.113.10:13338?key=0123456789abcdef"
	port, err := cfg.peerPort()
	if err != nil {
		t.Fatal(err)
	}
	if port != 13338 {
		t.Fatalf("port = %d, want 13338", port)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	cfg.PeerURI = "socks://203.0.113.10:13338"
	if _, err := cfg.peerPort(); err == nil {
		t.Fatal("socks:// must not be accepted as a peering URI")
	}
	cfg.PeerURI = "tls://203.0.113.10"
	if _, err := cfg.peerPort(); err == nil {
		t.Fatal("a peer URI without a port must be rejected")
	}
	cfg.PeerURI = ""
	if port, err := cfg.peerPort(); err != nil || port != 0 {
		t.Fatalf("empty peer URI: port=%d err=%v", port, err)
	}
}

// A session that must never expire is a supported setting, not a typo: the
// web form accepts "0" and "unlimited" and the signer then skips the expiry
// check. A negative or malformed value is still rejected.
func TestConfigSessionLifetime(t *testing.T) {
	cfg := testConfig(t)
	for _, value := range []string{"0", "unlimited", "Unlimited", "0s"} {
		cfg.Web.SessionTTL = value
		if err := cfg.validate(); err != nil {
			t.Fatalf("%q must be accepted: %v", value, err)
		}
		ttl, err := cfg.webSessionTTL()
		if err != nil || ttl != 0 {
			t.Fatalf("%q = %v, %v; want an unlimited session", value, ttl, err)
		}
	}
	cfg.Web.SessionTTL = "12h"
	if ttl, err := cfg.webSessionTTL(); err != nil || ttl != 12*time.Hour {
		t.Fatalf("12h = %v, %v", ttl, err)
	}
	for _, value := range []string{"", "-1h", "soon"} {
		cfg.Web.SessionTTL = value
		if err := cfg.validate(); err == nil {
			t.Fatalf("%q must be rejected", value)
		}
	}
}

func TestConfigAPIAuthDefaults(t *testing.T) {
	cfg := defaultConfig()
	cfg.applyDefaults()
	if cfg.apiAuthMode() != apiAuthRequired {
		t.Fatalf("apiAuthMode = %q, want required", cfg.apiAuthMode())
	}
	if cfg.APIRateLimit != 120 {
		t.Fatalf("apiRateLimit = %d, want 120", cfg.APIRateLimit)
	}
	if !cfg.tokenRequired() {
		t.Fatal("a default config must require a token")
	}
	// Open mode needs no credential: there is no allowlist entry to bind one
	// to, and the operator has already opened the exit to everyone.
	cfg.AllowAll = true
	if cfg.tokenRequired() {
		t.Fatal("open mode must not require a token")
	}
	// A typo must never open the API.
	cfg.AllowAll = false
	cfg.APIToken = "yes please"
	if cfg.apiAuthMode() != apiAuthRequired {
		t.Fatalf("an unknown mode became %q", cfg.apiAuthMode())
	}
}

func TestConfigValidatesAPISettings(t *testing.T) {
	cfg := testConfig(t)
	cfg.APIToken = apiAuthOff
	if err := cfg.validate(); err != nil {
		t.Fatalf("apiToken off must be accepted: %v", err)
	}
	cfg.APIToken = apiAuthOptional
	if err := cfg.validate(); err != nil {
		t.Fatalf("apiToken optional must be accepted: %v", err)
	}
	cfg.APIToken = "enabled"
	if err := cfg.validate(); err == nil {
		t.Fatal("an unknown apiToken must be rejected")
	}
	cfg.APIToken = apiAuthRequired
	cfg.APIRateLimit = -1
	if err := cfg.validate(); err == nil {
		t.Fatal("a negative apiRateLimit must be rejected")
	}
	cfg.APIRateLimit = 6001
	if err := cfg.validate(); err == nil {
		t.Fatal("apiRateLimit 6001 must be rejected")
	}
	cfg.APIRateLimit = 0
	if err := cfg.validate(); err != nil {
		t.Fatalf("apiRateLimit 0 (disabled) must be accepted: %v", err)
	}
}

func TestExitLinkCarriesPeer(t *testing.T) {
	cfg := testConfig(t)
	cfg.Name = "Berlin exit"
	cfg.PeerURI = "tls://203.0.113.10:13338?key=0123456789abcdef"

	link := exitLink(cfg, "token")
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("peer") != cfg.PeerURI {
		t.Fatalf("peer = %q, want %q", query.Get("peer"), cfg.PeerURI)
	}
	if query.Get("a") != cfg.ListenAddress {
		t.Fatalf("address = %q", query.Get("a"))
	}

	cfg.PeerURI = ""
	if strings.Contains(exitLink(cfg, "token"), "peer=") {
		t.Fatal("an outbound-only node must not advertise a peer URI")
	}
}

func TestExitLinkWithoutAnInvite(t *testing.T) {
	cfg := testConfig(t)
	cfg.Name = "Berlin exit"

	parsed, err := url.Parse(exitLink(cfg, ""))
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("i") != "" {
		t.Fatalf("a client link must not carry an invite: %q", query.Get("i"))
	}
	if query.Get("a") != cfg.ListenAddress || query.Get("p") == "" {
		t.Fatalf("client link = %q", parsed.String())
	}
}
