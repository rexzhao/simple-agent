package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/model"
)

func TestSplitRootArgsExtractsMailboxMCPForSessionResume(t *testing.T) {
	args, err := splitRootArgs([]string{"session", "resume", "sess-1", "--mailbox-mcp", "127.0.0.1:39123"})
	if err != nil {
		t.Fatalf("splitRootArgs() error = %v", err)
	}
	if args.mailboxMCP != "127.0.0.1:39123" {
		t.Fatalf("mailboxMCP = %q, want address", args.mailboxMCP)
	}
	want := []string{"resume", "sess-1"}
	if len(args.commandArgs) != len(want) {
		t.Fatalf("commandArgs = %#v, want %#v", args.commandArgs, want)
	}
	for i := range want {
		if args.commandArgs[i] != want[i] {
			t.Fatalf("commandArgs = %#v, want %#v", args.commandArgs, want)
		}
	}
}

func TestMailboxMCPInitializeAndToolsList(t *testing.T) {
	queue := newMailboxQueue()
	server := httptest.NewServer(&mailboxMCPHandler{queue: queue})
	defer server.Close()

	initialized := mailboxMCPCallMethod(t, server.URL+"/mcp", "initialize", map[string]any{
		"protocolVersion": mailboxMCPProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "dev"},
	})
	if initialized["protocolVersion"] != mailboxMCPProtocolVersion {
		t.Fatalf("protocolVersion = %#v, want %q", initialized["protocolVersion"], mailboxMCPProtocolVersion)
	}

	listed := mailboxMCPCallMethod(t, server.URL+"/mcp", "tools/list", map[string]any{})
	tools, ok := listed["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list result = %#v, want tools array", listed)
	}
	names := map[string]bool{}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("tool = %#v, want object", raw)
		}
		name, _ := tool["name"].(string)
		names[name] = true
	}
	for _, want := range []string{"mailbox_post", "mailbox_get", "mailbox_wait", "mailbox_cancel"} {
		if !names[want] {
			t.Fatalf("tools/list names = %#v, missing %q", names, want)
		}
	}
}

func TestMailboxMCPTaskResultOnlyContainsFinalOutput(t *testing.T) {
	queue := newMailboxQueue()
	server := httptest.NewServer(&mailboxMCPHandler{queue: queue})
	defer server.Close()

	posted := mailboxMCPCallTool(t, server.URL+"/mcp", "mailbox_post", map[string]any{"prompt": "hello"})
	taskID, _ := posted.StructuredContent["task_id"].(string)
	if taskID == "" {
		t.Fatalf("mailbox_post structuredContent = %#v, want task_id", posted.StructuredContent)
	}

	task, err := queue.dequeue(context.Background())
	if err != nil {
		t.Fatalf("dequeue() error = %v", err)
	}
	turnCtx, cancel := context.WithCancel(context.Background())
	if !queue.startTask(task, cancel) {
		t.Fatalf("startTask() = false")
	}
	if turnCtx.Err() != nil {
		t.Fatalf("turn context unexpectedly cancelled: %v", turnCtx.Err())
	}
	queue.completeTask(task, "final answer")
	cancel()

	waited := mailboxMCPCallTool(t, server.URL+"/mcp", "mailbox_wait", map[string]any{"task_id": taskID, "timeout_ms": 100})
	if waited.IsError {
		t.Fatalf("mailbox_wait IsError = true, content = %#v", waited.StructuredContent)
	}
	if waited.StructuredContent["status"] != mailboxTaskCompleted {
		t.Fatalf("status = %#v, want completed", waited.StructuredContent["status"])
	}
	if waited.StructuredContent["result"] != "final answer" {
		t.Fatalf("result = %#v, want final answer", waited.StructuredContent["result"])
	}
	for _, forbidden := range []string{"events", "items", "tool_results", "deltas"} {
		if _, ok := waited.StructuredContent[forbidden]; ok {
			t.Fatalf("structuredContent exposed %q: %#v", forbidden, waited.StructuredContent)
		}
	}
}

func TestMailboxTaskRunCompletesWithFullFinalAssistantOutput(t *testing.T) {
	finalAnswer := strings.Repeat("final answer body ", 400) + "FINAL-SUFFIX"
	service, sessionID := newMailboxExecutionService(t, cliMailboxTurnRunner{
		run: func(ctx context.Context, request execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
			if request.Content != "review mailbox result" {
				t.Fatalf("RunSessionTurn content = %q, want mailbox prompt", request.Content)
			}
			if err := request.Publisher.Publish(eventbus.AssistantReady{
				TurnID:  request.TurnID,
				Message: model.Message{Role: model.MessageRoleAssistant, Content: "checking the diff first"},
			}); err != nil {
				return execution.SessionTurnResult{}, err
			}
			if err := request.Publisher.Publish(eventbus.AssistantReady{
				TurnID:  request.TurnID,
				Message: model.Message{Role: model.MessageRoleAssistant, Content: finalAnswer},
			}); err != nil {
				return execution.SessionTurnResult{}, err
			}
			return execution.SessionTurnResult{Incremental: true}, nil
		},
	})
	queue := newMailboxQueue()
	server := httptest.NewServer(&mailboxMCPHandler{queue: queue})
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	runErr := make(chan error, 1)
	go func() {
		runErr <- runAttachREPLWithScanner(ctx, service, sessionID, bufio.NewScanner(strings.NewReader("")), &stdout, &stderr, "sai", attachREPLSources{mailbox: queue})
	}()

	posted := mailboxMCPCallTool(t, server.URL+"/mcp", "mailbox_post", map[string]any{"prompt": "review mailbox result"})
	taskID, _ := posted.StructuredContent["task_id"].(string)
	waited := mailboxMCPCallTool(t, server.URL+"/mcp", "mailbox_wait", map[string]any{"task_id": taskID, "timeout_ms": 5000})
	if waited.IsError {
		t.Fatalf("mailbox_wait IsError = true, content = %#v", waited.StructuredContent)
	}
	if waited.StructuredContent["status"] != mailboxTaskCompleted {
		t.Fatalf("status = %#v, want completed", waited.StructuredContent["status"])
	}
	if waited.StructuredContent["result"] != finalAnswer {
		t.Fatalf("result length = %d, want full final answer length %d", len(fmt.Sprint(waited.StructuredContent["result"])), len(finalAnswer))
	}
	if strings.Contains(fmt.Sprint(waited.StructuredContent["result"]), "checking the diff first") {
		t.Fatalf("result includes process text: %q", waited.StructuredContent["result"])
	}
	if got := stderr.String(); !strings.Contains(got, "sai: mailbox task "+taskID) || !strings.Contains(got, "user: review mailbox result") {
		t.Fatalf("stderr = %q, want mailbox task id and prompt", got)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("runAttachREPLWithScanner() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runAttachREPLWithScanner() did not stop after context cancel")
	}
}

func TestWriteMailboxTaskStartWritesPrompt(t *testing.T) {
	var stderr bytes.Buffer
	err := writeMailboxTaskStart(&stderr, "sai", &mailboxTask{ID: "task_000123"}, "first line\nsecond line")
	if err != nil {
		t.Fatalf("writeMailboxTaskStart() error = %v", err)
	}
	want := "\nsai: mailbox task task_000123\nuser:\nfirst line\nsecond line\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestMailboxMCPWaitTimeoutAndQueuedCancel(t *testing.T) {
	queue := newMailboxQueue()
	server := httptest.NewServer(&mailboxMCPHandler{queue: queue})
	defer server.Close()

	posted := mailboxMCPCallTool(t, server.URL+"/mcp", "mailbox_post", map[string]any{"prompt": "slow"})
	taskID, _ := posted.StructuredContent["task_id"].(string)

	waited := mailboxMCPCallTool(t, server.URL+"/mcp", "mailbox_wait", map[string]any{"task_id": taskID, "timeout_ms": 1})
	if waited.StructuredContent["status"] != mailboxTaskQueued {
		t.Fatalf("timeout status = %#v, want queued", waited.StructuredContent["status"])
	}
	if waited.StructuredContent["timed_out"] != true {
		t.Fatalf("timed_out = %#v, want true", waited.StructuredContent["timed_out"])
	}

	cancelled := mailboxMCPCallTool(t, server.URL+"/mcp", "mailbox_cancel", map[string]any{"task_id": taskID})
	if cancelled.StructuredContent["status"] != mailboxTaskCancelled {
		t.Fatalf("cancel status = %#v, want cancelled", cancelled.StructuredContent["status"])
	}
	cancelledAgain := mailboxMCPCallTool(t, server.URL+"/mcp", "mailbox_cancel", map[string]any{"task_id": taskID})
	if cancelledAgain.StructuredContent["status"] != mailboxTaskCancelled {
		t.Fatalf("second cancel status = %#v, want cancelled", cancelledAgain.StructuredContent["status"])
	}
}

func TestMailboxQueueCancelRunningTaskCancelsTurnContext(t *testing.T) {
	queue := newMailboxQueue()
	snapshot, err := queue.post("stop")
	if err != nil {
		t.Fatalf("post() error = %v", err)
	}
	task, err := queue.dequeue(context.Background())
	if err != nil {
		t.Fatalf("dequeue() error = %v", err)
	}
	turnCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !queue.startTask(task, cancel) {
		t.Fatalf("startTask() = false")
	}

	cancelled, ok := queue.cancel(snapshot.TaskID)
	if !ok {
		t.Fatalf("cancel() ok = false")
	}
	if cancelled.Status != mailboxTaskCancelled {
		t.Fatalf("cancelled status = %q, want cancelled", cancelled.Status)
	}
	select {
	case <-turnCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancel() did not cancel turn context")
	}
}

type cliMailboxTurnRunner struct {
	run func(context.Context, execution.SessionTurnRequest) (execution.SessionTurnResult, error)
}

func (r cliMailboxTurnRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (r cliMailboxTurnRunner) RunSessionTurn(ctx context.Context, request execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	if r.run == nil {
		return execution.SessionTurnResult{Incremental: true}, nil
	}
	return r.run(ctx, request)
}

func newMailboxExecutionService(t *testing.T, runner execution.SessionTurnRunner) (*execution.Service, string) {
	t.Helper()

	service, err := execution.NewServiceWithOptions(t.TempDir(), execution.ServiceOptions{TurnRunner: runner})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	projectRoot := t.TempDir()
	project, err := service.CreateProject(projectRoot, "Mailbox Test")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	saveToolResults := true
	session, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{
		CreatedCWD:      project.Project.Root,
		ConfigPath:      filepath.Join(project.Project.Root, ".agents", "sai.yaml"),
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "fake-model",
		SaveToolResults: &saveToolResults,
	})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	return service, session.ID
}

type mailboxMCPToolResponse struct {
	StructuredContent map[string]any `json:"structuredContent"`
	IsError           bool           `json:"isError,omitempty"`
}

func mailboxMCPCallTool(t *testing.T, endpoint, name string, arguments map[string]any) mailboxMCPToolResponse {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post %s status = %d", name, resp.StatusCode)
	}
	var decoded struct {
		Result mailboxMCPToolResponse `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded.Error != nil {
		t.Fatalf("tool %s JSON-RPC error = %d %s", name, decoded.Error.Code, decoded.Error.Message)
	}
	return decoded.Result
}

func mailboxMCPCallMethod(t *testing.T, endpoint, method string, params map[string]any) map[string]any {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post %s status = %d", method, resp.StatusCode)
	}
	var decoded struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded.Error != nil {
		t.Fatalf("method %s JSON-RPC error = %d %s", method, decoded.Error.Code, decoded.Error.Message)
	}
	return decoded.Result
}
