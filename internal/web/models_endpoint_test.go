package web

// GET /api/models endpoint tests (C3): the configuration's models
// surface — sorted, default-marked, tier-carrying; the guardAPI GET
// checks apply; an unwired ModelList answers an empty list.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage/jsonl"
)

func modelsTestManager(t *testing.T) *httptest.Server {
	t.Helper()
	root := t.TempDir()
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
		ServerRoot: root,
		Store:      store,
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
	return ts
}

func TestModelsEndpoint(t *testing.T) {
	base := modelsTestManager(t)
	resp, err := http.Get(base.URL + "/api/models") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var models []struct {
		Provider        string   `json:"provider"`
		Model           string   `json:"model"`
		ReasoningLevels []string `json:"reasoning_levels"`
		Default         bool     `json:"default"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2", len(models))
	}
	if models[0].Provider != "fake" || models[0].Model != "fake-model" || !models[0].Default {
		t.Fatalf("models[0] = %+v, want fake/fake-model default", models[0])
	}
	if models[1].Model != "fake-reasoner" || models[1].Default {
		t.Fatalf("models[1] = %+v, want fake-reasoner non-default", models[1])
	}
	if len(models[1].ReasoningLevels) != 2 {
		t.Fatalf("reasoner tiers = %v, want 2", models[1].ReasoningLevels)
	}
}

// TestModelsEndpointGuarded pins the guardAPI wiring: a non-loopback
// host is refused even on a read-only GET (the same check the 7a
// approval endpoint test exercises for writes).
func TestModelsEndpointGuarded(t *testing.T) {
	base := modelsTestManager(t)
	req, _ := http.NewRequest("GET", base.URL+"/api/models", nil) //nolint:noctx // test
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-loopback host: status = %d, want 403", resp.StatusCode)
	}
}

// TestModelsEndpointDefaultShape pins the raw body shape: the
// default key is EXPLICITLY present on non-default entries (no
// omitempty — false and "key missing" are indistinguishable to a
// struct decoder, so the assertion reads the body as text).
func TestModelsEndpointDefaultShape(t *testing.T) {
	base := modelsTestManager(t)
	resp, err := http.Get(base.URL + "/api/models") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"default":false`) {
		t.Fatalf("non-default entry lacks an explicit \"default\":false — omitempty or shape drift: %s", body)
	}
	if !strings.Contains(string(body), `"default":true`) {
		t.Fatalf("default entry lacks \"default\":true: %s", body)
	}
}

func TestModelsEndpointUnwired(t *testing.T) {
	// A manager without ModelList answers an empty list, not an error.
	root := t.TempDir()
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerOptions{
		ServerRoot: root,
		Store:      store,
		AgentFor: func(projectCwd string) (*agent.Agent, error) {
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	server := New(manager)
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
	var models []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 0 {
		t.Fatalf("models = %d, want 0 (unwired)", len(models))
	}
}
