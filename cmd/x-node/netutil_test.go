package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestListenOnAssignedAddressBindsAnExistingAddress(t *testing.T) {
	log := newLogger("error")
	listener, err := listenOnAssignedAddress(context.Background(), "127.0.0.1:0", log, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
}

func TestListenOnAssignedAddressBindsAWildcard(t *testing.T) {
	log := newLogger("error")
	listener, err := listenOnAssignedAddress(context.Background(), "0.0.0.0:0", log, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
}

func TestListenOnAssignedAddressWaitsAndStops(t *testing.T) {
	// 192.0.2.1 is TEST-NET-1: it is never assigned to a local interface, so
	// the helper waits instead of binding and must return when the daemon is
	// asked to stop.
	log := newLogger("error")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if _, err := listenOnAssignedAddress(ctx, "192.0.2.1:9", log, "test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the helper took %s to notice the cancelled context", elapsed)
	}
}

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

func TestIPv6StateText(t *testing.T) {
	cases := []struct {
		state, source, detail, iface, want string
	}{
		{ipv6StateChecking, "", "", "", "checking the route"},
		{ipv6StateChecking, "2a01::1", "", "", "checking the route from 2a01::1"},
		{ipv6StateNone, "", "network is unreachable", "", "no global IPv6 address (network is unreachable)"},
		{ipv6StateBroken, "2a01::1", "i/o timeout", "", "2a01::1 is assigned, but the provider does not route IPv6 (i/o timeout)"},
		{ipv6StateOK, "2a01::1", "", "eth0", "available via 2a01::1 on eth0"},
		{ipv6StateOK, "2a01::1", "", "", "available via 2a01::1"},
	}
	for _, test := range cases {
		if got := ipv6StateText(test.state, test.source, test.detail, test.iface); got != test.want {
			t.Errorf("%s: got %q, want %q", test.state, got, test.want)
		}
	}
}

// A missing monitor must not look like a working IPv6 route: the dialer and the
// status API both call status on a nil pointer.
func TestIPv6MonitorNilStatus(t *testing.T) {
	var monitor *ipv6Monitor
	available, state, source, detail, iface := monitor.status()
	if available || state != ipv6StateNone || source != "" || detail == "" || iface != "" {
		t.Fatalf("nil monitor = %v/%q/%q/%q/%q", available, state, source, detail, iface)
	}
}

// A fresh monitor checks only the address: until the reachability probe
// finishes, an address is "checking", never "ok". Otherwise a provider that
// drops IPv6 would be advertised as working for the first seconds.
func TestIPv6MonitorStartsCheckingOrNone(t *testing.T) {
	monitor := newIPv6Monitor()
	_, state, source, _, _ := monitor.status()
	switch state {
	case ipv6StateChecking:
		if source == "" {
			t.Fatal("the checking state must carry the detected source address")
		}
	case ipv6StateNone:
		if source != "" {
			t.Fatalf("no address, but source = %q", source)
		}
	default:
		t.Fatalf("a fresh monitor reported %q", state)
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
	// With a monitor, "prefer" follows the measured route: a broken provider
	// must not turn every dial into a timeout.
	broken := &ipv6Monitor{state: ipv6StateBroken}
	dialer = newOutboundDialer(cfg, func() string { return mode }, broken)
	if dialer.familyAllowed(net.ParseIP("2606:4700:4700::1111")) {
		t.Fatal("IPv6 was allowed in prefer mode with a broken route")
	}
	working := &ipv6Monitor{available: true, state: ipv6StateOK}
	dialer = newOutboundDialer(cfg, func() string { return mode }, working)
	if !dialer.familyAllowed(net.ParseIP("2606:4700:4700::1111")) {
		t.Fatal("IPv6 was refused in prefer mode with a working route")
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
