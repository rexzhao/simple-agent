package execution

import (
	"errors"
	"testing"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestArchivedMetadataRetriesNoOpBeforeArchivedRejection(t *testing.T) {
	service, _, session := newExecutionServiceWithSession(t, t.TempDir(), nil)
	initialized, err := service.RenameSession(session.ID, "stable")
	if err != nil {
		t.Fatal(err)
	}
	session = initialized
	if _, err := service.SetSessionFullAccess(session.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetSessionDebug(session.ID, sessions.DebugSettings{RequestBodies: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ArchiveSession(session.ID); err != nil {
		t.Fatal(err)
	}

	selfSink := &fanoutRecordingSink{}
	selfRegistration := service.RegisterSessionIndexChangeSink(selfSink)
	defer selfRegistration.Unregister()
	assertArchivedMetadataRetry(t, service, session.ID, "stable", true, true)
	if got := selfSink.count(); got != 0 {
		t.Fatalf("self-archived retries published %d changes, want 0", got)
	}

	parent, err := service.CreateSession(session.ProjectID, SessionCreateMetadata{
		DisplayName: "Parent", CreatedCWD: session.CreatedCWD, Provider: "fake", ModelProfile: "default", ModelID: "model",
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := service.CreateInheritedSession(parent.ID, "Child")
	if err != nil {
		t.Fatal(err)
	}
	child, err = service.RenameSession(child.ID, "child-stable")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetSessionFullAccess(child.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetSessionDebug(child.ID, sessions.DebugSettings{RequestBodies: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ArchiveSession(parent.ID); err != nil {
		t.Fatal(err)
	}

	ancestorSink := &fanoutRecordingSink{}
	ancestorRegistration := service.RegisterSessionIndexChangeSink(ancestorSink)
	defer ancestorRegistration.Unregister()
	assertArchivedMetadataRetry(t, service, child.ID, child.DisplayName, true, true)
	if got := ancestorSink.count(); got != 0 {
		t.Fatalf("ancestor-archived retries published %d changes, want 0", got)
	}
}

func assertArchivedMetadataRetry(t *testing.T, service *Service, sessionID, displayName string, fullAccess, requestBodies bool) {
	t.Helper()
	if _, err := service.RenameSession(sessionID, displayName); err != nil {
		t.Fatalf("same-name archived rename error = %v", err)
	}
	if _, err := service.SetSessionFullAccess(sessionID, fullAccess); err != nil {
		t.Fatalf("same-value archived full-access error = %v", err)
	}
	if _, err := service.SetSessionDebug(sessionID, sessions.DebugSettings{RequestBodies: requestBodies}); err != nil {
		t.Fatalf("same-value archived debug error = %v", err)
	}

	if _, err := service.RenameSession(sessionID, displayName+"-different"); !errors.Is(err, ErrSessionArchived) {
		t.Fatalf("different archived rename error = %v, want ErrSessionArchived", err)
	}
	if _, err := service.SetSessionFullAccess(sessionID, !fullAccess); !errors.Is(err, ErrSessionArchived) {
		t.Fatalf("different archived full-access error = %v, want ErrSessionArchived", err)
	}
	if _, err := service.SetSessionDebug(sessionID, sessions.DebugSettings{RequestBodies: !requestBodies}); !errors.Is(err, ErrSessionArchived) {
		t.Fatalf("different archived debug error = %v, want ErrSessionArchived", err)
	}
}
