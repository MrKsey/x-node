package main

import "testing"

func TestParseCPUTimes(t *testing.T) {
	// user nice system idle iowait irq softirq steal
	times, ok := parseCPUTimes("cpu  100 20 30 400 50 10 5 2 0 0")
	if !ok {
		t.Fatal("a valid /proc/stat line was rejected")
	}
	if times.total != 617 {
		t.Fatalf("total = %d, want 617", times.total)
	}
	// Idle is idle + iowait: the CPU is not busy while it waits for the disk.
	if times.idle != 450 {
		t.Fatalf("idle = %d, want 450", times.idle)
	}
}

func TestParseCPUTimesRejectsOtherLines(t *testing.T) {
	for _, line := range []string{"", "intr 1 2 3", "cpu", "cpu0 1 2 3 4 5"} {
		if _, ok := parseCPUTimes(line); ok {
			t.Fatalf("%q must not parse as the total CPU line", line)
		}
	}
}

func TestParseMeminfo(t *testing.T) {
	text := "MemTotal:        4000000 kB\nMemFree:          500000 kB\nMemAvailable:    2500000 kB\n"
	total, available, ok := parseMeminfo(text)
	if !ok {
		t.Fatal("a valid /proc/meminfo was rejected")
	}
	if total != 4000000*1024 {
		t.Fatalf("total = %d, want bytes", total)
	}
	if available != 2500000*1024 {
		t.Fatalf("available = %d, want bytes", available)
	}
}

func TestParseMeminfoNeedsBothFields(t *testing.T) {
	if _, _, ok := parseMeminfo("MemTotal: 1000 kB\n"); ok {
		t.Fatal("a meminfo without MemAvailable must not be used")
	}
}
