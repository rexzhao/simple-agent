package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestDefinitions(t *testing.T) {
	definitions := Definitions()
	gotNames := make([]string, 0, len(definitions))
	seen := make(map[string]bool)
	for _, definition := range definitions {
		gotNames = append(gotNames, definition.Name)
		if definition.Description == "" {
			t.Fatalf("definition %q description is empty", definition.Name)
		}
		if definition.InputSchema["type"] != "object" {
			t.Fatalf("definition %q schema type = %#v, want object", definition.Name, definition.InputSchema["type"])
		}
		if seen[definition.Name] {
			t.Fatalf("duplicate tool definition %q", definition.Name)
		}
		seen[definition.Name] = true
	}

	wantNames := []string{
		ToolSubagentStart,
		ToolSubagentSend,
		ToolSubagentStatus,
		ToolSubagentWait,
		ToolSubagentCancel,
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("Definitions() names = %#v, want %#v", gotNames, wantNames)
	}

}

func TestStartStatusAndWaitSuccess(t *testing.T) {
	started := make(chan RunRequest, 1)
	finish := make(chan struct{})
	runner := runnerFunc(func(ctx context.Context, request RunRequest, inbox <-chan Message) (RunResult, error) {
		started <- request
		<-finish
		return RunResult{Output: "review complete"}, nil
	})
	manager := newTestManager(t, runner)

	startResult := execute(t, manager, ToolSubagentStart, map[string]any{
		"agent_id":     "reviewer",
		"prompt":       "review this change",
		"display_name": "UI Review",
		"job_name":     "review-1",
	})
	start := decodeSnapshot(t, startResult)
	if start.JobID == "" {
		t.Fatal("start job_id is empty")
	}
	if start.Status != StatusRunning {
		t.Fatalf("start status = %q, want %q", start.Status, StatusRunning)
	}
	if start.AgentID != "reviewer" || start.DisplayName != "UI Review" || start.JobName != "review-1" {
		t.Fatalf("start metadata = %#v", start)
	}

	request := receiveRunRequest(t, started)
	if request.JobID != start.JobID {
		t.Fatalf("request JobID = %q, want %q", request.JobID, start.JobID)
	}
	if request.AgentID != "reviewer" {
		t.Fatalf("request AgentID = %q, want reviewer", request.AgentID)
	}
	if request.ConfigPath != "reviewer.yaml" {
		t.Fatalf("request ConfigPath = %q, want reviewer.yaml", request.ConfigPath)
	}
	if request.Prompt != "review this change" {
		t.Fatalf("request Prompt = %q, want initial prompt", request.Prompt)
	}
	if request.DisplayName != "UI Review" || request.JobName != "review-1" {
		t.Fatalf("request metadata = %#v", request)
	}

	statusResult := execute(t, manager, ToolSubagentStatus, map[string]any{"job_id": start.JobID})
	status := decodeSnapshot(t, statusResult)
	if status.Status != StatusRunning {
		t.Fatalf("status = %q, want running", status.Status)
	}
	if status.DisplayName != "UI Review" || status.JobName != "review-1" {
		t.Fatalf("status metadata = %#v", status)
	}

	close(finish)
	waitResult := execute(t, manager, ToolSubagentWait, map[string]any{
		"job_id":     start.JobID,
		"timeout_ms": 1000,
	})
	waited := decodeSnapshot(t, waitResult)
	if waited.Status != StatusCompleted {
		t.Fatalf("wait status = %q, want completed", waited.Status)
	}
	if waited.Output != "review complete" {
		t.Fatalf("wait output = %q, want review complete", waited.Output)
	}
}

func TestSendDeliversMessageToRunner(t *testing.T) {
	received := make(chan Message, 1)
	release := make(chan struct{})
	runner := runnerFunc(func(ctx context.Context, request RunRequest, inbox <-chan Message) (RunResult, error) {
		select {
		case message := <-inbox:
			received <- message
			<-release
			return RunResult{Output: "done"}, nil
		case <-ctx.Done():
			return RunResult{}, ctx.Err()
		}
	})
	manager := newTestManager(t, runner)
	jobID := startJob(t, manager, "reviewer")

	sendResult := execute(t, manager, ToolSubagentSend, map[string]any{
		"job_id":  jobID,
		"message": "please inspect tests",
	})
	sent := decodeSnapshot(t, sendResult)
	if sent.Status != StatusRunning {
		t.Fatalf("send status = %q, want running", sent.Status)
	}
	if !sent.MessageQueued {
		t.Fatal("send message_queued = false, want true")
	}

	select {
	case message := <-received:
		if message.Content != "please inspect tests" {
			t.Fatalf("received message = %q, want follow-up", message.Content)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not receive message")
	}

	close(release)
	waitResult := execute(t, manager, ToolSubagentWait, map[string]any{
		"job_id":     jobID,
		"timeout_ms": 1000,
	})
	waited := decodeSnapshot(t, waitResult)
	if waited.Status != StatusCompleted {
		t.Fatalf("wait status = %q, want completed", waited.Status)
	}
}

func TestCancelPropagatesToRunnerContext(t *testing.T) {
	ctxErr := make(chan error, 1)
	runner := runnerFunc(func(ctx context.Context, request RunRequest, inbox <-chan Message) (RunResult, error) {
		<-ctx.Done()
		ctxErr <- ctx.Err()
		return RunResult{}, ctx.Err()
	})
	manager := newTestManager(t, runner)
	jobID := startJob(t, manager, "reviewer")

	cancelResult := execute(t, manager, ToolSubagentCancel, map[string]any{"job_id": jobID})
	canceled := decodeSnapshot(t, cancelResult)
	if canceled.Status != StatusCanceled {
		t.Fatalf("cancel status = %q, want canceled", canceled.Status)
	}
	if canceled.Error != "canceled" {
		t.Fatalf("cancel error = %q, want canceled", canceled.Error)
	}

	select {
	case err := <-ctxErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runner ctx error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner context was not canceled")
	}

	statusResult := execute(t, manager, ToolSubagentStatus, map[string]any{"job_id": jobID})
	status := decodeSnapshot(t, statusResult)
	if status.Status != StatusCanceled {
		t.Fatalf("status after cancel = %q, want canceled", status.Status)
	}
}

func TestWaitTimeout(t *testing.T) {
	runner := runnerFunc(func(ctx context.Context, request RunRequest, inbox <-chan Message) (RunResult, error) {
		<-ctx.Done()
		return RunResult{}, ctx.Err()
	})
	manager := newTestManager(t, runner)
	jobID := startJob(t, manager, "reviewer")

	waitResult := execute(t, manager, ToolSubagentWait, map[string]any{
		"job_id":     jobID,
		"timeout_ms": 5,
	})
	waited := decodeSnapshot(t, waitResult)
	if waited.Status != StatusRunning {
		t.Fatalf("wait timeout status = %q, want running", waited.Status)
	}
	if !waited.TimedOut {
		t.Fatal("wait timed_out = false, want true")
	}

	execute(t, manager, ToolSubagentCancel, map[string]any{"job_id": jobID})
}

func TestRunnerErrorBecomesFailedStatus(t *testing.T) {
	runner := runnerFunc(func(ctx context.Context, request RunRequest, inbox <-chan Message) (RunResult, error) {
		return RunResult{}, errors.New("child failed")
	})
	manager := newTestManager(t, runner)
	jobID := startJob(t, manager, "reviewer")

	waitResult := execute(t, manager, ToolSubagentWait, map[string]any{
		"job_id":     jobID,
		"timeout_ms": 1000,
	})
	waited := decodeSnapshot(t, waitResult)
	if waited.Status != StatusFailed {
		t.Fatalf("wait status = %q, want failed", waited.Status)
	}
	if waited.Error != "child failed" {
		t.Fatalf("wait error = %q, want child failed", waited.Error)
	}
}

func TestUnknownAgentAndJobErrors(t *testing.T) {
	manager := newTestManager(t, runnerFunc(func(ctx context.Context, request RunRequest, inbox <-chan Message) (RunResult, error) {
		return RunResult{Output: "unused"}, nil
	}))

	startResult := execute(t, manager, ToolSubagentStart, map[string]any{
		"agent_id":     "missing",
		"display_name": "reviewer",
	})
	assertToolError(t, startResult, `unknown subagent "missing"`)

	for _, tc := range []struct {
		name      string
		tool      string
		arguments map[string]any
	}{
		{name: "send", tool: ToolSubagentSend, arguments: map[string]any{"job_id": "missing", "message": "hi"}},
		{name: "status", tool: ToolSubagentStatus, arguments: map[string]any{"job_id": "missing"}},
		{name: "wait", tool: ToolSubagentWait, arguments: map[string]any{"job_id": "missing", "timeout_ms": 1}},
		{name: "cancel", tool: ToolSubagentCancel, arguments: map[string]any{"job_id": "missing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := execute(t, manager, tc.tool, tc.arguments)
			assertToolError(t, result, `unknown subagent job "missing"`)
		})
	}
}

func TestSendRejectsTerminalJobs(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		manager := newTestManager(t, runnerFunc(func(ctx context.Context, request RunRequest, inbox <-chan Message) (RunResult, error) {
			return RunResult{Output: "done"}, nil
		}))
		jobID := startJob(t, manager, "reviewer")
		execute(t, manager, ToolSubagentWait, map[string]any{"job_id": jobID, "timeout_ms": 1000})

		result := execute(t, manager, ToolSubagentSend, map[string]any{"job_id": jobID, "message": "late"})
		assertToolError(t, result, "already completed")
	})

	t.Run("failed", func(t *testing.T) {
		manager := newTestManager(t, runnerFunc(func(ctx context.Context, request RunRequest, inbox <-chan Message) (RunResult, error) {
			return RunResult{}, errors.New("failed")
		}))
		jobID := startJob(t, manager, "reviewer")
		execute(t, manager, ToolSubagentWait, map[string]any{"job_id": jobID, "timeout_ms": 1000})

		result := execute(t, manager, ToolSubagentSend, map[string]any{"job_id": jobID, "message": "late"})
		assertToolError(t, result, "already failed")
	})

	t.Run("canceled", func(t *testing.T) {
		manager := newTestManager(t, runnerFunc(func(ctx context.Context, request RunRequest, inbox <-chan Message) (RunResult, error) {
			<-ctx.Done()
			return RunResult{}, ctx.Err()
		}))
		jobID := startJob(t, manager, "reviewer")
		execute(t, manager, ToolSubagentCancel, map[string]any{"job_id": jobID})

		result := execute(t, manager, ToolSubagentSend, map[string]any{"job_id": jobID, "message": "late"})
		assertToolError(t, result, "already canceled")
	})
}

func TestMaxJobs(t *testing.T) {
	runner := runnerFunc(func(ctx context.Context, request RunRequest, inbox <-chan Message) (RunResult, error) {
		<-ctx.Done()
		return RunResult{}, ctx.Err()
	})
	manager, err := NewManager(map[string]string{"reviewer": "reviewer.yaml"}, runner, WithMaxJobs(1))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	first := execute(t, manager, ToolSubagentStart, map[string]any{"agent_id": "reviewer"})
	firstSnapshot := decodeSnapshot(t, first)
	second := execute(t, manager, ToolSubagentStart, map[string]any{"agent_id": "reviewer"})
	assertToolError(t, second, "maximum subagent jobs reached")

	execute(t, manager, ToolSubagentCancel, map[string]any{"job_id": firstSnapshot.JobID})
}

func TestDisplayNameDoesNotSelectConfig(t *testing.T) {
	started := make(chan RunRequest, 1)
	runner := runnerFunc(func(ctx context.Context, request RunRequest, inbox <-chan Message) (RunResult, error) {
		started <- request
		return RunResult{Output: "done"}, nil
	})
	manager, err := NewManager(map[string]string{
		"reviewer":   "reviewer.yaml",
		"researcher": "researcher.yaml",
	}, runner)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	startResult := execute(t, manager, ToolSubagentStart, map[string]any{
		"agent_id":     "reviewer",
		"display_name": "researcher",
	})
	start := decodeSnapshot(t, startResult)
	if start.AgentID != "reviewer" || start.DisplayName != "researcher" {
		t.Fatalf("start metadata = %#v", start)
	}

	request := receiveRunRequest(t, started)
	if request.AgentID != "reviewer" {
		t.Fatalf("request AgentID = %q, want reviewer", request.AgentID)
	}
	if request.ConfigPath != "reviewer.yaml" {
		t.Fatalf("request ConfigPath = %q, want reviewer.yaml", request.ConfigPath)
	}
}

func newTestManager(t *testing.T, runner Runner) *Manager {
	t.Helper()
	manager, err := NewManager(map[string]string{"reviewer": "reviewer.yaml"}, runner)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	return manager
}

func startJob(t *testing.T, manager *Manager, agentID string) string {
	t.Helper()
	result := execute(t, manager, ToolSubagentStart, map[string]any{"agent_id": agentID})
	snapshot := decodeSnapshot(t, result)
	if snapshot.JobID == "" {
		t.Fatal("job_id is empty")
	}
	return snapshot.JobID
}

func execute(t *testing.T, manager *Manager, tool string, arguments map[string]any) model.ToolResult {
	t.Helper()
	result, err := manager.Execute(context.Background(), tool, arguments)
	if err != nil {
		t.Fatalf("Execute(%q) error = %v", tool, err)
	}
	if result.Name != tool {
		t.Fatalf("Execute(%q) result name = %q", tool, result.Name)
	}
	return result
}

func decodeSnapshot(t *testing.T, result model.ToolResult) JobSnapshot {
	t.Helper()
	if result.IsError {
		t.Fatalf("result IsError = true, content = %s", result.Content)
	}
	var snapshot JobSnapshot
	if err := json.Unmarshal([]byte(result.Content), &snapshot); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", result.Content, err)
	}
	if !snapshot.OK {
		t.Fatalf("snapshot OK = false, content = %s", result.Content)
	}
	return snapshot
}

func assertToolError(t *testing.T, result model.ToolResult, wantSubstring string) {
	t.Helper()
	if !result.IsError {
		t.Fatalf("result IsError = false, content = %s", result.Content)
	}
	var payload errorPayload
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", result.Content, err)
	}
	if payload.OK {
		t.Fatalf("payload OK = true, content = %s", result.Content)
	}
	if !strings.Contains(payload.Error, wantSubstring) {
		t.Fatalf("payload error = %q, want substring %q", payload.Error, wantSubstring)
	}
}

func receiveRunRequest(t *testing.T, ch <-chan RunRequest) RunRequest {
	t.Helper()
	select {
	case request := <-ch:
		return request
	case <-time.After(time.Second):
		t.Fatal("runner did not receive request")
		return RunRequest{}
	}
}

type runnerFunc func(context.Context, RunRequest, <-chan Message) (RunResult, error)

func (f runnerFunc) Run(ctx context.Context, request RunRequest, inbox <-chan Message) (RunResult, error) {
	return f(ctx, request, inbox)
}
