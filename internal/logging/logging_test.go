package logging

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
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
	for _, want := range []string{`"event":"error"`, `"level":"error"`, `"message":"request model"`} {
		if !strings.Contains(logText, want) {
			t.Fatalf("log = %q, want contain %q", logText, want)
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
