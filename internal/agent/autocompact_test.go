package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/storage"
	"github.com/rexzhao/simple-agent/model"
)

func TestEstimateTextTokens(t *testing.T) {
	if got := estimateTextTokens(""); got != 0 {
		t.Fatalf("empty = %d", got)
	}
	if got := estimateTextTokens("abcd"); got != 1 {
		t.Fatalf("abcd = %d, want 1 (4 ascii/4)", got)
	}
	if got := estimateTextTokens("abc"); got != 1 {
		t.Fatalf("abc = %d, want 1 (ceil)", got)
	}
	if got := estimateTextTokens("工"); got != 1 {
		t.Fatalf("CJK char = %d, want 1 (one token per character)", got)
	}
	if got := estimateTextTokens("ab工"); got != 2 {
		t.Fatalf("mixed = %d, want 2", got)
	}
}

func TestEstimateRequestTokensIncludesOverheads(t *testing.T) {
	small := estimateRequestTokens(model.Request{Messages: []model.Message{{Role: model.MessageRoleUser, Content: "hi"}}})
	if small <= requestOverheadTokens {
		t.Fatalf("small request estimate = %d, want overheads included", small)
	}
	withTools := estimateRequestTokens(model.Request{
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "hi"}},
		Tools:    []model.ToolDefinition{{Name: "shell", Description: "run a command"}},
	})
	if withTools <= small {
		t.Fatalf("tools not counted: %d <= %d", withTools, small)
	}
}

func TestResolveContextBudget(t *testing.T) {
	budget := resolveContextBudget(100_000, 10_000)
	// capacity=90000, safety=1800, hard=88200, headroom=17640, soft=70560
	if budget.HardInputLimit != 88_200 || budget.SoftInputLimit != 70_560 {
		t.Fatalf("budget = %+v, want hard 88200 soft 70560", budget)
	}
	if budget.SoftInputLimit > budget.HardInputLimit {
		t.Fatalf("soft limit above hard: %+v", budget)
	}

	// A missing window falls back to the conservative default; a missing
	// output limit reserves a bounded fraction of the window.
	fallback := resolveContextBudget(0, 0)
	if fallback.HardInputLimit <= 0 || fallback.SoftInputLimit <= 0 {
		t.Fatalf("fallback budget degenerate: %+v", fallback)
	}

	// Degenerate configurations must still produce a budget: a tiny
	// window without an output limit caps the reserve at a quarter of
	// the window, and an output limit larger than the window falls back
	// the same way — a zero budget would silently disable compaction.
	tiny := resolveContextBudget(2_000, 0)
	if tiny.HardInputLimit <= 0 || tiny.SoftInputLimit <= 0 {
		t.Fatalf("tiny-window budget degenerate: %+v", tiny)
	}
	oversized := resolveContextBudget(1_000, 2_000)
	if oversized.HardInputLimit <= 0 || oversized.SoftInputLimit <= 0 {
		t.Fatalf("oversized-output budget degenerate: %+v", oversized)
	}
	// Windows smaller than the safety floor must not collapse to a zero
	// budget either: the safety margin yields to half the capacity.
	for _, window := range []int{200, 400, 600} {
		if budget := resolveContextBudget(window, 0); budget.HardInputLimit <= 0 {
			t.Fatalf("window %d budget degenerate: %+v", window, budget)
		}
	}
}

func TestCompactionReclaimsEnough(t *testing.T) {
	if !compactionReclaimsEnough(100_000, 1_000) {
		t.Fatal("large reclaim rejected")
	}
	// The minimum meaningful reclaim is 2048 tokens or 5% of the request.
	if compactionReclaimsEnough(100_000, 99_000) {
		t.Fatal("1% reclaim accepted")
	}
	if compactionReclaimsEnough(0, 0) != true {
		t.Fatal("degenerate input rejected")
	}
}

// autoCompactAgent builds an agent whose sessions resolve to a small
// budget so tests can cross the pressure limits with plain strings.
func autoCompactAgent(t *testing.T, provider model.Provider, executor ToolExecutor, apply func(*Options)) (*Agent, *Session) {
	t.Helper()
	return newTestAgent(t, provider, executor, func(options *Options) {
		options.ModelResolver = NewStaticResolver(provider, nil, 100_000, 10_000, "")
		options.AutoCompact = true
		if apply != nil {
			apply(options)
		}
	})
}

func compactedRecords(t *testing.T, session *Session) []storage.Record {
	t.Helper()
	records := session.SnapshotStream().Records
	var compacted []storage.Record
	for _, record := range records {
		if record.Type == storage.TypeContextCompacted {
			compacted = append(compacted, record)
		}
	}
	return compacted
}

func TestAutoCompactTriggersOnSoftPressureBeforeTurn(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "first answer"}},
		{model.TextDeltaEvent{Text: "summary of everything"}},
		{model.TextDeltaEvent{Text: "answer after compaction"}},
	}}
	_, session := autoCompactAgent(t, provider, nil, nil)

	if err := session.Send("small question"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)

	// A huge CJK input (~75k estimated tokens) crosses the soft limit.
	events := sendCollectEvents(t, session, strings.Repeat("工", 75_000))

	compacted := compactedRecords(t, session)
	if len(compacted) != 1 {
		t.Fatalf("auto compactions = %d, want 1", len(compacted))
	}
	if !strings.HasSuffix(compacted[0].Content, "summary of everything") {
		t.Fatalf("summary = %q", compacted[0].Content)
	}

	// The compaction events rode the run's event stream.
	var started, completed bool
	for _, event := range events {
		switch event := event.(type) {
		case model.CompactionStartedEvent:
			started = event.ItemID != ""
		case model.CompactionCompletedEvent:
			completed = event.ItemID == compacted[0].ItemID
		}
	}
	if !started || !completed {
		t.Fatalf("compaction events missing: started=%v completed=%v", started, completed)
	}

	// The continuation request carries the summary, not the huge input.
	if len(provider.requests) != 3 {
		t.Fatalf("provider requests = %d, want 3", len(provider.requests))
	}
	continuation := provider.requests[2].Messages
	if len(continuation) != 1 || continuation[0].Role != model.MessageRoleUser || !strings.HasSuffix(continuation[0].Content, "summary of everything") {
		t.Fatalf("continuation = %+v, want the summary as the sole message", continuation)
	}
	// The final answer sits after the summary in the folded context.
	snapshot := session.Snapshot()
	if len(snapshot.Context) != 2 || !strings.Contains(snapshot.Context[1].Content, "answer after compaction") {
		t.Fatalf("context after run = %+v", snapshot.Context)
	}
}

func TestAutoCompactSoftCooldownDefersSecondCompaction(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "first answer"}},
		{model.TextDeltaEvent{Text: "summary one"}},
		{model.TextDeltaEvent{Text: "answer two"}},
		{model.TextDeltaEvent{Text: "answer three"}},
	}}
	_, session := autoCompactAgent(t, provider, nil, nil)

	if err := session.Send("small"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)
	if err := session.Send(strings.Repeat("工", 75_000)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)
	if got := len(compactedRecords(t, session)); got != 1 {
		t.Fatalf("compactions after first big turn = %d, want 1", got)
	}

	// A second oversized turn hits soft pressure again, but only one new
	// user input arrived since the compaction: the cooldown defers.
	if err := session.Send(strings.Repeat("工", 75_000)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)
	if got := len(compactedRecords(t, session)); got != 1 {
		t.Fatalf("compactions during cooldown = %d, want still 1", got)
	}
}

func TestAutoCompactHardPressureFailsWhenSummaryCannotRelieve(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "first answer"}},
		// The summary comes back as huge as the context: hard pressure
		// compacts, but the replacement still exceeds the safe limit.
		{model.TextDeltaEvent{Text: strings.Repeat("工", 95_000)}},
	}}
	_, session := autoCompactAgent(t, provider, nil, nil)

	if err := session.Send("small"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)
	if err := session.Send(strings.Repeat("工", 95_000)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)

	records := compactedRecords(t, session)
	if len(records) != 1 {
		t.Fatalf("hard compactions = %d, want 1 (committed despite the failure)", len(records))
	}
	all := session.SnapshotStream().Records
	last := all[len(all)-1]
	if last.Type != storage.TypeRunFailed || !strings.Contains(last.Error, "could not reclaim enough context") {
		t.Fatalf("run outcome = %+v, want run.failed with the reclaim error", last)
	}
}

func TestAutoCompactSkipsWhenReclaimIsChurn(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "first answer"}},
		// The summary is nearly as large as the context: reclaiming under
		// the minimum would be churn, so nothing is committed.
		{model.TextDeltaEvent{Text: strings.Repeat("工", 74_500)}},
		{model.TextDeltaEvent{Text: "answer two"}},
	}}
	_, session := autoCompactAgent(t, provider, nil, nil)

	if err := session.Send("small"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)
	if err := session.Send(strings.Repeat("工", 75_000)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)

	if got := len(compactedRecords(t, session)); got != 0 {
		t.Fatalf("churn compaction committed: %d records", got)
	}
	// The turn still ran to completion from the un-compacted context.
	snapshot := session.Snapshot()
	if snapshot.Context[len(snapshot.Context)-1].Content != "answer two" {
		t.Fatalf("turn did not complete: %+v", snapshot.Context[len(snapshot.Context)-1])
	}
}

func TestAutoCompactDisabledLeavesContextAlone(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "first answer"}},
		{model.TextDeltaEvent{Text: "answer two"}},
	}}
	_, session := autoCompactAgent(t, provider, nil, func(options *Options) {
		options.AutoCompact = false
	})

	if err := session.Send("small"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)
	if err := session.Send(strings.Repeat("工", 95_000)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)

	if got := len(compactedRecords(t, session)); got != 0 {
		t.Fatalf("compaction with auto disabled: %d records", got)
	}
}

func TestSummarizeContextRetriesTransientFailures(t *testing.T) {
	originalBackoff := providerRetryBackoff
	providerRetryBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { providerRetryBackoff = originalBackoff })

	retryable := &model.ProviderError{Message: "server_error: temporary failure (code server_error)"}
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "answer to hello"}},
		{model.ErrorEvent{Err: retryable}},
		{model.TextDeltaEvent{Text: "recovered summary"}},
	}}
	_, session := autoCompactAgent(t, provider, nil, nil)

	if err := session.Send("hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)

	// No pressure in this small conversation: exercise the retry path
	// through the manual compact.
	if err := session.Compact(); err != nil {
		t.Fatalf("Compact after transient failure: %v", err)
	}
	if got := len(compactedRecords(t, session)); got != 1 {
		t.Fatalf("compactions = %d, want 1 after retry", got)
	}
}

func TestAutoCompactTriggersMidTurnAfterToolBatch(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		// Iteration 1: an assistant message with a tool call.
		{model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "c1", Name: "echo", Arguments: `{}`}}},
		// The mid-turn compaction summary.
		{model.TextDeltaEvent{Text: "mid-turn summary"}},
		// Iteration 2: the final answer.
		{model.TextDeltaEvent{Text: "final answer"}},
	}}
	executor := &fakeToolExecutor{result: model.ToolResult{Content: strings.Repeat("果", 16_000)}}
	_, session := autoCompactAgent(t, provider, executor, nil)

	// The user input (~60k estimated tokens) plus the tool result (~16k)
	// crosses the soft limit only after the tool batch: the pre-turn
	// check sees ~60k and stays quiet.
	events := sendCollectEvents(t, session, strings.Repeat("工", 60_000))

	compacted := compactedRecords(t, session)
	if len(compacted) != 1 || !strings.HasSuffix(compacted[0].Content, "mid-turn summary") {
		t.Fatalf("mid-turn compactions = %+v, want one mid-turn summary", compacted)
	}

	// The compaction events rode the run's stream, after the tool result.
	var pressureOrder []string
	for _, event := range events {
		switch event.(type) {
		case model.ToolResultEvent:
			pressureOrder = append(pressureOrder, "tool")
		case model.CompactionCompletedEvent:
			pressureOrder = append(pressureOrder, "compact")
		case model.AssistantMessageCompletedEvent:
			pressureOrder = append(pressureOrder, "assistant")
		}
	}
	// assistant (iteration 1, with the tool call), tool (result),
	// compact (mid-turn), assistant (final).
	if len(pressureOrder) != 4 || pressureOrder[0] != "assistant" || pressureOrder[1] != "tool" ||
		pressureOrder[2] != "compact" || pressureOrder[3] != "assistant" {
		t.Fatalf("event order = %v, want assistant → tool → compact → assistant", pressureOrder)
	}

	// The iteration-2 request continues from the summary alone: the
	// absorbed assistant+tool exchange is gone.
	if len(provider.requests) != 3 {
		t.Fatalf("provider requests = %d, want 3", len(provider.requests))
	}
	continuation := provider.requests[2].Messages
	if len(continuation) != 1 || continuation[0].Role != model.MessageRoleUser || !strings.HasSuffix(continuation[0].Content, "mid-turn summary") {
		t.Fatalf("iteration-2 request = %+v, want the summary as the sole message", continuation)
	}
}

func TestEstimateRequestTokensCountsToolSchema(t *testing.T) {
	without := estimateRequestTokens(model.Request{Messages: []model.Message{{Role: model.MessageRoleUser, Content: "hi"}}})
	with := estimateRequestTokens(model.Request{
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "hi"}},
		Tools:    []model.ToolDefinition{{Name: "shell", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}}}},
	})
	if with <= without+8 { // name + description + schema text
		t.Fatalf("input schema not counted: %d vs %d", with, without)
	}
}
