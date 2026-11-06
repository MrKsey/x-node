package main

import (
	"testing"
	"time"
)

func TestRuntimeTrackerPublishesConnections(t *testing.T) {
	cfg := testConfig(t)
	tracker := newRuntimeTracker(cfg, newLogger("error"))
	first := testAddr(t, 1)
	second := testAddr(t, 2)

	tracker.open(first, "1.1.1.1:443")
	tracker.open(first, "1.1.1.1:443")
	tracker.open(second, "example.com:80")
	tracker.flush(true)

	doc, err := readRuntime(cfg.statePath(runtimeFileName))
	if err != nil {
		t.Fatal(err)
	}
	if doc == nil || doc.TotalConnections != 3 {
		t.Fatalf("total connections = %+v, want 3", doc)
	}
	if len(doc.Clients) != 2 {
		t.Fatalf("clients = %d, want 2", len(doc.Clients))
	}
	byAddress := make(map[string]runtimeClient)
	for _, client := range doc.Clients {
		byAddress[client.Address] = client
	}
	if got := byAddress[first.String()].Connections; got != 2 {
		t.Fatalf("first client connections = %d, want 2", got)
	}
	if got := byAddress[first.String()].Destinations; len(got) != 1 || got[0] != "1.1.1.1:443" {
		t.Fatalf("destinations = %v", got)
	}

	tracker.close(first)
	tracker.flush(true)
	doc, err = readRuntime(cfg.statePath(runtimeFileName))
	if err != nil {
		t.Fatal(err)
	}
	if doc.TotalConnections != 2 {
		t.Fatalf("total after close = %d, want 2", doc.TotalConnections)
	}

	tracker.shutdown()
	if doc, err := readRuntime(cfg.statePath(runtimeFileName)); err != nil || doc != nil {
		t.Fatalf("runtime file must be removed on shutdown: doc=%+v err=%v", doc, err)
	}
}

func TestRuntimeTrackerCountsRefusals(t *testing.T) {
	cfg := testConfig(t)
	tracker := newRuntimeTracker(cfg, newLogger("error"))
	client := testAddr(t, 5)

	tracker.refused(client)
	tracker.refused(client)
	tracker.open(client, "1.1.1.1:443")

	live := tracker.live(client)
	if live.Refused != 2 {
		t.Fatalf("refused = %d, want 2", live.Refused)
	}
	if live.Connections != 1 {
		t.Fatalf("connections = %d, want 1", live.Connections)
	}

	tracker.flush(true)
	doc, err := readRuntime(cfg.statePath(runtimeFileName))
	if err != nil {
		t.Fatal(err)
	}
	if doc.TotalRefused != 2 {
		t.Fatalf("total refused = %d, want 2", doc.TotalRefused)
	}
	var found bool
	for _, row := range doc.Clients {
		if row.Address == client.String() {
			found = true
			if row.Refused != 2 {
				t.Fatalf("client refused = %d, want 2", row.Refused)
			}
		}
	}
	if !found {
		t.Fatal("the refused client is missing from the runtime file")
	}
	tracker.shutdown()
}

func TestRuntimeTrackerSeparatesSuspendedRefusals(t *testing.T) {
	cfg := testConfig(t)
	tracker := newRuntimeTracker(cfg, newLogger("error"))
	client := testAddr(t, 6)

	tracker.refused(client)
	tracker.suspendedRefused(client)
	tracker.suspendedRefused(client)

	live := tracker.live(client)
	if live.Refused != 3 {
		t.Fatalf("refused = %d, want 3", live.Refused)
	}
	if live.SuspendedRefused != 2 {
		t.Fatalf("suspended refused = %d, want 2", live.SuspendedRefused)
	}
	if live.QuotaRefused != 0 {
		t.Fatalf("quota refused = %d, want 0", live.QuotaRefused)
	}
	tracker.shutdown()
}

func TestMergeClientsWithLiveState(t *testing.T) {
	now := time.Now()
	first := testAddr(t, 1)
	second := testAddr(t, 2)
	third := testAddr(t, 3)
	doc := &runtimeFile{
		Version:          1,
		UpdatedAt:        now,
		TotalConnections: 2,
		Clients: []runtimeClient{
			{Address: first.String(), Connections: 2, LastSeenAt: now},
			{Address: second.String(), Connections: 0, LastSeenAt: now.Add(-time.Minute)},
		},
	}
	clients := []Client{
		{Address: first.String(), Label: "phone", Source: "invite"},
		{Address: third.String(), Label: "laptop", Source: "invite"},
	}

	rows := mergeClients(clients, doc, now)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	byAddress := make(map[string]clientRow)
	for _, row := range rows {
		byAddress[row.Address] = row
	}
	if row := byAddress[first.String()]; row.State != "connected" || row.Connections != 2 {
		t.Fatalf("first row = %+v", row)
	}
	if row := byAddress[second.String()]; row.State != "idle" || row.Source != "open" {
		t.Fatalf("second row = %+v", row)
	}
	if row := byAddress[third.String()]; row.State != "registered" {
		t.Fatalf("third row = %+v", row)
	}
	if rows[0].Address != first.String() {
		t.Fatalf("connected clients must come first: %+v", rows[0])
	}
}

func TestMergeClientsIgnoresStaleRuntime(t *testing.T) {
	now := time.Now()
	first := testAddr(t, 1)
	doc := &runtimeFile{
		UpdatedAt:        now.Add(-2 * time.Minute),
		TotalConnections: 1,
		Clients:          []runtimeClient{{Address: first.String(), Connections: 1, LastSeenAt: now}},
	}
	if runtimeFresh(doc, now) {
		t.Fatal("a two minute old file must be stale")
	}
	rows := mergeClients([]Client{{Address: first.String()}}, doc, now)
	if len(rows) != 1 || rows[0].State != "registered" || rows[0].Connections != 0 {
		t.Fatalf("rows = %+v", rows)
	}
}
