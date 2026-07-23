package webapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/execution"
)

const (
	defaultMaxConcurrentRuns = 8
	defaultMaxRunEvents      = 2048
	defaultMaxRunEventBytes  = 1 << 20 // 1 MiB per active run replay buffer.
	defaultTerminalRunLimit  = 64
	defaultTerminalRunTTL    = 10 * time.Minute
)

var (
	// ErrRunRegistryCapacity means the local Web application is already running
	// the maximum number of concurrent session runs. It prevents many active
	// runs from multiplying the bounded per-run replay buffer allocation.
	ErrRunRegistryCapacity = errors.New("web run registry is at its concurrent run limit")
	ErrRunRegistryClosed   = errors.New("web run registry is closed")
)

type runRegistryOptions struct {
	MaxConcurrentRuns int
	MaxRunEvents      int
	MaxRunEventBytes  int
	MaxTerminalRuns   int
	TerminalRunTTL    time.Duration
	Now               func() time.Time
	AfterFunc         func(time.Duration, func()) *time.Timer
}

func (o runRegistryOptions) withDefaults() runRegistryOptions {
	if o.MaxConcurrentRuns <= 0 {
		o.MaxConcurrentRuns = defaultMaxConcurrentRuns
	}
	if o.MaxRunEvents <= 0 {
		o.MaxRunEvents = defaultMaxRunEvents
	}
	if o.MaxRunEventBytes <= 0 {
		o.MaxRunEventBytes = defaultMaxRunEventBytes
	}
	if o.MaxTerminalRuns <= 0 {
		o.MaxTerminalRuns = defaultTerminalRunLimit
	}
	if o.TerminalRunTTL <= 0 {
		o.TerminalRunTTL = defaultTerminalRunTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.AfterFunc == nil {
		o.AfterFunc = time.AfterFunc
	}
	return o
}

type runRegistry struct {
	ctx     context.Context
	service *execution.Service
	log     io.Writer
	logMu   sync.Mutex
	options runRegistryOptions

	mu              sync.Mutex
	closed          bool
	byID            map[string]*managedRun
	activeBySession map[string]*managedRun
	terminalTimers  map[string]*time.Timer
}

type managedRun struct {
	id        string
	sessionID string
	run       *execution.SessionRun

	mu            sync.Mutex
	events        []runEvent
	eventBytes    int
	nextSeq       int64
	terminal      bool
	finishedAt    time.Time
	changed       chan struct{}
	maxEvents     int
	maxEventBytes int
}

type runEvent struct {
	Seq     int64
	Payload []byte
	Bytes   int
}

func newRunRegistry(ctx context.Context, service *execution.Service, logWriter io.Writer) *runRegistry {
	return newRunRegistryWithOptions(ctx, service, logWriter, runRegistryOptions{})
}

func newRunRegistryWithOptions(ctx context.Context, service *execution.Service, logWriter io.Writer, options runRegistryOptions) *runRegistry {
	if ctx == nil {
		ctx = context.Background()
	}
	return &runRegistry{
		ctx:             ctx,
		service:         service,
		log:             logWriter,
		options:         options.withDefaults(),
		byID:            make(map[string]*managedRun),
		activeBySession: make(map[string]*managedRun),
		terminalTimers:  make(map[string]*time.Timer),
	}
}

func newManagedRun(id, sessionID string, options runRegistryOptions) *managedRun {
	return &managedRun{
		id:            id,
		sessionID:     sessionID,
		changed:       make(chan struct{}),
		maxEvents:     options.MaxRunEvents,
		maxEventBytes: options.MaxRunEventBytes,
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
	managed := newManagedRun(id, sessionID, r.options)

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrRunRegistryClosed
	}
	if existing := r.activeBySession[sessionID]; existing != nil {
		if existing.run.Status() == execution.SessionRunRunning {
			r.mu.Unlock()
			return nil, execution.ErrSessionBusy
		}
		delete(r.activeBySession, sessionID)
	}
	if len(r.activeBySession) >= r.options.MaxConcurrentRuns {
		r.mu.Unlock()
		return nil, ErrRunRegistryCapacity
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
			r.logRunFailure(managed, err)
		}
	}
	managed.append(execution.NewSessionStreamEvent("run.settled", fields))
	managed.finish(r.options.Now().UTC())

	r.mu.Lock()
	if r.activeBySession[managed.sessionID] == managed {
		delete(r.activeBySession, managed.sessionID)
	}
	if !r.closed && r.byID[managed.id] == managed {
		r.retainTerminalLocked(managed)
	}
	r.mu.Unlock()
}

func (r *runRegistry) retainTerminalLocked(managed *managedRun) {
	for r.terminalCountLocked() > r.options.MaxTerminalRuns {
		oldest := r.oldestTerminalLocked()
		if oldest == nil {
			break
		}
		r.removeTerminalLocked(oldest)
	}
	if r.byID[managed.id] != managed || !managed.isTerminal() {
		return
	}
	if previous := r.terminalTimers[managed.id]; previous != nil {
		previous.Stop()
	}
	r.terminalTimers[managed.id] = r.options.AfterFunc(r.options.TerminalRunTTL, func() {
		r.expireTerminal(managed)
	})
}

func (r *runRegistry) expireTerminal(managed *managedRun) {
	if managed == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.byID[managed.id] != managed || !managed.isTerminal() {
		return
	}
	r.removeTerminalLocked(managed)
}

func (r *runRegistry) terminalCountLocked() int {
	count := 0
	for _, managed := range r.byID {
		if managed.isTerminal() {
			count++
		}
	}
	return count
}

func (r *runRegistry) oldestTerminalLocked() *managedRun {
	var oldest *managedRun
	for _, managed := range r.byID {
		if !managed.isTerminal() {
			continue
		}
		if oldest == nil || managed.finishedTime().Before(oldest.finishedTime()) {
			oldest = managed
		}
	}
	return oldest
}

func (r *runRegistry) removeTerminalLocked(managed *managedRun) {
	if managed == nil || r.byID[managed.id] != managed || !managed.isTerminal() {
		return
	}
	if timer := r.terminalTimers[managed.id]; timer != nil {
		timer.Stop()
		delete(r.terminalTimers, managed.id)
	}
	delete(r.byID, managed.id)
}

// Close releases terminal replay timers and makes the registry reject new
// runs. The launcher cancels the registry context before shutdown, which also
// cancels active SessionRuns.
func (r *runRegistry) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for _, timer := range r.terminalTimers {
		timer.Stop()
	}
	r.terminalTimers = make(map[string]*time.Timer)
	r.byID = make(map[string]*managedRun)
	r.activeBySession = make(map[string]*managedRun)
}

func (r *runRegistry) logRunFailure(managed *managedRun, err error) {
	if r == nil || r.log == nil || managed == nil || err == nil {
		return
	}
	r.logMu.Lock()
	defer r.logMu.Unlock()
	fmt.Fprintf(r.log, "sai: run %s for session %s failed: %v\n", managed.id, managed.sessionID, err)
}

func (r *runRegistry) get(id string) (*managedRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, false
	}
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
	payload := encodeRunEvent(event)
	r.mu.Lock()
	if r.terminal {
		r.mu.Unlock()
		return
	}
	r.nextSeq++
	r.events = append(r.events, runEvent{Seq: r.nextSeq, Payload: payload, Bytes: len(payload)})
	r.eventBytes += len(payload)
	r.trimEventsLocked()
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
}

func (r *managedRun) trimEventsLocked() {
	for len(r.events) > 0 && (len(r.events) > r.maxEvents || r.eventBytes > r.maxEventBytes) {
		dropped := r.events[0]
		r.events[0] = runEvent{}
		r.events = r.events[1:]
		r.eventBytes -= dropped.Bytes
	}
}

func (r *managedRun) finish(finishedAt time.Time) {
	r.mu.Lock()
	if r.terminal {
		r.mu.Unlock()
		return
	}
	// Session items are the durable source of truth. Once the run settles, a
	// late SSE client only needs run.settled to trigger its durable refresh, so
	// discard all transient output and tool activity retained for live replay.
	if len(r.events) > 0 {
		terminalEvent := r.events[len(r.events)-1]
		r.events = []runEvent{terminalEvent}
		r.eventBytes = terminalEvent.Bytes
	}
	r.terminal = true
	r.finishedAt = finishedAt
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
}

func (r *managedRun) isTerminal() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.terminal
}

func (r *managedRun) finishedTime() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finishedAt
}

func (r *managedRun) snapshot(after int64) ([]runEvent, bool, bool, int64, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	start := len(r.events)
	oldestSeq := int64(0)
	resyncRequired := false
	if len(r.events) > 0 {
		oldestSeq = r.events[0].Seq
		resyncRequired = after < oldestSeq-1
		for index, event := range r.events {
			if event.Seq > after {
				start = index
				break
			}
		}
	}
	items := append([]runEvent(nil), r.events[start:]...)
	return items, r.terminal, resyncRequired, oldestSeq, r.changed
}

func encodeRunEvent(event execution.SessionStreamEvent) []byte {
	payload, err := json.Marshal(event)
	if err == nil {
		return payload
	}
	// Session stream events originate from internal DTOs and should always be
	// JSON-serializable. Keep the replay buffer and SSE stream usable even if a
	// future caller accidentally supplies an unsupported value.
	return []byte(`{"type":"run.event_encoding_failed"}`)
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
		if errors.Is(err, ErrRunRegistryCapacity) {
			writeAPIError(w, http.StatusTooManyRequests, "run_capacity", "too many runs are currently active")
			return
		}
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
	if after < 0 {
		after = 0
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		events, terminal, resyncRequired, oldestSeq, changed := managed.snapshot(after)
		if resyncRequired {
			resync := execution.NewSessionStreamEvent("run.resync_required", map[string]any{
				"run_id":     managed.id,
				"session_id": managed.sessionID,
				"oldest_seq": oldestSeq,
			})
			if err := writeSSEEvent(w, 0, resync); err != nil {
				return
			}
			after = oldestSeq - 1
		}
		for _, item := range events {
			if err := writeSSEPayload(w, item.Seq, item.Payload); err != nil {
				return
			}
			after = item.Seq
		}
		if len(events) > 0 || resyncRequired {
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

func writeSSEEvent(w io.Writer, sequence int64, event execution.SessionStreamEvent) error {
	return writeSSEPayload(w, sequence, encodeRunEvent(event))
}

func writeSSEPayload(w io.Writer, sequence int64, payload []byte) error {
	var err error
	if sequence > 0 {
		_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", sequence, payload)
	} else {
		_, err = fmt.Fprintf(w, "data: %s\n\n", payload)
	}
	return err
}

func randomID(prefix string) (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + hex.EncodeToString(raw), nil
}
