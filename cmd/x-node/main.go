package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processStartedAt lets the web interface tell a settings file written before
// the process started from one it has already applied.
var processStartedAt = time.Now()

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	command := os.Args[1]
	if command == "help" || command == "-h" || command == "--help" {
		if len(os.Args) >= 3 {
			if !helpForCommand(os.Args[2]) {
				fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[2])
				usage()
				os.Exit(2)
			}
			return
		}
		printOverview(os.Stdout)
		return
	}
	args := os.Args[2:]
	if wantsHelp(args) {
		// Print the table from help.go, not the one the flag package builds:
		// the table explains the flags, defaults and examples.
		if !helpForCommand(command) {
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n", command)
			usage()
			os.Exit(2)
		}
		return
	}
	err := run(command, args)
	if errors.Is(err, flag.ErrHelp) {
		// The flag package stops at -h; the table in help.go is the real help.
		if !helpForCommand(command) {
			usage()
		}
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(command string, args []string) error {
	switch command {
	case "serve":
		return cmdServe(args)
	case "keygen":
		return cmdKeygen(args)
	case "invite":
		return cmdInvite(args)
	case "allow":
		return cmdAllow(args)
	case "clients":
		return cmdClients(args)
	case "usage":
		return cmdUsage(args)
	case "quota":
		return cmdQuota(args)
	case "peers":
		return cmdPeers(args)
	case "status":
		return cmdStatus(args)
	case "doctor":
		return cmdDoctor(args)
	case "update":
		return cmdUpdate(args)
	case "web":
		return cmdWeb(args)
	case "dns":
		return cmdDNS(args)
	case "version":
		return cmdVersion(args)
	case "helper":
		// Internal entry point for the web interface. The installer runs
		// "x-node helper serve" as a separate root service; it is not listed
		// in the command overview.
		return cmdHelper(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", command)
		usage()
		os.Exit(2)
		return nil
	}
}

func addConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "", "path to config.json (default /etc/x-node/config.json)")
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configFlag := addConfigFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(*configFlag)
	base, err := loadConfig(path)
	if err != nil {
		return err
	}
	cfg, err := loadSettingsOverlay(base)
	if err != nil {
		return fmt.Errorf("settings overlay: %w", err)
	}
	live := newSettings(cfg)
	log := newLogger(cfg.LogLevel)
	log.info("x-node %s starting (config %s)", daemonVersion, path)

	store, err := openStore(cfg, log)
	if err != nil {
		return err
	}
	usage, err := openUsage(cfg, log)
	if err != nil {
		return err
	}
	quotas, err := openQuotas(cfg, log)
	if err != nil {
		return err
	}
	family := newIPv6Monitor()
	_, state, source, detail, iface := family.status()
	log.info("IPv6 outbound: mode %s, %s", cfg.ipv6Mode(), ipv6StateText(state, source, detail, iface))
	log.info("allowlist: %d client(s), %d active invite(s)", len(store.List()), len(store.ListInvites()))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tracker := newRuntimeTracker(cfg, log)
	api := newAPIServer(cfg, store, tracker, log)
	api.live = live
	api.quotas = quotas
	api.usage = usage
	api.family = family
	socks := newSocksServer(cfg, store, tracker, log, api.handler())
	socks.live = live
	socks.usage = usage
	socks.quotas = quotas
	socks.udp.live = live
	socks.dialer = newOutboundDialer(cfg, func() string { return live.get().ipv6Mode() }, family)
	supervisor := newPeerSupervisor(cfg, path, log)

	var web *webServer
	if cfg.Web.Enabled {
		web, err = newWebServer(cfg, path, live, store, tracker, usage, quotas, family, log)
		if err != nil {
			return err
		}
		web.supervisor = supervisor
		web.conns = socks.conns
	}

	errs := make(chan error, 3)
	go func() { errs <- api.listenAndServe(ctx) }()
	go func() { errs <- socks.listenAndServe(ctx) }()
	if web != nil {
		go func() { errs <- web.listenAndServe(ctx) }()
		go web.history.run(ctx)
		go web.system.run(ctx)
	}
	go tracker.run(ctx)
	go supervisor.run(ctx)
	go usage.run(ctx)
	go family.run(ctx)
	go socks.watchQuotas(ctx)
	go watchSettings(ctx, base, live, quotas, log)
	// An upgrade can change a derived default of the Alfis configuration, and
	// the unit keeps running with the file written by the previous version.
	// Align it once in the background instead of waiting for the operator to
	// toggle the DNS switch.
	go syncInstalledDNSConfig(cfg, log)
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				store.Cleanup()
			}
		}
	}()

	select {
	case <-ctx.Done():
		log.info("shutting down")
		tracker.shutdown()
		usage.flush(true)
		return nil
	case err := <-errs:
		stop()
		tracker.shutdown()
		usage.flush(true)
		if err == nil {
			return nil
		}
		return err
	}
}

// watchSettings picks up settings.json changes made by the CLI or by a second
// browser tab. The file is small; a two-second poll is cheaper and more robust
// than a filesystem watcher.
func watchSettings(ctx context.Context, base Config, live *settings, quotas *quotaStore, log *logger) {
	path := base.statePath(settingsFileName)
	last := time.Time{}
	if info, err := os.Stat(path); err == nil {
		last = info.ModTime()
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			info, err := os.Stat(path)
			if err != nil || info.ModTime().Equal(last) {
				continue
			}
			last = info.ModTime()
			merged, err := loadSettingsOverlay(base)
			if err != nil {
				log.warn("settings.json is not applied: %v", err)
				continue
			}
			live.set(merged)
			quotas.UpdateDefaults(merged.Quotas)
			log.setLevel(merged.LogLevel)
			log.info("settings.json reloaded")
		}
	}
}

func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	addr, err := addrForKey(pub)
	if err != nil {
		return err
	}
	out := map[string]string{
		"privateKey": hex.EncodeToString(priv),
		"publicKey":  hex.EncodeToString(pub),
		"address":    addr.String(),
	}
	if *asJSON {
		raw, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	fmt.Println("privateKey:", out["privateKey"])
	fmt.Println("publicKey: ", out["publicKey"])
	fmt.Println("address:   ", out["address"])
	return nil
}

func cmdInvite(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "list":
			return cmdInviteList(args[1:])
		case "remove":
			return cmdInviteRemove(args[1:])
		}
	}
	fs := flag.NewFlagSet("invite", flag.ContinueOnError)
	configFlag := addConfigFlag(fs)
	uses := fs.Int("uses", 0, "how many clients may use this invite (default from config)")
	ttl := fs.String("ttl", "", "invite lifetime, e.g. 24h (default from config)")
	note := fs.String("note", "", "note stored with the invite")
	asJSON := fs.Bool("json", false, "print JSON instead of the QR")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(configPath(*configFlag))
	if err != nil {
		return err
	}
	log := newLogger("warn")
	store, err := openStore(cfg, log)
	if err != nil {
		return err
	}
	duration, err := cfg.inviteTTL()
	if err != nil {
		return err
	}
	if strings.TrimSpace(*ttl) != "" {
		duration, err = time.ParseDuration(*ttl)
		if err != nil || duration <= 0 {
			return fmt.Errorf("bad -ttl %q", *ttl)
		}
	}
	count := *uses
	if count <= 0 {
		count = cfg.InviteUses
	}
	token, invite, err := store.NewInvite(*note, duration, count)
	if err != nil {
		return err
	}
	if *asJSON {
		raw, _ := json.MarshalIndent(map[string]any{
			"link":      exitLink(cfg, token),
			"invite":    token,
			"expiresAt": invite.ExpiresAt.Format(time.RFC3339),
			"uses":      invite.Uses,
			"note":      invite.Note,
		}, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	printInvite(cfg, token, invite)
	return nil
}

func cmdAllow(args []string) error {
	if len(args) == 0 {
		helpForCommand("allow")
		return nil
	}
	action := args[0]
	fs := flag.NewFlagSet("allow "+action, flag.ContinueOnError)
	configFlag := addConfigFlag(fs)
	label := fs.String("label", "", "label for the client")
	asJSON := fs.Bool("json", false, "print JSON (link)")

	// The address is the only positional argument, but users write it before
	// or after the flags. The standard flag package stops at the first
	// positional, so split the two kinds of arguments by hand.
	var positional, flagArgs []string
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flagArgs = append(flagArgs, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if name == "label" || name == "config" {
			if i+1 >= len(rest) {
				return fmt.Errorf("flag %s needs a value", arg)
			}
			flagArgs = append(flagArgs, rest[i+1])
			i++
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
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
	switch action {
	case "list":
		clients := store.List()
		if len(clients) == 0 {
			fmt.Println("The allowlist is empty.")
			return nil
		}
		fmt.Printf("%-64s %-19s %-9s %s\n", "CLIENT (ADDRESS + LABEL)", "REGISTERED", "SOURCE", "TOKEN")
		for _, client := range clients {
			token := "no"
			if client.TokenHash != "" {
				token = "yes"
			}
			fmt.Printf("%-64s %-19s %-9s %s\n",
				clientName(client.Address, client.Label),
				client.RegisteredAt.Format("2006-01-02 15:04"), client.Source, token)
		}
		return nil
	case "add":
		if len(positional) != 1 {
			return errors.New("usage: x-node allow add <address> [-label TEXT]")
		}
		ip := net.ParseIP(strings.TrimSpace(positional[0]))
		if !isYggAddress(ip) {
			return fmt.Errorf("%q is not a Yggdrasil node address", positional[0])
		}
		client, existed, err := store.AddClient(ip, *label)
		if err != nil {
			return err
		}
		if existed {
			fmt.Println("updated", client.Address)
		} else {
			fmt.Println("added", client.Address)
		}
		return nil
	case "remove":
		if len(positional) != 1 {
			return errors.New("usage: x-node allow remove <address>")
		}
		ip := net.ParseIP(strings.TrimSpace(positional[0]))
		if ip == nil {
			return fmt.Errorf("%q is not an address", positional[0])
		}
		removed, err := store.RemoveClient(ip)
		if err != nil {
			return err
		}
		if !removed {
			return fmt.Errorf("%s is not in the allowlist", ip)
		}
		fmt.Println("removed", ip)
		return nil
	case "token":
		if len(positional) != 1 {
			return errors.New("usage: x-node allow token <address>")
		}
		ip := net.ParseIP(strings.TrimSpace(positional[0]))
		if !isYggAddress(ip) {
			return fmt.Errorf("%q is not a Yggdrasil node address", positional[0])
		}
		token, err := store.IssueToken(ip)
		if err != nil {
			if errors.Is(err, errNotAllowed) {
				return fmt.Errorf("%s is not in the allowlist (add it first)", ip)
			}
			return err
		}
		fmt.Println(token)
		return nil
	case "link":
		if len(positional) != 1 {
			return errors.New("usage: x-node allow link <address>")
		}
		ip := net.ParseIP(strings.TrimSpace(positional[0]))
		if !isYggAddress(ip) {
			return fmt.Errorf("%q is not a Yggdrasil node address", positional[0])
		}
		client, ok := store.Get(ip)
		if !ok {
			return fmt.Errorf("%s is not in the allowlist (add it first)", ip)
		}
		link := exitLink(cfg, "")
		if *asJSON {
			raw, _ := json.MarshalIndent(map[string]any{
				"ok":      true,
				"address": client.Address,
				"label":   client.Label,
				"link":    link,
			}, "", "  ")
			fmt.Println(string(raw))
			return nil
		}
		printClientLink(cfg, client)
		return nil
	default:
		return fmt.Errorf("unknown allow action %q", action)
	}
}

// cmdClients prints the registered clients and the ones with open SOCKS5
// connections. The live part comes from runtime.json, which the service keeps
// in the state directory.
func cmdClients(args []string) error {
	fs := flag.NewFlagSet("clients", flag.ContinueOnError)
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
	doc, err := readRuntime(cfg.statePath(runtimeFileName))
	if err != nil {
		return fmt.Errorf("read %s: %w", cfg.statePath(runtimeFileName), err)
	}
	now := time.Now()
	running := runtimeFresh(doc, now)
	rows := mergeClients(store.List(), doc, now)

	if *asJSON {
		raw, _ := json.MarshalIndent(map[string]any{
			"ok":             true,
			"serviceRunning": running,
			"updatedAt":      docUpdatedAt(doc),
			"clients":        rows,
		}, "", "  ")
		fmt.Println(string(raw))
		return nil
	}

	if !running {
		fmt.Println("The service is not running; showing the allowlist only.")
	}
	if len(rows) == 0 {
		fmt.Println("No registered clients.")
		fmt.Println("Create an invite: x-node invite")
		return nil
	}
	fmt.Printf("%-64s %-11s %5s %4s %7s  %-19s %-19s %s\n",
		"CLIENT (ADDRESS + LABEL)", "STATE", "CONNS", "UDP", "REFUSED", "LAST SEEN", "REGISTERED", "SOURCE")
	connected := 0
	for _, row := range rows {
		if row.State == "connected" {
			connected++
		}
		fmt.Printf("%-64s %-11s %5d %4d %7d  %-19s %-19s %s\n",
			clientName(row.Address, row.Label), row.State, row.Connections,
			row.UDPAssociations, row.Refused, formatWhen(row.LastSeenAt),
			formatWhen(row.RegisteredAt), row.Source)
	}
	fmt.Println()
	fmt.Printf("%d registered, %d connected", len(rows), connected)
	if running && doc != nil {
		fmt.Printf(", %d open connection(s), %d UDP association(s)",
			doc.TotalConnections, doc.TotalUDPAssociations)
	}
	fmt.Println()
	return nil
}

func docUpdatedAt(doc *runtimeFile) string {
	if doc == nil || doc.UpdatedAt.IsZero() {
		return ""
	}
	return doc.UpdatedAt.Format(time.RFC3339)
}

func formatWhen(value time.Time) string {
	if value.IsZero() {
		return "never"
	}
	return value.Local().Format("2006-01-02 15:04")
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	configFlag := addConfigFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(*configFlag)
	base, err := loadConfig(path)
	if err != nil {
		return err
	}
	cfg, err := loadSettingsOverlay(base)
	if err != nil {
		return fmt.Errorf("settings overlay: %w", err)
	}
	store, err := openStore(cfg, newLogger("warn"))
	if err != nil {
		return err
	}
	line := func(label string, values ...any) {
		fmt.Printf("%-19s %s\n", label+":", fmt.Sprint(values...))
	}
	section := func(title string) {
		fmt.Println()
		fmt.Println(title)
	}

	fmt.Printf("x-node %s\n", daemonVersion)

	section("Node")
	line("Name", cfg.Name)
	line("Yggdrasil address", cfg.ListenAddress)
	line("SOCKS5 port", cfg.SocksPort)
	if cfg.apiShared() {
		line("API port", "shared with SOCKS5 (HTTP /v1/... on the same port)")
	} else {
		line("API port", cfg.APIPort)
	}
	if cfg.APIRateLimit > 0 {
		line("API auth", fmt.Sprintf("%s, %d request(s)/min per source", cfg.apiAuthMode(), cfg.APIRateLimit))
	} else {
		line("API auth", cfg.apiAuthMode()+", rate limit off")
	}

	section("Yggdrasil")
	if strings.TrimSpace(cfg.PeerURI) != "" {
		line("Peer link", cfg.PeerURI)
	} else {
		line("Peer link", "not configured (no incoming peering)")
	}
	managed, manual, up, inbound := peerSummary(cfg)
	line("Public peers", fmt.Sprintf("%d wanted, %d up, auto replace %s", cfg.PeerCount, up, onOff(cfg.PeerAutoReplace)))
	if managed > 0 && managed != cfg.PeerCount {
		line("Managed peers", managed)
	}
	if manual > 0 {
		line("Manual peers", fmt.Sprintf("%d pinned", manual))
	}
	if inbound > 0 {
		line("Inbound peers", inbound)
	}

	section("Access")
	if cfg.AllowAll {
		line("Open mode", "on (allowAll: every Yggdrasil node may use the exit)")
	} else {
		line("Open mode", "off (only the allowlist)")
	}
	line("Registration", onOff(cfg.Registration))
	line("Allowlist", fmt.Sprintf("%d client(s)", len(store.List())))
	line("Invites", fmt.Sprintf("%d active (x-node invite list)", len(store.ListInvites())))

	section("Outbound")
	mode := cfg.ipv6Mode()
	detection := detectIPv6()
	state := ipv6StateText(detection.State, detection.Source, detection.Detail, interfaceForIP(net.ParseIP(detection.Source)))
	switch mode {
	case ipv6Off:
		line("IPv6", "off (IPv4 only), host state: "+state)
	case ipv6Prefer:
		line("IPv6", "prefer, "+state)
	default:
		line("IPv6", "auto, "+state)
	}
	if cfg.UDPEnabled {
		line("UDP relay", fmt.Sprintf("enabled, %d association(s), idle %s",
			cfg.MaxUDPAssociations, cfg.UDPIdleTimeout))
	} else {
		line("UDP relay", "disabled")
	}
	if cfg.DomainResolution {
		line("Domain names", "resolved on the exit (ATYP=domain)")
	} else {
		line("Domain names", "ATYP=domain refused, the client resolves")
	}

	section("Web interface")
	if cfg.Web.Enabled {
		for i, url := range webURLs(cfg) {
			if i == 0 {
				line("URL", url)
			} else {
				fmt.Printf("%-19s %s\n", "", url)
			}
		}
		host := cfg.webBindHost()
		if host == "" {
			host = "every address"
		}
		line("Listener", fmt.Sprintf("%s on %s:%d", webScheme(cfg), host, cfg.Web.Port))
		line("Login", currentWebUser(cfg.statePath(webCredentialsFileName)))
	} else {
		line("Web", "disabled")
	}

	section("DNS server")
	if cfg.DNS.Enabled {
		line("State", "enabled (x-node dns status)")
		line("Listen", cfg.dnsListen())
		if alfisWebUISupported(dnsBinaryPath()) {
			line("Alfis web", cfg.dnsPanelURL())
		} else {
			line("Alfis web", "not available in this Alfis build")
		}
	} else {
		line("State", "disabled")
	}

	section("Quotas")
	if cfg.Usage.Enabled {
		line("Accounting", "on, timezone "+cfg.Usage.Timezone)
	} else {
		line("Accounting", "off (quotas are not enforced)")
	}
	line("Defaults", quotaDefaultsSummary(cfg.Quotas))
	quotas, quotaErr := openQuotas(cfg, newLogger("warn"))
	if quotaErr == nil {
		line("Overrides", fmt.Sprintf("%d client(s) with their own limits", len(quotas.List())))
	}

	section("Live")
	if doc, err := readRuntime(cfg.statePath(runtimeFileName)); err == nil && runtimeFresh(doc, time.Now()) {
		line("Clients", fmt.Sprintf("%d connected, %d open connection(s)",
			len(doc.Clients), doc.TotalConnections))
		if cfg.UDPEnabled {
			line("UDP active", fmt.Sprintf("%d association(s), %d datagram(s) in, %d out",
				doc.TotalUDPAssociations, doc.TotalUDPDatagramsIn, doc.TotalUDPDatagramsOut))
			line("UDP traffic", fmt.Sprintf("%s in, %s out",
				formatBytes(doc.TotalUDPBytesIn), formatBytes(doc.TotalUDPBytesOut)))
		}
		if doc.TotalRefused > 0 {
			line("Refused", fmt.Sprintf("%d connection(s), max %d per client",
				doc.TotalRefused, cfg.MaxConnsPerClient))
		}
	} else {
		line("Clients", "service is not running")
	}
	if usage, err := openUsage(cfg, newLogger("warn")); err == nil {
		var today, month, total uint64
		for _, row := range usage.rows() {
			today += row.DayRx + row.DayTx
			month += row.MonthRx + row.MonthTx
			total += row.Rx + row.Tx
		}
		line("Traffic today", formatBytes(today))
		line("Traffic month", formatBytes(month))
		line("Traffic total", formatBytes(total))
	}

	section("Files")
	line("State directory", cfg.StateDir)
	if pendingSettings(cfg, path) {
		line("Config", path+" (changed since the service started; restart it to apply)")
	} else {
		line("Config", path)
	}
	return nil
}

// quotaDefaultsSummary renders the default limits for a client without its own
// quota entry.
func quotaDefaultsSummary(defaults QuotaDefaults) string {
	parts := []string{
		"monthly " + formatLimit(defaults.MonthlyBytes),
		"daily " + formatLimit(defaults.DailyBytes),
		"down " + formatRate(defaults.DownKbps),
		"up " + formatRate(defaults.UpKbps),
	}
	return strings.Join(parts, ", ")
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	configFlag := addConfigFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(*configFlag)
	fmt.Println("Config:      ", path)
	cfg, err := loadConfig(path)
	if err != nil {
		fmt.Println("  FAIL:", err)
		return err
	}
	fmt.Println("  OK")

	problems := 0
	check := func(name string, ok bool, detail string) {
		if ok {
			fmt.Printf("  OK   %s\n", name)
			return
		}
		problems++
		fmt.Printf("  FAIL %s: %s\n", name, detail)
	}

	check("listenAddress is in 200::/7", isYggAddress(net.ParseIP(cfg.ListenAddress)), cfg.ListenAddress)
	check("Yggdrasil address is assigned", interfaceHasAddress(cfg.ListenAddress),
		"is yggdrasil running and is the interface up?")

	stateOK := true
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		stateOK = false
	}
	probe := filepath.Join(cfg.StateDir, ".doctor")
	if stateOK {
		if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
			stateOK = false
		}
		_ = os.Remove(probe)
	}
	check("state directory is writable", stateOK, cfg.StateDir)
	if pendingSettings(cfg, path) {
		fmt.Println("  WARN the configuration file changed since the service started; restart it to apply")
	}

	store, err := openStore(cfg, newLogger("warn"))
	if err == nil {
		check("allowlist is readable", true, "")
		fmt.Printf("       %d client(s), %d active invite(s)\n", len(store.List()), len(store.ListInvites()))
	} else {
		check("allowlist is readable", false, err.Error())
	}

	checkPort := func(name string, port int) {
		addr := net.JoinHostPort(cfg.ListenAddress, strconv.Itoa(port))
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			check(name, false, err.Error())
			return
		}
		_ = conn.Close()
		check(name, true, "")
	}
	checkPort("SOCKS5 port is listening", cfg.SocksPort)
	if !cfg.apiShared() {
		checkPort("API port is listening", cfg.APIPort)
	}
	if cfg.UDPEnabled {
		// The relay binds an ephemeral UDP port for every association, so the
		// check proves the Yggdrasil address can carry UDP at all.
		probe, err := net.ListenUDP("udp", &net.UDPAddr{
			IP:   net.ParseIP(cfg.ListenAddress),
			Port: 0,
		})
		if err != nil {
			check("UDP relay can bind to the Yggdrasil address", false, err.Error())
		} else {
			_ = probe.Close()
			check("UDP relay can bind to the Yggdrasil address", true, "")
		}
	} else {
		fmt.Println("  OK   UDP relay is disabled")
	}

	if port, err := cfg.peerPort(); err != nil {
		check("peerURI is valid", false, err.Error())
	} else if port > 0 {
		check("Yggdrasil peering port is listening", localPortListening(port),
			fmt.Sprintf("nothing listens on TCP/%d; check Listen in yggdrasil.conf", port))
	}

	if health, err := fetchHealth(cfg); err != nil {
		check("API /v1/health", false, err.Error())
	} else {
		check("API /v1/health", health == cfg.Name, "unexpected name "+health)
	}

	// Authentication of the API is a security setting, not a failure: a node
	// that deliberately runs open must not fail its own doctor. The warning is
	// loud enough to be noticed in the output.
	switch cfg.apiAuthMode() {
	case apiAuthOff:
		fmt.Println("  WARN the API is not authenticated (apiToken off): any Yggdrasil node can read the status")
	case apiAuthOptional:
		fmt.Println("  WARN API tokens are optional (apiToken optional): the status is public")
	default:
		fmt.Printf("  OK   API requires a token (%d request(s)/min per source)\n", cfg.APIRateLimit)
	}

	checkIPv6(cfg)
	checkDomainResolution(cfg)
	checkWeb(cfg)
	checkQuotas(cfg)

	if peers, err := yggPeers(); err != nil {
		fmt.Println("  WARN could not read Yggdrasil peers:", err)
	} else {
		fmt.Printf("       Yggdrasil peers up: %d\n", peers)
		if peers == 0 {
			problems++
			fmt.Println("  FAIL no Yggdrasil peer is up: check /etc/yggdrasil/yggdrasil.conf")
		}
	}

	if live, err := newAdminSocket(cfg.AdminSocket, 3*time.Second).Peers(); err != nil {
		check("Yggdrasil admin socket is reachable", false, err.Error())
	} else {
		check("Yggdrasil admin socket is reachable", true, "")
		outbound, inbound := 0, 0
		for _, peer := range live {
			if peer.Inbound {
				inbound++
				continue
			}
			outbound++
		}
		fmt.Printf("       public peers: %d, inbound clients: %d\n", outbound, inbound)
		check("peerCount is filled", outbound >= cfg.PeerCount,
			fmt.Sprintf("%d of %d public peers", outbound, cfg.PeerCount))
	}

	if problems > 0 {
		return fmt.Errorf("%d problem(s) found", problems)
	}
	fmt.Println("No problems found.")
	return nil
}

// checkIPv6 reports whether outbound IPv6 connections are possible. A host
// without IPv6 is not broken: the exit falls back to IPv4, so the message is a
// note unless the operator asked for "prefer". A host that has an address the
// provider does not route is always a warning: the interface looks configured,
// yet every IPv6 destination would time out.
func checkIPv6(cfg Config) {
	mode := cfg.ipv6Mode()
	detection := detectIPv6()
	text := ipv6StateText(detection.State, detection.Source, detection.Detail, interfaceForIP(net.ParseIP(detection.Source)))
	switch mode {
	case ipv6Off:
		fmt.Println("  OK   IPv6 is off: outbound connections use IPv4 only")
		if detection.State == ipv6StateBroken {
			fmt.Printf("       note: %s\n", text)
		}
	case ipv6Prefer:
		if detection.Available {
			fmt.Printf("  OK   IPv6 is preferred and available (%s)\n", text)
			return
		}
		fmt.Printf("  WARN ipv6 is \"prefer\" but %s; connections fall back to IPv4\n", text)
	default:
		if detection.Available {
			fmt.Printf("  OK   IPv6 is available (%s)\n", text)
			return
		}
		if detection.State == ipv6StateBroken {
			fmt.Printf("  WARN %s; the exit uses IPv4 until the provider fixes the route\n", text)
			return
		}
		fmt.Printf("  OK   %s; outbound connections use IPv4\n", text)
	}
}

// checkDomainResolution proves that the exit can resolve names itself, which is
// what ATYP=domain needs. The lookup goes to the system resolver of the host.
func checkDomainResolution(cfg Config) {
	if !cfg.DomainResolution {
		fmt.Println("  OK   domain names are not resolved here (domainResolution off)")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := net.DefaultResolver.LookupHost(ctx, "one.one.one.one"); err != nil {
		fmt.Printf("  FAIL DNS resolution: %v\n", err)
		fmt.Println("       the exit resolves ATYP=domain requests itself; check /etc/resolv.conf")
		return
	}
	fmt.Println("  OK   DNS resolution works (needed for ATYP=domain)")
}

// checkWeb verifies the listener of the built-in interface. It dials the address
// the interface is bound to, so a firewall in front of the port is not a
// problem here: only the local listener is checked.
func checkWeb(cfg Config) {
	if !cfg.Web.Enabled {
		fmt.Println("  OK   web interface is disabled")
		return
	}
	host := cfg.webBindHost()
	if host == "" {
		host = "127.0.0.1"
	}
	address := net.JoinHostPort(host, strconv.Itoa(cfg.Web.Port))
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		fmt.Printf("  FAIL web interface is not listening on %s: %v\n", address, err)
		return
	}
	_ = conn.Close()
	fmt.Printf("  OK   web interface listens on %s\n", address)
	for _, url := range webURLs(cfg) {
		fmt.Printf("       %s\n", url)
	}
	if _, err := loadWebCredentials(cfg.statePath(webCredentialsFileName)); err != nil {
		fmt.Printf("  FAIL web credentials are missing (%s): run \"x-node web password\"\n", err)
	}
	switch cfg.webTLSMode() {
	case webTLSSelf:
		fmt.Println("  WARN the web interface uses a self-signed certificate: browsers show a warning")
		fmt.Println("       put a real certificate in web.certFile / web.keyFile or terminate TLS in nginx")
	case webTLSAuto:
		if cfg.Web.CertFile == "" && !fileExists(cfg.statePath("tls/web.crt")) {
			fmt.Println("  NOTE the web interface is plain HTTP; terminate TLS in nginx or set web.tls")
		}
	}
}

// checkQuotas prints the quota configuration and proves that the accounting
// file can be written: without it a quota can never be enforced.
func checkQuotas(cfg Config) {
	if !cfg.Usage.Enabled {
		fmt.Println("  OK   traffic accounting is off; quotas are not enforced")
		return
	}
	location, err := cfg.usageLocation()
	if err != nil {
		fmt.Printf("  FAIL usage timezone: %v\n", err)
		return
	}
	fmt.Printf("  OK   traffic accounting is on (day and month change at midnight %s)\n", location)
	fmt.Printf("       quota defaults: %s\n", quotaDefaultsSummary(cfg.Quotas))
}

func fetchHealth(cfg Config) (string, error) {
	url := "http://" + net.JoinHostPort(cfg.ListenAddress, strconv.Itoa(cfg.apiPort())) + "/v1/health"
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return "", err
	}
	var payload struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	return payload.Name, nil
}

func yggPeers() (int, error) {
	path, err := exec.LookPath("yggdrasilctl")
	if err != nil {
		return 0, errors.New("yggdrasilctl is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "getPeers").Output()
	if err != nil {
		return 0, err
	}
	return parseYggPeers(string(out)), nil
}

// parseYggPeers counts the peers whose State column is "Up". 0.5.x prints a
// box table; older builds printed "true"/"false", so the plain-text heuristic
// stays as a fallback for them.
func parseYggPeers(out string) int {
	up := 0
	table := false
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "│") {
			continue
		}
		table = true
		fields := strings.Split(line, "│")
		if len(fields) < 4 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(fields[2]), "Up") {
			up++
		}
	}
	if table {
		return up
	}
	for _, line := range strings.Split(out, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "true") && !strings.Contains(lower, "inbound") {
			up++
		}
	}
	return up
}

// localPortListening reports whether something accepts TCP connections on the
// port over loopback. Yggdrasil listens on [::] or 0.0.0.0, so one of the two
// addresses answers when the peering listener is up.
func localPortListening(port int) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), time.Second)
		if err == nil {
			_ = conn.Close()
			return true
		}
	}
	return false
}

func interfaceHasAddress(address string) bool {
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if network, ok := addr.(*net.IPNet); ok && network.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// formatBytes renders a counter for the CLI in decimal units: 1 MB is
// 1 000 000 bytes, which is what a provider and a speed test show. The quota
// parser accepts the binary MiB/GiB spellings explicitly when a file was
// written with them.
func formatBytes(bytes uint64) string {
	const unit = 1000
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	index := -1
	for value >= unit && index < len(units)-1 {
		value /= unit
		index++
	}
	return fmt.Sprintf("%.1f %s", value, units[index])
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit <= 1 {
		return value[:limit]
	}
	return value[:limit-1] + "…"
}

// clientName renders a client the way every CLI table shows it: the address
// first, then the label in parentheses. The label is what an operator
// recognises; the address is what the exit actually enforces.
func clientName(address, label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return address
	}
	return address + " (" + truncate(label, 24) + ")"
}
