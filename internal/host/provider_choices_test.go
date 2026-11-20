package host_test

import (
	"os"
	"path/filepath"
	"testing"

	"simple-agent/internal/config"
	"simple-agent/internal/host"
)

// 配置里写了思考档位，界面上就必须能选到 —— **不论什么协议**。
//
// 真实踩到的：Anthropic 的档位被无条件排除（当时顶层 reasoning_effort
// 会 400），后来支持了 output_config.effort，那句排除还在，
// 于是"配了 reasoning_efforts，界面上却没有 effort 选项"。
func TestProviderChoicesExposeEffortsForBothProtocols(t *testing.T) {
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
        reasoning_efforts: [low, medium, high, xhigh, max]
  oai:
    label: OpenAI 兼容
    base_url: https://example.test/v1
    models:
      - id: some-model
        reasoning_efforts: [low, high, max]
      - id: no-effort-model
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	choices := host.ProviderChoices(cfg.Registry())
	byID := map[string]host.ProviderChoice{}
	for _, choice := range choices {
		byID[choice.ProviderID] = choice
	}

	// Anthropic 模型：档位要暴露出来（这是之前缺的）
	anth, ok := byID["anth"]
	if !ok {
		t.Fatal("anthropic provider 应在列表里")
	}
	got := anth.Efforts["claude-opus-5-5"]
	if len(got) != 5 {
		t.Fatalf("Anthropic 模型应暴露 5 个档位，实际 %v", got)
	}
	for i, want := range []string{"low", "medium", "high", "xhigh", "max"} {
		if got[i] != want {
			t.Fatalf("档位顺序应与配置一致：%v", got)
		}
	}

	// OpenAI 兼容模型：同样暴露
	oai := byID["oai"]
	if len(oai.Efforts["some-model"]) != 3 {
		t.Fatalf("OpenAI 兼容模型应暴露 3 个档位，实际 %v", oai.Efforts["some-model"])
	}
	// 没配档位的模型不该有条目
	if _, exists := oai.Efforts["no-effort-model"]; exists {
		t.Fatal("没配 reasoning_efforts 的模型不该有档位条目")
	}
}

// registry 为 nil 时返回 nil（headless 单测会用 nil 构造配置）。
func TestProviderChoicesNilRegistry(t *testing.T) {
	if got := host.ProviderChoices(nil); got != nil {
		t.Fatalf("nil registry 应返回 nil，实际 %v", got)
	}
}