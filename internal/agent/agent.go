// Package agent is the application core: it drives the model provider and
// tool execution, and owns the session state layer. Every durable fact goes
// through Session's commit path; the storage package only ever sees
// records. All parameters are injected at construction — nothing reads
// configuration at runtime.
package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/storage"
)

// DefaultMaxIterations bounds one run when Options.MaxIterations is unset.
const DefaultMaxIterations = 8

// ToolExecutor executes one tool call by name.
type ToolExecutor interface {
	Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error)
}

// Options configures Agent construction.
type Options struct {
	Storage      storage.Store
	Tools        []model.Tool
	ToolExecutor ToolExecutor
	// SystemPrompt is the UNRENDERED template: {{time.now}} and its
	// siblings render at every request assembly, not here (§15.2).
	SystemPrompt  string
	MaxIterations int
	CWD           string
	// ModelResolver maps a session's recorded meta to its live model
	// configuration (§15.2). Required: the agent carries no model of
	// its own.
	ModelResolver ModelResolver
	// AutoCompact compacts the context automatically when the request
	// approaches the budget (see budget.go). The budget itself is
	// per-session, derived from the session's resolved model.
	AutoCompact bool
}

// Agent is one configured application instance: the project-side
// assembly (tools, prompt template, cwd) plus the model resolver. The
// model itself lives on the SESSION: each session resolves its
// recorded meta through the resolver (§15.2).
type Agent struct {
	store         storage.Store
	tools         []model.Tool
	toolExecutor  ToolExecutor
	systemPrompt  string // unrendered template
	resolver      ModelResolver
	maxIterations int
	cwd           string
	autoCompact   bool
	// clock is the time source for prompt rendering; tests inject a
	// fake to pin {{time.now}} refreshes without sleeping.
	clock func() time.Time
}

// New validates and assembles an agent.
func New(options Options) (*Agent, error) {
	if options.Storage == nil {
		return nil, fmt.Errorf("agent storage is required")
	}
	if options.ModelResolver == nil {
		return nil, fmt.Errorf("agent model resolver is required")
	}
	maxIterations := options.MaxIterations
	if maxIterations <= 0 {
		maxIterations = DefaultMaxIterations
	}
	return &Agent{
		store:         options.Storage,
		tools:         options.Tools,
		toolExecutor:  options.ToolExecutor,
		systemPrompt:  options.SystemPrompt,
		resolver:      options.ModelResolver,
		maxIterations: maxIterations,
		cwd:           options.CWD,
		autoCompact:   options.AutoCompact,
		clock:         time.Now,
	}, nil
}

// CWD returns the working directory tools operate in.
func (a *Agent) CWD() string { return a.cwd }

// NewSession creates a fresh, lazily persisted session. The session ID is
// minted immediately and available before any record exists. The
// session.created record is prepared here and committed ahead of the
// session's first real record — so an empty session never touches the
// storage.
func (a *Agent) NewSession(ns string, meta storage.SessionMeta) (*Session, error) {
	stored, err := a.store.Create(ns)
	if err != nil {
		return nil, err
	}
	resolved, err := resolveMeta(a.resolver, meta)
	if err != nil {
		stored.Close()
		return nil, err
	}
	return &Session{
		agent:    a,
		storage:  stored,
		state:    storage.NewState(stored.ID()),
		meta:     meta,
		resolved: resolved,
		budget:   resolveContextBudget(resolved.ContextWindow, resolved.OutputLimit),
		pendingCreated: &storage.Record{
			ItemID:         storage.NewSessionCreatedID(),
			Type:           storage.TypeSessionCreated,
			Provider:       meta.Provider,
			Model:          meta.Model,
			ReasoningLevel: meta.ReasoningLevel,
			CWD:            a.cwd,
			Time:           storage.NewTime(),
		},
	}, nil
}

// OpenSession reopens a stored session by ID inside the namespace,
// rebuilding its state by folding the records. The ns is opaque to the
// agent (§15.1) — hosts derive it from their own project semantics.
func (a *Agent) OpenSession(ns, id string) (*Session, error) {
	return a.openSession(func(sessionID string) (storage.Session, error) {
		return a.store.Open(ns, sessionID)
	}, id)
}

func (a *Agent) openSession(open func(string) (storage.Session, error), id string) (*Session, error) {
	stored, err := open(id)
	if err != nil {
		return nil, err
	}
	records, err := stored.Records()
	if err != nil {
		stored.Close()
		return nil, err
	}
	state := storage.NewState(stored.ID())
	for _, record := range records {
		storage.Fold(state, record)
	}
	// The session's model configuration comes from its session.created
	// record (§15.2). Two degenerate cases resolve to ErrModelUnavailable
	// (the resolver rejects empty provider/model — defaults are a
	// creation-time concern, never a resolution-time fallback):
	// (i) an empty record set (a lazily-created session that was never
	// committed) has no created record — its meta is zero and the
	// resolver refuses it; (ii) a created record with an empty provider
	// or model (a hand-edited or foreign log). Hosts that need a model
	// for case (i) create the session with an explicit meta instead of
	// opening a never-committed one.
	var meta storage.SessionMeta
	for _, record := range records {
		if record.Type == storage.TypeSessionCreated {
			meta = storage.SessionMeta{
				Provider:       record.Provider,
				Model:          record.Model,
				ReasoningLevel: record.ReasoningLevel,
			}
			break
		}
	}
	resolved, err := resolveMeta(a.resolver, meta)
	if err != nil {
		stored.Close()
		return nil, err
	}
	// Rebuild the since-compacted cooldown counter from the active
	// segment (§14.4): the user inputs after the last context.compacted
	// (all of them when the segment has none). The soft-attempt counter
	// stays memory-only — a restart re-attempts immediately.
	inputsSinceCompacted := 0
	hasCompacted := false
	for _, record := range records {
		switch record.Type {
		case storage.TypeUserInput:
			inputsSinceCompacted++
		case storage.TypeContextCompacted:
			inputsSinceCompacted = 0
			hasCompacted = true
		}
	}
	return &Session{
		agent: a, storage: stored, state: state, records: records,
		meta:                 meta,
		resolved:             resolved,
		budget:               resolveContextBudget(resolved.ContextWindow, resolved.OutputLimit),
		inputsSinceCompacted: inputsSinceCompacted,
		hasCompacted:         hasCompacted,
	}, nil
}

// renderSystemPrompt expands the agent's template with the current
// clock (§15.2: {{time.now}} refreshes at every assembly — the agent
// is long-lived, a frozen timestamp would lie).
func (a *Agent) renderSystemPrompt() string {
	if a.systemPrompt == "" {
		return ""
	}
	return strings.ReplaceAll(a.systemPrompt, "{{time.now}}", a.clock().UTC().Format(time.RFC3339))
}

// buildRequest assembles the model request from the folded context:
// the rendered system prompt first, then the sanitized wire history.
// A SESSION method: the model, parameters, and limits come from the
// session's resolved configuration (§15.2).
func (s *Session) buildRequest(context []model.Message) model.Request {
	a := s.agent
	messages := make([]model.Message, 0, len(context)+1)
	if prompt := a.renderSystemPrompt(); prompt != "" {
		messages = append(messages, model.Message{Role: model.MessageRoleSystem, Content: prompt})
	}
	messages = append(messages, sanitizeContext(context)...)
	return model.Request{
		Model:         s.meta.Model,
		Messages:      messages,
		Tools:         a.tools,
		Parameters:    s.resolved.Parameters,
		DeveloperRole: s.resolved.DeveloperRole,
		MaxTokens:     s.resolved.OutputLimit,
	}
}

// sanitizeContext patches a folded history so it always satisfies the
// provider contract: every assistant tool call is followed by its tool
// result. A run interrupted or crashed mid-tool leaves such gaps; the
// missing results are synthesized as interrupted errors in call order.
func sanitizeContext(context []model.Message) []model.Message {
	result := make([]model.Message, 0, len(context))
	var pending []model.ToolCall
	// flush appends placeholder results for every unresolved tool call,
	// in call order, so the next message follows a complete exchange.
	flush := func() {
		for _, call := range pending {
			result = append(result, model.Message{
				Role:       model.MessageRoleTool,
				ToolCallID: call.ID,
				Content:    "[tool execution interrupted]",
				IsError:    true,
			})
		}
		pending = nil
	}
	for _, message := range context {
		switch {
		case message.Role == model.MessageRoleAssistant && len(message.ToolCalls) > 0:
			flush()
			pending = append(pending, message.ToolCalls...)
			result = append(result, message)
		case message.Role == model.MessageRoleTool:
			for i, call := range pending {
				if call.ID == message.ToolCallID {
					pending = append(pending[:i], pending[i+1:]...)
					break
				}
			}
			result = append(result, message)
		default:
			// A user message (or a no-tool assistant message) cannot follow
			// unresolved tool calls: patch the gap first.
			flush()
			result = append(result, message)
		}
	}
	flush()
	return result
}

// compactionPrompt instructs the summary model call of /compact.
const compactionPrompt = `Summarize the conversation so far so that work can continue with this summary as the only context. Preserve:

- The user's goal and any constraints
- Decisions made and their reasons
- Files read or changed, and what was learned or done to them
- Commands run and their outcomes
- The current state and the next steps

Write the summary as a compact briefing addressed to the assistant that continues the work. State established facts as facts, not as claims to verify. Omit pleasantries.`

// buildCompactionRequest assembles the summary request: the compaction
// prompt instead of the normal system prompt, the sanitized history as
// payload, and no tools — a summary must not call tools. A SESSION
// method for the same reason as buildRequest.
func (s *Session) buildCompactionRequest(context []model.Message) model.Request {
	messages := make([]model.Message, 0, len(context)+1)
	messages = append(messages, model.Message{Role: model.MessageRoleSystem, Content: compactionPrompt})
	messages = append(messages, sanitizeContext(context)...)
	return model.Request{
		Model:         s.meta.Model,
		Messages:      messages,
		Parameters:    s.resolved.Parameters,
		DeveloperRole: s.resolved.DeveloperRole,
		MaxTokens:     s.resolved.OutputLimit,
	}
}

// summarizeContext runs the compaction summary call, retrying transient
// provider failures with the same backoff as a normal turn: an automatic
// compaction must not die to one hiccup while the context is already
// large. onRetry (optional) is notified before each backoff wait.
func (a *Agent) summarizeContext(ctx context.Context, session *Session, request model.Request, onRetry func(err error, attempt, maxAttempts int, delay time.Duration)) (string, error) {
	const maxAttempts = 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		summary, retryable, err := a.summarizeOnce(ctx, session.resolved.Provider, request)
		if err == nil {
			return summary, nil
		}
		if ctx.Err() != nil || !retryable || attempt == maxAttempts {
			return "", err
		}
		delay := providerRetryBackoff(attempt)
		if onRetry != nil {
			onRetry(err, attempt, maxAttempts, delay)
		}
		if err := waitForProviderRetry(ctx, delay); err != nil {
			return "", err
		}
	}
	panic("unreachable")
}

func (a *Agent) summarizeOnce(ctx context.Context, provider model.Provider, request model.Request) (string, bool, error) {
	stream, err := provider.Stream(ctx, request)
	if err != nil {
		return "", model.IsRetryableProviderError(err), err
	}
	var content strings.Builder
	for event := range stream {
		switch event := event.(type) {
		case model.TextDeltaEvent:
			content.WriteString(event.Text)
		case model.ErrorEvent:
			return "", model.IsRetryableProviderError(event.Err), event.Err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	return content.String(), false, nil
}

// contextPressure grades a request against the session's compaction
// budget. A free function: the budget lives on the session (§15.2).
func contextPressure(budget contextBudget, request model.Request) compactionPressure {
	estimated := estimateRequestTokens(request)
	if budget.HardInputLimit > 0 && estimated >= budget.HardInputLimit {
		return pressureHard
	}
	if budget.SoftInputLimit > 0 && estimated >= budget.SoftInputLimit {
		return pressureSoft
	}
	return pressureNone
}

// enabledToolNames indexes the request's tool schemas for the enabled-tool
// check during execution.
func enabledToolNames(toolSchemas []model.Tool) map[string]struct{} {
	enabled := make(map[string]struct{}, len(toolSchemas))
	for _, tool := range toolSchemas {
		enabled[tool.Name] = struct{}{}
	}
	return enabled
}

func newRunID() string { return storage.NewRunID() }

// eventMessage formats a failure event for the run.failed record.
func eventMessage(event model.ErrorEvent) string {
	if event.Message != "" {
		return fmt.Sprintf("%s: %v", event.Message, event.Err)
	}
	return event.Err.Error()
}

// isBlank reports whether an assistant message carries no content at all.
func isBlank(message model.Message) bool {
	return len(message.ToolCalls) == 0 && strings.TrimSpace(message.Content) == ""
}

// summaryFrame wraps the committed summary so that, replayed as the sole
// prior context, the continuing model treats it as its own memory of the
// session instead of a user assertion to verify against the world. It is
// English regardless of the conversation language: it is an instruction to
// the model, like the system prompt.
//
// Known costs of embedding the frame in the record (design §13): it is
// persisted verbatim (immutable wording), appears in CLI/UI rendering of
// the compact block, and is re-fed to the summary model on a repeated
// compaction as part of the prior summary. The alternative — a fold-time
// marker with request-time wrapping — was rejected to keep the record
// self-contained and the request layer ignorant of compaction.
const summaryFrame = "The following is a summary of the earlier conversation in this session. It is your own working memory of established facts: rely on it directly and do not re-verify it with the user or the repository.\n\n"
