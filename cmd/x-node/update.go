package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// The updater reads the latest GitHub release, downloads the archive that
// matches this machine, checks the SHA-256 published next to it and replaces
// the two binaries. It never touches the configuration, the allowlist or the
// state directory: an upgrade is a new executable, not a reinstall.

const (
	updateRepo          = "MrKsey/x-node"
	updateReleaseAPI    = "https://api.github.com/repos/" + updateRepo + "/releases/latest"
	updateChecksumAsset = "SHA256SUMS.txt"
	updateMaxArchive    = 128 << 20
	updateMaxBinary     = 64 << 20
)

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type releaseInfo struct {
	TagName     string         `json:"tag_name"`
	Body        string         `json:"body"`
	HTMLURL     string         `json:"html_url"`
	PublishedAt time.Time      `json:"published_at"`
	Assets      []releaseAsset `json:"assets"`
}

// updateInfo is both the answer of "x-node update check" and the payload of
// the web endpoint. The asset URLs stay unexported: the browser needs to know
// whether an upgrade is possible, not where the archive is downloaded from.
type updateInfo struct {
	Current     string `json:"current"`
	Latest      string `json:"latest,omitempty"`
	Available   bool   `json:"available"`
	URL         string `json:"url,omitempty"`
	Notes       string `json:"notes,omitempty"`
	PublishedAt string `json:"publishedAt,omitempty"`
	Asset       string `json:"asset,omitempty"`
	CanUpgrade  bool   `json:"canUpgrade"`
	Helper      bool   `json:"helper,omitempty"`
	Running     bool   `json:"running,omitempty"`
	Error       string `json:"error,omitempty"`

	assetURL    string
	checksumURL string
}

func updateHTTPClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Minute}
}

// checkForUpdate asks GitHub for the latest release and compares it with the
// running build.
func checkForUpdate(ctx context.Context, current string) (*updateInfo, error) {
	client := updateHTTPClient()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, updateReleaseAPI, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "x-node/"+current)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("reach GitHub: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("GitHub refused the request (HTTP %d); the anonymous rate limit is 60 checks per hour", response.StatusCode)
		}
		return nil, fmt.Errorf("GitHub returned HTTP %d", response.StatusCode)
	}
	var release releaseInfo
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&release); err != nil {
		return nil, fmt.Errorf("read the release: %w", err)
	}
	tag := strings.TrimSpace(release.TagName)
	if tag == "" {
		return nil, errors.New("the latest release has no tag")
	}
	latest := strings.TrimLeft(tag, "vV")
	info := &updateInfo{
		Current:   current,
		Latest:    latest,
		Available: versionNewer(latest, current),
		URL:       release.HTMLURL,
		Notes:     strings.TrimSpace(release.Body),
	}
	if !release.PublishedAt.IsZero() {
		info.PublishedAt = release.PublishedAt.Format(time.RFC3339)
	}
	if info.URL == "" {
		info.URL = "https://github.com/" + updateRepo + "/releases"
	}
	assetName := updateAssetName(latest)
	if assetName != "" {
		if asset, ok := findReleaseAsset(release.Assets, assetName); ok {
			info.Asset = asset.Name
			info.assetURL = asset.BrowserDownloadURL
		}
		if sums, ok := findReleaseAsset(release.Assets, updateChecksumAsset); ok {
			info.checksumURL = sums.BrowserDownloadURL
		}
	}
	// An upgrade needs a matching archive and the published checksums; without
	// them the dashboard only offers the release page.
	info.CanUpgrade = runtime.GOOS == "linux" &&
		info.assetURL != "" && info.checksumURL != "" &&
		strings.HasPrefix(info.assetURL, "https://")
	return info, nil
}

// updateAssetName is the archive the release workflow builds for this
// platform, e.g. x-node-1.6.9-linux-amd64.tar.gz.
func updateAssetName(version string) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	switch runtime.GOARCH {
	case "amd64", "arm64", "arm":
		return fmt.Sprintf("x-node-%s-linux-%s.tar.gz", version, runtime.GOARCH)
	default:
		return ""
	}
}

func findReleaseAsset(assets []releaseAsset, name string) (releaseAsset, bool) {
	for _, asset := range assets {
		if asset.Name == name {
			return asset, true
		}
	}
	return releaseAsset{}, false
}

// versionNewer reports whether candidate is newer than current. A development
// build ("dev") has no numeric version, so any published release wins.
func versionNewer(candidate, current string) bool {
	next, ok := parseVersion(candidate)
	if !ok {
		return false
	}
	now, ok := parseVersion(current)
	if !ok {
		return true
	}
	return compareVersions(next, now) > 0
}

func parseVersion(raw string) ([]int, bool) {
	cleaned := strings.TrimSpace(raw)
	cleaned = strings.TrimPrefix(strings.TrimPrefix(cleaned, "v"), "V")
	if index := strings.IndexAny(cleaned, "+-"); index >= 0 {
		cleaned = cleaned[:index]
	}
	if cleaned == "" {
		return nil, false
	}
	parts := strings.Split(cleaned, ".")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		out = append(out, value)
	}
	return out, true
}

func compareVersions(a, b []int) int {
	length := len(a)
	if len(b) > length {
		length = len(b)
	}
	for i := 0; i < length; i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// upgradeInstalled downloads, verifies and installs the latest release. It
// returns the version it replaced and the version it installed.
func upgradeInstalled(ctx context.Context, current string, logf func(string, ...any)) (string, string, error) {
	if runtime.GOOS != "linux" {
		return current, "", errors.New("releases are only published for Linux")
	}
	if os.Geteuid() != 0 {
		return current, "", errors.New("the upgrade writes /usr/local/bin: run it as root")
	}
	info, err := checkForUpdate(ctx, current)
	if err != nil {
		return current, "", err
	}
	if !info.Available {
		return current, info.Latest, fmt.Errorf("x-node %s is already the latest release", current)
	}
	if !info.CanUpgrade {
		return current, info.Latest, fmt.Errorf("release %s has no archive for %s/%s", info.Latest, runtime.GOOS, runtime.GOARCH)
	}
	logf("Downloading %s", info.Asset)
	archive, err := downloadUpdateFile(ctx, info.assetURL)
	if err != nil {
		return current, "", err
	}
	defer os.Remove(archive)
	logf("Downloading %s", updateChecksumAsset)
	sums, err := downloadUpdateFile(ctx, info.checksumURL)
	if err != nil {
		return current, "", err
	}
	defer os.Remove(sums)

	want, ok := checksumFor(sums, info.Asset)
	if !ok {
		return current, "", fmt.Errorf("%s does not list %s", updateChecksumAsset, info.Asset)
	}
	got, err := fileSHA256(archive)
	if err != nil {
		return current, "", err
	}
	if !strings.EqualFold(got, want) {
		return current, "", fmt.Errorf("checksum mismatch for %s", info.Asset)
	}
	logf("Checksum verified")

	binaries, cleanup, err := extractUpdateBinaries(archive)
	if err != nil {
		return current, "", err
	}
	defer cleanup()
	// The archive is named after the release, but the binary itself is the
	// authority: a wrong or truncated upload must not replace a working one.
	out, err := exec.CommandContext(ctx, binaries["x-node"], "version").Output()
	if err != nil {
		return current, "", fmt.Errorf("the downloaded x-node does not start: %w", err)
	}
	if strings.TrimSpace(string(out)) != info.Latest {
		return current, "", fmt.Errorf("the archive contains x-node %q, expected %s", strings.TrimSpace(string(out)), info.Latest)
	}

	executable, err := os.Executable()
	if err != nil {
		return current, "", err
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	dir := filepath.Dir(executable)
	for _, name := range []string{"x-node", "x-node-peerprobe"} {
		source, ok := binaries[name]
		if !ok {
			return current, "", fmt.Errorf("the archive has no %s", name)
		}
		raw, err := os.ReadFile(source)
		if err != nil {
			return current, "", err
		}
		if name == "x-node" {
			backupCurrentBinary(executable, current)
		}
		if err := writeFileAs(filepath.Join(dir, name), raw, 0o755, ""); err != nil {
			return current, "", fmt.Errorf("install %s: %w", name, err)
		}
	}
	return current, info.Latest, nil
}

func downloadUpdateFile(ctx context.Context, url string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "x-node-updater/"+daemonVersion)
	response, err := updateHTTPClient().Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", path.Base(url), response.StatusCode)
	}
	file, err := os.CreateTemp("", "x-node-update-*")
	if err != nil {
		return "", err
	}
	name := file.Name()
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, updateMaxArchive+1))
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(name)
		return "", copyErr
	}
	if closeErr != nil {
		_ = os.Remove(name)
		return "", closeErr
	}
	if written > updateMaxArchive {
		_ = os.Remove(name)
		return "", errors.New("the download is larger than 128 MiB")
	}
	return name, nil
}

// checksumFor reads a sha256sum file and returns the hash of one entry.
func checksumFor(file, name string) (string, bool) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if path.Base(strings.TrimPrefix(fields[1], "./")) != name {
			continue
		}
		return fields[0], true
	}
	return "", false
}

func fileSHA256(name string) (string, error) {
	file, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// extractUpdateBinaries unpacks the two executables into a temporary directory.
// Only the two known names are written: the rest of the archive (installer,
// docs, service units) is not needed for an in-place upgrade.
func extractUpdateBinaries(archive string) (map[string]string, func(), error) {
	file, err := os.Open(archive)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, nil, fmt.Errorf("read the archive: %w", err)
	}
	defer gzipReader.Close()
	dir, err := os.MkdirTemp("", "x-node-update-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	wanted := map[string]string{"x-node": "", "x-node-peerprobe": ""}
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("read the archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		base := path.Base(header.Name)
		if _, ok := wanted[base]; !ok || wanted[base] != "" {
			continue
		}
		if !strings.Contains(header.Name, "/bin/") {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(reader, updateMaxBinary+1))
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		if len(raw) > updateMaxBinary {
			cleanup()
			return nil, nil, fmt.Errorf("%s is larger than 64 MiB", base)
		}
		target := filepath.Join(dir, base)
		if err := os.WriteFile(target, raw, 0o700); err != nil {
			cleanup()
			return nil, nil, err
		}
		wanted[base] = target
	}
	for name, target := range wanted {
		if target == "" {
			cleanup()
			return nil, nil, fmt.Errorf("the archive has no bin/%s", name)
		}
	}
	return wanted, cleanup, nil
}

// backupCurrentBinary keeps the previous executable in /var/backups. A failed
// backup is not fatal: the upgrade itself has already been verified.
func backupCurrentBinary(executable, version string) {
	if os.Geteuid() != 0 {
		return
	}
	raw, err := os.ReadFile(executable)
	if err != nil {
		return
	}
	if err := os.MkdirAll(helperBackupDir, 0o755); err != nil {
		return
	}
	name := filepath.Join(helperBackupDir, "x-node-"+version)
	_ = writeFileAs(name, raw, 0o700, "")
}

// restartAfterUpgrade restarts both units after the answer has been written.
// The helper process is replaced by the new binary too, so it must not call
// systemd synchronously: --no-block returns before the unit stops.
func restartAfterUpgrade() {
	go func() {
		time.Sleep(time.Second)
		_ = exec.Command("systemctl", "restart", "--no-block", "x-node").Run()
		_ = exec.Command("systemctl", "restart", "--no-block", "x-node-helper").Run()
	}()
}

func cmdUpdate(args []string) error {
	action := "check"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action = args[0]
		args = args[1:]
	}
	switch action {
	case "check":
		return cmdUpdateCheck(args)
	case "upgrade":
		return cmdUpdateUpgrade(args)
	default:
		return fmt.Errorf("unknown update action %q (use check or upgrade)", action)
	}
}

func cmdUpdateCheck(args []string) error {
	fs := flag.NewFlagSet("update check", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	info, err := checkForUpdate(ctx, daemonVersion)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(info)
	}
	fmt.Printf("Current version: %s\n", info.Current)
	fmt.Printf("Latest release:  %s\n", info.Latest)
	if info.Available {
		fmt.Printf("A newer release is available: %s\n", info.URL)
		fmt.Println("Run \"x-node update upgrade\" as root to install it.")
	} else {
		fmt.Println("x-node is up to date.")
	}
	return nil
}

func cmdUpdateUpgrade(args []string) error {
	fs := flag.NewFlagSet("update upgrade", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	noRestart := fs.Bool("no-restart", false, "replace the binaries but do not restart the services")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	logf := func(format string, values ...any) {
		if !*asJSON {
			fmt.Printf(format+"\n", values...)
		}
	}
	from, to, err := upgradeInstalled(ctx, daemonVersion, logf)
	if err != nil {
		return err
	}
	restarted := false
	if !*noRestart {
		executable, _ := os.Executable()
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		if executable == helperBinaryPath {
			logf("Restarting x-node and the root helper")
			_ = exec.Command("systemctl", "restart", "--no-block", "x-node").Run()
			_ = exec.Command("systemctl", "restart", "--no-block", "x-node-helper").Run()
			restarted = true
		} else {
			logf("Restart the service to use the new binary")
		}
	}
	if *asJSON {
		return printJSON(map[string]any{
			"ok": true, "from": from, "to": to, "restarted": restarted,
		})
	}
	fmt.Printf("x-node %s installed (was %s)\n", to, from)
	return nil
}

func printJSON(value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}
