package execution

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

const (
	defaultSessionOutputMaxChars = 64 * 1024
	maximumSessionOutputMaxChars = 256 * 1024
	defaultSessionSearchLimit    = 20
	maximumSessionSearchLimit    = 100
)

type SessionSearchOptions struct {
	ProjectID       string
	NameRegex       string
	Statuses        []string
	IncludeArchived bool
	Limit           int
}

type SessionSearchMatch struct {
	SessionMetadata
	Name string `json:"name"`
}

type SessionSearchResult struct {
	Matches   []SessionSearchMatch `json:"matches"`
	Truncated bool                 `json:"truncated"`
}

type SessionOutputKind string

const (
	SessionOutputIntermediate SessionOutputKind = "intermediate"
	SessionOutputFinal        SessionOutputKind = "final"
	SessionOutputPartial      SessionOutputKind = "partial"
)

type SessionAssistantOutput struct {
	ItemID         string            `json:"item_id"`
	Seq            int64             `json:"seq"`
	TurnID         string            `json:"turn_id,omitempty"`
	AgentIteration int               `json:"agent_iteration,omitempty"`
	Content        string            `json:"content"`
	Kind           SessionOutputKind `json:"kind"`
	Complete       bool              `json:"complete"`
	HasToolCalls   bool              `json:"has_tool_calls"`
	ToolCallCount  int               `json:"tool_call_count"`
	Truncated      bool              `json:"truncated"`
}

type SessionInspection struct {
	SessionDetail
	Output *SessionAssistantOutput `json:"output"`
}

// SearchSessions finds sessions by their display label using Go's RE2 regular
// expression syntax. Search is always scoped to one active project; callers
// cannot use a session tool to enumerate other projects.
func (s *Service) SearchSessions(options SessionSearchOptions) (SessionSearchResult, error) {
	if s == nil || s.sessionStore == nil {
		return SessionSearchResult{}, fmt.Errorf("execution session store is not configured")
	}
	project, err := s.loadActiveProject(options.ProjectID)
	if err != nil {
		return SessionSearchResult{}, err
	}
	pattern := strings.TrimSpace(options.NameRegex)
	if pattern == "" {
		return SessionSearchResult{}, fmt.Errorf("session name regex is required")
	}
	matcher, err := regexp.Compile(pattern)
	if err != nil {
		return SessionSearchResult{}, fmt.Errorf("invalid session name regex: %w", err)
	}
	limit := options.Limit
	if limit <= 0 {
		limit = defaultSessionSearchLimit
	}
	if limit > maximumSessionSearchLimit {
		return SessionSearchResult{}, fmt.Errorf("session search limit cannot exceed %d", maximumSessionSearchLimit)
	}
	statuses := make(map[string]struct{}, len(options.Statuses))
	for _, status := range options.Statuses {
		status = strings.TrimSpace(status)
		switch status {
		case "running", "idle", "interrupted":
			statuses[status] = struct{}{}
		case "":
			return SessionSearchResult{}, fmt.Errorf("session search status must not be blank")
		default:
			return SessionSearchResult{}, fmt.Errorf("unknown session search status %q", status)
		}
	}

	states, err := s.sessionStore.ListStates(sessions.V2ListOptions{
		Archived: options.IncludeArchived,
		All:      options.IncludeArchived,
	})
	if err != nil {
		return SessionSearchResult{}, err
	}
	result := SessionSearchResult{Matches: []SessionSearchMatch{}}
	for _, session := range states {
		if session.ProjectID != project.ID {
			continue
		}
		name := CanonicalSessionName(session.ID, session.DisplayName)
		if !matcher.MatchString(name) {
			continue
		}
		metadata := sessionMetadataFromStore(session)
		if len(statuses) > 0 {
			if _, ok := statuses[metadata.Status]; !ok {
				continue
			}
		}
		if len(result.Matches) == limit {
			result.Truncated = true
			break
		}
		result.Matches = append(result.Matches, SessionSearchMatch{
			SessionMetadata: metadata,
			Name:            name,
		})
	}
	return result, nil
}

func CanonicalSessionName(sessionID, displayName string) string {
	if name := strings.TrimSpace(displayName); name != "" {
		return name
	}
	sessionID = strings.TrimSpace(sessionID)
	if len(sessionID) > 6 {
		sessionID = sessionID[len(sessionID)-6:]
	}
	return "Session " + sessionID
}

// InspectSession returns durable session metadata plus the newest persisted
// assistant item relevant to the current lifecycle state. Streaming deltas,
// reasoning, and tool results are deliberately not consulted.
func (s *Service) InspectSession(id string, maxOutputChars int) (SessionInspection, error) {
	if s == nil || s.sessionStore == nil {
		return SessionInspection{}, fmt.Errorf("execution session store is not configured")
	}
	if maxOutputChars <= 0 {
		maxOutputChars = defaultSessionOutputMaxChars
	}
	if maxOutputChars > maximumSessionOutputMaxChars {
		return SessionInspection{}, fmt.Errorf("session output limit cannot exceed %d characters", maximumSessionOutputMaxChars)
	}
	session, err := s.sessionStore.LoadExecutionState(id)
	if err != nil {
		return SessionInspection{}, err
	}
	session = s.hydrateSessionPricing(session)
	detail := sessionDetailFromStore(session)
	item, kind, ok := latestAssistantItemForSessionState(session, detail.Status)
	if !ok {
		return SessionInspection{SessionDetail: detail}, nil
	}
	content, err := s.sessionItemFullContent(id, item)
	if err != nil {
		return SessionInspection{}, err
	}
	content, truncated := truncateSessionOutput(content, maxOutputChars)
	return SessionInspection{
		SessionDetail: detail,
		Output: &SessionAssistantOutput{
			ItemID:         item.ID,
			Seq:            item.Seq,
			TurnID:         item.TurnID,
			AgentIteration: item.AgentIteration,
			Content:        content,
			Kind:           kind,
			Complete:       kind == SessionOutputFinal,
			HasToolCalls:   len(item.Message.ToolCalls) > 0,
			ToolCallCount:  len(item.Message.ToolCalls),
			Truncated:      truncated,
		},
	}, nil
}

func latestAssistantItemForSessionState(session sessions.SessionV2, status string) (sessions.SessionItem, SessionOutputKind, bool) {
	targetTurnID := ""
	kind := SessionOutputFinal
	switch status {
	case "running":
		targetTurnID = strings.TrimSpace(session.RunningTurnID)
		kind = SessionOutputIntermediate
	case "interrupted":
		targetTurnID = strings.TrimSpace(session.InterruptedTurnID)
		kind = SessionOutputPartial
	}
	var latest sessions.SessionItem
	found := false
	for _, item := range session.Items {
		if targetTurnID != "" && item.TurnID != targetTurnID {
			continue
		}
		if item.Kind != sessions.ItemKindMessage || item.Visibility != sessions.ItemVisibilityVisible || item.Audience != sessions.ItemAudienceModel {
			continue
		}
		if item.Message == nil || item.Message.Role != model.MessageRoleAssistant {
			continue
		}
		if !found || item.Seq > latest.Seq {
			latest = item
			found = true
		}
	}
	return latest, kind, found
}

func truncateSessionOutput(content string, limit int) (string, bool) {
	runes := []rune(content)
	if len(runes) <= limit {
		return content, false
	}
	return string(runes[:limit]), true
}
