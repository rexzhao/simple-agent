package webapp

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessionindex"
)

func TestSessionIndexWebSocketArchiveRestoreRenameAndCascadeDelete(t *testing.T) {
	server, service, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
	project, err := service.CreateProject(t.TempDir(), "Cascade Project")
	if err != nil {
		t.Fatal(err)
	}
	makeSession := func(name, parentID string) execution.SessionDetail {
		t.Helper()
		detail, createErr := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{
			DisplayName: name, ParentSessionID: parentID, CreatedCWD: project.Project.Root,
			Provider: "fake", ModelProfile: "default", ModelID: "model",
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return detail
	}
	parent := makeSession("Parent", "")
	child := makeSession("Child", parent.ID)
	connection := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), "http://"+strings.TrimPrefix(server.URL, "http://"))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, connection)
	_ = readWebAppMessage(t, connection)
	writeIndexSubscribe(t, connection, project.Project.ID, nil)
	_ = readWebAppMessage(t, connection)
	snapshotMessage, ok := readWebAppMessage(t, connection).(protocol.SnapshotMessage)
	if !ok {
		t.Fatal("session index snapshot missing")
	}
	var snapshot sessionindex.SessionIndexSnapshot
	if err := json.Unmarshal(snapshotMessage.Payload.Content.Inline, &snapshot); err != nil {
		t.Fatal(err)
	}
	lastSequence, err := strconv.ParseUint(string(snapshotMessage.Payload.Sequence), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	lastRevision, err := strconv.ParseUint(string(snapshotMessage.Payload.ResourceRevision), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	readChange := func() protocol.ChangeMessage {
		t.Helper()
		change := readIndexChange(t, connection)
		sequence, parseErr := strconv.ParseUint(string(change.Payload.Sequence), 10, 64)
		if parseErr != nil || sequence != lastSequence+1 {
			t.Fatalf("sequence=%s previous=%d err=%v", change.Payload.Sequence, lastSequence, parseErr)
		}
		revision, parseErr := strconv.ParseUint(string(change.Payload.ResourceRevision), 10, 64)
		if parseErr != nil || revision != lastRevision+1 {
			t.Fatalf("resource revision=%s previous=%d err=%v", change.Payload.ResourceRevision, lastRevision, parseErr)
		}
		lastSequence, lastRevision = sequence, revision
		return change
	}
	readUpsert := func() sessionindex.SessionSummary {
		return decodeIndexSummary(t, readChange())
	}
	if _, err := service.ArchiveSession(parent.ID); err != nil {
		t.Fatal(err)
	}
	archived := map[string]bool{}
	for i := 0; i < 2; i++ {
		summary := readUpsert()
		archived[summary.SessionID] = summary.Archived
	}
	if !archived[parent.ID] || !archived[child.ID] {
		t.Fatalf("archive projection=%v", archived)
	}
	if _, err := service.RestoreSession(parent.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if summary := readUpsert(); summary.Archived {
			t.Fatalf("restore summary=%#v", summary)
		}
	}
	if _, err := service.RenameSession(child.ID, "Child renamed"); err != nil {
		t.Fatal(err)
	}
	if summary := readUpsert(); summary.SessionID != child.ID || summary.DisplayName != "Child renamed" {
		t.Fatalf("rename summary=%#v", summary)
	}
	if _, err := service.ArchiveSession(parent.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if summary := readUpsert(); !summary.Archived {
			t.Fatalf("archive before delete summary=%#v", summary)
		}
	}
	if _, err := service.RemoveSession(parent.ID); err != nil {
		t.Fatal(err)
	}
	removed := map[string]bool{}
	for i := 0; i < 2; i++ {
		change := readChange()
		if len(change.Payload.Operations) != 1 || change.Payload.Operations[0].Op != sessionindex.OperationRemove {
			t.Fatalf("delete operation=%#v", change.Payload.Operations)
		}
		var operation struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(change.Payload.Operations[0].Raw, &operation); err != nil {
			t.Fatal(err)
		}
		removed[operation.Key] = true
	}
	if !removed[parent.ID] || !removed[child.ID] {
		t.Fatalf("cascade delete projection=%v", removed)
	}
}
