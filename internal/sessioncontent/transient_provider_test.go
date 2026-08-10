package sessioncontent

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

type noSubscriberPressureRunner struct {
	started chan struct{}
	release chan struct{}
}

type openBarrierExecutionRunner struct {
	started chan struct{}
	release chan struct{}
}

func (runner *openBarrierExecutionRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (runner *openBarrierExecutionRunner) RunSessionTurn(ctx context.Context, request execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	request.Emit(model.AgentIterationStartedEvent{Iteration: 1})
	request.Emit(model.TextDeltaEvent{Text: "after barrier", AssistantItemID: "item-barrier"})
	close(runner.started)
	select {
	case <-runner.release:
		return execution.SessionTurnResult{Incremental: true}, nil
	case <-ctx.Done():
		return execution.SessionTurnResult{}, ctx.Err()
	}
}

func (runner *noSubscriberPressureRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (runner *noSubscriberPressureRunner) RunSessionTurn(ctx context.Context, _ execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	close(runner.started)
	select {
	case <-runner.release:
		return execution.SessionTurnResult{Incremental: true}, nil
	case <-ctx.Done():
		return execution.SessionTurnResult{}, ctx.Err()
	}
}

func TestTransientRunSplitsUTF8AndDoesNotAdvanceDurableSequence(t *testing.T) {
	store, session := newContentTestStore(t, "transient-split")
	provider, err := NewProvider(store, ProviderOptions{TransientReplayEntries: 16, TransientReplayBytes: 2 * 1024 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	opened := openContent(t, provider, session.ID, nil)
	defer opened.Close()

	provider.mu.Lock()
	owner := provider.owners[session.ID]
	provider.mu.Unlock()
	if owner == nil {
		t.Fatal("owner was not retained")
	}
	owner.handleRunAdmitted(runAdmission{runID: "run-split", sessionID: session.ID})
	readTransientEvent(t, opened, protocol.SubscriptionEventRunStarted, "1")

	want := strings.Repeat("界", 100000)
	owner.handleRunEvent(runEventInput{runID: "run-split", sessionID: session.ID, event: execution.NewSessionStreamEvent("text.delta", map[string]any{
		"turn_id": "turn-1", "agent_iteration": 1, "item_id": "item-1", "text": want,
	})})
	var got strings.Builder
	for cursor := uint64(2); cursor <= 4; cursor++ {
		event := readTransientEvent(t, opened, protocol.SubscriptionEventTextDelta, strconv.FormatUint(cursor, 10))
		if !utf8.ValidString(event.Delta) {
			t.Fatalf("split delta at cursor %s is not valid UTF-8", event.RunCursor)
		}
		got.WriteString(event.Delta)
	}
	if got.String() != want {
		t.Fatalf("split delta reconstructed %d bytes, want %d", got.Len(), len(want))
	}
	if got := owner.journal.LastSequence(); got != opened.Sequence {
		t.Fatalf("transient run advanced durable sequence to %d, baseline was %d", got, opened.Sequence)
	}
}

func TestTransientToolProgressSplitsUTF8AtFrameBoundary(t *testing.T) {
	store, session := newContentTestStore(t, "transient-tool-split")
	provider, err := NewProvider(store, ProviderOptions{TransientReplayEntries: 16, TransientReplayBytes: 2 * 1024 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	opened := openContent(t, provider, session.ID, nil)
	defer opened.Close()
	provider.mu.Lock()
	owner := provider.owners[session.ID]
	provider.mu.Unlock()
	owner.handleRunAdmitted(runAdmission{runID: "run-tool-split", sessionID: session.ID})
	readTransientEvent(t, opened, protocol.SubscriptionEventRunStarted, "1")

	want := strings.Repeat("参数", 70000)
	owner.handleRunEvent(runEventInput{runID: "run-tool-split", sessionID: session.ID, event: execution.NewSessionStreamEvent("tool.progress", map[string]any{
		"turn_id": "turn-1", "agent_iteration": 1, "tool_call_id": "call-1", "name": "shell", "arguments_delta": want,
	})})
	var got strings.Builder
	for cursor := uint64(2); ; cursor++ {
		event := readTransientEvent(t, opened, protocol.SubscriptionEventToolProgress, strconv.FormatUint(cursor, 10))
		got.WriteString(event.ArgumentsDelta)
		if got.Len() == len(want) {
			break
		}
		if got.Len() > len(want) {
			t.Fatalf("split tool progress exceeded source length: %d > %d", got.Len(), len(want))
		}
	}
	if got.String() != want {
		t.Fatalf("split tool progress reconstructed %d bytes, want %d", got.Len(), len(want))
	}
}

func readTransientEvent(t *testing.T, opened syncengine.OpenedResource, wantType protocol.SubscriptionEventType, wantCursor string) protocol.TransientSubscriptionEvent {
	t.Helper()
	select {
	case entry := <-opened.Transient.Events:
		event, err := protocol.DecodeSubscriptionEvent(entry.Event)
		if err != nil {
			t.Fatal(err)
		}
		if event.Type != wantType || event.RunCursor != protocol.RunCursor(wantCursor) {
			t.Fatalf("transient event = %#v, want %q/%q", event, wantType, wantCursor)
		}
		return event
	case terminal := <-opened.Transient.Terminal:
		t.Fatalf("transient delivery terminated unexpectedly: %v", terminal.Reason)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for transient event")
	}
	return protocol.TransientSubscriptionEvent{}
}

func TestTransientReplayRetentionIsSlidingAndLiveRemainsContinuous(t *testing.T) {
	store, session := newContentTestStore(t, "transient-retention")
	provider, err := NewProvider(store, ProviderOptions{TransientReplayEntries: 2, TransientReplayBytes: 1 << 20, TransientLiveCapacity: 32})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	opened := openContent(t, provider, session.ID, nil)
	defer opened.Close()
	provider.mu.Lock()
	owner := provider.owners[session.ID]
	provider.mu.Unlock()
	owner.handleRunAdmitted(runAdmission{runID: "run-retention", sessionID: session.ID})
	readTransientEvent(t, opened, protocol.SubscriptionEventRunStarted, "1")
	for cursor := uint64(2); cursor <= 8; cursor++ {
		owner.handleRunEvent(runEventInput{runID: "run-retention", sessionID: session.ID, event: execution.NewSessionStreamEvent("text.delta", map[string]any{
			"turn_id": "turn-1", "agent_iteration": 1, "item_id": "item-1", "text": "x",
		})})
		readTransientEvent(t, opened, protocol.SubscriptionEventTextDelta, strconv.FormatUint(cursor, 10))
	}
	if got := owner.journal.LastSequence(); got != opened.Sequence {
		t.Fatalf("transient retention advanced durable sequence to %d, baseline was %d", got, opened.Sequence)
	}
	owner.mu.Lock()
	if len(owner.transientRun.replay) > 2 || owner.transientRun.replayBytes > provider.options.TransientReplayBytes {
		t.Fatalf("replay retention exceeded bounds: entries=%d bytes=%d", len(owner.transientRun.replay), owner.transientRun.replayBytes)
	}
	first := owner.transientRun.replay[0].Cursor
	last := owner.transientRun.replay[len(owner.transientRun.replay)-1].Cursor
	epoch := owner.transientRun.epoch
	owner.mu.Unlock()
	if first != "7" || last != "8" {
		t.Fatalf("retained replay cursors = %q..%q, want 7..8", first, last)
	}

	old, err := provider.OpenWithRunResume(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: session.ID}, nil, &protocol.RunResumeToken{RunEpoch: epoch, RunID: "run-retention", RunCursor: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if old.TransientResync != "active_run_cursor_too_old" {
		t.Fatalf("old run resume reason = %q, want active_run_cursor_too_old", old.TransientResync)
	}
	old.Close()

	window, err := provider.OpenWithRunResume(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: session.ID}, nil, &protocol.RunResumeToken{RunEpoch: epoch, RunID: "run-retention", RunCursor: "6"})
	if err != nil {
		t.Fatal(err)
	}
	defer window.Close()
	if window.TransientResync != "" {
		t.Fatalf("window resume unexpectedly resynced: %q", window.TransientResync)
	}
	if len(window.TransientReplay) != 2 || window.TransientReplay[0].Cursor != "7" || window.TransientReplay[1].Cursor != "8" {
		t.Fatalf("window replay = %#v, want cursors 7 and 8", window.TransientReplay)
	}
}

func TestTransientReplayByteRetentionEvictsOldEntriesWithoutBreakingLive(t *testing.T) {
	store, session := newContentTestStore(t, "transient-byte-retention")
	provider, err := NewProvider(store, ProviderOptions{TransientReplayEntries: 16, TransientReplayBytes: 16 * 1024, TransientLiveCapacity: 32})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	opened := openContent(t, provider, session.ID, nil)
	defer opened.Close()
	provider.mu.Lock()
	owner := provider.owners[session.ID]
	provider.mu.Unlock()
	owner.handleRunAdmitted(runAdmission{runID: "run-byte-retention", sessionID: session.ID})
	readTransientEvent(t, opened, protocol.SubscriptionEventRunStarted, "1")
	for cursor := uint64(2); cursor <= 9; cursor++ {
		owner.handleRunEvent(runEventInput{runID: "run-byte-retention", sessionID: session.ID, event: execution.NewSessionStreamEvent("text.delta", map[string]any{
			"turn_id": "turn-byte", "agent_iteration": 1, "item_id": "item-byte", "text": "byte-window",
		})})
		readTransientEvent(t, opened, protocol.SubscriptionEventTextDelta, strconv.FormatUint(cursor, 10))
	}
	owner.mu.Lock()
	if owner.transientRun.replayBytes > provider.options.TransientReplayBytes || len(owner.transientRun.replay) >= 16 {
		t.Fatalf("byte replay retention exceeded bounds: entries=%d bytes=%d", len(owner.transientRun.replay), owner.transientRun.replayBytes)
	}
	first, _ := strconv.ParseUint(string(owner.transientRun.replay[0].Cursor), 10, 64)
	owner.mu.Unlock()
	if first <= 2 {
		t.Fatalf("byte retention kept old cursor %d, want eviction beyond cursor 2", first)
	}
	owner.mu.Lock()
	epoch := owner.transientRun.epoch
	owner.mu.Unlock()
	old, err := provider.OpenWithRunResume(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: session.ID}, nil, &protocol.RunResumeToken{RunEpoch: epoch, RunID: "run-byte-retention", RunCursor: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if old.TransientResync != "active_run_cursor_too_old" {
		t.Fatalf("byte-window old resume reason = %q", old.TransientResync)
	}
	old.Close()
}

func TestTransientEventDuringOpenBarrierIsDeliveredAfterRegistration(t *testing.T) {
	runner := &openBarrierExecutionRunner{started: make(chan struct{}), release: make(chan struct{})}
	service, err := execution.NewServiceWithOptions(t.TempDir(), execution.ServiceOptions{TurnRunner: runner})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(t.TempDir(), "transient barrier")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{DisplayName: "transient barrier", CreatedCWD: project.Project.Root, ConfigPath: service.ConfigPath(), Provider: "fake", ModelProfile: "default", ModelID: "default"})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	releaseSnapshot := make(chan struct{})
	var once sync.Once
	provider, err := NewProvider(service.SessionStore(), ProviderOptions{BeforeSnapshot: func(string) {
		once.Do(func() {
			close(entered)
			<-releaseSnapshot
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	openedCh := make(chan syncengine.OpenedResource, 1)
	errCh := make(chan error, 1)
	go func() {
		opened, openErr := provider.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: session.ID}, nil)
		openedCh <- opened
		errCh <- openErr
	}()
	<-entered
	provider.mu.Lock()
	owner := provider.owners[session.ID]
	provider.mu.Unlock()
	if owner == nil {
		t.Fatal("owner was not allocated before the open barrier")
	}
	if owner.openInterest.Load() <= 0 {
		t.Fatal("open barrier did not publish atomic open interest")
	}
	if owner.subscriberHint.Load() != 0 {
		t.Fatalf("subscriber hint = %d during pre-registration barrier, want zero", owner.subscriberHint.Load())
	}
	coordinator := execution.NewSessionRunCoordinator(context.Background(), service, execution.SessionRunCoordinatorOptions{NewRunID: func() (string, error) { return "run-barrier", nil }})
	unregister := coordinator.RegisterRunEventObserver(provider)
	run, err := coordinator.Start(session.ID, execution.SessionMessageInput{Content: "barrier"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("barrier run did not start")
	}
	// The coordinator's registered observer invokes provider.RunAdmitted and
	// provider.RunEvent from the real execution source while the snapshot
	// barrier is blocked. They must be admitted by openInterest and remain
	// ordered behind the open task rather than manufacturing a gap.
	close(releaseSnapshot)
	opened := <-openedCh
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if got := owner.openInterest.Load(); got != 0 {
		t.Fatalf("open interest after capture barrier = %d, want zero", got)
	}
	defer opened.Close()
	readTransientEvent(t, opened, protocol.SubscriptionEventRunStarted, "1")
	text := readTransientEvent(t, opened, protocol.SubscriptionEventTextDelta, "2")
	if text.RunID != run.ID() || text.Delta != "after barrier" {
		t.Fatalf("barrier text event = %#v, want run %q and cursor 2", text, run.ID())
	}
	close(runner.release)
	if _, err := run.Wait(); err != nil {
		t.Fatalf("barrier run.Wait() error = %v", err)
	}
	unregister()
	coordinator.Close()
}

func TestLateOldRunEventCannotPoisonReplacementRun(t *testing.T) {
	store, session := newContentTestStore(t, "transient-old-run")
	provider, err := NewProvider(store, ProviderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	opened := openContent(t, provider, session.ID, nil)
	defer opened.Close()
	provider.mu.Lock()
	owner := provider.owners[session.ID]
	provider.mu.Unlock()
	// A late event after the previous durable active run has already cleared
	// must not create a poisoned desynced run state which would reject the next
	// admission.
	owner.handleRunEvent(runEventInput{runID: "run-old", sessionID: session.ID, event: execution.NewSessionStreamEvent("text.delta", map[string]any{
		"turn_id": "turn-old", "agent_iteration": 1, "item_id": "old-item", "text": "late",
	})})
	owner.mu.Lock()
	if owner.transientRun != nil {
		owner.mu.Unlock()
		t.Fatal("late event without an active run created transient state")
	}
	owner.mu.Unlock()
	owner.mu.Lock()
	owner.projection.snapshot.ActiveRun = &ActiveRunDescriptor{RunID: "run-new", SessionID: session.ID, Status: "running", Recoverable: true}
	owner.mu.Unlock()

	owner.handleRunEvent(runEventInput{runID: "run-old", sessionID: session.ID, event: execution.NewSessionStreamEvent("text.delta", map[string]any{
		"turn_id": "turn-old", "agent_iteration": 1, "item_id": "old-item", "text": "late",
	})})
	owner.mu.Lock()
	if owner.transientRun != nil {
		owner.mu.Unlock()
		t.Fatal("late old event created transient state for the replacement run")
	}
	owner.mu.Unlock()

	owner.handleRunAdmitted(runAdmission{runID: "run-new", sessionID: session.ID})
	event := readTransientEvent(t, opened, protocol.SubscriptionEventRunStarted, "1")
	if event.RunID != "run-new" {
		t.Fatalf("replacement run event identity = %q, want run-new", event.RunID)
	}
}

func TestDesyncedRunWithoutDurableActiveClearsOnReopen(t *testing.T) {
	store, session := newContentTestStore(t, "transient-desynced-clear")
	provider, err := NewProvider(store, ProviderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	opened := openContent(t, provider, session.ID, nil)
	defer opened.Close()
	provider.mu.Lock()
	owner := provider.owners[session.ID]
	provider.mu.Unlock()
	if owner == nil {
		t.Fatal("owner was not retained")
	}
	// Simulate a run that was admitted and then desynced while the durable
	// active-run descriptor is still present (the normal poisoned state).
	owner.handleRunAdmitted(runAdmission{runID: "run-desynced", sessionID: session.ID})
	readTransientEvent(t, opened, protocol.SubscriptionEventRunStarted, "1")
	owner.mu.Lock()
	owner.transientRun.desynced = true
	owner.mu.Unlock()
	owner.desyncTransient(ErrProviderInvalid)

	// Simulate the durable active-run descriptor being cleared while the owner
	// stays initialized (no rebuild). The retained desynced run must not keep a
	// later open permanently recovery-required.
	owner.mu.Lock()
	owner.projection.snapshot.ActiveRun = nil
	owner.mu.Unlock()

	recovered, err := provider.OpenWithRunResume(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: session.ID}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.TransientResync != "" {
		recovered.Close()
		t.Fatalf("reopen with cleared durable active run = %q, want no active-run recovery", recovered.TransientResync)
	}
	recovered.Close()
}

func TestNoSubscriberRunGapIsCoalescedAndReopenRequiresRecovery(t *testing.T) {
	runner := &noSubscriberPressureRunner{started: make(chan struct{}), release: make(chan struct{})}
	service, err := execution.NewServiceWithOptions(t.TempDir(), execution.ServiceOptions{TurnRunner: runner})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(t.TempDir(), "gap pressure")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{DisplayName: "gap pressure", CreatedCWD: project.Project.Root, ConfigPath: service.ConfigPath(), Provider: "fake", ModelProfile: "default", ModelID: "default"})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewProvider(service.SessionStore(), ProviderOptions{TransientQueueCapacity: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	registration := service.SessionStore().RegisterMutationSink(provider)
	if registration == nil {
		t.Fatal("register provider mutation sink")
	}
	defer registration.Unregister()
	initial := openContent(t, provider, session.ID, nil)
	initial.Close()

	coordinator := execution.NewSessionRunCoordinator(context.Background(), service, execution.SessionRunCoordinatorOptions{NewRunID: func() (string, error) { return "run-gap-pressure", nil }})
	run, err := coordinator.Start(session.ID, execution.SessionMessageInput{Content: "pressure"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("pressure runner did not start")
	}

	start := time.Now()
	for index := 0; index < 10000; index++ {
		provider.RunEvent(run, execution.NewSessionStreamEvent("text.delta", map[string]any{
			"turn_id": "turn-pressure", "agent_iteration": 1, "item_id": "item-pressure", "text": "x",
		}))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("no-subscriber producer path took %v for 10000 events", elapsed)
	}
	provider.mu.Lock()
	owner := provider.owners[session.ID]
	provider.mu.Unlock()
	if owner == nil {
		t.Fatal("provider owner was not retained")
	}
	owner.gapMu.Lock()
	gaps := len(owner.runGapPending)
	owner.gapMu.Unlock()
	if gaps > 1 {
		t.Fatalf("coalesced no-subscriber gap markers = %d, want at most one", gaps)
	}
	owner.mu.Lock()
	invalid := owner.invalid
	owner.mu.Unlock()
	if invalid {
		t.Fatal("coalesced no-subscriber gaps invalidated the owner")
	}

	recovery, err := provider.OpenWithRunResume(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: session.ID}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.TransientResync != "active_run_recovery_required" {
		recovery.Close()
		t.Fatalf("reopen transient recovery = %q, want active_run_recovery_required", recovery.TransientResync)
	}
	recovery.Close()
	close(runner.release)
	if _, err := run.Wait(); err != nil {
		t.Fatalf("run.Wait() error = %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		owner.gapMu.Lock()
		gaps := len(owner.runGapPending)
		owner.gapMu.Unlock()
		if gaps == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	owner.gapMu.Lock()
	gaps = len(owner.runGapPending)
	owner.gapMu.Unlock()
	if gaps != 0 {
		t.Fatalf("settled run left %d coalesced gap markers behind", gaps)
	}
	// The run settled and the durable active-run descriptor was cleared. A
	// reopen must no longer be permanently recovery-required: the desynced
	// in-memory run must not block the next open once the durable active run
	// is gone.
	serviceDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(serviceDeadline) {
		state, stateErr := service.SessionStore().LoadState(session.ID)
		if stateErr == nil && state.RunningRunID == "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	state, stateErr := service.SessionStore().LoadState(session.ID)
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	if state.RunningRunID != "" {
		t.Fatalf("settled run left durable RunningRunID = %q", state.RunningRunID)
	}
	recovered, err := provider.OpenWithRunResume(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: session.ID}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.TransientResync != "" {
		recovered.Close()
		t.Fatalf("reopen after durable clear = %q, want no active-run recovery", recovered.TransientResync)
	}
	recovered.Close()
	coordinator.Close()
}

func TestTransientOwnerQueueOverflowIsNonBlockingAndSignalsResourceRecovery(t *testing.T) {
	store, session := newContentTestStore(t, "transient-owner-overflow")
	provider, err := NewProvider(store, ProviderOptions{TransientQueueCapacity: 1, TransientQueueBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	opened := openContent(t, provider, session.ID, nil)
	defer opened.Close()

	provider.mu.Lock()
	owner := provider.owners[session.ID]
	provider.mu.Unlock()
	if owner == nil {
		t.Fatal("provider owner was not retained")
	}
	owner.handleRunAdmitted(runAdmission{runID: "run-owner-overflow", sessionID: session.ID})
	readTransientEvent(t, opened, protocol.SubscriptionEventRunStarted, "1")

	// Hold the owner projection lock so the worker cannot consume the first
	// task. The second producer submission fills the bounded queue and must
	// return without trying to invalidate synchronously under owner.mu.
	owner.mu.Lock()
	started := time.Now()
	var overflowErr error
	for index := 0; index < 8; index++ {
		overflowErr = owner.enqueue(ownerTask{runEvent: &runEventInput{
			runID: "run-owner-overflow", sessionID: session.ID,
			event: execution.NewSessionStreamEvent("text.delta", map[string]any{
				"turn_id": "turn-1", "agent_iteration": 1, "item_id": "item-1", "text": "queued",
			}),
		}, bytes: 256})
		if overflowErr == ErrQueueFull {
			break
		}
	}
	if overflowErr != ErrQueueFull {
		owner.mu.Unlock()
		t.Fatalf("overflow enqueue error = %v, want ErrQueueFull", overflowErr)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		owner.mu.Unlock()
		t.Fatalf("overflow producer path blocked for %s while owner.mu was held", elapsed)
	}
	owner.mu.Unlock()

	select {
	case terminal := <-opened.Transient.Terminal:
		if terminal.Reason != ErrQueueFull {
			t.Fatalf("overflow terminal = %v, want ErrQueueFull", terminal.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owner queue overflow did not signal transient recovery")
	}
}
