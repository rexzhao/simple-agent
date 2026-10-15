package web

// The approval event frames on the wire (C6c, §16.3): the pending and
// resolved events must reach the SSE stream — the chat client's
// approval card is built on them (encodeEventFrame's default branch
// silently drops unknown event types, so a missing case is invisible
// without this pin).

import (
	"encoding/json"
	"testing"

	"github.com/rexzhao/simple-agent/model"
)

func TestApprovalEventFrames(t *testing.T) {
	pending, ok := encodeEventFrame(model.ToolApprovalPendingEvent{
		ItemID:    "i1",
		Name:      "shell",
		Arguments: "rm -rf /tmp/x",
	})
	if !ok {
		t.Fatal("the pending approval event is dropped from the wire")
	}
	if pending.Kind != "event" || pending.Type != "tool_approval_pending" ||
		pending.Item != "i1" || pending.Name != "shell" || pending.Args != "rm -rf /tmp/x" {
		t.Fatalf("pending frame = %+v", pending)
	}
	// The exact JSON the client consumes: item/name/args, no outcome.
	encoded, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"event","t":"tool_approval_pending","item":"i1","name":"shell","args":"rm -rf /tmp/x"}`
	if string(encoded) != want {
		t.Fatalf("pending json = %s, want %s", encoded, want)
	}

	for _, tc := range []struct{ outcome, want string }{
		{"approved", `{"kind":"event","t":"tool_approval_resolved","item":"i1","outcome":"approved"}`},
		{"denied", `{"kind":"event","t":"tool_approval_resolved","item":"i1","outcome":"denied"}`},
		{"cancelled", `{"kind":"event","t":"tool_approval_resolved","item":"i1","outcome":"cancelled"}`},
	} {
		resolved, ok := encodeEventFrame(model.ToolApprovalResolvedEvent{ItemID: "i1", Outcome: tc.outcome})
		if !ok {
			t.Fatalf("the %s resolution is dropped from the wire", tc.outcome)
		}
		encoded, err := json.Marshal(resolved)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != tc.want {
			t.Fatalf("%s json = %s, want %s", tc.outcome, encoded, tc.want)
		}
	}
}
