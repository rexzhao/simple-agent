package execution

import (
	"testing"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestSessionMetadataIdempotentMutationsDoNotPublish(t *testing.T) {
	service, _, session := newExecutionServiceWithSession(t, t.TempDir(), nil)
	initialized, err := service.RenameSession(session.ID, "stable")
	if err != nil {
		t.Fatalf("initialize display name: %v", err)
	}
	session = initialized
	// CreateSession happened before registration, so only mutations below are
	// observable. The sink is stronger than LastSeq here: metadata writes can
	// update metadata bookkeeping without advancing the session lifecycle seq.
	sink := &fanoutRecordingSink{}
	registration := service.RegisterSessionIndexChangeSink(sink)
	defer registration.Unregister()

	if _, err := service.RenameSession(session.ID, session.DisplayName); err != nil {
		t.Fatalf("RenameSession(same name) error = %v", err)
	}
	if got := sink.count(); got != 0 {
		t.Fatalf("same-name rename published %d changes, want 0", got)
	}
	if _, err := service.RenameSession(session.ID, "changed"); err != nil {
		t.Fatalf("RenameSession(changed) error = %v", err)
	}
	if got := sink.count(); got != 1 {
		t.Fatalf("changed rename published %d changes, want 1", got)
	}
	if _, err := service.RenameSession(session.ID, "changed"); err != nil {
		t.Fatalf("RenameSession(repeated changed) error = %v", err)
	}
	if got := sink.count(); got != 1 {
		t.Fatalf("repeated rename published %d changes, want 1", got)
	}

	if _, err := service.SetSessionFullAccess(session.ID, false); err != nil {
		t.Fatalf("SetSessionFullAccess(default) error = %v", err)
	}
	if got := sink.count(); got != 1 {
		t.Fatalf("same full-access value published %d changes, want 1", got)
	}
	if _, err := service.SetSessionFullAccess(session.ID, true); err != nil {
		t.Fatalf("SetSessionFullAccess(true) error = %v", err)
	}
	if got := sink.count(); got != 2 {
		t.Fatalf("changed full-access value published %d changes, want 2", got)
	}
	if _, err := service.SetSessionFullAccess(session.ID, true); err != nil {
		t.Fatalf("SetSessionFullAccess(repeated true) error = %v", err)
	}
	if got := sink.count(); got != 2 {
		t.Fatalf("repeated full-access value published %d changes, want 2", got)
	}

	debug := sessions.DebugSettings{RequestBodies: true}
	if _, err := service.SetSessionDebug(session.ID, debug); err != nil {
		t.Fatalf("SetSessionDebug(true) error = %v", err)
	}
	if got := sink.count(); got != 3 {
		t.Fatalf("changed debug value published %d changes, want 3", got)
	}
	if _, err := service.SetSessionDebug(session.ID, debug); err != nil {
		t.Fatalf("SetSessionDebug(repeated true) error = %v", err)
	}
	if got := sink.count(); got != 3 {
		t.Fatalf("repeated debug value published %d changes, want 3", got)
	}
}
