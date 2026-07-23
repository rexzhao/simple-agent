package openaichat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

func BuildRequestBody(request model.Request, stream bool) ([]byte, error) {
	body := make(map[string]any, len(request.Parameters)+4)
	for key, value := range request.Parameters {
		body[key] = value
	}

	messages, err := buildMessages(request.Messages)
	if err != nil {
		return nil, err
	}

	body["model"] = request.Model
	body["messages"] = messages
	body["stream"] = stream
	if len(request.Tools) > 0 {
		body["tools"] = buildTools(request.Tools)
	}

	return json.Marshal(body)
}

func buildMessages(messages []model.Message) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		content, err := openAIChatMessageContent(message)
		if err != nil {
			return nil, err
		}
		item := map[string]any{
			"role":    string(message.Role),
			"content": content,
		}
		if len(message.ToolCalls) > 0 {
			item["tool_calls"] = buildMessageToolCalls(message.ToolCalls)
		}
		if message.Role == model.MessageRoleTool {
			item["tool_call_id"] = message.ToolCallID
		}
		out = append(out, item)
	}
	return out, nil
}

func openAIChatMessageContent(message model.Message) (any, error) {
	if len(message.ContentBlocks) == 0 {
		return message.Content, nil
	}
	if message.Role != model.MessageRoleUser {
		return nil, fmt.Errorf("OpenAI Chat content blocks are only supported for user messages")
	}
	if message.Content != "" {
		return nil, fmt.Errorf("OpenAI Chat message cannot set both content and content blocks")
	}

	content := make([]map[string]any, 0, len(message.ContentBlocks))
	for _, block := range message.ContentBlocks {
		switch strings.TrimSpace(block.Type) {
		case "", "input_text":
			content = append(content, map[string]any{"type": "text", "text": block.Text})
		case "input_image":
			if block.ImageBlob != nil {
				return nil, fmt.Errorf("OpenAI Chat image attachment must be materialized before requesting the model")
			}
			if strings.TrimSpace(block.ImageURL) == "" {
				return nil, fmt.Errorf("OpenAI Chat input_image requires image_url")
			}
			detail := strings.TrimSpace(block.Detail)
			if detail == "" {
				detail = "auto"
			}
			content = append(content, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": block.ImageURL, "detail": detail},
			})
		default:
			return nil, fmt.Errorf("unsupported OpenAI Chat content block type %q", block.Type)
		}
	}
	return content, nil
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
