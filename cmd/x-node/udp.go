package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// udpRelayBuffer is one full UDP datagram: the SOCKS5 header plus the
	// largest payload the kernel can hand over.
	udpRelayBuffer = 65535

	// udpRelayMaxFlows caps the distinct destinations of one association. A
	// client that scans ports would otherwise create one socket per datagram.
	udpRelayMaxFlows = 256

	// udpReportInterval throttles the runtime counters: a busy QUIC session
	// sends hundreds of datagrams per second and every one of them would take
	// the tracker lock.
	udpReportInterval = time.Second
)

// udpRelaySocket is the relay's own UDP socket. It is an interface so the
// association can be tested without real networking.
type udpRelaySocket interface {
	ReadFromUDP([]byte) (int, *net.UDPAddr, error)
	WriteToUDP([]byte, *net.UDPAddr) (int, error)
	SetReadDeadline(time.Time) error
	Close() error
	LocalAddr() net.Addr
}

// udpFlowSocket is one destination socket of an association.
type udpFlowSocket interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	SetReadDeadline(time.Time) error
	Close() error
}

// udpRelay enforces the association limits. It is separate from the TCP
// connection counters because an association is one long-lived relay socket
// rather than a stream.
type udpRelay struct {
	cfg  Config
	live *settings

	// policy and dial are fields so tests can allow loopback destinations and
	// point them at a local echo server.
	policy func(net.IP, int) bool
	dial   func(net.IP, int) (udpFlowSocket, error)

	mu        sync.Mutex
	total     int
	perClient map[string]int
}

func newUDPRelay(cfg Config) *udpRelay {
	return &udpRelay{
		cfg:       cfg,
		policy:    destinationAllowed,
		dial:      dialUDPDestination,
		perClient: make(map[string]int),
	}
}

func (r *udpRelay) config() Config {
	if r.live != nil {
		return r.live.get()
	}
	return r.cfg
}

func (r *udpRelay) acquire(source net.IP) bool {
	cfg := r.config()
	if !cfg.UDPEnabled {
		return false
	}
	key := clientKey(source)
	r.mu.Lock()
	defer r.mu.Unlock()
	if cfg.MaxUDPAssociations > 0 && r.total >= cfg.MaxUDPAssociations {
		return false
	}
	if cfg.MaxConnsPerClient > 0 && r.perClient[key] >= cfg.MaxConnsPerClient {
		return false
	}
	r.perClient[key]++
	r.total++
	return true
}

func (r *udpRelay) release(source net.IP) {
	key := clientKey(source)
	r.mu.Lock()
	if r.perClient[key] > 1 {
		r.perClient[key]--
	} else {
		delete(r.perClient, key)
	}
	if r.total > 0 {
		r.total--
	}
	r.mu.Unlock()
}

func dialUDPDestination(ip net.IP, port int) (udpFlowSocket, error) {
	return net.DialUDP("udp", nil, &net.UDPAddr{IP: ip, Port: port})
}

// udpAssociation is one client's relay: a UDP socket bound to the Yggdrasil
// address, a set of destination flows, and the control connection that keeps it
// alive.
type udpAssociation struct {
	cfg     Config
	relay   udpRelaySocket
	source  net.IP
	idle    time.Duration
	tracker *runtimeTracker
	usage   *usageStore
	dialer  *outboundDialer
	log     *logger

	policy func(net.IP, int) bool
	dial   func(net.IP, int) (udpFlowSocket, error)

	mu     sync.Mutex
	flows  map[string]*udpFlow
	client *net.UDPAddr

	datagramsIn  atomic.Uint64
	datagramsOut atomic.Uint64
	bytesIn      atomic.Uint64
	bytesOut     atomic.Uint64

	reportedIn       uint64
	reportedOut      uint64
	reportedBytesIn  uint64
	reportedBytesOut uint64
	lastReport       time.Time
}

func newUDPAssociation(
	cfg Config,
	relay udpRelaySocket,
	source net.IP,
	request socksRequest,
	idle time.Duration,
	limiter *udpRelay,
	tracker *runtimeTracker,
	log *logger,
) *udpAssociation {
	a := &udpAssociation{
		cfg:        cfg,
		relay:      relay,
		source:     source,
		idle:       idle,
		tracker:    tracker,
		log:        log,
		policy:     limiter.policy,
		dial:       limiter.dial,
		flows:      make(map[string]*udpFlow),
		lastReport: time.Now(),
	}
	if a.policy == nil {
		a.policy = destinationAllowed
	}
	if a.dial == nil {
		a.dial = dialUDPDestination
	}
	// A client that names its UDP source port in the request is pinned to it
	// from the first datagram; otherwise the first datagram pins the port.
	if request.port != 0 {
		a.client = &net.UDPAddr{IP: append(net.IP(nil), source...), Port: request.port}
	}
	return a
}

// run reads datagrams until the control connection closes or the association
// stays silent for the idle timeout. The reader is the control connection's
// buffered reader, so bytes the protocol sniff already consumed are not lost.
func (a *udpAssociation) run(reader *bufio.Reader) {
	control := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, reader)
		close(control)
	}()

	defer a.report()
	defer a.closeFlows()

	last := time.Now()
	buf := make([]byte, udpRelayBuffer)
	for {
		_ = a.relay.SetReadDeadline(time.Now().Add(time.Second))
		n, from, err := a.relay.ReadFromUDP(buf)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				select {
				case <-control:
					return
				default:
				}
				if time.Since(last) >= a.idle {
					a.log.debug("client %s: UDP association idle timeout", a.source)
					return
				}
				continue
			}
			return
		}
		if !a.acceptSource(from) {
			a.log.debug("client %s: UDP datagram from %s dropped: source mismatch", a.source, from)
			continue
		}
		last = time.Now()
		a.handle(buf[:n])
		if time.Since(a.lastReport) >= udpReportInterval {
			a.report()
		}
	}
}

// acceptSource enforces RFC 1928: only the address that owns the control
// connection may use the relay, and after the first datagram the UDP source
// port is pinned as well.
func (a *udpAssociation) acceptSource(from *net.UDPAddr) bool {
	if from == nil || from.IP == nil || !from.IP.Equal(a.source) {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil {
		a.client = &net.UDPAddr{
			IP:   append(net.IP(nil), from.IP...),
			Port: from.Port,
			Zone: from.Zone,
		}
		return true
	}
	return a.client.Port == from.Port && a.client.IP.Equal(from.IP)
}

func (a *udpAssociation) handle(packet []byte) {
	frag, host, port, payload, ok := parseSocksUDPDatagram(packet)
	if !ok {
		a.log.debug("client %s: malformed UDP datagram", a.source)
		return
	}
	if frag != 0 {
		// Fragment reassembly is not implemented: a client that needs large
		// datagrams must let the transport do the fragmentation.
		a.log.debug("client %s: UDP FRAG=%d is not supported", a.source, frag)
		return
	}
	destination := net.ParseIP(host)
	if destination == nil {
		if !a.cfg.DomainResolution || a.dialer == nil {
			a.log.debug("client %s: UDP domain %q is not accepted", a.source, host)
			return
		}
		resolved, err := a.dialer.resolveHost(context.Background(), host,
			func(ip net.IP) bool { return a.policy(ip, port) })
		if err != nil {
			a.log.info("client %s: UDP %s:%d: %v", a.source, host, port, err)
			return
		}
		destination = resolved
	}
	if !a.policy(destination, port) {
		a.log.info("client %s: UDP destination %s refused by policy", a.source, joinHostPort(destination, port))
		return
	}
	if portBlocked(port, a.cfg.BlockedPorts) {
		a.log.info("client %s: UDP port %d is blocked", a.source, port)
		return
	}
	flow := a.flowFor(destination, port)
	if flow == nil {
		return
	}
	if _, err := flow.conn.Write(payload); err != nil {
		a.log.debug("client %s: UDP send to %s: %v", a.source, flow.key, err)
		a.dropFlow(flow)
		return
	}
	a.datagramsIn.Add(1)
	a.bytesIn.Add(uint64(len(payload)))
}

func (a *udpAssociation) flowFor(destination net.IP, port int) *udpFlow {
	key := joinHostPort(destination, port)
	a.mu.Lock()
	if flow, ok := a.flows[key]; ok {
		a.mu.Unlock()
		return flow
	}
	if len(a.flows) >= udpRelayMaxFlows {
		a.mu.Unlock()
		a.log.warn("client %s: UDP flow limit reached, dropping %s", a.source, key)
		return nil
	}
	conn, err := a.dial(destination, port)
	if err != nil {
		a.mu.Unlock()
		a.log.debug("client %s: UDP dial %s: %v", a.source, key, err)
		return nil
	}
	flow := &udpFlow{
		conn: conn,
		ip:   append(net.IP(nil), destination...),
		port: port,
		key:  key,
	}
	a.flows[key] = flow
	a.mu.Unlock()
	go a.pumpFlow(flow)
	return flow
}

// pumpFlow carries the answers of one destination back to the client. The read
// deadline is the idle timeout, so a flow with no traffic releases its socket
// by itself.
func (a *udpAssociation) pumpFlow(flow *udpFlow) {
	defer a.dropFlow(flow)
	buf := make([]byte, udpRelayBuffer)
	for {
		_ = flow.conn.SetReadDeadline(time.Now().Add(a.idle))
		n, err := flow.conn.Read(buf)
		if err != nil {
			return
		}
		a.mu.Lock()
		client := a.client
		a.mu.Unlock()
		if client == nil {
			continue
		}
		packet := buildSocksUDPDatagram(flow.ip, flow.port, buf[:n])
		if _, err := a.relay.WriteToUDP(packet, client); err != nil {
			return
		}
		a.datagramsOut.Add(1)
		a.bytesOut.Add(uint64(n))
	}
}

func (a *udpAssociation) dropFlow(flow *udpFlow) {
	a.mu.Lock()
	if current, ok := a.flows[flow.key]; ok && current == flow {
		delete(a.flows, flow.key)
	}
	a.mu.Unlock()
	_ = flow.conn.Close()
}

func (a *udpAssociation) closeFlows() {
	a.mu.Lock()
	flows := make([]*udpFlow, 0, len(a.flows))
	for key, flow := range a.flows {
		flows = append(flows, flow)
		delete(a.flows, key)
	}
	a.mu.Unlock()
	for _, flow := range flows {
		_ = flow.conn.Close()
	}
}

// report publishes the counters the association accumulated since the last
// call. The runtime file is only refreshed every few seconds, so a per-second
// update is enough.
func (a *udpAssociation) report() {
	in := a.datagramsIn.Load()
	out := a.datagramsOut.Load()
	bytesIn := a.bytesIn.Load()
	bytesOut := a.bytesOut.Load()
	deltaIn := in - a.reportedIn
	deltaOut := out - a.reportedOut
	deltaBytesIn := bytesIn - a.reportedBytesIn
	deltaBytesOut := bytesOut - a.reportedBytesOut
	a.reportedIn = in
	a.reportedOut = out
	a.reportedBytesIn = bytesIn
	a.reportedBytesOut = bytesOut
	a.lastReport = time.Now()
	if a.tracker != nil && (deltaIn > 0 || deltaOut > 0 || deltaBytesIn > 0 || deltaBytesOut > 0) {
		a.tracker.udpTraffic(a.source, deltaIn, deltaOut, deltaBytesIn, deltaBytesOut)
	}
	if a.usage != nil && (deltaBytesIn > 0 || deltaBytesOut > 0) {
		a.usage.addTraffic(a.source, deltaBytesIn, deltaBytesOut)
	}
}

// udpFlow is one destination of an association: a connected UDP socket plus the
// header needed to send its answers back.
type udpFlow struct {
	conn udpFlowSocket
	ip   net.IP
	port int
	key  string
}

// parseSocksUDPDatagram parses the RFC 1928 UDP request header. The host is
// returned as a string: either the textual form of an address or a domain name
// that the caller resolves.
func parseSocksUDPDatagram(packet []byte) (frag byte, host string, port int, payload []byte, ok bool) {
	if len(packet) < 4 {
		return 0, "", 0, nil, false
	}
	if packet[0] != 0 || packet[1] != 0 {
		return 0, "", 0, nil, false
	}
	frag = packet[2]
	offset := 4
	switch packet[3] {
	case socksAtypIPv4:
		if len(packet) < offset+4+2 {
			return 0, "", 0, nil, false
		}
		host = net.IP(packet[offset : offset+4]).String()
		offset += 4
	case socksAtypIPv6:
		if len(packet) < offset+16+2 {
			return 0, "", 0, nil, false
		}
		host = net.IP(packet[offset : offset+16]).String()
		offset += 16
	case socksAtypDomain:
		if len(packet) < offset+1 {
			return 0, "", 0, nil, false
		}
		length := int(packet[offset])
		offset++
		if length == 0 || len(packet) < offset+length+2 {
			return 0, "", 0, nil, false
		}
		host = string(packet[offset : offset+length])
		offset += length
	default:
		return 0, "", 0, nil, false
	}
	port = int(binary.BigEndian.Uint16(packet[offset : offset+2]))
	offset += 2
	return frag, host, port, packet[offset:], true
}

// buildSocksUDPDatagram wraps one payload in the RFC 1928 UDP response header.
func buildSocksUDPDatagram(ip net.IP, port int, payload []byte) []byte {
	var packet []byte
	if v4 := ip.To4(); v4 != nil {
		packet = make([]byte, 0, 10+len(payload))
		packet = append(packet, 0, 0, 0, socksAtypIPv4)
		packet = append(packet, v4...)
	} else {
		packet = make([]byte, 0, 22+len(payload))
		packet = append(packet, 0, 0, 0, socksAtypIPv6)
		packet = append(packet, ip.To16()...)
	}
	packet = binary.BigEndian.AppendUint16(packet, uint16(port))
	return append(packet, payload...)
}
