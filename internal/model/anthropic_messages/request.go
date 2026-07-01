package anthropicmessages

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

const toolUseNotImplementedMessage = "Anthropic Messages tool use adapter is not implemented yet"

func BuildRequestBody(request model.Request, stream bool) ([]byte, error) {
	if len(request.Tools) > 0 {
		return nil, fmt.Errorf("%s: request tools are not supported", toolUseNotImplementedMessage)
	}

	body := make(map[string]any, len(request.Parameters)+4)
	for key, value := range request.Parameters {
		body[key] = value
	}

	messages, system, err := buildMessages(request.Messages)
	if err != nil {
		return nil, err
	}

	body["model"] = request.Model
	body["messages"] = messages
	body["stream"] = stream
	if system != "" {
		body["system"] = system
	}

	return json.Marshal(body)
}

func buildMessages(messages []model.Message) ([]map[string]any, string, error) {
	out := make([]map[string]any, 0, len(messages))
	systemParts := []string{}

	for _, message := range messages {
		switch message.Role {
		case model.MessageRoleSystem, model.MessageRoleDeveloper:
			content := strings.TrimSpace(message.Content)
			if content != "" {
				systemParts = append(systemParts, content)
			}
		case model.MessageRoleUser, model.MessageRoleAssistant:
			if message.Role == model.MessageRoleAssistant && len(message.ToolCalls) > 0 {
				return nil, "", fmt.Errorf("%s: assistant tool calls are not supported", toolUseNotImplementedMessage)
			}
			out = append(out, map[string]any{
				"role":    string(message.Role),
				"content": message.Content,
			})
		case model.MessageRoleTool:
			return nil, "", fmt.Errorf("%s: tool result messages are not supported", toolUseNotImplementedMessage)
		default:
			return nil, "", fmt.Errorf("unsupported Anthropic Messages role %q", message.Role)
		}
	}

	return out, strings.Join(systemParts, "\n\n"), nil
}
