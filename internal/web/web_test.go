package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage"
	"github.com/rexzhao/simple-agent/storage/memory"
)

// webFakeProvider is a scriptable provider for the web integration
// tests: each turn streams one canned event sequence.
type webFakeProvider struct {
	turns [][]model.Event
}

func (p *webFakeProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	if len(p.turns) == 0 {
		return nil, errors.New("unexpected model request")
	}
	turn := p.turns[0]
	p.turns = p.turns[1:]
	events := make(chan model.Event, len(turn))
	for _, event := range turn {
		events <- event
	}
	close(events)
	return events, nil
}

// newTestServer builds a live manager-backed server over a deterministic
// fake run: a registered project (temp dir), a memory store, and one
// session created under it. The memory store's HasSession/OpenIn work
// without materialization, so sessions with no records still resolve.
func newTestServer(t *testing.T, turns [][]model.Event) (*agent.Session, *Server, *httptest.Server) {
	t.Helper()
	return newTestManagerServer(t, &webFakeProvider{turns: turns})
}

// newTestManagerServerOpts is newTestManagerServer with ServerOptions
// (e.g. WithStaticDir for route-level tests).
func newTestManagerServerOpts(t *testing.T, provider model.Provider, opts ...ServerOption) (*agent.Session, *Server, *httptest.Server) {
	t.Helper()
	if provider == nil {
		provider = &webFakeProvider{}
	}
	session, server, httpServer := newTestManagerServerInner(t, provider)
	// Rebuild only the HTTP layer over the same manager when options
	// are present: the session's hub is the manager's, so /api routes
	// keep working.
	if len(opts) > 0 {
		optsServer := New(server.manager, opts...)
		httpServer2 := httptest.NewServer(optsServer.Handler())
		t.Cleanup(func() {
			httpServer2.Close()
			optsServer.Close()
		})
		return session, optsServer, httpServer2
	}
	return session, server, httpServer
}

// newTestManagerServer wires a full manager: server root + registry with
// one project, a shared store, and the per-project agent factory.
func newTestManagerServer(t *testing.T, provider model.Provider) (*agent.Session, *Server, *httptest.Server) {
	t.Helper()
	return newTestManagerServerInner(t, provider)
}

func newTestManagerServerInner(t *testing.T, provider model.Provider) (*agent.Session, *Server, *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	projectDir := t.TempDir()
	registry := &projects.Registry{Projects: []projects.Entry{
		{Path: projectDir, Added: time.Now().UTC()},
	}}
	if err := registry.Save(projects.FilePath(root)); err != nil {
		t.Fatal(err)
	}
	store := memory.OpenStore()
	factory := func(projectCwd string) (*agent.Agent, error) {
		return agent.New(agent.Options{
			ModelResolver: agent.NewStaticResolver(provider, nil, 0, 0, ""),
			Storage:       store,
			CWD:           projectCwd,
		})
	}
	manager, err := NewManager(ManagerOptions{
		ServerRoot: root,
		Store:      store,
		AgentFor:   factory,
	})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := factory(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	// Create the session through the factory's agent (so the header
	// content matches it), materialize the header, and reopen it
	// THROUGH the manager — the returned object is the manager's, so
	// direct sends notify the hub exactly like HTTP requests. The ns is
	// the manager's registry-derived one (memory store ownership check).
	created, err := ag.NewSession(projects.ProjectID(projectDir), testSessionMeta)
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
	t.Cleanup(func() {
		httpServer.Close()
		server.Close()
		_ = session.Close()
	})
	return session, server, httpServer
}

// sseClient collects frames from the stream endpoint.
type sseClient struct {
	url    string
	resp   *http.Response
	frames chan map[string]any
	done   chan struct{}
}

// close aborts the connection — the SSE handler runs until the client
// goes away, and httptest.Server.Close waits for outstanding requests.
func (c *sseClient) close() {
	c.resp.Body.Close()
	<-c.done
}

func openStream(t *testing.T, base string, session *agent.Session, from string) *sseClient {
	t.Helper()
	url := base + api(session, "/stream")
	if from != "" {
		url += "?from=" + from
	}
	resp, err := http.DefaultClient.Get(url) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("stream status %d", resp.StatusCode)
	}
	c := &sseClient{url: url, resp: resp, frames: make(chan map[string]any, 1024), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		defer resp.Body.Close()
		// SSE wire format: "data: <json>\n\n" per frame.
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			payload, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var f map[string]any
			if err := json.Unmarshal([]byte(payload), &f); err != nil {
				return
			}
			c.frames <- f
		}
	}()
	return c
}

// nextFrame waits for one frame.
func (c *sseClient) next(t *testing.T, timeout time.Duration) map[string]any {
	t.Helper()
	select {
	case f := <-c.frames:
		return f
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a frame")
		return nil
	}
}

// collectRecords reads frames until a status frame shows running=false,
// returning the record sequence (snapshot + live records).
func collectRecords(t *testing.T, c *sseClient, settle time.Duration) []map[string]any {
	t.Helper()
	var records []map[string]any
	deadline := time.After(settle)
	for {
		select {
		case f := <-c.frames:
			switch f["kind"] {
			case "snapshot":
				for _, r := range f["records"].([]any) {
					records = append(records, r.(map[string]any))
				}
			case "record":
				records = append(records, f["r"].(map[string]any))
			case "status":
				if running, ok := f["running"].(bool); ok && !running && len(records) > 0 {
					return records
				}
			}
		case <-deadline:
			t.Fatalf("timed out; got %d records", len(records))
		}
	}
}

// INV1: the SSE record sequence equals what a deterministic run commits.
func TestStreamMatchesSessionRecords(t *testing.T) {
	turn := []model.Event{
		model.ReasoningDeltaEvent{Text: "thinking"},
		model.TextDeltaEvent{Text: "hello "},
		model.TextDeltaEvent{Text: "world"},
	}
	session, _, base := newTestServer(t, [][]model.Event{turn, turn})

	c := openStream(t, base.URL, session, "")
	defer c.close()
	snap := c.next(t, 2*time.Second)
	if snap["kind"] != "snapshot" {
		t.Fatalf("first frame is %v, want snapshot", snap["kind"])
	}
	// A serve-side session materializes its header up front (the lazy
	// "first record commits session.created" is a CLI-session
	// behavior): exactly the header, nothing else.
	initial := snap["records"].([]any)
	if len(initial) != 1 || initial[0].(map[string]any)["t"] != "session.created" {
		t.Fatalf("fresh serve session snapshot = %v, want only the header", initial)
	}

	// Drive one run and collect its live records. The header was in the
	// snapshot (materialized at session creation); the live stream
	// carries the run's two records.
	if err := postJSON(t, base.URL, api(session, "/send"), map[string]string{"text": "hi"}); err != nil {
		t.Fatal(err)
	}
	records := collectRecords(t, c, 5*time.Second)
	types := []string{}
	for _, r := range records {
		types = append(types, r["t"].(string))
	}
	// user.input, assistant.message — in order, exactly once each; with
	// the snapshot's header the full sequence is the session's records.
	if len(records) != 2 || types[0] != "user.input" || types[1] != "assistant.message" {
		t.Fatalf("record types = %v", types)
	}
	if content := records[1]["content"]; content != "hello world" {
		t.Fatalf("assistant content = %v", content)
	}
}

// INV4: while a run is active, further sends get a stable 409 code. The
// gated provider keeps the run deterministically in flight.
func TestConcurrentSendOneWins(t *testing.T) {
	gated := &gatedStream{gate: make(chan struct{})}
	session, _, httpServer := newTestManagerServer(t, gated)

	if err := postJSON(t, httpServer.URL, api(session, "/send"), map[string]string{"text": "first"}); err != nil {
		t.Fatal(err)
	}
	// The run is gated mid-stream: a second send must be refused with
	// the stable code, not an error string.
	resp, err := http.DefaultClient.Post(httpServer.URL+api(session, "/send"), "application/json", strings.NewReader(`{"text":"second"}`)) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second send: status %d, want 409", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "run_active" {
		t.Fatalf("conflict code = %q, want run_active", body["error"])
	}

	close(gated.gate)
	waitSettled(t, session)
}

// INV2: reconnect with from= resumes with zero retransmission; an
// unknown from falls back to the window with a fresh anchor.
func TestReconnectResumeAndFallback(t *testing.T) {
	turn := []model.Event{model.TextDeltaEvent{Text: "one"}}
	session, _, base := newTestServer(t, [][]model.Event{turn, turn})

	c := openStream(t, base.URL, session, "")
	defer c.close()
	snap0 := c.next(t, 2*time.Second) // snapshot: the materialized header
	if err := postJSON(t, base.URL, api(session, "/send"), map[string]string{"text": "first"}); err != nil {
		t.Fatal(err)
	}
	records := collectRecords(t, c, 5*time.Second)

	// The client's head: recompute the chain independently (the
	// cross-implementation check the digest exists for) — snapshot
	// records first, then the live ones, in order.
	head := chainGenesis(session.ID())
	for _, r := range recordsJSON(t, snapshotRecords(t, snap0)) {
		head = chainStep(head, r)
	}
	for _, r := range recordsJSON(t, records) {
		head = chainStep(head, r)
	}

	// Resume: the server's snapshot must anchor exactly at our head and
	// replay nothing.
	if err := postJSON(t, base.URL, api(session, "/send"), map[string]string{"text": "second"}); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	c2 := openStream(t, base.URL, session, head)
	snap := c2.next(t, 2*time.Second)
	defer c2.close()
	if snap["kind"] != "snapshot" || snap["resumed"] != true {
		t.Fatalf("resume snapshot = %v", snap)
	}
	// The resume replays exactly the records committed after our head:
	// the second run's user input and its answer — nothing more.
	replays := snap["records"].([]any)
	if len(replays) != 2 {
		t.Fatalf("resume replayed %d records, want the 2 after our head", len(replays))
	}
	if replays[0].(map[string]any)["t"] != "user.input" || replays[1].(map[string]any)["t"] != "assistant.message" {
		t.Fatalf("replayed types = %v", replays)
	}
	if replays[0].(map[string]any)["content"] != "second" {
		t.Fatalf("replayed content = %v", replays[0])
	}
	if snap["anchor"] != head {
		t.Fatalf("anchor = %v, want our head", snap["anchor"])
	}

	// Fallback: an unknown digest replays the trailing window.
	c3 := openStream(t, base.URL, session, strings.Repeat("ab", 32))
	snap3 := c3.next(t, 2*time.Second)
	defer c3.close()
	if snap3["resumed"] == true {
		t.Fatal("unknown digest resumed")
	}
	recs3 := snap3["records"].([]any)
	if len(recs3) == 0 {
		t.Fatal("fallback window is empty")
	}
}

// INV3: a mid-stream connection recovers the in-flight item through the
// snapshot frame.
func TestMidstreamConnectSeesStreaming(t *testing.T) {
	gated := &gatedStream{gate: make(chan struct{})}
	session, _, httpServer := newTestManagerServer(t, gated)

	if err := session.Send("hi"); err != nil {
		t.Fatal(err)
	}
	// Let the streaming slot populate.
	time.Sleep(100 * time.Millisecond)

	c := openStream(t, httpServer.URL, session, "")
	defer c.close()
	snap := c.next(t, 2*time.Second)
	streaming, ok := snap["streaming"].(map[string]any)
	if !ok || streaming["item"] == "" {
		t.Fatalf("snapshot carries no in-flight item: %v", snap["streaming"])
	}
	// The wire DTO carries the accumulated content, not the internal
	// StreamingItem shape (that bug left the recovered row empty).
	if streaming["content"] != "partial" {
		t.Fatalf("in-flight content = %v, want partial", streaming["content"])
	}
	close(gated.gate)
	// Let the run finish before cleanup tears the server down.
	waitSettled(t, session)
}

// INV6: two concurrent clients see identical record sequences.
func TestMultiClientConsistency(t *testing.T) {
	turn := []model.Event{model.TextDeltaEvent{Text: "answer"}}
	session, _, base := newTestServer(t, [][]model.Event{turn})

	c1 := openStream(t, base.URL, session, "")
	c2 := openStream(t, base.URL, session, "")
	defer c1.close()
	defer c2.close()
	c1.next(t, 2*time.Second)
	c2.next(t, 2*time.Second)

	if err := postJSON(t, base.URL, api(session, "/send"), map[string]string{"text": "hi"}); err != nil {
		t.Fatal(err)
	}
	r1 := collectRecords(t, c1, 5*time.Second)
	r2 := collectRecords(t, c2, 5*time.Second)
	if len(r1) != len(r2) {
		t.Fatalf("clients diverged: %d vs %d records", len(r1), len(r2))
	}
	for i := range r1 {
		if r1[i]["item"] != r2[i]["item"] {
			t.Fatalf("record %d differs: %v vs %v", i, r1[i]["item"], r2[i]["item"])
		}
	}
}

// INV5: a client that never reads does not stall the run.
func TestSlowClientDoesNotStallRun(t *testing.T) {
	turn := []model.Event{model.TextDeltaEvent{Text: "answer"}}
	session, _, base := newTestServer(t, [][]model.Event{turn, turn, turn, turn, turn})

	// Open a client and never read its body.
	resp, err := http.DefaultClient.Get(base.URL + api(session, "/stream")) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	for i := 0; i < 3; i++ {
		if err := postJSON(t, base.URL, api(session, "/send"), map[string]string{"text": fmt.Sprintf("m%d", i)}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		waitSettled(t, session)
	}
	// The run completed; the slow client's connection was closed by
	// overflow or sits ignored — either way the agent proceeded.
	if n := len(session.SnapshotStream().Records); n < 4 {
		t.Fatalf("records stalled at %d", n)
	}
}

// The digest chain: a Go reimplementation of the page's algorithm must
// produce the server's head — pinning the two implementations to the
// same spec, including the drift-prone samples (escapes, invalid UTF-8,
// empty fields).
func TestChainCrossImplementation(t *testing.T) {
	turn := []model.Event{model.TextDeltaEvent{Text: "x"}}
	session, _, base := newTestServer(t, [][]model.Event{turn})

	if err := postJSON(t, base.URL, api(session, "/send"), map[string]string{"text": "hi"}); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)

	// The server's state digest.
	res, err := http.DefaultClient.Get(base.URL + api(session, "/state")) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var st state
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}

	// An independent fold over the session's records — the same walk the
	// page performs over its store.
	head := chainGenesis(session.ID())
	for _, r := range session.SnapshotStream().Records {
		head = chainStep(head, r)
	}
	if head != st.Digest {
		t.Fatalf("independent fold %s != server head %s", head, st.Digest)
	}
}

// Drift-prone samples for the chain: escapes and invalid UTF-8 must fold
// identically on both sides (records are normalized at the append
// boundary, so the chain sees valid UTF-8 only).
func TestChainTuples(t *testing.T) {
	genesis := chainGenesis("s1")
	r := storageRecord("i1", "user.input", "plain <html> & \u2028 中文", "", false)
	h1 := chainStep(genesis, r)
	if h1 == genesis {
		t.Fatal("chain did not advance")
	}
	// Same tuple, different field boundaries must not collide
	// (length-prefix injectivity).
	a := storageRecord("i", "t", "ab", "", false)
	b := storageRecord("i", "t", "a", "", false)
	if chainStep(genesis, a) == chainStep(genesis, b) {
		t.Fatal("field-boundary collision in the chain input")
	}
}

// --- helpers -----------------------------------------------------------

type gatedStream struct {
	gate chan struct{}
}

func (g *gatedStream) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	events := make(chan model.Event)
	go func() {
		defer close(events)
		events <- model.TextDeltaEvent{Text: "partial"}
		<-g.gate
		events <- model.TextDeltaEvent{Text: " done"}
	}()
	return events, nil
}

func storageRecord(item, typ, content, reasoning string, isErr bool) storage.Record {
	return storage.Record{ItemID: item, Type: typ, Content: content, Reasoning: reasoning, IsError: isErr}
}

// api builds a session-scoped route path.
func api(session *agent.Session, suffix string) string {
	return "/api/s/" + session.ID() + suffix
}

func postJSON(t *testing.T, base, path string, body any) error {
	t.Helper()
	data, _ := json.Marshal(body)
	return postJSONRaw(t, base, path, string(data))
}

func postJSONRaw(t *testing.T, base, path, body string) error {
	t.Helper()
	resp, err := http.DefaultClient.Post(base+path, "application/json", strings.NewReader(body)) //nolint:noctx // test
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %d", path, resp.StatusCode)
	}
	return nil
}

func waitSettled(t *testing.T, session *agent.Session) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for session.Running() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the run to settle")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func recordsJSON(t *testing.T, maps []map[string]any) []storage.Record {
	t.Helper()
	out := make([]storage.Record, 0, len(maps))
	for _, m := range maps {
		data, _ := json.Marshal(m)
		var r storage.Record
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// The truncated-window contract the page's self-check depends on:
// folding the windowed records onto the snapshot's anchor must yield
// the server's chain head. A page that folds from genesis instead of
// the anchor drifts exactly when the window truncates (more=true) —
// the bug the first real browser session caught.
func TestWindowAnchorFoldsToHead(t *testing.T) {
	turns := make([][]model.Event, 130)
	for i := range turns {
		turns[i] = []model.Event{model.TextDeltaEvent{Text: "answer"}}
	}
	session, _, base := newTestServer(t, turns)
	for i := 0; i < 130; i++ {
		if err := postJSON(t, base.URL, api(session, "/send"), map[string]string{"text": "m"}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		waitSettled(t, session)
	}

	res, err := http.DefaultClient.Get(base.URL + api(session, "/stream?window=100")) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	scanner := bufio.NewScanner(res.Body)
	if !scanner.Scan() {
		t.Fatal("no snapshot frame")
	}
	line := scanner.Text()
	payload, ok := strings.CutPrefix(line, "data: ")
	if !ok {
		t.Fatalf("not a data line: %q", line)
	}
	var snap Frame
	if err := json.Unmarshal([]byte(payload), &snap); err != nil {
		t.Fatal(err)
	}
	var windowed []storage.Record
	if err := json.Unmarshal(snap.Records, &windowed); err != nil {
		t.Fatal(err)
	}
	if snap.Total != 261 || len(windowed) != 100 || !snap.More {
		t.Fatalf("window = total %d, %d records, more=%v", snap.Total, len(windowed), snap.More)
	}

	// The page's fold: start from the anchor, fold the window.
	head := snap.Anchor
	for _, r := range windowed {
		head = chainStep(head, r)
	}
	if head != snap.Digest {
		t.Fatalf("fold(anchor, window) = %s, want server head %s", head, snap.Digest)
	}
}

// snapshotRecords extracts a snapshot frame's records as JSON maps.
func snapshotRecords(t *testing.T, frame map[string]any) []map[string]any {
	t.Helper()
	raw, ok := frame["records"].([]any)
	if !ok {
		t.Fatalf("snapshot has no records: %v", frame)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	return out
}
