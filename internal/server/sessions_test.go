package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/sessionprojector"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestSessionMetadataAPIsListDetailNoItemsAndServerCount(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	first := sessions.SessionV2{
		ID:              "session-one",
		CreatedAt:       time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC),
		DisplayName:     "Session One",
		LastUsedAt:      time.Date(2026, 7, 3, 12, 30, 0, 0, time.UTC),
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

	for _, endpoint := range []string{
		"/sessions",
		"/sessions/session-one",
		"/sessions/session-one/items",
		"/sessions/session-one/items/item-1/content",
		"/sessions/session-one/content/" + strings.Repeat("0", 64),
	} {
		raw, body := getRawJSONStatus(t, baseURL+endpoint, "", http.StatusForbidden)
		assertErrorCode(t, body, "permission_denied")
		if bytes.Contains(raw, []byte("registry-token")) || bytes.Contains(raw, []byte("SECRET ITEM CONTENT")) {
			t.Fatalf("permission error for %s leaked sensitive content: %s", endpoint, raw)
		}
	}

	serverInfo := getJSONWithToken(t, baseURL+"/server", "registry-token")
	if got := serverInfo["session_count"]; got != float64(2) {
		t.Fatalf("/server session_count = %#v, want 2", got)
	}
	if got := serverInfo["project_count"]; got != float64(0) {
		t.Fatalf("/server project_count = %#v, want 0", got)
	}
	if got := serverInfo["running_turns"]; got != float64(0) {
		t.Fatalf("/server running_turns = %#v, want 0", got)
	}

	_, bareListBody := getRawJSONStatus(t, baseURL+"/sessions", "registry-token", http.StatusBadRequest)
	assertErrorCode(t, bareListBody, "invalid_query")

	listRaw, listBody := getRawJSON(t, baseURL+"/sessions?all_projects=true")
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
		if session["id"] == "session-one" {
			if session["display_name"] != "Session One" || session["archived"] != false || session["last_used_at"] == nil {
				t.Fatalf("session-one lifecycle metadata = %#v, want display_name/archived/last_used_at", session)
			}
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
	if detail["display_name"] != "Session One" || detail["archived"] != false || detail["last_used_at"] == nil {
		t.Fatalf("detail lifecycle metadata = %#v, want display_name/archived/last_used_at", detail)
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
	projectStore := projectstore.NewStore(filepath.Join(t.TempDir(), "projects"))
	projectRoot := t.TempDir()
	project, _, err := projectStore.Create(projectRoot, "Repo")
	if err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
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
	process := startProjectAPIServerWithSessions(t, projectStore, store, "registry-token", nil)
	process.sessionDefaults = defaults
	createURL := "http://" + process.Addr() + "/projects/" + project.ID + "/sessions"

	createdRaw, created := postRawJSONWithToken(t, createURL, "", "registry-token")
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

	_, second := postRawJSONWithToken(t, createURL, "{}", "registry-token")
	if second["id"] == "" || second["id"] == id {
		t.Fatalf("second create response = %#v, want distinct id", second)
	}
}

func TestSessionCreateRequiresRegistryToken(t *testing.T) {
	store := sessions.NewV2Store(filepath.Join(t.TempDir(), "sessions"))
	projectStore := projectstore.NewStore(filepath.Join(t.TempDir(), "projects"))
	projectRoot := t.TempDir()
	project, _, err := projectStore.Create(projectRoot, "Repo")
	if err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	process := startProjectAPIServerWithSessions(t, projectStore, store, "registry-token", nil)
	baseURL := "http://" + process.Addr()
	createURL := baseURL + "/projects/" + project.ID + "/sessions"

	for _, tt := range []struct {
		name  string
		token string
	}{
		{name: "missing token"},
		{name: "wrong token", token: "wrong-token"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, body := postRawJSONStatus(t, createURL, "", tt.token, http.StatusForbidden)
			assertErrorCode(t, body, "permission_denied")
			if bytes.Contains(raw, []byte("registry-token")) {
				t.Fatalf("permission error leaked registry token: %s", raw)
			}
		})
	}

	_, created := postRawJSONWithToken(t, createURL, "", "registry-token")
	if created["id"] == "" {
		t.Fatalf("created response missing id: %#v", created)
	}

	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "non-empty object", body: `{"content":"secret prompt"}`},
		{name: "null", body: `null`},
		{name: "array", body: `[]`},
		{name: "string", body: `"secret prompt"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, body := postRawJSONStatus(t, createURL, tt.body, "registry-token", http.StatusBadRequest)
			assertErrorCode(t, body, "invalid_request")
			if bytes.Contains(raw, []byte("secret prompt")) || bytes.Contains(raw, []byte("registry-token")) {
				t.Fatalf("invalid create response leaked secret: %s", raw)
			}
		})
	}

	infos, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("len(List()) = %d, want only the valid created session after invalid bodies", len(infos))
	}
}

func TestSessionMetadataUpdateRenameArchiveAuthBusyAndPreservesTimeline(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	lastUsedAt := time.Date(2026, 7, 4, 11, 1, 0, 0, time.UTC)
	if _, err := store.SaveMetadata(sessions.SessionV2{
		ID:           "update-session",
		LastUsedAt:   lastUsedAt,
		Provider:     "codex",
		ModelProfile: "default",
		ModelID:      "gpt-5",
	}); err != nil {
		t.Fatalf("SaveMetadata(update-session) error = %v", err)
	}
	item := appendServerTestItem(t, store, "update-session", sessions.SessionItem{
		ID:         "item-1",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "timeline secret"},
	})
	if _, err := store.ReplaceActiveHistory("update-session", []string{item.ID}); err != nil {
		t.Fatalf("ReplaceActiveHistory(update-session) error = %v", err)
	}
	if _, err := store.AppendCompaction("update-session", sessions.CompactionCheckpoint{
		ID:                 "compact-1",
		SummaryItemID:      "summary-1",
		ReplacementHistory: []string{item.ID},
	}); err != nil {
		t.Fatalf("AppendCompaction(update-session) error = %v", err)
	}
	process := startSessionAPIServerWithToken(t, store, sessions.SessionV2{}, "registry-token")
	baseURL := "http://" + process.Addr()

	raw, body := patchRawJSONStatus(t, baseURL+"/sessions/update-session", `{"display_name":"No Token"}`, "", http.StatusForbidden)
	assertErrorCode(t, body, "permission_denied")
	if bytes.Contains(raw, []byte("registry-token")) || bytes.Contains(raw, []byte("timeline secret")) {
		t.Fatalf("permission error leaked sensitive content: %s", raw)
	}

	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "empty body", body: ""},
		{name: "empty object", body: "{}"},
		{name: "blank display name", body: `{"display_name":"   "}`},
		{name: "invalid archived", body: `{"archived":"true"}`},
		{name: "unsupported field", body: `{"name":"Wrong"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, body := patchRawJSONStatus(t, baseURL+"/sessions/update-session", tt.body, "registry-token", http.StatusBadRequest)
			assertErrorCode(t, body, "invalid_request")
			if bytes.Contains(raw, []byte("registry-token")) || bytes.Contains(raw, []byte("timeline secret")) {
				t.Fatalf("invalid update response leaked sensitive content: %s", raw)
			}
		})
	}

	renamed, err := RenameSessionWithToken(context.Background(), process.Addr(), "registry-token", "update-session", "Renamed Session", 2*time.Second)
	if err != nil {
		t.Fatalf("RenameSessionWithToken() error = %v", err)
	}
	if renamed.DisplayName != "Renamed Session" || renamed.Archived || !renamed.LastUsedAt.Equal(lastUsedAt) {
		t.Fatalf("renamed session = %#v, want display name, non-archived, stable last_used_at", renamed)
	}
	archived, err := ArchiveSessionWithToken(context.Background(), process.Addr(), "registry-token", "update-session", 2*time.Second)
	if err != nil {
		t.Fatalf("ArchiveSessionWithToken() error = %v", err)
	}
	if archived.DisplayName != "Renamed Session" || !archived.Archived || !archived.LastUsedAt.Equal(lastUsedAt) {
		t.Fatalf("archived session = %#v, want archived with stable metadata", archived)
	}

	stored, err := store.Load("update-session")
	if err != nil {
		t.Fatalf("Load(update-session) error = %v", err)
	}
	if got, want := responseSessionItemIDs(stored.Items), []string{"item-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stored item IDs = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(stored.ActiveHistory, []string{"item-1"}) || len(stored.Compactions) != 1 || stored.Compactions[0].ID != "compact-1" {
		t.Fatalf("stored timeline state = active %#v compactions %#v, want preserved", stored.ActiveHistory, stored.Compactions)
	}
	if !stored.LastUsedAt.Equal(lastUsedAt) {
		t.Fatalf("stored LastUsedAt = %s, want %s", stored.LastUsedAt, lastUsedAt)
	}

	_, cancelBusy := context.WithCancel(context.Background())
	if got := process.beginSessionTurn("update-session", "turn-busy", cancelBusy); got != beginTurnStarted {
		cancelBusy()
		t.Fatalf("beginSessionTurn(update-session) = %v, want started", got)
	}
	raw, body = patchRawJSONStatus(t, baseURL+"/sessions/update-session", `{"display_name":"Busy Rename"}`, "registry-token", http.StatusConflict)
	process.endSessionTurn("update-session")
	cancelBusy()
	assertErrorCode(t, body, "session_busy")
	if bytes.Contains(raw, []byte("Busy Rename")) || bytes.Contains(raw, []byte("timeline secret")) {
		t.Fatalf("busy response leaked update or timeline content: %s", raw)
	}
}

func TestProjectSessionAPIsCreateListFilterAndClient(t *testing.T) {
	projectStore := projectstore.NewStore(filepath.Join(t.TempDir(), "projects"))
	sessionStore := sessions.NewV2Store(filepath.Join(t.TempDir(), "sessions"))
	projectOne, _, err := projectStore.Create(mkdirServerTestDir(t, "project-one"), "Project One")
	if err != nil {
		t.Fatalf("Create(project one) error = %v", err)
	}
	projectTwo, _, err := projectStore.Create(mkdirServerTestDir(t, "project-two"), "Project Two")
	if err != nil {
		t.Fatalf("Create(project two) error = %v", err)
	}
	defaults := sessions.SessionV2{
		Provider:      "codex-work",
		ModelProfile:  "gpt-5.5",
		ModelID:       "gpt-5.5-real",
		CWD:           mkdirServerTestDir(t, "created-cwd"),
		ConfigPath:    filepath.Join(t.TempDir(), "sai.yaml"),
		EnabledTools:  []string{"read_file"},
		EnabledSkills: []string{"reviewer"},
	}
	if _, err := sessionStore.SaveMetadata(sessions.SessionV2{
		ID:              "project-one-old",
		ProjectID:       projectOne.ID,
		CreatedCWD:      defaults.CWD,
		Provider:        "codex-work",
		ModelProfile:    "gpt-5.5",
		ModelID:         "gpt-5.5-real",
		CWD:             defaults.CWD,
		SaveToolResults: true,
	}); err != nil {
		t.Fatalf("SaveMetadata(project-one-old) error = %v", err)
	}
	appendServerTestItem(t, sessionStore, "project-one-old", sessions.SessionItem{
		ID:         "item-1",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "project one"},
	})
	if _, err := sessionStore.SaveMetadata(sessions.SessionV2{
		ID:              "project-two-old",
		ProjectID:       projectTwo.ID,
		CreatedCWD:      defaults.CWD,
		Provider:        "codex-work",
		ModelProfile:    "gpt-5.5",
		ModelID:         "gpt-5.5-real",
		CWD:             defaults.CWD,
		SaveToolResults: true,
	}); err != nil {
		t.Fatalf("SaveMetadata(project-two-old) error = %v", err)
	}
	if _, err := sessionStore.SaveMetadata(sessions.SessionV2{
		ID:              "legacy-without-project",
		Provider:        "codex-work",
		ModelProfile:    "gpt-5.5",
		ModelID:         "gpt-5.5-real",
		CWD:             defaults.CWD,
		SaveToolResults: true,
	}); err != nil {
		t.Fatalf("SaveMetadata(legacy-without-project) error = %v", err)
	}

	process := startProjectSessionAPIServer(t, projectStore, sessionStore, defaults, "registry-token")
	baseURL := "http://" + process.Addr()

	raw, body := getRawJSONStatus(t, baseURL+"/projects/"+projectOne.ID+"/sessions", "", http.StatusForbidden)
	assertErrorCode(t, body, "permission_denied")
	if stringContainsAny(raw, "registry-token", projectOne.Root) {
		t.Fatalf("permission error leaked token or project root: %s", raw)
	}
	raw, body = postRawJSONStatus(t, baseURL+"/projects/"+projectOne.ID+"/sessions", "", "", http.StatusForbidden)
	assertErrorCode(t, body, "permission_denied")
	if stringContainsAny(raw, "registry-token", projectOne.Root) {
		t.Fatalf("permission error leaked token or project root: %s", raw)
	}

	listed, err := ListProjectSessionsWithToken(context.Background(), process.Addr(), "registry-token", projectOne.ID, 2*time.Second)
	if err != nil {
		t.Fatalf("ListProjectSessionsWithToken() error = %v", err)
	}
	if got := sessionMetadataIDs(listed); !reflect.DeepEqual(got, []string{"project-one-old"}) {
		t.Fatalf("project one sessions before create = %#v, want project-one-old only", got)
	}
	if listed[0].ProjectID != projectOne.ID || listed[0].CreatedCWD != defaults.CWD || listed[0].LastSeq != 1 {
		t.Fatalf("project one metadata = %#v, want project identity and last_seq", listed[0])
	}

	created, err := CreateProjectSessionWithToken(context.Background(), process.Addr(), "registry-token", projectOne.ID, 2*time.Second)
	if err != nil {
		t.Fatalf("CreateProjectSessionWithToken() error = %v", err)
	}
	if created.ID == "" || created.ProjectID != projectOne.ID || created.CreatedCWD != defaults.CWD || created.CWD != defaults.CWD {
		t.Fatalf("created project session = %#v, want project id and created cwd from defaults", created)
	}
	if created.Provider != defaults.Provider || created.ModelProfile != defaults.ModelProfile || created.ModelID != defaults.ModelID {
		t.Fatalf("created model defaults = %#v, want %#v", created, defaults)
	}

	stored, err := sessionStore.Load(created.ID)
	if err != nil {
		t.Fatalf("Load(created project session) error = %v", err)
	}
	if stored.ProjectID != projectOne.ID || stored.CreatedCWD != defaults.CWD || stored.CWD != defaults.CWD {
		t.Fatalf("stored project session identity = %#v, want project id and created cwd", stored)
	}
	if len(stored.Items) != 0 || len(stored.ActiveHistory) != 0 || stored.LastSeq != 0 {
		t.Fatalf("stored project session has timeline state: items=%#v active=%#v last_seq=%d", stored.Items, stored.ActiveHistory, stored.LastSeq)
	}

	listed, err = ListProjectSessionsWithToken(context.Background(), process.Addr(), "registry-token", projectOne.ID, 2*time.Second)
	if err != nil {
		t.Fatalf("ListProjectSessionsWithToken(after create) error = %v", err)
	}
	if got, want := sessionMetadataIDSet(listed), map[string]bool{"project-one-old": true, created.ID: true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("project one sessions after create = %#v, want %#v", got, want)
	}
	for _, item := range listed {
		if item.ProjectID != projectOne.ID {
			t.Fatalf("project one list returned session for project %q: %#v", item.ProjectID, item)
		}
	}
}

func TestSessionListArchivedFilteringGlobalAndProject(t *testing.T) {
	projectStore := projectstore.NewStore(filepath.Join(t.TempDir(), "projects"))
	sessionStore := sessions.NewV2Store(filepath.Join(t.TempDir(), "sessions"))
	projectOne, _, err := projectStore.Create(mkdirServerTestDir(t, "archive-project-one"), "Project One")
	if err != nil {
		t.Fatalf("Create(project one) error = %v", err)
	}
	projectTwo, _, err := projectStore.Create(mkdirServerTestDir(t, "archive-project-two"), "Project Two")
	if err != nil {
		t.Fatalf("Create(project two) error = %v", err)
	}
	lastUsedAt := time.Date(2026, 7, 4, 10, 1, 0, 0, time.UTC)
	for _, session := range []sessions.SessionV2{
		{
			ID:           "project-one-active",
			ProjectID:    projectOne.ID,
			CreatedCWD:   projectOne.Root,
			CWD:          projectOne.Root,
			LastUsedAt:   lastUsedAt,
			Provider:     "codex",
			ModelProfile: "default",
			ModelID:      "gpt-5",
		},
		{
			ID:           "project-one-archived",
			DisplayName:  "Archived One",
			Archived:     true,
			ProjectID:    projectOne.ID,
			CreatedCWD:   projectOne.Root,
			CWD:          projectOne.Root,
			LastUsedAt:   lastUsedAt.Add(time.Minute),
			Provider:     "codex",
			ModelProfile: "default",
			ModelID:      "gpt-5",
		},
		{
			ID:           "project-two-archived",
			Archived:     true,
			ProjectID:    projectTwo.ID,
			CreatedCWD:   projectTwo.Root,
			CWD:          projectTwo.Root,
			LastUsedAt:   lastUsedAt.Add(2 * time.Minute),
			Provider:     "codex",
			ModelProfile: "default",
			ModelID:      "gpt-5",
		},
	} {
		if _, err := sessionStore.SaveMetadata(session); err != nil {
			t.Fatalf("SaveMetadata(%s) error = %v", session.ID, err)
		}
	}
	process := startProjectSessionAPIServer(t, projectStore, sessionStore, sessions.SessionV2{}, "registry-token")

	globalActive, err := ListSessionsWithOptions(context.Background(), process.Addr(), "registry-token", SessionListOptions{}, 2*time.Second)
	if err != nil {
		t.Fatalf("ListSessionsWithOptions(active) error = %v", err)
	}
	if got, want := sessionMetadataIDs(globalActive), []string{"project-one-active"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("global active IDs = %#v, want %#v", got, want)
	}
	globalArchived, err := ListSessionsWithOptions(context.Background(), process.Addr(), "registry-token", SessionListOptions{Archived: true}, 2*time.Second)
	if err != nil {
		t.Fatalf("ListSessionsWithOptions(archived) error = %v", err)
	}
	if got, want := sessionMetadataIDs(globalArchived), []string{"project-two-archived", "project-one-archived"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("global archived IDs = %#v, want %#v", got, want)
	}
	if !globalArchived[1].Archived || globalArchived[1].DisplayName != "Archived One" || globalArchived[1].LastUsedAt.IsZero() {
		t.Fatalf("archived metadata = %#v, want lifecycle fields", globalArchived[1])
	}

	projectActive, err := ListProjectSessionsWithOptions(context.Background(), process.Addr(), "registry-token", projectOne.ID, SessionListOptions{}, 2*time.Second)
	if err != nil {
		t.Fatalf("ListProjectSessionsWithOptions(active) error = %v", err)
	}
	if got, want := sessionMetadataIDs(projectActive), []string{"project-one-active"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("project active IDs = %#v, want %#v", got, want)
	}
	projectArchived, err := ListProjectSessionsWithOptions(context.Background(), process.Addr(), "registry-token", projectOne.ID, SessionListOptions{Archived: true}, 2*time.Second)
	if err != nil {
		t.Fatalf("ListProjectSessionsWithOptions(archived) error = %v", err)
	}
	if got, want := sessionMetadataIDs(projectArchived), []string{"project-one-archived"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("project archived IDs = %#v, want %#v", got, want)
	}
}

func TestProjectSessionCreatePersistsMetadataBody(t *testing.T) {
	projectStore := projectstore.NewStore(filepath.Join(t.TempDir(), "projects"))
	sessionStore := sessions.NewV2Store(filepath.Join(t.TempDir(), "sessions"))
	projectOne, _, err := projectStore.Create(mkdirServerTestDir(t, "project-one"), "Project One")
	if err != nil {
		t.Fatalf("Create(project one) error = %v", err)
	}
	projectTwo, _, err := projectStore.Create(mkdirServerTestDir(t, "project-two"), "Project Two")
	if err != nil {
		t.Fatalf("Create(project two) error = %v", err)
	}

	defaults := sessions.SessionV2{
		Provider:        "default-provider",
		ModelProfile:    "default-profile",
		ModelID:         "default-model",
		ModelParameters: map[string]any{"temperature": 0.1},
		CWD:             mkdirServerTestDir(t, "default-cwd"),
		ConfigPath:      filepath.Join(t.TempDir(), "default.yaml"),
		EnabledTools:    []string{"default_tool"},
		EnabledMCP:      []string{"default-mcp"},
		EnabledSkills:   []string{"default-skill"},
		ShowReasoning:   false,
		Context: contextwindow.Metadata{
			ContextWindow:           32000,
			ContextWindowSource:     string(contextwindow.WindowSourceEstimated),
			WarningThresholdPercent: contextwindow.WarningThresholdPercent,
		},
		SaveToolResults: true,
	}
	process := startProjectSessionAPIServer(t, projectStore, sessionStore, defaults, "registry-token")

	createdCWD := mkdirServerTestDir(t, "metadata-cwd")
	configPath := filepath.Join(t.TempDir(), "metadata.yaml")
	bodyRaw, err := json.Marshal(map[string]any{
		"created_cwd":       createdCWD,
		"config_path":       configPath,
		"provider":          "codex-work",
		"model_profile":     "gpt-5.5",
		"model_id":          "gpt-5.5-real",
		"model_parameters":  map[string]any{"max_output_tokens": 2048, "store": false},
		"enabled_tools":     []string{"read_file", "grep_files"},
		"enabled_mcp":       []string{"local"},
		"enabled_skills":    []string{"reviewer", "go"},
		"show_reasoning":    true,
		"context":           map[string]any{"context_window": 400000, "context_window_source": "configured", "warning_threshold_percent": 70, "last_request_tokens": 1234},
		"save_tool_results": false,
	})
	if err != nil {
		t.Fatalf("Marshal(project session create body) error = %v", err)
	}

	createdRaw, created := postRawJSONStatus(t, "http://"+process.Addr()+"/projects/"+projectOne.ID+"/sessions", string(bodyRaw), "registry-token", http.StatusCreated)
	assertNoSessionTimelineLeak(t, createdRaw)
	if created["id"] == "" {
		t.Fatalf("created response missing id: %#v", created)
	}
	for key, want := range map[string]any{
		"project_id":        projectOne.ID,
		"created_cwd":       createdCWD,
		"cwd":               createdCWD,
		"config_path":       configPath,
		"provider":          "codex-work",
		"model_profile":     "gpt-5.5",
		"model_id":          "gpt-5.5-real",
		"show_reasoning":    true,
		"save_tool_results": false,
	} {
		if got := created[key]; got != want {
			t.Fatalf("created[%s] = %#v, want %#v in %#v", key, got, want, created)
		}
	}
	params := created["model_parameters"].(map[string]any)
	if params["max_output_tokens"] != float64(2048) || params["store"] != false {
		t.Fatalf("created model_parameters = %#v, want metadata body", params)
	}
	if got, want := stringSliceFromJSON(t, created["enabled_tools"]), []string{"read_file", "grep_files"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("created enabled_tools = %#v, want %#v", got, want)
	}
	if got, want := stringSliceFromJSON(t, created["enabled_mcp"]), []string{"local"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("created enabled_mcp = %#v, want %#v", got, want)
	}
	if got, want := stringSliceFromJSON(t, created["enabled_skills"]), []string{"reviewer", "go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("created enabled_skills = %#v, want %#v", got, want)
	}
	context := created["context"].(map[string]any)
	if context["context_window"] != float64(400000) || context["warning_threshold_percent"] != float64(70) || context["last_request_tokens"] != float64(1234) {
		t.Fatalf("created context = %#v, want metadata body", context)
	}

	stored, err := sessionStore.Load(created["id"].(string))
	if err != nil {
		t.Fatalf("Load(created project session) error = %v", err)
	}
	if stored.ProjectID != projectOne.ID || stored.ProjectID == projectTwo.ID {
		t.Fatalf("stored project id = %q, want URL project %q and not body project %q", stored.ProjectID, projectOne.ID, projectTwo.ID)
	}
	if stored.CreatedCWD != createdCWD || stored.CWD != createdCWD || stored.ConfigPath != configPath {
		t.Fatalf("stored paths = cwd %q created_cwd %q config %q, want metadata", stored.CWD, stored.CreatedCWD, stored.ConfigPath)
	}
	if stored.Provider != "codex-work" || stored.ModelProfile != "gpt-5.5" || stored.ModelID != "gpt-5.5-real" {
		t.Fatalf("stored model metadata = %#v, want request metadata", stored)
	}
	if fmt.Sprint(stored.ModelParameters["max_output_tokens"]) != "2048" || stored.ModelParameters["store"] != false {
		t.Fatalf("stored model_parameters = %#v, want request metadata", stored.ModelParameters)
	}
	if !reflect.DeepEqual(stored.EnabledTools, []string{"read_file", "grep_files"}) {
		t.Fatalf("stored enabled_tools = %#v", stored.EnabledTools)
	}
	if !reflect.DeepEqual(stored.EnabledMCP, []string{"local"}) {
		t.Fatalf("stored enabled_mcp = %#v", stored.EnabledMCP)
	}
	if !reflect.DeepEqual(stored.EnabledSkills, []string{"reviewer", "go"}) {
		t.Fatalf("stored enabled_skills = %#v", stored.EnabledSkills)
	}
	if !stored.ShowReasoning {
		t.Fatal("stored show_reasoning = false, want true")
	}
	if stored.Context.ContextWindow != 400000 || stored.Context.WarningThresholdPercent != 70 || stored.Context.LastRequestTokens != 1234 {
		t.Fatalf("stored context = %#v, want request metadata", stored.Context)
	}
	if stored.SaveToolResults {
		t.Fatal("stored save_tool_results = true, want false from request metadata")
	}

	_, defaultCreated := postRawJSONStatus(t, "http://"+process.Addr()+"/projects/"+projectOne.ID+"/sessions", "{}", "registry-token", http.StatusCreated)
	if defaultCreated["project_id"] != projectOne.ID || defaultCreated["provider"] != defaults.Provider || defaultCreated["created_cwd"] != defaults.CWD {
		t.Fatalf("default project session from {} = %#v, want URL project id and server defaults", defaultCreated)
	}
}

func TestProjectSessionListSkipsUnrelatedCorruptSessionBeforeReplay(t *testing.T) {
	projectStore := projectstore.NewStore(filepath.Join(t.TempDir(), "projects"))
	sessionRoot := filepath.Join(t.TempDir(), "sessions")
	sessionStore := sessions.NewV2Store(sessionRoot)
	targetProject, _, err := projectStore.Create(mkdirServerTestDir(t, "target-project"), "Target")
	if err != nil {
		t.Fatalf("Create(target project) error = %v", err)
	}
	otherProject, _, err := projectStore.Create(mkdirServerTestDir(t, "other-project"), "Other")
	if err != nil {
		t.Fatalf("Create(other project) error = %v", err)
	}
	if _, err := sessionStore.SaveMetadata(sessions.SessionV2{
		ID:              "target-session",
		ProjectID:       targetProject.ID,
		CreatedCWD:      targetProject.Root,
		Provider:        "codex-work",
		ModelProfile:    "gpt-5.5",
		ModelID:         "gpt-5.5-real",
		CWD:             targetProject.Root,
		SaveToolResults: true,
	}); err != nil {
		t.Fatalf("SaveMetadata(target-session) error = %v", err)
	}
	appendServerTestItem(t, sessionStore, "target-session", sessions.SessionItem{
		ID:         "target-item",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "target"},
	})
	if _, err := sessionStore.SaveMetadata(sessions.SessionV2{
		ID:              "unrelated-corrupt",
		ProjectID:       otherProject.ID,
		CreatedCWD:      otherProject.Root,
		Provider:        "codex-work",
		ModelProfile:    "gpt-5.5",
		ModelID:         "gpt-5.5-real",
		CWD:             otherProject.Root,
		SaveToolResults: true,
	}); err != nil {
		t.Fatalf("SaveMetadata(unrelated-corrupt) error = %v", err)
	}
	writeCorruptSessionSegmentForServerTest(t, sessionRoot, "unrelated-corrupt")

	process := startProjectSessionAPIServer(t, projectStore, sessionStore, sessions.SessionV2{}, "registry-token")
	listed, err := ListProjectSessionsWithToken(context.Background(), process.Addr(), "registry-token", targetProject.ID, 2*time.Second)
	if err != nil {
		t.Fatalf("ListProjectSessionsWithToken() error = %v", err)
	}
	if got := sessionMetadataIDs(listed); !reflect.DeepEqual(got, []string{"target-session"}) {
		t.Fatalf("target project sessions = %#v, want target-session only", got)
	}
	if listed[0].LastSeq != 1 || listed[0].ProjectID != targetProject.ID {
		t.Fatalf("target project session metadata = %#v, want last_seq and project id", listed[0])
	}
}

func TestProjectSessionAPIsRequireExistingActiveProject(t *testing.T) {
	projectStore := projectstore.NewStore(filepath.Join(t.TempDir(), "projects"))
	sessionStore := sessions.NewV2Store(filepath.Join(t.TempDir(), "sessions"))
	archived, _, err := projectStore.Create(mkdirServerTestDir(t, "archived-project"), "Archived")
	if err != nil {
		t.Fatalf("Create(archived) error = %v", err)
	}
	archiveProjectForServerTest(t, projectStore, archived)

	process := startProjectSessionAPIServer(t, projectStore, sessionStore, sessions.SessionV2{}, "registry-token")
	baseURL := "http://" + process.Addr()

	for _, tt := range []struct {
		name       string
		projectID  string
		wantStatus int
		wantCode   string
	}{
		{name: "missing", projectID: "project-missing", wantStatus: http.StatusNotFound, wantCode: "project_not_found"},
		{name: "invalid", projectID: "bad%20project", wantStatus: http.StatusBadRequest, wantCode: "invalid_project_id"},
		{name: "archived", projectID: archived.ID, wantStatus: http.StatusConflict, wantCode: "project_archived"},
	} {
		t.Run(tt.name+" list", func(t *testing.T) {
			_, body := getRawJSONStatus(t, baseURL+"/projects/"+tt.projectID+"/sessions", "registry-token", tt.wantStatus)
			assertErrorCode(t, body, tt.wantCode)
		})
		t.Run(tt.name+" create", func(t *testing.T) {
			_, body := postRawJSONStatus(t, baseURL+"/projects/"+tt.projectID+"/sessions", "", "registry-token", tt.wantStatus)
			assertErrorCode(t, body, tt.wantCode)
		})
	}

	infos, err := sessionStore.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("sessions after rejected project session creates = %#v, want none", infos)
	}
}

func TestSessionMetadataStructuredErrors(t *testing.T) {
	process := startSessionAPIServer(t, sessions.NewV2Store(filepath.Join(t.TempDir(), "sessions")), sessions.SessionV2{})
	baseURL := "http://" + process.Addr()

	req, err := http.NewRequest(http.MethodGet, baseURL+"/sessions/missing-session", nil)
	if err != nil {
		t.Fatalf("NewRequest(missing) error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer registry-token")
	resp, err := http.DefaultClient.Do(req)
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

	req, err = http.NewRequest(http.MethodPut, baseURL+"/sessions", nil)
	if err != nil {
		t.Fatalf("NewRequest(PUT /sessions) error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer registry-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /sessions error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET" {
		t.Fatalf("PUT /sessions status/allow = %d/%q, want 405 GET", resp.StatusCode, resp.Header.Get("Allow"))
	}
	body = decodeJSON(t, resp)
	if got := body["error"].(map[string]any)["code"]; got != "method_not_allowed" {
		t.Fatalf("PUT /sessions error = %#v, want method_not_allowed", body)
	}

	req, err = http.NewRequest(http.MethodPost, baseURL+"/sessions/missing-session", nil)
	if err != nil {
		t.Fatalf("NewRequest(POST /sessions/id) error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer registry-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /sessions/id error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, PATCH, DELETE" {
		t.Fatalf("POST /sessions/id status/allow = %d/%q, want 405 GET, PATCH, DELETE", resp.StatusCode, resp.Header.Get("Allow"))
	}
	body = decodeJSON(t, resp)
	if got := body["error"].(map[string]any)["code"]; got != "method_not_allowed" {
		t.Fatalf("POST /sessions/id error = %#v, want method_not_allowed", body)
	}

	req, err = http.NewRequest(http.MethodGet, baseURL+"/sessions/missing-session/items/extra/path", nil)
	if err != nil {
		t.Fatalf("NewRequest(GET bad path) error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer registry-token")
	resp, err = http.DefaultClient.Do(req)
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

func TestSessionStreamConnectsAndPublishesJSONEventShapes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "stream-session")
	process := startSessionAPIServer(t, store, sessions.SessionV2{})
	conn := dialSessionStream(t, process, "stream-session")
	waitForStreamSubscribers(t, process, "stream-session", 1)

	events := []SessionStreamEvent{
		NewSessionStreamEvent("turn.started", map[string]any{"turn_id": "turn-1"}),
		NewSessionStreamEvent("text.delta", map[string]any{"turn_id": "turn-1", "text": "hello"}),
		NewSessionStreamEvent("tool.started", map[string]any{"turn_id": "turn-1", "name": "read_file", "preview": "docs/server-gui.md"}),
		NewSessionStreamEvent("tool.finished", map[string]any{"turn_id": "turn-1", "name": "read_file", "is_error": false}),
		NewSessionStreamEvent("item.appended", map[string]any{"seq": int64(1), "item_id": "item-1"}),
		NewSessionStreamEvent("turn.committed", map[string]any{"turn_id": "turn-1", "last_seq": int64(1)}),
		NewSessionStreamEvent("turn.failed", map[string]any{"turn_id": "turn-2", "message": "context window exceeded"}),
		NewSessionStreamEvent("compact.started", map[string]any{"reason": "user_requested"}),
		NewSessionStreamEvent("compaction.created", map[string]any{"seq": int64(2), "compaction_id": "compact-1"}),
	}
	for _, event := range events {
		if err := process.PublishSessionEvent("stream-session", event); err != nil {
			t.Fatalf("PublishSessionEvent(%s) error = %v", event["type"], err)
		}
		got := readSessionStreamEvent(t, conn)
		if got["type"] != event["type"] {
			t.Fatalf("stream event type = %#v, want %#v in %#v", got["type"], event["type"], got)
		}
	}
}

func TestSessionStreamAfterSeqCatchesUpPersistedItemsBeforeLiveEvents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "catchup-session")
	for i := 1; i <= 3; i++ {
		appendServerTestItem(t, store, "catchup-session", sessions.SessionItem{
			ID:         fmt.Sprintf("item-%d", i),
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant, Content: fmt.Sprintf("persisted text %d", i)},
		})
	}
	process := startSessionAPIServer(t, store, sessions.SessionV2{})
	conn := dialSessionStreamWithQuery(t, process, "catchup-session", "after_seq=1")

	for _, want := range []struct {
		seq int64
		id  string
	}{
		{seq: 2, id: "item-2"},
		{seq: 3, id: "item-3"},
	} {
		got := readSessionStreamEvent(t, conn)
		if got["type"] != "item.appended" || int64(got["seq"].(float64)) != want.seq || got["item_id"] != want.id {
			t.Fatalf("catch-up event = %#v, want item.appended seq %d id %s", got, want.seq, want.id)
		}
		if _, ok := got["text"]; ok {
			t.Fatalf("catch-up event replayed text delta: %#v", got)
		}
	}

	if err := process.PublishSessionEvent("catchup-session", NewSessionStreamEvent("text.delta", map[string]any{"turn_id": "turn-live", "text": "live"})); err != nil {
		t.Fatalf("PublishSessionEvent(live) error = %v", err)
	}
	got := readSessionStreamEvent(t, conn)
	if got["type"] != "text.delta" || got["text"] != "live" {
		t.Fatalf("live stream event = %#v, want text.delta live after catch-up", got)
	}
}

func TestSessionStreamAfterSeqCatchesUpItemUpdatedBeforeLiveEvents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "catchup-updated")
	pending := appendServerTestItem(t, store, "catchup-updated", sessions.SessionItem{
		ID:         "tool-1",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceModel,
		Status:     sessions.ItemStatusPending,
		Message:    &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1"},
	})
	updated := pending
	updated.Status = sessions.ItemStatusCompleted
	updated.Message = &model.Message{Role: model.MessageRoleTool, Content: "tool result", ToolCallID: "call-1"}
	if _, err := store.UpdateItem("catchup-updated", updated); err != nil {
		t.Fatalf("UpdateItem(tool-1) error = %v", err)
	}

	process := startSessionAPIServer(t, store, sessions.SessionV2{})
	conn := dialSessionStreamWithQuery(t, process, "catchup-updated", fmt.Sprintf("after_seq=%d", pending.Seq))

	got := readSessionStreamEvent(t, conn)
	if got["type"] != "item.updated" || int64(got["seq"].(float64)) != pending.Seq+1 || got["item_id"] != "tool-1" {
		t.Fatalf("catch-up event = %#v, want item.updated seq %d id tool-1", got, pending.Seq+1)
	}
	if _, ok := got["content"]; ok {
		t.Fatalf("catch-up event included item content: %#v", got)
	}

	if err := process.PublishSessionEvent("catchup-updated", NewSessionStreamEvent("text.delta", map[string]any{"turn_id": "turn-live", "text": "live"})); err != nil {
		t.Fatalf("PublishSessionEvent(live) error = %v", err)
	}
	got = readSessionStreamEvent(t, conn)
	if got["type"] != "text.delta" || got["text"] != "live" {
		t.Fatalf("live stream event = %#v, want text.delta live after catch-up", got)
	}
}

func TestSessionStreamAfterSeqCatchesUpCompactionEventsInSeqOrder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "catchup-compact")
	existing := appendServerTestItem(t, store, "catchup-compact", sessions.SessionItem{
		ID:         "existing-user",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "existing"},
	})
	if _, err := store.ReplaceActiveHistory("catchup-compact", []string{existing.ID}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}
	saved, err := store.AppendCompactionCheckpoint("catchup-compact", sessions.SessionItem{
		ID:         "summary-1",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityHidden,
		Audience:   sessions.ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "summary secret"},
	}, sessions.CompactionCheckpoint{
		ID:                    "compact-1",
		Reason:                "user_requested",
		Phase:                 "manual",
		Trigger:               "manual",
		SummaryItemID:         "summary-1",
		PreviousActiveHistory: []string{existing.ID},
		ReplacementHistory:    []string{existing.ID, "summary-1"},
	})
	if err != nil {
		t.Fatalf("AppendCompactionCheckpoint() error = %v", err)
	}
	summary, ok := findSessionItemByID(saved.Items, "summary-1")
	if !ok {
		t.Fatalf("summary item missing from saved session: %#v", saved.Items)
	}
	process := startSessionAPIServer(t, store, sessions.SessionV2{})
	conn := dialSessionStreamWithQuery(t, process, "catchup-compact", "after_seq=3")

	events := []map[string]any{
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
	}
	want := []struct {
		eventType string
		seq       int64
		idKey     string
		idValue   string
	}{
		{eventType: "item.appended", seq: summary.Seq, idKey: "item_id", idValue: "summary-1"},
		{eventType: "compaction.created", seq: summary.Seq + 1, idKey: "compaction_id", idValue: "compact-1"},
		{eventType: "active_history.replaced", seq: summary.Seq + 2},
	}
	for i, wantEvent := range want {
		got := events[i]
		if got["type"] != wantEvent.eventType || int64(got["seq"].(float64)) != wantEvent.seq {
			t.Fatalf("catch-up event[%d] = %#v, want %s seq %d", i, got, wantEvent.eventType, wantEvent.seq)
		}
		if wantEvent.idKey != "" && got[wantEvent.idKey] != wantEvent.idValue {
			t.Fatalf("catch-up event[%d] %s = %#v, want %q; event=%#v", i, wantEvent.idKey, got[wantEvent.idKey], wantEvent.idValue, got)
		}
	}
	if _, ok := events[2]["item_ids"]; ok {
		t.Fatalf("active_history.replaced catch-up leaked item_ids: %#v", events[2])
	}

	if err := process.PublishSessionEvent("catchup-compact", NewSessionStreamEvent("text.delta", map[string]any{"turn_id": "turn-live", "text": "live"})); err != nil {
		t.Fatalf("PublishSessionEvent(live) error = %v", err)
	}
	got := readSessionStreamEvent(t, conn)
	if got["type"] != "text.delta" || got["text"] != "live" {
		t.Fatalf("live stream event = %#v, want text.delta live after catch-up", got)
	}
}

func TestSessionStreamAfterSeqCatchesUpCompactedTurnActiveHistoryAtPersistedSeq(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	session := sessions.SessionV2{
		ID:              "catchup-compacted-turn",
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		SaveToolResults: true,
	}
	if _, err := store.SaveMetadata(session); err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	existing := appendServerTestItem(t, store, "catchup-compacted-turn", sessions.SessionItem{
		ID:         "existing-user",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "existing"},
	})
	if _, err := store.ReplaceActiveHistory("catchup-compacted-turn", []string{existing.ID}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}
	loaded, err := store.Load("catchup-compacted-turn")
	if err != nil {
		t.Fatalf("Load(catchup-compacted-turn) error = %v", err)
	}
	saved, err := store.SaveCompactedTurn(loaded, sessions.SessionItem{
		ID:         "summary-1",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityHidden,
		Audience:   sessions.ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "summary secret"},
	}, sessions.CompactionCheckpoint{
		ID:                    "compact-1",
		Reason:                "context_limit",
		Phase:                 "pre_turn",
		Trigger:               "auto",
		SummaryItemID:         "summary-1",
		PreviousActiveHistory: []string{existing.ID},
		ReplacementHistory:    []string{existing.ID, "summary-1"},
	}, []sessions.SessionItem{
		{
			ID:         "user-after-compact",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "user after compact"},
		},
		{
			ID:         "assistant-after-compact",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "assistant after compact"},
		},
	}, []string{"existing-user", "summary-1", "user-after-compact", "assistant-after-compact"})
	if err != nil {
		t.Fatalf("SaveCompactedTurn() error = %v", err)
	}
	if got := responseSessionItemIDs(saved.Items); !reflect.DeepEqual(got, []string{"existing-user", "summary-1", "user-after-compact", "assistant-after-compact"}) {
		t.Fatalf("saved item IDs = %#v, want existing summary user assistant", got)
	}
	if saved.Items[1].Seq != 4 || saved.Items[2].Seq != 6 || saved.Items[3].Seq != 7 || saved.LastSeq != 9 {
		t.Fatalf("saved seqs summary/user/assistant/last = %d/%d/%d/%d, want 4/6/7/9", saved.Items[1].Seq, saved.Items[2].Seq, saved.Items[3].Seq, saved.LastSeq)
	}

	process := startSessionAPIServer(t, store, sessions.SessionV2{})
	conn := dialSessionStreamWithQuery(t, process, "catchup-compacted-turn", "after_seq=5")
	events := []map[string]any{
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
	}
	want := []struct {
		eventType string
		seq       int64
		itemID    string
	}{
		{eventType: "item.appended", seq: 6, itemID: "user-after-compact"},
		{eventType: "item.appended", seq: 7, itemID: "assistant-after-compact"},
		{eventType: "active_history.replaced", seq: 8},
	}
	for i, wantEvent := range want {
		got := events[i]
		if got["type"] != wantEvent.eventType || int64(got["seq"].(float64)) != wantEvent.seq {
			t.Fatalf("catch-up event[%d] = %#v, want %s seq %d", i, got, wantEvent.eventType, wantEvent.seq)
		}
		if wantEvent.itemID != "" && got["item_id"] != wantEvent.itemID {
			t.Fatalf("catch-up event[%d] item_id = %#v, want %q; event=%#v", i, got["item_id"], wantEvent.itemID, got)
		}
		if _, ok := got["compaction_id"]; ok {
			t.Fatalf("catch-up event[%d] unexpectedly replayed compaction metadata after cursor: %#v", i, got)
		}
	}

	if err := process.PublishSessionEvent("catchup-compacted-turn", NewSessionStreamEvent("text.delta", map[string]any{"turn_id": "turn-live", "text": "live"})); err != nil {
		t.Fatalf("PublishSessionEvent(live) error = %v", err)
	}
	got := readSessionStreamEvent(t, conn)
	if got["type"] != "text.delta" || got["text"] != "live" {
		t.Fatalf("live stream event = %#v, want text.delta live after catch-up", got)
	}
}

func TestSessionEventBusBridgePublishesTransientAndPersistedEvents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "bridge-session")
	session, err := store.Load("bridge-session")
	if err != nil {
		t.Fatalf("Load(bridge-session) error = %v", err)
	}
	projector, err := sessionprojector.New(store, session)
	if err != nil {
		t.Fatalf("sessionprojector.New() error = %v", err)
	}
	defer projector.Close()
	bus := eventbus.NewBusWithCheckpoint(projector.CheckpointHandler())
	process := startSessionAPIServer(t, store, sessions.SessionV2{})
	conn := dialSessionStream(t, process, "bridge-session")
	waitForStreamSubscribers(t, process, "bridge-session", 1)
	waitBridge := process.startSessionEventBusBridge("bridge-session", "turn-bridge", bus, session.LastSeq)
	defer waitBridge()
	defer bus.Close()

	if err := bus.Publish(eventbus.TurnStarted{TurnID: "turn-bridge"}); err != nil {
		t.Fatalf("Publish(TurnStarted) error = %v", err)
	}
	if err := bus.Publish(eventbus.ModelEvent{Event: model.TextDeltaEvent{Text: "live text"}}); err != nil {
		t.Fatalf("Publish(ModelEvent text) error = %v", err)
	}
	if err := bus.Publish(eventbus.ModelEvent{Event: model.ToolCallDoneEvent{ToolCall: model.ToolCall{
		ID:        "call-1",
		Name:      "read_file",
		Arguments: `{"path":"SECRET ARGUMENTS"}`,
	}}}); err != nil {
		t.Fatalf("Publish(ModelEvent tool call) error = %v", err)
	}
	if err := bus.Publish(eventbus.TurnInputReady{TurnID: "turn-bridge", Message: model.Message{Role: model.MessageRoleUser, Content: "hello bridge"}}); err != nil {
		t.Fatalf("Publish(TurnInputReady) error = %v", err)
	}
	assistant := model.Message{
		Role:    model.MessageRoleAssistant,
		Content: "assistant needs tool",
		ToolCalls: []model.ToolCall{{
			ID:        "call-1",
			Name:      "read_file",
			Arguments: `{"path":"SECRET ARGUMENTS"}`,
		}},
	}
	if err := bus.Publish(eventbus.AssistantReady{TurnID: "turn-bridge", Message: assistant}); err != nil {
		t.Fatalf("Publish(AssistantReady) error = %v", err)
	}
	if err := bus.Publish(eventbus.ToolResultReady{TurnID: "turn-bridge", Result: model.ToolResult{
		ToolCallID: "call-1",
		Name:       "read_file",
		Content:    "SECRET TOOL RESULT",
	}}); err != nil {
		t.Fatalf("Publish(ToolResultReady) error = %v", err)
	}
	if err := bus.Publish(eventbus.TurnCompleted{TurnID: "turn-bridge"}); err != nil {
		t.Fatalf("Publish(TurnCompleted) error = %v", err)
	}

	events := make([]map[string]any, 0, 6)
	for len(events) < 6 {
		events = append(events, readSessionStreamEvent(t, conn))
	}
	wantTypes := []string{
		"text.delta",
		"tool.started",
		"item.appended",
		"item.appended",
		"item.appended",
		"item.updated",
	}
	for i, want := range wantTypes {
		if events[i]["type"] != want {
			t.Fatalf("event[%d] type = %#v, want %q; events=%#v", i, events[i]["type"], want, events)
		}
	}
	if events[0]["text"] != "live text" {
		t.Fatalf("text event = %#v, want live text", events[0])
	}
	if events[1]["name"] != "read_file" || events[1]["arguments"] != nil {
		t.Fatalf("tool.started event = %#v, want sanitized read_file", events[1])
	}
	for i, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("Marshal(event[%d]) error = %v", i, err)
		}
		if bytes.Contains(raw, []byte("SECRET TOOL RESULT")) || bytes.Contains(raw, []byte("SECRET ARGUMENTS")) {
			t.Fatalf("event[%d] leaked sensitive content: %s", i, raw)
		}
	}

	loaded, err := store.Load("bridge-session")
	if err != nil {
		t.Fatalf("Load(bridge-session) after bridge events error = %v", err)
	}
	var toolItem sessions.SessionItem
	for _, item := range loaded.Items {
		if item.Message != nil && item.Message.Role == model.MessageRoleTool {
			toolItem = item
		}
	}
	if toolItem.ID == "" || toolItem.Status != sessions.ItemStatusCompleted {
		t.Fatalf("tool item = %#v, want completed persisted tool item", toolItem)
	}
	if events[5]["item_id"] != toolItem.ID {
		t.Fatalf("item.updated item_id = %#v, want %q", events[5]["item_id"], toolItem.ID)
	}
	if int64(events[5]["seq"].(float64)) <= toolItem.Seq {
		t.Fatalf("item.updated seq = %#v, want update record seq greater than item birth seq %d", events[5]["seq"], toolItem.Seq)
	}

	bus.Close()
	waitBridge()
}

func TestSessionStreamAfterSeqBadQueryFailsBeforeUpgrade(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "catchup-session")
	process := startSessionAPIServer(t, store, sessions.SessionV2{})

	header := http.Header{}
	header.Set("Authorization", "Bearer registry-token")
	conn, resp, err := websocket.DefaultDialer.Dial("ws://"+process.Addr()+"/sessions/catchup-session/stream?after_seq=-1", header)
	if err == nil {
		_ = conn.Close()
		t.Fatal("Dial(stream negative after_seq) error = nil, want handshake failure")
	}
	if resp == nil {
		t.Fatalf("Dial(stream negative after_seq) response = nil, error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative after_seq stream status = %d, want 400", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	assertErrorCode(t, body, "invalid_query")
}

func TestSessionStreamRequiresRegistryToken(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "stream-auth-session")
	process := startSessionAPIServerWithToken(t, store, sessions.SessionV2{}, "registry-token")

	for _, tt := range []struct {
		name   string
		header http.Header
	}{
		{name: "missing token"},
		{name: "wrong token", header: http.Header{"Authorization": []string{"Bearer wrong-token"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn, resp, err := websocket.DefaultDialer.Dial("ws://"+process.Addr()+"/sessions/stream-auth-session/stream", tt.header)
			if err == nil {
				_ = conn.Close()
				t.Fatal("Dial(stream without valid auth) error = nil, want handshake failure")
			}
			if resp == nil {
				t.Fatalf("Dial(stream without valid auth) response = nil, error = %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("stream auth status = %d, want 403", resp.StatusCode)
			}
			raw, body := readRawJSON(t, resp)
			assertErrorCode(t, body, "permission_denied")
			if bytes.Contains(raw, []byte("registry-token")) {
				t.Fatalf("stream auth error leaked registry token: %s", raw)
			}
		})
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer registry-token")
	conn, resp, err := websocket.DefaultDialer.Dial("ws://"+process.Addr()+"/sessions/stream-auth-session/stream", header)
	if err != nil {
		if resp != nil && resp.Body != nil {
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			t.Fatalf("Dial(stream with valid auth) error = %v; status=%d body=%s", err, resp.StatusCode, raw)
		}
		t.Fatalf("Dial(stream with valid auth) error = %v", err)
	}
	_ = conn.Close()
}

func TestSessionStreamMissingSessionFailsBeforeUpgrade(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "existing-session")
	appendServerTestItem(t, store, "existing-session", sessions.SessionItem{
		ID:         "secret-item",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "SECRET PROMPT TOOL BLOB"},
	})
	process := startSessionAPIServer(t, store, sessions.SessionV2{})

	header := http.Header{}
	header.Set("Authorization", "Bearer registry-token")
	conn, resp, err := websocket.DefaultDialer.Dial("ws://"+process.Addr()+"/sessions/missing-session/stream", header)
	if err == nil {
		_ = conn.Close()
		t.Fatal("Dial(missing stream) error = nil, want handshake failure")
	}
	if resp == nil {
		t.Fatalf("Dial(missing stream) response = nil, error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing stream status = %d, want 404", resp.StatusCode)
	}
	raw, body := readRawJSON(t, resp)
	assertErrorCode(t, body, "session_not_found")
	for _, forbidden := range []string{"SECRET PROMPT TOOL BLOB", "registry-token"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("missing stream error leaked %q: %s", forbidden, raw)
		}
	}
}

func TestSessionStreamInvalidSessionIDFailsBeforeUpgrade(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "existing-session")
	appendServerTestItem(t, store, "existing-session", sessions.SessionItem{
		ID:         "secret-item",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "SECRET INVALID STREAM CONTENT"},
	})
	process := startSessionAPIServer(t, store, sessions.SessionV2{})

	header := http.Header{}
	header.Set("Authorization", "Bearer registry-token")
	conn, resp, err := websocket.DefaultDialer.Dial("ws://"+process.Addr()+"/sessions/bad%20session/stream", header)
	if err == nil {
		_ = conn.Close()
		t.Fatal("Dial(invalid-id stream) error = nil, want handshake failure")
	}
	if resp == nil {
		t.Fatalf("Dial(invalid-id stream) response = nil, error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid-id stream status = %d, want 400", resp.StatusCode)
	}
	raw, body := readRawJSON(t, resp)
	assertErrorCode(t, body, "invalid_session_id")
	for _, forbidden := range []string{"SECRET INVALID STREAM CONTENT", "registry-token"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("invalid-id stream error leaked %q: %s", forbidden, raw)
		}
	}
}

func TestSessionStreamFanoutAndSessionIsolation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "fanout-session")
	saveServerTestSession(t, store, "other-session")
	process := startSessionAPIServer(t, store, sessions.SessionV2{})

	first := dialSessionStream(t, process, "fanout-session")
	second := dialSessionStream(t, process, "fanout-session")
	other := dialSessionStream(t, process, "other-session")
	waitForStreamSubscribers(t, process, "fanout-session", 2)
	waitForStreamSubscribers(t, process, "other-session", 1)

	event := NewSessionStreamEvent("text.delta", map[string]any{"turn_id": "turn-1", "text": "same-session"})
	if err := process.PublishSessionEvent("fanout-session", event); err != nil {
		t.Fatalf("PublishSessionEvent(fanout-session) error = %v", err)
	}
	for name, conn := range map[string]*websocket.Conn{"first": first, "second": second} {
		got := readSessionStreamEvent(t, conn)
		if got["type"] != "text.delta" || got["text"] != "same-session" {
			t.Fatalf("%s stream event = %#v, want text.delta same-session", name, got)
		}
	}

	if err := other.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline(other) error = %v", err)
	}
	if _, _, err := other.ReadMessage(); err == nil {
		t.Fatal("other-session received fanout-session event")
	} else if !os.IsTimeout(err) {
		t.Fatalf("other-session ReadMessage() error = %v, want timeout", err)
	}
}

func TestSessionStreamShutdownClosesConnections(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "shutdown-stream")
	process := startSessionAPIServer(t, store, sessions.SessionV2{})
	conn := dialSessionStream(t, process, "shutdown-stream")
	waitForStreamSubscribers(t, process, "shutdown-stream", 1)

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- process.Shutdown(context.Background())
	}()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown() did not return")
	}

	if got := process.streams.subscriberCount("shutdown-stream"); got != 0 {
		t.Fatalf("subscriberCount after shutdown = %d, want 0", got)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline(shutdown stream) error = %v", err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("ReadMessage() after shutdown error = nil, want closed connection")
	}
}

func TestSessionSendMessagePersistsSuccessfulTurnAndPublishesEvents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "send-success")
	runner := fakeSessionTurnRunner{
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			if request.Session.ID != "send-success" {
				t.Fatalf("runner session id = %q, want send-success", request.Session.ID)
			}
			if request.Content != "hello server" {
				t.Fatalf("runner content = %q, want hello server", request.Content)
			}
			if request.Publisher != nil || request.TurnID != "" {
				t.Fatalf("legacy runner request got publisher=%T turn_id=%q, want legacy path", request.Publisher, request.TurnID)
			}
			request.Emit(model.TextDeltaEvent{Text: "hi "})
			request.Emit(model.TextDeltaEvent{Text: "there"})
			request.Emit(model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "call-1", Name: "read_file", Arguments: `{"path":"secret.txt"}`}})
			request.Emit(model.ToolResultEvent{Result: model.ToolResult{ToolCallID: "call-1", Name: "read_file", Content: "tool result secret"}})
			return serverTestTurnResult(request.Session,
				model.Message{Role: model.MessageRoleUser, Content: request.Content},
				model.Message{Role: model.MessageRoleAssistant, Content: "hi there"},
			), nil
		},
	}
	process := startSessionAPIServerWithTurnRunner(t, store, sessions.SessionV2{}, "registry-token", runner)
	conn := dialSessionStream(t, process, "send-success")
	waitForStreamSubscribers(t, process, "send-success", 1)

	_, body := postRawJSONStatus(t, "http://"+process.Addr()+"/sessions/send-success/messages", `{"content":"hello server"}`, "registry-token", http.StatusOK)
	if body["status"] != "committed" || body["turn_id"] == "" || body["last_seq"] == nil {
		t.Fatalf("send response = %#v, want committed turn metadata", body)
	}

	events := []map[string]any{
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
	}
	wantTypes := []string{"turn.started", "text.delta", "text.delta", "tool.started", "tool.finished", "item.appended", "item.appended", "turn.committed"}
	for i, want := range wantTypes {
		if events[i]["type"] != want {
			t.Fatalf("event[%d] type = %#v, want %q; events=%#v", i, events[i]["type"], want, events)
		}
	}
	if events[1]["text"] != "hi " || events[2]["text"] != "there" {
		t.Fatalf("text delta events = %#v/%#v, want streamed text", events[1], events[2])
	}
	if events[3]["name"] != "read_file" || events[4]["name"] != "read_file" || events[4]["is_error"] != false {
		t.Fatalf("tool events = %#v/%#v, want sanitized tool status", events[3], events[4])
	}
	if events[4]["content"] != nil || events[4]["arguments"] != nil {
		t.Fatalf("tool event leaked content or arguments: %#v", events[4])
	}
	if events[7]["last_seq"] != body["last_seq"] {
		t.Fatalf("committed last_seq = %#v, response last_seq = %#v", events[7]["last_seq"], body["last_seq"])
	}

	session, err := store.Load("send-success")
	if err != nil {
		t.Fatalf("Load(send-success) error = %v", err)
	}
	if len(session.Items) != 2 {
		t.Fatalf("len(session.Items) = %d, want persisted user+assistant: %#v", len(session.Items), session.Items)
	}
	if session.Items[0].Message == nil || session.Items[0].Message.Role != model.MessageRoleUser || session.Items[0].Message.Content != "hello server" {
		t.Fatalf("user item = %#v, want persisted user message", session.Items[0])
	}
	if session.Items[1].Message == nil || session.Items[1].Message.Role != model.MessageRoleAssistant || session.Items[1].Message.Content != "hi there" {
		t.Fatalf("assistant item = %#v, want persisted assistant message", session.Items[1])
	}
	if session.Items[0].TurnID == "" || session.Items[0].TurnID != session.Items[1].TurnID {
		t.Fatalf("turn ids = %q/%q, want same non-empty turn id", session.Items[0].TurnID, session.Items[1].TurnID)
	}
	if !reflect.DeepEqual(session.ActiveHistory, []string{session.Items[0].ID, session.Items[1].ID}) {
		t.Fatalf("ActiveHistory = %#v, want new user+assistant item ids", session.ActiveHistory)
	}
}

func TestSessionSendMessageIncrementalPersistsAndPublishesViaBus(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "send-incremental")
	runner := fakeIncrementalSessionTurnRunner{
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			if request.Session.ID != "send-incremental" {
				t.Fatalf("runner session id = %q, want send-incremental", request.Session.ID)
			}
			if request.TurnID == "" || request.Publisher == nil {
				t.Fatalf("runner turn id/publisher = %q/%T, want incremental request", request.TurnID, request.Publisher)
			}
			request.Emit(model.TextDeltaEvent{Text: "live text"})
			request.Emit(model.ToolCallDoneEvent{ToolCall: model.ToolCall{
				ID:        "call-1",
				Name:      "read_file",
				Arguments: `{"path":"SECRET ARGUMENTS"}`,
			}})
			assistant := model.Message{
				Role:    model.MessageRoleAssistant,
				Content: "assistant needs tool",
				ToolCalls: []model.ToolCall{{
					ID:        "call-1",
					Name:      "read_file",
					Arguments: `{"path":"SECRET ARGUMENTS"}`,
				}},
			}
			if err := request.Publisher.Publish(eventbus.AssistantReady{TurnID: request.TurnID, Message: assistant}); err != nil {
				return SessionTurnResult{}, err
			}
			result := model.ToolResult{
				ToolCallID: "call-1",
				Name:       "read_file",
				Content:    "SECRET TOOL RESULT",
			}
			if err := request.Publisher.Publish(eventbus.ToolResultReady{TurnID: request.TurnID, Result: result}); err != nil {
				return SessionTurnResult{}, err
			}
			request.Emit(model.ToolResultEvent{Result: result})
			if err := request.Publisher.Publish(eventbus.AssistantReady{TurnID: request.TurnID, Message: model.Message{
				Role:    model.MessageRoleAssistant,
				Content: "final answer",
			}}); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	process := startSessionAPIServerWithTurnRunner(t, store, sessions.SessionV2{}, "registry-token", runner)
	conn := dialSessionStream(t, process, "send-incremental")
	waitForStreamSubscribers(t, process, "send-incremental", 1)

	_, body := postRawJSONStatus(t, "http://"+process.Addr()+"/sessions/send-incremental/messages", `{"content":"hello incremental"}`, "registry-token", http.StatusOK)
	if body["status"] != "committed" || body["turn_id"] == "" || body["last_seq"] == nil {
		t.Fatalf("send response = %#v, want committed turn metadata", body)
	}

	events := make([]map[string]any, 0, 10)
	for len(events) < 10 {
		events = append(events, readSessionStreamEvent(t, conn))
	}
	wantTypes := []string{
		"turn.started",
		"item.appended",
		"text.delta",
		"tool.started",
		"item.appended",
		"item.appended",
		"item.updated",
		"tool.finished",
		"item.appended",
		"turn.committed",
	}
	for i, want := range wantTypes {
		if events[i]["type"] != want {
			t.Fatalf("event[%d] type = %#v, want %q; events=%#v", i, events[i]["type"], want, events)
		}
	}
	if events[2]["text"] != "live text" {
		t.Fatalf("text event = %#v, want live text", events[2])
	}
	if events[3]["name"] != "read_file" || events[3]["arguments"] != nil {
		t.Fatalf("tool.started event = %#v, want sanitized read_file", events[3])
	}
	if events[7]["content"] != nil || events[7]["arguments"] != nil {
		t.Fatalf("tool.finished event leaked content or arguments: %#v", events[7])
	}
	for i, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("Marshal(event[%d]) error = %v", i, err)
		}
		if bytes.Contains(raw, []byte("SECRET TOOL RESULT")) || bytes.Contains(raw, []byte("SECRET ARGUMENTS")) {
			t.Fatalf("event[%d] leaked sensitive content: %s", i, raw)
		}
	}

	session, err := store.Load("send-incremental")
	if err != nil {
		t.Fatalf("Load(send-incremental) error = %v", err)
	}
	if session.RunningTurnID != "" || session.InterruptedTurnID != "" || !session.InterruptedAt.IsZero() {
		t.Fatalf("turn metadata = running %q interrupted %q at %s, want cleared successful turn", session.RunningTurnID, session.InterruptedTurnID, session.InterruptedAt)
	}
	if len(session.Items) != 4 {
		t.Fatalf("len(session.Items) = %d, want user+assistant+tool+final: %#v", len(session.Items), session.Items)
	}
	if session.Items[0].Message == nil || session.Items[0].Message.Role != model.MessageRoleUser || session.Items[0].Message.Content != "hello incremental" {
		t.Fatalf("user item = %#v, want persisted prompt", session.Items[0])
	}
	if session.Items[1].Message == nil || session.Items[1].Message.Role != model.MessageRoleAssistant || len(session.Items[1].Message.ToolCalls) != 1 {
		t.Fatalf("assistant tool-call item = %#v, want assistant with tool call", session.Items[1])
	}
	if session.Items[2].Message == nil || session.Items[2].Message.Role != model.MessageRoleTool || session.Items[2].Status != sessions.ItemStatusCompleted || session.Items[2].Message.Content != "SECRET TOOL RESULT" {
		t.Fatalf("tool item = %#v, want completed persisted tool result", session.Items[2])
	}
	if session.Items[3].Message == nil || session.Items[3].Message.Role != model.MessageRoleAssistant || session.Items[3].Message.Content != "final answer" {
		t.Fatalf("final assistant item = %#v, want final answer", session.Items[3])
	}
	if !reflect.DeepEqual(session.ActiveHistory, []string{session.Items[0].ID, session.Items[1].ID, session.Items[2].ID, session.Items[3].ID}) {
		t.Fatalf("ActiveHistory = %#v, want all incremental item ids", session.ActiveHistory)
	}
	if session.LastSeq != int64(body["last_seq"].(float64)) {
		t.Fatalf("LastSeq = %d, response last_seq = %#v", session.LastSeq, body["last_seq"])
	}
	messages, err := session.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if len(messages) != 4 || messages[1].Role != model.MessageRoleAssistant || len(messages[1].ToolCalls) != 1 || messages[2].Role != model.MessageRoleTool || messages[2].ToolCallID != "call-1" {
		t.Fatalf("materialized messages = %#v, want legal assistant/tool exchange", messages)
	}
}

func TestSessionSendMessageIncrementalCompactsBeforeTurnViaProjector(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "send-incremental-compact")
	existingUser := appendServerTestItem(t, store, "send-incremental-compact", sessions.SessionItem{
		ID:         "existing-user",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "existing prompt"},
	})
	existingAssistant := appendServerTestItem(t, store, "send-incremental-compact", sessions.SessionItem{
		ID:         "existing-assistant",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "existing answer"},
	})
	if _, err := store.ReplaceActiveHistory("send-incremental-compact", []string{existingUser.ID, existingAssistant.ID}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}

	summaryMessage := model.Message{Role: model.MessageRoleDeveloper, Content: "<compaction_summary>\nserver incremental compact\n</compaction_summary>"}
	summary := sessions.SessionItem{
		ID:         "summary-1",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityHidden,
		Audience:   sessions.ItemAudienceModel,
		Message:    &summaryMessage,
	}
	checkpoint := sessions.CompactionCheckpoint{
		ID:                    "compact-1",
		Reason:                "context_limit",
		Phase:                 "pre_turn",
		Trigger:               "auto",
		SummaryItemID:         summary.ID,
		PreviousActiveHistory: []string{existingUser.ID, existingAssistant.ID},
		ReplacementHistory:    []string{summary.ID},
	}
	runnerSawCompactedSession := false
	runner := fakeIncrementalSessionTurnRunner{
		plan: func(ctx context.Context, request SessionTurnRequest) (SessionCompactionResult, error) {
			if request.TurnID == "" || request.Content != "after compact" {
				t.Fatalf("planner request turn/content = %q/%q, want turn and prompt", request.TurnID, request.Content)
			}
			if !reflect.DeepEqual(request.Session.ActiveHistory, []string{existingUser.ID, existingAssistant.ID}) {
				t.Fatalf("planner ActiveHistory = %#v, want existing active history", request.Session.ActiveHistory)
			}
			return SessionCompactionResult{
				Session: request.Session,
				Compaction: SessionCompactionPlan{
					SummaryItem: summary,
					Checkpoint:  checkpoint,
				},
			}, nil
		},
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			runnerSawCompactedSession = true
			if !reflect.DeepEqual(request.Session.ActiveHistory, []string{summary.ID}) {
				t.Fatalf("runner ActiveHistory = %#v, want compacted history before user prompt", request.Session.ActiveHistory)
			}
			if request.Session.RunningTurnID != request.TurnID {
				t.Fatalf("runner RunningTurnID = %q, want %q", request.Session.RunningTurnID, request.TurnID)
			}
			if len(request.Session.Items) != 3 || request.Session.Items[2].ID != summary.ID {
				t.Fatalf("runner session items = %#v, want existing items plus summary only", request.Session.Items)
			}
			if err := request.Publisher.Publish(eventbus.AssistantReady{TurnID: request.TurnID, Message: model.Message{
				Role:    model.MessageRoleAssistant,
				Content: "assistant after compact",
			}}); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	process := startSessionAPIServerWithTurnRunner(t, store, sessions.SessionV2{}, "registry-token", runner)
	conn := dialSessionStream(t, process, "send-incremental-compact")
	waitForStreamSubscribers(t, process, "send-incremental-compact", 1)

	_, body := postRawJSONStatus(t, "http://"+process.Addr()+"/sessions/send-incremental-compact/messages", `{"content":"after compact"}`, "registry-token", http.StatusOK)
	if body["status"] != "committed" || body["turn_id"] == "" || body["last_seq"] == nil {
		t.Fatalf("send response = %#v, want committed turn metadata", body)
	}
	if !runnerSawCompactedSession {
		t.Fatal("runner was not called with compacted session")
	}

	events := make([]map[string]any, 0, 7)
	for len(events) < 7 {
		events = append(events, readSessionStreamEvent(t, conn))
	}
	wantTypes := []string{
		"turn.started",
		"item.appended",
		"compaction.created",
		"active_history.replaced",
		"item.appended",
		"item.appended",
		"turn.committed",
	}
	for i, want := range wantTypes {
		if events[i]["type"] != want {
			t.Fatalf("event[%d] type = %#v, want %q; events=%#v", i, events[i]["type"], want, events)
		}
	}
	if events[1]["item_id"] != summary.ID {
		t.Fatalf("summary event = %#v, want summary item appended", events[1])
	}
	if events[2]["compaction_id"] != checkpoint.ID {
		t.Fatalf("compaction event = %#v, want compact-1", events[2])
	}

	session, err := store.Load("send-incremental-compact")
	if err != nil {
		t.Fatalf("Load(send-incremental-compact) error = %v", err)
	}
	if session.RunningTurnID != "" || session.InterruptedTurnID != "" || !session.InterruptedAt.IsZero() {
		t.Fatalf("turn metadata = running %q interrupted %q at %s, want cleared successful turn", session.RunningTurnID, session.InterruptedTurnID, session.InterruptedAt)
	}
	if len(session.Items) != 5 {
		t.Fatalf("len(session.Items) = %d, want existing+summary+user+assistant without duplicate legacy save: %#v", len(session.Items), session.Items)
	}
	if len(session.Compactions) != 1 || session.Compactions[0].ID != checkpoint.ID {
		t.Fatalf("Compactions = %#v, want compact-1", session.Compactions)
	}
	userItem := session.Items[3]
	assistantItem := session.Items[4]
	if userItem.Message == nil || userItem.Message.Role != model.MessageRoleUser || userItem.Message.Content != "after compact" {
		t.Fatalf("user item = %#v, want persisted prompt after compaction", userItem)
	}
	if assistantItem.Message == nil || assistantItem.Message.Role != model.MessageRoleAssistant || assistantItem.Message.Content != "assistant after compact" {
		t.Fatalf("assistant item = %#v, want incremental assistant response", assistantItem)
	}
	if !reflect.DeepEqual(session.ActiveHistory, []string{summary.ID, userItem.ID, assistantItem.ID}) {
		t.Fatalf("ActiveHistory = %#v, want compacted history plus incremental turn", session.ActiveHistory)
	}
	if session.LastSeq != int64(body["last_seq"].(float64)) {
		t.Fatalf("LastSeq = %d, response last_seq = %#v", session.LastSeq, body["last_seq"])
	}
	messages, err := session.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if len(messages) != 3 || messages[0].Role != model.MessageRoleDeveloper || messages[1].Role != model.MessageRoleUser || messages[2].Role != model.MessageRoleAssistant {
		t.Fatalf("materialized messages = %#v, want legal compacted user/assistant history", messages)
	}
}

func TestSessionSendMessagePersistsPlannedCompactionAndSuccessfulTurnAtomically(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "send-compact-success")
	existing := appendServerTestItem(t, store, "send-compact-success", sessions.SessionItem{
		ID:         "existing-user",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "existing"},
	})
	if _, err := store.ReplaceActiveHistory("send-compact-success", []string{existing.ID}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}
	runner := fakeSessionTurnRunner{
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			summaryMessage := model.Message{Role: model.MessageRoleDeveloper, Content: "<compaction_summary>\nplanned\n</compaction_summary>"}
			summary := sessions.SessionItem{
				ID:         "summary-1",
				Kind:       sessions.ItemKindMessage,
				Visibility: sessions.ItemVisibilityHidden,
				Audience:   sessions.ItemAudienceModel,
				Message:    &summaryMessage,
			}
			checkpoint := sessions.CompactionCheckpoint{
				ID:                    "compact-1",
				Reason:                "context_limit",
				Phase:                 "pre_turn",
				Trigger:               "auto",
				SummaryItemID:         summary.ID,
				PreviousActiveHistory: []string{existing.ID},
				ReplacementHistory:    []string{existing.ID, summary.ID},
			}
			userItem := serverTestSessionItemFromMessage("msg-000003", model.Message{Role: model.MessageRoleUser, Content: request.Content})
			assistantItem := serverTestSessionItemFromMessage("msg-000004", model.Message{Role: model.MessageRoleAssistant, Content: "assistant after compact"})
			return SessionTurnResult{
				Session: request.Session,
				Compaction: &SessionCompactionPlan{
					SummaryItem: summary,
					Checkpoint:  checkpoint,
				},
				Items:         []sessions.SessionItem{userItem, assistantItem},
				ActiveHistory: []string{existing.ID, summary.ID, userItem.ID, assistantItem.ID},
			}, nil
		},
	}
	process := startSessionAPIServerWithTurnRunner(t, store, sessions.SessionV2{}, "registry-token", runner)

	_, body := postRawJSONStatus(t, "http://"+process.Addr()+"/sessions/send-compact-success/messages", `{"content":"after compact"}`, "registry-token", http.StatusOK)
	if body["status"] != "committed" {
		t.Fatalf("send response = %#v, want committed", body)
	}

	session, err := store.Load("send-compact-success")
	if err != nil {
		t.Fatalf("Load(send-compact-success) error = %v", err)
	}
	if got := responseSessionItemIDs(session.Items); !reflect.DeepEqual(got, []string{"existing-user", "summary-1", "msg-000003", "msg-000004"}) {
		t.Fatalf("item IDs = %#v, want existing summary user assistant", got)
	}
	if len(session.Compactions) != 1 || session.Compactions[0].ID != "compact-1" {
		t.Fatalf("Compactions = %#v, want compact-1", session.Compactions)
	}
	if !reflect.DeepEqual(session.ActiveHistory, []string{"existing-user", "summary-1", "msg-000003", "msg-000004"}) {
		t.Fatalf("ActiveHistory = %#v, want replacement plus successful turn", session.ActiveHistory)
	}
	if session.Items[1].Visibility != sessions.ItemVisibilityHidden || session.Items[2].Message.Content != "after compact" || session.Items[3].Message.Content != "assistant after compact" {
		t.Fatalf("persisted items = %#v, want hidden summary and visible turn", session.Items)
	}
	if session.LastSeq != 9 {
		t.Fatalf("LastSeq = %d, want compact+turn transaction through seq 9", session.LastSeq)
	}
}

func TestSessionSendMessageRejectsBusySession(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "busy-session")
	saveServerTestSession(t, store, "other-session")
	saveServerTestSession(t, store, "fallback-session")
	type startedTurn struct {
		sessionID string
		content   string
	}
	started := make(chan startedTurn, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() {
		releaseOnce.Do(func() { close(release) })
	}
	defer closeRelease()
	runner := fakeSessionTurnRunner{
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			started <- startedTurn{sessionID: request.Session.ID, content: request.Content}
			<-release
			return serverTestTurnResult(request.Session,
				model.Message{Role: model.MessageRoleUser, Content: request.Content},
				model.Message{Role: model.MessageRoleAssistant, Content: "done"},
			), nil
		},
	}
	process := startSessionAPIServerWithTurnRunner(t, store, sessions.SessionV2{}, "registry-token", runner)
	baseURL := "http://" + process.Addr()
	firstDone := make(chan map[string]any, 1)
	go func() {
		_, body := postRawJSONStatus(t, baseURL+"/sessions/busy-session/messages", `{"content":"first"}`, "registry-token", http.StatusOK)
		firstDone <- body
	}()
	waitStarted := func(want startedTurn) {
		t.Helper()
		select {
		case got := <-started:
			if got != want {
				t.Fatalf("started turn = %#v, want %#v", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s turn to start", want.sessionID)
		}
	}
	waitStarted(startedTurn{sessionID: "busy-session", content: "first"})

	otherDone := make(chan map[string]any, 1)
	go func() {
		_, body := postRawJSONStatus(t, baseURL+"/sessions/other-session/messages", `{"content":"other"}`, "registry-token", http.StatusOK)
		otherDone <- body
	}()
	waitStarted(startedTurn{sessionID: "other-session", content: "other"})

	_, serverInfo := getRawJSON(t, baseURL+"/server")
	if serverInfo["running_turns"] != float64(2) {
		t.Fatalf("/server running_turns = %#v, want 2", serverInfo["running_turns"])
	}

	_, detail := getRawJSON(t, baseURL+"/sessions/busy-session")
	if detail["status"] != "running" {
		t.Fatalf("busy session status = %#v, want running", detail["status"])
	}
	_, detail = getRawJSON(t, baseURL+"/sessions/other-session")
	if detail["status"] != "running" {
		t.Fatalf("other session status = %#v, want running", detail["status"])
	}
	_, detail = getRawJSON(t, baseURL+"/sessions/fallback-session")
	if detail["status"] != "idle" {
		t.Fatalf("fallback session status = %#v, want idle", detail["status"])
	}

	raw, body := postRawJSONStatus(t, baseURL+"/sessions/busy-session/messages", `{"content":"second"}`, "registry-token", http.StatusConflict)
	assertErrorCode(t, body, "session_busy")
	if bytes.Contains(raw, []byte("first")) || bytes.Contains(raw, []byte("second")) || bytes.Contains(raw, []byte("other")) {
		t.Fatalf("busy response leaked prompt content: %s", raw)
	}
	select {
	case got := <-started:
		t.Fatalf("busy request started another turn instead of returning session_busy: %#v", got)
	default:
	}

	closeRelease()
	for name, done := range map[string]<-chan map[string]any{
		"first": firstDone,
		"other": otherDone,
	} {
		select {
		case body := <-done:
			if body["status"] != "committed" {
				t.Fatalf("%s response = %#v, want committed", name, body)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s turn to finish", name)
		}
	}
}

func TestServerShutdownImmediateCancelsRunningTurnAndStopsServer(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "shutdown-busy")
	started := make(chan struct{})
	canceled := make(chan struct{})
	runner := fakeSessionTurnRunner{
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return SessionTurnResult{}, ctx.Err()
		},
	}
	process := startSessionAPIServerWithTurnRunner(t, store, sessions.SessionV2{}, "registry-token", runner)
	baseURL := "http://" + process.Addr()
	firstDone := make(chan map[string]any, 1)
	go func() {
		_, body := postRawJSONStatus(t, baseURL+"/sessions/shutdown-busy/messages", `{"content":"SECRET PROMPT TOKEN DETAILS"}`, "registry-token", http.StatusInternalServerError)
		firstDone <- body
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for turn to start")
	}

	_, body := postRawJSONStatus(t, baseURL+"/server/shutdown", "", "registry-token", http.StatusOK)
	if body["status"] != "shutting_down" || body["wait"] != false || body["timed_out"] != false {
		t.Fatalf("shutdown response = %#v, want immediate shutting_down", body)
	}

	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for running turn context cancellation")
	}
	select {
	case body := <-firstDone:
		assertErrorCode(t, body, "turn_failed")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for canceled turn response")
	}
	waitForServerStopped(t, process.Addr())

	session, err := store.Load("shutdown-busy")
	if err != nil {
		t.Fatalf("Load(shutdown-busy) error = %v", err)
	}
	if session.RunningTurnID != "" || session.InterruptedTurnID != "turn-000001" || session.InterruptedAt.IsZero() {
		t.Fatalf("shutdown metadata = running %q interrupted %q at %s, want interrupted turn-000001", session.RunningTurnID, session.InterruptedTurnID, session.InterruptedAt)
	}
	if session.LastSeq != 0 || len(session.Items) != 0 {
		t.Fatalf("shutdown session replay = last_seq %d items %#v, want no committed replay", session.LastSeq, session.Items)
	}
}

func TestServerShutdownWaitDrainsRunningTurnAndRejectsNewTurns(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "shutdown-wait")
	started := make(chan struct{})
	release := make(chan struct{})
	runner := fakeSessionTurnRunner{
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return SessionTurnResult{}, ctx.Err()
			}
			return serverTestTurnResult(request.Session,
				model.Message{Role: model.MessageRoleUser, Content: request.Content},
				model.Message{Role: model.MessageRoleAssistant, Content: "done"},
			), nil
		},
	}
	process := startSessionAPIServerWithTurnRunner(t, store, sessions.SessionV2{}, "registry-token", runner)
	baseURL := "http://" + process.Addr()
	firstDone := make(chan map[string]any, 1)
	go func() {
		_, body := postRawJSONStatus(t, baseURL+"/sessions/shutdown-wait/messages", `{"content":"first"}`, "registry-token", http.StatusOK)
		firstDone <- body
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for turn to start")
	}

	shutdownDone := make(chan map[string]any, 1)
	go func() {
		_, body := postRawJSONStatus(t, baseURL+"/server/shutdown?wait=true&timeout_ms=2000", "", "registry-token", http.StatusOK)
		shutdownDone <- body
	}()
	waitForShutdownRejection(t, baseURL+"/sessions/shutdown-wait/messages")

	close(release)
	select {
	case body := <-firstDone:
		if body["status"] != "committed" {
			t.Fatalf("drained turn response = %#v, want committed", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for turn to finish")
	}
	select {
	case body := <-shutdownDone:
		if body["status"] != "shutting_down" || body["wait"] != true || body["timed_out"] != false {
			t.Fatalf("wait shutdown response = %#v, want drained shutdown", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for wait shutdown response")
	}
	waitForServerStopped(t, process.Addr())

	session, err := store.Load("shutdown-wait")
	if err != nil {
		t.Fatalf("Load(shutdown-wait) error = %v", err)
	}
	if session.RunningTurnID != "" || session.InterruptedTurnID != "" || !session.InterruptedAt.IsZero() {
		t.Fatalf("drained metadata = running %q interrupted %q at %s, want cleared without interruption", session.RunningTurnID, session.InterruptedTurnID, session.InterruptedAt)
	}
	if session.LastSeq != 5 || len(session.Items) != 2 {
		t.Fatalf("drained session replay = last_seq %d items %#v, want committed turn", session.LastSeq, session.Items)
	}
}

func TestServerShutdownWaitTimeoutCancelsRunningTurnAndStopsServer(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "shutdown-timeout")
	started := make(chan struct{})
	canceled := make(chan struct{})
	runner := fakeSessionTurnRunner{
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return SessionTurnResult{}, ctx.Err()
		},
	}
	process := startSessionAPIServerWithTurnRunner(t, store, sessions.SessionV2{}, "registry-token", runner)
	baseURL := "http://" + process.Addr()
	firstDone := make(chan map[string]any, 1)
	go func() {
		_, body := postRawJSONStatus(t, baseURL+"/sessions/shutdown-timeout/messages", `{"content":"first"}`, "registry-token", http.StatusInternalServerError)
		firstDone <- body
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for turn to start")
	}

	_, body := postRawJSONStatus(t, baseURL+"/server/shutdown?wait=true&timeout_ms=25", "", "registry-token", http.StatusOK)
	if body["status"] != "shutting_down" || body["wait"] != true || body["timed_out"] != true {
		t.Fatalf("timeout shutdown response = %#v, want timed_out shutdown", body)
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for timeout cancellation")
	}
	select {
	case body := <-firstDone:
		assertErrorCode(t, body, "turn_failed")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for timeout turn response")
	}
	waitForServerStopped(t, process.Addr())

	session, err := store.Load("shutdown-timeout")
	if err != nil {
		t.Fatalf("Load(shutdown-timeout) error = %v", err)
	}
	if session.RunningTurnID != "" || session.InterruptedTurnID != "turn-000001" || session.InterruptedAt.IsZero() {
		t.Fatalf("timeout metadata = running %q interrupted %q at %s, want interrupted turn-000001", session.RunningTurnID, session.InterruptedTurnID, session.InterruptedAt)
	}
	if session.LastSeq != 0 || len(session.Items) != 0 {
		t.Fatalf("timeout session replay = last_seq %d items %#v, want no committed replay", session.LastSeq, session.Items)
	}
}

func TestServerStartupMarksStaleRunningTurnInterruptedWithoutReplay(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	if _, err := store.SaveMetadata(sessions.SessionV2{
		ID:               "recover-running",
		Provider:         "codex",
		ModelProfile:     "default",
		ModelID:          "gpt-5",
		CWD:              t.TempDir(),
		RunningTurnID:    "turn-000123",
		RunningStartedAt: time.Date(2026, 7, 4, 1, 2, 3, 0, time.UTC),
	}); err != nil {
		t.Fatalf("SaveMetadata(recover-running) error = %v", err)
	}
	existing := appendServerTestItem(t, store, "recover-running", sessions.SessionItem{
		ID:         "existing-user",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "already committed"},
	})
	if _, err := store.ReplaceActiveHistory("recover-running", []string{existing.ID}); err != nil {
		t.Fatalf("ReplaceActiveHistory(recover-running) error = %v", err)
	}

	process := startSessionAPIServerWithTurnRunner(t, store, sessions.SessionV2{}, "registry-token", fakeSessionTurnRunner{
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			t.Fatalf("runner was called during startup recovery")
			return SessionTurnResult{}, nil
		},
	})

	session, err := store.Load("recover-running")
	if err != nil {
		t.Fatalf("Load(recover-running) error = %v", err)
	}
	if session.RunningTurnID != "" || session.InterruptedTurnID != "turn-000123" || session.InterruptedAt.IsZero() {
		t.Fatalf("recovered metadata = running %q interrupted %q at %s, want interrupted stale turn", session.RunningTurnID, session.InterruptedTurnID, session.InterruptedAt)
	}
	if got := responseSessionItemIDs(session.Items); !reflect.DeepEqual(got, []string{"existing-user"}) {
		t.Fatalf("recovered item IDs = %#v, want only committed history", got)
	}
	_, detail := getRawJSON(t, "http://"+process.Addr()+"/sessions/recover-running")
	if detail["status"] != "interrupted" || detail["interrupted_turn_id"] != "turn-000123" {
		t.Fatalf("recovered session detail = %#v, want interrupted status and turn id", detail)
	}
}

func TestServeContextCancelUsesImmediateStopSemantics(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "signal-stop")
	started := make(chan struct{})
	canceled := make(chan struct{})
	runner := fakeSessionTurnRunner{
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return SessionTurnResult{}, ctx.Err()
		},
	}
	process, err := Start(Options{
		CWD:          t.TempDir(),
		Listen:       "127.0.0.1:0",
		AuthToken:    "registry-token",
		SessionStore: store,
		TurnRunner:   runner,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	serveCtx, cancelServe := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- process.Serve(serveCtx)
	}()
	waitForHealthyServer(t, process.Addr())
	serveStopped := false
	t.Cleanup(func() {
		if serveStopped {
			return
		}
		_ = process.Shutdown(context.Background())
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Fatal("Serve() did not stop")
		}
	})

	baseURL := "http://" + process.Addr()
	firstDone := make(chan map[string]any, 1)
	go func() {
		_, body := postRawJSONStatus(t, baseURL+"/sessions/signal-stop/messages", `{"content":"first"}`, "registry-token", http.StatusInternalServerError)
		firstDone <- body
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for turn to start")
	}

	cancelServe()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for signal-style cancellation")
	}
	select {
	case body := <-firstDone:
		assertErrorCode(t, body, "turn_failed")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for canceled turn response")
	}
	select {
	case err := <-serveDone:
		serveStopped = true
		if err != nil {
			t.Fatalf("Serve() after context cancel error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Serve() to stop")
	}
	waitForServerStopped(t, process.Addr())
}

func TestSessionSendMessageFailedTurnStaysTransient(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "failed-turn")
	existing := appendServerTestItem(t, store, "failed-turn", sessions.SessionItem{
		ID:         "existing-user",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "existing safe content"},
	})
	if _, err := store.ReplaceActiveHistory("failed-turn", []string{existing.ID}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}
	runner := fakeSessionTurnRunner{
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			request.Emit(model.TextDeltaEvent{Text: "partial transient text"})
			return SessionTurnResult{}, fmt.Errorf("provider secret failure")
		},
	}
	process := startSessionAPIServerWithTurnRunner(t, store, sessions.SessionV2{}, "registry-token", runner)
	conn := dialSessionStream(t, process, "failed-turn")
	waitForStreamSubscribers(t, process, "failed-turn", 1)

	raw, body := postRawJSONStatus(t, "http://"+process.Addr()+"/sessions/failed-turn/messages", `{"content":"new prompt secret"}`, "registry-token", http.StatusInternalServerError)
	assertErrorCode(t, body, "turn_failed")
	for _, forbidden := range []string{"new prompt secret", "provider secret failure", "partial transient text", "registry-token"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("failed turn response leaked %q: %s", forbidden, raw)
		}
	}

	events := []map[string]any{
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
	}
	if events[0]["type"] != "turn.started" || events[1]["type"] != "text.delta" || events[2]["type"] != "turn.failed" {
		t.Fatalf("failure events = %#v, want started/delta/failed", events)
	}
	if events[2]["message"] != "turn failed" {
		t.Fatalf("turn.failed message = %#v, want sanitized failure", events[2]["message"])
	}

	session, err := store.Load("failed-turn")
	if err != nil {
		t.Fatalf("Load(failed-turn) error = %v", err)
	}
	if len(session.Items) != 1 || session.Items[0].ID != existing.ID {
		t.Fatalf("session items after failed turn = %#v, want unchanged existing item", session.Items)
	}
	if !reflect.DeepEqual(session.ActiveHistory, []string{existing.ID}) {
		t.Fatalf("ActiveHistory after failed turn = %#v, want unchanged existing item id", session.ActiveHistory)
	}
}

func TestSessionSendMessageRequiresTokenAndValidRequest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "send-validation")
	process := startSessionAPIServerWithTurnRunner(t, store, sessions.SessionV2{}, "registry-token", fakeSessionTurnRunner{})
	baseURL := "http://" + process.Addr()

	for _, tt := range []struct {
		name  string
		token string
	}{
		{name: "missing token"},
		{name: "wrong token", token: "wrong-token"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, body := postRawJSONStatus(t, baseURL+"/sessions/send-validation/messages", `{"content":"secret prompt"}`, tt.token, http.StatusForbidden)
			assertErrorCode(t, body, "permission_denied")
			if bytes.Contains(raw, []byte("secret prompt")) || bytes.Contains(raw, []byte("registry-token")) {
				t.Fatalf("permission error leaked secret: %s", raw)
			}
		})
	}

	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "empty body", body: ""},
		{name: "malformed json", body: "{"},
		{name: "missing content", body: `{}`},
		{name: "non-string content", body: `{"content":42}`},
		{name: "empty content", body: `{"content":""}`},
		{name: "blank content", body: `{"content":"   "}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, body := postRawJSONStatus(t, baseURL+"/sessions/send-validation/messages", tt.body, "registry-token", http.StatusBadRequest)
			assertErrorCode(t, body, "invalid_request")
			if bytes.Contains(raw, []byte("registry-token")) {
				t.Fatalf("invalid request error leaked registry token: %s", raw)
			}
		})
	}

	session, err := store.Load("send-validation")
	if err != nil {
		t.Fatalf("Load(send-validation) error = %v", err)
	}
	if len(session.Items) != 0 || len(session.ActiveHistory) != 0 {
		t.Fatalf("invalid/security requests changed session: items=%#v active=%#v", session.Items, session.ActiveHistory)
	}
}

func TestSessionSendMessageMissingAndCorruptSessionErrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "corrupt-send")
	segmentsDir := filepath.Join(root, "corrupt-send", "segments")
	if err := os.MkdirAll(segmentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(segments) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(segmentsDir, "000001.jsonl"), []byte(`{"seq":1,"type":"item.appended","item":`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(corrupt segment) error = %v", err)
	}
	process := startSessionAPIServerWithTurnRunner(t, store, sessions.SessionV2{}, "registry-token", fakeSessionTurnRunner{})
	baseURL := "http://" + process.Addr()

	_, body := postRawJSONStatus(t, baseURL+"/sessions/missing-session/messages", `{"content":"hello"}`, "registry-token", http.StatusNotFound)
	assertErrorCode(t, body, "session_not_found")

	_, body = postRawJSONStatus(t, baseURL+"/sessions/corrupt-send/messages", `{"content":"hello"}`, "registry-token", http.StatusInternalServerError)
	assertErrorCode(t, body, "session_corrupted")
}

func TestSessionSendMessageSaveFailureDoesNotAppendItems(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "save-failure")
	runner := fakeSessionTurnRunner{
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			return SessionTurnResult{
				Session: request.Session,
				Items: []sessions.SessionItem{{
					Kind:       sessions.ItemKindMessage,
					Visibility: sessions.ItemVisibilityVisible,
					Audience:   sessions.ItemAudienceUser,
					Message:    &model.Message{Role: model.MessageRoleUser, Content: "failed save prompt"},
				}},
				ActiveHistory: []string{""},
			}, nil
		},
	}
	process := startSessionAPIServerWithTurnRunner(t, store, sessions.SessionV2{}, "registry-token", runner)
	conn := dialSessionStream(t, process, "save-failure")
	waitForStreamSubscribers(t, process, "save-failure", 1)

	raw, body := postRawJSONStatus(t, "http://"+process.Addr()+"/sessions/save-failure/messages", `{"content":"failed save prompt"}`, "registry-token", http.StatusInternalServerError)
	assertErrorCode(t, body, "session_store_error")
	if bytes.Contains(raw, []byte("failed save prompt")) {
		t.Fatalf("save failure response leaked prompt: %s", raw)
	}
	if got := readSessionStreamEvent(t, conn); got["type"] != "turn.started" {
		t.Fatalf("first event = %#v, want turn.started", got)
	}
	if got := readSessionStreamEvent(t, conn); got["type"] != "turn.failed" {
		t.Fatalf("second event = %#v, want turn.failed", got)
	}

	session, err := store.Load("save-failure")
	if err != nil {
		t.Fatalf("Load(save-failure) error = %v", err)
	}
	if len(session.Items) != 0 || len(session.ActiveHistory) != 0 {
		t.Fatalf("save failure changed session: items=%#v active=%#v", session.Items, session.ActiveHistory)
	}
}

func TestSessionCompactCommandPersistsCheckpointAndPublishesEvents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "compact-success")
	existing := appendServerTestItem(t, store, "compact-success", sessions.SessionItem{
		ID:         "existing-user",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "existing visible secret"},
	})
	if _, err := store.ReplaceActiveHistory("compact-success", []string{existing.ID}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}
	planner := fakeSessionCompactPlanner{
		plan: func(ctx context.Context, request SessionCompactionRequest) (SessionCompactionResult, error) {
			if request.Session.ID != "compact-success" {
				t.Fatalf("planner session id = %q, want compact-success", request.Session.ID)
			}
			summaryMessage := model.Message{Role: model.MessageRoleDeveloper, Content: "<compaction_summary>\nsummary secret\n</compaction_summary>"}
			summary := sessions.SessionItem{
				ID:         "summary-1",
				Kind:       sessions.ItemKindMessage,
				Visibility: sessions.ItemVisibilityHidden,
				Audience:   sessions.ItemAudienceModel,
				Message:    &summaryMessage,
			}
			return SessionCompactionResult{
				Session: request.Session,
				Compaction: SessionCompactionPlan{
					SummaryItem: summary,
					Checkpoint: sessions.CompactionCheckpoint{
						ID:                    "compact-1",
						Reason:                "user_requested",
						Phase:                 "manual",
						Trigger:               "manual",
						SummaryItemID:         summary.ID,
						PreviousActiveHistory: []string{existing.ID},
						ReplacementHistory:    []string{existing.ID, summary.ID},
					},
				},
			}, nil
		},
	}
	process := startSessionAPIServerWithCompactPlanner(t, store, sessions.SessionV2{}, "registry-token", planner)
	conn := dialSessionStream(t, process, "compact-success")
	waitForStreamSubscribers(t, process, "compact-success", 1)

	raw, body := postRawJSONStatus(t, "http://"+process.Addr()+"/sessions/compact-success/commands/compact", `{}`, "registry-token", http.StatusOK)
	if body["status"] != "committed" || body["compaction_id"] != "compact-1" || body["summary_item_id"] != "summary-1" || body["last_seq"] == nil {
		t.Fatalf("compact response = %#v, want committed metadata", body)
	}
	for _, forbidden := range []string{"summary secret", "existing visible secret", "<compaction_summary>", "registry-token"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("compact response leaked %q: %s", forbidden, raw)
		}
	}

	events := []map[string]any{
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
		readSessionStreamEvent(t, conn),
	}
	wantTypes := []string{"compact.started", "item.appended", "compaction.created", "active_history.replaced", "compact.completed"}
	for i, want := range wantTypes {
		if events[i]["type"] != want {
			t.Fatalf("event[%d] type = %#v, want %q; events=%#v", i, events[i]["type"], want, events)
		}
	}
	if events[0]["reason"] != "user_requested" || events[1]["item_id"] != "summary-1" || events[2]["compaction_id"] != "compact-1" || events[4]["last_seq"] != body["last_seq"] {
		t.Fatalf("compact events = %#v, want metadata only", events)
	}
	if _, ok := events[3]["item_ids"]; ok {
		t.Fatalf("active_history.replaced event leaked item_ids: %#v", events[3])
	}
	eventPayload, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("Marshal(events) error = %v", err)
	}
	if bytes.Contains(eventPayload, []byte("summary secret")) || bytes.Contains(eventPayload, []byte("existing visible secret")) || bytes.Contains(eventPayload, []byte("existing-user")) {
		t.Fatalf("compact events leaked content: %s", eventPayload)
	}

	session, err := store.Load("compact-success")
	if err != nil {
		t.Fatalf("Load(compact-success) error = %v", err)
	}
	if got := responseSessionItemIDs(session.Items); !reflect.DeepEqual(got, []string{"existing-user", "summary-1"}) {
		t.Fatalf("item IDs = %#v, want existing plus summary", got)
	}
	if len(session.Compactions) != 1 || session.Compactions[0].ID != "compact-1" {
		t.Fatalf("Compactions = %#v, want compact-1", session.Compactions)
	}
	if !reflect.DeepEqual(session.ActiveHistory, []string{"existing-user", "summary-1"}) {
		t.Fatalf("ActiveHistory = %#v, want replacement", session.ActiveHistory)
	}
	if session.Items[1].Visibility != sessions.ItemVisibilityHidden || session.Items[1].Message.Content != "<compaction_summary>\nsummary secret\n</compaction_summary>" {
		t.Fatalf("summary item = %#v, want hidden persisted summary", session.Items[1])
	}
}

func TestSessionCompactCommandRejectsBusySessionAndReflectsRunningStatus(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "compact-busy")
	existing := appendServerTestItem(t, store, "compact-busy", sessions.SessionItem{
		ID:         "existing-user",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "existing"},
	})
	if _, err := store.ReplaceActiveHistory("compact-busy", []string{existing.ID}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	planner := fakeSessionCompactPlanner{
		plan: func(ctx context.Context, request SessionCompactionRequest) (SessionCompactionResult, error) {
			close(started)
			<-release
			summaryMessage := model.Message{Role: model.MessageRoleDeveloper, Content: "<compaction_summary>\nplanned\n</compaction_summary>"}
			summary := sessions.SessionItem{
				ID:         "summary-1",
				Kind:       sessions.ItemKindMessage,
				Visibility: sessions.ItemVisibilityHidden,
				Audience:   sessions.ItemAudienceModel,
				Message:    &summaryMessage,
			}
			return SessionCompactionResult{
				Session: request.Session,
				Compaction: SessionCompactionPlan{
					SummaryItem: summary,
					Checkpoint: sessions.CompactionCheckpoint{
						ID:                    "compact-1",
						Reason:                "user_requested",
						Phase:                 "manual",
						Trigger:               "manual",
						SummaryItemID:         summary.ID,
						PreviousActiveHistory: []string{existing.ID},
						ReplacementHistory:    []string{existing.ID, summary.ID},
					},
				},
			}, nil
		},
	}
	runner := fakeSessionTurnRunner{
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			return SessionTurnResult{}, fmt.Errorf("turn runner should not run while compact is busy")
		},
	}
	process := startSessionAPIServerWithRunners(t, store, sessions.SessionV2{}, "registry-token", runner, planner)
	baseURL := "http://" + process.Addr()
	firstDone := make(chan map[string]any, 1)
	go func() {
		_, body := postRawJSONStatus(t, baseURL+"/sessions/compact-busy/commands/compact", `{}`, "registry-token", http.StatusOK)
		firstDone <- body
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for compact to start")
	}

	_, serverInfo := getRawJSON(t, baseURL+"/server")
	if serverInfo["running_turns"] != float64(1) {
		t.Fatalf("/server running_turns = %#v, want 1", serverInfo["running_turns"])
	}
	_, detail := getRawJSON(t, baseURL+"/sessions/compact-busy")
	if detail["status"] != "running" {
		t.Fatalf("session status = %#v, want running", detail["status"])
	}

	raw, body := postRawJSONStatus(t, baseURL+"/sessions/compact-busy/messages", `{"content":"secret prompt"}`, "registry-token", http.StatusConflict)
	assertErrorCode(t, body, "session_busy")
	if bytes.Contains(raw, []byte("secret prompt")) {
		t.Fatalf("busy message response leaked prompt: %s", raw)
	}
	_, body = postRawJSONStatus(t, baseURL+"/sessions/compact-busy/commands/compact", `{}`, "registry-token", http.StatusConflict)
	assertErrorCode(t, body, "session_busy")

	close(release)
	select {
	case body := <-firstDone:
		if body["status"] != "committed" {
			t.Fatalf("first compact response = %#v, want committed", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first compact to finish")
	}
}

func TestSessionCompactCommandFailureLeavesSessionUnchangedAndSanitizesErrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "compact-failure")
	existing := appendServerTestItem(t, store, "compact-failure", sessions.SessionItem{
		ID:         "existing-user",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "existing secret"},
	})
	if _, err := store.ReplaceActiveHistory("compact-failure", []string{existing.ID}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}
	before, err := store.Load("compact-failure")
	if err != nil {
		t.Fatalf("Load(before) error = %v", err)
	}
	planner := fakeSessionCompactPlanner{
		plan: func(ctx context.Context, request SessionCompactionRequest) (SessionCompactionResult, error) {
			return SessionCompactionResult{}, fmt.Errorf("summary provider leaked secret")
		},
	}
	process := startSessionAPIServerWithCompactPlanner(t, store, sessions.SessionV2{}, "registry-token", planner)
	conn := dialSessionStream(t, process, "compact-failure")
	waitForStreamSubscribers(t, process, "compact-failure", 1)

	raw, body := postRawJSONStatus(t, "http://"+process.Addr()+"/sessions/compact-failure/commands/compact", `{}`, "registry-token", http.StatusInternalServerError)
	assertErrorCode(t, body, "compact_failed")
	for _, forbidden := range []string{"summary provider leaked secret", "existing secret", "registry-token"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("compact failure response leaked %q: %s", forbidden, raw)
		}
	}
	if got := readSessionStreamEvent(t, conn); got["type"] != "compact.started" {
		t.Fatalf("first event = %#v, want compact.started", got)
	}
	if got := readSessionStreamEvent(t, conn); got["type"] != "compact.failed" || got["message"] != "compact failed" {
		t.Fatalf("second event = %#v, want sanitized compact.failed", got)
	}

	after, err := store.Load("compact-failure")
	if err != nil {
		t.Fatalf("Load(after) error = %v", err)
	}
	if !reflect.DeepEqual(after.Items, before.Items) || !reflect.DeepEqual(after.Compactions, before.Compactions) || !reflect.DeepEqual(after.ActiveHistory, before.ActiveHistory) || after.LastSeq != before.LastSeq {
		t.Fatalf("session changed after failed compact:\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestSessionCompactCommandRequiresTokenValidRequestAndExistingSession(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "compact-validation")
	saveServerTestSession(t, store, "compact-corrupt")
	segmentsDir := filepath.Join(root, "compact-corrupt", "segments")
	if err := os.MkdirAll(segmentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(segments) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(segmentsDir, "000001.jsonl"), []byte(`{"seq":1,"type":"item.appended","item":`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(corrupt segment) error = %v", err)
	}
	process := startSessionAPIServerWithCompactPlanner(t, store, sessions.SessionV2{}, "registry-token", fakeSessionCompactPlanner{})
	baseURL := "http://" + process.Addr()

	for _, tt := range []struct {
		name  string
		token string
	}{
		{name: "missing token"},
		{name: "wrong token", token: "wrong-token"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, body := postRawJSONStatus(t, baseURL+"/sessions/compact-validation/commands/compact", `{}`, tt.token, http.StatusForbidden)
			assertErrorCode(t, body, "permission_denied")
			if bytes.Contains(raw, []byte("registry-token")) {
				t.Fatalf("permission error leaked registry token: %s", raw)
			}
		})
	}

	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "non-empty object", body: `{"content":"secret prompt"}`},
		{name: "null", body: `null`},
		{name: "array", body: `[]`},
		{name: "string", body: `"secret prompt"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, body := postRawJSONStatus(t, baseURL+"/sessions/compact-validation/commands/compact", tt.body, "registry-token", http.StatusBadRequest)
			assertErrorCode(t, body, "invalid_request")
			if bytes.Contains(raw, []byte("secret prompt")) {
				t.Fatalf("invalid request leaked request content: %s", raw)
			}
		})
	}
	_, body := postRawJSONStatus(t, baseURL+"/sessions/missing-session/commands/compact", `{}`, "registry-token", http.StatusNotFound)
	assertErrorCode(t, body, "session_not_found")
	_, body = postRawJSONStatus(t, baseURL+"/sessions/compact-corrupt/commands/compact", `{}`, "registry-token", http.StatusInternalServerError)
	assertErrorCode(t, body, "session_corrupted")

	session, err := store.Load("compact-validation")
	if err != nil {
		t.Fatalf("Load(compact-validation) error = %v", err)
	}
	if len(session.Items) != 0 || len(session.Compactions) != 0 || len(session.ActiveHistory) != 0 {
		t.Fatalf("validation/security requests changed session: items=%#v compactions=%#v active=%#v", session.Items, session.Compactions, session.ActiveHistory)
	}
}

func TestSessionItemsPaginationBeforeAfter(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "session-items")
	for i := 1; i <= 5; i++ {
		appendServerTestItem(t, store, "session-items", sessions.SessionItem{
			ID:         fmt.Sprintf("item-%d", i),
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: fmt.Sprintf("message-%d", i)},
		})
	}
	process := startSessionAPIServer(t, store, sessions.SessionV2{})
	baseURL := "http://" + process.Addr()

	_, before := getRawJSON(t, baseURL+"/sessions/session-items/items?before_seq=5&limit=2")
	if got := responseItemSeqs(t, before); !reflect.DeepEqual(got, []int64{3, 4}) {
		t.Fatalf("before_seq page seqs = %#v, want [3 4]", got)
	}
	if before["oldest_seq"] != float64(3) || before["newest_seq"] != float64(4) {
		t.Fatalf("before_seq cursors = oldest:%#v newest:%#v, want 3/4", before["oldest_seq"], before["newest_seq"])
	}
	if before["has_more_before"] != true || before["has_more_after"] != true {
		t.Fatalf("before_seq booleans = before:%#v after:%#v, want true/true", before["has_more_before"], before["has_more_after"])
	}

	_, after := getRawJSON(t, baseURL+"/sessions/session-items/items?after_seq=2&limit=2")
	if got := responseItemSeqs(t, after); !reflect.DeepEqual(got, []int64{3, 4}) {
		t.Fatalf("after_seq page seqs = %#v, want [3 4]", got)
	}
	if after["oldest_seq"] != float64(3) || after["newest_seq"] != float64(4) {
		t.Fatalf("after_seq cursors = oldest:%#v newest:%#v, want 3/4", after["oldest_seq"], after["newest_seq"])
	}
	if after["has_more_before"] != true || after["has_more_after"] != true {
		t.Fatalf("after_seq booleans = before:%#v after:%#v, want true/true", after["has_more_before"], after["has_more_after"])
	}
}

func TestSessionItemsEmptyPageCursorFields(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "empty-session")
	process := startSessionAPIServer(t, store, sessions.SessionV2{})
	baseURL := "http://" + process.Addr()

	_, body := getRawJSON(t, baseURL+"/sessions/empty-session/items")
	if got := responseItems(t, body); len(got) != 0 {
		t.Fatalf("empty page items = %#v, want empty", got)
	}
	if body["oldest_seq"] != float64(0) || body["newest_seq"] != float64(0) {
		t.Fatalf("empty page cursors = oldest:%#v newest:%#v, want 0/0", body["oldest_seq"], body["newest_seq"])
	}
	if body["has_more_before"] != false || body["has_more_after"] != false {
		t.Fatalf("empty page booleans = before:%#v after:%#v, want false/false", body["has_more_before"], body["has_more_after"])
	}
}

func TestSessionItemsDefaultAndMaxLimits(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "limit-session")
	total := maxSessionItemsLimit + 1
	for i := 1; i <= total; i++ {
		appendServerTestItem(t, store, "limit-session", sessions.SessionItem{
			ID:         fmt.Sprintf("item-%03d", i),
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: fmt.Sprintf("message-%d", i)},
		})
	}
	process := startSessionAPIServer(t, store, sessions.SessionV2{})
	baseURL := "http://" + process.Addr()

	_, defaultPage := getRawJSON(t, baseURL+"/sessions/limit-session/items")
	defaultSeqs := responseItemSeqs(t, defaultPage)
	if len(defaultSeqs) != defaultSessionItemsLimit {
		t.Fatalf("default page len = %d, want %d", len(defaultSeqs), defaultSessionItemsLimit)
	}
	if defaultSeqs[0] != int64(total-defaultSessionItemsLimit+1) || defaultSeqs[len(defaultSeqs)-1] != int64(total) {
		t.Fatalf("default page seqs first/last = %d/%d, want latest %d items through %d", defaultSeqs[0], defaultSeqs[len(defaultSeqs)-1], defaultSessionItemsLimit, total)
	}
	if defaultPage["oldest_seq"] != float64(total-defaultSessionItemsLimit+1) || defaultPage["newest_seq"] != float64(total) {
		t.Fatalf("default cursors = oldest:%#v newest:%#v, want latest page bounds", defaultPage["oldest_seq"], defaultPage["newest_seq"])
	}
	if defaultPage["has_more_before"] != true || defaultPage["has_more_after"] != false {
		t.Fatalf("default booleans = before:%#v after:%#v, want true/false", defaultPage["has_more_before"], defaultPage["has_more_after"])
	}

	_, maxPage := getRawJSON(t, fmt.Sprintf("%s/sessions/limit-session/items?limit=%d", baseURL, maxSessionItemsLimit+100))
	maxSeqs := responseItemSeqs(t, maxPage)
	if len(maxSeqs) != maxSessionItemsLimit {
		t.Fatalf("max-clamped page len = %d, want %d", len(maxSeqs), maxSessionItemsLimit)
	}
	if maxSeqs[0] != 2 || maxSeqs[len(maxSeqs)-1] != int64(total) {
		t.Fatalf("max-clamped page seqs first/last = %d/%d, want 2/%d", maxSeqs[0], maxSeqs[len(maxSeqs)-1], total)
	}
	if maxPage["has_more_before"] != true || maxPage["has_more_after"] != false {
		t.Fatalf("max booleans = before:%#v after:%#v, want true/false", maxPage["has_more_before"], maxPage["has_more_after"])
	}
}

func TestSessionItemsChatAndDebugFilteringAndNarrowDTO(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "filter-session")

	appendServerTestItem(t, store, "filter-session", sessions.SessionItem{
		ID:         "visible-user",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "hello"},
	})
	largeContent := strings.Repeat("L", sessionItemInlineMessageBytes) + "SECRET-LARGE-TAIL"
	appendServerTestItem(t, store, "filter-session", sessions.SessionItem{
		ID:         "visible-assistant",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceModel,
		Message: &model.Message{
			Role:    model.MessageRoleAssistant,
			Content: largeContent,
			ToolCalls: []model.ToolCall{{
				ID:        "call-secret",
				Name:      "read_file",
				Arguments: "SECRET TOOL ARGUMENTS",
			}},
		},
	})
	if _, err := store.ReplaceActiveHistory("filter-session", []string{"visible-user"}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}
	_, err := store.AppendCompactionCheckpoint("filter-session", sessions.SessionItem{
		ID:         "summary-1",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityHidden,
		Audience:   sessions.ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "compaction summary secret"},
	}, sessions.CompactionCheckpoint{
		ID:                    "compact-1",
		Reason:                "test",
		Phase:                 "manual",
		Trigger:               "manual",
		SummaryItemID:         "summary-1",
		PreviousActiveHistory: []string{"visible-user"},
		ReplacementHistory:    []string{"visible-user", "summary-1"},
	})
	if err != nil {
		t.Fatalf("AppendCompactionCheckpoint() error = %v", err)
	}
	appendServerTestItem(t, store, "filter-session", sessions.SessionItem{
		ID:         "debug-item",
		Kind:       sessions.ItemKindRuntimeContext,
		Visibility: sessions.ItemVisibilityDebug,
		Audience:   sessions.ItemAudienceInternal,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "debug secret"},
	})
	appendServerTestItem(t, store, "filter-session", sessions.SessionItem{
		ID:         "tool-result",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleTool, Content: "tool result secret", ToolCallID: "call-secret", IsError: true},
	})

	process := startSessionAPIServerWithToken(t, store, sessions.SessionV2{}, "registry-token")
	baseURL := "http://" + process.Addr()

	chatRaw, chat := getRawJSON(t, baseURL+"/sessions/filter-session/items")
	assertNoItemDTOLeak(t, chatRaw)
	if got := responseItemIDs(t, chat); !reflect.DeepEqual(got, []string{"visible-user", "visible-assistant"}) {
		t.Fatalf("chat item IDs = %#v, want visible chat-facing items only", got)
	}
	for _, forbidden := range []string{"compaction summary secret", "debug secret", "tool result secret", "SECRET-LARGE-TAIL", "SECRET TOOL ARGUMENTS"} {
		if bytes.Contains(chatRaw, []byte(forbidden)) {
			t.Fatalf("chat response leaked %q: %s", forbidden, chatRaw)
		}
	}
	items := chat["items"].([]any)
	userMessage := items[0].(map[string]any)["message"].(map[string]any)
	userContent := userMessage["content"].(map[string]any)
	if userContent["inline"] != "hello" {
		t.Fatalf("small message content = %#v, want inline hello", userContent)
	}
	assistantMessage := items[1].(map[string]any)["message"].(map[string]any)
	assistantContent := assistantMessage["content"].(map[string]any)
	if _, ok := assistantContent["inline"]; ok {
		t.Fatalf("large message content unexpectedly included inline: %#v", assistantContent)
	}
	if assistantContent["truncated"] != true || assistantContent["size_bytes"] != float64(len(largeContent)) {
		t.Fatalf("large message content metadata = %#v, want truncated size", assistantContent)
	}

	for _, tt := range []struct {
		name  string
		token string
	}{
		{name: "missing token"},
		{name: "wrong token", token: "wrong-token"},
	} {
		t.Run("debug "+tt.name, func(t *testing.T) {
			raw, body := getRawJSONStatus(t, baseURL+"/sessions/filter-session/items?view=debug&limit=20", tt.token, http.StatusForbidden)
			assertErrorCode(t, body, "permission_denied")
			for _, forbidden := range []string{"compaction summary secret", "debug secret", "tool result secret", "SECRET-LARGE-TAIL", "SECRET TOOL ARGUMENTS", "registry-token"} {
				if bytes.Contains(raw, []byte(forbidden)) {
					t.Fatalf("debug permission error leaked %q: %s", forbidden, raw)
				}
			}
		})
	}

	debugRaw, debug := getRawJSONStatus(t, baseURL+"/sessions/filter-session/items?view=debug&limit=20", "registry-token", http.StatusOK)
	assertNoItemDTOLeak(t, debugRaw)
	if got := responseItemIDs(t, debug); !reflect.DeepEqual(got, []string{"visible-user", "visible-assistant", "summary-1", "debug-item", "tool-result"}) {
		t.Fatalf("debug item IDs = %#v, want all items", got)
	}
	for _, want := range []string{"compaction summary secret", "debug secret", "tool result secret"} {
		if !bytes.Contains(debugRaw, []byte(want)) {
			t.Fatalf("debug response missing %q: %s", want, debugRaw)
		}
	}
	if bytes.Contains(debugRaw, []byte("SECRET TOOL ARGUMENTS")) {
		t.Fatalf("debug response leaked tool call arguments: %s", debugRaw)
	}
}

func TestSessionItemEndpointRefetchesUpdatedToolItemAndKeepsChatViewNarrow(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "item-detail")

	appendServerTestItem(t, store, "item-detail", sessions.SessionItem{
		ID:         "visible-assistant",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceModel,
		Message: &model.Message{
			Role:    model.MessageRoleAssistant,
			Content: "assistant safe content",
			ToolCalls: []model.ToolCall{{
				ID:        "call-secret",
				Name:      "read_file",
				Arguments: "SECRET TOOL ARGUMENTS",
			}},
		},
	})
	appendServerTestItem(t, store, "item-detail", sessions.SessionItem{
		ID:         "hidden-summary",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityHidden,
		Audience:   sessions.ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "hidden summary secret"},
	})
	pending := appendServerTestItem(t, store, "item-detail", sessions.SessionItem{
		ID:         "tool-result",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceModel,
		Status:     sessions.ItemStatusPending,
		Message:    &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-secret"},
	})
	largeToolContent := strings.Repeat("tool output ", 500) + "TOOL-RESULT-TAIL"
	updated := pending
	updated.Status = sessions.ItemStatusError
	updated.Message = &model.Message{Role: model.MessageRoleTool, Content: largeToolContent, ToolCallID: "call-secret", IsError: true}
	if _, err := store.UpdateItem("item-detail", updated); err != nil {
		t.Fatalf("UpdateItem(tool-result) error = %v", err)
	}

	process := startSessionAPIServerWithToken(t, store, sessions.SessionV2{}, "registry-token")
	baseURL := "http://" + process.Addr()

	assistantRaw, assistant := getRawJSONStatus(t, baseURL+"/sessions/item-detail/items/visible-assistant", "registry-token", http.StatusOK)
	assertNoItemDTOLeak(t, assistantRaw)
	if assistant["id"] != "visible-assistant" {
		t.Fatalf("assistant item id = %#v, want visible-assistant", assistant["id"])
	}
	if !bytes.Contains(assistantRaw, []byte("assistant safe content")) {
		t.Fatalf("assistant chat item missing visible content: %s", assistantRaw)
	}
	if bytes.Contains(assistantRaw, []byte("SECRET TOOL ARGUMENTS")) {
		t.Fatalf("assistant chat item leaked tool call arguments: %s", assistantRaw)
	}

	for _, tt := range []struct {
		name  string
		token string
	}{
		{name: "missing token"},
		{name: "wrong token", token: "wrong-token"},
	} {
		t.Run("debug "+tt.name, func(t *testing.T) {
			raw, body := getRawJSONStatus(t, baseURL+"/sessions/item-detail/items/tool-result?view=debug", tt.token, http.StatusForbidden)
			assertErrorCode(t, body, "permission_denied")
			for _, forbidden := range []string{"TOOL-RESULT-TAIL", "SECRET TOOL ARGUMENTS", "registry-token"} {
				if bytes.Contains(raw, []byte(forbidden)) {
					t.Fatalf("debug permission error leaked %q: %s", forbidden, raw)
				}
			}
		})
	}

	raw, body := getRawJSONStatus(t, baseURL+"/sessions/item-detail/items/tool-result", "registry-token", http.StatusNotFound)
	assertErrorCode(t, body, "item_not_found")
	if bytes.Contains(raw, []byte("TOOL-RESULT-TAIL")) {
		t.Fatalf("chat tool item error leaked tool result: %s", raw)
	}
	raw, body = getRawJSONStatus(t, baseURL+"/sessions/item-detail/items/hidden-summary", "registry-token", http.StatusNotFound)
	assertErrorCode(t, body, "item_not_found")
	if bytes.Contains(raw, []byte("hidden summary secret")) {
		t.Fatalf("chat hidden item error leaked hidden content: %s", raw)
	}

	debugRaw, debug := getRawJSONStatus(t, baseURL+"/sessions/item-detail/items/tool-result?view=debug", "registry-token", http.StatusOK)
	if debug["status"] != sessions.ItemStatusError {
		t.Fatalf("debug tool status = %#v, want error; body=%#v", debug["status"], debug)
	}
	message := debug["message"].(map[string]any)
	if message["role"] != string(model.MessageRoleTool) || message["tool_call_id"] != "call-secret" || message["is_error"] != true {
		t.Fatalf("debug tool message = %#v, want role/tool_call_id/is_error", message)
	}
	content := message["content"].(map[string]any)
	if content["inline"] != largeToolContent {
		t.Fatalf("debug tool content = %#v, want full inline blob-resolved content", content)
	}
	if !bytes.Contains(debugRaw, []byte("TOOL-RESULT-TAIL")) {
		t.Fatalf("debug tool item missing blob-resolved tail: %s", debugRaw)
	}

	raw, body = getRawJSONStatus(t, baseURL+"/sessions/item-detail/items/tool-result/content?view=debug", "registry-token", http.StatusNotFound)
	assertErrorCode(t, body, "not_found")
	if bytes.Contains(raw, []byte("TOOL-RESULT-TAIL")) {
		t.Fatalf("legacy item content route leaked tool result: %s", raw)
	}
}

func TestSessionItemsBadQueryStructuredErrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "bad-query-session")
	process := startSessionAPIServer(t, store, sessions.SessionV2{})
	baseURL := "http://" + process.Addr()

	for _, tt := range []struct {
		name  string
		query string
	}{
		{name: "malformed before", query: "before_seq=abc"},
		{name: "negative after", query: "after_seq=-1"},
		{name: "malformed limit", query: "limit=abc"},
		{name: "negative limit", query: "limit=-1"},
		{name: "bad view", query: "view=all"},
		{name: "both cursors", query: "before_seq=2&after_seq=1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, baseURL+"/sessions/bad-query-session/items?"+tt.query, nil)
			if err != nil {
				t.Fatalf("NewRequest(items?%s) error = %v", tt.query, err)
			}
			req.Header.Set("Authorization", "Bearer registry-token")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Get(items?%s) error = %v", tt.query, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			body := decodeJSON(t, resp)
			if got := body["error"].(map[string]any)["code"]; got != "invalid_query" {
				t.Fatalf("error = %#v, want invalid_query", body)
			}
		})
	}
}

func TestSessionItemsMissingAndCorruptSessionErrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "corrupt-session")
	segmentsDir := filepath.Join(root, "corrupt-session", "segments")
	if err := os.MkdirAll(segmentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(segments) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(segmentsDir, "000001.jsonl"), []byte(`{"seq":1,"type":"item.appended","item":`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(corrupt segment) error = %v", err)
	}
	process := startSessionAPIServer(t, store, sessions.SessionV2{})
	baseURL := "http://" + process.Addr()

	req, err := http.NewRequest(http.MethodGet, baseURL+"/sessions/missing-session/items", nil)
	if err != nil {
		t.Fatalf("NewRequest(missing items) error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer registry-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Get(missing items) error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing items status = %d, want 404", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	if got := body["error"].(map[string]any)["code"]; got != "session_not_found" {
		t.Fatalf("missing items error = %#v, want session_not_found", body)
	}

	req, err = http.NewRequest(http.MethodGet, baseURL+"/sessions/corrupt-session/items", nil)
	if err != nil {
		t.Fatalf("NewRequest(corrupt items) error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer registry-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Get(corrupt items) error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("corrupt items status = %d, want 500", resp.StatusCode)
	}
	body = decodeJSON(t, resp)
	if got := body["error"].(map[string]any)["code"]; got != "session_store_error" {
		t.Fatalf("corrupt items error = %#v, want session_store_error", body)
	}
}

func TestServerShutdownRequiresRegistryToken(t *testing.T) {
	process := startSessionAPIServerWithToken(t, sessions.NewV2Store(filepath.Join(t.TempDir(), "sessions")), sessions.SessionV2{}, "registry-token")
	baseURL := "http://" + process.Addr()

	for _, tt := range []struct {
		name  string
		token string
	}{
		{name: "missing token"},
		{name: "wrong token", token: "wrong-token"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, body := postRawJSONStatus(t, baseURL+"/server/shutdown", "", tt.token, http.StatusForbidden)
			assertErrorCode(t, body, "permission_denied")
			if bytes.Contains(raw, []byte("registry-token")) {
				t.Fatalf("permission error leaked registry token: %s", raw)
			}
			waitForHealthyServer(t, process.Addr())
		})
	}

	_, body := postRawJSONStatus(t, baseURL+"/server/shutdown", "", "registry-token", http.StatusOK)
	if body["status"] != "shutting_down" {
		t.Fatalf("shutdown response = %#v, want shutting_down", body)
	}
}

func TestLegacySessionItemContentRouteIsRemoved(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "content-session")
	appendServerTestItem(t, store, "content-session", sessions.SessionItem{
		ID:         "visible-user",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "hello chat"},
	})
	process := startSessionAPIServer(t, store, sessions.SessionV2{})
	baseURL := "http://" + process.Addr()

	raw, body := getRawJSONStatus(t, baseURL+"/sessions/content-session/items/visible-user/content", "registry-token", http.StatusNotFound)
	assertErrorCode(t, body, "not_found")
	if bytes.Contains(raw, []byte("hello chat")) {
		t.Fatalf("removed item content route leaked item content: %s", raw)
	}
}

func TestSessionBlobContentEndpointRequiresSessionReachability(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "blob-session")
	saveServerTestSession(t, store, "other-session")

	largeVisibleContent := strings.Repeat("blob-backed visible content ", 300) + "VISIBLE-BLOB-TAIL"
	loaded, err := store.Load("blob-session")
	if err != nil {
		t.Fatalf("Load(blob-session) error = %v", err)
	}
	saved, err := store.SaveTurn(loaded, []sessions.SessionItem{{
		ID:         "visible-blob",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: largeVisibleContent},
	}}, []string{"visible-blob"})
	if err != nil {
		t.Fatalf("SaveTurn(visible blob) error = %v", err)
	}
	visibleRef := *saved.Items[0].Content.Blob
	hiddenRef, err := store.WriteBlob([]byte("hidden blob secret"), "utf-8", "text/plain")
	if err != nil {
		t.Fatalf("WriteBlob(hidden) error = %v", err)
	}
	orphanRef, err := store.WriteBlob([]byte("orphan blob secret"), "utf-8", "text/plain")
	if err != nil {
		t.Fatalf("WriteBlob(orphan) error = %v", err)
	}

	appendServerTestItem(t, store, "blob-session", sessions.SessionItem{
		ID:         "hidden-blob",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityHidden,
		Audience:   sessions.ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleDeveloper},
		Content: &sessions.StoredContent{
			Blob:    &hiddenRef,
			Preview: "hidden preview",
		},
	})

	process := startSessionAPIServerWithToken(t, store, sessions.SessionV2{}, "registry-token")
	baseURL := "http://" + process.Addr()

	itemsRaw, itemsBody := getRawJSON(t, baseURL+"/sessions/blob-session/items")
	assertNoItemDTOLeak(t, itemsRaw)
	if got := responseItemIDs(t, itemsBody); !reflect.DeepEqual(got, []string{"visible-blob"}) {
		t.Fatalf("chat item IDs = %#v, want visible blob item only", got)
	}
	itemContent := responseItems(t, itemsBody)[0].(map[string]any)["message"].(map[string]any)["content"].(map[string]any)
	blobDTO := itemContent["blob"].(map[string]any)
	if blobDTO["hash"] != visibleRef.Hash || blobDTO["size_bytes"] != float64(visibleRef.SizeBytes) || itemContent["inline"] != nil {
		t.Fatalf("blob item DTO content = %#v, want blob ref without inline", itemContent)
	}
	if bytes.Contains(itemsRaw, []byte("VISIBLE-BLOB-TAIL")) || bytes.Contains(itemsRaw, []byte("hidden blob secret")) {
		t.Fatalf("items response leaked blob body: %s", itemsRaw)
	}

	raw, body := getRawJSONStatus(t, baseURL+"/sessions/blob-session/content/"+visibleRef.Hash, "registry-token", http.StatusOK)
	assertNoContentDTOLeak(t, raw)
	if body["blob_hash"] != visibleRef.Hash || body["content"] != largeVisibleContent || body["encoding"] != "utf-8" || body["media_type"] != "text/plain" {
		t.Fatalf("blob content response = %#v, want visible content and metadata", body)
	}
	if body["offset"] != float64(0) || body["size_bytes"] != float64(visibleRef.SizeBytes) || body["bytes_returned"] != float64(visibleRef.SizeBytes) || body["has_more"] != false {
		t.Fatalf("blob content range metadata = %#v, want full content", body)
	}

	_, body = getRawJSONStatus(t, baseURL+"/sessions/blob-session/content/"+visibleRef.Hash+"?offset=5&max_bytes=4", "registry-token", http.StatusOK)
	if body["content"] != "back" || body["offset"] != float64(5) || body["bytes_returned"] != float64(4) || body["has_more"] != true {
		t.Fatalf("blob content range response = %#v, want offset 5 max 4", body)
	}

	raw, body = getRawJSONStatus(t, baseURL+"/sessions/blob-session/items/visible-blob/content", "registry-token", http.StatusNotFound)
	assertErrorCode(t, body, "not_found")
	if bytes.Contains(raw, []byte("VISIBLE-BLOB-TAIL")) {
		t.Fatalf("removed item content route leaked visible blob content: %s", raw)
	}

	raw, body = getRawJSONStatus(t, baseURL+"/sessions/blob-session/content/"+hiddenRef.Hash, "registry-token", http.StatusNotFound)
	assertErrorCode(t, body, "content_unavailable")
	if bytes.Contains(raw, []byte("hidden blob secret")) {
		t.Fatalf("chat blob content error leaked hidden content: %s", raw)
	}
	_, body = getRawJSONStatus(t, baseURL+"/sessions/blob-session/content/"+hiddenRef.Hash+"?view=debug", "registry-token", http.StatusOK)
	if body["content"] != "hidden blob secret" {
		t.Fatalf("debug blob content response = %#v, want hidden content", body)
	}

	raw, body = getRawJSONStatus(t, baseURL+"/sessions/other-session/content/"+visibleRef.Hash, "registry-token", http.StatusNotFound)
	assertErrorCode(t, body, "content_unavailable")
	if bytes.Contains(raw, []byte("VISIBLE-BLOB-TAIL")) {
		t.Fatalf("other-session blob content error leaked visible content: %s", raw)
	}
	raw, body = getRawJSONStatus(t, baseURL+"/sessions/blob-session/content/"+orphanRef.Hash, "registry-token", http.StatusNotFound)
	assertErrorCode(t, body, "content_unavailable")
	if bytes.Contains(raw, []byte("orphan blob secret")) {
		t.Fatalf("orphan blob content error leaked orphan content: %s", raw)
	}

	_, body = getRawJSONStatus(t, baseURL+"/sessions/blob-session/content/not-a-hash", "registry-token", http.StatusBadRequest)
	assertErrorCode(t, body, "invalid_blob_hash")
	_, body = getRawJSONStatus(t, baseURL+"/content/"+visibleRef.Hash, "registry-token", http.StatusNotFound)
	assertErrorCode(t, body, "not_found")
}

func TestLegacySessionItemContentRouteRemovedAndDoesNotLeakPrivateContent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "private-content-session")
	appendServerTestItem(t, store, "private-content-session", sessions.SessionItem{
		ID:         "hidden-summary",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityHidden,
		Audience:   sessions.ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "hidden summary secret"},
	})
	appendServerTestItem(t, store, "private-content-session", sessions.SessionItem{
		ID:         "runtime-context",
		Kind:       sessions.ItemKindRuntimeContext,
		Visibility: sessions.ItemVisibilityDebug,
		Audience:   sessions.ItemAudienceInternal,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "runtime context secret"},
	})
	appendServerTestItem(t, store, "private-content-session", sessions.SessionItem{
		ID:         "tool-result",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleTool, Content: "tool result secret", ToolCallID: "call-secret", IsError: true},
	})
	process := startSessionAPIServerWithToken(t, store, sessions.SessionV2{}, "registry-token")
	baseURL := "http://" + process.Addr()

	for _, tt := range []struct {
		name  string
		token string
	}{
		{name: "missing token"},
		{name: "wrong token", token: "wrong-token"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, body := getRawJSONStatus(t, baseURL+"/sessions/private-content-session/items/hidden-summary/content?view=debug", tt.token, http.StatusForbidden)
			assertErrorCode(t, body, "permission_denied")
			if bytes.Contains(raw, []byte("hidden summary secret")) {
				t.Fatalf("permission error leaked debug content: %s", raw)
			}
		})
	}

	for _, itemID := range []string{"hidden-summary", "runtime-context", "tool-result"} {
		raw, body := getRawJSONStatus(t, baseURL+"/sessions/private-content-session/items/"+itemID+"/content?view=debug", "registry-token", http.StatusNotFound)
		assertErrorCode(t, body, "not_found")
		for _, forbidden := range []string{"hidden summary secret", "runtime context secret", "tool result secret", "call-secret"} {
			if bytes.Contains(raw, []byte(forbidden)) {
				t.Fatalf("removed item content route for %s leaked %q: %s", itemID, forbidden, raw)
			}
		}
	}
}

func TestLegacySessionItemContentRouteRemovedForAllVariants(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := sessions.NewV2Store(root)
	saveServerTestSession(t, store, "content-errors")
	appendServerTestItem(t, store, "content-errors", sessions.SessionItem{
		ID:         "empty-message",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser},
	})
	process := startSessionAPIServer(t, store, sessions.SessionV2{})
	baseURL := "http://" + process.Addr()

	for _, endpoint := range []string{
		"/sessions/missing-session/items/item-1/content",
		"/sessions/content-errors/items/missing-item/content",
		"/sessions/content-errors/items/empty-message/content",
		"/sessions/content-errors/items/empty-message/content?offset=abc",
		"/sessions/content-errors/items/empty-message/content?view=all",
	} {
		_, body := getRawJSONStatus(t, baseURL+endpoint, "registry-token", http.StatusNotFound)
		assertErrorCode(t, body, "not_found")
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

	req, err := http.NewRequest(http.MethodGet, "http://"+process.Addr()+"/sessions/corrupt-session", nil)
	if err != nil {
		t.Fatalf("NewRequest(corrupt session) error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer registry-token")
	resp, err := http.DefaultClient.Do(req)
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

	return startSessionAPIServerWithToken(t, store, defaults, "registry-token")
}

func startSessionAPIServerWithToken(t *testing.T, store *sessions.V2Store, defaults sessions.SessionV2, token string) *Process {
	t.Helper()

	return startSessionAPIServerWithTurnRunner(t, store, defaults, token, nil)
}

func startSessionAPIServerWithTurnRunner(t *testing.T, store *sessions.V2Store, defaults sessions.SessionV2, token string, runner SessionTurnRunner) *Process {
	t.Helper()

	return startSessionAPIServerWithRunners(t, store, defaults, token, runner, nil)
}

func startSessionAPIServerWithCompactPlanner(t *testing.T, store *sessions.V2Store, defaults sessions.SessionV2, token string, planner SessionCompactPlanner) *Process {
	t.Helper()

	return startSessionAPIServerWithRunners(t, store, defaults, token, nil, planner)
}

func startSessionAPIServerWithRunners(t *testing.T, store *sessions.V2Store, defaults sessions.SessionV2, token string, runner SessionTurnRunner, planner SessionCompactPlanner) *Process {
	t.Helper()

	process, err := Start(Options{
		CWD:             t.TempDir(),
		Listen:          "127.0.0.1:0",
		Version:         "test-version",
		AuthToken:       token,
		SessionStore:    store,
		SessionDefaults: defaults,
		TurnRunner:      runner,
		CompactPlanner:  planner,
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

func startProjectSessionAPIServer(t *testing.T, projectStore *projectstore.Store, sessionStore *sessions.V2Store, defaults sessions.SessionV2, token string) *Process {
	t.Helper()

	process, err := Start(Options{
		CWD:             defaults.CWD,
		Listen:          "127.0.0.1:0",
		Version:         "test-version",
		AuthToken:       token,
		ProjectStore:    projectStore,
		SessionStore:    sessionStore,
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

func mkdirServerTestDir(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", path, err)
	}
	return path
}

func archiveProjectForServerTest(t *testing.T, store *projectstore.Store, project projectstore.Project) {
	t.Helper()

	if _, err := store.Archive(project.ID); err != nil {
		t.Fatalf("Archive(%s) error = %v", project.ID, err)
	}
}

func writeCorruptSessionSegmentForServerTest(t *testing.T, sessionRoot, sessionID string) {
	t.Helper()

	segmentsDir := filepath.Join(sessionRoot, sessionID, "segments")
	if err := os.MkdirAll(segmentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", segmentsDir, err)
	}
	path := filepath.Join(segmentsDir, "000001.jsonl")
	if err := os.WriteFile(path, []byte(`{"seq":1,"type":"item.appended","item":`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func sessionMetadataIDs(items []SessionMetadata) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func sessionMetadataIDSet(items []SessionMetadata) map[string]bool {
	ids := make(map[string]bool, len(items))
	for _, item := range items {
		ids[item.ID] = true
	}
	return ids
}

func stringSliceFromJSON(t *testing.T, value any) []string {
	t.Helper()

	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("value = %T(%#v), want JSON array", value, value)
	}
	items := make([]string, 0, len(raw))
	for _, item := range raw {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("array item = %T(%#v), want string", item, item)
		}
		items = append(items, text)
	}
	return items
}

type fakeSessionTurnRunner struct {
	run func(context.Context, SessionTurnRequest) (SessionTurnResult, error)
}

func (r fakeSessionTurnRunner) RunSessionTurn(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
	if r.run == nil {
		return SessionTurnResult{}, fmt.Errorf("fake turn runner was called unexpectedly")
	}
	return r.run(ctx, request)
}

type fakeIncrementalSessionTurnRunner struct {
	run     func(context.Context, SessionTurnRequest) (SessionTurnResult, error)
	support func(context.Context, SessionTurnRequest) (bool, error)
	plan    func(context.Context, SessionTurnRequest) (SessionCompactionResult, error)
}

func (r fakeIncrementalSessionTurnRunner) RunSessionTurn(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
	if r.run == nil {
		return SessionTurnResult{}, fmt.Errorf("fake incremental turn runner was called unexpectedly")
	}
	return r.run(ctx, request)
}

func (r fakeIncrementalSessionTurnRunner) SupportsIncrementalSessionTurn(ctx context.Context, request SessionTurnRequest) (bool, error) {
	if r.support != nil {
		return r.support(ctx, request)
	}
	return true, nil
}

func (r fakeIncrementalSessionTurnRunner) PlanSessionTurnCompaction(ctx context.Context, request SessionTurnRequest) (SessionCompactionResult, error) {
	if r.plan != nil {
		return r.plan(ctx, request)
	}
	return SessionCompactionResult{}, nil
}

type fakeSessionCompactPlanner struct {
	plan func(context.Context, SessionCompactionRequest) (SessionCompactionResult, error)
}

func (p fakeSessionCompactPlanner) PlanSessionCompaction(ctx context.Context, request SessionCompactionRequest) (SessionCompactionResult, error) {
	if p.plan == nil {
		return SessionCompactionResult{}, fmt.Errorf("fake compact planner was called unexpectedly")
	}
	return p.plan(ctx, request)
}

func serverTestTurnResult(session sessions.SessionV2, messages ...model.Message) SessionTurnResult {
	existingIDs := map[string]struct{}{}
	for _, item := range session.Items {
		existingIDs[item.ID] = struct{}{}
	}
	activeHistory := append([]string(nil), session.ActiveHistory...)
	items := make([]sessions.SessionItem, 0, len(messages))
	for _, message := range messages {
		id := serverTestNextSessionItemID(existingIDs, message)
		item := serverTestSessionItemFromMessage(id, message)
		items = append(items, item)
		activeHistory = append(activeHistory, id)
		existingIDs[id] = struct{}{}
	}
	return SessionTurnResult{
		Session:       session,
		Items:         items,
		ActiveHistory: activeHistory,
	}
}

func serverTestNextSessionItemID(existing map[string]struct{}, message model.Message) string {
	prefix := "msg"
	if message.Role == model.MessageRoleSystem || message.Role == model.MessageRoleDeveloper {
		prefix = "runtime"
	}
	for i := len(existing) + 1; ; i++ {
		id := fmt.Sprintf("%s-%06d", prefix, i)
		if _, ok := existing[id]; !ok {
			return id
		}
	}
}

func serverTestSessionItemFromMessage(id string, message model.Message) sessions.SessionItem {
	messageCopy := message
	messageCopy.ToolCalls = append([]model.ToolCall(nil), message.ToolCalls...)
	item := sessions.SessionItem{
		ID:         id,
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceModel,
		Message:    &messageCopy,
	}
	switch message.Role {
	case model.MessageRoleSystem, model.MessageRoleDeveloper:
		item.Kind = sessions.ItemKindRuntimeContext
		item.Visibility = sessions.ItemVisibilityHidden
	case model.MessageRoleUser:
		item.Audience = sessions.ItemAudienceUser
	}
	return item
}

func getRawJSON(t *testing.T, url string) ([]byte, map[string]any) {
	t.Helper()

	return getRawJSONStatus(t, url, "registry-token", http.StatusOK)
}

func getRawJSONStatus(t *testing.T, url, token string, wantStatus int) ([]byte, map[string]any) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest(%s) error = %v", url, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("Get(%s) status = %d, want %d", url, resp.StatusCode, wantStatus)
	}
	return readRawJSON(t, resp)
}

func assertErrorCode(t *testing.T, body map[string]any, want string) {
	t.Helper()

	errorBody, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error body = %#v, want error object", body)
	}
	if got := errorBody["code"]; got != want {
		t.Fatalf("error code = %#v, want %q in %#v", got, want, body)
	}
}

func postRawJSON(t *testing.T, url, body string) ([]byte, map[string]any) {
	t.Helper()

	return postRawJSONWithToken(t, url, body, "")
}

func postRawJSONWithToken(t *testing.T, url, body, token string) ([]byte, map[string]any) {
	t.Helper()

	return postRawJSONStatus(t, url, body, token, http.StatusCreated)
}

func postRawJSONStatus(t *testing.T, url, body, token string, wantStatus int) ([]byte, map[string]any) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest(POST %s) error = %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Post(%s) error = %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("Post(%s) status = %d, want %d", url, resp.StatusCode, wantStatus)
	}
	return readRawJSON(t, resp)
}

func patchRawJSONStatus(t *testing.T, url, body, token string, wantStatus int) ([]byte, map[string]any) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPatch, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest(PATCH %s) error = %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Patch(%s) error = %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("Patch(%s) status = %d, want %d", url, resp.StatusCode, wantStatus)
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

func saveServerTestSession(t *testing.T, store *sessions.V2Store, id string) {
	t.Helper()

	if _, err := store.SaveMetadata(sessions.SessionV2{
		ID:              id,
		Provider:        "codex",
		ModelProfile:    "default",
		ModelID:         "gpt-5",
		ModelParameters: map[string]any{"temperature": 0.2},
		EnabledTools:    []string{"read_file"},
		Context: contextwindow.Metadata{
			ContextWindow: 128000,
		},
		SaveToolResults: true,
	}); err != nil {
		t.Fatalf("SaveMetadata(%s) error = %v", id, err)
	}
}

func appendServerTestItem(t *testing.T, store *sessions.V2Store, sessionID string, item sessions.SessionItem) sessions.SessionItem {
	t.Helper()

	saved, err := store.AppendItem(sessionID, item)
	if err != nil {
		t.Fatalf("AppendItem(%s, %s) error = %v", sessionID, item.ID, err)
	}
	return saved
}

func dialSessionStream(t *testing.T, process *Process, sessionID string) *websocket.Conn {
	return dialSessionStreamWithQuery(t, process, sessionID, "")
}

func dialSessionStreamWithQuery(t *testing.T, process *Process, sessionID, rawQuery string) *websocket.Conn {
	t.Helper()

	header := http.Header{}
	header.Set("Authorization", "Bearer registry-token")
	target := "ws://" + process.Addr() + "/sessions/" + sessionID + "/stream"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	conn, resp, err := websocket.DefaultDialer.Dial(target, header)
	if err != nil {
		if resp != nil && resp.Body != nil {
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			t.Fatalf("Dial(session stream %s) error = %v; status=%d body=%s", sessionID, err, resp.StatusCode, raw)
		}
		t.Fatalf("Dial(session stream %s) error = %v", sessionID, err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})
	return conn
}

func waitForStreamSubscribers(t *testing.T, process *Process, sessionID string, want int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := process.streams.subscriberCount(sessionID); got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subscriberCount(%s) = %d, want %d", sessionID, process.streams.subscriberCount(sessionID), want)
}

func waitForShutdownRejection(t *testing.T, url string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"content":"second"}`))
		if err != nil {
			t.Fatalf("NewRequest(POST %s) error = %v", url, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer registry-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Post(%s) while waiting for shutdown rejection error = %v", url, err)
		}
		body := decodeJSON(t, resp)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable {
			assertErrorCode(t, body, "server_shutting_down")
			return
		}
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("Post(%s) status = %d body=%#v, want eventual 503 or interim 409", url, resp.StatusCode, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to reject new turns during shutdown", url)
}

func waitForServerStopped(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := CheckHealth(context.Background(), addr, 100*time.Millisecond); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server %s stayed healthy after shutdown", addr)
}

func readSessionStreamEvent(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline(stream) error = %v", err)
	}
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage(stream) error = %v", err)
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("stream message type = %d, want text", messageType)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatalf("Unmarshal(stream event) error = %v; payload=%s", err, payload)
	}
	if event["type"] == "" {
		t.Fatalf("stream event missing type: %#v", event)
	}
	return event
}

func responseItems(t *testing.T, body map[string]any) []any {
	t.Helper()

	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items = %T(%#v), want array", body["items"], body["items"])
	}
	return items
}

func responseItemSeqs(t *testing.T, body map[string]any) []int64 {
	t.Helper()

	items := responseItems(t, body)
	seqs := make([]int64, 0, len(items))
	for _, raw := range items {
		item := raw.(map[string]any)
		seqs = append(seqs, int64(item["seq"].(float64)))
	}
	return seqs
}

func responseItemIDs(t *testing.T, body map[string]any) []string {
	t.Helper()

	items := responseItems(t, body)
	ids := make([]string, 0, len(items))
	for _, raw := range items {
		item := raw.(map[string]any)
		ids = append(ids, item["id"].(string))
	}
	return ids
}

func responseSessionItemIDs(items []sessions.SessionItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func assertNoItemDTOLeak(t *testing.T, raw []byte) {
	t.Helper()

	for _, forbidden := range [][]byte{
		[]byte(`"active_history"`),
		[]byte(`"compactions"`),
		[]byte(`"context"`),
		[]byte(`"cwd"`),
		[]byte(`"enabled_tools"`),
		[]byte(`"model_parameters"`),
		[]byte(`"save_tool_results"`),
		[]byte(`"tool_calls"`),
		[]byte(`"tool_call_id"`),
		[]byte(`"is_error"`),
	} {
		if bytes.Contains(raw, forbidden) {
			t.Fatalf("item response leaked %s: %s", forbidden, raw)
		}
	}
}

func assertNoContentDTOLeak(t *testing.T, raw []byte) {
	t.Helper()

	for _, forbidden := range [][]byte{
		[]byte(`"active_history"`),
		[]byte(`"audience"`),
		[]byte(`"compactions"`),
		[]byte(`"context"`),
		[]byte(`"created_at"`),
		[]byte(`"cwd"`),
		[]byte(`"enabled_tools"`),
		[]byte(`"is_error"`),
		[]byte(`"message"`),
		[]byte(`"model_parameters"`),
		[]byte(`"role"`),
		[]byte(`"save_tool_results"`),
		[]byte(`"tool_calls"`),
		[]byte(`"tool_call_id"`),
		[]byte(`"visibility"`),
	} {
		if bytes.Contains(raw, forbidden) {
			t.Fatalf("content response leaked %s: %s", forbidden, raw)
		}
	}
}
