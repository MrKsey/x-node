package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
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
	Password  string `json:"password,omitempty"`
	Error     string `json:"error,omitempty"`
}

// dnsWebFields fills the link and the password only when the installed Alfis
// build has the web UI. An upstream nogui binary runs the resolver fine, but
// its panel link would lead nowhere.
func dnsWebFields(status *webDNSStatus, cfg Config) {
	status.WebURL = ""
	status.Password = ""
	if status.WebUI {
		status.WebURL = cfg.dnsWebURL()
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
			fmt.Printf("Web UI:      %s\n", cfg.dnsWebURL())
		} else {
			fmt.Println("Web UI:      not available in this Alfis build")
		}
		fmt.Printf("Service:     %s\n", status.State)
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
