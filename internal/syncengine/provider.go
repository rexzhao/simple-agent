package syncengine

import (
	"context"
	"fmt"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

// Principal is the caller identity supplied to a resource provider. The sync
// engine does not interpret the identifier; authorization remains provider
// owned.
type Principal struct {
	ID string
}

// ResourceProvider is the provider contract for one closed resource type.
//
// Open is one owner-barrier operation. While holding the resource owner's
// serialization lock, an implementation must capture the immutable snapshot,
// the journal's stable epoch/sequence and ResumeDecision, and register the
// bounded live delivery source. It must release that lock before returning to
// delivery code. A mutation after the barrier therefore cannot fall between
// snapshot/decision capture and live registration.
//
// The returned Snapshot is always the barrier view. The gateway sends it for a
// no-token snapshot or a resync decision; for current/replay decisions it
// sends the decision's replay entries (if any) and then the live source.
type ResourceProvider interface {
	Type() protocol.ResourceType
	Authorize(ctx context.Context, principal Principal, key protocol.ResourceKey) error
	Open(ctx context.Context, key protocol.ResourceKey, resume *protocol.ResumeToken) (OpenedResource, error)
}

// RunResumeProvider is an optional extension for resources that expose a
// transient active-run stream. Keeping it optional preserves the durable-only
// provider contract for every other resource type.
type RunResumeProvider interface {
	OpenWithRunResume(ctx context.Context, key protocol.ResourceKey, resume *protocol.ResumeToken, activeRunResume *protocol.RunResumeToken) (OpenedResource, error)
}

// OpenedResource is the complete result of the provider's atomic open
// barrier. Decision.ToSequence and Sequence are the same barrier sequence.
// Changes contains only JournalEntry values strictly after that sequence and
// carries epoch, sequence, and previous_sequence on every value. Terminal is
// the typed end/desync signal for that bounded source.
type OpenedResource struct {
	Snapshot         Snapshot
	StreamEpoch      string
	Sequence         uint64
	Decision         ResumeDecision
	LiveFromSequence uint64
	Changes          <-chan JournalEntry
	Terminal         <-chan LiveTerminal
	TransientReplay  []TransientEvent
	Transient        TransientDelivery
	TransientResync  string
	Close            func()
}

// Snapshot is an immutable-at-the-boundary resource view. The engine copies
// the content when accepting it and callers receive copies from Clone/Inline.
type Snapshot struct {
	Content          SnapshotContent
	ResourceRevision protocol.ResourceRevision
}

func (s Snapshot) Clone() Snapshot {
	return Snapshot{
		Content:          s.Content.Clone(),
		ResourceRevision: s.ResourceRevision,
	}
}

func (s Snapshot) Validate() error {
	if err := protocol.ValidateResourceRevision(s.ResourceRevision); err != nil {
		return fmt.Errorf("%w: snapshot resource revision: %v", ErrInvalidSnapshot, err)
	}
	return s.Content.Validate()
}

// SnapshotContent is a strict inline/blob union. Blob storage and serving are
// deliberately outside this package.
type SnapshotContent struct {
	Inline []byte
	Blob   *protocol.BlobDescriptor
}

func NewInlineSnapshotContent(content []byte) SnapshotContent {
	return SnapshotContent{Inline: append([]byte(nil), content...)}
}

func NewBlobSnapshotContent(descriptor protocol.BlobDescriptor) SnapshotContent {
	return SnapshotContent{Blob: &descriptor}
}

func (c SnapshotContent) Clone() SnapshotContent {
	clone := SnapshotContent{Inline: append([]byte(nil), c.Inline...)}
	if c.Blob != nil {
		descriptor := *c.Blob
		clone.Blob = &descriptor
	}
	return clone
}

// InlineBytes returns a copy so a snapshot cannot be mutated through a
// returned byte slice.
func (c SnapshotContent) InlineBytes() []byte {
	return append([]byte(nil), c.Inline...)
}

func (c SnapshotContent) BlobDescriptor() (protocol.BlobDescriptor, bool) {
	if c.Blob == nil {
		return protocol.BlobDescriptor{}, false
	}
	return *c.Blob, true
}

func (c SnapshotContent) Validate() error {
	if err := protocol.ValidateSnapshotContent(protocol.SnapshotContent{
		Inline: c.Inline,
		Blob:   c.Blob,
	}); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSnapshot, err)
	}
	return nil
}

// ResourceChange is a committed durable resource change. Operation details
// remain an opaque, validated protocol DTO; the engine never models them as
// map[string]any or rewrites resource-specific schemas.
type ResourceChange struct {
	ResourceRevision protocol.ResourceRevision
	Operations       []protocol.ChangeOperation
}

func (c ResourceChange) Clone() ResourceChange {
	clone := ResourceChange{
		ResourceRevision: c.ResourceRevision,
		Operations:       make([]protocol.ChangeOperation, len(c.Operations)),
	}
	for i, operation := range c.Operations {
		clone.Operations[i] = protocol.ChangeOperation{
			Op:  operation.Op,
			Raw: append([]byte(nil), operation.Raw...),
		}
	}
	return clone
}

func (c ResourceChange) Validate() error {
	if err := protocol.ValidateResourceRevision(c.ResourceRevision); err != nil {
		return fmt.Errorf("%w: change resource revision: %v", ErrInvalidChange, err)
	}
	if len(c.Operations) == 0 {
		return fmt.Errorf("%w: at least one operation is required", ErrInvalidChange)
	}
	for index, operation := range c.Operations {
		if err := protocol.ValidateChangeOperation(operation); err != nil {
			return fmt.Errorf("%w: operation %d: %v", ErrInvalidChange, index, err)
		}
	}
	return nil
}
