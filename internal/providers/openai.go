package providers

import (
	"context"
	json "encoding/json/v2"
	"strings"
)

// OpenAIProvider 实现 OpenAI Chat Completions 协议（含所有兼容实现）。
type OpenAIProvider struct {
	config ProviderConfig
	apiKey string
}

// NewOpenAIProvider 构造适配器。
func NewOpenAIProvider(config ProviderConfig, apiKey string) *OpenAIProvider {
	return &OpenAIProvider{config: config, apiKey: apiKey}
}

// Config 返回配置。
func (p *OpenAIProvider) Config() ProviderConfig { return p.config }

func (p *OpenAIProvider) endpoint() string {
	return strings.TrimRight(p.config.BaseURL, "/") + "/chat/completions"
}

// Stream 发起流式对话。
func (p *OpenAIProvider) Stream(ctx context.Context, req ChatRequest) (<-chan Event, error) {
	body, err := buildOpenAIBody(req, p.config.IncludeUsage, p.config.ExtraBody, p.config.ModelExtra(req.Model))
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	if p.apiKey != "" {
		headers["Authorization"] = "Bearer " + p.apiKey
	}
	return postStream(ctx, streamOptions{
		URL:         p.endpoint(),
		Headers:     headers,
		Body:        body,
		Parser:      newOpenAIParser(p.config.IncludeUsage),
		IdleTimeout: p.config.IdleTimeout,
	})
}

// buildOpenAIBody 构造请求体。
func buildOpenAIBody(req ChatRequest, includeUsage bool, extraBody, modelExtra map[string]any) ([]byte, error) {
	messages, err := ToOpenAIMessages(req.Messages, req.Blobs)
	if err != nil {
		return nil, err
	}
	// 系统提示插到最前面
	if strings.TrimSpace(req.System) != "" {
		messages = append([]map[string]any{{"role": "system", "content": req.System}}, messages...)
	}
	if err := requireNonEmptyMessages(messages); err != nil {
		return nil, err
	}

	body := map[string]any{
		"model":    req.Model,
		"messages": messages,
		"stream":   true,
	}
	if includeUsage {
		// 部分服务端在 stream 模式下默认不返回 usage，需显式要求
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.Temperature != nil {
		// 保留三位小数，避免浮点尾巴
		body["temperature"] = float64(int(*req.Temperature*1000+0.5)) / 1000
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tool.Name,
					"description": tool.Description,
					"parameters":  tool.Parameters,
				},
			})
		}
		body["tools"] = tools
	}
	// 厂商特有参数（reasoning_effort / thinking 等）最后合并，允许覆盖上面的默认值；
	// 模型级参数再覆盖一层
	for key, value := range extraBody {
		body[key] = value
	}
	for key, value := range modelExtra {
		body[key] = value
	}
	applyReasoningEffort(body, req.ReasoningEffort)
	return json.Marshal(body)
}

// applyReasoningEffort 用请求里的档位覆盖模型配置：
// off 表示显式关闭（删掉该键），其它值原样写入，空串表示沿用配置。
func applyReasoningEffort(body map[string]any, effort string) {
	switch effort {
	case "":
		return
	case "off":
		delete(body, "reasoning_effort")
		delete(body, "thinking")
	default:
		body["reasoning_effort"] = effort
	}
}

// openAIParser 是有状态的流解析器。
//
// 两处必须有状态：
//   - finish_reason 只记录停止原因，Done 在 [DONE]/EOF/收到 usage 之后才发
//     （OpenAI 官方在 finish_reason 之后单独发一块 usage）；
//   - 同名事件不会重复 Done。
type openAIParser struct {
	includeUsage bool
	stopReason   string
	usageSeen    bool
	doneSent     bool
	// 部分网关（含一些代理）在 tool_calls 里不带 index，或每条都从 0 开始。
	// 那样多个工具的参数会拼进同一个缓冲，最后变成非法 JSON、整个回合失败。
	// 这里按 tool call 的 id 记住分配过的下标，id 变了就换一个空位。
	toolIndex map[string]int
	nextTool  int
}

func newOpenAIParser(includeUsage bool) *openAIParser {
	return &openAIParser{includeUsage: includeUsage, toolIndex: map[string]int{}}
}

// toolSlot 给一个 tool call 分配稳定的下标（见 openAIParser 的说明）。
func (p *openAIParser) toolSlot(id string, reported int) int {
	if id == "" {
		return reported
	}
	if slot, ok := p.toolIndex[id]; ok {
		return slot
	}
	slot := reported
	used := map[int]bool{}
	for existing, taken := range p.toolIndex {
		if existing != id {
			used[taken] = true
		}
	}
	if used[slot] {
		slot = p.nextTool
		for used[slot] {
			slot++
		}
	}
	p.toolIndex[id] = slot
	if slot >= p.nextTool {
		p.nextTool = slot + 1
	}
	return slot
}

// Parse 实现 Parser。
func (p *openAIParser) Parse(sse SSEEvent) []Event {
	data := strings.TrimSpace(sse.Data)
	if data == "" {
		return nil
	}
	if data == "[DONE]" {
		return p.emitDone()
	}

	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return []Event{errorEvent("chunk 解析失败：%v（%s）", err, data)}
	}

	if chunk.Error != nil {
		return []Event{errorEvent("%s", chunk.Error.Message)}
	}

	var events []Event

	if chunk.Usage != nil {
		p.usageSeen = true
		events = append(events, Event{
			Kind:         EventUsage,
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
		})
	}

	if len(chunk.Choices) > 0 {
		choice := chunk.Choices[0]
		if text := choice.Delta.ReasoningContent; text != "" {
			events = append(events, Event{Kind: EventReasoning, Text: text})
		} else if text := choice.Delta.Reasoning; text != "" {
			events = append(events, Event{Kind: EventReasoning, Text: text})
		}
		if text := choice.Delta.Content; text != "" {
			events = append(events, Event{Kind: EventText, Text: text})
		}
		for _, call := range choice.Delta.ToolCalls {
			slot := p.toolSlot(call.ID, call.Index)
			if call.ID != "" || call.Function.Name != "" {
				events = append(events, Event{
					Kind:  EventToolStart,
					Index: slot,
					ID:    call.ID,
					Name:  call.Function.Name,
				})
			}
			if call.Function.Arguments != "" {
				events = append(events, Event{
					Kind:  EventToolArgs,
					Index: slot,
					Delta: call.Function.Arguments,
				})
			}
		}
		if choice.FinishReason != nil {
			// 只记录，不发 Done：后面可能还有 usage 块
			p.stopReason = *choice.FinishReason
		}
	}

	// 尽早结束：不必干等 [DONE]（否则网关不发 [DONE] 时要等到读超时）
	ready := (p.includeUsage && p.stopReason != "" && p.usageSeen) ||
		(!p.includeUsage && p.stopReason != "")
	if ready {
		events = append(events, p.emitDone()...)
	}
	return events
}

// Finish 实现 Parser：EOF 时的兜底。
//
// 关键场景：网关忽略了 stream_options（很多兼容实现如此），从不发 usage，
// 也不发 [DONE] 就断开。此时 stopReason 已记录 —— 必须补 Done，否则一次正常
// 回答会因为「通道关闭但无终止事件」被判成失败。
func (p *openAIParser) Finish() []Event {
	if p.doneSent {
		return nil
	}
	if p.stopReason == "" {
		return []Event{errorEvent("流在 finish_reason 之前结束（回答可能被截断）")}
	}
	return p.emitDone()
}

func (p *openAIParser) emitDone() []Event {
	if p.doneSent {
		return nil
	}
	p.doneSent = true
	return []Event{doneEvent(p.stopReason)}
}

// 编译期断言：适配器实现 Provider。
var _ Provider = (*OpenAIProvider)(nil)
