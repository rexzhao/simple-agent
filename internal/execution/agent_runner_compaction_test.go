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

func TestAutoCompactionUsageCountUsesPreviousProviderUsage(t *testing.T) {
	tests := []struct {
		name     string
		metadata contextwindow.Metadata
		want     int64
	}{
		{
			name: "recorded count",
			metadata: contextwindow.Metadata{
				LastUsageSource:      string(contextwindow.UsageSourceProvider),
				LastUsageCountTokens: 252000,
				LastTotalTokens:      240000,
			},
			want: 252000,
		},
		{
			name: "legacy provider total",
			metadata: contextwindow.Metadata{
				LastUsageSource: string(contextwindow.UsageSourceProvider),
				LastTotalTokens: 252000,
			},
			want: 252000,
		},
		{
			name: "component fallback",
			metadata: contextwindow.Metadata{
				LastUsageSource:      string(contextwindow.UsageSourceProvider),
				LastInputTokens:      200000,
				LastOutputTokens:     10000,
				LastCachedTokens:     40000,
				LastCacheWriteTokens: 2000,
			},
			want: 252000,
		},
		{
			name: "local estimate is not a model response",
			metadata: contextwindow.Metadata{
				LastUsageSource:      string(contextwindow.UsageSourceEstimated),
				LastUsageCountTokens: 300000,
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := autoCompactionUsageCount(tt.metadata); got != tt.want {
				t.Fatalf("autoCompactionUsageCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestAutoCompactionThresholdUsesModelLimits(t *testing.T) {
	tests := []struct {
		name             string
		inputLimit       int
		contextWindow    int
		outputLimit      int
		reserved         int
		thresholdPercent int
		want             int64
	}{
		{
			name:             "input limit with default reserve",
			inputLimit:       272000,
			contextWindow:    400000,
			outputLimit:      128000,
			thresholdPercent: 80,
			want:             252000,
		},
		{
			name:             "input limit with configured reserve",
			inputLimit:       272000,
			contextWindow:    400000,
			outputLimit:      128000,
			reserved:         12000,
			thresholdPercent: 80,
			want:             260000,
		},
		{
			name:             "context minus output without input limit",
			contextWindow:    400000,
			outputLimit:      128000,
			thresholdPercent: 80,
			want:             272000,
		},
		{
			name:             "legacy percent fallback without model limits",
			contextWindow:    400000,
			thresholdPercent: 80,
			want:             320000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := autoCompactionThreshold(
				tt.inputLimit,
				tt.contextWindow,
				tt.outputLimit,
				tt.reserved,
				tt.thresholdPercent,
			)
			if got != tt.want {
				t.Fatalf("autoCompactionThreshold() = %d, want %d", got, tt.want)
			}
		})
	}
}

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
		InputTokens: 100, OutputTokens: 80, TotalTokens: 1280,
		CachedTokens: 900, CacheWriteTokens: 200, ReasoningTokens: 64,
	}
	if plan.usage == nil || *plan.usage != wantUsage {
		t.Fatalf("plan usage = %#v, want %#v", plan.usage, wantUsage)
	}
	if plan.context == nil ||
		plan.context.LastInputTokens <= 0 ||
		plan.context.LastInputTokens >= wantUsage.TotalTokens ||
		plan.context.LastCachedTokens != 0 ||
		plan.context.LastCacheWriteTokens != 0 ||
		plan.context.LastUsageSource != string(contextwindow.UsageSourceEstimated) {
		t.Fatalf("plan context = %#v, want estimated replacement-history usage", plan.context)
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

func TestAutoCompactionRequestBytesMeasuresResponsesReplay(t *testing.T) {
	runtime := &agentRunnerRuntime{
		modelType: config.ProviderTypeOpenAIResponses,
		modelID:   "gpt-test",
	}
	small := runtime.autoCompactionRequestBytes([]model.Message{{Role: model.MessageRoleUser, Content: "short"}})
	large := runtime.autoCompactionRequestBytes([]model.Message{{Role: model.MessageRoleUser, Content: strings.Repeat("x", 4096)}})
	if small <= 0 || large <= small+4000 {
		t.Fatalf("request sizes small=%d large=%d, want serialized replay growth", small, large)
	}
}

func TestResolveSummaryModelPinsSessionParametersForSessionModel(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"fake": {
				Name:    "fake",
				BaseURL: "http://127.0.0.1:1/v1",
				APIKey:  "test-key",
				Models: map[string]config.ModelProfile{
					"default": {
						ID:         "fake-model-v2",
						Parameters: map[string]any{"temperature": 0.9},
					},
					"summary": {
						ID:         "fake-summary",
						Parameters: map[string]any{"temperature": 0.1},
					},
				},
			},
		},
	}
	sessionParameters := map[string]any{"temperature": 0.2, "reasoning_effort": "high"}
	runtime := &agentRunnerRuntime{
		config:       cfg,
		providerName: "fake",
		modelProfile: "default",
		modelID:      "fake-model",
		parameters:   sessionParameters,
	}

	resolved, err := runtime.resolveSummaryModel()
	if err != nil {
		t.Fatalf("resolveSummaryModel() error = %v", err)
	}
	if resolved.ModelID != "fake-model" {
		t.Fatalf("summary model ID = %q, want session-pinned fake-model", resolved.ModelID)
	}
	if !reflect.DeepEqual(resolved.Parameters, sessionParameters) {
		t.Fatalf("summary parameters = %#v, want session parameters %#v", resolved.Parameters, sessionParameters)
	}
	resolved.Parameters["temperature"] = 99
	if sessionParameters["temperature"] != 0.2 {
		t.Fatalf("resolveSummaryModel aliased runtime parameters: %#v", sessionParameters)
	}

	// A separately configured summary model keeps its own config parameters.
	runtime.config.Compaction = config.CompactionConfig{SummaryProvider: "fake", SummaryModel: "summary"}
	resolved, err = runtime.resolveSummaryModel()
	if err != nil {
		t.Fatalf("resolveSummaryModel(dedicated summary model) error = %v", err)
	}
	if resolved.ModelID != "fake-summary" || !reflect.DeepEqual(resolved.Parameters, map[string]any{"temperature": 0.1}) {
		t.Fatalf("dedicated summary model = %q parameters %#v, want its own config", resolved.ModelID, resolved.Parameters)
	}
}

func TestPlanCompactionCheckpointLogsRemoteFailureBeforeSummaryFallback(t *testing.T) {
	const responseSecret = "prompt secret returned by compact endpoint"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses/compact":
			http.Error(w, responseSecret, http.StatusNotFound)
		case "/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fallback summary\"}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
		default:
			http.NotFound(w, r)
		}
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
		},
	}
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
	t.Cleanup(func() {
		_ = logger.Close()
	})
	runtime := &agentRunnerRuntime{
		config:       cfg,
		providerName: "openai",
		modelProfile: "default",
		modelID:      "gpt-5.6",
		parameters:   parameters,
		session:      session,
		logger:       logger,
	}

	plan, err := runtime.planCompactionCheckpoint(context.Background(), compactionCheckpointOptions{
		reason:  "context_limit",
		phase:   "pre_turn",
		trigger: "auto",
	})
	if err != nil {
		t.Fatalf("planCompactionCheckpoint() error = %v", err)
	}
	if !strings.HasPrefix(plan.summaryItem.ID, "summary-") {
		t.Fatalf("summary item ID = %q, want local summary fallback", plan.summaryItem.ID)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("logger.Close() error = %v", err)
	}
	logData, err := os.ReadFile(logger.Path())
	if err != nil {
		t.Fatalf("ReadFile(compaction log) error = %v", err)
	}
	logText := string(logData)
	for _, want := range []string{
		`"event":"error"`,
		`"level":"error"`,
		`"provider":"openai"`,
		`"model":"gpt-5.6"`,
		`"message":"POST /responses/compact failed (reason=context_limit phase=pre_turn trigger=auto); falling back to local summary"`,
		`"error":"404 Not Found"`,
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("compaction log = %s, want contain %q", logText, want)
		}
	}
	if strings.Contains(logText, responseSecret) {
		t.Fatalf("compaction log leaked provider response body: %s", logText)
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
