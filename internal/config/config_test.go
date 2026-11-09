package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"simple-agent/internal/config"
	"simple-agent/internal/providers"
)

// writeConfig 在临时目录写一份配置，返回路径。
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "simple-agent.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaults(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "missing.toml"), nil)
	if err != nil {
		t.Fatalf("缺配置文件不应报错：%v", err)
	}
	if cfg.AllowShell {
		t.Fatal("命令执行默认应关闭")
	}
	if cfg.MaxToolRounds != 12 {
		t.Fatalf("工具轮数默认应为 12，实际 %d", cfg.MaxToolRounds)
	}
	if cfg.DataDir == "" || cfg.Workspace == "" {
		t.Fatalf("应补全默认目录：%+v", cfg)
	}
}

func TestFileOverridesDefaults(t *testing.T) {
	path := writeConfig(t, `
active_provider = "deepseek"
active_model = "deepseek-chat"
system_prompt = "回答尽量简短"
allow_shell = true
max_tool_rounds = 3
workspace = "/tmp/ws"

[providers.deepseek]
base_url = "https://example.test/v1"
models = ["m1", "m2"]
`)
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowShell || cfg.MaxToolRounds != 3 {
		t.Fatalf("文件未生效：%+v", cfg)
	}
	if cfg.ActiveProvider != "deepseek" || cfg.ActiveModel != "deepseek-chat" {
		t.Fatalf("选择未生效：%+v", cfg)
	}
	if cfg.Providers["deepseek"].BaseURL != "https://example.test/v1" {
		t.Fatalf("providers 覆盖未解析：%+v", cfg.Providers)
	}
}

func TestEnvAndArgsOverrideFile(t *testing.T) {
	path := writeConfig(t, "allow_shell = true\nmax_tool_rounds = 5\n")
	// 只用 SIMPLE_AGENT_ALLOW_SHELL 一种说法（Rust 阶段收敛过：两个变量会打架）
	t.Setenv("SIMPLE_AGENT_ALLOW_SHELL", "0")
	t.Setenv("SIMPLE_AGENT_MAX_TOOL_ROUNDS", "2")
	t.Setenv("SIMPLE_AGENT_DATA", "/tmp/data")

	cfg, err := config.Load(path, []string{"--workspace", "/tmp/argws"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowShell {
		t.Fatal("SIMPLE_AGENT_ALLOW_SHELL=0 应关闭命令执行")
	}
	if cfg.MaxToolRounds != 2 {
		t.Fatalf("环境变量应覆盖文件：%d", cfg.MaxToolRounds)
	}
	if cfg.DataDir != "/tmp/data" {
		t.Fatalf("数据目录未被环境变量覆盖：%q", cfg.DataDir)
	}
	if cfg.Workspace != "/tmp/argws" {
		t.Fatalf("命令行应覆盖工作区：%q", cfg.Workspace)
	}
}

// 无法识别的布尔值按关闭处理（安全默认），并给出警告。
func TestUnrecognizedBoolWarns(t *testing.T) {
	t.Setenv("SIMPLE_AGENT_ALLOW_SHELL", "yess")
	cfg, err := config.Load(filepath.Join(t.TempDir(), "x.toml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowShell {
		t.Fatal("无法识别的值应按关闭处理")
	}
}

// "12abc" 不能被悄悄解析成 12。
func TestInvalidMaxToolRoundsIgnored(t *testing.T) {
	t.Setenv("SIMPLE_AGENT_MAX_TOOL_ROUNDS", "12abc")
	cfg, err := config.Load(filepath.Join(t.TempDir(), "x.toml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxToolRounds != 12 {
		t.Fatalf("非法值应被忽略并保留默认 12，实际 %d", cfg.MaxToolRounds)
	}
}

func TestAllowShellFlag(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "x.toml"), []string{"--allow-shell"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowShell {
		t.Fatal("--allow-shell 应生效")
	}
}

func TestHelpAndUnknownArgs(t *testing.T) {
	if _, err := config.Load(filepath.Join(t.TempDir(), "x.toml"), []string{"--help"}); err != config.ErrHelp {
		t.Fatalf("--help 应返回 ErrHelp，实际 %v", err)
	}
	if !strings.Contains(config.Help(), "--allow-shell") {
		t.Fatal("帮助信息应包含关键选项")
	}
	if _, err := config.Load(filepath.Join(t.TempDir(), "x.toml"), []string{"--nope"}); err == nil {
		t.Fatal("未知参数应报错")
	}
	if _, err := config.Load(filepath.Join(t.TempDir(), "x.toml"), []string{"--workspace"}); err == nil {
		t.Fatal("缺参数值应报错")
	}
}

func TestInvalidTOML(t *testing.T) {
	path := writeConfig(t, "allow_shell = [1,2\n")
	if _, err := config.Load(path, nil); err == nil {
		t.Fatal("非法 TOML 应报错")
	}
}

// 配置里的 providers 覆盖要真的作用到注册表：改 base_url、加新厂商、改模型列表。
func TestRegistryAppliesOverrides(t *testing.T) {
	path := writeConfig(t, `
[providers.deepseek]
base_url = "https://proxy.test/v1"
models = ["custom-model"]

[providers.mycorp]
label = "公司内网"
base_url = "http://10.0.0.5:8000/v1"
api_key_env = "MYCORP_KEY"
models = ["internal-1"]
`)
	t.Setenv("MYCORP_KEY", "sk-test")
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := cfg.Registry()

	deepseek, ok := registry.Get("deepseek")
	if !ok {
		t.Fatal("内置提供商应仍在")
	}
	if deepseek.Config.BaseURL != "https://proxy.test/v1" {
		t.Fatalf("base_url 未覆盖：%q", deepseek.Config.BaseURL)
	}
	if ids := deepseek.Config.ModelIDs(); len(ids) != 1 || ids[0] != "custom-model" {
		t.Fatalf("模型列表未覆盖：%v", ids)
	}

	custom, ok := registry.Get("mycorp")
	if !ok {
		t.Fatal("新厂商应被加入")
	}
	if custom.Config.Label != "公司内网" || custom.Config.Kind != providers.KindOpenAI {
		t.Fatalf("新厂商配置不一致：%+v", custom.Config)
	}
	if !custom.Configured() {
		t.Fatal("有环境变量凭据时应视为已配置")
	}

	// 模拟提供商仍在末尾，且不可被覆盖成真实厂商
	if _, ok := registry.Get("simulated"); !ok {
		t.Fatal("模拟提供商应保留")
	}
}

// 覆盖只写 base_url 时，其它字段保持原值。
func TestPartialOverrideKeepsDefaults(t *testing.T) {
	path := writeConfig(t, `
[providers.dashscope]
base_url = "http://localhost:9999/v1"
`)
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := cfg.Registry().Get("dashscope")
	if !ok {
		t.Fatal("提供商应存在")
	}
	if entry.Config.APIKeyEnv != "DASHSCOPE_API_KEY" {
		t.Fatalf("未写的字段应保持原值：%q", entry.Config.APIKeyEnv)
	}
	if len(entry.Config.Models) == 0 {
		t.Fatal("未写 models 时应保留内置模型列表")
	}
}

// YAML 是默认格式：同样能读出所有字段（含 providers 覆盖与窗口）。
func TestLoadYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "simple-agent.yaml")
	content := `
active_provider: paperhub
active_model: glm-5.3
allow_shell: true
max_tool_rounds: 4
providers:
  paperhub:
    label: PaperHub
    base_url: https://example.test/v1
    api_key_env: PAPERHUB_API_KEY
    models:
      - id: glm-5.3
        context_window: 200000
      - id: deepseek-v4.1-flash
        context_window: 128000
        reasoning_effort: medium
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatalf("YAML 应能解析：%v", err)
	}
	if cfg.ActiveProvider != "paperhub" || cfg.ActiveModel != "glm-5.3" {
		t.Fatalf("默认选择不一致：%+v", cfg)
	}
	if !cfg.AllowShell || cfg.MaxToolRounds != 4 {
		t.Fatalf("基础字段不一致：%+v", cfg)
	}
	override := cfg.Providers["paperhub"]
	if override.BaseURL != "https://example.test/v1" || len(override.Models) != 2 {
		t.Fatalf("providers 覆盖不一致：%+v", override)
	}
	if override.Models[0].ID != "glm-5.3" || override.Models[0].ContextWindow != 200000 {
		t.Fatalf("模型参数未解析：%+v", override.Models[0])
	}

	// 注册表里窗口与模型参数都要生效
	entry, ok := cfg.Registry().Get("paperhub")
	if !ok || entry.Config.ContextWindow("glm-5.3") != 200000 {
		t.Fatalf("窗口未生效：%+v", entry.Config)
	}
	if got := entry.Config.ModelExtra("deepseek-v4.1-flash")["reasoning_effort"]; got != "medium" {
		t.Fatalf("模型参数未生效：%v", got)
	}
	if extra := entry.Config.ModelExtra("glm-5.3"); len(extra) != 0 {
		t.Fatalf("glm-5.3 不应带 reasoning_effort：%v", extra)
	}
}

// 默认路径优先找 yaml（当前目录 / 用户配置目录），其次 yml，最后兼容 toml。
func TestDefaultPathPrefersYAML(t *testing.T) {
	dir := t.TempDir()
	// 隔离用户配置目录，避免读到本机真实的 simple-agent.toml
	t.Setenv("APPDATA", filepath.Join(dir, "appdata"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
	if err := os.MkdirAll(filepath.Join(dir, "appdata"), 0o755); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	if got := filepath.Base(config.DefaultPath()); got != "simple-agent.yaml" {
		t.Fatalf("没有配置文件时应默认 yaml，实际 %s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "simple-agent.toml"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(config.DefaultPath()); got != "simple-agent.toml" {
		t.Fatalf("只有旧 toml 时应回退到它，实际 %s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "simple-agent.yaml"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(config.DefaultPath()); got != "simple-agent.yaml" {
		t.Fatalf("yaml 存在时应优先，实际 %s", got)
	}
}

// 按模型的请求体参数（model_extra）要能解析并且优先于厂商级 extra_body。
func TestModelExtraParsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "simple-agent.yaml")
	content := `
providers:
  paperhub:
    base_url: https://example.test/v1
    extra_body:
      reasoning_effort: low
    models:
      - id: deepseek-v4.1-flash
        reasoning_effort: medium
      - id: glm-5.3
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := cfg.Registry().Get("paperhub")
	if !ok {
		t.Fatal("paperhub 未注册")
	}
	// 模型自己的参数优先于厂商级
	if got := entry.Config.ModelExtra("deepseek-v4.1-flash")["reasoning_effort"]; got != "medium" {
		t.Fatalf("模型参数未解析：%v", got)
	}
	if got := entry.Config.ModelExtra("glm-5.3")["reasoning_effort"]; got != "low" {
		t.Fatalf("没写模型参数时应继承厂商级：%v", got)
	}
}

// token 数量支持 128K / 1M / 2G / 1.5K / 纯整数。
func TestTokenSuffixParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "simple-agent.yaml")
	content := `
providers:
  paperhub:
    base_url: https://example.test/v1
    models:
      - id: k
        context_window: 128K
        max_output: 8K
      - id: m
        context_window: 1M
        max_output: 64k
      - id: decimal
        context_window: 1.5K
      - id: plain
        context_window: 32000
        max_output: 4096
      - id: g
        context_window: 2G
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := cfg.Registry().Get("paperhub")
	cases := map[string][2]int{
		"k":       {128_000, 8_000},
		"m":       {1_000_000, 64_000},
		"decimal": {1_500, 0},
		"plain":   {32_000, 4_096},
		"g":       {2_000_000_000, 0},
	}
	for id, want := range cases {
		model, ok := entry.Config.Model(id)
		if !ok {
			t.Fatalf("模型 %s 未注册", id)
		}
		if model.ContextWindow != want[0] || model.MaxOutput != want[1] {
			t.Fatalf("%s：window=%d max_output=%d，期望 %v", id, model.ContextWindow, model.MaxOutput, want)
		}
	}

	// 非法写法要报错，而不是静默变成 0
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("providers:\n  p:\n    models:\n      - id: x\n        context_window: \"128X\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(bad, nil); err == nil {
		t.Fatal("非法 token 写法应报错")
	}
}

// 思考档位：列表与默认档位都要解析；off 表示默认不带该参数。
func TestReasoningEffortsParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "simple-agent.yaml")
	content := `
providers:
  paperhub:
    base_url: https://example.test/v1
    models:
      - id: with-off
        reasoning_efforts: [off, low, medium, high]
        reasoning_effort: off
      - id: glm
        reasoning_efforts: [low, high, max]
        reasoning_effort: high
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := cfg.Registry().Get("paperhub")
	if got := entry.Config.Efforts("with-off"); len(got) != 4 {
		t.Fatalf("档位列表未解析：%v", got)
	}
	// off → 请求体里不带 reasoning_effort
	if extra := entry.Config.ModelExtra("with-off"); extra["reasoning_effort"] != nil {
		t.Fatalf("off 时不应带参数：%v", extra)
	}
	if got := entry.Config.ModelExtra("glm")["reasoning_effort"]; got != "high" {
		t.Fatalf("默认档位未解析：%v", got)
	}
}

// vision 是配置字段，**不是**请求体参数：写成模型属性时绝不能泄露到请求体里
// （严格的网关会因未知顶层字段直接 400）。
func TestVisionIsNotSentInRequestBody(t *testing.T) {
	yamlPath := filepath.Join(t.TempDir(), "simple-agent.yaml")
	yamlContent := `
providers:
  paperhub:
    base_url: https://example.test/v1
    models:
      - id: see
        vision: true
      - id: blind
        vision: false
`
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	tomlPath := writeConfig(t, `
[providers.paperhub]
base_url = "https://example.test/v1"

[[providers.paperhub.models]]
id = "see"
vision = true

[[providers.paperhub.models]]
id = "blind"
vision = false
`)

	for _, path := range []string{yamlPath, tomlPath} {
		cfg, err := config.Load(path, nil)
		if err != nil {
			t.Fatalf("%s 解析失败：%v", filepath.Base(path), err)
		}
		entry, ok := cfg.Registry().Get("paperhub")
		if !ok {
			t.Fatalf("%s：paperhub 未注册", filepath.Base(path))
		}
		if !entry.Config.SupportsVision("see") {
			t.Fatalf("%s：vision: true 未生效", filepath.Base(path))
		}
		if entry.Config.SupportsVision("blind") {
			t.Fatalf("%s：vision: false 被当成支持", filepath.Base(path))
		}
		for _, id := range []string{"see", "blind"} {
			if _, leaked := entry.Config.ModelExtra(id)["vision"]; leaked {
				t.Fatalf("%s：%s 的 vision 泄露进了请求体参数：%v",
					filepath.Base(path), id, entry.Config.ModelExtra(id))
			}
		}
	}
}

// 模型可以配置显示名；没配时界面用 id。
func TestModelLabelParsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "simple-agent.yaml")
	content := `
providers:
  paperhub:
    base_url: https://example.test/v1
    models:
      - id: deepseek-v4.1-flash
        label: DeepSeek V4.1 Flash
        context_window: 128K
      - id: glm-5.3
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := cfg.Registry().Get("paperhub")
	if got := entry.Config.ModelLabel("deepseek-v4.1-flash"); got != "DeepSeek V4.1 Flash" {
		t.Fatalf("显示名未解析：%q", got)
	}
	// 没配 label 的退回 id
	if got := entry.Config.ModelLabel("glm-5.3"); got != "glm-5.3" {
		t.Fatalf("没配 label 时应退回 id：%q", got)
	}
}
