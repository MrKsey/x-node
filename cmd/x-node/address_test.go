package main

import (
	"encoding/hex"
	"net"
	"testing"
)

func TestAddrForKeyFixedVector(t *testing.T) {
	pub, err := hex.DecodeString("3b6a27bcceb6a42d62a3a8d02a6f0d73653215771de243a63ac048a18b59da29")
	if err != nil {
		t.Fatal(err)
	}
	ip, err := addrForKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	const want = "202:24ae:c219:8a4a:de94:eae2:b97e:ac87"
	if ip.String() != want {
		t.Fatalf("address = %s, want %s", ip, want)
	}
	if !isYggAddress(ip) {
		t.Fatal("derived address is not recognised as a Yggdrasil node address")
	}
	if isYggAddress(net.ParseIP("2001:db8::1")) {
		t.Fatal("a global IPv6 address was accepted as Yggdrasil")
	}
}
