package main

import (
	"net"
	"testing"
	"time"
)

func TestConnectionRegistryClosesOneClient(t *testing.T) {
	registry := newConnectionRegistry()
	first := testAddr(t, 31)
	second := testAddr(t, 32)

	firstLocal, firstRemote := net.Pipe()
	defer firstRemote.Close()
	secondLocal, secondRemote := net.Pipe()
	defer secondRemote.Close()

	releaseFirst := registry.track(first, firstLocal)
	defer releaseFirst()
	releaseSecond := registry.track(second, secondLocal)
	defer releaseSecond()

	if got := len(registry.addresses()); got != 2 {
		t.Fatalf("addresses = %d, want 2", got)
	}
	if closed := registry.closeClient(first); closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	_ = firstRemote.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := firstRemote.Read(make([]byte, 1)); err == nil {
		t.Fatal("the closed client socket is still open")
	}
	// The other client must not be affected.
	_ = secondRemote.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := secondRemote.Read(make([]byte, 1)); err == nil {
		t.Fatal("a connection of another client was closed")
	}
	if got := len(registry.addresses()); got != 1 {
		t.Fatalf("addresses after close = %d, want 1", got)
	}
}

func TestConnectionRegistryReleaseIsIdempotent(t *testing.T) {
	registry := newConnectionRegistry()
	local, remote := net.Pipe()
	defer remote.Close()
	release := registry.track(testAddr(t, 33), local)
	release()
	release()
	if got := len(registry.addresses()); got != 0 {
		t.Fatalf("addresses = %d, want 0", got)
	}
}
