package webapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/execution"
)

type runRegistry struct {
	ctx     context.Context
	service *execution.Service

	mu              sync.Mutex
	byID            map[string]*managedRun
	activeBySession map[string]*managedRun
}

type managedRun struct {
	id        string
	sessionID string
	run       *execution.SessionRun

	mu       sync.Mutex
	events   []runEvent
	nextSeq  int64
	terminal bool
	changed  chan struct{}
}

type runEvent struct {
	Seq   int64
	Event execution.SessionStreamEvent
}

func newRunRegistry(ctx context.Context, service *execution.Service) *runRegistry {
	return &runRegistry{
		ctx:             ctx,
		service:         service,
		byID:            make(map[string]*managedRun),
		activeBySession: make(map[string]*managedRun),
	}
}

func (r *runRegistry) start(sessionID, content string) (*managedRun, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("message content must be a non-empty string")
	}
	id, err := randomID("run-")
	if err != nil {
		return nil, err
	}
	managed := &managedRun{
		id:        id,
		sessionID: sessionID,
		changed:   make(chan struct{}),
	}

	r.mu.Lock()
	if existing := r.activeBySession[sessionID]; existing != nil && existing.run.Status() == execution.SessionRunRunning {
		r.mu.Unlock()
		return nil, execution.ErrSessionBusy
	}
	managed.run = r.service.StartSessionRun(r.ctx, sessionID, content, managed.append)
	r.byID[id] = managed
	r.activeBySession[sessionID] = managed
	r.mu.Unlock()

	go r.await(managed)
	return managed, nil
}

func (r *runRegistry) await(managed *managedRun) {
	result, err := managed.run.Wait()
	status := string(managed.run.Status())
	fields := map[string]any{
		"run_id":   managed.id,
		"status":   status,
		"turn_id":  result.TurnID,
		"last_seq": result.LastSeq,
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fields["message"] = "run cancelled"
		} else {
			fields["message"] = "run failed"
		}
	}
	managed.append(execution.NewSessionStreamEvent("run.settled", fields))
	managed.finish()

	r.mu.Lock()
	if r.activeBySession[managed.sessionID] == managed {
		delete(r.activeBySession, managed.sessionID)
	}
	r.mu.Unlock()
}

func (r *runRegistry) get(id string) (*managedRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	managed, ok := r.byID[id]
	return managed, ok
}

func (r *runRegistry) cancel(id string) (*managedRun, bool) {
	managed, ok := r.get(id)
	if ok {
		managed.run.Cancel()
	}
	return managed, ok
}

func (r *managedRun) append(event execution.SessionStreamEvent) {
	if event == nil {
		return
	}
	r.mu.Lock()
	r.nextSeq++
	r.events = append(r.events, runEvent{Seq: r.nextSeq, Event: event})
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
}

func (r *managedRun) finish() {
	r.mu.Lock()
	r.terminal = true
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
}

func (r *managedRun) snapshot(after int64) ([]runEvent, bool, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := len(r.events)
	for i, event := range r.events {
		if event.Seq > after {
			start = i
			break
		}
	}
	items := append([]runEvent(nil), r.events[start:]...)
	return items, r.terminal, r.changed
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if _, err := s.service.GetSession(r.PathValue("sessionID")); err != nil {
		writeServiceError(w, err)
		return
	}
	managed, err := s.runs.start(r.PathValue("sessionID"), body.Content)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"run_id":     managed.id,
		"session_id": managed.sessionID,
		"status":     string(execution.SessionRunRunning),
	})
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.runs.cancel(r.PathValue("runID"))
	if !ok {
		writeAPIError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"run_id": managed.id,
		"status": string(managed.run.Status()),
	})
}

func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.runs.get(r.PathValue("runID"))
	if !ok {
		writeAPIError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "stream_unsupported", "streaming is unavailable")
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		events, terminal, changed := managed.snapshot(after)
		for _, item := range events {
			payload, err := json.Marshal(item.Event)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", item.Seq, payload); err != nil {
				return
			}
			after = item.Seq
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		if terminal {
			return
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		case <-time.After(15 * time.Second):
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func randomID(prefix string) (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + hex.EncodeToString(raw), nil
}
