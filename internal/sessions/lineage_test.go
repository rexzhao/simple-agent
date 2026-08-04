package sessions

import (
	"strings"
	"testing"
)

func TestV2StorePersistsImmutableSessionLineage(t *testing.T) {
	store := NewV2Store(t.TempDir())
	root, err := store.SaveMetadata(SessionV2{ID: "session-root", DisplayName: "Root"})
	if err != nil {
		t.Fatalf("SaveMetadata(root) error = %v", err)
	}
	if root.CreatedBy != SessionCreatedByUser || root.ParentSessionID != "" || root.RootSessionID != root.ID || root.SpawnDepth != 0 {
		t.Fatalf("root lineage = %#v, want user root", root)
	}

	child, err := store.SaveMetadata(SessionV2{
		ID:              "session-child",
		CreatedBy:       SessionCreatedByAgent,
		ParentSessionID: root.ID,
		RootSessionID:   root.ID,
		SpawnDepth:      1,
	})
	if err != nil {
		t.Fatalf("SaveMetadata(child) error = %v", err)
	}
	loaded, err := store.LoadExecutionState(child.ID)
	if err != nil {
		t.Fatalf("Load(child) error = %v", err)
	}
	if loaded.CreatedBy != SessionCreatedByAgent || loaded.ParentSessionID != root.ID || loaded.RootSessionID != root.ID || loaded.SpawnDepth != 1 {
		t.Fatalf("loaded child lineage = %#v, want agent child of root", loaded)
	}
	states, err := store.ListStates(V2ListOptions{All: true})
	if err != nil {
		t.Fatalf("ListStates() error = %v", err)
	}
	var found bool
	for _, state := range states {
		if state.ID == child.ID {
			found = true
			if state.CreatedBy != SessionCreatedByAgent || state.ParentSessionID != root.ID || state.RootSessionID != root.ID || state.SpawnDepth != 1 {
				t.Fatalf("child state lineage = %#v", state)
			}
		}
	}
	if !found {
		t.Fatal("child missing from session list")
	}

	loaded.ParentSessionID = "different-parent"
	if _, err := store.SaveMetadata(loaded); err == nil || !strings.Contains(err.Error(), "lineage is immutable") {
		t.Fatalf("SaveMetadata(reparent) error = %v, want immutable lineage error", err)
	}
}

func TestV2StoreRejectsInvalidSessionLineage(t *testing.T) {
	store := NewV2Store(t.TempDir())
	tests := []struct {
		name    string
		session SessionV2
	}{
		{name: "unknown creator", session: SessionV2{ID: "unknown", CreatedBy: "external"}},
		{name: "self parent", session: SessionV2{ID: "self", ParentSessionID: "self"}},
		{name: "negative depth", session: SessionV2{ID: "negative", SpawnDepth: -1}},
		{name: "invalid parent id", session: SessionV2{ID: "bad-parent", ParentSessionID: "not/a/session"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.SaveMetadata(test.session); err == nil {
				t.Fatalf("SaveMetadata(%#v) error = nil, want lineage validation error", test.session)
			}
		})
	}
}
