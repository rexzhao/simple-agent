package web

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/storage"
	"github.com/rexzhao/simple-agent/internal/storage/jsonl"
	"github.com/rexzhao/simple-agent/internal/storage/memory"
	"github.com/rexzhao/simple-agent/model"
)

// stagedProvider routes turns by the user input's content, so tests
// can interleave flood and gated turns without plan-index bookkeeping
// (the flood count varies with when the eviction lands). Gated turns
// park at a controlled point so tests can freeze the hub loop
// mid-stream and register clients inside the (processed, real-time]
// window the seq filter exists for.
type stagedTurn struct {
	pre     int           // deltas before the gate
	gate    chan struct{} // nil: no gate
	entered chan struct{} // closed when the gate is reached (nil: n/a)
	post    int           // deltas after the gate
}

type stagedProvider struct {
	mu     sync.Mutex
	byName map[string]stagedTurn
}

func (p *stagedProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	// Summary requests (the compaction prompt as the system message)
	// route by name "summary"; conversational turns by the user input.
	name := ""
	if n := len(request.Messages); n > 0 {
		if request.Messages[0].Role == model.MessageRoleSystem &&
			strings.Contains(request.Messages[0].Content, "Summarize the conversation so far") {
			name = "summary"
		} else {
			name = request.Messages[n-1].Content
		}
	}
	p.mu.Lock()
	turn, ok := p.byName[name]
	p.mu.Unlock()
	if !ok {
		return nil, errors.New("unexpected model request: " + name)
	}
	events := make(chan model.Event, 1024)
	go func() {
		defer close(events)
		emit := func(n int, text string) bool {
			for i := 0; i < n; i++ {
				select {
				case events <- model.TextDeltaEvent{Text: text}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}
		if !emit(turn.pre, "a") {
			return
		}
		if turn.gate != nil {
			if turn.entered != nil {
				close(turn.entered)
			}
			select {
			case <-turn.gate:
			case <-ctx.Done():
				return
			}
		}
		if !emit(turn.post, "b") {
			return
		}
		select {
		case events <- model.MessageDoneEvent{}:
		case <-ctx.Done():
		}
	}()
	return events, nil
}

// sseGet opens a stream request bound to a context deadline: without
// it, a blocked ReadString (no frame coming, hub idle) never returns
// and the test's server cleanup hangs on the open connection — a test
// failure would turn into a package timeout. The deadline is generous
// (30s); the caller must defer the cancel.
func sseGet(t *testing.T, url string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return res, cancel
}

// TestOutofBandStatusReadsMirror pins H2: out-of-band status frames
// (Notice and friends) must read the hub's running/compacting MIRROR,
// not the live session state. The hub's digest/count trail the session
// by definition; mixing live running/compacting with mirror digests
// emits frames that contradict the stream's order (an idle frame racing
// ahead of records the client has not yet received).
func TestOutofBandStatusReadsMirror(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	provider := &stagedProvider{byName: map[string]stagedTurn{
		"one": {pre: 1, post: 1},                               // settles before the freeze
		"two": {pre: 1, gate: gate, entered: entered, post: 1}, // parks mid-stream
	}}
	session, srv, _ := newTestManagerServer(t, provider)
	hub := srv.manager.hubOf(session.ID())

	// Observe frames on a directly-registered client.
	c, _, err := hub.register("", DefaultWindow)
	if err != nil {
		t.Fatal(err)
	}

	// One quick turn BEFORE the freeze: the hub processes it fully, so
	// its processed position advances past the client's registration
	// seq (out-of-band frames carry the processed seq; a client whose
	// snapshot is at or beyond it skips the frame — H3's rule — and
	// this test wants the frame delivered to observe its fields).
	if err := session.Send("one"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return !session.Running() })
	waitFor(t, 5*time.Second, func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.records) >= 3 // session.created + user.input + assistant
	})
	hub.mu.Lock()
	countAtFreeze := len(hub.records)
	hub.mu.Unlock()

	// Drain the frames turn "one" already queued on the client, so the
	// next frame read is unambiguously the out-of-band one below.
	for {
		select {
		case <-c.ch:
		default:
			goto drained
		}
	}
drained:

	// Freeze the loop, then start the gated run: live state becomes
	// running with streaming content, but the hub mirror stays idle
	// (the run's events sit unprocessed in the frozen backlog).
	hub.mu.Lock()
	if err := session.Send("two"); err != nil {
		hub.mu.Unlock()
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		hub.mu.Unlock()
		t.Fatal("run never reached its gate")
	}

	// Fire the out-of-band broadcast directly (broadcastStatusLocked
	// expects the caller to hold h.mu).
	hub.broadcastStatusLocked()
	hub.mu.Unlock()

	select {
	case raw := <-c.ch:
		var f Frame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		if f.Kind != "status" {
			t.Fatalf("expected a status frame, got %s", f.Kind)
		}
		if f.Running == nil || *f.Running {
			t.Fatal("out-of-band status must read the hub mirror (idle), not the live session (running)")
		}
		if f.Count != countAtFreeze {
			t.Fatalf("status frame count %d must match the mirror at freeze time %d", f.Count, countAtFreeze)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no out-of-band status frame")
	}

	// Let the run finish before the server cleanup.
	close(gate)
	waitSettled(t, session)
}

// TestSnapshotSeqFiltersReplayedFrames pins H3 with a completed turn:
// a client registering while the hub trails the session gets a
// real-time snapshot (seq S); the hub's backlog replay of Transient and
// Status events with seq <= S must be skipped for that client — they
// are already reflected in its snapshot. Record frames are exempt: the
// snapshot's records come from the hub mirror (seq P <= S), so record
// frames are how the client catches up.
func TestSnapshotSeqFiltersReplayedFrames(t *testing.T) {
	provider := &stagedProvider{byName: map[string]stagedTurn{"hi": {pre: 1, post: 1}}}
	session, srv, _ := newTestManagerServer(t, provider)
	hub := srv.manager.hubOf(session.ID())

	// Freeze at P, run a full turn to completion (real-time S is past
	// the terminal status), then register INSIDE the (P, S] window.
	hub.mu.Lock()
	if err := session.Send("hi"); err != nil {
		hub.mu.Unlock()
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return !session.Running() })
	c, _, err := hub.registerLocked("", DefaultWindow)
	if err != nil {
		hub.mu.Unlock()
		t.Fatal(err)
	}
	hub.mu.Unlock()

	// After the unfreeze the hub drains the whole backlog. The client
	// must see ONLY record frames — everything else of the turn is
	// already folded into its snapshot.
	records := 0
	deadline := time.After(2 * time.Second)
	for {
		var raw []byte
		var ok bool
		select {
		case raw, ok = <-c.ch:
			if !ok {
				t.Fatal("client channel closed")
			}
		case <-deadline:
			if records < 2 {
				t.Fatalf("record frames must still be delivered, got %d", records)
			}
			return
		}
		var f Frame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		switch f.Kind {
		case "record":
			records++
		case "event", "status":
			t.Fatalf("frame replayed past the client's snapshot seq: kind=%s type=%s", f.Kind, f.Type)
		}
	}
}

// TestSnapshotSeqFiltersReplayedDeltas pins H3 with a live stream: the
// snapshot's Streaming content is real-time (seq S), so the hub's
// replay of deltas with seq <= S must be skipped — otherwise the same
// text appears twice. Deltas after S flow normally; the record replaces
// the streaming row as always. The invariant, exactly: the snapshot's
// streaming content plus every delta frame the client receives equals
// the final record's content, with no overlap.
//
// The (apply, enqueue) window the invariant guards is a per-delta race
// in the run goroutine — a single pass catches a regression only with
// low probability, so the sequence repeats 50 times inside the test
// (each pass also exercises a different consumed-prefix length k).
func TestSnapshotSeqFiltersReplayedDeltas(t *testing.T) {
	const (
		rounds     = 50
		preDeltas  = 200
		postDeltas = 50
	)
	byName := make(map[string]stagedTurn, rounds)
	gates := make([]chan struct{}, rounds)
	entered := make([]chan struct{}, rounds)
	for i := 0; i < rounds; i++ {
		gates[i] = make(chan struct{})
		entered[i] = make(chan struct{})
		byName[fmt.Sprintf("hi%d", i)] = stagedTurn{pre: preDeltas, gate: gates[i], entered: entered[i], post: postDeltas}
	}
	provider := &stagedProvider{byName: byName}
	session, srv, _ := newTestManagerServer(t, provider)
	hub := srv.manager.hubOf(session.ID())

	for i := 0; i < rounds; i++ {
		// Freeze first: the run's events all land in the backlog. The
		// per-run channel holds the pre-gate deltas the run has
		// consumed (buffer 256 — no wedge; the drain spawns when the
		// loop processes the run-start status after the unfreeze).
		hub.mu.Lock()
		if err := session.Send(fmt.Sprintf("hi%d", i)); err != nil {
			hub.mu.Unlock()
			t.Fatal(err)
		}
		select {
		case <-entered[i]:
		case <-time.After(10 * time.Second):
			hub.mu.Unlock()
			t.Fatalf("round %d: run never reached its gate", i)
		}
		// The provider has buffered its pre-gate deltas; make sure the
		// run has consumed at least one, so the snapshot's streaming
		// slot is non-empty (the skip path is what this test exercises).
		waitFor(t, 10*time.Second, func() bool {
			snap := session.SnapshotStreamState()
			return snap.Streaming != nil && snap.Streaming.Message.Content != ""
		})

		// Register inside the window: the snapshot is real-time — its
		// streaming slot carries however many pre-gate deltas the run
		// has consumed by now (seq <= S on the wire).
		c, snapRaw, err := hub.registerLocked("", DefaultWindow)
		if err != nil {
			hub.mu.Unlock()
			t.Fatal(err)
		}
		hub.mu.Unlock()

		var snap Frame
		if err := json.Unmarshal(snapRaw, &snap); err != nil {
			t.Fatal(err)
		}
		if snap.Streaming == nil {
			t.Fatalf("round %d: snapshot must carry the in-flight streaming item", i)
		}
		snapText := snap.Streaming.Content
		if snapText == "" || strings.Trim(snapText, "a") != "" {
			t.Fatalf("round %d: snapshot streaming content must be pre-gate a-deltas, got %q", i, snapText)
		}
		if snap.Running == nil || !*snap.Running {
			t.Fatalf("round %d: snapshot must report the live run (real-time by design)", i)
		}

		// Release and read until THIS item's assistant record: deltas
		// precede their record in seq order, so the record is the
		// deterministic end of the interesting frames.
		close(gates[i])
		var deltaText strings.Builder
		recordContent := ""
		deadline := time.After(10 * time.Second)
	read:
		for {
			select {
			case raw, ok := <-c.ch:
				if !ok {
					t.Fatalf("round %d: client channel closed", i)
				}
				var f Frame
				if err := json.Unmarshal(raw, &f); err != nil {
					t.Fatal(err)
				}
				switch {
				case f.Kind == "event" && f.Type == "text_delta":
					deltaText.WriteString(f.Text)
				case f.Kind == "record" && f.Record != nil:
					var r struct {
						Item    string `json:"item"`
						T       string `json:"t"`
						Content string `json:"content"`
					}
					_ = json.Unmarshal(f.Record, &r)
					if r.T == "assistant.message" && r.Item == snap.Streaming.Item {
						recordContent = r.Content
						break read
					}
				}
			case <-deadline:
				t.Fatalf("round %d: the assistant record frame was not delivered (records bypass the filter)", i)
			}
		}
		// The no-duplication invariant: snapshot content + received
		// deltas == the final record, exactly.
		if got := snapText + deltaText.String(); got != recordContent {
			t.Fatalf("round %d: snapshot (%d a's) + deltas (%d chars) != record (%d chars): duplicated or lost text",
				i, len(snapText), deltaText.Len(), len(recordContent))
		}
		hub.unregister(c)
		waitSettled(t, session)
	}
}

// TestNoticeFrameReachesFreshClient pins the notice delivery rule: a
// notice-bearing status frame must reach EVERY client, including one
// whose connect snapshot is at or past the hub's processed position —
// the snapshot frame carries no Notice field, so the seq filter would
// otherwise silently swallow the only frame that carries it (and an
// idle session might never produce another). The frame omits the
// positional fields on purpose (stale running/compacting/digest must
// not regress a client newer than the hub's mirror; both clients treat
// absent positional fields as no-change).
func TestNoticeFrameReachesFreshClient(t *testing.T) {
	provider := &stagedProvider{byName: map[string]stagedTurn{
		"one": {pre: 1, post: 1},
	}}
	session, srv, _ := newTestManagerServer(t, provider)
	hub := srv.manager.hubOf(session.ID())

	// A fresh client: its startSeq is the real-time position, which the
	// hub can never be past — any frame carrying the hub's processedSeq
	// is skippable for it.
	c, _, err := hub.register("", DefaultWindow)
	if err != nil {
		t.Fatal(err)
	}

	hub.Notice("compact failed: boom")

	select {
	case raw := <-c.ch:
		var f Frame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		if f.Kind != "status" || f.Notice != "compact failed: boom" {
			t.Fatalf("expected a notice-bearing status frame, got kind=%s notice=%q", f.Kind, f.Notice)
		}
		// Positional fields must be absent (stale values would regress
		// a client newer than the mirror).
		if f.Running != nil || f.Compacting != nil || f.Digest != "" {
			t.Fatalf("notice frame must omit positional fields, got running=%v compacting=%v digest=%q",
				f.Running, f.Compacting, f.Digest)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notice frame never reached the fresh client")
	}
}

// TestCompactionSegmentSwitch pins §14.5's truncation protocol end to
// end: a committed compaction truncates the session's record view to
// [session.created, compaction], the hub's mirror follows with a fresh
// segment genesis, and every connected client receives a reissued
// snapshot (resumed=false) whose wire-level invariant holds — folding
// the frame's records from its anchor reproduces its digest. The chain
// head is then compared across a restart: a hub over the reopened
// session derives the same head from the same segment.
func TestCompactionSegmentSwitch(t *testing.T) {
	root := t.TempDir()
	projectDir := t.TempDir()
	provider := &stagedProvider{byName: map[string]stagedTurn{
		"hi":      {pre: 1, post: 1},
		"summary": {pre: 2, post: 0},
	}}
	store := memory.OpenStore()
	registry := &projects.Registry{Projects: []projects.Entry{
		{Path: projectDir, Added: time.Now().UTC()},
	}}
	if err := registry.Save(projects.FilePath(root)); err != nil {
		t.Fatal(err)
	}
	factory := func(projectCwd string) (*agent.Agent, error) {
		return agent.New(agent.Options{
			ModelResolver: agent.NewStaticResolver(provider, nil, 0, 0, ""),
			Storage:       store,
			CWD:           projectCwd,
		})
	}
	manager, err := NewManager(ManagerOptions{ServerRoot: root, Store: store, AgentFor: factory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	ag, err := factory(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := ag.NewSession(projects.ProjectID(projectDir), agent.SessionMetaForTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.MaterializeHeader(); err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	session, err := manager.Session(created.ID())
	if err != nil {
		t.Fatal(err)
	}
	server := New(manager)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	// A connected client sees the whole cycle live.
	res, cancel := sseGet(t, httpServer.URL+api(session, "/stream"))
	defer cancel()
	defer res.Body.Close()
	br := bufio.NewReader(res.Body)

	if err := session.Send("hi"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	// Drain to the run's terminal status so the compaction's frames are
	// the next ones read.
	sawTerminal := false
	for !sawTerminal {
		f := nextFrame(t, br)
		if f.Kind == "status" && f.Running != nil && !*f.Running {
			sawTerminal = true
		}
	}

	// The pre-compaction chain head (a client's held anchor).
	stateRes, err := http.Get(httpServer.URL + api(session, "/state")) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var preState state
	if err := json.NewDecoder(stateRes.Body).Decode(&preState); err != nil {
		t.Fatal(err)
	}
	stateRes.Body.Close()

	// The compaction: the hub truncates its mirror and reissues the
	// snapshot (no record frame for the compacted commit — folding it
	// onto a client's old chain would be drift).
	if err := session.Compact(); err != nil {
		t.Fatal(err)
	}
	var snap *Frame
	for snap == nil {
		f := nextFrame(t, br)
		if f.Kind == "snapshot" {
			snap = &f
		}
	}
	if snap.Resumed {
		t.Fatal("the reissued snapshot must be resumed=false — clients do a clean rebuild")
	}
	if snap.Total != 2 {
		t.Fatalf("reissued snapshot total = %d, want 2 [session.created, compaction]", snap.Total)
	}
	var records []storage.Record
	if err := json.Unmarshal(snap.Records, &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 ||
		records[0].Type != storage.TypeSessionCreated ||
		records[1].Type != storage.TypeContextCompacted {
		t.Fatalf("reissued records = %v, want [session.created, context.compacted]", records)
	}
	// The wire-level invariant (the same one the clients verify): fold
	// the frame's records from its anchor and the result must equal the
	// frame's digest.
	head := snap.Anchor
	for _, r := range records {
		head = chainStep(head, r)
	}
	if head != snap.Digest {
		t.Fatalf("fold(anchor, records) = %s, frame digest = %s — the reissued snapshot is not self-consistent", head, snap.Digest)
	}
	restartHead := snap.Digest

	// A pre-compaction anchor is stale after the truncation: /history
	// must refuse it (409) — serving a window from the NEW chain under
	// the old anchor would fake drift on the client's strict fold.
	histRes, err := http.Get(httpServer.URL + api(session, "/history?before="+preState.Digest)) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer histRes.Body.Close()
	if histRes.StatusCode != http.StatusConflict {
		t.Fatalf("/history with a pre-compaction anchor = %d, want 409 stale_anchor", histRes.StatusCode)
	}

	// Restart consistency (§14.4): the reopened session replays the
	// same active segment, and a hub over it derives the same chain
	// head — the per-segment genesis is a function of the segment, not
	// of process history.
	id := session.ID()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := ag.OpenSession(projects.ProjectID(projectDir), id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	freshRecords := reopened.SnapshotStream().Records
	if len(freshRecords) != 2 || freshRecords[0].ItemID != records[0].ItemID ||
		freshRecords[1].ItemID != records[1].ItemID {
		t.Fatalf("fresh Open records = %v, want the same [created, compaction] as the truncated view", freshRecords)
	}
	h2 := NewHub(reopened)
	h2state := h2.state()
	h2.Stop()
	if h2state.Digest != restartHead {
		t.Fatalf("restarted hub head %s != pre-restart head %s — the segment genesis is not restart-stable", h2state.Digest, restartHead)
	}
}

// nextFrame reads one SSE data frame.
func nextFrame(t *testing.T, br *bufio.Reader) Frame {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("no frame within the deadline")
		}
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream read: %v", err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var f Frame
		if err := json.Unmarshal([]byte(strings.TrimRight(line[6:], "\n")), &f); err != nil {
			t.Fatalf("frame: %v (%s)", err, line)
		}
		return f
	}
}

// writeSegment writes a session's log.jsonl by hand — the store's
// on-disk shape is user data, and the P1 regression class is exactly
// "disk states the writer never produces" (headerless segments from a
// hand-edited first line or a failed header copy).
func writeSegment(t *testing.T, store *jsonl.Store, projectDir, id string, records []storage.Record) {
	t.Helper()
	dir := filepath.Join(store.Root(), projects.ProjectID(projectDir), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range records {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(data))
	}
	if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSegmentShapesOpenAndStream pins the P1 regression for the two
// degraded segment SHAPES that carry a session.created header but
// lost other structure: a segment-0 log with the header and input
// only, and a post-compaction segment starting at the compaction.
// Both must open through the manager and stream, not panic in
// NewHub's genesis derivation. (A log with NO header at all is
// refused — see TestOpenEmptyMetaRejectedByProductionResolver and
// TestOpenEmptyMetaMaps409.)
func TestSegmentShapesOpenAndStream(t *testing.T) {
	for _, tc := range []struct {
		name    string
		records []storage.Record
	}{
		{
			name: "segment 0 with header and input only",
			records: []storage.Record{
				// The model identity lives on the session.created
				// record alone (§15.2): the header is present, and
				// the rest of the log starts at user.input.
				{ItemID: "h00000000000000000000000000", Type: storage.TypeSessionCreated, Time: "2026-01-01T00:00:00Z", Provider: "test", Model: "model-test"},
				{ItemID: "u00000000000000000000000001", Type: storage.TypeUserInput, Time: "2026-01-01T00:00:00Z", RunID: "run-1", Content: "hi"},
			},
		},
		{
			name: "post-compaction segment",
			records: []storage.Record{
				{ItemID: "h00000000000000000000000000", Type: storage.TypeSessionCreated, Time: "2026-01-01T00:00:00Z", Provider: "test", Model: "model-test"},
				{ItemID: "x00000000000000000000000001", Type: storage.TypeContextCompacted, Time: "2026-01-01T00:00:00Z", Content: "summary"},
				{ItemID: "u00000000000000000000000002", Type: storage.TypeUserInput, Time: "2026-01-01T00:01:00Z", RunID: "run-2", Content: "after"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			projectDir := t.TempDir()
			store, err := jsonl.OpenStore(filepath.Join(root, "data"))
			if err != nil {
				t.Fatal(err)
			}
			registry := &projects.Registry{Projects: []projects.Entry{
				{Path: projectDir, Added: time.Now().UTC()},
			}}
			if err := registry.Save(projects.FilePath(root)); err != nil {
				t.Fatal(err)
			}
			id := storage.NewSessionID()
			writeSegment(t, store, projectDir, id, tc.records)

			manager, err := NewManager(ManagerOptions{ServerRoot: root, Store: store, AgentFor: func(string) (*agent.Agent, error) {
				return agent.New(agent.Options{
					ModelResolver: agent.NewStaticResolver(&stagedProvider{}, nil, 0, 0, ""),
					Storage:       store,
					CWD:           projectDir,
				})
			}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { manager.Close() })
			server := New(manager)
			httpServer := httptest.NewServer(server.Handler())
			t.Cleanup(httpServer.Close)

			// The stream opens and carries a snapshot — no panic, no
			// wedge (the request has a deadline: a wedged open hangs
			// here, which is the P1 failure mode).
			res, cancel := sseGet(t, httpServer.URL+"/api/s/"+id+"/stream")
			defer cancel()
			defer res.Body.Close()
			br := bufio.NewReader(res.Body)
			f := nextFrame(t, br)
			if f.Kind != "snapshot" {
				t.Fatalf("first frame = %s, want a snapshot", f.Kind)
			}
			var got []storage.Record
			if err := json.Unmarshal(f.Records, &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.records) {
				t.Fatalf("snapshot records = %d, want %d (the full headerless segment)", len(got), len(tc.records))
			}
		})
	}
}

// TestCompactedSegmentChainStable pins the genesis rule for the
// post-compaction shape [created, compacted, …]: the segment seeds
// from the COMPACTION's ItemID (not the header — §14.4), and the
// chain head is stable across a reopen (the same derivation runs at
// truncation time and at fresh Open).
func TestCompactedSegmentChainStable(t *testing.T) {
	root := t.TempDir()
	projectDir := t.TempDir()
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	id := storage.NewSessionID()
	records := []storage.Record{
		{ItemID: "h00000000000000000000000000", Type: storage.TypeSessionCreated, Time: "2026-01-01T00:00:00Z", Provider: "test", Model: "model-test"},
		{ItemID: "x00000000000000000000000001", Type: storage.TypeContextCompacted, Time: "2026-01-01T00:00:00Z", Content: "summary"},
		{ItemID: "u00000000000000000000000002", Type: storage.TypeUserInput, Time: "2026-01-01T00:01:00Z", RunID: "run-2", Content: "after"},
	}
	writeSegment(t, store, projectDir, id, records)

	ag, err := agent.New(agent.Options{
		ModelResolver: agent.NewStaticResolver(&stagedProvider{}, nil, 0, 0, ""),
		Storage:       store,
		CWD:           projectDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.OpenSession(projects.ProjectID(projectDir), id)
	if err != nil {
		t.Fatal(err)
	}
	h1 := NewHub(session)
	head1 := h1.state().Digest
	h1.Stop()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: same segment, same derivation, same chain.
	reopened, err := ag.OpenSession(projects.ProjectID(projectDir), id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	h2 := NewHub(reopened)
	head2 := h2.state().Digest
	h2.Stop()
	if head1 != head2 {
		t.Fatalf("reopened chain head %s != original %s — the headerless segment genesis is not restart-stable", head2, head1)
	}
	// And the derivation matches the production rule (segmentGenesis):
	// [created, compacted, …] seeds from the compaction (records[1]).
	folded := segmentGenesis(id, records)
	for _, r := range records {
		folded = chainStep(folded, r)
	}
	if head1 != folded {
		t.Fatalf("chain head %s != rule-derived %s", head1, folded)
	}
}

// TestManagerOpenPanicDoesNotWedge pins the defer guard in Manager.open:
// a panic anywhere in the open path (injected through the agent factory
// — any panic source is equivalent once it happens under the opening
// placeholder) must not leave the placeholder behind. The NEXT request
// for the same id must return — an error is fine, a hang is the P1
// failure mode.
func TestManagerOpenPanicDoesNotWedge(t *testing.T) {
	root := t.TempDir()
	projectDir := t.TempDir()
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	registry := &projects.Registry{Projects: []projects.Entry{
		{Path: projectDir, Added: time.Now().UTC()},
	}}
	if err := registry.Save(projects.FilePath(root)); err != nil {
		t.Fatal(err)
	}
	id := storage.NewSessionID()
	writeSegment(t, store, projectDir, id, []storage.Record{
		{ItemID: "u00000000000000000000000001", Type: storage.TypeUserInput, Time: "2026-01-01T00:00:00Z", RunID: "run-1", Content: "hi"},
	})

	manager, err := NewManager(ManagerOptions{ServerRoot: root, Store: store, AgentFor: func(string) (*agent.Agent, error) {
		panic("injected: factory assembly exploded")
	}})
	if err != nil {
		t.Fatal(err)
	}
	// The teardown is asynchronous ON PURPOSE: this test exists to fail
	// under a regression (a leaked opening placeholder parks the handler
	// forever), and a graceful httptest Close / Manager.Close would
	// block on that parked handler — burying the test's assertion under
	// a package-level timeout. Both closers run in goroutines so the
	// failure surfaces as the assertion line; on the healthy path both
	// complete immediately (everything here is test-local).
	t.Cleanup(func() { go manager.Close() })
	server := New(manager)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(func() { go httpServer.Close() })

	url := httpServer.URL + "/api/s/" + id + "/state"
	for attempt := 1; attempt <= 2; attempt++ {
		// The request must COMPLETE within the deadline: the first
		// carries the panic (recovered by net/http → connection
		// reset), the second proves the opening placeholder was
		// cleaned — a leftover placeholder blocks on <-inFlight
		// forever, the P1 wedge.
		done := make(chan error, 1)
		go func() {
			res, err := http.Get(url) //nolint:noctx // test
			if err == nil {
				res.Body.Close()
			}
			done <- err
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("request %d never returned — the open panic wedged the session (opening placeholder leaked)", attempt)
		}
	}
}

// TestCompactTruncationKeepsHeader pins the third truncation point:
// compacting a session whose segment is [created, input, …] produces
// [session.created, context.compacted] — the header is never dropped
// and never fabricated — and the in-process view equals what a fresh
// Open replays, record by record.
func TestCompactTruncationKeepsHeader(t *testing.T) {
	root := t.TempDir()
	projectDir := t.TempDir()
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	id := storage.NewSessionID()
	writeSegment(t, store, projectDir, id, []storage.Record{
		{ItemID: "h00000000000000000000000000", Type: storage.TypeSessionCreated, Time: "2026-01-01T00:00:00Z", Provider: "test", Model: "model-test"},
		{ItemID: "u00000000000000000000000001", Type: storage.TypeUserInput, Time: "2026-01-01T00:00:00Z", RunID: "run-1", Content: "hi"},
	})
	provider := &stagedProvider{byName: map[string]stagedTurn{
		"more":    {pre: 1, post: 1},
		"summary": {pre: 1, post: 0},
	}}
	ag, err := agent.New(agent.Options{
		ModelResolver: agent.NewStaticResolver(provider, nil, 0, 0, ""),
		Storage:       store,
		CWD:           projectDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.OpenSession(projects.ProjectID(projectDir), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Send("more"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	if err := session.Compact(); err != nil {
		t.Fatal(err)
	}
	inProc := session.SnapshotStream().Records
	// With a created header on disk the truncation keeps it: the
	// post-compact segment is [session.created, context.compacted]
	// (the created header is never fabricated and never dropped).
	if len(inProc) != 2 || inProc[0].Type != storage.TypeSessionCreated || inProc[1].Type != storage.TypeContextCompacted {
		t.Fatalf("in-process records after the compact = %v, want [session.created context.compacted]", recordTypesWeb(inProc))
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := ag.OpenSession(projects.ProjectID(projectDir), id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	fresh := reopened.SnapshotStream().Records
	if len(fresh) != len(inProc) {
		t.Fatalf("fresh Open records = %v, in-process = %v — the headerless truncation diverged from the replay", recordTypesWeb(fresh), recordTypesWeb(inProc))
	}
	for i := range fresh {
		if fresh[i].ItemID != inProc[i].ItemID || fresh[i].Type != inProc[i].Type {
			t.Fatalf("record %d: fresh %s/%s != in-process %s/%s", i, fresh[i].Type, fresh[i].ItemID, inProc[i].Type, inProc[i].ItemID)
		}
	}
}

func recordTypesWeb(records []storage.Record) []string {
	types := make([]string, len(records))
	for i, r := range records {
		types[i] = r.Type
	}
	return types
}

// TestOpenUnavailableModelMaps409 pins acceptance (c) on the web side: a
// session whose recorded model no longer resolves opens as a stable
// 409 model_unavailable — never a 500, never a silent fallback.
func TestOpenUnavailableModelMaps409(t *testing.T) {
	root := t.TempDir()
	projectDir := t.TempDir()
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	registry := &projects.Registry{Projects: []projects.Entry{
		{Path: projectDir, Added: time.Now().UTC()},
	}}
	if err := registry.Save(projects.FilePath(root)); err != nil {
		t.Fatal(err)
	}

	// Create the session with a resolver that works.
	good := agent.NewStaticResolver(&recordingProvider{}, nil, 0, 0, "")
	agGood, err := agent.New(agent.Options{Storage: store, ModelResolver: good, CWD: projectDir})
	if err != nil {
		t.Fatal(err)
	}
	ns := projects.ProjectID(projectDir)
	session, err := agGood.NewSession(ns, storage.SessionMeta{Provider: "gone", Model: "removed-model"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.MaterializeHeader(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	// Serve with a resolver whose configuration no longer has the model.
	bad := agent.NewStaticResolver(nil, nil, 0, 0, "")
	_ = bad
	manager, err := NewManager(ManagerOptions{
		ServerRoot: root,
		Store:      store,
		AgentFor: func(string) (*agent.Agent, error) {
			return agent.New(agent.Options{Storage: store, ModelResolver: &unavailableWebResolver{}})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	server := New(manager)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	res, err := http.Get(httpServer.URL + "/api/s/" + session.ID() + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.StatusCode)
	}
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error != "model_unavailable" {
		t.Fatalf("error code = %q, want model_unavailable", body.Error)
	}
	if !strings.Contains(body.Message, "removed-model") {
		t.Fatalf("message does not name the model: %q", body.Message)
	}
}

// productionWebResolver builds the production config-backed resolver
// (the web package can import internal/config). The provider
// constructor is only reached after a successful resolve — for the
// empty-meta cases it never runs.
func productionWebResolver(t *testing.T) agent.ModelResolver {
	t.Helper()
	cfg := &config.Config{
		DefaultProvider: "fake",
		DefaultModel:    "fake-model",
		Providers: map[string]config.ProviderConfig{
			"fake": {
				Name:    "fake",
				BaseURL: "http://127.0.0.1:1",
				APIKey:  "test",
				Models: map[string]config.ModelProfile{
					"fake-model": {ID: "fake-model", Type: config.ProviderTypeOpenAIChat},
				},
			},
		},
	}
	resolver, err := config.NewModelResolver(cfg, func(config.ResolvedModel) (model.Provider, error) {
		return &recordingProvider{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

// unavailableWebResolver fails every Resolve with the typed error.
type unavailableWebResolver struct{}

func (r *unavailableWebResolver) Resolve(meta storage.SessionMeta) (agent.Resolved, error) {
	return agent.Resolved{}, agent.ErrModelUnavailable
}
func (r *unavailableWebResolver) List() []agent.ModelDescriptor { return nil }

// recordingProvider is a minimal provider for the web test (the agent
// package's recordingProvider is not exported).
type recordingProvider struct{}

func (p *recordingProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	events := make(chan model.Event)
	close(events)
	return events, nil
}

// TestRecordFrameReasoningLevelWireAndTuple pins the 2b wire invariants:
// a session.created with an empty reasoning_level serializes to the
// same JSON as before the field existed (omitempty drops it — old
// records keep their exact bytes), a recorded level rides the wire,
// and the v2 digest tuple COVERS the level (two records differing only
// in reasoning_level must fold differently — §15.2: no field stays
// outside the tamper-proof tuple).
func TestRecordFrameReasoningLevelWireAndTuple(t *testing.T) {
	empty := storage.Record{ItemID: "h1", Type: storage.TypeSessionCreated, Provider: "p", Model: "m"}
	withLevel := storage.Record{ItemID: "h1", Type: storage.TypeSessionCreated, Provider: "p", Model: "m", ReasoningLevel: "high"}

	emptyJSON, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(emptyJSON, []byte("reasoning_level")) {
		t.Fatalf("empty level serialized into the wire: %s", emptyJSON)
	}
	withJSON, err := json.Marshal(withLevel)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(withJSON, []byte("reasoning_level")) {
		t.Fatalf("recorded level missing from the wire: %s", withJSON)
	}
	// The digest tuple is v2: ReasoningLevel IS part of chainInput.
	if chainInput(withLevel) == chainInput(empty) {
		t.Fatal("chainInput ignores ReasoningLevel — the field would sit outside the tamper-proof tuple")
	}
}

// TestV1HeadMissesUnderV2 pins the upgrade contract (§15.2): every
// digest a v1 client can hold — genesis, post-compaction genesis, and
// record heads — differs under sai/v2/, so a v1 head is never a false
// hit for a v2 server/client pair. A miss costs only the window
// replay (the safe resync path the store already takes on unknown
// anchors — pinned generically by store_check s9, and for a genuine
// v1 digest by s14).
func TestV1HeadMissesUnderV2(t *testing.T) {
	const v1Genesis = "sai/v1/"
	sha := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	sessionID := "s1"
	compactedID := "x1"
	r := storage.Record{ItemID: "i1", Type: storage.TypeUserInput, Time: "t1", Content: "hi"}

	// v1 genesis / post-compaction genesis / first head, computed with
	// the frozen v1 rules (the prefix; the v1 tuple is the v2 tuple
	// minus the trailing reasoning_level field — for a record with an
	// empty level the encodings coincide, which is exactly why the
	// PREFIX is what a v2 bump must change).
	v1Seg0 := sha(v1Genesis + sessionID)
	v1SegN := sha(v1Genesis + sessionID + "/" + compactedID)
	v1Head := sha(v1Seg0 + "\n" + chainInput(r))

	if v1Seg0 == chainGenesis(sessionID) {
		t.Fatal("v1 segment-0 genesis collides with v2 — upgrade would be silent")
	}
	if v1SegN == chainGenesisAt(sessionID, compactedID) {
		t.Fatal("v1 post-compaction genesis collides with v2")
	}
	if v1Head == chainStep(chainGenesis(sessionID), r) {
		t.Fatal("v1 record head collides with v2")
	}
}

// TestOpenEmptyMetaMaps409 pins the web open-layer for empty meta
// (§15.2): a session whose created record lacks provider/model opens
// as 409 model_unavailable, the same code as a removed model — one
// stable contract for "the session's model cannot run here".
func TestOpenEmptyMetaMaps409(t *testing.T) {
	root := t.TempDir()
	projectDir := t.TempDir()
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	registry := &projects.Registry{Projects: []projects.Entry{
		{Path: projectDir, Added: time.Now().UTC()},
	}}
	if err := registry.Save(projects.FilePath(root)); err != nil {
		t.Fatal(err)
	}

	// A foreign log: created record with empty provider/model.
	id := storage.NewSessionID()
	writeSegment(t, store, projectDir, id, []storage.Record{
		{ItemID: "h00000000000000000000000001", Type: storage.TypeSessionCreated, Time: "2026-01-01T00:00:00Z"},
		{ItemID: "u00000000000000000000000002", Type: storage.TypeUserInput, Time: "2026-01-01T00:01:00Z", RunID: "run-1", Content: "hi"},
	})

	manager, err := NewManager(ManagerOptions{ServerRoot: root, Store: store, AgentFor: func(string) (*agent.Agent, error) {
		return agent.New(agent.Options{
			// The PRODUCTION config resolver (not the static test
			// bridge): the empty-meta guard it enforces is what maps
			// to the 409. Deleting that guard in config/resolver.go
			// turns this test red (200 instead of 409).
			ModelResolver: productionWebResolver(t),
			Storage:       store,
			CWD:           projectDir,
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	server := New(manager)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	res, err := http.Get(httpServer.URL + "/api/s/" + id + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error != "model_unavailable" {
		t.Fatalf("error code = %q, want model_unavailable", body.Error)
	}
}
