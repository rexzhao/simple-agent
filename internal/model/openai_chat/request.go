package openaichat

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

func BuildRequestBody(request model.Request, stream bool) ([]byte, error) {
	compatibility, _ := resolveCompatibility("")
	return buildRequestBody(request, stream, compatibility)
}

func buildRequestBody(request model.Request, stream bool, compatibility chatCompatibility) ([]byte, error) {
	body := make(map[string]any, len(request.Parameters)+4)
	for key, value := range request.Parameters {
		body[key] = value
	}

	toolNames := newToolNameMapper(request.Tools)
	messages, err := buildMessages(request.Messages, request.DeveloperRole, compatibility, toolNames)
	if err != nil {
		return nil, err
	}

	body["model"] = request.Model
	body["messages"] = messages
	body["stream"] = stream
	if len(request.Tools) > 0 {
		body["tools"] = buildTools(request.Tools, toolNames)
	}
	compatibility.prepareRequest(body, request, stream)

	return json.Marshal(body)
}

func buildMessages(messages []model.Message, developerRole model.MessageRole, compatibility chatCompatibility, toolNames ...*toolNameMapper) ([]map[string]any, error) {
	switch developerRole {
	case "", model.MessageRoleDeveloper, model.MessageRoleSystem:
	default:
		return nil, fmt.Errorf("unsupported OpenAI Chat developer role mapping %q", developerRole)
	}
	out := make([]map[string]any, 0, len(messages))
	var mapper *toolNameMapper
	if len(toolNames) > 0 {
		mapper = toolNames[0]
	}
	for _, message := range messages {
		content, err := openAIChatMessageContent(message)
		if err != nil {
			return nil, err
		}
		role := message.Role
		if role == model.MessageRoleDeveloper && developerRole != "" {
			role = developerRole
		}
		item := map[string]any{
			"role":    string(role),
			"content": content,
		}
		if len(message.ToolCalls) > 0 {
			item["tool_calls"] = buildMessageToolCalls(message.ToolCalls, mapper)
		}
		if message.Role == model.MessageRoleTool {
			item["tool_call_id"] = message.ToolCallID
		}
		compatibility.prepareMessage(item, message)
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

func buildMessageToolCalls(toolCalls []model.ToolCall, toolNames ...*toolNameMapper) []map[string]any {
	var mapper *toolNameMapper
	if len(toolNames) > 0 {
		mapper = toolNames[0]
	}
	out := make([]map[string]any, 0, len(toolCalls))
	for _, toolCall := range toolCalls {
		out = append(out, map[string]any{
			"id":   toolCall.ID,
			"type": "function",
			"function": map[string]any{
				"name":      mapper.providerName(toolCall.Name),
				"arguments": toolCall.Arguments,
			},
		})
	}
	return out
}

func buildTools(tools []model.Tool, toolNames ...*toolNameMapper) []map[string]any {
	var mapper *toolNameMapper
	if len(toolNames) > 0 {
		mapper = toolNames[0]
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		parameters := tool.InputSchema
		if parameters == nil {
			parameters = map[string]any{}
		}

		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        mapper.providerName(tool.Name),
				"description": tool.Description,
				"parameters":  parameters,
			},
		})
	}
	return out
}

var openAIChatToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type toolNameMapper struct {
	toProvider map[string]string
	toInternal map[string]string
	used       map[string]struct{}
	reserved   map[string]struct{}
	nextAlias  int
}

func newToolNameMapper(tools []model.Tool) *toolNameMapper {
	mapper := &toolNameMapper{
		toProvider: make(map[string]string, len(tools)),
		toInternal: make(map[string]string, len(tools)),
		used:       make(map[string]struct{}, len(tools)),
		reserved:   make(map[string]struct{}, len(tools)),
	}
	for _, tool := range tools {
		if isValidOpenAIChatToolName(tool.Name) {
			mapper.reserved[tool.Name] = struct{}{}
		}
	}
	for _, tool := range tools {
		mapper.providerName(tool.Name)
	}
	return mapper
}

func (m *toolNameMapper) providerName(internalName string) string {
	if m == nil {
		return internalName
	}
	if name, ok := m.toProvider[internalName]; ok {
		return name
	}
	name := internalName
	if !isValidOpenAIChatToolName(name) || m.isUsed(name) {
		name = m.nextToolAlias()
	}
	m.toProvider[internalName] = name
	m.toInternal[name] = internalName
	m.used[name] = struct{}{}
	return name
}

func (m *toolNameMapper) internalName(providerName string) string {
	if m == nil {
		return providerName
	}
	if name, ok := m.toInternal[providerName]; ok {
		return name
	}
	return providerName
}

func (m *toolNameMapper) nextToolAlias() string {
	for {
		name := fmt.Sprintf("tool_%d", m.nextAlias)
		m.nextAlias++
		if m.isUsed(name) {
			continue
		}
		if _, reserved := m.reserved[name]; reserved {
			continue
		}
		return name
	}
}

func (m *toolNameMapper) isUsed(name string) bool {
	_, ok := m.used[name]
	return ok
}

func isValidOpenAIChatToolName(name string) bool {
	return len(name) > 0 && len(name) <= 64 && openAIChatToolNamePattern.MatchString(name)
}
