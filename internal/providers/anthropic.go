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
	body, err := buildAnthropicBody(req, p.config.ExtraBody, p.config.ModelExtra(req.Model))
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
func buildAnthropicBody(req ChatRequest, extraBody, modelExtra map[string]any) ([]byte, error) {
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
	if strings.TrimSpace(req.System) != "" {
		// Anthropic 的 system 是顶层字段，不是消息
		body["system"] = req.System
	}
	if req.Temperature != nil {
		body["temperature"] = float64(int(*req.Temperature*1000+0.5)) / 1000
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			tools = append(tools, map[string]any{
				"name":         tool.Name,
				"description":  tool.Description,
				"input_schema": tool.Parameters,
			})
		}
		body["tools"] = tools
	}
	// 厂商特有参数（如 thinking）最后合并；模型级覆盖厂商级
	for key, value := range extraBody {
		body[key] = value
	}
	for key, value := range modelExtra {
		body[key] = value
	}
	// 注意：**不要**写 reasoning_effort。Anthropic Messages API 没有这个字段，
	// 未知的顶层字段会被拒（400）。要开思考请通过 extra_body 传
	// thinking: {type: enabled, budget_tokens: N}——厂商特有的参数走那条路。
	return json.Marshal(body)
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
		if event.Message.Usage.InputTokens > 0 {
			return []Event{{Kind: EventUsage, InputTokens: event.Message.Usage.InputTokens}}
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
