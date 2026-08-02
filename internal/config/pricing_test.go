package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModelPricingLoadsAndResolvesAsProfileMetadata(t *testing.T) {
	dir := t.TempDir()
	providers := filepath.Join(dir, "providers")
	if err := os.MkdirAll(providers, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeFile(t, filepath.Join(dir, "sai.yaml"), "default_provider: test\ndefault_model: model\nprovider_dir: providers\n")
	writeFile(t, filepath.Join(providers, "test.yaml"), `name: test
base_url: https://example.test/v1
models:
  model:
    id: model-id
    pricing:
      input_cache_hit: 0.15
      input_cache_miss: 1.5
      cache_write: 2
      output: 7.5
      currency: usd
      long_context_threshold: 272000
      long_context:
        input_cache_hit: 0.3
        input_cache_miss: 3
        cache_write: 4
        output: 15
`)

	cfg, err := Load(filepath.Join(dir, "sai.yaml"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	pricing := cfg.Providers["test"].Models["model"].Pricing
	if pricing == nil || pricing.InputCacheHit != 0.15 || pricing.InputCacheMiss != 1.5 || pricing.CacheWrite != 2 || pricing.Output != 7.5 || pricing.Currency != "USD" || pricing.LongContextThreshold != 272000 || pricing.LongContext == nil || pricing.LongContext.Output != 15 {
		t.Fatalf("pricing = %#v, want normalized profile pricing", pricing)
	}
	resolved, err := cfg.ResolveModel("test", "model")
	if err != nil {
		t.Fatalf("ResolveModel() error = %v", err)
	}
	if resolved.Pricing == nil || resolved.Pricing == pricing {
		t.Fatalf("resolved pricing = %#v, want a copied pricing snapshot", resolved.Pricing)
	}
	if resolved.Pricing.LongContext == nil || resolved.Pricing.LongContext == pricing.LongContext {
		t.Fatalf("resolved long pricing = %#v, want a copied pricing snapshot", resolved.Pricing.LongContext)
	}
}

func TestModelPricingRejectsNegativeValues(t *testing.T) {
	dir := t.TempDir()
	providers := filepath.Join(dir, "providers")
	if err := os.MkdirAll(providers, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeFile(t, filepath.Join(dir, "sai.yaml"), "provider_dir: providers\n")
	writeFile(t, filepath.Join(providers, "test.yaml"), `name: test
base_url: https://example.test/v1
models:
  model:
    id: model-id
    pricing:
      input_cache_hit: -1
`)
	if _, err := Load(filepath.Join(dir, "sai.yaml")); err == nil {
		t.Fatal("Load() error = nil, want negative pricing validation error")
	}
}
