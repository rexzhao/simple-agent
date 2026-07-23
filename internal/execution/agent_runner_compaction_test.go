package execution

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/contextwindow"
	eventlog "github.com/rexzhao/simple-agent/internal/logging"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestPlanCompactionCheckpointUsesStandaloneResponsesCompaction(t *testing.T) {
	requestBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses/compact" {
			t.Errorf("path = %q, want /responses/compact", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		requestBody <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"object":"response.compaction",
			"output":[
				{"type":"message","role":"developer","content":"retained"},
				{"type":"compaction","id":"cmp_1","encrypted_content":"sealed"}
			],
			"usage":{
				"input_tokens":1200,
				"output_tokens":80,
				"total_tokens":1280,
				"input_tokens_details":{"cached_tokens":900,"cache_write_tokens":200},
				"output_tokens_details":{"reasoning_tokens":64}
			}
		}`)
	}))
	defer server.Close()

	parameters := map[string]any{
		"responses": map[string]any{
			"compaction": map[string]any{"mode": "responses-compact"},
		},
	}
	cfg := &config.Config{
		Compaction: config.CompactionConfig{Enabled: true, ThresholdPercent: 80},
		Providers: map[string]config.ProviderConfig{
			"openai": {
				Name:    "openai",
				BaseURL: server.URL,
				APIKey:  "test-key",
				Models: map[string]config.ModelProfile{
					"default": {
						ID:         "gpt-5.6",
						Type:       config.ProviderTypeOpenAIResponses,
						Parameters: parameters,
					},
				},
			},
		}}
	session := sessions.SessionV2{
		ID: "session-1",
		Items: []sessions.SessionItem{
			{
				ID:         "user-1",
				Kind:       sessions.ItemKindMessage,
				Visibility: sessions.ItemVisibilityVisible,
				Audience:   sessions.ItemAudienceUser,
				Message:    &model.Message{Role: model.MessageRoleUser, Content: "Do work"},
			},
			{
				ID:         "assistant-1",
				Kind:       sessions.ItemKindMessage,
				Visibility: sessions.ItemVisibilityVisible,
				Audience:   sessions.ItemAudienceModel,
				Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "Done"},
			},
		},
		ActiveHistory: []string{"user-1", "assistant-1"},
	}
	logger, err := eventlog.Open(filepath.Join(t.TempDir(), "logs", "sai.jsonl"), eventlog.Attributes{
		Provider: "openai",
		Model:    "gpt-5.6",
	})
	if err != nil {
		t.Fatalf("logging.Open() error = %v", err)
	}
	runtime := &agentRunnerRuntime{
		config:       cfg,
		providerName: "openai",
		modelProfile: "default",
		modelID:      "gpt-5.6",
		parameters:   parameters,
		session:      session,
		contextTracker: contextwindow.NewTracker(contextwindow.Window{
			Tokens: 400000,
			Source: contextwindow.WindowSourceConfigured,
		}, contextwindow.Metadata{}),
		logger: logger,
	}

	plan, err := runtime.planCompactionCheckpoint(context.Background(), compactionCheckpointOptions{
		reason:  "user_requested",
		phase:   "manual",
		trigger: "manual",
	})
	if err != nil {
		t.Fatalf("planCompactionCheckpoint() error = %v", err)
	}
	if plan.summaryItem.Kind != sessions.ItemKindCompaction || plan.summaryItem.Message == nil {
		t.Fatalf("summary item = %#v, want remote compaction item", plan.summaryItem)
	}
	if plan.summaryItem.Message.Role != model.MessageRoleProvider || len(plan.summaryItem.Message.ProviderItems) != 2 {
		t.Fatalf("provider message = %#v", plan.summaryItem.Message)
	}
	if !reflect.DeepEqual(plan.checkpoint.PreviousActiveHistory, session.ActiveHistory) {
		t.Fatalf("PreviousActiveHistory = %#v, want %#v", plan.checkpoint.PreviousActiveHistory, session.ActiveHistory)
	}
	if !reflect.DeepEqual(plan.checkpoint.ReplacementHistory, []string{plan.summaryItem.ID}) {
		t.Fatalf("ReplacementHistory = %#v", plan.checkpoint.ReplacementHistory)
	}
	if len(plan.messages) != 1 || plan.messages[0].Role != model.MessageRoleProvider {
		t.Fatalf("plan messages = %#v", plan.messages)
	}
	wantUsage := model.Usage{
		InputTokens: 1200, OutputTokens: 80, TotalTokens: 1280,
		CachedTokens: 900, CacheWriteTokens: 200, ReasoningTokens: 64,
	}
	if plan.usage == nil || *plan.usage != wantUsage {
		t.Fatalf("plan usage = %#v, want %#v", plan.usage, wantUsage)
	}
	if plan.context == nil ||
		plan.context.LastInputTokens != 1200 ||
		plan.context.LastCachedTokens != 900 ||
		plan.context.LastCacheWriteTokens != 200 ||
		plan.context.LastUsageSource != string(contextwindow.UsageSourceProvider) {
		t.Fatalf("plan context = %#v, want compact usage metadata", plan.context)
	}
	if plan.context.LastUsageAnchorMessages != 0 || plan.context.LastUsageAnchorHash != "" {
		t.Fatalf("compact usage must not anchor the next response request: %#v", plan.context)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("logger.Close() error = %v", err)
	}
	logData, err := os.ReadFile(logger.Path())
	if err != nil {
		t.Fatalf("ReadFile(compaction log) error = %v", err)
	}
	for _, want := range []string{`"event":"usage"`, `"cached_tokens":900`, `"cache_write_tokens":200`} {
		if !strings.Contains(string(logData), want) {
			t.Fatalf("compaction log = %s, want contain %q", logData, want)
		}
	}

	var body map[string]any
	if err := json.Unmarshal(<-requestBody, &body); err != nil {
		t.Fatalf("Unmarshal(request body) error = %v", err)
	}
	if _, ok := body["stream"]; ok {
		t.Fatalf("compact request contains stream: %#v", body)
	}
	input, ok := body["input"].([]any)
	if !ok || len(input) != 2 {
		t.Fatalf("compact input = %#v, want two messages", body["input"])
	}
}

func TestExpandRemoteCompactionHistoryRestoresCompleteLedger(t *testing.T) {
	providerMessage := func(id string) sessions.SessionItem {
		return sessions.SessionItem{
			ID:         id,
			Kind:       sessions.ItemKindCompaction,
			Visibility: sessions.ItemVisibilityHidden,
			Audience:   sessions.ItemAudienceModel,
			Message: &model.Message{
				Role: model.MessageRoleProvider,
				ProviderItems: []model.ProviderItem{{
					Origin: "https://api.openai.com/v1",
					Model:  "gpt-5.6",
					Data:   json.RawMessage(`{"type":"compaction","encrypted_content":"sealed"}`),
				}},
			},
		}
	}
	session := sessions.SessionV2{
		ID: "session-1",
		Items: []sessions.SessionItem{
			{ID: "user-1", Kind: sessions.ItemKindMessage, Message: &model.Message{Role: model.MessageRoleUser, Content: "one"}},
			{ID: "assistant-1", Kind: sessions.ItemKindMessage, Message: &model.Message{Role: model.MessageRoleAssistant, Content: "one"}},
			providerMessage("compaction-1"),
			{ID: "user-2", Kind: sessions.ItemKindMessage, Message: &model.Message{Role: model.MessageRoleUser, Content: "two"}},
			{ID: "assistant-2", Kind: sessions.ItemKindMessage, Message: &model.Message{Role: model.MessageRoleAssistant, Content: "two"}},
			providerMessage("compaction-2"),
		},
		ActiveHistory: []string{"compaction-2"},
		Compactions: []sessions.CompactionCheckpoint{
			{
				SummaryItemID:         "compaction-1",
				PreviousActiveHistory: []string{"user-1", "assistant-1"},
				ReplacementHistory:    []string{"compaction-1"},
			},
			{
				SummaryItemID:         "compaction-2",
				PreviousActiveHistory: []string{"compaction-1", "user-2", "assistant-2"},
				ReplacementHistory:    []string{"compaction-2"},
			},
		},
	}

	expanded, err := expandRemoteCompactionHistory(session)
	if err != nil {
		t.Fatalf("expandRemoteCompactionHistory() error = %v", err)
	}
	want := []string{"user-1", "assistant-1", "user-2", "assistant-2"}
	if !reflect.DeepEqual(expanded.ActiveHistory, want) {
		t.Fatalf("ActiveHistory = %#v, want %#v", expanded.ActiveHistory, want)
	}
}
