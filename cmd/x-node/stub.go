package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// The landing page of the host. The installer renders the file with a random
// palette and fonts; the daemon fills the two dynamic fields on every request.
// A page with a frozen timestamp looks like a mock-up, and a script that moves
// its counters looks like a fake, so the values come from the server.
const (
	stubPagePath = "/var/www/x-node/index.html"
	stubListen   = "127.0.0.1:18084"
)

type stubServer struct {
	path string
	log  *logger
}

func newStubServer(log *logger) *stubServer {
	return &stubServer{path: stubPagePath, log: log}
}

func (s *stubServer) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(rw, r)
		return
	}
	page, err := os.ReadFile(s.path)
	if err != nil {
		page = []byte(stubFallbackPage)
	}
	requestID := newRequestID()
	body := strings.NewReplacer(
		"{{timestamp}}", time.Now().UTC().Format(time.RFC3339),
		"{{request_id}}", requestID,
	).Replace(string(page))
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set("X-Request-Id", requestID)
	rw.WriteHeader(http.StatusUnauthorized)
	_, _ = io.WriteString(rw, body)
}

// listenAndServe serves the landing page until the context is cancelled. The
// page is decoration: a busy port or a broken socket must not stop the daemon,
// so a failure is logged and the goroutine waits for the shutdown.
func (s *stubServer) listenAndServe(ctx context.Context) error {
	listener, err := net.Listen("tcp", stubListen)
	if err != nil {
		s.log.warn("landing page listener on %s: %v", stubListen, err)
		<-ctx.Done()
		return nil
	}
	server := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	s.log.info("landing page listening on %s", stubListen)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.log.warn("landing page: %v", err)
	}
	return nil
}

func newRequestID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(raw)
}

// stubFallbackPage is served when the rendered file is missing, for example in
// a local "go run" session. It carries the same two placeholders as the file.
const stubFallbackPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>401 Unauthorized</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  html, body { height: 100%; }
  body {
    margin: 0; padding: 32px; display: flex; align-items: center; justify-content: center;
    background: #0d1117; color: #e6edf3;
    font: 14px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif;
  }
  .card {
    width: min(620px, 100%); background: #151b23; border: 1px solid #232c38;
    border-radius: 16px; overflow: hidden;
  }
  .bar {
    display: flex; align-items: center; gap: 10px; padding: 14px 18px;
    border-bottom: 1px solid #232c38;
    font: 12.5px/1 ui-monospace, Menlo, Consolas, monospace; color: #8b949e;
  }
  .badge {
    margin-left: auto; padding: 5px 10px; border-radius: 999px;
    color: #58a6ff; background: rgba(88,166,255,.14); border: 1px solid rgba(88,166,255,.38);
    font: 600 11px/1 ui-monospace, Menlo, Consolas, monospace;
  }
  .body { padding: 26px 22px 24px; }
  h1 { margin: 0; font-size: 16px; font-weight: 600; }
  p { margin: 6px 0 18px; font-size: 13px; color: #8b949e; }
  pre {
    margin: 0; padding: 18px; background: rgba(0,0,0,.22); border: 1px solid #232c38;
    border-radius: 11px; font: 13px/1.75 ui-monospace, Menlo, Consolas, monospace;
    overflow-x: auto;
  }
  .k, .p { color: #8b949e; }
  .v { color: #58a6ff; }
  .s { color: #3fb950; }
</style>
</head>
<body>
<main class="card">
  <div class="bar"><span>api</span><span class="badge">HTTP 401</span></div>
  <div class="body">
    <h1>401 Unauthorized</h1>
    <p>The request did not carry valid credentials.</p>
    <pre>{
  <span class="k">"status"</span><span class="p">:</span>     <span class="v">"UNAUTHORIZED"</span>,
  <span class="k">"timestamp"</span><span class="p">:</span>  <span class="s">"{{timestamp}}"</span>,
  <span class="k">"request_id"</span><span class="p">:</span> <span class="s">"{{request_id}}"</span>
}</pre>
  </div>
</main>
</body>
</html>
`
