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
	projectcontext "github.com/rexzhao/simple-agent/internal/context"
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
	if _, err := s.log.Append(sessionlog.Record{Type: sessionlog.TypeRunStarted, RunID: runID}); err != nil {
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

// pump forwards agent events to the UI channel and settles the run when the
// agent loop finishes.
func (s *Session) pump(ctx context.Context, runID string, request model.Request, events chan model.Event) {
	stream, err := agent.Stream(ctx, request, agent.Options{
		Provider:     s.app.provider,
		ToolExecutor: s.app.toolRegistry,
		MaxTurns:     s.app.maxTurns,
		TurnID:       newID("turn"),
		Publisher:    s.pub,
	})
	var settleStatus string
	var settleError string
	if err != nil {
		settleStatus = sessionlog.RunStatusFailed
		settleError = err.Error()
	} else {
		settleStatus = sessionlog.RunStatusCommitted
		for event := range stream {
			switch event := event.(type) {
			case model.ErrorEvent:
				settleStatus = sessionlog.RunStatusFailed
				settleError = eventMessage(event)
			}
			select {
			case events <- event:
			default:
				// The UI channel is best-effort: durable state always
				// arrives through the log, so dropping a transient event
				// under backpressure is safe.
			}
		}
		if ctx.Err() != nil {
			settleStatus = sessionlog.RunStatusInterrupted
			settleError = ""
		}
	}
	close(events)
	_, _ = s.log.Append(sessionlog.Record{Type: sessionlog.TypeRunSettled, RunID: runID, Status: settleStatus, Error: settleError})
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
	messages = append(messages, context...)
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

// renderPrompt expands the config prompt template with the run values.
func renderPrompt(template, cwd, configDir string) (string, error) {
	rendered, err := projectcontext.RenderPromptTemplate(template, projectcontext.PromptRenderValues{
		CWD:       cwd,
		ConfigDir: configDir,
		Now:       time.Now().UTC(),
	})
	if err != nil {
		return "", fmt.Errorf("render prompt.system_prompt: %w", err)
	}
	return rendered, nil
}

func newID(prefix string) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(raw[:])
}
