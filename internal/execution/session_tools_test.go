package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestSessionToolDefinitionsAndExplicitSelection(t *testing.T) {
	definitions := sessionToolDefinitions()
	if len(definitions) != 8 {
		t.Fatalf("len(sessionToolDefinitions()) = %d, want 8", len(definitions))
	}
	gotNames := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		gotNames = append(gotNames, definition.Name)
		if !IsSessionTool(definition.Name) {
			t.Fatalf("IsSessionTool(%q) = false", definition.Name)
		}
		if definition.Description == "" || definition.InputSchema["type"] != "object" {
			t.Fatalf("definition %q is incomplete: %#v", definition.Name, definition)
		}
		if definition.InputSchema["additionalProperties"] != false {
			t.Fatalf("definition %q permits additional properties", definition.Name)
		}
	}
	wantNames := []string{
		ToolSessionModels, ToolSessionStart, ToolSessionSearch, ToolSessionGet,
		ToolSessionHistory, ToolSessionSend, ToolSessionWait, ToolSessionStop,
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("session tool names = %#v, want %#v", gotNames, wantNames)
	}

	registry, builtinSchemas, err := enabledToolsForRun(t.TempDir(), []string{ToolSessionStart, "read_file", ToolSessionGet})
	if err != nil {
		t.Fatalf("enabledToolsForRun() error = %v", err)
	}
	if registry == nil || len(builtinSchemas) != 1 || builtinSchemas[0].Name != "read_file" {
		t.Fatalf("built-in selection = registry %p schemas %#v", registry, builtinSchemas)
	}
	sessionSchemas := enabledSessionToolSchemas([]string{"read_file", ToolSessionGet, ToolSessionStart})
	if len(sessionSchemas) != 2 || sessionSchemas[0].Name != ToolSessionGet || sessionSchemas[1].Name != ToolSessionStart {
		t.Fatalf("session schema selection = %#v", sessionSchemas)
	}
	childTools := enabledToolsForAgentChild([]string{
		"read_file", ToolSessionStart, ToolSessionSearch, ToolSessionSend, ToolSessionWait,
	})
	wantChildTools := []string{"read_file", ToolSessionSearch, ToolSessionWait}
	if !reflect.DeepEqual(childTools, wantChildTools) {
		t.Fatalf("enabledToolsForAgentChild() = %#v, want %#v", childTools, wantChildTools)
	}

	dispatch := runToolExecutor{
		sessionTools:        &sessionToolExecutor{},
		enabledSessionTools: enabledSessionToolSet([]string{ToolSessionModels}),
	}
	if _, err := dispatch.Execute(context.Background(), ToolSessionStop, map[string]any{}); err == nil || err.Error() != `session tool "session_stop" is not enabled for this run` {
		t.Fatalf("disabled session tool dispatch error = %v", err)
	}
	allowed, err := dispatch.Execute(context.Background(), ToolSessionModels, map[string]any{})
	if err != nil {
		t.Fatalf("enabled session tool dispatch error = %v", err)
	}
	assertSessionToolErrorCode(t, allowed, "service_unavailable")
}

func TestSessionToolsStartInspectQueueAndWait(t *testing.T) {
	home := t.TempDir()
	releaseFirst := make(chan struct{})
	firstStarted := make(chan SessionTurnRequest, 1)
	var (
		mu       sync.Mutex
		contents []string
		calls    int
	)
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			mu.Lock()
			calls++
			call := calls
			contents = append(contents, request.Content)
			mu.Unlock()
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "persisted: "+request.Content)); err != nil {
				return SessionTurnResult{}, err
			}
			if call == 1 {
				firstStarted <- request
				select {
				case <-releaseFirst:
				case <-ctx.Done():
					return SessionTurnResult{}, ctx.Err()
				}
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, err := NewServiceWithOptions(home, ServiceOptions{TurnRunner: runner})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	projectRoot := mkdirProjectRoot(t, "session-tools-project")
	project, err := service.CreateProject(projectRoot, "Session Tools")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	showReasoning := true
	saveToolResults := true
	parent, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{
		DisplayName:     "Parent",
		CreatedCWD:      projectRoot,
		ConfigPath:      filepath.Join(home, "sai.yaml"),
		Provider:        "provider-a",
		ModelProfile:    "model-a",
		ModelID:         "model-id-a",
		ReasoningLevel:  "high",
		ModelParameters: map[string]any{"temperature": 0.25, "reasoning_effort": "high"},
		EnabledTools:    []string{ToolSessionStart, ToolSessionGet, ToolSessionSend, ToolSessionWait},
		EnabledMCP:      []string{"mcp-a"},
		EnabledSkills:   []string{"skill-a"},
		ShowReasoning:   &showReasoning,
		Context:         &contextwindow.Metadata{ContextWindow: 64000, ContextWindowSource: "configured", WarningThresholdPercent: 80, LastTotalTokens: 999},
		SaveToolResults: &saveToolResults,
	})
	if err != nil {
		t.Fatalf("CreateSession(parent) error = %v", err)
	}
	coordinator := NewSessionRunCoordinator(context.Background(), service, SessionRunCoordinatorOptions{MaxConcurrentRuns: 4})
	service.SetSessionRunCoordinator(coordinator)
	defer service.ClearSessionRunCoordinator(coordinator)
	defer coordinator.Close()
	executor := &sessionToolExecutor{service: service, coordinator: coordinator, caller: parent}

	startResult := executeSessionTool(t, executor, ToolSessionStart, map[string]any{
		"name": "Research child", "prompt": "first task",
	})
	startPayload := decodeSessionToolPayload(t, startResult)
	childID := requiredPayloadString(t, startPayload, "session_id")
	if requiredPayloadString(t, startPayload, "name") != "Research child" {
		t.Fatalf("session_start payload = %#v", startPayload)
	}

	var firstRequest SessionTurnRequest
	select {
	case firstRequest = <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("child first turn did not start")
	}
	if firstRequest.Session.ID != childID || firstRequest.SessionService != service || firstRequest.RunCoordinator != coordinator {
		t.Fatalf("child request dependencies = session %q service %p coordinator %p", firstRequest.Session.ID, firstRequest.SessionService, firstRequest.RunCoordinator)
	}

	child, err := service.GetSession(childID)
	if err != nil {
		t.Fatalf("GetSession(child) error = %v", err)
	}
	if child.ParentSessionID != parent.ID || child.RootSessionID != parent.ID || child.SpawnDepth != 1 || child.CreatedBy != "agent" {
		t.Fatalf("child lineage = parent %q root %q depth %d created_by %q", child.ParentSessionID, child.RootSessionID, child.SpawnDepth, child.CreatedBy)
	}
	if child.Provider != parent.Provider || child.ModelProfile != parent.ModelProfile || child.ModelID != parent.ModelID || canonicalTestJSON(t, child.ModelParameters) != canonicalTestJSON(t, parent.ModelParameters) {
		t.Fatalf("child model snapshot = %#v, want parent %#v", child, parent)
	}
	if want := []string{ToolSessionGet, ToolSessionWait}; !reflect.DeepEqual(child.EnabledTools, want) {
		t.Fatalf("child enabled tools = %#v, want leaf-worker tools %#v", child.EnabledTools, want)
	}
	if child.Context.LastTotalTokens != 0 || child.Context.ContextWindow != parent.Context.ContextWindow {
		t.Fatalf("child context = %#v, want fresh usage with inherited window", child.Context)
	}
	searchResult := executeSessionTool(t, executor, ToolSessionSearch, map[string]any{"name_regex": "^Research child$"})
	searchPayload := decodeSessionToolPayload(t, searchResult)
	matches, ok := searchPayload["matches"].([]any)
	if !ok || len(matches) != 1 || matches[0].(map[string]any)["id"] != childID {
		t.Fatalf("session_search payload = %#v", searchPayload)
	}

	// The assistant item was published durably before the fake runner blocked.
	var getPayload map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for {
		getResult := executeSessionTool(t, executor, ToolSessionGet, map[string]any{"session_id": childID})
		getPayload = decodeSessionToolPayload(t, getResult)
		inspection := payloadMap(t, getPayload, "inspection")
		if output, ok := inspection["output"].(map[string]any); ok && output["content"] == "persisted: first task" {
			if output["kind"] != "intermediate" || output["complete"] != false {
				t.Fatalf("running output = %#v", output)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session_get never observed persisted intermediate output: %#v", getPayload)
		}
		time.Sleep(10 * time.Millisecond)
	}

	queueResult := executeSessionTool(t, executor, ToolSessionSend, map[string]any{
		"session_id": childID, "mode": "queue", "message": "second task",
	})
	queuePayload := decodeSessionToolPayload(t, queueResult)
	if queuePayload["delivery"] != "queued" {
		t.Fatalf("session_send queue payload = %#v", queuePayload)
	}

	timedResult := executeSessionTool(t, executor, ToolSessionWait, map[string]any{
		"session_id": childID, "timeout_ms": 0,
	})
	timedPayload := decodeSessionToolPayload(t, timedResult)
	if timedPayload["completed"] != false || timedPayload["timed_out"] != true {
		t.Fatalf("session_wait(timeout) payload = %#v", timedPayload)
	}

	close(releaseFirst)
	waitResult := executeSessionTool(t, executor, ToolSessionWait, map[string]any{
		"session_id": childID, "timeout_ms": 5000,
	})
	waitPayload := decodeSessionToolPayload(t, waitResult)
	if waitPayload["completed"] != true || waitPayload["timed_out"] != false {
		t.Fatalf("session_wait(completed) payload = %#v", waitPayload)
	}
	inspection := payloadMap(t, waitPayload, "inspection")
	output := payloadMap(t, inspection, "output")
	if output["content"] != "persisted: second task" || output["kind"] != "final" || output["complete"] != true {
		t.Fatalf("completed output = %#v", output)
	}
	mu.Lock()
	gotContents := append([]string(nil), contents...)
	mu.Unlock()
	if !reflect.DeepEqual(gotContents, []string{"first task", "second task"}) {
		t.Fatalf("runner contents = %#v", gotContents)
	}
}

func TestSessionToolsStrictSteerStopAndProjectScope(t *testing.T) {
	home := t.TempDir()
	started := make(chan string, 1)
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			started <- request.Session.ID
			<-ctx.Done()
			return SessionTurnResult{}, ctx.Err()
		},
	}
	service, parent, child, otherProjectSession := newSessionToolTestSessions(t, home, runner)
	coordinator := NewSessionRunCoordinator(context.Background(), service, SessionRunCoordinatorOptions{})
	service.SetSessionRunCoordinator(coordinator)
	defer service.ClearSessionRunCoordinator(coordinator)
	defer coordinator.Close()
	executor := &sessionToolExecutor{service: service, coordinator: coordinator, caller: parent}

	steerResult := executeSessionTool(t, executor, ToolSessionSend, map[string]any{
		"session_id": child.ID, "mode": "steer", "message": "too late",
	})
	assertSessionToolErrorCode(t, steerResult, "session_not_steerable")

	run, err := coordinator.Start(child.ID, SessionMessageInput{Content: "work"}, nil)
	if err != nil {
		t.Fatalf("coordinator.Start() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("target run did not start")
	}
	steered := executeSessionTool(t, executor, ToolSessionSend, map[string]any{
		"session_id": child.ID, "mode": "steer", "message": "active guidance",
	})
	steeredPayload := decodeSessionToolPayload(t, steered)
	if steeredPayload["delivery"] != "steered" || steeredPayload["run_id"] != run.ID() {
		t.Fatalf("active steer payload = %#v", steeredPayload)
	}

	stopResult := executeSessionTool(t, executor, ToolSessionStop, map[string]any{"session_id": child.ID})
	stopPayload := decodeSessionToolPayload(t, stopResult)
	if stopPayload["cancellation_requested"] != true || stopPayload["run_id"] != run.ID() {
		t.Fatalf("session_stop payload = %#v", stopPayload)
	}
	select {
	case <-run.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("stopped run did not settle")
	}
	if _, err := run.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped run error = %v, want context.Canceled", err)
	}

	idleStop := executeSessionTool(t, executor, ToolSessionStop, map[string]any{"session_id": child.ID})
	idlePayload := decodeSessionToolPayload(t, idleStop)
	if idlePayload["status"] != "idle" || idlePayload["cancellation_requested"] != false {
		t.Fatalf("idle session_stop payload = %#v", idlePayload)
	}

	selfStop := executeSessionTool(t, executor, ToolSessionStop, map[string]any{"session_id": parent.ID})
	assertSessionToolErrorCode(t, selfStop, "self_stop_forbidden")
	selfWait := executeSessionTool(t, executor, ToolSessionWait, map[string]any{"session_id": parent.ID, "timeout_ms": 0})
	assertSessionToolErrorCode(t, selfWait, "self_wait_forbidden")

	outside := executeSessionTool(t, executor, ToolSessionGet, map[string]any{"session_id": otherProjectSession.ID})
	assertSessionToolErrorCode(t, outside, "session_forbidden")
}

func TestSessionStartValidatesModelPairDepthAndCoordinator(t *testing.T) {
	service, parent, _, _ := newSessionToolTestSessions(t, t.TempDir(), fakeExecutionTurnRunner{supports: true})
	executor := &sessionToolExecutor{service: service, caller: parent}

	missingModel := executeSessionTool(t, executor, ToolSessionStart, map[string]any{
		"prompt": "x", "provider": "provider-a",
	})
	assertSessionToolErrorCode(t, missingModel, "invalid_arguments")

	noCoordinator := executeSessionTool(t, executor, ToolSessionStart, map[string]any{"prompt": "x"})
	assertSessionToolErrorCode(t, noCoordinator, "coordinator_unavailable")

	deep := parent
	deep.SpawnDepth = maximumAgentSessionSpawnDepth
	executor.caller = deep
	tooDeep := executeSessionTool(t, executor, ToolSessionStart, map[string]any{"prompt": "x"})
	assertSessionToolErrorCode(t, tooDeep, "spawn_depth_exceeded")
}

func TestSessionStartCapacityFailureReturnsRecoverableCreatedSession(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, _ SessionTurnRequest) (SessionTurnResult, error) {
			started <- struct{}{}
			select {
			case <-release:
				return SessionTurnResult{Incremental: true}, nil
			case <-ctx.Done():
				return SessionTurnResult{}, ctx.Err()
			}
		},
	}
	service, parent, occupied, _ := newSessionToolTestSessions(t, t.TempDir(), runner)
	coordinator := NewSessionRunCoordinator(context.Background(), service, SessionRunCoordinatorOptions{MaxConcurrentRuns: 1})
	defer coordinator.Close()
	executor := &sessionToolExecutor{service: service, coordinator: coordinator, caller: parent}

	active, err := coordinator.Start(occupied.ID, SessionMessageInput{Content: "occupy capacity"}, nil)
	if err != nil {
		t.Fatalf("coordinator.Start(occupied) error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("capacity-occupying run did not start")
	}

	result := executeSessionTool(t, executor, ToolSessionStart, map[string]any{
		"prompt": "created but not admitted",
		"name":   "Recoverable child",
	})
	assertSessionToolErrorCode(t, result, "run_capacity_reached")
	var payload map[string]any
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("decode capacity error result %q: %v", result.Content, err)
	}
	childID := requiredPayloadString(t, payload, "session_id")
	if payload["created"] != true {
		t.Fatalf("capacity error payload = %#v, want created=true", payload)
	}
	child, err := service.GetSession(childID)
	if err != nil {
		t.Fatalf("GetSession(recoverable child) error = %v", err)
	}
	if child.ParentSessionID != parent.ID || child.DisplayName != "Recoverable child" || child.Status != "idle" {
		t.Fatalf("recoverable child = %#v", child)
	}

	close(release)
	if _, err := active.Wait(); err != nil {
		t.Fatalf("capacity-occupying run Wait() error = %v", err)
	}
}

func TestSessionToolsValidateArgumentsAtExecutorBoundary(t *testing.T) {
	service, parent, child, _ := newSessionToolTestSessions(t, t.TempDir(), fakeExecutionTurnRunner{supports: true})
	executor := &sessionToolExecutor{service: service, caller: parent}
	tooLongName := make([]rune, maximumSessionDisplayNameRunes+1)
	for index := range tooLongName {
		tooLongName[index] = '会'
	}

	tests := []struct {
		name      string
		tool      string
		arguments map[string]any
	}{
		{
			name: "start name over Unicode limit",
			tool: ToolSessionStart,
			arguments: map[string]any{
				"prompt": "task",
				"name":   string(tooLongName),
			},
		},
		{
			name:      "search invalid regex",
			tool:      ToolSessionSearch,
			arguments: map[string]any{"name_regex": "["},
		},
		{
			name: "search unknown status",
			tool: ToolSessionSearch,
			arguments: map[string]any{
				"name_regex": ".*",
				"statuses":   []any{"paused"},
			},
		},
		{
			name:      "search explicit zero limit",
			tool:      ToolSessionSearch,
			arguments: map[string]any{"name_regex": ".*", "limit": 0},
		},
		{
			name:      "search over maximum limit",
			tool:      ToolSessionSearch,
			arguments: map[string]any{"name_regex": ".*", "limit": maximumSessionSearchLimit + 1},
		},
		{
			name:      "search fractional limit",
			tool:      ToolSessionSearch,
			arguments: map[string]any{"name_regex": ".*", "limit": 1.5},
		},
		{
			name:      "get explicit zero output limit",
			tool:      ToolSessionGet,
			arguments: map[string]any{"session_id": child.ID, "max_output_chars": 0},
		},
		{
			name:      "get over maximum output limit",
			tool:      ToolSessionGet,
			arguments: map[string]any{"session_id": child.ID, "max_output_chars": maximumSessionOutputMaxChars + 1},
		},
		{
			name:      "history combines legacy cursors",
			tool:      ToolSessionHistory,
			arguments: map[string]any{"session_id": child.ID, "before_seq": 2, "after_seq": 1},
		},
		{
			name:      "history explicit zero legacy cursor",
			tool:      ToolSessionHistory,
			arguments: map[string]any{"session_id": child.ID, "before_seq": 0},
		},
		{
			name:      "history cursor without direction",
			tool:      ToolSessionHistory,
			arguments: map[string]any{"session_id": child.ID, "cursor": 2},
		},
		{
			name:      "history direction without cursor",
			tool:      ToolSessionHistory,
			arguments: map[string]any{"session_id": child.ID, "direction": "before"},
		},
		{
			name:      "history unknown direction",
			tool:      ToolSessionHistory,
			arguments: map[string]any{"session_id": child.ID, "cursor": 2, "direction": "sideways"},
		},
		{
			name:      "history cursor mixed with legacy cursor",
			tool:      ToolSessionHistory,
			arguments: map[string]any{"session_id": child.ID, "cursor": 2, "direction": "before", "before_seq": 1},
		},
		{
			name:      "history over maximum limit",
			tool:      ToolSessionHistory,
			arguments: map[string]any{"session_id": child.ID, "limit": maximumSessionChatItemsLimit + 1},
		},
		{
			name:      "wait negative timeout",
			tool:      ToolSessionWait,
			arguments: map[string]any{"session_id": child.ID, "timeout_ms": -1},
		},
		{
			name:      "wait explicit zero output limit",
			tool:      ToolSessionWait,
			arguments: map[string]any{"session_id": child.ID, "max_output_chars": 0},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := executeSessionTool(t, executor, test.tool, test.arguments)
			assertSessionToolErrorCode(t, result, "invalid_arguments")
		})
	}
}

func TestSessionHistoryReadsVisibleItemsWithPaginationAndProjectScope(t *testing.T) {
	service, parent, child, outside := newSessionToolTestSessions(t, t.TempDir(), fakeExecutionTurnRunner{supports: true})
	executor := &sessionToolExecutor{service: service, caller: parent}

	for index, content := range []string{"first", "second", "third"} {
		item := sessions.SessionItemFromMessage(fmt.Sprintf("message-%d", index), model.Message{
			Role:    model.MessageRoleAssistant,
			Content: content,
		})
		if _, err := service.sessionStore.AppendItem(child.ID, item); err != nil {
			t.Fatalf("AppendItem(%q) error = %v", content, err)
		}
	}

	latestResult := executeSessionTool(t, executor, ToolSessionHistory, map[string]any{
		"session_id": child.ID,
		"limit":      2,
	})
	latestPayload := decodeSessionToolPayload(t, latestResult)
	history := payloadMap(t, latestPayload, "history")
	items, ok := history["items"].([]any)
	if !ok || len(items) != 2 || items[0].(map[string]any)["id"] != "message-1" || items[1].(map[string]any)["id"] != "message-2" {
		t.Fatalf("session_history latest payload = %#v", latestPayload)
	}
	if history["has_more_before"] != true || history["has_more_after"] != false {
		t.Fatalf("session_history latest cursors = %#v", history)
	}

	olderResult := executeSessionTool(t, executor, ToolSessionHistory, map[string]any{
		"session_id": child.ID,
		"cursor":     int(history["oldest_seq"].(float64)),
		"direction":  "before",
		"limit":      2,
	})
	olderPayload := decodeSessionToolPayload(t, olderResult)
	olderHistory := payloadMap(t, olderPayload, "history")
	olderItems := olderHistory["items"].([]any)
	if len(olderItems) != 1 || olderItems[0].(map[string]any)["id"] != "message-0" {
		t.Fatalf("session_history older payload = %#v", olderPayload)
	}

	// The retired before_seq parameter keeps working and maps to the same page.
	legacyResult := executeSessionTool(t, executor, ToolSessionHistory, map[string]any{
		"session_id": child.ID,
		"before_seq": int(history["oldest_seq"].(float64)),
		"limit":      2,
	})
	legacyPayload := decodeSessionToolPayload(t, legacyResult)
	legacyItems := payloadMap(t, legacyPayload, "history")["items"].([]any)
	if len(legacyItems) != 1 || legacyItems[0].(map[string]any)["id"] != "message-0" {
		t.Fatalf("session_history legacy before_seq payload = %#v", legacyPayload)
	}

	// direction=after reads items newer than the cursor.
	newerResult := executeSessionTool(t, executor, ToolSessionHistory, map[string]any{
		"session_id": child.ID,
		"cursor":     int(olderItems[0].(map[string]any)["seq"].(float64)),
		"direction":  "after",
		"limit":      2,
	})
	newerPayload := decodeSessionToolPayload(t, newerResult)
	newerItems := payloadMap(t, newerPayload, "history")["items"].([]any)
	if len(newerItems) != 2 || newerItems[0].(map[string]any)["id"] != "message-1" || newerItems[1].(map[string]any)["id"] != "message-2" {
		t.Fatalf("session_history newer payload = %#v", newerPayload)
	}

	outsideResult := executeSessionTool(t, executor, ToolSessionHistory, map[string]any{"session_id": outside.ID})
	assertSessionToolErrorCode(t, outsideResult, "session_forbidden")
}

// Agents started before the cursor/direction contract still call with the
// retired before_seq/after_seq pair; the conflict error must teach the
// current contract so the next call self-corrects.
func TestSessionHistoryLegacyCursorConflictTeachesCurrentContract(t *testing.T) {
	service, parent, child, _ := newSessionToolTestSessions(t, t.TempDir(), fakeExecutionTurnRunner{supports: true})
	executor := &sessionToolExecutor{service: service, caller: parent}

	result := executeSessionTool(t, executor, ToolSessionHistory, map[string]any{
		"session_id": child.ID,
		"before_seq": 2,
		"after_seq":  1,
	})
	assertSessionToolErrorCode(t, result, "invalid_arguments")
	var payload map[string]any
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("decode error result %q: %v", result.Content, err)
	}
	message, _ := payload["error"].(string)
	if !strings.Contains(message, "cursor") || !strings.Contains(message, "direction") {
		t.Fatalf("session_history conflict error = %q, want it to teach cursor/direction", message)
	}
}

func TestSessionToolsListModelsAndStartExplicitModel(t *testing.T) {
	home := t.TempDir()
	service, parent, _, _ := newSessionToolTestSessions(t, home, fakeExecutionTurnRunner{supports: true})
	providersDir := filepath.Join(home, "providers")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(providers) error = %v", err)
	}
	rootConfig := `default_provider: fake
default_model: fast
provider_dir: providers
tools:
  enabled: [session_start, session_models]
`
	providerConfig := `name: fake
base_url: http://127.0.0.1:1/v1
api_key: test-key
models:
  fast:
    id: fake-fast
  precise:
    id: fake-precise
    context_window: 128000
    reasoning_config:
      parameter: reasoning_effort
      default: high
      levels:
        low: low
        high: high
`
	if err := os.WriteFile(filepath.Join(home, "sai.yaml"), []byte(rootConfig), 0o600); err != nil {
		t.Fatalf("WriteFile(root config) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(providersDir, "fake.yaml"), []byte(providerConfig), 0o600); err != nil {
		t.Fatalf("WriteFile(provider config) error = %v", err)
	}
	coordinator := NewSessionRunCoordinator(context.Background(), service, SessionRunCoordinatorOptions{})
	service.SetSessionRunCoordinator(coordinator)
	defer service.ClearSessionRunCoordinator(coordinator)
	defer coordinator.Close()
	executor := &sessionToolExecutor{service: service, coordinator: coordinator, caller: parent}

	modelsResult := executeSessionTool(t, executor, ToolSessionModels, map[string]any{})
	modelsPayload := decodeSessionToolPayload(t, modelsResult)
	models, ok := modelsPayload["models"].([]any)
	if !ok || len(models) != 2 {
		t.Fatalf("session_models payload = %#v", modelsPayload)
	}
	precise := models[1].(map[string]any)
	if precise["provider"] != "fake" || precise["model"] != "precise" || precise["model_id"] != "fake-precise" {
		t.Fatalf("session_models precise entry = %#v", precise)
	}

	startResult := executeSessionTool(t, executor, ToolSessionStart, map[string]any{
		"prompt": "explicit model task", "name": "Precise child",
		"provider": "fake", "model": "precise", "reasoning_level": "low",
	})
	startPayload := decodeSessionToolPayload(t, startResult)
	child, err := service.GetSession(requiredPayloadString(t, startPayload, "session_id"))
	if err != nil {
		t.Fatalf("GetSession(explicit child) error = %v", err)
	}
	if child.ParentSessionID != parent.ID || child.Provider != "fake" || child.ModelProfile != "precise" || child.ModelID != "fake-precise" || child.ReasoningLevel != "low" {
		t.Fatalf("explicit child = %#v", child)
	}
	if want := []string{ToolSessionModels}; !reflect.DeepEqual(child.EnabledTools, want) {
		t.Fatalf("explicit child enabled tools = %#v, want %#v", child.EnabledTools, want)
	}
}

func newSessionToolTestSessions(t *testing.T, home string, runner SessionTurnRunner) (*Service, SessionDetail, SessionDetail, SessionDetail) {
	t.Helper()
	service, err := NewServiceWithOptions(home, ServiceOptions{TurnRunner: runner})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	projectRoot := mkdirProjectRoot(t, "tool-project-a")
	project, err := service.CreateProject(projectRoot, "A")
	if err != nil {
		t.Fatalf("CreateProject(A) error = %v", err)
	}
	otherRoot := mkdirProjectRoot(t, "tool-project-b")
	otherProject, err := service.CreateProject(otherRoot, "B")
	if err != nil {
		t.Fatalf("CreateProject(B) error = %v", err)
	}
	save := true
	metadata := func(cwd string) SessionCreateMetadata {
		return SessionCreateMetadata{
			CreatedCWD: cwd, ConfigPath: filepath.Join(home, "sai.yaml"),
			Provider: "provider-a", ModelProfile: "model-a", ModelID: "model-id-a",
			SaveToolResults: &save,
		}
	}
	parent, err := service.CreateSession(project.Project.ID, metadata(projectRoot))
	if err != nil {
		t.Fatalf("CreateSession(parent) error = %v", err)
	}
	childMetadata := metadata(projectRoot)
	childMetadata.ParentSessionID = parent.ID
	child, err := service.CreateSession(project.Project.ID, childMetadata)
	if err != nil {
		t.Fatalf("CreateSession(child) error = %v", err)
	}
	outside, err := service.CreateSession(otherProject.Project.ID, metadata(otherRoot))
	if err != nil {
		t.Fatalf("CreateSession(outside) error = %v", err)
	}
	return service, parent, child, outside
}

func executeSessionTool(t *testing.T, executor *sessionToolExecutor, name string, arguments map[string]any) model.ToolResult {
	t.Helper()
	result, err := executor.Execute(context.Background(), name, arguments)
	if err != nil {
		t.Fatalf("Execute(%s) error = %v", name, err)
	}
	return result
}

func decodeSessionToolPayload(t *testing.T, result model.ToolResult) map[string]any {
	t.Helper()
	if result.IsError {
		t.Fatalf("tool result is error: %s", result.Content)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("decode tool result %q: %v", result.Content, err)
	}
	if payload["ok"] != true {
		t.Fatalf("tool payload ok = %#v, payload = %#v", payload["ok"], payload)
	}
	return payload
}

func assertSessionToolErrorCode(t *testing.T, result model.ToolResult, code string) {
	t.Helper()
	if !result.IsError {
		t.Fatalf("tool result IsError = false, content = %s", result.Content)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("decode error result %q: %v", result.Content, err)
	}
	if payload["ok"] != false || payload["code"] != code {
		t.Fatalf("tool error payload = %#v, want code %q", payload, code)
	}
}

func requiredPayloadString(t *testing.T, payload map[string]any, name string) string {
	t.Helper()
	value, ok := payload[name].(string)
	if !ok || value == "" {
		t.Fatalf("payload[%q] = %#v, want non-empty string in %#v", name, payload[name], payload)
	}
	return value
}

func payloadMap(t *testing.T, payload map[string]any, name string) map[string]any {
	t.Helper()
	value, ok := payload[name].(map[string]any)
	if !ok {
		t.Fatalf("payload[%q] = %#v (%T), want object in %#v", name, payload[name], payload[name], payload)
	}
	return value
}

func canonicalTestJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal(%#v) error = %v", value, err)
	}
	return string(data)
}
