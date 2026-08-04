package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/model/httpstream"
)

func TestStreamEmitsToolRequestedStartedFinishedInOrder(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{}`},
				},
			},
			{
				model.TextDeltaEvent{Text: "final"},
			},
		},
	}
	executor := &fakeToolExecutor{result: model.ToolResult{Name: "echo", Content: "tool output"}}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		MaxTurns:     4,
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	var lifecycle []string
	var iterations []int
	for _, event := range gotEvents {
		switch event := event.(type) {
		case model.AgentIterationStartedEvent:
			iterations = append(iterations, event.Iteration)
		case model.ToolCallDoneEvent:
			lifecycle = append(lifecycle, "requested")
		case model.ToolStartedEvent:
			lifecycle = append(lifecycle, "started")
		case model.ToolResultEvent:
			lifecycle = append(lifecycle, "finished")
		}
	}
	if want := []string{"requested", "started", "finished"}; !reflect.DeepEqual(lifecycle, want) {
		t.Fatalf("tool lifecycle = %#v, want %#v", lifecycle, want)
	}
	if want := []int{1, 2}; !reflect.DeepEqual(iterations, want) {
		t.Fatalf("agent iterations = %#v, want %#v", iterations, want)
	}
}

func TestStreamCheckpointsVisibleAssistantOutputWithoutAppendingTwice(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{{
		model.TextDeltaEvent{Text: ""},
		model.TextDeltaEvent{Text: "a"},
		model.TextDeltaEvent{Text: "b"},
		model.TextDeltaEvent{Text: "c"},
	}}}
	publisher := &checkpointingPublisher{fakePublisher: &fakePublisher{}}
	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "hello"}},
	}, Options{
		Provider:  provider,
		TurnID:    "turn-1",
		Publisher: publisher,
		AssistantCheckpoint: &AssistantCheckpointPolicy{
			MinInterval: time.Hour,
			MinNewRunes: 2,
		},
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}
	got := collectAgentEvents(t, events)
	result, ok := <-results
	if !ok || len(result.Messages) != 2 {
		t.Fatalf("result = %#v, ok=%v, want completed message history", result, ok)
	}

	var checkpoints []eventbus.AssistantTextCheckpoint
	var ready eventbus.AssistantReady
	for _, event := range publisher.events {
		switch event := event.(type) {
		case eventbus.AssistantTextCheckpoint:
			checkpoints = append(checkpoints, event)
		case eventbus.AssistantReady:
			ready = event
		}
	}
	if len(checkpoints) != 2 {
		t.Fatalf("checkpoints = %#v, want first and threshold/final checkpoint only", checkpoints)
	}
	if checkpoints[0].Content != "a" || checkpoints[1].Content != "abc" {
		t.Fatalf("checkpoint contents = %#v, want a then abc", checkpoints)
	}
	if checkpoints[0].ItemID == "" || checkpoints[0].ItemID != checkpoints[1].ItemID || checkpoints[1].ItemID != ready.ItemID {
		t.Fatalf("checkpoint/ready identities = %#v / %q, want one stable item id", checkpoints, ready.ItemID)
	}
	if ready.Message.Content != "abc" {
		t.Fatalf("final assistant content = %q, want abc", ready.Message.Content)
	}
	var deltas []model.TextDeltaEvent
	for _, event := range got {
		if delta, ok := event.(model.TextDeltaEvent); ok {
			if delta.Text != "" {
				deltas = append(deltas, delta)
			}
		}
	}
	if len(deltas) != 3 || !deltas[0].DurableCheckpointed || deltas[1].DurableCheckpointed || !deltas[2].DurableCheckpointed {
		t.Fatalf("delta checkpoint markers = %#v, want true,false,true", deltas)
	}
}

func TestStreamStopsBeforeTransientDeltaWhenAssistantCheckpointFails(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{{model.TextDeltaEvent{Text: "partial"}}}}
	publisher := &checkpointingPublisher{fakePublisher: &fakePublisher{
		errKind: eventbus.KindAssistantCheckpoint,
		err:     errors.New("checkpoint unavailable"),
	}}
	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "hello"}},
	}, Options{Provider: provider, TurnID: "turn-1", Publisher: publisher})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := collectAgentEvents(t, events)
	for _, event := range got {
		if _, ok := event.(model.TextDeltaEvent); ok {
			t.Fatalf("got transient text delta %#v after checkpoint failure", event)
		}
	}
	foundError := false
	for _, event := range got {
		if errEvent, ok := event.(model.ErrorEvent); ok && errEvent.Message == "persist assistant output" {
			foundError = true
		}
	}
	if !foundError {
		t.Fatalf("events = %#v, want persistence error", got)
	}
	for _, event := range publisher.events {
		if _, ok := event.(eventbus.AssistantReady); ok {
			t.Fatalf("published AssistantReady after checkpoint failure")
		}
	}
}

func TestStreamDoesNotCheckpointModelOnlyReasoning(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{{model.ReasoningDeltaEvent{Text: "private reasoning"}}}}
	publisher := &checkpointingPublisher{fakePublisher: &fakePublisher{}}
	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "think"}},
	}, Options{Provider: provider, TurnID: "turn-1", Publisher: publisher})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := collectAgentEvents(t, events)
	for _, event := range publisher.events {
		if _, ok := event.(eventbus.AssistantTextCheckpoint); ok {
			t.Fatalf("published checkpoint for model-only reasoning: %#v", event)
		}
		if _, ok := event.(eventbus.AssistantReady); ok {
			t.Fatalf("published visible assistant item for model-only reasoning: %#v", event)
		}
	}
	firstErrorEvent(t, got)
}

func TestStreamGivesEachAssistantIterationItsOwnItemIdentity(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{
			model.TextDeltaEvent{Text: "before tool"},
			model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "call-1", Name: "echo", Arguments: `{}`}},
		},
		{model.TextDeltaEvent{Text: "after tool"}},
	}}
	publisher := &checkpointingPublisher{fakePublisher: &fakePublisher{}}
	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "use echo"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: &fakeToolExecutor{result: model.ToolResult{Name: "echo", ToolCallID: "call-1", Content: "echoed"}},
		TurnID:       "turn-1",
		Publisher:    publisher,
		MaxTurns:     2,
		AssistantCheckpoint: &AssistantCheckpointPolicy{
			MinInterval: time.Hour,
			MinNewRunes: 1000,
		},
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}
	_ = collectAgentEvents(t, events)
	if _, ok := <-results; !ok {
		t.Fatal("results closed without final turn result")
	}
	var checkpoints []eventbus.AssistantTextCheckpoint
	var ready []eventbus.AssistantReady
	for _, event := range publisher.events {
		switch event := event.(type) {
		case eventbus.AssistantTextCheckpoint:
			checkpoints = append(checkpoints, event)
		case eventbus.AssistantReady:
			ready = append(ready, event)
		}
	}
	if len(checkpoints) != 2 || len(ready) != 2 {
		t.Fatalf("assistant lifecycle = %d checkpoints, %d ready; events = %#v", len(checkpoints), len(ready), publisher.events)
	}
	if checkpoints[0].ItemID == checkpoints[1].ItemID || ready[0].ItemID != checkpoints[0].ItemID || ready[1].ItemID != checkpoints[1].ItemID {
		t.Fatalf("assistant identities = checkpoints %#v ready %#v, want one identity per iteration", checkpoints, ready)
	}
}

func TestStreamCancellationAfterPartialOutputFlushesCheckpoint(t *testing.T) {
	provider := &cancellingProvider{
		firstReceived:  make(chan struct{}),
		releaseSecond:  make(chan struct{}),
		secondReceived: make(chan struct{}),
	}
	publisher := &checkpointingPublisher{
		fakePublisher: &fakePublisher{},
		checkpointed:  make(chan string, 4),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, results, err := StreamWithResult(ctx, model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "cancel me"}},
	}, Options{
		Provider: provider, TurnID: "turn-1", Publisher: publisher,
		AssistantCheckpoint: &AssistantCheckpointPolicy{MinInterval: time.Hour, MinNewRunes: 1000},
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}
	// The first event is iteration-start. Collect the remaining unbuffered
	// output concurrently so the provider can deliver the second, below-
	// threshold delta before cancellation.
	if _, ok := <-events; !ok {
		t.Fatal("agent events closed before provider emitted output")
	}
	eventsDone := make(chan []model.Event, 1)
	go func() { eventsDone <- collectAgentEvents(t, events) }()
	if got := <-publisher.checkpointed; got != "a" {
		t.Fatalf("first checkpoint = %q, want immediate checkpoint a", got)
	}
	close(provider.releaseSecond)
	<-provider.secondReceived
	cancel()
	got := <-eventsDone
	if _, ok := <-results; ok {
		t.Fatal("got turn result after cancellation")
	}
	var checkpoints []eventbus.AssistantTextCheckpoint
	for _, event := range publisher.events {
		if checkpoint, ok := event.(eventbus.AssistantTextCheckpoint); ok {
			checkpoints = append(checkpoints, checkpoint)
		}
	}
	if len(checkpoints) != 2 || checkpoints[0].Content != "a" || checkpoints[1].Content != "ab" {
		t.Fatalf("checkpoints = %#v, want a then forced ab", checkpoints)
	}
	if checkpoints[0].ItemID == "" || checkpoints[0].ItemID != checkpoints[1].ItemID {
		t.Fatalf("checkpoint identities = %#v, want one item", checkpoints)
	}
	if len(got) < 2 {
		t.Fatalf("events = %#v, want deltas and cancellation error", got)
	}
}

func TestStreamDoesNotCheckpointEmptyOrWhitespaceOnlyOutput(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{{
		model.TextDeltaEvent{Text: ""},
		model.TextDeltaEvent{Text: " \n\t  "},
	}}}
	publisher := &checkpointingPublisher{fakePublisher: &fakePublisher{}}
	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model: "model-test", Messages: []model.Message{{Role: model.MessageRoleUser, Content: "only whitespace"}},
	}, Options{Provider: provider, TurnID: "turn-whitespace", Publisher: publisher})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}
	_ = collectAgentEvents(t, events)
	for range results {
		// A provider turn may still produce a normal empty result; it must not
		// create a visible assistant item.
	}
	for _, event := range publisher.events {
		switch event.(type) {
		case eventbus.AssistantTextCheckpoint, eventbus.AssistantReady:
			t.Fatalf("publisher event = %#v, want no visible assistant lifecycle", event)
		}
	}
}

func TestStreamFailureAfterUncheckpointedTailFlushesCheckpoint(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{{
		model.TextDeltaEvent{Text: "a"},
		model.TextDeltaEvent{Text: "b"},
		model.ErrorEvent{Err: errors.New("provider failed"), Message: "provider failed"},
	}}}
	publisher := &checkpointingPublisher{fakePublisher: &fakePublisher{}}
	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model: "model-test", Messages: []model.Message{{Role: model.MessageRoleUser, Content: "fail me"}},
	}, Options{
		Provider: provider, TurnID: "turn-failure", Publisher: publisher,
		AssistantCheckpoint: &AssistantCheckpointPolicy{MinInterval: time.Hour, MinNewRunes: 1000},
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}
	got := collectAgentEvents(t, events)
	for range results {
		t.Fatal("got turn result after provider failure")
	}
	var checkpoints []eventbus.AssistantTextCheckpoint
	for _, event := range publisher.events {
		if checkpoint, ok := event.(eventbus.AssistantTextCheckpoint); ok {
			checkpoints = append(checkpoints, checkpoint)
		}
	}
	if len(checkpoints) != 2 || checkpoints[0].Content != "a" || checkpoints[1].Content != "ab" || checkpoints[0].ItemID != checkpoints[1].ItemID {
		t.Fatalf("checkpoints = %#v, want a then forced ab on one item", checkpoints)
	}
	if firstErrorEvent(t, got).Message != "provider failed" {
		t.Fatalf("failure events = %#v, want provider failure", got)
	}
}

func TestStreamRetriesServerErrorBeforeAnyProviderProgress(t *testing.T) {
	originalBackoff := providerRetryBackoff
	providerRetryBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { providerRetryBackoff = originalBackoff })

	provider := &fakeProvider{turns: [][]model.Event{
		{model.ErrorEvent{
			Err:     &model.ProviderError{Message: "server_error: temporary failure (code server_error)"},
			Message: "OpenAI Responses stream error",
		}},
		{model.TextDeltaEvent{Text: "recovered"}},
	}}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "continue"}},
	}, Options{Provider: provider, MaxTurns: 2})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := collectAgentEvents(t, events)
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	if collectText(got) != "recovered" {
		t.Fatalf("text = %q, want recovered", collectText(got))
	}
	var retry model.ProviderRetryEvent
	found := false
	for _, event := range got {
		if value, ok := event.(model.ProviderRetryEvent); ok {
			retry = value
			found = true
		}
	}
	if !found || retry.Attempt != 2 || retry.MaxAttempts != 5 {
		t.Fatalf("retry event = %#v, found=%v", retry, found)
	}
}

func TestStreamDoesNotRetryServerErrorAfterProviderProgress(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{{
		model.TextDeltaEvent{Text: "partial"},
		model.ErrorEvent{
			Err:     &model.ProviderError{Message: "server_error: temporary failure (code server_error)"},
			Message: "OpenAI Responses stream error",
		},
	}}}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "continue"}},
	}, Options{Provider: provider, MaxTurns: 2})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := collectAgentEvents(t, events)
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests = %d, want no retry", len(provider.requests))
	}
	if collectText(got) != "partial" {
		t.Fatalf("text = %q, want partial", collectText(got))
	}
	if firstErrorEvent(t, got).Err == nil {
		t.Fatal("error event is missing")
	}
}

func TestStreamRetryDoesNotForwardRetryableErrorEvent(t *testing.T) {
	originalBackoff := providerRetryBackoff
	providerRetryBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { providerRetryBackoff = originalBackoff })

	provider := &fakeProvider{turns: [][]model.Event{
		{model.ErrorEvent{
			Err:     &model.ProviderError{Message: "server_error: temporary failure (code server_error)"},
			Message: "OpenAI Responses stream error",
		}},
		{model.TextDeltaEvent{Text: "recovered"}},
	}}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "continue"}},
	}, Options{Provider: provider, MaxTurns: 2})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := collectAgentEvents(t, events)
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	for _, event := range got {
		if errEvent, ok := event.(model.ErrorEvent); ok {
			t.Fatalf("retryable error leaked to event stream: %#v", errEvent)
		}
	}
	if collectText(got) != "recovered" {
		t.Fatalf("text = %q, want recovered", collectText(got))
	}
}

func TestStreamSuccessfulTurnMakesSingleProviderRequest(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{{
		model.TextDeltaEvent{Text: "done"},
	}}}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "continue"}},
	}, Options{Provider: provider, MaxTurns: 1})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := collectAgentEvents(t, events)
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests = %d, want exactly 1 (success must not re-request)", len(provider.requests))
	}
	if collectText(got) != "done" {
		t.Fatalf("text = %q, want done", collectText(got))
	}
}

func TestStreamRetriesStreamCallStatusError(t *testing.T) {
	originalBackoff := providerRetryBackoff
	providerRetryBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { providerRetryBackoff = originalBackoff })

	provider := &fakeProvider{
		turns: [][]model.Event{
			nil,
			{model.TextDeltaEvent{Text: "recovered"}},
		},
		streamErrs: []error{
			&httpstream.StatusError{StatusCode: 500, Status: "500 Internal Server Error"},
		},
	}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "continue"}},
	}, Options{Provider: provider, MaxTurns: 2})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := collectAgentEvents(t, events)
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	var retry model.ProviderRetryEvent
	found := false
	for _, event := range got {
		if value, ok := event.(model.ProviderRetryEvent); ok {
			retry = value
			found = true
		}
		if errEvent, ok := event.(model.ErrorEvent); ok {
			t.Fatalf("retryable Stream() error leaked to event stream: %#v", errEvent)
		}
	}
	if !found || retry.Attempt != 2 || retry.MaxAttempts != 5 || retry.Reason != "server_error" {
		t.Fatalf("retry event = %#v, found=%v", retry, found)
	}
	if collectText(got) != "recovered" {
		t.Fatalf("text = %q, want recovered", collectText(got))
	}
}

func TestStreamDoesNotRetryStreamCallClientError(t *testing.T) {
	provider := &fakeProvider{
		turns:      [][]model.Event{nil},
		streamErrs: []error{&httpstream.StatusError{StatusCode: 400, Status: "400 Bad Request"}},
	}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "continue"}},
	}, Options{Provider: provider, MaxTurns: 1})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := collectAgentEvents(t, events)
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests = %d, want no retry", len(provider.requests))
	}
	if errEvent := firstErrorEvent(t, got); errEvent.Message != "request model" {
		t.Fatalf("error event = %#v, want terminal request model", errEvent)
	}
}

func TestStreamRetriesStreamCallRequestTimeout(t *testing.T) {
	originalBackoff := providerRetryBackoff
	providerRetryBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { providerRetryBackoff = originalBackoff })

	provider := &fakeProvider{
		turns: [][]model.Event{
			nil,
			{model.TextDeltaEvent{Text: "recovered"}},
		},
		streamErrs: []error{&httpstream.RequestTimeoutError{Timeout: 15 * time.Second}},
	}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "continue"}},
	}, Options{Provider: provider, MaxTurns: 2})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := collectAgentEvents(t, events)
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	var retry model.ProviderRetryEvent
	found := false
	for _, event := range got {
		if value, ok := event.(model.ProviderRetryEvent); ok {
			retry = value
			found = true
		}
	}
	if !found || retry.Reason != "timeout" {
		t.Fatalf("retry event = %#v, found=%v, want reason timeout", retry, found)
	}
}

func TestStreamRetriesIdleTimeoutOnlyOnce(t *testing.T) {
	originalBackoff := providerRetryBackoff
	providerRetryBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { providerRetryBackoff = originalBackoff })

	idleError := model.ErrorEvent{
		Err:     &httpstream.StreamIdleTimeoutError{Timeout: 2 * time.Minute},
		Message: "OpenAI Responses stream idle timeout",
	}
	provider := &fakeProvider{turns: [][]model.Event{
		{idleError},
		{idleError},
		{model.TextDeltaEvent{Text: "must not reach"}},
	}}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "continue"}},
	}, Options{Provider: provider, MaxTurns: 2})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := collectAgentEvents(t, events)
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2 (idle retried only once)", len(provider.requests))
	}
	retries := 0
	for _, event := range got {
		if value, ok := event.(model.ProviderRetryEvent); ok {
			retries++
			if value.Reason != "timeout" {
				t.Fatalf("retry reason = %q, want timeout", value.Reason)
			}
		}
	}
	if retries != 1 {
		t.Fatalf("retry events = %d, want exactly 1", retries)
	}
	if errEvent := firstErrorEvent(t, got); errEvent.Err == nil {
		t.Fatal("terminal error event is missing")
	}
}

func TestStreamRetryReasonRateLimited(t *testing.T) {
	originalBackoff := providerRetryBackoff
	providerRetryBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { providerRetryBackoff = originalBackoff })

	provider := &fakeProvider{
		turns: [][]model.Event{
			nil,
			{model.TextDeltaEvent{Text: "recovered"}},
		},
		streamErrs: []error{&httpstream.StatusError{StatusCode: 429, Status: "429 Too Many Requests"}},
	}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "continue"}},
	}, Options{Provider: provider, MaxTurns: 2})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := collectAgentEvents(t, events)
	var retry model.ProviderRetryEvent
	found := false
	for _, event := range got {
		if value, ok := event.(model.ProviderRetryEvent); ok {
			retry = value
			found = true
		}
	}
	if !found || retry.Reason != "rate_limited" {
		t.Fatalf("retry event = %#v, found=%v, want reason rate_limited", retry, found)
	}
}

func TestStreamExhaustsRetryBudget(t *testing.T) {
	originalBackoff := providerRetryBackoff
	providerRetryBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { providerRetryBackoff = originalBackoff })

	serverError := model.ErrorEvent{
		Err:     &model.ProviderError{Message: "server_error: persistent failure (code server_error)"},
		Message: "OpenAI Responses stream error",
	}
	provider := &fakeProvider{turns: [][]model.Event{
		{serverError}, {serverError}, {serverError}, {serverError}, {serverError},
	}}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "continue"}},
	}, Options{Provider: provider, MaxTurns: 2})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := collectAgentEvents(t, events)
	if len(provider.requests) != 5 {
		t.Fatalf("provider requests = %d, want exactly 5", len(provider.requests))
	}
	var attempts []int
	for _, event := range got {
		if value, ok := event.(model.ProviderRetryEvent); ok {
			attempts = append(attempts, value.Attempt)
		}
	}
	if want := []int{2, 3, 4, 5}; !reflect.DeepEqual(attempts, want) {
		t.Fatalf("retry attempts = %#v, want %#v", attempts, want)
	}
	if errEvent := firstErrorEvent(t, got); errEvent.Err == nil {
		t.Fatal("terminal error event is missing after budget exhaustion")
	}
}

func TestStreamStopsRetryWhenContextCancelledDuringBackoff(t *testing.T) {
	originalBackoff := providerRetryBackoff
	providerRetryBackoff = func(int) time.Duration { return time.Minute }
	t.Cleanup(func() { providerRetryBackoff = originalBackoff })

	provider := &fakeProvider{turns: [][]model.Event{
		{model.ErrorEvent{
			Err:     &model.ProviderError{Message: "server_error: temporary failure (code server_error)"},
			Message: "OpenAI Responses stream error",
		}},
		{model.TextDeltaEvent{Text: "must not reach"}},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := Stream(ctx, model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "continue"}},
	}, Options{Provider: provider, MaxTurns: 2})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	var got []model.Event
	timeout := time.After(time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				goto drained
			}
			got = append(got, event)
			if _, isRetry := event.(model.ProviderRetryEvent); isRetry {
				cancel()
			}
		case <-timeout:
			t.Fatal("timed out waiting for agent events")
		}
	}
drained:
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests = %d, want no further request after cancel", len(provider.requests))
	}
	errEvent := firstErrorEvent(t, got)
	if errEvent.Message != "retry model request" {
		t.Fatalf("error event = %#v, want retry model request", errEvent)
	}
}

func TestStreamRetriesAnthropicOverloadedError(t *testing.T) {
	originalBackoff := providerRetryBackoff
	providerRetryBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { providerRetryBackoff = originalBackoff })

	provider := &fakeProvider{turns: [][]model.Event{
		{model.ErrorEvent{
			Err:     &model.ProviderError{Message: "overloaded_error: Overloaded"},
			Message: "Anthropic Messages stream error",
		}},
		{model.TextDeltaEvent{Text: "recovered"}},
	}}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "continue"}},
	}, Options{Provider: provider, MaxTurns: 2})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := collectAgentEvents(t, events)
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	if collectText(got) != "recovered" {
		t.Fatalf("text = %q, want recovered", collectText(got))
	}
}

func TestStreamOmitsToolStartedForValidationFailures(t *testing.T) {
	tests := []struct {
		name    string
		tools   []model.Tool
		args    string
		wantMsg string
	}{
		{
			name:    "invalid arguments",
			tools:   []model.Tool{{Name: "echo"}},
			args:    `{"text":`,
			wantMsg: "invalid tool arguments",
		},
		{
			name:    "disabled tool",
			tools:   nil,
			args:    `{}`,
			wantMsg: "is not enabled",
		},
		{
			name:    "missing executor",
			tools:   []model.Tool{{Name: "echo"}},
			args:    `{}`,
			wantMsg: "tool executor is not configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &fakeProvider{
				turns: [][]model.Event{
					{
						model.ToolCallDoneEvent{
							ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: tt.args},
						},
					},
					{
						model.TextDeltaEvent{Text: "recovered"},
					},
				},
			}
			var executor ToolExecutor
			if tt.name != "missing executor" {
				executor = &fakeToolExecutor{result: model.ToolResult{Name: "echo", Content: "should not run"}}
			}

			events, err := Stream(context.Background(), model.Request{
				Model:    "model-test",
				Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
				Tools:    tt.tools,
			}, Options{
				Provider:     provider,
				ToolExecutor: executor,
				MaxTurns:     4,
			})
			if err != nil {
				t.Fatalf("Stream() error = %v", err)
			}

			gotEvents := collectAgentEvents(t, events)
			if hasToolStartedEvent(gotEvents) {
				t.Fatalf("events = %#v, want no ToolStartedEvent for validation failure", gotEvents)
			}
			result := firstToolResult(t, gotEvents)
			if !result.IsError || !strings.Contains(result.Content, tt.wantMsg) {
				t.Fatalf("tool result = %#v, want error containing %q", result, tt.wantMsg)
			}
			if fake, ok := executor.(*fakeToolExecutor); ok && fake.called {
				t.Fatal("executor was called for validation failure")
			}
		})
	}
}

func TestStreamExecutesToolResultAndContinuesUntilFinalText(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{"text":"hello"}`},
				},
			},
			{
				model.TextDeltaEvent{Text: "final"},
			},
		},
	}
	executor := &fakeToolExecutor{
		result: model.ToolResult{Name: "echo", Content: "tool output"},
	}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		MaxTurns:     4,
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	if gotText := collectText(gotEvents); gotText != "final" {
		t.Fatalf("text events = %q, want final", gotText)
	}
	result := firstToolResult(t, gotEvents)
	if result.ToolCallID != "call_1" || result.Name != "echo" || result.Content != "tool output" || result.IsError {
		t.Fatalf("tool result = %#v, want successful echo result", result)
	}
	if executor.name != "echo" {
		t.Fatalf("executor name = %q, want echo", executor.name)
	}
	if !reflect.DeepEqual(executor.arguments, map[string]any{"text": "hello"}) {
		t.Fatalf("executor arguments = %#v, want text argument", executor.arguments)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("len(requests) = %d, want 2", len(provider.requests))
	}

	secondMessages := provider.requests[1].Messages
	if len(secondMessages) != 3 {
		t.Fatalf("len(second request messages) = %d, want 3: %#v", len(secondMessages), secondMessages)
	}
	assertAgentMessage(t, secondMessages[1], model.MessageRoleAssistant, "", "call_1")
	assertAgentMessage(t, secondMessages[2], model.MessageRoleTool, "tool output", "call_1")
}

func TestStreamWithResultAppendsFinalAssistantMessage(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.TextDeltaEvent{Text: "hello "},
				model.TextDeltaEvent{Text: "there"},
			},
		},
	}

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Say hi"}},
	}, Options{
		Provider: provider,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	if gotText := collectText(gotEvents); gotText != "hello there" {
		t.Fatalf("text events = %q, want hello there", gotText)
	}
	result := collectTurnResult(t, results)
	if len(result.Messages) != 2 {
		t.Fatalf("len(result messages) = %d, want 2: %#v", len(result.Messages), result.Messages)
	}
	assertAgentMessage(t, result.Messages[0], model.MessageRoleUser, "Say hi", "")
	assertAgentMessage(t, result.Messages[1], model.MessageRoleAssistant, "hello there", "")
}

func TestStreamWithResultPersistsResponseStateOnAssistantMessage(t *testing.T) {
	wantState := model.ResponseState{
		ID: "resp_1", Origin: "https://api.openai.com/v1", Model: "gpt-5.6", Stored: false,
		MessageID: "msg_1", MessagePhase: "final_answer",
		ReasoningItems: []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"cipher"}`)},
	}
	provider := &fakeProvider{turns: [][]model.Event{{
		model.TextDeltaEvent{Text: "answer"},
		model.ResponseStateEvent{State: wantState},
	}}}
	publisher := &fakePublisher{}

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "gpt-5.6",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Question"}},
	}, Options{Provider: provider, Publisher: publisher, TurnID: "turn-1"})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}
	gotEvents := collectAgentEvents(t, events)
	if gotText := collectText(gotEvents); gotText != "answer" {
		t.Fatalf("text events = %q, want answer", gotText)
	}
	for _, event := range gotEvents {
		if _, ok := event.(model.ResponseStateEvent); ok {
			t.Fatal("ResponseStateEvent leaked to agent consumers")
		}
	}

	result := collectTurnResult(t, results)
	if len(result.Messages) != 2 || result.Messages[1].ResponseState == nil {
		t.Fatalf("result messages = %#v, want assistant response state", result.Messages)
	}
	if !reflect.DeepEqual(*result.Messages[1].ResponseState, wantState) {
		t.Fatalf("result response state = %#v, want %#v", *result.Messages[1].ResponseState, wantState)
	}
	if len(publisher.events) != 1 {
		t.Fatalf("publisher events = %#v, want one AssistantReady", publisher.events)
	}
	ready, ok := publisher.events[0].(eventbus.AssistantReady)
	if !ok || ready.Message.ResponseState == nil || !reflect.DeepEqual(*ready.Message.ResponseState, wantState) {
		t.Fatalf("published event = %#v, want AssistantReady with response state", publisher.events[0])
	}
}

func TestStreamWithResultIncludesToolHistoryAndFinalAssistantText(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ReasoningDeltaEvent{Text: "inspect first"},
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{"text":"hello"}`},
				},
			},
			{
				model.TextDeltaEvent{Text: "final"},
			},
		},
	}
	executor := &fakeToolExecutor{
		result: model.ToolResult{Name: "echo", Content: "tool output"},
	}

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		MaxTurns:     4,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	if gotText := collectText(gotEvents); gotText != "final" {
		t.Fatalf("text events = %q, want final", gotText)
	}
	result := collectTurnResult(t, results)
	if len(result.Messages) != 4 {
		t.Fatalf("len(result messages) = %d, want 4: %#v", len(result.Messages), result.Messages)
	}
	assertAgentMessage(t, result.Messages[0], model.MessageRoleUser, "Use a tool", "")
	assertAgentMessage(t, result.Messages[1], model.MessageRoleAssistant, "", "call_1")
	if result.Messages[1].ReasoningContent != "inspect first" {
		t.Fatalf("first assistant reasoning = %q, want inspect first", result.Messages[1].ReasoningContent)
	}
	assertAgentMessage(t, result.Messages[2], model.MessageRoleTool, "tool output", "call_1")
	assertAgentMessage(t, result.Messages[3], model.MessageRoleAssistant, "final", "")
	if got := provider.requests[1].Messages[1].ReasoningContent; got != "inspect first" {
		t.Fatalf("replayed assistant reasoning = %q, want inspect first", got)
	}
}

func TestStreamWithResultFailsWhenFinalResponseHasNoVisibleOutput(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{}`},
				},
			},
			{
				model.ReasoningDeltaEvent{Text: "thinking only"},
			},
		},
	}
	executor := &fakeToolExecutor{
		result: model.ToolResult{Name: "echo", Content: "tool output"},
	}

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		MaxTurns:     4,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	errorEvent := firstErrorEvent(t, gotEvents)
	if errorEvent.Err == nil || !strings.Contains(errorEvent.Err.Error(), "empty final response") {
		t.Fatalf("error event = %#v, want empty final response", errorEvent)
	}
	if _, ok := <-results; ok {
		t.Fatal("results produced for empty final response")
	}
	if len(provider.requests) != 2 {
		t.Fatalf("len(requests) = %d, want 2", len(provider.requests))
	}
}

func TestStreamWithPublisherPublishesDurableEventsInOrder(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{"text":"hello"}`},
				},
			},
			{
				model.TextDeltaEvent{Text: "final"},
			},
		},
	}
	publisher := &fakePublisher{}
	var executeCheckErr error
	executor := &fakeToolExecutor{
		result: model.ToolResult{Name: "echo", Content: "tool output"},
		onExecute: func() {
			if len(publisher.events) != 1 {
				executeCheckErr = fmt.Errorf("publisher events before tool execution = %d, want 1", len(publisher.events))
				return
			}
			if _, ok := publisher.events[0].(eventbus.AssistantReady); !ok {
				executeCheckErr = fmt.Errorf("first publisher event = %T, want AssistantReady", publisher.events[0])
			}
		},
	}

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		MaxTurns:     4,
		TurnID:       "turn-1",
		Publisher:    publisher,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	if executeCheckErr != nil {
		t.Fatal(executeCheckErr)
	}
	if gotText := collectText(gotEvents); gotText != "final" {
		t.Fatalf("text events = %q, want final", gotText)
	}
	if result := firstToolResult(t, gotEvents); result.ToolCallID != "call_1" || result.Content != "tool output" {
		t.Fatalf("tool result event = %#v, want call_1/tool output", result)
	}
	turnResult := collectTurnResult(t, results)
	if len(turnResult.Messages) != 4 {
		t.Fatalf("len(result messages) = %d, want 4", len(turnResult.Messages))
	}

	if len(publisher.events) != 3 {
		t.Fatalf("publisher events = %#v, want 3 events", publisher.events)
	}
	firstAssistant, ok := publisher.events[0].(eventbus.AssistantReady)
	if !ok {
		t.Fatalf("publisher event[0] = %T, want AssistantReady", publisher.events[0])
	}
	if firstAssistant.TurnID != "turn-1" || firstAssistant.Message.Role != model.MessageRoleAssistant || len(firstAssistant.Message.ToolCalls) != 1 || firstAssistant.Message.ToolCalls[0].ID != "call_1" {
		t.Fatalf("first AssistantReady = %#v, want turn-1 assistant call_1", firstAssistant)
	}
	toolResult, ok := publisher.events[1].(eventbus.ToolResultReady)
	if !ok {
		t.Fatalf("publisher event[1] = %T, want ToolResultReady", publisher.events[1])
	}
	if toolResult.TurnID != "turn-1" || toolResult.Result.ToolCallID != "call_1" || toolResult.Result.Content != "tool output" {
		t.Fatalf("ToolResultReady = %#v, want call_1/tool output", toolResult)
	}
	finalAssistant, ok := publisher.events[2].(eventbus.AssistantReady)
	if !ok {
		t.Fatalf("publisher event[2] = %T, want AssistantReady", publisher.events[2])
	}
	if finalAssistant.TurnID != "turn-1" || finalAssistant.Message.Content != "final" || len(finalAssistant.Message.ToolCalls) != 0 {
		t.Fatalf("final AssistantReady = %#v, want final no-tool assistant", finalAssistant)
	}
}

func TestStreamWithPublisherAssistantReadyFailureAborts(t *testing.T) {
	publishErr := errors.New("persist assistant failed")

	t.Run("final response", func(t *testing.T) {
		provider := &fakeProvider{
			turns: [][]model.Event{
				{model.TextDeltaEvent{Text: "final"}},
			},
		}
		publisher := &fakePublisher{errKind: eventbus.KindAssistantReady, err: publishErr}

		events, results, err := StreamWithResult(context.Background(), model.Request{
			Model:    "model-test",
			Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Say hi"}},
		}, Options{
			Provider:  provider,
			TurnID:    "turn-1",
			Publisher: publisher,
		})
		if err != nil {
			t.Fatalf("StreamWithResult() error = %v", err)
		}

		gotEvents := collectAgentEvents(t, events)
		errorEvent := firstErrorEvent(t, gotEvents)
		if !errors.Is(errorEvent.Err, publishErr) || errorEvent.Message != "persist assistant" {
			t.Fatalf("error event = %#v, want persist assistant error", errorEvent)
		}
		assertNoTurnResult(t, results)
		if len(provider.requests) != 1 {
			t.Fatalf("len(provider.requests) = %d, want 1", len(provider.requests))
		}
	})

	t.Run("tool round", func(t *testing.T) {
		provider := &fakeProvider{
			turns: [][]model.Event{
				{
					model.ToolCallDoneEvent{
						ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{}`},
					},
				},
			},
		}
		executor := &fakeToolExecutor{result: model.ToolResult{Name: "echo", Content: "tool output"}}
		publisher := &fakePublisher{errKind: eventbus.KindAssistantReady, err: publishErr}

		events, results, err := StreamWithResult(context.Background(), model.Request{
			Model:    "model-test",
			Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
			Tools:    []model.Tool{{Name: "echo"}},
		}, Options{
			Provider:     provider,
			ToolExecutor: executor,
			TurnID:       "turn-1",
			Publisher:    publisher,
		})
		if err != nil {
			t.Fatalf("StreamWithResult() error = %v", err)
		}

		gotEvents := collectAgentEvents(t, events)
		errorEvent := firstErrorEvent(t, gotEvents)
		if !errors.Is(errorEvent.Err, publishErr) || errorEvent.Message != "persist assistant" {
			t.Fatalf("error event = %#v, want persist assistant error", errorEvent)
		}
		if executor.called {
			t.Fatal("executor ran after AssistantReady publish failed")
		}
		if hasToolResultEvent(gotEvents) {
			t.Fatalf("events = %#v, want no ToolResultEvent", gotEvents)
		}
		assertNoTurnResult(t, results)
		if len(provider.requests) != 1 {
			t.Fatalf("len(provider.requests) = %d, want 1", len(provider.requests))
		}
	})
}

func TestStreamWithPublisherToolResultReadyFailureAborts(t *testing.T) {
	publishErr := errors.New("persist tool result failed")
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{}`},
				},
			},
			{
				model.TextDeltaEvent{Text: "should not request"},
			},
		},
	}
	executor := &fakeToolExecutor{result: model.ToolResult{Name: "echo", Content: "tool output"}}
	publisher := &fakePublisher{errKind: eventbus.KindToolResultReady, err: publishErr}

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		TurnID:       "turn-1",
		Publisher:    publisher,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	errorEvent := firstErrorEvent(t, gotEvents)
	if !errors.Is(errorEvent.Err, publishErr) || errorEvent.Message != "persist tool result" {
		t.Fatalf("error event = %#v, want persist tool result error", errorEvent)
	}
	if !executor.called {
		t.Fatal("executor was not called before ToolResultReady publish")
	}
	if hasToolResultEvent(gotEvents) {
		t.Fatalf("events = %#v, want no ToolResultEvent", gotEvents)
	}
	assertNoTurnResult(t, results)
	if len(provider.requests) != 1 {
		t.Fatalf("len(provider.requests) = %d, want no second model request", len(provider.requests))
	}
	if len(publisher.events) != 2 {
		t.Fatalf("publisher events = %#v, want AssistantReady and ToolResultReady", publisher.events)
	}
}

func TestStreamWithPublisherRequiresTurnID(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{model.TextDeltaEvent{Text: "unused"}},
		},
	}
	publisher := &fakePublisher{}

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Say hi"}},
	}, Options{
		Provider:  provider,
		Publisher: publisher,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	errorEvent := firstErrorEvent(t, gotEvents)
	if errorEvent.Message != "persist turn" || !strings.Contains(errorEvent.Err.Error(), "turn id is required") {
		t.Fatalf("error event = %#v, want missing turn id error", errorEvent)
	}
	assertNoTurnResult(t, results)
	if len(provider.requests) != 0 {
		t.Fatalf("len(provider.requests) = %d, want 0", len(provider.requests))
	}
	if len(publisher.events) != 0 {
		t.Fatalf("publisher events = %#v, want none", publisher.events)
	}
}

func TestStreamMalformedToolArgumentsAppendsToolErrorAndContinues(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{"text":`},
				},
			},
			{
				model.TextDeltaEvent{Text: "recovered"},
			},
		},
	}
	executor := &fakeToolExecutor{
		result: model.ToolResult{Name: "echo", Content: "should not run"},
	}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		MaxTurns:     4,
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	result := firstToolResult(t, gotEvents)
	if !result.IsError {
		t.Fatalf("tool result IsError = false, want true: %#v", result)
	}
	if !strings.Contains(result.Content, "invalid tool arguments") {
		t.Fatalf("tool result content = %q, want invalid arguments message", result.Content)
	}
	if executor.called {
		t.Fatal("executor was called for malformed arguments")
	}
	if gotText := collectText(gotEvents); gotText != "recovered" {
		t.Fatalf("text events = %q, want recovered", gotText)
	}

	secondMessages := provider.requests[1].Messages
	assertAgentMessage(t, secondMessages[2], model.MessageRoleTool, result.Content, "call_1")
	if !secondMessages[2].IsError {
		t.Fatalf("tool result message IsError = false, want true")
	}
}

func TestStreamStopsWithClearErrorAtMaxTurns(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{}`},
				},
			},
			{
				model.TextDeltaEvent{Text: "unexpected"},
			},
		},
	}
	executor := &fakeToolExecutor{
		result: model.ToolResult{Name: "echo", Content: "tool output"},
	}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		MaxTurns:     1,
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	errorEvent := firstErrorEvent(t, gotEvents)
	if errorEvent.Err == nil || !strings.Contains(errorEvent.Err.Error(), "max_turns 1") {
		t.Fatalf("error event = %#v, want max_turns error", errorEvent)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1", len(provider.requests))
	}
	if gotText := collectText(gotEvents); gotText != "" {
		t.Fatalf("text events = %q, want empty", gotText)
	}
}

type fakeProvider struct {
	turns      [][]model.Event
	streamErrs []error
	requests   []model.Request
}

type cancellingProvider struct {
	firstReceived  chan struct{}
	releaseSecond  chan struct{}
	secondReceived chan struct{}
}

func (p *cancellingProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	events := make(chan model.Event)
	go func() {
		defer close(events)
		events <- model.TextDeltaEvent{Text: "a"}
		close(p.firstReceived)
		<-p.releaseSecond
		events <- model.TextDeltaEvent{Text: "b"}
		close(p.secondReceived)
		<-ctx.Done()
	}()
	return events, nil
}

func (p *fakeProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	if len(p.requests) >= len(p.turns) {
		return nil, fmt.Errorf("unexpected model request %d", len(p.requests)+1)
	}

	turn := len(p.requests)
	p.requests = append(p.requests, copyAgentRequest(request))

	if turn < len(p.streamErrs) && p.streamErrs[turn] != nil {
		return nil, p.streamErrs[turn]
	}

	events := make(chan model.Event, len(p.turns[turn]))
	for _, event := range p.turns[turn] {
		events <- event
	}
	close(events)
	return events, nil
}

type fakeToolExecutor struct {
	called    bool
	name      string
	arguments map[string]any
	result    model.ToolResult
	err       error
	onExecute func()
}

func (e *fakeToolExecutor) Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error) {
	e.called = true
	e.name = name
	e.arguments = arguments
	if e.onExecute != nil {
		e.onExecute()
	}
	return e.result, e.err
}

type fakePublisher struct {
	events  []eventbus.Event
	errKind string
	err     error
}

type checkpointingPublisher struct {
	*fakePublisher
	checkpointed chan string
}

func (p *checkpointingPublisher) Publish(event eventbus.Event) error {
	return p.fakePublisher.Publish(event)
}

func (p *checkpointingPublisher) PublishAssistantCheckpoint(turnID string, agentIteration int, itemID, content string) error {
	err := p.Publish(eventbus.AssistantTextCheckpoint{
		TurnID: turnID, AgentIteration: agentIteration, ItemID: itemID, Content: content,
	})
	if err == nil && p.checkpointed != nil {
		p.checkpointed <- content
	}
	return err
}

func (p *fakePublisher) Publish(event eventbus.Event) error {
	p.events = append(p.events, event)
	if p.errKind != "" && event.Kind() == p.errKind {
		return p.err
	}
	return nil
}

func copyAgentRequest(request model.Request) model.Request {
	copied := request
	copied.Messages = append([]model.Message(nil), request.Messages...)
	for i := range copied.Messages {
		copied.Messages[i].ToolCalls = append([]model.ToolCall(nil), request.Messages[i].ToolCalls...)
	}
	copied.Tools = append([]model.Tool(nil), request.Tools...)
	if request.Parameters != nil {
		copied.Parameters = make(map[string]any, len(request.Parameters))
		for key, value := range request.Parameters {
			copied.Parameters[key] = value
		}
	}
	return copied
}

func collectAgentEvents(t *testing.T, events <-chan model.Event) []model.Event {
	t.Helper()

	var got []model.Event
	timeout := time.After(time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return got
			}
			got = append(got, event)
		case <-timeout:
			t.Fatal("timed out waiting for agent events")
		}
	}
}

func collectText(events []model.Event) string {
	var text strings.Builder
	for _, event := range events {
		if event, ok := event.(model.TextDeltaEvent); ok {
			text.WriteString(event.Text)
		}
	}
	return text.String()
}

func firstToolResult(t *testing.T, events []model.Event) model.ToolResult {
	t.Helper()

	for _, event := range events {
		if event, ok := event.(model.ToolResultEvent); ok {
			return event.Result
		}
	}
	t.Fatal("missing ToolResultEvent")
	return model.ToolResult{}
}

func firstErrorEvent(t *testing.T, events []model.Event) model.ErrorEvent {
	t.Helper()

	for _, event := range events {
		if event, ok := event.(model.ErrorEvent); ok {
			return event
		}
	}
	t.Fatal("missing ErrorEvent")
	return model.ErrorEvent{}
}

func hasToolResultEvent(events []model.Event) bool {
	for _, event := range events {
		if _, ok := event.(model.ToolResultEvent); ok {
			return true
		}
	}
	return false
}

func hasToolStartedEvent(events []model.Event) bool {
	for _, event := range events {
		if _, ok := event.(model.ToolStartedEvent); ok {
			return true
		}
	}
	return false
}

func collectTurnResult(t *testing.T, results <-chan TurnResult) TurnResult {
	t.Helper()

	select {
	case result, ok := <-results:
		if !ok {
			t.Fatal("result channel closed without TurnResult")
		}
		select {
		case extra, ok := <-results:
			if ok {
				t.Fatalf("unexpected extra TurnResult: %#v", extra)
			}
		default:
		}
		return result
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for TurnResult")
	}
	return TurnResult{}
}

func assertNoTurnResult(t *testing.T, results <-chan TurnResult) {
	t.Helper()

	select {
	case result, ok := <-results:
		if ok {
			t.Fatalf("unexpected TurnResult: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for result channel to close")
	}
}

func assertAgentMessage(t *testing.T, message model.Message, role model.MessageRole, content string, toolCallID string) {
	t.Helper()

	if message.Role != role {
		t.Fatalf("message role = %q, want %q", message.Role, role)
	}
	if message.Content != content {
		t.Fatalf("message content = %q, want %q", message.Content, content)
	}
	switch role {
	case model.MessageRoleAssistant:
		if toolCallID == "" {
			if len(message.ToolCalls) != 0 {
				t.Fatalf("assistant tool calls = %#v, want none", message.ToolCalls)
			}
			return
		}
		if len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != toolCallID {
			t.Fatalf("assistant tool calls = %#v, want id %q", message.ToolCalls, toolCallID)
		}
	case model.MessageRoleTool:
		if message.ToolCallID != toolCallID {
			t.Fatalf("tool_call_id = %q, want %q", message.ToolCallID, toolCallID)
		}
	}
}
