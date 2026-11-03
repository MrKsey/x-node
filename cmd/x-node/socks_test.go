package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestReadRequestIPv6Connect(t *testing.T) {
	raw := []byte{socksVersion, socksCmdConnect, 0, socksAtypIPv6}
	raw = append(raw, net.ParseIP("2606:4700:4700::1111").To16()...)
	raw = binary.BigEndian.AppendUint16(raw, 443)

	request, err := readRequest(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("readRequest: %v", err)
	}
	if request.cmd != socksCmdConnect || request.atyp != socksAtypIPv6 {
		t.Fatalf("request = %+v", request)
	}
	if !request.ip.Equal(net.ParseIP("2606:4700:4700::1111")) || request.port != 443 {
		t.Fatalf("destination = %s:%d", request.ip, request.port)
	}
}

func TestWriteReplyIPv6(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		_ = writeReply(server, socksReplySuccess, net.ParseIP("200::1"), 51234)
		_ = server.Close()
	}()

	reply := make([]byte, 22)
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if reply[0] != socksVersion || reply[1] != socksReplySuccess || reply[3] != socksAtypIPv6 {
		t.Fatalf("unexpected reply header %v", reply[:4])
	}
	if !net.IP(reply[4:20]).Equal(net.ParseIP("200::1")) {
		t.Fatalf("bound address = %s", net.IP(reply[4:20]))
	}
	if port := binary.BigEndian.Uint16(reply[20:22]); port != 51234 {
		t.Fatalf("bound port = %d", port)
	}
}

// TestServeConnectIPv6Loopback carries a real stream to an IPv6 loopback server.
// The destination policy is replaced because production refuses loopback; every
// other part of the path — request parsing, dial, reply and relay — is the one a
// client uses.
func TestServeConnectIPv6Loopback(t *testing.T) {
	echo, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable: %v", err)
	}
	defer echo.Close()
	go func() {
		conn, err := echo.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	cfg := testConfig(t)
	cfg.AllowAll = true
	store, err := openStore(cfg, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	server := newSocksServer(cfg, store, nil, newLogger("error"), nil)
	server.sourceOK = func(net.IP) bool { return true }
	server.policy = func(net.IP, int) bool { return true }

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.serve(ctx, listener) }()

	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := client.Write([]byte{socksVersion, 1, 0}); err != nil {
		t.Fatal(err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(client, method); err != nil {
		t.Fatal(err)
	}
	if method[0] != socksVersion || method[1] != 0 {
		t.Fatalf("method reply = %v", method)
	}

	port := echo.Addr().(*net.TCPAddr).Port
	request := []byte{socksVersion, socksCmdConnect, 0, socksAtypIPv6}
	request = append(request, net.IPv6loopback.To16()...)
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if _, err := client.Write(request); err != nil {
		t.Fatal(err)
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(client, header); err != nil {
		t.Fatal(err)
	}
	if header[1] != socksReplySuccess {
		t.Fatalf("connect refused with reply %d", header[1])
	}
	switch header[3] {
	case socksAtypIPv4:
		_, err = io.ReadFull(client, make([]byte, 6))
	case socksAtypIPv6:
		_, err = io.ReadFull(client, make([]byte, 18))
	default:
		t.Fatalf("unexpected address type %d", header[3])
	}
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Write([]byte("hello over IPv6")); err != nil {
		t.Fatal(err)
	}
	echoed := make([]byte, len("hello over IPv6"))
	if _, err := io.ReadFull(client, echoed); err != nil {
		t.Fatal(err)
	}
	if string(echoed) != "hello over IPv6" {
		t.Fatalf("echo = %q", echoed)
	}
}
