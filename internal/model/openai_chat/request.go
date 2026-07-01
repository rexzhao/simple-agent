package openaichat

import (
	"encoding/json"

	"github.com/rexzhao/simple-agent/internal/model"
)

func BuildRequestBody(request model.Request, stream bool) ([]byte, error) {
	body := make(map[string]any, len(request.Parameters)+4)
	for key, value := range request.Parameters {
		body[key] = value
	}

	body["model"] = request.Model
	body["messages"] = buildMessages(request.Messages)
	body["stream"] = stream
	if len(request.Tools) > 0 {
		body["tools"] = buildTools(request.Tools)
	}

	return json.Marshal(body)
}

func buildMessages(messages []model.Message) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		item := map[string]any{
			"role":    string(message.Role),
			"content": message.Content,
		}
		if len(message.ToolCalls) > 0 {
			item["tool_calls"] = buildMessageToolCalls(message.ToolCalls)
		}
		if message.Role == model.MessageRoleTool {
			item["tool_call_id"] = message.ToolCallID
		}
		out = append(out, item)
	}
	return out
}

func buildMessageToolCalls(toolCalls []model.ToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(toolCalls))
	for _, toolCall := range toolCalls {
		out = append(out, map[string]any{
			"id":   toolCall.ID,
			"type": "function",
			"function": map[string]any{
				"name":      toolCall.Name,
				"arguments": toolCall.Arguments,
			},
		})
	}
	return out
}

func buildTools(tools []model.Tool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		parameters := tool.InputSchema
		if parameters == nil {
			parameters = map[string]any{}
		}

		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  parameters,
			},
		})
	}
	return out
}
