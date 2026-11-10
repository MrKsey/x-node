package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The DNS server is Alfis running as a separate unit. This file is the small
// bridge between the x-node configuration, the root helper and the CLI.

type webDNSStatus struct {
	Enabled   bool   `json:"enabled"`
	Running   bool   `json:"running"`
	State     string `json:"state"`
	Installed bool   `json:"installed"`
	Config    bool   `json:"config"`
	Listen    string `json:"listen"`
	WebUI     bool   `json:"webUI"`
	WebURL    string `json:"webURL"`
	// ProxyPath is the panel address on the X-Node web server, e.g.
	// /<random>/dns/. It is the link the dashboard opens.
	ProxyPath string `json:"proxyPath"`
	// WebReady is true when the Alfis panel answered through its API. The
	// dashboard enables the "Open Alfis web UI" button only then: right after
	// a restart the process still loads the chain and refuses connections.
	WebReady bool `json:"webReady"`
	// Syncing reports the first start: Alfis downloads the blockchain, which
	// takes a while and keeps a CPU core busy.
	Syncing     bool   `json:"syncing"`
	SyncHave    uint64 `json:"syncHave"`
	SyncHeight  uint64 `json:"syncHeight"`
	SyncPercent int    `json:"syncPercent"`
	Nodes       int    `json:"nodes"`
	Password    string `json:"password,omitempty"`
	Error       string `json:"error,omitempty"`
}

// alfisStatus mirrors the part of /api/status the dashboard shows. The panel
// publishes the sync counters there, so no log parsing is needed.
type alfisStatus struct {
	SyncedBlocks uint64 `json:"synced_blocks"`
	SyncHeight   uint64 `json:"sync_height"`
	Syncing      bool   `json:"syncing"`
	Nodes        int    `json:"nodes"`
}

// alfisSession keeps the cookie of the panel login. Logging in on every
// dashboard poll would create a session per refresh; the cookie lives in memory
// and is dropped when the target or the password changes.
var alfisSession struct {
	mu       sync.Mutex
	target   string
	password string
	cookie   string
}

// alfisStatusCache keeps the last answer of the panel for a few seconds. The
// dashboard polls the status every five seconds, and while Alfis is starting
// every request would wait for the whole client timeout.
var alfisStatusCache struct {
	mu        sync.Mutex
	target    string
	password  string
	at        time.Time
	status    alfisStatus
	ok        bool
	reachable bool
}

const alfisStatusTTL = 10 * time.Second

// alfisWebStatus asks the Alfis panel for its state. The answer also proves
// that the panel is reachable through the proxy: the same address is dialled.
// reachable is true when the panel answered at all, even when the stored
// password is stale and the status call stays unauthorized.
func alfisWebStatus(cfg Config) (alfisStatus, bool, bool) {
	if !cfg.DNS.Enabled || strings.TrimSpace(cfg.DNS.WebPassword) == "" {
		return alfisStatus{}, false, false
	}
	target := cfg.dnsWebTarget()
	if target == "" {
		return alfisStatus{}, false, false
	}
	alfisStatusCache.mu.Lock()
	if alfisStatusCache.target == target &&
		alfisStatusCache.password == cfg.DNS.WebPassword &&
		time.Since(alfisStatusCache.at) < alfisStatusTTL {
		live, ok, reachable := alfisStatusCache.status, alfisStatusCache.ok, alfisStatusCache.reachable
		alfisStatusCache.mu.Unlock()
		return live, ok, reachable
	}
	alfisStatusCache.mu.Unlock()
	live, ok, reachable := fetchAlfisStatus(cfg, target)
	alfisStatusCache.mu.Lock()
	alfisStatusCache.target = target
	alfisStatusCache.password = cfg.DNS.WebPassword
	alfisStatusCache.at = time.Now()
	alfisStatusCache.status = live
	alfisStatusCache.ok = ok
	alfisStatusCache.reachable = reachable
	alfisStatusCache.mu.Unlock()
	return live, ok, reachable
}

func fetchAlfisStatus(cfg Config, target string) (alfisStatus, bool, bool) {
	base := "http://" + target
	client := &http.Client{Timeout: 2 * time.Second}

	alfisSession.mu.Lock()
	if alfisSession.target != target || alfisSession.password != cfg.DNS.WebPassword {
		alfisSession.target = target
		alfisSession.password = cfg.DNS.WebPassword
		alfisSession.cookie = ""
	}
	cookie := alfisSession.cookie
	alfisSession.mu.Unlock()

	fetch := func() (alfisStatus, int, error) {
		request, err := http.NewRequest(http.MethodGet, base+"/api/status", nil)
		if err != nil {
			return alfisStatus{}, 0, err
		}
		if cookie != "" {
			request.AddCookie(&http.Cookie{Name: "alfis_session", Value: cookie})
		}
		response, err := client.Do(request)
		if err != nil {
			return alfisStatus{}, 0, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return alfisStatus{}, response.StatusCode, nil
		}
		var live alfisStatus
		if err := json.NewDecoder(response.Body).Decode(&live); err != nil {
			return alfisStatus{}, response.StatusCode, err
		}
		return live, response.StatusCode, nil
	}

	live, code, err := fetch()
	if err != nil {
		return alfisStatus{}, false, false
	}
	if code == http.StatusUnauthorized {
		fresh, ok := alfisLogin(client, base, cfg.DNS.WebPassword)
		if !ok {
			// The panel answered, only the login did not: the page itself is
			// up and the operator can open it.
			return alfisStatus{}, false, true
		}
		cookie = fresh
		alfisSession.mu.Lock()
		alfisSession.cookie = fresh
		alfisSession.mu.Unlock()
		live, code, err = fetch()
		if err != nil {
			return alfisStatus{}, false, true
		}
	}
	if code != http.StatusOK {
		return alfisStatus{}, false, true
	}
	return live, true, true
}

// alfisLogin exchanges the panel password for a session cookie.
func alfisLogin(client *http.Client, base, password string) (string, bool) {
	body := strings.NewReader(`{"password":` + strconv.Quote(password) + `}`)
	response, err := client.Post(base+"/api/login", "application/json", body)
	if err != nil {
		return "", false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", false
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == "alfis_session" {
			return cookie.Value, true
		}
	}
	return "", false
}

// fillAlfisLive adds the panel state to the status: the sync counters and the
// readiness flag the dashboard uses to enable the panel link.
func fillAlfisLive(status *webDNSStatus, cfg Config) {
	if !status.Running || !status.WebUI {
		return
	}
	live, ok, reachable := alfisWebStatus(cfg)
	if !reachable {
		return
	}
	status.WebReady = true
	if !ok {
		return
	}
	status.Syncing = live.Syncing
	status.SyncHave = live.SyncedBlocks
	status.SyncHeight = live.SyncHeight
	status.Nodes = live.Nodes
	if live.SyncHeight > 0 {
		percent := live.SyncedBlocks * 100 / live.SyncHeight
		if percent > 100 {
			percent = 100
		}
		status.SyncPercent = int(percent)
	}
}

// dnsWebFields fills the link and the password only when the installed Alfis
// build has the web UI. An upstream nogui binary runs the resolver fine, but
// its panel link would lead nowhere.
func dnsWebFields(status *webDNSStatus, cfg Config) {
	status.WebURL = ""
	status.ProxyPath = ""
	status.Password = ""
	if status.WebUI {
		status.WebURL = cfg.dnsPanelURL()
		status.ProxyPath = cfg.dnsWebPath() + "/"
		status.Password = cfg.DNS.WebPassword
	}
}

func (w *webServer) dnsStatus(cfg Config) webDNSStatus {
	status := webDNSStatus{
		Enabled: cfg.DNS.Enabled,
		State:   "unknown",
		Listen:  cfg.dnsListen(),
		WebUI:   alfisWebUISupported(dnsBinaryPath()),
	}
	dnsWebFields(&status, cfg)
	if !requestHelperInstalled() {
		status.Error = "the root helper is not running"
		return status
	}
	out, err := runHelper(helperRequest{Action: "dns-status"})
	if err != nil {
		status.Error = err.Error()
		return status
	}
	var data struct {
		Installed bool   `json:"installed"`
		Config    bool   `json:"config"`
		Active    bool   `json:"active"`
		State     string `json:"state"`
		WebUI     bool   `json:"webUI"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		status.Error = err.Error()
		return status
	}
	status.Installed = data.Installed
	status.Config = data.Config
	status.Running = data.Active
	status.WebUI = data.WebUI
	dnsWebFields(&status, cfg)
	if data.State != "" {
		status.State = data.State
	}
	fillAlfisLive(&status, cfg)
	return status
}

// applyDNSConfig asks the root helper to write the Alfis files and start or
// stop the unit. The CLI can call the same code directly when it runs as root.
func applyDNSConfig(cfg Config) error {
	if requestHelperInstalled() {
		enabled := cfg.DNS.Enabled
		_, err := runHelper(helperRequest{
			Action:  "dns-sync",
			Enabled: &enabled,
			Content: cfg.alfisConfig(),
		})
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("the root helper is not running; run this command as root")
	}
	_, err := helperDNSSync(cfg.DNS.Enabled, cfg.alfisConfig())
	return err
}

func cmdDNS(args []string) error {
	fs := flag.NewFlagSet("dns", flag.ContinueOnError)
	configFlag := addConfigFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(*configFlag)
	cfg, err := loadEffectiveConfig(path)
	if err != nil {
		return err
	}
	action := "status"
	if fs.NArg() > 0 {
		action = strings.ToLower(strings.TrimSpace(fs.Arg(0)))
	}

	switch action {
	case "status":
		status := dnsStatusForCLI(cfg)
		fmt.Printf("DNS server:  %s\n", onOff(cfg.DNS.Enabled))
		fmt.Printf("Listen:      %s\n", cfg.dnsListen())
		if status.WebUI {
			fmt.Printf("Web UI:      %s\n", cfg.dnsPanelURL())
		} else {
			fmt.Println("Web UI:      not available in this Alfis build")
		}
		fmt.Printf("Service:     %s\n", status.State)
		if status.Syncing {
			fmt.Printf("Sync:        %d/%d blocks (%d%%)\n",
				status.SyncHave, status.SyncHeight, status.SyncPercent)
		}
		if status.Error != "" {
			fmt.Printf("Helper:      %s\n", status.Error)
		}
		if status.WebUI && cfg.DNS.Enabled && strings.TrimSpace(cfg.DNS.WebPassword) != "" {
			fmt.Println("The Alfis web password is stored in dns.webPassword; the dashboard shows it.")
		}
		return nil
	case "on", "off":
		if os.Geteuid() != 0 {
			return errors.New("run this command as root")
		}
		cfg.DNS.Enabled = action == "on"
		if cfg.DNS.Enabled && strings.TrimSpace(cfg.DNS.WebPassword) == "" &&
			alfisWebUISupported(dnsBinaryPath()) {
			password, err := generatePassword()
			if err != nil {
				return err
			}
			cfg.DNS.WebPassword = password
			fmt.Printf("Alfis web password: %s\n", password)
		}
		cfg.applyDefaults()
		if err := cfg.validate(); err != nil {
			return err
		}
		if err := saveSettingsOverlay(cfg); err != nil {
			return err
		}
		if err := applyDNSConfig(cfg); err != nil {
			return err
		}
		fmt.Printf("DNS server is %s\n", onOff(cfg.DNS.Enabled))
		return nil
	default:
		return errors.New("usage: x-node dns on|off|status [-config PATH]")
	}
}

func dnsStatusForCLI(cfg Config) webDNSStatus {
	status := webDNSStatus{
		Enabled: cfg.DNS.Enabled,
		State:   "unknown",
		Listen:  cfg.dnsListen(),
		WebUI:   alfisWebUISupported(dnsBinaryPath()),
	}
	dnsWebFields(&status, cfg)
	if requestHelperInstalled() {
		out, err := runHelper(helperRequest{Action: "dns-status"})
		if err != nil {
			status.Error = err.Error()
			return status
		}
		var data struct {
			Installed bool   `json:"installed"`
			Config    bool   `json:"config"`
			Active    bool   `json:"active"`
			State     string `json:"state"`
			WebUI     bool   `json:"webUI"`
		}
		if err := json.Unmarshal([]byte(out), &data); err != nil {
			status.Error = err.Error()
			return status
		}
		status.Installed = data.Installed
		status.Config = data.Config
		status.Running = data.Active
		status.WebUI = data.WebUI
		dnsWebFields(&status, cfg)
		if data.State != "" {
			status.State = data.State
		}
		fillAlfisLive(&status, cfg)
		return status
	}
	if os.Geteuid() == 0 {
		if raw, err := helperDNSStatus(); err == nil {
			if document, ok := raw.(map[string]any); ok {
				if value, ok := document["installed"].(bool); ok {
					status.Installed = value
				}
				if value, ok := document["config"].(bool); ok {
					status.Config = value
				}
				if value, ok := document["active"].(bool); ok {
					status.Running = value
				}
				if value, ok := document["webUI"].(bool); ok {
					status.WebUI = value
				}
				dnsWebFields(&status, cfg)
				if value, ok := document["state"].(string); ok && value != "" {
					status.State = value
				}
				fillAlfisLive(&status, cfg)
				return status
			}
		}
	}
	status.Error = "the root helper is not running"
	return status
}

// dnsConfigChanged reports whether the Alfis files have to be rewritten. The
// forwarder list is compared as a string: its order is meaningful and the list
// is tiny.
func dnsConfigChanged(before, after Config) bool {
	return before.DNS.Enabled != after.DNS.Enabled ||
		before.dnsListen() != after.dnsListen() ||
		before.dnsWebListen() != after.dnsWebListen() ||
		before.DNS.WebPassword != after.DNS.WebPassword ||
		before.DNS.CacheMemoryLimitMB != after.DNS.CacheMemoryLimitMB ||
		strings.Join(before.DNS.Forwarders, ",") != strings.Join(after.DNS.Forwarders, ",")
}
