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
}

func openQuotas(cfg Config, log *logger) (*quotaStore, error) {
	store := &quotaStore{
		path:     cfg.statePath(quotaFileName),
		quotas:   make(map[string]Quota),
		defaults: cfg.Quotas,
		log:      log,
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
	address := ip.String()
	q.mu.Lock()
	defer q.mu.Unlock()
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
	} else if _, ok := q.quotas[address]; ok {
		delete(q.quotas, address)
		removed = 1
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
	q.mu.Unlock()
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
	_, explicit := quotas.quotas[ip.String()]
	return quotaState{
		Address:      ip.String(),
		MonthlyBytes: quota.MonthlyBytes,
		DailyBytes:   quota.DailyBytes,
		DownKbps:     quota.DownKbps,
		UpKbps:       quota.UpKbps,
		DayUsed:      day,
		MonthUsed:    month,
		Explicit:     explicit,
	}
}

// parseByteSize reads "100GiB", "5G", "500M" or a plain number of bytes.
func parseByteSize(value string) (int64, error) {
	raw := strings.TrimSpace(strings.ToUpper(value))
	if raw == "" {
		return 0, fmt.Errorf("empty size")
	}
	units := []struct {
		suffix string
		factor float64
	}{
		{"TIB", 1 << 40}, {"TB", 1e12}, {"TI", 1 << 40}, {"T", 1 << 40},
		{"GIB", 1 << 30}, {"GB", 1e9}, {"GI", 1 << 30}, {"G", 1 << 30},
		{"MIB", 1 << 20}, {"MB", 1e6}, {"MI", 1 << 20}, {"M", 1 << 20},
		{"KIB", 1 << 10}, {"KB", 1e3}, {"KI", 1 << 10}, {"K", 1 << 10},
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

// formatRate renders kilobits per second for the CLI.
func formatRate(kbps int) string {
	switch {
	case kbps <= 0:
		return "unlimited"
	case kbps >= 1_000_000:
		return fmt.Sprintf("%.1f Gbps", float64(kbps)/1e6)
	case kbps >= 1_000:
		return fmt.Sprintf("%.1f Mbps", float64(kbps)/1e3)
	default:
		return fmt.Sprintf("%d Kbps", kbps)
	}
}

// shaper is a token bucket. It is created per connection and limits the speed
// of one direction. A nil shaper means "no limit".
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
	rate := float64(kbps) * 1000 / 8 // kilobits to bytes
	burst := rate / 4
	if burst < 16*1024 {
		burst = 16 * 1024
	}
	return &shaper{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

// wait blocks until n bytes may be sent at the configured rate.
func (s *shaper) wait(n int) {
	if s == nil || n <= 0 {
		return
	}
	s.mu.Lock()
	now := time.Now()
	elapsed := now.Sub(s.last).Seconds()
	if elapsed > 0 {
		s.tokens += elapsed * s.rate
		if s.tokens > s.burst {
			s.tokens = s.burst
		}
		s.last = now
	}
	need := float64(n)
	if s.tokens >= need {
		s.tokens -= need
		s.mu.Unlock()
		return
	}
	deficit := need - s.tokens
	s.tokens = 0
	s.mu.Unlock()
	time.Sleep(time.Duration(deficit / s.rate * float64(time.Second)))
}
