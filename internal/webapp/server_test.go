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
	"reflect"
	"testing"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/model"
)

const testToken = "test-capability-token"

type webTestRunner struct{}

type blockingWebTestRunner struct{}

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

func TestServerRequiresTokenForAPIAndServesEmbeddedUI(t *testing.T) {
	server, _ := newWebTestServer(t)

	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("GET / error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d", response.StatusCode)
	}
	body, _ := io.ReadAll(response.Body)
	if !bytes.Contains(body, []byte("SAI")) {
		t.Fatalf("GET / body = %q", body)
	}

	response, err = http.Get(server.URL + "/api/projects")
	if err != nil {
		t.Fatalf("GET unauthenticated projects error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET unauthenticated projects status = %d", response.StatusCode)
	}
}

func TestServerProjectSessionAndRunFlow(t *testing.T) {
	server, service := newWebTestServer(t)
	root := t.TempDir()
	writeWebTestConfig(t, root)

	created := doJSONRequest(t, http.MethodPost, server.URL+"/api/projects", map[string]string{
		"root":         root,
		"display_name": "Web Test",
	})
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("POST project status = %d body=%s", created.StatusCode, readBody(created))
	}
	var projectResult execution.ProjectCreateResult
	decodeResponse(t, created, &projectResult)
	if projectResult.Project.ID == "" {
		t.Fatal("created project id is empty")
	}
	modelsResponse := doJSONRequest(t, http.MethodGet, server.URL+"/api/projects/"+projectResult.Project.ID+"/models", nil)
	if modelsResponse.StatusCode != http.StatusOK {
		t.Fatalf("GET models status = %d body=%s", modelsResponse.StatusCode, readBody(modelsResponse))
	}
	var modelOptions execution.SessionModelOptions
	decodeResponse(t, modelsResponse, &modelOptions)
	if modelOptions.DefaultProvider != "fake" || modelOptions.DefaultModel != "fast" || len(modelOptions.Models) != 2 {
		t.Fatalf("GET models = %#v", modelOptions)
	}

	created = doJSONRequest(t, http.MethodPost, server.URL+"/api/projects/"+projectResult.Project.ID+"/sessions", map[string]string{
		"provider":      "fake",
		"model_profile": "precise",
	})
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("POST session status = %d body=%s", created.StatusCode, readBody(created))
	}
	var session execution.SessionDetail
	decodeResponse(t, created, &session)
	if session.ID == "" {
		t.Fatal("created session id is empty")
	}
	if session.Provider != "fake" || session.ModelProfile != "precise" || session.ModelID != "fake-precise" {
		t.Fatalf("created session model = %q/%q/%q", session.Provider, session.ModelProfile, session.ModelID)
	}

	created = doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/"+session.ID+"/runs", map[string]string{"content": "hi"})
	if created.StatusCode != http.StatusAccepted {
		t.Fatalf("POST run status = %d body=%s", created.StatusCode, readBody(created))
	}
	var run struct {
		ID string `json:"run_id"`
	}
	decodeResponse(t, created, &run)
	if run.ID == "" {
		t.Fatal("run id is empty")
	}

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/runs/"+run.ID+"/events", nil)
	if err != nil {
		t.Fatalf("NewRequest(events) error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET events error = %v", err)
	}
	defer response.Body.Close()
	events, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll(events) error = %v", err)
	}
	for _, eventType := range []string{"turn.started", "text.delta", "turn.committed", "run.settled"} {
		if !bytes.Contains(events, []byte(`"type":"`+eventType+`"`)) {
			t.Fatalf("events missing %q: %s", eventType, events)
		}
	}

	page, err := service.GetSessionChatItems(session.ID)
	if err != nil {
		t.Fatalf("GetSessionChatItems() error = %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("persisted chat items = %d, want 2: %#v", len(page.Items), page.Items)
	}
}

func TestServerCancelsRun(t *testing.T) {
	server, _ := newWebTestServerWithRunner(t, blockingWebTestRunner{})
	root := t.TempDir()
	writeWebTestConfig(t, root)

	created := doJSONRequest(t, http.MethodPost, server.URL+"/api/projects", map[string]string{"root": root})
	var projectResult execution.ProjectCreateResult
	decodeResponse(t, created, &projectResult)
	created = doJSONRequest(t, http.MethodPost, server.URL+"/api/projects/"+projectResult.Project.ID+"/sessions", map[string]string{})
	var session execution.SessionDetail
	decodeResponse(t, created, &session)
	created = doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/"+session.ID+"/runs", map[string]string{"content": "wait"})
	var run struct {
		ID string `json:"run_id"`
	}
	decodeResponse(t, created, &run)

	cancelled := doJSONRequest(t, http.MethodDelete, server.URL+"/api/runs/"+run.ID, map[string]string{})
	if cancelled.StatusCode != http.StatusAccepted {
		t.Fatalf("POST cancel status = %d body=%s", cancelled.StatusCode, readBody(cancelled))
	}
	cancelled.Body.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/runs/"+run.ID+"/events", nil)
	if err != nil {
		t.Fatalf("NewRequest(events) error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET events error = %v", err)
	}
	defer response.Body.Close()
	events, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll(events) error = %v", err)
	}
	if !bytes.Contains(events, []byte(`"type":"run.settled"`)) || !bytes.Contains(events, []byte(`"status":"cancelled"`)) {
		t.Fatalf("cancel events = %s", events)
	}
}

func TestServerProviderSettingsPreserveSecretsAndWriteReasoningDefaults(t *testing.T) {
	server, _ := newWebTestServer(t)
	root := t.TempDir()
	writeWebTestConfig(t, root)

	created := doJSONRequest(t, http.MethodPost, server.URL+"/api/projects", map[string]string{"root": root})
	var projectResult execution.ProjectCreateResult
	decodeResponse(t, created, &projectResult)
	baseURL := server.URL + "/api/projects/" + projectResult.Project.ID

	response := doJSONRequest(t, http.MethodGet, baseURL+"/provider-settings", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET provider settings status = %d body=%s", response.StatusCode, readBody(response))
	}
	var document execution.ProviderSettingsDocument
	decodeResponse(t, response, &document)
	if len(document.Providers) != 1 || document.Providers[0].APIKey != "" || !document.Providers[0].APIKeyConfigured {
		t.Fatalf("provider settings exposed or lost API key state: %#v", document.Providers)
	}

	input := execution.ProviderSettingsInput{
		Name:       "fake",
		BaseURL:    "http://127.0.0.1:1/v1",
		KeepAPIKey: true,
		Models: []execution.ProviderModelSettings{{
			Profile:       "gpt",
			ID:            "gpt-5.5",
			Type:          "openai-responses",
			ContextWindow: 400000,
		}},
	}
	response = doJSONRequest(t, http.MethodPut, baseURL+"/providers/fake", input)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PUT provider status = %d body=%s", response.StatusCode, readBody(response))
	}
	decodeResponse(t, response, &document)
	model := document.Providers[0].Models[0]
	if model.ReasoningConfig.Parameter != "reasoning.effort" || model.ReasoningConfig.Default != "xhigh" {
		t.Fatalf("reasoning default = %#v", model.ReasoningConfig)
	}

	response = doJSONRequest(t, http.MethodGet, baseURL+"/models", nil)
	var options execution.SessionModelOptions
	decodeResponse(t, response, &options)
	wantLevels := []string{"minimal", "low", "medium", "high", "xhigh"}
	if len(options.Models) != 1 || !reflect.DeepEqual(options.Models[0].ReasoningLevels, wantLevels) {
		t.Fatalf("session model reasoning levels = %#v, want %#v", options.Models, wantLevels)
	}

	providerData, err := os.ReadFile(filepath.Join(root, ".agents", "providers", "fake.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(provider) error = %v", err)
	}
	if !bytes.Contains(providerData, []byte("api_key: test-key")) || !bytes.Contains(providerData, []byte("reasoning_config:")) {
		t.Fatalf("persisted provider = %s", providerData)
	}
}

func newWebTestServer(t *testing.T) (*httptest.Server, *execution.Service) {
	return newWebTestServerWithRunner(t, webTestRunner{})
}

func newWebTestServerWithRunner(t *testing.T, runner execution.SessionTurnRunner) (*httptest.Server, *execution.Service) {
	t.Helper()
	home := t.TempDir()
	service, err := execution.NewServiceWithOptions(home, execution.ServiceOptions{TurnRunner: runner})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	app, err := NewServer(ServerOptions{Context: context.Background(), Service: service, Token: testToken, CWD: home})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)
	return server, service
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

func writeWebTestConfig(t *testing.T, root string) {
	t.Helper()
	agentsDir := filepath.Join(root, ".agents")
	providersDir := filepath.Join(agentsDir, "providers")
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
    context_window: 32000
  precise:
    id: fake-precise
    context_window: 64000
`
	if err := os.WriteFile(filepath.Join(agentsDir, "sai.yaml"), []byte(rootConfig), 0o600); err != nil {
		t.Fatalf("WriteFile(root config) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(providersDir, "fake.yaml"), []byte(providerConfig), 0o600); err != nil {
		t.Fatalf("WriteFile(provider config) error = %v", err)
	}
}

var _ execution.SessionIncrementalSupporter = webTestRunner{}
var _ execution.SessionTurnRunner = webTestRunner{}
var _ execution.SessionIncrementalSupporter = blockingWebTestRunner{}
var _ execution.SessionTurnRunner = blockingWebTestRunner{}
