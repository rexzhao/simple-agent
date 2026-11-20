package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"simple-agent/internal/config"
)

// Anthropic 协议的思考档位要变成 `output_config.effort`，不能留着
// `reasoning_effort`（那是 OpenAI 兼容字段，Anthropic 会 400）。
//
// 配置里统一写 reasoning_effort（最直观），由 config 按 kind 搬到正确位置 ——
// 不然用户得为每个协议记一套写法，写错了还只看到 400。
func TestAnthropicEffortBecomesOutputConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "simple-agent.yaml")
	yaml := `
providers:
  anth:
    label: Anthropic
    kind: anthropic
    base_url: https://example.test/anthropic/v1
    models:
      - id: claude-opus-5-5
        context_window: 1000K
        max_output: 128K
        reasoning_efforts: [low, medium, high, xhigh, max]
        reasoning_effort: high
  oai:
    label: OpenAI 兼容
    base_url: https://example.test/v1
    models:
      - id: some-model
        reasoning_effort: high
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := cfg.Registry()

	// Anthropic：reasoning_effort 搬进 output_config.effort
	anth, ok := registry.Get("anth")
	if !ok {
		t.Fatal("anthropic provider 没加载")
	}
	extra := anth.Config.Models[0].ExtraBody
	if _, exists := extra["reasoning_effort"]; exists {
		t.Fatalf("Anthropic 不该保留 reasoning_effort（会 400）：%v", extra)
	}
	outputConfig, ok := extra["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("应生成 output_config，实际：%v", extra)
	}
	if got := outputConfig["effort"]; got != "high" {
		t.Fatalf("output_config.effort 应为 high，实际 %v", got)
	}

	// OpenAI 兼容：保持原样，不要动
	oai, ok := registry.Get("oai")
	if !ok {
		t.Fatal("openai provider 没加载")
	}
	oaiExtra := oai.Config.Models[0].ExtraBody
	if got := oaiExtra["reasoning_effort"]; got != "high" {
		t.Fatalf("OpenAI 兼容协议应保持 reasoning_effort=high，实际 %v", oaiExtra)
	}
	if _, exists := oaiExtra["output_config"]; exists {
		t.Fatalf("OpenAI 兼容协议不该有 output_config：%v", oaiExtra)
	}
}