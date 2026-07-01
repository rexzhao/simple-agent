package openairesponses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

func BuildRequestBody(request model.Request, stream bool) ([]byte, error) {
	body, _, err := buildRequestBody(request, stream)
	return body, err
}

func buildRequestBody(request model.Request, stream bool) ([]byte, *toolNameMapper, error) {
	toolNames := newToolNameMapper(request.Tools)

	input, err := buildInput(request.Messages, toolNames)
	if err != nil {
		return nil, nil, err
	}

	body, err := buildParameters(request.Parameters, toolNames)
	if err != nil {
		return nil, nil, err
	}

	body["model"] = request.Model
	body["input"] = input
	body["stream"] = stream
	if len(request.Tools) > 0 {
		body["tools"] = buildTools(request.Tools, toolNames)
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	return data, toolNames, nil
}

func buildParameters(parameters map[string]any, toolNames *toolNameMapper) (map[string]any, error) {
	body := make(map[string]any, len(parameters)+3)
	for key, value := range parameters {
		if isUnsupportedParameter(key) {
			return nil, fmt.Errorf("OpenAI Responses adapter does not support parameter %q", key)
		}
		if key == "max_tokens" {
			if _, ok := parameters["max_output_tokens"]; !ok {
				body["max_output_tokens"] = value
			}
			continue
		}
		if key == "tool_choice" {
			body[key] = mapToolChoice(value, toolNames)
			continue
		}
		body[key] = value
	}
	return body, nil
}

func isUnsupportedParameter(key string) bool {
	switch key {
	case "tools",
		"tool_resources",
		"tool_outputs",
		"functions",
		"function_call",
		"function_call_output",
		"function_call_outputs",
		"previous_response_id",
		"web_search_options":
		return true
	default:
		return false
	}
}

func mapToolChoice(value any, toolNames *toolNameMapper) any {
	choice, ok := value.(map[string]any)
	if !ok {
		return value
	}
	choiceType, _ := choice["type"].(string)
	if choiceType == "allowed_tools" {
		tools, ok := choice["tools"].([]any)
		if !ok {
			return value
		}
		out := copyMap(choice)
		outTools := make([]any, 0, len(tools))
		for _, tool := range tools {
			outTools = append(outTools, mapToolChoiceTool(tool, toolNames))
		}
		out["tools"] = outTools
		return out
	}

	name, ok := choice["name"].(string)
	if !ok || choiceType != "function" {
		return value
	}

	out := copyMap(choice)
	out["name"] = toolNames.responsesName(name)
	return out
}

func mapToolChoiceTool(value any, toolNames *toolNameMapper) any {
	tool, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if toolType, _ := tool["type"].(string); toolType != "function" {
		return value
	}
	name, ok := tool["name"].(string)
	if !ok {
		return value
	}

	out := copyMap(tool)
	out["name"] = toolNames.responsesName(name)
	return out
}

func copyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func buildInput(messages []model.Message, toolNames *toolNameMapper) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case model.MessageRoleSystem, model.MessageRoleDeveloper, model.MessageRoleUser:
			out = append(out, map[string]any{
				"role":    string(message.Role),
				"content": message.Content,
			})
		case model.MessageRoleAssistant:
			if message.Content != "" || len(message.ToolCalls) == 0 {
				out = append(out, map[string]any{
					"role":    string(message.Role),
					"content": message.Content,
				})
			}
			for _, toolCall := range message.ToolCalls {
				out = append(out, buildFunctionCallInput(toolCall, toolNames))
			}
		case model.MessageRoleTool:
			out = append(out, map[string]any{
				"type":    "function_call_output",
				"call_id": message.ToolCallID,
				"output":  message.Content,
			})
		default:
			return nil, fmt.Errorf("unsupported OpenAI Responses role %q", message.Role)
		}
	}
	return out, nil
}

func buildFunctionCallInput(toolCall model.ToolCall, toolNames *toolNameMapper) map[string]any {
	return map[string]any{
		"type":      "function_call",
		"call_id":   toolCall.ID,
		"name":      toolNames.responsesName(toolCall.Name),
		"arguments": responseToolCallArguments(toolCall.Arguments),
	}
}

func responseToolCallArguments(arguments string) string {
	if strings.TrimSpace(arguments) == "" {
		return "{}"
	}
	return arguments
}

func buildTools(tools []model.Tool, toolNames *toolNameMapper) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		out = append(out, map[string]any{
			"type":        "function",
			"name":        toolNames.responsesName(tool.Name),
			"description": tool.Description,
			"parameters":  responsesInputSchema(tool.InputSchema),
		})
	}
	return out
}

func responsesInputSchema(schema map[string]any) map[string]any {
	if len(schema) > 0 {
		return schema
	}
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

type toolNameMapper struct {
	toResponses map[string]string
	toInternal  map[string]string
	used        map[string]struct{}
	reserved    map[string]struct{}
	nextAlias   int
}

func newToolNameMapper(tools []model.Tool) *toolNameMapper {
	mapper := &toolNameMapper{
		toResponses: make(map[string]string, len(tools)),
		toInternal:  make(map[string]string, len(tools)),
		used:        make(map[string]struct{}, len(tools)),
		reserved:    make(map[string]struct{}, len(tools)),
	}
	for _, tool := range tools {
		if isValidOpenAIResponsesToolName(tool.Name) {
			mapper.reserved[tool.Name] = struct{}{}
		}
	}
	for _, tool := range tools {
		mapper.mapToolName(tool.Name)
	}
	return mapper
}

func (m *toolNameMapper) responsesName(internalName string) string {
	if m == nil {
		return internalName
	}
	if name, ok := m.toResponses[internalName]; ok {
		return name
	}
	return m.mapToolName(internalName)
}

func (m *toolNameMapper) internalName(responsesName string) string {
	if m == nil {
		return responsesName
	}
	if name, ok := m.toInternal[responsesName]; ok {
		return name
	}
	return responsesName
}

func (m *toolNameMapper) mapToolName(internalName string) string {
	if name, ok := m.toResponses[internalName]; ok {
		return name
	}

	name := internalName
	if !isValidOpenAIResponsesToolName(name) || m.isUsed(name) {
		name = m.nextToolAlias()
	}
	m.toResponses[internalName] = name
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

func isValidOpenAIResponsesToolName(name string) bool {
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
