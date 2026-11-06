// Command x-node-peerprobe ranks the public Yggdrasil peers and prints the
// fastest ones that completed a real protocol handshake.
//
// It is used by the exit-node installer: the server needs three good outbound
// peers, and a plain TCP connect is not enough to tell a working Yggdrasil
// node from a web server on the same port.
package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yggdrasil-network/yggdrasil-go/src/config"
	"github.com/yggdrasil-network/yggdrasil-go/src/core"
)

const defaultPeersURL = "https://codeload.github.com/yggdrasil-network/public-peers/zip/refs/heads/master"

var (
	uriPattern = regexp.MustCompile("`((?:tcp|tls|quic|ws|wss)://[^`\\s]+)`")
	yggRange   = net.IPNet{IP: net.ParseIP("200::"), Mask: net.CIDRMask(7, 128)}

	schemeRank = map[string]int{"tls": 0, "tcp": 1, "wss": 2, "ws": 3, "quic": 4}
)

var seedPeers = []string{
	"tls://box.paulll.cc:13338",
	"tcp://yggno.de:18226",
	"quic://novosibirsk.mie.foundation:9952",
}

type candidate struct {
	URI     string
	Host    string
	Port    int
	Scheme  string
	Country string
	City    string
}

// probedPeer is one result line: the URI that completed a handshake, the
// Yggdrasil round-trip measured for it and the catalogue location, so a person
// can pick a peer by country instead of reading host names.
type probedPeer struct {
	URI       string  `json:"uri"`
	LatencyMs float64 `json:"latencyMs"`
	Country   string  `json:"country,omitempty"`
	City      string  `json:"city,omitempty"`
}

// progressReport is one "progress {json}" line on stderr. The daemon forwards
// these numbers to the web interface, so a search that takes minutes shows how
// many peers were checked instead of looking stuck.
type progressReport struct {
	Stage   string `json:"stage"`
	Total   int    `json:"total"`
	Pool    int    `json:"pool,omitempty"`
	Checked int    `json:"checked"`
	Alive   int    `json:"alive"`
}

var progressEnabled bool

func reportProgress(report progressReport) {
	if !progressEnabled {
		return
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return
	}
	fmt.Fprintln(os.Stderr, "progress "+string(raw))
}

type silentLogger struct{}

func (silentLogger) Printf(string, ...any) {}
func (silentLogger) Println(...any)        {}
func (silentLogger) Infof(string, ...any)  {}
func (silentLogger) Infoln(...any)         {}
func (silentLogger) Warnf(string, ...any)  {}
func (silentLogger) Warnln(...any)         {}
func (silentLogger) Errorf(string, ...any) {}
func (silentLogger) Errorln(...any)        {}
func (silentLogger) Debugf(string, ...any) {}
func (silentLogger) Debugln(...any)        {}
func (silentLogger) Traceln(...any)        {}

func main() {
	count := flag.Int("count", 3, "how many peers to print")
	timeout := flag.Duration("timeout", 8*time.Second, "timeout of one protocol probe batch")
	pool := flag.Int("pool", 160, "how many candidates get the protocol probe")
	batch := flag.Int("batch", 5, "peers probed at once")
	source := flag.String("source", defaultPeersURL, "public peer list archive")
	plain := flag.Bool("plain", false, "print one URI per line instead of JSON")
	progress := flag.Bool("progress", false, "print progress lines to stderr")
	flag.Parse()
	progressEnabled = *progress

	if *count < 1 {
		*count = 1
	}
	if *batch < 1 {
		*batch = 5
	}

	peers, err := loadPeers(*source)
	if err != nil || len(peers) == 0 {
		if err != nil {
			fmt.Fprintln(os.Stderr, "peer list:", err)
		}
		peers = seeds()
	}
	reportProgress(progressReport{Stage: "catalogue", Total: len(peers)})

	ranked := rankByTCP(peers, *pool)
	reportProgress(progressReport{Stage: "ranked", Total: len(peers), Pool: len(ranked)})
	alive, checked := probeUntil(ranked, *count, *batch, *timeout, len(peers))
	if len(alive) == 0 {
		// The network may block the probe itself; the seeds are still better
		// than an empty Peers list in the Yggdrasil config.
		for _, item := range seeds() {
			alive = append(alive, probedPeer{URI: item.URI})
			if len(alive) >= *count {
				break
			}
		}
	}
	if len(alive) > *count {
		alive = alive[:*count]
	}
	reportProgress(progressReport{
		Stage: "done", Total: len(peers), Pool: len(ranked),
		Checked: checked, Alive: len(alive),
	})

	if *plain {
		for _, peer := range alive {
			fmt.Println(peer.URI)
		}
		return
	}
	raw, _ := json.MarshalIndent(alive, "", "  ")
	fmt.Println(string(raw))
}

func seeds() []candidate {
	out := make([]candidate, 0, len(seedPeers))
	for _, uri := range seedPeers {
		if parsed, ok := parseURI(uri); ok {
			out = append(out, parsed)
		}
	}
	return out
}

func loadPeers(source string) ([]candidate, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	response, err := client.Get(source)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer list returned %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	var all []candidate
	seen := make(map[string]struct{})
	for _, file := range reader.File {
		if file.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(file.Name), ".md") {
			continue
		}
		name := strings.ReplaceAll(file.Name, "\\", "/")
		parts := strings.Split(name, "/")
		if len(parts) < 3 {
			continue
		}
		country := strings.TrimSuffix(strings.ToLower(parts[len(parts)-1]), ".md")
		if country == "" || country == "readme" {
			continue
		}
		content, err := readZipFile(file)
		if err != nil {
			continue
		}
		for _, parsed := range parseMarkdown(string(content), country) {
			if _, ok := seen[parsed.URI]; ok {
				continue
			}
			seen[parsed.URI] = struct{}{}
			all = append(all, parsed)
		}
	}
	return onePerHost(all), nil
}

func readZipFile(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(io.LimitReader(reader, 1<<20))
}

func parseMarkdown(text, country string) []candidate {
	var out []candidate
	city := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			title := strings.TrimSpace(strings.TrimLeft(line, "#"))
			if len(line)-len(strings.TrimLeft(line, "#")) >= 2 {
				city = cleanCity(title)
			}
			continue
		}
		for _, match := range uriPattern.FindAllStringSubmatch(line, -1) {
			parsed, ok := parseURI(match[1])
			if !ok {
				continue
			}
			parsed.Country = country
			parsed.City = city
			out = append(out, parsed)
		}
	}
	return out
}

func cleanCity(title string) string {
	for _, separator := range []string{",", " - ", " (", "/"} {
		if index := strings.Index(title, separator); index > 0 {
			title = title[:index]
		}
	}
	return strings.TrimSpace(title)
}

func parseURI(raw string) (candidate, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return candidate{}, false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if _, ok := schemeRank[scheme]; !ok {
		return candidate{}, false
	}
	host := parsed.Hostname()
	if host == "" || strings.ContainsAny(host, " \t") {
		return candidate{}, false
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port <= 0 || port > 65535 {
		return candidate{}, false
	}
	return candidate{URI: raw, Host: host, Port: port, Scheme: scheme}, true
}

func onePerHost(all []candidate) []candidate {
	best := make(map[string]candidate)
	order := make([]string, 0, len(all))
	for _, item := range all {
		existing, ok := best[item.Host]
		if !ok {
			best[item.Host] = item
			order = append(order, item.Host)
			continue
		}
		if schemeRank[item.Scheme] < schemeRank[existing.Scheme] {
			best[item.Host] = item
		}
	}
	out := make([]candidate, 0, len(order))
	for _, host := range order {
		out = append(out, best[host])
	}
	return out
}

func rankByTCP(peers []candidate, pool int) []candidate {
	type measured struct {
		candidate candidate
		rtt       float64
	}
	results := make([]measured, 0, len(peers))
	type job struct{ index int }
	jobs := make(chan job)
	done := make(chan measured, len(peers))
	workers := 32
	if len(peers) < workers {
		workers = len(peers)
	}
	for i := 0; i < workers; i++ {
		go func() {
			for j := range jobs {
				item := peers[j.index]
				rtt := 0.0
				if item.Scheme != "quic" {
					started := time.Now()
					conn, err := net.DialTimeout("tcp", net.JoinHostPort(item.Host, strconv.Itoa(item.Port)), 2500*time.Millisecond)
					if err == nil {
						rtt = float64(time.Since(started).Microseconds()) / 1000.0
						_ = conn.Close()
					}
				}
				done <- measured{candidate: item, rtt: rtt}
			}
		}()
	}
	go func() {
		for i := range peers {
			jobs <- job{index: i}
		}
		close(jobs)
	}()
	for range peers {
		results = append(results, <-done)
	}
	sort.SliceStable(results, func(i, j int) bool {
		a, b := results[i].rtt, results[j].rtt
		if a == 0 && b != 0 {
			return false
		}
		if b == 0 && a != 0 {
			return true
		}
		if a != b {
			return a < b
		}
		return results[i].candidate.URI < results[j].candidate.URI
	})
	if pool > 0 && len(results) > pool {
		results = results[:pool]
	}
	out := make([]candidate, 0, len(results))
	for _, item := range results {
		out = append(out, item.candidate)
	}
	return out
}

func probeUntil(peers []candidate, count, batch int, timeout time.Duration, total int) ([]probedPeer, int) {
	alive := make([]probedPeer, 0, count)
	byURI := make(map[string]candidate, len(peers))
	for _, item := range peers {
		byURI[item.URI] = item
	}
	checked := 0
	for start := 0; start < len(peers) && len(alive) < count; start += batch {
		end := start + batch
		if end > len(peers) {
			end = len(peers)
		}
		slice := peers[start:end]
		uris := make([]string, 0, len(slice))
		for _, item := range slice {
			uris = append(uris, item.URI)
		}
		results := probeBatch(uris, timeout)
		checked += len(uris)
		for _, uri := range uris {
			if latency := results[uri]; latency > 0 {
				entry := probedPeer{URI: uri, LatencyMs: latency}
				if item, ok := byURI[uri]; ok {
					entry.Country = item.Country
					entry.City = item.City
				}
				alive = append(alive, entry)
			}
		}
		reportProgress(progressReport{
			Stage: "probe", Total: total, Pool: len(peers), Checked: checked, Alive: len(alive),
		})
	}
	return alive, checked
}

func probeBatch(uris []string, timeout time.Duration) map[string]float64 {
	out := make(map[string]float64)
	if len(uris) == 0 {
		return out
	}
	nc := config.GenerateConfig()
	nc.IfName = "none"
	nc.Listen = []string{}
	nc.AdminListen = "none"
	nc.MulticastInterfaces = nil
	nc.Peers = nil

	node, err := core.New(nc.Certificate, silentLogger{}, core.PeerFilter(func(ip net.IP) bool {
		return !yggRange.Contains(ip)
	}))
	if err != nil {
		return out
	}
	defer node.Stop()

	attempted := make([]string, 0, len(uris))
	for _, raw := range uris {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			continue
		}
		if ip := net.ParseIP(parsed.Hostname()); ip != nil && yggRange.Contains(ip) {
			continue
		}
		if err := node.CallPeer(parsed, ""); err != nil {
			continue
		}
		attempted = append(attempted, raw)
	}

	deadline := time.Now().Add(timeout)
	for {
		for _, peer := range node.GetPeers() {
			if peer.Up && peer.Latency > 0 {
				out[peer.URI] = float64(peer.Latency.Microseconds()) / 1000.0
			}
		}
		complete := true
		for _, uri := range attempted {
			if _, ok := out[uri]; !ok {
				complete = false
				break
			}
		}
		if complete || time.Now().After(deadline) {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	return out
}
