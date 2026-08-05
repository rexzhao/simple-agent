package sessions

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

type mutationProbe struct {
	mu      sync.Mutex
	records []Mutation
	ready   chan struct{}
	once    sync.Once
	block   <-chan struct{}
	all     chan Mutation
}

func (p *mutationProbe) PublishMutation(mutation Mutation) error {
	p.once.Do(func() { close(p.ready) })
	if p.block != nil {
		<-p.block
	}
	p.mu.Lock()
	p.records = append(p.records, mutation)
	p.mu.Unlock()
	p.all <- mutation
	return nil
}

func waitMutation(t *testing.T, ch <-chan Mutation) Mutation {
	t.Helper()
	select {
	case mutation := <-ch:
		return mutation
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for post-commit mutation")
		return Mutation{}
	}
}

func testMutationSession(t *testing.T, id string) (*V2Store, SessionV2) {
	t.Helper()
	store := NewV2Store(t.TempDir())
	session, err := store.SaveMetadata(SessionV2{ID: id, ProjectID: "project", Provider: "codex", ModelProfile: "default", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	return store, session
}

func TestMutationSinkAllCommittedContentWritersNotifyOnce(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*V2Store, SessionV2) error
		write func(*V2Store, SessionV2) error
	}{
		{
			name: "metadata",
			write: func(store *V2Store, session SessionV2) error {
				session.DisplayName = "updated"
				_, err := store.SaveMetadata(session)
				return err
			},
		},
		{
			name: "append item",
			write: func(store *V2Store, session SessionV2) error {
				_, err := store.AppendItem(session.ID, SessionItemFromMessage("item-1", model.Message{Role: model.MessageRoleUser, Content: "one"}))
				return err
			},
		},
		{
			name: "update item",
			setup: func(store *V2Store, session SessionV2) error {
				_, err := store.AppendItem(session.ID, SessionItemFromMessage("item-1", model.Message{Role: model.MessageRoleUser, Content: "one"}))
				return err
			},
			write: func(store *V2Store, session SessionV2) error {
				_, err := store.UpdateItem(session.ID, SessionItem{ID: "item-1", Message: &model.Message{Role: model.MessageRoleUser, Content: "two"}})
				return err
			},
		},
		{
			name: "replace active history",
			setup: func(store *V2Store, session SessionV2) error {
				_, err := store.AppendItem(session.ID, SessionItemFromMessage("item-1", model.Message{Role: model.MessageRoleUser, Content: "one"}))
				return err
			},
			write: func(store *V2Store, session SessionV2) error {
				_, err := store.ReplaceActiveHistory(session.ID, []string{"item-1"})
				return err
			},
		},
		{
			name: "append compaction",
			write: func(store *V2Store, session SessionV2) error {
				_, err := store.AppendCompaction(session.ID, CompactionCheckpoint{ID: "checkpoint-1", CreatedAt: time.Now(), Reason: "test", Phase: "complete", Trigger: "manual", SummaryItemID: "summary-1", ReplacementHistory: []string{"summary-1"}})
				return err
			},
		},
		{
			name: "append compaction checkpoint",
			write: func(store *V2Store, session SessionV2) error {
				summary := SessionItemFromMessage("summary-1", model.Message{Role: model.MessageRoleProvider, Content: "summary"})
				checkpoint := CompactionCheckpoint{ID: "checkpoint-1", Reason: "test", Phase: "complete", Trigger: "manual", SummaryItemID: summary.ID, ReplacementHistory: []string{summary.ID}}
				_, err := store.AppendCompactionCheckpoint(session.ID, summary, checkpoint)
				return err
			},
		},
		{
			name: "append items and active history transaction",
			write: func(store *V2Store, session SessionV2) error {
				_, err := store.AppendItemsAndReplaceActiveHistory(session.ID, []SessionItem{SessionItemFromMessage("item-1", model.Message{Role: model.MessageRoleUser, Content: "one"})}, []string{"item-1"})
				return err
			},
		},
		{
			name: "create run",
			write: func(store *V2Store, session SessionV2) error {
				_, err := store.CreateRun(session.ID, "run-1", "", nil, time.Now())
				return err
			},
		},
		{
			name: "start turn",
			setup: func(store *V2Store, session SessionV2) error {
				_, err := store.CreateRun(session.ID, "run-1", "", nil, time.Now())
				return err
			},
			write: func(store *V2Store, session SessionV2) error {
				_, err := store.StartTurn(session.ID, "run-1", "turn-1", 0, time.Now())
				return err
			},
		},
		{
			name: "settle turn",
			setup: func(store *V2Store, session SessionV2) error {
				if _, err := store.CreateRun(session.ID, "run-1", "", nil, time.Now()); err != nil {
					return err
				}
				_, err := store.StartTurn(session.ID, "run-1", "turn-1", 0, time.Now())
				return err
			},
			write: func(store *V2Store, session SessionV2) error {
				_, err := store.SetTurnStatus(session.ID, "run-1", "turn-1", TurnStatusCommitted, time.Now())
				return err
			},
		},
		{
			name: "settle run",
			setup: func(store *V2Store, session SessionV2) error {
				_, err := store.CreateRun(session.ID, "run-1", "", nil, time.Now())
				return err
			},
			write: func(store *V2Store, session SessionV2) error {
				_, err := store.SetRunStatus(session.ID, "run-1", RunStatusCancelled, time.Now())
				return err
			},
		},
		{
			name: "mark read",
			setup: func(store *V2Store, session SessionV2) error {
				if _, err := store.CreateRun(session.ID, "run-1", "", nil, time.Now()); err != nil {
					return err
				}
				_, err := store.SetRunStatus(session.ID, "run-1", RunStatusCommitted, time.Now())
				return err
			},
			write: func(store *V2Store, session SessionV2) error {
				_, _, err := store.MarkRead(session.ID, "run-1")
				return err
			},
		},
		{
			name: "clear running turn",
			setup: func(store *V2Store, session SessionV2) error {
				if _, err := store.CreateRun(session.ID, "run-1", "", nil, time.Now()); err != nil {
					return err
				}
				_, err := store.StartTurn(session.ID, "run-1", "turn-1", 0, time.Now())
				return err
			},
			write: func(store *V2Store, session SessionV2) error {
				_, err := store.ClearRunningTurn(session.ID, "turn-1")
				return err
			},
		},
		{
			name: "clear interrupted turn",
			setup: func(store *V2Store, session SessionV2) error {
				if _, err := store.CreateRun(session.ID, "run-1", "", nil, time.Now()); err != nil {
					return err
				}
				if _, err := store.StartTurn(session.ID, "run-1", "turn-1", 0, time.Now()); err != nil {
					return err
				}
				_, err := store.MarkTurnInterrupted(session.ID, "turn-1")
				return err
			},
			write: func(store *V2Store, session SessionV2) error {
				_, err := store.ClearInterruptedTurn(session.ID)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, session := testMutationSession(t, "notify-"+stringsToID(test.name))
			if test.setup != nil {
				if err := test.setup(store, session); err != nil {
					t.Fatal(err)
				}
			}
			probe := &mutationProbe{ready: make(chan struct{}), all: make(chan Mutation, 8)}
			registration := store.RegisterMutationSink(probe)
			defer registration.Unregister()
			if err := test.write(store, session); err != nil {
				t.Fatal(err)
			}
			mutation := waitMutation(t, probe.all)
			if mutation.Overflow || mutation.SessionID != session.ID || mutation.Revision < 0 {
				t.Fatalf("notification = %#v, want one committed session mutation", mutation)
			}
			select {
			case extra := <-probe.all:
				t.Fatalf("one transaction produced an extra notification: %#v", extra)
			case <-time.After(80 * time.Millisecond):
			}
		})
	}
}

func stringsToID(value string) string {
	out := make([]rune, 0, len(value))
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}

func TestMutationSinkRollbackDoesNotNotifyAndSlowSinkCannotBlockWrites(t *testing.T) {
	store, session := testMutationSession(t, "notify-rollback")
	block := make(chan struct{})
	probe := &mutationProbe{ready: make(chan struct{}), block: block, all: make(chan Mutation, 16)}
	registration := store.RegisterMutationSinkWithOptions(probe, MutationSinkOptions{QueueCapacity: 1})
	defer registration.Unregister()
	if _, err := store.AppendItem(session.ID, SessionItemFromMessage("item-1", model.Message{Role: model.MessageRoleUser, Content: "one"})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-probe.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("slow sink did not start")
	}
	start := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := store.AppendItem(session.ID, SessionItemFromMessage(fmt.Sprintf("item-%d", i+2), model.Message{Role: model.MessageRoleUser, Content: "queued"})); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("durable writes waited for a blocked sink: %s", elapsed)
	}
	close(block)
	var sawOverflow bool
	deadline := time.After(3 * time.Second)
	for !sawOverflow {
		select {
		case mutation := <-probe.all:
			if mutation.Overflow {
				sawOverflow = true
			}
		case <-deadline:
			t.Fatal("bounded sink queue did not report recoverable overflow")
		}
	}
	if _, err := store.AppendItem(session.ID, SessionItemFromMessage("after-overflow", model.Message{Role: model.MessageRoleUser, Content: "recovered"})); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case mutation := <-probe.all:
			if mutation.SessionID == session.ID && mutation.Revision > 0 {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatal("sink did not recover after overflow")
		}
	}
}

func TestMutationSinkFailedTransactionDoesNotPublish(t *testing.T) {
	store, session := testMutationSession(t, "notify-failed")
	probe := &mutationProbe{ready: make(chan struct{}), all: make(chan Mutation, 4)}
	registration := store.RegisterMutationSink(probe)
	defer registration.Unregister()
	first := SessionItemFromMessage("item-1", model.Message{Role: model.MessageRoleUser, Content: "one"})
	if _, err := store.AppendItem(session.ID, first); err != nil {
		t.Fatal(err)
	}
	waitMutation(t, probe.all)
	before, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendItemsAndReplaceActiveHistory(session.ID, []SessionItem{first}, []string{first.ID}); err == nil {
		t.Fatal("duplicate transaction succeeded")
	}
	select {
	case mutation := <-probe.all:
		t.Fatalf("rolled back transaction published mutation: %#v", mutation)
	case <-time.After(150 * time.Millisecond):
	}
	state, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastSeq != before.LastSeq {
		t.Fatalf("failed transaction changed LastSeq: %d", state.LastSeq)
	}
}

func TestMutationSinkUnregisterWaitsOnlyForReleasableCallback(t *testing.T) {
	store, session := testMutationSession(t, "notify-close")
	block := make(chan struct{})
	probe := &mutationProbe{ready: make(chan struct{}), block: block, all: make(chan Mutation, 2)}
	registration := store.RegisterMutationSink(probe)
	if _, err := store.AppendItem(session.ID, SessionItemFromMessage("item-1", model.Message{Role: model.MessageRoleUser, Content: "one"})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-probe.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not start")
	}
	done := make(chan struct{})
	go func() {
		registration.Unregister()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Unregister returned while callback was blocked")
	case <-time.After(50 * time.Millisecond):
	}
	close(block)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Unregister leaked after callback release")
	}
	for {
		select {
		case <-probe.all:
		default:
			goto drained
		}
	}
drained:
	if _, err := store.AppendItem(session.ID, SessionItemFromMessage("item-2", model.Message{Role: model.MessageRoleUser, Content: "after"})); err != nil {
		t.Fatal(err)
	}
	select {
	case mutation := <-probe.all:
		if mutation.SessionID == session.ID {
			t.Fatalf("unregistered sink received mutation: %#v", mutation)
		}
	case <-time.After(100 * time.Millisecond):
	}
}
