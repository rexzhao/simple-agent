package execution

import (
	"sync"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

// activeQueueSnapshot records one run.prompt_queue emission with the full
// prompt payload so tests can assert order and steer flags.
type activeQueueSnapshot struct {
	Prompts []activePrompt
}

// collectActiveQueueSnapshots returns an emit func that records every
// run.prompt_queue snapshot (deep-copying the prompt list) in emission order.
func collectActiveQueueSnapshots(mu *sync.Mutex, snapshots *[]activeQueueSnapshot) func(SessionStreamEvent) {
	return func(event SessionStreamEvent) {
		if event == nil {
			return
		}
		if eventType, _ := event["type"].(string); eventType != "run.prompt_queue" {
			return
		}
		raw, _ := event["prompts"].([]activePrompt)
		mu.Lock()
		*snapshots = append(*snapshots, activeQueueSnapshot{Prompts: append([]activePrompt(nil), raw...)})
		mu.Unlock()
	}
}

func queueContents(prompts []activePrompt) []string {
	contents := make([]string, 0, len(prompts))
	for _, prompt := range prompts {
		contents = append(contents, prompt.Content)
	}
	return contents
}

func lastActiveQueueSnapshot(snapshots []activeQueueSnapshot) []activePrompt {
	if len(snapshots) == 0 {
		return nil
	}
	return snapshots[len(snapshots)-1].Prompts
}

// TestSetActivePromptSteerPromotesToTop verifies the core queue invariant:
// promoting a queued prompt to steer moves it above every plain queued prompt
// while keeping relative order, and demoting returns it below remaining
// steers. Every mutation publishes a full snapshot carrying the steer flag.
func TestSetActivePromptSteerPromotesToTop(t *testing.T) {
	var mu sync.Mutex
	var snapshots []activeQueueSnapshot
	run := &SessionRun{accepting: true}
	run.setActiveTurn("turn-1", collectActiveQueueSnapshots(&mu, &snapshots))

	for _, content := range []string{"first", "second", "third"} {
		if err := run.AppendActive(content); err != nil {
			t.Fatalf("AppendActive(%q) error = %v", content, err)
		}
	}

	if !run.SetActivePromptSteer("ap-2", true) {
		t.Fatal("SetActivePromptSteer(ap-2, true) = false, want true")
	}
	got := lastActiveQueueSnapshot(snapshots)
	if !sameStringSlice(queueContents(got), []string{"second", "first", "third"}) {
		t.Fatalf("queue after steer = %#v, want second promoted to top", queueContents(got))
	}
	if !got[0].Steer || got[1].Steer || got[2].Steer {
		t.Fatalf("steer flags after promotion = %#v, want only the first prompt steered", got)
	}

	// A second steer joins the steer group behind the existing one.
	if !run.SetActivePromptSteer("ap-3", true) {
		t.Fatal("SetActivePromptSteer(ap-3, true) = false, want true")
	}
	got = lastActiveQueueSnapshot(snapshots)
	if !sameStringSlice(queueContents(got), []string{"second", "third", "first"}) {
		t.Fatalf("queue after second steer = %#v, want steers second,third on top", queueContents(got))
	}

	// Demoting drops the prompt back into the plain queue at the top of its
	// group: the stable partition keeps its relative position, so a
	// promote→demote round-trip restores the original order.
	if !run.SetActivePromptSteer("ap-2", false) {
		t.Fatal("SetActivePromptSteer(ap-2, false) = false, want true")
	}
	got = lastActiveQueueSnapshot(snapshots)
	if !sameStringSlice(queueContents(got), []string{"third", "second", "first"}) {
		t.Fatalf("queue after demote = %#v, want second back atop the plain queue", queueContents(got))
	}
	if got[0].Steer != true || got[1].Steer || got[2].Steer {
		t.Fatalf("steer flags after demote = %#v, want only third steered", got)
	}

	if run.SetActivePromptSteer("ap-missing", true) {
		t.Fatal("SetActivePromptSteer(missing) = true, want false")
	}
	var nilRun *SessionRun
	if nilRun.SetActivePromptSteer("ap-1", true) {
		t.Fatal("SetActivePromptSteer on nil run = true, want false")
	}
}

// TestMoveActivePromptReordersWithinGroup verifies up/down reordering of
// plain queued prompts, clamped at the queue ends.
func TestMoveActivePromptReordersWithinGroup(t *testing.T) {
	run := &SessionRun{accepting: true}
	run.setActiveTurn("turn-1", nil)
	for _, content := range []string{"a", "b", "c"} {
		if err := run.AppendActive(content); err != nil {
			t.Fatalf("AppendActive(%q) error = %v", content, err)
		}
	}

	if !run.MoveActivePrompt("ap-3", -1) {
		t.Fatal("MoveActivePrompt(ap-3, up) = false, want true")
	}
	if got := queueContents(run.activeQueue); !sameStringSlice(got, []string{"a", "c", "b"}) {
		t.Fatalf("queue after move up = %#v, want a,c,b", got)
	}
	if !run.MoveActivePrompt("ap-3", -1) {
		t.Fatal("MoveActivePrompt(ap-3, up) = false, want true")
	}
	if got := queueContents(run.activeQueue); !sameStringSlice(got, []string{"c", "a", "b"}) {
		t.Fatalf("queue after second move up = %#v, want c,a,b", got)
	}
	// Clamped at the top: still reports success, order unchanged.
	if !run.MoveActivePrompt("ap-3", -1) {
		t.Fatal("MoveActivePrompt(ap-3, up clamped) = false, want true")
	}
	if got := queueContents(run.activeQueue); !sameStringSlice(got, []string{"c", "a", "b"}) {
		t.Fatalf("queue after clamped move = %#v, want unchanged c,a,b", got)
	}
	if !run.MoveActivePrompt("ap-1", 1) {
		t.Fatal("MoveActivePrompt(ap-1, down) = false, want true")
	}
	if got := queueContents(run.activeQueue); !sameStringSlice(got, []string{"c", "b", "a"}) {
		t.Fatalf("queue after move down = %#v, want c,b,a", got)
	}
	if run.MoveActivePrompt("ap-missing", 1) {
		t.Fatal("MoveActivePrompt(missing) = true, want false")
	}
	var nilRun *SessionRun
	if nilRun.MoveActivePrompt("ap-1", 1) {
		t.Fatal("MoveActivePrompt on nil run = true, want false")
	}
}

// TestMoveActivePromptClampsAtGroupBoundary verifies that reordering can
// never violate the steers-on-top invariant: plain queued prompts cannot move
// above the steer group and steers cannot sink below it.
func TestMoveActivePromptClampsAtGroupBoundary(t *testing.T) {
	run := &SessionRun{accepting: true}
	run.setActiveTurn("turn-1", nil)
	for _, content := range []string{"queue-1", "queue-2"} {
		if err := run.AppendActive(content); err != nil {
			t.Fatalf("AppendActive(%q) error = %v", content, err)
		}
	}
	if !run.SetActivePromptSteer("ap-2", true) {
		t.Fatal("SetActivePromptSteer(ap-2, true) = false, want true")
	}
	// Queue is now [queue-2 (steer), queue-1].
	if got := queueContents(run.activeQueue); !sameStringSlice(got, []string{"queue-2", "queue-1"}) {
		t.Fatalf("queue after steer = %#v, want queue-2 on top", got)
	}

	// The plain queued prompt cannot climb into the steer group.
	if !run.MoveActivePrompt("ap-1", -1) {
		t.Fatal("MoveActivePrompt(ap-1, up) = false, want true")
	}
	if got := queueContents(run.activeQueue); !sameStringSlice(got, []string{"queue-2", "queue-1"}) {
		t.Fatalf("queue after boundary move = %#v, want unchanged", got)
	}
	// The steer cannot sink into the plain group.
	if !run.MoveActivePrompt("ap-2", 1) {
		t.Fatal("MoveActivePrompt(ap-2, down) = false, want true")
	}
	if got := queueContents(run.activeQueue); !sameStringSlice(got, []string{"queue-2", "queue-1"}) {
		t.Fatalf("queue after steer move down = %#v, want unchanged", got)
	}

	// Inside the steer group reordering still works.
	if !run.SetActivePromptSteer("ap-1", true) {
		t.Fatal("SetActivePromptSteer(ap-1, true) = false, want true")
	}
	if !run.MoveActivePrompt("ap-1", -1) {
		t.Fatal("MoveActivePrompt(ap-1, up in steer group) = false, want true")
	}
	if got := queueContents(run.activeQueue); !sameStringSlice(got, []string{"queue-1", "queue-2"}) {
		t.Fatalf("queue after steer group reorder = %#v, want queue-1,queue-2", got)
	}
}

// TestSteerPromptsDrainFirstIntoActiveTurn verifies that the active prompt
// drain injects steer prompts ahead of earlier plain queued prompts.
func TestSteerPromptsDrainFirstIntoActiveTurn(t *testing.T) {
	run := &SessionRun{accepting: true}
	run.setActiveTurn("turn-1", nil)
	for _, content := range []string{"first", "second"} {
		if err := run.AppendActive(content); err != nil {
			t.Fatalf("AppendActive(%q) error = %v", content, err)
		}
	}
	if !run.SetActivePromptSteer("ap-2", true) {
		t.Fatal("SetActivePromptSteer(ap-2, true) = false, want true")
	}

	drain := run.activePromptDrain()
	messages := drain(SessionActivePromptCheckpointBeforeTerminal)
	contents := make([]string, 0, len(messages))
	for _, message := range messages {
		if message.Role != model.MessageRoleUser {
			t.Fatalf("drained message role = %q, want user", message.Role)
		}
		contents = append(contents, message.Content)
	}
	if !sameStringSlice(contents, []string{"second", "first"}) {
		t.Fatalf("drained contents = %#v, want steer second drained first", contents)
	}
}

// TestFollowUpDrainKeepsSteerOrderAndDropsStrict verifies the no-loss Web
// policy under the new ordering: Web steer prompts left behind at settle run
// as follow-up turns ahead of plain queued prompts, while strict agent steers
// are still dropped rather than converted.
func TestFollowUpDrainKeepsSteerOrderAndDropsStrict(t *testing.T) {
	run := &SessionRun{accepting: true}
	run.setActiveTurn("turn-1", nil)
	if err := run.AppendActive("plain"); err != nil {
		t.Fatalf("AppendActive() error = %v", err)
	}
	if err := run.AppendActive("priority"); err != nil {
		t.Fatalf("AppendActive() error = %v", err)
	}
	if err := run.TrySteer("agent steer"); err != nil {
		t.Fatalf("TrySteer() error = %v", err)
	}
	if !run.SetActivePromptSteer("ap-2", true) {
		t.Fatal("SetActivePromptSteer(ap-2, true) = false, want true")
	}
	// Queue order: agent steer (strict), priority (web steer), plain.
	if got := queueContents(run.activeQueue); !sameStringSlice(got, []string{"agent steer", "priority", "plain"}) {
		t.Fatalf("queue = %#v, want steers ahead of plain", got)
	}

	drained := run.drainFollowUpQueue()
	if !sameStringSlice(drained, []string{"priority", "plain"}) {
		t.Fatalf("follow-up drain = %#v, want web steer first, strict steer dropped", drained)
	}
	if len(run.activeQueue) != 0 {
		t.Fatalf("activeQueue after follow-up drain = %#v, want empty", run.activeQueue)
	}
}

// TestTrySteerSortsAheadOfPlainQueue verifies that a strict agent steer
// joining a queue that already holds plain Web prompts is prioritized above
// them, consistent with the steers-on-top invariant.
func TestTrySteerSortsAheadOfPlainQueue(t *testing.T) {
	run := &SessionRun{accepting: true}
	run.setActiveTurn("turn-1", nil)
	if err := run.AppendActive("queued"); err != nil {
		t.Fatalf("AppendActive() error = %v", err)
	}
	if err := run.TrySteer("steer now"); err != nil {
		t.Fatalf("TrySteer() error = %v", err)
	}
	if got := queueContents(run.activeQueue); !sameStringSlice(got, []string{"steer now", "queued"}) {
		t.Fatalf("queue = %#v, want strict steer ahead of plain queued prompt", got)
	}
	if !run.activeQueue[0].Steer || !run.activeQueue[0].strict {
		t.Fatalf("strict steer flags = %+v, want Steer and strict set", run.activeQueue[0])
	}
}
