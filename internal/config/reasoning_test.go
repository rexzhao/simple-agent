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
