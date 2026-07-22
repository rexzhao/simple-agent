package logging

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/model/httpstream"
)

type Attributes struct {
	Provider string
	Model    string
	Level    string
}

type Logger struct {
	path           string
	root           string
	sessionDir     string
	provider       string
	model          string
	level          string
	agentIteration int
	now            func() time.Time
	file           *os.File
	writer         *bufio.Writer
}

func Open(path string, attributes Attributes) (*Logger, error) {
	logger := &Logger{
		provider: attributes.Provider,
		model:    attributes.Model,
		level:    normalizedLevel(attributes.Level),
		now:      time.Now,
	}
	configuredPath := strings.TrimSpace(path)
	if configuredPath == "" {
		return logger, nil
	}

	logger.root = sessionRoot(configuredPath)
	logger.sessionDir = filepath.Join(logger.root, sessionID(logger.now))
	logger.path = filepath.Join(logger.sessionDir, "sai.jsonl")
	return logger, nil
}

func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

func (l *Logger) LogEvent(event model.Event) error {
	if l == nil || l.path == "" {
		return nil
	}
	if err := l.ensureOpen(); err != nil {
		return err
	}
	if started, ok := event.(model.AgentIterationStartedEvent); ok {
		l.agentIteration = started.Iteration
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

func (l *Logger) ensureOpen() error {
	if l.writer != nil {
		return nil
	}
	if err := os.MkdirAll(l.root, 0o755); err != nil {
		return fmt.Errorf("create JSONL log root %q: %w", l.root, err)
	}
	if err := os.Mkdir(l.sessionDir, 0o755); err != nil {
		return fmt.Errorf("create JSONL log session directory %q: %w", l.sessionDir, err)
	}
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("open JSONL log %q: %w", l.path, err)
	}
	l.file = file
	l.writer = bufio.NewWriter(file)
	return nil
}

func (l *Logger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}

	file := l.file
	writer := l.writer
	l.file = nil
	l.writer = nil

	var closeErr error
	if writer != nil {
		if err := writer.Flush(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("flush JSONL log %q: %w", l.path, err))
		}
	}
	if err := file.Close(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("close JSONL log %q: %w", l.path, err))
	}
	return closeErr
}

func (l *Logger) eventRecord(event model.Event) map[string]any {
	record := map[string]any{
		"time":     l.now().UTC().Format(time.RFC3339Nano),
		"level":    l.levelFor(event),
		"event":    string(event.Type()),
		"provider": l.provider,
		"model":    l.model,
	}
	if l.agentIteration > 0 {
		record["agent_iteration"] = l.agentIteration
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
	case model.SubagentCompletionEvent:
		record["job_id"] = event.JobID
		record["agent_id"] = event.AgentID
		if event.DisplayName != "" {
			record["display_name"] = event.DisplayName
		}
		if event.JobName != "" {
			record["job_name"] = event.JobName
		}
		record["status"] = event.Status
	case model.ErrorEvent:
		record["is_error"] = true
		record["message"] = safeErrorMessage(event)
		if detail := safeErrorDetail(event.Err); detail != "" {
			record["error"] = detail
		}
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

func sessionRoot(path string) string {
	path = filepath.Clean(path)
	if filepath.Ext(filepath.Base(path)) != "" {
		return filepath.Dir(path)
	}
	return path
}

func sessionID(now func() time.Time) string {
	return now().UTC().Format("20060102T150405.000000000Z") + "-" + randomHex(4)
}

func randomHex(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func safeErrorMessage(event model.ErrorEvent) string {
	if strings.TrimSpace(event.Message) != "" {
		return event.Message
	}
	return "error"
}

func safeErrorDetail(err error) string {
	if err == nil {
		return ""
	}
	var statusErr *httpstream.StatusError
	if errors.As(err, &statusErr) {
		status := strings.TrimSpace(statusErr.Status)
		if status == "" && statusErr.StatusCode != 0 {
			status = fmt.Sprintf("HTTP %d", statusErr.StatusCode)
		}
		if status == "" {
			status = "HTTP request failed"
		}
		if statusErr.Attempts > 1 {
			return fmt.Sprintf("%s after %d attempts", status, statusErr.Attempts)
		}
		return status
	}
	detail := strings.Join(strings.Fields(err.Error()), " ")
	const maxDetailRunes = 512
	runes := []rune(detail)
	if len(runes) > maxDetailRunes {
		detail = string(runes[:maxDetailRunes]) + "..."
	}
	return detail
}
