package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

type Executor interface {
	Execute(ctx context.Context, arguments map[string]any) (model.ToolResult, error)
}

type ExecutorFunc func(ctx context.Context, arguments map[string]any) (model.ToolResult, error)

func (f ExecutorFunc) Execute(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
	return f(ctx, arguments)
}

type Entry struct {
	Definition model.Tool
	Executor   Executor
}

type Registry struct {
	entries map[string]Entry
}

func NewRegistry() *Registry {
	return &Registry{
		entries: make(map[string]Entry),
	}
}

func (r *Registry) Register(definition model.Tool, executor Executor) error {
	if strings.TrimSpace(definition.Name) == "" {
		return fmt.Errorf("tool name must not be blank")
	}
	if executor == nil {
		return fmt.Errorf("tool %q executor must not be nil", definition.Name)
	}
	if _, ok := r.entries[definition.Name]; ok {
		return fmt.Errorf("tool %q already registered", definition.Name)
	}

	r.entries[definition.Name] = Entry{
		Definition: definition,
		Executor:   executor,
	}
	return nil
}

func (r *Registry) Lookup(name string) (Entry, bool) {
	entry, ok := r.entries[name]
	return entry, ok
}

func (r *Registry) EnabledSchemas(enabled []string) ([]model.Tool, error) {
	schemas := make([]model.Tool, 0, len(enabled))
	for _, name := range enabled {
		entry, ok := r.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("enabled tool %q is not registered", name)
		}
		schemas = append(schemas, entry.Definition)
	}
	return schemas, nil
}

func (r *Registry) Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error) {
	entry, ok := r.Lookup(name)
	if !ok {
		return model.ToolResult{}, fmt.Errorf("tool %q is not registered", name)
	}
	return entry.Executor.Execute(ctx, arguments)
}
