package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
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
	name := ""
	if n := len(request.Messages); n > 0 {
		name = request.Messages[n-1].Content
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
	hub := srv.manager.sessions[session.ID()].hub

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
	hub := srv.manager.sessions[session.ID()].hub

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
	hub := srv.manager.sessions[session.ID()].hub

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
	hub := srv.manager.sessions[session.ID()].hub

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
