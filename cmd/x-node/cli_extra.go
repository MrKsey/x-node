package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
)

// cmdUsage prints the traffic accounting.
func cmdUsage(args []string) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	configFlag := addConfigFlag(fs)
	since := fs.String("since", "", "only show clients seen within this window, e.g. 24h")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(configPath(*configFlag))
	if err != nil {
		return err
	}
	usage, err := openUsage(cfg, newLogger("warn"))
	if err != nil {
		return err
	}
	store, err := openStore(cfg, newLogger("warn"))
	if err != nil {
		return err
	}
	labels := make(map[string]string)
	for _, client := range store.List() {
		labels[client.Address] = client.Label
	}

	var window time.Duration
	if strings.TrimSpace(*since) != "" {
		window, err = time.ParseDuration(*since)
		if err != nil || window <= 0 {
			return fmt.Errorf("bad -since %q", *since)
		}
	}
	now := time.Now()
	rows := usage.rows()
	filtered := rows[:0]
	for _, row := range rows {
		if window > 0 && !row.LastSeenAt.IsZero() && now.Sub(row.LastSeenAt) > window {
			continue
		}
		filtered = append(filtered, row)
	}
	rows = filtered

	if *asJSON {
		payload := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			payload = append(payload, map[string]any{
				"address":         row.Address,
				"label":           labels[row.Address],
				"rx":              row.Rx,
				"tx":              row.Tx,
				"dayRx":           row.DayRx,
				"dayTx":           row.DayTx,
				"monthRx":         row.MonthRx,
				"monthTx":         row.MonthTx,
				"tcpConnections":  row.TCPConnections,
				"udpAssociations": row.UDPAssociations,
				"refused":         row.Refused,
				"lastSeenAt":      row.LastSeenAt,
			})
		}
		raw, _ := json.MarshalIndent(map[string]any{
			"ok":       true,
			"timezone": cfg.Usage.Timezone,
			"clients":  payload,
		}, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	if len(rows) == 0 {
		fmt.Println("No traffic recorded yet.")
		return nil
	}
	fmt.Printf("%-64s %11s %11s %11s %6s %6s\n",
		"CLIENT (ADDRESS + LABEL)", "TODAY", "THIS MONTH", "TOTAL", "TCP", "UDP")
	for _, row := range rows {
		fmt.Printf("%-64s %11s %11s %11s %6d %6d\n",
			clientName(row.Address, labels[row.Address]),
			formatBytes(row.DayRx+row.DayTx),
			formatBytes(row.MonthRx+row.MonthTx),
			formatBytes(row.Rx+row.Tx),
			row.TCPConnections, row.UDPAssociations)
	}
	fmt.Println()
	fmt.Printf("%d client(s), timezone %s. rx = upload, tx = download.\n", len(rows), cfg.Usage.Timezone)
	return nil
}

// cmdQuota manages the per-client limits.
func cmdQuota(args []string) error {
	if len(args) == 0 {
		helpForCommand("quota")
		return nil
	}
	action := args[0]
	fs := flag.NewFlagSet("quota "+action, flag.ContinueOnError)
	configFlag := addConfigFlag(fs)
	monthly := fs.String("monthly", "", "monthly traffic cap, 0 removes it")
	daily := fs.String("daily", "", "daily traffic cap, 0 removes it")
	down := fs.String("down", "", "download throttle, 0 removes it")
	up := fs.String("up", "", "upload throttle, 0 removes it")
	asJSON := fs.Bool("json", false, "print JSON (show)")
	all := fs.Bool("all", false, "reset every client")

	positional, flagArgs, err := splitPositional(args[1:], map[string]bool{
		"config": true, "monthly": true, "daily": true, "down": true, "up": true,
	})
	if err != nil {
		return err
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	cfg, err := loadConfig(configPath(*configFlag))
	if err != nil {
		return err
	}
	quotas, err := openQuotas(cfg, newLogger("warn"))
	if err != nil {
		return err
	}
	usage, err := openUsage(cfg, newLogger("warn"))
	if err != nil {
		return err
	}
	store, err := openStore(cfg, newLogger("warn"))
	if err != nil {
		return err
	}
	labels := make(map[string]string)
	for _, client := range store.List() {
		labels[client.Address] = client.Label
	}

	switch action {
	case "show":
		states := quotaStates(quotas, usage, store.List())
		if len(positional) == 1 {
			ip := net.ParseIP(strings.TrimSpace(positional[0]))
			if ip == nil {
				return fmt.Errorf("%q is not an address", positional[0])
			}
			filtered := states[:0]
			for _, state := range states {
				if state.Address == ip.String() {
					filtered = append(filtered, state)
				}
			}
			states = filtered
		}
		if *asJSON {
			raw, _ := json.MarshalIndent(map[string]any{
				"ok":       true,
				"defaults": quotas.Defaults(),
				"clients":  states,
			}, "", "  ")
			fmt.Println(string(raw))
			return nil
		}
		if len(states) == 0 {
			fmt.Println("No clients.")
			return nil
		}
		fmt.Printf("%-64s %11s %11s %11s %11s %-10s %-10s\n",
			"CLIENT (ADDRESS + LABEL)", "TODAY", "MONTH", "DAILY CAP", "MONTHLY CAP", "DOWN", "UP")
		for _, state := range states {
			daily := "unlimited"
			if state.DailyBytes > 0 {
				daily = formatBytes(uint64(state.DailyBytes))
			}
			monthly := "unlimited"
			if state.MonthlyBytes > 0 {
				monthly = formatBytes(uint64(state.MonthlyBytes))
			}
			fmt.Printf("%-64s %11s %11s %11s %11s %-10s %-10s\n",
				clientName(state.Address, labels[state.Address]),
				formatBytes(state.DayUsed), formatBytes(state.MonthUsed),
				daily, monthly, formatRate(state.DownKbps), formatRate(state.UpKbps))
		}
		return nil

	case "set":
		if len(positional) != 1 {
			return errors.New("usage: x-node quota set <address> [-monthly 100GB] [-daily 5GB] [-down 10Mbps] [-up 5Mbps]")
		}
		ip := net.ParseIP(strings.TrimSpace(positional[0]))
		if ip == nil || !isYggAddress(ip) {
			return fmt.Errorf("%q is not a Yggdrasil node address", positional[0])
		}
		if !store.Allowed(ip) && !cfg.AllowAll {
			return fmt.Errorf("%s is not in the allowlist (add it first)", ip)
		}
		quota := quotas.Get(ip)
		changed := false
		if *monthly != "" {
			value, err := parseByteSize(*monthly)
			if err != nil {
				return err
			}
			quota.MonthlyBytes, changed = value, true
		}
		if *daily != "" {
			value, err := parseByteSize(*daily)
			if err != nil {
				return err
			}
			quota.DailyBytes, changed = value, true
		}
		if *down != "" {
			value, err := parseBitRate(*down)
			if err != nil {
				return err
			}
			quota.DownKbps, changed = value, true
		}
		if *up != "" {
			value, err := parseBitRate(*up)
			if err != nil {
				return err
			}
			quota.UpKbps, changed = value, true
		}
		if !changed {
			return errors.New("nothing to change: pass -monthly, -daily, -down or -up")
		}
		if err := quotas.Set(quota); err != nil {
			return err
		}
		fmt.Printf("quota updated for %s: monthly %s, daily %s, down %s, up %s\n",
			quota.Address, formatLimit(quota.MonthlyBytes), formatLimit(quota.DailyBytes),
			formatRate(quota.DownKbps), formatRate(quota.UpKbps))
		return nil

	case "reset":
		if *all {
			removed := quotas.Reset("", true)
			usage.reset("", true)
			fmt.Printf("reset %d quota(s) and every traffic counter\n", removed)
			return nil
		}
		if len(positional) != 1 {
			return errors.New("usage: x-node quota reset <address>|--all")
		}
		ip := net.ParseIP(strings.TrimSpace(positional[0]))
		if ip == nil {
			return fmt.Errorf("%q is not an address", positional[0])
		}
		removed := quotas.Reset(ip.String(), false)
		usage.reset(ip.String(), false)
		if removed == 0 {
			fmt.Printf("%s had no explicit quota; its traffic counters are reset\n", ip)
		} else {
			fmt.Printf("quota removed for %s; the defaults apply again\n", ip)
		}
		return nil

	default:
		return fmt.Errorf("unknown quota action %q", action)
	}
}

// cmdInviteList shows the invites that are still usable. The token is stored
// with the invite, so the link can be shown again: recreating an invite for a
// client that lost the QR code is not necessary.
func cmdInviteList(args []string) error {
	fs := flag.NewFlagSet("invite list", flag.ContinueOnError)
	configFlag := addConfigFlag(fs)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(configPath(*configFlag))
	if err != nil {
		return err
	}
	store, err := openStore(cfg, newLogger("warn"))
	if err != nil {
		return err
	}
	invites := store.ListInvites()
	if *asJSON {
		type row struct {
			Hash      string    `json:"hash"`
			Note      string    `json:"note"`
			CreatedAt time.Time `json:"createdAt"`
			ExpiresAt time.Time `json:"expiresAt"`
			Uses      int       `json:"uses"`
			Used      int       `json:"used"`
			Token     string    `json:"token,omitempty"`
			Link      string    `json:"link,omitempty"`
		}
		rows := make([]row, 0, len(invites))
		for _, invite := range invites {
			item := row{
				Hash: invite.Hash, Note: invite.Note, CreatedAt: invite.CreatedAt,
				ExpiresAt: invite.ExpiresAt, Uses: invite.Uses, Used: invite.Used,
				Token: invite.Token,
			}
			if invite.Token != "" {
				item.Link = exitLink(cfg, invite.Token)
			}
			rows = append(rows, item)
		}
		raw, _ := json.MarshalIndent(map[string]any{"ok": true, "invites": rows}, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	if len(invites) == 0 {
		fmt.Println("No active invites. Create one: x-node invite")
		return nil
	}
	fmt.Printf("%-14s %-24s %-8s %-20s %s\n", "ID", "NOTE", "USES", "EXPIRES", "LINK")
	for _, invite := range invites {
		link := "-"
		if invite.Token != "" {
			link = exitLink(cfg, invite.Token)
		}
		fmt.Printf("%-14s %-24s %-8s %-20s %s\n",
			invite.Hash[:12]+"…", truncate(invite.Note, 24),
			fmt.Sprintf("%d / %d", invite.Used, invite.Uses),
			invite.ExpiresAt.Local().Format("2006-01-02 15:04"), link)
	}
	fmt.Println()
	fmt.Println("Use the ID prefix with \"x-node invite remove <id>\".")
	return nil
}

// cmdInviteRemove deletes an invite by the prefix shown by "invite list".
func cmdInviteRemove(args []string) error {
	fs := flag.NewFlagSet("invite remove", flag.ContinueOnError)
	configFlag := addConfigFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: x-node invite remove <id>")
	}
	prefix := strings.TrimSpace(fs.Arg(0))
	if len(prefix) < 4 {
		return errors.New("the invite ID prefix must be at least 4 characters")
	}
	cfg, err := loadConfig(configPath(*configFlag))
	if err != nil {
		return err
	}
	store, err := openStore(cfg, newLogger("warn"))
	if err != nil {
		return err
	}
	var matches []Invite
	for _, invite := range store.ListInvites() {
		if strings.HasPrefix(invite.Hash, prefix) {
			matches = append(matches, invite)
		}
	}
	switch len(matches) {
	case 0:
		return fmt.Errorf("no active invite starts with %q", prefix)
	case 1:
	default:
		return fmt.Errorf("%q matches %d invites; use a longer prefix", prefix, len(matches))
	}
	removed, err := store.DeleteInvite(matches[0].Hash)
	if err != nil {
		return err
	}
	if !removed {
		return errors.New("the invite disappeared; run \"x-node invite list\" again")
	}
	fmt.Println("removed invite", matches[0].Hash[:12]+"…")
	return nil
}

func formatLimit(bytes int64) string {
	if bytes <= 0 {
		return "unlimited"
	}
	return formatBytes(uint64(bytes))
}

// cmdWeb prints the URL and manages the password.
func cmdWeb(args []string) error {
	if len(args) == 0 {
		helpForCommand("web")
		return nil
	}
	action := args[0]
	fs := flag.NewFlagSet("web "+action, flag.ContinueOnError)
	configFlag := addConfigFlag(fs)
	user := fs.String("user", "admin", "login name")
	password := fs.String("password", "", "explicit password instead of a generated one")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := loadConfig(configPath(*configFlag))
	if err != nil {
		return err
	}
	credPath := cfg.statePath(webCredentialsFileName)

	switch action {
	case "url":
		if !cfg.Web.Enabled {
			return errors.New("the web interface is disabled in the configuration")
		}
		for _, url := range webURLs(cfg) {
			fmt.Println(url)
		}
		fmt.Println()
		fmt.Println("Login:", currentWebUser(credPath))
		return nil

	case "password":
		value := strings.TrimSpace(*password)
		generated := false
		if value == "" {
			value, err = generatePassword()
			if err != nil {
				return err
			}
			generated = true
		}
		creds, err := loadWebCredentials(credPath)
		if err != nil {
			creds = webCredentials{}
		}
		creds.Username = strings.TrimSpace(*user)
		if creds.Username == "" {
			creds.Username = "admin"
		}
		hash, err := hashWebPassword(value)
		if err != nil {
			return err
		}
		creds.PasswordHash = hash
		if creds.SessionKey == "" {
			fresh, err := newWebCredentials(creds.Username, value)
			if err != nil {
				return err
			}
			creds.SessionKey = fresh.SessionKey
		}
		if err := saveWebCredentials(credPath, creds); err != nil {
			return err
		}
		if generated {
			fmt.Println("New password:", value)
			fmt.Println("This is the only time it is shown.")
		} else {
			fmt.Println("Password updated.")
		}
		fmt.Println("Login:", creds.Username)
		fmt.Println("The running service picks up the new password within a second.")
		fmt.Println("Existing sessions are still valid until they expire.")
		return nil

	case "reset-path":
		path, err := randomWebPath()
		if err != nil {
			return err
		}
		cfg.Web.Path = path
		if err := saveSettingsOverlay(cfg); err != nil {
			return err
		}
		fmt.Println("New web path:", path)
		fmt.Println("The running service picks it up within a few seconds.")
		return nil

	default:
		return fmt.Errorf("unknown web action %q", action)
	}
}

func currentWebUser(path string) string {
	creds, err := loadWebCredentials(path)
	if err != nil || creds.Username == "" {
		return "admin"
	}
	return creds.Username
}

// cmdVersion prints the version and, with -json, the build environment.
func cmdVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *asJSON {
		raw, _ := json.MarshalIndent(map[string]any{
			"version":   daemonVersion,
			"go":        runtime.Version(),
			"os":        runtime.GOOS,
			"arch":      runtime.GOARCH,
			"pid":       os.Getpid(),
			"startedAt": processStartedAt.Format(time.RFC3339),
		}, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	fmt.Println(daemonVersion)
	return nil
}

// splitPositional separates positional arguments from flags so a user can write
// "allow add 200::1 -label laptop" or "allow add -label laptop 200::1".
func splitPositional(args []string, withValue map[string]bool) (positional, flagArgs []string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flagArgs = append(flagArgs, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if withValue[name] {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("flag %s needs a value", arg)
			}
			flagArgs = append(flagArgs, args[i+1])
			i++
		}
	}
	return positional, flagArgs, nil
}

// sortedKeys is a small helper for stable JSON output in tests.
func sortedKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
