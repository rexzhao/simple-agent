package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage"
)

// testConfigForResolver builds a minimal Config with one provider and
// one model, parameterized over the reasoning configuration.
func testConfigForResolver(t *testing.T, reasoning ReasoningConfig) *Config {
	t.Helper()
	return &Config{
		DefaultProvider: "fake",
		DefaultModel:    "fake-model",
		Providers: map[string]ProviderConfig{
			"fake": {
				Name:    "fake",
				BaseURL: "http://127.0.0.1:1",
				APIKey:  "test",
				Models: map[string]ModelProfile{
					"fake-model": {
						ID:              "fake-model",
						Type:            ProviderTypeOpenAIChat,
						ContextWindow:   50_000,
						OutputLimit:     5_000,
						ReasoningConfig: reasoning,
					},
				},
			},
		},
	}
}

// TestConfigResolverCachesProviderByConstruction pins acceptance (b)
// on the PRODUCTION resolver: the provider builder runs at most once
// per provider name across sequential and concurrent Resolve calls.
func TestConfigResolverCachesProviderByConstruction(t *testing.T) {
	cfg := testConfigForResolver(t, ReasoningConfig{})
	var constructed int64
	resolver, err := NewModelResolver(cfg, func(ResolvedModel) (model.Provider, error) {
		atomic.AddInt64(&constructed, 1)
		return &nullProvider{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	meta := storage.SessionMeta{Provider: "fake", Model: "fake-model"}

	// Sequential.
	for i := 0; i < 3; i++ {
		if _, err := resolver.Resolve(meta); err != nil {
			t.Fatal(err)
		}
	}
	// Concurrent.
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := resolver.Resolve(meta); err != nil {
				t.Error(err)
			}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	if constructed != 1 {
		t.Fatalf("provider constructed %d times, want exactly 1 (cached per name)", constructed)
	}
}

// TestConfigResolverEmptyReasoningLevelIsDefault pins acceptance (f) on
// the PRODUCTION resolver: an empty ReasoningLevel (old sessions
// without the field) resolves to the model's DEFAULT reasoning
// parameters — not an error, not a skip of the reasoning refinement.
func TestConfigResolverEmptyReasoningLevelIsDefault(t *testing.T) {
	// The model's reasoning config maps "high" to a specific parameter
	// value; the default (empty level) maps to the config's Default.
	reasoning := ReasoningConfig{
		Type:      "effort",
		Parameter: "reasoning_effort",
		Default:   "medium",
		Levels:    map[string]any{"low": "low", "medium": "medium", "high": "high"},
	}
	cfg := testConfigForResolver(t, reasoning)
	resolver, err := NewModelResolver(cfg, func(ResolvedModel) (model.Provider, error) {
		return &nullProvider{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Empty level (old session): resolves with the DEFAULT tier.
	resolved, err := resolver.Resolve(storage.SessionMeta{Provider: "fake", Model: "fake-model"})
	if err != nil {
		t.Fatalf("empty reasoning level: %v", err)
	}
	if got := resolved.Parameters["reasoning_effort"]; got != "medium" {
		t.Fatalf("empty level parameters = %v, want the model default (medium)", resolved.Parameters)
	}

	// Explicit level: refines to that tier.
	resolvedHigh, err := resolver.Resolve(storage.SessionMeta{Provider: "fake", Model: "fake-model", ReasoningLevel: "high"})
	if err != nil {
		t.Fatalf("explicit reasoning level: %v", err)
	}
	if got := resolvedHigh.Parameters["reasoning_effort"]; got != "high" {
		t.Fatalf("high level parameters = %v, want high", resolvedHigh.Parameters)
	}
}

// TestConfigResolverUnknownModelIsTypedUnavailable pins acceptance (c)'s
// config-side wrap: a recorded model that no longer resolves carries
// agent.ErrModelUnavailable (never a bare config error).
func TestConfigResolverUnknownModelIsTypedUnavailable(t *testing.T) {
	reasoning := ReasoningConfig{
		Type:      "effort",
		Parameter: "reasoning_effort",
		Levels:    map[string]any{"low": "low", "high": "high"},
	}
	cfg := testConfigForResolver(t, reasoning)
	resolver, err := NewModelResolver(cfg, func(ResolvedModel) (model.Provider, error) {
		return &nullProvider{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Resolve(storage.SessionMeta{Provider: "fake", Model: "removed-model"})
	if !errors.Is(err, agent.ErrModelUnavailable) {
		t.Fatalf("unknown model: %v, want agent.ErrModelUnavailable", err)
	}
	// An unsupported reasoning level is the same typed error.
	_, err = resolver.Resolve(storage.SessionMeta{Provider: "fake", Model: "fake-model", ReasoningLevel: "bogus"})
	if !errors.Is(err, agent.ErrModelUnavailable) {
		t.Fatalf("unsupported reasoning level: %v, want agent.ErrModelUnavailable", err)
	}
}

// nullProvider is a do-nothing provider for resolver tests that never
// make model calls.
type nullProvider struct{}

func (p *nullProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	events := make(chan model.Event)
	close(events)
	return events, nil
}

// TestLoadProviderRequiresBaseURL pins the load-time check: a provider
// file with a missing or slash-only base_url fails Load with a
// file-naming error instead of surfacing as a panic at the first
// session open. The slash-only forms must match the provider
// CONSTRUCTORS' emptiness test exactly (TrimSpace + TrimRight "/") —
// a plain TrimSpace here would let "/" through and the panic returns.
func TestLoadProviderRequiresBaseURL(t *testing.T) {
	for name, baseURL := range map[string]string{
		"missing":        "",
		"slash-only":     "/",
		"double-slash":   "//",
		"spaces+slashes": "  /  ",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			provider := "name: fake\nbase_url: " + baseURL + "\napi_key: test\nmodels:\n  fake-model:\n    id: fake-model\n    type: openai-chat\n"
			if err := os.WriteFile(filepath.Join(dir, "fake.yaml"), []byte(provider), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadProviderConfigs(dir)
			if err == nil {
				t.Fatal("provider with degenerate base_url loaded")
			}
			if !strings.Contains(err.Error(), "base_url is required") || !strings.Contains(err.Error(), "fake.yaml") {
				t.Fatalf("error = %v, want base_url is required naming the file", err)
			}
		})
	}
	// A real URL with a trailing slash still loads (the constructors
	// strip it — the check must not reject valid values).
	dir := t.TempDir()
	provider := "name: fake\nbase_url: http://127.0.0.1:1/\napi_key: test\nmodels:\n  fake-model:\n    id: fake-model\n    type: openai-chat\n"
	if err := os.WriteFile(filepath.Join(dir, "fake.yaml"), []byte(provider), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProviderConfigs(dir); err != nil {
		t.Fatalf("valid trailing-slash base_url rejected: %v", err)
	}
}

// TestConfigResolverRejectsEmptyProviderOrModel pins the strictness
// rule (§15.2): resolution NEVER substitutes defaults for an empty
// provider or model — a session whose created record lacks either is
// model_unavailable, not silently re-pointed at the current default.
// The three degenerate combinations all fail with the typed error.
func TestConfigResolverRejectsEmptyProviderOrModel(t *testing.T) {
	cfg := testConfigForResolver(t, ReasoningConfig{})
	resolver, err := NewModelResolver(cfg, func(ResolvedModel) (model.Provider, error) {
		return &nullProvider{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, meta := range map[string]storage.SessionMeta{
		"both-empty":    {Provider: "", Model: ""},
		"provider-only": {Provider: "fake", Model: ""},
		"model-only":    {Provider: "", Model: "fake-model"},
		"whitespace":    {Provider: "  ", Model: "  "},
		"ws-provider":   {Provider: "  ", Model: "fake-model"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolver.Resolve(meta)
			if !errors.Is(err, agent.ErrModelUnavailable) {
				t.Fatalf("Resolve(%+v): %v, want ErrModelUnavailable", meta, err)
			}
		})
	}
}
