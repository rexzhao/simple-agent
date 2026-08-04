package webapp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/execution"
)

func TestContinueRequestRejectsNewContent(t *testing.T) {
	if _, err := (startRunRequest{Continue: true, Content: "new input"}).messageInput(); err == nil {
		t.Fatal("continue request with content was accepted")
	}
}

func TestManagedRunBoundsActiveReplayBuffer(t *testing.T) {
	options := runRegistryOptions{
		MaxRunEvents:     3,
		MaxRunEventBytes: 512,
	}.withDefaults()
	managed := newManagedRun("run-buffer", "session-buffer", options)
	for index := 0; index < 10; index++ {
		managed.append(execution.NewSessionStreamEvent("text.delta", map[string]any{
			"text": strings.Repeat("x", 80),
		}))
	}

	events, terminal, resyncRequired, oldestSeq, _ := managed.snapshot(0)
	if terminal {
		t.Fatal("managed run unexpectedly terminal")
	}
	if !resyncRequired || oldestSeq <= 1 {
		t.Fatalf("snapshot replay state = resync:%t oldest:%d, want a truncated replay window", resyncRequired, oldestSeq)
	}
	if len(events) > options.MaxRunEvents {
		t.Fatalf("retained events = %d, want at most %d", len(events), options.MaxRunEvents)
	}
	managed.mu.Lock()
	bytes := managed.eventBytes
	managed.mu.Unlock()
	if bytes > options.MaxRunEventBytes {
		t.Fatalf("retained event bytes = %d, want at most %d", bytes, options.MaxRunEventBytes)
	}
	if len(events) == 0 || events[len(events)-1].Seq != 10 {
		t.Fatalf("retained events = %#v, want newest event sequence 10", events)
	}
}

func TestManagedRunFinishCollapsesReplayToSettledEvent(t *testing.T) {
	options := runRegistryOptions{}.withDefaults()
	managed := newManagedRun("run-terminal", "session-terminal", options)
	managed.append(execution.NewSessionStreamEvent("text.delta", map[string]any{"text": "partial"}))
	managed.append(execution.NewSessionStreamEvent("tool.started", map[string]any{"name": "read_file"}))
	managed.append(execution.NewSessionStreamEvent("run.settled", map[string]any{"status": "committed"}))
	managed.finish(time.Now().UTC())

	events, terminal, resyncRequired, oldestSeq, _ := managed.snapshot(0)
	if !terminal {
		t.Fatal("managed run terminal = false, want true")
	}
	if len(events) != 1 || !strings.Contains(string(events[0].Payload), `"type":"run.settled"`) {
		t.Fatalf("terminal replay events = %#v, want only run.settled", events)
	}
	if !resyncRequired || oldestSeq != events[0].Seq {
		t.Fatalf("terminal replay state = resync:%t oldest:%d events:%#v", resyncRequired, oldestSeq, events)
	}
}

func TestRunRegistryUsesExecutionCoordinatorCapacityError(t *testing.T) {
	if ErrRunRegistryCapacity != execution.ErrSessionRunCoordinatorCapacity {
		t.Fatalf("ErrRunRegistryCapacity = %v, want execution coordinator capacity error", ErrRunRegistryCapacity)
	}
}

func TestRunRegistryEvictsTerminalRunsByTTLAndLimit(t *testing.T) {
	registry := newRunRegistryWithOptions(context.Background(), nil, nil, runRegistryOptions{
		MaxTerminalRuns: 2,
		TerminalRunTTL:  30 * time.Millisecond,
	})
	defer registry.Close()

	first := registerTerminalRunForTest(registry, "run-first", time.Now().Add(-time.Second))
	second := registerTerminalRunForTest(registry, "run-second", time.Now())
	third := registerTerminalRunForTest(registry, "run-third", time.Now().Add(time.Second))
	if _, ok := registry.get(first.id); ok {
		t.Fatal("oldest terminal run remained after terminal run limit eviction")
	}
	if _, ok := registry.get(second.id); !ok {
		t.Fatal("second terminal run was unexpectedly evicted by terminal run limit")
	}
	if _, ok := registry.get(third.id); !ok {
		t.Fatal("newest terminal run was unexpectedly evicted by terminal run limit")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, secondPresent := registry.get(second.id)
		_, thirdPresent := registry.get(third.id)
		if !secondPresent && !thirdPresent {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("terminal runs were not evicted after their TTL")
}

func TestRunEventsSignalsResyncForTruncatedReplay(t *testing.T) {
	options := runRegistryOptions{MaxRunEvents: 1}.withDefaults()
	managed := newManagedRun("run-resync", "session-resync", options)
	managed.append(execution.NewSessionStreamEvent("text.delta", map[string]any{"text": "first"}))
	managed.append(execution.NewSessionStreamEvent("run.settled", map[string]any{"status": "committed"}))
	managed.finish(time.Now().UTC())

	registry := newRunRegistryWithOptions(context.Background(), nil, nil, options)
	defer registry.Close()
	registry.mu.Lock()
	registry.byID[managed.id] = managed
	registry.mu.Unlock()
	server := &Server{runs: registry}

	request := httptest.NewRequest("GET", "/api/runs/"+managed.id+"/events", nil)
	request.SetPathValue("runID", managed.id)
	response := httptest.NewRecorder()
	server.handleRunEvents(response, request)

	body := response.Body.String()
	if !strings.Contains(body, `"type":"run.resync_required"`) {
		t.Fatalf("SSE body missing resync event: %s", body)
	}
	if !strings.Contains(body, `"oldest_stream_event_id":2`) || !strings.Contains(body, `"oldest_seq":2`) {
		t.Fatalf("SSE body missing compatible oldest stream event IDs: %s", body)
	}
	if !strings.Contains(body, `"type":"run.settled"`) {
		t.Fatalf("SSE body missing terminal event: %s", body)
	}
}

func TestRunEventsResyncIncludesDurableRequiredRevision(t *testing.T) {
	service, err := execution.NewServiceWithOptions(t.TempDir(), execution.ServiceOptions{TurnRunner: webTestRunner{}})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	project, err := service.CreateProject(t.TempDir(), "resync revision")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	session, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	registry := newRunRegistryWithOptions(context.Background(), service, nil, runRegistryOptions{MaxRunEvents: 1})
	defer registry.Close()
	managed := newManagedRun("run-required-revision", session.ID, registry.options)
	managed.append(execution.NewSessionStreamEvent("text.delta", map[string]any{"text": "first"}))
	managed.append(execution.NewSessionStreamEvent("run.settled", map[string]any{"status": "committed"}))
	managed.finish(time.Now().UTC())
	registry.mu.Lock()
	registry.byID[managed.id] = managed
	registry.mu.Unlock()

	server := &Server{runs: registry}
	request := httptest.NewRequest("GET", "/api/runs/"+managed.id+"/events?after=0", nil)
	request.SetPathValue("runID", managed.id)
	response := httptest.NewRecorder()
	server.handleRunEvents(response, request)

	body := response.Body.String()
	if !strings.Contains(body, `"required_revision":"0"`) {
		t.Fatalf("SSE body missing durable required_revision: %s", body)
	}
	if !strings.Contains(body, `"oldest_stream_event_id":2`) {
		t.Fatalf("SSE body missing oldest_stream_event_id: %s", body)
	}
}

func TestRunSettledUsesOneDurableWatermarkWhenCancelledResultIsStale(t *testing.T) {
	server, service, app := newWebTestAppServerWithRunner(t, blockingWebTestRunner{})
	_, session := createWebProjectAndSession(t, server)

	// Use a coordinator without the Web registry callbacks so the test can
	// inject a deliberately stale execution result into settleRun while the
	// cancelled run has already committed its durable interruption records.
	coordinator := execution.NewSessionRunCoordinator(context.Background(), service, execution.SessionRunCoordinatorOptions{
		NewRunID: func() (string, error) { return "run-stale-settlement", nil },
	})
	defer coordinator.Close()
	run, err := coordinator.Start(session.ID, execution.SessionMessageInput{Content: "cancel me"}, nil)
	if err != nil {
		t.Fatalf("coordinator.Start() error = %v", err)
	}
	managed := newManagedRun(run.ID(), session.ID, app.runs.options)
	managed.run = run
	app.runs.mu.Lock()
	app.runs.byID[managed.id] = managed
	app.runs.mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := service.GetSession(session.ID)
		if getErr == nil && current.RunningRunID == run.ID() {
			break
		}
		select {
		case <-run.Done():
			t.Fatal("run settled before durable running state was observed")
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	current, err := service.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession(before cancel) error = %v", err)
	}
	if current.RunningRunID != run.ID() {
		t.Fatalf("running_run_id = %q, want %q", current.RunningRunID, run.ID())
	}

	run.Cancel()
	result, runErr := run.Wait()
	if runErr == nil || run.Status() != execution.SessionRunCancelled {
		t.Fatalf("cancelled run status/error = %s/%v, want cancelled with an error", run.Status(), runErr)
	}
	final, err := service.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession(after cancel) error = %v", err)
	}
	if final.LastSeq <= 0 {
		t.Fatalf("cancelled session LastSeq = %d, want durable interruption records", final.LastSeq)
	}
	// Model a stale result reaching the presentation adapter. The durable
	// interruption revision is intentionally newer than this result value.
	result.LastSeq = final.LastSeq - 1
	if result.LastSeq >= final.LastSeq {
		t.Fatalf("stale result LastSeq = %d, final durable LastSeq = %d", result.LastSeq, final.LastSeq)
	}
	app.runs.settleRun(run, result, runErr)

	events, terminal, _, _, _ := managed.snapshot(0)
	if !terminal || len(events) != 1 {
		t.Fatalf("settled replay = terminal:%t events:%#v, want one terminal event", terminal, events)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("decode run.settled payload: %v", err)
	}
	gotLastSeq, ok := payload["last_seq"].(float64)
	if !ok {
		t.Fatalf("run.settled last_seq = %#v, want numeric field", payload["last_seq"])
	}
	gotRevision, ok := payload["committed_revision"].(string)
	if !ok {
		t.Fatalf("run.settled committed_revision = %#v, want decimal string", payload["committed_revision"])
	}
	if int64(gotLastSeq) != final.LastSeq || gotRevision != strconv.FormatInt(final.LastSeq, 10) || gotRevision != strconv.FormatInt(int64(gotLastSeq), 10) {
		t.Fatalf("run.settled watermark fields = last_seq:%v committed_revision:%q, want %d/%q", gotLastSeq, gotRevision, final.LastSeq, strconv.FormatInt(final.LastSeq, 10))
	}
}

func TestRunEventsAfterZeroReplaysTerminalRunWithStableIDs(t *testing.T) {
	options := runRegistryOptions{
		MaxRunEvents:     8,
		MaxRunEventBytes: 512,
	}.withDefaults()
	managed := newManagedRun("run-terminal-replay", "session-terminal-replay", options)
	managed.append(execution.NewSessionStreamEvent("turn.started", map[string]any{
		"turn_id": "turn-terminal-replay",
	}))
	managed.append(execution.NewSessionStreamEvent("text.delta", map[string]any{
		"text": "hello",
	}))
	managed.append(execution.NewSessionStreamEvent("run.settled", map[string]any{
		"run_id": "run-terminal-replay",
		"status": "completed",
	}))
	managed.finish(time.Now().UTC())

	registry := newRunRegistryWithOptions(context.Background(), nil, nil, options)
	defer registry.Close()
	registry.mu.Lock()
	registry.byID[managed.id] = managed
	registry.mu.Unlock()
	server := &Server{runs: registry}

	capture := func(after string) string {
		request := httptest.NewRequest("GET", "/api/runs/"+managed.id+"/events?after="+after, nil)
		request.SetPathValue("runID", managed.id)
		response := httptest.NewRecorder()
		server.handleRunEvents(response, request)
		return response.Body.String()
	}

	first := capture("0")
	if !strings.Contains(first, "run.resync_required") {
		t.Fatalf("after=0 terminal replay should require resync after terminal compaction, body=%q", first)
	}
	if !strings.Contains(first, "id: 3\n") {
		t.Fatalf("terminal replay should retain the original SSE event ID, body=%q", first)
	}
	if strings.Count(first, "id: ") != 1 {
		t.Fatalf("resync notification must not consume an SSE event ID, body=%q", first)
	}
	if !strings.Contains(first, `"oldest_stream_event_id":3`) || !strings.Contains(first, `"oldest_seq":3`) {
		t.Fatalf("resync notification missing compatible oldest stream event IDs, body=%q", first)
	}

	second := capture("0")
	if second != first {
		t.Fatalf("replaying the same terminal run from a second connection changed the stream:\nfirst=%q\nsecond=%q", first, second)
	}

	fromSettled := capture("2")
	if strings.Contains(fromSettled, "run.resync_required") || !strings.Contains(fromSettled, "id: 3\n") {
		t.Fatalf("after=2 should replay only the settled event without resync, body=%q", fromSettled)
	}
	if strings.Count(fromSettled, "id: ") != 1 {
		t.Fatalf("after=2 should emit exactly one retained event, body=%q", fromSettled)
	}

	if body := capture("3"); body != "" {
		t.Fatalf("after the terminal event should return an empty replay, body=%q", body)
	}
}

type eventBeforeStartReturnStarter struct {
	service                   *execution.Service
	admitted                  *bool
	emittedBeforeRegistration *bool
}

func (s eventBeforeStartReturnStarter) StartSessionRunWithInput(
	ctx context.Context,
	sessionID string,
	input execution.SessionMessageInput,
	emit func(execution.SessionStreamEvent),
) *execution.SessionRun {
	if s.admitted != nil && !*s.admitted && s.emittedBeforeRegistration != nil {
		*s.emittedBeforeRegistration = true
	}
	if emit != nil {
		emit(execution.NewSessionStreamEvent("turn.started", map[string]any{
			"turn_id": "turn-before-start-return",
		}))
	}
	return s.service.StartSessionRunWithInput(ctx, sessionID, input, emit)
}

func TestRunRegistryAdoptsEventsObservedBeforeCoordinatorStartReturns(t *testing.T) {
	server, service, app := newWebTestAppServerWithRunner(t, webTestRunner{})
	_, session := createWebProjectAndSession(t, server)
	admitted := false
	emittedBeforeRegistration := false

	coordinator := execution.NewSessionRunCoordinator(context.Background(), eventBeforeStartReturnStarter{
		service:                   service,
		admitted:                  &admitted,
		emittedBeforeRegistration: &emittedBeforeRegistration,
	}, execution.SessionRunCoordinatorOptions{
		NewRunID: func() (string, error) {
			return "run-before-start-return", nil
		},
		OnRunAdmitted: func(run *execution.CoordinatedSessionRun) error {
			if err := app.runs.admitRun(run); err != nil {
				return err
			}
			admitted = true
			return nil
		},
		OnRunAdmissionFailed: app.runs.rejectRun,
		OnRunEvent:           app.runs.observeRunEvent,
		OnRunSettled:         app.runs.settleRun,
	})
	defer coordinator.Close()

	run, err := coordinator.Start(session.ID, execution.SessionMessageInput{Content: "hello"}, nil)
	if err != nil {
		t.Fatalf("coordinator.Start() error = %v", err)
	}
	if emittedBeforeRegistration {
		t.Fatal("synchronous starter emitted before replay registration")
	}
	managed, ok := app.runs.get(run.ID())
	if !ok {
		t.Fatal("run registry lost the event observed before Start returned")
	}
	if managed.run != run {
		t.Fatal("run registry did not reconcile the early event with the returned coordinated run")
	}
	events, _, _, _, _ := managed.snapshot(0)
	if len(events) == 0 || !strings.Contains(string(events[0].Payload), "turn-before-start-return") {
		t.Fatalf("early event was not retained as the first replay event: %#v", events)
	}
}

func TestRunRegistryLogsUnderlyingFailure(t *testing.T) {
	service, err := execution.NewServiceWithOptions(t.TempDir(), execution.ServiceOptions{TurnRunner: webTestRunner{}})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}

	var logs strings.Builder
	registry := newRunRegistry(context.Background(), service, &logs)
	defer registry.Close()
	managed, err := registry.start("missing-session", "hello")
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	for {
		_, terminal, _, _, changed := managed.snapshot(0)
		if terminal {
			break
		}
		<-changed
	}

	got := logs.String()
	if !strings.Contains(got, "missing-session") || !strings.Contains(got, "session not found") {
		t.Fatalf("failure log = %q, want session id and underlying error", got)
	}
}

func TestRunRegistryAdoptsAgentStartedCoordinatorRun(t *testing.T) {
	runner := enteredBlockingWebTestRunner{entered: make(chan struct{}), once: &sync.Once{}}
	server, _, app := newWebTestAppServerWithRunner(t, runner)
	_, session := createWebProjectAndSession(t, server)

	run, err := app.runs.coordinator.Start(session.ID, execution.SessionMessageInput{Content: "agent-started"}, nil)
	if err != nil {
		t.Fatalf("coordinator.Start() error = %v", err)
	}
	select {
	case <-runner.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("agent-started run did not enter runner")
	}

	managed, ok := app.runs.get(run.ID())
	if !ok || managed.run != run || managed.sessionID != session.ID {
		t.Fatalf("adopted run = %#v/%t, want coordinator handle", managed, ok)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, terminal, _, _, changed := managed.snapshot(0)
		if len(events) > 0 && strings.Contains(string(events[0].Payload), `"type":"turn.started"`) {
			if terminal {
				t.Fatal("agent-started run became terminal before cancellation")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent-started event was not replayable: %#v", events)
		}
		select {
		case <-changed:
		case <-time.After(10 * time.Millisecond):
		}
	}

	run.Cancel()
	if _, err := run.Wait(); err == nil {
		t.Fatal("cancelled agent-started run returned nil error")
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		events, terminal, _, _, changed := managed.snapshot(0)
		if terminal {
			if len(events) != 1 || !strings.Contains(string(events[0].Payload), `"type":"run.settled"`) || !strings.Contains(string(events[0].Payload), `"status":"cancelled"`) {
				t.Fatalf("terminal replay = %#v", events)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("adopted run did not become terminal")
		}
		select {
		case <-changed:
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func registerTerminalRunForTest(registry *runRegistry, id string, finishedAt time.Time) *managedRun {
	managed := newManagedRun(id, "session-"+id, registry.options)
	managed.append(execution.NewSessionStreamEvent("run.settled", map[string]any{"status": "committed"}))
	managed.finish(finishedAt.UTC())
	registry.mu.Lock()
	registry.byID[managed.id] = managed
	registry.retainTerminalLocked(managed)
	registry.mu.Unlock()
	return managed
}
