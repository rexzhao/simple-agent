package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/rexzhao/simple-agent/internal/chatapp"
	"github.com/rexzhao/simple-agent/internal/sessionlog"
)

func newTestChat(t *testing.T) *chatUI {
	t.Helper()
	root := t.TempDir()
	providerDir := filepath.Join(root, "providers")
	if err := os.MkdirAll(providerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	providerYAML := `name: fake
base_url: http://127.0.0.1:1/v1
api_key: test-key
models:
  test-model:
    id: test-model
    type: openai-chat
`
	if err := os.WriteFile(filepath.Join(providerDir, "fake.yaml"), []byte(providerYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	rootYAML := "default_provider: fake\ndefault_model: test-model\nprovider_dir: providers\n"
	if err := os.WriteFile(filepath.Join(root, "sai.yaml"), []byte(rootYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := chatapp.New(chatapp.Options{ServerRoot: root, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	chat := newChatUI(app)
	if err := chat.newSession(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if chat.session != nil {
			chat.session.Close()
		}
	})
	return chat
}

// TestClearDoesNotPanicDuringLayout reproduces the /clear crash: the input
// handler clears chat.items mid-frame, and the same frame's layout re-measures
// retained list rows with stale indices. The row builders must survive an
// index past the new slice.
func TestClearDoesNotPanicDuringLayout(t *testing.T) {
	chat := newTestChat(t)
	chat.items = []sessionlog.Item{
		{ID: "item-1", Kind: sessionlog.ItemKindUser, Content: "first"},
		{ID: "item-2", Kind: sessionlog.ItemKindAssistant, Content: "second"},
	}
	tt := ui.NewTester(chat.view, 400, 400)
	if !tt.HasText("first") || !tt.HasText("second") {
		t.Fatalf("initial frame missing items: %q", tt.Texts())
	}

	chat.draft = "/clear"
	if err := tt.Click("Send"); err != nil {
		t.Fatal(err)
	}
	// The click's frame runs the submit handler and then lays the tree out;
	// without the row-builder guards this panics with index out of range.
	if len(chat.items) != 0 {
		t.Fatalf("items after /clear = %d, want 0", len(chat.items))
	}
	if chat.notice != "new session" {
		t.Fatalf("notice = %q, want %q", chat.notice, "new session")
	}
	if !tt.HasText("new session") {
		t.Fatalf("frame after /clear missing notice: %q", tt.Texts())
	}
}

// TestListClosuresSurviveStaleIndices pins the exact crash from the field
// report: mygo's ListState.remember calls the Key closure of a retained list
// element with the previous frame's anchor index, while the frame's submit
// handler has already replaced chat.items with a shorter slice. Every index
// closure must tolerate out-of-range values.
func TestListClosuresSurviveStaleIndices(t *testing.T) {
	chat := newTestChat(t)
	chat.items = []sessionlog.Item{
		{ID: "item-1", Kind: sessionlog.ItemKindUser, Content: "first"},
		{ID: "item-2", Kind: sessionlog.ItemKindAssistant, Content: "second"},
	}
	tt := ui.NewTester(chat.view, 400, 400)
	tt.Frame() // builds the list, remembers the anchor row index

	// Simulate the mid-frame /clear: the retained element still has n=2
	// while the state behind the closures is empty. remember would call
	// Key(0) on the old element; the guard must return nil, not panic.
	chat.items = nil
	if key := chat.list.Key(0); key != nil {
		t.Fatalf("Key(0) = %v, want nil for an empty slice", key)
	}
	if key := chat.list.Key(1); key != nil {
		t.Fatalf("Key(1) = %v, want nil for an empty slice", key)
	}
	if key := chat.list.Key(-1); key != nil {
		t.Fatalf("Key(-1) = %v, want nil", key)
	}

	// The same defense on the resume picker closures: build the picker with
	// sessions present, then clear the slice behind the retained closures.
	chat.sessions = []sessionlog.Summary{{ID: "s1", Title: "one"}}
	chat.showResume = true
	tt.Frame()
	chat.sessions = nil
	if key := chat.pickList.Key(0); key != nil {
		t.Fatalf("picker Key(0) = %v, want nil for an empty slice", key)
	}
	if label := chat.pickList.Label(0); label != "" {
		t.Fatalf("picker Label(0) = %q, want empty", label)
	}
}
