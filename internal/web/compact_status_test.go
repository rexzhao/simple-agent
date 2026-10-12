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

	// For every completed event, its item's record must come AFTER it.
	indexOf := func(kind, t, item string) int {
		for i, f := range frames {
			switch kind {
			case "event":
				if f.Kind == "event" && f.Type == t && f.Item == item {
					return i
				}
			case "record":
				if f.Kind == "record" && f.Record != nil {
					var r struct {
						Item string `json:"item"`
						T    string `json:"t"`
					}
					_ = json.Unmarshal(f.Record, &r)
					if r.T == "assistant.message" && r.Item == item {
						return i
					}
				}
			}
		}
		return -1
	}
	checked := 0
	for _, f := range frames {
		if f.Kind == "event" && (f.Type == "assistant_message_completed" || f.Type == "assistant_message_failed") {
			ev := indexOf("event", f.Type, f.Item)
			rec := indexOf("record", "assistant.message", f.Item)
			if rec == -1 {
				t.Fatalf("item %s: event seen but no record", f.Item)
			}
			if ev > rec {
				t.Fatalf("item %s: record frame (%d) precedes its completed event (%d)", f.Item, rec, ev)
			}
			checked++
		}
	}
	if checked == 0 {
		for i, f := range frames {
			t.Logf("frame[%d] kind=%s type=%s item=%s", i, f.Kind, f.Type, f.Item)
		}
		t.Fatal("no completed events seen")
	}
}
