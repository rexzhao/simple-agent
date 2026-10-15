package web

// The C6a dialog-logic fixture driver: runs the node script that
// exercises the chat client's pure new-session/delete decisions
// (exact /new matching, default selection, reasoning visibility,
// error-code → hint mapping). Same shape as TestChatStoreMirror.

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewSessionFixture runs testdata/newsession_check.mjs: the pure
// logic behind the chat client's new-session dialog and delete
// confirmation (clients/chat/src/newsession.ts). Skips without node.
func TestNewSessionFixture(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; the dialog logic check needs node >= 23.6 (unflagged type stripping)")
	}
	script, err := filepath.Abs(filepath.Join("testdata", "newsession_check.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("newsession_check.mjs: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "newsession_check: OK (5 scenarios: s1,s2,s3,s4,s5)") {
		t.Fatalf("newsession_check.mjs: unexpected output\n%s", out)
	}
}
