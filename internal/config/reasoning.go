package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

var canonicalReasoningLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// ReasoningConfig type constants. The empty value means effort mapping for
// backward compatibility with existing provider files.
const (
	ReasoningTypeEffort       = "effort"
	ReasoningTypeBudgetTokens = "budget_tokens"
)

// NormalizeReasoningType canonicalizes a reasoning config type. It returns the
// empty string for unrecognized values so validation can report a precise
// error instead of silently downgrading.
func NormalizeReasoningType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", ReasoningTypeEffort:
		return ""
	case ReasoningTypeBudgetTokens:
		return ReasoningTypeBudgetTokens
	default:
		return value
	}
}

// normalizeReasoningConfig fills the Type field when it is empty and freezes
// the canonical form used by equality and persistence.
func normalizeReasoningConfig(reasoning ReasoningConfig) ReasoningConfig {
	reasoning.Type = NormalizeReasoningType(reasoning.Type)
	return reasoning
}

// DefaultReasoningConfig returns common provider/model mappings based on Pi's
// thinking-level catalog. Unknown models remain unconfigured so an unsupported
// parameter is never sent speculatively.
func DefaultReasoningConfig(providerName, baseURL string, model ModelProfile) ReasoningConfig {
	modelID := strings.ToLower(strings.TrimSpace(model.ID))
	modelType := strings.TrimSpace(model.Type)
	if modelType == "" {
		modelType = ProviderTypeOpenAIChat
	}
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	baseURL = strings.ToLower(strings.TrimSpace(baseURL))

	if modelType == ProviderTypeOpenAICodex && strings.HasPrefix(modelID, "gpt-5") {
		levels := commonEffortLevels()
		levels["off"] = "none"
		levels["minimal"] = "low"
		addOpenAIExtendedLevels(levels, modelID)
		return normalizeReasoningConfig(ReasoningConfig{Parameter: "reasoning.effort", Default: preferredOpenAIDefault(levels), Levels: levels})
	}

	if modelType == ProviderTypeOpenAIResponses && strings.HasPrefix(modelID, "gpt-5") {
		levels := commonEffortLevels()
		if providerName == "openai" || strings.Contains(baseURL, "api.openai.com") {
			levels["off"] = "none"
			if strings.Contains(modelID, "gpt-5.5") {
				delete(levels, "minimal")
			}
		}
		addOpenAIExtendedLevels(levels, modelID)
		return normalizeReasoningConfig(ReasoningConfig{Parameter: "reasoning.effort", Default: preferredOpenAIDefault(levels), Levels: levels})
	}

	if modelType == ProviderTypeAnthropicMessages && isAdaptiveClaudeModel(modelID) {
		levels := commonEffortLevels()
		levels["minimal"] = "low"
		levels["max"] = "max"
		if supportsClaudeXHigh(modelID) {
			levels["xhigh"] = "xhigh"
		}
		return normalizeReasoningConfig(ReasoningConfig{Parameter: "output_config.effort", Default: "high", Levels: levels})
	}

	if modelType != ProviderTypeOpenAIChat {
		return ReasoningConfig{}
	}
	parameter := "reasoning_effort"
	if providerName == "openrouter" || strings.Contains(baseURL, "openrouter.ai") {
		parameter = "reasoning.effort"
	}
	if modelID == "glm-5.2" || strings.Contains(modelID, "deepseek-v4") {
		// DeepSeek v4 (including the fast deepseek-v4-flash tier) exposes
		// exactly three thinking levels: low, high, and max. GLM-5.2 keeps
		// its own compact set and only advertises low/medium on Z.ai.
		levels := deepseekV4Levels()
		if modelID == "glm-5.2" {
			levels = map[string]any{"high": "high", "max": "max"}
			if isZAIEndpoint(providerName, baseURL) {
				levels["low"] = "high"
				levels["medium"] = "high"
			}
		}
		return normalizeReasoningConfig(ReasoningConfig{
			Parameter: parameter,
			Default:   "high",
			Levels:    levels,
		})
	}
	if strings.HasPrefix(modelID, "gpt-5") || strings.HasPrefix(modelID, "o1") || strings.HasPrefix(modelID, "o3") || strings.HasPrefix(modelID, "o4") {
		levels := commonEffortLevels()
		levels["off"] = "none"
		addOpenAIExtendedLevels(levels, modelID)
		return normalizeReasoningConfig(ReasoningConfig{Parameter: parameter, Default: preferredOpenAIDefault(levels), Levels: levels})
	}
	return ReasoningConfig{}
}

func ReasoningLevelNames(levels map[string]any) []string {
	result := make([]string, 0, len(levels))
	seen := make(map[string]struct{}, len(levels))
	for _, level := range canonicalReasoningLevels {
		if _, ok := levels[level]; ok {
			result = append(result, level)
			seen[level] = struct{}{}
		}
	}
	custom := make([]string, 0, len(levels)-len(seen))
	for level := range levels {
		if _, ok := seen[level]; !ok {
			custom = append(custom, level)
		}
	}
	sort.Strings(custom)
	return append(result, custom...)
}

// ResolveReasoningLevel returns the unified level that ApplyReasoningLevel
// would apply: the explicit selection, or the configured default when the
// selection is empty. It returns "" when no reasoning levels are configured,
// matching ApplyReasoningLevel's no-op case.
func ResolveReasoningLevel(reasoning ReasoningConfig, selected string) string {
	selected = strings.TrimSpace(selected)
	if selected == "" {
		selected = strings.TrimSpace(reasoning.Default)
	}
	if selected == "" || len(reasoning.Levels) == 0 {
		return ""
	}
	return selected
}

func commonEffortLevels() map[string]any {
	return map[string]any{
		"minimal": "minimal",
		"low":     "low",
		"medium":  "medium",
		"high":    "high",
	}
}

// deepseekV4Levels returns the three thinking levels shared by every
// DeepSeek v4 member, including the fast deepseek-v4-flash tier: low, high,
// and max. SAI exposes exactly these options so a session always picks a
// 思考等级 from the real catalog instead of a binary on/off switch.
func deepseekV4Levels() map[string]any {
	return map[string]any{
		"low":  "low",
		"high": "high",
		"max":  "max",
	}
}

// isZAIEndpoint reports whether a provider routes to Z.ai (Zhipu) endpoints,
// which collapse the low/medium thinking names onto the heavier effort value.
func isZAIEndpoint(providerName, baseURL string) bool {
	return providerName == "zai" || providerName == "z-ai" ||
		strings.Contains(baseURL, "api.z.ai") || strings.Contains(baseURL, "bigmodel.cn")
}

func addOpenAIExtendedLevels(levels map[string]any, modelID string) {
	if strings.Contains(modelID, "gpt-5.2") || strings.Contains(modelID, "gpt-5.3") || strings.Contains(modelID, "gpt-5.4") || strings.Contains(modelID, "gpt-5.5") || strings.Contains(modelID, "gpt-5.6") {
		levels["xhigh"] = "xhigh"
	}
	if strings.Contains(modelID, "gpt-5.6") {
		levels["max"] = "max"
	}
}

func preferredOpenAIDefault(levels map[string]any) string {
	if _, ok := levels["xhigh"]; ok {
		return "xhigh"
	}
	return "high"
}

func isAdaptiveClaudeModel(modelID string) bool {
	return strings.Contains(modelID, "opus-4-6") || strings.Contains(modelID, "opus-4.6") ||
		strings.Contains(modelID, "opus-4-7") || strings.Contains(modelID, "opus-4.7") ||
		strings.Contains(modelID, "opus-4-8") || strings.Contains(modelID, "opus-4.8") ||
		strings.Contains(modelID, "sonnet-4-6") || strings.Contains(modelID, "sonnet-4.6") ||
		strings.Contains(modelID, "sonnet-5") || strings.Contains(modelID, "sonnet.5") ||
		strings.Contains(modelID, "fable-5")
}

func supportsClaudeXHigh(modelID string) bool {
	return strings.Contains(modelID, "opus-4-7") || strings.Contains(modelID, "opus-4.7") ||
		strings.Contains(modelID, "opus-4-8") || strings.Contains(modelID, "opus-4.8") ||
		strings.Contains(modelID, "sonnet-5") || strings.Contains(modelID, "sonnet.5") ||
		strings.Contains(modelID, "fable-5")
}

func ApplyReasoningLevel(parameters map[string]any, reasoning ReasoningConfig, selected string) (map[string]any, error) {
	result := copyParameters(parameters)
	selected = ResolveReasoningLevel(reasoning, selected)
	if selected == "" {
		return result, nil
	}
	value, ok := reasoning.Levels[selected]
	if !ok {
		return nil, fmt.Errorf("unknown reasoning level %q", selected)
	}
	if reasoning.Type == ReasoningTypeBudgetTokens {
		number, ok := numericLevelValue(value)
		if !ok {
			return nil, fmt.Errorf("reasoning level %q must map to a number for budget_tokens type", selected)
		}
		value = number
	}
	path := strings.TrimSpace(reasoning.Parameter)
	if path == "" {
		return nil, fmt.Errorf("reasoning parameter path is required")
	}
	parts := strings.Split(path, ".")
	current := result
	for index, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("reasoning parameter path %q is invalid", path)
		}
		if index == len(parts)-1 {
			current[part] = value
			break
		}
		next := map[string]any{}
		if existing, ok := current[part].(map[string]any); ok {
			for key, item := range existing {
				next[key] = item
			}
		}
		current[part] = next
		current = next
	}
	return result, nil
}

// numericLevelValue converts a reasoning level value to its numeric form for
// budget_tokens mappings. It accepts int, int64, float64, and json.Number so
// YAML and JSON decoders keep the same target value.
func numericLevelValue(value any) (any, bool) {
	switch value := value.(type) {
	case int:
		return value, true
	case int64:
		return value, true
	case float64:
		return value, true
	case json.Number:
		if parsed, err := value.Int64(); err == nil {
			return parsed, true
		}
		if parsed, err := value.Float64(); err == nil {
			return parsed, true
		}
		return nil, false
	default:
		return nil, false
	}
}
