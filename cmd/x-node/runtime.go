package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"sort"
	"sync"
	"time"
)

// runtimeFileName is the live state the service publishes for the CLI. It is
// written into the state directory, which is only readable by the service user
// and root, and it is removed on a clean shutdown.
const runtimeFileName = "runtime.json"

// runtimeFreshFor is how old the published state may be before the CLI treats
// the service as stopped. The tracker refreshes the file every heartbeat.
const runtimeFreshFor = 90 * time.Second

// runtimeClient is one address with open SOCKS5 connections.
type runtimeClient struct {
	Address      string    `json:"address"`
	Connections  int       `json:"connections"`
	Since        time.Time `json:"since"`
	LastSeenAt   time.Time `json:"lastSeenAt"`
	Destinations []string  `json:"destinations,omitempty"`

	// openTargets counts the open connections per destination. Destinations is
	// its published projection: a target leaves the list when its last
	// connection closes, so the popover never shows destinations of
	// connections that are already gone.
	openTargets map[string]int

	// Refused counts the connections the limits turned away since the service
	// started. QuotaRefused is the part caused by a traffic quota and
	// SuspendedRefused the part caused by the operator's block, so a client can
	// tell "maxConnsPerClient" from "you are out of traffic" or "you are
	// suspended".
	Refused          int `json:"refused,omitempty"`
	QuotaRefused     int `json:"quotaRefused,omitempty"`
	SuspendedRefused int `json:"suspendedRefused,omitempty"`

	UDPAssociations int    `json:"udpAssociations,omitempty"`
	UDPDatagramsIn  uint64 `json:"udpDatagramsIn,omitempty"`
	UDPDatagramsOut uint64 `json:"udpDatagramsOut,omitempty"`
	UDPBytesIn      uint64 `json:"udpBytesIn,omitempty"`
	UDPBytesOut     uint64 `json:"udpBytesOut,omitempty"`
}

// runtimeFile is the JSON document in runtime.json.
type runtimeFile struct {
	Version          int             `json:"version"`
	PID              int             `json:"pid"`
	StartedAt        time.Time       `json:"startedAt"`
	UpdatedAt        time.Time       `json:"updatedAt"`
	TotalConnections int             `json:"totalConnections"`
	Clients          []runtimeClient `json:"clients"`

	TotalRefused int `json:"totalRefused,omitempty"`

	TotalUDPAssociations int    `json:"totalUdpAssociations,omitempty"`
	TotalUDPDatagramsIn  uint64 `json:"totalUdpDatagramsIn,omitempty"`
	TotalUDPDatagramsOut uint64 `json:"totalUdpDatagramsOut,omitempty"`
	TotalUDPBytesIn      uint64 `json:"totalUdpBytesIn,omitempty"`
	TotalUDPBytesOut     uint64 `json:"totalUdpBytesOut,omitempty"`
}

// runtimeTracker counts the open SOCKS5 connections per client and publishes
// them. A client that is only in the allowlist is "registered"; one with open
// connections is "connected".
type runtimeTracker struct {
	mu        sync.Mutex
	path      string
	log       *logger
	startedAt time.Time
	clients   map[string]*runtimeClient
	total     int
	dirty     bool
	lastWrite time.Time

	udpTotal        int
	udpDatagramsIn  uint64
	udpDatagramsOut uint64
	udpBytesIn      uint64
	udpBytesOut     uint64
	totalRefused    int
}

func newRuntimeTracker(cfg Config, log *logger) *runtimeTracker {
	return &runtimeTracker{
		path:      cfg.statePath(runtimeFileName),
		log:       log,
		startedAt: time.Now(),
		clients:   make(map[string]*runtimeClient),
	}
}

// run publishes the state until the context is cancelled and removes the file
// on a clean shutdown, so a stopped service is not reported as running.
func (t *runtimeTracker) run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	t.flush(true)
	for {
		select {
		case <-ctx.Done():
			_ = os.Remove(t.path)
			return
		case <-ticker.C:
			t.flush(false)
		}
	}
}

// shutdown publishes the final state and removes the live file. The process
// exits right after the serve loop returns, so this cannot be left to the
// tracker goroutine.
func (t *runtimeTracker) shutdown() {
	t.flush(true)
	_ = os.Remove(t.path)
}

// runtimeDestinationsMax caps the published destination list. The live file is
// rewritten every heartbeat, so a client with hundreds of open connections
// must not make it grow without a bound.
const runtimeDestinationsMax = 8

// refreshDestinations rebuilds the published list from the open connections.
// The order is sorted to keep the published file stable between heartbeats.
func (c *runtimeClient) refreshDestinations() {
	if len(c.openTargets) == 0 {
		c.Destinations = nil
		return
	}
	targets := make([]string, 0, len(c.openTargets))
	for target := range c.openTargets {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	if len(targets) > runtimeDestinationsMax {
		targets = targets[:runtimeDestinationsMax]
	}
	c.Destinations = targets
}

func (t *runtimeTracker) open(ip net.IP, target string) {
	if ip == nil {
		return
	}
	key := ip.String()
	now := time.Now()
	t.mu.Lock()
	client, ok := t.clients[key]
	if !ok {
		client = &runtimeClient{Address: key, Since: now}
		t.clients[key] = client
	}
	client.Connections++
	client.LastSeenAt = now
	if target != "" {
		if client.openTargets == nil {
			client.openTargets = make(map[string]int)
		}
		client.openTargets[target]++
		// Rebuild only when the set of targets changes, not on every extra
		// connection to a destination that is already listed.
		if client.openTargets[target] == 1 {
			client.refreshDestinations()
		}
	}
	t.total++
	t.dirty = true
	t.mu.Unlock()
}

func (t *runtimeTracker) close(ip net.IP, target string) {
	if ip == nil {
		return
	}
	key := ip.String()
	now := time.Now()
	t.mu.Lock()
	if client, ok := t.clients[key]; ok {
		if client.Connections > 1 {
			client.Connections--
			if target != "" {
				switch client.openTargets[target] {
				case 0:
				case 1:
					delete(client.openTargets, target)
					client.refreshDestinations()
				default:
					client.openTargets[target]--
				}
			}
		} else {
			client.Connections = 0
			client.openTargets = nil
			client.Destinations = nil
			client.Since = time.Time{}
		}
		client.LastSeenAt = now
		if t.total > 0 {
			t.total--
		}
	}
	t.dirty = true
	t.mu.Unlock()
}

// forget drops the live row of one address. The tracker keeps a row after the
// last connection closes so the last-seen stamp survives, which is right for a
// registered client and wrong for one the operator has just deleted: without
// this the row stayed in the published state until the service restarted.
func (t *runtimeTracker) forget(ip net.IP) {
	if t == nil || ip == nil {
		return
	}
	key := ip.String()
	t.mu.Lock()
	if client, ok := t.clients[key]; ok {
		// The connections are closed by the caller, so their close() calls will
		// not find the row any more: settle the total here.
		if t.total >= client.Connections {
			t.total -= client.Connections
		} else {
			t.total = 0
		}
		delete(t.clients, key)
	}
	t.dirty = true
	t.mu.Unlock()
}

// touch records that a registered client is alive even when it has no open
// connection right now (the status endpoint calls it).
func (t *runtimeTracker) touch(ip net.IP) {
	if ip == nil {
		return
	}
	key := ip.String()
	now := time.Now()
	t.mu.Lock()
	client, ok := t.clients[key]
	if !ok {
		client = &runtimeClient{Address: key}
		t.clients[key] = client
	}
	client.LastSeenAt = now
	t.dirty = true
	t.mu.Unlock()
}

// refused records one connection the limits turned away. The counter is what
// the client's status call reads to tell a limit problem from a dead exit.
func (t *runtimeTracker) refused(ip net.IP) {
	t.countRefused(ip, false, false)
}

// quotaRefused records a refusal caused by a traffic quota. The total grows as
// well, so an old client still sees that something was refused; the new field
// lets a current client name the actual reason.
func (t *runtimeTracker) quotaRefused(ip net.IP) {
	t.countRefused(ip, true, false)
}

// suspendedRefused records a connection refused because the operator suspended
// the client. It is counted separately: a suspension is not a connection limit,
// and reporting it as one made the app warn about a limit that never existed.
func (t *runtimeTracker) suspendedRefused(ip net.IP) {
	t.countRefused(ip, false, true)
}

func (t *runtimeTracker) countRefused(ip net.IP, quota, suspended bool) {
	if ip == nil {
		return
	}
	key := ip.String()
	now := time.Now()
	t.mu.Lock()
	client, ok := t.clients[key]
	if !ok {
		client = &runtimeClient{Address: key}
		t.clients[key] = client
	}
	client.Refused++
	if quota {
		client.QuotaRefused++
	}
	if suspended {
		client.SuspendedRefused++
	}
	client.LastSeenAt = now
	t.totalRefused++
	t.dirty = true
	t.mu.Unlock()
}

// live returns the current counters of one client. The status API uses it to
// report how loaded the client is and how often the limits refused it.
func (t *runtimeTracker) live(ip net.IP) runtimeClient {
	if t == nil || ip == nil {
		return runtimeClient{}
	}
	key := ip.String()
	t.mu.Lock()
	defer t.mu.Unlock()
	if client, ok := t.clients[key]; ok {
		return *client
	}
	return runtimeClient{}
}

// udpOpen records that a client opened a UDP association.
func (t *runtimeTracker) udpOpen(ip net.IP) {
	if ip == nil {
		return
	}
	key := ip.String()
	now := time.Now()
	t.mu.Lock()
	client, ok := t.clients[key]
	if !ok {
		client = &runtimeClient{Address: key, Since: now}
		t.clients[key] = client
	}
	client.UDPAssociations++
	client.LastSeenAt = now
	t.udpTotal++
	t.dirty = true
	t.mu.Unlock()
}

// udpClose records that a UDP association ended.
func (t *runtimeTracker) udpClose(ip net.IP) {
	if ip == nil {
		return
	}
	key := ip.String()
	now := time.Now()
	t.mu.Lock()
	if client, ok := t.clients[key]; ok {
		if client.UDPAssociations > 0 {
			client.UDPAssociations--
		}
		client.LastSeenAt = now
	}
	if t.udpTotal > 0 {
		t.udpTotal--
	}
	t.dirty = true
	t.mu.Unlock()
}

// udpTraffic adds the datagram and byte deltas of one association. The caller
// throttles the calls, so a busy relay does not take the lock per packet.
func (t *runtimeTracker) udpTraffic(ip net.IP, datagramsIn, datagramsOut, bytesIn, bytesOut uint64) {
	if ip == nil {
		return
	}
	key := ip.String()
	now := time.Now()
	t.mu.Lock()
	client, ok := t.clients[key]
	if !ok {
		client = &runtimeClient{Address: key, Since: now}
		t.clients[key] = client
	}
	client.UDPDatagramsIn += datagramsIn
	client.UDPDatagramsOut += datagramsOut
	client.UDPBytesIn += bytesIn
	client.UDPBytesOut += bytesOut
	client.LastSeenAt = now
	t.udpDatagramsIn += datagramsIn
	t.udpDatagramsOut += datagramsOut
	t.udpBytesIn += bytesIn
	t.udpBytesOut += bytesOut
	t.dirty = true
	t.mu.Unlock()
}

// flush writes the document when something changed, and at least once per
// heartbeat so the CLI can tell a quiet service from a stopped one.
func (t *runtimeTracker) flush(force bool) {
	t.mu.Lock()
	if !force && !t.dirty && time.Since(t.lastWrite) < 15*time.Second {
		t.mu.Unlock()
		return
	}
	doc := t.snapshotLocked()
	t.dirty = false
	t.lastWrite = time.Now()
	t.mu.Unlock()
	if err := writeJSONAtomic(t.path, doc); err != nil {
		t.log.warn("could not write %s: %v", t.path, err)
	}
}

func (t *runtimeTracker) snapshotLocked() runtimeFile {
	clients := make([]runtimeClient, 0, len(t.clients))
	for _, client := range t.clients {
		// A client with no connections and no recent activity is not useful in
		// the live file: the allowlist already carries its last seen time.
		if client.Connections == 0 && time.Since(client.LastSeenAt) > 10*time.Minute {
			continue
		}
		clients = append(clients, *client)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].Address < clients[j].Address })
	return runtimeFile{
		Version:              1,
		PID:                  os.Getpid(),
		StartedAt:            t.startedAt,
		UpdatedAt:            time.Now(),
		TotalConnections:     t.total,
		Clients:              clients,
		TotalRefused:         t.totalRefused,
		TotalUDPAssociations: t.udpTotal,
		TotalUDPDatagramsIn:  t.udpDatagramsIn,
		TotalUDPDatagramsOut: t.udpDatagramsOut,
		TotalUDPBytesIn:      t.udpBytesIn,
		TotalUDPBytesOut:     t.udpBytesOut,
	}
}

// readRuntime loads the live state. A missing file is not an error for the CLI:
// it means the service has not run or has shut down cleanly.
func readRuntime(path string) (*runtimeFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc runtimeFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

func runtimeFresh(doc *runtimeFile, now time.Time) bool {
	if doc == nil || doc.UpdatedAt.IsZero() {
		return false
	}
	return now.Sub(doc.UpdatedAt) <= runtimeFreshFor
}

// clientRow is one line of `x-node clients`: the allowlist entry merged with
// the live connection counters.
type clientRow struct {
	Address         string    `json:"address"`
	Label           string    `json:"label"`
	Source          string    `json:"source"`
	State           string    `json:"state"`
	Connections     int       `json:"connections"`
	UDPAssociations int       `json:"udpAssociations"`
	UDPDatagramsIn  uint64    `json:"udpDatagramsIn"`
	UDPDatagramsOut uint64    `json:"udpDatagramsOut"`
	UDPBytesIn      uint64    `json:"udpBytesIn"`
	UDPBytesOut     uint64    `json:"udpBytesOut"`
	Refused         int       `json:"refused,omitempty"`
	QuotaRefused    int       `json:"quotaRefused,omitempty"`
	Destinations    []string  `json:"destinations,omitempty"`
	RegisteredAt    time.Time `json:"registeredAt"`
	LastSeenAt      time.Time `json:"lastSeenAt"`
}

// mergeClients combines the allowlist with the runtime document. Entries that
// only exist in the runtime are included only in open mode: without an
// allowlist every address that connects is a client, but with one an address
// that is not in it was removed by the operator and must not reappear from the
// counters the tracker still holds.
func mergeClients(clients []Client, doc *runtimeFile, now time.Time, openMode bool) []clientRow {
	live := make(map[string]runtimeClient)
	if runtimeFresh(doc, now) {
		for _, client := range doc.Clients {
			live[client.Address] = client
		}
	}
	rows := make([]clientRow, 0, len(clients)+len(live))
	seen := make(map[string]bool, len(clients))
	for _, client := range clients {
		seen[client.Address] = true
		row := clientRow{
			Address:      client.Address,
			Label:        client.Label,
			Source:       client.Source,
			RegisteredAt: client.RegisteredAt,
			LastSeenAt:   client.LastSeenAt,
			State:        "registered",
		}
		if item, ok := live[client.Address]; ok {
			row.Connections = item.Connections
			row.UDPAssociations = item.UDPAssociations
			row.UDPDatagramsIn = item.UDPDatagramsIn
			row.UDPDatagramsOut = item.UDPDatagramsOut
			row.UDPBytesIn = item.UDPBytesIn
			row.UDPBytesOut = item.UDPBytesOut
			row.Refused = item.Refused
			row.QuotaRefused = item.QuotaRefused
			row.Destinations = item.Destinations
			if item.LastSeenAt.After(row.LastSeenAt) {
				row.LastSeenAt = item.LastSeenAt
			}
			if item.Connections > 0 || item.UDPAssociations > 0 {
				row.State = "connected"
			} else if now.Sub(item.LastSeenAt) < 5*time.Minute {
				row.State = "idle"
			}
		}
		rows = append(rows, row)
	}
	for address, item := range live {
		if seen[address] || !openMode {
			continue
		}
		row := clientRow{
			Address:         address,
			Source:          "open",
			State:           "connected",
			Connections:     item.Connections,
			UDPAssociations: item.UDPAssociations,
			UDPDatagramsIn:  item.UDPDatagramsIn,
			UDPDatagramsOut: item.UDPDatagramsOut,
			UDPBytesIn:      item.UDPBytesIn,
			UDPBytesOut:     item.UDPBytesOut,
			Refused:         item.Refused,
			QuotaRefused:    item.QuotaRefused,
			Destinations:    item.Destinations,
			LastSeenAt:      item.LastSeenAt,
		}
		if item.Connections == 0 && item.UDPAssociations == 0 {
			row.State = "idle"
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if (rows[i].State == "connected") != (rows[j].State == "connected") {
			return rows[i].State == "connected"
		}
		return rows[i].Address < rows[j].Address
	})
	return rows
}
