package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The dashboard chart needs more than the handful of samples a browser can
// collect: it must survive a page reload and show the last day. The daemon
// samples the traffic counters every few seconds and keeps them in memory;
// nothing is written to disk, so the history starts empty after a restart.

const (
	trafficSampleInterval = 5 * time.Second
	trafficRetention      = 24 * time.Hour
)

type trafficPoint struct {
	At time.Time `json:"at"`
	Rx uint64    `json:"rx"`
	Tx uint64    `json:"tx"`
}

type trafficHistory struct {
	usage *usageStore

	mu     sync.Mutex
	points []trafficPoint
	lastRx uint64
	lastTx uint64
	seeded bool
}

func newTrafficHistory(usage *usageStore) *trafficHistory {
	return &trafficHistory{usage: usage}
}

func (h *trafficHistory) run(ctx context.Context) {
	if h == nil {
		return
	}
	h.sample()
	ticker := time.NewTicker(trafficSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.sample()
		}
	}
}

func (h *trafficHistory) sample() {
	if h == nil || h.usage == nil {
		return
	}
	rx, tx := h.totals()
	now := time.Now()

	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.seeded {
		// The first sample is the baseline: the counters are cumulative, so
		// their absolute value is not a rate.
		h.lastRx, h.lastTx = rx, tx
		h.seeded = true
		h.points = append(h.points, trafficPoint{At: now})
		return
	}
	deltaRx := counterDelta(rx, h.lastRx)
	deltaTx := counterDelta(tx, h.lastTx)
	h.lastRx, h.lastTx = rx, tx
	h.points = append(h.points, trafficPoint{At: now, Rx: deltaRx, Tx: deltaTx})

	cutoff := now.Add(-trafficRetention)
	cut := 0
	for cut < len(h.points) && h.points[cut].At.Before(cutoff) {
		cut++
	}
	if cut > 0 {
		h.points = append([]trafficPoint(nil), h.points[cut:]...)
	}
}

// counterDelta handles a counter that went backwards: the usage file can be
// reset by "quota reset", and a negative difference would break the chart.
func counterDelta(current, previous uint64) uint64 {
	if current < previous {
		return 0
	}
	return current - previous
}

func (h *trafficHistory) totals() (rx, tx uint64) {
	for _, row := range h.usage.rows() {
		rx += row.Rx
		tx += row.Tx
	}
	return rx, tx
}

// snapshot aggregates the samples into buckets so the browser receives a few
// hundred points regardless of the selected range.
func (h *trafficHistory) snapshot(window time.Duration) []trafficPoint {
	if h == nil {
		return nil
	}
	bucket := trafficBucket(window)
	now := time.Now()
	start := now.Add(-window).Truncate(bucket)

	h.mu.Lock()
	samples := append([]trafficPoint(nil), h.points...)
	h.mu.Unlock()

	count := int(window/bucket) + 1
	if count < 1 {
		count = 1
	}
	if count > 600 {
		count = 600
	}
	points := make([]trafficPoint, count)
	for i := range points {
		points[i].At = start.Add(time.Duration(i) * bucket)
	}
	for _, sample := range samples {
		if sample.At.Before(start) {
			continue
		}
		index := int(sample.At.Sub(start) / bucket)
		if index < 0 || index >= len(points) {
			continue
		}
		points[index].Rx += sample.Rx
		points[index].Tx += sample.Tx
	}
	return points
}

func trafficBucket(window time.Duration) time.Duration {
	switch {
	case window <= 15*time.Minute:
		return trafficSampleInterval
	case window <= time.Hour:
		return 30 * time.Second
	case window <= 6*time.Hour:
		return 2 * time.Minute
	default:
		return 5 * time.Minute
	}
}

func parseTrafficRange(raw string) (time.Duration, bool) {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "", "1h":
		return time.Hour, true
	case "15m":
		return 15 * time.Minute, true
	case "6h":
		return 6 * time.Hour, true
	case "24h":
		return 24 * time.Hour, true
	}
	return 0, false
}

func (w *webServer) handleTraffic(rw http.ResponseWriter, r *http.Request) {
	window, ok := parseTrafficRange(r.URL.Query().Get("range"))
	if !ok {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_range", Message: "use 15m, 1h, 6h or 24h"})
		return
	}
	points := w.history.snapshot(window)
	writeJSON(rw, http.StatusOK, map[string]any{
		"ok":           true,
		"range":        strconv.FormatInt(int64(window/time.Second), 10) + "s",
		"interval":     strconv.FormatInt(int64(trafficBucket(window)/time.Second), 10) + "s",
		"points":       points,
		"sampledEvery": strconv.FormatInt(int64(trafficSampleInterval/time.Second), 10) + "s",
	})
}
