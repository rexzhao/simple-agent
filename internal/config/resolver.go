package config

import (
	"fmt"
	"strings"
	"sync"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage"
)

// NewModelResolver implements agent.ModelResolver over the loaded
// configuration (§15.2). Provider instances are cached per provider
// NAME: safe because the configuration is immutable in-process (read
// once at startup, never reloaded). If hot reload ever lands, the cache
// key must widen to the configuration fingerprint (baseURL/key) or
// different configurations would share — and pollute — one client.
func NewModelResolver(cfg *Config, newProvider func(ResolvedModel) (model.Provider, error)) (agent.ModelResolver, error) {
	return &configResolver{cfg: cfg, newProvider: newProvider}, nil
}

type configResolver struct {
	cfg         *Config
	newProvider func(ResolvedModel) (model.Provider, error)

	mu        sync.Mutex
	providers map[string]model.Provider // provider name → shared instance
}

// Resolve maps a session's recorded meta to its live configuration.
// A recorded model that no longer resolves is agent.ErrModelUnavailable
// — never a silent fallback to the default. An EMPTY provider or model
// is the same error: ResolveModel would substitute the configured
// default, silently re-pointing the session at whatever the current
// default happens to be (§15.2 forbids exactly that). Defaults are a
// CREATION-time concern (the host builds SessionMeta from its startup
// parameters); resolution is strict.
func (r *configResolver) Resolve(meta storage.SessionMeta) (agent.Resolved, error) {
	if strings.TrimSpace(meta.Provider) == "" || strings.TrimSpace(meta.Model) == "" {
		return agent.Resolved{}, fmt.Errorf("%w: session.created records an empty provider or model", agent.ErrModelUnavailable)
	}
	resolved, err := r.cfg.ResolveModel(meta.Provider, meta.Model)
	if err != nil {
		return agent.Resolved{}, fmt.Errorf("%w: %v", agent.ErrModelUnavailable, err)
	}
	// The reasoning level refines the parameters — for BOTH the empty
	// level (old sessions: the model's Default tier) and an explicit
	// one. Skipping the call for empty would leave the raw profile
	// parameters instead of the default-refined form.
	parameters, err := ApplyReasoningLevel(resolved.Parameters, resolved.ReasoningConfig, meta.ReasoningLevel)
	if err != nil {
		return agent.Resolved{}, fmt.Errorf("%w: reasoning level %q for %s/%s: %v",
			agent.ErrModelUnavailable, meta.ReasoningLevel, meta.Provider, meta.Model, err)
	}
	developerRole := model.MessageRoleDeveloper
	if resolved.DeveloperRole != "" {
		developerRole = model.MessageRole(resolved.DeveloperRole)
	}
	return agent.Resolved{
		Provider:        r.providerFor(resolved),
		Parameters:      parameters,
		ContextWindow:   resolved.ContextWindow,
		OutputLimit:     resolved.OutputLimit,
		DeveloperRole:   developerRole,
		ReasoningLevels: reasoningLevelNames(resolved.ReasoningConfig),
	}, nil
}

// providerFor returns the shared provider instance for the resolved
// model's provider name, constructing it at most once. A construction
// failure panics: the provider configuration was already validated at
// load time, so reaching here with an error is a programming fault,
// not a runtime condition to surface per-session.
func (r *configResolver) providerFor(resolved ResolvedModel) model.Provider {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.providers == nil {
		r.providers = make(map[string]model.Provider)
	}
	if p, ok := r.providers[resolved.ProviderName]; ok {
		return p
	}
	p, err := r.newProvider(resolved)
	if err != nil {
		panic(fmt.Sprintf("construct provider %q: %v", resolved.ProviderName, err))
	}
	r.providers[resolved.ProviderName] = p
	return p
}

// List enumerates the configured models (agent.ModelResolver's
// interface position; milestone C's GET /api/models consumes it).
func (r *configResolver) List() []agent.ModelDescriptor {
	infos := r.cfg.ModelList()
	out := make([]agent.ModelDescriptor, 0, len(infos))
	for _, info := range infos {
		provider, ok := r.cfg.Providers[info.Provider]
		if !ok {
			continue
		}
		profile, ok := provider.Models[info.Profile]
		if !ok {
			continue
		}
		out = append(out, agent.ModelDescriptor{
			Provider: info.Provider,
			Model:    info.Profile,
			// The reasoning tiers come from the profile's config; empty
			// means the model takes no reasoning configuration.
			ReasoningLevels: reasoningLevelNames(profile.ReasoningConfig),
			// The default mark: exactly the default_provider +
			// default_model pair (zero entries when no default is
			// configured).
			Default: info.Provider == r.cfg.DefaultProvider && info.Profile == r.cfg.DefaultModel,
		})
	}
	return out
}

// reasoningLevelNames extracts the supported reasoning tier names in
// canonical order (off..max, custom tiers alphabetically) — the same
// ordering the reasoning level selector shows.
func reasoningLevelNames(rc ReasoningConfig) []string {
	return ReasoningLevelNames(rc.Levels)
}
