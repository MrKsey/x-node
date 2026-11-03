package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSocksUDPDatagramRoundTrip(t *testing.T) {
	packet := buildSocksUDPDatagram(net.ParseIP("1.2.3.4"), 443, []byte("hello"))
	frag, host, port, payload, ok := parseSocksUDPDatagram(packet)
	if !ok || frag != 0 || host != "1.2.3.4" || port != 443 || string(payload) != "hello" {
		t.Fatalf("round trip = %v %v %d %q ok=%v", frag, host, port, payload, ok)
	}

	packet = buildSocksUDPDatagram(net.ParseIP("2606:4700:4700::1111"), 53, []byte("dns"))
	_, host, port, payload, ok = parseSocksUDPDatagram(packet)
	if !ok || host != "2606:4700:4700::1111" || port != 53 || string(payload) != "dns" {
		t.Fatalf("IPv6 round trip = %v %d %q ok=%v", host, port, payload, ok)
	}

	// FRAG is reported, not rejected by the parser: the association decides.
	packet[2] = 1
	if frag, _, _, _, ok := parseSocksUDPDatagram(packet); !ok || frag != 1 {
		t.Fatalf("fragment flag was lost: %d ok=%v", frag, ok)
	}

	// A domain name is parsed as a name; the association resolves it when
	// domainResolution is enabled.
	domain := []byte{0, 0, 0, socksAtypDomain, 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0, 53}
	frag, host, port, _, ok = parseSocksUDPDatagram(domain)
	if !ok || frag != 0 || host != "example" || port != 53 {
		t.Fatalf("domain datagram = %q %d ok=%v", host, port, ok)
	}
}

func TestUDPAssociationEcho(t *testing.T) {
	cfg := testUDPConfig()
	association, relay, closeControl := startTestAssociation(t, cfg, nil, echoDial)
	defer closeControl()

	client := &net.UDPAddr{IP: net.ParseIP("200::1"), Port: 41000}
	relay.inbound <- fakeDatagram{
		data: buildSocksUDPDatagram(net.ParseIP("1.2.3.4"), 443, []byte("hello")),
		from: client,
	}

	reply := readFakeReply(t, relay)
	_, source, port, payload, ok := parseSocksUDPDatagram(reply.data)
	if !ok || source != "1.2.3.4" || port != 443 || string(payload) != "hello" {
		t.Fatalf("reply = %v %d %q ok=%v", source, port, payload, ok)
	}
	if reply.from == nil || reply.from.Port != client.Port {
		t.Fatalf("reply was sent to %v, want %v", reply.from, client)
	}
	waitForCounter(t, "datagrams in", association.datagramsIn.Load, 1)
	waitForCounter(t, "datagrams out", association.datagramsOut.Load, 1)
}

func TestUDPAssociationRejectsBlockedDestination(t *testing.T) {
	cfg := testUDPConfig()
	var dials atomic.Int64
	association, relay, closeControl := startTestAssociation(t, cfg, destinationAllowed, func(net.IP, int) (udpFlowSocket, error) {
		dials.Add(1)
		return newFakeFlow(), nil
	})
	defer closeControl()
	_ = association

	relay.inbound <- fakeDatagram{
		data: buildSocksUDPDatagram(net.ParseIP("127.0.0.1"), 53, []byte("blocked")),
		from: &net.UDPAddr{IP: net.ParseIP("200::1"), Port: 41001},
	}
	assertNoFakeReply(t, relay, 200*time.Millisecond)
	if dials.Load() != 0 {
		t.Fatal("a loopback UDP destination was dialed")
	}
}

func TestUDPAssociationRejectsFragment(t *testing.T) {
	cfg := testUDPConfig()
	var dials atomic.Int64
	association, relay, closeControl := startTestAssociation(t, cfg, allowAllDestinations, func(net.IP, int) (udpFlowSocket, error) {
		dials.Add(1)
		return newFakeFlow(), nil
	})
	defer closeControl()
	_ = association

	packet := buildSocksUDPDatagram(net.ParseIP("1.2.3.4"), 443, []byte("fragment"))
	packet[2] = 1
	relay.inbound <- fakeDatagram{
		data: packet,
		from: &net.UDPAddr{IP: net.ParseIP("200::1"), Port: 41002},
	}
	assertNoFakeReply(t, relay, 200*time.Millisecond)
	if dials.Load() != 0 {
		t.Fatal("a fragmented UDP datagram was forwarded")
	}
}

func TestUDPAssociationRejectsForeignSource(t *testing.T) {
	cfg := testUDPConfig()
	association, relay, closeControl := startTestAssociation(t, cfg, allowAllDestinations, echoDial)
	defer closeControl()
	_ = association

	first := &net.UDPAddr{IP: net.ParseIP("200::1"), Port: 41003}
	second := &net.UDPAddr{IP: net.ParseIP("200::1"), Port: 41004}
	relay.inbound <- fakeDatagram{
		data: buildSocksUDPDatagram(net.ParseIP("1.2.3.4"), 443, []byte("first")),
		from: first,
	}
	reply := readFakeReply(t, relay)
	if reply.from.Port != first.Port {
		t.Fatalf("reply went to port %d, want %d", reply.from.Port, first.Port)
	}

	relay.inbound <- fakeDatagram{
		data: buildSocksUDPDatagram(net.ParseIP("1.2.3.4"), 443, []byte("second")),
		from: second,
	}
	assertNoFakeReply(t, relay, 200*time.Millisecond)
}

func TestUDPAssociationHonoursRequestPort(t *testing.T) {
	cfg := testUDPConfig()
	relay := newFakeRelay()
	limiter := newUDPRelay(cfg)
	limiter.policy = allowAllDestinations
	limiter.dial = echoDial
	controlReader, controlWriter := io.Pipe()
	defer controlWriter.Close()
	association := newUDPAssociation(
		cfg, relay, net.ParseIP("200::1"),
		socksRequest{atyp: socksAtypIPv4, ip: net.IPv4zero, port: 41010},
		2*time.Second, limiter, nil, newLogger("error"),
	)
	go association.run(bufio.NewReader(controlReader))

	// The request named the UDP source port, so a datagram from another port is
	// ignored even before the first packet was accepted.
	relay.inbound <- fakeDatagram{
		data: buildSocksUDPDatagram(net.ParseIP("1.2.3.4"), 443, []byte("wrong")),
		from: &net.UDPAddr{IP: net.ParseIP("200::1"), Port: 41011},
	}
	assertNoFakeReply(t, relay, 200*time.Millisecond)
	relay.inbound <- fakeDatagram{
		data: buildSocksUDPDatagram(net.ParseIP("1.2.3.4"), 443, []byte("right")),
		from: &net.UDPAddr{IP: net.ParseIP("200::1"), Port: 41010},
	}
	reply := readFakeReply(t, relay)
	if _, _, _, payload, ok := parseSocksUDPDatagram(reply.data); !ok || string(payload) != "right" {
		t.Fatalf("payload = %q ok=%v", payload, ok)
	}
}

func TestUDPRelayLimits(t *testing.T) {
	cfg := defaultConfig()
	cfg.MaxUDPAssociations = 1
	cfg.MaxConnsPerClient = 1
	relay := newUDPRelay(cfg)
	ip := net.ParseIP("200::1")
	if !relay.acquire(ip) {
		t.Fatal("the first association was refused")
	}
	if relay.acquire(ip) {
		t.Fatal("the association limit was not enforced")
	}
	relay.release(ip)
	if !relay.acquire(ip) {
		t.Fatal("the slot was not released")
	}
	relay.release(ip)

	cfg.UDPEnabled = false
	disabled := newUDPRelay(cfg)
	if disabled.acquire(ip) {
		t.Fatal("a disabled relay accepted an association")
	}
}

func TestRuntimeTrackerUDP(t *testing.T) {
	cfg := testUDPConfig()
	tracker := newRuntimeTracker(cfg, newLogger("error"))
	ip := net.ParseIP("200::1")
	tracker.udpOpen(ip)
	tracker.udpTraffic(ip, 2, 3, 200, 300)
	doc := tracker.snapshotLocked()
	if doc.TotalUDPAssociations != 1 || doc.TotalUDPDatagramsIn != 2 ||
		doc.TotalUDPDatagramsOut != 3 || doc.TotalUDPBytesIn != 200 || doc.TotalUDPBytesOut != 300 {
		t.Fatalf("totals = %+v", doc)
	}
	if len(doc.Clients) != 1 || doc.Clients[0].UDPAssociations != 1 {
		t.Fatalf("clients = %+v", doc.Clients)
	}
	tracker.udpClose(ip)
	doc = tracker.snapshotLocked()
	if doc.TotalUDPAssociations != 0 || doc.Clients[0].UDPAssociations != 0 {
		t.Fatalf("after close = %+v", doc)
	}
}

func TestUDPAssociateRefusedWhenDisabled(t *testing.T) {
	_, listener, cancel := startTestSocksServer(t, func(cfg *Config) {
		cfg.UDPEnabled = false
	})
	defer cancel()

	control, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(control, method); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Write([]byte{5, socksCmdUDPAssociate, 0, socksAtypIPv4, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(control, reply); err != nil {
		t.Fatal(err)
	}
	if reply[1] != socksReplyCommandNotSupp {
		t.Fatalf("reply code = %d, want %d", reply[1], socksReplyCommandNotSupp)
	}
}

// TestUDPAssociationLoopback runs the real relay over loopback with a UDP echo
// server. Some sandboxes block UDP loopback entirely; the test skips there
// instead of reporting a false failure.
func TestUDPAssociationLoopback(t *testing.T) {
	if !loopbackUDPWorks() {
		t.Skip("UDP loopback is not available in this environment")
	}
	echo, stopEcho := startUDPEcho(t)
	defer stopEcho()

	server, listener, cancel := startTestSocksServer(t, nil)
	defer cancel()
	// The real policy blocks loopback, which is where the test echo lives.
	server.udp.policy = allowAllDestinations

	control, relay := openUDPAssociation(t, listener.Addr().String())
	defer control.Close()

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	payload := []byte("udp-echo")
	if _, err := client.WriteToUDP(buildSocksUDPDatagram(echo.IP, echo.Port, payload), relay); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("no UDP reply: %v", err)
	}
	_, source, port, reply, ok := parseSocksUDPDatagram(buf[:n])
	if !ok || source != echo.IP.String() || port != echo.Port || string(reply) != string(payload) {
		t.Fatalf("reply = %v %d %q ok=%v", source, port, reply, ok)
	}
}

// TestUDPAssociationOverYggdrasil runs the real relay over the node's own
// Yggdrasil address. It is skipped unless the address is assigned to the host,
// so it can be run on the server without a phone:
//
//	X_NODE_TEST_ADDRESS=201:... go test -run TestUDPAssociationOverYggdrasil
func TestUDPAssociationOverYggdrasil(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("X_NODE_TEST_ADDRESS"))
	if address == "" {
		t.Skip("set X_NODE_TEST_ADDRESS to run the integration test")
	}
	ip := net.ParseIP(address)
	if !isYggAddress(ip) {
		t.Fatalf("%q is not a Yggdrasil address", address)
	}
	if !interfaceHasAddress(address) {
		t.Skipf("%s is not assigned to this host", address)
	}

	echo, stopEcho := startUDPEcho(t)
	defer stopEcho()

	cfg := testUDPConfig()
	cfg.ListenAddress = address
	cfg.StateDir = t.TempDir()
	cfg.AllowAll = true
	cfg.UDPIdleTimeout = "5s"
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	server := newSocksServer(cfg, store, nil, newLogger("error"), nil)
	// The echo server is on loopback, which the production policy blocks.
	server.udp.policy = allowAllDestinations

	listener, err := net.Listen("tcp", net.JoinHostPort(address, "0"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.serve(ctx, listener) }()
	defer listener.Close()

	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: ip}, Timeout: 3 * time.Second}
	control, err := dialer.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	relay := udpAssociate(t, control)

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	payload := []byte("ygg-udp")
	if _, err := client.WriteToUDP(buildSocksUDPDatagram(echo.IP, echo.Port, payload), relay); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("no reply through the Yggdrasil relay: %v", err)
	}
	_, _, _, reply, ok := parseSocksUDPDatagram(buf[:n])
	if !ok || string(reply) != string(payload) {
		t.Fatalf("reply = %q ok=%v", reply, ok)
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testUDPConfig() Config {
	cfg := defaultConfig()
	cfg.ListenAddress = "200::1"
	cfg.BlockedPorts = nil
	cfg.UDPEnabled = true
	cfg.UDPIdleTimeout = "2s"
	cfg.MaxUDPAssociations = 4
	cfg.MaxConnsPerClient = 4
	cfg.MaxConnsTotal = 8
	return cfg
}

func allowAllDestinations(net.IP, int) bool { return true }

func echoDial(net.IP, int) (udpFlowSocket, error) { return newFakeFlow(), nil }

func startTestAssociation(
	t *testing.T,
	cfg Config,
	policy func(net.IP, int) bool,
	dial func(net.IP, int) (udpFlowSocket, error),
) (*udpAssociation, *fakeRelay, func()) {
	t.Helper()
	if policy == nil {
		policy = allowAllDestinations
	}
	relay := newFakeRelay()
	limiter := newUDPRelay(cfg)
	limiter.policy = policy
	limiter.dial = dial
	controlReader, controlWriter := io.Pipe()
	association := newUDPAssociation(
		cfg, relay, net.ParseIP("200::1"), socksRequest{},
		2*time.Second, limiter, nil, newLogger("error"),
	)
	go association.run(bufio.NewReader(controlReader))
	return association, relay, func() { _ = controlWriter.Close() }
}

func readFakeReply(t *testing.T, relay *fakeRelay) fakeDatagram {
	t.Helper()
	select {
	case reply := <-relay.outbound:
		return reply
	case <-time.After(2 * time.Second):
		t.Fatal("no UDP reply from the association")
		return fakeDatagram{}
	}
}

func assertNoFakeReply(t *testing.T, relay *fakeRelay, wait time.Duration) {
	t.Helper()
	select {
	case reply := <-relay.outbound:
		t.Fatalf("unexpected UDP reply: %q", reply.data)
	case <-time.After(wait):
	}
}

// waitForCounter polls a counter until it reaches the expected value. The
// association increments its counters after the socket call, so the reply can
// reach the test a moment before the number becomes visible.
func waitForCounter(t *testing.T, name string, get func() uint64, want uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := get(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s = %d, want %d", name, get(), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// fakeRelay is an in-memory UDP socket. The test writes to inbound and reads
// the association's answers from outbound.
type fakeRelay struct {
	mu       sync.Mutex
	deadline time.Time
	inbound  chan fakeDatagram
	outbound chan fakeDatagram
	closed   chan struct{}
	once     sync.Once
}

type fakeDatagram struct {
	data []byte
	from *net.UDPAddr
}

func newFakeRelay() *fakeRelay {
	return &fakeRelay{
		inbound:  make(chan fakeDatagram, 8),
		outbound: make(chan fakeDatagram, 8),
		closed:   make(chan struct{}),
	}
}

func (f *fakeRelay) ReadFromUDP(p []byte) (int, *net.UDPAddr, error) {
	f.mu.Lock()
	deadline := f.deadline
	f.mu.Unlock()
	timeout := deadlineChannel(deadline)
	select {
	case d := <-f.inbound:
		return copy(p, d.data), d.from, nil
	case <-timeout:
		return 0, nil, timeoutError{}
	case <-f.closed:
		return 0, nil, net.ErrClosed
	}
}

func (f *fakeRelay) WriteToUDP(p []byte, addr *net.UDPAddr) (int, error) {
	packet := fakeDatagram{data: append([]byte(nil), p...), from: addr}
	select {
	case f.outbound <- packet:
		return len(p), nil
	case <-f.closed:
		return 0, net.ErrClosed
	}
}

func (f *fakeRelay) SetReadDeadline(t time.Time) error {
	f.mu.Lock()
	f.deadline = t
	f.mu.Unlock()
	return nil
}

func (f *fakeRelay) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeRelay) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.ParseIP("200::1"), Port: 5000}
}

// fakeFlow echoes everything the association writes, which is exactly what a
// UDP echo server does.
type fakeFlow struct {
	mu       sync.Mutex
	deadline time.Time
	replies  chan []byte
	closed   chan struct{}
	once     sync.Once
}

func newFakeFlow() *fakeFlow {
	return &fakeFlow{
		replies: make(chan []byte, 8),
		closed:  make(chan struct{}),
	}
}

func (f *fakeFlow) Read(p []byte) (int, error) {
	f.mu.Lock()
	deadline := f.deadline
	f.mu.Unlock()
	timeout := deadlineChannel(deadline)
	select {
	case data := <-f.replies:
		return copy(p, data), nil
	case <-timeout:
		return 0, timeoutError{}
	case <-f.closed:
		return 0, net.ErrClosed
	}
}

func (f *fakeFlow) Write(p []byte) (int, error) {
	data := append([]byte(nil), p...)
	select {
	case f.replies <- data:
		return len(p), nil
	case <-f.closed:
		return 0, net.ErrClosed
	}
}

func (f *fakeFlow) SetReadDeadline(t time.Time) error {
	f.mu.Lock()
	f.deadline = t
	f.mu.Unlock()
	return nil
}

func (f *fakeFlow) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func deadlineChannel(deadline time.Time) <-chan time.Time {
	if deadline.IsZero() {
		return nil
	}
	wait := time.Until(deadline)
	if wait <= 0 {
		wait = time.Millisecond
	}
	return time.After(wait)
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// startTestSocksServer builds a SOCKS5 server on loopback. The source check is
// relaxed because the real one only accepts Yggdrasil addresses.
func startTestSocksServer(t *testing.T, tweak func(*Config)) (*socksServer, net.Listener, context.CancelFunc) {
	t.Helper()
	cfg := testUDPConfig()
	cfg.ListenAddress = "127.0.0.1"
	cfg.StateDir = t.TempDir()
	cfg.AllowAll = true
	if tweak != nil {
		tweak(&cfg)
	}
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	server := newSocksServer(cfg, store, nil, newLogger("error"), nil)
	server.sourceOK = func(net.IP) bool { return true }
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = server.serve(ctx, listener) }()
	stop := func() {
		cancel()
		_ = listener.Close()
	}
	return server, listener, stop
}

func loopbackUDPWorks() bool {
	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return false
	}
	defer server.Close()
	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return false
	}
	defer client.Close()
	if _, err := client.WriteToUDP([]byte("probe"), server.LocalAddr().(*net.UDPAddr)); err != nil {
		return false
	}
	_ = server.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 16)
	_, _, err = server.ReadFromUDP(buf)
	return err == nil
}

func startUDPEcho(t *testing.T) (*net.UDPAddr, func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteToUDP(buf[:n], from)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr), func() {
		_ = conn.Close()
		<-done
	}
}

// openUDPAssociation performs the SOCKS5 handshake and returns the control
// connection plus the relay address from the reply.
func openUDPAssociation(t *testing.T, serverAddr string) (net.Conn, *net.UDPAddr) {
	t.Helper()
	conn, err := net.Dial("tcp", serverAddr)
	if err != nil {
		t.Fatal(err)
	}
	relay := udpAssociate(t, conn)
	return conn, relay
}

func udpAssociate(t *testing.T, conn net.Conn) *net.UDPAddr {
	t.Helper()
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(conn, method); err != nil {
		t.Fatal(err)
	}
	if method[0] != 5 || method[1] != 0 {
		t.Fatalf("method reply = %v", method)
	}
	if _, err := conn.Write([]byte{5, socksCmdUDPAssociate, 0, socksAtypIPv4, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		t.Fatal(err)
	}
	if header[0] != 5 || header[1] != socksReplySuccess {
		t.Fatalf("associate reply = %v", header)
	}
	var ip net.IP
	switch header[3] {
	case socksAtypIPv4:
		raw := make([]byte, 4)
		if _, err := io.ReadFull(conn, raw); err != nil {
			t.Fatal(err)
		}
		ip = net.IP(raw)
	case socksAtypIPv6:
		raw := make([]byte, 16)
		if _, err := io.ReadFull(conn, raw); err != nil {
			t.Fatal(err)
		}
		ip = net.IP(raw)
	default:
		t.Fatalf("unexpected address type %d", header[3])
	}
	var portRaw [2]byte
	if _, err := io.ReadFull(conn, portRaw[:]); err != nil {
		t.Fatal(err)
	}
	return &net.UDPAddr{IP: ip, Port: int(binary.BigEndian.Uint16(portRaw[:]))}
}
