package execution

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

const continueParentRunPrefix = "run-continue-parent-"

// RegisterContinueParentSubscription records the child completion before the
// caller returns the session_start result. The follow-up scan also closes the
// small race where the child settled before this registration was committed.
func (s *Service) RegisterContinueParentSubscription(parentSessionID, childSessionID, childRunID string) error {
	if s == nil || s.sessionStore == nil {
		return fmt.Errorf("execution session store is not configured")
	}
	parentSessionID = strings.TrimSpace(parentSessionID)
	childSessionID = strings.TrimSpace(childSessionID)
	childRunID = strings.TrimSpace(childRunID)
	if parentSessionID == "" || childSessionID == "" || childRunID == "" {
		return fmt.Errorf("parent session id, child session id, and child run id are required")
	}
	deliveryID := sessions.NewSessionCompletionDeliveryID(parentSessionID, childSessionID, childRunID)
	delivery, err := s.sessionStore.RegisterSessionCompletion(parentSessionID, childSessionID, childRunID, deliveryID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("register child completion subscription: %w", err)
	}
	// Do one immediate durable-status check before returning. The asynchronous
	// scan below is still required for transient SQLite errors and for the
	// parent-idle admission step, but this closes the fast-settle/register race
	// without depending on a scheduler handoff.
	if parent, loadErr := s.sessionStore.LoadState(parentSessionID); loadErr == nil {
		s.reconcileCompletionDelivery(parent, delivery)
	}
	go s.processCompletionInbox(parentSessionID)
	return nil
}

func (s *Service) onRunIdle(run *CoordinatedSessionRun) {
	// The coordinator hot path only schedules this scan. SQLite claims provide
	// the cross-goroutine/process deduplication; no process-local queue is
	// required for correctness.
	//
	// Do not create a tight retry loop when an automatically started run fails
	// before it can create its durable runs row (for example, while a required
	// provider remains unavailable). The claimed delivery stays durable and is
	// retried on the next independent run settlement or process startup. If the
	// row exists, scan now so the successfully admitted delivery is consumed.
	if run != nil && strings.HasPrefix(run.ID(), continueParentRunPrefix) {
		if _, err := s.sessionStore.GetRun(run.SessionID(), run.ID()); err != nil {
			return
		}
	}
	go s.scanCompletionInboxes()
}

func (s *Service) scanCompletionInboxes() {
	if s == nil || s.sessionStore == nil || s.sessionRunCoordinator() == nil {
		return
	}
	states, err := s.sessionStore.ListStates(sessions.V2ListOptions{All: true})
	if err != nil {
		return
	}
	for _, state := range states {
		s.processCompletionInbox(state.ID)
	}
}

// onRunSettledForCompletion is intentionally called asynchronously from the
// lifecycle observer. A failed inbox write must not delay the coordinator's
// await/remove path; the durable row remains pending and is found by a later
// scan or restart.
func (s *Service) onRunSettledForCompletion(run *CoordinatedSessionRun) {
	if s == nil || s.sessionStore == nil || run == nil {
		return
	}
	child, err := s.sessionStore.LoadState(run.SessionID())
	if err != nil {
		return
	}
	parentID := strings.TrimSpace(child.ParentSessionID)
	if parentID == "" {
		return
	}
	childStatus := string(run.Status())
	if childStatus == "" {
		childStatus = sessions.RunStatusFailed
	}
	settledAt := time.Now().UTC()
	if durableRun, err := s.sessionStore.GetRun(run.SessionID(), run.ID()); err == nil {
		childStatus = durableRun.Status
		if !durableRun.SettledAt.IsZero() {
			settledAt = durableRun.SettledAt
		}
	}
	// Registration may not have committed yet. In that case the registration
	// path performs the durable-status check after inserting the row.
	if err := s.sessionStore.MarkSessionCompletionDelivered(parentID, child.ID, run.ID(), childStatus, settledAt); err != nil {
		return
	}
	go s.processCompletionInbox(parentID)
}

func (s *Service) processCompletionInbox(parentSessionID string) {
	if s == nil || s.sessionStore == nil {
		return
	}
	parentSessionID = strings.TrimSpace(parentSessionID)
	if parentSessionID == "" {
		return
	}
	parent, err := s.sessionStore.LoadState(parentSessionID)
	if err != nil {
		return
	}
	deliveries, err := s.sessionStore.ListSessionInbox(parentSessionID)
	if err != nil {
		return
	}
	for _, delivery := range deliveries {
		if delivery.Status != sessions.SessionInboxStatusPending {
			continue
		}
		s.reconcileCompletionDelivery(parent, delivery)
	}

	// Reload after reconciliation so terminal child status and any existing
	// claim are evaluated from the same durable row that will be updated.
	deliveries, err = s.sessionStore.ListSessionInbox(parentSessionID)
	if err != nil {
		return
	}
	if parent.Archived {
		for _, delivery := range deliveries {
			if delivery.Status != sessions.SessionInboxStatusConsumed {
				_ = s.sessionStore.RejectSessionCompletionDelivery(parentSessionID, delivery.DeliveryID, "parent session is archived", time.Now().UTC())
			}
		}
		return
	}
	coordinator := s.sessionRunCoordinator()
	if coordinator == nil {
		return
	}
	for _, delivery := range deliveries {
		if delivery.Status != sessions.SessionInboxStatusDelivered {
			continue
		}
		s.dispatchCompletionDelivery(parent, delivery, coordinator)
	}
}

func (s *Service) reconcileCompletionDelivery(parent sessions.SessionV2, delivery sessions.SessionInboxDelivery) {
	childRun, err := s.sessionStore.GetRun(delivery.ChildSessionID, delivery.ChildRunID)
	if err == nil {
		if childRun.Status == sessions.RunStatusRunning {
			return
		}
		_ = s.sessionStore.MarkSessionCompletionDelivered(parent.ID, delivery.ChildSessionID, delivery.ChildRunID, childRun.Status, childRun.SettledAt)
		return
	}
	if !errors.Is(err, sessions.ErrNotFound) {
		return
	}
	// A child can be deleted while its parent inbox remains. Preserve a compact
	// completion descriptor rather than attempting to resurrect or inspect it.
	child, childErr := s.sessionStore.LoadState(delivery.ChildSessionID)
	if errors.Is(childErr, sessions.ErrNotFound) {
		_ = s.sessionStore.MarkSessionCompletionDelivered(parent.ID, delivery.ChildSessionID, delivery.ChildRunID, "deleted", time.Now().UTC())
		return
	}
	if childErr != nil {
		return
	}
	if child.RunningRunID == delivery.ChildRunID {
		return
	}
	// A coordinator run that failed before CreateRun could not leave a durable
	// run row. Once it is no longer active, the subscription is still safe to
	// deliver as a compact unknown/settled notification.
	if child.LastRunID == delivery.ChildRunID || child.LatestRunID == delivery.ChildRunID {
		status := child.LastRunStatus
		if status == "" {
			status = "settled"
		}
		_ = s.sessionStore.MarkSessionCompletionDelivered(parent.ID, delivery.ChildSessionID, delivery.ChildRunID, status, time.Now().UTC())
	}
}

func completionParentRunID(deliveryID string) string {
	return continueParentRunPrefix + strings.TrimPrefix(strings.TrimSpace(deliveryID), "delivery-")
}

func completionNotification(delivery sessions.SessionInboxDelivery) string {
	status := strings.TrimSpace(delivery.ChildStatus)
	if status == "" {
		status = "settled"
	}
	return fmt.Sprintf("Internal child-completion notification. Child session %q run %q settled with status %q. Read the child's persisted output using session_get or session_history before deciding how to continue the parent task. This is a notification, not a new user request; do not repeat the child's prompt.", delivery.ChildSessionID, delivery.ChildRunID, status)
}

func (s *Service) dispatchCompletionDelivery(parent sessions.SessionV2, delivery sessions.SessionInboxDelivery, coordinator *SessionRunCoordinator) {
	stableRunID := completionParentRunID(delivery.DeliveryID)

	// A previous process may have admitted this exact run and crashed before
	// marking the inbox consumed. Discover it before claiming/starting again.
	existing, err := s.sessionStore.GetRun(parent.ID, stableRunID)
	if err == nil {
		if existing.Status == sessions.RunStatusRunning {
			return
		}
		claimed, claimErr := s.sessionStore.ClaimSessionCompletionDelivery(parent.ID, delivery.DeliveryID, stableRunID)
		if claimErr == nil && (claimed || delivery.StartedRunID == stableRunID) {
			_ = s.sessionStore.ConsumeSessionCompletionDelivery(parent.ID, delivery.DeliveryID, stableRunID, time.Now().UTC())
		}
		return
	}
	if !errors.Is(err, sessions.ErrNotFound) {
		return
	}

	if _, active := coordinator.ActiveForSession(parent.ID); active {
		return
	}
	if delivery.StartedRunID != "" {
		// The prior process may have crashed after claiming but before the
		// coordinator could admit the stable run. If the run was not found
		// above, release that durable claim so this scan can retry it.
		if delivery.StartedRunID != stableRunID {
			return
		}
		if err := s.sessionStore.ClearSessionCompletionClaim(parent.ID, delivery.DeliveryID, stableRunID, "retrying unstarted delivery"); err != nil {
			return
		}
	}
	claimed, err := s.sessionStore.ClaimSessionCompletionDelivery(parent.ID, delivery.DeliveryID, stableRunID)
	if err != nil || !claimed {
		return
	}
	input := SessionMessageInput{
		Content:    completionNotification(delivery),
		Internal:   true,
		DeliveryID: delivery.DeliveryID,
	}
	if _, err := coordinator.StartWithID(parent.ID, input, stableRunID, nil); err != nil {
		_ = s.sessionStore.ClearSessionCompletionClaim(parent.ID, delivery.DeliveryID, stableRunID, err.Error())
		return
	}
	// Leave the row delivered/claimed until the new run has a durable runs row.
	// This preserves retryability if asynchronous startup fails before
	// CreateRun, while the stable id prevents another start once admission has
	// succeeded. The next idle scan consumes it after observing the run.
}
