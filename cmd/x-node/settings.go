package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// settings holds the configuration that is currently in effect. The daemon
// reads it per operation, so a change made in the web interface applies to the
// next connection without a restart. Fields that require a listener to be
// rebound (addresses, ports, TLS mode) are reported as "restart required".
type settings struct {
	mu  sync.RWMutex
	cfg Config
}

func newSettings(cfg Config) *settings {
	return &settings{cfg: cfg}
}

func (s *settings) get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *settings) set(cfg Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// update applies a change to the live copy and returns the new configuration.
func (s *settings) update(change func(*Config)) Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(&s.cfg)
	return s.cfg
}

// settingsFileName is the overlay the web interface writes. It lives in the
// state directory, which the service owns, so the UI never needs root and the
// file written by the installer in /etc/x-node stays untouched.
const settingsFileName = "settings.json"

// loadSettingsOverlay applies the partial document written by the web
// interface over the file configuration. A missing key keeps the value from
// /etc/x-node/config.json.
func loadSettingsOverlay(cfg Config) (Config, error) {
	path := cfg.statePath(settingsFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	merged := cfg
	if err := json.Unmarshal(raw, &merged); err != nil {
		return cfg, err
	}
	merged.applyDefaults()
	if err := merged.validate(); err != nil {
		return cfg, err
	}
	return merged, nil
}

// saveSettingsOverlay writes the managed fields as a partial document. Only
// the keys the web interface is allowed to change are written, so a future
// configuration key is not frozen by an old overlay.
func saveSettingsOverlay(cfg Config) error {
	document := map[string]any{
		"allowAll":               cfg.AllowAll,
		"registration":           cfg.Registration,
		"inviteTTL":              cfg.InviteTTL,
		"inviteUses":             cfg.InviteUses,
		"maxClients":             cfg.MaxClients,
		"maxConnsPerClient":      cfg.MaxConnsPerClient,
		"maxConnsTotal":          cfg.MaxConnsTotal,
		"dialTimeout":            cfg.DialTimeout,
		"idleTimeout":            cfg.IdleTimeout,
		"udpEnabled":             cfg.UDPEnabled,
		"udpIdleTimeout":         cfg.UDPIdleTimeout,
		"maxUDPAssociations":     cfg.MaxUDPAssociations,
		"blockedPorts":           cfg.BlockedPorts,
		"logLevel":               cfg.LogLevel,
		"ipv6":                   cfg.IPv6,
		"domainResolution":       cfg.DomainResolution,
		"peerCount":              cfg.PeerCount,
		"peerAutoReplace":        cfg.PeerAutoReplace,
		"peerDeadTimeout":        cfg.PeerDeadTimeout,
		"peerCheckInterval":      cfg.PeerCheckInterval,
		"peerRefreshInterval":    cfg.PeerRefreshInterval,
		"peerImprovementPercent": cfg.PeerImprovementPercent,
		"quotas":                 cfg.Quotas,
		"usage":                  cfg.Usage,
		"web":                    cfg.Web,
	}
	return writeJSONAtomic(cfg.statePath(settingsFileName), document)
}

// pendingSettings reports whether the file configuration is newer than the
// running service, which means the changes in /etc/x-node/config.json are not
// in effect yet. The overlay in the state directory is hot-reloaded by the
// daemon, so it never needs a restart; the file configuration does, because it
// carries the listener addresses and ports.
//
// The moment the service started comes from runtime.json: it is the only source
// that works both inside the daemon and in a CLI command. A file written in the
// first two seconds of the process life is treated as the one it started with.
func pendingSettings(cfg Config, configPath string) bool {
	info, err := os.Stat(configPath)
	if err != nil {
		return false
	}
	started := processStartedAt
	if doc, err := readRuntime(cfg.statePath(runtimeFileName)); err == nil && doc != nil && !doc.StartedAt.IsZero() {
		started = doc.StartedAt
	}
	return info.ModTime().After(started.Add(2 * time.Second))
}
