package mcp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestToolNamePrefixesServerAndTool(t *testing.T) {
	got, err := ToolName("local", "search")
	if err != nil {
		t.Fatalf("ToolName() error = %v", err)
	}

	want := "mcp.local.search"
	if got != want {
		t.Fatalf("ToolName() = %q, want %q", got, want)
	}
}

func TestParseToolNameReturnsServerAndOriginalTool(t *testing.T) {
	serverID, toolName, err := ParseToolName("mcp.local.search")
	if err != nil {
		t.Fatalf("ParseToolName() error = %v", err)
	}
	if serverID != "local" || toolName != "search" {
		t.Fatalf("ParseToolName() = %q, %q; want local, search", serverID, toolName)
	}
}

func TestParseToolNameRejectsMalformedNames(t *testing.T) {
	for _, name := range []string{"search", "mcp.local", "mcp..search", "mcp.local."} {
		_, _, err := ParseToolName(name)
		if err == nil {
			t.Fatalf("ParseToolName(%q) error = nil, want error", name)
		}
	}
}

func TestConvertToolsMapsDescriptionAndSchema(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
		},
	}

	tools, err := ConvertTools("local", []ToolDefinition{
		{
			Name:        "search",
			Description: "search documents",
			InputSchema: schema,
		},
	})
	if err != nil {
		t.Fatalf("ConvertTools() error = %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("ConvertTools() returned %d tools, want 1", len(tools))
	}

	if tools[0].Name != "mcp.local.search" {
		t.Fatalf("Name = %q, want mcp.local.search", tools[0].Name)
	}
	if tools[0].Description != "search documents" {
		t.Fatalf("Description = %q, want search documents", tools[0].Description)
	}
	if !reflect.DeepEqual(tools[0].InputSchema, schema) {
		t.Fatalf("InputSchema = %#v, want %#v", tools[0].InputSchema, schema)
	}
}

func TestConvertToolsDefaultsBlankInputSchemaToObject(t *testing.T) {
	tools, err := ConvertTools("local", []ToolDefinition{{Name: "empty_schema"}})
	if err != nil {
		t.Fatalf("ConvertTools() error = %v", err)
	}

	want := map[string]any{"type": "object"}
	if !reflect.DeepEqual(tools[0].InputSchema, want) {
		t.Fatalf("InputSchema = %#v, want %#v", tools[0].InputSchema, want)
	}
}

func TestConvertToolsRejectsBlankServerID(t *testing.T) {
	_, err := ConvertTools(" \t", nil)
	if err == nil {
		t.Fatalf("ConvertTools() error = nil, want error")
	}
}

func TestConvertToolsRejectsBlankToolName(t *testing.T) {
	_, err := ConvertTools("local", []ToolDefinition{{Name: " \t"}})
	if err == nil {
		t.Fatalf("ConvertTools() error = nil, want error")
	}
}

func TestConvertToolsRejectsDuplicateConvertedNames(t *testing.T) {
	_, err := ConvertTools("local", []ToolDefinition{
		{Name: "search"},
		{Name: " search "},
	})
	if err == nil {
		t.Fatalf("ConvertTools() error = nil, want error")
	}
}

func TestEnabledSchemasReturnsOnlyEnabledMCPTools(t *testing.T) {
	tools := []model.Tool{
		{Name: "mcp.local.search", Description: "search"},
		{Name: "mcp.local.read", Description: "read"},
		{Name: "mcp.git.status", Description: "status"},
	}

	got, err := EnabledSchemas(tools, []string{"mcp.local.read"})
	if err != nil {
		t.Fatalf("EnabledSchemas() error = %v", err)
	}

	want := []model.Tool{tools[1]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EnabledSchemas() = %#v, want %#v", got, want)
	}
}

func TestEnabledSchemasSkipsNonMCPEnabledTools(t *testing.T) {
	tools := []model.Tool{
		{Name: "mcp.local.search", Description: "search"},
		{Name: "mcp.local.read", Description: "read"},
	}

	got, err := EnabledSchemas(tools, []string{"list_files", "mcp.local.read", "read_file"})
	if err != nil {
		t.Fatalf("EnabledSchemas() error = %v", err)
	}

	want := []model.Tool{tools[1]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EnabledSchemas() = %#v, want %#v", got, want)
	}
}

func TestEnabledSchemasPreservesEnabledOrder(t *testing.T) {
	tools := []model.Tool{
		{Name: "mcp.local.search"},
		{Name: "mcp.local.read"},
		{Name: "mcp.git.status"},
	}

	got, err := EnabledSchemas(tools, []string{"mcp.git.status", "mcp.local.search"})
	if err != nil {
		t.Fatalf("EnabledSchemas() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("EnabledSchemas() returned %d tools, want 2", len(got))
	}

	gotNames := []string{got[0].Name, got[1].Name}
	wantNames := []string{"mcp.git.status", "mcp.local.search"}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("EnabledSchemas() names = %#v, want %#v", gotNames, wantNames)
	}
}

func TestEnabledSchemasRejectsUnknownTool(t *testing.T) {
	tools := []model.Tool{{Name: "mcp.local.search"}}

	_, err := EnabledSchemas(tools, []string{"mcp.local.missing"})
	if err == nil {
		t.Fatalf("EnabledSchemas() error = nil, want error")
	}
	if !strings.Contains(err.Error(), `enabled MCP tool "mcp.local.missing" is not available`) {
		t.Fatalf("EnabledSchemas() error = %q, want unknown tool message", err)
	}
}

func TestEnabledSchemasReturnsEmptyWhenEnabledIsEmpty(t *testing.T) {
	tools := []model.Tool{{Name: "mcp.local.search"}}

	got, err := EnabledSchemas(tools, nil)
	if err != nil {
		t.Fatalf("EnabledSchemas() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("EnabledSchemas() returned %d tools, want 0", len(got))
	}
}

func TestToModelToolResultJoinsTextBlocksAndPreservesIsError(t *testing.T) {
	result := ToModelToolResult("mcp.local.search", ToolCallResult{
		Content: []json.RawMessage{
			json.RawMessage(`{"type":"text","text":"first"}`),
			json.RawMessage(`{"type":"text","text":"second"}`),
		},
		IsError: true,
	})

	want := model.ToolResult{
		Name:    "mcp.local.search",
		Content: "first\nsecond",
		IsError: true,
	}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("ToModelToolResult() = %#v, want %#v", result, want)
	}
}

func TestToModelToolResultStringifiesNonTextBlocks(t *testing.T) {
	result := ToModelToolResult("mcp.local.search", ToolCallResult{
		Content: []json.RawMessage{
			json.RawMessage(`{"type":"image","data":"abc"}`),
		},
	})

	if result.Content != `{"type":"image","data":"abc"}` {
		t.Fatalf("Content = %q, want JSON block", result.Content)
	}
}
