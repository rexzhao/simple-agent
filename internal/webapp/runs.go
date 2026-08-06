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
	"github.com/rexzhao/simple-agent/internal/model"
)

const (
	defaultMaxConcurrentRuns = 8
	defaultMaxRunEvents      = 2048
	defaultMaxRunEventBytes  = 1 << 20 // 1 MiB per active run replay buffer.
	defaultTerminalRunLimit  = 64
	defaultTerminalRunTTL    = 10 * time.Minute

	maxRunRequestBytes       = 17 * 1024 * 1024
	maxRunImageAttachments   = model.MaxImageInputAttachments
	maxRunImageBytes         = model.MaxImageInputBytes
	maxRunImageTotalBytes    = model.MaxImageInputTotalBytes
	maxActivePromptMoveDelta = 64
)

var (
	// ErrRunRegistryCapacity means the local Web application is already running
	// the maximum number of concurrent session runs. It prevents many active
	// runs from multiplying the bounded per-run replay buffer allocation.
	ErrRunRegistryCapacity = execution.ErrSessionRunCoordinatorCapacity
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
	ctx         context.Context
	service     *execution.Service
	coordinator *execution.SessionRunCoordinator
	log         io.Writer
	logMu       sync.Mutex
	options     runRegistryOptions

	mu             sync.Mutex
	closed         bool
	byID           map[string]*managedRun
	terminalTimers map[string]*time.Timer
}

type managedRun struct {
	id        string
	sessionID string
	run       *execution.CoordinatedSessionRun

	mu            sync.Mutex
	events        []runEvent
	eventBytes    int
	nextSeq       int64
	terminal      bool
	startedAt     time.Time
	finishedAt    time.Time
	turnID        string
	changed       chan struct{}
	maxEvents     int
	maxEventBytes int
}

type runEvent struct {
	Seq     int64
	Payload []byte
	Bytes   int
}

// activeRunSnapshot is the server-owned descriptor a browser uses to reattach
// to any coordinated run, whether it was started by Web or by an agent tool.
// Session data remains durable; this only identifies the in-memory SSE stream
// and its current turn.
type activeRunSnapshot struct {
	RunID     string    `json:"run_id"`
	SessionID string    `json:"session_id"`
	TurnID    string    `json:"turn_id,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Status    string    `json:"status"`
}

func newRunRegistry(ctx context.Context, service *execution.Service, logWriter io.Writer) *runRegistry {
	return newRunRegistryWithOptions(ctx, service, logWriter, runRegistryOptions{})
}

func newRunRegistryWithOptions(ctx context.Context, service *execution.Service, logWriter io.Writer, options runRegistryOptions) *runRegistry {
	if ctx == nil {
		ctx = context.Background()
	}
	resolvedOptions := options.withDefaults()
	registry := &runRegistry{
		ctx:            ctx,
		service:        service,
		log:            logWriter,
		options:        resolvedOptions,
		byID:           make(map[string]*managedRun),
		terminalTimers: make(map[string]*time.Timer),
	}
	coordinatorOptions := execution.SessionRunCoordinatorOptions{
		MaxConcurrentRuns:    resolvedOptions.MaxConcurrentRuns,
		OnRunAdmitted:        registry.admitRun,
		OnRunAdmissionFailed: registry.rejectRun,
		OnRunEvent:           registry.observeRunEvent,
		OnRunSettled:         registry.settleRun,
	}
	if service != nil {
		coordinatorOptions.DurableAdmitter = service
	}
	coordinator := execution.NewSessionRunCoordinator(ctx, service, coordinatorOptions)
	registry.coordinator = coordinator
	if service != nil {
		service.SetSessionRunCoordinator(coordinator)
	}
	return registry
}

func newManagedRun(id, sessionID string, options runRegistryOptions) *managedRun {
	return &managedRun{
		id:            id,
		sessionID:     sessionID,
		startedAt:     options.Now().UTC(),
		changed:       make(chan struct{}),
		maxEvents:     options.MaxRunEvents,
		maxEventBytes: options.MaxRunEventBytes,
	}
}

func (r *runRegistry) start(sessionID, content string) (*managedRun, error) {
	return r.startWithInput(sessionID, execution.SessionMessageInput{Content: content})
}

func (r *runRegistry) startWithInput(sessionID string, input execution.SessionMessageInput) (*managedRun, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}
	if strings.TrimSpace(input.Content) == "" && len(input.ContentBlocks) == 0 && !input.Continue {
		return nil, fmt.Errorf("message content or image attachment is required")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrRunRegistryClosed
	}
	r.mu.Unlock()

	coordinated, err := r.coordinator.Start(sessionID, input, nil)
	if err != nil {
		return nil, err
	}
	managed, ok := r.get(coordinated.ID())
	if !ok {
		coordinated.Cancel()
		return nil, ErrRunRegistryClosed
	}
	return managed, nil
}

func (r *runRegistry) startContinue(sessionID string) (*managedRun, error) {
	return r.startWithInput(sessionID, execution.SessionMessageInput{Continue: true})
}

// startDurable is the Web command entry point. The coordinator still owns
// the single active-run/replay path; this method only supplies the durable
// identity fingerprint and returns the compact status acknowledgement.
func (r *runRegistry) startDurable(sessionID, content, runID, inputFingerprint string) (string, error) {
	return r.startDurableWithInput(sessionID, execution.SessionMessageInput{Content: content}, runID, inputFingerprint)
}

// continueDurable is the Web command entry point for a durable continuation.
// It deliberately supplies no content: the execution service resolves the
// interrupted target from durable session state while holding the same
// admission lock used by run.start, and persists that target in PreviousRunID.
func (r *runRegistry) continueDurable(sessionID, runID, inputFingerprint string) (string, error) {
	return r.startDurableWithInput(sessionID, execution.SessionMessageInput{Continue: true}, runID, inputFingerprint)
}

func (r *runRegistry) startDurableWithInput(sessionID string, input execution.SessionMessageInput, runID, inputFingerprint string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	runID = strings.TrimSpace(runID)
	if sessionID == "" || runID == "" {
		return "", fmt.Errorf("session id and run id are required")
	}
	if input.Continue {
		if strings.TrimSpace(input.Content) != "" || len(input.ContentBlocks) != 0 {
			return "", fmt.Errorf("continue cannot include new message content")
		}
	} else if strings.TrimSpace(input.Content) == "" && len(input.ContentBlocks) == 0 {
		return "", fmt.Errorf("message content or image attachment is required")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return "", ErrRunRegistryClosed
	}
	r.mu.Unlock()
	coordinated, admission, err := r.coordinator.StartDurable(sessionID, input, runID, inputFingerprint, nil)
	if err != nil {
		return "", err
	}
	if coordinated != nil && !admission.Created {
		return string(admission.Status), nil
	}
	if !admission.Created {
		return string(admission.Status), nil
	}
	if coordinated == nil {
		return "", fmt.Errorf("durable run admission returned no active handle")
	}
	if _, ok := r.get(coordinated.ID()); !ok {
		coordinated.Cancel()
		return "", ErrRunRegistryClosed
	}
	return string(execution.SessionRunRunning), nil
}

// admitRun is the first presentation-adapter phase of coordinator admission.
// It runs before the execution starter, which means a synchronous starter and
// an agent/session-tool start have the same replay registration guarantee as a
// Web start.
func (r *runRegistry) admitRun(run *execution.CoordinatedSessionRun) error {
	if r == nil || run == nil || strings.TrimSpace(run.ID()) == "" {
		return ErrRunRegistryClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRunRegistryClosed
	}
	if existing := r.byID[run.ID()]; existing != nil {
		if existing.run == run {
			return nil
		}
		return fmt.Errorf("run %q is already registered", run.ID())
	}
	managed := newManagedRun(run.ID(), run.SessionID(), r.options)
	managed.startedAt = run.StartedAt()
	managed.run = run
	r.byID[managed.id] = managed
	// Keep the run-start boundary in the same replay buffer as the execution
	// events. The HTTP admission response only gives the browser the identity
	// needed to connect; it must not be used to synthesize the transient run
	// container. This event is appended before the starter is invoked so a
	// synchronous/very fast run still replays its authoritative start boundary.
	managed.append(execution.NewSessionStreamEvent("run.started", map[string]any{
		"run_id":     run.ID(),
		"session_id": run.SessionID(),
		"status":     string(execution.SessionRunRunning),
	}))
	return nil
}

// rejectRun rolls back adapter admission when the coordinator cannot start a
// SessionRun after reserving the handle (for example, a nil starter result).
func (r *runRegistry) rejectRun(run *execution.CoordinatedSessionRun) {
	if r == nil || run == nil {
		return
	}
	r.mu.Lock()
	if managed := r.byID[run.ID()]; managed != nil && managed.run == run && !managed.isTerminal() {
		delete(r.byID, run.ID())
	}
	r.mu.Unlock()
}

// observeRunEvent is installed on the shared execution coordinator, so runs
// started by Web requests and runs started by agent session tools enter the
// same bounded replay registry. Registration has already happened in
// admitRun; this observer only appends to the existing buffer.
func (r *runRegistry) observeRunEvent(run *execution.CoordinatedSessionRun, event execution.SessionStreamEvent) {
	managed, ok := r.lookupManaged(run)
	if !ok {
		return
	}
	managed.append(event)
}

// settleRun finalizes replay for every coordinated run. The coordinator calls
// it before removing the active handle, which closes the discovery race for a
// browser attaching as the run settles.
func (r *runRegistry) settleRun(run *execution.CoordinatedSessionRun, result execution.SessionMessageResult, err error) {
	managed, ok := r.lookupManaged(run)
	if !ok {
		return
	}
	status := string(run.Status())
	lastSeq := result.LastSeq
	if current, ok := r.currentSessionWatermark(managed.sessionID); ok {
		// The run result can be a stale execution snapshot on failure or
		// cancellation. Once the session is readable, both compatibility
		// fields must describe the same final durable watermark.
		lastSeq = current
	}
	fields := map[string]any{
		"run_id":             managed.id,
		"status":             status,
		"turn_id":            result.TurnID,
		"last_seq":           lastSeq,
		"committed_revision": strconv.FormatInt(lastSeq, 10),
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
	if !r.closed && r.byID[managed.id] == managed {
		r.retainTerminalLocked(managed)
	}
	r.mu.Unlock()
}

func (r *runRegistry) lookupManaged(run *execution.CoordinatedSessionRun) (*managedRun, bool) {
	if r == nil || run == nil || strings.TrimSpace(run.ID()) == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, false
	}
	managed, ok := r.byID[run.ID()]
	if !ok || managed.run != run {
		return nil, false
	}
	return managed, true
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
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	for _, timer := range r.terminalTimers {
		timer.Stop()
	}
	r.terminalTimers = make(map[string]*time.Timer)
	r.byID = make(map[string]*managedRun)
	coordinator := r.coordinator
	service := r.service
	r.mu.Unlock()
	if service != nil {
		service.ClearSessionRunCoordinator(coordinator)
	}
	coordinator.Close()
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
	id = strings.TrimSpace(id)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, false
	}
	managed, ok := r.byID[id]
	r.mu.Unlock()
	return managed, ok
}

// activeRunControl resolves a process-local control target. The lookup first
// checks the Web adapter's retained identity so a run ID cannot be used to
// search another session, then confirms that the same handle is still owned by
// the shared coordinator. No durable row or operation claim is consulted.
func (r *runRegistry) activeRunControl(sessionID, runID string) (*execution.CoordinatedSessionRun, error) {
	sessionID = strings.TrimSpace(sessionID)
	runID = strings.TrimSpace(runID)
	if r == nil {
		return nil, execution.ErrRunControlNotFound
	}
	managed, ok := r.get(runID)
	if !ok || managed == nil {
		return nil, execution.ErrRunControlNotFound
	}
	if managed.sessionID != sessionID {
		return nil, execution.ErrRunControlWrongSession
	}
	if managed.run == nil {
		return nil, execution.ErrRunControlNotActive
	}
	if managed.isTerminal() || managed.run.Status() != execution.SessionRunRunning {
		return nil, execution.ErrRunControlRunSettled
	}
	if r.coordinator == nil {
		return nil, execution.ErrRunControlNotActive
	}
	active, ok := r.coordinator.ActiveForSession(sessionID)
	if !ok || active.ID() != runID || !active.ActiveControlReady() {
		return nil, execution.ErrRunControlNotActive
	}
	return active, nil
}

func (r *runRegistry) removeActivePrompt(sessionID, runID, promptID string) error {
	active, err := r.activeRunControl(sessionID, runID)
	if err != nil {
		return err
	}
	if !active.ActivePromptReady() {
		return execution.ErrRunControlNotActive
	}
	if !active.RemoveActive(promptID) {
		return execution.ErrRunControlPromptNotFound
	}
	return nil
}

func (r *runRegistry) steerActivePrompt(sessionID, runID, promptID string, steer bool) error {
	active, err := r.activeRunControl(sessionID, runID)
	if err != nil {
		return err
	}
	if !active.ActivePromptReady() {
		return execution.ErrRunControlNotActive
	}
	if !active.SetActivePromptSteer(promptID, steer) {
		return execution.ErrRunControlPromptNotFound
	}
	return nil
}

func (r *runRegistry) moveActivePrompt(sessionID, runID, promptID string, delta int) (bool, error) {
	active, err := r.activeRunControl(sessionID, runID)
	if err != nil {
		return false, err
	}
	if !active.ActivePromptReady() {
		return false, execution.ErrRunControlNotActive
	}
	found, moved := active.MoveActivePromptResult(promptID, delta)
	if !found {
		return false, execution.ErrRunControlPromptNotFound
	}
	return moved, nil
}

func (r *runRegistry) cancelToolCall(sessionID, runID, toolCallID string) error {
	active, err := r.activeRunControl(sessionID, runID)
	if err != nil {
		return err
	}
	if !active.CancelToolCall(toolCallID) {
		return execution.ErrRunControlToolNotActive
	}
	return nil
}

func (r *runRegistry) activeRuns() []activeRunSnapshot {
	if r == nil || r.coordinator == nil {
		return []activeRunSnapshot{}
	}
	descriptors := r.coordinator.ActiveRuns()
	active := make([]activeRunSnapshot, 0, len(descriptors))
	for _, descriptor := range descriptors {
		active = append(active, activeRunSnapshot{
			RunID: descriptor.RunID, SessionID: descriptor.SessionID,
			TurnID: descriptor.TurnID, StartedAt: descriptor.StartedAt,
			Status: descriptor.Status,
		})
	}
	return active
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
	turnID := ""
	if eventType, _ := event["type"].(string); eventType == "turn.started" {
		turnID, _ = event["turn_id"].(string)
	}
	payload := encodeRunEvent(event)
	r.mu.Lock()
	if r.terminal {
		r.mu.Unlock()
		return
	}
	if turnID != "" {
		r.turnID = turnID
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

func (r *managedRun) activeSnapshot() (activeRunSnapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.terminal || r.run == nil || r.run.Status() != execution.SessionRunRunning {
		return activeRunSnapshot{}, false
	}
	return activeRunSnapshot{
		RunID:     r.id,
		SessionID: r.sessionID,
		TurnID:    r.turnID,
		StartedAt: r.startedAt,
		Status:    string(execution.SessionRunRunning),
	}, true
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

type startRunRequest struct {
	Content  string                    `json:"content"`
	Images   []startRunImageAttachment `json:"images,omitempty"`
	Continue bool                      `json:"continue,omitempty"`
}

type startRunImageAttachment struct {
	DataURL string `json:"data_url"`
	Detail  string `json:"detail,omitempty"`
}

func (request startRunRequest) messageInput() (execution.SessionMessageInput, error) {
	if request.Continue {
		if strings.TrimSpace(request.Content) != "" || len(request.Images) != 0 {
			return execution.SessionMessageInput{}, fmt.Errorf("continue cannot include new message content; use POST /continue without content")
		}
		return execution.SessionMessageInput{Continue: true}, nil
	}
	if len(request.Images) > maxRunImageAttachments {
		return execution.SessionMessageInput{}, fmt.Errorf("at most %d images may be attached", maxRunImageAttachments)
	}

	blocks := make([]model.InputContentBlock, 0, len(request.Images))
	totalBytes := 0
	for index, attachment := range request.Images {
		normalizedMediaType, raw, err := model.ParseSupportedImageDataURL(attachment.DataURL)
		if err != nil {
			return execution.SessionMessageInput{}, fmt.Errorf("image %d: %w", index+1, err)
		}
		if len(raw) > maxRunImageBytes {
			return execution.SessionMessageInput{}, fmt.Errorf("image %d exceeds the %d MiB limit", index+1, maxRunImageBytes/(1024*1024))
		}
		totalBytes += len(raw)
		if totalBytes > maxRunImageTotalBytes {
			return execution.SessionMessageInput{}, fmt.Errorf("attached images exceed the %d MiB total limit", maxRunImageTotalBytes/(1024*1024))
		}

		detail := strings.ToLower(strings.TrimSpace(attachment.Detail))
		switch detail {
		case "", "auto":
			detail = "auto"
		case "low", "high":
		default:
			return execution.SessionMessageInput{}, fmt.Errorf("image %d has unsupported detail %q", index+1, attachment.Detail)
		}
		blocks = append(blocks, model.InputContentBlock{
			Type:     "input_image",
			ImageURL: model.ImageDataURL(normalizedMediaType, raw),
			Detail:   detail,
		})
	}
	if strings.TrimSpace(request.Content) == "" && len(blocks) == 0 {
		return execution.SessionMessageInput{}, fmt.Errorf("message content or image attachment is required")
	}
	return execution.SessionMessageInput{Content: request.Content, ContentBlocks: blocks}, nil
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var body startRunRequest
	if !decodeJSONWithLimit(w, r, &body, maxRunRequestBytes) {
		return
	}
	input, err := body.messageInput()
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_image_attachment", err.Error())
		return
	}
	if err := s.service.ValidateSessionMessageInput(r.PathValue("sessionID"), input); err != nil {
		if errors.Is(err, execution.ErrUnsupportedModelInput) {
			writeAPIError(w, http.StatusBadRequest, "unsupported_model_input", err.Error())
			return
		}
		writeServiceError(w, err)
		return
	}
	managed, err := s.runs.startWithInput(r.PathValue("sessionID"), input)
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

func (s *Server) handleContinueRun(w http.ResponseWriter, r *http.Request) {
	var body startRunRequest
	if !decodeOptionalJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Content) != "" || len(body.Images) != 0 || body.Continue {
		writeAPIError(w, http.StatusBadRequest, "invalid_continue", "Continue accepts no new content; send an empty POST body")
		return
	}
	sessionID := r.PathValue("sessionID")
	if err := s.service.ValidateContinue(sessionID); err != nil {
		writeServiceError(w, err)
		return
	}
	managed, err := s.runs.startContinue(sessionID)
	if err != nil {
		if errors.Is(err, ErrRunRegistryCapacity) {
			writeAPIError(w, http.StatusTooManyRequests, "run_capacity", "too many runs are currently active")
			return
		}
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"run_id": managed.id, "session_id": managed.sessionID, "status": string(execution.SessionRunRunning),
	})
}

func (s *Server) handleListActiveRuns(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"runs": s.runs.activeRuns()})
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

func (s *Server) handleCancelToolCall(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.runs.get(r.PathValue("runID"))
	if !ok {
		writeAPIError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	toolCallID := r.PathValue("toolCallID")
	if toolCallID == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_tool_call_id", "tool call id is required")
		return
	}
	if !managed.run.CancelToolCall(toolCallID) {
		writeAPIError(w, http.StatusNotFound, "not_found", "tool call not found or already finished")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"run_id":       managed.id,
		"tool_call_id": toolCallID,
		"status":       "cancelled",
	})
}

type appendActiveRequest struct {
	Content string `json:"content"`
}

// handleAppendActive queues a user prompt on a running session run. The prompt
// is appended to the in-flight turn at the next safe checkpoint, or sent as a
// follow-up turn if the active turn settles first; it is never dropped. The
// current queue is published to the run's event stream as run.prompt_queue.
func (s *Server) handleAppendActive(w http.ResponseWriter, r *http.Request) {
	var body appendActiveRequest
	if !decodeJSONWithLimit(w, r, &body, maxRunRequestBytes) {
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_content", "content is required")
		return
	}
	managed, ok := s.runs.get(r.PathValue("runID"))
	if !ok {
		writeAPIError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if err := managed.run.AppendActive(body.Content); err != nil {
		if errors.Is(err, execution.ErrSessionRunSettled) {
			writeAPIError(w, http.StatusConflict, "run_settled", "run is no longer accepting prompts")
			return
		}
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"run_id": managed.id,
		"status": "accepted",
	})
}

// handleRemoveActivePrompt deletes a not-yet-sent queued prompt from a running
// session run. Only prompts still in the queue can be removed; once a prompt
// has been drained into a turn it is durable session input. The updated queue
// is published to the run's event stream as run.prompt_queue.
func (s *Server) handleRemoveActivePrompt(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.runs.get(r.PathValue("runID"))
	if !ok {
		writeAPIError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if !managed.run.RemoveActive(r.PathValue("promptID")) {
		writeAPIError(w, http.StatusNotFound, "not_found", "queued prompt not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"run_id": managed.id,
		"status": "removed",
	})
}

type steerActivePromptRequest struct {
	Steer bool `json:"steer"`
}

// handleSteerActivePrompt marks a not-yet-sent queued prompt as a steer
// prompt (or demotes it back to the plain queue). Steer prompts always sort
// ahead of plain queued prompts and drain first; like every Web queued
// prompt they stay no-loss and run as a follow-up turn if the active turn
// settles first. The updated queue is published as run.prompt_queue.
func (s *Server) handleSteerActivePrompt(w http.ResponseWriter, r *http.Request) {
	var body steerActivePromptRequest
	if !decodeJSONWithLimit(w, r, &body, maxRunRequestBytes) {
		return
	}
	managed, ok := s.runs.get(r.PathValue("runID"))
	if !ok {
		writeAPIError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if !managed.run.SetActivePromptSteer(r.PathValue("promptID"), body.Steer) {
		writeAPIError(w, http.StatusNotFound, "not_found", "queued prompt not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"run_id": managed.id,
		"status": "updated",
	})
}

type moveActivePromptRequest struct {
	Direction string `json:"direction"`
}

// handleMoveActivePrompt reorders a not-yet-sent queued prompt one step up or
// down within its priority group (steer prompts ahead of plain queued
// prompts). Moves clamp at the group boundary. The updated queue is published
// as run.prompt_queue.
func (s *Server) handleMoveActivePrompt(w http.ResponseWriter, r *http.Request) {
	var body moveActivePromptRequest
	if !decodeJSONWithLimit(w, r, &body, maxRunRequestBytes) {
		return
	}
	delta := 0
	switch body.Direction {
	case "up":
		delta = -1
	case "down":
		delta = 1
	default:
		writeAPIError(w, http.StatusBadRequest, "invalid_direction", `direction must be "up" or "down"`)
		return
	}
	managed, ok := s.runs.get(r.PathValue("runID"))
	if !ok {
		writeAPIError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if !managed.run.MoveActivePrompt(r.PathValue("promptID"), delta) {
		writeAPIError(w, http.StatusNotFound, "not_found", "queued prompt not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"run_id": managed.id,
		"status": "updated",
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
			resyncFields := map[string]any{
				"run_id":                 managed.id,
				"session_id":             managed.sessionID,
				"oldest_seq":             oldestSeq, // Legacy name; this is a stream event ID.
				"oldest_stream_event_id": oldestSeq,
			}
			if requiredRevision, ok := s.runs.requiredRevision(managed.sessionID); ok {
				resyncFields["required_revision"] = requiredRevision
			}
			resync := execution.NewSessionStreamEvent("run.resync_required", resyncFields)
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

// requiredRevision returns the durable session watermark that a client should
// use when repairing a replay gap. It is intentionally omitted when the
// registry has no service or the session cannot be read; fabricating a
// revision would make the resync contract less reliable than omitting the
// additive hint.
func (r *runRegistry) requiredRevision(sessionID string) (string, bool) {
	lastSeq, ok := r.currentSessionWatermark(sessionID)
	if !ok {
		return "", false
	}
	return strconv.FormatInt(lastSeq, 10), true
}

func (r *runRegistry) currentSessionWatermark(sessionID string) (int64, bool) {
	if r == nil || r.service == nil {
		return 0, false
	}
	session, err := r.service.GetSession(sessionID)
	if err != nil {
		return 0, false
	}
	return session.LastSeq, true
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
