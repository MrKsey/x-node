package main

import (
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"
)

// openTestUsage builds a store on a temporary state directory with a fixed
// timezone, so the day and month boundaries are predictable in the test.
func openTestUsage(t *testing.T, cfg Config) *usageStore {
	t.Helper()
	store, err := openUsage(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestUsageCountsAndPersists(t *testing.T) {
	cfg := testConfig(t)
	cfg.Usage.Enabled = true
	cfg.Usage.Timezone = "UTC"
	store := openTestUsage(t, cfg)
	ip := net.ParseIP("202:24ae:c219:8a4a:de94:eae2:b97e:ac87")

	store.addTraffic(ip, 1000, 4000)
	store.addTraffic(ip, 500, 0)
	store.addConnection(ip)
	store.addUDPAssociation(ip)
	store.addRefused(ip)

	day, month := store.used(ip)
	if day != 5500 || month != 5500 {
		t.Fatalf("used = %d day, %d month, want 5500", day, month)
	}
	row, ok := store.get(ip.String())
	if !ok {
		t.Fatal("the client is missing from the counters")
	}
	if row.Rx != 1500 || row.Tx != 4000 {
		t.Fatalf("rx/tx = %d/%d, want 1500/4000", row.Rx, row.Tx)
	}
	if row.TCPConnections != 1 || row.UDPAssociations != 1 || row.Refused != 1 {
		t.Fatalf("counters = %d/%d/%d, want 1/1/1",
			row.TCPConnections, row.UDPAssociations, row.Refused)
	}

	store.flush(true)
	if _, err := os.Stat(cfg.statePath(usageFileName)); err != nil {
		t.Fatalf("usage.json was not written: %v", err)
	}

	// A restart reads the same numbers back: a quota must survive a service
	// restart, otherwise a client could reset its traffic by bouncing it.
	reopened := openTestUsage(t, cfg)
	day, month = reopened.used(ip)
	if day != 5500 || month != 5500 {
		t.Fatalf("after reopen used = %d/%d, want 5500/5500", day, month)
	}
}

func TestUsageResetsOnNewDay(t *testing.T) {
	cfg := testConfig(t)
	cfg.Usage.Enabled = true
	cfg.Usage.Timezone = "UTC"
	store := openTestUsage(t, cfg)
	ip := net.ParseIP("202:24ae:c219:8a4a:de94:eae2:b97e:ac87")
	store.addTraffic(ip, 1000, 0)

	// Rewind the stored day, as if the counters were written yesterday.
	store.mu.Lock()
	store.clients[ip.String()].Day = "2000-01-01"
	store.mu.Unlock()

	day, month := store.used(ip)
	if day != 0 {
		t.Fatalf("day = %d, want 0 after the date changed", day)
	}
	if month != 1000 {
		t.Fatalf("month = %d, want 1000: the month did not change", month)
	}
}

func TestUsageReset(t *testing.T) {
	cfg := testConfig(t)
	cfg.Usage.Enabled = true
	first := net.ParseIP("202:24ae:c219:8a4a:de94:eae2:b97e:ac87")
	second := net.ParseIP("202:24ae:c219:8a4a:de94:eae2:b97e:ac88")
	store := openTestUsage(t, cfg)
	store.addTraffic(first, 100, 100)
	store.addTraffic(second, 200, 200)

	store.reset(first.String(), false)
	if day, _ := store.used(first); day != 0 {
		t.Fatalf("reset client still has %d bytes", day)
	}
	if day, _ := store.used(second); day != 400 {
		t.Fatalf("the other client lost its counters: %d", day)
	}

	store.reset("", true)
	if day, _ := store.used(second); day != 0 {
		t.Fatalf("reset --all left %d bytes", day)
	}
}

func TestUsageFileShape(t *testing.T) {
	cfg := testConfig(t)
	cfg.Usage.Enabled = true
	cfg.Usage.Timezone = "Europe/Moscow"
	store := openTestUsage(t, cfg)
	ip := net.ParseIP("202:24ae:c219:8a4a:de94:eae2:b97e:ac87")
	store.addTraffic(ip, 10, 20)
	store.flush(true)

	raw, err := os.ReadFile(cfg.statePath(usageFileName))
	if err != nil {
		t.Fatal(err)
	}
	var doc usageFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("usage.json is not valid JSON: %v", err)
	}
	if doc.Timezone != "Europe/Moscow" {
		t.Fatalf("timezone = %q", doc.Timezone)
	}
	client := doc.Clients[ip.String()]
	if client == nil {
		t.Fatal("the client is missing from usage.json")
	}
	if want := time.Now().In(mustLocation(t, "Europe/Moscow")).Format("2006-01-02"); client.Day != want {
		t.Fatalf("day = %q, want %q", client.Day, want)
	}
}

func TestUsageDisabledWritesNothing(t *testing.T) {
	cfg := testConfig(t)
	cfg.Usage.Enabled = false
	store := openTestUsage(t, cfg)
	store.addTraffic(net.ParseIP("202:24ae:c219:8a4a:de94:eae2:b97e:ac87"), 1, 1)
	store.flush(true)
	if _, err := os.Stat(cfg.statePath(usageFileName)); err == nil {
		t.Fatal("usage.json was written while accounting is disabled")
	}
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return location
}
