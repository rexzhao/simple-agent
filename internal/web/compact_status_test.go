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
// deadline passes, returning the frames seen.
func readFrames(t *testing.T, url string, cond func(Frame) bool) []Frame {
	t.Helper()
	res, err := http.Get(url) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	br := bufio.NewReader(res.Body)
	var frames []Frame
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
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
	}
	t.Fatal("condition never met")
	return nil
}

// TestCompactStatusBroadcast pins the compacting transient state on
// the wire: status frames while the compaction holds the slot, and a
// compacting field in the connect-time snapshot. A client that cannot
// see the compaction assumes idle mid-compact — the exact assumption
// that routed sends into the steer-panic path.
func TestCompactStatusBroadcast(t *testing.T) {
	provider := &blockSummaryProvider{freeAt: 1, release: make(chan struct{})}
	t.Cleanup(func() { close(provider.release) })
	session, _, base := newTestManagerServer(t, provider)

	// One turn of context, then settle.
	if err := postJSON(t, base.URL, api(session, "/send"), map[string]string{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return !session.Running() })

	// A subscriber is watching before the compaction starts.
	frames := make(chan []Frame, 1)
	go func() {
		fs := readFrames(t, base.URL+api(session, "/stream"), func(f Frame) bool {
			return f.Kind == "status" && f.Compacting != nil && *f.Compacting
		})
		frames <- fs
	}()

	// Trigger the compaction; its summary blocks.
	if err := postJSON(t, base.URL, api(session, "/compact"), map[string]string{}); err != nil {
		t.Fatal(err)
	}

	select {
	case fs := <-frames:
		sawSnapshot := false
		for _, f := range fs {
			if f.Kind == "snapshot" {
				sawSnapshot = true
			}
		}
		if !sawSnapshot {
			t.Fatal("no snapshot frame before the compacting status")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no compacting:true status frame within 10s")
	}

	// Release and wait for the end-of-compact notice broadcast.
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
// hub now drains queued events before forwarding a record.
func TestEventRecordCausalOrder(t *testing.T) {
	session, _, base := newTestServer(t, [][]model.Event{{model.TextDeltaEvent{Text: "a1"}, model.MessageDoneEvent{}}})

	res, err := http.Get(base.URL + api(session, "/stream")) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	// Send while the subscriber is connected; read frames until the
	// run settles with both assistant records delivered.
	if err := postJSON(t, base.URL, api(session, "/send"), map[string]string{"text": "q"}); err != nil {
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
	entry, release, _, err := srv.manager.open(session.ID())
	if err != nil {
		t.Fatal(err)
	}
	entry.hub.handleEvent(model.CompactionFailedEvent{ItemID: "x1", Reason: "summary was empty"})
	release()

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
	entry, release, _, err := srv.manager.open(session.ID())
	if err != nil {
		t.Fatal(err)
	}
	entry.hub.handleEvent(model.CompactionFailedEvent{ItemID: "x1", Reason: "summary was empty"})
	release()

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
	streamRes, err := http.Get(base.URL + api(session, "/stream")) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
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
