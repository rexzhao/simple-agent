package web

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTopLevelRoutes pins the landing contract: / redirects to the chat
// client (the default), /chat explains the mount when no client is
// served, /log is the embedded zero-dependency view.
// noRedirect returns a client that surfaces redirect responses
// instead of following them, for asserting the 302 contracts.
func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// TestTopLevelRoutes pins the landing contract: / redirects to the chat
// client (the default), /chat explains the mount when no client is
// served, /log is the embedded zero-dependency view.
func TestTopLevelRoutes(t *testing.T) {
	_, _, base := newTestServer(t, nil)

	res, err := noRedirect().Get(base.URL + "/") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/chat" {
		t.Fatalf("GET /: status %d location %q, want 302 /chat", res.StatusCode, res.Header.Get("Location"))
	}

	res, err = http.DefaultClient.Get(base.URL + "/chat") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /chat: status %d, want 200 (landing)", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET /chat: content-type %q", ct)
	}

	// The landing must point the way out: the log view link.
	res, err = http.DefaultClient.Get(base.URL + "/chat") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	buf, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf), `href="/log"`) {
		t.Fatal("landing: no /log link")
	}
}

// TestStaticChatDir pins the mounted-client contract: /chat serves the
// directory's index, assets resolve under /chat/, and the MIME pins
// hold for the built files too.
func TestStaticChatDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "assets")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><title>chat</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, base := newTestManagerServerOpts(t, nil, WithStaticDir(dir))

	res, err := http.DefaultClient.Get(base.URL + "/chat") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /chat: status %d, want 200 (index)", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET /chat: content-type %q", ct)
	}

	res, err = http.DefaultClient.Get(base.URL + "/chat/assets/app.js") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /chat/assets/app.js: status %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("GET /chat/assets/app.js: content-type %q", ct)
	}

	// / still lands on the client.
	res, err = noRedirect().Get(base.URL + "/") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/chat" {
		t.Fatalf("GET /: status %d location %q, want 302 /chat", res.StatusCode, res.Header.Get("Location"))
	}
}

// TestCORSPreflight pins the dev-server contract: a loopback http origin
// gets preflight answers on /api paths, a foreign origin gets nothing
// (its preflight falls through to the redirect, which the browser treats
// as CORS failure).
func TestCORSPreflight(t *testing.T) {
	_, _, base := newTestServer(t, nil)

	req, _ := http.NewRequest(http.MethodOptions, base.URL+"/api/s/x/send", nil) //nolint:noctx // test
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight: status %d, want 204", res.StatusCode)
	}
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("preflight: ACAO %q, want origin echo", got)
	}
	if got := res.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") || !strings.Contains(got, "PUT") || !strings.Contains(got, "DELETE") {
		t.Fatalf("preflight: allow-methods %q", got)
	}
	if got := res.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Content-Type") {
		t.Fatalf("preflight: allow-headers %q", got)
	}

	// A foreign origin preflight: no CORS headers, and the fall-through
	// is the / redirect — a preflight that cannot succeed.
	req, _ = http.NewRequest(http.MethodOptions, base.URL+"/api/s/x/send", nil) //nolint:noctx // test
	req.Header.Set("Origin", "http://evil.example")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("foreign preflight: ACAO must be absent")
	}
}

// TestCORSEcho pins that actual requests from a loopback origin carry
// the echo so the browser can read the response (GET /api/projects
// works from a dev server page).
func TestCORSEcho(t *testing.T) {
	_, _, base := newTestServer(t, nil)

	req, _ := http.NewRequest(http.MethodGet, base.URL+"/api/projects", nil) //nolint:noctx // test
	req.Header.Set("Origin", "http://127.0.0.1:5173")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/projects: status %d", res.StatusCode)
	}
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:5173" {
		t.Fatalf("GET /api/projects: ACAO %q, want origin echo", got)
	}

	// Foreign origin: guardAPI rejects, and no echo either way.
	req, _ = http.NewRequest(http.MethodGet, base.URL+"/api/projects", nil) //nolint:noctx // test
	req.Header.Set("Origin", "http://evil.example")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: status %d, want 403", res.StatusCode)
	}
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("foreign origin: ACAO must be absent")
	}
}
