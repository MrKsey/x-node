package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		raw  string
		want []int
		ok   bool
	}{
		{"1.6.10", []int{1, 6, 10}, true},
		{"v1.6.9", []int{1, 6, 9}, true},
		{"V2", []int{2}, true},
		{"1.4.4+24", []int{1, 4, 4}, true},
		{"1.6.9-rc1", []int{1, 6, 9}, true},
		{"dev", nil, false},
		{"", nil, false},
		{"1.x.0", nil, false},
	}
	for _, test := range cases {
		got, ok := parseVersion(test.raw)
		if ok != test.ok {
			t.Fatalf("parseVersion(%q) ok = %v, want %v", test.raw, ok, test.ok)
		}
		if !ok {
			continue
		}
		if len(got) != len(test.want) {
			t.Fatalf("parseVersion(%q) = %v, want %v", test.raw, got, test.want)
		}
		for i := range got {
			if got[i] != test.want[i] {
				t.Fatalf("parseVersion(%q) = %v, want %v", test.raw, got, test.want)
			}
		}
	}
}

func TestVersionNewer(t *testing.T) {
	cases := []struct {
		candidate string
		current   string
		want      bool
	}{
		{"1.6.10", "1.6.9", true},
		{"1.6.9", "1.6.10", false},
		{"1.6.9", "1.6.9", false},
		{"2.0.0", "1.99.99", true},
		{"1.6.9.1", "1.6.9", true},
		{"dev", "1.6.9", false},
		{"1.6.9", "dev", true},
	}
	for _, test := range cases {
		if got := versionNewer(test.candidate, test.current); got != test.want {
			t.Fatalf("versionNewer(%q, %q) = %v, want %v", test.candidate, test.current, got, test.want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	if got := compareVersions([]int{1, 6, 9}, []int{1, 6, 9}); got != 0 {
		t.Fatalf("equal versions compare as %d", got)
	}
	if got := compareVersions([]int{1, 6, 9}, []int{1, 7}); got != -1 {
		t.Fatalf("shorter older version compares as %d", got)
	}
	if got := compareVersions([]int{1, 7}, []int{1, 6, 9}); got != 1 {
		t.Fatalf("newer version compares as %d", got)
	}
}

func TestUpdateAssetName(t *testing.T) {
	name := updateAssetName("1.6.10")
	if runtime.GOOS != "linux" {
		if name != "" {
			t.Fatalf("updateAssetName on %s = %q, want empty", runtime.GOOS, name)
		}
		return
	}
	switch runtime.GOARCH {
	case "amd64", "arm64", "arm":
		want := "x-node-1.6.10-linux-" + runtime.GOARCH + ".tar.gz"
		if name != want {
			t.Fatalf("updateAssetName = %q, want %q", name, want)
		}
	default:
		if name != "" {
			t.Fatalf("updateAssetName on linux/%s = %q, want empty", runtime.GOARCH, name)
		}
	}
}

func TestChecksumFor(t *testing.T) {
	file := filepath.Join(t.TempDir(), "SHA256SUMS.txt")
	content := strings.Join([]string{
		"aaaa  x-node-1.6.9-linux-amd64.tar.gz",
		"bbbb  ./x-node-1.6.10-linux-amd64.tar.gz",
		"cccc  dist/x-node-1.6.10-linux-arm64.tar.gz",
		"malformed line",
	}, "\n")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := checksumFor(file, "x-node-1.6.9-linux-amd64.tar.gz"); !ok || got != "aaaa" {
		t.Fatalf("checksumFor plain entry = %q, %v", got, ok)
	}
	if got, ok := checksumFor(file, "x-node-1.6.10-linux-amd64.tar.gz"); !ok || got != "bbbb" {
		t.Fatalf("checksumFor ./ entry = %q, %v", got, ok)
	}
	if got, ok := checksumFor(file, "x-node-1.6.10-linux-arm64.tar.gz"); !ok || got != "cccc" {
		t.Fatalf("checksumFor subdirectory entry = %q, %v", got, ok)
	}
	if _, ok := checksumFor(file, "x-node-9.9.9-linux-amd64.tar.gz"); ok {
		t.Fatal("checksumFor found a missing entry")
	}
}

func writeTestArchive(t *testing.T, entries map[string]string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "release.tar.gz")
	file, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for header, body := range entries {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: header, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestExtractUpdateBinaries(t *testing.T) {
	archive := writeTestArchive(t, map[string]string{
		"x-node-1.6.10-linux-amd64/bin/x-node":           "daemon",
		"x-node-1.6.10-linux-amd64/bin/x-node-peerprobe": "probe",
		"x-node-1.6.10-linux-amd64/bin/install.sh":       "installer",
		"x-node-1.6.10-linux-amd64/README.md":            "docs",
	})
	binaries, cleanup, err := extractUpdateBinaries(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for name, want := range map[string]string{"x-node": "daemon", "x-node-peerprobe": "probe"} {
		raw, err := os.ReadFile(binaries[name])
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(raw) != want {
			t.Fatalf("%s contains %q, want %q", name, raw, want)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(binaries["x-node"]), "install.sh")); !os.IsNotExist(err) {
		t.Fatal("extractUpdateBinaries wrote an unexpected file")
	}
}

func TestExtractUpdateBinariesRequiresBoth(t *testing.T) {
	archive := writeTestArchive(t, map[string]string{
		"x-node-1.6.10-linux-amd64/bin/x-node": "daemon",
	})
	if _, _, err := extractUpdateBinaries(archive); err == nil {
		t.Fatal("extractUpdateBinaries accepted an archive without x-node-peerprobe")
	}
}
