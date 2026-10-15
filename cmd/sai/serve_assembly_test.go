package main

// newServeManager assembly tests (C3): the production serve wiring —
// the ModelList resolver reaches /api/models. K3 (the resolver
// dropped from the assembly) must turn this red.

import (
	"strings"

	"encoding/json"
	"github.com/rexzhao/simple-agent/internal/projects"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rexzhao/simple-agent/internal/web"
)

// TestServeManagerModelsEndpoint builds the serve-mode Manager through
// the same newServeManager the serve() entry point uses, with a temp
// root and a fake provider config (the entrypoint-test fixture
// shape), and verifies GET /api/models returns the configuration's
// models with the default mark.
func TestServeManagerModelsEndpoint(t *testing.T) {
	root := t.TempDir()
	cfg := `default_provider: fake
default_model: fake-model
`
	if err := os.WriteFile(filepath.Join(root, "sai.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := `name: fake
base_url: http://127.0.0.1:1
api_key: test
models:
  fake-model:
    id: fake-model
    type: openai-chat
  fake-reasoner:
    id: fake-reasoner
    type: openai-chat
`
	if err := os.MkdirAll(filepath.Join(root, "providers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "providers", "fake.yaml"), []byte(provider), 0o644); err != nil {
		t.Fatal(err)
	}

	factory, err := assembleFactory(root, "fake", "fake-model")
	if err != nil {
		t.Fatalf("assembleFactory: %v", err)
	}
	manager, err := newServeManager(factory, root)
	if err != nil {
		t.Fatalf("newServeManager: %v", err)
	}
	t.Cleanup(func() { manager.Close() })
	server := web.New(manager)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(func() { ts.Close(); server.Close() })

	resp, err := http.Get(ts.URL + "/api/models") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var models []struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Default  bool   `json:"default"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2", len(models))
	}
	if models[0].Model != "fake-model" || !models[0].Default {
		t.Fatalf("models[0] = %+v, want fake-model default=true", models[0])
	}
	if models[1].Model != "fake-reasoner" || models[1].Default {
		t.Fatalf("models[1] = %+v, want fake-reasoner default=false", models[1])
	}
}

// TestServeManagerCreateSession pins the serve-seam wiring for the
// create endpoint: through newServeManager (the production assembly),
// POST /api/projects/{pid}/sessions creates a session reachable at
// GET /api/s/{id}/state (the table's fast path). The ModelList
// resolver must be wired (CreateSession depends on it).
func TestServeManagerCreateSession(t *testing.T) {
	root := t.TempDir()
	projectDir := t.TempDir()
	// Register the project so the ns resolves.
	reg := map[string]any{"projects": []map[string]any{{"path": projectDir, "added": "2026-10-15T00:00:00Z"}}}
	regJSON, _ := json.Marshal(reg)
	if err := os.WriteFile(filepath.Join(root, "projects.json"), regJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	// Provider config (dead port — creation only reads config).
	cfg := `default_provider: fake
default_model: fake-model
`
	if err := os.WriteFile(filepath.Join(root, "sai.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := `name: fake
base_url: http://127.0.0.1:1
api_key: test
models:
  fake-model:
    id: fake-model
    type: openai-chat
`
	if err := os.MkdirAll(filepath.Join(root, "providers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "providers", "fake.yaml"), []byte(provider), 0o644); err != nil {
		t.Fatal(err)
	}

	factory, err := assembleFactory(root, "fake", "fake-model")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := newServeManager(factory, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	server := web.New(manager)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(func() { ts.Close(); server.Close() })

	ns := projects.ProjectID(projectDir)
	req, _ := http.NewRequest("POST", ts.URL+"/api/projects/"+ns+"/sessions",
		strings.NewReader(`{"provider":"fake","model":"fake-model"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d, want 201", resp.StatusCode)
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ID == "" {
		t.Fatal("no session id in the create response")
	}

	// The created session is reachable through the table fast path.
	stateResp, err := http.Get(ts.URL + "/api/s/" + body.ID + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	stateResp.Body.Close()
	if stateResp.StatusCode != http.StatusOK {
		t.Fatalf("state on the created session: %d, want 200", stateResp.StatusCode)
	}
}
