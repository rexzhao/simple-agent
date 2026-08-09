package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/model"
)

const testToken = "test-capability-token"

type webTestRunner struct{}
type blockingWebTestRunner struct{}

type enteredBlockingWebTestRunner struct {
	entered chan struct{}
	once    *sync.Once
}

func (webTestRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (webTestRunner) RunSessionTurn(_ context.Context, request execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	request.Emit(model.TextDeltaEvent{Text: "hello from web"})
	if err := request.Publisher.Publish(eventbus.AssistantReady{
		TurnID:  request.TurnID,
		Message: model.Message{Role: model.MessageRoleAssistant, Content: "hello from web"},
	}); err != nil {
		return execution.SessionTurnResult{}, err
	}
	return execution.SessionTurnResult{Incremental: true}, nil
}

func (blockingWebTestRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (blockingWebTestRunner) RunSessionTurn(ctx context.Context, _ execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	<-ctx.Done()
	return execution.SessionTurnResult{}, ctx.Err()
}

func (r enteredBlockingWebTestRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (r enteredBlockingWebTestRunner) RunSessionTurn(ctx context.Context, _ execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	r.once.Do(func() { close(r.entered) })
	<-ctx.Done()
	return execution.SessionTurnResult{}, ctx.Err()
}

func newWebTestServer(t *testing.T) (*httptest.Server, *execution.Service) {
	return newWebTestServerWithRunner(t, webTestRunner{})
}

func newWebTestServerWithRunner(t *testing.T, runner execution.SessionTurnRunner) (*httptest.Server, *execution.Service) {
	t.Helper()
	server, service, _ := newWebTestAppServerWithRunner(t, runner)
	return server, service
}

func newWebTestAppServerWithRunner(t *testing.T, runner execution.SessionTurnRunner) (*httptest.Server, *execution.Service, *Server) {
	t.Helper()
	home := t.TempDir()
	service, err := execution.NewServiceWithOptions(home, execution.ServiceOptions{TurnRunner: runner})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	writeWebTestConfig(t, home)
	app, err := NewServer(ServerOptions{Context: context.Background(), Service: service, Token: testToken, CWD: home})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(app.Close)
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)
	return server, service, app
}

// createWebProjectAndSession creates durable state through the execution
// service. Tests of business behavior therefore do not depend on a removed
// project/session REST adapter.
func createWebProjectAndSession(t *testing.T, service *execution.Service) (execution.Project, execution.SessionDetail) {
	t.Helper()
	result, err := service.CreateProject(t.TempDir(), "Web Test")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	session, err := service.CreateSession(result.Project.ID, execution.SessionCreateMetadata{
		DisplayName: "Web Session", CreatedCWD: result.Project.Root,
		Provider: "fake", ModelProfile: "fast", ModelID: "fake-model",
	})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	return result.Project, session
}

func doJSONRequest(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal(body) error = %v", err)
	}
	request, err := http.NewRequest(method, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request %s %s error = %v", method, url, err)
	}
	return response
}

func doRawAPIRequest(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request %s %s error = %v", method, url, err)
	}
	return response
}

func decodeResponse(t *testing.T, response *http.Response, target any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatalf("Decode(response) error = %v", err)
	}
}

func readBody(response *http.Response) string {
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return string(raw)
}

func responseErrorCode(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("Decode(error response) error = %v", err)
	}
	return payload.Error.Code
}

func writeWebTestConfig(t *testing.T, root string) {
	t.Helper()
	providersDir := filepath.Join(root, "providers")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(providers) error = %v", err)
	}
	rootConfig := `default_provider: fake
default_model: fast
provider_dir: providers
`
	providerConfig := `name: fake
base_url: http://127.0.0.1:1/v1
api_key: test-key
models:
  fast:
    id: fake-model
    input: [text, image]
    context_window: 32000
  precise:
    id: fake-precise
    context_window: 64000
`
	if err := os.WriteFile(filepath.Join(root, "sai.yaml"), []byte(rootConfig), 0o600); err != nil {
		t.Fatalf("WriteFile(root config) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(providersDir, "fake.yaml"), []byte(providerConfig), 0o600); err != nil {
		t.Fatalf("WriteFile(provider config) error = %v", err)
	}
}

type queuedPromptPayload struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Steer   bool   `json:"steer"`
}

type promptQueueObservation struct {
	runID string
	event execution.SessionStreamEvent
}

type promptQueueObserver struct {
	events chan promptQueueObservation
	mu     sync.Mutex
	latest map[string][]queuedPromptPayload
}

func newPromptQueueObserver() *promptQueueObserver {
	return &promptQueueObserver{events: make(chan promptQueueObservation, 128), latest: make(map[string][]queuedPromptPayload)}
}

func (observer *promptQueueObserver) RunAdmitted(*execution.CoordinatedSessionRun)        {}
func (observer *promptQueueObserver) RunAdmissionFailed(*execution.CoordinatedSessionRun) {}
func (observer *promptQueueObserver) RunSettled(*execution.CoordinatedSessionRun, execution.SessionMessageResult, error) {
}
func (observer *promptQueueObserver) RunEvent(run *execution.CoordinatedSessionRun, event execution.SessionStreamEvent) {
	if run == nil || event == nil || event["type"] != "run.prompt_queue" {
		return
	}
	observation := promptQueueObservation{runID: run.ID(), event: event}
	if prompts := promptQueueFromEvent(event); prompts != nil {
		observer.mu.Lock()
		observer.latest[run.ID()] = append([]queuedPromptPayload(nil), prompts...)
		observer.mu.Unlock()
	}
	select {
	case observer.events <- observation:
	default:
	}
}

func (observer *promptQueueObserver) queue(runID string) ([]queuedPromptPayload, bool) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	prompts, ok := observer.latest[runID]
	if !ok {
		return nil, false
	}
	return append([]queuedPromptPayload(nil), prompts...), true
}

func promptQueueFromEvent(event execution.SessionStreamEvent) []queuedPromptPayload {
	raw, ok := event["prompts"]
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var prompts []queuedPromptPayload
	if json.Unmarshal(encoded, &prompts) != nil {
		return nil
	}
	return prompts
}

func waitForPromptQueue(t *testing.T, observer *promptQueueObserver, runID string, count int) []queuedPromptPayload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if prompts, ok := observer.queue(runID); ok && len(prompts) == count {
			return prompts
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run prompt queue did not reach %d items", count)
	return nil
}

func waitForPromptQueueOrder(t *testing.T, observer *promptQueueObserver, runID string, want []string) []queuedPromptPayload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if prompts, ok := observer.queue(runID); ok && len(prompts) == len(want) {
			matches := true
			for index, prompt := range prompts {
				if prompt.Content != want[index] {
					matches = false
				}
			}
			if matches {
				return prompts
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run prompt queue order did not become %#v", want)
	return nil
}

func waitForPromptContent(t *testing.T, observer *promptQueueObserver, runID, content string) []queuedPromptPayload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if prompts, ok := observer.queue(runID); ok {
			for _, prompt := range prompts {
				if prompt.Content == content {
					return prompts
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run prompt queue did not publish %q", content)
	return nil
}

var _ execution.SessionIncrementalSupporter = webTestRunner{}
var _ execution.SessionTurnRunner = webTestRunner{}
var _ execution.SessionIncrementalSupporter = blockingWebTestRunner{}
var _ execution.SessionTurnRunner = blockingWebTestRunner{}
var _ execution.SessionIncrementalSupporter = enteredBlockingWebTestRunner{}
var _ execution.SessionTurnRunner = enteredBlockingWebTestRunner{}
