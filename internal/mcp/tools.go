package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

func ToolName(serverID, toolName string) (string, error) {
	serverID = strings.TrimSpace(serverID)
	if serverID == "" {
		return "", fmt.Errorf("MCP server id must not be blank")
	}

	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return "", fmt.Errorf("MCP tool name must not be blank")
	}

	return "mcp." + serverID + "." + toolName, nil
}

func ParseToolName(name string) (string, string, error) {
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, "mcp.") {
		return "", "", fmt.Errorf("MCP tool name %q must start with mcp.", name)
	}

	rest := strings.TrimPrefix(name, "mcp.")
	serverID, toolName, ok := strings.Cut(rest, ".")
	if !ok || strings.TrimSpace(serverID) == "" || strings.TrimSpace(toolName) == "" {
		return "", "", fmt.Errorf("MCP tool name %q must have form mcp.<server>.<tool>", name)
	}
	return serverID, toolName, nil
}

func ConvertTools(serverID string, definitions []ToolDefinition) ([]model.Tool, error) {
	serverID = strings.TrimSpace(serverID)
	if serverID == "" {
		return nil, fmt.Errorf("MCP server id must not be blank")
	}

	tools := make([]model.Tool, 0, len(definitions))
	seen := make(map[string]struct{}, len(definitions))

	for _, definition := range definitions {
		name, err := ToolName(serverID, definition.Name)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("MCP tool %q is duplicated", name)
		}
		seen[name] = struct{}{}

		inputSchema := definition.InputSchema
		if len(inputSchema) == 0 {
			inputSchema = map[string]any{"type": "object"}
		}

		tools = append(tools, model.Tool{
			Name:        name,
			Description: definition.Description,
			InputSchema: inputSchema,
		})
	}

	return tools, nil
}

func EnabledSchemas(tools []model.Tool, enabled []string) ([]model.Tool, error) {
	byName := make(map[string]model.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name] = tool
	}

	schemas := make([]model.Tool, 0, len(enabled))
	for _, name := range enabled {
		if !strings.HasPrefix(name, "mcp.") {
			continue
		}

		tool, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("enabled MCP tool %q is not available", name)
		}
		schemas = append(schemas, tool)
	}
	return schemas, nil
}

func ToModelToolResult(name string, result ToolCallResult) model.ToolResult {
	return model.ToolResult{
		Name:    name,
		Content: toolResultContent(result.Content),
		IsError: result.IsError,
	}
}

func toolResultContent(blocks []json.RawMessage) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		var textBlock struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(block, &textBlock); err == nil && textBlock.Type == "text" {
			parts = append(parts, textBlock.Text)
			continue
		}
		if len(block) > 0 {
			parts = append(parts, string(block))
		}
	}
	return strings.Join(parts, "\n")
}
