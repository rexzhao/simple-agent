package syncengine

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

// IsKnownResourceType is the single closed V1 resource catalog. Adding a
// resource therefore requires an explicit provider registration point rather
// than silently accepting a dynamic type.
func IsKnownResourceType(resourceType protocol.ResourceType) bool {
	switch resourceType {
	case protocol.ResourceTypeProjectIndex,
		protocol.ResourceTypeSessionIndex,
		protocol.ResourceTypeSessionContent,
		protocol.ResourceTypeProviderSettings,
		protocol.ResourceTypeModelCatalog,
		protocol.ResourceTypeCodexLogin:
		return true
	default:
		return false
	}
}

func ValidateResourceKey(key protocol.ResourceKey) error {
	if !IsKnownResourceType(key.Type) {
		return fmt.Errorf("%w: %q", ErrUnknownResourceType, key.Type)
	}
	if err := protocol.ValidateResourceKey(key); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidResourceKey, err)
	}
	return nil
}

// ProviderRegistry serializes provider registration and lookup. Registration
// is expected during application setup; Open only takes a read lock and calls
// provider code outside the registry lock.
type ProviderRegistry struct {
	mu        sync.RWMutex
	providers map[protocol.ResourceType]ResourceProvider
}

func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{providers: make(map[protocol.ResourceType]ResourceProvider)}
}

func (r *ProviderRegistry) Register(provider ResourceProvider) error {
	if provider == nil {
		return fmt.Errorf("%w: provider is nil", ErrInvalidOpenedResource)
	}
	resourceType := provider.Type()
	if !IsKnownResourceType(resourceType) {
		return fmt.Errorf("%w: %q", ErrUnknownResourceType, resourceType)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[resourceType]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateProvider, resourceType)
	}
	r.providers[resourceType] = provider
	return nil
}

func (r *ProviderRegistry) Provider(resourceType protocol.ResourceType) (ResourceProvider, error) {
	if !IsKnownResourceType(resourceType) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownResourceType, resourceType)
	}
	r.mu.RLock()
	provider, exists := r.providers[resourceType]
	r.mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrProviderNotRegistered, resourceType)
	}
	return provider, nil
}

// Open performs the required authorize-then-open lifecycle. Resource provider
// Open implementations must honor the atomic barrier documented on
// ResourceProvider; the registry deliberately does not split snapshot,
// journal decision, and live registration into separate calls.
func (r *ProviderRegistry) Open(ctx context.Context, principal Principal, key protocol.ResourceKey, resume *protocol.ResumeToken) (OpenedResource, error) {
	if err := ValidateResourceKey(key); err != nil {
		return OpenedResource{}, err
	}
	provider, err := r.Provider(key.Type)
	if err != nil {
		return OpenedResource{}, err
	}
	if err := provider.Authorize(ctx, principal, key); err != nil {
		return OpenedResource{}, fmt.Errorf("authorize %s/%s: %w", key.Type, key.ID, err)
	}
	opened, err := provider.Open(ctx, key, resume)
	if err != nil {
		return OpenedResource{}, fmt.Errorf("open %s/%s: %w", key.Type, key.ID, err)
	}
	if err := validateOpenedResource(opened, resume); err != nil {
		if opened.Close != nil {
			opened.Close()
		}
		return OpenedResource{}, fmt.Errorf("open %s/%s: %w", key.Type, key.ID, err)
	}

	// Snapshot and replay bytes are copied before they cross the provider
	// boundary. The live channels are owned streams, not mutable snapshot
	// values.
	opened.Snapshot = opened.Snapshot.Clone()
	opened.Decision = opened.Decision.Clone()
	originalClose := opened.Close
	var closeOnce sync.Once
	opened.Close = func() {
		closeOnce.Do(originalClose)
	}
	return opened, nil
}

func validateOpenedResource(opened OpenedResource, resume *protocol.ResumeToken) error {
	if strings.TrimSpace(opened.StreamEpoch) == "" {
		return fmt.Errorf("%w: stream epoch is required", ErrInvalidOpenedResource)
	}
	if opened.Changes == nil {
		return fmt.Errorf("%w: changes channel is required", ErrInvalidOpenedResource)
	}
	if opened.Terminal == nil {
		return fmt.Errorf("%w: terminal channel is required", ErrInvalidOpenedResource)
	}
	if opened.Close == nil {
		return fmt.Errorf("%w: close function is required", ErrInvalidOpenedResource)
	}
	if err := opened.Snapshot.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidOpenedResource, err)
	}

	decision := opened.Decision
	if decision.StreamEpoch != opened.StreamEpoch {
		return fmt.Errorf("%w: decision epoch does not match opened epoch", ErrInvalidOpenedResource)
	}
	if decision.ToSequence != opened.Sequence {
		return fmt.Errorf("%w: decision barrier sequence does not match opened sequence", ErrInvalidOpenedResource)
	}
	if opened.Sequence == ^uint64(0) || opened.LiveFromSequence != opened.Sequence+1 {
		return fmt.Errorf("%w: live source does not start immediately after barrier", ErrInvalidOpenedResource)
	}
	if err := validateDecisionShape(decision, resume, opened.StreamEpoch, opened.Sequence); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidOpenedResource, err)
	}
	return nil
}

func validateDecisionShape(decision ResumeDecision, resume *protocol.ResumeToken, epoch string, barrier uint64) error {
	if decision.StreamEpoch != epoch || decision.ToSequence != barrier {
		return fmt.Errorf("decision barrier is inconsistent")
	}
	if len(decision.Entries) > 0 {
		if decision.Action != SyncActionReplay || decision.Classification != ResumeReplayAvailable {
			return fmt.Errorf("replay entries require a replay decision")
		}
	}

	switch decision.Classification {
	case ResumeNoToken:
		if resume != nil || decision.Action != SyncActionSnapshot || decision.Reason != ResyncReasonNoResume || len(decision.Entries) != 0 {
			return fmt.Errorf("no-token decision is inconsistent")
		}
		if decision.FromSequence != 0 {
			return fmt.Errorf("no-token decision has a from sequence")
		}
	case ResumeCurrentExact:
		sequence, err := validatedResumeSequence(resume, epoch)
		if err != nil || decision.Action != SyncActionCurrent || decision.Reason != "" || len(decision.Entries) != 0 || sequence != barrier || decision.FromSequence != barrier {
			return fmt.Errorf("current decision is inconsistent")
		}
	case ResumeReplayAvailable:
		sequence, err := validatedResumeSequence(resume, epoch)
		if err != nil || decision.Action != SyncActionReplay || decision.Reason != "" || len(decision.Entries) == 0 || sequence >= barrier || decision.FromSequence != decision.Entries[0].Sequence {
			return fmt.Errorf("replay decision is inconsistent")
		}
		if decision.Entries[0].PreviousSequence != sequence {
			return fmt.Errorf("replay does not start after resume sequence")
		}
		if err := validateReplayEntries(decision.Entries, epoch, barrier); err != nil {
			return err
		}
	case ResumeEpochMismatch, ResumeTooOld, ResumeAhead, ResumeInvalid:
		if resume == nil || decision.Action != SyncActionResync || len(decision.Entries) != 0 {
			return fmt.Errorf("resync decision is inconsistent")
		}
		wantReason := map[ResumeClassification]ResyncReason{
			ResumeEpochMismatch: ResyncReasonEpochMismatch,
			ResumeTooOld:        ResyncReasonTooOld,
			ResumeAhead:         ResyncReasonAhead,
			ResumeInvalid:       ResyncReasonInvalidResume,
		}[decision.Classification]
		if decision.Reason != wantReason {
			return fmt.Errorf("resync reason does not match classification")
		}
	default:
		return fmt.Errorf("unknown resume classification %q", decision.Classification)
	}
	return nil
}

func validatedResumeSequence(resume *protocol.ResumeToken, epoch string) (uint64, error) {
	if resume == nil || resume.StreamEpoch != epoch {
		return 0, fmt.Errorf("resume epoch does not match")
	}
	return protocol.ParseUint64Decimal(string(resume.Sequence))
}

func validateReplayEntries(entries []JournalEntry, epoch string, barrier uint64) error {
	if len(entries) == 0 {
		return fmt.Errorf("replay is empty")
	}
	for index, entry := range entries {
		if entry.StreamEpoch != epoch {
			return fmt.Errorf("replay entry %d has a different epoch", index)
		}
		if err := entry.Change.Validate(); err != nil {
			return fmt.Errorf("replay entry %d: %v", index, err)
		}
		if index > 0 {
			previous := entries[index-1]
			if entry.PreviousSequence != previous.Sequence || entry.Sequence != previous.Sequence+1 {
				return fmt.Errorf("replay entries are not continuous")
			}
		}
	}
	if entries[len(entries)-1].Sequence != barrier {
		return fmt.Errorf("replay does not end at barrier sequence")
	}
	return nil
}

// Engine is the transport-independent facade used by later gateway layers.
// It owns no socket, goroutine, or WebSocket DTO state.
type Engine struct {
	providers *ProviderRegistry
}

func NewEngine(providers *ProviderRegistry) (*Engine, error) {
	if providers == nil {
		return nil, fmt.Errorf("provider registry is required")
	}
	return &Engine{providers: providers}, nil
}

func (e *Engine) Open(ctx context.Context, principal Principal, key protocol.ResourceKey, resume *protocol.ResumeToken) (OpenedResource, error) {
	if e == nil || e.providers == nil {
		return OpenedResource{}, fmt.Errorf("provider registry is required")
	}
	return e.providers.Open(ctx, principal, key, resume)
}
