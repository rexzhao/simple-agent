package agent_test

// ModelResolver.List tests (C3): the descriptor list is stable-sorted
// (provider, then model), the Default mark is exactly one entry (or
// zero when no default is configured), and the tiers ride along.

import (
	"reflect"
	"testing"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/model"
)

func listResolver(t *testing.T, cfg *config.Config) agent.ModelResolver {
	t.Helper()
	resolver, err := config.NewModelResolver(cfg, func(config.ResolvedModel) (model.Provider, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func TestModelListSortedAndDefaultMarked(t *testing.T) {
	cfg := validateTestConfig() // fake/{fake-model, fake-reasoner} + other/other-model; default = fake/fake-model
	resolver := listResolver(t, cfg)
	list := resolver.List()

	// The list is sorted: provider name, then model name.
	keys := make([]string, len(list))
	for i, d := range list {
		keys[i] = d.Provider + "/" + d.Model
	}
	want := []string{"fake/fake-model", "fake/fake-reasoner", "other/other-model"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("list order = %v, want %v", keys, want)
	}

	// Exactly one Default: the configured default pair.
	defaults := 0
	for _, d := range list {
		if d.Default {
			defaults++
			if d.Provider != "fake" || d.Model != "fake-model" {
				t.Fatalf("default = %s/%s, want fake/fake-model", d.Provider, d.Model)
			}
		}
	}
	if defaults != 1 {
		t.Fatalf("defaults = %d, want exactly 1", defaults)
	}

	// The reasoner carries its tiers.
	for _, d := range list {
		if d.Model == "fake-reasoner" {
			if !reflect.DeepEqual(d.ReasoningLevels, []string{"low", "high"}) {
				t.Fatalf("fake-reasoner tiers = %v, want [low high]", d.ReasoningLevels)
			}
		}
	}
}

func TestModelListNoDefaultWhenUnset(t *testing.T) {
	cfg := validateTestConfig()
	cfg.DefaultProvider = ""
	cfg.DefaultModel = ""
	resolver := listResolver(t, cfg)
	for _, d := range resolver.List() {
		if d.Default {
			t.Fatalf("default = %s/%s, want none", d.Provider, d.Model)
		}
	}
}
