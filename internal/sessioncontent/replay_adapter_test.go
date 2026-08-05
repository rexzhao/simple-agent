package sessioncontent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

func TestHistoryDescriptorOperationsReplayToFreshSnapshot(t *testing.T) {
	store, session := newContentTestStore(t, "session-replay-adapter")
	for i := 1; i <= 50; i++ {
		if _, err := store.AppendItem(session.ID, sessions.SessionItemFromMessage("item-"+itoa(i), model.Message{Role: model.MessageRoleUser, Content: itoa(i)})); err != nil {
			t.Fatal(err)
		}
	}
	p, err := NewProvider(store, ProviderOptions{HistoryLimit: 50})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	registration := store.RegisterMutationSink(p)
	defer registration.Unregister()
	opened := openContent(t, p, session.ID, nil)
	defer opened.Close()
	local := decodeSnapshot(t, opened)
	appendItem := func(id string, item sessions.SessionItem) {
		t.Helper()
		if _, err := store.AppendItem(session.ID, item); err != nil {
			t.Fatal(err)
		}
		change := nextChange(t, opened)
		if err := applyTestChange(&local, change.Change); err != nil {
			t.Fatal(err)
		}
	}
	appendItem("item-51", sessions.SessionItemFromMessage("item-51", model.Message{Role: model.MessageRoleUser, Content: "51"}))
	if len(local.History.Items) != 50 || local.History.Items[0].Key.ItemID != "item-2" || local.History.Items[len(local.History.Items)-1].Key.ItemID != "item-51" {
		t.Fatalf("50->51 window replay = %#v", local.History.Items)
	}
	updated := sessions.SessionItem{ID: "item-10", Message: &model.Message{Role: model.MessageRoleUser, Content: "ten-updated"}}
	if _, err := store.UpdateItem(session.ID, updated); err != nil {
		t.Fatal(err)
	}
	change := nextChange(t, opened)
	if !hasTestOperation(change.Change.Operations, OpItemUpsert) {
		t.Fatalf("item update operations = %#v", change.Change.Operations)
	}
	if err := applyTestChange(&local, change.Change); err != nil {
		t.Fatal(err)
	}
	hidden := sessions.SessionItemFromMessage("hidden-item", model.Message{Role: model.MessageRoleUser, Content: "secret"})
	hidden.Visibility = sessions.ItemVisibilityHidden
	if _, err := store.AppendItem(session.ID, hidden); err != nil {
		t.Fatal(err)
	}
	change = nextChange(t, opened)
	if err := applyTestChange(&local, change.Change); err != nil {
		t.Fatal(err)
	}
	for _, item := range local.History.Items {
		if item.Key.ItemID == "hidden-item" {
			t.Fatal("hidden item leaked into visible history replay")
		}
	}
	summary := sessions.SessionItemFromMessage("summary-1", model.Message{Role: model.MessageRoleProvider, Content: "summary"})
	checkpoint := sessions.CompactionCheckpoint{ID: "checkpoint-1", Reason: "test", Phase: "complete", Trigger: "manual", SummaryItemID: summary.ID, ReplacementHistory: []string{summary.ID}}
	if _, err := store.AppendCompactionCheckpoint(session.ID, summary, checkpoint); err != nil {
		t.Fatal(err)
	}
	change = nextChange(t, opened)
	if !hasTestOperation(change.Change.Operations, OpCompactionReplace) || !hasTestOperation(change.Change.Operations, OpHistoryDescriptorReplace) {
		t.Fatalf("compaction operations = %#v", change.Change.Operations)
	}
	if err := applyTestChange(&local, change.Change); err != nil {
		t.Fatal(err)
	}
	freshOpened := openContent(t, p, session.ID, nil)
	defer freshOpened.Close()
	fresh := decodeSnapshot(t, freshOpened)
	if !EqualSnapshot(local, fresh) {
		t.Fatalf("replayed snapshot differs from fresh snapshot\nreplayed=%#v\nfresh=%#v", local, fresh)
	}
}

func applyTestChange(snapshot *Snapshot, change syncengine.ResourceChange) error {
	for _, operation := range change.Operations {
		switch operation.Op {
		case OpMetadataReplace:
			var payload struct {
				Metadata SessionMetadata `json:"metadata"`
			}
			if err := json.Unmarshal(operation.Raw, &payload); err != nil {
				return err
			}
			snapshot.Session = payload.Metadata
		case OpItemUpsert:
			var payload struct {
				Item Item `json:"item"`
			}
			if err := json.Unmarshal(operation.Raw, &payload); err != nil {
				return err
			}
			found := false
			for index := range snapshot.History.Items {
				if snapshot.History.Items[index].Key == payload.Item.Key {
					snapshot.History.Items[index] = payload.Item
					found = true
					break
				}
			}
			if !found {
				snapshot.History.Items = append(snapshot.History.Items, payload.Item)
			}
			sort.Slice(snapshot.History.Items, func(i, j int) bool { return snapshot.History.Items[i].Seq < snapshot.History.Items[j].Seq })
		case OpItemRemove:
			var payload struct {
				Key ItemKey `json:"key"`
			}
			if err := json.Unmarshal(operation.Raw, &payload); err != nil {
				return err
			}
			filtered := snapshot.History.Items[:0]
			for _, item := range snapshot.History.Items {
				if item.Key != payload.Key {
					filtered = append(filtered, item)
				}
			}
			snapshot.History.Items = filtered
		case OpHistoryDescriptorReplace:
			var payload struct {
				Descriptor HistoryWindowDescriptor `json:"descriptor"`
			}
			if err := json.Unmarshal(operation.Raw, &payload); err != nil {
				return err
			}
			snapshot.History.Descriptor = payload.Descriptor
		case OpActiveRunReplace:
			var payload struct {
				ActiveRun *ActiveRunDescriptor `json:"active_run"`
			}
			if err := json.Unmarshal(operation.Raw, &payload); err != nil {
				return err
			}
			snapshot.ActiveRun = payload.ActiveRun
		case OpActiveRunClear:
			snapshot.ActiveRun = nil
		case OpCompactionReplace:
			var payload struct {
				Compaction CompactionState `json:"compaction"`
			}
			if err := json.Unmarshal(operation.Raw, &payload); err != nil {
				return err
			}
			snapshot.Compaction = payload.Compaction
		default:
			return fmt.Errorf("unsupported test operation %q", operation.Op)
		}
	}
	return snapshot.Validate()
}

func hasTestOperation(operations []protocol.ChangeOperation, want string) bool {
	for _, operation := range operations {
		if operation.Op == want {
			return true
		}
	}
	return false
}

func itoa(value int) string { return strconv.Itoa(value) }
