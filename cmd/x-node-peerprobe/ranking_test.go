package main

import "testing"

// A country with more servers than the limit keeps its first ones, and the
// neighbours never push a home server out of the round.
func TestDiscoveryTargetsCoversEveryHomeServer(t *testing.T) {
	peers := []candidate{
		{URI: "tls://msk-1.example:1", Host: "msk-1.example", Country: "russia", Scheme: "tls"},
		{URI: "tcp://msk-1.example:2", Host: "msk-1.example", Country: "russia", Scheme: "tcp"},
		{URI: "tls://msk-2.example:1", Host: "msk-2.example", Country: "russia", Scheme: "tls"},
		{URI: "tls://spb-1.example:1", Host: "spb-1.example", Country: "russia", Scheme: "tls"},
		{URI: "tls://fi-1.example:1", Host: "fi-1.example", Country: "finland", Scheme: "tls"},
		{URI: "tls://am-1.example:1", Host: "am-1.example", Country: "armenia", Scheme: "tls"},
	}
	got := discoveryTargets(peers, "russia", 4)
	if len(got) != 4 {
		t.Fatalf("targets = %d, want the limit 4: %v", len(got), got)
	}
	hosts := map[string]bool{}
	for _, peer := range got {
		hosts[peer.Host] = true
	}
	for _, host := range []string{"msk-1.example", "msk-2.example", "spb-1.example"} {
		if !hosts[host] {
			t.Fatalf("the home server %s is missing from %v", host, got)
		}
	}
}

func TestDiscoveryTargetsFallsBackToTheClosestCountries(t *testing.T) {
	peers := []candidate{
		{URI: "tls://fi-1.example:1", Host: "fi-1.example", Country: "finland", Scheme: "tls"},
		{URI: "tls://de-1.example:1", Host: "de-1.example", Country: "germany", Scheme: "tls"},
	}
	got := discoveryTargets(peers, "russia", 10)
	if len(got) != 2 {
		t.Fatalf("targets = %v, want both neighbours when the home country is empty", got)
	}
}

// A country with more servers than the round can test still leaves part of the
// window to the closest foreign countries: the nearest peers may sit across the
// border, so the round must be able to compare them with the local ones.
func TestDiscoveryTargetsKeepsRoomForTheClosestCountries(t *testing.T) {
	var peers []candidate
	for _, host := range []string{"ru-1", "ru-2", "ru-3", "ru-4", "ru-5", "ru-6", "ru-7", "ru-8"} {
		peers = append(peers, candidate{
			URI:     "tls://" + host + ".example:1",
			Host:    host + ".example",
			Country: "russia",
			Scheme:  "tls",
		})
	}
	peers = append(peers,
		candidate{URI: "tls://fi-1.example:1", Host: "fi-1.example", Country: "finland", Scheme: "tls"},
		candidate{URI: "tls://de-1.example:1", Host: "de-1.example", Country: "germany", Scheme: "tls"},
	)
	got := discoveryTargets(peers, "russia", 8)
	if len(got) != 8 {
		t.Fatalf("targets = %d, want the limit 8: %v", len(got), got)
	}
	hosts := map[string]bool{}
	for _, peer := range got {
		hosts[peer.Host] = true
	}
	if !hosts["fi-1.example"] || !hosts["de-1.example"] {
		t.Fatalf("the closest countries are missing from %v", got)
	}
	if !hosts["ru-1.example"] {
		t.Fatalf("the home servers are missing from %v", got)
	}
}

func TestDiscoveryTargetsRespectsTheLimit(t *testing.T) {
	var peers []candidate
	for _, host := range []string{"a", "b", "c", "d", "e"} {
		peers = append(peers, candidate{
			URI:     "tls://" + host + ".example:1",
			Host:    host + ".example",
			Country: "russia",
			Scheme:  "tls",
		})
	}
	if got := discoveryTargets(peers, "russia", 3); len(got) != 3 {
		t.Fatalf("targets = %v, want 3", got)
	}
}

// The catalogue is ranked around the detected location: the home country first,
// then the closest countries. A large country must not beat a neighbouring one
// that is physically nearer to the server.
func TestRankCatalogPutsTheClosestCountriesFirst(t *testing.T) {
	peers := []candidate{
		{URI: "tls://ru-far.example:1", Host: "ru-far.example", Country: "russia", Scheme: "tls"},
		{URI: "tls://fi-1.example:1", Host: "fi-1.example", Country: "finland", Scheme: "tls"},
		{URI: "tls://de-1.example:1", Host: "de-1.example", Country: "germany", Scheme: "tls"},
		{URI: "tls://ru-home.example:1", Host: "ru-home.example", Country: "russia", Scheme: "tls"},
	}
	// Helsinki: Finland is next door, Germany is farther, Russia is on the
	// other side of the border and must still come first as the home country.
	got := rankCatalog(peers, "russia", 60.17, 24.94)
	if len(got) != 4 {
		t.Fatalf("ranked = %v", got)
	}
	if got[0].Country != "russia" || got[1].Country != "russia" {
		t.Fatalf("the home country must come first: %v", got)
	}
	if got[2].Country != "finland" || got[3].Country != "germany" {
		t.Fatalf("the closest countries must follow the home one: %v", got)
	}
}

func TestCountryBySlugOrCode(t *testing.T) {
	for _, value := range []string{"united-states", "USA", "us"} {
		item, ok := countryBySlugOrCode(value)
		if !ok || item.Slug != "united-states" {
			t.Fatalf("countryBySlugOrCode(%q) = %v, %v", value, item, ok)
		}
	}
	if _, ok := countryBySlugOrCode("not-a-country"); ok {
		t.Fatal("an unknown country must not resolve")
	}
}
