package main

// newServeManager assembly tests (C3): the production serve wiring —
// the ModelList resolver reaches /api/models. K3 (the resolver
// dropped from the assembly) must turn this red.

import (
	"encoding/json"
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
