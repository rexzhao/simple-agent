package agent_test

// ValidateSessionMeta tests (C2): the shared creation-time check —
// the CLI's /new and the web create endpoint both call it, and the
// production resolver now rejects unsupported reasoning levels for
// models (the B1 sink-down). The kills: removing the level check or
// the provider/model check turns these red.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage"
)

// validateTestConfig: two models — one with reasoning tiers, one
// without; plus an unknown-model surface.
func validateTestConfig() *config.Config {
	return &config.Config{
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
							Parameter: "reasoning_effort",
							Default:   "low",
							Levels:    map[string]any{"low": "low", "high": "high"},
						}},
				},
			},
			"other": {
				Name:    "other",
				BaseURL: "http://127.0.0.1:1",
				APIKey:  "test",
				Models: map[string]config.ModelProfile{
					"other-model": {ID: "other-model", Type: config.ProviderTypeOpenAIChat},
				},
			},
		},
	}
}

func validateResolver(t *testing.T) agent.ModelResolver {
	t.Helper()
	stub := stubValidateProvider{}
	resolver, err := config.NewModelResolver(validateTestConfig(), func(config.ResolvedModel) (model.Provider, error) {
		return stub, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func TestValidateSessionMetaAccepts(t *testing.T) {
	resolver := validateResolver(t)
	for _, tc := range []struct {
		name string
		meta storage.SessionMeta
	}{
		{"empty level", storage.SessionMeta{Provider: "fake", Model: "fake-model"}},
		{"valid level", storage.SessionMeta{Provider: "fake", Model: "fake-reasoner", ReasoningLevel: "high"}},
		{"default level on reasoner", storage.SessionMeta{Provider: "fake", Model: "fake-reasoner"}},
		{"cross provider", storage.SessionMeta{Provider: "other", Model: "other-model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := agent.ValidateSessionMeta(resolver, tc.meta); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

func TestValidateSessionMetaRejects(t *testing.T) {
	resolver := validateResolver(t)
	for _, tc := range []struct {
		name string
		meta storage.SessionMeta
		want string
	}{
		{"unknown provider", storage.SessionMeta{Provider: "nope", Model: "fake-model"}, "unknown provider"},
		{"unknown model", storage.SessionMeta{Provider: "fake", Model: "no-such"}, "unknown model"},
		{"level on non-reasoner", storage.SessionMeta{Provider: "fake", Model: "fake-model", ReasoningLevel: "high"}, "takes no reasoning levels"},
		{"unsupported level", storage.SessionMeta{Provider: "fake", Model: "fake-reasoner", ReasoningLevel: "ultra"}, "ultra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := agent.ValidateSessionMeta(resolver, tc.meta)
			if !errors.Is(err, agent.ErrModelUnavailable) {
				t.Fatalf("validate: %v, want ErrModelUnavailable", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

// stubValidateProvider satisfies model.Provider; ValidateSessionMeta
// never streams.
type stubValidateProvider struct{}

func (stubValidateProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	return nil, errors.New("stub: never called")
}

// TestValidateSessionMetaSkipsUnreportedTiers pins the nil/empty
// distinction: a resolver that leaves ReasoningLevels nil (a
// third-party ModelResolver, or the static one) does NOT get
// "takes no reasoning levels" — the tier check is skipped, not
// misapplied.
func TestValidateSessionMetaSkipsUnreportedTiers(t *testing.T) {
	provider := &stubValidateProvider{}
	resolver := agent.NewStaticResolver(provider, map[string]any{"reasoning_effort": "high"}, 0, 0, "")
	// The static resolver's Resolved.ReasoningLevels is nil (it does
	// not report tiers) — a non-empty level must NOT be rejected.
	err := agent.ValidateSessionMeta(resolver, storage.SessionMeta{Provider: "p", Model: "m", ReasoningLevel: "high"})
	if err != nil {
		t.Fatalf("validate with unreported tiers: %v, want nil (skip, not reject)", err)
	}
}
