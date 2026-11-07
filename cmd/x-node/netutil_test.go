package main

import (
	"net"
	"testing"
	"time"
)

func TestParseByteSize(t *testing.T) {
	cases := map[string]int64{
		"0":      0,
		"1024":   1024,
		"1K":     1000,
		"1MB":    1000000,
		"512MiB": 512 << 20,
		"5G":     5000000000,
		"2GiB":   2 << 30,
		"1TiB":   1 << 40,
		"1.5GiB": 3 << 29,
	}
	for input, want := range cases {
		got, err := parseByteSize(input)
		if err != nil {
			t.Fatalf("%q: %v", input, err)
		}
		if got != want {
			t.Fatalf("%q = %d, want %d", input, got, want)
		}
	}
	if _, err := parseByteSize("abc"); err == nil {
		t.Fatal("a word was accepted as a size")
	}
	if _, err := parseByteSize("-5G"); err == nil {
		t.Fatal("a negative size was accepted")
	}
}

func TestParseBitRate(t *testing.T) {
	cases := map[string]int{
		"0":       0,
		"512Kbps": 512,
		"10Mbps":  10000,
		"1Gbps":   1000000,
		"2M":      2000,
	}
	for input, want := range cases {
		got, err := parseBitRate(input)
		if err != nil {
			t.Fatalf("%q: %v", input, err)
		}
		if got != want {
			t.Fatalf("%q = %d, want %d", input, got, want)
		}
	}
}

func TestIsGlobalIPv6(t *testing.T) {
	global := []string{"2606:4700:4700::1111", "2a01:ecc0:5000:329::2"}
	for _, value := range global {
		if !isGlobalIPv6(net.ParseIP(value)) {
			t.Fatalf("%s should be global", value)
		}
	}
	local := []string{"::1", "fe80::1", "fc00::1", "200::1", "201:bd5a:efe4:92c:284c:bbcd:5b53:ea3b"}
	for _, value := range local {
		if isGlobalIPv6(net.ParseIP(value)) {
			t.Fatalf("%s should not be global", value)
		}
	}
	if isGlobalIPv6(net.ParseIP("1.2.3.4")) {
		t.Fatal("IPv4 is not IPv6")
	}
}

func TestInterfaceForIP(t *testing.T) {
	// The loopback address is assigned to a loopback interface on every host,
	// so this proves that the lookup compares the address and not the name.
	iface := interfaceForIP(net.ParseIP("127.0.0.1"))
	if iface == "" {
		t.Fatal("the loopback address has no interface")
	}
	if iface := interfaceForIP(net.ParseIP("192.0.2.1")); iface != "" {
		t.Fatalf("an unassigned address reported interface %q", iface)
	}
	if iface := interfaceForIP(nil); iface != "" {
		t.Fatalf("a nil address reported interface %q", iface)
	}
}

func TestDialerFamilyFilter(t *testing.T) {
	cfg := testConfig(t)
	mode := ipv6Off
	dialer := newOutboundDialer(cfg, func() string { return mode }, nil)
	if dialer.familyAllowed(net.ParseIP("2606:4700:4700::1111")) {
		t.Fatal("IPv6 was allowed with mode off")
	}
	if !dialer.familyAllowed(net.ParseIP("1.2.3.4")) {
		t.Fatal("IPv4 was refused with mode off")
	}
	if !dialer.familyAllowed(net.ParseIP("::1")) {
		t.Fatal("loopback must stay reachable for local tests")
	}
	mode = ipv6Prefer
	if !dialer.familyAllowed(net.ParseIP("2606:4700:4700::1111")) {
		t.Fatal("IPv6 was refused with mode prefer")
	}
}

func TestDialerOrder(t *testing.T) {
	cfg := testConfig(t)
	mode := ipv6Prefer
	dialer := newOutboundDialer(cfg, func() string { return mode }, nil)
	ordered := dialer.order([]net.IP{
		net.ParseIP("1.1.1.1"),
		net.ParseIP("2606:4700:4700::1111"),
		net.ParseIP("8.8.8.8"),
	})
	if ordered[0].To4() != nil {
		t.Fatalf("IPv6 was not first: %v", ordered)
	}
	mode = ipv6Off
	dialer = newOutboundDialer(cfg, func() string { return mode }, nil)
	ordered = dialer.order([]net.IP{
		net.ParseIP("2606:4700:4700::1111"),
		net.ParseIP("1.1.1.1"),
	})
	if ordered[0].To4() == nil {
		t.Fatalf("IPv4 was not first with mode off: %v", ordered)
	}
}

func TestShaperDoesNotBlockSmallWrites(t *testing.T) {
	shaper := newShaper(100000) // 100 Mbps
	started := time.Now()
	for i := 0; i < 10; i++ {
		shaper.wait(1024)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("small writes took %s", elapsed)
	}
	if newShaper(0) != nil {
		t.Fatal("a zero rate must produce a nil shaper")
	}
}

func TestQuotaEnforcement(t *testing.T) {
	cfg := testConfig(t)
	log := newLogger("error")
	quotas, err := openQuotas(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := openUsage(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	ip := testAddr(t, 3)
	if allowed, _ := quotas.allow(ip, usage); !allowed {
		t.Fatal("a client without a quota was refused")
	}
	if err := quotas.Set(Quota{Address: ip.String(), DailyBytes: 1000}); err != nil {
		t.Fatal(err)
	}
	if allowed, _ := quotas.allow(ip, usage); !allowed {
		t.Fatal("the client was refused before the cap")
	}
	usage.addTraffic(ip, 600, 500)
	allowed, reason := quotas.allow(ip, usage)
	if allowed {
		t.Fatal("the daily cap was not enforced")
	}
	if reason != "daily quota exhausted" {
		t.Fatalf("reason = %q", reason)
	}
}
