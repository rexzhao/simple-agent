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
	Provider      model.Provider
	ProviderName  string // session metadata
	ModelID       string // session metadata and requests
	Storage       storage.Store
	Tools         []model.Tool
	ToolExecutor  ToolExecutor
	SystemPrompt  string
	DeveloperRole model.MessageRole
	Parameters    map[string]any
	MaxTokens     int
	MaxIterations int
	ProjectID     string // jsonl directory grouping
	CWD           string
	// ContextWindow is the model's context window in tokens; 0 means a
	// conservative default. With AutoCompact it bounds the request budget.
	ContextWindow int
	// AutoCompact compacts the context automatically when the request
	// approaches the budget (see budget.go).
	AutoCompact bool
}

// Agent is one configured application instance: provider, tools, prompts,
// and the session store, all immutable after construction.
type Agent struct {
	provider      model.Provider
	providerName  string
	modelID       string
	store         storage.Store
	tools         []model.Tool
	toolExecutor  ToolExecutor
	systemPrompt  string
	developerRole model.MessageRole
	parameters    map[string]any
	outputLimit   int
	maxIterations int
	projectID     string
	cwd           string
	budget        contextBudget
	autoCompact   bool
}

// New validates and assembles an agent.
func New(options Options) (*Agent, error) {
	if options.Provider == nil {
		return nil, fmt.Errorf("agent provider is required")
	}
	if options.Storage == nil {
		return nil, fmt.Errorf("agent storage is required")
	}
	maxIterations := options.MaxIterations
	if maxIterations <= 0 {
		maxIterations = DefaultMaxIterations
	}
	developerRole := options.DeveloperRole
	if developerRole == "" {
		developerRole = model.MessageRoleDeveloper
	}
	return &Agent{
		provider:      options.Provider,
		providerName:  options.ProviderName,
		modelID:       options.ModelID,
		store:         options.Storage,
		tools:         options.Tools,
		toolExecutor:  options.ToolExecutor,
		systemPrompt:  options.SystemPrompt,
		developerRole: developerRole,
		parameters:    options.Parameters,
		outputLimit:   options.MaxTokens,
		maxIterations: maxIterations,
		projectID:     options.ProjectID,
		cwd:           options.CWD,
		budget:        resolveContextBudget(options.ContextWindow, options.MaxTokens),
		autoCompact:   options.AutoCompact,
	}, nil
}

// CWD returns the working directory tools operate in.
func (a *Agent) CWD() string { return a.cwd }

// NewSession creates a fresh, lazily persisted session. The session ID is
// minted immediately and available before any record exists. The
// session.created record is prepared here and committed ahead of the
// session's first real record — so an empty session never touches the
// storage.
func (a *Agent) NewSession() (*Session, error) {
	stored, err := a.store.Create(a.projectID)
	if err != nil {
		return nil, err
	}
	return &Session{
		agent:   a,
		storage: stored,
		state:   storage.NewState(stored.ID()),
		pendingCreated: &storage.Record{
			ItemID:   storage.NewSessionCreatedID(),
			Type:     storage.TypeSessionCreated,
			Provider: a.providerName,
			Model:    a.modelID,
			CWD:      a.cwd,
			Time:     storage.NewTime(),
		},
	}, nil
}

// OpenSession reopens a stored session by ID, rebuilding its state by
// folding the records.
func (a *Agent) OpenSession(id string) (*Session, error) {
	stored, err := a.store.Open(id)
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
	return &Session{agent: a, storage: stored, state: state, records: records}, nil
}

// buildRequest assembles the model request from the folded context: system
// prompt first, then the sanitized wire history.
func (a *Agent) buildRequest(context []model.Message) model.Request {
	messages := make([]model.Message, 0, len(context)+1)
	if a.systemPrompt != "" {
		messages = append(messages, model.Message{Role: model.MessageRoleSystem, Content: a.systemPrompt})
	}
	messages = append(messages, sanitizeContext(context)...)
	return model.Request{
		Model:         a.modelID,
		Messages:      messages,
		Tools:         a.tools,
		Parameters:    a.parameters,
		DeveloperRole: a.developerRole,
		MaxTokens:     a.outputLimit,
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

Write the summary as a compact briefing addressed to the assistant that continues the work. Omit pleasantries.`

// buildCompactionRequest assembles the summary request: the compaction
// prompt instead of the normal system prompt, the sanitized history as
// payload, and no tools — a summary must not call tools.
func (a *Agent) buildCompactionRequest(context []model.Message) model.Request {
	messages := make([]model.Message, 0, len(context)+1)
	messages = append(messages, model.Message{Role: model.MessageRoleSystem, Content: compactionPrompt})
	messages = append(messages, sanitizeContext(context)...)
	return model.Request{
		Model:         a.modelID,
		Messages:      messages,
		Parameters:    a.parameters,
		DeveloperRole: a.developerRole,
		MaxTokens:     a.outputLimit,
	}
}

// summarizeContext runs the compaction summary call, retrying transient
// provider failures with the same backoff as a normal turn: an automatic
// compaction must not die to one hiccup while the context is already
// large. onRetry (optional) is notified before each backoff wait.
func (a *Agent) summarizeContext(ctx context.Context, request model.Request, onRetry func(err error, attempt int, delay time.Duration)) (string, error) {
	const maxAttempts = 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		summary, retryable, err := a.summarizeOnce(ctx, request)
		if err == nil {
			return summary, nil
		}
		if ctx.Err() != nil || !retryable || attempt == maxAttempts {
			return "", err
		}
		delay := providerRetryBackoff(attempt)
		if onRetry != nil {
			onRetry(err, attempt, delay)
		}
		if err := waitForProviderRetry(ctx, delay); err != nil {
			return "", err
		}
	}
	panic("unreachable")
}

func (a *Agent) summarizeOnce(ctx context.Context, request model.Request) (string, bool, error) {
	stream, err := a.provider.Stream(ctx, request)
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

// contextPressure grades a request against the compaction budget.
func (a *Agent) contextPressure(request model.Request) compactionPressure {
	estimated := estimateRequestTokens(request)
	if a.budget.HardInputLimit > 0 && estimated >= a.budget.HardInputLimit {
		return pressureHard
	}
	if a.budget.SoftInputLimit > 0 && estimated >= a.budget.SoftInputLimit {
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
