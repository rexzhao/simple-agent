package sessioncontent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func validSchemaSnapshot() Snapshot {
	now := time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC)
	metadata := SessionMetadata{
		ID: "session-schema", Version: 2, CreatedAt: now, UpdatedAt: now, LastUsedAt: now,
		Provider: "codex", ModelProfile: "default", ModelID: "codex/model", Status: SessionStatusIdle,
		Debug: sessions.DebugSettings{}, Context: contextwindow.Metadata{ContextWindowSource: "unknown"},
	}
	return Snapshot{
		SchemaVersion: SchemaVersion,
		Session:       metadata,
		History:       HistoryWindow{Items: []Item{}, Descriptor: HistoryWindowDescriptor{Limit: 50, VisibleOnly: true}},
		Compaction:    CompactionState{Checkpoints: []CompactionCheckpoint{}},
	}
}

func TestSnapshotStrictInvalidTable(t *testing.T) {
	valid := validSchemaSnapshot()
	now := valid.Session.CreatedAt
	activeMetadata := valid.Session
	activeMetadata.Status = SessionStatusRunning
	activeMetadata.CurrentRunID = "run-1"
	activeMetadata.RunningRunID = "run-1"
	activeMetadata.RunningTurnID = "turn-1"
	active := &ActiveRunDescriptor{RunID: "run-1", SessionID: activeMetadata.ID, TurnID: "turn-1", StartedAt: now, Status: "running", Recoverable: true}

	tests := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{"schema version", func(s *Snapshot) { s.SchemaVersion = 2 }},
		{"metadata status", func(s *Snapshot) { s.Session.Status = "working" }},
		{"metadata relation", func(s *Snapshot) { s.Session.RunningRunID = "run-1"; s.Session.CurrentRunID = "other-run" }},
		{"active run id", func(s *Snapshot) {
			s.Session = activeMetadata
			s.ActiveRun = &ActiveRunDescriptor{SessionID: activeMetadata.ID, TurnID: "turn-1", StartedAt: now, Status: "running", Recoverable: true}
		}},
		{"active run status", func(s *Snapshot) { s.Session = activeMetadata; a := *active; a.Status = "committed"; s.ActiveRun = &a }},
		{"active run time", func(s *Snapshot) {
			s.Session = activeMetadata
			a := *active
			a.StartedAt = time.Time{}
			s.ActiveRun = &a
		}},
		{"active run recoverability", func(s *Snapshot) { s.Session = activeMetadata; a := *active; a.Recoverable = false; s.ActiveRun = &a }},
		{"active run session relation", func(s *Snapshot) {
			s.Session = activeMetadata
			a := *active
			a.SessionID = "other-session"
			s.ActiveRun = &a
		}},
		{"item kind", func(s *Snapshot) {
			s.History.Items = []Item{validItem()}
			s.History.Items[0].Kind = "unknown"
			s.History.Descriptor.OldestItemSeq = "1"
			s.History.Descriptor.NewestItemSeq = "1"
		}},
		{"item visibility", func(s *Snapshot) {
			s.History.Items = []Item{validItem()}
			s.History.Items[0].Visibility = "private"
			s.History.Descriptor.OldestItemSeq = "1"
			s.History.Descriptor.NewestItemSeq = "1"
		}},
		{"message role", func(s *Snapshot) {
			s.History.Items = []Item{validItem()}
			s.History.Items[0].Message = &ItemMessage{Role: "unknown"}
			s.History.Descriptor.OldestItemSeq = "1"
			s.History.Descriptor.NewestItemSeq = "1"
		}},
		{"negative cursor", func(s *Snapshot) { s.History.Descriptor.BeforeItemSeq = "-1" }},
		{"noncanonical cursor", func(s *Snapshot) { s.History.Descriptor.BeforeItemSeq = "01" }},
		{"latest has newer", func(s *Snapshot) { s.History.Descriptor.HasMoreAfter = true }},
		{"history bound mismatch", func(s *Snapshot) {
			s.History.Items = []Item{validItem()}
			s.History.Descriptor.OldestItemSeq = "2"
			s.History.Descriptor.NewestItemSeq = "1"
		}},
		{"compaction checkpoint id", func(s *Snapshot) {
			s.Compaction.Checkpoints = []CompactionCheckpoint{validCheckpoint(now)}
			s.Compaction.Checkpoints[0].ID = ""
		}},
		{"compaction checkpoint time", func(s *Snapshot) {
			s.Compaction.Checkpoints = []CompactionCheckpoint{validCheckpoint(now)}
			s.Compaction.Checkpoints[0].CreatedAt = time.Time{}
		}},
		{"compaction replacement", func(s *Snapshot) {
			s.Compaction.Checkpoints = []CompactionCheckpoint{validCheckpoint(now)}
			s.Compaction.Checkpoints[0].ReplacementHistory = nil
		}},
		{"session id whitespace", func(s *Snapshot) { s.Session.ID = " session-schema" }},
		{"session id control", func(s *Snapshot) { s.Session.ID = "session\n-schema" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := valid
			test.mutate(&snapshot)
			if err := snapshot.Validate(); err == nil {
				t.Fatal("Validate() = nil, want strict schema error")
			}
		})
	}
}

func validItem() Item {
	now := time.Date(2025, time.January, 2, 3, 4, 6, 0, time.UTC)
	return Item{Key: ItemKey{TurnID: "turn-1", AgentIteration: 0, ItemID: "item-1"}, Seq: 1, CreatedAt: now, Kind: "message", Visibility: "visible", Audience: "user", Message: &ItemMessage{Role: "user"}}
}

func validCheckpoint(now time.Time) CompactionCheckpoint {
	return CompactionCheckpoint{ID: "checkpoint-1", CreatedAt: now, Reason: "test", Phase: "completed", Trigger: "manual", SummaryItemID: "summary-1", ReplacementHistory: []string{"summary-1"}}
}

func TestSessionContentSchemaOptionalityAndCanonicalUTF8(t *testing.T) {
	snapshot := validSchemaSnapshot()
	raw, err := json.Marshal(snapshot.Session)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "archived_at") || strings.Contains(string(raw), "interrupted_at") {
		t.Fatalf("zero optional times were emitted: %s", raw)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	content := ItemContent{Inline: "界"}
	if err := content.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ItemContent{Inline: "\xff"}).Validate(); err == nil {
		t.Fatal("invalid UTF-8 content was accepted")
	}
	if err := (ItemContent{Inline: "full", Preview: "preview"}).Validate(); err == nil {
		t.Fatal("ambiguous inline+preview content was accepted")
	}
}

func TestSnapshotJSONRequiredFieldTable(t *testing.T) {
	raw, err := json.Marshal(validSchemaSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"schema_version", "session", "history", "active_run", "compaction"} {
		t.Run("missing "+field, func(t *testing.T) {
			copyObject := make(map[string]json.RawMessage, len(object))
			for key, value := range object {
				copyObject[key] = value
			}
			delete(copyObject, field)
			encoded, marshalErr := json.Marshal(copyObject)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			var decoded Snapshot
			if err := json.Unmarshal(encoded, &decoded); err == nil {
				t.Fatalf("missing required %s was accepted", field)
			}
		})
	}
}
