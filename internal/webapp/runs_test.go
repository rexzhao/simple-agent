package webapp

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/execution"
)

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

func TestRunRegistryRejectsRunsAboveConcurrentLimit(t *testing.T) {
	registry := newRunRegistryWithOptions(context.Background(), nil, nil, runRegistryOptions{MaxConcurrentRuns: 1})
	defer registry.Close()
	registry.mu.Lock()
	registry.activeBySession["already-running"] = newManagedRun("run-active", "already-running", registry.options)
	registry.mu.Unlock()

	_, err := registry.start("another-session", "hello")
	if !errors.Is(err, ErrRunRegistryCapacity) {
		t.Fatalf("start() error = %v, want ErrRunRegistryCapacity", err)
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
	if !strings.Contains(body, `"type":"run.settled"`) {
		t.Fatalf("SSE body missing terminal event: %s", body)
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
