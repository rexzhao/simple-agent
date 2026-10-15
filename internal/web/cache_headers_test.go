package web

// The static trees' cache policy (the "rebuilt dist shows the old UI"
// fix): the HTML entries and every un-hashed file revalidate on each
// load (no-cache), and only the content-hashed assets/ files are
// immutable — both /chat (the external build) and /log (the embedded
// view) go through the same wrapper.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// newChatStaticServer builds a manager-less server over a fake dist
// tree: an index.html, one hashed asset, one un-hashed file.
func newChatStaticServer(t *testing.T) *httptest.Server {
	t.Helper()
	dist := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dist, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		if err := os.WriteFile(filepath.Join(dist, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "<html>chat</html>")
	write("assets/index-9uxWyZ9T.js", "console.log('new build')")
	write("favicon.svg", "<svg/>")
	// The route-level fixture rebuilds the HTTP layer over a real
	// manager with the static option.
	_, _, ts := newTestManagerServerOpts(t, nil, WithStaticDir(dist))
	return ts
}

func TestChatStaticCacheHeaders(t *testing.T) {
	ts := newChatStaticServer(t)
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/chat/", "no-cache"},
		{"/chat/index.html", "no-cache"},
		{"/chat/assets/index-9uxWyZ9T.js", "public, max-age=31536000, immutable"},
		{"/chat/favicon.svg", "no-cache"},
	} {
		resp, err := http.Get(ts.URL + tc.path) //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.path, resp.StatusCode)
		}
		if got := resp.Header.Get("Cache-Control"); got != tc.want {
			t.Fatalf("%s: Cache-Control = %q, want %q", tc.path, got, tc.want)
		}
	}
	// The bare /chat redirects to /chat/ and the redirect itself must
	// not be heuristic-cached either.
	resp, err := http.Get(ts.URL + "/chat") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("/chat redirect: Cache-Control = %q, want no-cache", got)
	}
}

func TestLogCacheHeaders(t *testing.T) {
	// The embedded /log tree has NO hashed names — everything
	// revalidates, nothing is immutable (the same file names ride
	// across binary versions).
	_, _, base := newTestServer(t, nil)
	for _, path := range []string{"/log/", "/log/index.html", "/log/app.js", "/log/reducer.mjs"} {
		resp, err := http.Get(base.URL + path) //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", path, resp.StatusCode)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("%s: Cache-Control = %q, want no-cache", path, got)
		}
	}
}

// TestLandingCacheHeaders: the no-dist landing (the mount-point
// instructions) is an HTML entry too — it must revalidate like one.
func TestLandingCacheHeaders(t *testing.T) {
	// A manager-less server with NO WithStaticDir: /chat serves the
	// landing. Only the static routes are exercised, so the manager
	// is never touched (and Close is not called on it).
	server := New(nil)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	for _, path := range []string{"/chat", "/chat/"} {
		resp, err := http.Get(ts.URL + path) //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", path, resp.StatusCode)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("%s (landing): Cache-Control = %q, want no-cache", path, got)
		}
	}
}
