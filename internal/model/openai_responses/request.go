package openairesponses

import (
	"encoding/json"
	"fmt"

	"github.com/rexzhao/simple-agent/internal/model"
)

func BuildRequestBody(request model.Request, stream bool) ([]byte, error) {
	if len(request.Tools) > 0 {
		return nil, fmt.Errorf("OpenAI Responses adapter does not support tools yet")
	}

	input, err := buildInput(request.Messages)
	if err != nil {
		return nil, err
	}

	body, err := buildParameters(request.Parameters)
	if err != nil {
		return nil, err
	}

	body["model"] = request.Model
	body["input"] = input
	body["stream"] = stream

	return json.Marshal(body)
}

func buildParameters(parameters map[string]any) (map[string]any, error) {
	body := make(map[string]any, len(parameters)+3)
	for key, value := range parameters {
		if isUnsupportedToolParameter(key) {
			return nil, fmt.Errorf("OpenAI Responses adapter does not support tools yet: parameter %q is not supported", key)
		}
		if key == "max_tokens" {
			if _, ok := parameters["max_output_tokens"]; !ok {
				body["max_output_tokens"] = value
			}
			continue
		}
		body[key] = value
	}
	return body, nil
}

func isUnsupportedToolParameter(key string) bool {
	switch key {
	case "tools",
		"tool_choice",
		"parallel_tool_calls",
		"max_tool_calls",
		"tool_resources",
		"tool_outputs",
		"functions",
		"function_call",
		"function_call_output",
		"function_call_outputs",
		"web_search_options":
		return true
	default:
		return false
	}
}

func buildInput(messages []model.Message) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		if len(message.ToolCalls) > 0 {
			return nil, fmt.Errorf("OpenAI Responses adapter does not support assistant tool calls yet")
		}

		switch message.Role {
		case model.MessageRoleSystem, model.MessageRoleDeveloper, model.MessageRoleUser, model.MessageRoleAssistant:
			out = append(out, map[string]any{
				"role":    string(message.Role),
				"content": message.Content,
			})
		case model.MessageRoleTool:
			return nil, fmt.Errorf("OpenAI Responses adapter does not support tool messages yet")
		default:
			return nil, fmt.Errorf("unsupported OpenAI Responses role %q", message.Role)
		}
	}
	return out, nil
}
