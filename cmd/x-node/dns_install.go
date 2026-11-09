package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The DNS server is Alfis running as x-node-dns.service. The installer only
// downloads Alfis when the DNS server was enabled during installation, so an
// operator who turns it on later from the web interface or with "x-node dns on"
// would find the unit failing with "Alfis is not installed". These functions
// fetch the binary on demand, so the switch alone is enough to bring the
// resolver up.
const (
	// alfisVersion is the upstream release used when the X-Node archive cannot
	// be downloaded. The bundled binary is preferred: it is built with the web
	// UI, which the dashboard link expects.
	alfisVersion   = "0.10.0"
	alfisMaxBinary = 64 << 20
)

// installAlfis returns the path of the Alfis binary, downloading and installing
// it first when it is missing. The X-Node release archive is tried first; an
// upstream nogui build is the fallback for a local build or an architecture the
// release does not cover.
func installAlfis(ctx context.Context) (string, error) {
	if binary := dnsBinaryPath(); binary != "" {
		return binary, nil
	}
	releaseErr := error(nil)
	if binary, err := installAlfisFromRelease(ctx); err == nil {
		return binary, nil
	} else {
		releaseErr = err
	}
	if binary, err := installAlfisFromUpstream(ctx); err == nil {
		return binary, nil
	} else if releaseErr != nil {
		return "", fmt.Errorf("the X-Node archive: %v; the upstream build: %w", releaseErr, err)
	} else {
		return "", err
	}
}

// installAlfisFromRelease unpacks bin/alfis from the archive of the latest
// X-Node release. The checksum is verified when the release publishes one.
func installAlfisFromRelease(ctx context.Context) (string, error) {
	info, err := checkForUpdate(ctx, daemonVersion)
	if err != nil {
		return "", err
	}
	if info.assetURL == "" {
		return "", fmt.Errorf("release %s has no archive for %s/%s", info.Latest, runtime.GOOS, runtime.GOARCH)
	}
	archive, err := downloadUpdateFile(ctx, info.assetURL)
	if err != nil {
		return "", err
	}
	defer os.Remove(archive)
	if info.checksumURL != "" {
		sums, err := downloadUpdateFile(ctx, info.checksumURL)
		if err == nil {
			defer os.Remove(sums)
			if want, ok := checksumFor(sums, path.Base(info.Asset)); ok {
				got, err := fileSHA256(archive)
				if err != nil {
					return "", err
				}
				if !strings.EqualFold(got, want) {
					return "", fmt.Errorf("checksum mismatch for %s", info.Asset)
				}
			}
		}
	}
	raw, err := readAlfisFromArchive(archive)
	if err != nil {
		return "", err
	}
	return writeAlfis(raw)
}

// readAlfisFromArchive returns bin/alfis from a release tarball. Only that one
// file is read: the archive also carries the installer and the docs, which the
// DNS server does not need.
func readAlfisFromArchive(archive string) ([]byte, error) {
	file, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("read the archive: %w", err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg || path.Base(header.Name) != "alfis" {
			continue
		}
		if !strings.Contains(header.Name, "/bin/") {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(reader, alfisMaxBinary+1))
		if err != nil {
			return nil, err
		}
		if len(raw) > alfisMaxBinary {
			return nil, errors.New("the Alfis binary is larger than 64 MiB")
		}
		return raw, nil
	}
	return nil, errors.New("the archive has no bin/alfis")
}

// installAlfisFromUpstream downloads the nogui build published by the Alfis
// project. DNS works with it, but its panel does not exist, so the dashboard
// hides the web UI link.
func installAlfisFromUpstream(ctx context.Context) (string, error) {
	arch := ""
	switch runtime.GOARCH {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	case "arm":
		arch = "armhf"
	default:
		return "", fmt.Errorf("there is no Alfis build for %s", runtime.GOARCH)
	}
	url := fmt.Sprintf(
		"https://github.com/Revertron/Alfis/releases/download/v%s/alfis-linux-%s-v%s-nogui",
		alfisVersion, arch, alfisVersion,
	)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "x-node/"+daemonVersion)
	response, err := updateHTTPClient().Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download Alfis: HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, alfisMaxBinary+1))
	if err != nil {
		return "", err
	}
	if len(raw) > alfisMaxBinary {
		return "", errors.New("the Alfis binary is larger than 64 MiB")
	}
	return writeAlfis(raw)
}

// writeAlfis installs the binary atomically: a half-written file must never
// replace a working one.
func writeAlfis(raw []byte) (string, error) {
	if len(raw) < 1<<20 {
		return "", errors.New("the downloaded Alfis binary is too small")
	}
	if err := os.MkdirAll(filepath.Dir(helperDNSUnitBin), 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Dir(helperDNSUnitBin), err)
	}
	temp := helperDNSUnitBin + ".new"
	if err := os.WriteFile(temp, raw, 0o755); err != nil {
		return "", fmt.Errorf("write %s: %w", temp, err)
	}
	if err := os.Chown(temp, 0, 0); err != nil {
		_ = os.Remove(temp)
		return "", fmt.Errorf("chown %s: %w", temp, err)
	}
	if err := os.Rename(temp, helperDNSUnitBin); err != nil {
		_ = os.Remove(temp)
		return "", fmt.Errorf("install %s: %w", helperDNSUnitBin, err)
	}
	return helperDNSUnitBin, nil
}

// installAlfisWithTimeout bounds the download: the web request waits for the
// answer, so a stalled mirror must not hold the settings form forever.
func installAlfisWithTimeout() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	return installAlfis(ctx)
}
