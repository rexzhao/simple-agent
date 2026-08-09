package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/execution"
)

type fastFailureWebTestRunner struct{}

func (fastFailureWebTestRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (fastFailureWebTestRunner) RunSessionTurn(context.Context, execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	return execution.SessionTurnResult{}, errors.New("fast runner failure")
}

func TestRunRegistryUsesExecutionCoordinatorCapacityError(t *testing.T) {
	if ErrRunRegistryCapacity != execution.ErrSessionRunCoordinatorCapacity {
		t.Fatalf("ErrRunRegistryCapacity = %v, want execution coordinator capacity error", ErrRunRegistryCapacity)
	}
}

func TestStartDurableHandoffAllowsSettledFastRun(t *testing.T) {
	tests := []struct {
		name   string
		runner execution.SessionTurnRunner
	}{
		{name: "committed", runner: webTestRunner{}},
		{name: "failed", runner: fastFailureWebTestRunner{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, service, app := newWebTestAppServerWithRunner(t, test.runner)
			_, session := createWebProjectAndSession(t, service)
			settled := make(chan struct{})
			app.runs.options.beforeDurableAdmissionCheck = func(run *execution.CoordinatedSessionRun) {
				result, runErr := run.Wait()
				// This is the same presentation callback used by the coordinator.
				// Invoke it synchronously before startDurable's post-admission
				// check, forcing the exact fast-run ordering under test.
				app.runs.settleRun(run, result, runErr)
				close(settled)
			}

			runID := "run-fast-handoff-" + test.name
			status, err := app.runs.startDurable(session.ID, "hello", runID, "fingerprint-"+test.name)
			if err != nil {
				t.Fatalf("startDurable() error = %v, want durable admission acknowledgement", err)
			}
			if status != string(execution.SessionRunRunning) {
				t.Fatalf("startDurable() status = %q, want %q admission status", status, execution.SessionRunRunning)
			}
			select {
			case <-settled:
			default:
				t.Fatal("settlement hook did not run before startDurable returned")
			}
			if _, ok := app.runs.get(runID); ok {
				t.Fatal("settled run leaked into the process-local control map")
			}
		})
	}
}

func TestStartDurableHandoffHonorsClose(t *testing.T) {
	_, service, app := newWebTestAppServerWithRunner(t, blockingWebTestRunner{})
	_, session := createWebProjectAndSession(t, service)
	closed := make(chan struct{})
	app.runs.options.beforeDurableAdmissionCheck = func(*execution.CoordinatedSessionRun) {
		app.runs.Close()
		close(closed)
	}

	_, err := app.runs.startDurable(session.ID, "hello", "run-close-handoff", "close-fingerprint")
	if !errors.Is(err, ErrRunRegistryClosed) {
		t.Fatalf("startDurable() error = %v, want ErrRunRegistryClosed after handoff close", err)
	}
	select {
	case <-closed:
	default:
		t.Fatal("close hook did not run")
	}
	if _, ok := app.runs.get("run-close-handoff"); ok {
		t.Fatal("closed registry retained the handed-off run")
	}
}

func TestRunSettledUsesOneDurableWatermarkWhenCancelledResultIsStale(t *testing.T) {
	_, service, app := newWebTestAppServerWithRunner(t, blockingWebTestRunner{})
	_, session := createWebProjectAndSession(t, service)
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
	managed := newManagedRun(run.ID(), session.ID)
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

	var logs synchronizedLogBuffer
	registry := newRunRegistry(context.Background(), service, &logs)
	defer registry.Close()
	run, err := registry.coordinator.Start("missing-session", execution.SessionMessageInput{Content: "hello"}, nil)
	if err != nil {
		t.Fatalf("coordinator.Start() error = %v", err)
	}
	if _, err := run.Wait(); err == nil {
		t.Fatal("missing-session run unexpectedly succeeded")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := logs.String()
		if strings.Contains(got, "missing-session") && strings.Contains(got, "session not found") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("failure log = %q, want session id and underlying error", logs.String())
}

type synchronizedLogBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (buffer *synchronizedLogBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.b.Write(value)
}

func (buffer *synchronizedLogBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.b.String()
}

func TestRunRegistryAdoptsAgentStartedCoordinatorRun(t *testing.T) {
	runner := enteredBlockingWebTestRunner{entered: make(chan struct{}), once: &sync.Once{}}
	_, service, app := newWebTestAppServerWithRunner(t, runner)
	_, session := createWebProjectAndSession(t, service)

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
	if run.Status() != execution.SessionRunRunning {
		t.Fatalf("adopted run status = %s, want active", run.Status())
	}

	run.Cancel()
	if _, err := run.Wait(); err == nil {
		t.Fatal("cancelled agent-started run returned nil error")
	}
	if run.Status() != execution.SessionRunCancelled {
		t.Fatalf("adopted run settled status = %s, want cancelled", run.Status())
	}
}
