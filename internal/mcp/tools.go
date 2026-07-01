package mcp

import (
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
