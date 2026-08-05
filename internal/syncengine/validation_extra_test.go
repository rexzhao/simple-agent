package syncengine

import (
	"errors"
	"testing"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

func TestValidateOpenedResourceRejectsBarrierAndReplayInconsistency(t *testing.T) {
	live, err := NewLiveSubscription("epoch", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	base := OpenedResource{
		Snapshot: Snapshot{
			Content:          NewInlineSnapshotContent([]byte(`{"items":[]}`)),
			ResourceRevision: "3",
		},
		StreamEpoch:      "epoch",
		Sequence:         2,
		LiveFromSequence: 3,
		Changes:          live.Delivery().Entries,
		Terminal:         live.Delivery().Terminal,
		Close:            live.Close,
	}
	base.Decision = ResumeDecision{
		StreamEpoch:    "epoch",
		Action:         SyncActionCurrent,
		Classification: ResumeCurrentExact,
		FromSequence:   2,
		ToSequence:     1,
	}
	if err := validateOpenedResource(base, &protocol.ResumeToken{StreamEpoch: "epoch", Sequence: "2"}); !errors.Is(err, ErrInvalidOpenedResource) {
		t.Fatalf("inconsistent decision error = %v", err)
	}
	live.Close()
}
