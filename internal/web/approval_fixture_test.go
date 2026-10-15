package web

// The C6c approval-card fixture driver: runs the node script that
// exercises the chat client's approval state machine (the REAL
// SessionStore fed the approval wire shapes) and the pure card
// decisions. Same shape as TestNewSessionFixture.

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestApprovalFixture runs testdata/approval_check.mjs: the approval
// card's state — snapshot restore, the pending→resolved event
// sequence, the tool-lifecycle clearing, and the error-code → action
// mapping with double-submit protection. Skips without node.
func TestApprovalFixture(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; the approval card check needs node >= 23.6 (unflagged type stripping)")
	}
	script, err := filepath.Abs(filepath.Join("testdata", "approval_check.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("approval_check.mjs: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "approval_check: OK (6 scenarios: s10,s11,s12,s13,s14,s15)") {
		t.Fatalf("approval_check.mjs: unexpected output\n%s", out)
	}
}
