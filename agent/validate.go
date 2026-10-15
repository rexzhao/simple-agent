package agent

import (
	"fmt"
	"strings"

	"github.com/rexzhao/simple-agent/storage"
)

// ValidateSessionMeta checks that a session's meta resolves against the
// given resolver AND that the reasoning level is one the model supports:
// a non-empty level for a model with no reasoning configuration, or a
// level outside the model's tiers, is ErrModelUnavailable with a
// user-facing message naming the parameter. This is the shared
// creation-time check (§17.2): the CLI's /new and the web's create
// endpoint both call it, so an invalid combination gets the same answer
// everywhere.
//
// The OPEN path (OpenSession on an existing record) deliberately does
// NOT go through this function: an old session whose recorded level
// no longer resolves still opens (the resolver's own behavior), so
// the open contract stays "resolve or refuse on model identity, never
// re-validate history." Re-validating on open would change whether
// old sessions can be reopened — a separate decision, not this one.
func ValidateSessionMeta(resolver ModelResolver, meta storage.SessionMeta) error {
	resolved, err := resolver.Resolve(meta)
	if err != nil {
		return err
	}
	if strings.TrimSpace(meta.ReasoningLevel) == "" {
		return nil
	}
	// NIL means the resolver did not report tiers (a third-party
	// ModelResolver that leaves the field zero) — skip the check
	// rather than misreport "takes no reasoning levels". Non-nil
	// empty is the config resolver's "no reasoning configuration".
	if resolved.ReasoningLevels == nil {
		return nil
	}
	if len(resolved.ReasoningLevels) == 0 {
		return fmt.Errorf("%w: model %q takes no reasoning levels (drop -reasoning)",
			ErrModelUnavailable, meta.Model)
	}
	for _, level := range resolved.ReasoningLevels {
		if level == meta.ReasoningLevel {
			return nil
		}
	}
	return fmt.Errorf("%w: reasoning level %q is not one of %s (model %q)",
		ErrModelUnavailable, meta.ReasoningLevel,
		strings.Join(resolved.ReasoningLevels, ", "), meta.Model)
}
