package providers

import (
	"os"
	"strings"
)

// Resolved 是一家已解析凭据的提供商。
type Resolved struct {
	Config ProviderConfig
	APIKey string
}

// Configured 报告凭据是否齐备（无需鉴权的本地服务视为已配置）。
//
// 只有空白字符的 Key 视为未配置：否则会带着空格发请求，拿回一个 401，
// 用户还看不出是配置问题。
func (r Resolved) Configured() bool {
	return r.Config.APIKeyEnv == "" || strings.TrimSpace(r.APIKey) != ""
}

// Registry 管理全部提供商（内置 + 配置覆盖）。
type Registry struct {
	providers []Resolved
	// ActiveProvider / ActiveModel 来自配置文件，可为空。
	ActiveProvider string
	ActiveModel    string
	SystemPrompt   string
	AllowShell     bool
}

// builtinProviders 返回内置提供商。
//
// ContextWindow 的数值**待复核**：写的是各厂商文档里的常见值，但没有逐项核对，
// 也不保证与最新版本一致。0 表示未知；压缩逻辑会回落到保守默认 32k
// （宁可早压缩，也不要超窗口被拒）。
//
// IncludeUsage：只有确认支持 stream_options.include_usage 的才打开。不认识这个
// 字段的实现会直接 400，因此兼容厂商（moonshot/zhipu/dashscope）先关掉，
// 冒烟测试通过后再逐个打开。
func builtinProviders() []ProviderConfig {
	openai := KindOpenAI
	return []ProviderConfig{
		{
			ID: "deepseek", Label: "DeepSeek", Kind: openai,
			BaseURL: "https://api.deepseek.com/v1", APIKeyEnv: "DEEPSEEK_API_KEY",
			Models: []ModelConfig{
				{ID: "deepseek-chat", ContextWindow: 64_000},
				{ID: "deepseek-reasoner", ContextWindow: 64_000},
			},
			IncludeUsage: true,
		},
		{
			ID: "openai", Label: "OpenAI", Kind: openai,
			BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY",
			Models: []ModelConfig{
				{ID: "gpt-4o-mini", ContextWindow: 128_000},
				{ID: "gpt-4o", ContextWindow: 128_000},
			},
			IncludeUsage: true,
		},
		{
			ID: "anthropic", Label: "Anthropic", Kind: KindAnthropic,
			BaseURL: "https://api.anthropic.com/v1", APIKeyEnv: "ANTHROPIC_API_KEY",
			Models: []ModelConfig{
				{ID: "claude-sonnet-4-5", ContextWindow: 200_000},
				{ID: "claude-haiku-4-5", ContextWindow: 200_000},
			},
			IncludeUsage: true,
		},
		{
			ID: "moonshot", Label: "Moonshot", Kind: openai,
			BaseURL: "https://api.moonshot.cn/v1", APIKeyEnv: "MOONSHOT_API_KEY",
			Models: []ModelConfig{
				{ID: "moonshot-v1-8k", ContextWindow: 8_000},
				{ID: "moonshot-v1-32k", ContextWindow: 32_000},
			},
			IncludeUsage: false,
		},
		{
			ID: "zhipu", Label: "智谱 GLM", Kind: openai,
			BaseURL: "https://open.bigmodel.cn/api/paas/v4", APIKeyEnv: "ZHIPU_API_KEY",
			Models: []ModelConfig{
				{ID: "glm-4-plus", ContextWindow: 128_000},
				{ID: "glm-4-flash", ContextWindow: 128_000},
			},
			IncludeUsage: false,
		},
		{
			ID: "dashscope", Label: "通义千问", Kind: openai,
			BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", APIKeyEnv: "DASHSCOPE_API_KEY",
			Models: []ModelConfig{
				{ID: "qwen-max", ContextWindow: 32_000},
				{ID: "qwen-plus", ContextWindow: 128_000},
			},
			IncludeUsage: false,
		},
		{
			ID: "ollama", Label: "Ollama（本地）", Kind: openai,
			BaseURL: "http://localhost:11434/v1", APIKeyEnv: "",
			Models: []ModelConfig{
				{ID: "qwen2.5:7b", ContextWindow: 32_000},
				{ID: "llama3.2:3b", ContextWindow: 8_000},
			},
			IncludeUsage: false,
		},
	}
}

// NewRegistry 构造注册表（内置列表 + 环境变量凭据）。
func NewRegistry() *Registry {
	configs := builtinProviders()
	resolved := make([]Resolved, 0, len(configs))
	for _, cfg := range configs {
		key := ""
		if cfg.APIKeyEnv != "" {
			key = os.Getenv(cfg.APIKeyEnv)
		}
		resolved = append(resolved, Resolved{Config: cfg, APIKey: key})
	}
	// 模拟提供商也在注册表里，这样 Get/Build 不需要靠字符串 "simulated" 特判
	// （用户配置里出现同名 provider 时才不会撞上）。它不会出现在选择器里。
	resolved = append(resolved, Resolved{Config: SimulatedConfig()})
	return &Registry{providers: resolved}
}

// Override 是配置文件对某个提供商的覆盖。
//
// 空字段表示「保持原值」：只写 base_url 就只换地址。ID 不存在时新增一家。
type Override struct {
	ID        string
	Label     string
	Kind      Kind
	BaseURL   string
	APIKeyEnv *string
	// Models 是模型列表（每一项带自己的参数）。
	Models []ModelSpec
	// ExtraBody 是这家厂商对所有模型共用的请求体参数（模型的同名参数优先）。
	ExtraBody map[string]any
}

// ModelSpec 是配置文件里的一个模型：id + 该模型自己的参数。
type ModelSpec struct {
	ID            string
	Label         string
	ContextWindow int
	// MaxOutput 是单次回复的最大输出 token 数（0 = 不限制）。
	MaxOutput int
	// ReasoningEfforts 是这个模型支持的思考档位。
	ReasoningEfforts []string
	// Vision 表示这个模型支持图片输入（未配置 = 不支持）。
	Vision    bool
	ExtraBody map[string]any
}

// ApplyOverrides 应用配置覆盖（同 ID 覆盖，新 ID 新增；模拟提供商永远保留在末尾）。
func (r *Registry) ApplyOverrides(overrides []Override) {
	for _, o := range overrides {
		if o.ID == "" || o.ID == "simulated" {
			continue // 模拟提供商不可被配置覆盖
		}
		found := false
		for i := range r.providers {
			cfg := &r.providers[i].Config
			if cfg.ID != o.ID {
				continue
			}
			found = true
			if o.Label != "" {
				cfg.Label = o.Label
			}
			if o.Kind != "" {
				cfg.Kind = o.Kind
			}
			if o.BaseURL != "" {
				cfg.BaseURL = o.BaseURL
			}
			if o.APIKeyEnv != nil {
				cfg.APIKeyEnv = *o.APIKeyEnv
				r.providers[i].APIKey = ""
				if cfg.APIKeyEnv != "" {
					r.providers[i].APIKey = os.Getenv(cfg.APIKeyEnv)
				}
			}
			if len(o.Models) > 0 {
				cfg.Models = mergeModelExtras(o.Models, o.ExtraBody)
			}
			if len(o.ExtraBody) > 0 && len(o.Models) == 0 {
				cfg.ExtraBody = o.ExtraBody
			}
			break
		}
		if found {
			continue
		}
		cfg := ProviderConfig{
			ID:        o.ID,
			Label:     pick(o.Label, o.ID),
			Kind:      pickKind(o.Kind),
			BaseURL:   o.BaseURL,
			Models:    mergeModelExtras(o.Models, o.ExtraBody),
			ExtraBody: o.ExtraBody,
		}
		if o.APIKeyEnv != nil {
			cfg.APIKeyEnv = *o.APIKeyEnv
		}
		key := ""
		if cfg.APIKeyEnv != "" {
			key = os.Getenv(cfg.APIKeyEnv)
		}
		// 插在模拟提供商之前，保持它在末尾
		last := len(r.providers) - 1
		if last >= 0 && r.providers[last].Config.Kind == KindSimulated {
			r.providers = append(r.providers[:last], append([]Resolved{{Config: cfg, APIKey: key}}, r.providers[last:]...)...)
		} else {
			r.providers = append(r.providers, Resolved{Config: cfg, APIKey: key})
		}
	}
}

// mergeModelExtras 把厂商级参数合并进每个模型（模型同名参数优先）。
func mergeModelExtras(specs []ModelSpec, providerExtra map[string]any) []ModelConfig {
	out := make([]ModelConfig, 0, len(specs))
	for _, spec := range specs {
		extra := map[string]any{}
		for key, value := range providerExtra {
			extra[key] = value
		}
		for key, value := range spec.ExtraBody {
			extra[key] = value
		}
		if len(extra) == 0 {
			extra = nil
		}
		out = append(out, ModelConfig{
			ID:               spec.ID,
			Label:            spec.Label,
			ContextWindow:    spec.ContextWindow,
			MaxOutput:        spec.MaxOutput,
			ReasoningEfforts: spec.ReasoningEfforts,
			Vision:           spec.Vision,
			ExtraBody:        extra,
		})
	}
	return out
}

func pick(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func pickKind(kind Kind) Kind {
	if kind != "" {
		return kind
	}
	return KindOpenAI
}

// All 返回全部提供商。
func (r *Registry) All() []Resolved { return r.providers }

// Configured 返回凭据齐备的提供商（UI 只在这些之间切换）。
//
// 模拟提供商不算「可用提供商」——它是没有可用凭据时的回退路径。
func (r *Registry) Configured() []Resolved {
	out := make([]Resolved, 0, len(r.providers))
	for _, p := range r.providers {
		if p.Config.Kind == KindSimulated {
			continue
		}
		if p.Configured() {
			out = append(out, p)
		}
	}
	return out
}

// Get 按 ID 查找。
func (r *Registry) Get(id string) (Resolved, bool) {
	for _, p := range r.providers {
		if p.Config.ID == id {
			return p, true
		}
	}
	return Resolved{}, false
}

// Initial 返回初始选择：优先配置文件的 Active，否则要求环境变量凭据的提供商中
// 第一个可用的。
//
// 不自动选本地服务（ollama）：它没有 APIKeyEnv，无法在这里判断服务是否在跑，
// 自动选中会让用户没配任何东西时看到一串连接失败，而不是「请配置凭据」的提示。
// 想用它就在配置文件（simple-agent.yaml）里显式指定 active_provider: ollama。
//
// 注意：**与 Rust 版不同，已经用户确认**。Rust 版的 ollama 网络 APIKeyEnv 为空，
// 所以 is_configured() 永远为真，没有 Key 时会自动选中 ollama；新行为改为走
// 模拟提示引导用户配置。不要改回去（TestRegistryInitialRequiresCredentials 守着）。
func (r *Registry) Initial() (Selection, bool) {
	if r.ActiveProvider != "" {
		if p, ok := r.Get(r.ActiveProvider); ok && p.Configured() {
			model := r.ActiveModel
			if model == "" || !contains(p.Config.ModelIDs(), model) {
				model = firstModel(p.Config)
			}
			if model != "" {
				return Selection{ProviderID: p.Config.ID, Model: model}, true
			}
		}
	}
	for _, p := range r.Configured() {
		if p.Config.APIKeyEnv == "" {
			continue // 本地服务需要显式配置才启用
		}
		model := firstModel(p.Config)
		if model == "" {
			continue
		}
		return Selection{ProviderID: p.Config.ID, Model: model}, true
	}
	return Selection{}, false
}

func firstModel(cfg ProviderConfig) string {
	if len(cfg.Models) == 0 {
		return ""
	}
	return cfg.Models[0].ID
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// Build 按配置构造 Provider 实现（按 Kind 分派，不按 ID 字符串判断）。
func (r *Registry) Build(sel Selection) (Provider, error) {
	p, ok := r.Get(sel.ProviderID)
	if !ok {
		return nil, ErrUnknownProvider
	}
	if !p.Configured() {
		return nil, &Error{Kind: "auth", Message: "缺少 API Key（环境变量 " + p.Config.APIKeyEnv + "）"}
	}
	switch p.Config.Kind {
	case KindOpenAI:
		return NewOpenAIProvider(p.Config, p.APIKey), nil
	case KindAnthropic:
		return NewAnthropicProvider(p.Config, p.APIKey), nil
	case KindSimulated:
		return NewSimulatedProvider(), nil
	default:
		return nil, ErrUnknownProvider
	}
}

// SimulatedSelection 返回模拟提供商的 Selection（无凭据时的回退）。
func SimulatedSelection() Selection {
	return Selection{ProviderID: "simulated", Model: "none"}
}

// SimulatedConfig 是模拟提供商的配置。
func SimulatedConfig() ProviderConfig {
	return ProviderConfig{
		ID: "simulated", Label: "本地提示", Kind: KindSimulated,
		Models: []ModelConfig{{ID: "none"}},
	}
}
