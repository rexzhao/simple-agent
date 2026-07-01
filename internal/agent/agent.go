package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

const DefaultMaxTurns = 8

type ToolExecutor interface {
	Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error)
}

type Options struct {
	Provider     model.Provider
	ToolExecutor ToolExecutor
	MaxTurns     int
}

func Stream(ctx context.Context, request model.Request, options Options) (<-chan model.Event, error) {
	if options.Provider == nil {
		return nil, fmt.Errorf("agent provider is required")
	}

	maxTurns := options.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}

	events := make(chan model.Event)
	go run(ctx, request, options, maxTurns, events)
	return events, nil
}

func run(ctx context.Context, request model.Request, options Options, maxTurns int, events chan<- model.Event) {
	defer close(events)

	messages := append([]model.Message(nil), request.Messages...)
	enabledTools := enabledToolNames(request.Tools)

	for turn := 1; turn <= maxTurns; turn++ {
		request.Messages = messages

		assistantContent, toolCalls, stopped := streamModelTurn(ctx, options.Provider, request, events)
		if stopped || len(toolCalls) == 0 {
			return
		}

		messages = append(messages, model.Message{
			Role:      model.MessageRoleAssistant,
			Content:   assistantContent,
			ToolCalls: toolCalls,
		})
		for _, toolCall := range toolCalls {
			result := executeToolCall(ctx, options.ToolExecutor, enabledTools, toolCall)
			events <- model.ToolResultEvent{Result: result}
			messages = append(messages, model.Message{
				Role:       model.MessageRoleTool,
				Content:    result.Content,
				ToolCallID: result.ToolCallID,
				IsError:    result.IsError,
			})
		}

		if turn == maxTurns {
			events <- model.ErrorEvent{
				Err: fmt.Errorf("agent reached max_turns %d before the model returned a final response", maxTurns),
			}
			return
		}
	}
}

func streamModelTurn(ctx context.Context, provider model.Provider, request model.Request, out chan<- model.Event) (string, []model.ToolCall, bool) {
	stream, err := provider.Stream(ctx, request)
	if err != nil {
		out <- model.ErrorEvent{Err: err, Message: "request model"}
		return "", nil, true
	}

	var assistantContent strings.Builder
	var toolCalls []model.ToolCall
	for event := range stream {
		switch event := event.(type) {
		case model.TextDeltaEvent:
			assistantContent.WriteString(event.Text)
		case model.ToolCallDoneEvent:
			toolCalls = append(toolCalls, event.ToolCall)
		case model.ErrorEvent:
			out <- event
			return assistantContent.String(), nil, true
		}
		out <- event
	}
	return assistantContent.String(), toolCalls, false
}

func executeToolCall(ctx context.Context, executor ToolExecutor, enabledTools map[string]struct{}, toolCall model.ToolCall) model.ToolResult {
	arguments, err := parseToolArguments(toolCall.Arguments)
	if err != nil {
		return toolErrorResult(toolCall, "invalid tool arguments: %v", err)
	}
	if _, ok := enabledTools[toolCall.Name]; !ok {
		return toolErrorResult(toolCall, "tool %q is not enabled", toolCall.Name)
	}
	if executor == nil {
		return toolErrorResult(toolCall, "tool executor is not configured")
	}

	result, err := executor.Execute(ctx, toolCall.Name, arguments)
	if err != nil {
		return toolErrorResult(toolCall, "%v", err)
	}
	if result.ToolCallID == "" {
		result.ToolCallID = toolCall.ID
	}
	if result.Name == "" {
		result.Name = toolCall.Name
	}
	return result
}

func parseToolArguments(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}

	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.UseNumber()

	var arguments map[string]any
	if err := decoder.Decode(&arguments); err != nil {
		return nil, err
	}
	if arguments == nil {
		return nil, fmt.Errorf("must be a JSON object")
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("must contain a single JSON object")
	}
	return arguments, nil
}

func toolErrorResult(toolCall model.ToolCall, format string, args ...any) model.ToolResult {
	return model.ToolResult{
		ToolCallID: toolCall.ID,
		Name:       toolCall.Name,
		Content:    fmt.Sprintf(format, args...),
		IsError:    true,
	}
}

func enabledToolNames(toolSchemas []model.Tool) map[string]struct{} {
	enabled := make(map[string]struct{}, len(toolSchemas))
	for _, tool := range toolSchemas {
		enabled[tool.Name] = struct{}{}
	}
	return enabled
}
