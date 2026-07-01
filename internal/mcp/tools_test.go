package mcp

import (
	"reflect"
	"testing"
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
