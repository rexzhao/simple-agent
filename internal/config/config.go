// Package config 加载配置文件与启动参数。
//
// 配置来源优先级（后者覆盖前者）：内置默认 → 配置文件 → 环境变量 → 命令行。
//
// 配置文件默认用 **YAML**（simple-agent.yaml / .yml）；为了兼容旧配置，
// 同目录下的 simple-agent.toml 仍然能读（按扩展名选择解析器）。
package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"

	"simple-agent/internal/providers"
)

// Config 是应用配置。
type Config struct {
	// ActiveProvider / ActiveModel 是启动时的模型选择。
	ActiveProvider string `toml:"active_provider" yaml:"active_provider"`
	ActiveModel    string `toml:"active_model" yaml:"active_model"`
	// SystemPrompt 是追加在默认系统提示后的内容。
	SystemPrompt string `toml:"system_prompt" yaml:"system_prompt"`
	// AllowShell 控制是否启用 bash/pwsh 工具。默认 **关闭**（对齐 Rust 版）。
	AllowShell bool `toml:"allow_shell" yaml:"allow_shell"`
	// MaxToolRounds 是一次用户输入内最多几轮工具调用（0 = 不限，默认 12）。
	MaxToolRounds int `toml:"max_tool_rounds" yaml:"max_tool_rounds"`
	// Workspace 是工具的工作区根目录（默认当前目录）。
	Workspace string `toml:"workspace" yaml:"workspace"`
	// DataDir 是会话数据目录（默认按平台取值）。
	DataDir string `toml:"data_dir" yaml:"data_dir"`
	// Language 是界面语言：auto（默认，跟随系统）/ zh / en。
	Language string `toml:"language" yaml:"language"`
	// Providers 覆盖内置提供商（同 ID 覆盖，新 ID 新增）。
	Providers map[string]ProviderOverride `toml:"providers" yaml:"providers"`
	// Tailscale 是可选的 tsnet 配置：启用后程序自己就是 tailnet 上的一个节点，
	// 目标设备**不需要**安装 Tailscale 客户端。
	Tailscale TailscaleConfig `toml:"tailscale" yaml:"tailscale"`
}

// TailscaleConfig 是内置 Tailscale（tsnet）节点的配置。
//
// 全部可选：Enable 为 false 时一切照旧走普通 TCP。
type TailscaleConfig struct {
	// Enable 打开内置 tailscale。
	Enable bool `toml:"enable" yaml:"enable"`
	// Hostname 是节点在 tailnet 里显示的名字（默认 主机名-角色）。
	Hostname string `toml:"hostname" yaml:"hostname"`
	// AuthKey 是 tailnet 认证密钥（tskey-...）。也可以只用环境变量 TS_AUTHKEY，
	// 避免把密钥写进配置文件。
	//
	// 状态目录里已有身份时可以不填（重启复用同一个节点）。
	AuthKey string `toml:"auth_key" yaml:"auth_key"`
	// StateDir 是节点身份/状态目录（默认 <data_dir>/tailscale/<角色>）。
	//
	// **必须持久化，且每个角色分开**：换目录等于换一台新节点。
	StateDir string `toml:"state_dir" yaml:"state_dir"`
	// ControlURL 指向自建的 headscale（为空时用官方控制面）。
	ControlURL string `toml:"control_url" yaml:"control_url"`
	// Ephemeral 让节点离线后自动从 tailnet 移除（一次性调试用）。
	Ephemeral bool `toml:"ephemeral" yaml:"ephemeral"`
	// Tags 是节点的标签（advertise-tags，需在 ACL 里预授权）。
	Tags []string `toml:"tags" yaml:"tags"`
	// Verbose 打开 tsnet 的详细日志（排障用）。
	Verbose bool `toml:"verbose" yaml:"verbose"`
}

// Enabled 报告是否启用了 tsnet。
func (t TailscaleConfig) Enabled() bool { return t.Enable }

// AuthKeyOrEnv 返回实际使用的认证密钥（环境变量优先，便于密钥不落盘）。
func (t TailscaleConfig) AuthKeyOrEnv() string {
	if key := strings.TrimSpace(os.Getenv("TS_AUTHKEY")); key != "" {
		return key
	}
	return strings.TrimSpace(t.AuthKey)
}

// ProviderOverride 覆盖某个提供商的字段。
type ProviderOverride struct {
	Label     string `toml:"label" yaml:"label"`
	Kind      string `toml:"kind" yaml:"kind"`
	BaseURL   string `toml:"base_url" yaml:"base_url"`
	APIKeyEnv string `toml:"api_key_env" yaml:"api_key_env"`
	// Models 是模型列表：每一项描述一个模型**自己的参数**。
	// 也接受旧的裸字符串写法（"deepseek-chat"），等价于只写 id。
	Models []ModelEntry `toml:"models" yaml:"models"`
	// ExtraBody 是这家厂商对所有模型共用的请求体参数（模型自身的同名参数优先）。
	ExtraBody map[string]any `toml:"extra_body" yaml:"extra_body"`
}

// ModelEntry 描述一个模型以及它自己的参数。
//
// 例：
//
//	models:
//	  - id: deepseek-v4.1-flash
//	    context_window: 128000
//	    reasoning_effort: medium
//	  - id: glm-5.3
//	    context_window: 200000      # 始终思考，不需要 reasoning_effort
type ModelEntry struct {
	ID    string `toml:"id" yaml:"id"`
	Label string `toml:"label" yaml:"label"`
	// ContextWindow 是上下文窗口（token），支持 128K / 1M / 2G 这样的写法；
	// 0 表示未知（按 32k 保守处理）。
	ContextWindow Tokens `toml:"context_window" yaml:"context_window"`
	// MaxOutput 是单次回复的最大输出 token 数，同样支持 8K / 128K 等写法。
	// 0 表示不限制（交给服务端默认值）。
	MaxOutput Tokens `toml:"max_output" yaml:"max_output"`
	// Vision 表示这个模型支持图片输入。
	//
	// **未配置 = 不支持**（用户决定）：宁可让用户显式打开，也不让图片请求打到
	// 不支持视觉的模型上（例如 glm-5.3 会直接返回 400）。
	Vision bool `toml:"vision" yaml:"vision"`
	// ReasoningEfforts 是这个模型支持的思考档位（如 [off, low, medium, high]）。
	// 界面按这个列表给出可选档位；空表示没有档位可选。
	ReasoningEfforts []string `toml:"reasoning_efforts" yaml:"reasoning_efforts"`
	// ExtraBody 是这个模型特有的请求体参数（额外的兜底入口）。
	ExtraBody map[string]any `toml:"extra_body" yaml:"extra_body"`
	// params 收集「直接写在模型上的」请求体参数（如 reasoning_effort）。
	params map[string]any
}

// UnmarshalYAML 支持两种写法：裸字符串（旧格式）与对象。
func (m *ModelEntry) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		m.ID = node.Value
		return nil
	}
	type plain ModelEntry
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*m = ModelEntry(decoded)
	return m.collectParams(node)
}

// UnmarshalTOML 支持裸字符串（旧格式）与表。
func (m *ModelEntry) UnmarshalTOML(value any) error {
	switch typed := value.(type) {
	case string:
		m.ID = typed
		return nil
	case map[string]any:
		for key, raw := range typed {
			switch key {
			case "id":
				m.ID, _ = raw.(string)
			case "label":
				m.Label, _ = raw.(string)
			case "context_window":
				if parsed, err := parseTokensValue(raw); err == nil {
					m.ContextWindow = parsed
				}
			case "max_output":
				if parsed, err := parseTokensValue(raw); err == nil {
					m.MaxOutput = parsed
				}
			case "reasoning_efforts":
				if list, ok := raw.([]any); ok {
					for _, item := range list {
						if text, ok := item.(string); ok {
							m.ReasoningEfforts = append(m.ReasoningEfforts, text)
						}
					}
				}
			case "vision":
				// 必须显式认领：落进 default 就会被当成请求体参数发出去，
				// 严格的网关会因未知顶层字段直接 400。
				if flag, ok := raw.(bool); ok {
					m.Vision = flag
				}
			case "extra_body":
				if body, ok := raw.(map[string]any); ok {
					m.ExtraBody = body
				}
			default:
				if m.params == nil {
					m.params = map[string]any{}
				}
				m.params[key] = raw
			}
		}
		return nil
	}
	return fmt.Errorf("models 里的每一项应是字符串或对象，实际 %T", value)
}

// collectParams 把对象里「不认识的键」当成请求体参数（如 reasoning_effort）。
//
// **已知键必须列全**：漏一个（历史上漏过 `vision`）就会把它当请求体参数发出去。
// 这里是白名单，不是黑名单——新增字段务必同步加进来。
func (m *ModelEntry) collectParams(node *yaml.Node) error {
	known := map[string]bool{
		"id": true, "label": true, "context_window": true, "max_output": true,
		"reasoning_efforts": true, "vision": true, "extra_body": true,
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if known[key] {
			continue
		}
		var value any
		if err := node.Content[i+1].Decode(&value); err != nil {
			return fmt.Errorf("解析模型参数 %s 失败：%w", key, err)
		}
		if m.params == nil {
			m.params = map[string]any{}
		}
		m.params[key] = value
	}
	return nil
}

// Tokens 是 token 数量，配置里可以写整数，也可以写 128K / 1M / 2G（不区分大小写，
// 允许小数，如 1.5K；K/M/G 按 1000/1000²/1000³ 计算，与厂商文档口径一致）。
type Tokens int

// String 便于日志与报错。
func (t Tokens) String() string { return strconv.Itoa(int(t)) }

// UnmarshalYAML 接受整数或带单位的字符串。
func (t *Tokens) UnmarshalYAML(node *yaml.Node) error {
	value, err := parseTokens(node.Value)
	if err != nil {
		return err
	}
	*t = value
	return nil
}

// UnmarshalTOML 接受整数或带单位的字符串。
func (t *Tokens) UnmarshalTOML(value any) error {
	parsed, err := parseTokensValue(value)
	if err != nil {
		return err
	}
	*t = parsed
	return nil
}

// parseTokensValue 处理 TOML/YAML 解出来的原始值。
func parseTokensValue(value any) (Tokens, error) {
	switch typed := value.(type) {
	case int64:
		return Tokens(typed), nil
	case int:
		return Tokens(typed), nil
	case float64:
		return Tokens(typed), nil
	case string:
		return parseTokens(typed)
	}
	return 0, fmt.Errorf("token 数应该是整数或形如 128K 的字符串，实际 %T", value)
}

// parseTokens 解析 "128000" / "128K" / "1M" / "2G" / "1.5K"。
func parseTokens(text string) (Tokens, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, nil
	}
	multiplier := 1.0
	switch {
	case strings.HasSuffix(strings.ToLower(trimmed), "k"):
		multiplier, trimmed = 1_000, trimmed[:len(trimmed)-1]
	case strings.HasSuffix(strings.ToLower(trimmed), "m"):
		multiplier, trimmed = 1_000_000, trimmed[:len(trimmed)-1]
	case strings.HasSuffix(strings.ToLower(trimmed), "g"):
		multiplier, trimmed = 1_000_000_000, trimmed[:len(trimmed)-1]
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(trimmed), 64)
	if err != nil {
		return 0, fmt.Errorf("%q 不是合法的 token 数（可用 128K / 1M / 2G）：%w", text, err)
	}
	value := number * multiplier
	if value < 0 {
		return 0, fmt.Errorf("token 数不能为负：%q", text)
	}
	return Tokens(value), nil
}

// requestParams 返回这个模型要合并进请求体的参数。
//
// "reasoning_effort: off" 表示默认关闭思考：不把这个键发出去。
func (m ModelEntry) requestParams() map[string]any {
	if len(m.params) == 0 && len(m.ExtraBody) == 0 {
		return nil
	}
	out := map[string]any{}
	for key, value := range m.params {
		out[key] = value
	}
	for key, value := range m.ExtraBody {
		out[key] = value
	}
	if effort, ok := out["reasoning_effort"].(string); ok && effort == "off" {
		delete(out, "reasoning_effort")
	}
	return out
}

// Defaults 返回内置默认值。
func Defaults() Config {
	return Config{
		MaxToolRounds: 12,
	}
}

// Load 读取配置文件（不存在时不报错），然后应用环境变量与命令行覆盖。
func Load(path string, args []string) (Config, error) {
	cfg := Defaults()
	if path == "" {
		path = DefaultPath()
	}
	if data, err := os.ReadFile(path); err == nil {
		if err := decode(path, data, &cfg); err != nil {
			return cfg, err
		}
	} else if !os.IsNotExist(err) {
		return cfg, fmt.Errorf("读取配置文件 %s 失败：%w", path, err)
	}

	applyEnv(&cfg)
	if err := applyArgs(&cfg, args); err != nil {
		return cfg, err
	}
	if cfg.Workspace == "" {
		if wd, err := os.Getwd(); err == nil {
			cfg.Workspace = wd
		}
	}
	if cfg.DataDir == "" {
		cfg.DataDir = DefaultDataDir()
	}
	return cfg, nil
}

// decode 按扩展名解析配置：.yaml/.yml 用 YAML，其它（默认 .toml）用 TOML。
func decode(path string, data []byte, cfg *Config) error {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return fmt.Errorf("解析配置文件 %s 失败：%w", path, err)
		}
	default:
		if err := toml.Unmarshal(data, cfg); err != nil {
			return fmt.Errorf("解析配置文件 %s 失败：%w", path, err)
		}
	}
	return nil
}

// applyEnv 应用环境变量覆盖。
func applyEnv(cfg *Config) {
	if v, ok := os.LookupEnv("SIMPLE_AGENT_ALLOW_SHELL"); ok {
		value, recognized := parseBool(v)
		if !recognized {
			// 拼错了要能看出来，否则用户会纳闷「为什么设置了没生效」
			log.Printf("警告：SIMPLE_AGENT_ALLOW_SHELL=%q 无法识别，按关闭处理"+
				"（可用值：1/true/yes/on 或 0/false/no/off）", v)
		}
		cfg.AllowShell = value
	}
	if v, ok := os.LookupEnv("SIMPLE_AGENT_DATA"); ok && v != "" {
		cfg.DataDir = v
	}
	// 界面语言：环境变量覆盖配置文件（优先级：环境变量 > 配置 > auto）
	if v, ok := os.LookupEnv("SIMPLE_AGENT_LANG"); ok && strings.TrimSpace(v) != "" {
		cfg.Language = v
	}
	if v, ok := os.LookupEnv("SIMPLE_AGENT_MAX_TOOL_ROUNDS"); ok {
		if n, err := parsePositive(v); err == nil {
			cfg.MaxToolRounds = n
		}
	}
}

// applyArgs 解析命令行参数。
func applyArgs(cfg *Config, args []string) error {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--allow-shell":
			cfg.AllowShell = true
		case "--no-shell":
			cfg.AllowShell = false
		case "--workspace", "-w":
			if i+1 >= len(args) {
				return fmt.Errorf("%s 需要一个路径参数", args[i])
			}
			i++
			cfg.Workspace = args[i]
		case "--config", "-c":
			// 配置文件路径由调用方处理，这里只吃掉参数
			if i+1 >= len(args) {
				return fmt.Errorf("%s 需要一个路径参数", args[i])
			}
			i++
		case "--help", "-h":
			return ErrHelp
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("未知参数：%s", args[i])
			}
		}
	}
	return nil
}

// ErrHelp 表示用户请求帮助。
var ErrHelp = fmt.Errorf("help")

// Help 返回用法说明。
func Help() string {
	return `simple-agent — 本地桌面 AI 助手

用法：simple-agent [选项]

选项：
  --listen ADDR      作为执行端监听（默认只监听回环，如 127.0.0.1:7801）；
                     数据目录仍然只能被一个进程使用
                     注意：直连（--listen）没有认证——监听非回环地址时，
                     任何能连上的人都能操作这个执行端（执行命令、修改文件）
  --connect ADDR     作为展示端连接执行端（与本地模式互斥：不写会话日志、不拿单实例锁）
  --relay ADDR       把本进程作为执行端注册到 relay（断线会自动重连并重新注册）
  --route HOSTID     展示端经 relay 转发（与 --connect 一起用：--connect RELAY --route ID）
  --token TOKEN      relay 的固定口令：relay / 执行端 / 展示端三处必须写成同一个值。
                     它只保护 relay 这一段（--listen 直连仍是开放的），也不加密流量
  --headless         不创建窗口，只作为执行端运行（配合 --listen / --relay）
  --name NAME        执行端在 relay 上的名字（默认主机名；同名会被 relay 拒绝）
  --net-log          打印网络收发日志（每个包的 kind/字节数，排查连接用）
  --tailscale        启用内置 tailscale（tsnet）：本进程自己成为 tailnet 节点，
                     对面设备**不需要**安装 Tailscale 客户端。执行端/展示端/relay 都可用
  --ts-authkey KEY   tailnet 认证密钥（也可用环境变量 TS_AUTHKEY，优先）。
                     不填时复用状态目录里已有的身份；两者都没有会等人工授权
  --ts-hostname NAME 节点在 tailnet 里的名字（默认「主机名-角色」，如 pc-executor）
  --ts-state DIR     节点身份目录（默认 <data_dir>/tailscale/<角色>）。
                     必须持久化：换目录等于换一台新节点
  relay              子命令：simple-agent relay --listen 0.0.0.0:7900 --token 口令，只做帧转发
  --allow-shell        启用 bash/pwsh 工具（默认关闭）
  --no-shell           关闭命令执行
  --workspace, -w DIR  工具的工作区根目录（默认当前目录）
  --config, -c FILE    配置文件路径（默认见下）
  --help, -h           显示本说明

配置文件：` + DefaultPath() + `
  （YAML 格式；旧版 simple-agent.toml 仍然能读）
环境变量：
  DEEPSEEK_API_KEY / OPENAI_API_KEY / ANTHROPIC_API_KEY / MOONSHOT_API_KEY
  ZHIPU_API_KEY / DASHSCOPE_API_KEY     提供商凭据
  SIMPLE_AGENT_ALLOW_SHELL=1            启用命令执行
  SIMPLE_AGENT_MAX_TOOL_ROUNDS=N        工具轮数上限
  SIMPLE_AGENT_DATA=DIR                 数据目录
  SIMPLE_AGENT_LANG=auto|zh|en          界面语言（默认 auto，跟随系统）
`
}

// DefaultPath 返回默认配置文件路径（当前目录优先，其次是配置目录）。
func DefaultPath() string {
	// 先看当前目录，再看用户配置目录；每一处按 yaml → yml → toml（旧格式）找
	names := []string{"simple-agent.yaml", "simple-agent.yml", "simple-agent.toml"}
	for _, base := range configDirs() {
		for _, name := range names {
			candidate := filepath.Join(base, name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "simple-agent", "simple-agent.yaml")
	}
	return "simple-agent.yaml"
}

// configDirs 返回查找配置文件的目录（当前目录优先）。
func configDirs() []string {
	var dirs []string
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	if dir, err := os.UserConfigDir(); err == nil {
		dirs = append(dirs, filepath.Join(dir, "simple-agent"))
	}
	return dirs
}

// DefaultDataDir 返回默认数据目录。
func DefaultDataDir() string {
	if v, ok := os.LookupEnv("SIMPLE_AGENT_DATA"); ok && v != "" {
		return v
	}
	if runtime.GOOS == "linux" {
		if xdg, ok := os.LookupEnv("XDG_DATA_HOME"); ok && xdg != "" {
			return filepath.Join(xdg, "simple-agent")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", "simple-agent")
		}
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "simple-agent")
	}
	return "simple-agent-data"
}

// Registry 把配置（含 providers 覆盖）转成提供商注册表。
//
// 覆盖按 ID 排序后应用：TOML 的 map 遍历顺序随机，排序让结果可复现。
func (c Config) Registry() *providers.Registry {
	registry := providers.NewRegistry()
	registry.ActiveProvider = c.ActiveProvider
	registry.ActiveModel = c.ActiveModel
	registry.SystemPrompt = c.SystemPrompt
	registry.AllowShell = c.AllowShell

	ids := make([]string, 0, len(c.Providers))
	for id := range c.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	overrides := make([]providers.Override, 0, len(ids))
	for _, id := range ids {
		item := c.Providers[id]
		specs := make([]providers.ModelSpec, 0, len(item.Models))
		for _, model := range item.Models {
			specs = append(specs, providers.ModelSpec{
				ID:               model.ID,
				Label:            model.Label,
				ContextWindow:    int(model.ContextWindow),
				MaxOutput:        int(model.MaxOutput),
				ReasoningEfforts: model.ReasoningEfforts,
				Vision:           model.Vision,
				ExtraBody:        model.requestParams(),
			})
		}
		override := providers.Override{
			ID:        id,
			Label:     item.Label,
			Kind:      providers.Kind(item.Kind),
			BaseURL:   item.BaseURL,
			Models:    specs,
			ExtraBody: item.ExtraBody,
		}
		if item.APIKeyEnv != "" {
			env := item.APIKeyEnv
			override.APIKeyEnv = &env
		}
		overrides = append(overrides, override)
	}
	registry.ApplyOverrides(overrides)
	return registry
}

// parseBool 解析布尔环境变量；第二个返回值表示是否识别成功。
//
// 无法识别的值按 false 处理（默认拒绝，安全），但调用方会给出警告。
func parseBool(v string) (value bool, recognized bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off", "":
		return false, true
	}
	return false, false
}

// parsePositive 解析非负整数。用 strconv.Atoi 而不是 Sscanf：
// 后者会把 "12abc" 解析成 12，用户写错了也看不出来。
func parsePositive(v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("需要非负整数：%w", err)
	}
	if n < 0 {
		return 0, fmt.Errorf("需要非负整数，实际 %d", n)
	}
	return n, nil
}
