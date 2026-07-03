package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestSessionMetadataAPIsListDetailNoItemsAndServerCount(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	first := sessions.SessionV2{
		ID:              "session-one",
		CreatedAt:       time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC),
		Provider:        "paperhub",
		ModelProfile:    "glm-5.2-fast",
		ModelID:         "glm-5.2",
		ModelParameters: map[string]any{"temperature": 0.2},
		CWD:             `F:\work\simple-agent`,
		ConfigPath:      filepath.Join(root, "..", "sai.yaml"),
		Context: contextwindow.Metadata{
			ContextWindow:           128000,
			ContextWindowSource:     string(contextwindow.WindowSourceConfigured),
			WarningThresholdPercent: contextwindow.WarningThresholdPercent,
			LastRequestTokens:       1000,
		},
		SaveToolResults: true,
	}
	if _, err := store.SaveMetadata(first); err != nil {
		t.Fatalf("SaveMetadata(first) error = %v", err)
	}
	if _, err := store.AppendItem("session-one", sessions.SessionItem{
		ID:         "item-1",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "SECRET ITEM CONTENT"},
	}); err != nil {
		t.Fatalf("AppendItem() error = %v", err)
	}
	if _, err := store.SaveMetadata(sessions.SessionV2{
		ID:              "session-two",
		Provider:        "openai",
		ModelProfile:    "default",
		ModelID:         "gpt-5.1",
		SaveToolResults: true,
	}); err != nil {
		t.Fatalf("SaveMetadata(second) error = %v", err)
	}

	process := startSessionAPIServer(t, store, sessions.SessionV2{})
	baseURL := "http://" + process.Addr()

	serverInfo := getJSON(t, baseURL+"/server")
	if got := serverInfo["session_count"]; got != float64(2) {
		t.Fatalf("/server session_count = %#v, want 2", got)
	}
	if got := serverInfo["running_turns"]; got != float64(0) {
		t.Fatalf("/server running_turns = %#v, want 0", got)
	}

	listRaw, listBody := getRawJSON(t, baseURL+"/sessions")
	assertNoSessionTimelineLeak(t, listRaw)
	listSessions := listBody["sessions"].([]any)
	infos, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	gotIDs := make([]string, 0, len(listSessions))
	wantIDs := make([]string, 0, len(infos))
	for _, item := range listSessions {
		session := item.(map[string]any)
		gotIDs = append(gotIDs, session["id"].(string))
		if _, ok := session["items"]; ok {
			t.Fatalf("list session includes items: %#v", session)
		}
		if _, ok := session["messages"]; ok {
			t.Fatalf("list session includes messages: %#v", session)
		}
		if session["id"] == "session-one" && session["last_seq"] != float64(1) {
			t.Fatalf("session-one last_seq = %#v, want 1", session["last_seq"])
		}
	}
	for _, info := range infos {
		wantIDs = append(wantIDs, info.ID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("list IDs = %#v, want store order %#v", gotIDs, wantIDs)
	}

	detailRaw, detail := getRawJSON(t, baseURL+"/sessions/session-one")
	assertNoSessionTimelineLeak(t, detailRaw)
	if detail["id"] != "session-one" || detail["status"] != "idle" || detail["last_seq"] != float64(1) {
		t.Fatalf("detail = %#v, want idle session-one last_seq 1", detail)
	}
	context := detail["context"].(map[string]any)
	if context["context_window"] != float64(128000) || context["last_request_tokens"] != float64(1000) {
		t.Fatalf("detail context = %#v, want context metadata", context)
	}
	if _, ok := detail["items"]; ok {
		t.Fatalf("detail includes items: %#v", detail)
	}
	if _, ok := detail["messages"]; ok {
		t.Fatalf("detail includes messages: %#v", detail)
	}
}

func TestSessionCreateUsesDefaultsAndDoesNotPersistItems(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	defaults := sessions.SessionV2{
		Provider:        "codex-work",
		ModelProfile:    "gpt-5.5",
		ModelID:         "gpt-5.5-real",
		ModelParameters: map[string]any{"store": false, "temperature": 0.1},
		CWD:             `F:\work\simple-agent`,
		ConfigPath:      filepath.Join(root, "..", "sai.yaml"),
		EnabledTools:    []string{"read_file"},
		EnabledMCP:      []string{"local"},
		EnabledSkills:   []string{"reviewer"},
		ShowReasoning:   true,
		Context: contextwindow.Metadata{
			ContextWindow:           400000,
			ContextWindowSource:     string(contextwindow.WindowSourceConfigured),
			WarningThresholdPercent: contextwindow.WarningThresholdPercent,
		},
		SaveToolResults: false,
	}
	process := startSessionAPIServer(t, store, defaults)

	createdRaw, created := postRawJSON(t, "http://"+process.Addr()+"/sessions", "")
	assertNoSessionTimelineLeak(t, createdRaw)
	if created["id"] == "" {
		t.Fatalf("created response missing id: %#v", created)
	}
	for key, want := range map[string]any{
		"provider":          "codex-work",
		"model_profile":     "gpt-5.5",
		"model_id":          "gpt-5.5-real",
		"status":            "idle",
		"last_seq":          float64(0),
		"show_reasoning":    true,
		"save_tool_results": true,
	} {
		if got := created[key]; got != want {
			t.Fatalf("created[%s] = %#v, want %#v in %#v", key, got, want, created)
		}
	}
	context := created["context"].(map[string]any)
	if context["context_window"] != float64(400000) {
		t.Fatalf("created context = %#v, want context window", context)
	}
	params := created["model_parameters"].(map[string]any)
	if params["store"] != false || params["temperature"] != 0.1 {
		t.Fatalf("created model_parameters = %#v, want defaults", params)
	}

	id := created["id"].(string)
	session, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load(created) error = %v", err)
	}
	if session.Provider != "codex-work" || session.ModelProfile != "gpt-5.5" || session.ModelID != "gpt-5.5-real" {
		t.Fatalf("stored model metadata = %#v", session)
	}
	if len(session.Items) != 0 || len(session.ActiveHistory) != 0 || session.LastSeq != 0 {
		t.Fatalf("stored session has timeline state: items=%#v active=%#v last_seq=%d", session.Items, session.ActiveHistory, session.LastSeq)
	}
	if !session.SaveToolResults {
		t.Fatal("stored SaveToolResults = false, want true")
	}

	_, second := postRawJSON(t, "http://"+process.Addr()+"/sessions", "{}")
	if second["id"] == "" || second["id"] == id {
		t.Fatalf("second create response = %#v, want distinct id", second)
	}
}

func TestSessionMetadataStructuredErrors(t *testing.T) {
	process := startSessionAPIServer(t, sessions.NewV2Store(filepath.Join(t.TempDir(), "sessions")), sessions.SessionV2{})
	baseURL := "http://" + process.Addr()

	resp, err := http.Get(baseURL + "/sessions/missing-session")
	if err != nil {
		t.Fatalf("Get(missing) error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	if got := body["error"].(map[string]any)["code"]; got != "session_not_found" {
		t.Fatalf("missing error = %#v, want session_not_found", body)
	}

	req, err := http.NewRequest(http.MethodPut, baseURL+"/sessions", nil)
	if err != nil {
		t.Fatalf("NewRequest(PUT /sessions) error = %v", err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /sessions error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, POST" {
		t.Fatalf("PUT /sessions status/allow = %d/%q, want 405 GET, POST", resp.StatusCode, resp.Header.Get("Allow"))
	}
	body = decodeJSON(t, resp)
	if got := body["error"].(map[string]any)["code"]; got != "method_not_allowed" {
		t.Fatalf("PUT /sessions error = %#v, want method_not_allowed", body)
	}

	resp, err = http.Post(baseURL+"/sessions/missing-session", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /sessions/id error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET" {
		t.Fatalf("POST /sessions/id status/allow = %d/%q, want 405 GET", resp.StatusCode, resp.Header.Get("Allow"))
	}
	body = decodeJSON(t, resp)
	if got := body["error"].(map[string]any)["code"]; got != "method_not_allowed" {
		t.Fatalf("POST /sessions/id error = %#v, want method_not_allowed", body)
	}

	resp, err = http.Get(baseURL + "/sessions/missing-session/items")
	if err != nil {
		t.Fatalf("GET bad path error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bad path status = %d, want 404", resp.StatusCode)
	}
	body = decodeJSON(t, resp)
	if got := body["error"].(map[string]any)["code"]; got != "not_found" {
		t.Fatalf("bad path error = %#v, want not_found", body)
	}
}

func TestSessionDetailCorruptStoreErrorIsStructured5xx(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(filepath.Join(root, "corrupt-session"), 0o755); err != nil {
		t.Fatalf("MkdirAll(corrupt-session) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "corrupt-session", "meta.json"), []byte(`{"id":"corrupt-session",`), 0o600); err != nil {
		t.Fatalf("WriteFile(meta.json) error = %v", err)
	}
	process := startSessionAPIServer(t, sessions.NewV2Store(root), sessions.SessionV2{})

	resp, err := http.Get("http://" + process.Addr() + "/sessions/corrupt-session")
	if err != nil {
		t.Fatalf("Get(corrupt session) error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("corrupt session status = %d, want 500", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	if got := body["error"].(map[string]any)["code"]; got != "session_store_error" {
		t.Fatalf("corrupt session error = %#v, want session_store_error", body)
	}
}

func startSessionAPIServer(t *testing.T, store *sessions.V2Store, defaults sessions.SessionV2) *Process {
	t.Helper()

	process, err := Start(Options{
		CWD:             t.TempDir(),
		ConfigPath:      filepath.Join(t.TempDir(), "sai.yaml"),
		Listen:          "127.0.0.1:0",
		Version:         "test-version",
		SessionStore:    store,
		SessionDefaults: defaults,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- process.Serve(context.Background())
	}()
	waitForHealthyServer(t, process.Addr())
	t.Cleanup(func() {
		_ = process.Shutdown(context.Background())
		select {
		case err := <-serveDone:
			if err != nil {
				t.Fatalf("Serve() error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Serve() did not stop")
		}
	})
	return process
}

func getRawJSON(t *testing.T, url string) ([]byte, map[string]any) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Get(%s) status = %d, want 200", url, resp.StatusCode)
	}
	return readRawJSON(t, resp)
}

func postRawJSON(t *testing.T, url, body string) ([]byte, map[string]any) {
	t.Helper()

	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("Post(%s) error = %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("Post(%s) status = %d, want 201", url, resp.StatusCode)
	}
	return readRawJSON(t, resp)
}

func readRawJSON(t *testing.T, resp *http.Response) ([]byte, map[string]any) {
	t.Helper()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll(%s) error = %v", resp.Request.URL, err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v; body=%s", resp.Request.URL, err, raw)
	}
	return raw, body
}

func assertNoSessionTimelineLeak(t *testing.T, raw []byte) {
	t.Helper()

	for _, forbidden := range [][]byte{
		[]byte("SECRET ITEM CONTENT"),
		[]byte(`"items"`),
		[]byte(`"messages"`),
		[]byte(`"tool_results"`),
	} {
		if bytes.Contains(raw, forbidden) {
			t.Fatalf("metadata response leaked %s: %s", forbidden, raw)
		}
	}
}
