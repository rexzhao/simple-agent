package providers

import (
	"context"
	json "encoding/json/v2"
	"strings"
)

// AnthropicProvider 实现 Anthropic Messages 协议。
type AnthropicProvider struct {
	config ProviderConfig
	apiKey string
}

// NewAnthropicProvider 构造适配器。
func NewAnthropicProvider(config ProviderConfig, apiKey string) *AnthropicProvider {
	return &AnthropicProvider{config: config, apiKey: apiKey}
}

// Config 返回配置。
func (p *AnthropicProvider) Config() ProviderConfig { return p.config }

func (p *AnthropicProvider) endpoint() string {
	return strings.TrimRight(p.config.BaseURL, "/") + "/messages"
}

// anthropicVersion 是 API 版本头。
const anthropicVersion = "2023-06-01"

// defaultMaxTokens 是未指定时的 max_tokens（Anthropic 要求必填）。
const defaultMaxTokens = 4096

// Stream 发起流式对话。
func (p *AnthropicProvider) Stream(ctx context.Context, req ChatRequest) (<-chan Event, error) {
	body, err := buildAnthropicBody(req, p.config.ExtraBody, p.config.ModelExtra(req.Model), p.config.CacheControl)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"anthropic-version": anthropicVersion}
	if p.apiKey != "" {
		headers["x-api-key"] = p.apiKey
	}
	return postStream(ctx, streamOptions{
		URL:         p.endpoint(),
		Headers:     headers,
		Body:        body,
		Parser:      &anthropicParser{},
		IdleTimeout: p.config.IdleTimeout,
	})
}

// buildAnthropicBody 构造请求体。
func buildAnthropicBody(req ChatRequest, extraBody, modelExtra map[string]any,
	cache CacheControlConfig) ([]byte, error) {
	messages, err := ToAnthropicMessages(req.Messages, req.Blobs)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmptyMessages(messages); err != nil {
		return nil, err
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	body := map[string]any{
		"model":      req.Model,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     true,
	}
	cacheControl := cache.control()
	if strings.TrimSpace(req.System) != "" {
		// Anthropic 的 system 是顶层字段，不是消息。
		//
		// **显式打缓存断点**（cache_control）：Anthropic 的 prompt caching
		// 默认不开，必须在某个 block 上标 `cache_control` 才会去建缓存。
		// 不标的话服务端返回的 cache_read/cache_creation 恒为 0
		// （实测这个网关就是这样：跑 8 轮全是 0，界面因此永远不显示命中率）。
		//
		// 打在 system 上：它最长、最稳定（每轮都一样），命中率最高。
		block := map[string]any{"type": "text", "text": req.System}
		if cacheControl != nil {
			block["cache_control"] = cacheControl
		}
		body["system"] = []any{block}
	}
	if req.Temperature != nil {
		body["temperature"] = float64(int(*req.Temperature*1000+0.5)) / 1000
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for index, tool := range req.Tools {
			entry := map[string]any{
				"name":         tool.Name,
				"description":  tool.Description,
				"input_schema": tool.Parameters,
			}
			// **只在最后一个 tool 上打点**：缓存按前缀生效，标记最后一个
			// 就等于"把整段 tools 定义都缓存起来"。
			//
			// 单独缓存 tools 的价值：工具定义（schema）很长，而对话历史
			// 每轮都在变。分开打点后，历史变化不会让 tools 那段失效。
			// 这是 pi 的做法（anthropic-messages.ts 的 index === length-1）。
			if cacheControl != nil && !cache.NoTools && index == len(req.Tools)-1 {
				entry["cache_control"] = cacheControl
			}
			tools = append(tools, entry)
		}
		body["tools"] = tools
	}
	// 最后一条消息打点：让**多轮对话**每轮都命中上一轮的前缀 ——
	// 这是省钱的大头（回合越多，累积的上下文越大，缓存省得越多）。
	//
	// 打在最后一条 user/assistant 消息的**最后一个 content block** 上。
	// 只标最后一条：Anthropic 按"标记之前的内容"做缓存前缀，
	// 标最后一条就等于"把整个对话历史缓存起来"。
	if cacheControl != nil && !cache.NoMessages {
		markLastMessageCacheControl(messages, cacheControl)
	}
	// 厂商特有参数（如 thinking）最后合并；模型级覆盖厂商级
	for key, value := range extraBody {
		body[key] = value
	}
	for key, value := range modelExtra {
		body[key] = value
	}
	// 思考档位：Anthropic 用 `output_config.effort`（不是顶层的
	// reasoning_effort —— 那个字段它不认，会 400）。
	//
	// 只在这一层写，而且**允许被 extra_body 覆盖**：不同网关支持的档位
	// 集合不一样（有的只有 low/high/max），配置里用 extra_body 能压掉。
	if effort := strings.TrimSpace(req.ReasoningEffort); effort != "" {
		config, _ := body["output_config"].(map[string]any)
		if config == nil {
			config = map[string]any{}
		}
		// 已经由 extra_body/modelExtra 显式给了 effort 就不覆盖
		if _, set := config["effort"]; !set {
			config["effort"] = effort
			body["output_config"] = config
		}
	}
	// 注意：**不要**写 reasoning_effort。Anthropic Messages API 没有这个字段，
	// 未知的顶层字段会被拒（400）。要开思考请通过 extra_body 传
	// thinking: {type: enabled, budget_tokens: N}——厂商特有的参数走那条路。
	//
	// **必须用确定性序列化**：messages/system 是 map，json/v2 默认按随机
	// 迭代顺序输出键，每次请求前缀都不一样，prompt caching 永远不命中。
	return marshalDeterministic(body)
}

// control 生成请求里用的 cache_control 标记；Disabled 时返回 nil（不发）。
func (c CacheControlConfig) control() map[string]any {
	if c.Disabled {
		return nil
	}
	marker := map[string]any{"type": "ephemeral"}
	// ttl 只在要 1h 时写：默认 5 分钟不打这个字段，请求体更干净
	// （网关对未知字段宽容度不一，能不写就不写）。
	if c.TTL == "1h" {
		marker["ttl"] = "1h"
	}
	return marker
}

// markLastMessageCacheControl 在最后一条 user/assistant 消息的末尾打缓存断点。
//
// 只在最后一条上标：Anthropic 的缓存是**前缀式**的 —— 标记点之前的内容
// 会被缓存。标在最后一条的最后一个 block 上，等于"把整个对话历史缓存起来"，
// 下一轮追加新消息时，之前的部分就全部命中。
//
// 跳过的位置：
//   - 最后一条是 assistant（预填充场景）：不标，等下一轮 user 消息再说
//   - 消息没有 content block（空消息）
func markLastMessageCacheControl(messages []map[string]any, marker map[string]any) {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		role, _ := message["role"].(string)
		if role != "user" && role != "assistant" {
			continue
		}
		blocks, ok := message["content"].([]any)
		if !ok || len(blocks) == 0 {
			return
		}
		// 标在最后一个 block 上（文本/图片/工具结果都行）
		last, ok := blocks[len(blocks)-1].(map[string]any)
		if !ok {
			return
		}
		last["cache_control"] = marker
		return
	}
}

// anthropicParser 是有状态的解析器：只有 tool_use 块的 stop 才算工具调用结束。
//
// Anthropic 对**每个** content block（包括文本块）都发 content_block_stop，
// 因此必须记录哪些 index 属于 tool_use，否则会产生幽灵工具调用。
type anthropicParser struct {
	toolIndices map[int]struct{}
	doneSent    bool
}

// Parse 实现 Parser。
func (p *anthropicParser) Parse(sse SSEEvent) []Event {
	data := strings.TrimSpace(sse.Data)
	if data == "" {
		return nil
	}

	var event struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Message struct {
			Usage struct {
				InputTokens int `json:"input_tokens"`
				// 缓存读/写：Anthropic 把命中缓存和写入缓存分开计价，
				// 两者都不算在 input_tokens 里，需要单独取。
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage *struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return []Event{errorEvent("chunk 解析失败：%v（%s）", err, data)}
	}

	switch event.Type {
	case "message_start":
		// input_tokens 可能是 0 而缓存字段非 0（整段 prompt 都命中缓存时），
		// 所以判据是三者任一大于 0，不能只看 InputTokens。
		usage := event.Message.Usage
		if usage.InputTokens > 0 || usage.CacheReadInputTokens > 0 || usage.CacheCreationInputTokens > 0 {
			return []Event{{
				Kind:             EventUsage,
				InputTokens:      usage.InputTokens,
				CacheReadTokens:  usage.CacheReadInputTokens,
				CacheWriteTokens: usage.CacheCreationInputTokens,
				// message_start 带的就是完整输入用量（含缓存字段）。
				// message_delta 只带 output，那条事件不设这个标记 ——
				// 否则会把这里已经拿到的缓存数抹成 0。
				CacheReported: true,
			}}
		}
		return nil

	case "content_block_start":
		if event.ContentBlock.Type == "tool_use" {
			if p.toolIndices == nil {
				p.toolIndices = map[int]struct{}{}
			}
			p.toolIndices[event.Index] = struct{}{}
			return []Event{{
				Kind:  EventToolStart,
				Index: event.Index,
				ID:    event.ContentBlock.ID,
				Name:  event.ContentBlock.Name,
			}}
		}
		return nil

	case "content_block_delta":
		switch event.Delta.Type {
		case "text_delta":
			if event.Delta.Text != "" {
				return []Event{{Kind: EventText, Text: event.Delta.Text}}
			}
		case "thinking_delta":
			if event.Delta.Thinking != "" {
				return []Event{{Kind: EventReasoning, Text: event.Delta.Thinking}}
			}
		case "input_json_delta":
			if event.Delta.PartialJSON != "" {
				return []Event{{Kind: EventToolArgs, Index: event.Index, Delta: event.Delta.PartialJSON}}
			}
		}
		return nil

	case "content_block_stop":
		// 只有 tool_use 块才报结束（文本块也会发 stop）
		if p.toolIndices != nil {
			if _, ok := p.toolIndices[event.Index]; ok {
				delete(p.toolIndices, event.Index)
				return []Event{{Kind: EventToolEnd, Index: event.Index}}
			}
		}
		return nil

	case "message_delta":
		var events []Event
		if event.Usage != nil && event.Usage.OutputTokens > 0 {
			events = append(events, Event{Kind: EventUsage, OutputTokens: event.Usage.OutputTokens})
		}
		if event.Delta.StopReason != "" {
			events = append(events, doneEvent(event.Delta.StopReason))
			p.doneSent = true
		}
		return events

	case "message_stop":
		if p.doneSent {
			return nil
		}
		p.doneSent = true
		return []Event{doneEvent("")}

	case "error":
		if event.Error != nil {
			return []Event{errorEvent("%s", event.Error.Message)}
		}
		return []Event{errorEvent("未知的流错误")}

	case "ping":
		return nil
	}
	return nil
}

// Finish 实现 Parser：EOF 兜底。
//
// 正常流一定会有 message_delta（带 stop_reason）或 message_stop；两者都没有说明
// 连接中途断了，必须报错而不是当成正常结束。
func (p *anthropicParser) Finish() []Event {
	if p.doneSent {
		return nil
	}
	return []Event{errorEvent("流在 message_delta/message_stop 之前结束（回答可能被截断）")}
}

var _ Provider = (*AnthropicProvider)(nil)
