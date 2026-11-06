package main

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The System Info card on the dashboard needs more than the instant values the
// process can report: a 15 minute history survives a page reload and shows
// whether a spike was a moment or a trend. The sampler reads /proc, so it is
// Linux only; on another system the endpoint returns an empty history and the
// card says so.

const (
	systemSampleInterval = 5 * time.Second
	systemRetention      = 15 * time.Minute
)

type systemPoint struct {
	At  time.Time `json:"at"`
	CPU float64   `json:"cpu"`
	Mem float64   `json:"mem"`
}

type cpuTimes struct {
	idle  uint64
	total uint64
}

type systemMonitor struct {
	mu       sync.Mutex
	points   []systemPoint
	previous cpuTimes
	seeded   bool

	cpu       float64
	mem       float64
	memTotal  uint64
	memUsed   uint64
	available bool
}

func newSystemMonitor() *systemMonitor {
	return &systemMonitor{}
}

func (m *systemMonitor) run(ctx context.Context) {
	if m == nil {
		return
	}
	m.sample()
	ticker := time.NewTicker(systemSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.sample()
		}
	}
}

func (m *systemMonitor) sample() {
	if m == nil {
		return
	}
	now := time.Now()
	current, cpuOK := readCPUTimes()
	total, available, memOK := readMemory()

	m.mu.Lock()
	defer m.mu.Unlock()
	if memOK && total > 0 {
		m.memTotal = total
		m.memUsed = total - available
		m.mem = float64(m.memUsed) / float64(total) * 100
	}
	// /proc/stat exists only on Linux. Elsewhere the card reports that the
	// history is unavailable instead of waiting for samples that never come.
	m.available = cpuOK
	if !cpuOK {
		return
	}
	if !m.seeded {
		// The first sample is the baseline: the counters are cumulative, so
		// their absolute value is not a rate.
		m.previous = current
		m.seeded = true
		m.points = append(m.points, systemPoint{At: now, Mem: m.mem})
		return
	}
	deltaTotal := current.total - m.previous.total
	deltaIdle := current.idle - m.previous.idle
	m.previous = current
	if deltaTotal > 0 {
		m.cpu = float64(deltaTotal-deltaIdle) / float64(deltaTotal) * 100
		if m.cpu < 0 {
			m.cpu = 0
		}
		if m.cpu > 100 {
			m.cpu = 100
		}
	}
	m.points = append(m.points, systemPoint{At: now, CPU: m.cpu, Mem: m.mem})

	cutoff := now.Add(-systemRetention)
	cut := 0
	for cut < len(m.points) && m.points[cut].At.Before(cutoff) {
		cut++
	}
	if cut > 0 {
		m.points = append([]systemPoint(nil), m.points[cut:]...)
	}
}

// snapshot returns the last 15 minutes and the current values. The points are
// copied under the lock, so the JSON encoder never races the sampler.
func (m *systemMonitor) snapshot() ([]systemPoint, float64, float64, uint64, uint64, bool) {
	if m == nil {
		return nil, 0, 0, 0, 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	points := make([]systemPoint, len(m.points))
	copy(points, m.points)
	return points, m.cpu, m.mem, m.memUsed, m.memTotal, m.available
}

// readCPUTimes parses the first line of /proc/stat. The fields are user, nice,
// system, idle, iowait, irq, softirq and steal; guest time is already included
// in user and must not be counted twice.
func readCPUTimes() (cpuTimes, bool) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	line := strings.SplitN(string(raw), "\n", 2)[0]
	return parseCPUTimes(line)
}

func parseCPUTimes(line string) (cpuTimes, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuTimes{}, false
	}
	var out cpuTimes
	for index, field := range fields[1:] {
		if index >= 8 {
			break
		}
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuTimes{}, false
		}
		out.total += value
		if index == 3 || index == 4 {
			out.idle += value
		}
	}
	if out.total == 0 {
		return cpuTimes{}, false
	}
	return out, true
}

// readMemory returns MemTotal and MemAvailable in bytes.
func readMemory() (uint64, uint64, bool) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	return parseMeminfo(string(raw))
}

func parseMeminfo(text string) (uint64, uint64, bool) {
	var total, available uint64
	var haveTotal, haveAvailable bool
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total, haveTotal = value*1024, true
		case "MemAvailable:":
			available, haveAvailable = value*1024, true
		}
	}
	if !haveTotal || !haveAvailable || total == 0 {
		return 0, 0, false
	}
	return total, available, true
}

func (w *webServer) handleSystem(rw http.ResponseWriter, r *http.Request) {
	points, cpu, mem, used, total, available := w.system.snapshot()
	writeJSON(rw, http.StatusOK, map[string]any{
		"ok":        true,
		"available": available,
		"points":    points,
		"cpu":       cpu,
		"mem":       mem,
		"memUsed":   used,
		"memTotal":  total,
		"range":     "15m",
		"interval":  strconv.FormatInt(int64(systemSampleInterval/time.Second), 10) + "s",
	})
}
