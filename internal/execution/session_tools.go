package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

const (
	ToolSessionModels  = "session_models"
	ToolSessionStart   = "session_start"
	ToolSessionSearch  = "session_search"
	ToolSessionGet     = "session_get"
	ToolSessionHistory = "session_history"
	ToolSessionSend    = "session_send"
	ToolSessionWait    = "session_wait"
	ToolSessionStop    = "session_stop"

	maximumAgentSessionSpawnDepth  = 4
	maximumSessionDisplayNameRunes = 120
	defaultSessionWaitTimeout      = 30 * time.Second
)

// IsSessionTool reports whether name belongs to the durable session
// orchestration toolset. These tools are enabled explicitly through
// tools.enabled just like built-ins, but their executor is scoped to the
// calling session at runtime.
func IsSessionTool(name string) bool {
	switch name {
	case ToolSessionModels, ToolSessionStart, ToolSessionSearch, ToolSessionGet, ToolSessionHistory, ToolSessionSend, ToolSessionWait, ToolSessionStop:
		return true
	default:
		return false
	}
}

func sessionToolDefinitions() []model.Tool {
	return []model.Tool{
		{
			Name:        ToolSessionModels,
			Description: "List provider and model profiles available for starting a session in the current project. Credentials and provider secrets are never returned.",
			InputSchema: sessionToolObjectSchema(map[string]any{}, nil),
		},
		{
			Name:        ToolSessionStart,
			Description: "Create a named durable child session and start its first turn asynchronously. Omit provider and model to inherit this session's exact runtime model snapshot; otherwise provide both to select a configured model. Set on_settle=continue_parent to durably notify this parent and automatically start one new parent run after the child settles; the parent does not need to call session_wait.",
			InputSchema: sessionToolObjectSchema(map[string]any{
				"prompt": map[string]any{
					"type":        "string",
					"description": "Initial task or instructions for the child session.",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "Optional display name, up to 120 Unicode characters.",
				},
				"provider": map[string]any{
					"type":        "string",
					"description": "Optional configured provider. Must be supplied together with model.",
				},
				"model": map[string]any{
					"type":        "string",
					"description": "Optional configured model profile. Must be supplied together with provider.",
				},
				"reasoning_level": map[string]any{
					"type":        "string",
					"description": "Optional configured reasoning level. When omitted with provider/model, the caller's exact parameters are inherited.",
				},
				"on_settle": map[string]any{
					"type":        "string",
					"enum":        []any{"none", "continue_parent"},
					"default":     "none",
					"description": "Completion behavior for the child's initial run. none leaves the parent untouched; continue_parent durably wakes the parent with a compact completion notification after the child settles.",
				},
			}, []any{"prompt"}),
		},
		{
			Name:        ToolSessionSearch,
			Description: "Search durable sessions in the current project by display name using Go RE2 regular-expression syntax.",
			InputSchema: sessionToolObjectSchema(map[string]any{
				"name_regex": map[string]any{
					"type":        "string",
					"description": "RE2 expression matched against the session display name (or generated fallback name).",
				},
				"statuses": map[string]any{
					"type":        "array",
					"description": "Optional status filter.",
					"items":       map[string]any{"type": "string", "enum": []any{"running", "idle", "interrupted"}},
				},
				"include_archived": map[string]any{
					"type":        "boolean",
					"description": "Include archived sessions. Defaults to false.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum matches to return.",
					"minimum":     1,
					"maximum":     maximumSessionSearchLimit,
				},
			}, []any{"name_regex"}),
		},
		{
			Name:        ToolSessionGet,
			Description: "Inspect a session in the current project and return its latest persisted assistant item. While running this is the latest persisted intermediate item; streaming deltas never count.",
			InputSchema: sessionToolObjectSchema(map[string]any{
				"session_id": map[string]any{
					"type":        "string",
					"description": "Durable session id.",
				},
				"max_output_chars": map[string]any{
					"type":        "integer",
					"description": "Maximum Unicode characters of assistant output to return.",
					"minimum":     1,
					"maximum":     maximumSessionOutputMaxChars,
				},
			}, []any{"session_id"}),
		},
		{
			Name:        ToolSessionHistory,
			Description: "Read one page of persisted, user-visible conversation history for a session in the current project. Use session_search to resolve a name to a session id first.",
			InputSchema: sessionToolObjectSchema(map[string]any{
				"session_id": map[string]any{
					"type":        "string",
					"description": "Durable session id.",
				},
				"cursor": map[string]any{
					"type":        "integer",
					"description": "Pagination cursor: an item sequence number taken from a previous page (oldest_seq or newest_seq). When omitted, the newest page is returned.",
					"minimum":     1,
				},
				"direction": map[string]any{
					"type":        "string",
					"enum":        []any{"before", "after"},
					"description": "Selects which side of cursor to read: \"before\" returns older items, \"after\" returns newer items. Required when cursor is provided; omit together with cursor for the newest page.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum items to return; defaults to 50.",
					"minimum":     1,
					"maximum":     maximumSessionChatItemsLimit,
				},
				"align_turn": map[string]any{
					"type":        "boolean",
					"description": "Align the page to whole turns: the oldest edge is extended backwards so the page never starts mid-turn and always contains at least one complete turn; it may exceed limit for long turns. Defaults to false.",
				},
			}, []any{"session_id"}),
		},
		{
			Name:        ToolSessionSend,
			Description: "Send input to a session in the current project. mode=steer strictly targets the currently active turn and fails if its steer gate has closed; mode=queue schedules an independent next turn, starting an idle session when needed.",
			InputSchema: sessionToolObjectSchema(map[string]any{
				"session_id": map[string]any{
					"type":        "string",
					"description": "Durable target session id.",
				},
				"mode": map[string]any{
					"type":        "string",
					"enum":        []any{"steer", "queue"},
					"description": "Strictly steer the active turn, or queue a separate turn.",
				},
				"message": map[string]any{
					"type":        "string",
					"description": "Message to deliver verbatim.",
				},
			}, []any{"session_id", "mode", "message"}),
		},
		{
			Name:        ToolSessionWait,
			Description: "Wait for the run active at invocation time to finish, up to timeout_ms, then return persisted session output. A timeout never cancels the session.",
			InputSchema: sessionToolObjectSchema(map[string]any{
				"session_id": map[string]any{
					"type":        "string",
					"description": "Durable target session id. Waiting on the calling session itself is rejected.",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": "Maximum wait in milliseconds; defaults to 30000.",
					"minimum":     0,
				},
				"max_output_chars": map[string]any{
					"type":        "integer",
					"description": "Maximum Unicode characters of assistant output to return.",
					"minimum":     1,
					"maximum":     maximumSessionOutputMaxChars,
				},
			}, []any{"session_id"}),
		},
		{
			Name:        ToolSessionStop,
			Description: "Request cancellation of the active run for a session in the current project. Stopping an idle session is an idempotent no-op; the durable session is not deleted.",
			InputSchema: sessionToolObjectSchema(map[string]any{
				"session_id": map[string]any{
					"type":        "string",
					"description": "Durable target session id. Stopping the calling session itself is rejected.",
				},
			}, []any{"session_id"}),
		},
	}
}

func sessionToolObjectSchema(properties map[string]any, required []any) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func enabledSessionToolSchemas(enabled []string) []model.Tool {
	byName := make(map[string]model.Tool)
	for _, definition := range sessionToolDefinitions() {
		byName[definition.Name] = definition
	}
	schemas := make([]model.Tool, 0, len(enabled))
	for _, name := range enabled {
		if definition, ok := byName[name]; ok {
			schemas = append(schemas, definition)
		}
	}
	return schemas
}

// enabledToolsForAgentChild turns an agent-created child into a leaf worker.
// It may inspect and wait on project sessions, but it cannot recursively spawn
// work or inject input into another session. The runtime applies this filter as
// well as session creation so children persisted by older releases are covered.
func enabledToolsForAgentChild(enabled []string) []string {
	filtered := make([]string, 0, len(enabled))
	for _, name := range enabled {
		switch name {
		case ToolSessionStart, ToolSessionSend:
			continue
		default:
			filtered = append(filtered, name)
		}
	}
	return filtered
}

type sessionToolExecutor struct {
	service     *Service
	coordinator *SessionRunCoordinator
	caller      SessionDetail
}

func newSessionToolExecutor(service *Service, coordinator *SessionRunCoordinator, caller sessions.SessionV2) *sessionToolExecutor {
	return &sessionToolExecutor{
		service:     service,
		coordinator: coordinator,
		caller:      sessionDetailFromStore(caller),
	}
}

func (e *sessionToolExecutor) Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return sessionToolError(name, "canceled", "session tool call was canceled")
	}
	if e == nil || e.service == nil {
		return sessionToolError(name, "service_unavailable", "session service is not configured")
	}
	switch name {
	case ToolSessionModels:
		return e.models(name)
	case ToolSessionStart:
		return e.start(name, arguments)
	case ToolSessionSearch:
		return e.search(name, arguments)
	case ToolSessionGet:
		return e.get(name, arguments)
	case ToolSessionHistory:
		return e.history(name, arguments)
	case ToolSessionSend:
		return e.send(name, arguments)
	case ToolSessionWait:
		return e.wait(ctx, name, arguments)
	case ToolSessionStop:
		return e.stop(name, arguments)
	default:
		return model.ToolResult{}, fmt.Errorf("session tool %q is not registered", name)
	}
}

func (e *sessionToolExecutor) models(toolName string) (model.ToolResult, error) {
	options, err := e.service.ConfiguredSessionModels(e.caller.ProjectID)
	if err != nil {
		return sessionToolFailure(toolName, err)
	}
	models := make([]map[string]any, 0, len(options.Models))
	for _, option := range options.Models {
		models = append(models, map[string]any{
			"provider":                option.Provider,
			"model":                   option.ModelProfile,
			"model_profile":           option.ModelProfile,
			"model_id":                option.ModelID,
			"reasoning_levels":        option.ReasoningLevels,
			"default_reasoning_level": option.DefaultReasoningLevel,
		})
	}
	return sessionToolResult(toolName, map[string]any{
		"ok":               true,
		"models":           models,
		"default_provider": options.DefaultProvider,
		"default_model":    options.DefaultModel,
	})
}

func (e *sessionToolExecutor) start(toolName string, arguments map[string]any) (model.ToolResult, error) {
	prompt, err := requiredRawSessionString(arguments, "prompt")
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	name, err := optionalTrimmedSessionString(arguments, "name")
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	if utf8.RuneCountInString(name) > maximumSessionDisplayNameRunes {
		return sessionToolError(toolName, "invalid_arguments", fmt.Sprintf("name cannot exceed %d Unicode characters", maximumSessionDisplayNameRunes))
	}
	provider, err := optionalTrimmedSessionString(arguments, "provider")
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	modelProfile, err := optionalTrimmedSessionString(arguments, "model")
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	reasoningLevel, err := optionalTrimmedSessionString(arguments, "reasoning_level")
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	onSettle, err := optionalTrimmedSessionString(arguments, "on_settle")
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	if _, supplied := arguments["on_settle"]; supplied && onSettle == "" {
		return sessionToolError(toolName, "invalid_arguments", "on_settle must not be blank")
	}
	if onSettle == "" {
		onSettle = "none"
	}
	if onSettle != "none" && onSettle != "continue_parent" {
		return sessionToolError(toolName, "invalid_arguments", "on_settle must be one of none or continue_parent")
	}
	if (provider == "") != (modelProfile == "") {
		return sessionToolError(toolName, "invalid_arguments", "provider and model must be supplied together")
	}
	if e.caller.SpawnDepth >= maximumAgentSessionSpawnDepth {
		return sessionToolError(toolName, "spawn_depth_exceeded", fmt.Sprintf("session spawn depth cannot exceed %d", maximumAgentSessionSpawnDepth))
	}
	if e.coordinator == nil {
		return sessionToolError(toolName, "coordinator_unavailable", "session run coordinator is not configured")
	}

	var child SessionDetail
	if provider == "" && reasoningLevel == "" {
		child, err = e.service.CreateInheritedSession(e.caller.ID, name)
	} else {
		if provider == "" {
			provider = e.caller.Provider
			modelProfile = e.caller.ModelProfile
		}
		cwd := strings.TrimSpace(e.caller.CWD)
		if cwd == "" {
			cwd = e.caller.CreatedCWD
		}
		child, err = e.service.CreateConfiguredSession(e.caller.ProjectID, ConfiguredSessionOptions{
			CWD:             cwd,
			DisplayName:     name,
			ParentSessionID: e.caller.ID,
			Provider:        provider,
			ModelProfile:    modelProfile,
			ReasoningLevel:  reasoningLevel,
		})
	}
	if err != nil {
		return sessionToolFailure(toolName, err)
	}
	run, err := e.coordinator.Start(child.ID, SessionMessageInput{Content: prompt}, nil)
	if err != nil {
		code := sessionToolErrorCode(err)
		return sessionToolErrorFields(toolName, code, safeSessionToolError(err), map[string]any{
			"session_id": child.ID,
			"created":    true,
			"on_settle":  onSettle,
		})
	}
	if onSettle == "continue_parent" {
		if err := e.service.RegisterContinueParentSubscription(e.caller.ID, child.ID, run.ID()); err != nil {
			return sessionToolErrorFields(toolName, sessionToolErrorCode(err), safeSessionToolError(err), map[string]any{
				"session_id": child.ID,
				"run_id":     run.ID(),
				"created":    true,
				"on_settle":  onSettle,
			})
		}
	}
	return sessionToolResult(toolName, map[string]any{
		"ok":                true,
		"session_id":        child.ID,
		"run_id":            run.ID(),
		"name":              CanonicalSessionName(child.ID, child.DisplayName),
		"status":            "running",
		"provider":          child.Provider,
		"model":             child.ModelProfile,
		"model_id":          child.ModelID,
		"on_settle":         onSettle,
		"parent_session_id": child.ParentSessionID,
		"root_session_id":   child.RootSessionID,
		"spawn_depth":       child.SpawnDepth,
	})
}

func (e *sessionToolExecutor) search(toolName string, arguments map[string]any) (model.ToolResult, error) {
	pattern, err := requiredTrimmedSessionString(arguments, "name_regex")
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return sessionToolError(toolName, "invalid_arguments", fmt.Sprintf("name_regex is not a valid RE2 expression: %v", err))
	}
	statuses, err := optionalSessionStringSlice(arguments, "statuses")
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	for _, status := range statuses {
		switch strings.TrimSpace(status) {
		case "running", "idle", "interrupted":
		case "":
			return sessionToolError(toolName, "invalid_arguments", "statuses must not contain a blank value")
		default:
			return sessionToolError(toolName, "invalid_arguments", fmt.Sprintf("statuses contains unknown status %q", status))
		}
	}
	includeArchived, err := optionalSessionBool(arguments, "include_archived", false)
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	limit, err := optionalSessionIntegerInRange(arguments, "limit", 0, 1, maximumSessionSearchLimit)
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	result, err := e.service.SearchSessions(SessionSearchOptions{
		ProjectID:       e.caller.ProjectID,
		NameRegex:       pattern,
		Statuses:        statuses,
		IncludeArchived: includeArchived,
		Limit:           limit,
	})
	if err != nil {
		return sessionToolFailure(toolName, err)
	}
	return sessionToolResult(toolName, map[string]any{
		"ok":        true,
		"matches":   result.Matches,
		"truncated": result.Truncated,
	})
}

func (e *sessionToolExecutor) get(toolName string, arguments map[string]any) (model.ToolResult, error) {
	target, result, ok := e.targetFromArguments(toolName, arguments)
	if !ok {
		return result, nil
	}
	maxOutputChars, err := optionalSessionIntegerInRange(arguments, "max_output_chars", 0, 1, maximumSessionOutputMaxChars)
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	inspection, err := e.service.InspectSession(target.ID, maxOutputChars)
	if err != nil {
		return sessionToolFailure(toolName, err)
	}
	return sessionToolResult(toolName, map[string]any{"ok": true, "inspection": inspection})
}

func (e *sessionToolExecutor) history(toolName string, arguments map[string]any) (model.ToolResult, error) {
	target, result, ok := e.targetFromArguments(toolName, arguments)
	if !ok {
		return result, nil
	}
	cursor, err := optionalSessionIntegerInRange(arguments, "cursor", 0, 1, int(^uint(0)>>1))
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	direction, err := optionalTrimmedSessionString(arguments, "direction")
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	if direction != "" && direction != "before" && direction != "after" {
		return sessionToolError(toolName, "invalid_arguments", "direction must be \"before\" (items older than cursor) or \"after\" (items newer than cursor)")
	}
	if direction != "" && cursor == 0 {
		return sessionToolError(toolName, "invalid_arguments", "direction requires cursor; omit both for the newest page")
	}
	if cursor > 0 && direction == "" {
		return sessionToolError(toolName, "invalid_arguments", "cursor requires direction: \"before\" returns items older than cursor, \"after\" returns items newer than cursor")
	}
	// before_seq/after_seq are the retired cursor parameters. They stay
	// accepted so agents started before the cursor/direction contract keep
	// working, but they are no longer advertised in the tool schema, and
	// misuse errors teach the current contract.
	beforeSeq, err := optionalSessionIntegerInRange(arguments, "before_seq", 0, 1, int(^uint(0)>>1))
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	afterSeq, err := optionalSessionIntegerInRange(arguments, "after_seq", 0, 1, int(^uint(0)>>1))
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	if beforeSeq > 0 && afterSeq > 0 {
		return sessionToolError(toolName, "invalid_arguments", "before_seq and after_seq cannot be combined; retry with exactly one of them, or use cursor with direction: \"before\" returns items older than cursor, \"after\" returns items newer than cursor")
	}
	if cursor > 0 && (beforeSeq > 0 || afterSeq > 0) {
		return sessionToolError(toolName, "invalid_arguments", "cursor cannot be combined with before_seq/after_seq; use cursor with direction only")
	}
	if cursor > 0 {
		if direction == "after" {
			afterSeq = cursor
		} else {
			beforeSeq = cursor
		}
	}
	limit, err := optionalSessionIntegerInRange(arguments, "limit", 0, 1, maximumSessionChatItemsLimit)
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	alignTurn, err := optionalSessionBool(arguments, "align_turn", false)
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	page, err := e.service.GetSessionChatItemsPage(target.ID, SessionItemsOptions{
		BeforeSeq: int64(beforeSeq),
		AfterSeq:  int64(afterSeq),
		Limit:     limit,
		AlignTurn: alignTurn,
	})
	if err != nil {
		return sessionToolFailure(toolName, err)
	}
	return sessionToolResult(toolName, map[string]any{
		"ok":         true,
		"session_id": target.ID,
		"history":    page,
	})
}

func (e *sessionToolExecutor) send(toolName string, arguments map[string]any) (model.ToolResult, error) {
	target, result, ok := e.targetFromArguments(toolName, arguments)
	if !ok {
		return result, nil
	}
	if target.Archived {
		return sessionToolError(toolName, "session_archived", "archived session cannot receive messages")
	}
	mode, err := requiredTrimmedSessionString(arguments, "mode")
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	message, err := requiredRawSessionString(arguments, "message")
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	if e.coordinator == nil {
		return sessionToolError(toolName, "coordinator_unavailable", "session run coordinator is not configured")
	}

	switch mode {
	case "steer":
		run, active := e.coordinator.ActiveForSession(target.ID)
		if !active {
			return sessionToolError(toolName, "session_not_steerable", "session has no active turn accepting steer messages")
		}
		if err := run.TrySteer(message); err != nil {
			return sessionToolFailure(toolName, err)
		}
		return sessionToolResult(toolName, map[string]any{
			"ok": true, "session_id": target.ID, "run_id": run.ID(), "delivery": "steered",
		})
	case "queue":
		return e.queueMessage(toolName, target.ID, message)
	default:
		return sessionToolError(toolName, "invalid_arguments", "mode must be either steer or queue")
	}
}

func (e *sessionToolExecutor) queueMessage(toolName, sessionID, message string) (model.ToolResult, error) {
	event := PromptEvent{Source: PromptSourceAgentSession, Mode: PromptModeEnqueueTurn, Content: message}
	for attempt := 0; attempt < 3; attempt++ {
		if active, ok := e.coordinator.ActiveForSession(sessionID); ok {
			if _, err := active.Enqueue(event); err == nil {
				return sessionToolResult(toolName, map[string]any{
					"ok": true, "session_id": sessionID, "run_id": active.ID(), "delivery": "queued",
				})
			} else if !errors.Is(err, ErrSessionRunSettled) {
				return sessionToolFailure(toolName, err)
			}
		}
		run, err := e.coordinator.Start(sessionID, SessionMessageInput{Content: message}, nil)
		if err == nil {
			return sessionToolResult(toolName, map[string]any{
				"ok": true, "session_id": sessionID, "run_id": run.ID(), "delivery": "started",
			})
		}
		if !errors.Is(err, ErrSessionBusy) {
			return sessionToolFailure(toolName, err)
		}
	}
	return sessionToolError(toolName, "session_busy", "session run changed while queueing the message; retry")
}

func (e *sessionToolExecutor) wait(ctx context.Context, toolName string, arguments map[string]any) (model.ToolResult, error) {
	target, result, ok := e.targetFromArguments(toolName, arguments)
	if !ok {
		return result, nil
	}
	if target.ID == e.caller.ID {
		return sessionToolError(toolName, "self_wait_forbidden", "a session cannot wait for its own active run")
	}
	timeoutMS, err := optionalSessionInteger(arguments, "timeout_ms", int(defaultSessionWaitTimeout/time.Millisecond))
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	if _, supplied := arguments["timeout_ms"]; supplied && timeoutMS < 0 {
		return sessionToolError(toolName, "invalid_arguments", "timeout_ms must be non-negative")
	}
	maxOutputChars, err := optionalSessionIntegerInRange(arguments, "max_output_chars", 0, 1, maximumSessionOutputMaxChars)
	if err != nil {
		return sessionToolError(toolName, "invalid_arguments", err.Error())
	}
	if e.coordinator == nil {
		return sessionToolError(toolName, "coordinator_unavailable", "session run coordinator is not configured")
	}

	run, active := e.coordinator.ActiveForSession(target.ID)
	if !active {
		return e.waitResult(toolName, target.ID, nil, true, false, maxOutputChars)
	}
	timer := time.NewTimer(time.Duration(timeoutMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-run.Done():
		_, _ = run.Wait()
		return e.waitResult(toolName, target.ID, run, true, false, maxOutputChars)
	case <-timer.C:
		return e.waitResult(toolName, target.ID, run, false, true, maxOutputChars)
	case <-ctx.Done():
		return sessionToolError(toolName, "canceled", "session wait was canceled")
	}
}

func (e *sessionToolExecutor) waitResult(toolName, sessionID string, run *CoordinatedSessionRun, completed, timedOut bool, maxOutputChars int) (model.ToolResult, error) {
	inspection, err := e.service.InspectSession(sessionID, maxOutputChars)
	if err != nil {
		return sessionToolFailure(toolName, err)
	}
	payload := map[string]any{
		"ok":         true,
		"session_id": sessionID,
		"completed":  completed,
		"timed_out":  timedOut,
		"inspection": inspection,
	}
	if run != nil {
		payload["run_id"] = run.ID()
		payload["run_status"] = string(run.Status())
	}
	return sessionToolResult(toolName, payload)
}

func (e *sessionToolExecutor) stop(toolName string, arguments map[string]any) (model.ToolResult, error) {
	target, result, ok := e.targetFromArguments(toolName, arguments)
	if !ok {
		return result, nil
	}
	if target.ID == e.caller.ID {
		return sessionToolError(toolName, "self_stop_forbidden", "a session cannot stop its own active run")
	}
	if e.coordinator == nil {
		return sessionToolError(toolName, "coordinator_unavailable", "session run coordinator is not configured")
	}
	run, active := e.coordinator.ActiveForSession(target.ID)
	if !active {
		return sessionToolResult(toolName, map[string]any{
			"ok": true, "session_id": target.ID, "status": "idle", "cancellation_requested": false,
		})
	}
	run.Cancel()
	return sessionToolResult(toolName, map[string]any{
		"ok": true, "session_id": target.ID, "run_id": run.ID(), "status": "cancellation_requested", "cancellation_requested": true,
	})
}

func (e *sessionToolExecutor) targetFromArguments(toolName string, arguments map[string]any) (SessionDetail, model.ToolResult, bool) {
	id, err := requiredTrimmedSessionString(arguments, "session_id")
	if err != nil {
		result, _ := sessionToolError(toolName, "invalid_arguments", err.Error())
		return SessionDetail{}, result, false
	}
	target, err := e.service.GetSession(id)
	if err != nil {
		result, _ := sessionToolFailure(toolName, err)
		return SessionDetail{}, result, false
	}
	if target.ProjectID != e.caller.ProjectID {
		result, _ := sessionToolError(toolName, "session_forbidden", "target session is outside the current project")
		return SessionDetail{}, result, false
	}
	return target, model.ToolResult{}, true
}

func sessionToolResult(toolName string, payload any) (model.ToolResult, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return model.ToolResult{}, err
	}
	return model.ToolResult{Name: toolName, Content: string(data)}, nil
}

func sessionToolError(toolName, code, message string) (model.ToolResult, error) {
	return sessionToolErrorFields(toolName, code, message, nil)
}

func sessionToolErrorFields(toolName, code, message string, fields map[string]any) (model.ToolResult, error) {
	payload := map[string]any{"ok": false, "code": code, "error": message}
	for key, value := range fields {
		payload[key] = value
	}
	result, err := sessionToolResult(toolName, payload)
	result.IsError = true
	return result, err
}

func sessionToolFailure(toolName string, err error) (model.ToolResult, error) {
	return sessionToolError(toolName, sessionToolErrorCode(err), safeSessionToolError(err))
}

func sessionToolErrorCode(err error) string {
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		return "session_not_found"
	case errors.Is(err, ErrSessionNotSteerable):
		return "session_not_steerable"
	case errors.Is(err, ErrSessionRunCoordinatorCapacity):
		return "run_capacity_reached"
	case errors.Is(err, ErrSessionRunCoordinatorClosed):
		return "coordinator_unavailable"
	case errors.Is(err, ErrSessionBusy), errors.Is(err, ErrSessionRunSettled):
		return "session_busy"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "session_operation_failed"
	}
}

func safeSessionToolError(err error) string {
	switch sessionToolErrorCode(err) {
	case "session_not_found":
		return "session was not found"
	case "session_not_steerable":
		return "session has no active turn accepting steer messages"
	case "run_capacity_reached":
		return "active session run capacity has been reached"
	case "coordinator_unavailable":
		return "session run coordinator is unavailable"
	case "session_busy":
		return "session is busy; retry the operation"
	case "canceled":
		return "session operation was canceled"
	default:
		if err == nil {
			return "session operation failed"
		}
		return err.Error()
	}
}

func requiredRawSessionString(arguments map[string]any, name string) (string, error) {
	value, ok := arguments[name]
	if !ok {
		return "", fmt.Errorf("%s is required", name)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s must not be blank", name)
	}
	return text, nil
}

func requiredTrimmedSessionString(arguments map[string]any, name string) (string, error) {
	value, err := requiredRawSessionString(arguments, name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func optionalTrimmedSessionString(arguments map[string]any, name string) (string, error) {
	value, ok := arguments[name]
	if !ok {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return strings.TrimSpace(text), nil
}

func optionalSessionBool(arguments map[string]any, name string, defaultValue bool) (bool, error) {
	value, ok := arguments[name]
	if !ok {
		return defaultValue, nil
	}
	boolean, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return boolean, nil
}

func optionalSessionStringSlice(arguments map[string]any, name string) ([]string, error) {
	value, ok := arguments[name]
	if !ok {
		return nil, nil
	}
	switch values := value.(type) {
	case []string:
		return append([]string(nil), values...), nil
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("%s must contain only strings", name)
			}
			result = append(result, text)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("%s must be an array of strings", name)
	}
}

func optionalSessionInteger(arguments map[string]any, name string, defaultValue int) (int, error) {
	value, ok := arguments[name]
	if !ok {
		return defaultValue, nil
	}
	var parsed int64
	switch value := value.(type) {
	case int:
		return value, nil
	case int64:
		parsed = value
	case float64:
		if value != float64(int64(value)) {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		parsed = int64(value)
	case json.Number:
		integer, err := strconv.ParseInt(value.String(), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		parsed = integer
	default:
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	converted := int(parsed)
	if int64(converted) != parsed {
		return 0, fmt.Errorf("%s is outside the supported integer range", name)
	}
	return converted, nil
}

// optionalSessionIntegerInRange validates executor calls independently of the
// model-facing JSON Schema. Tool calls normally pass schema validation first,
// but keeping the boundary here prevents alternate callers and tests from
// silently turning explicit zero or negative values into service defaults.
func optionalSessionIntegerInRange(arguments map[string]any, name string, defaultValue, minimum, maximum int) (int, error) {
	value, err := optionalSessionInteger(arguments, name, defaultValue)
	if err != nil {
		return 0, err
	}
	if _, supplied := arguments[name]; supplied && (value < minimum || value > maximum) {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minimum, maximum)
	}
	return value, nil
}
