package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cmdPeers reports and changes the public peer settings. The running service
// re-reads the config on every check, so no restart is needed.
func cmdPeers(args []string) error {
	action := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action = args[0]
		args = args[1:]
	}
	switch action {
	case "status":
		return peersStatus(args)
	case "count":
		return peersSetCount(args)
	case "auto":
		return peersSetAuto(args)
	case "refresh":
		return peersRefresh(args)
	default:
		return fmt.Errorf("unknown peers action %q (use status, count, auto or refresh)", action)
	}
}

type peerRow struct {
	URI       string  `json:"uri"`
	Up        bool    `json:"up"`
	LatencyMs float64 `json:"latencyMs"`
}

func peersStatus(args []string) error {
	fs := flag.NewFlagSet("peers", flag.ContinueOnError)
	configFlag := addConfigFlag(fs)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(configPath(*configFlag))
	if err != nil {
		return err
	}

	managed := cleanPeerList(cfg.Peers)
	if state, err := readPeersState(cfg.statePath(peersStateFile)); err == nil && len(state.Managed) > 0 {
		managed = cleanPeerList(state.Managed)
	}

	rows := make([]peerRow, 0, len(managed))
	live, liveErr := newAdminSocket(cfg.AdminSocket, 3*time.Second).Peers()
	byKey := make(map[string]adminPeer, len(live))
	for _, peer := range live {
		byKey[peer.URI] = peer
		byKey[peerKeyOfURI(peer.URI)] = peer
	}
	for _, uri := range managed {
		row := peerRow{URI: uri}
		state, ok := byKey[uri]
		if !ok {
			state, ok = byKey[peerKeyOfURI(uri)]
		}
		if ok {
			row.Up = state.Up
			if state.Latency > 0 {
				row.LatencyMs = float64(state.Latency) / float64(time.Millisecond)
			}
		}
		rows = append(rows, row)
	}

	if *asJSON {
		raw, _ := json.MarshalIndent(map[string]any{
			"ok":             liveErr == nil,
			"peerCount":      cfg.PeerCount,
			"autoReplace":    cfg.PeerAutoReplace,
			"managed":        rows,
			"adminReachable": liveErr == nil,
		}, "", "  ")
		fmt.Println(string(raw))
		return nil
	}

	fmt.Printf("Peer count:    %d  (public peers managed by x-node)\n", cfg.PeerCount)
	fmt.Printf("Auto replace:  %s\n", onOff(cfg.PeerAutoReplace))
	fmt.Printf("Timing:        check every %s, replace after %s, refresh every %s\n",
		cfg.PeerCheckInterval, cfg.PeerDeadTimeout, cfg.PeerRefreshInterval)
	fmt.Printf("Managed:       %d of %d\n", len(rows), cfg.PeerCount)
	if liveErr != nil {
		fmt.Printf("Yggdrasil:     admin socket is not reachable: %v\n", liveErr)
	}
	if len(rows) == 0 {
		fmt.Println("No public peers are managed yet; the service fills the list on the next check.")
		return nil
	}
	fmt.Println()
	fmt.Printf("%-46s %-7s %s\n", "URI", "STATE", "LATENCY")
	for _, row := range rows {
		state := "unknown"
		if liveErr == nil {
			if row.Up {
				state = "up"
			} else {
				state = "down"
			}
		}
		latency := "—"
		if row.LatencyMs > 0 {
			latency = fmt.Sprintf("%.1f ms", row.LatencyMs)
		}
		fmt.Printf("%-46s %-7s %s\n", truncate(row.URI, 46), state, latency)
	}
	fmt.Println()
	fmt.Println("Inbound clients are never counted against peerCount and are never replaced.")
	return nil
}

func peersSetCount(args []string) error {
	fs := flag.NewFlagSet("peers count", flag.ContinueOnError)
	configFlag := addConfigFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	value := strings.TrimSpace(fs.Arg(0))
	count, err := strconv.Atoi(value)
	if err != nil || count < 1 || count > 50 {
		return fmt.Errorf("peer count must be a number between 1 and 50")
	}
	path := configPath(*configFlag)
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	if err := updatePeerConfig(path, func(doc map[string]any) {
		doc["peerCount"] = count
	}); err != nil {
		return err
	}
	fmt.Printf("peerCount set to %d; the service applies it within %s\n", count, cfg.PeerCheckInterval)
	return nil
}

func peersSetAuto(args []string) error {
	fs := flag.NewFlagSet("peers auto", flag.ContinueOnError)
	configFlag := addConfigFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	var enabled bool
	switch strings.ToLower(strings.TrimSpace(fs.Arg(0))) {
	case "on", "true", "1", "yes":
		enabled = true
	case "off", "false", "0", "no":
		enabled = false
	default:
		return fmt.Errorf("use \"peers auto on\" or \"peers auto off\"")
	}
	path := configPath(*configFlag)
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	if err := updatePeerConfig(path, func(doc map[string]any) {
		doc["peerAutoReplace"] = enabled
	}); err != nil {
		return err
	}
	fmt.Printf("automatic replacement is %s; the service applies it within %s\n",
		onOff(enabled), cfg.PeerCheckInterval)
	return nil
}

func peersRefresh(args []string) error {
	fs := flag.NewFlagSet("peers refresh", flag.ContinueOnError)
	configFlag := addConfigFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(configPath(*configFlag))
	if err != nil {
		return err
	}
	path := cfg.statePath(peerRefreshFile)
	if err := os.WriteFile(path, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Printf("A peer probe will run on the next check (within %s).\n", cfg.PeerCheckInterval)
	return nil
}

// updatePeerConfig rewrites the JSON file atomically and keeps its owner and
// group, so the service can still read a file changed by root.
func updatePeerConfig(path string, mutate func(map[string]any)) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	mutate(doc)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o640); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	chownToDir(name, dir)
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return nil
}
