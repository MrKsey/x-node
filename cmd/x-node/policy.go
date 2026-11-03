package main

import (
	"net"
	"strconv"
)

// blockedIPv4 are the destinations an exit node must never reach on behalf of
// a client: private, carrier-grade NAT, loopback, link-local, documentation,
// benchmarking, multicast and reserved space. A client can still reach its own
// LAN directly; the exit only carries ordinary internet traffic.
var blockedIPv4 = mustCIDRs(
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.88.99.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
)

// allowedIPv6 is the global unicast space. An exit node carries ordinary
// internet traffic only, so everything outside 2000::/3 — Yggdrasil (200::/7),
// unique local, link-local, loopback, multicast — is refused by construction
// rather than by an ever-growing blocklist.
var allowedIPv6 = mustCIDRs("2000::/3")

// blockedIPv6 are ranges inside the global unicast space that are not ordinary
// destinations: documentation, benchmarking and special-purpose prefixes.
var blockedIPv6 = mustCIDRs(
	"200::/7",       // Yggdrasil itself: the exit is not a router into the mesh
	"fc00::/7",      // unique local
	"fe80::/10",     // link-local
	"ff00::/8",      // multicast
	"::/128",        // unspecified
	"::1/128",       // loopback
	"2001:2::/48",   // benchmarking (RFC 5180)
	"2001:10::/28",  // ORCHID (RFC 4843)
	"2001:20::/28",  // ORCHIDv2 (RFC 7343)
	"2001:db8::/32", // documentation (RFC 3849)
	"3fff::/20",     // documentation (RFC 9637)
)

func mustCIDRs(values ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			panic("bad built-in CIDR " + value + ": " + err.Error())
		}
		out = append(out, network)
	}
	return out
}

// destinationAllowed reports whether a CONNECT request may leave through the
// exit. Only global unicast addresses are accepted; the port list is checked
// separately.
func destinationAllowed(ip net.IP, port int) bool {
	if port <= 0 || port > 65535 {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		for _, network := range blockedIPv4 {
			if network.Contains(v4) {
				return false
			}
		}
		return !v4.Equal(net.IPv4bcast)
	}
	v6 := ip.To16()
	if v6 == nil {
		return false
	}
	allowed := false
	for _, network := range allowedIPv6 {
		if network.Contains(v6) {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	for _, network := range blockedIPv6 {
		if network.Contains(v6) {
			return false
		}
	}
	return true
}

func portBlocked(port int, blocked []int) bool {
	for _, candidate := range blocked {
		if candidate == port {
			return true
		}
	}
	return false
}

// clientKey is the map key used for per-client connection limits.
func clientKey(ip net.IP) string {
	return ip.String()
}

func joinHostPort(ip net.IP, port int) string {
	return net.JoinHostPort(ip.String(), strconv.Itoa(port))
}
