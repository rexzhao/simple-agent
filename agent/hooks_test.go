package agent

// Policy hook integration tests (B2 step 7b): the hook wiring through
// the REAL run loop — Ask→approve executes, Ask→deny lands is_error
// and the run continues, Ask→Stop leaves no tool.result (the
// interruption rule), policy Allow/Deny emit no approval events, and
// the zero hook is byte-identical to no hook.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage"
	"github.com/rexzhao/simple-agent/storage/memory"
)

// countingTool counts its executions — the probe for "deny must
// prevent the effect".
type countingTool struct {
	calls *int
}

func (c countingTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: "echo"}
}

func (c countingTool) Execute(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
	*c.calls++
	return model.ToolResult{Content: "tool ran"}, nil
}

// hookTestAgent builds an agent with the given hooks over a scripted
// two-turn provider (turn 1: tool call; turn 2: final answer).
func hookTestAgent(t *testing.T, hooks Hooks, tool Tool) (*Agent, *Session) {
	t.Helper()
	provider := &fakeProvider{turns: [][]model.Event{
		{model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "c1", Name: "echo", Arguments: `{}`}}},
		{model.TextDeltaEvent{Text: "final"}, model.MessageDoneEvent{}},
	}}
	if tool == nil {
		tool = testTool{def: model.ToolDefinition{Name: "echo"}, exec: func(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error) {
			return model.ToolResult{Content: "tool ran"}, nil
		}}
	}
	ag, err := New(Options{
		ModelResolver: NewStaticResolver(provider, nil, 0, 0, ""),
		Storage:       memory.OpenStore(),
		Tools:         []Tool{tool},
		Hooks:         hooks,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession("test-ns", testSessionMetaAgent())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return ag, session
}

// TestHookAskApproveExecutesTool: Ask → the host approves → the tool
// executes and its result commits; the run settles normally.
func TestHookAskApproveExecutesTool(t *testing.T) {
	hooks := Hooks{
		BeforeToolCall: func(ctx context.Context, call ToolCallInfo) ToolDecision {
			return ToolDecision{Action: ToolActionAsk}
		},
	}
	_, session := hookTestAgent(t, hooks, nil)

	sub := session.Stream()
	defer sub.Stop()
	if err := session.Send("go"); err != nil {
		t.Fatal(err)
	}

	// Wait for the pending state, then approve.
	deadline := time.Now().Add(2 * time.Second)
	var itemID string
	for itemID == "" {
		select {
		case ev := <-sub.Events():
			if tr, ok := ev.(Transient); ok {
				if pe, ok := tr.Event.(model.ToolApprovalPendingEvent); ok {
					itemID = pe.ItemID
				}
			}
		case <-time.After(time.Until(deadline)):
			t.Fatal("Pending event never arrived")
		}
	}
	if err := session.ResolveApproval(itemID, true, ""); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)

	// The tool executed and its result committed.
	for _, r := range session.SnapshotStream().Records {
		if r.Type == storage.TypeToolResult && r.Content == "tool ran" {
			return // pass
		}
	}
	t.Fatal("the approved tool never executed")
}

// TestHookAskDenyLandsErrorResultAndRunContinues: Ask → the host
// denies with a reason → an is_error tool.result carries it, the run
// continues to the final answer, and AfterToolCall observed the
// denial.
func TestHookAskDenyLandsErrorResultAndRunContinues(t *testing.T) {
	var observed []model.ToolResult
	hooks := Hooks{
		BeforeToolCall: func(ctx context.Context, call ToolCallInfo) ToolDecision {
			return ToolDecision{Action: ToolActionAsk}
		},
		AfterToolCall: func(ctx context.Context, call ToolCallInfo, result model.ToolResult) {
			observed = append(observed, result)
		},
	}
	_, session := hookTestAgent(t, hooks, nil)

	sub := session.Stream()
	defer sub.Stop()
	if err := session.Send("go"); err != nil {
		t.Fatal(err)
	}

	var itemID string
	deadline := time.Now().Add(2 * time.Second)
	for itemID == "" {
		select {
		case ev := <-sub.Events():
			if tr, ok := ev.(Transient); ok {
				if pe, ok := tr.Event.(model.ToolApprovalPendingEvent); ok {
					itemID = pe.ItemID
				}
			}
		case <-time.After(time.Until(deadline)):
			t.Fatal("Pending event never arrived")
		}
	}
	if err := session.ResolveApproval(itemID, false, "not on my watch"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)

	// The denial landed as an is_error tool.result with the reason.
	found := false
	for _, r := range session.SnapshotStream().Records {
		if r.Type == storage.TypeToolResult && r.IsError && strings.Contains(r.Content, "not on my watch") {
			found = true
		}
	}
	if !found {
		t.Fatal("the denial did not land as an is_error tool.result with the reason")
	}
	// The run continued: a final assistant message exists.
	hasFinal := false
	for _, r := range session.SnapshotStream().Records {
		if r.Type == storage.TypeAssistantMessage && r.Content == "final" {
			hasFinal = true
		}
	}
	if !hasFinal {
		t.Fatal("the run did not continue after the denial")
	}
	// AfterToolCall observed the denial (the same is_error result).
	if len(observed) != 1 || !observed[0].IsError {
		t.Fatalf("AfterToolCall observed %+v, want one is_error result", observed)
	}
}

// TestHookAskStopLeavesNoToolResult: Ask → the run is stopped → no
// tool.result commits (the interruption rule; sanitizeContext patches
// at the next assembly) and the run ends interrupted.
func TestHookAskStopLeavesNoToolResult(t *testing.T) {
	hooks := Hooks{
		BeforeToolCall: func(ctx context.Context, call ToolCallInfo) ToolDecision {
			return ToolDecision{Action: ToolActionAsk}
		},
	}
	_, session := hookTestAgent(t, hooks, nil)

	sub := session.Stream()
	defer sub.Stop()
	if err := session.Send("go"); err != nil {
		t.Fatal(err)
	}

	var itemID string
	deadline := time.Now().Add(2 * time.Second)
	for itemID == "" {
		select {
		case ev := <-sub.Events():
			if tr, ok := ev.(Transient); ok {
				if pe, ok := tr.Event.(model.ToolApprovalPendingEvent); ok {
					itemID = pe.ItemID
				}
			}
		case <-time.After(time.Until(deadline)):
			t.Fatal("Pending event never arrived")
		}
	}
	// Stop the run while the approval is pending.
	session.Stop()
	waitSettled(t, session)

	// No tool.result was written.
	for _, r := range session.SnapshotStream().Records {
		if r.Type == storage.TypeToolResult {
			t.Fatalf("a stopped approval wrote a tool.result: %+v", r)
		}
	}
	// The run ended interrupted, not failed: the run's error is
	// ErrInterrupted (the API-level contract), and no run.failed
	// record was written (the deferred failure commit skips
	// interrupted runs).
	run := session.lastRun
	if run == nil {
		t.Fatal("no last run after settle")
	}
	select {
	case <-run.Done():
	default:
		t.Fatal("the run never completed")
	}
	if !errors.Is(run.Err(), ErrInterrupted) {
		t.Fatalf("run error = %v, want ErrInterrupted", run.Err())
	}
	for _, r := range session.SnapshotStream().Records {
		if r.Type == "run.failed" {
			t.Fatalf("the interrupted approval run wrote a failure record: %+v", r)
		}
	}
}

// TestHookPolicyAllowAndDenyEmitNoApprovalEvents: a policy hook that
// only returns Allow or Deny never produces approval events (instant
// decisions do not flash the card) — the golden baseline extended to
// "a hook that always Allows".
func TestHookPolicyAllowAndDenyEmitNoApprovalEvents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action ToolAction
	}{
		{"allow", ToolActionAllow},
		{"deny", ToolActionDeny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hooks := Hooks{
				BeforeToolCall: func(ctx context.Context, call ToolCallInfo) ToolDecision {
					return ToolDecision{Action: tc.action, Reason: "policy"}
				},
			}
			_, session := hookTestAgent(t, hooks, nil)
			sub := session.Stream()
			defer sub.Stop()
			if err := session.Send("go"); err != nil {
				t.Fatal(err)
			}
			waitSettled(t, session)
			for ev := range drainEvents(t, sub) {
				if tr, ok := ev.(Transient); ok {
					switch tr.Event.(type) {
					case model.ToolApprovalPendingEvent, model.ToolApprovalResolvedEvent:
						t.Fatalf("policy %s produced an approval event", tc.name)
					}
				}
			}
			// The policy deny lands as an is_error tool.result and the
			// run continues (the kill: deny terminating the run fails
			// here — no denial record, no final message).
			if tc.action == ToolActionDeny {
				denied := false
				for _, r := range session.SnapshotStream().Records {
					if r.Type == storage.TypeToolResult && r.IsError && strings.Contains(r.Content, "policy") {
						denied = true
					}
				}
				if !denied {
					t.Fatal("the policy denial did not land as an is_error tool.result")
				}
				hasFinal := false
				for _, r := range session.SnapshotStream().Records {
					if r.Type == storage.TypeAssistantMessage && r.Content == "final" {
						hasFinal = true
					}
				}
				if !hasFinal {
					t.Fatal("the run did not continue after the policy denial")
				}
			}
		})
	}
}

// TestHookZeroValueEqualsNoHook: Hooks{} (the zero value) produces
// the same event-type sequence as no hook at all.
func TestHookZeroValueEqualsNoHook(t *testing.T) {
	collect := func(hooks Hooks) []string {
		_, session := hookTestAgent(t, hooks, nil)
		sub := session.Stream()
		defer sub.Stop()
		if err := session.Send("go"); err != nil {
			t.Fatal(err)
		}
		waitSettled(t, session)
		var types []string
		deadline := time.After(2 * time.Second)
	collect:
		for {
			select {
			case ev := <-sub.Events():
				switch e := ev.(type) {
				case RecordCommitted:
					types = append(types, "record:"+string(e.Record.Type))
				case Transient:
					types = append(types, "transient:"+string(e.Event.Type()))
				case Status:
					types = append(types, "status")
					if !e.Running {
						break collect
					}
				}
			case <-deadline:
				t.Fatalf("stream never settled; events: %v", types)
			}
		}
		return types
	}
	withZero := collect(Hooks{})
	// The no-hook path is the same Options minus Hooks.
	provider := &fakeProvider{turns: [][]model.Event{
		{model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "c1", Name: "echo", Arguments: `{}`}}},
		{model.TextDeltaEvent{Text: "final"}, model.MessageDoneEvent{}},
	}}
	ag, err := New(Options{
		ModelResolver: NewStaticResolver(provider, nil, 0, 0, ""),
		Storage:       memory.OpenStore(),
		Tools: []Tool{testTool{def: model.ToolDefinition{Name: "echo"}, exec: func(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error) {
			return model.ToolResult{Content: "tool ran"}, nil
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession("test-ns", testSessionMetaAgent())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	sub := session.Stream()
	defer sub.Stop()
	if err := session.Send("go"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	var noHook []string
	deadline := time.After(2 * time.Second)
collect2:
	for {
		select {
		case ev := <-sub.Events():
			switch e := ev.(type) {
			case RecordCommitted:
				noHook = append(noHook, "record:"+string(e.Record.Type))
			case Transient:
				noHook = append(noHook, "transient:"+string(e.Event.Type()))
			case Status:
				noHook = append(noHook, "status")
				if !e.Running {
					break collect2
				}
			}
		case <-deadline:
			t.Fatalf("stream never settled; events: %v", noHook)
		}
	}
	if len(withZero) != len(noHook) {
		t.Fatalf("zero-hook events %v != no-hook events %v", withZero, noHook)
	}
	for i := range withZero {
		if withZero[i] != noHook[i] {
			t.Fatalf("event[%d]: zero-hook %s != no-hook %s", i, withZero[i], noHook[i])
		}
	}
}

// TestHookPanicFailsClosed: a panicking policy hook is treated as
// Deny with the reason noted — never fail-open.
func TestHookPanicFailsClosed(t *testing.T) {
	hooks := Hooks{
		BeforeToolCall: func(ctx context.Context, call ToolCallInfo) ToolDecision {
			panic("injected policy panic")
		},
	}
	_, session := hookTestAgent(t, hooks, nil)
	if err := session.Send("go"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	for _, r := range session.SnapshotStream().Records {
		if r.Type == storage.TypeToolResult && r.IsError && strings.Contains(r.Content, "approval hook panicked") {
			return // pass
		}
	}
	t.Fatal("the panicking hook did not fail closed (no denial result)")
}

// TestHookAfterNotCalledOnCancel: AfterToolCall does not run for a
// cancelled call (no result object exists).
func TestHookAfterNotCalledOnCancel(t *testing.T) {
	var afterCalls int
	hooks := Hooks{
		BeforeToolCall: func(ctx context.Context, call ToolCallInfo) ToolDecision {
			return ToolDecision{Action: ToolActionAsk}
		},
		AfterToolCall: func(ctx context.Context, call ToolCallInfo, result model.ToolResult) {
			afterCalls++
		},
	}
	_, session := hookTestAgent(t, hooks, nil)
	sub := session.Stream()
	defer sub.Stop()
	if err := session.Send("go"); err != nil {
		t.Fatal(err)
	}
	var itemID string
	deadline := time.Now().Add(2 * time.Second)
	for itemID == "" {
		select {
		case ev := <-sub.Events():
			if tr, ok := ev.(Transient); ok {
				if pe, ok := tr.Event.(model.ToolApprovalPendingEvent); ok {
					itemID = pe.ItemID
				}
			}
		case <-time.After(time.Until(deadline)):
			t.Fatal("Pending event never arrived")
		}
	}
	session.Stop()
	waitSettled(t, session)
	if afterCalls != 0 {
		t.Fatalf("AfterToolCall ran %d times for a cancelled call", afterCalls)
	}
}

func drainEvents(t *testing.T, sub *StreamSubscription) <-chan SessionEvent {
	t.Helper()
	ch := make(chan SessionEvent, 64)
	go func() {
		defer close(ch)
		deadline := time.After(2 * time.Second)
		for {
			select {
			case ev := <-sub.Events():
				ch <- ev
				if st, ok := ev.(Status); ok && !st.Running {
					return
				}
			case <-deadline:
				return
			}
		}
	}()
	return ch
}

// TestHookDenyPreventsExecution pins the core safety property: a
// denial (policy or human) must prevent the tool's EFFECT — the
// executor is never called. The record shape alone (is_error) is not
// enough: a regression that executes and then refuses would keep
// every record-shape assertion green while the side effect happened.
func TestHookDenyPreventsExecution(t *testing.T) {
	t.Run("policy deny", func(t *testing.T) {
		calls := 0
		hooks := Hooks{
			BeforeToolCall: func(ctx context.Context, call ToolCallInfo) ToolDecision {
				return ToolDecision{Action: ToolActionDeny, Reason: "no"}
			},
		}
		_, session := hookTestAgent(t, hooks, countingTool{calls: &calls})
		if err := session.Send("go"); err != nil {
			t.Fatal(err)
		}
		waitSettled(t, session)
		if calls != 0 {
			t.Fatalf("the policy-denied tool executed %d times — deny must prevent the effect", calls)
		}
	})
	t.Run("ask deny", func(t *testing.T) {
		calls := 0
		hooks := Hooks{
			BeforeToolCall: func(ctx context.Context, call ToolCallInfo) ToolDecision {
				return ToolDecision{Action: ToolActionAsk}
			},
		}
		_, session := hookTestAgent(t, hooks, countingTool{calls: &calls})
		sub := session.Stream()
		defer sub.Stop()
		if err := session.Send("go"); err != nil {
			t.Fatal(err)
		}
		item := waitPendingItem(t, sub)
		if err := session.ResolveApproval(item, false, "no"); err != nil {
			t.Fatal(err)
		}
		waitSettled(t, session)
		if calls != 0 {
			t.Fatalf("the human-denied tool executed %d times — deny must prevent the effect", calls)
		}
	})
	t.Run("ask approve executes once", func(t *testing.T) {
		calls := 0
		hooks := Hooks{
			BeforeToolCall: func(ctx context.Context, call ToolCallInfo) ToolDecision {
				return ToolDecision{Action: ToolActionAsk}
			},
		}
		_, session := hookTestAgent(t, hooks, countingTool{calls: &calls})
		sub := session.Stream()
		defer sub.Stop()
		if err := session.Send("go"); err != nil {
			t.Fatal(err)
		}
		item := waitPendingItem(t, sub)
		if err := session.ResolveApproval(item, true, ""); err != nil {
			t.Fatal(err)
		}
		waitSettled(t, session)
		if calls != 1 {
			t.Fatalf("the approved tool executed %d times, want exactly 1", calls)
		}
	})
}

// waitPendingItem waits for the Pending event and returns its item.
func waitPendingItem(t *testing.T, sub *StreamSubscription) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-sub.Events():
			if tr, ok := ev.(Transient); ok {
				if pe, ok := tr.Event.(model.ToolApprovalPendingEvent); ok {
					return pe.ItemID
				}
			}
		case <-deadline:
			t.Fatal("Pending event never arrived")
		}
	}
}
