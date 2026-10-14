package agent

// The approval state machine (B2 step 7a, §16.3): when a tool call's
// policy decision is Ask, the SESSION owns the pending state — the
// Pending event and the snapshot's AwaitingApproval flag are set in
// one s.mu critical section, the host resolves through
// ResolveApproval (typed errors), and the wait is bounded by the run
// ctx. Step 7b wires the policy hook that returns Ask; the mechanism
// here is complete and exercised through the internal entry.

import (
	"context"
	"errors"
	"fmt"

	"github.com/rexzhao/simple-agent/model"
)

// Approval outcomes (ToolApprovalResolvedEvent.Outcome).
const (
	ApprovalOutcomeApproved  = "approved"
	ApprovalOutcomeDenied    = "denied"
	ApprovalOutcomeCancelled = "cancelled"
)

var (
	// ErrApprovalUnknown: no such pending approval — it may have
	// already landed or never existed.
	ErrApprovalUnknown = errors.New("unknown approval request")
	// ErrApprovalResolved: the pending approval was already answered.
	ErrApprovalResolved = errors.New("approval request already resolved")
)

// approvalDecision is one host answer.
type approvalDecision struct {
	allow  bool
	reason string
}

// pendingApproval is one awaiting tool call.
type pendingApproval struct {
	itemID   string
	call     model.ToolCall
	decision chan approvalDecision // buffered 1; the waiter receives at most one value
	resolved bool                  // set by the first ResolveApproval
}

// awaitApproval enters the pending state and blocks until the host
// resolves, the ctx is cancelled, or the session closes. The Pending
// event and the snapshot flag are set in ONE critical section; the
// Resolved event is emitted in the same critical section as the
// decision taking effect. Cancellation is an interruption: no
// tool.result is written (the dangling call is patched by
// sanitizeContext at the next request assembly).
func (s *Session) awaitApproval(ctx context.Context, call model.ToolCall, itemID string) (approvalDecision, error) {
	decision := approvalDecision{allow: false, reason: ""}
	p := &pendingApproval{
		itemID:   itemID,
		call:     call,
		decision: make(chan approvalDecision, 1),
	}
	s.mu.Lock()
	if s.state.Running == nil || s.state.Running.ItemID != itemID {
		s.mu.Unlock()
		return decision, fmt.Errorf("approval: no running tool slot for %s", itemID)
	}
	s.state.Running.AwaitingApproval = true
	s.pendingApproval = p
	s.enqueueStreamLocked(Transient{Event: model.ToolApprovalPendingEvent{
		ItemID: itemID, Name: call.Name, Arguments: call.Arguments,
	}}, true)
	s.mu.Unlock()

	select {
	case d := <-p.decision:
		// The decision and the cancellation can race in the select
		// (both ready). CANCELLATION WINS: recheck ctx so a stopped
		// run never proceeds to execution — the run was torn down,
		// executing the tool would violate the interruption rule
		// (§13).
		if ctx.Err() != nil {
			s.resolveCancelled(itemID)
			return decision, ctx.Err()
		}
		// The decision is effective (ResolveApproval wrote it); emit
		// the resolution and clear the flag in one section.
		s.mu.Lock()
		s.pendingApproval = nil
		if s.state.Running != nil && s.state.Running.ItemID == itemID {
			s.state.Running.AwaitingApproval = false
		}
		outcome := ApprovalOutcomeDenied
		if d.allow {
			outcome = ApprovalOutcomeApproved
		}
		s.enqueueStreamLocked(Transient{Event: model.ToolApprovalResolvedEvent{
			ItemID: itemID, Outcome: outcome,
		}}, true)
		s.mu.Unlock()
		return d, nil
	case <-ctx.Done():
		s.resolveCancelled(itemID)
		return decision, ctx.Err()
	}
}

// resolveCancelled clears the pending state and emits the cancelled
// resolution in one critical section (the shared tail of both
// cancellation paths).
func (s *Session) resolveCancelled(itemID string) {
	s.mu.Lock()
	s.pendingApproval = nil
	if s.state.Running != nil && s.state.Running.ItemID == itemID {
		s.state.Running.AwaitingApproval = false
	}
	s.enqueueStreamLocked(Transient{Event: model.ToolApprovalResolvedEvent{
		ItemID: itemID, Outcome: ApprovalOutcomeCancelled,
	}}, true)
	s.mu.Unlock()
}

// ResolveApproval answers a pending tool approval. allow=false lands
// as a tool.result is_error carrying the reason (the same shape a
// policy denial takes); the typed errors distinguish "no such
// pending" from "already answered".
//
// Race with cancellation: if the run's ctx cancels while a resolve is
// in flight, CANCELLATION WINS — the wait returns as cancelled and
// the Resolved event carries "cancelled", even though this call may
// have already returned nil. The Resolved event is the source of
// truth for the outcome.
func (s *Session) ResolveApproval(itemID string, allow bool, reason string) error {
	s.mu.Lock()
	p := s.pendingApproval
	if p == nil || p.itemID != itemID {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrApprovalUnknown, itemID)
	}
	if p.resolved {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrApprovalResolved, itemID)
	}
	p.resolved = true
	s.mu.Unlock()
	// The waiter emits the resolution event and clears the state in
	// its own critical section (the decision channel is buffered, so
	// this never blocks the resolving host).
	p.decision <- approvalDecision{allow: allow, reason: reason}
	return nil
}
