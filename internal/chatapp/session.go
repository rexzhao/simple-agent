package chatapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessionlog"
)

// Session is one conversation: the append-only log plus the active run.
// Exactly one run may be active at a time; Send while running is rejected.
// The minimal app does not queue input mid-run.
type Session struct {
	app *App
	log *sessionlog.Session
	pub *sessionlog.Publisher

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	events  chan model.Event
}

func newSession(app *App, log *sessionlog.Session) *Session {
	return &Session{app: app, log: log, pub: sessionlog.NewPublisher(log)}
}

// ID returns the session ID.
func (s *Session) ID() string { return s.log.ID() }

// Snapshot returns the folded session state.
func (s *Session) Snapshot() sessionlog.State { return s.log.Snapshot() }

// RecordsAfter returns the session's stored records after seq, for tailing
// the JSONL stream from outside.
func (s *Session) RecordsAfter(seq int64) ([]sessionlog.Record, error) {
	return s.log.RecordsAfter(seq)
}

// Subscribe notifies the caller after every durable append.
func (s *Session) Subscribe() (<-chan struct{}, func()) { return s.log.Subscribe() }

// Running reports whether a run is active.
func (s *Session) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Events returns the transient event stream of the active run. The channel is
// closed when the run settles; between runs it is nil-safe (Receive returns
// false immediately).
func (s *Session) Events() <-chan model.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events
}

// Send appends the user input durably and starts the agent run. It returns as
// soon as the run is launched; output arrives on Events and via Subscribe.
//
// The check, the durable appends, and the running-flag transition happen in
// one lock section, so a concurrent Send can never interleave its records
// with another run's: the loser is rejected before anything is written.
func (s *Session) Send(text string) error {
	text = strings.TrimRight(text, "\r\n")
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("input is empty")
	}

	runID := newID("run")
	events := make(chan model.Event, 128)
	ctx, cancel := context.WithCancel(context.Background())

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		cancel()
		return fmt.Errorf("a run is already active; stop it first")
	}
	if _, err := s.log.Append(sessionlog.Record{Type: sessionlog.TypeUserInput, RunID: runID, Content: text}); err != nil {
		s.mu.Unlock()
		cancel()
		return err
	}
	request := s.app.request(s.log.Snapshot().Context)
	s.running = true
	s.cancel = cancel
	s.events = events
	s.mu.Unlock()

	s.pub.SetRun(runID)
	go s.pump(ctx, runID, request, events)
	return nil
}

// Stop cancels the active run. The pump settles it as interrupted.
func (s *Session) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Close stops any active run and closes the log.
func (s *Session) Close() error {
	s.Stop()
	// Give the pump a moment to settle the run before closing the log; a
	// settle after close would fail and leave the run forever "running" on
	// reopen. The pump's settle is the last write before it clears running.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if !running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return s.log.Close()
}

// pump forwards agent events to the UI channel and records the failure when
// the agent loop finishes with an error. A committed or interrupted run
// writes nothing: committed is the absence of failure, and an interruption
// simply leaves the user input unanswered.
func (s *Session) pump(ctx context.Context, runID string, request model.Request, events chan model.Event) {
	stream, err := agent.Stream(ctx, request, agent.Options{
		Provider:     s.app.provider,
		ToolExecutor: s.app.toolRegistry,
		MaxTurns:     s.app.maxTurns,
		TurnID:       newID("turn"),
		Publisher:    s.pub,
	})
	var failure string
	if err != nil {
		failure = err.Error()
	} else {
		for event := range stream {
			if failureEvent, ok := event.(model.ErrorEvent); ok && ctx.Err() == nil {
				failure = eventMessage(failureEvent)
			}
			select {
			case events <- event:
			default:
				// The UI channel is best-effort: durable state always
				// arrives through the log, so dropping a transient event
				// under backpressure is safe.
			}
		}
	}
	close(events)
	if failure != "" {
		_, _ = s.log.Append(sessionlog.Record{Type: sessionlog.TypeRunFailed, RunID: runID, Error: failure})
	}
	s.mu.Lock()
	s.running = false
	s.cancel = nil
	s.events = nil
	s.mu.Unlock()
}

// request builds the model request from the folded context.
func (a *App) request(context []model.Message) model.Request {
	messages := make([]model.Message, 0, len(context)+1)
	if a.systemPrompt != "" {
		messages = append(messages, model.Message{Role: model.MessageRoleSystem, Content: a.systemPrompt})
	}
	messages = append(messages, sanitizeContext(context)...)
	return model.Request{
		Model:         a.resolved.ModelID,
		Messages:      messages,
		Tools:         a.toolSchemas,
		Parameters:    a.parameters,
		DeveloperRole: a.developerRole,
		MaxTokens:     a.outputLimit,
	}
}

func eventMessage(event model.ErrorEvent) string {
	if event.Message != "" {
		return fmt.Sprintf("%s: %v", event.Message, event.Err)
	}
	return event.Err.Error()
}

// renderPrompt expands the config prompt template's {{cwd}}, {{config_dir}},
// and {{time.now}} placeholders.
func renderPrompt(template, cwd, configDir string) (string, error) {
	replacements := map[string]string{
		"cwd":        cwd,
		"config_dir": configDir,
		"time.now":   time.Now().UTC().Format(time.RFC3339),
	}
	var rendered strings.Builder
	offset := 0
	for {
		start := strings.Index(template[offset:], "{{")
		if start < 0 {
			rendered.WriteString(template[offset:])
			return rendered.String(), nil
		}
		start += offset
		rendered.WriteString(template[offset:start])
		end := strings.Index(template[start+2:], "}}")
		if end < 0 {
			return "", fmt.Errorf("prompt placeholder starting at byte %d is missing closing }}", start)
		}
		end += start + 2
		name := strings.TrimSpace(template[start+2 : end])
		replacement, ok := replacements[name]
		if !ok {
			return "", fmt.Errorf("unknown prompt placeholder %q; supported placeholders: config_dir, cwd, time.now", name)
		}
		rendered.WriteString(replacement)
		offset = end + 2
	}
}

func newID(prefix string) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(raw[:])
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
