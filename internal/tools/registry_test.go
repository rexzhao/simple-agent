package tools

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestRegistryRegisterAndLookup(t *testing.T) {
	registry := NewRegistry()
	definition := model.Tool{
		Name:        "read_file",
		Description: "Read a file from the workspace.",
		InputSchema: map[string]any{
			"type": "object",
		},
	}

	err := registry.Register(definition, ExecutorFunc(func(context.Context, map[string]any) (model.ToolResult, error) {
		return model.ToolResult{}, nil
	}))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	entry, ok := registry.Lookup("read_file")
	if !ok {
		t.Fatal("Lookup() ok = false, want true")
	}
	if !reflect.DeepEqual(entry.Definition, definition) {
		t.Fatalf("Lookup() definition = %#v, want %#v", entry.Definition, definition)
	}

	if _, ok := registry.Lookup("missing"); ok {
		t.Fatal("Lookup() missing ok = true, want false")
	}
}

func TestRegistryRejectsDuplicateRegister(t *testing.T) {
	registry := NewRegistry()
	definition := model.Tool{Name: "read_file"}
	executor := ExecutorFunc(func(context.Context, map[string]any) (model.ToolResult, error) {
		return model.ToolResult{}, nil
	})

	if err := registry.Register(definition, executor); err != nil {
		t.Fatalf("Register() first error = %v", err)
	}
	err := registry.Register(definition, executor)
	if err == nil {
		t.Fatal("Register() second error = nil, want error")
	}
	if !strings.Contains(err.Error(), `tool "read_file" already registered`) {
		t.Fatalf("Register() second error = %q", err)
	}
}

func TestRegistryRejectsBlankToolName(t *testing.T) {
	registry := NewRegistry()
	executor := ExecutorFunc(func(context.Context, map[string]any) (model.ToolResult, error) {
		return model.ToolResult{}, nil
	})

	err := registry.Register(model.Tool{Name: " \t\n"}, executor)
	if err == nil {
		t.Fatal("Register() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "tool name must not be blank") {
		t.Fatalf("Register() error = %q", err)
	}
}

func TestRegistryRejectsNilExecutor(t *testing.T) {
	registry := NewRegistry()

	err := registry.Register(model.Tool{Name: "read_file"}, nil)
	if err == nil {
		t.Fatal("Register() error = nil, want error")
	}
	if !strings.Contains(err.Error(), `tool "read_file" executor must not be nil`) {
		t.Fatalf("Register() error = %q", err)
	}
}

func TestRegistryEnabledSchemasFiltersEnabledTools(t *testing.T) {
	registry := NewRegistry()
	definitions := []model.Tool{
		{Name: "list_files", Description: "List files."},
		{Name: "read_file", Description: "Read a file."},
		{Name: "shell", Description: "Run a shell command."},
	}
	for _, definition := range definitions {
		err := registry.Register(definition, ExecutorFunc(func(context.Context, map[string]any) (model.ToolResult, error) {
			return model.ToolResult{}, nil
		}))
		if err != nil {
			t.Fatalf("Register(%q) error = %v", definition.Name, err)
		}
	}

	schemas, err := registry.EnabledSchemas([]string{"read_file", "list_files"})
	if err != nil {
		t.Fatalf("EnabledSchemas() error = %v", err)
	}

	want := []model.Tool{definitions[1], definitions[0]}
	if !reflect.DeepEqual(schemas, want) {
		t.Fatalf("EnabledSchemas() = %#v, want %#v", schemas, want)
	}
}

func TestRegistryEnabledSchemasRejectsUnknownEnabledTool(t *testing.T) {
	registry := NewRegistry()

	_, err := registry.EnabledSchemas([]string{"missing"})
	if err == nil {
		t.Fatal("EnabledSchemas() error = nil, want error")
	}
	if !strings.Contains(err.Error(), `enabled tool "missing" is not registered`) {
		t.Fatalf("EnabledSchemas() error = %q", err)
	}
}

func TestRegistryExecuteCallsExecutor(t *testing.T) {
	registry := NewRegistry()
	ctx := context.WithValue(context.Background(), contextKey("trace"), "call")
	arguments := map[string]any{"text": "hello"}
	called := false

	err := registry.Register(model.Tool{Name: "echo"}, ExecutorFunc(func(gotCtx context.Context, gotArguments map[string]any) (model.ToolResult, error) {
		called = true
		if gotCtx.Value(contextKey("trace")) != "call" {
			t.Fatalf("executor ctx value = %v, want call", gotCtx.Value(contextKey("trace")))
		}
		if !reflect.DeepEqual(gotArguments, arguments) {
			t.Fatalf("executor arguments = %#v, want %#v", gotArguments, arguments)
		}
		return model.ToolResult{
			ToolCallID: "call_1",
			Name:       "echo",
			Content:    "hello",
		}, nil
	}))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	result, err := registry.Execute(ctx, "echo", arguments)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !called {
		t.Fatal("executor was not called")
	}

	want := model.ToolResult{
		ToolCallID: "call_1",
		Name:       "echo",
		Content:    "hello",
	}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("Execute() result = %#v, want %#v", result, want)
	}
}

type contextKey string
