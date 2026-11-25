package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeStubPage(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.html")
	page := `<!DOCTYPE html><p>{{timestamp}} {{request_id}}</p>`
	if err := os.WriteFile(path, []byte(page), 0o644); err != nil {
		t.Fatalf("write the stub page: %v", err)
	}
	return path
}

// TestStubPageFillsFreshValues is the regression of "the timestamp never
// changes": the page used to be a static file, so every visit showed the value
// written at install time. The daemon must fill both fields per request.
func TestStubPageFillsFreshValues(t *testing.T) {
	server := &stubServer{path: writeStubPage(t), log: newLogger("error")}

	first := httptest.NewRecorder()
	server.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/", nil))
	if first.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", first.Code)
	}
	if got := first.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	requestID := first.Header().Get("X-Request-Id")
	if requestID == "" {
		t.Fatal("the response carries no X-Request-Id")
	}
	body := first.Body.String()
	if strings.Contains(body, "{{") {
		t.Fatalf("a placeholder survived the request: %q", body)
	}
	if !strings.Contains(body, requestID) {
		t.Fatalf("the body does not carry the request id %q: %q", requestID, body)
	}

	second := httptest.NewRecorder()
	server.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/", nil))
	if second.Header().Get("X-Request-Id") == requestID {
		t.Fatalf("two requests share the request id %q", requestID)
	}
	if first.Body.String() == second.Body.String() {
		t.Fatalf("two requests share the body %q", body)
	}
}

func TestStubPageServesOnlyTheRoot(t *testing.T) {
	server := &stubServer{path: writeStubPage(t), log: newLogger("error")}
	for _, path := range []string{"/", "/index.html"} {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", path, recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("/robots.txt: status = %d, want 404", recorder.Code)
	}
}

// TestStubPageFallsBackWithoutTheFile covers a local run, where the installer
// has not rendered the page into /var/www.
func TestStubPageFallsBackWithoutTheFile(t *testing.T) {
	server := &stubServer{
		path: filepath.Join(t.TempDir(), "missing.html"),
		log:  newLogger("error"),
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "{{") {
		t.Fatalf("a placeholder survived the fallback: %q", body)
	}
	if !strings.Contains(body, "UNAUTHORIZED") {
		t.Fatalf("the fallback page does not name the status: %q", body)
	}
}
