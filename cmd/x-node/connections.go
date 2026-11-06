package main

import (
	"net"
	"sync"
)

// connectionRegistry tracks the client sockets that are carrying traffic right
// now. A quota or a suspension must stop the connections that are already
// open: checking only when a connection starts lets a client keep a stream
// alive for hours after its cap is used up.
//
// Only the client side of a relay is tracked. Closing it makes both copy
// directions fail and the upstream socket is closed by the relay's defer.
type connectionRegistry struct {
	mu      sync.Mutex
	next    int64
	entries map[int64]connectionEntry
}

type connectionEntry struct {
	address string
	conn    net.Conn
}

func newConnectionRegistry() *connectionRegistry {
	return &connectionRegistry{entries: make(map[int64]connectionEntry)}
}

// track remembers one connection and returns the function that forgets it.
func (r *connectionRegistry) track(ip net.IP, conn net.Conn) func() {
	if r == nil || ip == nil || conn == nil {
		return func() {}
	}
	r.mu.Lock()
	r.next++
	id := r.next
	r.entries[id] = connectionEntry{address: ip.String(), conn: conn}
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.entries, id)
			r.mu.Unlock()
		})
	}
}

// closeClient closes every tracked connection of one address and reports how
// many sockets were closed.
func (r *connectionRegistry) closeClient(ip net.IP) int {
	if r == nil || ip == nil {
		return 0
	}
	key := ip.String()
	r.mu.Lock()
	conns := make([]net.Conn, 0, len(r.entries))
	for id, entry := range r.entries {
		if entry.address != key {
			continue
		}
		conns = append(conns, entry.conn)
		delete(r.entries, id)
	}
	r.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	return len(conns)
}

// addresses returns the addresses that have at least one tracked connection.
func (r *connectionRegistry) addresses() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]bool, len(r.entries))
	out := make([]string, 0, len(r.entries))
	for _, entry := range r.entries {
		if seen[entry.address] {
			continue
		}
		seen[entry.address] = true
		out = append(out, entry.address)
	}
	return out
}
