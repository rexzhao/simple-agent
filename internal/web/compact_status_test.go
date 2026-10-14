package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

// blockSummaryProvider answers the first N calls instantly and blocks
// every later call until released (the manual compaction's summary is
// the blocked one).
type blockSummaryProvider struct {
	mu      sync.Mutex
	turns   int
	freeAt  int // calls with index > freeAt block
	release chan struct{}
	// blocked, when non-nil, is closed once — the first time a call
	// parks on release. Tests synchronize on it before closing release:
	// the run-start Status precedes the provider call, so releasing on
	// the Status alone races the capture of the release channel (a
	// late capture parks on the replacement channel forever).
	blockedOnce sync.Once
	blocked     chan struct{}
}

func (p *blockSummaryProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	p.mu.Lock()
	p.turns++
	n := p.turns
	free := p.freeAt
	p.mu.Unlock()
	events := make(chan model.Event, 2)
	if n > free {
		p.mu.Lock()
		release := p.release
		p.mu.Unlock()
		if p.blocked != nil {
			p.blockedOnce.Do(func() { close(p.blocked) })
		}
		go func() {
			select {
			case <-release:
			case <-ctx.Done():
			}
			close(events)
		}()
		return events, nil
	}
	go func() {
		defer close(events)
		events <- model.TextDeltaEvent{Text: "answer"}
		events <- model.MessageDoneEvent{}
	}()
	return events, nil
}

// readFrames streams SSE frames until the condition is met or the
// deadline passes, returning the frames seen. The request carries a
// context deadline: without it, a blocked read (condition never met,
// hub idle) never returns, and the test's server cleanup hangs on the
// open connection forever.
func readFrames(t *testing.T, url string, cond func(Frame) bool) []Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	br := bufio.NewReader(res.Body)
	var frames []Frame
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream read after %d frames: %v", len(frames), err)
		}
		line = strings.TrimRight(line, "\n")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var f Frame
		if err := json.Unmarshal([]byte(line[6:]), &f); err != nil {
			t.Fatalf("frame: %v (%s)", err, line)
		}
		frames = append(frames, f)
		if cond(f) {
			return frames
		}
		select {
		case <-ctx.Done():
			t.Fatal("condition never met")
		default:
		}
	}
}

// TestCompactStatusBroadcast pins the compacting transient state on
// the wire: status frames while the compaction holds the slot, and a
// compacting field in the connect-time snapshot. A client that cannot
// see the compaction assumes idle mid-compact — the exact assumption
// that routed sends into the steer-panic path.
func TestCompactStatusBroadcast(t *testing.T) {
	provider := &blockSummaryProvider{freeAt: 1, release: make(chan struct{}), blocked: make(chan struct{})}
	t.Cleanup(func() { close(provider.release) })
	session, _, base := newTestManagerServer(t, provider)

	// One turn of context, then settle.
	if err := postJSON(t, base.URL, api(session, "/send"), map[string]string{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return !session.Running() })

	// Connect the subscriber FIRST, in the main goroutine: once
	// http.Get returns, the hub has registered the client and flushed
	// the connect snapshot (the response headers travel with the first
	// write) — the compacting status broadcast cannot race the
	// registration.
	res, cancel := sseGet(t, base.URL+api(session, "/stream"))
	defer cancel()
	defer res.Body.Close()
	br := bufio.NewReader(res.Body)

	// Trigger the compaction; its summary blocks.
	if err := postJSON(t, base.URL, api(session, "/compact"), map[string]string{}); err != nil {
		t.Fatal(err)
	}

	// The compacting:true transition arrives as a live status frame
	// (the client was registered before the compact started).
	sawSnapshot := false
	compactingSeen := false
	deadline := time.Now().Add(10 * time.Second)
	for !compactingSeen && time.Now().Before(deadline) {
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
		switch f.Kind {
		case "snapshot":
			// The subscriber connected while idle — its connect snapshot
			// must NOT report compacting (the compact has not started).
			sawSnapshot = true
			if f.Compacting != nil && *f.Compacting {
				t.Fatal("connect snapshot reported compacting before the compact started")
			}
		case "status":
			if f.Compacting != nil && *f.Compacting {
				compactingSeen = true
			}
		}
	}
	if !compactingSeen {
		t.Fatal("no compacting:true status frame within 10s")
	}
	if !sawSnapshot {
		t.Fatal("no snapshot frame before the compacting status")
	}

	// Release and wait for the end-of-compact notice broadcast. The
	// summary must have PARKED first: the compacting:true status
	// precedes the provider call, and releasing before the call
	// captures the channel would park it on the replacement forever.
	select {
	case <-provider.blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("the summary never parked on the release channel")
	}
	provider.mu.Lock()
	release := provider.release
	provider.release = make(chan struct{})
	provider.mu.Unlock()
	close(release)
	// The end state arrives either as the Notice broadcast (status) or
	// already folded into a fresh connect's snapshot — either proves
	// compacting is no longer held.
	readFrames(t, base.URL+api(session, "/stream"), func(f Frame) bool {
		return (f.Kind == "status" || f.Kind == "snapshot") && f.Compacting != nil && !*f.Compacting
	})
}

// TestEventRecordCausalOrder pins the wire ordering contract: for each
// item, its completed/failed event precedes its record frame. The run
// emits events before committing, but the hub's select chose channels
// randomly — a record could overtake its own completed event, and a
// client would re-create a live item the record already closed. The
// hub now consumes the single ordered stream, so the interleave is
// structural (seq total order): the strengthened assertions below pin
// the full frame sequence — user.input record → running:true status →
// the run's events and records → terminal running:false status, with
// nothing after the terminal. (Driven via session.Send, not POST
// /send: handleSend's ClearNotices broadcasts an out-of-band status
// frame whose position is legitimately racy — clients must tolerate
// it, but this test pins the stream's own order.)
func TestEventRecordCausalOrder(t *testing.T) {
	session, _, base := newTestServer(t, [][]model.Event{{model.TextDeltaEvent{Text: "a1"}, model.MessageDoneEvent{}}})

	res, cancel := sseGet(t, base.URL+api(session, "/stream"))
	defer cancel()
	defer res.Body.Close()

	// Send while the subscriber is connected; read frames until the
	// run settles with both assistant records delivered.
	if err := session.Send("q"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(res.Body)
	var frames []Frame
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var f Frame
		if err := json.Unmarshal([]byte(strings.TrimRight(line[6:], "\n")), &f); err != nil {
			t.Fatalf("frame: %v", err)
		}
		frames = append(frames, f)
		assistantRecords := 0
		for _, g := range frames {
			if g.Kind == "record" && g.Record != nil {
				var r struct {
					T string `json:"t"`
				}
				_ = json.Unmarshal(g.Record, &r)
				if r.T == "assistant.message" {
					assistantRecords++
				}
			}
		}
		if f.Kind == "status" && f.Running != nil && !*f.Running && assistantRecords >= 1 {
			break
		}
	}

	// For every item: its started event and all its deltas precede its
	// assistant.message record. (completed is emitted AFTER the commit
	// by design — no order constraint on it; the client is idempotent
	// for events trailing their records, which store_check pins.)
	indexOf := func(pred func(Frame) bool) int {
		for i, f := range frames {
			if pred(f) {
				return i
			}
		}
		return -1
	}
	recordOf := func(item string) int {
		return indexOf(func(f Frame) bool {
			if f.Kind != "record" || f.Record == nil {
				return false
			}
			var r struct {
				Item string `json:"item"`
				T    string `json:"t"`
			}
			_ = json.Unmarshal(f.Record, &r)
			return r.T == "assistant.message" && r.Item == item
		})
	}
	checkedStreams, checkedCompleted := 0, 0
	for fi, f := range frames {
		if f.Kind != "event" || f.Item == "" {
			continue
		}
		switch f.Type {
		case "assistant_message_started", "text_delta", "reasoning_delta":
			rec := recordOf(f.Item)
			if rec == -1 {
				continue // a live item that never completed (interrupted run)
			}
			if fi > rec {
				t.Fatalf("item %s: %s event (%d) trails its record (%d)", f.Item, f.Type, fi, rec)
			}
			checkedStreams++
		case "assistant_message_completed", "assistant_message_failed":
			if recordOf(f.Item) == -1 {
				t.Fatalf("item %s: completed event seen but no record", f.Item)
			}
			checkedCompleted++
		}
	}
	if checkedStreams == 0 || checkedCompleted == 0 {
		for i, f := range frames {
			t.Logf("frame[%d] kind=%s type=%s item=%s", i, f.Kind, f.Type, f.Item)
		}
		t.Fatalf("insufficient events: %d stream events, %d completed", checkedStreams, checkedCompleted)
	}

	// Strengthened (step 3): the stream's seq total order makes the
	// frame interleave structural — pin the transitions, not just the
	// per-item pairs.
	idxFirstEvent, idxUserInput, idxRunningTrue, idxRunningFalse, idxLastAssistantRecord := -1, -1, -1, -1, -1
	for fi, f := range frames {
		switch {
		case f.Kind == "record" && f.Record != nil:
			var r struct {
				Item string `json:"item"`
				T    string `json:"t"`
			}
			_ = json.Unmarshal(f.Record, &r)
			switch r.T {
			case "user.input":
				if idxUserInput == -1 {
					idxUserInput = fi
				}
			case "assistant.message":
				idxLastAssistantRecord = fi
			}
		case f.Kind == "event" && idxFirstEvent == -1:
			idxFirstEvent = fi
		case f.Kind == "status":
			if f.Running != nil {
				if *f.Running && idxRunningTrue == -1 {
					idxRunningTrue = fi
				}
				if !*f.Running && idxRunningFalse == -1 {
					idxRunningFalse = fi
				}
			}
		}
	}
	if idxUserInput == -1 || idxRunningTrue == -1 || idxRunningFalse == -1 || idxLastAssistantRecord == -1 || idxFirstEvent == -1 {
		t.Fatalf("missing frames: user.input=%d running:true@%d first-event@%d assistant-record=%d running:false@%d",
			idxUserInput, idxRunningTrue, idxFirstEvent, idxLastAssistantRecord, idxRunningFalse)
	}
	if idxUserInput > idxRunningTrue {
		t.Fatalf("user.input record (%d) must precede the running:true status (%d)", idxUserInput, idxRunningTrue)
	}
	if idxRunningTrue > idxFirstEvent {
		t.Fatalf("running:true status (%d) must precede the run's events (%d)", idxRunningTrue, idxFirstEvent)
	}
	if idxRunningFalse < idxLastAssistantRecord {
		t.Fatalf("terminal running:false status (%d) must follow the last assistant record (%d)", idxRunningFalse, idxLastAssistantRecord)
	}
	// The terminal status is the run's last word: no record or event
	// frame may trail it.
	for fi := idxRunningFalse + 1; fi < len(frames); fi++ {
		f := frames[fi]
		if f.Kind == "record" || f.Kind == "event" {
			t.Fatalf("frame after terminal status: [%d] kind=%s type=%s item=%s", fi, f.Kind, f.Type, f.Item)
		}
	}
}

// TestCompactionFailedEventWireAndNotice pins the degraded soft
// compaction's two web surfaces: the event maps onto a wire frame, and
// the hub caches it as the notice so /api/state carries it to pages
// that reconnect later.
func TestCompactionFailedEventWireAndNotice(t *testing.T) {
	provider := &webFakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "x"}, model.MessageDoneEvent{}},
	}}
	session, srv, base := newTestManagerServer(t, provider)

	// The wire frame.
	frame, ok := encodeEventFrame(model.CompactionFailedEvent{ItemID: "x1", Reason: "summary was empty"})
	if !ok || frame.Type != "compaction_failed" || frame.Item != "x1" || frame.Text != "summary was empty" {
		t.Fatalf("encodeEventFrame = %+v ok=%v", frame, ok)
	}

	// Precondition: no notice yet.
	res, err := http.Get(base.URL + api(session, "/state")) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Notice string `json:"notice"`
	}
	_ = json.NewDecoder(res.Body).Decode(&st)
	res.Body.Close()
	if st.Notice != "" {
		t.Fatalf("precondition: notice = %q, want empty", st.Notice)
	}

	// Feed the event the way the events channel would; /api/state must
	// then carry it (pages that reconnect later still see it).
	lease, err := srv.manager.pool.Acquire(session.ID())
	if err != nil {
		t.Fatal(err)
	}
	srv.manager.hubOf(session.ID()).handleEvent(model.CompactionFailedEvent{ItemID: "x1", Reason: "summary was empty"}, 1)
	lease.Release()

	res2, err := http.Get(base.URL + api(session, "/state")) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var st2 struct {
		Notice string `json:"notice"`
	}
	_ = json.NewDecoder(res2.Body).Decode(&st2)
	res2.Body.Close()
	if !strings.Contains(st2.Notice, "compaction failed") || !strings.Contains(st2.Notice, "summary was empty") {
		t.Fatalf("notice = %q, want the compaction failure cached", st2.Notice)
	}
}

// TestNoticeClearedByNextSend pins the notice lifecycle: a soft
// compaction failure surfaces on the status frame and /api/state, and
// the user's next accepted Send clears it (notices describe a past
// event; a new action supersedes them) — it never sticks indefinitely.
func TestNoticeClearedByNextSend(t *testing.T) {
	provider := &webFakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "a1"}, model.MessageDoneEvent{}},
		{model.TextDeltaEvent{Text: "a2"}, model.MessageDoneEvent{}},
	}}
	session, srv, base := newTestManagerServer(t, provider)
	if err := postJSON(t, base.URL, api(session, "/send"), map[string]string{"text": "q1"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return !session.Running() })

	// Feed a compaction failure the way the events channel would.
	lease, err := srv.manager.pool.Acquire(session.ID())
	if err != nil {
		t.Fatal(err)
	}
	srv.manager.hubOf(session.ID()).handleEvent(model.CompactionFailedEvent{ItemID: "x1", Reason: "summary was empty"}, 1)
	lease.Release()

	// Visible on /api/state.
	res, err := http.Get(base.URL + api(session, "/state")) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Notice string `json:"notice"`
	}
	_ = json.NewDecoder(res.Body).Decode(&st)
	res.Body.Close()
	if !strings.Contains(st.Notice, "compaction failed") {
		t.Fatalf("notice after the event = %q, want the failure", st.Notice)
	}

	// The next accepted Send clears it — observed from a subscriber
	// attached BEFORE the send. The reading happens on one connection
	// with a manual loop (readFrames' helper hides the per-line
	// interleaving this needs): snapshot first, then the send, then
	// frames until a status carrying an empty notice.
	streamRes, streamCancel := sseGet(t, base.URL+api(session, "/stream"))
	defer streamCancel()
	defer streamRes.Body.Close()
	br := bufio.NewReader(streamRes.Body)
	readLine := func() (string, error) {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return "", err
			}
			if strings.HasPrefix(line, "data: ") {
				return strings.TrimRight(line[6:], "\n"), nil
			}
		}
	}
	// The snapshot lands first; consume it.
	if _, err := readLine(); err != nil {
		t.Fatal(err)
	}

	sendErr := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		sendErr <- postJSONRaw(t, base.URL, api(session, "/send"), `{"text":"q2"}`)
	}()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-sendErr:
			if err != nil {
				t.Fatalf("second send: %v", err)
			}
			continue
		case <-deadline:
			t.Fatal("no notice-empty status frame within 10s of the next Send")
		default:
		}
		data, err := readLine()
		if err != nil {
			t.Fatal(err)
		}
		var f Frame
		if err := json.Unmarshal([]byte(data), &f); err != nil {
			t.Fatalf("frame: %v (%s)", err, data)
		}
		if f.Kind == "status" && f.Notice == "" {
			break
		}
	}
	// Drain any pending send result.
	select {
	case err := <-sendErr:
		if err != nil {
			t.Fatal(err)
		}
	default:
	}
	waitFor(t, 5*time.Second, func() bool { return !session.Running() })

	// And /api/state agrees.
	res2, err := http.Get(base.URL + api(session, "/state")) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var st2 struct {
		Notice string `json:"notice"`
	}
	_ = json.NewDecoder(res2.Body).Decode(&st2)
	res2.Body.Close()
	if st2.Notice != "" {
		t.Fatalf("notice after the next Send = %q, want empty", st2.Notice)
	}
}

// TestCompactionEventsBroadcastImmediately pins the folded-in behavior
// of the notice lifecycle: compaction_failed and compaction_completed
// push their status frames INSIDE the event handler — a connected page
// sees the notice the moment the event lands (not at the run's settle,
// and not as a bare event frame).
func TestCompactionEventsBroadcastImmediately(t *testing.T) {
	provider := &webFakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "x"}, model.MessageDoneEvent{}},
	}}
	session, srv, base := newTestManagerServer(t, provider)

	streamCtx, cancelStream := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStream()
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, base.URL+api(session, "/stream"), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	br := bufio.NewReader(res.Body)
	// The request context bounds the whole read: a regression that
	// makes the status broadcast go missing manifests as SILENCE — the
	// context fires, ReadString errors, the assertion path runs — not
	// a hang to the package timeout.
	nextFrame := func() Frame {
		t.Helper()
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatal(err)
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
	// Consume the connect-time snapshot.
	if f := nextFrame(); f.Kind != "snapshot" {
		t.Fatalf("first frame = %s, want snapshot", f.Kind)
	}

	// No run is active: feed the events the way the events channel
	// would. The failure must arrive as a STATUS frame carrying the
	// notice — not as a bare event frame.
	lease, err := srv.manager.pool.Acquire(session.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	srv.manager.hubOf(session.ID()).handleEvent(model.CompactionFailedEvent{ItemID: "x1", Reason: "summary was empty"}, 1)
	f := nextFrame()
	if f.Kind == "event" {
		t.Fatal("the failure arrived as a bare event frame — the status broadcast is missing")
	}
	if f.Kind != "status" || !strings.Contains(f.Notice, "compaction failed") {
		t.Fatalf("frame after the failure = kind %s notice %q, want a status frame carrying the failure", f.Kind, f.Notice)
	}

	// And the completion clears it — also immediately. (The failure's
	// own event frame is still queued ahead; skip event frames.)
	srv.manager.hubOf(session.ID()).handleEvent(model.CompactionCompletedEvent{ItemID: "x1"}, 1)
	for {
		f = nextFrame()
		if f.Kind == "event" {
			continue
		}
		break
	}
	if f.Kind != "status" || f.Notice != "" {
		t.Fatalf("frame after the completion = kind %s notice %q, want a status frame with an empty notice", f.Kind, f.Notice)
	}
}

// TestManualCompactLifecycleEvents pins N3: a manual /compact emits the
// same CompactionStarted/Completed transient events as auto-compaction,
// and they reach SSE clients as event frames through the session stream
// (this is what let handleCompact's polling be deleted).
func TestManualCompactLifecycleEvents(t *testing.T) {
	// Turn 1: the context-building send; turn 2: the compaction summary.
	provider := &webFakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "answer"}, model.MessageDoneEvent{}},
		{model.TextDeltaEvent{Text: "summary"}, model.MessageDoneEvent{}},
	}}
	session, _, base := newTestManagerServer(t, provider)

	if err := session.Send("hello"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)

	// Connect first (registration is complete once http.Get returns),
	// then trigger — the lifecycle events cannot race the subscription.
	res, cancel := sseGet(t, base.URL+api(session, "/stream"))
	defer cancel()
	defer res.Body.Close()

	if err := postJSON(t, base.URL, api(session, "/compact"), map[string]string{}); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(res.Body)
	sawStarted, sawCompleted := false, false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
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
		if f.Kind != "event" {
			continue
		}
		switch f.Type {
		case "compaction_started":
			sawStarted = true
		case "compaction_completed":
			sawCompleted = true
		}
		if sawCompleted {
			break
		}
	}
	if !sawStarted || !sawCompleted {
		t.Fatalf("manual compact lifecycle events missing: started=%v completed=%v", sawStarted, sawCompleted)
	}
}
