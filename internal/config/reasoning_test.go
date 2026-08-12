package config

import (
	"reflect"
	"testing"
)

func TestDefaultReasoningConfigMatchesPiOpenAIMappings(t *testing.T) {
	tests := []struct {
		name      string
		provider  string
		baseURL   string
		model     ModelProfile
		parameter string
		defaultID string
		levels    []string
	}{
		{
			name:      "Codex GPT 5.5 maps minimal to low and supports xhigh",
			provider:  "codex",
			model:     ModelProfile{ID: "gpt-5.5", Type: ProviderTypeOpenAICodex},
			parameter: "reasoning.effort",
			defaultID: "xhigh",
			levels:    []string{"off", "minimal", "low", "medium", "high", "xhigh"},
		},
		{
			name:      "OpenAI GPT 5.5 omits unsupported minimal",
			provider:  "openai",
			model:     ModelProfile{ID: "gpt-5.5", Type: ProviderTypeOpenAIResponses},
			parameter: "reasoning.effort",
			defaultID: "xhigh",
			levels:    []string{"off", "low", "medium", "high", "xhigh"},
		},
		{
			name:      "OpenRouter uses nested reasoning effort",
			provider:  "openrouter",
			baseURL:   "https://openrouter.ai/api/v1",
			model:     ModelProfile{ID: "gpt-5.6-sol", Type: ProviderTypeOpenAIChat},
			parameter: "reasoning.effort",
			defaultID: "xhigh",
			levels:    []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"},
		},
		{
			name:      "GLM 5.2 exposes only supported effort names",
			provider:  "zai",
			model:     ModelProfile{ID: "glm-5.2", Type: ProviderTypeOpenAIChat},
			parameter: "reasoning_effort",
			defaultID: "high",
			levels:    []string{"low", "medium", "high", "max"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := DefaultReasoningConfig(test.provider, test.baseURL, test.model)
			if got.Parameter != test.parameter || got.Default != test.defaultID {
				t.Fatalf("DefaultReasoningConfig() = parameter %q default %q, want %q %q", got.Parameter, got.Default, test.parameter, test.defaultID)
			}
			if levels := ReasoningLevelNames(got.Levels); !reflect.DeepEqual(levels, test.levels) {
				t.Fatalf("levels = %#v, want %#v", levels, test.levels)
			}
		})
	}
}

func TestDefaultReasoningConfigForDeepSeekV4Flash(t *testing.T) {
	tests := []struct {
		name      string
		provider  string
		baseURL   string
		model     ModelProfile
		parameter string
		defaultID string
		levels    []string
		lowValue  any
	}{
		{
			name:      "deepseek-v4-flash exposes low/high/max only",
			provider:  "paperhub",
			baseURL:   "https://tc-paperhub.diezhi.net/v1",
			model:     ModelProfile{ID: "deepseek-v4-flash", Type: ProviderTypeOpenAIChat},
			parameter: "reasoning_effort",
			defaultID: "high",
			levels:    []string{"low", "high", "max"},
			lowValue:  "low",
		},
		{
			name:      "deepseek-v4 keeps the same three levels",
			provider:  "paperhub",
			baseURL:   "https://tc-paperhub.diezhi.net/v1",
			model:     ModelProfile{ID: "deepseek-v4", Type: ProviderTypeOpenAIChat},
			parameter: "reasoning_effort",
			defaultID: "high",
			levels:    []string{"low", "high", "max"},
			lowValue:  "low",
		},
		{
			name:      "deepseek-v4-flash keeps low/high/max on zai too",
			provider:  "zai",
			baseURL:   "https://api.z.ai/api/paas/v4",
			model:     ModelProfile{ID: "deepseek-v4-flash", Type: ProviderTypeOpenAIChat},
			parameter: "reasoning_effort",
			defaultID: "high",
			levels:    []string{"low", "high", "max"},
			lowValue:  "low",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := DefaultReasoningConfig(test.provider, test.baseURL, test.model)
			if got.Parameter != test.parameter || got.Default != test.defaultID {
				t.Fatalf("DefaultReasoningConfig() = parameter %q default %q, want %q %q", got.Parameter, got.Default, test.parameter, test.defaultID)
			}
			if levels := ReasoningLevelNames(got.Levels); !reflect.DeepEqual(levels, test.levels) {
				t.Fatalf("levels = %#v, want %#v", levels, test.levels)
			}
			if got.Levels["low"] != test.lowValue {
				t.Fatalf("levels[low] = %#v, want %#v", got.Levels["low"], test.lowValue)
			}
			if _, ok := got.Levels["medium"]; ok {
				t.Fatalf("levels contains medium, want low/high/max only: %#v", got.Levels)
			}
		})
	}
}

func TestDefaultReasoningConfigForAdaptiveClaude(t *testing.T) {
	got := DefaultReasoningConfig("anthropic", "", ModelProfile{ID: "claude-opus-4-7", Type: ProviderTypeAnthropicMessages})
	if got.Parameter != "output_config.effort" || got.Default != "high" {
		t.Fatalf("DefaultReasoningConfig() = %#v", got)
	}
	want := []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	if levels := ReasoningLevelNames(got.Levels); !reflect.DeepEqual(levels, want) {
		t.Fatalf("levels = %#v, want %#v", levels, want)
	}
}

func TestDefaultReasoningConfigLeavesUnknownModelAlone(t *testing.T) {
	got := DefaultReasoningConfig("custom", "https://example.test/v1", ModelProfile{ID: "plain-model", Type: ProviderTypeOpenAIChat})
	if len(got.Levels) != 0 || got.Parameter != "" || got.Default != "" {
		t.Fatalf("DefaultReasoningConfig() = %#v, want empty", got)
	}
}

func TestResolveReasoningLevel(t *testing.T) {
	reasoning := ReasoningConfig{
		Parameter: "reasoning_effort",
		Default:   "high",
		Levels:    map[string]any{"low": "low", "high": "high"},
	}
	tests := []struct {
		name      string
		reasoning ReasoningConfig
		selected  string
		want      string
	}{
		{name: "explicit selection wins", reasoning: reasoning, selected: "low", want: "low"},
		{name: "empty selection falls back to default", reasoning: reasoning, selected: "", want: "high"},
		{name: "whitespace selection falls back to default", reasoning: reasoning, selected: "  ", want: "high"},
		{name: "selection is trimmed", reasoning: reasoning, selected: " low ", want: "low"},
		{name: "no levels configured", reasoning: ReasoningConfig{Default: "high"}, selected: "high", want: ""},
		{name: "empty config", reasoning: ReasoningConfig{}, selected: "", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ResolveReasoningLevel(test.reasoning, test.selected); got != test.want {
				t.Fatalf("ResolveReasoningLevel() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestApplyReasoningLevelSetsNestedValueWithoutMutatingDefaults(t *testing.T) {
	parameters := map[string]any{
		"reasoning": map[string]any{"summary": "auto", "effort": "medium"},
	}
	got, err := ApplyReasoningLevel(parameters, ReasoningConfig{
		Parameter: "reasoning.effort",
		Default:   "high",
		Levels:    map[string]any{"high": "xhigh"},
	}, "")
	if err != nil {
		t.Fatalf("ApplyReasoningLevel() error = %v", err)
	}
	reasoning := got["reasoning"].(map[string]any)
	if reasoning["summary"] != "auto" || reasoning["effort"] != "xhigh" {
		t.Fatalf("reasoning = %#v", reasoning)
	}
	if original := parameters["reasoning"].(map[string]any)["effort"]; original != "medium" {
		t.Fatalf("input effort mutated to %#v", original)
	}
}

func TestApplyReasoningLevelBudgetTokensWritesNumber(t *testing.T) {
	got, err := ApplyReasoningLevel(map[string]any{}, ReasoningConfig{
		Type:      ReasoningTypeBudgetTokens,
		Parameter: "thinking.budget_tokens",
		Default:   "high",
		Levels:    map[string]any{"low": int64(2048), "high": int64(8192)},
	}, "low")
	if err != nil {
		t.Fatalf("ApplyReasoningLevel() error = %v", err)
	}
	thinking, ok := got["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking = %#v, want nested map", got["thinking"])
	}
	if thinking["budget_tokens"] != int64(2048) {
		t.Fatalf("budget_tokens = %#v, want 2048", thinking["budget_tokens"])
	}
}

func TestApplyReasoningLevelBudgetTokensRejectsNonNumeric(t *testing.T) {
	_, err := ApplyReasoningLevel(map[string]any{}, ReasoningConfig{
		Type:      ReasoningTypeBudgetTokens,
		Parameter: "thinking.budget_tokens",
		Default:   "high",
		Levels:    map[string]any{"high": "high"},
	}, "high")
	if err == nil {
		t.Fatal("ApplyReasoningLevel() error = nil, want numeric-level rejection")
	}
}

func TestNormalizeReasoningType(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{value: "", want: ""},
		{value: "effort", want: ""},
		{value: "EFFORT", want: ""},
		{value: "budget_tokens", want: "budget_tokens"},
		{value: "BUDGET_TOKENS", want: "budget_tokens"},
		{value: "unknown", want: "unknown"},
	}
	for _, test := range tests {
		if got := NormalizeReasoningType(test.value); got != test.want {
			t.Fatalf("NormalizeReasoningType(%q) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestDefaultReasoningConfigAnthropicKeepsEffortDefault(t *testing.T) {
	// The adaptive-Claude default stays on the effort mapping so existing
	// sessions keep their historical reasoning behavior. budget_tokens is
	// enabled explicitly by the user or models.dev fill, never by surprise.
	got := DefaultReasoningConfig("anthropic", "", ModelProfile{ID: "claude-opus-4-7", Type: ProviderTypeAnthropicMessages})
	if got.Type != "" || got.Parameter != "output_config.effort" || got.Default != "high" {
		t.Fatalf("DefaultReasoningConfig() = %#v, want effort default", got)
	}
}
