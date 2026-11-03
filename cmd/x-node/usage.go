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

// Traffic accounting. The counters are per client and per period (day and
// month); they survive a restart because they live in usage.json in the state
// directory. The relay reports bytes once per second per connection, so the
// hot path never takes a lock per packet.

const (
	usageFileName = "usage.json"
	// usageFlushInterval is how often a dirty document is written. A crash
	// loses at most this much accounting, which is acceptable for a quota.
	usageFlushInterval = 10 * time.Second
)

// usageClient is the traffic of one client. Rx is what the client sent into
// the exit (upload), Tx is what the exit sent back (download).
type usageClient struct {
	Address string `json:"address"`

	Rx uint64 `json:"rx"`
	Tx uint64 `json:"tx"`

	Day   string `json:"day"`
	DayRx uint64 `json:"dayRx"`
	DayTx uint64 `json:"dayTx"`

	Month   string `json:"month"`
	MonthRx uint64 `json:"monthRx"`
	MonthTx uint64 `json:"monthTx"`

	TCPConnections  uint64 `json:"tcpConnections"`
	UDPAssociations uint64 `json:"udpAssociations"`
	Refused         uint64 `json:"refused"`

	FirstSeenAt time.Time `json:"firstSeenAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
}

type usageFile struct {
	Version   int                     `json:"version"`
	UpdatedAt time.Time               `json:"updatedAt"`
	Timezone  string                  `json:"timezone"`
	Clients   map[string]*usageClient `json:"clients"`
}

type usageStore struct {
	mu       sync.Mutex
	enabled  bool
	path     string
	location *time.Location
	log      *logger

	clients map[string]*usageClient
	dirty   bool
	lastAt  time.Time
}

func openUsage(cfg Config, log *logger) (*usageStore, error) {
	location, err := cfg.usageLocation()
	if err != nil {
		return nil, err
	}
	store := &usageStore{
		enabled:  cfg.Usage.Enabled,
		path:     cfg.statePath(usageFileName),
		location: location,
		log:      log,
		clients:  make(map[string]*usageClient),
	}
	if !store.enabled {
		return store, nil
	}
	raw, err := os.ReadFile(store.path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, err
	}
	var doc usageFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		// A corrupt file must not stop the exit: start fresh and keep the old
		// file for inspection.
		log.warn("could not parse %s: %v (starting with empty counters)", store.path, err)
		_ = os.Rename(store.path, store.path+".broken")
		return store, nil
	}
	for address, client := range doc.Clients {
		if client == nil {
			continue
		}
		client.Address = address
		store.clients[address] = client
	}
	return store, nil
}

// run flushes the document until the context is cancelled.
func (u *usageStore) run(ctx context.Context) {
	if !u.enabled {
		return
	}
	ticker := time.NewTicker(usageFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			u.flush(true)
			return
		case <-ticker.C:
			u.flush(false)
		}
	}
}

func (u *usageStore) flush(force bool) {
	if !u.enabled {
		return
	}
	u.mu.Lock()
	if !u.dirty && !force {
		u.mu.Unlock()
		return
	}
	doc := usageFile{
		Version:   1,
		UpdatedAt: time.Now().UTC(),
		Timezone:  u.location.String(),
		Clients:   make(map[string]*usageClient, len(u.clients)),
	}
	for address, client := range u.clients {
		copied := *client
		doc.Clients[address] = &copied
	}
	u.dirty = false
	path := u.path
	u.mu.Unlock()

	if err := writeJSONAtomic(path, doc); err != nil {
		u.log.warn("could not write %s: %v", path, err)
	}
}

// client returns the counters of one address, creating them when needed. The
// caller must hold the lock.
func (u *usageStore) clientLocked(address string, now time.Time) *usageClient {
	client, ok := u.clients[address]
	if !ok {
		client = &usageClient{Address: address, FirstSeenAt: now}
		u.clients[address] = client
	}
	day := now.Format("2006-01-02")
	month := now.Format("2006-01")
	if client.Day != day {
		client.Day = day
		client.DayRx = 0
		client.DayTx = 0
	}
	if client.Month != month {
		client.Month = month
		client.MonthRx = 0
		client.MonthTx = 0
	}
	return client
}

// addTraffic records bytes carried for one client.
func (u *usageStore) addTraffic(ip net.IP, rx, tx uint64) {
	if u == nil || !u.enabled || ip == nil || (rx == 0 && tx == 0) {
		return
	}
	address := ip.String()
	now := time.Now().In(u.location)
	u.mu.Lock()
	client := u.clientLocked(address, now)
	client.Rx += rx
	client.Tx += tx
	client.DayRx += rx
	client.DayTx += tx
	client.MonthRx += rx
	client.MonthTx += tx
	client.LastSeenAt = now
	u.dirty = true
	u.mu.Unlock()
}

// addConnection counts one accepted TCP connection.
func (u *usageStore) addConnection(ip net.IP) {
	if u == nil || !u.enabled || ip == nil {
		return
	}
	address := ip.String()
	now := time.Now().In(u.location)
	u.mu.Lock()
	client := u.clientLocked(address, now)
	client.TCPConnections++
	client.LastSeenAt = now
	u.dirty = true
	u.mu.Unlock()
}

// addUDPAssociation counts one UDP association.
func (u *usageStore) addUDPAssociation(ip net.IP) {
	if u == nil || !u.enabled || ip == nil {
		return
	}
	address := ip.String()
	now := time.Now().In(u.location)
	u.mu.Lock()
	client := u.clientLocked(address, now)
	client.UDPAssociations++
	client.LastSeenAt = now
	u.dirty = true
	u.mu.Unlock()
}

// addRefused counts one request refused because of a limit or a quota.
func (u *usageStore) addRefused(ip net.IP) {
	if u == nil || !u.enabled || ip == nil {
		return
	}
	address := ip.String()
	now := time.Now().In(u.location)
	u.mu.Lock()
	client := u.clientLocked(address, now)
	client.Refused++
	client.LastSeenAt = now
	u.dirty = true
	u.mu.Unlock()
}

// used returns the day and month totals of one client.
func (u *usageStore) used(ip net.IP) (day uint64, month uint64) {
	if u == nil || !u.enabled || ip == nil {
		return 0, 0
	}
	address := ip.String()
	now := time.Now().In(u.location)
	u.mu.Lock()
	defer u.mu.Unlock()
	client := u.clientLocked(address, now)
	return client.DayRx + client.DayTx, client.MonthRx + client.MonthTx
}

// reset zeroes the counters of one client or of every client.
func (u *usageStore) reset(address string, all bool) {
	if !u.enabled {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for key, client := range u.clients {
		if !all && key != address {
			continue
		}
		client.Rx, client.Tx = 0, 0
		client.DayRx, client.DayTx = 0, 0
		client.MonthRx, client.MonthTx = 0, 0
		client.TCPConnections, client.UDPAssociations, client.Refused = 0, 0, 0
	}
	u.dirty = true
}

// rows returns a sorted copy of the counters.
func (u *usageStore) rows() []usageClient {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]usageClient, 0, len(u.clients))
	for _, client := range u.clients {
		out = append(out, *client)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

// get returns the counters of one address.
func (u *usageStore) get(address string) (usageClient, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	client, ok := u.clients[address]
	if !ok {
		return usageClient{}, false
	}
	return *client, true
}
