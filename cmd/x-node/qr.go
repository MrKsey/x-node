package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// exitLink is the payload of the QR code. Every value is URL-encoded, so the
// IPv6 address keeps its colons inside the query string.
func exitLink(cfg Config, invite string) string {
	values := url.Values{}
	values.Set("v", "1")
	values.Set("a", cfg.ListenAddress)
	values.Set("p", strconv.Itoa(cfg.SocksPort))
	// The client always connects to the port the API is actually on. With a
	// shared listener that is the SOCKS5 port, and the path still selects the
	// HTTP API.
	values.Set("api", strconv.Itoa(cfg.apiPort()))
	values.Set("n", cfg.Name)
	if strings.TrimSpace(cfg.PeerURI) != "" {
		values.Set("peer", strings.TrimSpace(cfg.PeerURI))
	}
	if strings.TrimSpace(invite) != "" {
		values.Set("i", invite)
	}
	return "yggnode://exit?" + values.Encode()
}

// renderQR draws the payload with ANSI colours. Two module rows share one text
// row through the upper/lower half-block characters, so the code is twice as
// narrow and twice as short as a plain two-spaces-per-module bitmap. The quiet
// zone is part of the bitmap.
func renderQR(payload string) (string, error) {
	code, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		return "", err
	}
	code.DisableBorder = false
	bitmap := code.Bitmap()
	var out strings.Builder
	for y := 0; y < len(bitmap); y += 2 {
		top := bitmap[y]
		var bottom []bool
		if y+1 < len(bitmap) {
			bottom = bitmap[y+1]
		}
		for x, upper := range top {
			lower := x < len(bottom) && bottom[x]
			switch {
			case upper && lower:
				out.WriteString("\x1b[40m ")
			case !upper && !lower:
				out.WriteString("\x1b[47m ")
			case upper:
				out.WriteString("\x1b[30;47m▀")
			default:
				out.WriteString("\x1b[30;47m▄")
			}
		}
		out.WriteString("\x1b[0m\n")
	}
	return out.String(), nil
}

// printInvite prints the QR, the link and the metadata of a fresh invite.
func printInvite(cfg Config, token string, invite Invite) {
	link := exitLink(cfg, token)
	if art, err := renderQR(link); err == nil {
		fmt.Print(art)
	}
	fmt.Println()
	fmt.Println("Exit node:", cfg.Name)
	fmt.Println("Address:  ", cfg.ListenAddress)
	fmt.Println("SOCKS5:   ", strconv.Itoa(cfg.SocksPort))
	if cfg.apiShared() {
		fmt.Println("API:       shared with SOCKS5 (HTTP /v1/... on the same port)")
	} else {
		fmt.Println("API:      ", strconv.Itoa(cfg.APIPort))
	}
	if strings.TrimSpace(cfg.PeerURI) != "" {
		fmt.Println("Peer:     ", cfg.PeerURI)
	}
	fmt.Println("Invite:   ", token)
	fmt.Println("Expires:  ", invite.ExpiresAt.Format(time.RFC3339), "("+time.Until(invite.ExpiresAt).Round(time.Minute).String()+")")
	fmt.Println("Uses:     ", fmt.Sprintf("%d", invite.Uses))
	if invite.Note != "" {
		fmt.Println("Note:     ", invite.Note)
	}
	fmt.Println()
	fmt.Println("Link:")
	fmt.Println(link)
	fmt.Println()
	fmt.Println("Scan the QR in Y-Node → Exit node → Add, or open the link on the device.")
	fmt.Println("The client must be connected to Yggdrasil: registration goes over the tunnel.")
}
