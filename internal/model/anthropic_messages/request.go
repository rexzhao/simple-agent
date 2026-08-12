package anthropicmessages

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

func BuildRequestBody(request model.Request, stream bool) ([]byte, error) {
	body, _, err := buildRequestBody(request, stream)
	return body, err
}

func buildRequestBody(request model.Request, stream bool) ([]byte, *toolNameMapper, error) {
	toolNames := newToolNameMapper(request.Tools)
	body := make(map[string]any, len(request.Parameters)+4)
	for key, value := range request.Parameters {
		body[key] = value
	}
	applyAnthropicThinking(body)

	messages, system, err := buildMessages(request.Messages, toolNames)
	if err != nil {
		return nil, nil, err
	}

	body["model"] = request.Model
	body["messages"] = messages
	body["stream"] = stream
	if system != "" {
		body["system"] = system
	}
	if len(request.Tools) > 0 {
		body["tools"] = buildTools(request.Tools, toolNames)
	}
	if _, configured := body["max_tokens"]; !configured && request.MaxTokens > 0 {
		body["max_tokens"] = request.MaxTokens
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	return data, toolNames, nil
}

// applyAnthropicThinking converts dot-separated thinking.* parameters into the
// nested Anthropic thinking block. Values are written as the canonical
// {type: enabled, budget_tokens: N} shape so a budget_tokens reasoning config
// sends the native structure instead of a flat thinking field. An explicit
// thinking.type value (enabled/disabled/adaptive) is preserved; budget_tokens
// only fills in the default enabled type when no type was provided.
func applyAnthropicThinking(body map[string]any) {
	rawBudget, hasBudget := body["thinking.budget_tokens"]
	rawType, hasType := body["thinking.type"]
	if !hasBudget && !hasType {
		return
	}
	delete(body, "thinking.budget_tokens")
	delete(body, "thinking.type")
	thinking := map[string]any{}
	if hasType {
		thinking["type"] = rawType
	}
	if hasBudget {
		if !hasType {
			thinking["type"] = "enabled"
		}
		thinking["budget_tokens"] = rawBudget
	}
	body["thinking"] = thinking
}

func buildMessages(messages []model.Message, toolNames *toolNameMapper) ([]map[string]any, string, error) {
	out := make([]map[string]any, 0, len(messages))
	systemParts := []string{}

	for _, message := range messages {
		switch message.Role {
		case model.MessageRoleSystem, model.MessageRoleDeveloper:
			content := strings.TrimSpace(message.Content)
			if content != "" {
				systemParts = append(systemParts, content)
			}
		case model.MessageRoleUser:
			content, err := anthropicUserContent(message)
			if err != nil {
				return nil, "", err
			}
			out = append(out, map[string]any{
				"role":    string(message.Role),
				"content": content,
			})
		case model.MessageRoleAssistant:
			if len(message.ContentBlocks) > 0 {
				return nil, "", fmt.Errorf("Anthropic Messages content blocks are only supported for user messages")
			}
			if len(message.ToolCalls) > 0 {
				content, err := buildAssistantContent(message, toolNames)
				if err != nil {
					return nil, "", err
				}
				out = append(out, map[string]any{
					"role":    string(message.Role),
					"content": content,
				})
				continue
			}
			out = append(out, map[string]any{
				"role":    string(message.Role),
				"content": message.Content,
			})
		case model.MessageRoleTool:
			out = appendToolResultMessage(out, buildToolResultBlock(message))
		default:
			return nil, "", fmt.Errorf("unsupported Anthropic Messages role %q", message.Role)
		}
	}

	return out, strings.Join(systemParts, "\n\n"), nil
}

func anthropicUserContent(message model.Message) (any, error) {
	if len(message.ContentBlocks) == 0 {
		return message.Content, nil
	}
	if message.Content != "" {
		return nil, fmt.Errorf("Anthropic Messages message cannot set both content and content blocks")
	}

	content := make([]map[string]any, 0, len(message.ContentBlocks))
	for _, block := range message.ContentBlocks {
		switch strings.TrimSpace(block.Type) {
		case "", "input_text":
			content = append(content, map[string]any{"type": "text", "text": block.Text})
		case "input_image":
			if block.ImageBlob != nil {
				return nil, fmt.Errorf("Anthropic image attachment must be materialized before requesting the model")
			}
			mediaType, data, err := model.ParseImageDataURL(block.ImageURL)
			if err != nil {
				return nil, fmt.Errorf("Anthropic input_image: %w", err)
			}
			switch mediaType {
			case "image/jpeg", "image/png", "image/gif", "image/webp":
			default:
				return nil, fmt.Errorf("unsupported Anthropic image media type %q", mediaType)
			}
			content = append(content, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": mediaType,
					"data":       base64.StdEncoding.EncodeToString(data),
				},
			})
		default:
			return nil, fmt.Errorf("unsupported Anthropic content block type %q", block.Type)
		}
	}
	return content, nil
}

func buildTools(tools []model.Tool, toolNames *toolNameMapper) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		out = append(out, map[string]any{
			"name":         toolNames.anthropicName(tool.Name),
			"description":  tool.Description,
			"input_schema": anthropicInputSchema(tool.InputSchema),
		})
	}
	return out
}

func anthropicInputSchema(schema map[string]any) map[string]any {
	if len(schema) > 0 {
		return schema
	}
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func buildAssistantContent(message model.Message, toolNames *toolNameMapper) ([]map[string]any, error) {
	content := make([]map[string]any, 0, len(message.ToolCalls)+1)
	if message.Content != "" {
		content = append(content, map[string]any{
			"type": "text",
			"text": message.Content,
		})
	}
	for _, toolCall := range message.ToolCalls {
		input, err := parseToolCallInput(toolCall.Arguments)
		if err != nil {
			return nil, fmt.Errorf("parse Anthropic assistant tool call %q input: %w", toolCall.ID, err)
		}
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    toolCall.ID,
			"name":  toolNames.anthropicName(toolCall.Name),
			"input": input,
		})
	}
	return content, nil
}

func parseToolCallInput(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}

	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.UseNumber()

	var input map[string]any
	if err := decoder.Decode(&input); err != nil {
		return nil, err
	}
	if input == nil {
		return nil, fmt.Errorf("must be a JSON object")
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("must contain a single JSON object")
	}
	return input, nil
}

func appendToolResultMessage(messages []map[string]any, block map[string]any) []map[string]any {
	if len(messages) > 0 && messages[len(messages)-1]["role"] == "user" {
		if blocks, ok := messages[len(messages)-1]["content"].([]map[string]any); ok {
			messages[len(messages)-1]["content"] = append(blocks, block)
			return messages
		}
	}
	return append(messages, map[string]any{
		"role":    "user",
		"content": []map[string]any{block},
	})
}

func buildToolResultBlock(message model.Message) map[string]any {
	block := map[string]any{
		"type":        "tool_result",
		"tool_use_id": message.ToolCallID,
		"content":     message.Content,
	}
	if message.IsError {
		block["is_error"] = true
	}
	return block
}

type toolNameMapper struct {
	toAnthropic map[string]string
	toInternal  map[string]string
	used        map[string]struct{}
	reserved    map[string]struct{}
	nextAlias   int
}

func newToolNameMapper(tools []model.Tool) *toolNameMapper {
	mapper := &toolNameMapper{
		toAnthropic: make(map[string]string, len(tools)),
		toInternal:  make(map[string]string, len(tools)),
		used:        make(map[string]struct{}, len(tools)),
		reserved:    make(map[string]struct{}, len(tools)),
	}
	for _, tool := range tools {
		if isValidAnthropicToolName(tool.Name) {
			mapper.reserved[tool.Name] = struct{}{}
		}
	}
	for _, tool := range tools {
		mapper.mapToolName(tool.Name)
	}
	return mapper
}

func (m *toolNameMapper) anthropicName(internalName string) string {
	if m == nil {
		return internalName
	}
	if name, ok := m.toAnthropic[internalName]; ok {
		return name
	}
	return m.mapToolName(internalName)
}

func (m *toolNameMapper) internalName(anthropicName string) string {
	if m == nil {
		return anthropicName
	}
	if name, ok := m.toInternal[anthropicName]; ok {
		return name
	}
	return anthropicName
}

func (m *toolNameMapper) mapToolName(internalName string) string {
	if name, ok := m.toAnthropic[internalName]; ok {
		return name
	}

	name := internalName
	if !isValidAnthropicToolName(name) || m.isUsed(name) {
		name = m.nextToolAlias()
	}
	m.toAnthropic[internalName] = name
	m.toInternal[name] = internalName
	m.used[name] = struct{}{}
	return name
}

func (m *toolNameMapper) nextToolAlias() string {
	for {
		name := fmt.Sprintf("tool_%d", m.nextAlias)
		m.nextAlias++
		if m.isUsed(name) {
			continue
		}
		if _, ok := m.reserved[name]; ok {
			continue
		}
		return name
	}
}

func (m *toolNameMapper) isUsed(name string) bool {
	_, ok := m.used[name]
	return ok
}

func isValidAnthropicToolName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '_' || char == '-':
		default:
			return false
		}
	}
	return true
}
