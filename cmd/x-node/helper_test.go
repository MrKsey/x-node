package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hasWantsLine reports whether the unit asks for a unit through a Wants= line,
// either alone or together with another target.
func hasWantsLine(unit, name string) bool {
	for _, line := range strings.Split(unit, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Wants=") && strings.Contains(trimmed, name) {
			return true
		}
	}
	return false
}

func TestStripYggdrasilDependency(t *testing.T) {
	old := `[Unit]
Description=X-Node — Yggdrasil exit node
After=network-online.target yggdrasil.service
Wants=network-online.target
Requires=yggdrasil.service
PartOf=yggdrasil.service

[Service]
ExecStart=/usr/local/bin/x-node serve
`
	fixed, changed := stripYggdrasilDependency(old)
	if !changed {
		t.Fatal("the old unit was reported as unchanged")
	}
	if strings.Contains(fixed, "Requires=yggdrasil.service") {
		t.Fatal("Requires=yggdrasil.service survived")
	}
	if strings.Contains(fixed, "PartOf=yggdrasil.service") {
		t.Fatal("PartOf=yggdrasil.service survived")
	}
	if !hasWantsLine(fixed, "yggdrasil.service") {
		t.Fatalf("the unit has no yggdrasil want:\n%s", fixed)
	}
	if !strings.Contains(fixed, "ExecStart=/usr/local/bin/x-node serve") {
		t.Fatal("an unrelated line was lost")
	}
	if !strings.Contains(fixed, "After=network-online.target yggdrasil.service") {
		t.Fatal("the ordering line was lost")
	}
}

func TestStripYggdrasilDependencyKeepsAFixedUnit(t *testing.T) {
	unit := `[Unit]
Description=X-Node
After=yggdrasil.service
Wants=yggdrasil.service
`
	fixed, changed := stripYggdrasilDependency(unit)
	if changed {
		t.Fatal("a fixed unit was reported as changed")
	}
	if fixed != unit {
		t.Fatalf("a fixed unit was rewritten:\n%s", fixed)
	}
}

func TestStripYggdrasilDependencyKeepsOneWant(t *testing.T) {
	unit := `[Unit]
Wants=yggdrasil.service
Requires=yggdrasil.service
PartOf=yggdrasil.service
`
	fixed, changed := stripYggdrasilDependency(unit)
	if !changed {
		t.Fatal("the old unit was reported as unchanged")
	}
	if count := strings.Count(fixed, "Wants=yggdrasil.service"); count != 1 {
		t.Fatalf("Wants=yggdrasil.service appears %d times:\n%s", count, fixed)
	}
}

func TestStripYggdrasilDependencyHandlesSpacing(t *testing.T) {
	unit := "[Unit]\nRequires = yggdrasil.service\nPartOf = yggdrasil.service\n"
	fixed, changed := stripYggdrasilDependency(unit)
	if !changed {
		t.Fatal("a unit with spaces around '=' was reported as unchanged")
	}
	if strings.Contains(fixed, "Requires") || strings.Contains(fixed, "PartOf") {
		t.Fatalf("the old dependency survived:\n%s", fixed)
	}
	if !hasWantsLine(fixed, "yggdrasil.service") {
		t.Fatalf("the unit has no yggdrasil want:\n%s", fixed)
	}
}

func TestStripYggdrasilDependencyKeepsComments(t *testing.T) {
	unit := "[Unit]\n# Requires=yggdrasil.service was removed here\n"
	if _, changed := stripYggdrasilDependency(unit); changed {
		t.Fatal("a comment was rewritten")
	}
}

func TestRepairUnitDependencyWritesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x-node.service")
	old := "[Unit]\nRequires=yggdrasil.service\nPartOf=yggdrasil.service\n"
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	// The repair calls systemctl, which is not available in a test; the file
	// is what the test can check, so the error is ignored on purpose.
	_ = repairUnitDependency(path)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Requires=yggdrasil.service") {
		t.Fatalf("the file still has the old dependency:\n%s", raw)
	}
	if !strings.Contains(string(raw), "Wants=yggdrasil.service") {
		t.Fatalf("the file has no yggdrasil want:\n%s", raw)
	}
}

func TestRepairUnitDependencyIgnoresAMissingFile(t *testing.T) {
	if err := repairUnitDependency(filepath.Join(t.TempDir(), "absent.service")); err != nil {
		t.Fatalf("a missing unit returned an error: %v", err)
	}
}

func TestDNSUnitAddsWebUIOnlyWhenSupported(t *testing.T) {
	withUI := dnsUnit("/usr/local/bin/alfis", true)
	if !strings.Contains(withUI, " -n -c /etc/x-node/alfis.conf --web-ui") {
		t.Fatalf("the web UI flag is missing:\n%s", withUI)
	}
	withoutUI := dnsUnit("/usr/local/bin/alfis", false)
	if strings.Contains(withoutUI, "--web-ui") {
		t.Fatalf("a build without the web UI got the flag:\n%s", withoutUI)
	}
	if !strings.Contains(withoutUI, " -n -c /etc/x-node/alfis.conf\n") {
		t.Fatalf("the ExecStart line changed unexpectedly:\n%s", withoutUI)
	}
}

func TestAlfisWebUISupportedReadsTheBinary(t *testing.T) {
	dir := t.TempDir()
	withUI := filepath.Join(dir, "alfis-webui")
	if err := os.WriteFile(withUI, []byte("prefix alfis_session suffix"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !alfisWebUISupported(withUI) {
		t.Fatal("a webui binary was reported as unsupported")
	}
	withoutUI := filepath.Join(dir, "alfis-nogui")
	if err := os.WriteFile(withoutUI, []byte("This build of ALFIS has no web UI support!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if alfisWebUISupported(withoutUI) {
		t.Fatal("a nogui binary was reported as supported")
	}
	// The negative marker wins even when the cookie name is also present.
	both := filepath.Join(dir, "alfis-both")
	if err := os.WriteFile(both, []byte("alfis_session This build of ALFIS has no web UI support!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if alfisWebUISupported(both) {
		t.Fatal("the no-webui marker was ignored")
	}
	if alfisWebUISupported(filepath.Join(dir, "absent")) {
		t.Fatal("a missing binary was reported as supported")
	}
}

func TestHelperConfigTargetKnowsAlfis(t *testing.T) {
	path, group, err := helperConfigTarget("alfis")
	if err != nil {
		t.Fatal(err)
	}
	if path != helperDNSConfig || group != "x-node" {
		t.Fatalf("alfis target = %q, group %q", path, group)
	}
	if _, _, err := helperConfigTarget("nope"); err == nil {
		t.Fatal("an unknown file was accepted")
	}
}
