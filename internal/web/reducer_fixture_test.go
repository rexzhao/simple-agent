package web

import (
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

// TestPageReducerFixture runs the page's actual state machine (the
// reducer app.js consumes) against a live in-process server through the
// three lifecycle scenarios. Both page-state bugs of the review rounds
// (prefixAnchor overwritten on resume; the chain folded from genesis on
// a truncated window) lived exactly here — CI's blind spot until the
// chain state moved into a DOM-free module. Skips without node.
func TestPageReducerFixture(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; the page reducer check needs node >= 18")
	}

	// Enough records that a window of 50 truncates (more=true).
	turns := make([][]model.Event, 80)
	for i := range turns {
		turns[i] = []model.Event{model.TextDeltaEvent{Text: "answer"}}
	}
	session, server, base := newTestServer(t, turns)
	for i := 0; i < 80; i++ {
		if err := postJSON(t, base.URL, api(session, "/send"), map[string]string{"text": "m"}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		waitSettled(t, session)
	}
	// A second session in the same project for the switch-semantics
	// scenario (scenario 4 in the fixture).
	second := createSecondSession(t, server)

	script, err := filepath.Abs(filepath.Join("static", "reducer_check.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script, base.URL, session.ID(), second)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node reducer check failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("unexpected node output: %s", out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

// TestStaticMIMETypes pins what a real browser sees: module scripts
// enforce strict MIME checking, and on Windows the Go mime table comes
// from the registry (".mjs" -> text/plain), which makes the page fail
// to load outside the pinned types. Linux defaults are correct, so this
// also guards against someone removing the pin.
func TestStaticMIMETypes(t *testing.T) {
	_, _, base := newTestServer(t, nil)
	for _, c := range []struct{ path, want string }{
		{"/log/app.js", "text/javascript"},
		{"/log/chain.mjs", "text/javascript"},
		{"/log/reducer.mjs", "text/javascript"},
		{"/log/style.css", "text/css"},
		{"/log", "text/html"},
	} {
		res, err := http.DefaultClient.Get(base.URL + c.path) //nolint:noctx // test
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		res.Body.Close()
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, c.want) {
			t.Errorf("%s Content-Type = %q, want %s", c.path, ct, c.want)
		}
	}
}

// createSecondSession opens a second session under the same project as
// the test server's first one (the switch-semantics fixture scenario):
// the manager's own factory builds the agent, so the project id matches.
func createSecondSession(t *testing.T, s *Server) string {
	t.Helper()
	m := s.manager
	m.mu.Lock()
	entries := m.entriesLocked()
	m.mu.Unlock()
	if len(entries) == 0 {
		t.Fatal("no registered project for a second session")
	}
	ag, err := m.agentFor(entries[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := ag.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := created.MaterializeHeader(); err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	return created.ID()
}

// TestChatStoreMirror runs the chat client's SessionStore (the TS
// mirror of the page reducer) and the page reducer itself against
// identical synthetic frame sequences, asserting agreement on every
// shared field — the mirror-consistency that would otherwise live in
// CI's blind spot (the store is TypeScript; node >= 22.6 runs it via
// type stripping). Serverless: the server contract is pinned by
// TestPageReducerFixture. Skips without node.
func TestChatStoreMirror(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; the chat store mirror check needs node >= 22.6")
	}
	script, err := filepath.Abs(filepath.Join("static", "store_check.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("store_check.mjs: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "store_check: OK") {
		t.Fatalf("store_check.mjs: unexpected output\n%s", out)
	}
}
