package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

// Mutations must be JSON content-type (a preflighted type our zero-CORS
// policy never allows) and loopback-originated: a browser page anywhere
// can fire simple cross-site POSTs (text/plain), and /api/send executes
// shell commands.
func TestCSRFWriteGuards(t *testing.T) {
	session, _, base := newTestServer(t, [][]model.Event{{model.TextDeltaEvent{Text: "x"}}})

	// Simple cross-site POST: text/plain, foreign origin.
	req, _ := http.NewRequest("POST", base.URL+api(session, "/send"), strings.NewReader(`{"text":"pwned"}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", "http://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin simple POST: status %d, want 403", resp.StatusCode)
	}

	// JSON but foreign origin: still refused.
	req2, _ := http.NewRequest("POST", base.URL+api(session, "/send"), strings.NewReader(`{"text":"pwned"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Origin", "http://evil.example")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin JSON POST: status %d, want 403", resp2.StatusCode)
	}

	// JSON but no content-type: refused (the CSRF-relevant part).
	req3, _ := http.NewRequest("POST", base.URL+api(session, "/send"), strings.NewReader(`{"text":"x"}`))
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("no content-type POST: status %d, want 415", resp3.StatusCode)
	}

	// Same-origin JSON goes through.
	if err := postJSON(t, base.URL, api(session, "/send"), map[string]string{"text": "fine"}); err != nil {
		t.Fatalf("same-origin JSON POST failed: %v", err)
	}
}

// GET never mutates.
func TestCSRFNoGetMutations(t *testing.T) {
	_, server, _ := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/s/some/session/send", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Fatalf("GET send route: %d, want 405/404 (route not registered for GET)", rec.Code)
	}
}
