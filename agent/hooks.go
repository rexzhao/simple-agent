package agent

// The tool approval policy hook (B2 step 7b, §16.3): a synchronous
// BeforeToolCall decides Allow / Deny / Ask; Ask hands the decision to
// the 7a state machine. AfterToolCall observes finished calls. The
// zero value (or a nil hook) allows everything — the behavior is
// byte-identical to the pre-hook agent.

import (
	"context"
	"fmt"

	"github.com/rexzhao/simple-agent/model"
)

// ToolAction is the policy hook's decision.
type ToolAction int

const (
	// ToolActionAllow executes the call.
	ToolActionAllow ToolAction = iota
	// ToolActionDeny refuses it: the refusal lands as a tool.result
	// is_error carrying the reason (the model sees it and can adjust);
	// the run continues.
	ToolActionDeny
	// ToolActionAsk hands the decision to the session's approval flow
	// (the 7a state machine): the run goroutine waits, bounded by the
	// run ctx.
	ToolActionAsk
)

// ToolCallInfo is the hook's view of one tool call.
type ToolCallInfo struct {
	// SessionID routes approval cards by session (one agent serves
	// many sessions in a host).
	SessionID string
	// RunID identifies the run.
	RunID string
	// ItemID is the identity of the future tool.result (the approval
	// identity).
	ItemID string
	// Name is the tool's name.
	Name string
	// Arguments is the raw JSON arguments string.
	Arguments string
}

// ToolDecision is the hook's answer.
type ToolDecision struct {
	Action ToolAction
	// Reason surfaces as the tool.result error content on Deny.
	Reason string
}

// Hooks is the host's tool policy. All fields are optional; the zero
// value allows every call and observes nothing.
type Hooks struct {
	// BeforeToolCall is a synchronous POLICY hook: it decides, never
	// blocks. Returning Ask hands the decision to the session's
	// approval flow (the run goroutine waits, bounded by run ctx). A
	// panic is fail-closed: treated as Deny with the reason noted.
	BeforeToolCall func(ctx context.Context, call ToolCallInfo) ToolDecision
	// AfterToolCall observes the finished call (telemetry, audit).
	// Boundaries: runs for executed calls AND for denials (the
	// observer sees the same is_error result the model sees); does
	// NOT run for cancelled calls (no result object exists); never
	// runs at process exit. A panic inside the hook is recovered and
	// silently discarded (the agent holds no logger); hosts that need
	// telemetry should handle their own errors inside the hook.
	AfterToolCall func(ctx context.Context, call ToolCallInfo, result model.ToolResult)
}

// decideToolCall runs the policy hook with panic protection. A nil
// hook allows; a panicking hook fails closed (Deny, reason noted).
func (a *Agent) decideToolCall(ctx context.Context, call ToolCallInfo) (decision ToolDecision) {
	if a.hooks.BeforeToolCall == nil {
		return ToolDecision{Action: ToolActionAllow}
	}
	defer func() {
		if r := recover(); r != nil {
			decision = ToolDecision{
				Action: ToolActionDeny,
				Reason: fmt.Sprintf("approval hook panicked: %v", r),
			}
		}
	}()
	return a.hooks.BeforeToolCall(ctx, call)
}

// observeToolCall runs the after-hook with panic protection; errors
// and panics are swallowed (the observer must not affect the run).
func (a *Agent) observeToolCall(ctx context.Context, call ToolCallInfo, result model.ToolResult) {
	if a.hooks.AfterToolCall == nil {
		return
	}
	defer func() { _ = recover() }()
	a.hooks.AfterToolCall(ctx, call, result)
}
