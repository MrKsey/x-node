package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// peersStateFile keeps the public peers the supervisor is responsible for.
	// It lives in the state directory, so a replacement survives a restart.
	peersStateFile = "peers.json"
	// peerRefreshFile asks the running service to probe the public list now.
	peerRefreshFile = "peers.refresh"

	defaultAdminSocket   = "unix:///var/run/yggdrasil/yggdrasil.sock"
	defaultPeerProbePath = "/usr/local/bin/x-node-peerprobe"

	// A freshly added link needs time to finish its handshake before it may be
	// declared dead, otherwise a slow peer is dropped right after it is dialled.
	peerGracePeriod  = 90 * time.Second
	peerAdminTimeout = 5 * time.Second
	peerProbeTimeout = 5 * time.Minute
	// A candidate that failed to dial is not retried for this long.
	peerPenalty = 10 * time.Minute
)

// adminPeer is one entry of the Yggdrasil admin "getPeers" response.
type adminPeer struct {
	URI       string        `json:"remote"`
	Up        bool          `json:"up"`
	Inbound   bool          `json:"inbound"`
	Address   string        `json:"address"`
	Key       string        `json:"key"`
	Port      int           `json:"port"`
	Priority  int           `json:"priority"`
	Cost      int           `json:"cost"`
	BytesRecv uint64        `json:"bytes_recvd"`
	BytesSent uint64        `json:"bytes_sent"`
	Uptime    float64       `json:"uptime"`
	Latency   time.Duration `json:"latency"`
}

// adminSelf is the Yggdrasil admin "getSelf" response: the node identity and
// the size of its routing table.
type adminSelf struct {
	BuildName      string `json:"build_name"`
	BuildVersion   string `json:"build_version"`
	Key            string `json:"key"`
	Address        string `json:"address"`
	RoutingEntries int    `json:"routing_entries"`
	Subnet         string `json:"subnet"`
}

// peerNode is the part of the Yggdrasil admin API the supervisor needs. The
// service adds and removes peers through it instead of rewriting
// yggdrasil.conf and restarting the node, which would drop every client.
type peerNode interface {
	Peers() ([]adminPeer, error)
	AddPeer(uri string) error
	RemovePeer(uri string) error
}

type adminRequest struct {
	Name      string `json:"request"`
	KeepAlive bool   `json:"keepalive"`
	Arguments any    `json:"arguments"`
}

type adminResponse struct {
	Status   string          `json:"status"`
	Error    string          `json:"error"`
	Response json.RawMessage `json:"response"`
}

// adminSocket talks to the Yggdrasil admin endpoint, which speaks one JSON
// request and one JSON response per connection.
type adminSocket struct {
	address string
	timeout time.Duration
}

func newAdminSocket(address string, timeout time.Duration) adminSocket {
	if timeout <= 0 {
		timeout = peerAdminTimeout
	}
	return adminSocket{address: address, timeout: timeout}
}

// adminTarget resolves an admin address into a dial network and target. The
// Yggdrasil config uses unix:///path; a plain path and tcp://host:port are
// accepted as well.
func adminTarget(address string) (network, target string, err error) {
	value := strings.TrimSpace(address)
	if value == "" {
		return "", "", errors.New("adminSocket is empty")
	}
	if !strings.Contains(value, "://") {
		return "unix", value, nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", "", fmt.Errorf("adminSocket %q is not a URL: %w", address, err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "unix":
		if parsed.Path == "" {
			return "", "", fmt.Errorf("adminSocket %q has no path", address)
		}
		return "unix", parsed.Path, nil
	case "tcp":
		if parsed.Host == "" {
			return "", "", fmt.Errorf("adminSocket %q has no host", address)
		}
		return "tcp", parsed.Host, nil
	default:
		return "", "", fmt.Errorf("adminSocket %q must use unix:// or tcp://", address)
	}
}

func (a adminSocket) call(name string, arguments any, out any) error {
	network, target, err := adminTarget(a.address)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout(network, target, a.timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(a.timeout))

	payload, err := json.Marshal(adminRequest{Name: name, Arguments: arguments})
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if _, err := conn.Write(payload); err != nil {
		return err
	}

	var response adminResponse
	if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&response); err != nil {
		return err
	}
	if response.Status != "success" {
		if response.Error != "" {
			return errors.New(response.Error)
		}
		return fmt.Errorf("admin request %q failed", name)
	}
	if out == nil || len(response.Response) == 0 {
		return nil
	}
	return json.Unmarshal(response.Response, out)
}

func (a adminSocket) Peers() ([]adminPeer, error) {
	var response struct {
		Peers []adminPeer `json:"peers"`
	}
	if err := a.call("getPeers", map[string]any{}, &response); err != nil {
		return nil, err
	}
	return response.Peers, nil
}

func (a adminSocket) Self() (adminSelf, error) {
	var self adminSelf
	if err := a.call("getSelf", map[string]any{}, &self); err != nil {
		return adminSelf{}, err
	}
	return self, nil
}

func (a adminSocket) AddPeer(uri string) error {
	err := a.call("addPeer", map[string]string{"uri": uri}, nil)
	if err != nil && isAlreadyConfigured(err) {
		return nil
	}
	return err
}

func (a adminSocket) RemovePeer(uri string) error {
	err := a.call("removePeer", map[string]string{"uri": uri}, nil)
	if err != nil && isNotConfigured(err) {
		return nil
	}
	return err
}

func isAlreadyConfigured(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already configured")
}

func isNotConfigured(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not configured")
}

// probeResult is one line of x-node-peerprobe output. Country and city come
// from the public peer catalogue, so the operator can pick a peer by place
// instead of reading host names.
type probeResult struct {
	URI       string  `json:"uri"`
	LatencyMs float64 `json:"latencyMs"`
	Country   string  `json:"country,omitempty"`
	City      string  `json:"city,omitempty"`
}

// peerProbeProgress is what x-node-peerprobe reports while it walks the public
// catalogue. The dashboard shows it next to "Find peers", so a search that
// takes minutes explains itself instead of looking stuck.
type peerProbeProgress struct {
	Stage     string    `json:"stage"`
	Total     int       `json:"total"`
	Pool      int       `json:"pool,omitempty"`
	Checked   int       `json:"checked"`
	Alive     int       `json:"alive"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// peerSettings is the part of the config the supervisor applies. It is
// re-read on every check, so the count and the switch can be changed without
// restarting the service.
type peerSettings struct {
	Count              int
	AutoReplace        bool
	DeadTimeout        time.Duration
	CheckInterval      time.Duration
	RefreshInterval    time.Duration
	ImprovementPercent int
	Seed               []string
	// Manual is the operator's own list. It is kept connected on top of
	// Count and is never trimmed or replaced by the supervisor.
	Manual []string
}

type peersState struct {
	Seed    []string `json:"seed"`
	Managed []string `json:"managed"`
}

// peerSupervisor keeps the configured number of public peers alive.
//
// It never touches inbound peers: a client that dials the exit node is not in
// the managed list, is not counted against peerCount and is never removed.
type peerSupervisor struct {
	cfgPath   string
	statePath string
	log       *logger
	node      peerNode
	probe     func(context.Context, int) ([]probeResult, error)

	mu        sync.Mutex
	settings  peerSettings
	seed      []string
	managed   []string
	deadSince map[string]time.Time
	addedAt   map[string]time.Time
	penalised map[string]time.Time
	pool      []probeResult
	lastProbe time.Time
	probing   bool
	progress  peerProbeProgress
	changed   bool
	stamp     string

	warnMu   sync.Mutex
	lastWarn time.Time
	warnText string

	// wake asks the run loop for an immediate check instead of waiting for the
	// next tick. A refresh request from the web interface must not sit for the
	// whole check interval.
	wake chan struct{}

	// runCtx is the context of the run loop, so a probe started from the web
	// interface is cancelled with the daemon and not with the HTTP request.
	runCtx context.Context
}

func newPeerSupervisor(cfg Config, cfgPath string, log *logger) *peerSupervisor {
	s := &peerSupervisor{
		cfgPath:   cfgPath,
		statePath: cfg.statePath(peersStateFile),
		log:       log,
		node:      newAdminSocket(cfg.AdminSocket, peerAdminTimeout),
		settings:  peerSettingsFrom(cfg),
		deadSince: map[string]time.Time{},
		addedAt:   map[string]time.Time{},
		penalised: map[string]time.Time{},
		wake:      make(chan struct{}, 1),
	}
	s.probe = newPeerProbe(cfg.PeerProbePath, s.setProbeProgress)
	s.loadState(cfg.Peers)
	return s
}

func peerSettingsFrom(cfg Config) peerSettings {
	dead, err := cfg.peerDeadTimeout()
	if err != nil {
		dead = 90 * time.Second
	}
	check, err := cfg.peerCheckInterval()
	if err != nil {
		check = 30 * time.Second
	}
	refresh, err := cfg.peerRefreshInterval()
	if err != nil {
		refresh = 6 * time.Hour
	}
	return peerSettings{
		Count:              cfg.PeerCount,
		AutoReplace:        cfg.PeerAutoReplace,
		DeadTimeout:        dead,
		CheckInterval:      check,
		RefreshInterval:    refresh,
		ImprovementPercent: cfg.PeerImprovementPercent,
		Seed:               cleanPeerList(cfg.Peers),
		Manual:             cleanPeerList(cfg.ManualPeers),
	}
}

func (s *peerSupervisor) loadState(seed []string) {
	if state, err := readPeersState(s.statePath); err == nil && len(state.Managed) > 0 {
		s.managed = cleanPeerList(state.Managed)
		s.seed = cleanPeerList(state.Seed)
		if len(s.seed) == 0 {
			s.seed = append([]string(nil), s.managed...)
		}
		return
	}
	s.managed = cleanPeerList(seed)
	s.seed = append([]string(nil), s.managed...)
}

// run checks the links until the context is cancelled. The interval is read
// from the settings again after every tick, so a new value applies at once.
func (s *peerSupervisor) run(ctx context.Context) {
	defer s.saveState()
	s.mu.Lock()
	s.runCtx = ctx
	s.mu.Unlock()
	s.reloadConfig()
	// Write the starting list once, so peers.json exists even before the first
	// replacement and "peers status" always reads the live list.
	s.saveState()
	s.mu.Lock()
	// A full list at startup does not need an immediate download of the public
	// catalogue; the first probe happens after the refresh interval.
	if len(s.managed) >= s.settings.Count {
		s.lastProbe = time.Now()
	}
	s.mu.Unlock()
	s.log.info("peer supervisor: %d public peer(s), auto replace %s",
		s.settings.Count, onOff(s.settings.AutoReplace))

	for {
		s.step(ctx)
		wait := s.settings.CheckInterval
		if wait < 5*time.Second {
			wait = 5 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-time.After(wait):
		}
	}
}

// Notify asks the supervisor to run a check right now. A pending notification
// is not queued twice: one immediate check is enough.
func (s *peerSupervisor) Notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *peerSupervisor) step(ctx context.Context) {
	s.reloadConfig()

	s.mu.Lock()
	settings := s.settings
	s.mu.Unlock()

	if s.takeRefreshRequest() || s.probeDue() {
		s.startProbe(ctx, settings)
	}

	peers, err := s.node.Peers()
	if err != nil {
		s.warn("cannot read the Yggdrasil admin socket: %v", err)
		return
	}
	outbound := make(map[string]adminPeer, len(peers))
	for _, peer := range peers {
		// Clients connect to the exit node from the outside: Yggdrasil reports
		// them as inbound links and the supervisor leaves them alone.
		if peer.Inbound {
			continue
		}
		// The admin API drops the query string from the URI, so a peer
		// configured as tls://host:port?key=... comes back as tls://host:port.
		// The map is keyed by that form and lookups go through peerKeyOfURI.
		outbound[peerKeyOfURI(peer.URI)] = peer
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	// Peers already configured in yggdrasil.conf are part of the node too. The
	// installer writes the probed list to both files, and an operator may add a
	// peer by hand; adopting them first keeps the total at peerCount instead of
	// letting the configured and the managed lists stack up.
	s.adoptLocked(outbound, settings.Manual)
	// Manual links belong to the operator: put a missing one back, but never
	// count it against peerCount and never replace it.
	s.restoreManualLocked(settings.Manual, outbound, now)
	// A peer that is configured but absent from the admin response is missing
	// from the running node (for example after a Yggdrasil restart, which drops
	// runtime peers). Put it back instead of waiting for the dead-peer timer.
	s.restoreMissingLocked(outbound, now)

	for _, uri := range s.managed {
		if state, ok := outbound[peerKeyOfURI(uri)]; ok && state.Up {
			delete(s.deadSince, uri)
			continue
		}
		if _, ok := s.deadSince[uri]; !ok {
			s.deadSince[uri] = now
			s.log.debug("peer %s is down; waiting %s before replacing it", uri, settings.DeadTimeout)
		}
	}

	if settings.AutoReplace {
		if dead := s.firstDeadLocked(outbound, now, settings); dead != "" {
			s.replaceLocked(ctx, dead, outbound, now, settings)
		}
	}
	s.topUpLocked(ctx, outbound, now, settings)
	s.trimLocked(outbound, now, settings)
	if settings.AutoReplace {
		s.improveLocked(outbound, now, settings)
	}
	s.saveLocked()
}

// adoptLocked adds every outbound peer that is not in the managed list yet.
// The URI from the seed list is preferred: it may carry a ?key= that the admin
// response drops, and the supervisor must be able to re-add the peer later.
// Manual peers are skipped: they are kept by restoreManualLocked and must not
// be trimmed when the automatic list is longer than peerCount.
func (s *peerSupervisor) adoptLocked(outbound map[string]adminPeer, manual []string) {
	known := make(map[string]bool, len(s.managed))
	for _, uri := range s.managed {
		known[peerKeyOfURI(uri)] = true
	}
	for _, uri := range manual {
		known[peerKeyOfURI(uri)] = true
	}
	seeded := make(map[string]string, len(s.seed))
	for _, uri := range s.seed {
		seeded[peerKeyOfURI(uri)] = uri
	}
	for key, peer := range outbound {
		if known[key] {
			continue
		}
		uri := peer.URI
		if fromSeed, ok := seeded[key]; ok {
			uri = fromSeed
		}
		s.managed = append(s.managed, uri)
		known[key] = true
		s.changed = true
		s.log.info("peer %s adopted from the Yggdrasil configuration", uri)
	}
}

// restoreMissingLocked re-adds managed peers that are not configured in the
// running Yggdrasil process. This is what brings the list back after a
// Yggdrasil restart, because peers added through the admin socket live only in
// memory.
func (s *peerSupervisor) restoreMissingLocked(outbound map[string]adminPeer, now time.Time) {
	for _, uri := range s.managed {
		if _, ok := outbound[peerKeyOfURI(uri)]; ok {
			continue
		}
		if err := s.node.AddPeer(uri); err != nil {
			s.warn("could not restore %s: %v", uri, err)
			continue
		}
		s.addedAt[uri] = now
		s.log.info("peer %s restored in the running Yggdrasil node", uri)
	}
}

// restoreManualLocked re-adds the operator's peers. Unlike the managed list
// they are not capped, replaced or removed; a dead manual link is simply dialled
// again on the next check.
func (s *peerSupervisor) restoreManualLocked(manual []string, outbound map[string]adminPeer, now time.Time) {
	for _, uri := range manual {
		if _, ok := outbound[peerKeyOfURI(uri)]; ok {
			continue
		}
		if err := s.node.AddPeer(uri); err != nil {
			s.warn("could not add manual peer %s: %v", uri, err)
			continue
		}
		s.addedAt[uri] = now
		s.log.info("manual peer %s added to the running Yggdrasil node", uri)
	}
}

// reloadConfig applies peer settings when the file changed. Only the peer
// part is hot-reloaded; ports and addresses still need a restart. The overlay
// written by the web interface is read as well, so a manual peer or a new
// peerCount from the dashboard applies without root.
func (s *peerSupervisor) reloadConfig() {
	stamp := configStamp(s.cfgPath)
	if stamp == s.stamp {
		return
	}
	s.stamp = stamp
	cfg, err := loadEffectiveConfig(s.cfgPath)
	if err != nil {
		s.warn("cannot reload %s: %v", s.cfgPath, err)
		return
	}
	next := peerSettingsFrom(cfg)

	s.mu.Lock()
	previous := s.settings
	s.settings = next
	reseeded := false
	if !sameStrings(next.Seed, s.seed) && len(next.Seed) > 0 {
		s.reseedLocked(next.Seed)
		reseeded = true
	}
	s.mu.Unlock()

	if previous.Count != next.Count || previous.AutoReplace != next.AutoReplace {
		s.log.info("peer settings reloaded: %d public peer(s), auto replace %s",
			next.Count, onOff(next.AutoReplace))
	}
	if reseeded {
		s.log.info("public peer list reseeded from %s", s.cfgPath)
	}
}

func (s *peerSupervisor) reseedLocked(seed []string) {
	seed = cleanPeerList(seed)
	if len(seed) == 0 {
		return
	}
	previous := s.managed
	s.managed = append([]string(nil), seed...)
	s.seed = append([]string(nil), seed...)
	wanted := make(map[string]bool, len(seed))
	for _, uri := range seed {
		wanted[uri] = true
	}
	for _, uri := range previous {
		if wanted[uri] {
			continue
		}
		if err := s.node.RemovePeer(uri); err != nil {
			s.log.warn("could not remove %s: %v", uri, err)
		}
		delete(s.deadSince, uri)
		delete(s.addedAt, uri)
	}
	for _, uri := range seed {
		if containsString(previous, uri) {
			continue
		}
		if err := s.node.AddPeer(uri); err != nil {
			s.log.warn("could not add %s: %v", uri, err)
			continue
		}
		s.addedAt[uri] = time.Now()
	}
	s.changed = true
}

func (s *peerSupervisor) firstDeadLocked(outbound map[string]adminPeer, now time.Time, settings peerSettings) string {
	manual := peerKeySet(settings.Manual)
	for _, uri := range s.managed {
		if manual[peerKeyOfURI(uri)] {
			continue
		}
		if state, ok := outbound[peerKeyOfURI(uri)]; ok && state.Up {
			continue
		}
		since, ok := s.deadSince[uri]
		if !ok || now.Sub(since) < settings.DeadTimeout {
			continue
		}
		if born, ok := s.addedAt[uri]; ok && now.Sub(born) < peerGracePeriod {
			continue
		}
		return uri
	}
	return ""
}

func (s *peerSupervisor) replaceLocked(
	ctx context.Context,
	dead string,
	outbound map[string]adminPeer,
	now time.Time,
	settings peerSettings,
) {
	candidate, ok := s.takeCandidateLocked(outbound, now, settings.Manual)
	if !ok {
		s.startProbeLocked(ctx, settings)
		return
	}
	if err := s.node.RemovePeer(dead); err != nil {
		s.log.warn("could not remove %s: %v", dead, err)
	}
	if err := s.node.AddPeer(candidate.URI); err != nil {
		s.log.warn("could not add %s: %v", candidate.URI, err)
		s.penalised[candidate.URI] = now.Add(peerPenalty)
		return
	}
	s.managed = replaceString(s.managed, dead, candidate.URI)
	delete(s.deadSince, dead)
	delete(s.addedAt, dead)
	s.addedAt[candidate.URI] = now
	s.changed = true
	s.log.info("peer %s replaced with %s", dead, candidate.URI)
}

func (s *peerSupervisor) topUpLocked(
	ctx context.Context,
	outbound map[string]adminPeer,
	now time.Time,
	settings peerSettings,
) {
	for s.autoCountLocked(settings.Manual) < settings.Count {
		candidate, ok := s.takeCandidateLocked(outbound, now, settings.Manual)
		if !ok {
			s.startProbeLocked(ctx, settings)
			return
		}
		if err := s.node.AddPeer(candidate.URI); err != nil {
			s.log.warn("could not add %s: %v", candidate.URI, err)
			s.penalised[candidate.URI] = now.Add(peerPenalty)
			continue
		}
		s.managed = append(s.managed, candidate.URI)
		s.addedAt[candidate.URI] = now
		s.changed = true
		s.log.info("peer %s added to the public list", candidate.URI)
	}
}

func (s *peerSupervisor) trimLocked(
	outbound map[string]adminPeer,
	now time.Time,
	settings peerSettings,
) {
	for s.autoCountLocked(settings.Manual) > settings.Count {
		worst := s.worstLocked(outbound, now, false, settings.Manual)
		if worst == "" {
			return
		}
		if err := s.node.RemovePeer(worst); err != nil {
			s.log.warn("could not remove %s: %v", worst, err)
		}
		s.managed = removeString(s.managed, worst)
		delete(s.deadSince, worst)
		delete(s.addedAt, worst)
		s.changed = true
		s.log.info("peer %s removed: peerCount is %d", worst, settings.Count)
	}
}

// autoCountLocked counts the managed peers that are not manual. A manual peer
// can also be present in the managed list, because it is written to
// yggdrasil.conf as well. It must not take a slot of peerCount: otherwise the
// supervisor thinks the list is full while the dashboard shows fewer automatic
// peers than the operator asked for.
func (s *peerSupervisor) autoCountLocked(manual []string) int {
	manualKeys := peerKeySet(manual)
	count := 0
	for _, uri := range s.managed {
		if !manualKeys[peerKeyOfURI(uri)] {
			count++
		}
	}
	return count
}

// upAutoCountLocked is how many automatic managed peers are connected now.
// improvement uses it to keep the last working link: swapping it out would drop
// the node to zero up peers until the new link finishes its handshake.
func (s *peerSupervisor) upAutoCountLocked(outbound map[string]adminPeer, manual []string) int {
	manualKeys := peerKeySet(manual)
	count := 0
	for _, uri := range s.managed {
		if manualKeys[peerKeyOfURI(uri)] {
			continue
		}
		if state, ok := outbound[peerKeyOfURI(uri)]; ok && state.Up {
			count++
		}
	}
	return count
}

// improveLocked swaps the slowest link for a clearly faster candidate. The
// margin keeps the supervisor from chasing latency noise.
func (s *peerSupervisor) improveLocked(
	outbound map[string]adminPeer,
	now time.Time,
	settings peerSettings,
) {
	if len(s.pool) == 0 {
		return
	}
	// Keep the last working link: a replacement spends at least one handshake
	// before it can carry traffic, and a node with a single peer would drop to
	// zero up peers in the meantime.
	if s.upAutoCountLocked(outbound, settings.Manual) < 2 {
		return
	}
	worst, worstLatency := s.worstUpLocked(outbound, now, settings.Manual)
	if worst == "" || worstLatency <= 0 {
		return
	}
	margin := float64(settings.ImprovementPercent)
	for _, candidate := range s.pool {
		if candidate.LatencyMs <= 0 {
			continue
		}
		if candidate.LatencyMs*100 >= worstLatency*(100-margin) {
			continue
		}
		if s.hostInUseLocked(candidate.URI, outbound, settings.Manual) {
			continue
		}
		if err := s.node.RemovePeer(worst); err != nil {
			s.log.warn("could not remove %s: %v", worst, err)
			return
		}
		if err := s.node.AddPeer(candidate.URI); err != nil {
			s.log.warn("could not add %s: %v", candidate.URI, err)
			s.penalised[candidate.URI] = now.Add(peerPenalty)
			return
		}
		s.managed = replaceString(s.managed, worst, candidate.URI)
		delete(s.deadSince, worst)
		delete(s.addedAt, worst)
		s.addedAt[candidate.URI] = now
		s.changed = true
		s.log.info("peer %s replaced with %s: %.1f ms instead of %.1f ms",
			worst, candidate.URI, candidate.LatencyMs, worstLatency)
		return
	}
}

// worstUpLocked returns the slowest live managed peer. A link added recently
// is skipped: its first latency sample is not trustworthy yet.
func (s *peerSupervisor) worstUpLocked(outbound map[string]adminPeer, now time.Time, manual []string) (string, float64) {
	manualKeys := peerKeySet(manual)
	worst := ""
	worstLatency := 0.0
	for _, uri := range s.managed {
		if manualKeys[peerKeyOfURI(uri)] {
			continue
		}
		state, ok := outbound[peerKeyOfURI(uri)]
		if !ok || !state.Up || state.Latency <= 0 {
			continue
		}
		if born, ok := s.addedAt[uri]; ok && now.Sub(born) < peerGracePeriod {
			continue
		}
		latency := float64(state.Latency) / float64(time.Millisecond)
		if worst == "" || latency > worstLatency {
			worst = uri
			worstLatency = latency
		}
	}
	return worst, worstLatency
}

// worstLocked picks the link to drop when the list is longer than peerCount:
// a dead peer first, then the slowest one.
func (s *peerSupervisor) worstLocked(outbound map[string]adminPeer, now time.Time, onlyFresh bool, manual []string) string {
	manualKeys := peerKeySet(manual)
	worst := ""
	worstLatency := 0.0
	worstDown := false
	for _, uri := range s.managed {
		if manualKeys[peerKeyOfURI(uri)] {
			continue
		}
		state, ok := outbound[peerKeyOfURI(uri)]
		up := ok && state.Up
		if onlyFresh {
			if born, ok := s.addedAt[uri]; !ok || now.Sub(born) >= peerGracePeriod {
				continue
			}
		}
		if worst == "" {
			worst, worstDown = uri, !up
			if up && state.Latency > 0 {
				worstLatency = float64(state.Latency) / float64(time.Millisecond)
			}
			continue
		}
		if worstDown && up {
			continue
		}
		if !worstDown && !up {
			worst, worstDown, worstLatency = uri, true, 0
			continue
		}
		if !up {
			continue
		}
		latency := 0.0
		if state.Latency > 0 {
			latency = float64(state.Latency) / float64(time.Millisecond)
		}
		if latency > worstLatency {
			worst, worstLatency = uri, latency
		}
	}
	return worst
}

// takeCandidateLocked removes and returns the best pool entry that is not in
// use, shares no host with a live link and is not penalised.
func (s *peerSupervisor) takeCandidateLocked(outbound map[string]adminPeer, now time.Time, manual []string) (probeResult, bool) {
	used := make(map[string]bool, len(s.managed)+len(outbound)+len(manual))
	for _, uri := range s.managed {
		used[peerHost(uri)] = true
	}
	for _, uri := range manual {
		used[peerHost(uri)] = true
	}
	for uri := range outbound {
		used[peerHost(uri)] = true
	}

	var picked probeResult
	found := false
	kept := s.pool[:0]
	for _, candidate := range s.pool {
		if containsString(s.managed, candidate.URI) {
			continue
		}
		if _, ok := outbound[peerKeyOfURI(candidate.URI)]; ok {
			continue
		}
		if used[peerHost(candidate.URI)] {
			continue
		}
		if until, ok := s.penalised[candidate.URI]; ok && until.After(now) {
			continue
		}
		if !found {
			picked = candidate
			found = true
			continue
		}
		kept = append(kept, candidate)
	}
	s.pool = kept
	return picked, found
}

func (s *peerSupervisor) hostInUseLocked(uri string, outbound map[string]adminPeer, manual []string) bool {
	host := peerHost(uri)
	for _, managed := range s.managed {
		if peerHost(managed) == host {
			return true
		}
	}
	for _, item := range manual {
		if peerHost(item) == host {
			return true
		}
	}
	for peerURI := range outbound {
		if peerHost(peerURI) == host {
			return true
		}
	}
	return false
}

func (s *peerSupervisor) probeDue() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probing {
		return false
	}
	return s.lastProbe.IsZero() || time.Since(s.lastProbe) >= s.settings.RefreshInterval
}

func (s *peerSupervisor) startProbe(ctx context.Context, settings peerSettings) {
	s.mu.Lock()
	s.startProbeLocked(ctx, settings)
	s.mu.Unlock()
}

// startProbeLocked launches the probe; the caller holds s.mu.
func (s *peerSupervisor) startProbeLocked(ctx context.Context, settings peerSettings) {
	if s.probing {
		return
	}
	s.probing = true
	s.progress = peerProbeProgress{Stage: "starting", UpdatedAt: time.Now()}
	// The probe result also feeds the manual peer picker, and the peers that are
	// already connected are filtered out of it. Ask for the automatic count plus
	// a couple of dozen spare candidates, so the picker always has something to
	// show even after the supervisor took its share.
	count := settings.Count + 25
	if count < 25 {
		count = 25
	}
	if count > 60 {
		count = 60
	}
	go func() {
		results, err := s.probe(ctx, count)
		s.mu.Lock()
		s.probing = false
		s.lastProbe = time.Now()
		if err != nil {
			s.progress.Stage = "failed"
			s.progress.UpdatedAt = time.Now()
			s.mu.Unlock()
			s.warn("peer probe failed: %v", err)
			return
		}
		s.pool = s.filterPoolLocked(results)
		s.mu.Unlock()
		s.log.info("peer probe: %d candidate(s) alive", len(results))
	}()
}

// setProbeProgress is called by x-node-peerprobe for every batch it checks.
func (s *peerSupervisor) setProbeProgress(item peerProbeProgress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item.UpdatedAt = time.Now()
	s.progress = item
}

// Progress reports how far the current (or last) catalogue probe got.
func (s *peerSupervisor) Progress() peerProbeProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.progress
}

func (s *peerSupervisor) filterPoolLocked(results []probeResult) []probeResult {
	usedHosts := make(map[string]bool, len(s.managed))
	for _, uri := range s.managed {
		usedHosts[peerHost(uri)] = true
	}
	for _, uri := range s.settings.Manual {
		usedHosts[peerHost(uri)] = true
	}
	now := time.Now()
	out := make([]probeResult, 0, len(results))
	for _, result := range results {
		uri := strings.TrimSpace(result.URI)
		if uri == "" || containsString(s.managed, uri) {
			continue
		}
		host := peerHost(uri)
		if usedHosts[host] {
			continue
		}
		if until, ok := s.penalised[uri]; ok && until.After(now) {
			continue
		}
		usedHosts[host] = true
		out = append(out, probeResult{
			URI:       uri,
			LatencyMs: result.LatencyMs,
			Country:   result.Country,
			City:      result.City,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].LatencyMs < out[j].LatencyMs
	})
	return out
}

// Candidates returns the probed peers that are not in use, best first. The web
// interface shows them as a list, so an operator can pin one by hand instead of
// typing a URI from a browser tab.
func (s *peerSupervisor) Candidates() []probeResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]probeResult, len(s.pool))
	copy(out, s.pool)
	return out
}

// Probing reports whether a probe is running right now, so the web interface
// can show a spinner instead of an empty list.
func (s *peerSupervisor) Probing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probing
}

// ProbeIfEmpty fills the candidate pool when nothing has been probed yet. The
// supervisor skips the catalogue download while the managed list is complete,
// so without this the manual peer picker stays empty until the first refresh
// interval or a dead peer.
func (s *peerSupervisor) ProbeIfEmpty() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probing || len(s.pool) > 0 || s.runCtx == nil {
		return
	}
	// A finished probe with no survivors must not start again on every status
	// poll: that kept the web button on "Searching…" forever. Wait for the
	// normal refresh interval before trying the catalogue again.
	if !s.lastProbe.IsZero() && time.Since(s.lastProbe) < s.settings.RefreshInterval {
		return
	}
	s.startProbeLocked(s.runCtx, s.settings)
}

// takeRefreshRequest consumes the marker written by "x-node peers refresh".
func (s *peerSupervisor) takeRefreshRequest() bool {
	path := filepath.Join(filepath.Dir(s.statePath), peerRefreshFile)
	if _, err := os.Stat(path); err != nil {
		return false
	}
	_ = os.Remove(path)
	return true
}

func (s *peerSupervisor) saveState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changed = true
	s.saveLocked()
}

func (s *peerSupervisor) saveLocked() {
	if !s.changed {
		return
	}
	state := peersState{
		Seed:    append([]string(nil), s.seed...),
		Managed: append([]string(nil), s.managed...),
	}
	if err := writePeersState(s.statePath, state); err != nil {
		s.log.warn("could not save %s: %v", s.statePath, err)
		return
	}
	s.changed = false
}

// warn logs a repeated error once per five minutes: a Yggdrasil that is down
// would otherwise fill the journal with the same line every check.
func (s *peerSupervisor) warn(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	s.warnMu.Lock()
	defer s.warnMu.Unlock()
	if text == s.warnText && time.Since(s.lastWarn) < 5*time.Minute {
		return
	}
	s.warnText = text
	s.lastWarn = time.Now()
	s.log.warn("%s", text)
}

func readPeersState(path string) (peersState, error) {
	var state peersState
	raw, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, err
	}
	return state, nil
}

func writePeersState(path string, state peersState) error {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// probeStderr splits the stderr of x-node-peerprobe into progress lines and
// ordinary error text. The command writes both, so the daemon cannot simply
// buffer one of them.
type probeStderr struct {
	report func(peerProbeProgress)
	errors bytes.Buffer
	line   []byte
}

func (w *probeStderr) Write(chunk []byte) (int, error) {
	w.line = append(w.line, chunk...)
	for {
		index := bytes.IndexByte(w.line, '\n')
		if index < 0 {
			break
		}
		w.handle(string(w.line[:index]))
		w.line = w.line[index+1:]
	}
	return len(chunk), nil
}

func (w *probeStderr) flush() {
	if len(w.line) > 0 {
		w.handle(string(w.line))
		w.line = nil
	}
}

func (w *probeStderr) handle(line string) {
	line = strings.TrimRight(line, "\r")
	payload, ok := strings.CutPrefix(line, "progress ")
	if !ok {
		w.errors.WriteString(line)
		w.errors.WriteByte('\n')
		return
	}
	var item peerProbeProgress
	if err := json.Unmarshal([]byte(payload), &item); err == nil && w.report != nil {
		w.report(item)
	}
}

// newPeerProbe runs x-node-peerprobe, which downloads the public peer list and
// completes a real Yggdrasil handshake with every candidate it reports. The
// probe prints "progress {json}" lines to stderr; they are forwarded to report
// while the search is running.
func newPeerProbe(path string, report func(peerProbeProgress)) func(context.Context, int) ([]probeResult, error) {
	return func(ctx context.Context, count int) ([]probeResult, error) {
		binary := strings.TrimSpace(path)
		// A configuration written by an older version may point at a binary the
		// installer no longer places there; the default is always present.
		if binary == "" {
			binary = defaultPeerProbePath
		} else if _, err := os.Stat(binary); err != nil {
			binary = defaultPeerProbePath
		}
		runCtx, cancel := context.WithTimeout(ctx, peerProbeTimeout)
		defer cancel()
		cmd := exec.CommandContext(runCtx, binary, "-count", strconv.Itoa(count), "-progress")
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		stderr := &probeStderr{report: report}
		cmd.Stderr = stderr
		err := cmd.Run()
		stderr.flush()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.errors.String()))
			}
			return nil, err
		}
		var results []probeResult
		if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
			return nil, fmt.Errorf("decode probe output: %w", err)
		}
		sort.SliceStable(results, func(i, j int) bool {
			return results[i].LatencyMs < results[j].LatencyMs
		})
		return results, nil
	}
}

// peerHost is the host of a peer URI, used to keep one link per server.
func peerHost(uri string) string {
	parsed, err := url.Parse(strings.TrimSpace(uri))
	if err != nil {
		return uri
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return uri
	}
	return host
}

// peerKeyOfURI drops the query string, which Yggdrasil removes from the link
// info it reports for a live peer.
func peerKeyOfURI(uri string) string {
	parsed, err := url.Parse(strings.TrimSpace(uri))
	if err != nil {
		return uri
	}
	host := parsed.Hostname()
	if host == "" {
		return uri
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := ""
	if parsed.Port() != "" {
		port = ":" + parsed.Port()
	}
	return parsed.Scheme + "://" + host + port + parsed.Path
}

// peerKeySet maps peer keys without their query strings, for lookups that must
// treat tls://host:port?key=... and tls://host:port as one link.
func peerKeySet(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, uri := range list {
		out[peerKeyOfURI(uri)] = true
	}
	return out
}

func cleanPeerList(list []string) []string {
	out := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, raw := range list {
		uri := strings.TrimSpace(raw)
		if uri == "" || seen[uri] {
			continue
		}
		if err := validatePeerURI(uri); err != nil {
			continue
		}
		seen[uri] = true
		out = append(out, uri)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, value := range a {
		seen[value]++
	}
	for _, value := range b {
		if seen[value] == 0 {
			return false
		}
		seen[value]--
	}
	return true
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func replaceString(list []string, old, next string) []string {
	out := make([]string, 0, len(list))
	for _, item := range list {
		if item == old {
			item = next
		}
		out = append(out, item)
	}
	return out
}

func removeString(list []string, value string) []string {
	out := make([]string, 0, len(list))
	for _, item := range list {
		if item != value {
			out = append(out, item)
		}
	}
	return out
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}
