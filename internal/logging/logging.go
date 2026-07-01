package logging

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

type Attributes struct {
	Provider string
	Model    string
	Level    string
}

type Logger struct {
	path     string
	provider string
	model    string
	level    string
	now      func() time.Time
	file     *os.File
	writer   *bufio.Writer
}

func Open(path string, attributes Attributes) (*Logger, error) {
	logger := &Logger{
		path:     strings.TrimSpace(path),
		provider: attributes.Provider,
		model:    attributes.Model,
		level:    normalizedLevel(attributes.Level),
		now:      time.Now,
	}
	if logger.path == "" {
		return logger, nil
	}

	if dir := filepath.Dir(logger.path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create JSONL log directory %q: %w", dir, err)
		}
	}

	file, err := os.OpenFile(logger.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open JSONL log %q: %w", logger.path, err)
	}
	logger.file = file
	logger.writer = bufio.NewWriter(file)
	return logger, nil
}

func (l *Logger) LogEvent(event model.Event) error {
	if l == nil || l.writer == nil {
		return nil
	}

	record := l.eventRecord(event)
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode JSONL log event %q: %w", event.Type(), err)
	}
	if _, err := l.writer.Write(data); err != nil {
		return fmt.Errorf("write JSONL log %q: %w", l.path, err)
	}
	if err := l.writer.WriteByte('\n'); err != nil {
		return fmt.Errorf("write JSONL log %q: %w", l.path, err)
	}
	return nil
}

func (l *Logger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}

	if err := l.writer.Flush(); err != nil {
		_ = l.file.Close()
		return fmt.Errorf("flush JSONL log %q: %w", l.path, err)
	}
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("close JSONL log %q: %w", l.path, err)
	}
	return nil
}

func (l *Logger) eventRecord(event model.Event) map[string]any {
	record := map[string]any{
		"time":     l.now().UTC().Format(time.RFC3339Nano),
		"level":    l.levelFor(event),
		"event":    string(event.Type()),
		"provider": l.provider,
		"model":    l.model,
	}

	switch event := event.(type) {
	case model.ToolCallDeltaEvent:
		record["tool_index"] = event.Index
		if event.ID != "" {
			record["tool_call_id"] = event.ID
		}
		if event.Name != "" {
			record["tool_name"] = event.Name
		}
	case model.ToolCallDoneEvent:
		if event.ToolCall.ID != "" {
			record["tool_call_id"] = event.ToolCall.ID
		}
		if event.ToolCall.Name != "" {
			record["tool_name"] = event.ToolCall.Name
		}
	case model.ToolResultEvent:
		if event.Result.ToolCallID != "" {
			record["tool_call_id"] = event.Result.ToolCallID
		}
		if event.Result.Name != "" {
			record["tool_name"] = event.Result.Name
		}
		record["is_error"] = event.Result.IsError
	case model.UsageEvent:
		record["usage"] = map[string]int{
			"input_tokens":  event.Usage.InputTokens,
			"output_tokens": event.Usage.OutputTokens,
			"total_tokens":  event.Usage.TotalTokens,
		}
	case model.ErrorEvent:
		record["is_error"] = true
		record["message"] = safeErrorMessage(event)
	}

	return record
}

func (l *Logger) levelFor(event model.Event) string {
	if event.Type() == model.EventTypeError {
		return "error"
	}
	return l.level
}

func normalizedLevel(level string) string {
	level = strings.TrimSpace(strings.ToLower(level))
	if level == "" {
		return "info"
	}
	return level
}

func safeErrorMessage(event model.ErrorEvent) string {
	if strings.TrimSpace(event.Message) != "" {
		return event.Message
	}
	return "error"
}
