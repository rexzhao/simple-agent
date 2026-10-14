package web

// Approval endpoint tests (B2 step 7a): the wire contract of
// POST /api/s/{id}/approval — guardAPI, session-level codes first,
// then the approval-level codes. The "pending approval exists" cases
// (200 / repeated 409) need the Ask state, which has no production
// trigger until 7b wires the policy hook; they land with 7b.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage/jsonl"
	"github.com/rexzhao/simple-agent/tools"
)

func approvalPost(base string, sessionID string, body string, headers map[string]string) (*http.Response, error) {
	req, _ := http.NewRequest("POST", base+"/api/s/"+sessionID+"/approval", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

// TestApprovalEndpointUnknownSession: an unregistered session gets
// the SESSION-level code first (unknown_session), never the
// approval-level ones.
func TestApprovalEndpointUnknownSession(t *testing.T) {
	_, _, base := newTestServer(t, nil)
	resp, err := approvalPost(base.URL, "s0000doesnotexist00", `{"item":"x","allow":true}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "unknown_session" {
		t.Fatalf("code = %q, want unknown_session (session-level first)", body["error"])
	}
}

// TestApprovalEndpointNoPending: a valid session with no pending
// approval → 404 approval_unknown.
func TestApprovalEndpointNoPending(t *testing.T) {
	session, _, base := newTestServer(t, nil)
	resp, err := approvalPost(base.URL, session.ID(), `{"item":"s0000000000000000000000001","allow":true}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "approval_unknown" {
		t.Fatalf("code = %q, want approval_unknown", body["error"])
	}
}

// TestApprovalEndpointBadRequests: malformed JSON and a missing item
// are plain-text 400s (the parse-layer boundary, §4).
func TestApprovalEndpointBadRequests(t *testing.T) {
	session, _, base := newTestServer(t, nil)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"bad json", `{not json`},
		{"missing item", `{"allow":true}`},
		{"empty item", `{"item":"  ","allow":true}`},
	} {
		resp, err := approvalPost(base.URL, session.ID(), tc.body, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", tc.name, resp.StatusCode)
		}
	}
}

// TestApprovalEndpointGuarded: the endpoint is a write — the CSRF
// guards apply (cross-origin simple POST is 403).
func TestApprovalEndpointGuarded(t *testing.T) {
	session, _, base := newTestServer(t, nil)
	resp, err := approvalPost(base.URL, session.ID(), `{"item":"x","allow":true}`, map[string]string{
		"Content-Type": "text/plain",
		"Origin":       "http://evil.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin simple POST: status = %d, want 403", resp.StatusCode)
	}
}

// newApprovalTestServer builds a manager whose agent asks for approval
// on every tool call (the 7b policy hook wiring — the production path
// to a pending state).
func newApprovalTestServer(t *testing.T) (*agent.Session, *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	projectDir := t.TempDir()
	if err := (&projects.Registry{Projects: []projects.Entry{
		{Path: projectDir, Added: time.Now().UTC()},
	}}).Save(projects.FilePath(root)); err != nil {
		t.Fatal(err)
	}
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &webFakeProvider{turns: [][]model.Event{
		{model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "c1", Name: "echo", Arguments: `{}`}}},
		{model.TextDeltaEvent{Text: "final"}, model.MessageDoneEvent{}},
	}}
	factory := func(projectCwd string) (*agent.Agent, error) {
		return agent.New(agent.Options{
			ModelResolver: agent.NewStaticResolver(provider, nil, 0, 0, ""),
			Storage:       store,
			CWD:           projectCwd,
			Tools: []agent.Tool{tools.Named(model.ToolDefinition{Name: "echo"}, tools.ExecutorFunc(func(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
				return model.ToolResult{Content: "tool ran"}, nil
			}))},
			Hooks: agent.Hooks{
				BeforeToolCall: func(ctx context.Context, call agent.ToolCallInfo) agent.ToolDecision {
					return agent.ToolDecision{Action: agent.ToolActionAsk}
				},
			},
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
	t.Cleanup(func() { manager.Close() })
	server := New(manager)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(func() { httpServer.Close(); server.Close() })

	session, err := manager.Session("")
	_ = session
	// Create a session through the factory's agent directly.
	ag, err := factory(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := ag.NewSession(projects.ProjectID(projectDir), testSessionMeta)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.MaterializeHeader(); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	return sess, httpServer
}

// waitForPendingApproval polls /api/state until the tool slot carries
// awaiting_approval, returning the item id.
func waitForPendingApproval(t *testing.T, base, sessionID string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base + "/api/s/" + sessionID + "/state") //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			Tool *struct {
				Item             string `json:"item"`
				AwaitingApproval bool   `json:"awaiting_approval"`
			} `json:"tool"`
		}
		err = json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if st.Tool != nil && st.Tool.AwaitingApproval {
			return st.Tool.Item
		}
		if time.Now().After(deadline) {
			t.Fatal("the approval never became pending")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestApprovalEndpointResolve: a pending approval resolves through
// the endpoint (200) and the session receives the decision (the tool
// executes); a second resolve of the same item gets 409
// approval_resolved.
func TestApprovalEndpointResolve(t *testing.T) {
	session, base := newApprovalTestServer(t)

	// Start a run that will ask for approval.
	resp, err := approvalPost(base.URL, session.ID(), `{"item":"x","allow":true}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() // unknown — fine, no pending yet

	// Drive the run through the send endpoint.
	sendResp, err := http.Post(base.URL+"/api/s/"+session.ID()+"/send", "application/json",
		strings.NewReader(`{"text":"use the tool"}`)) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	sendResp.Body.Close()
	if sendResp.StatusCode != http.StatusOK {
		t.Fatalf("send: %d", sendResp.StatusCode)
	}

	item := waitForPendingApproval(t, base.URL, session.ID())

	// Resolve: 200.
	resp2, err := approvalPost(base.URL, session.ID(), `{"item":"`+item+`","allow":true}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("resolve: %d, want 200", resp2.StatusCode)
	}
	// The approved tool actually executed: the run settles with the
	// full record set (created, input, assistant, tool.result,
	// assistant = 5) — the kill (approve wired to deny) still reaches 5
	// but the fourth record is an is_error, so also assert through the
	// endpoint-observable count AND check no notice appeared.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base.URL + "/api/s/" + session.ID() + "/state") //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			Count int `json:"count"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if st.Count >= 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run never settled after the approval (count = %d)", st.Count)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The tool.result carries the TOOL'S OUTPUT, not a denial: read it
	// through /history (the kill — approve wired to deny — lands an
	// is_error refusal instead of the execution result).
	// /history needs a before-anchor: use the current chain head from
	// /api/state.
	stResp, err := http.Get(base.URL + "/api/s/" + session.ID() + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var stFull struct {
		Digest string `json:"digest"`
	}
	if err := json.NewDecoder(stResp.Body).Decode(&stFull); err != nil {
		t.Fatal(err)
	}
	stResp.Body.Close()
	histResp, err := http.Get(base.URL + "/api/s/" + session.ID() + "/history?limit=10&before=" + stFull.Digest) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer histResp.Body.Close()
	var hist struct {
		Records []struct {
			Type    string `json:"t"`
			Content string `json:"content"`
			IsError bool   `json:"is_error"`
		} `json:"records"`
	}
	if err := json.NewDecoder(histResp.Body).Decode(&hist); err != nil {
		t.Fatalf("history decode: %v (status %d)", err, histResp.StatusCode)
	}
	toolRan := false
	for _, r := range hist.Records {
		if r.Type == "tool.result" && !r.IsError && r.Content == "tool ran" {
			toolRan = true
		}
	}
	if !toolRan {
		t.Fatal("the approved tool did not execute — its result is missing or is a denial")
	}

	// A second resolve of the same item: either 409 approval_resolved
	// (the entry is still marked resolved before the waiter drains) or
	// 404 approval_unknown (the waiter already drained and cleared
	// it) — both are typed, retry-tolerant answers; anything else is a
	// contract violation.
	resp3, err := approvalPost(base.URL, session.ID(), `{"item":"`+item+`","allow":true}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(resp3.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	switch resp3.StatusCode {
	case http.StatusConflict:
		if body["error"] != "approval_resolved" {
			t.Fatalf("second resolve 409 code: %q", body["error"])
		}
	case http.StatusNotFound:
		if body["error"] != "approval_unknown" {
			t.Fatalf("second resolve 404 code: %q", body["error"])
		}
	default:
		t.Fatalf("second resolve: %d %v, want 409 approval_resolved or 404 approval_unknown", resp3.StatusCode, body)
	}
}
