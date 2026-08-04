package execution

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestServicePublishesSessionLifecycleAfterSuccessfulStoreWrites(t *testing.T) {
	service, project, parent := newExecutionServiceWithSession(t, t.TempDir(), nil)
	subscription := service.LifecycleHub().Subscribe()
	defer subscription.Close()

	child, err := service.CreateInheritedSession(parent.ID, "agent child")
	if err != nil {
		t.Fatalf("CreateInheritedSession() error = %v", err)
	}
	childCreated := nextLifecycleEvent(t, subscription)
	assertLifecycleEvent(t, childCreated, LifecycleSessionCreated)
	assertSessionMetadataPayload(t, childCreated, child.ID, project.Project.ID)
	var childPayload map[string]any
	decodeLifecyclePayload(t, childCreated, &childPayload)
	childMetadata := childPayload["session"].(map[string]any)
	if childMetadata["created_by"] != sessions.SessionCreatedByAgent || childMetadata["parent_session_id"] != parent.ID {
		t.Fatalf("child session metadata = %#v, want agent child lineage", childMetadata)
	}

	if _, err := service.RenameSession(parent.ID, "renamed"); err != nil {
		t.Fatalf("RenameSession() error = %v", err)
	}
	assertLifecycleEvent(t, nextLifecycleEvent(t, subscription), LifecycleSessionUpdated)
	if _, err := service.SetSessionFullAccess(parent.ID, true); err != nil {
		t.Fatalf("SetSessionFullAccess() error = %v", err)
	}
	assertLifecycleEvent(t, nextLifecycleEvent(t, subscription), LifecycleSessionUpdated)
	if _, err := service.SetSessionDebug(parent.ID, sessions.DebugSettings{RequestBodies: true}); err != nil {
		t.Fatalf("SetSessionDebug() error = %v", err)
	}
	assertLifecycleEvent(t, nextLifecycleEvent(t, subscription), LifecycleSessionUpdated)

	if _, err := service.ArchiveSession(parent.ID); err != nil {
		t.Fatalf("ArchiveSession() error = %v", err)
	}
	for range 2 {
		event := nextLifecycleEvent(t, subscription)
		assertLifecycleEvent(t, event, LifecycleSessionArchived)
		var payload map[string]any
		decodeLifecyclePayload(t, event, &payload)
		if got := payload["cascade_root_id"]; got != parent.ID {
			t.Fatalf("archive cascade_root_id = %#v, want %q", got, parent.ID)
		}
	}
	if _, err := service.RestoreSession(parent.ID); err != nil {
		t.Fatalf("RestoreSession() error = %v", err)
	}
	restored := nextLifecycleEvent(t, subscription)
	assertLifecycleEvent(t, restored, LifecycleSessionUpdated)
	var restoredPayload map[string]any
	decodeLifecyclePayload(t, restored, &restoredPayload)
	if restoredPayload["reason"] != "restore" {
		t.Fatalf("restore event payload = %#v, want reason restore", restoredPayload)
	}

	// Removal requires an archived root. Re-archive it and consume the
	// cascade events before checking that deletion is represented by one
	// compact root-plus-descendants event.
	if _, err := service.ArchiveSession(parent.ID); err != nil {
		t.Fatalf("ArchiveSession(before remove) error = %v", err)
	}
	assertLifecycleEvent(t, nextLifecycleEvent(t, subscription), LifecycleSessionArchived)
	if _, err := service.RemoveSession(parent.ID); err != nil {
		t.Fatalf("RemoveSession() error = %v", err)
	}
	deleted := nextLifecycleEvent(t, subscription)
	assertLifecycleEvent(t, deleted, LifecycleSessionDeleted)
	var deletedPayload map[string]any
	decodeLifecyclePayload(t, deleted, &deletedPayload)
	if deletedPayload["session"] != parent.ID || deletedPayload["project"] != project.Project.ID {
		t.Fatalf("delete payload = %#v, want root session/project", deletedPayload)
	}
	if descendants, ok := deletedPayload["descendants"].([]any); !ok || len(descendants) != 1 || descendants[0] != child.ID {
		t.Fatalf("delete descendants = %#v, want child %q", deletedPayload["descendants"], child.ID)
	}
}

func TestServiceDoesNotPublishFailedCascade(t *testing.T) {
	service, project, parent := newExecutionServiceWithSession(t, t.TempDir(), nil)
	subscription := service.LifecycleHub().Subscribe()
	defer subscription.Close()

	child, err := service.CreateInheritedSession(parent.ID, "busy child")
	if err != nil {
		t.Fatalf("CreateInheritedSession() error = %v", err)
	}
	assertLifecycleEvent(t, nextLifecycleEvent(t, subscription), LifecycleSessionCreated)
	if _, err := service.sessionStore.MarkTurnRunning(child.ID, "turn-busy"); err != nil {
		t.Fatalf("MarkTurnRunning() error = %v", err)
	}
	if _, err := service.ArchiveSession(parent.ID); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("ArchiveSession() error = %v, want ErrSessionBusy", err)
	}
	select {
	case event := <-subscription.Events():
		t.Fatalf("failed archive published event %#v", event)
	case <-time.After(100 * time.Millisecond):
	}

	state, err := service.sessionStore.LoadState(parent.ID)
	if err != nil {
		t.Fatalf("LoadState(parent) error = %v", err)
	}
	if state.Archived {
		t.Fatalf("failed archive changed parent state = %#v", state)
	}
	_ = project // retain the fixture's project creation in this focused test.
}

func TestSessionRunCoordinatorPublishesRunLifecycleForSharedStarts(t *testing.T) {
	service, project, session := newExecutionServiceWithSession(t, t.TempDir(), nil)
	_ = project
	starter := newCoordinatorTestStarter()
	coordinator := NewSessionRunCoordinator(nil, starter, SessionRunCoordinatorOptions{
		NewRunID: func() (string, error) { return "run-lifecycle", nil },
	})
	service.SetSessionRunCoordinator(coordinator)
	defer func() {
		service.ClearSessionRunCoordinator(coordinator)
		coordinator.Close()
	}()
	subscription := service.LifecycleHub().Subscribe()
	defer subscription.Close()
	// The fixture's creation event was published before the subscription.

	run, err := coordinator.Start(session.ID, SessionMessageInput{Content: "agent tool start"}, nil)
	if err != nil {
		t.Fatalf("coordinator.Start() error = %v", err)
	}
	started := nextLifecycleEvent(t, subscription)
	assertLifecycleEvent(t, started, LifecycleRunStarted)
	var startedPayload map[string]any
	decodeLifecyclePayload(t, started, &startedPayload)
	if startedPayload["run"] != run.ID() || startedPayload["session"] != session.ID || startedPayload["status"] != string(SessionRunRunning) {
		t.Fatalf("run.started payload = %#v", startedPayload)
	}

	starter.complete(session.ID)
	if _, err := run.Wait(); err != nil {
		t.Fatalf("run.Wait() error = %v", err)
	}
	settled := nextLifecycleEvent(t, subscription)
	assertLifecycleEvent(t, settled, LifecycleRunSettled)
	var settledPayload map[string]any
	decodeLifecyclePayload(t, settled, &settledPayload)
	if settledPayload["run"] != run.ID() || settledPayload["session"] != session.ID || settledPayload["status"] != string(SessionRunCommitted) {
		t.Fatalf("run.settled payload = %#v", settledPayload)
	}
	if _, ok := settledPayload["metadata"].(map[string]any); !ok {
		t.Fatalf("run.settled metadata = %#v, want SessionMetadata", settledPayload["metadata"])
	}
	if got, ok := settledPayload["committed_revision"].(string); !ok || got == "" {
		t.Fatalf("run.settled committed_revision = %#v, want decimal string", settledPayload["committed_revision"])
	}
	state, err := service.sessionStore.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(after settled) error = %v", err)
	}
	if got := settledPayload["committed_revision"]; got != strconv.FormatInt(state.LastSeq, 10) {
		t.Fatalf("run.settled committed_revision = %#v, want %q", got, strconv.FormatInt(state.LastSeq, 10))
	}
}

func TestLifecycleHubDropsSlowSubscribersWithoutBlocking(t *testing.T) {
	hub := NewLifecycleHubWithOptions(LifecycleHubOptions{SubscriberBuffer: 1})
	subscription := hub.Subscribe()
	defer subscription.Close()

	started := time.Now()
	hub.Publish(NewLifecycleEvent("session.updated", map[string]any{"value": 1}))
	hub.Publish(NewLifecycleEvent("session.updated", map[string]any{"value": 2}))
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("publishing to slow subscriber took %s", elapsed)
	}
	if _, open := <-subscription.Events(); !open {
		t.Fatal("slow subscriber closed without preserving its queued event")
	}
	if _, open := <-subscription.Events(); open {
		t.Fatal("slow subscriber remained registered after queue overflow")
	}
}

func nextLifecycleEvent(t *testing.T, subscription *LifecycleSubscription) LifecycleEvent {
	t.Helper()
	select {
	case event, ok := <-subscription.Events():
		if !ok {
			t.Fatal("lifecycle subscription closed unexpectedly")
		}
		return event
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for lifecycle event")
		return LifecycleEvent{}
	}
}

func assertLifecycleEvent(t *testing.T, event LifecycleEvent, wantType string) {
	t.Helper()
	if event.Type != wantType {
		t.Fatalf("lifecycle event type = %q, want %q", event.Type, wantType)
	}
	var payload map[string]any
	decodeLifecyclePayload(t, event, &payload)
	if payload["type"] != wantType {
		t.Fatalf("lifecycle payload type = %#v, want %q", payload["type"], wantType)
	}
}

func assertSessionMetadataPayload(t *testing.T, event LifecycleEvent, sessionID, projectID string) {
	t.Helper()
	var payload map[string]any
	decodeLifecyclePayload(t, event, &payload)
	metadata, ok := payload["session"].(map[string]any)
	if !ok {
		t.Fatalf("session payload = %#v, want SessionMetadata object", payload["session"])
	}
	for _, field := range []string{"id", "created_at", "updated_at", "project_id", "last_seq", "revision", "full_access", "debug"} {
		if _, ok := metadata[field]; !ok {
			t.Fatalf("session metadata missing %q: %#v", field, metadata)
		}
	}
	if metadata["id"] != sessionID || metadata["project_id"] != projectID {
		t.Fatalf("session metadata id/project = %#v/%#v", metadata["id"], metadata["project_id"])
	}
}

func decodeLifecyclePayload(t *testing.T, event LifecycleEvent, target any) {
	t.Helper()
	if err := json.Unmarshal(event.Payload, target); err != nil {
		t.Fatalf("decode lifecycle payload %q: %v; payload=%s", event.Type, err, event.Payload)
	}
}
