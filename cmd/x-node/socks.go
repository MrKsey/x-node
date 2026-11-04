package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	socksVersion = 5

	socksCmdConnect      = 1
	socksCmdUDPAssociate = 3

	socksAtypIPv4   = 1
	socksAtypDomain = 3
	socksAtypIPv6   = 4

	socksReplySuccess          = 0
	socksReplyGeneralFailure   = 1
	socksReplyNotAllowed       = 2
	socksReplyNetworkUnreach   = 3
	socksReplyHostUnreach      = 4
	socksReplyConnRefused      = 5
	socksReplyTTLExpired       = 6
	socksReplyCommandNotSupp   = 7
	socksReplyAddrNotSupported = 8
)

type socksServer struct {
	cfg     Config
	live    *settings
	store   *Store
	tracker *runtimeTracker
	usage   *usageStore
	quotas  *quotaStore
	log     *logger
	dialer  *outboundDialer

	// api is the HTTP mux served on this listener when apiPort is 0. The first
	// byte of a connection tells SOCKS5 (0x05) from HTTP (a method letter).
	api http.Handler

	// udp owns the UDP associations. It has its own limit on top of the TCP
	// connection limits because one association is a long-lived relay socket.
	udp *udpRelay

	// sourceOK is the Yggdrasil source check. It is a field so the UDP tests can
	// run over loopback; production always uses isYggAddress.
	sourceOK func(net.IP) bool

	// policy decides which destinations may leave through the exit. It is a
	// field so the CONNECT test can reach a loopback echo server; production
	// always uses destinationAllowed.
	policy func(net.IP, int) bool

	total     atomic.Int64
	mu        sync.Mutex
	perClient map[string]int
}

func newSocksServer(cfg Config, store *Store, tracker *runtimeTracker, log *logger, api http.Handler) *socksServer {
	return &socksServer{
		cfg:       cfg,
		store:     store,
		tracker:   tracker,
		log:       log,
		api:       api,
		udp:       newUDPRelay(cfg),
		sourceOK:  isYggAddress,
		policy:    destinationAllowed,
		perClient: make(map[string]int),
	}
}

// config returns the configuration in effect right now. The web interface can
// change most limits without a restart, so a long-lived listener must not keep
// the copy it was started with.
func (s *socksServer) config() Config {
	if s.live != nil {
		return s.live.get()
	}
	return s.cfg
}

func (s *socksServer) listenAndServe(ctx context.Context) error {
	cfg := s.config()
	addr := net.JoinHostPort(cfg.ListenAddress, strconv.Itoa(cfg.SocksPort))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen SOCKS5 on %s: %w", addr, err)
	}
	s.log.info("SOCKS5 listening on %s", addr)
	if cfg.apiShared() {
		s.log.info("registration API shares the SOCKS5 listener")
	}
	if cfg.UDPEnabled {
		s.log.info("SOCKS5 UDP ASSOCIATE enabled (max %d association(s))", cfg.MaxUDPAssociations)
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	return s.serve(ctx, listener)
}

// serve accepts connections until the listener is closed. It is separate from
// listenAndServe so tests can hand it an already bound listener.
func (s *socksServer) serve(ctx context.Context, listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.warn("accept failed: %v", err)
			continue
		}
		go s.serveConn(conn)
	}
}

func (s *socksServer) serveConn(conn net.Conn) {
	source := remoteTCPIP(conn)
	allowSource := s.sourceOK
	if allowSource == nil {
		allowSource = isYggAddress
	}
	if !allowSource(source) {
		s.log.warn("connection from %s refused: not a Yggdrasil address", conn.RemoteAddr())
		_ = conn.Close()
		return
	}
	reader := bufio.NewReader(conn)
	first, err := reader.Peek(1)
	if err != nil {
		_ = conn.Close()
		return
	}
	// A SOCKS5 connection starts with its version byte. Anything else on a
	// shared listener is an HTTP request for the registration API; when the API
	// has its own port a stray byte is still a protocol error.
	if first[0] != socksVersion {
		if s.config().apiShared() && s.api != nil {
			s.serveHTTP(conn, reader)
			return
		}
		s.log.debug("connection from %s: not a SOCKS5 greeting", source)
		_ = conn.Close()
		return
	}
	defer conn.Close()

	if !s.acquire(source) {
		s.log.warn("connection from %s refused: connection limit", source)
		if s.tracker != nil {
			s.tracker.refused(source)
		}
		if s.usage != nil {
			s.usage.addRefused(source)
		}
		return
	}
	defer s.release(source)

	cfg := s.config()
	if !cfg.AllowAll && !s.store.Allowed(source) {
		s.log.info("client %s is not in the allowlist", source)
		// The SOCKS reply is sent after the request is parsed, so the client
		// gets a proper "not allowed" instead of a reset.
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		_ = readGreeting(reader)
		_ = writeMethod(conn)
		if _, err := readRequest(reader); err == nil {
			_ = writeReply(conn, socksReplyNotAllowed, net.IPv4zero, 0)
		}
		return
	}
	if allowed, reason := s.quotas.allow(source, s.usage); !allowed {
		s.log.info("client %s refused: %s", source, reason)
		if s.usage != nil {
			s.usage.addRefused(source)
		}
		if s.tracker != nil {
			s.tracker.quotaRefused(source)
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		_ = readGreeting(reader)
		_ = writeMethod(conn)
		if _, err := readRequest(reader); err == nil {
			_ = writeReply(conn, socksReplyNotAllowed, net.IPv4zero, 0)
		}
		return
	}
	s.store.Touch(source)
	if s.tracker != nil {
		s.tracker.touch(source)
	}

	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := readGreeting(reader); err != nil {
		s.log.debug("client %s: greeting: %v", source, err)
		return
	}
	if err := writeMethod(conn); err != nil {
		return
	}
	request, err := readRequest(reader)
	if err != nil {
		s.log.debug("client %s: request: %v", source, err)
		return
	}
	switch request.cmd {
	case socksCmdConnect:
		s.serveConnect(conn, source, request)
	case socksCmdUDPAssociate:
		s.serveUDPAssociate(conn, reader, source, request)
	default:
		s.log.debug("client %s: unsupported command %d", source, request.cmd)
		_ = writeReply(conn, socksReplyCommandNotSupp, nil, 0)
	}
}

// serveConnect carries one TCP stream to its destination.
func (s *socksServer) serveConnect(conn net.Conn, source net.IP, request socksRequest) {
	cfg := s.config()
	if request.atyp == socksAtypDomain && !cfg.DomainResolution {
		_ = writeReply(conn, socksReplyAddrNotSupported, net.IPv4zero, 0)
		return
	}
	policy := s.policy
	if policy == nil {
		policy = destinationAllowed
	}
	if portBlocked(request.port, cfg.BlockedPorts) {
		s.log.info("client %s: port %d is blocked", source, request.port)
		_ = writeReply(conn, socksReplyNotAllowed, net.IPv4zero, 0)
		return
	}
	allow := func(ip net.IP) bool { return policy(ip, request.port) }
	host := request.host()
	if host == "" {
		_ = writeReply(conn, socksReplyAddrNotSupported, net.IPv4zero, 0)
		return
	}
	if request.atyp != socksAtypDomain && !allow(request.ip) {
		s.log.info("client %s: destination %s refused by policy", source, joinHostPort(request.ip, request.port))
		_ = writeReply(conn, socksReplyNotAllowed, net.IPv4zero, 0)
		return
	}

	target := net.JoinHostPort(host, strconv.Itoa(request.port))
	dialer := s.dialer
	if dialer == nil {
		dialer = newOutboundDialer(cfg, func() string { return cfg.ipv6Mode() }, nil)
	}
	upstream, usedIP, err := dialer.DialHost(context.Background(), host, request.port, allow)
	if err != nil {
		code := socksReplyGeneralFailure
		var netErr net.Error
		switch {
		case errors.Is(err, errDestinationRefused):
			code = socksReplyNotAllowed
		case errors.Is(err, errNoRoute):
			code = socksReplyHostUnreach
		case errors.Is(err, context.DeadlineExceeded):
			code = socksReplyTTLExpired
		case errors.As(err, &netErr) && netErr.Timeout():
			code = socksReplyTTLExpired
		case isRefused(err):
			code = socksReplyConnRefused
		case isHostUnreachable(err):
			code = socksReplyHostUnreach
		}
		if errors.Is(err, errNoRoute) {
			s.log.info("client %s: %s: %v (IPv6 mode %s)", source, target, err, cfg.ipv6Mode())
		} else {
			s.log.debug("client %s: dial %s: %v", source, target, err)
		}
		_ = writeReply(conn, byte(code), net.IPv4zero, 0)
		return
	}
	defer upstream.Close()
	if usedIP != nil && !usedIP.Equal(request.ip) {
		s.log.debug("client %s -> %s resolved to %s", source, host, usedIP)
	}

	if tcp, ok := upstream.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(30 * time.Second)
		_ = tcp.SetNoDelay(true)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	bound := upstream.LocalAddr().(*net.TCPAddr)
	if err := writeReply(conn, socksReplySuccess, bound.IP, bound.Port); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})

	idle, _ := cfg.idleTimeout()
	s.log.debug("client %s -> %s established", source, target)
	if s.tracker != nil {
		s.tracker.open(source, target)
		defer s.tracker.close(source)
	}
	if s.usage != nil {
		s.usage.addConnection(source)
	}

	// One bucket pair per client, shared by all of its connections: a speed
	// limit is the rate of the client, not of a single TCP stream.
	up, down := s.quotas.Limiters(source)
	relayMetered(conn, upstream, idle, up, down, func(rx, tx uint64) {
		if s.usage != nil {
			s.usage.addTraffic(source, rx, tx)
		}
	})
	s.log.debug("client %s -> %s closed", source, target)
}

// serveUDPAssociate starts one RFC 1928 UDP relay for the client. The TCP
// control connection stays open for the whole association: its close is what
// ends the relay, and its source address is what authorises the datagrams.
func (s *socksServer) serveUDPAssociate(conn net.Conn, reader *bufio.Reader, source net.IP, request socksRequest) {
	cfg := s.config()
	if !cfg.UDPEnabled {
		s.log.info("client %s: UDP ASSOCIATE refused: UDP is disabled", source)
		_ = writeReply(conn, socksReplyCommandNotSupp, nil, 0)
		return
	}
	if request.atyp == socksAtypDomain {
		_ = writeReply(conn, socksReplyAddrNotSupported, nil, 0)
		return
	}
	if !s.udp.acquire(source) {
		s.log.warn("client %s: UDP ASSOCIATE refused: association limit", source)
		if s.tracker != nil {
			s.tracker.refused(source)
		}
		if s.usage != nil {
			s.usage.addRefused(source)
		}
		// Not "not allowed": the client is allowlisted and the exit is fine,
		// it is only out of association slots. A denial reply would make the
		// app blacklist this exit for TCP as well.
		_ = writeReply(conn, socksReplyGeneralFailure, nil, 0)
		return
	}
	defer s.udp.release(source)

	bindIP := net.ParseIP(cfg.ListenAddress)
	relay, err := net.ListenUDP("udp", &net.UDPAddr{IP: bindIP, Port: 0})
	if err != nil {
		s.log.warn("client %s: UDP relay bind on %s: %v", source, cfg.ListenAddress, err)
		_ = writeReply(conn, socksReplyGeneralFailure, nil, 0)
		return
	}
	defer relay.Close()

	local := relay.LocalAddr().(*net.UDPAddr)
	if err := writeReply(conn, socksReplySuccess, local.IP, local.Port); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})

	idle, _ := cfg.udpIdleTimeout()
	association := newUDPAssociation(cfg, relay, source, request, idle, s.udp, s.tracker, s.log)
	association.usage = s.usage
	association.dialer = s.dialer
	association.up, association.down = s.quotas.Limiters(source)
	s.log.info("client %s: UDP association on %s", source, local)
	if s.tracker != nil {
		s.tracker.udpOpen(source)
		defer s.tracker.udpClose(source)
	}
	if s.usage != nil {
		s.usage.addUDPAssociation(source)
	}
	association.run(reader)
	s.log.info("client %s: UDP association closed (%d datagram(s) in, %d out)",
		source, association.datagramsIn.Load(), association.datagramsOut.Load())
}

// trackSource records that a client is alive. Only a client the exit would
// actually serve is recorded: a health check from the exit itself, or a probe
// from a stranger, must not appear in `x-node clients` as an open client
// that was never registered.
func (s *socksServer) trackSource(source net.IP) {
	if source == nil {
		return
	}
	if !s.config().AllowAll && !s.store.Allowed(source) {
		return
	}
	s.store.Touch(source)
	if s.tracker != nil {
		s.tracker.touch(source)
	}
}

// serveHTTP answers one API request on the shared listener. The source address
// is the same one the API sees as RemoteAddr, so its Yggdrasil and allowlist
// checks keep working unchanged.
func (s *socksServer) serveHTTP(conn net.Conn, reader *bufio.Reader) {
	source := remoteTCPIP(conn)
	s.trackSource(source)
	server := &http.Server{
		Handler:           s.api,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	// Serve hands the single connection to a handler goroutine and returns as
	// soon as the listener reports that no more connections are coming. The
	// handler goroutine owns the socket from there and closes it itself.
	_ = server.Serve(&oneConnListener{
		conn: &bufferedConn{Conn: conn, reader: reader},
	})
}

// bufferedConn keeps the bytes the protocol sniff already read.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// oneConnListener is a net.Listener that yields a single connection and then
// reports that it is closed. It lets http.Server serve a connection that was
// accepted by another protocol's loop.
type oneConnListener struct {
	conn  net.Conn
	taken atomic.Bool
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.taken.CompareAndSwap(false, true) {
		return l.conn, nil
	}
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error { return nil }

func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

func (s *socksServer) acquire(source net.IP) bool {
	key := clientKey(source)
	cfg := s.config()
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg.MaxConnsTotal > 0 && s.total.Load() >= int64(cfg.MaxConnsTotal) {
		return false
	}
	if cfg.MaxConnsPerClient > 0 && s.perClient[key] >= cfg.MaxConnsPerClient {
		return false
	}
	s.perClient[key]++
	s.total.Add(1)
	return true
}

func (s *socksServer) release(source net.IP) {
	key := clientKey(source)
	s.mu.Lock()
	if s.perClient[key] > 1 {
		s.perClient[key]--
	} else {
		delete(s.perClient, key)
	}
	s.mu.Unlock()
	s.total.Add(-1)
}

func remoteTCPIP(conn net.Conn) net.IP {
	if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		return addr.IP
	}
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// readGreeting consumes the method negotiation and returns the methods the
// client offered. Only "no authentication" is supported: the client is already
// authenticated by its Yggdrasil source address.
func readGreeting(conn io.Reader) error {
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return err
	}
	if header[0] != socksVersion {
		return fmt.Errorf("unsupported SOCKS version %d", header[0])
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}
	for _, method := range methods {
		if method == 0 {
			return nil
		}
	}
	return errors.New("the client offered no supported authentication method")
}

func writeMethod(conn net.Conn) error {
	_, err := conn.Write([]byte{socksVersion, 0})
	return err
}

// socksRequest is a parsed SOCKS5 request. For CONNECT the address is the
// destination; for UDP ASSOCIATE it is the address the client will send its
// datagrams from (all zeroes when the client does not know it yet).
type socksRequest struct {
	cmd    byte
	atyp   byte
	ip     net.IP
	domain string
	port   int
}

// host returns the destination as a name or as the textual form of an address.
func (r socksRequest) host() string {
	if r.atyp == socksAtypDomain {
		return r.domain
	}
	if r.ip == nil {
		return ""
	}
	return r.ip.String()
}

// readRequest parses a SOCKS5 request. A domain name is kept as a string: the
// exit resolves it itself, so the destination policy can be applied to every
// result instead of trusting the client.
func readRequest(conn io.Reader) (socksRequest, error) {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return socksRequest{}, err
	}
	if header[0] != socksVersion {
		return socksRequest{}, fmt.Errorf("unsupported SOCKS version %d", header[0])
	}
	if header[1] != socksCmdConnect && header[1] != socksCmdUDPAssociate {
		return socksRequest{}, fmt.Errorf("unsupported command %d", header[1])
	}
	request := socksRequest{cmd: header[1], atyp: header[3]}
	switch request.atyp {
	case socksAtypIPv4:
		raw := make([]byte, 4)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return socksRequest{}, err
		}
		request.ip = net.IP(raw)
	case socksAtypIPv6:
		raw := make([]byte, 16)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return socksRequest{}, err
		}
		request.ip = net.IP(raw)
	case socksAtypDomain:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return socksRequest{}, err
		}
		if length[0] == 0 {
			return socksRequest{}, errors.New("empty domain name")
		}
		raw := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, raw); err != nil {
			return socksRequest{}, err
		}
		request.domain = string(raw)
	default:
		return socksRequest{}, fmt.Errorf("unsupported address type %d", request.atyp)
	}
	var portRaw [2]byte
	if _, err := io.ReadFull(conn, portRaw[:]); err != nil {
		return socksRequest{}, err
	}
	request.port = int(binary.BigEndian.Uint16(portRaw[:]))
	return request, nil
}

// writeReply writes a SOCKS5 reply. A UDP ASSOCIATE reply carries the
// Yggdrasil address of the relay, so the address type follows the address
// itself; CONNECT replies keep the IPv4 form they always had.
func writeReply(conn net.Conn, code byte, ip net.IP, port int) error {
	var reply []byte
	if v4 := ip.To4(); v4 != nil {
		reply = make([]byte, 10)
		reply[3] = socksAtypIPv4
		copy(reply[4:8], v4)
		binary.BigEndian.PutUint16(reply[8:10], uint16(port))
	} else if v6 := ip.To16(); v6 != nil {
		reply = make([]byte, 22)
		reply[3] = socksAtypIPv6
		copy(reply[4:20], v6)
		binary.BigEndian.PutUint16(reply[20:22], uint16(port))
	} else {
		reply = make([]byte, 10)
		reply[3] = socksAtypIPv4
		binary.BigEndian.PutUint16(reply[8:10], uint16(port))
	}
	reply[0] = socksVersion
	reply[1] = code
	reply[2] = 0
	_, err := conn.Write(reply)
	return err
}

func relayIdle(a, b net.Conn, idle time.Duration) {
	relayMetered(a, b, idle, nil, nil, nil)
}

// relayMetered copies both directions, throttles them when a shaper is given
// and reports the bytes carried. The callback is called at most once per
// second per direction, so a busy stream does not take the accounting lock per
// read.
func relayMetered(client, upstream net.Conn, idle time.Duration, up, down *shaper, report func(rx, tx uint64)) {
	var wg sync.WaitGroup
	wg.Add(2)
	go copyDirection(upstream, client, idle, up, report, true, &wg)
	go copyDirection(client, upstream, idle, down, report, false, &wg)
	wg.Wait()
}

func copyDirection(dst, src net.Conn, idle time.Duration, limiter *shaper, report func(rx, tx uint64), upload bool, wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	var pending uint64
	lastReport := time.Now()

	flush := func() {
		if report == nil || pending == 0 {
			return
		}
		if upload {
			report(pending, 0)
		} else {
			report(0, pending)
		}
		pending = 0
		lastReport = time.Now()
	}
	defer flush()

	for {
		if idle > 0 {
			_ = src.SetReadDeadline(time.Now().Add(idle))
		}
		n, err := src.Read(buf)
		if n > 0 {
			limiter.wait(n)
			if idle > 0 {
				_ = dst.SetWriteDeadline(time.Now().Add(idle))
			}
			written, werr := dst.Write(buf[:n])
			if written > 0 {
				pending += uint64(written)
				if time.Since(lastReport) >= time.Second {
					flush()
				}
			}
			if werr != nil {
				break
			}
		}
		if err != nil {
			if tcp, ok := dst.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			}
			break
		}
	}
}

func isRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

func isHostUnreachable(err error) bool {
	return errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH)
}
