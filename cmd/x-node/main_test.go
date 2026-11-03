package main

import "testing"

func TestParseYggPeersTable(t *testing.T) {
	out := `┌──────────────────────────────┬───────┬─────┐
│             URI              │ State │ Dir │
├──────────────────────────────┼───────┼─────┤
│ tls://box.example:13338      │ Down  │ Out │
│ tls://yggno.example:18227    │ Up    │ Out │
│ tls://dc.example:1337        │ Up    │ Out │
└──────────────────────────────┴───────┴─────┘`
	if got := parseYggPeers(out); got != 2 {
		t.Fatalf("parseYggPeers = %d, want 2", got)
	}
}

func TestParseYggPeersLegacy(t *testing.T) {
	out := "tls://a.example:1 true outbound\n" +
		"tls://b.example:2 false outbound\n" +
		"tls://c.example:3 true inbound\n"
	if got := parseYggPeers(out); got != 1 {
		t.Fatalf("parseYggPeers = %d, want 1", got)
	}
}
