package web

// POST /api/projects/{pid}/sessions endpoint tests (C4): the create
// path — unknown project, strict fields, shared validation, and
// success with stream connectivity (the table fast path).

import (
	"encoding/json"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/model"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/storage/jsonl"
)

// createTestServer builds a manager with one registered project and
// the model-list wired (the newServeManager shape, but in-process).
func createTestServer(t *testing.T) (*httptest.Server, string) {
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
					"fake-reasoner": {ID: "fake-reasoner", Type: config.ProviderTypeOpenAIChat,
						ReasoningConfig: config.ReasoningConfig{
							Parameter: "reasoning_effort", Default: "low",
							Levels: map[string]any{"low": "low", "high": "high"},
						}},
				},
			},
		},
	}
	resolver, err := config.NewModelResolver(cfg, func(config.ResolvedModel) (model.Provider, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerOptions{
		ServerRoot:    root,
		Store:         store,
		IdleWindow:    50 * time.Millisecond,
		SweepInterval: 20 * time.Millisecond,
		AgentFor: func(projectCwd string) (*agent.Agent, error) {
			return agent.New(agent.Options{
				ModelResolver: resolver,
				Storage:       store,
				CWD:           projectCwd,
			})
		},
		ModelList: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	server := New(manager)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(func() { ts.Close(); server.Close() })
	return ts, projects.ProjectID(projectDir)
}

func createPost(base, ns string, body string) (*http.Response, error) {
	req, _ := http.NewRequest("POST", base+"/api/projects/"+ns+"/sessions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func TestCreateSessionUnknownProject(t *testing.T) {
	base, _ := createTestServer(t)
	resp, err := createPost(base.URL, "p0000000000000000", `{"provider":"fake","model":"fake-model"}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["error"] != "unknown_project" {
		t.Fatalf("code = %q, want unknown_project", body["error"])
	}
}

func TestCreateSessionStrictFields(t *testing.T) {
	base, ns := createTestServer(t)
	resp, err := createPost(base.URL, ns, `{"provider":"fake","model":"fake-model","bogus":true}`)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field: status = %d, want 400", resp.StatusCode)
	}
}

func TestCreateSessionInvalidMeta(t *testing.T) {
	base, ns := createTestServer(t)
	resp, err := createPost(base.URL, ns, `{"provider":"fake","model":"fake-model","reasoning_level":"bogus"}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["error"] != "model_unavailable" {
		t.Fatalf("code = %q, want model_unavailable", body["error"])
	}
}

func TestCreateSessionSuccess(t *testing.T) {
	base, ns := createTestServer(t)
	resp, err := createPost(base.URL, ns, `{"provider":"fake","model":"fake-model"}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var body struct {
		ID string `json:"id"`
		NS string `json:"ns"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ID == "" || body.NS != ns {
		t.Fatalf("body = %+v, want a non-empty id and ns=%s", body, ns)
	}

	// The new session is reachable through the stream (the table's
	// fast path — no Locate needed).
	streamResp, err := http.Get(base.URL + "/api/s/" + body.ID + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		t.Fatalf("state on the new session: %d, want 200 (table fast path)", streamResp.StatusCode)
	}
}

// TestCreateSessionEvictedAfterRelease pins the manager-level lease
// release: a session created through the endpoint (with no message,
// no stream connection) must be evicted by the sweeper — the
// subsequent state request returns 404 unknown_session. Deleting the
// lease.Release() in Manager.CreateSession turns this red (the entry
// would stay pinned forever).
func TestCreateSessionEvictedAfterRelease(t *testing.T) {
	base, ns := createTestServer(t)
	resp, err := createPost(base.URL, ns, `{"provider":"fake","model":"fake-model"}`)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}

	// Wait for the sweeper to evict (the created entry has no refs
	// after the endpoint released it, and the disk has nothing). The
	// poll interval must EXCEED the idle window (50ms): each state
	// request acquires a lease and resets the idle timer — polling
	// faster than the window would keep the session alive forever.
	deadline := time.Now().Add(10 * time.Second)
	for {
		stateResp, err := http.Get(base.URL + "/api/s/" + body.ID + "/state") //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		stateResp.Body.Close()
		if stateResp.StatusCode == http.StatusNotFound {
			// The entry is gone (the disk never had anything — the
			// 404 is the unknown-session path).
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the created-then-released session was never evicted (lease not released?)")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestCreateSessionBadRequestCodes pins the 400 codes: unexpected_field
// (unknown JSON key) and invalid_input (missing provider/model).
func TestCreateSessionBadRequestCodes(t *testing.T) {
	base, ns := createTestServer(t)
	for _, tc := range []struct {
		name string
		body string
		code string
	}{
		{"unknown field", `{"provider":"fake","model":"fake-model","bogus":1}`, "unexpected_field"},
		{"missing provider", `{"model":"fake-model"}`, "invalid_input"},
		{"missing model", `{"provider":"fake"}`, "invalid_input"},
		{"malformed json", `{`, "invalid_input"},
		{"type mismatch", `{"provider":1,"model":"fake-model"}`, "invalid_input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := createPost(base.URL, ns, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var body map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["error"] != tc.code {
				t.Fatalf("code = %q, want %q", body["error"], tc.code)
			}
		})
	}
}
