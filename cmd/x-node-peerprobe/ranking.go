package main

import (
	"sort"
	"strings"
)

// rankCatalog puts the peers of the home country first and then orders them by
// the distance from the detected location. The country is only a prior: the
// answer of a search is the measured Yggdrasil time, not the country.
func rankCatalog(peers []candidate, home string, lat, lon float64) []candidate {
	out := append([]candidate(nil), peers...)
	distance := func(peer candidate) float64 {
		item, ok := countryBySlug(peer.Country)
		if !ok {
			return 20000
		}
		return distanceKm(lat, lon, item.Lat, item.Lon)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if home != "" {
			left := out[i].Country == home
			right := out[j].Country == home
			if left != right {
				return left
			}
		}
		delta := distance(out[i]) - distance(out[j])
		if delta > 1 {
			return false
		}
		if delta < -1 {
			return true
		}
		if out[i].Country != out[j].Country {
			return out[i].Country < out[j].Country
		}
		if out[i].City != out[j].City {
			return out[i].City < out[j].City
		}
		return out[i].URI < out[j].URI
	})
	return out
}

// discoveryTargets picks the servers of one search round: the nodes of the home
// country first and the closest countries next, one URI per server. The country
// is only a prior, not the answer: the nearest peers may sit across the border,
// and a large country has servers on the other side of the continent, so a
// quarter of the window is always kept for the closest foreign countries. A
// server whose country has no peers at all gets the closest countries instead.
func discoveryTargets(peers []candidate, home string, limit int) []candidate {
	if limit < 1 || len(peers) == 0 {
		return nil
	}
	out := make([]candidate, 0, limit)
	seen := make(map[string]bool, len(peers))
	homeBudget := limit
	if home != "" {
		homeBudget = limit - limit/4
		if homeBudget < 1 {
			homeBudget = 1
		}
		for _, peer := range peers {
			if peer.Country != home {
				continue
			}
			host := strings.ToLower(strings.TrimSpace(peer.Host))
			if host == "" || seen[host] {
				continue
			}
			seen[host] = true
			out = append(out, peer)
			if len(out) >= homeBudget {
				break
			}
		}
	}
	// The rest of the window goes to the closest countries in the ranked
	// order, so the round can compare a local server with a faster one on the
	// other side of the border.
	for _, peer := range peers {
		if len(out) >= limit {
			break
		}
		if home != "" && peer.Country == home {
			continue
		}
		host := strings.ToLower(strings.TrimSpace(peer.Host))
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, peer)
	}
	return out
}
