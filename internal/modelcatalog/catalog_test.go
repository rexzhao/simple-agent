package modelcatalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestCatalogSearchAndNormalization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"anthropic": {
				"models": {
					"claude-sonnet-4-6": {
						"name": "Claude Sonnet 4.6",
						"description": "Workhorse",
						"reasoning": true,
						"reasoning_options": [
							{"type": "effort", "values": ["low", "medium", "high", "max"]},
							{"type": "budget_tokens", "min": 1024}
						],
						"modalities": {"input": ["text", "image", "pdf"]},
						"limit": {"context": 1000000, "output": 128000},
						"cost": {"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75}
					}
				}
			},
			"google": {
				"models": {
					"gemini-2.5-flash": {
						"name": "Gemini 2.5 Flash",
						"reasoning": true,
						"reasoning_options": [
							{"type": "toggle"},
							{"type": "budget_tokens", "min": 0, "max": 24576}
						],
						"modalities": {"input": ["text", "image", "audio", "video", "pdf"]},
						"limit": {"context": 1048576, "output": 65536},
						"cost": {"input": 0.3, "output": 2.5, "cache_read": 0.03}
					}
				}
			},
			"deepseek": {
				"models": {
					"deepseek-v4-flash": {
						"name": "DeepSeek V4 Flash",
						"reasoning": true,
						"reasoning_options": [
							{"type": "toggle"},
							{"type": "effort", "values": ["low", "high", "max"]}
						],
						"modalities": {"input": ["text"]},
						"limit": {"context": 1000000, "output": 384000}
					}
				}
			}
		}`))
	}))
	defer server.Close()

	catalog := New(Options{URL: server.URL, TTL: 0})
	ctx := context.Background()

	models, err := catalog.Search(ctx, "sonnet", SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search(sonnet) error = %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("Search(sonnet) = %d models, want 1", len(models))
	}
	claude := models[0]
	if claude.ID != "claude-sonnet-4-6" || claude.Provider != "anthropic" {
		t.Fatalf("claude model = %#v", claude)
	}
	if claude.ContextWindow != 1000000 || claude.OutputLimit != 128000 {
		t.Fatalf("claude limits = %#v", claude)
	}
	// pdf dropped; text+image kept
	if !reflect.DeepEqual(claude.Input, []string{"text", "image"}) {
		t.Fatalf("claude input = %#v, want [text image]", claude.Input)
	}
	if !reflect.DeepEqual(claude.Reasoning.EffortLevels, []string{"low", "medium", "high", "max"}) {
		t.Fatalf("claude effort = %#v", claude.Reasoning.EffortLevels)
	}
	if claude.Reasoning.BudgetMin == nil || *claude.Reasoning.BudgetMin != 1024 {
		t.Fatalf("claude budget min = %#v", claude.Reasoning.BudgetMin)
	}
	if claude.Pricing == nil || claude.Pricing.Input != 3 || claude.Pricing.Output != 15 || claude.Pricing.CacheRead != 0.3 {
		t.Fatalf("claude pricing = %#v", claude.Pricing)
	}

	geminiModels, err := catalog.Search(ctx, "gemini-2.5", SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search(gemini) error = %v", err)
	}
	if len(geminiModels) != 1 {
		t.Fatalf("Search(gemini) = %d, want 1", len(geminiModels))
	}
	gemini := geminiModels[0]
	if !reflect.DeepEqual(gemini.Input, []string{"text", "image"}) {
		t.Fatalf("gemini input = %#v, want [text image]", gemini.Input)
	}
	if !gemini.Reasoning.SupportsToggle {
		t.Fatalf("gemini supports_toggle = false, want true")
	}
	if gemini.Reasoning.BudgetMax == nil || *gemini.Reasoning.BudgetMax != 24576 {
		t.Fatalf("gemini budget max = %#v", gemini.Reasoning.BudgetMax)
	}
	if len(gemini.Reasoning.EffortLevels) != 0 {
		t.Fatalf("gemini effort levels = %#v, want none", gemini.Reasoning.EffortLevels)
	}

	// none -> off normalization
	deepseekModels, err := catalog.Search(ctx, "deepseek-v4", SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search(deepseek) error = %v", err)
	}
	if len(deepseekModels) != 1 {
		t.Fatalf("Search(deepseek) = %d, want 1", len(deepseekModels))
	}
	if !reflect.DeepEqual(deepseekModels[0].Reasoning.EffortLevels, []string{"low", "high", "max"}) {
		t.Fatalf("deepseek effort = %#v", deepseekModels[0].Reasoning.EffortLevels)
	}
}

func TestCatalogSearchEmptyAndNoMatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"p":{"models":{"m-1":{"modalities":{"input":["text"]}}}}}`))
	}))
	defer server.Close()

	catalog := New(Options{URL: server.URL, TTL: 0})
	ctx := context.Background()

	models, err := catalog.Search(ctx, "  ", SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search(empty) error = %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("Search(empty) = %d, want 0", len(models))
	}
	models, err = catalog.Search(ctx, "zzz-no-match", SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search(no match) error = %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("Search(no match) = %d, want 0", len(models))
	}
}

func TestCatalogCachesFetch(t *testing.T) {
	var fetches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		_, _ = w.Write([]byte(`{"p":{"models":{"m-1":{"modalities":{"input":["text"]}}}}}`))
	}))
	defer server.Close()

	catalog := New(Options{URL: server.URL, TTL: 0})
	ctx := context.Background()
	if _, err := catalog.Data(ctx); err != nil {
		t.Fatalf("Data() error = %v", err)
	}
	if _, err := catalog.Data(ctx); err != nil {
		t.Fatalf("Data() second error = %v", err)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1 (cached)", fetches)
	}
}