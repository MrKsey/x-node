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
)

// errNoRoute means that the name resolved but no address passed the policy or
// the address family filter.
var errNoRoute = errors.New("no usable address")

// ipv6Monitor caches whether this host can open a global IPv6 connection.
type ipv6Monitor struct {
	mu        sync.RWMutex
	available bool
	detail    string
	checked   time.Time
}

func newIPv6Monitor() *ipv6Monitor {
	monitor := &ipv6Monitor{}
	monitor.refresh()
	return monitor
}

// refresh asks the kernel for the source address it would use for a global
// IPv6 destination. A UDP connect does not send anything, so this works even
// when the host is offline.
func (m *ipv6Monitor) refresh() {
	available, detail := detectIPv6()
	m.mu.Lock()
	m.available = available
	m.detail = detail
	m.checked = time.Now()
	m.mu.Unlock()
}

func (m *ipv6Monitor) run(ctx context.Context) {
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

func (m *ipv6Monitor) status() (bool, string) {
	if m == nil {
		return false, "not checked"
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.available, m.detail
}

func detectIPv6() (bool, string) {
	conn, err := net.Dial("udp6", ipv6ProbeAddress)
	if err != nil {
		return false, err.Error()
	}
	defer conn.Close()
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || local.IP == nil {
		return false, "no local IPv6 address"
	}
	ip := local.IP
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false, "only " + ip.String()
	}
	return true, ip.String()
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
		return true
	default:
		available, _ := d.family.status()
		return available
	}
}

// order puts the preferred family first. Happy Eyeballs interleaves the two,
// but the first attempt should still be the one the operator asked for.
func (d *outboundDialer) order(ips []net.IP) []net.IP {
	preferV6 := d.mode() == ipv6Prefer
	if d.mode() == ipv6Auto && d.family != nil {
		preferV6, _ = d.family.status()
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
