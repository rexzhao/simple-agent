package providers

import (
	"strings"
	"testing"
)

// Anthropic 的思考档位必须写成 `output_config.effort`，
// 且**不能**出现顶层 reasoning_effort（未知顶层字段会 400）。
//
// 用户配的档位集合是 low/medium/high/xhigh/max（claude-opus-5-5）。
func TestAnthropicBodyUsesOutputConfigEffort(t *testing.T) {
	req := ChatRequest{
		Model:           "claude-opus-5-5",
		System:          "你是助手",
		Messages:        buildStabilityMessages(),
		ReasoningEffort: "xhigh",
	}
	body, err := buildAnthropicBody(req, nil, nil, CacheControlConfig{})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)

	if strings.Contains(text, "reasoning_effort") {
		t.Fatalf("不该出现 reasoning_effort（Anthropic 会 400）：%s", text)
	}
	if !strings.Contains(text, `"output_config":{"effort":"xhigh"}`) {
		t.Fatalf("应写成 output_config.effort=xhigh：%s", text)
	}
}

// 没设档位时不要凭空造一个 output_config —— 有些模型不接受这个字段。
func TestAnthropicBodyOmitsOutputConfigWhenNoEffort(t *testing.T) {
	req := ChatRequest{
		Model:    "claude-opus-5-5",
		Messages: buildStabilityMessages(),
	}
	body, err := buildAnthropicBody(req, nil, nil, CacheControlConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "output_config") {
		t.Fatalf("没有档位时不该写 output_config：%s", string(body))
	}
}

// extra_body 里显式给了 output_config.effort 时，请求里的档位不该覆盖它。
//
// 场景：网关支持的档位集合与配置不同，用户用 extra_body 压掉。
func TestAnthropicExtraBodyEffortWins(t *testing.T) {
	req := ChatRequest{
		Model:           "claude-opus-5-5",
		Messages:        buildStabilityMessages(),
		ReasoningEffort: "high",
	}
	modelExtra := map[string]any{
		"output_config": map[string]any{"effort": "max"},
	}
	body, err := buildAnthropicBody(req, nil, modelExtra, CacheControlConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"effort":"max"`) {
		t.Fatalf("extra_body 的档位应优先：%s", string(body))
	}
}