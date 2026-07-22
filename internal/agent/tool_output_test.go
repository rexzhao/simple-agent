package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestLimitToolResultOutputLeavesSmallContentUnchanged(t *testing.T) {
	want := model.ToolResult{Name: "read_file", Content: "small output"}
	if got := limitToolResultOutput(want); got != want {
		t.Fatalf("limitToolResultOutput() = %#v, want %#v", got, want)
	}
}

func TestLimitToolResultOutputTruncatesNonShellFromHead(t *testing.T) {
	content := strings.Repeat("界", defaultToolOutputMaxBytes)
	got := limitToolResultOutput(model.ToolResult{Name: "read_file", Content: content})

	if !utf8.ValidString(got.Content) {
		t.Fatal("truncated output is not valid UTF-8")
	}
	if !strings.HasPrefix(got.Content, "界") {
		t.Fatalf("truncated output does not keep the head: %q", got.Content[:100])
	}
	for _, want := range []string{"Tool output truncated", "showing first", "50.0KB", "150.0KB"} {
		if !strings.Contains(got.Content, want) {
			t.Fatalf("truncated output = %q, want contain %q", got.Content[len(got.Content)-200:], want)
		}
	}
}

func TestLimitToolResultOutputUsesPiLineLimit(t *testing.T) {
	lines := make([]string, defaultToolOutputMaxLines+5)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%04d", i+1)
	}
	got := limitToolResultOutput(model.ToolResult{Name: "grep_files", Content: strings.Join(lines, "\n")})

	if !strings.Contains(got.Content, "line-2000") || strings.Contains(got.Content, "line-2001") {
		t.Fatalf("truncated output did not keep exactly the first %d lines", defaultToolOutputMaxLines)
	}
	for _, want := range []string{"showing first 2000 of 2005 lines", "2000 line limit"} {
		if !strings.Contains(got.Content, want) {
			t.Fatalf("truncated output = %q, want contain %q", got.Content[len(got.Content)-200:], want)
		}
	}
}

func TestLimitToolResultOutputKeepsShellTail(t *testing.T) {
	lines := make([]string, defaultToolOutputMaxLines+5)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%04d", i+1)
	}
	got := limitToolResultOutput(model.ToolResult{Name: "shell", Content: strings.Join(lines, "\n")})

	if strings.Contains(got.Content, "line-0005") || !strings.Contains(got.Content, "line-2005") {
		t.Fatalf("truncated shell output did not keep the last %d lines", defaultToolOutputMaxLines)
	}
	if !strings.Contains(got.Content, "showing last 2000 of 2005 lines") {
		t.Fatalf("truncated shell output = %q, want tail truncation notice", got.Content[len(got.Content)-200:])
	}
}

func TestExecuteToolCallLimitsSuccessfulAndErrorOutputs(t *testing.T) {
	toolCall := model.ToolCall{ID: "call-1", Name: "custom", Arguments: `{}`}
	enabled := map[string]struct{}{"custom": {}}
	out := make(chan model.Event, 2)

	success := executeToolCall(context.Background(), staticToolExecutor{
		result: model.ToolResult{Content: strings.Repeat("x", defaultToolOutputMaxBytes+1)},
	}, enabled, toolCall, out)
	if !strings.Contains(success.Content, "Tool output truncated") {
		t.Fatalf("successful tool output was not limited: %d bytes", len(success.Content))
	}

	failure := executeToolCall(context.Background(), staticToolExecutor{
		err: errors.New(strings.Repeat("y", defaultToolOutputMaxBytes+1)),
	}, enabled, toolCall, out)
	if !failure.IsError || !strings.Contains(failure.Content, "Tool output truncated") {
		t.Fatalf("error tool output was not limited: %#v", failure)
	}
}

type staticToolExecutor struct {
	result model.ToolResult
	err    error
}

func (e staticToolExecutor) Execute(context.Context, string, map[string]any) (model.ToolResult, error) {
	return e.result, e.err
}
