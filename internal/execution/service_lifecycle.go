package execution

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

const (
	LifecycleSessionCreated  = "session.created"
	LifecycleSessionUpdated  = "session.updated"
	LifecycleSessionArchived = "session.archived"
	LifecycleSessionDeleted  = "session.deleted"
	LifecycleRunStarted      = "run.started"
	LifecycleRunSettled      = "run.settled"
)

func (s *Service) publishSessionLifecycle(eventType string, session sessions.SessionV2, fields map[string]any) {
	if s == nil || s.lifecycleHub == nil {
		return
	}
	// Read-side hydration keeps the event's SessionMetadata contract consistent
	// with ListSessions/GetSession without writing anything after the successful
	// store commit that caused the event.
	session = s.hydrateSessionPricing(session)
	session = s.hydrateSessionDebug(session)
	payload := make(map[string]any, len(fields)+3)
	for key, value := range fields {
		payload[key] = value
	}
	metadata := sessionMetadataFromStore(session)
	payload["session"] = metadata
	payload["session_id"] = metadata.ID
	payload["project_id"] = metadata.ProjectID
	s.lifecycleHub.Publish(NewLifecycleEvent(eventType, payload))
}

func (s *Service) publishSessionCreated(session sessions.SessionV2) {
	s.publishSessionLifecycle(LifecycleSessionCreated, session, nil)
}

func (s *Service) publishSessionUpdated(session sessions.SessionV2, reason string) {
	fields := map[string]any{}
	if strings.TrimSpace(reason) != "" {
		fields["reason"] = reason
	}
	s.publishSessionLifecycle(LifecycleSessionUpdated, session, fields)
}

func (s *Service) publishSessionArchived(session sessions.SessionV2, rootID string, descendants []string) {
	fields := map[string]any{
		"cascade_root_id": strings.TrimSpace(rootID),
		"descendants":     append([]string(nil), descendants...),
	}
	s.publishSessionLifecycle(LifecycleSessionArchived, session, fields)
}

func (s *Service) publishSessionDeleted(id, projectID string, descendants []string) {
	if s == nil || s.lifecycleHub == nil {
		return
	}
	id = strings.TrimSpace(id)
	projectID = strings.TrimSpace(projectID)
	fields := map[string]any{
		// session/project are the compact identifiers used by incremental
		// clients. The *_id aliases preserve the naming used by run events.
		"session":     id,
		"session_id":  id,
		"project":     projectID,
		"project_id":  projectID,
		"descendants": append([]string(nil), descendants...),
	}
	s.lifecycleHub.Publish(NewLifecycleEvent(LifecycleSessionDeleted, fields))
}

type sessionDeletionCascade struct {
	rootID      string
	descendants []string
}

func sessionDeletionCascades(states []sessions.SessionV2) []sessionDeletionCascade {
	byID := make(map[string]sessions.SessionV2, len(states))
	children := make(map[string][]string)
	for _, state := range states {
		byID[state.ID] = state
		parentID := strings.TrimSpace(state.ParentSessionID)
		if parentID != "" {
			children[parentID] = append(children[parentID], state.ID)
		}
	}
	for parentID := range children {
		sort.Strings(children[parentID])
	}
	roots := make([]string, 0, len(states))
	for _, state := range states {
		parentID := strings.TrimSpace(state.ParentSessionID)
		if parentID == "" {
			roots = append(roots, state.ID)
			continue
		}
		if _, exists := byID[parentID]; !exists {
			roots = append(roots, state.ID)
		}
	}
	sort.Strings(roots)
	cascades := make([]sessionDeletionCascade, 0, len(roots))
	for _, rootID := range roots {
		seen := map[string]bool{rootID: true}
		queue := append([]string(nil), children[rootID]...)
		descendants := make([]string, 0, len(queue))
		for len(queue) > 0 {
			childID := queue[0]
			queue = queue[1:]
			if seen[childID] {
				continue
			}
			seen[childID] = true
			descendants = append(descendants, childID)
			queue = append(queue, children[childID]...)
		}
		cascades = append(cascades, sessionDeletionCascade{rootID: rootID, descendants: descendants})
	}
	return cascades
}

func (s *Service) publishRunStarted(run *CoordinatedSessionRun) {
	if s == nil || s.lifecycleHub == nil || run == nil {
		return
	}
	lastSeq := int64(0)
	if s.sessionStore != nil {
		if session, err := s.sessionStore.LoadState(run.SessionID()); err == nil {
			lastSeq = session.LastSeq
		}
	}
	s.publishRunLifecycle(LifecycleRunStarted, run, string(SessionRunRunning), lastSeq, nil, nil)
}

func (s *Service) publishRunSettled(run *CoordinatedSessionRun, result SessionMessageResult, runErr error) {
	if s == nil || run == nil {
		return
	}
	// Durable child completion delivery is independent of the best-effort SSE
	// hub. Keep it on the lifecycle fanout even when no Web subscriber exists.
	go s.onRunSettledForCompletion(run)
	if s.lifecycleHub == nil {
		return
	}
	status := string(run.Status())
	lastSeq := result.LastSeq
	var metadata *SessionMetadata
	if s.sessionStore != nil {
		if session, err := s.sessionStore.LoadState(run.SessionID()); err == nil {
			if session.LastSeq > lastSeq {
				lastSeq = session.LastSeq
			}
			session = s.hydrateSessionPricing(session)
			session = s.hydrateSessionDebug(session)
			value := sessionMetadataFromStore(session)
			metadata = &value
		}
	}
	s.publishRunLifecycle(LifecycleRunSettled, run, status, lastSeq, metadata, runErr)
}

func (s *Service) publishRunLifecycle(eventType string, run *CoordinatedSessionRun, status string, lastSeq int64, metadata *SessionMetadata, runErr error) {
	if s == nil || s.lifecycleHub == nil || run == nil {
		return
	}
	fields := map[string]any{
		"run":        run.ID(),
		"run_id":     run.ID(),
		"session":    run.SessionID(),
		"session_id": run.SessionID(),
		"status":     status,
		"last_seq":   lastSeq,
	}
	if eventType == LifecycleRunSettled {
		// last_seq is retained as the numeric compatibility field. The
		// decimal string is safe for browser consumers that cannot represent
		// every int64 exactly.
		fields["committed_revision"] = strconv.FormatInt(lastSeq, 10)
	}
	if metadata != nil {
		// settled metadata is a point-in-time copy. The pointer is only used
		// during JSON marshaling by Publish and is never mutated afterwards.
		fields["metadata"] = *metadata
		fields["session_metadata"] = *metadata
	}
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			fields["message"] = "run cancelled"
		} else {
			fields["message"] = "run failed"
		}
	}
	s.lifecycleHub.Publish(NewLifecycleEvent(eventType, fields))
}
