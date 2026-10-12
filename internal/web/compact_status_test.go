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
		go func() {
			select {
			case <-p.release:
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
