package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/storage"
)

// TestJSChainFixture closes the cross-language blind spot: the Go-side
// cross-implementation test cannot catch JS-specific semantics (UTF-16
// code-unit lengths, escaper differences), so this test generates
// fixtures from the Go implementation and runs the page's actual chain
// module under node. Skipped when node is unavailable — the page imports
// the very module under test (static/chain.mjs), so there is no drift
// window between what is tested and what ships.
func TestJSChainFixture(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; the JS chain check needs node >= 18")
	}

	// Drift-prone samples: multi-byte text (UTF-16 length bug), HTML
	// escapes, astral planes (surrogate pairs!), empty and missing
	// fields, tool calls.
	records := []storage.Record{
		{ItemID: "i1", Type: storage.TypeUserInput, Time: "t1", Content: "你好世界 🎉 <html> & \u2028"},
		{ItemID: "i2", Type: storage.TypeAssistantMessage, Time: "t2", Content: "回答", Reasoning: "思考过程"},
		{ItemID: "", Type: storage.TypeSessionCreated, Time: ""},
		{ItemID: "i3", Type: storage.TypeToolResult, Time: "t3", ToolCallID: "c1", ToolName: "shell", IsError: true, Content: "exit 1"},
		{ItemID: "i4", Type: storage.TypeAssistantMessage, Time: "t4", Content: "done",
			ToolCalls: []storage.WireToolCall{{ID: "c1", Name: "shell", Arguments: `{"cmd":"ls"}`}}},
		// v2 tuple members (§15.2): reasoning_level rides the digest.
		// A session.created WITH a level and its twin WITHOUT must fold
		// differently — the level is inside the tamper-proof tuple.
		{ItemID: "h2", Type: storage.TypeSessionCreated, Time: "t5", Provider: "p", Model: "m", ReasoningLevel: "high"},
		{ItemID: "h3", Type: storage.TypeSessionCreated, Time: "t5", Provider: "p", Model: "m"},
	}

	var lines []string
	sessionID := "fixture-session"
	head := chainGenesis(sessionID)
	lines = append(lines, marshalLine(t, map[string]any{"session": sessionID, "genesis": head}))
	emit := func(r storage.Record) {
		head = chainStep(head, r)
		lines = append(lines, marshalLine(t, map[string]any{
			"record": json.RawMessage(mustJSON(t, r)),
			"input":  chainInput(r),
			"digest": head,
		}))
	}
	for _, r := range records {
		emit(r)
	}

	// The post-compaction segment (§14.4): a context.compacted commit
	// truncates the view to [session.created, compaction] and the chain
	// re-genesis from the compaction's ItemID — the segment line pins
	// that Go and JS derive the identical seed.
	compacted := storage.Record{ItemID: "x1", Type: storage.TypeContextCompacted, Time: "t9", Content: "summary"}
	head = chainGenesisAt(sessionID, compacted.ItemID)
	lines = append(lines, marshalLine(t, map[string]any{
		"session": sessionID, "compacted": compacted.ItemID, "genesis": head,
	}))
	emit(records[2]) // the session.created copy re-folds onto the new genesis
	emit(compacted)

	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "fixtures.jsonl")
	if err := os.WriteFile(fixturePath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	script, err := filepath.Abs(filepath.Join("testdata", "chain_check.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script)
	fixture, err := os.Open(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	cmd.Stdin = fixture
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node chain check failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("unexpected node output: %s", out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

func marshalLine(t *testing.T, v any) string {
	t.Helper()
	return string(mustJSON(t, v))
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
