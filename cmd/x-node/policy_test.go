package main

import (
	"net"
	"testing"
)

func TestDestinationAllowed(t *testing.T) {
	tests := []struct {
		address string
		port    int
		want    bool
	}{
		{"1.1.1.1", 443, true},
		{"8.8.8.8", 53, true},
		{"93.184.216.34", 80, true},
		{"10.0.0.1", 80, false},
		{"172.16.5.4", 80, false},
		{"192.168.1.1", 80, false},
		{"100.64.0.1", 80, false},
		{"127.0.0.1", 80, false},
		{"169.254.1.1", 80, false},
		{"0.0.0.0", 80, false},
		{"224.0.0.1", 80, false},
		{"255.255.255.255", 80, false},
		{"1.1.1.1", 0, false},
		{"1.1.1.1", 70000, false},
		{"2001:4860:4860::8888", 443, true},
		{"2606:4700:4700::1111", 443, true},
		{"2a01:ecc0:5000:329::2", 443, true},
		{"::ffff:1.1.1.1", 443, true},
		{"::ffff:10.0.0.1", 443, false},
		{"200::1", 443, false},
		{"fc00::1", 443, false},
		{"fd00::1", 443, false},
		{"fe80::1", 443, false},
		{"::1", 443, false},
		{"::", 443, false},
		{"ff02::1", 443, false},
		{"100::1", 443, false},
		{"64:ff9b::1.1.1.1", 443, false},
		{"2001:db8::1", 443, false},
		{"2001:2::1", 443, false},
		{"2001:10::1", 443, false},
		{"2001:20::1", 443, false},
		{"3fff::1", 443, false},
	}
	for _, test := range tests {
		ip := net.ParseIP(test.address)
		if ip == nil {
			t.Fatalf("bad test address %q", test.address)
		}
		if got := destinationAllowed(ip, test.port); got != test.want {
			t.Errorf("destinationAllowed(%s, %d) = %v, want %v", test.address, test.port, got, test.want)
		}
	}
}

func TestPortBlocked(t *testing.T) {
	if !portBlocked(25, []int{25}) {
		t.Fatal("port 25 should be blocked")
	}
	if portBlocked(443, []int{25}) {
		t.Fatal("port 443 should not be blocked")
	}
}
