package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/execution"
)

type enteredBlockingWebTestRunner struct {
	entered chan struct{}
	once    *sync.Once
}

func (r enteredBlockingWebTestRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (r enteredBlockingWebTestRunner) RunSessionTurn(ctx context.Context, _ execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	r.once.Do(func() { close(r.entered) })
	<-ctx.Done()
	return execution.SessionTurnResult{}, ctx.Err()
}

func TestServerSecurityBootstrapAndJSONContract(t *testing.T) {
	server, service, app := newWebTestAppServerWithRunner(t, webTestRunner{})

	originalVersion := Version
	Version = "v-behavior-test"
	t.Cleanup(func() { Version = originalVersion })
	response := doJSONRequest(t, http.MethodGet, server.URL+"/api/bootstrap", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET bootstrap status = %d body=%s", response.StatusCode, readBody(response))
	}
	for name, want := range map[string]string{
		"Content-Security-Policy": "default-src 'self'",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
	} {
		if got := response.Header.Get(name); !strings.Contains(got, want) {
			t.Fatalf("GET bootstrap header %s = %q, want %q", name, got, want)
		}
	}
	var bootstrap struct {
		Version    string `json:"version"`
		CWD        string `json:"cwd"`
		ServerRoot string `json:"server_root"`
		ConfigPath string `json:"config_path"`
	}
	decodeResponse(t, response, &bootstrap)
	if bootstrap.Version != "v-behavior-test" || bootstrap.CWD != service.ServerRoot() || bootstrap.ServerRoot != service.ServerRoot() || bootstrap.ConfigPath != service.ConfigPath() {
		t.Fatalf("GET bootstrap = %#v, want version and resolved server paths", bootstrap)
	}

	request := httptest.NewRequest(http.MethodGet, "http://example.com/api/bootstrap", nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || responseErrorCode(t, recorder.Result()) != "invalid_host" {
		t.Fatalf("invalid host response = %d %s", recorder.Code, recorder.Body.String())
	}

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/bootstrap", nil)
	if err != nil {
		t.Fatalf("NewRequest(origin) error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Origin", "https://attacker.example.test")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET bootstrap with invalid origin error = %v", err)
	}
	if response.StatusCode != http.StatusForbidden || responseErrorCode(t, response) != "invalid_origin" {
		t.Fatalf("invalid origin status = %d, want forbidden invalid_origin", response.StatusCode)
	}

	response, err = http.Get(server.URL + "/client/side/route")
	if err != nil {
		t.Fatalf("GET SPA fallback error = %v", err)
	}
	spaBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(SPA fallback) error = %v", err)
	}
	if response.StatusCode != http.StatusOK || !bytes.Contains(spaBody, []byte("SAI")) {
		t.Fatalf("GET SPA fallback = %d %q, want embedded UI", response.StatusCode, spaBody)
	}

	for _, body := range []string{
		`{"root":"ignored","unexpected":true}`,
		`{"root":"ignored"} {"root":"second"}`,
	} {
		response = doRawAPIRequest(t, http.MethodPost, server.URL+"/api/projects", body)
		if response.StatusCode != http.StatusBadRequest || responseErrorCode(t, response) != "invalid_json" {
			t.Fatalf("POST invalid JSON %q status = %d, want bad request invalid_json", body, response.StatusCode)
		}
	}
}

func TestServerLifecycleFailuresUseStableHTTPContract(t *testing.T) {
	runner := enteredBlockingWebTestRunner{entered: make(chan struct{}), once: &sync.Once{}}
	server, _, app := newWebTestAppServerWithRunner(t, runner)
	project, session := createWebProjectAndSession(t, server)

	response := doJSONRequest(t, http.MethodDelete, server.URL+"/api/sessions/"+session.ID, nil)
	if response.StatusCode != http.StatusUnprocessableEntity || responseErrorCode(t, response) != "request_failed" {
		t.Fatalf("DELETE active session status = %d, want 422 request_failed", response.StatusCode)
	}
	response = doJSONRequest(t, http.MethodDelete, server.URL+"/api/projects/"+project.ID, nil)
	if response.StatusCode != http.StatusUnprocessableEntity || responseErrorCode(t, response) != "request_failed" {
		t.Fatalf("DELETE active project status = %d, want 422 request_failed", response.StatusCode)
	}
	response = doJSONRequest(t, http.MethodGet, server.URL+"/api/sessions/session-missing", nil)
	if response.StatusCode != http.StatusNotFound || responseErrorCode(t, response) != "not_found" {
		t.Fatalf("GET missing session status = %d, want 404 not_found", response.StatusCode)
	}

	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/"+session.ID+"/runs", map[string]string{"content": "block"})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("POST blocking run status = %d body=%s", response.StatusCode, readBody(response))
	}
	var run struct {
		ID string `json:"run_id"`
	}
	decodeResponse(t, response, &run)
	select {
	case <-runner.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking runner was not entered")
	}
	managed, ok := app.runs.get(run.ID)
	if !ok {
		t.Fatalf("run %s not found in registry", run.ID)
	}

	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/projects/"+project.ID+"/archive", map[string]string{})
	if response.StatusCode != http.StatusConflict || responseErrorCode(t, response) != "session_busy" {
		t.Fatalf("POST archive project with active run status = %d, want 409 session_busy", response.StatusCode)
	}
	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/"+session.ID+"/archive", map[string]string{})
	if response.StatusCode != http.StatusConflict || responseErrorCode(t, response) != "session_busy" {
		t.Fatalf("POST archive session with active run status = %d, want 409 session_busy", response.StatusCode)
	}

	response = doJSONRequest(t, http.MethodDelete, server.URL+"/api/runs/"+run.ID, nil)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE active run status = %d body=%s", response.StatusCode, readBody(response))
	}
	response.Body.Close()
	waitForManagedRunTerminal(t, managed)
}

func TestServerSessionFullAccessEndpoint(t *testing.T) {
	server, _, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
	project, session := createWebProjectAndSession(t, server)
	if session.FullAccess {
		t.Fatalf("new session FullAccess = true, want default false")
	}

	response := doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/"+session.ID+"/full-access", map[string]any{"full_access": true})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST full-access status = %d body=%s", response.StatusCode, readBody(response))
	}
	var updated execution.SessionDetail
	decodeResponse(t, response, &updated)
	if !updated.FullAccess {
		t.Fatalf("POST full-access session = %#v, want full_access true", updated)
	}

	response = doJSONRequest(t, http.MethodGet, server.URL+"/api/sessions/"+session.ID, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET session status = %d", response.StatusCode)
	}
	var fetched execution.SessionDetail
	decodeResponse(t, response, &fetched)
	if !fetched.FullAccess {
		t.Fatalf("GET session full_access = false, want persisted true")
	}

	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/"+session.ID+"/full-access", map[string]any{"full_access": false})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST full-access off status = %d body=%s", response.StatusCode, readBody(response))
	}
	decodeResponse(t, response, &updated)
	if updated.FullAccess {
		t.Fatalf("POST full-access off session = %#v, want full_access false", updated)
	}

	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/session-missing/full-access", map[string]any{"full_access": true})
	if response.StatusCode != http.StatusNotFound || responseErrorCode(t, response) != "not_found" {
		t.Fatalf("POST full-access missing session status = %d, want 404 not_found", response.StatusCode)
	}

	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/projects/"+project.ID+"/sessions", map[string]any{"full_access": true})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("POST session with full_access status = %d body=%s", response.StatusCode, readBody(response))
	}
	var created execution.SessionDetail
	decodeResponse(t, response, &created)
	if !created.FullAccess {
		t.Fatalf("POST session with full_access = %#v, want full_access true", created)
	}
}

func TestServerSessionDebugEndpoint(t *testing.T) {
	server, _, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
	_, session := createWebProjectAndSession(t, server)
	if session.Debug.RequestBodies {
		t.Fatalf("new session Debug.RequestBodies = true, want default false")
	}

	response := doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/"+session.ID+"/debug", map[string]any{
		"debug": map[string]any{"request_bodies": true},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST debug status = %d body=%s", response.StatusCode, readBody(response))
	}
	var updated execution.SessionDetail
	decodeResponse(t, response, &updated)
	if !updated.Debug.RequestBodies {
		t.Fatalf("POST debug session = %#v, want request-body capture enabled", updated)
	}

	response = doJSONRequest(t, http.MethodGet, server.URL+"/api/sessions/"+session.ID, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET debug session status = %d", response.StatusCode)
	}
	var fetched execution.SessionDetail
	decodeResponse(t, response, &fetched)
	if !fetched.Debug.RequestBodies {
		t.Fatalf("GET session debug.request_bodies = false, want persisted true")
	}

	// The flat form remains accepted for callers that used the original
	// request_bodies setting before the debug namespace was introduced.
	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/"+session.ID+"/debug", map[string]any{"request_bodies": false})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST flat debug status = %d body=%s", response.StatusCode, readBody(response))
	}
	flatBody := readBody(response)
	var flat execution.SessionDetail
	if err := json.Unmarshal([]byte(flatBody), &flat); err != nil {
		t.Fatalf("decode flat debug response: %v", err)
	}
	if flat.Debug.RequestBodies {
		t.Fatalf("POST flat debug session = %#v, want request-body capture disabled", flat)
	}

	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/session-missing/debug", map[string]any{"debug": map[string]any{"request_bodies": true}})
	if response.StatusCode != http.StatusNotFound || responseErrorCode(t, response) != "not_found" {
		t.Fatalf("POST debug missing session status = %d, want 404 not_found", response.StatusCode)
	}
}

func TestServerActivePromptQueueLifecycle(t *testing.T) {
	runner := enteredBlockingWebTestRunner{entered: make(chan struct{}), once: &sync.Once{}}
	server, _, app := newWebTestAppServerWithRunner(t, runner)
	_, session := createWebProjectAndSession(t, server)

	response := doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/"+session.ID+"/runs", map[string]string{"content": "block"})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("POST blocking run status = %d body=%s", response.StatusCode, readBody(response))
	}
	var run struct {
		ID string `json:"run_id"`
	}
	decodeResponse(t, response, &run)
	select {
	case <-runner.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking runner was not entered")
	}
	managed, ok := app.runs.get(run.ID)
	if !ok {
		t.Fatalf("run %s not found in registry", run.ID)
	}

	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/runs/"+run.ID+"/prompts", map[string]string{"content": "follow up"})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("POST active prompt status = %d body=%s", response.StatusCode, readBody(response))
	}
	response.Body.Close()
	queued := waitForPromptQueue(t, managed, 1)
	if queued[0].ID == "" || queued[0].Content != "follow up" {
		t.Fatalf("active prompt queue = %#v, want stable id and content", queued)
	}

	response = doJSONRequest(t, http.MethodDelete, server.URL+"/api/runs/"+run.ID+"/prompts/"+queued[0].ID, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("DELETE active prompt status = %d body=%s", response.StatusCode, readBody(response))
	}
	response.Body.Close()
	waitForPromptQueue(t, managed, 0)

	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/runs/"+run.ID+"/prompts", map[string]string{"content": "   "})
	if response.StatusCode != http.StatusBadRequest || responseErrorCode(t, response) != "invalid_content" {
		t.Fatalf("POST empty active prompt status = %d, want 400 invalid_content", response.StatusCode)
	}
	response = doJSONRequest(t, http.MethodDelete, server.URL+"/api/runs/"+run.ID+"/prompts/ap-missing", nil)
	if response.StatusCode != http.StatusNotFound || responseErrorCode(t, response) != "not_found" {
		t.Fatalf("DELETE missing active prompt status = %d, want 404 not_found", response.StatusCode)
	}

	response = doJSONRequest(t, http.MethodDelete, server.URL+"/api/runs/"+run.ID, nil)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE active run status = %d body=%s", response.StatusCode, readBody(response))
	}
	response.Body.Close()
	waitForManagedRunTerminal(t, managed)
	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/runs/"+run.ID+"/prompts", map[string]string{"content": "too late"})
	if response.StatusCode != http.StatusConflict || responseErrorCode(t, response) != "run_settled" {
		t.Fatalf("POST prompt to settled run status = %d, want 409 run_settled", response.StatusCode)
	}
}

func TestServerActivePromptSteerAndMove(t *testing.T) {
	runner := enteredBlockingWebTestRunner{entered: make(chan struct{}), once: &sync.Once{}}
	server, _, app := newWebTestAppServerWithRunner(t, runner)
	_, session := createWebProjectAndSession(t, server)

	response := doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/"+session.ID+"/runs", map[string]string{"content": "block"})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("POST blocking run status = %d body=%s", response.StatusCode, readBody(response))
	}
	var run struct {
		ID string `json:"run_id"`
	}
	decodeResponse(t, response, &run)
	select {
	case <-runner.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking runner was not entered")
	}
	managed, ok := app.runs.get(run.ID)
	if !ok {
		t.Fatalf("run %s not found in registry", run.ID)
	}

	for _, content := range []string{"first", "second", "third"} {
		response = doJSONRequest(t, http.MethodPost, server.URL+"/api/runs/"+run.ID+"/prompts", map[string]string{"content": content})
		if response.StatusCode != http.StatusAccepted {
			t.Fatalf("POST active prompt %q status = %d body=%s", content, response.StatusCode, readBody(response))
		}
		response.Body.Close()
	}
	queued := waitForPromptQueueOrder(t, managed, []string{"first", "second", "third"})
	if queued[0].Steer || queued[1].Steer || queued[2].Steer {
		t.Fatalf("fresh queue steer flags = %#v, want all plain", queued)
	}
	byContent := make(map[string]string, len(queued))
	for _, prompt := range queued {
		byContent[prompt.Content] = prompt.ID
	}

	// Promote "second" to steer: it jumps to the top of the queue.
	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/runs/"+run.ID+"/prompts/"+byContent["second"]+"/steer", map[string]bool{"steer": true})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST steer status = %d body=%s", response.StatusCode, readBody(response))
	}
	response.Body.Close()
	queued = waitForPromptQueueOrder(t, managed, []string{"second", "first", "third"})
	if !queued[0].Steer || queued[1].Steer || queued[2].Steer {
		t.Fatalf("steer flags after promotion = %#v, want only second steered", queued)
	}

	// Reorder within the plain group: "third" moves above "first".
	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/runs/"+run.ID+"/prompts/"+byContent["third"]+"/move", map[string]string{"direction": "up"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST move status = %d body=%s", response.StatusCode, readBody(response))
	}
	response.Body.Close()
	waitForPromptQueueOrder(t, managed, []string{"second", "third", "first"})

	// The plain group cannot climb above the steer: moving "third" up again is
	// a clamped no-op.
	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/runs/"+run.ID+"/prompts/"+byContent["third"]+"/move", map[string]string{"direction": "up"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST clamped move status = %d body=%s", response.StatusCode, readBody(response))
	}
	response.Body.Close()
	waitForPromptQueueOrder(t, managed, []string{"second", "third", "first"})

	// Demote "second" back to the plain queue.
	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/runs/"+run.ID+"/prompts/"+byContent["second"]+"/steer", map[string]bool{"steer": false})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST demote status = %d body=%s", response.StatusCode, readBody(response))
	}
	response.Body.Close()
	waitForPromptQueueOrder(t, managed, []string{"second", "third", "first"})

	// Error branches: invalid direction, missing prompt, missing run.
	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/runs/"+run.ID+"/prompts/"+byContent["first"]+"/move", map[string]string{"direction": "sideways"})
	if response.StatusCode != http.StatusBadRequest || responseErrorCode(t, response) != "invalid_direction" {
		t.Fatalf("POST invalid move direction status = %d, want 400 invalid_direction", response.StatusCode)
	}
	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/runs/"+run.ID+"/prompts/ap-missing/steer", map[string]bool{"steer": true})
	if response.StatusCode != http.StatusNotFound || responseErrorCode(t, response) != "not_found" {
		t.Fatalf("POST steer missing prompt status = %d, want 404 not_found", response.StatusCode)
	}
	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/runs/"+run.ID+"/prompts/ap-missing/move", map[string]string{"direction": "up"})
	if response.StatusCode != http.StatusNotFound || responseErrorCode(t, response) != "not_found" {
		t.Fatalf("POST move missing prompt status = %d, want 404 not_found", response.StatusCode)
	}
	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/runs/run-missing/prompts/ap-1/steer", map[string]bool{"steer": true})
	if response.StatusCode != http.StatusNotFound || responseErrorCode(t, response) != "not_found" {
		t.Fatalf("POST steer missing run status = %d, want 404 not_found", response.StatusCode)
	}

	response = doJSONRequest(t, http.MethodDelete, server.URL+"/api/runs/"+run.ID, nil)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE active run status = %d body=%s", response.StatusCode, readBody(response))
	}
	response.Body.Close()
	waitForManagedRunTerminal(t, managed)
}

func TestServerSessionArchiveAndRemoveCascade(t *testing.T) {
	server, service, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
	_, session := createWebProjectAndSession(t, server)

	// Child sessions are normally spawned by the agent at runtime; create the
	// lineage directly through the service.
	child, err := service.CreateSession(session.ProjectID, execution.SessionCreateMetadata{
		DisplayName: "Child", ParentSessionID: session.ID, CreatedCWD: session.CreatedCWD,
		Provider: "fake", ModelProfile: "default", ModelID: "model",
	})
	if err != nil {
		t.Fatalf("CreateSession(child) error = %v", err)
	}
	grandchild, err := service.CreateSession(session.ProjectID, execution.SessionCreateMetadata{
		DisplayName: "Grandchild", ParentSessionID: child.ID, CreatedCWD: session.CreatedCWD,
		Provider: "fake", ModelProfile: "default", ModelID: "model",
	})
	if err != nil {
		t.Fatalf("CreateSession(grandchild) error = %v", err)
	}

	// Archiving the root cascades to every descendant.
	response := doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/"+session.ID+"/archive", map[string]string{})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST archive status = %d body=%s", response.StatusCode, readBody(response))
	}
	response.Body.Close()
	for _, id := range []string{session.ID, child.ID, grandchild.ID} {
		stored, err := service.GetSession(id)
		if err != nil || !stored.Archived {
			t.Fatalf("GetSession(%s) archived = %v, err = %v; want archived after cascade", id, stored.Archived, err)
		}
	}

	// Removing the root deletes the whole subtree.
	response = doJSONRequest(t, http.MethodDelete, server.URL+"/api/sessions/"+session.ID, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("DELETE session status = %d body=%s", response.StatusCode, readBody(response))
	}
	response.Body.Close()
	for _, id := range []string{session.ID, child.ID, grandchild.ID} {
		response = doJSONRequest(t, http.MethodGet, server.URL+"/api/sessions/"+id, nil)
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("GET removed session %s status = %d, want 404", id, response.StatusCode)
		}
		response.Body.Close()
	}
}

func TestServerProviderManagementDiscoveryAndCodexErrors(t *testing.T) {
	type observedRequest struct {
		Path          string
		Authorization string
	}
	requests := make(chan observedRequest, 1)
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- observedRequest{Path: r.URL.Path, Authorization: r.Header.Get("Authorization")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-z"},{"id":"model-a"},{"id":"model-a"}]}`))
	}))
	defer providerServer.Close()
	server, _ := newWebTestServer(t)

	input := execution.ProviderSettingsInput{
		Name:    "remote",
		BaseURL: providerServer.URL + "/v1",
		APIKey:  "remote-secret",
		Models: []execution.ProviderModelSettings{{
			Profile: "main",
			ID:      "configured-model",
			Type:    "openai-chat",
			Input:   []string{"text"},
		}},
	}
	response := doJSONRequest(t, http.MethodPost, server.URL+"/api/providers", input)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("POST provider status = %d body=%s", response.StatusCode, readBody(response))
	}
	var document execution.ProviderSettingsDocument
	decodeResponse(t, response, &document)
	remote := providerSettingsByName(document.Providers, "remote")
	if remote == nil || remote.APIKey != "" || !remote.APIKeyConfigured {
		t.Fatalf("POST provider document = %#v, want hidden configured remote key", document)
	}

	response = doJSONRequest(t, http.MethodPatch, server.URL+"/api/provider-default", map[string]string{"provider": "remote", "model": "main"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PATCH provider default status = %d body=%s", response.StatusCode, readBody(response))
	}
	decodeResponse(t, response, &document)
	if document.DefaultProvider != "remote" || document.DefaultModel != "main" {
		t.Fatalf("PATCH provider default = %q/%q, want remote/main", document.DefaultProvider, document.DefaultModel)
	}

	response = doJSONRequest(t, http.MethodGet, server.URL+"/api/providers/remote/models", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET discovered models status = %d body=%s", response.StatusCode, readBody(response))
	}
	var modelsPayload struct {
		Models []string `json:"models"`
	}
	decodeResponse(t, response, &modelsPayload)
	if !reflect.DeepEqual(modelsPayload.Models, []string{"model-a", "model-z"}) {
		t.Fatalf("GET discovered models = %#v, want sorted unique models", modelsPayload.Models)
	}
	select {
	case request := <-requests:
		if request.Path != "/v1/models" || request.Authorization != "Bearer remote-secret" {
			t.Fatalf("model discovery request = %#v, want authenticated /v1/models", request)
		}
	case <-time.After(time.Second):
		t.Fatal("provider did not receive model discovery request")
	}

	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/providers", input)
	if response.StatusCode != http.StatusUnprocessableEntity || responseErrorCode(t, response) != "request_failed" {
		t.Fatalf("POST duplicate provider status = %d, want 422 request_failed", response.StatusCode)
	}
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		response = doJSONRequest(t, method, server.URL+"/api/providers/fake/codex-login", map[string]string{})
		if response.StatusCode != http.StatusUnprocessableEntity || responseErrorCode(t, response) != "request_failed" {
			t.Fatalf("%s non-Codex login status = %d, want 422 request_failed", method, response.StatusCode)
		}
	}
}

func createWebProjectAndSession(t *testing.T, server *httptest.Server) (execution.Project, execution.SessionDetail) {
	t.Helper()
	response := doJSONRequest(t, http.MethodPost, server.URL+"/api/projects", map[string]string{"root": t.TempDir(), "display_name": "Behavior Test"})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("POST project status = %d body=%s", response.StatusCode, readBody(response))
	}
	var projectResult execution.ProjectCreateResult
	decodeResponse(t, response, &projectResult)
	response = doJSONRequest(t, http.MethodPost, server.URL+"/api/projects/"+projectResult.Project.ID+"/sessions", map[string]string{})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("POST session status = %d body=%s", response.StatusCode, readBody(response))
	}
	var session execution.SessionDetail
	decodeResponse(t, response, &session)
	return projectResult.Project, session
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

type queuedPromptPayload struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Steer   bool   `json:"steer"`
}

func waitForPromptQueue(t *testing.T, managed *managedRun, count int) []queuedPromptPayload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, _, _, _, changed := managed.snapshot(0)
		for index := len(events) - 1; index >= 0; index-- {
			var event struct {
				Type    string                `json:"type"`
				Prompts []queuedPromptPayload `json:"prompts"`
			}
			if json.Unmarshal(events[index].Payload, &event) == nil && event.Type == "run.prompt_queue" && len(event.Prompts) == count {
				return event.Prompts
			}
		}
		select {
		case <-changed:
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("run prompt queue did not reach %d items", count)
	return nil
}

// waitForPromptQueueOrder polls until the newest run.prompt_queue snapshot
// holds exactly the wanted contents in order, returning the full payload.
func waitForPromptQueueOrder(t *testing.T, managed *managedRun, want []string) []queuedPromptPayload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, _, _, _, changed := managed.snapshot(0)
		for index := len(events) - 1; index >= 0; index-- {
			var event struct {
				Type    string                `json:"type"`
				Prompts []queuedPromptPayload `json:"prompts"`
			}
			if json.Unmarshal(events[index].Payload, &event) != nil || event.Type != "run.prompt_queue" || len(event.Prompts) != len(want) {
				continue
			}
			matches := true
			for i, content := range want {
				if event.Prompts[i].Content != content {
					matches = false
					break
				}
			}
			if matches {
				return event.Prompts
			}
			break
		}
		select {
		case <-changed:
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("run prompt queue order did not become %#v", want)
	return nil
}

func waitForManagedRunTerminal(t *testing.T, managed *managedRun) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, terminal, _, _, changed := managed.snapshot(0)
		if terminal {
			return
		}
		select {
		case <-changed:
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("managed run did not become terminal")
}

func providerSettingsByName(providers []execution.ProviderSettings, name string) *execution.ProviderSettings {
	for index := range providers {
		if providers[index].Name == name {
			return &providers[index]
		}
	}
	return nil
}

var _ execution.SessionIncrementalSupporter = enteredBlockingWebTestRunner{}
var _ execution.SessionTurnRunner = enteredBlockingWebTestRunner{}

func TestServerSessionSnapshot(t *testing.T) {
	server, service := newWebTestServer(t)
	root := t.TempDir()

	// Create project + session + run (to produce durable items).
	created := doJSONRequest(t, http.MethodPost, server.URL+"/api/projects", map[string]string{
		"root":         root,
		"display_name": "Snapshot Test",
	})
	var projectResult execution.ProjectCreateResult
	decodeResponse(t, created, &projectResult)

	created = doJSONRequest(t, http.MethodPost, server.URL+"/api/projects/"+projectResult.Project.ID+"/sessions", map[string]string{
		"provider":      "fake",
		"model_profile": "precise",
	})
	var session execution.SessionDetail
	decodeResponse(t, created, &session)

	// Start a run to produce durable items.
	created = doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/"+session.ID+"/runs", map[string]string{"content": "hi"})
	var run struct {
		ID string `json:"run_id"`
	}
	decodeResponse(t, created, &run)

	// Wait for the run to settle by polling items, then add a small delay
	// to avoid racing with the session store's final metadata write on Windows.
	deadline := time.Now().Add(5 * time.Second)
	var chatPage execution.SessionItemsPage
	for time.Now().Before(deadline) {
		chatPage, _ = service.GetSessionChatItems(session.ID)
		if len(chatPage.Items) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(chatPage.Items) < 2 {
		t.Fatalf("expected at least 2 chat items, got %d", len(chatPage.Items))
	}
	time.Sleep(50 * time.Millisecond)

	// Fetch the snapshot (retry once to tolerate transient file-lock on Windows).
	var snapshot execution.SessionSnapshot
	for attempt := 0; attempt < 3; attempt++ {
		response := doJSONRequest(t, http.MethodGet, server.URL+"/api/sessions/"+session.ID+"/snapshot", nil)
		if response.StatusCode == http.StatusOK {
			decodeResponse(t, response, &snapshot)
			break
		}
		if attempt == 2 {
			t.Fatalf("GET snapshot status = %d body=%s", response.StatusCode, readBody(response))
		}
		response.Body.Close()
		time.Sleep(50 * time.Millisecond)
	}

	// Verify identity.
	if snapshot.SessionID != session.ID {
		t.Fatalf("snapshot session_id = %q, want %q", snapshot.SessionID, session.ID)
	}

	// Verify revision = LastSeq (as decimal string).
	detail, err := service.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	wantRevision := strconv.FormatInt(detail.LastSeq, 10)
	if snapshot.Revision != wantRevision {
		t.Fatalf("snapshot revision = %q, want %q (LastSeq=%d)", snapshot.Revision, wantRevision, detail.LastSeq)
	}

	// Verify session detail matches.
	if snapshot.Session.ID != session.ID {
		t.Fatalf("snapshot session.id = %q, want %q", snapshot.Session.ID, session.ID)
	}
	if snapshot.Session.LastSeq != detail.LastSeq {
		t.Fatalf("snapshot session.last_seq = %d, want %d", snapshot.Session.LastSeq, detail.LastSeq)
	}
	if snapshot.Session.Revision != wantRevision || detail.Revision != wantRevision {
		t.Fatalf("session revisions = snapshot %q/detail %q, want %q", snapshot.Session.Revision, detail.Revision, wantRevision)
	}

	// Verify history matches GetSessionChatItems.
	if len(snapshot.History.Items) != len(chatPage.Items) {
		t.Fatalf("snapshot history items = %d, want %d", len(snapshot.History.Items), len(chatPage.Items))
	}
	if snapshot.History.NewestSeq != chatPage.NewestSeq {
		t.Fatalf("snapshot history newest_seq = %d, want %d", snapshot.History.NewestSeq, chatPage.NewestSeq)
	}

	// Verify revision is a string (not a number) in raw JSON.
	var rawBody string
	for attempt := 0; attempt < 3; attempt++ {
		rawResponse := doJSONRequest(t, http.MethodGet, server.URL+"/api/sessions/"+session.ID+"/snapshot", nil)
		rawBody = readBody(rawResponse)
		if strings.Contains(rawBody, `"revision":"`) {
			break
		}
		if attempt == 2 {
			t.Fatalf("snapshot raw JSON does not contain revision as string: %s", rawBody)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(rawBody, `"revision":"`+wantRevision+`"`) {
		t.Fatalf("snapshot raw JSON does not contain revision as string %q: %s", wantRevision, rawBody)
	}

	// Verify snapshot for non-existent session returns not_found.
	notFoundResponse := doJSONRequest(t, http.MethodGet, server.URL+"/api/sessions/nonexistent/snapshot", nil)
	if notFoundResponse.StatusCode != http.StatusNotFound || responseErrorCode(t, notFoundResponse) != "not_found" {
		t.Fatalf("GET snapshot nonexistent status = %d, want 404 not_found", notFoundResponse.StatusCode)
	}
}
