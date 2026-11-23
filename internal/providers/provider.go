// Package providers 是模型提供商抽象层。
//
// 设计目标：新增一家提供商 = 一个 Kind 变体 + 一个适配文件，不触碰 UI 与会话层。
//
// 分层：
//   - ChatRequest / Event：与厂商无关的中立协议；
//   - Provider：把中立请求变成事件流（context 取消即停止）；
//   - 各厂商适配（openai.go / anthropic.go）：请求体构建 + 流式 chunk 解析，
//     纯函数实现，可用固定样本单测；
//   - transport.go：net/http + SSE 读取。
package providers

import (
	"context"
	"errors"
	"time"

	"fmt"
	"simple-agent/internal/message"
)

// Kind 是提供商协议族。
type Kind string

const (
	// KindOpenAI 是 OpenAI Chat Completions 协议（OpenAI、DeepSeek、Moonshot、
	// 智谱、通义、Ollama、vLLM、LM Studio 等兼容实现）。
	KindOpenAI Kind = "openai-compatible"
	// KindAnthropic 是 Anthropic Messages 协议。
	KindAnthropic Kind = "anthropic"
	// KindSimulated 是本地模拟（无可用凭据时给 UI 一条提示流）。
	KindSimulated Kind = "simulated"
)

// ProviderConfig 是一家提供商的配置。
type ProviderConfig struct {
	ID        string
	Label     string
	Kind      Kind
	BaseURL   string
	APIKeyEnv string
	Models    []ModelConfig
	// IncludeUsage 对应 OpenAI 的 stream_options.include_usage（默认开）。
	IncludeUsage bool
	// ExtraBody 会原样合并进请求体（顶层键），用于厂商特有参数：
	// 例如 paperhub 的 reasoning_effort / thinking，开启模型的思考输出。
	ExtraBody map[string]any

	// IdleTimeout 是流空闲超时（超过这个时间没有任何字节就断开）。0 用默认 180s。
	// 做成配置项是为了能用短超时做测试，也让高级用户能按网络情况调整。
	IdleTimeout time.Duration

	// CacheControl 控制 Anthropic 的 prompt caching 断点（只对 KindAnthropic 生效）。
	//
	// 默认（零值）= 全部打开：system + 最后一个 tool + 最后一条消息。
	// Anthropic 默认**不缓存**，必须在请求里显式标 `cache_control`，
	// 否则 cache_read/cache_creation 恒为 0（界面上的命中率永远不出现）。
	CacheControl CacheControlConfig
}

// CacheControlConfig 是缓存断点的开关。
//
// 为什么要可配：`cache_control` 是 Anthropic 独有的字段，某些兼容网关
// 在 tools 上不认它（pi 专门为此留了 supportsCacheControlOnTools 开关）。
// 这家网关实测三处都支持，但留着开关方便遇到 400 时逐个关掉排查。
type CacheControlConfig struct {
	// Disabled 全关（不发任何 cache_control）。
	Disabled bool
	// NoTools 不在 tools 上打点（网关不支持 tools.cache_control 时用）。
	NoTools bool
	// NoMessages 不在消息上打点。
	NoMessages bool
	// TTL 是缓存存活时间："5m"（默认，等价于不写）或 "1h"。
	//
	// 1h 的写入费更贵（2x 而非 1.25x），但命中仍按 10% 计价；
	// 适合长时间连续对话。默认 5m 对"停一会儿再聊"更划算。
	TTL string
}

// ModelConfig 是单个模型的配置。
type ModelConfig struct {
	ID    string
	Label string
	// ContextWindow 是该模型的上下文窗口（token）。0 表示未知（默认按 32k 处理）。
	ContextWindow int
	// MaxOutput 是单次回复的最大输出 token 数（0 = 交给服务端默认值）。
	MaxOutput int
	// ReasoningEfforts 是这个模型支持的思考档位（如 off/low/medium/high/max）。
	// 空表示该模型没有可选的思考档位。
	ReasoningEfforts []string
	// Vision 表示这个模型支持图片输入（未配置 = 不支持）。
	Vision bool
	// ExtraBody 是这个模型特有的请求体参数（如 reasoning_effort / thinking）。
	ExtraBody map[string]any
}

// Model 按 ID 查找模型配置。
func (p ProviderConfig) Model(id string) (ModelConfig, bool) {
	for _, model := range p.Models {
		if model.ID == id {
			return model, true
		}
	}
	return ModelConfig{}, false
}

// Efforts 返回某个模型支持的思考档位。
func (p ProviderConfig) Efforts(id string) []string {
	if model, ok := p.Model(id); ok {
		return model.ReasoningEfforts
	}
	return nil
}

// SupportsVision 报告某个模型是否支持图片输入（未配置 = 不支持）。
func (p ProviderConfig) SupportsVision(id string) bool {
	if model, ok := p.Model(id); ok {
		return model.Vision
	}
	return false
}

// ModelLabel 返回模型的显示名（没有配置 label 时退回 id）。
func (p ProviderConfig) ModelLabel(id string) string {
	if model, ok := p.Model(id); ok && model.Label != "" {
		return model.Label
	}
	return id
}

// MaxOutput 返回某个模型的最大输出 token 数（0 = 不限制）。
func (p ProviderConfig) MaxOutputOf(id string) int {
	if model, ok := p.Model(id); ok {
		return model.MaxOutput
	}
	return 0
}

// ModelExtra 返回某个模型的请求体参数（没有则 nil）。
func (p ProviderConfig) ModelExtra(id string) map[string]any {
	if model, ok := p.Model(id); ok {
		return model.ExtraBody
	}
	return nil
}

// ModelIDs 返回模型 ID 列表。
func (p ProviderConfig) ModelIDs() []string {
	ids := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		ids = append(ids, m.ID)
	}
	return ids
}

// ContextWindow 返回指定模型的窗口（未知返回 0）。
func (p ProviderConfig) ContextWindow(model string) int {
	for _, m := range p.Models {
		if m.ID == model {
			return m.ContextWindow
		}
	}
	return 0
}

// Selection 是本轮请求使用的提供商与模型。
//
// 带 json tag：它会随命令载荷与状态快照跨进程（展示端 ↔ 执行端），
// 非 Go 的客户端按 snake_case 解析，没有 tag 就会读到空值。
type Selection struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
}

// ToolSpec 是传给模型的工具声明。
type ToolSpec struct {
	Name        string
	Description string
	Parameters  any
}

// ChatRequest 是与厂商无关的对话请求。
type ChatRequest struct {
	Model       string
	System      string
	Messages    []message.Message
	Tools       []ToolSpec
	Temperature *float64
	MaxTokens   int
	// ReasoningEffort 覆盖思考档位：
	//   ""    沿用模型配置（providers 里的 reasoning_effort）
	//   "off" 显式关闭（请求体里不带 reasoning_effort）
	//   其它  原样写进 reasoning_effort
	ReasoningEffort string
	// Blobs 把 `blob:sha256:<hex>` 引用读成字节。
	//
	// 请求是在**异步开流的 goroutine** 里组装的，所以图片在这里读（不在主线程）；
	// 读不到时降级成「[图片丢失]」文字，不让整个会话发不出去。
	Blobs BlobReader
}

// EventKind 是流式事件类型。
type EventKind string

const (
	// EventText 是正文增量。
	EventText EventKind = "text"
	// EventReasoning 是推理增量（DeepSeek-R1 / Claude thinking）。
	EventReasoning EventKind = "reasoning"
	// EventToolStart 是工具调用开始。
	//
	// 同一 Index 的 ToolStart 可能出现多次（有些网关每个分片都重复带 id/name），
	// 消费方必须以 Index 为主键**去重合并**，不能每次 start 都新开一个调用。
	EventToolStart EventKind = "toolStart"
	// EventToolArgs 是工具参数增量——**不完整 JSON 片段，必须累积**。
	EventToolArgs EventKind = "toolArgs"
	// EventToolEnd 表示某个工具调用的参数已完整。
	//
	// **可能不出现**：OpenAI 协议没有「单个调用结束」的信号，OpenAI 适配器
	// 从不发这个事件。因此消费方绝不能只靠 ToolEnd 判断调用结束——
	// 收到 EventDone 时，必须把所有仍在缓冲的调用都视为已结束。
	// 只剩 ToolEnd 这一条路径的实现会在 OpenAI 上丢掉全部工具调用。
	EventToolEnd EventKind = "toolEnd"
	// EventUsage 是 token 用量。
	EventUsage EventKind = "usage"
	// EventDone 是本轮正常结束。
	EventDone EventKind = "done"
	// EventError 是流内错误。
	EventError EventKind = "error"
)

// Event 是流式事件（中立表示）。
type Event struct {
	Kind EventKind

	// 文本增量 / 推理增量
	Text string

	// 工具调用
	Index int
	ID    string
	Name  string
	Delta string

	// 用量
	InputTokens  int
	OutputTokens int
	// CacheReadTokens 是本次请求里**命中缓存**的输入 token 数。
	//
	// 两家语义一致（都是"没花钱重算的那部分"），但字段名不同：
	//   OpenAI     → usage.prompt_tokens_details.cached_tokens
	//   Anthropic  → usage.cache_read_input_tokens
	// 0 表示未知/没命中（旧网关一般不发这些字段）。
	CacheReadTokens int
	// CacheWriteTokens 是本次请求**写入缓存**的输入 token 数。
	//
	// 只有 Anthropic 有这个概念（usage.cache_creation_input_tokens）：它按
	// 额外费率收一次写入费。OpenAI 是自动缓存、不单列，恒为 0。
	CacheWriteTokens int
	// CacheReported 表示这条 usage 事件**带了**缓存字段。
	//
	// 为什么需要它：Anthropic 分两个事件报用量 —— message_start 带
	// input 与缓存字段，message_delta 只带 output。若下游按"每次都覆盖
	// 缓存字段"处理，第二个事件会把已经拿到的缓存数抹成 0
	// （实测就是这样：命中率显示成 0%）。
	//
	// 所以缓存字段要按「带了才覆盖」处理，而这个标记就是"带没带"。
	// OpenAI 的 usage 只有一块，带就 true。
	CacheReported bool

	// 结束/错误
	StopReason string
	Err        error
}

// Provider 把中立请求变成事件流。
type Provider interface {
	Config() ProviderConfig
	// Stream 发起流式请求。返回的 channel 在流结束或出错后关闭；
	// ctx 取消时底层 HTTP 请求终止、channel 关闭。
	//
	// 调用方契约：必须**要么读到 channel 关闭，要么 cancel ctx**。channel 有缓冲
	// 但不是无限的，长期不读且不取消会让发送方 goroutine 卡在 select 上。
	// 推荐做法：每次 Stream 都派生一个可取消的 ctx，停止生成、删除会话、
	// 回合结束时都 cancel（回合结束时 cancel 无副作用）。
	//
	// 终止事件契约：正常结束必发 EventDone（带 StopReason）；失败必发
	// EventError。ctx 取消时**两者都不发**，直接关通道。
	Stream(ctx context.Context, req ChatRequest) (<-chan Event, error)
}

// Error 是请求失败原因。
type Error struct {
	Kind    string
	Message string
	Status  int
}

func (e *Error) Error() string {
	switch {
	case e.Status != 0:
		return fmt.Sprintf("%s (HTTP %d): %s", e.Kind, e.Status, e.Message)
	default:
		return fmt.Sprintf("%s: %s", e.Kind, e.Message)
	}
}

var (
	// ErrMissingAPIKey 表示未配置凭据。
	ErrMissingAPIKey = errors.New("missing api key")
	// ErrUnknownProvider 表示未知提供商。
	ErrUnknownProvider = errors.New("unknown provider")
)
