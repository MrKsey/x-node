package main

import (
	"crypto/ed25519"
	"fmt"
	"net"
)

// yggNodePrefix is the first byte of every Yggdrasil node address. Subnet
// prefixes use 0x03, node addresses use 0x02.
const yggNodePrefix = 0x02

// addrForKey derives the Yggdrasil IPv6 address from an ed25519 public key.
//
// The derivation is the one used by yggdrasil-go: the key is inverted, the
// leading one bits and the first zero bit are dropped, and the rest is packed
// after the 0x02 prefix and the count of leading ones. Keeping it here lets
// the exit daemon verify a registration without linking the whole router.
func addrForKey(publicKey ed25519.PublicKey) (net.IP, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must be %d bytes", ed25519.PublicKeySize)
	}
	var buf [ed25519.PublicKeySize]byte
	copy(buf[:], publicKey)
	for i := range buf {
		buf[i] = ^buf[i]
	}

	var out [16]byte
	out[0] = yggNodePrefix

	var packed []byte
	var bits byte
	var nBits int
	done := false
	ones := byte(0)
	for i := 0; i < 8*len(buf); i++ {
		bit := (buf[i/8] & (0x80 >> byte(i%8))) >> byte(7-(i%8))
		if !done && bit != 0 {
			ones++
			continue
		}
		if !done {
			done = true
			continue
		}
		bits = (bits << 1) | bit
		nBits++
		if nBits == 8 {
			nBits = 0
			packed = append(packed, bits)
		}
	}
	out[1] = ones
	copy(out[2:], packed)
	return net.IP(out[:]), nil
}

// isYggAddress reports whether ip belongs to the Yggdrasil node range
// 200::/7 and is a node address (not a routed /64 prefix).
func isYggAddress(ip net.IP) bool {
	v6 := ip.To16()
	if v6 == nil || ip.To4() != nil {
		return false
	}
	return v6[0] == yggNodePrefix
}

// isYggRange reports whether ip is anywhere in 200::/7.
func isYggRange(ip net.IP) bool {
	v6 := ip.To16()
	if v6 == nil || ip.To4() != nil {
		return false
	}
	return v6[0]&0xFE == 0x02
}
