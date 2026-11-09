package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The web interface runs as the unprivileged service user. Two operations need
// root: restarting a systemd unit and writing a file under /etc. Instead of
// giving the whole daemon root, the installer starts a small root service,
// "x-node helper serve", which listens on a unix socket that only the x-node
// group can open. The service accepts a fixed set of actions and validates
// every request.
//
// A sudoers rule would be shorter, but the daemon runs with NoNewPrivileges=yes
// (see deploy/systemd/x-node.service), which makes sudo refuse to run as root.
// A separate service keeps that hardening in place.

const (
	helperSocketPath = "/run/x-node/helper.sock"
	helperRuntimeDir = "/run/x-node"
	helperBinaryPath = "/usr/local/bin/x-node"
	helperMainConfig = "/etc/x-node/config.json"
	helperYggConfig  = "/etc/yggdrasil/yggdrasil.conf"
	helperManifest   = "/var/lib/x-node/install.json"
	helperBackupDir  = "/var/backups"
	helperMaxConfig  = 512 << 10

	// The DNS server is a separate process: Alfis needs CAP_NET_BIND_SERVICE
	// for port 53, which the hardened x-node service deliberately does not
	// have. The helper owns the config, the unit and the enable/disable calls.
	helperDNSConfig  = "/etc/x-node/alfis.conf"
	helperDNSState   = "/var/lib/x-node-dns"
	helperDNSUnit    = "/etc/systemd/system/x-node-dns.service"
	helperDNSUnitBin = "/usr/local/bin/alfis"
	// helperUnitPath is the service the installer writes. The helper repairs it
	// at start, because an in-place upgrade only replaces binaries.
	helperUnitPath = "/etc/systemd/system/x-node.service"
)

// helperRequest is the document the web interface sends.
type helperRequest struct {
	Action  string `json:"action"`
	Service string `json:"service,omitempty"`
	File    string `json:"file,omitempty"`
	Content string `json:"content,omitempty"`
	// Enabled is used by dns-sync: true writes the configuration and starts the
	// unit, false stops and disables it.
	Enabled *bool `json:"enabled,omitempty"`
}

// requestHelperInstalled reports whether the root helper is running. Without
// it the dashboard hides the restart and config buttons instead of failing on
// every click.
func requestHelperInstalled() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	info, err := os.Stat(helperSocketPath)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSocket != 0
}

// cmdHelper is the root side. "serve" starts the socket service the installer
// runs; without arguments it reads one JSON request from stdin, which is handy
// for debugging by hand.
func cmdHelper(args []string) error {
	if len(args) == 1 && args[0] == "serve" {
		return helperServe()
	}
	if len(args) != 0 {
		return errors.New("usage: x-node helper [serve]")
	}
	if os.Geteuid() != 0 {
		return errors.New("x-node helper must run as root")
	}
	var request helperRequest
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&request); err != nil {
		return fmt.Errorf("parse request: %w", err)
	}
	response, err := helperDispatch(request)
	if err != nil {
		return err
	}
	writeJSONLine(response)
	return nil
}

// helperServe accepts one request per connection on the unix socket. The
// socket is root:x-node 0660, so only the service user and root can talk to
// it; the daemon itself never runs as root.
func helperServe() error {
	if runtime.GOOS != "linux" {
		return errors.New("the helper socket is only available on Linux")
	}
	if os.Geteuid() != 0 {
		return errors.New("x-node helper serve must run as root")
	}
	if err := os.MkdirAll(helperRuntimeDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", helperRuntimeDir, err)
	}
	// An installation updated from the dashboard keeps the unit file it was
	// installed with. The old one stopped x-node together with yggdrasil, and
	// systemd cannot drop a dependency with a drop-in, so the helper repairs
	// the file once at start.
	if err := repairUnitDependency(helperUnitPath); err != nil {
		fmt.Fprintf(os.Stderr, "x-node helper: could not repair %s: %v\n", helperUnitPath, err)
	}
	if err := os.Remove(helperSocketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove the old socket: %w", err)
	}
	listener, err := net.Listen("unix", helperSocketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", helperSocketPath, err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(helperSocketPath)
	}()
	if err := os.Chmod(helperSocketPath, 0o660); err != nil {
		return fmt.Errorf("chmod the socket: %w", err)
	}
	if entry, err := user.LookupGroup("x-node"); err == nil {
		if gid, err := strconv.Atoi(entry.Gid); err == nil {
			_ = os.Chown(helperSocketPath, 0, gid)
		}
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go helperConnection(conn)
	}
}

// repairUnitDependency drops the yggdrasil dependency that made systemd stop
// x-node together with yggdrasil. Requires= and PartOf= cannot be removed with
// a drop-in, so the unit file itself is rewritten; every other line, including
// an operator's own settings, is kept as it is. A missing unit is not an error:
// the helper also runs in a test or on a machine where the service is not
// installed.
func repairUnitDependency(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	fixed, changed := stripYggdrasilDependency(string(raw))
	if !changed {
		return nil
	}
	if err := writeFileAs(path, []byte(fixed), 0o644, ""); err != nil {
		return err
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %s", strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(os.Stderr, "x-node helper: %s no longer stops x-node with yggdrasil\n", path)
	return nil
}

// stripYggdrasilDependency rewrites the two dependency lines of a unit:
// Requires=yggdrasil.service becomes Wants=yggdrasil.service, and
// PartOf=yggdrasil.service is dropped. The rest of the file is returned
// unchanged, so the operator's own settings survive.
func stripYggdrasilDependency(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	hasWants := false
	for _, line := range lines {
		compact := compactUnitLine(line)
		if strings.HasPrefix(compact, "Wants=") && strings.Contains(compact, "yggdrasil.service") {
			hasWants = true
			break
		}
	}
	out := make([]string, 0, len(lines)+1)
	changed := false
	for _, line := range lines {
		switch compactUnitLine(line) {
		case "PartOf=yggdrasil.service":
			changed = true
		case "Requires=yggdrasil.service":
			changed = true
			if !hasWants {
				out = append(out, "Wants=yggdrasil.service")
				hasWants = true
			}
		default:
			out = append(out, line)
		}
	}
	if !changed {
		return text, false
	}
	return strings.Join(out, "\n"), true
}

// compactUnitLine removes the whitespace a unit line may carry around "=", so
// "Requires = yggdrasil.service" and "Requires=yggdrasil.service" are the same
// line. A comment keeps its "#" and never matches a directive.
func compactUnitLine(line string) string {
	return strings.Join(strings.Fields(line), "")
}

// helperConnection handles one request. A request is one JSON document and the
// answer is one JSON document, so neither side has to close anything.
func helperConnection(conn net.Conn) {
	defer conn.Close()
	// The upgrade action downloads and verifies a release, which can take a
	// while on a slow link; the other actions finish long before this limit.
	_ = conn.SetDeadline(time.Now().Add(20 * time.Minute))
	var request helperRequest
	if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&request); err != nil {
		writeJSONTo(conn, map[string]any{"ok": false, "error": "bad request: " + err.Error()})
		return
	}
	response, err := helperDispatch(request)
	if err != nil {
		writeJSONTo(conn, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSONTo(conn, response)
}

func helperDispatch(request helperRequest) (any, error) {
	switch request.Action {
	case "restart":
		if err := helperRestart(request.Service); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "service": request.Service}, nil
	case "read-config":
		return helperReadConfig(request.File)
	case "write-config":
		path, err := helperWriteConfig(request.File, request.Content)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "path": path}, nil
	case "dns-status":
		return helperDNSStatus()
	case "dns-sync":
		if request.Enabled == nil {
			return nil, errors.New("dns-sync needs the enabled field")
		}
		return helperDNSSync(*request.Enabled, request.Content)
	case "upgrade":
		return helperUpgrade()
	default:
		return nil, fmt.Errorf("unknown helper action %q", request.Action)
	}
}

// dnsBinaryPath finds the Alfis binary the unit should run. The installer puts
// it in /usr/local/bin; a distribution package keeps it in /usr/bin.
func dnsBinaryPath() string {
	if path, err := exec.LookPath("alfis"); err == nil {
		return path
	}
	if info, err := os.Stat(helperDNSUnitBin); err == nil && !info.IsDir() {
		return helperDNSUnitBin
	}
	return ""
}

// alfisWebUICache keeps the last probe of the Alfis binary. The dashboard asks
// for the DNS state every few seconds, and reading a 20 MB executable on every
// request would be wasteful.
var alfisWebUICache struct {
	sync.Mutex
	path      string
	size      int64
	mod       time.Time
	supported bool
}

// alfisWebUISupported reports whether the Alfis binary was built with the
// "webui" feature. The nogui binaries published upstream are built without it
// and exit when --web-ui is passed, so the unit must not use the flag there.
//
// The session cookie name is compiled into the webui module only, while the
// "no web UI support" message is compiled into the branch that rejects the
// flag. Both are string literals, so stripping the symbols keeps them.
func alfisWebUISupported(binary string) bool {
	if binary == "" {
		return false
	}
	info, err := os.Stat(binary)
	if err != nil {
		return false
	}
	alfisWebUICache.Lock()
	if alfisWebUICache.path == binary && alfisWebUICache.size == info.Size() &&
		alfisWebUICache.mod.Equal(info.ModTime()) {
		supported := alfisWebUICache.supported
		alfisWebUICache.Unlock()
		return supported
	}
	alfisWebUICache.Unlock()

	data, err := os.ReadFile(binary)
	supported := err == nil &&
		bytes.Contains(data, []byte("alfis_session")) &&
		!bytes.Contains(data, []byte("This build of ALFIS has no web UI support!"))

	alfisWebUICache.Lock()
	alfisWebUICache.path = binary
	alfisWebUICache.size = info.Size()
	alfisWebUICache.mod = info.ModTime()
	alfisWebUICache.supported = supported
	alfisWebUICache.Unlock()
	return supported
}

// helperDNSStatus reports what the dashboard needs to draw the DNS card. The
// service state comes from systemd; a missing unit is "inactive", not an error.
func helperDNSStatus() (any, error) {
	binary := dnsBinaryPath()
	state, _ := systemctlOutput("is-active", "x-node-dns")
	if state == "" {
		state = "inactive"
	}
	enabled, _ := systemctlOutput("is-enabled", "x-node-dns")
	config := false
	if info, err := os.Stat(helperDNSConfig); err == nil && !info.IsDir() {
		config = true
	}
	return map[string]any{
		"ok":        true,
		"installed": binary != "",
		"binary":    binary,
		"webUI":     alfisWebUISupported(binary),
		"config":    config,
		"active":    state == "active" || state == "activating",
		"enabled":   enabled == "enabled",
		"state":     state,
	}, nil
}

func systemctlOutput(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// helperDNSSync writes the Alfis configuration and the unit, then starts or
// stops the service. The configuration is generated by the daemon from the
// live settings, so this function never has to parse TOML.
func helperDNSSync(enabled bool, content string) (any, error) {
	if !enabled {
		out, err := systemctlOutput("disable", "--now", "x-node-dns")
		if err != nil {
			// A unit that does not exist is already in the wanted state.
			message := strings.ToLower(out + " " + err.Error())
			if !strings.Contains(message, "not found") && !strings.Contains(message, "no such file") &&
				!strings.Contains(message, "does not exist") {
				return nil, fmt.Errorf("stop x-node-dns: %s", strings.TrimSpace(out))
			}
		}
		return helperDNSStatus()
	}

	binary := dnsBinaryPath()
	if binary == "" {
		return nil, errors.New("Alfis is not installed; install the alfis binary in /usr/local/bin or run the installer again")
	}
	if strings.TrimSpace(content) == "" {
		return nil, errors.New("the DNS configuration is empty")
	}
	if !strings.Contains(content, "[dns]") || !strings.Contains(content, "[webui]") {
		return nil, errors.New("the DNS configuration has no [dns] or [webui] section")
	}
	if len(content) > helperMaxConfig {
		return nil, fmt.Errorf("the DNS configuration is larger than %d KiB", helperMaxConfig>>10)
	}
	if err := os.MkdirAll(helperDNSState, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", helperDNSState, err)
	}
	if entry, err := user.Lookup("x-node"); err == nil {
		if uid, uidErr := strconv.Atoi(entry.Uid); uidErr == nil {
			if gid, gidErr := strconv.Atoi(entry.Gid); gidErr == nil {
				if err := os.Chown(helperDNSState, uid, gid); err != nil {
					return nil, fmt.Errorf("chown %s: %w", helperDNSState, err)
				}
			}
		}
	}
	if err := helperBackup(helperDNSConfig); err != nil {
		return nil, err
	}
	if err := writeFileAs(helperDNSConfig, []byte(content), 0o640, "x-node"); err != nil {
		return nil, fmt.Errorf("write %s: %w", helperDNSConfig, err)
	}
	if err := writeFileAs(helperDNSUnit, []byte(dnsUnit(binary, alfisWebUISupported(binary))), 0o644, ""); err != nil {
		return nil, fmt.Errorf("write %s: %w", helperDNSUnit, err)
	}
	if _, err := systemctlOutput("daemon-reload"); err != nil {
		return nil, fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if _, err := systemctlOutput("enable", "x-node-dns"); err != nil {
		return nil, fmt.Errorf("enable x-node-dns: %w", err)
	}
	if _, err := systemctlOutput("restart", "x-node-dns"); err != nil {
		return nil, fmt.Errorf("start x-node-dns: %w", err)
	}
	return helperDNSStatus()
}

func dnsUnit(binary string, webUI bool) string {
	// --web-ui makes a build without the feature exit at once, and systemd
	// would restart it forever. The installer bundles a build with the web UI,
	// but an upstream nogui binary or a package from the distribution may still
	// be in place, so the flag is added only when the binary supports it.
	webUIFlag := ""
	if webUI {
		webUIFlag = " --web-ui"
	}
	return fmt.Sprintf(`[Unit]
Description=X-Node DNS resolver (Alfis)
Wants=network-online.target
After=network-online.target yggdrasil.service

[Service]
User=x-node
Group=x-node
WorkingDirectory=%s
ExecStart=%s -n -c %s%s
Restart=always
RestartSec=5
SecureBits=keep-caps
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=%s
PrivateTmp=true

[Install]
WantedBy=multi-user.target
`, helperDNSState, binary, helperDNSConfig, webUIFlag, helperDNSState)
}

// helperUpgrade replaces the installed binaries with the latest release and
// restarts both units. The answer is written before the restart: this process
// is running the old binary and will be stopped by systemd a second later.
func helperUpgrade() (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	from, to, err := upgradeInstalled(ctx, daemonVersion, func(format string, values ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", values...)
	})
	if err != nil {
		return nil, err
	}
	restartAfterUpgrade()
	return map[string]any{"ok": true, "from": from, "to": to, "restarted": true}, nil
}

// helperReadConfig returns a configuration file to the web interface. The
// yggdrasil.conf file belongs to the yggdrasil group, which the service user is
// not in, so reading it needs the same root path as writing it.
func helperReadConfig(file string) (any, error) {
	path, _, err := helperConfigTarget(file)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"ok":       true,
		"path":     path,
		"content":  string(raw),
		"size":     len(raw),
		"modified": info.ModTime(),
	}, nil
}

// helperRestart queues a systemd restart. --no-block keeps the answer quick:
// restarting x-node stops the daemon that asked for it, and a synchronous call
// would be killed with it before the reply is written.
func helperRestart(service string) error {
	switch service {
	case "x-node", "yggdrasil", "x-node-dns":
	default:
		return fmt.Errorf("refusing to restart %q", service)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "restart", "--no-block", service).CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("systemctl restart %s: %s", service, message)
	}
	return nil
}

// helperWriteConfig validates the document, keeps a backup and replaces the
// file atomically. The owner and the group stay those the service expects.
func helperWriteConfig(file, content string) (string, error) {
	path, group, err := helperConfigTarget(file)
	if err != nil {
		return "", err
	}
	if len(content) == 0 {
		return "", errors.New("the configuration is empty")
	}
	if len(content) > helperMaxConfig {
		return "", fmt.Errorf("the configuration is larger than %d KiB", helperMaxConfig>>10)
	}
	if err := helperValidateConfig(file, content); err != nil {
		return "", err
	}
	if err := helperBackup(path); err != nil {
		return "", err
	}
	if err := writeFileAs(path, []byte(content), 0o640, group); err != nil {
		return "", err
	}
	return path, nil
}

func helperConfigTarget(file string) (path, group string, err error) {
	switch file {
	case "main":
		return helperMainConfig, "x-node", nil
	case "yggdrasil":
		return yggConfigPath(), "yggdrasil", nil
	default:
		return "", "", fmt.Errorf("unknown configuration %q (use main or yggdrasil)", file)
	}
}

// yggConfigPath prefers the path recorded by the installer, so a non-default
// location keeps working.
func yggConfigPath() string {
	raw, err := os.ReadFile(helperManifest)
	if err == nil {
		var manifest struct {
			YggConf string `json:"yggConf"`
		}
		if json.Unmarshal(raw, &manifest) == nil && strings.TrimSpace(manifest.YggConf) != "" {
			return manifest.YggConf
		}
	}
	return helperYggConfig
}

func helperValidateConfig(file, content string) error {
	var document any
	if err := json.Unmarshal([]byte(content), &document); err != nil {
		return fmt.Errorf("not valid JSON: %w", err)
	}
	tmp, err := os.CreateTemp("", "x-node-config-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	switch file {
	case "main":
		if _, err := loadConfig(name); err != nil {
			return err
		}
	case "yggdrasil":
		var fields map[string]any
		if err := json.Unmarshal([]byte(content), &fields); err != nil {
			return err
		}
		for _, key := range []string{"Listen", "Peers"} {
			if raw, ok := fields[key]; ok {
				if err := validateStringList(key, raw); err != nil {
					return err
				}
			}
		}
		// Yggdrasil is the only authority on its own format. Running the
		// normaliser catches a typo before the service is restarted.
		if binary, err := exec.LookPath("yggdrasil"); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, binary, "-useconffile", name, "-normaliseconf", "-json").CombinedOutput()
			if err != nil {
				message := strings.TrimSpace(string(out))
				if message == "" {
					message = err.Error()
				}
				return fmt.Errorf("Yggdrasil rejected the configuration: %s", message)
			}
		}
	}
	return nil
}

func validateStringList(key string, raw any) error {
	list, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("%s must be a list of strings", key)
	}
	for _, item := range list {
		if _, ok := item.(string); !ok {
			return fmt.Errorf("%s must be a list of strings", key)
		}
	}
	return nil
}

// helperBackup copies the current file into /var/backups. A failed backup is
// reported instead of silently overwriting the only copy.
func helperBackup(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(helperBackupDir, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("%s.%s", filepath.Base(path), time.Now().Format("20060102-150405"))
	target := filepath.Join(helperBackupDir, name)
	if err := writeFileAs(target, raw, 0o600, ""); err != nil {
		return fmt.Errorf("write backup %s: %w", target, err)
	}
	return nil
}

// writeFileAs writes a temporary file next to the target and renames it, so a
// reader never sees a half-written configuration.
func writeFileAs(path string, raw []byte, mode os.FileMode, group string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	if group != "" && runtime.GOOS == "linux" {
		if entry, err := user.LookupGroup(group); err == nil {
			if gid, err := strconv.Atoi(entry.Gid); err == nil {
				_ = os.Chown(name, 0, gid)
			}
		}
	}
	return os.Rename(name, path)
}

func writeJSONLine(value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	fmt.Println(string(raw))
}

func writeJSONTo(conn net.Conn, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = conn.Write(raw)
}
