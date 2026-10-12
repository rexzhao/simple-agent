package web

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/storage/jsonl"
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

// TestFirstStreamSeesHistory pins the cold-open contract: the FIRST
// request against a session with records on disk (hub not yet created)
// must receive them in the snapshot. The seeding used to race the run
// goroutine — a client registering in that window got records:[] and,
// because historical records never re-send, an empty thread until a
// reconnect (the blank chat view).
func TestFirstStreamSeesHistory(t *testing.T) {
	provider := &webFakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "one"}},
	}}
	// Manager one: create the session and land records on disk.
	manager, _, _, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)
	id := createJSONLSession(t, provider, store, projectDir)
	session, err := manager.Session(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Send("m1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return !session.Running() })

	// Manager two over the same root: every hub is cold again. The
	// first manager must be closed first — it holds the root serve.lock.
	manager.Close()
	// The first-ever /stream request is the racing client.
	factory := func(projectCwd string) (*agent.Agent, error) {
		return agent.New(agent.Options{
			Provider:     provider,
			ProviderName: "test",
			ModelID:      "model-test",
			Storage:      store,
			ProjectID:    jsonl.ProjectID(projectCwd),
			CWD:          projectCwd,
		})
	}
	m2, err := NewManager(ManagerOptions{
		ServerRoot: manager.root,
		Store:      store,
		AgentFor:   factory,
	})
	if err != nil {
		t.Fatal(err)
	}
	s2 := New(m2)
	hs2 := httptest.NewServer(s2.Handler())
	t.Cleanup(func() {
		hs2.Close()
		s2.Close()
	})

	res, err := http.Get(hs2.URL + "/api/s/" + id + "/stream") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	// The stream never ends (SSE): read just the first data line —
	// the snapshot frame — then stop. io.ReadAll would block forever.
	br := bufio.NewReader(res.Body)
	var snapshot string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			snapshot = strings.TrimSuffix(strings.TrimPrefix(line, "data: "), "\n")
			break
		}
	}
	if !strings.Contains(snapshot, `"kind":"snapshot"`) {
		t.Fatalf("no snapshot frame: %s", snapshot)
	}
	if !strings.Contains(snapshot, `"t":"user.input"`) || !strings.Contains(snapshot, `"t":"assistant.message"`) {
		t.Fatalf("first stream missed history records: %s", snapshot[:min(300, len(snapshot))])
	}
}
