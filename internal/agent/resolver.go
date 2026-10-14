package agent

import (
	"errors"
	"fmt"

	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage"
)

// ErrModelUnavailable reports a session whose recorded model no longer
// resolves against the configuration: the provider or model name was
// removed, or the reasoning level is not one the model supports. Opening
// such a session fails — the alternative (silently falling back to a
// default model) would change the conversation's behavior without a
// trace in the record.
var ErrModelUnavailable = errors.New("the session's model is not available in the configuration")

// Resolved is the per-session model configuration: what a SessionMeta
// resolves to through a ModelResolver. The provider instance is shared
// per provider name (resolvers cache); everything else is per session.
type Resolved struct {
	Provider      model.Provider
	Parameters    map[string]any
	ContextWindow int
	OutputLimit   int
	DeveloperRole model.MessageRole
}

// ModelResolver maps a session's recorded meta to its live model
// configuration (§15.2). The agent package defines the interface — the
// configuration package implements it over sai.yaml — so the dependency
// direction is config → agent, never the reverse.
type ModelResolver interface {
	// Resolve returns the live configuration for a recorded meta, or
	// ErrModelUnavailable (use errors.Is) when it no longer resolves.
	Resolve(meta storage.SessionMeta) (Resolved, error)

	// List enumerates the configured models for UI selection. B1 leaves
	// the interface position only; milestone C's GET /api/models
	// consumes it.
	List() []ModelDescriptor
}

// ModelDescriptor is one selectable model entry (the List payload).
type ModelDescriptor struct {
	Provider string
	Model    string
	// ReasoningLevels are the model's supported reasoning tiers; empty
	// means the model takes no reasoning configuration.
	ReasoningLevels []string
}

// staticResolver resolves every meta to one fixed configuration — the
// test default (fake providers) and the bridge for call sites that
// have not migrated to a config-backed resolver yet.
type staticResolver struct {
	resolved Resolved
}

// NewStaticResolver resolves every session to the given configuration.
func NewStaticResolver(provider model.Provider, parameters map[string]any, contextWindow, outputLimit int, developerRole model.MessageRole) ModelResolver {
	if developerRole == "" {
		developerRole = model.MessageRoleDeveloper
	}
	return &staticResolver{resolved: Resolved{
		Provider: provider, Parameters: parameters,
		ContextWindow: contextWindow, OutputLimit: outputLimit,
		DeveloperRole: developerRole,
	}}
}

func (r *staticResolver) Resolve(meta storage.SessionMeta) (Resolved, error) {
	return r.resolved, nil
}

func (r *staticResolver) List() []ModelDescriptor { return nil }

// resolveMeta is the single resolution entry the session paths use: it
// turns a recorded meta into the session's Resolved, classifying the
// failure as ErrModelUnavailable with the meta named for the operator.
func resolveMeta(resolver ModelResolver, meta storage.SessionMeta) (Resolved, error) {
	resolved, err := resolver.Resolve(meta)
	if err != nil {
		if errors.Is(err, ErrModelUnavailable) {
			return Resolved{}, fmt.Errorf("session model %q/%q (reasoning %q): %w",
				meta.Provider, meta.Model, meta.ReasoningLevel, ErrModelUnavailable)
		}
		return Resolved{}, err
	}
	return resolved, nil
}

// SessionMetaForTest is the meta web/CLI tests record in new sessions
// (the test provider/model names). Production hosts derive it from
// their configuration; tests share one constant so the wire fixtures
// stay stable.
var SessionMetaForTest = storage.SessionMeta{Provider: "test", Model: "model-test"}
