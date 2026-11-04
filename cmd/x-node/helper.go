package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// giving the whole daemon root, the installer grants sudo for exactly one
// command:
//
//	x-node ALL=(root) NOPASSWD: /usr/local/bin/x-node helper
//
// The helper reads one JSON request from stdin and accepts a fixed set of
// actions. The arguments of the sudoers rule are matched exactly, so no other
// x-node command can be run through it.

const (
	helperSudoersPath = "/etc/sudoers.d/x-node-web"
	helperBinaryPath  = "/usr/local/bin/x-node"
	helperMainConfig  = "/etc/x-node/config.json"
	helperYggConfig   = "/etc/yggdrasil/yggdrasil.conf"
	helperManifest    = "/var/lib/x-node/install.json"
	helperBackupDir   = "/var/backups"
	helperMaxConfig   = 512 << 10
)

// helperRequest is the stdin document of "x-node helper".
type helperRequest struct {
	Action  string `json:"action"`
	Service string `json:"service,omitempty"`
	File    string `json:"file,omitempty"`
	Content string `json:"content,omitempty"`
}

// requestHelperInstalled reports whether the installer granted the web
// interface the right to call the helper. Without it the dashboard hides the
// restart and config buttons instead of failing on every click.
func requestHelperInstalled() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if _, err := os.Stat(helperSudoersPath); err != nil {
		return false
	}
	if _, err := os.Stat(helperBinaryPath); err != nil {
		return false
	}
	return true
}

// cmdHelper is the root side. It is deliberately undocumented in the command
// overview: it is an internal entry point of the web interface.
func cmdHelper(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: x-node helper (reads one JSON request from stdin)")
	}
	if os.Geteuid() != 0 {
		return errors.New("x-node helper must run as root; it is called through sudo")
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	var request helperRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return fmt.Errorf("parse request: %w", err)
	}
	switch request.Action {
	case "restart":
		return helperRestart(request.Service)
	case "read-config":
		return helperReadConfig(request.File)
	case "write-config":
		path, err := helperWriteConfig(request.File, request.Content)
		if err != nil {
			return err
		}
		writeJSONLine(map[string]any{"ok": true, "path": path})
		return nil
	default:
		return fmt.Errorf("unknown helper action %q", request.Action)
	}
}

// helperReadConfig returns a configuration file to the web interface. The
// yggdrasil.conf file belongs to the yggdrasil group, which the service user is
// not in, so reading it needs the same root path as writing it.
func helperReadConfig(file string) error {
	path, _, err := helperConfigTarget(file)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	writeJSONLine(map[string]any{
		"ok":       true,
		"path":     path,
		"content":  string(raw),
		"size":     len(raw),
		"modified": info.ModTime(),
	})
	return nil
}

// helperRestart queues a systemd restart. --no-block matters: the helper is a
// child of x-node and lives in the same cgroup, so a synchronous restart would
// kill the helper (and the HTTP response) halfway through.
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
	writeJSONLine(map[string]any{"ok": true, "service": service})
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
