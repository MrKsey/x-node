package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type logLevel int

const (
	levelDebug logLevel = iota
	levelInfo
	levelWarn
	levelError
)

// logRingSize is how many recent lines the web interface can show. The service
// still writes everything to stderr, where journald keeps it.
const logRingSize = 500

// logEntry is one line kept for the web interface.
type logEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

type logger struct {
	mu    sync.Mutex
	level logLevel
	ring  []logEntry
	next  int
}

func newLogger(level string) *logger {
	l := &logger{level: levelInfo}
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		l.level = levelDebug
	case "info", "":
		l.level = levelInfo
	case "warn", "warning":
		l.level = levelWarn
	case "error":
		l.level = levelError
	}
	return l
}

func (l *logger) setLevel(level string) {
	if l == nil {
		return
	}
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		l.mu.Lock()
		l.level = levelDebug
		l.mu.Unlock()
	case "info", "":
		l.mu.Lock()
		l.level = levelInfo
		l.mu.Unlock()
	case "warn", "warning":
		l.mu.Lock()
		l.level = levelWarn
		l.mu.Unlock()
	case "error":
		l.mu.Lock()
		l.level = levelError
		l.mu.Unlock()
	}
}

func (l *logger) log(level logLevel, format string, args ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	current := l.level
	l.mu.Unlock()
	if level < current {
		return
	}
	label := "INFO"
	switch level {
	case levelDebug:
		label = "DEBUG"
	case levelWarn:
		label = "WARN"
	case levelError:
		label = "ERROR"
	}
	message := fmt.Sprintf(format, args...)
	stamp := time.Now().Format("2006-01-02T15:04:05Z07:00")
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(os.Stderr, "%s %-5s %s\n", stamp, label, message)
	if l.ring == nil {
		l.ring = make([]logEntry, logRingSize)
	}
	l.ring[l.next] = logEntry{Time: stamp, Level: label, Message: message}
	l.next = (l.next + 1) % len(l.ring)
}

// recent returns the newest lines, oldest first.
func (l *logger) recent(limit int) []logEntry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > len(l.ring) {
		limit = len(l.ring)
	}
	out := make([]logEntry, 0, limit)
	start := l.next - limit
	if start < 0 {
		start += len(l.ring)
	}
	for i := 0; i < limit; i++ {
		entry := l.ring[(start+i)%len(l.ring)]
		if entry.Message == "" {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func (l *logger) debug(format string, args ...any) { l.log(levelDebug, format, args...) }
func (l *logger) info(format string, args ...any)  { l.log(levelInfo, format, args...) }
func (l *logger) warn(format string, args ...any)  { l.log(levelWarn, format, args...) }
func (l *logger) error(format string, args ...any) { l.log(levelError, format, args...) }
