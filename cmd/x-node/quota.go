package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Per-client quotas. A quota caps the traffic of one address and can throttle
// its speed. The limits are enforced when a new connection or UDP association
// starts: a client over its cap is refused with a SOCKS5 "not allowed" reply.
// Connections that are already open are not torn down, because a quota is a
// fair-use limit, not a kill switch.

const quotaFileName = "quotas.json"

// Quota is one client's limits. Zero means "no limit".
type Quota struct {
	Address      string    `json:"address"`
	MonthlyBytes int64     `json:"monthlyBytes"`
	DailyBytes   int64     `json:"dailyBytes"`
	DownKbps     int       `json:"downKbps"`
	UpKbps       int       `json:"upKbps"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type quotaFile struct {
	Version int     `json:"version"`
	Quotas  []Quota `json:"quotas"`
}

type quotaStore struct {
	mu       sync.Mutex
	path     string
	quotas   map[string]Quota
	defaults QuotaDefaults
	log      *logger
	// limiters holds one shared token bucket pair per client. A speed limit is
	// a property of the client, not of one TCP connection: with a per-connection
	// shaper ten parallel downloads would each get the full rate.
	limiters map[string]*clientLimiters
}

// clientLimiters is the upload and download bucket of one address. Both are
// always non-nil so a quota change can update them in place.
type clientLimiters struct {
	up   *shaper
	down *shaper
}

func openQuotas(cfg Config, log *logger) (*quotaStore, error) {
	store := &quotaStore{
		path:     cfg.statePath(quotaFileName),
		quotas:   make(map[string]Quota),
		defaults: cfg.Quotas,
		log:      log,
		limiters: make(map[string]*clientLimiters),
	}
	raw, err := os.ReadFile(store.path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, err
	}
	var doc quotaFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.warn("could not parse %s: %v (starting without quotas)", store.path, err)
		_ = os.Rename(store.path, store.path+".broken")
		return store, nil
	}
	for _, quota := range doc.Quotas {
		if net.ParseIP(quota.Address) == nil {
			continue
		}
		store.quotas[quota.Address] = quota
	}
	return store, nil
}

// Get returns the effective quota of an address: its own entry, or the
// defaults from the configuration.
func (q *quotaStore) Get(ip net.IP) Quota {
	if ip == nil {
		return Quota{}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.effectiveQuotaLocked(ip.String())
}

// Set stores an explicit quota.
func (q *quotaStore) Set(quota Quota) error {
	if net.ParseIP(quota.Address) == nil {
		return fmt.Errorf("%q is not an address", quota.Address)
	}
	if quota.MonthlyBytes < 0 || quota.DailyBytes < 0 || quota.DownKbps < 0 || quota.UpKbps < 0 {
		return fmt.Errorf("quota values must not be negative")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	quota.UpdatedAt = time.Now().UTC()
	q.quotas[quota.Address] = quota
	q.refreshLimiterLocked(quota.Address)
	return q.saveLocked()
}

// Reset removes the explicit quota, so the client falls back to the defaults.
func (q *quotaStore) Reset(address string, all bool) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	removed := 0
	if all {
		removed = len(q.quotas)
		q.quotas = make(map[string]Quota)
		for key := range q.limiters {
			q.refreshLimiterLocked(key)
		}
	} else if _, ok := q.quotas[address]; ok {
		delete(q.quotas, address)
		removed = 1
		q.refreshLimiterLocked(address)
	}
	if removed > 0 {
		if err := q.saveLocked(); err != nil {
			q.log.warn("could not save %s: %v", q.path, err)
		}
	}
	return removed
}

// List returns the explicit quotas, sorted by address.
func (q *quotaStore) List() []Quota {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Quota, 0, len(q.quotas))
	for _, quota := range q.quotas {
		out = append(out, quota)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

// Defaults returns the configuration defaults.
func (q *quotaStore) Defaults() QuotaDefaults {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.defaults
}

// UpdateDefaults applies new defaults from a settings reload.
func (q *quotaStore) UpdateDefaults(defaults QuotaDefaults) {
	q.mu.Lock()
	q.defaults = defaults
	for key := range q.limiters {
		q.refreshLimiterLocked(key)
	}
	q.mu.Unlock()
}

// refreshLimiterLocked updates the buckets of one client after a quota change.
// The caller holds q.mu, and refreshLimiterLocked itself uses effectiveQuota.
func (q *quotaStore) refreshLimiterLocked(address string) {
	limiters, ok := q.limiters[address]
	if !ok {
		return
	}
	quota := q.effectiveQuotaLocked(address)
	limiters.up.setRate(quota.UpKbps)
	limiters.down.setRate(quota.DownKbps)
}

// effectiveQuotaLocked is Get without taking the lock again.
func (q *quotaStore) effectiveQuotaLocked(address string) Quota {
	if quota, ok := q.quotas[address]; ok {
		return quota
	}
	return Quota{
		Address:      address,
		MonthlyBytes: q.defaults.MonthlyBytes,
		DailyBytes:   q.defaults.DailyBytes,
		DownKbps:     q.defaults.DownKbps,
		UpKbps:       q.defaults.UpKbps,
	}
}

// Limiters returns the shared buckets of one client. Every TCP connection and
// every UDP association of the address waits on the same pair, so the rate is
// the rate of the client, not of one socket. A nil store or address returns
// unlimited buckets.
func (q *quotaStore) Limiters(ip net.IP) (up, down *shaper) {
	if q == nil || ip == nil {
		return nil, nil
	}
	address := ip.String()
	q.mu.Lock()
	defer q.mu.Unlock()
	limiters, ok := q.limiters[address]
	if !ok {
		quota := q.effectiveQuotaLocked(address)
		limiters = newClientLimiters(quota)
		q.limiters[address] = limiters
	}
	return limiters.up, limiters.down
}

// newClientLimiters builds the buckets of one client. An unlimited direction
// still gets a bucket, so a later quota change applies without new connections.
func newClientLimiters(quota Quota) *clientLimiters {
	up, down := newShaper(quota.UpKbps), newShaper(quota.DownKbps)
	if up == nil {
		up = &shaper{}
	}
	if down == nil {
		down = &shaper{}
	}
	return &clientLimiters{up: up, down: down}
}

func (q *quotaStore) saveLocked() error {
	doc := quotaFile{Version: 1, Quotas: make([]Quota, 0, len(q.quotas))}
	for _, quota := range q.quotas {
		doc.Quotas = append(doc.Quotas, quota)
	}
	sort.Slice(doc.Quotas, func(i, j int) bool { return doc.Quotas[i].Address < doc.Quotas[j].Address })
	return writeJSONAtomic(q.path, doc)
}

// allow reports whether a new connection may start, and why not.
func (q *quotaStore) allow(ip net.IP, usage *usageStore) (bool, string) {
	if q == nil || ip == nil {
		return true, ""
	}
	quota := q.Get(ip)
	if quota.DailyBytes <= 0 && quota.MonthlyBytes <= 0 {
		return true, ""
	}
	day, month := usage.used(ip)
	if quota.DailyBytes > 0 && int64(day) >= quota.DailyBytes {
		return false, "daily quota exhausted"
	}
	if quota.MonthlyBytes > 0 && int64(month) >= quota.MonthlyBytes {
		return false, "monthly quota exhausted"
	}
	return true, ""
}

// quotaState is the status of one client for the CLI and the web interface.
type quotaState struct {
	Address      string `json:"address"`
	MonthlyBytes int64  `json:"monthlyBytes"`
	DailyBytes   int64  `json:"dailyBytes"`
	DownKbps     int    `json:"downKbps"`
	UpKbps       int    `json:"upKbps"`
	DayUsed      uint64 `json:"dayUsed"`
	MonthUsed    uint64 `json:"monthUsed"`
	Explicit     bool   `json:"explicit"`
	// Exhausted tells the client why the exit refuses new connections. The app
	// shows "traffic quota exhausted" instead of guessing a connection limit.
	Exhausted bool   `json:"exhausted"`
	Reason    string `json:"reason,omitempty"`
}

func quotaStates(quotas *quotaStore, usage *usageStore, clients []Client) []quotaState {
	seen := make(map[string]bool, len(clients))
	states := make([]quotaState, 0, len(clients))
	for _, client := range clients {
		ip := net.ParseIP(client.Address)
		if ip == nil {
			continue
		}
		seen[client.Address] = true
		states = append(states, buildQuotaState(quotas, usage, ip))
	}
	for _, quota := range quotas.List() {
		if seen[quota.Address] {
			continue
		}
		if ip := net.ParseIP(quota.Address); ip != nil {
			states = append(states, buildQuotaState(quotas, usage, ip))
		}
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Address < states[j].Address })
	return states
}

func buildQuotaState(quotas *quotaStore, usage *usageStore, ip net.IP) quotaState {
	quota := quotas.Get(ip)
	day, month := usage.used(ip)
	state := quotaState{
		Address:      ip.String(),
		MonthlyBytes: quota.MonthlyBytes,
		DailyBytes:   quota.DailyBytes,
		DownKbps:     quota.DownKbps,
		UpKbps:       quota.UpKbps,
		DayUsed:      day,
		MonthUsed:    month,
		Explicit:     quotas.explicit(ip),
	}
	switch {
	case quota.DailyBytes > 0 && int64(day) >= quota.DailyBytes:
		state.Exhausted, state.Reason = true, "daily quota exhausted"
	case quota.MonthlyBytes > 0 && int64(month) >= quota.MonthlyBytes:
		state.Exhausted, state.Reason = true, "monthly quota exhausted"
	}
	return state
}

// parseByteSize reads "100GB", "5G", "500M", "100GiB" or a plain number of
// bytes. The bare suffixes are decimal, like the display: 1 MB = 1 000 000
// bytes. The explicit MiB/GiB spellings keep the binary meaning.
func parseByteSize(value string) (int64, error) {
	raw := strings.TrimSpace(strings.ToUpper(value))
	if raw == "" {
		return 0, fmt.Errorf("empty size")
	}
	units := []struct {
		suffix string
		factor float64
	}{
		{"TIB", 1 << 40}, {"TB", 1e12}, {"TI", 1 << 40}, {"T", 1e12},
		{"GIB", 1 << 30}, {"GB", 1e9}, {"GI", 1 << 30}, {"G", 1e9},
		{"MIB", 1 << 20}, {"MB", 1e6}, {"MI", 1 << 20}, {"M", 1e6},
		{"KIB", 1 << 10}, {"KB", 1e3}, {"KI", 1 << 10}, {"K", 1e3},
		{"B", 1},
	}
	factor := float64(1)
	number := raw
	for _, unit := range units {
		if strings.HasSuffix(raw, unit.suffix) {
			factor = unit.factor
			number = strings.TrimSpace(strings.TrimSuffix(raw, unit.suffix))
			break
		}
	}
	parsed, err := strconv.ParseFloat(number, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%q is not a size", value)
	}
	result := parsed * factor
	if result > math.MaxInt64 {
		return 0, fmt.Errorf("%q is too large", value)
	}
	return int64(result), nil
}

// parseBitRate reads "10Mbps", "512Kbps" or a plain number of kilobits.
func parseBitRate(value string) (int, error) {
	raw := strings.TrimSpace(strings.ToUpper(value))
	if raw == "" {
		return 0, fmt.Errorf("empty speed")
	}
	units := []struct {
		suffix string
		factor float64
	}{
		{"GBPS", 1e6}, {"GBIT", 1e6}, {"GB", 1e6}, {"G", 1e6},
		{"MBPS", 1e3}, {"MBIT", 1e3}, {"MB", 1e3}, {"M", 1e3},
		{"KBPS", 1}, {"KBIT", 1}, {"KB", 1}, {"K", 1},
	}
	factor := float64(1)
	number := raw
	for _, unit := range units {
		if strings.HasSuffix(raw, unit.suffix) {
			factor = unit.factor
			number = strings.TrimSpace(strings.TrimSuffix(raw, unit.suffix))
			break
		}
	}
	parsed, err := strconv.ParseFloat(number, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%q is not a speed", value)
	}
	result := parsed * factor
	if result > 10_000_000 {
		return 0, fmt.Errorf("%q is too large", value)
	}
	return int(result), nil
}

// explicit reports whether the address has its own quota entry.
func (q *quotaStore) explicit(ip net.IP) bool {
	if q == nil || ip == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.quotas[ip.String()]
	return ok
}

// formatRate renders kilobits per second for the CLI. Speeds are shown in
// megabits per second, because that is the unit an operator sets and compares
// against a speed test.
func formatRate(kbps int) string {
	switch {
	case kbps <= 0:
		return "unlimited"
	case kbps >= 1_000_000:
		return trimRate(kbps, 1e6) + " Gbps"
	default:
		return trimRate(kbps, 1e3) + " Mbps"
	}
}

// trimRate formats value/unit with at most two decimals and no trailing zeros.
func trimRate(value int, unit float64) string {
	text := strconv.FormatFloat(float64(value)/unit, 'f', 2, 64)
	text = strings.TrimRight(text, "0")
	return strings.TrimRight(text, ".")
}

// shaper is a token bucket. It limits one direction of one client; every
// connection of that client waits on the same bucket. A nil shaper means "no
// limit", and a bucket with rate 0 does the same without allocating a new one.
type shaper struct {
	rate  float64 // bytes per second
	burst float64

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newShaper(kbps int) *shaper {
	if kbps <= 0 {
		return nil
	}
	s := &shaper{last: time.Now()}
	s.setRate(kbps)
	// Start with a full bucket: the first burst must not wait.
	s.mu.Lock()
	s.tokens = s.burst
	s.mu.Unlock()
	return s
}

// setRate changes the speed of an existing bucket. A quota change applies to
// the connections that are already open, not only to new ones.
func (s *shaper) setRate(kbps int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if kbps <= 0 {
		s.rate, s.burst, s.tokens = 0, 0, 0
		s.last = time.Now()
		return
	}
	s.rate = float64(kbps) * 1000 / 8 // kilobits to bytes
	s.burst = s.rate / 4
	if s.burst < 16*1024 {
		s.burst = 16 * 1024
	}
	if s.tokens > s.burst {
		s.tokens = s.burst
	}
	if s.last.IsZero() {
		s.last = time.Now()
	}
}

// wait reserves n bytes and blocks until the bucket has paid for them. The
// reservation may make the balance negative, so concurrent callers each sleep
// their own share and the aggregate rate stays at the configured value.
func (s *shaper) wait(n int) {
	if s == nil || n <= 0 {
		return
	}
	s.mu.Lock()
	if s.rate <= 0 {
		s.mu.Unlock()
		return
	}
	now := time.Now()
	if !s.last.IsZero() {
		if elapsed := now.Sub(s.last).Seconds(); elapsed > 0 {
			s.tokens += elapsed * s.rate
			if s.tokens > s.burst {
				s.tokens = s.burst
			}
		}
	}
	s.last = now
	s.tokens -= float64(n)
	delay := time.Duration(0)
	if s.tokens < 0 {
		delay = time.Duration(-s.tokens / s.rate * float64(time.Second))
		// A single huge write must not sleep for minutes with the lock held;
		// the next call continues where this one stopped.
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
	}
	s.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
}
