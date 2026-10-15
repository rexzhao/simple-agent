package main

// newServeManager assembly tests (C3): the production serve wiring —
// the ModelList resolver reaches /api/models. K3 (the resolver
// dropped from the assembly) must turn this red.

import (
	"strings"
	"time"

	"bytes"
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

// TestServeManagerProjectRegistry drives the register/unregister
// endpoints through the real serve assembly (the C8 seam test): the
// CLI-facing surface a scripts user gets.
func TestServeManagerProjectRegistry(t *testing.T) {
	root := t.TempDir()
	projectDir := t.TempDir()
	writeServeConfig(t, root)
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

	// Register through the endpoint.
	pathBody, _ := json.Marshal(map[string]string{"path": projectDir})
	resp, err := http.Post(ts.URL+"/api/projects", "application/json", bytes.NewReader(pathBody)) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || body["id"] != projects.ProjectID(projectDir) {
		t.Fatalf("register: %d %v", resp.StatusCode, body)
	}

	// The listing carries it.
	found := false
	deadline := time.Now().Add(4 * time.Second)
	for !found && time.Now().Before(deadline) {
		listResp, err := http.Get(ts.URL + "/api/projects") //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		var listBody struct {
			Projects []struct {
				ID string `json:"id"`
			} `json:"projects"`
		}
		_ = json.NewDecoder(listResp.Body).Decode(&listBody)
		listResp.Body.Close()
		for _, p := range listBody.Projects {
			if p.ID == projects.ProjectID(projectDir) {
				found = true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		t.Fatal("the registered project never appeared in /api/projects")
	}

	// Unregister: 200, then the listing drops it.
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/projects/"+projects.ProjectID(projectDir), nil) //nolint:noctx // test
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("unregister: %d", delResp.StatusCode)
	}
	gone := false
	deadline = time.Now().Add(4 * time.Second)
	for !gone && time.Now().Before(deadline) {
		listResp, err := http.Get(ts.URL + "/api/projects") //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		var listBody struct {
			Projects []struct {
				ID string `json:"id"`
			} `json:"projects"`
		}
		_ = json.NewDecoder(listResp.Body).Decode(&listBody)
		listResp.Body.Close()
		gone = true
		for _, p := range listBody.Projects {
			if p.ID == projects.ProjectID(projectDir) {
				gone = false
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !gone {
		t.Fatal("the unregistered project never left /api/projects")
	}
}

// writeServeConfig lays out the config + provider the assembly needs
// (dead port — no network contact).
func writeServeConfig(t *testing.T, root string) {
	t.Helper()
	cfg := "default_provider: fake\ndefault_model: fake-model\n"
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
}
