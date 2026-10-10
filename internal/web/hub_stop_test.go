package web

import (
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/storage/memory"
)

// A stopped hub must release its subscription: a dead subscriber whose
// buffer fills would block the committer (the agent) on every later
// commit. This is the API-contract regression test for Hub.Stop.
func TestStoppedHubDoesNotBlockCommitter(t *testing.T) {
	// Instant successful turns: each Send commits user.input +
	// assistant.message and settles immediately.
	turns := make([][]model.Event, 2000)
	for i := range turns {
		turns[i] = []model.Event{model.TextDeltaEvent{Text: "x"}}
	}
	provider := &webFakeProvider{turns: turns}
	ag, err := agent.New(agent.Options{
		Provider:     provider,
		ProviderName: "test",
		ModelID:      "model-test",
		Storage:      memory.OpenStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession()
	if err != nil {
		t.Fatal(err)
	}

	hub := NewHub(session)
	// Let the hub's loop start and register its subscription.
	time.Sleep(50 * time.Millisecond)
	hub.Stop()
	time.Sleep(50 * time.Millisecond)

	// Commit more records than the subscription buffer holds: with the
	// subscription still registered, deliver would block forever once
	// the 1024-slot buffer fills.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < len(turns); i++ {
			if err := session.Send("m"); err != nil {
				t.Errorf("send %d: %v", i, err)
				return
			}
			waitSettled(t, session)
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("commits stalled after the hub stopped — the dead subscription blocked the committer")
	}
	_ = session.Close()
}

var _ model.Provider = (*webFakeProvider)(nil)
