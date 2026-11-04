package main

import (
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
)

// helperRequest is the document the web interface sends.
type helperRequest struct {
	Action  string `json:"action"`
	Service string `json:"service,omitempty"`
	File    string `json:"file,omitempty"`
	Content string `json:"content,omitempty"`
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

// helperConnection handles one request. A request is one JSON document and the
// answer is one JSON document, so neither side has to close anything.
func helperConnection(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
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
	default:
		return nil, fmt.Errorf("unknown helper action %q", request.Action)
	}
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
	case "x-node", "yggdrasil":
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
