package webapp

import (
	"context"
	"encoding/json"
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

func TestRunSettledUsesOneDurableWatermarkWhenCancelledResultIsStale(t *testing.T) {
	server, service, app := newWebTestAppServerWithRunner(t, blockingWebTestRunner{})
	_, session := createWebProjectAndSession(t, server)
	subscription := service.LifecycleHub().Subscribe()
	defer subscription.Close()

	// Use a coordinator without the Web registry callbacks so the test can
	// inject a deliberately stale execution result into settleRun while the
	// cancelled run has already committed its durable interruption records.
	coordinator := execution.NewSessionRunCoordinator(context.Background(), service, execution.SessionRunCoordinatorOptions{
		NewRunID: func() (string, error) { return "run-stale-settlement", nil },
	})
	defer coordinator.Close()
	service.SetSessionRunCoordinator(coordinator)
	defer service.SetSessionRunCoordinator(app.runs.coordinator)
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
	if !managed.isTerminal() {
		t.Fatal("managed run is not terminal after settlement")
	}

	var payload map[string]any
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case event := <-subscription.Events():
			if event.Type != execution.LifecycleRunSettled || !strings.Contains(string(event.Payload), run.ID()) {
				continue
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("decode run.settled lifecycle payload: %v", err)
			}
			goto settledEventReceived
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("run.settled lifecycle event was not observed")

settledEventReceived:
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
	deadline := time.Now().Add(5 * time.Second)
	for !managed.isTerminal() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !managed.isTerminal() {
		t.Fatal("failed run did not become terminal")
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
	if managed.isTerminal() || run.Status() != execution.SessionRunRunning {
		t.Fatalf("adopted run status = managed terminal:%t run:%s, want active", managed.isTerminal(), run.Status())
	}

	run.Cancel()
	if _, err := run.Wait(); err == nil {
		t.Fatal("cancelled agent-started run returned nil error")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !managed.isTerminal() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !managed.isTerminal() || run.Status() != execution.SessionRunCancelled {
		t.Fatalf("adopted run settled state = terminal:%t status:%s", managed.isTerminal(), run.Status())
	}
}

func registerTerminalRunForTest(registry *runRegistry, id string, finishedAt time.Time) *managedRun {
	managed := newManagedRun(id, "session-"+id, registry.options)
	managed.finish(finishedAt.UTC())
	registry.mu.Lock()
	registry.byID[managed.id] = managed
	registry.retainTerminalLocked(managed)
	registry.mu.Unlock()
	return managed
}
