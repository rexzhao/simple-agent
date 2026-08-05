package syncengine

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

// JournalConfig defines hard bounds for one resource stream. Both limits are
// enforced. A change larger than MaxBytes is rejected instead of evicting the
// entire journal or silently creating an unreplayable sequence.
type JournalConfig struct {
	StreamEpoch     string
	InitialSequence uint64
	MaxEntries      int
	MaxBytes        int
}

// JournalEntry is the internally sequenced form of a committed resource
// change. The resource key is held by the owning stream, so it is not repeated
// here.
type JournalEntry struct {
	StreamEpoch      string
	Sequence         uint64
	PreviousSequence uint64
	Change           ResourceChange
	SizeBytes        int
}

func (e JournalEntry) Clone() JournalEntry {
	e.Change = e.Change.Clone()
	return e
}

// JournalStats exposes bounds and the retained sequence window without
// exposing mutable entries.
type JournalStats struct {
	StreamEpoch   string
	FirstSequence uint64
	LastSequence  uint64
	Count         int
	Bytes         int
	MaxEntries    int
	MaxBytes      int
}

// Journal stores only the bounded replay window. It has no subscriber
// delivery queue: Append never waits for a slow consumer, and later gateway
// layers must explicitly choose a bounded delivery policy (overflow means
// resync rather than silent loss).
type Journal struct {
	mu sync.RWMutex

	epoch      string
	last       uint64
	maxEntries int
	maxBytes   int
	entries    []JournalEntry
	bytes      int
}

func NewJournal(config JournalConfig) (*Journal, error) {
	if strings.TrimSpace(config.StreamEpoch) == "" {
		return nil, ErrInvalidEpoch
	}
	if config.MaxEntries <= 0 || config.MaxBytes <= 0 {
		return nil, fmt.Errorf("journal limits must be positive")
	}
	return &Journal{
		epoch:      config.StreamEpoch,
		last:       config.InitialSequence,
		maxEntries: config.MaxEntries,
		maxBytes:   config.MaxBytes,
	}, nil
}

// NewBoundedJournal is a compact constructor for callers that do not need a
// non-zero initial sequence.
func NewBoundedJournal(streamEpoch string, maxEntries, maxBytes int) (*Journal, error) {
	return NewJournal(JournalConfig{
		StreamEpoch: streamEpoch,
		MaxEntries:  maxEntries,
		MaxBytes:    maxBytes,
	})
}

func (j *Journal) Append(change ResourceChange) (JournalEntry, error) {
	if j == nil {
		return JournalEntry{}, ErrJournalClosed
	}
	if err := change.Validate(); err != nil {
		return JournalEntry{}, err
	}
	encoded, err := json.Marshal(struct {
		ResourceRevision protocol.ResourceRevision  `json:"resource_revision"`
		Operations       []protocol.ChangeOperation `json:"operations"`
	}{
		ResourceRevision: change.ResourceRevision,
		Operations:       change.Operations,
	})
	if err != nil {
		return JournalEntry{}, fmt.Errorf("encode change: %w", err)
	}
	if len(encoded) > j.maxBytes {
		return JournalEntry{}, fmt.Errorf("%w: %d > %d bytes", ErrChangeTooLarge, len(encoded), j.maxBytes)
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	if j.last == math.MaxUint64 {
		return JournalEntry{}, ErrSequenceExhausted
	}
	entry := JournalEntry{
		StreamEpoch:      j.epoch,
		Sequence:         j.last + 1,
		PreviousSequence: j.last,
		Change:           change.Clone(),
		SizeBytes:        len(encoded),
	}
	j.last = entry.Sequence
	j.entries = append(j.entries, entry)
	j.bytes += entry.SizeBytes
	for len(j.entries) > j.maxEntries || j.bytes > j.maxBytes {
		j.bytes -= j.entries[0].SizeBytes
		j.entries[0] = JournalEntry{}
		j.entries = j.entries[1:]
	}
	return entry.Clone(), nil
}

// Clear drops retained replay data while preserving epoch and the current
// sequence. A resume at the current sequence is still current; an older
// resume is too old because there is no retained path to it.
func (j *Journal) Clear() {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = nil
	j.bytes = 0
}

// Reset starts a new stream epoch and resets its sequence to zero. This is the
// process/rebuild boundary at which clients must snapshot; no old entry is
// replayable across the reset.
func (j *Journal) Reset(streamEpoch string) error {
	if j == nil {
		return ErrJournalClosed
	}
	if strings.TrimSpace(streamEpoch) == "" {
		return ErrInvalidEpoch
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if streamEpoch == j.epoch {
		return ErrEpochUnchanged
	}
	j.epoch = streamEpoch
	j.last = 0
	j.entries = nil
	j.bytes = 0
	return nil
}

func (j *Journal) Epoch() string {
	if j == nil {
		return ""
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.epoch
}

func (j *Journal) LastSequence() uint64 {
	if j == nil {
		return 0
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.last
}

func (j *Journal) Stats() JournalStats {
	if j == nil {
		return JournalStats{}
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	stats := JournalStats{
		StreamEpoch:  j.epoch,
		LastSequence: j.last,
		Count:        len(j.entries),
		Bytes:        j.bytes,
		MaxEntries:   j.maxEntries,
		MaxBytes:     j.maxBytes,
	}
	if len(j.entries) > 0 {
		stats.FirstSequence = j.entries[0].Sequence
	}
	return stats
}

// ResumeClassification describes the reason for a replay/current/resync
// decision. It is intentionally not a wire protocol DTO.
type ResumeClassification string

const (
	ResumeNoToken         ResumeClassification = "no_token"
	ResumeCurrentExact    ResumeClassification = "current_exact"
	ResumeCurrent         ResumeClassification = ResumeCurrentExact
	ResumeExact           ResumeClassification = ResumeCurrentExact
	ResumeReplayAvailable ResumeClassification = "replay_available"
	ResumeEpochMismatch   ResumeClassification = "epoch_mismatch"
	ResumeTooOld          ResumeClassification = "too_old"
	ResumeAhead           ResumeClassification = "ahead"
	ResumeInvalid         ResumeClassification = "invalid"
)

type SyncAction string

const (
	SyncActionSnapshot SyncAction = "snapshot"
	SyncActionCurrent  SyncAction = "current"
	SyncActionReplay   SyncAction = "replay"
	SyncActionResync   SyncAction = "resync"
)

type ResyncReason string

const (
	ResyncReasonNoResume             ResyncReason = "no_resume"
	ResyncReasonEpochMismatch        ResyncReason = "epoch_mismatch"
	ResyncReasonTooOld               ResyncReason = "too_old"
	ResyncReasonAhead                ResyncReason = "ahead"
	ResyncReasonInvalidResume        ResyncReason = "invalid_resume"
	ResyncReasonJournalDiscontinuity ResyncReason = "journal_discontinuity"
)

// ResumeDecision is a typed sync decision. A Resync action is a decision, not
// a prebuilt resync_required wire message; the gateway chooses how to encode
// it later.
type ResumeDecision struct {
	Action         SyncAction
	Classification ResumeClassification
	Reason         ResyncReason
	StreamEpoch    string
	FromSequence   uint64
	ToSequence     uint64
	Entries        []JournalEntry
}

func (d ResumeDecision) IsResync() bool {
	return d.Action == SyncActionResync
}

func (d ResumeDecision) Clone() ResumeDecision {
	clone := d
	clone.Entries = make([]JournalEntry, len(d.Entries))
	for i, entry := range d.Entries {
		clone.Entries[i] = entry.Clone()
	}
	return clone
}

// Decide classifies a resume token against one stable journal view. Replay
// results are deep copies, so a consumer cannot mutate the retained journal or
// another consumer's replay.
func (j *Journal) Decide(token *protocol.ResumeToken) ResumeDecision {
	if j == nil {
		return ResumeDecision{
			Action:         SyncActionResync,
			Classification: ResumeInvalid,
			Reason:         ResyncReasonInvalidResume,
		}
	}
	j.mu.RLock()
	defer j.mu.RUnlock()

	base := ResumeDecision{StreamEpoch: j.epoch, ToSequence: j.last}
	if token == nil {
		base.Action = SyncActionSnapshot
		base.Classification = ResumeNoToken
		base.Reason = ResyncReasonNoResume
		return base
	}
	if err := protocol.ValidateResumeToken(token); err != nil {
		base.Action = SyncActionResync
		base.Classification = ResumeInvalid
		base.Reason = ResyncReasonInvalidResume
		return base
	}
	if token.StreamEpoch != j.epoch {
		base.Action = SyncActionResync
		base.Classification = ResumeEpochMismatch
		base.Reason = ResyncReasonEpochMismatch
		return base
	}
	sequence, err := protocol.ParseUint64Decimal(string(token.Sequence))
	if err != nil {
		// A valid decimal outside the journal's uint64 domain cannot be a
		// valid local position and is necessarily ahead of this journal.
		base.Action = SyncActionResync
		base.Classification = ResumeAhead
		base.Reason = ResyncReasonAhead
		return base
	}
	if sequence > j.last {
		base.Action = SyncActionResync
		base.Classification = ResumeAhead
		base.Reason = ResyncReasonAhead
		return base
	}
	if sequence == j.last {
		base.Action = SyncActionCurrent
		base.Classification = ResumeCurrent
		base.FromSequence = sequence
		return base
	}
	if len(j.entries) == 0 {
		base.Action = SyncActionResync
		base.Classification = ResumeTooOld
		base.Reason = ResyncReasonTooOld
		return base
	}

	start := sequence + 1
	if start < j.entries[0].Sequence {
		base.Action = SyncActionResync
		base.Classification = ResumeTooOld
		base.Reason = ResyncReasonTooOld
		return base
	}
	entryOffset := start - j.entries[0].Sequence
	if entryOffset >= uint64(len(j.entries)) {
		base.Action = SyncActionResync
		base.Classification = ResumeTooOld
		base.Reason = ResyncReasonTooOld
		return base
	}
	entryIndex := int(entryOffset)
	if j.entries[entryIndex].Sequence != start || j.entries[entryIndex].PreviousSequence != sequence {
		base.Action = SyncActionResync
		base.Classification = ResumeTooOld
		base.Reason = ResyncReasonJournalDiscontinuity
		return base
	}
	for index := entryIndex; index < len(j.entries); index++ {
		entry := j.entries[index]
		if index > entryIndex && (entry.PreviousSequence != j.entries[index-1].Sequence || entry.Sequence != j.entries[index-1].Sequence+1) {
			base.Action = SyncActionResync
			base.Classification = ResumeTooOld
			base.Reason = ResyncReasonJournalDiscontinuity
			return base
		}
		base.Entries = append(base.Entries, entry.Clone())
	}
	base.Action = SyncActionReplay
	base.Classification = ResumeReplayAvailable
	base.FromSequence = start
	return base
}
