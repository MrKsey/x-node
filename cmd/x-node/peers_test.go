package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeNode is an in-memory Yggdrasil admin API. Clients are represented by
// inbound entries, which the supervisor must never touch.
type fakeNode struct {
	peers   []adminPeer
	added   []string
	removed []string
	err     error
}

func (f *fakeNode) Peers() ([]adminPeer, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.peers, nil
}

func (f *fakeNode) AddPeer(uri string) error {
	f.added = append(f.added, uri)
	f.peers = append(f.peers, adminPeer{URI: uri, Up: true})
	return nil
}

func (f *fakeNode) RemovePeer(uri string) error {
	f.removed = append(f.removed, uri)
	kept := f.peers[:0]
	for _, peer := range f.peers {
		if peer.URI != uri {
			kept = append(kept, peer)
		}
	}
	f.peers = kept
	return nil
}

func newTestSupervisor(t *testing.T, cfg Config, node peerNode, pool []probeResult) *peerSupervisor {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s := newPeerSupervisor(cfg, path, newLogger("error"))
	s.node = node
	s.probe = func(context.Context, int) ([]probeResult, error) { return nil, nil }
	s.pool = pool
	s.lastProbe = time.Now()
	return s
}

func testPeerConfig(t *testing.T, count int) Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.PeerCount = count
	cfg.PeerAutoReplace = true
	cfg.PeerDeadTimeout = "90s"
	cfg.PeerCheckInterval = "30s"
	cfg.PeerRefreshInterval = "6h"
	cfg.PeerImprovementPercent = 25
	cfg.AdminSocket = "unix:///nonexistent.sock"
	return cfg
}

func TestPeerSupervisorFillsTheList(t *testing.T) {
	cfg := testPeerConfig(t, 3)
	cfg.Peers = []string{"tls://a.example:1"}
	node := &fakeNode{peers: []adminPeer{{URI: "tls://a.example:1", Up: true}}}
	pool := []probeResult{
		{URI: "tls://b.example:2", LatencyMs: 20},
		{URI: "tls://c.example:3", LatencyMs: 30},
	}
	s := newTestSupervisor(t, cfg, node, pool)
	s.step(context.Background())

	if len(s.managed) != 3 {
		t.Fatalf("managed = %v, want 3 peers", s.managed)
	}
	if len(node.added) != 2 {
		t.Fatalf("added = %v, want b and c", node.added)
	}
	if len(node.removed) != 0 {
		t.Fatalf("removed = %v, want nothing", node.removed)
	}
}

func TestPeerSupervisorAdoptsConfiguredPeersInsteadOfDuplicating(t *testing.T) {
	cfg := testPeerConfig(t, 3)
	// The installer writes the same three peers to yggdrasil.conf and to the
	// managed list; an older release added three more through the admin socket.
	cfg.Peers = []string{"tls://seed1.example:1", "tls://seed2.example:2", "tls://seed3.example:3"}
	state := peersState{
		Seed:    cfg.Peers,
		Managed: []string{"tcp://managed1.example:1", "tcp://managed2.example:2", "tcp://managed3.example:3"},
	}
	if err := writePeersState(cfg.statePath(peersStateFile), state); err != nil {
		t.Fatal(err)
	}
	node := &fakeNode{peers: []adminPeer{
		{URI: "tls://seed1.example:1", Up: true},
		{URI: "tls://seed2.example:2", Up: true},
		{URI: "tls://seed3.example:3", Up: true},
		{URI: "tcp://managed1.example:1", Up: true},
		{URI: "tcp://managed2.example:2", Up: true},
		{URI: "tcp://managed3.example:3", Up: true},
	}}
	s := newTestSupervisor(t, cfg, node, nil)
	s.step(context.Background())

	if len(s.managed) != 3 {
		t.Fatalf("managed = %v, want peerCount peers", s.managed)
	}
	if len(node.removed) != 3 {
		t.Fatalf("removed = %v, want the three extra links", node.removed)
	}
}

func TestPeerSupervisorRestoresPeersAfterYggdrasilRestart(t *testing.T) {
	cfg := testPeerConfig(t, 2)
	cfg.Peers = []string{"tls://a.example:1", "tls://b.example:2"}
	node := &fakeNode{}
	s := newTestSupervisor(t, cfg, node, nil)
	s.step(context.Background())

	if len(node.added) != 2 {
		t.Fatalf("added = %v, want both managed peers re-added", node.added)
	}
	if len(s.managed) != 2 {
		t.Fatalf("managed = %v", s.managed)
	}
}

func TestPeerSupervisorReplacesDeadPeer(t *testing.T) {
	cfg := testPeerConfig(t, 3)
	cfg.Peers = []string{"tls://a.example:1", "tls://b.example:2", "tls://c.example:3"}
	node := &fakeNode{peers: []adminPeer{
		{URI: "tls://a.example:1", Up: true},
		{URI: "tls://b.example:2", Up: false},
		{URI: "tls://c.example:3", Up: true},
		{URI: "tls://phone:443", Inbound: true, Up: true},
	}}
	pool := []probeResult{{URI: "tls://d.example:4", LatencyMs: 15}}
	s := newTestSupervisor(t, cfg, node, pool)
	s.deadSince["tls://b.example:2"] = time.Now().Add(-2 * time.Minute)
	s.step(context.Background())

	if !containsString(node.removed, "tls://b.example:2") {
		t.Fatalf("removed = %v, want the dead peer", node.removed)
	}
	if !containsString(node.added, "tls://d.example:4") {
		t.Fatalf("added = %v, want the replacement", node.added)
	}
	if containsString(node.removed, "tls://phone:443") {
		t.Fatal("an inbound client must never be removed")
	}
	if !containsString(s.managed, "tls://d.example:4") || containsString(s.managed, "tls://b.example:2") {
		t.Fatalf("managed = %v", s.managed)
	}
}

func TestPeerSupervisorLeavesClientsAlone(t *testing.T) {
	cfg := testPeerConfig(t, 1)
	cfg.Peers = []string{"tls://a.example:1"}
	node := &fakeNode{peers: []adminPeer{
		{URI: "tls://a.example:1", Up: true},
		{URI: "tls://phone:443", Inbound: true, Up: true},
	}}
	s := newTestSupervisor(t, cfg, node, nil)
	s.step(context.Background())

	if len(node.removed) != 0 {
		t.Fatalf("removed = %v, want nothing: clients are inbound peers", node.removed)
	}
	if len(s.managed) != 1 || s.managed[0] != "tls://a.example:1" {
		t.Fatalf("managed = %v, want only the public peer", s.managed)
	}
}

func TestPeerSupervisorKeepsDeadPeerWhenAutoReplaceIsOff(t *testing.T) {
	cfg := testPeerConfig(t, 3)
	cfg.PeerAutoReplace = false
	cfg.Peers = []string{"tls://a.example:1", "tls://b.example:2", "tls://c.example:3"}
	node := &fakeNode{peers: []adminPeer{
		{URI: "tls://a.example:1", Up: true},
		{URI: "tls://b.example:2", Up: false},
		{URI: "tls://c.example:3", Up: true},
	}}
	pool := []probeResult{{URI: "tls://d.example:4", LatencyMs: 15}}
	s := newTestSupervisor(t, cfg, node, pool)
	s.deadSince["tls://b.example:2"] = time.Now().Add(-2 * time.Minute)
	s.step(context.Background())

	if len(node.added) != 0 || len(node.removed) != 0 {
		t.Fatalf("added = %v, removed = %v; auto replace is off", node.added, node.removed)
	}
}

func TestPeerSupervisorTrimsToCount(t *testing.T) {
	cfg := testPeerConfig(t, 2)
	cfg.Peers = []string{
		"tls://a.example:1", "tls://b.example:2",
		"tls://c.example:3", "tls://d.example:4",
	}
	node := &fakeNode{peers: []adminPeer{
		{URI: "tls://a.example:1", Up: true, Latency: 10 * time.Millisecond},
		{URI: "tls://b.example:2", Up: true, Latency: 20 * time.Millisecond},
		{URI: "tls://c.example:3", Up: true, Latency: 30 * time.Millisecond},
		{URI: "tls://d.example:4", Up: true, Latency: 40 * time.Millisecond},
	}}
	s := newTestSupervisor(t, cfg, node, nil)
	s.step(context.Background())

	if len(s.managed) != 2 {
		t.Fatalf("managed = %v, want 2 peers", s.managed)
	}
	if containsString(s.managed, "tls://d.example:4") || containsString(s.managed, "tls://c.example:3") {
		t.Fatalf("managed = %v, want the two fastest peers", s.managed)
	}
}

func TestPeerSupervisorDoesNotLetManualPeersFillAutomaticSlots(t *testing.T) {
	cfg := testPeerConfig(t, 2)
	// The manual peer is also in the seed list, as an older release wrote it
	// to both places. It must not count as one of the two automatic peers.
	cfg.Peers = []string{"tls://manual.example:1", "tls://a.example:2"}
	cfg.ManualPeers = []string{"tls://manual.example:1"}
	node := &fakeNode{peers: []adminPeer{
		{URI: "tls://manual.example:1", Up: true},
		{URI: "tls://a.example:2", Up: true},
	}}
	s := newTestSupervisor(t, cfg, node, []probeResult{{URI: "tls://b.example:3", LatencyMs: 20}})
	s.step(context.Background())

	if !containsString(node.added, "tls://b.example:3") {
		t.Fatalf("added = %v, want a second automatic peer", node.added)
	}
	if got := s.autoCountLocked(cfg.ManualPeers); got != 2 {
		t.Fatalf("automatic peers = %d, want 2", got)
	}
}

func TestPeerSupervisorKeepsTheLastWorkingLink(t *testing.T) {
	cfg := testPeerConfig(t, 2)
	cfg.Peers = []string{"tls://a.example:1", "tls://b.example:2"}
	node := &fakeNode{peers: []adminPeer{
		{URI: "tls://a.example:1", Up: true, Latency: 100 * time.Millisecond},
		{URI: "tls://b.example:2", Up: false},
	}}
	// The candidate is much faster, but swapping the only live link would leave
	// the node without a working peer until the new one finishes its handshake.
	s := newTestSupervisor(t, cfg, node, []probeResult{{URI: "tls://c.example:3", LatencyMs: 5}})
	s.step(context.Background())

	if containsString(node.removed, "tls://a.example:1") {
		t.Fatalf("removed = %v, want the only up peer kept", node.removed)
	}
}

func TestPeerSupervisorSwapsInFasterPeer(t *testing.T) {
	cfg := testPeerConfig(t, 3)
	cfg.Peers = []string{"tls://a.example:1", "tls://b.example:2", "tls://c.example:3"}
	node := &fakeNode{peers: []adminPeer{
		{URI: "tls://a.example:1", Up: true, Latency: 10 * time.Millisecond},
		{URI: "tls://b.example:2", Up: true, Latency: 20 * time.Millisecond},
		{URI: "tls://c.example:3", Up: true, Latency: 100 * time.Millisecond},
	}}
	s := newTestSupervisor(t, cfg, node, []probeResult{{URI: "tls://d.example:4", LatencyMs: 10}})
	s.step(context.Background())

	if !containsString(node.removed, "tls://c.example:3") {
		t.Fatalf("removed = %v, want the slowest peer", node.removed)
	}
	if !containsString(node.added, "tls://d.example:4") {
		t.Fatalf("added = %v, want the faster candidate", node.added)
	}
}

func TestPeerSupervisorKeepsPeerWhenCandidateIsNotFasterEnough(t *testing.T) {
	cfg := testPeerConfig(t, 3)
	cfg.Peers = []string{"tls://a.example:1", "tls://b.example:2", "tls://c.example:3"}
	node := &fakeNode{peers: []adminPeer{
		{URI: "tls://a.example:1", Up: true, Latency: 10 * time.Millisecond},
		{URI: "tls://b.example:2", Up: true, Latency: 20 * time.Millisecond},
		{URI: "tls://c.example:3", Up: true, Latency: 100 * time.Millisecond},
	}}
	// 90 ms is better than 100 ms, but not by the 25 % margin.
	s := newTestSupervisor(t, cfg, node, []probeResult{{URI: "tls://d.example:4", LatencyMs: 90}})
	s.step(context.Background())

	if len(node.removed) != 0 || len(node.added) != 0 {
		t.Fatalf("added = %v, removed = %v; the margin must stop the swap", node.added, node.removed)
	}
}

func TestPeerSupervisorReseedsFromConfig(t *testing.T) {
	cfg := testPeerConfig(t, 2)
	cfg.Peers = []string{"tls://a.example:1", "tls://b.example:2"}
	node := &fakeNode{peers: []adminPeer{
		{URI: "tls://a.example:1", Up: true},
		{URI: "tls://b.example:2", Up: true},
	}}
	s := newTestSupervisor(t, cfg, node, nil)
	s.step(context.Background())

	cfg.Peers = []string{"tls://x1.example:7", "tls://y2.example:8"}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s.step(context.Background())

	if !sameStrings(s.managed, []string{"tls://x1.example:7", "tls://y2.example:8"}) {
		t.Fatalf("managed = %v, want the new seed", s.managed)
	}
	if !containsString(node.removed, "tls://a.example:1") {
		t.Fatalf("removed = %v, want the old peers", node.removed)
	}
}

func TestPeerSupervisorPersistsState(t *testing.T) {
	cfg := testPeerConfig(t, 2)
	cfg.Peers = []string{"tls://a.example:1"}
	node := &fakeNode{peers: []adminPeer{
		{URI: "tls://a.example:1", Up: true},
	}}
	// The supervisor adds the second peer, which changes the list and writes
	// the state file.
	s := newTestSupervisor(t, cfg, node, []probeResult{{URI: "tls://b.example:2", LatencyMs: 20}})
	s.step(context.Background())

	state, err := readPeersState(cfg.statePath(peersStateFile))
	if err != nil {
		t.Fatal(err)
	}
	if !sameStrings(state.Managed, s.managed) {
		t.Fatalf("state = %v, want %v", state.Managed, s.managed)
	}

	// A restart picks the stored list, not the config seed.
	cfg.Peers = []string{"tls://other.example:9"}
	restarted := newPeerSupervisor(cfg, s.cfgPath, newLogger("error"))
	if !sameStrings(restarted.managed, state.Managed) {
		t.Fatalf("restart managed = %v, want %v", restarted.managed, state.Managed)
	}
}

func TestPeerSupervisorMatchesPeersWithoutQuery(t *testing.T) {
	// The admin API reports a live peer without the ?key= option, while the
	// configured URI still carries it. The link must not look dead.
	cfg := testPeerConfig(t, 1)
	cfg.Peers = []string{"tls://a.example:1?key=abc"}
	node := &fakeNode{peers: []adminPeer{{URI: "tls://a.example:1", Up: true}}}
	s := newTestSupervisor(t, cfg, node, nil)
	s.step(context.Background())

	if len(node.removed) != 0 {
		t.Fatalf("removed = %v, want nothing: the peer is up", node.removed)
	}

	// A dead peer is removed by the URI that was configured, query included.
	cfg.Peers = []string{"tls://a.example:1?key=abc"}
	node = &fakeNode{peers: []adminPeer{{URI: "tls://a.example:1", Up: false}}}
	s = newTestSupervisor(t, cfg, node, []probeResult{{URI: "tls://b.example:2", LatencyMs: 15}})
	s.deadSince["tls://a.example:1?key=abc"] = time.Now().Add(-2 * time.Minute)
	s.step(context.Background())

	if !containsString(node.removed, "tls://a.example:1?key=abc") {
		t.Fatalf("removed = %v, want the configured URI", node.removed)
	}
}

func TestAdminSocketProtocol(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	requests := make(chan string, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var request adminRequest
				if err := json.NewDecoder(conn).Decode(&request); err != nil {
					return
				}
				requests <- request.Name
				switch request.Name {
				case "getPeers":
					_ = json.NewEncoder(conn).Encode(map[string]any{
						"status": "success",
						"response": map[string]any{
							"peers": []map[string]any{{
								"remote":  "tls://a.example:1",
								"up":      true,
								"inbound": false,
								"latency": int64(24 * time.Millisecond),
							}},
						},
					})
				case "addPeer", "removePeer":
					_ = json.NewEncoder(conn).Encode(map[string]any{"status": "success"})
				}
			}(conn)
		}
	}()

	socket := newAdminSocket("tcp://"+listener.Addr().String(), time.Second)
	peers, err := socket.Peers()
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].URI != "tls://a.example:1" || peers[0].Latency != 24*time.Millisecond {
		t.Fatalf("peers = %+v", peers)
	}
	if err := socket.AddPeer("tls://b.example:2"); err != nil {
		t.Fatal(err)
	}
	if err := socket.RemovePeer("tls://a.example:1"); err != nil {
		t.Fatal(err)
	}
	close(requests)
	var seen []string
	for name := range requests {
		seen = append(seen, name)
	}
	if len(seen) != 3 {
		t.Fatalf("requests = %v", seen)
	}
}

func TestAdminSocketErrorResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var request adminRequest
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			return
		}
		_ = json.NewEncoder(conn).Encode(map[string]any{
			"status": "error",
			"error":  "peer is not configured",
		})
	}()

	socket := newAdminSocket("tcp://"+listener.Addr().String(), time.Second)
	if err := socket.RemovePeer("tls://a.example:1"); err != nil {
		t.Fatalf("a missing peer is not an error: %v", err)
	}
}

func TestAdminTarget(t *testing.T) {
	cases := []struct {
		address string
		network string
		target  string
	}{
		{"unix:///var/run/yggdrasil/yggdrasil.sock", "unix", "/var/run/yggdrasil/yggdrasil.sock"},
		{"/var/run/yggdrasil/yggdrasil.sock", "unix", "/var/run/yggdrasil/yggdrasil.sock"},
		{"tcp://127.0.0.1:9001", "tcp", "127.0.0.1:9001"},
	}
	for _, item := range cases {
		network, target, err := adminTarget(item.address)
		if err != nil {
			t.Fatalf("%s: %v", item.address, err)
		}
		if network != item.network || target != item.target {
			t.Fatalf("%s = %s %s", item.address, network, target)
		}
	}
	if _, _, err := adminTarget("http://127.0.0.1:9001"); err == nil {
		t.Fatal("http:// must be rejected")
	}
}

// A manual peer is often written to the managed list as well. It must be
// counted once, as a manual peer, so the automatic total the status line
// reports still matches peerCount.
func TestSummarizePeersSeparatesManualPeers(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var request adminRequest
				if err := json.NewDecoder(conn).Decode(&request); err != nil {
					return
				}
				if request.Name != "getPeers" {
					_ = json.NewEncoder(conn).Encode(map[string]any{"status": "success"})
					return
				}
				_ = json.NewEncoder(conn).Encode(map[string]any{
					"status": "success",
					"response": map[string]any{
						"peers": []map[string]any{
							{"remote": "tls://auto-a.example:1", "up": true, "inbound": false},
							{"remote": "tls://auto-b.example:2", "up": true, "inbound": false},
							{"remote": "tls://manual-a.example:3", "up": true, "inbound": false},
							{"remote": "tls://manual-b.example:4", "up": false, "inbound": false},
							{"remote": "ws://127.0.0.1:5000", "up": true, "inbound": true},
						},
					},
				})
			}(conn)
		}
	}()

	dir := t.TempDir()
	state := peersState{Managed: []string{
		"tls://auto-a.example:1",
		"tls://auto-b.example:2",
		"tls://manual-a.example:3",
	}}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, peersStateFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		StateDir:    dir,
		AdminSocket: "tcp://" + listener.Addr().String(),
		PeerCount:   2,
		ManualPeers: []string{"tls://manual-a.example:3", "tls://manual-b.example:4"},
	}
	counts := summarizePeers(cfg)
	if counts.Automatic != 2 {
		t.Fatalf("Automatic = %d, want 2", counts.Automatic)
	}
	if counts.AutomaticUp != 2 {
		t.Fatalf("AutomaticUp = %d, want 2", counts.AutomaticUp)
	}
	if counts.Manual != 2 {
		t.Fatalf("Manual = %d, want 2", counts.Manual)
	}
	if counts.ManualUp != 1 {
		t.Fatalf("ManualUp = %d, want 1", counts.ManualUp)
	}
	if counts.Inbound != 1 {
		t.Fatalf("Inbound = %d, want 1", counts.Inbound)
	}
}

func TestPeerKeyOfURIDropsQuery(t *testing.T) {
	got := peerKeyOfURI("wss://160.20.158.195:443/ygg?key=abc")
	if got != "wss://160.20.158.195:443/ygg" {
		t.Fatalf("peerKeyOfURI = %q", got)
	}
	got = peerKeyOfURI("tls://[201:db8::1]:13338?key=abc")
	if got != "tls://[201:db8::1]:13338" {
		t.Fatalf("peerKeyOfURI = %q", got)
	}
}

func TestConfigPeerDefaults(t *testing.T) {
	cfg := defaultConfig()
	cfg.applyDefaults()
	if cfg.PeerCount != 3 {
		t.Fatalf("PeerCount = %d", cfg.PeerCount)
	}
	if !cfg.PeerAutoReplace {
		t.Fatal("automatic replacement must be on by default")
	}
	if _, err := cfg.peerDeadTimeout(); err != nil {
		t.Fatal(err)
	}
	cfg.ListenAddress = "202:24ae:c219:8a4a:de94:eae2:b97e:ac87"
	if err := cfg.validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}

	cfg.PeerCount = 0
	if err := cfg.validate(); err == nil {
		t.Fatal("peerCount 0 must be rejected")
	}
	cfg.PeerCount = 51
	if err := cfg.validate(); err == nil {
		t.Fatal("peerCount 51 must be rejected")
	}
	cfg.PeerCount = 3
	cfg.Peers = []string{"socks://a.example:1"}
	if err := cfg.validate(); err == nil {
		t.Fatal("a socks:// peer must be rejected")
	}
}
