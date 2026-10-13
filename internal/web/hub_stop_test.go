package web

import (
	"bufio"
	"encoding/json"
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

// A web-only session has no CLI attached: nobody consumes the run's
// per-run event channel (the CLI's transition path, deleted in step 4),
// whose sends BLOCK when full. Before the hub migrated to the stream it
// drained that channel implicitly; the migration must keep the run
// unblocked — a turn emitting more than the channel buffer (256)
// transient events must complete on its own.
func TestWebOnlyRunDrainsLegacyEvents(t *testing.T) {
	deltas := make([]model.Event, 0, 301)
	for i := 0; i < 300; i++ {
		deltas = append(deltas, model.TextDeltaEvent{Text: "x"})
	}
	deltas = append(deltas, model.MessageDoneEvent{})
	provider := &webFakeProvider{turns: [][]model.Event{deltas}}
	session, _, _ := newTestManagerServer(t, provider)

	if err := session.Send("hi"); err != nil {
		t.Fatal(err)
	}
	// Nobody drains: the run must still settle by itself.
	waitFor(t, 5*time.Second, func() bool { return !session.Running() })
}

// TestHubEvictionResubscribesAndReissues pins the Lost-recovery path
// (milestone A step 3): when the hub's stream subscription is evicted
// (buffer overflow on non-droppable events), the hub must re-subscribe,
// rebuild its mirror from the fresh snapshot, and reissue a window
// snapshot to its connected SSE clients — which then continue receiving
// live frames. The eviction is forced deterministically: the hub's loop
// is frozen (h.mu held by the test) while enough turns overflow the
// subscription's 1024-slot buffer.
func TestHubEvictionResubscribesAndReissues(t *testing.T) {
	turns := make([][]model.Event, 500)
	for i := range turns {
		turns[i] = []model.Event{model.TextDeltaEvent{Text: "x"}, model.MessageDoneEvent{}}
	}
	provider := &webFakeProvider{turns: turns}
	session, srv, base := newTestManagerServer(t, provider)

	// One SSE client, connected before the flood: a background reader
	// keeps the connection drained (the client buffer is 256; the
	// reissued snapshot must reach it after recovery).
	res, cancel := sseGet(t, base.URL+api(session, "/stream"))
	defer cancel()
	defer res.Body.Close()
	frames := make(chan Frame, 8192)
	go func() {
		br := bufio.NewReader(res.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				close(frames)
				return
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var f Frame
			if err := json.Unmarshal([]byte(strings.TrimRight(line[6:], "\n")), &f); err != nil {
				close(frames)
				return
			}
			frames <- f
		}
	}()

	// The connect snapshot arrives first.
	select {
	case f, ok := <-frames:
		if !ok || f.Kind != "snapshot" {
			t.Fatalf("first frame is not a snapshot: %+v", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no connect snapshot")
	}

	hub := srv.manager.sessions[session.ID()].hub

	// Freeze the hub loop, then flood: each turn commits 2 records +
	// 1 terminal status (3 non-droppables); ~342 turns overflow the
	// 1024-slot subscription buffer and the next record evicts.
	hub.mu.Lock()
	for i := 0; i < 450; i++ {
		if err := session.Send("m"); err != nil {
			hub.mu.Unlock()
			t.Fatalf("send %d: %v", i, err)
		}
		waitSettled(t, session)
	}
	select {
	case <-hub.stream.Lost():
	case <-time.After(10 * time.Second):
		hub.mu.Unlock()
		t.Fatal("hub subscription was not evicted")
	}
	hub.mu.Unlock()

	// Recovery: the hub loop consumes its backlog, takes Lost,
	// re-subscribes, and reissues a window snapshot (resumed=false,
	// the full trailing window) to the connected client.
	sawReissue := false
	deadline := time.After(10 * time.Second)
	for !sawReissue {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("SSE connection closed during recovery")
			}
			if f.Kind == "snapshot" && !f.Resumed && f.Total >= 900 {
				// 450 turns × 2 records = 900; the mirror was rebuilt
				// from the fresh snapshot, not the stale one.
				if !f.More {
					t.Fatalf("reissued snapshot must mark scroll-back: total=%d", f.Total)
				}
				sawReissue = true
			}
		case <-deadline:
			t.Fatal("no window reissue after eviction")
		}
	}

	// The re-subscribed hub keeps serving live frames: a fresh turn's
	// records reach the same SSE connection.
	if err := session.Send("after"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	sawRecord := false
	deadline = time.After(10 * time.Second)
	for !sawRecord {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("SSE connection closed before the post-recovery record")
			}
			if f.Kind == "record" && f.Record != nil {
				var r struct {
					T string `json:"t"`
				}
				_ = json.Unmarshal(f.Record, &r)
				if r.T == "user.input" {
					sawRecord = true
				}
			}
		case <-deadline:
			t.Fatal("no live record frame after recovery")
		}
	}
}
