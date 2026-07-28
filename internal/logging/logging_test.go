package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/model/httpstream"
)

func TestOpenPrecomputesPathWithoutCreatingLogFiles(t *testing.T) {
	parent := t.TempDir()
	logRoot := filepath.Join(parent, "logs")
	logger, err := Open(filepath.Join(logRoot, "sai.jsonl"), Attributes{
		Provider: "fake",
		Model:    "model-default",
		Level:    "info",
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() {
		if err := logger.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	futurePath := logger.Path()
	if futurePath == "" {
		t.Fatal("Path() is empty, want precomputed session log path")
	}
	if filepath.Base(futurePath) != "sai.jsonl" {
		t.Fatalf("Path() = %q, want sai.jsonl file", futurePath)
	}
	assertPathUnder(t, futurePath, logRoot)
	assertPathDoesNotExist(t, logRoot)
	assertPathDoesNotExist(t, filepath.Dir(futurePath))
	assertPathDoesNotExist(t, futurePath)
}

func TestLogEventLazilyCreatesSessionLog(t *testing.T) {
	parent := t.TempDir()
	logRoot := filepath.Join(parent, "logs")
	logger, err := Open(filepath.Join(logRoot, "sai.jsonl"), Attributes{
		Provider: "fake",
		Model:    "model-default",
		Level:    "info",
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	futurePath := logger.Path()

	if err := logger.LogEvent(model.TextDeltaEvent{Text: "hidden response body"}); err != nil {
		t.Fatalf("LogEvent() error = %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if got := logger.Path(); got != futurePath {
		t.Fatalf("Path() after LogEvent = %q, want original future path %q", got, futurePath)
	}
	if info, err := os.Stat(logRoot); err != nil || !info.IsDir() {
		t.Fatalf("Stat(%q) = %v, %v; want directory", logRoot, info, err)
	}
	if info, err := os.Stat(filepath.Dir(futurePath)); err != nil || !info.IsDir() {
		t.Fatalf("Stat(%q) = %v, %v; want directory", filepath.Dir(futurePath), info, err)
	}
	data, err := os.ReadFile(futurePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", futurePath, err)
	}
	logText := string(data)
	for _, want := range []string{`"event":"text_delta"`, `"provider":"fake"`, `"model":"model-default"`} {
		if !strings.Contains(logText, want) {
			t.Fatalf("log = %q, want contain %q", logText, want)
		}
	}
	if strings.Contains(logText, "hidden response body") {
		t.Fatalf("log leaked event body: %s", logText)
	}
}

func TestRecordRequestBodyWritesExactOwnerOnlyCapture(t *testing.T) {
	logRoot := filepath.Join(t.TempDir(), "logs")
	logger, err := Open(filepath.Join(logRoot, "sai.jsonl"), Attributes{
		Provider: "codex",
		Model:    "gpt-test",
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	body := []byte(`{"model":"gpt-test","input":[{"role":"user","content":"diagnose"}]}`)
	if err := logger.RecordRequestBody("/responses", body); err != nil {
		t.Fatalf("RecordRequestBody() error = %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	path := filepath.Join(filepath.Dir(logger.Path()), "responses-request-000001.json")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(got) != string(body) {
		t.Fatalf("request capture = %q, want exact body %q", got, body)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err != nil {
			t.Fatalf("Stat(%q) error = %v", path, err)
		} else if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("request capture permissions = %v, want owner-only", info.Mode().Perm())
		}
	}
}

func TestConcurrentFirstEventAndRequestBodyShareLogger(t *testing.T) {
	logger, err := Open(filepath.Join(t.TempDir(), "logs", "sai.jsonl"), Attributes{
		Provider: "codex",
		Model:    "gpt-test",
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	go func() {
		ready.Done()
		<-start
		errs <- logger.LogEvent(model.AgentIterationStartedEvent{Iteration: 1})
	}()
	go func() {
		ready.Done()
		<-start
		errs <- logger.RecordRequestBody("/responses", []byte(`{"model":"gpt-test"}`))
	}()
	ready.Wait()
	close(start)

	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent logger operation error = %v", err)
		}
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if _, err := os.Stat(logger.Path()); err != nil {
		t.Fatalf("Stat(%q) error = %v", logger.Path(), err)
	}
	requestPath := filepath.Join(filepath.Dir(logger.Path()), "responses-request-000001.json")
	if _, err := os.Stat(requestPath); err != nil {
		t.Fatalf("Stat(%q) error = %v", requestPath, err)
	}
}

func TestLogEventAnnotatesRecordsWithAgentIteration(t *testing.T) {
	logRoot := filepath.Join(t.TempDir(), "logs")
	logger, err := Open(filepath.Join(logRoot, "sai.jsonl"), Attributes{Provider: "fake", Model: "model-default"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := logger.LogEvent(model.AgentIterationStartedEvent{Iteration: 3}); err != nil {
		t.Fatalf("LogEvent(iteration) error = %v", err)
	}
	if err := logger.LogEvent(model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "call-1", Name: "read_file"}}); err != nil {
		t.Fatalf("LogEvent(tool) error = %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	data, err := os.ReadFile(logger.Path())
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", logger.Path(), err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2", len(lines))
	}
	for _, line := range lines {
		if !strings.Contains(line, `"agent_iteration":3`) {
			t.Fatalf("log line = %q, want agent_iteration 3", line)
		}
	}
}

func TestLogEventRecordsCacheUsageMetadata(t *testing.T) {
	logger, err := Open(filepath.Join(t.TempDir(), "logs", "sai.jsonl"), Attributes{Provider: "fake", Model: "model-default"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := logger.LogEvent(model.UsageEvent{Usage: model.Usage{
		InputTokens: 10, OutputTokens: 5, TotalTokens: 25,
		CachedTokens: 8, CacheWriteTokens: 2, ReasoningTokens: 3,
	}}); err != nil {
		t.Fatalf("LogEvent() error = %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	data, err := os.ReadFile(logger.Path())
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", logger.Path(), err)
	}
	logText := string(data)
	for _, want := range []string{`"cached_tokens":8`, `"cache_write_tokens":2`, `"reasoning_tokens":3`} {
		if !strings.Contains(logText, want) {
			t.Fatalf("log = %q, want contain %q", logText, want)
		}
	}
}

func TestCloseIsIdempotentAndFlushesBufferedData(t *testing.T) {
	parent := t.TempDir()
	logRoot := filepath.Join(parent, "logs")
	logger, err := Open(filepath.Join(logRoot, "sai.jsonl"), Attributes{
		Provider: "fake",
		Model:    "model-default",
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	futurePath := logger.Path()

	if err := logger.LogEvent(model.TextDeltaEvent{Text: "hidden response body"}); err != nil {
		t.Fatalf("LogEvent() error = %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}

	data, err := os.ReadFile(futurePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", futurePath, err)
	}
	logText := string(data)
	if !strings.Contains(logText, `"event":"text_delta"`) {
		t.Fatalf("log = %q, want flushed text_delta record", logText)
	}
	if strings.Contains(logText, "hidden response body") {
		t.Fatalf("log leaked event body: %s", logText)
	}
}

func TestCloseStillClosesFileWhenFlushFails(t *testing.T) {
	parent := t.TempDir()
	logRoot := filepath.Join(parent, "logs")
	logger, err := Open(filepath.Join(logRoot, "sai.jsonl"), Attributes{
		Provider: "fake",
		Model:    "model-default",
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := logger.LogEvent(model.TextDeltaEvent{Text: "hidden response body"}); err != nil {
		t.Fatalf("LogEvent() error = %v", err)
	}
	if err := logger.file.Close(); err != nil {
		t.Fatalf("pre-close file error = %v", err)
	}

	err = logger.Close()
	if err == nil {
		t.Fatal("Close() error = nil, want flush and close errors")
	}
	got := err.Error()
	for _, want := range []string{"flush JSONL log", "close JSONL log"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Close() error = %q, want contain %q", got, want)
		}
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestCloseReturnsFileCloseError(t *testing.T) {
	parent := t.TempDir()
	logRoot := filepath.Join(parent, "logs")
	logger, err := Open(filepath.Join(logRoot, "sai.jsonl"), Attributes{
		Provider: "fake",
		Model:    "model-default",
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := logger.LogEvent(model.TextDeltaEvent{Text: "hidden response body"}); err != nil {
		t.Fatalf("LogEvent() error = %v", err)
	}
	if err := logger.writer.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if err := logger.file.Close(); err != nil {
		t.Fatalf("pre-close file error = %v", err)
	}

	err = logger.Close()
	if err == nil {
		t.Fatal("Close() error = nil, want file close error")
	}
	if got := err.Error(); !strings.Contains(got, "close JSONL log") {
		t.Fatalf("Close() error = %q, want close JSONL log context", got)
	}
}

func TestFirstErrorEventLazilyCreatesSessionLog(t *testing.T) {
	parent := t.TempDir()
	logRoot := filepath.Join(parent, "logs")
	logger, err := Open(filepath.Join(logRoot, "sai.jsonl"), Attributes{
		Provider: "fake",
		Model:    "model-default",
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	futurePath := logger.Path()

	if err := logger.LogEvent(model.ErrorEvent{Err: errors.New("boom"), Message: "request model"}); err != nil {
		t.Fatalf("LogEvent() error = %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	data, err := os.ReadFile(futurePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", futurePath, err)
	}
	logText := string(data)
	for _, want := range []string{`"event":"error"`, `"level":"error"`, `"message":"request model"`, `"error":"boom"`} {
		if !strings.Contains(logText, want) {
			t.Fatalf("log = %q, want contain %q", logText, want)
		}
	}
}

func TestErrorEventHTTPStatusErrorLogsStatusWithoutResponseBody(t *testing.T) {
	parent := t.TempDir()
	logRoot := filepath.Join(parent, "logs")
	logger, err := Open(filepath.Join(logRoot, "sai.jsonl"), Attributes{
		Provider: "fake",
		Model:    "model-default",
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	futurePath := logger.Path()

	statusErr := &httpstream.StatusError{
		StatusCode: 429,
		Status:     "429 Too Many Requests",
		Body:       "prompt secret body with Authorization: Bearer direct-secret-value",
		Attempts:   2,
	}
	if err := logger.LogEvent(model.ErrorEvent{Err: fmt.Errorf("request failed: %w", statusErr), Message: "request model"}); err != nil {
		t.Fatalf("LogEvent() error = %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	data, err := os.ReadFile(futurePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", futurePath, err)
	}
	logText := string(data)
	if !strings.Contains(logText, `"error":"429 Too Many Requests after 2 attempts"`) {
		t.Fatalf("log = %q, want status-only error detail", logText)
	}
	for _, leaked := range []string{"prompt secret body", "direct-secret-value", "Bearer direct-secret-value"} {
		if strings.Contains(logText, leaked) {
			t.Fatalf("log = %q, leaked %q", logText, leaked)
		}
	}
}

func TestOpenWithEmptyPathDisablesLogging(t *testing.T) {
	logger, err := Open(" \t ", Attributes{Provider: "fake", Model: "model-default"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got := logger.Path(); got != "" {
		t.Fatalf("Path() = %q, want empty", got)
	}
	if err := logger.LogEvent(model.TextDeltaEvent{Text: "ignored"}); err != nil {
		t.Fatalf("LogEvent() error = %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func assertPathUnder(t *testing.T, path, root string) {
	t.Helper()

	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("Rel(%q, %q) error = %v", root, path, err)
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." || filepath.IsAbs(rel) {
		t.Fatalf("path %q is not under root %q", path, root)
	}
}

func assertPathDoesNotExist(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%q exists, want absent", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
}
