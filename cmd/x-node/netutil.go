package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Outbound address selection.
//
// A client may send a destination as an IP address (ATYP=ipv4/ipv6) or as a
// domain name (ATYP=domain). In the second case the exit resolves the name,
// checks every result against the destination policy and connects to the first
// address that answers. IPv6 and IPv4 are tried in parallel with the 250 ms
// stagger of RFC 8305 (Happy Eyeballs), so a host without a working IPv6 route
// costs at most a quarter of a second instead of a timeout.

const (
	// happyEyeballsDelay is the pause before the next candidate is tried.
	happyEyeballsDelay = 250 * time.Millisecond
	// maxDialCandidates caps the addresses tried for one name: a host with ten
	// AAAA records must not turn one SOCKS request into ten dials.
	maxDialCandidates = 6
	// ipv6ProbeAddress is a global address used only to ask the kernel which
	// source address and route it would pick. A UDP "dial" sends no packet.
	ipv6ProbeAddress = "[2606:4700:4700::1111]:53"
	// ipv6RefreshInterval is how often the availability is re-checked: a VPS
	// can gain or lose IPv6 without the daemon restarting.
	ipv6RefreshInterval = 5 * time.Minute
	// ipv6ProbeTimeout bounds one reachability attempt. A provider that
	// blackholes IPv6 would otherwise cost the whole dial timeout.
	ipv6ProbeTimeout = 2 * time.Second
)

// The states the web interface and the API report for outbound IPv6.
const (
	// ipv6StateChecking is the first state of the daemon: the address check
	// has finished, the reachability probe is still running.
	ipv6StateChecking = "checking"
	// ipv6StateOK means a global address exists and a real IPv6 destination
	// answered, so outbound IPv6 connections work.
	ipv6StateOK = "ok"
	// ipv6StateNone means the host has no global IPv6 address at all.
	ipv6StateNone = "none"
	// ipv6StateBroken means the host has an address, but the provider does not
	// route it to the internet. This is the case a plain "is an address
	// assigned?" check misses, and the reason the reachability probe exists.
	ipv6StateBroken = "broken"
)

// ipv6ProbeTargets are real global IPv6 destinations. Two are used so a single
// blocked anycast address does not look like a broken provider.
var ipv6ProbeTargets = []string{
	"[2606:4700:4700::1111]:443", // Cloudflare
	"[2001:4860:4860::8888]:443", // Google
}

// errNoRoute means that the name resolved but no address passed the policy or
// the address family filter.
var errNoRoute = errors.New("no usable address")

// ipv6Detection is the result of one check.
type ipv6Detection struct {
	// Available is true only when an address exists and the route works.
	Available bool
	State     string
	// Source is the global address the kernel picked, empty without one.
	Source string
	// Detail explains a state that is not ok: why there is no address or why
	// the assigned one cannot reach the internet.
	Detail string
}

// ipv6Monitor caches whether this host can open a global IPv6 connection.
type ipv6Monitor struct {
	mu        sync.RWMutex
	available bool
	state     string
	source    string
	detail    string
	iface     string
	checked   time.Time
}

func newIPv6Monitor() *ipv6Monitor {
	monitor := &ipv6Monitor{state: ipv6StateChecking, detail: "checking the IPv6 route"}
	// The address check is instant: a UDP connect sends no packet. The
	// reachability probe follows in run(), so a provider that blackholes IPv6
	// does not delay the daemon start.
	monitor.refreshLocal()
	return monitor
}

// refreshLocal fills the state from the address check alone. Until the
// reachability probe finishes, an address is not treated as usable.
func (m *ipv6Monitor) refreshLocal() {
	detection := detectIPv6Address()
	state := detection.State
	detail := detection.Detail
	if state == ipv6StateOK {
		state = ipv6StateChecking
		detail = "checking the route"
	}
	iface := ""
	if detection.Source != "" {
		iface = interfaceForIP(net.ParseIP(detection.Source))
	}
	m.mu.Lock()
	m.available = false
	m.state = state
	m.source = detection.Source
	m.detail = detail
	m.iface = iface
	m.checked = time.Now()
	m.mu.Unlock()
}

// refresh runs the full check: the address and a real connection to a global
// IPv6 destination.
func (m *ipv6Monitor) refresh() {
	detection := detectIPv6()
	iface := ""
	if detection.Source != "" {
		iface = interfaceForIP(net.ParseIP(detection.Source))
	}
	m.mu.Lock()
	m.available = detection.Available
	m.state = detection.State
	m.source = detection.Source
	m.detail = detection.Detail
	m.iface = iface
	m.checked = time.Now()
	m.mu.Unlock()
}

func (m *ipv6Monitor) run(ctx context.Context) {
	// The first probe runs right away: the constructor only checked that an
	// address exists.
	m.refresh()
	ticker := time.NewTicker(ipv6RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refresh()
		}
	}
}

func (m *ipv6Monitor) status() (available bool, state, source, detail, iface string) {
	if m == nil {
		return false, ipv6StateNone, "", "not checked", ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.available, m.state, m.source, m.detail, m.iface
}

// detectIPv6Address asks the kernel which source address it would use for a
// global IPv6 destination. A UDP connect does not send anything, so this works
// even when the host is offline, and it tells "no address" apart from "an
// address that cannot reach anything".
func detectIPv6Address() ipv6Detection {
	conn, err := net.Dial("udp6", ipv6ProbeAddress)
	if err != nil {
		return ipv6Detection{State: ipv6StateNone, Detail: err.Error()}
	}
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	_ = conn.Close()
	if !ok || local.IP == nil {
		return ipv6Detection{State: ipv6StateNone, Detail: "no local IPv6 address"}
	}
	ip := local.IP
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return ipv6Detection{State: ipv6StateNone, Detail: "only " + ip.String()}
	}
	return ipv6Detection{State: ipv6StateOK, Source: ip.String()}
}

// detectIPv6 reports whether outbound IPv6 connections really work. An address
// is not enough: a provider can leave the interface up and drop every IPv6
// packet, which used to be reported as "available" and made the exit try IPv6
// destinations that could only time out.
func detectIPv6() ipv6Detection {
	address := detectIPv6Address()
	if address.State != ipv6StateOK {
		return address
	}
	if err := probeIPv6Route(); err != nil {
		return ipv6Detection{
			State:  ipv6StateBroken,
			Source: address.Source,
			Detail: "no IPv6 destination answered: " + err.Error(),
		}
	}
	return ipv6Detection{Available: true, State: ipv6StateOK, Source: address.Source}
}

// probeIPv6Route opens a TCP connection to a well-known global IPv6 address.
// The exit carries ordinary internet traffic, so a successful handshake is the
// proof that the provider routes the address.
func probeIPv6Route() error {
	var lastErr error
	for _, target := range ipv6ProbeTargets {
		dialer := net.Dialer{Timeout: ipv6ProbeTimeout}
		conn, err := dialer.Dial("tcp6", target)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no probe target")
	}
	return lastErr
}

// ipv6StateText is the one-line description the daemon log, "x-node status"
// and "x-node doctor" use. It keeps "no address" apart from "an address the
// provider does not route", because the second case needs a different fix.
func ipv6StateText(state, source, detail, iface string) string {
	switch state {
	case ipv6StateChecking:
		if source != "" {
			return "checking the route from " + source
		}
		return "checking the route"
	case ipv6StateNone:
		return "no global IPv6 address (" + detail + ")"
	case ipv6StateBroken:
		return source + " is assigned, but the provider does not route IPv6 (" + detail + ")"
	default:
		text := "available via " + source
		if iface != "" {
			text += " on " + iface
		}
		return text
	}
}

// interfaceForIP names the interface that carries the address. The settings
// page shows it next to the source address, so an operator can tell which link
// outbound IPv6 traffic leaves through without running "ip -6 addr".
func interfaceForIP(ip net.IP) string {
	if ip == nil {
		return ""
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			switch value := addr.(type) {
			case *net.IPNet:
				if value.IP.Equal(ip) {
					return iface.Name
				}
			case *net.IPAddr:
				if value.IP.Equal(ip) {
					return iface.Name
				}
			}
		}
	}
	return ""
}

// listenOnAssignedAddress binds a TCP listener on an address that may not be
// assigned to an interface yet.
//
// The daemon listens on the Yggdrasil address, and yggdrasil assigns that
// address only after its own start: on a boot, or right after "systemctl
// restart yggdrasil", net.Listen fails with EADDRNOTAVAIL. The daemon used to
// exit on that error, so systemd had to start it again and the web interface
// was down in between. Waiting keeps the rest of the daemon alive: the listener
// is created as soon as the address appears, and a listener that is already
// bound keeps working when the address disappears and comes back.
func listenOnAssignedAddress(ctx context.Context, addr string, log *logger, what string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	// A wildcard address or a host name is not tied to an interface: bind it
	// right away and report a real error.
	if ip == nil || ip.IsUnspecified() {
		return net.Listen("tcp", addr)
	}
	delay := time.Second
	warned := false
	for {
		if interfaceForIP(ip) == "" {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !warned {
				log.warn("%s: %s is not assigned to any interface yet, waiting for yggdrasil", what, host)
				warned = true
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			if delay < 5*time.Second {
				delay += time.Second
			}
			continue
		}
		listener, err := net.Listen("tcp", addr)
		if err == nil {
			return listener, nil
		}
		// The address can vanish between the check and the bind. Wait for it
		// again; a failure while the address is present (a busy port, a
		// permission) is a real error and is returned.
		if interfaceForIP(ip) != "" {
			return nil, err
		}
	}
}

// isGlobalIPv6 reports whether the address is a routable IPv6 destination, as
// opposed to loopback, link-local, unique-local or the Yggdrasil mesh.
func isGlobalIPv6(ip net.IP) bool {
	v6 := ip.To16()
	if v6 == nil || ip.To4() != nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if isYggAddress(ip) {
		return false
	}
	// Unique local addresses (fc00::/7) are not reachable from the internet.
	if len(v6) == net.IPv6len && v6[0]&0xfe == 0xfc {
		return false
	}
	return true
}

// outboundDialer dials destinations on behalf of a SOCKS client.
type outboundDialer struct {
	timeout  time.Duration
	mode     func() string
	family   *ipv6Monitor
	resolver *net.Resolver

	// dial is a field so tests can replace the network with a fake.
	dial func(ctx context.Context, network, address string) (net.Conn, error)
}

func newOutboundDialer(cfg Config, mode func() string, family *ipv6Monitor) *outboundDialer {
	timeout, _ := cfg.dialTimeout()
	return &outboundDialer{
		timeout:  timeout,
		mode:     mode,
		family:   family,
		resolver: net.DefaultResolver,
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, address)
		},
	}
}

// DialHost connects to host:port. For a literal address the family rules still
// apply; for a name every result is filtered by allow before it is tried.
func (d *outboundDialer) DialHost(ctx context.Context, host string, port int, allow func(net.IP) bool) (net.Conn, net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if allow != nil && !allow(ip) {
			return nil, ip, errDestinationRefused
		}
		if !d.familyAllowed(ip) {
			return nil, ip, fmt.Errorf("%w: IPv6 is disabled on this exit", errNoRoute)
		}
		conn, err := d.dialOne(ctx, ip, port)
		return conn, ip, err
	}

	lookupCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	addresses, err := d.resolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	candidates := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		ip := address.IP
		if allow != nil && !allow(ip) {
			continue
		}
		if !d.familyAllowed(ip) {
			continue
		}
		candidates = append(candidates, ip)
		if len(candidates) >= maxDialCandidates {
			break
		}
	}
	if len(candidates) == 0 {
		return nil, nil, fmt.Errorf("%w: %s", errNoRoute, host)
	}
	candidates = d.order(candidates)
	return d.race(ctx, candidates, port)
}

// errDestinationRefused is returned when the policy rejects the only address
// of a request. It maps to a SOCKS5 "not allowed" reply.
var errDestinationRefused = errors.New("destination refused by policy")

// familyAllowed applies the ipv6 setting to one destination.
func (d *outboundDialer) familyAllowed(ip net.IP) bool {
	if ip.To4() != nil || !isGlobalIPv6(ip) {
		// IPv4 and special addresses (loopback, Yggdrasil) are not affected by
		// the setting: a local test must keep working with ipv6 "off".
		return true
	}
	switch d.mode() {
	case ipv6Off:
		return false
	case ipv6Prefer:
		// "prefer" asks for IPv6 first, not for IPv6 even when the host cannot
		// reach anything: a provider that drops IPv6 must not turn every dial
		// into a timeout. A nil monitor means "not wired" (a unit test), which
		// keeps the old answer.
		if d.family == nil {
			return true
		}
		available, _, _, _, _ := d.family.status()
		return available
	default:
		available, _, _, _, _ := d.family.status()
		return available
	}
}

// order puts the preferred family first. Happy Eyeballs interleaves the two,
// but the first attempt should still be the one the operator asked for.
func (d *outboundDialer) order(ips []net.IP) []net.IP {
	preferV6 := d.mode() == ipv6Prefer
	if d.mode() == ipv6Auto && d.family != nil {
		preferV6, _, _, _, _ = d.family.status()
	}
	ordered := make([]net.IP, 0, len(ips))
	add := func(wantV6 bool) {
		for _, ip := range ips {
			isV6 := ip.To4() == nil
			if isV6 == wantV6 {
				ordered = append(ordered, ip)
			}
		}
	}
	add(preferV6)
	add(!preferV6)
	return ordered
}

// dialOne connects to a single address.
func (d *outboundDialer) dialOne(ctx context.Context, ip net.IP, port int) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	network := "tcp4"
	if ip.To4() == nil {
		network = "tcp6"
	}
	return d.dial(dialCtx, network, net.JoinHostPort(ip.String(), itoa(port)))
}

// race starts the candidates with a 250 ms stagger and returns the first
// connection that succeeds. The losers are cancelled and their sockets closed.
func (d *outboundDialer) race(ctx context.Context, ips []net.IP, port int) (net.Conn, net.IP, error) {
	raceCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	type result struct {
		conn net.Conn
		ip   net.IP
		err  error
	}
	results := make(chan result, len(ips))
	var won atomic.Bool

	for index, ip := range ips {
		go func(index int, ip net.IP) {
			if index > 0 {
				timer := time.NewTimer(time.Duration(index) * happyEyeballsDelay)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-raceCtx.Done():
					results <- result{ip: ip, err: raceCtx.Err()}
					return
				}
			}
			conn, err := d.dialOne(raceCtx, ip, port)
			if err != nil {
				results <- result{ip: ip, err: err}
				return
			}
			if !won.CompareAndSwap(false, true) {
				_ = conn.Close()
				results <- result{ip: ip, err: errNoRoute}
				return
			}
			results <- result{conn: conn, ip: ip}
		}(index, ip)
	}

	var firstErr error
	for range ips {
		res := <-results
		if res.err == nil && res.conn != nil {
			cancel()
			return res.conn, res.ip, nil
		}
		if firstErr == nil && res.err != nil && !errors.Is(res.err, errNoRoute) {
			firstErr = res.err
		}
	}
	if firstErr == nil {
		firstErr = errNoRoute
	}
	return nil, nil, firstErr
}

// resolveHost resolves a name for the UDP relay. The result is filtered by the
// same rules as a TCP destination.
func (d *outboundDialer) resolveHost(ctx context.Context, host string, allow func(net.IP) bool) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if allow != nil && !allow(ip) {
			return nil, errDestinationRefused
		}
		if !d.familyAllowed(ip) {
			return nil, errNoRoute
		}
		return ip, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	addresses, err := d.resolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	var candidates []net.IP
	for _, address := range addresses {
		ip := address.IP
		if allow != nil && !allow(ip) {
			continue
		}
		if !d.familyAllowed(ip) {
			continue
		}
		candidates = append(candidates, ip)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: %s", errNoRoute, host)
	}
	candidates = d.order(candidates)
	return candidates[0], nil
}

// itoa avoids importing strconv in the hot path helpers above.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var buf [6]byte
	index := len(buf)
	for value > 0 {
		index--
		buf[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[index:])
}
