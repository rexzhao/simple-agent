package sessioncontent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestLargeUTF8ToolArgumentsAndReasoningUseCompleteStableBlobs(t *testing.T) {
	store, session := newContentTestStore(t, "session-large-item")
	session.ShowReasoning = true
	if _, err := store.SaveMetadata(session); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("界", 80)
	reasoning := strings.Repeat("推理", 80)
	arguments := `{"text":"` + strings.Repeat("参数", 80) + `"}`
	item := sessions.SessionItemFromMessage("large-item", model.Message{
		Role:             model.MessageRoleAssistant,
		Content:          content,
		ReasoningContent: reasoning,
		ToolCalls:        []model.ToolCall{{ID: "call-1", Name: "tool", Arguments: arguments}},
	})
	if _, err := store.AppendItem(session.ID, item); err != nil {
		t.Fatal(err)
	}
	writer := &testBlobWriter{}
	p, err := NewProvider(store, ProviderOptions{MaxItemContentBytes: 16, BlobWriter: writer})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	registration := store.RegisterMutationSink(p)
	defer registration.Unregister()
	opened := openContent(t, p, session.ID, nil)
	defer opened.Close()
	snapshot := decodeSnapshot(t, opened)
	message := snapshot.History.Items[0].Message
	if message == nil || message.Content == nil || message.Content.Blob == nil || message.Content.Truncated {
		t.Fatalf("content projection = %#v, want complete blob", message)
	}
	if message.Reasoning == nil || message.Reasoning.Blob == nil || message.Reasoning.Truncated {
		t.Fatalf("reasoning projection = %#v, want complete blob", message.Reasoning)
	}
	if len(message.Content.Preview) > 240 || !strings.HasPrefix(message.Content.Preview, "界") || !strings.HasPrefix(message.Reasoning.Preview, "推理") {
		t.Fatalf("UTF-8 previews were not bounded at code-point boundaries: content=%q reasoning=%q", message.Content.Preview, message.Reasoning.Preview)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Arguments == nil || message.ToolCalls[0].Arguments.Blob == nil {
		t.Fatalf("tool arguments projection = %#v, want complete blob", message.ToolCalls)
	}
	for _, expected := range [][]byte{[]byte(reasoning), []byte(arguments), []byte(content)} {
		found := false
		for _, actual := range writer.contents {
			if bytes.Equal(actual, expected) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("complete large value was not sent to BlobWriter: %q", expected[:minInt(len(expected), 32)])
		}
	}
	calls := writer.calls
	if _, err := store.AppendItem(session.ID, sessions.SessionItemFromMessage("small-item", model.Message{Role: model.MessageRoleUser, Content: "small"})); err != nil {
		t.Fatal(err)
	}
	_ = nextChange(t, opened)
	if writer.calls != calls {
		t.Fatalf("unchanged large item generated new blobs: calls before=%d after=%d", calls, writer.calls)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
