package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

// steerFlowProvider holds the stream open so the test can steer mid-run.
type steerFlowProvider struct {
	gate chan struct{}
}

func (p *steerFlowProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	events := make(chan model.Event, 4)
	go func() {
		defer close(events)
		events <- model.TextDeltaEvent{Text: "answering"}
		<-p.gate
		events <- model.TextDeltaEvent{Text: "…"}
	}()
	return events, nil
}

// release unblocks any gated stream; idempotent.
func (p *steerFlowProvider) release() {
	select {
	case <-p.gate:
	default:
		close(p.gate)
	}
}

// The steer endpoints: queue (with identity), edit, withdraw, and the
// 409s for the idle/racing states. The queue surfaces in /api/state and
// the SSE stream, and the consumed record carries the same ItemID.
func TestSteerEndpoints(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	// A gated stream must never wedge the cleanup path: manager.Close
	// waits for every run to settle, and the gate outlives assertions.
	t.Cleanup(provider.release)
	session, server, base := newTestManagerServer(t, provider)

	// Idle: steer refuses with the stable code.
	res, _ := http.DefaultClient.Post(base.URL+api(session, "/steer"), "application/json", strings.NewReader(`{"text":"x"}`)) //nolint:noctx // test
	res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("idle steer: %d, want 409", res.StatusCode)
	}

	// Start a run, then steer twice through the API.
	if err := postJSON(t, base.URL, api(session, "/send"), map[string]string{"text": "go"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	steer := func(text string) string {
		resp, err := http.DefaultClient.Post(base.URL+api(session, "/steer"), "application/json", strings.NewReader(`{"text":"`+text+`"}`)) //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("steer %q: %d", text, resp.StatusCode)
		}
		var body struct {
			Item string `json:"item"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.Item
	}
	keep := steer("keep")
	drop := steer("drop")

	// The queue is visible in /api/state.
	stRes, err := http.DefaultClient.Get(base.URL + api(session, "/state")) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var st state
	_ = json.NewDecoder(stRes.Body).Decode(&st)
	stRes.Body.Close()
	if len(st.Steered) != 2 {
		t.Fatalf("state steered = %d, want 2", len(st.Steered))
	}

	// Withdraw one, edit the other.
	req, _ := http.NewRequest("DELETE", base.URL+api(session, "/steer?item=")+drop, nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 { //nolint:noctx // test
		t.Fatalf("withdraw: %v %d", err, resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	put, _ := http.NewRequest("PUT", base.URL+api(session, "/steer"), strings.NewReader(`{"item":"`+keep+`","text":"keep, edited"}`))
	put.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(put); err != nil || resp.StatusCode != 200 { //nolint:noctx // test
		t.Fatalf("update: %v %d", err, resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// A withdrawn id is gone.
	put2, _ := http.NewRequest("PUT", base.URL+api(session, "/steer"), strings.NewReader(`{"item":"`+drop+`","text":"x"}`))
	put2.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(put2); err != nil || resp.StatusCode != 404 { //nolint:noctx // test
		t.Fatalf("update withdrawn: %v %d", err, resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Finish the run and collect the record stream: the kept steer
	// lands as user.input with the SAME item id; the dropped one never
	// lands.
	close(provider.gate)
	c := openStream(t, base.URL, session, "")
	defer c.close()
	deadline := time.After(10 * time.Second)
	var keepLanded, dropLanded bool
	for !keepLanded {
		select {
		case f := <-c.frames:
			switch f["kind"] {
			case "snapshot":
				for _, raw := range f["records"].([]any) {
					r := raw.(map[string]any)
					if r["item"] == keep && r["t"] == "user.input" && r["content"] == "keep, edited" {
						keepLanded = true
					}
					if r["item"] == drop {
						dropLanded = true
					}
				}
			case "record":
				r := f["r"].(map[string]any)
				if r["item"] == keep && r["t"] == "user.input" && r["content"] == "keep, edited" {
					keepLanded = true
				}
				if r["item"] == drop {
					dropLanded = true
				}
			}
		case <-deadline:
			t.Fatalf("timeout: keepLanded=%v", keepLanded)
		}
	}
	if dropLanded {
		t.Fatal("withdrawn steer landed as a record")
	}
	_ = server
}
