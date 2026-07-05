package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestProjectAPIsCreateDuplicateListDetailAndClient(t *testing.T) {
	store := projectstore.NewStore(filepath.Join(t.TempDir(), "projects"))
	process := startProjectAPIServer(t, store, "registry-token")
	baseURL := "http://" + process.Addr()
	projectRoot := t.TempDir()
	canonicalRoot, err := projectstore.CanonicalRoot(projectRoot)
	if err != nil {
		t.Fatalf("CanonicalRoot() error = %v", err)
	}

	raw, body := postRawJSONStatus(t, baseURL+"/projects", fmt.Sprintf(`{"root":%q,"display_name":"Secret Path"}`, projectRoot), "", http.StatusForbidden)
	assertErrorCode(t, body, "permission_denied")
	if stringContainsAny(raw, "registry-token", projectRoot) {
		t.Fatalf("permission error leaked token or path: %s", raw)
	}
	_, body = getRawJSONStatus(t, baseURL+"/projects", "", http.StatusForbidden)
	assertErrorCode(t, body, "permission_denied")

	created, err := CreateProjectWithToken(context.Background(), process.Addr(), "registry-token", projectRoot, "Repo", 2*time.Second)
	if err != nil {
		t.Fatalf("CreateProjectWithToken() error = %v", err)
	}
	if !created.Created || created.StatusCode != http.StatusCreated {
		t.Fatalf("created = %#v, want Created true status 201", created)
	}
	if created.Project.ID == "" || created.Project.Root != canonicalRoot || created.Project.DisplayName != "Repo" || created.Project.Archived {
		t.Fatalf("created project = %#v, want canonical active project", created.Project)
	}

	_, duplicate := postRawJSONStatus(t, baseURL+"/projects", fmt.Sprintf(`{"cwd":%q,"display_name":"Different"}`, filepath.Join(projectRoot, ".")), "registry-token", http.StatusOK)
	if duplicate["id"] != created.Project.ID {
		t.Fatalf("duplicate project id = %#v, want %q", duplicate["id"], created.Project.ID)
	}
	if duplicate["display_name"] != "Repo" {
		t.Fatalf("duplicate display_name = %#v, want original name Repo", duplicate["display_name"])
	}

	projects, err := ListProjectsWithToken(context.Background(), process.Addr(), "registry-token", 2*time.Second)
	if err != nil {
		t.Fatalf("ListProjectsWithToken() error = %v", err)
	}
	if len(projects) != 1 || projects[0].ID != created.Project.ID {
		t.Fatalf("ListProjectsWithToken() = %#v, want created project only", projects)
	}

	detail, err := GetProjectWithToken(context.Background(), process.Addr(), "registry-token", created.Project.ID, 2*time.Second)
	if err != nil {
		t.Fatalf("GetProjectWithToken() error = %v", err)
	}
	if detail.ID != created.Project.ID || detail.Root != canonicalRoot {
		t.Fatalf("project detail = %#v, want created project", detail)
	}

	_, body = getRawJSONStatus(t, baseURL+"/projects/"+created.Project.ID, "", http.StatusForbidden)
	assertErrorCode(t, body, "permission_denied")
}

func TestProjectCreateValidationAndUnavailableStore(t *testing.T) {
	process := startProjectAPIServer(t, projectstore.NewStore(filepath.Join(t.TempDir(), "projects")), "registry-token")
	baseURL := "http://" + process.Addr()

	_, body := postRawJSONStatus(t, baseURL+"/projects", `{"display_name":"missing root"}`, "registry-token", http.StatusBadRequest)
	assertErrorCode(t, body, "invalid_request")

	filePath := filepath.Join(t.TempDir(), "file.txt")
	writeServerTestFile(t, filePath, "not a directory")
	_, body = postRawJSONStatus(t, baseURL+"/projects", fmt.Sprintf(`{"root":%q}`, filePath), "registry-token", http.StatusBadRequest)
	assertErrorCode(t, body, "invalid_project_root")

	noStore := startProjectAPIServer(t, nil, "registry-token")
	_, body = getRawJSONStatus(t, "http://"+noStore.Addr()+"/projects", "registry-token", http.StatusServiceUnavailable)
	assertErrorCode(t, body, "project_store_unavailable")
}

func TestProjectArchiveAndRemoveDeletesArchivedProject(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "projects")
	store := projectstore.NewStore(storeRoot)
	archiveRoot := t.TempDir()
	archiveProject, _, err := store.Create(archiveRoot, "Archive")
	if err != nil {
		t.Fatalf("Create(archive) error = %v", err)
	}
	extraProjectData := filepath.Join(storeRoot, archiveProject.ID, "data.bin")
	if err := os.WriteFile(extraProjectData, []byte("project data"), 0o600); err != nil {
		t.Fatalf("WriteFile(extraProjectData) error = %v", err)
	}
	process := startProjectAPIServer(t, store, "registry-token")
	baseURL := "http://" + process.Addr()

	raw, body := deleteRawJSONStatus(t, baseURL+"/projects/"+archiveProject.ID, "", http.StatusForbidden)
	assertErrorCode(t, body, "permission_denied")
	if stringContainsAny(raw, "registry-token", archiveRoot) {
		t.Fatalf("permission error leaked token or path: %s", raw)
	}

	_, body = deleteRawJSONStatus(t, baseURL+"/projects/"+archiveProject.ID, "registry-token", http.StatusConflict)
	assertErrorCode(t, body, "project_active")

	archived, err := ArchiveProjectWithToken(context.Background(), process.Addr(), "registry-token", archiveProject.ID, 2*time.Second)
	if err != nil {
		t.Fatalf("ArchiveProjectWithToken() error = %v", err)
	}
	if archived.ID != archiveProject.ID || !archived.Archived {
		t.Fatalf("archive result = %#v, want archived project metadata", archived)
	}
	listed, err := store.List()
	if err != nil {
		t.Fatalf("List() after archive error = %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("List() after archive = %#v, want no active projects", listed)
	}
	loadedArchive, err := store.Load(archiveProject.ID)
	if err != nil {
		t.Fatalf("Load(archived) error = %v", err)
	}
	if !loadedArchive.Archived {
		t.Fatalf("Load(archived).Archived = false, want true")
	}

	deleted, err := RemoveProjectWithToken(context.Background(), process.Addr(), "registry-token", archiveProject.ID, 2*time.Second)
	if err != nil {
		t.Fatalf("RemoveProjectWithToken() error = %v", err)
	}
	if deleted.Status != "removed" || deleted.ID != archiveProject.ID {
		t.Fatalf("remove result = %#v, want removed status and id", deleted)
	}
	if _, err := store.Load(archiveProject.ID); !errors.Is(err, projectstore.ErrNotFound) {
		t.Fatalf("Load(removed) error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(storeRoot, archiveProject.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed project directory stat error = %v, want not exist", err)
	}
}

func TestProjectRemoveRejectsRunningProjectTurn(t *testing.T) {
	projectStore := projectstore.NewStore(filepath.Join(t.TempDir(), "projects"))
	projectRoot := t.TempDir()
	project, _, err := projectStore.Create(projectRoot, "Busy")
	if err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	sessionStore := sessions.NewV2Store(filepath.Join(t.TempDir(), "sessions"))
	if _, err := sessionStore.SaveMetadata(sessions.SessionV2{
		ID:           "busy-project-session",
		Archived:     true,
		Provider:     "codex",
		ModelProfile: "default",
		ModelID:      "gpt-5",
		ProjectID:    project.ID,
		CWD:          project.Root,
	}); err != nil {
		t.Fatalf("SaveMetadata(busy session) error = %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	runner := fakeSessionTurnRunner{
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			close(started)
			<-release
			return serverTestTurnResult(request.Session,
				model.Message{Role: model.MessageRoleUser, Content: request.Content},
				model.Message{Role: model.MessageRoleAssistant, Content: "done"},
			), nil
		},
	}
	process := startProjectAPIServerWithSessions(t, projectStore, sessionStore, "registry-token", runner)
	baseURL := "http://" + process.Addr()
	firstDone := make(chan map[string]any, 1)
	go func() {
		_, body := postRawJSONStatus(t, baseURL+"/sessions/busy-project-session/messages", `{"content":"SECRET PROJECT PROMPT"}`, "registry-token", http.StatusOK)
		firstDone <- body
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for running turn")
	}

	raw, body := deleteRawJSONStatus(t, baseURL+"/projects/"+project.ID, "registry-token", http.StatusConflict)
	assertErrorCode(t, body, "project_busy")
	for _, forbidden := range [][]byte{
		[]byte("SECRET PROJECT PROMPT"),
		[]byte("registry-token"),
		[]byte("busy-project-session"),
	} {
		if bytes.Contains(raw, forbidden) {
			t.Fatalf("project_busy response leaked %s: %s", forbidden, raw)
		}
	}
	if loaded, err := projectStore.Load(project.ID); err != nil || loaded.Archived {
		t.Fatalf("project after busy remove = %#v err=%v, want active metadata", loaded, err)
	}

	close(release)
	select {
	case body := <-firstDone:
		if body["status"] != "committed" {
			t.Fatalf("first turn response = %#v, want committed", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for running turn to finish")
	}
}

func startProjectAPIServer(t *testing.T, store *projectstore.Store, token string) *Process {
	t.Helper()

	process, err := Start(Options{
		Listen:       "127.0.0.1:0",
		Version:      "test-version",
		AuthToken:    token,
		ProjectStore: store,
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

func startProjectAPIServerWithSessions(t *testing.T, projectStore *projectstore.Store, sessionStore *sessions.V2Store, token string, runner SessionTurnRunner) *Process {
	t.Helper()

	process, err := Start(Options{
		CWD:          t.TempDir(),
		Listen:       "127.0.0.1:0",
		Version:      "test-version",
		AuthToken:    token,
		ProjectStore: projectStore,
		SessionStore: sessionStore,
		TurnRunner:   runner,
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

func writeServerTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func stringContainsAny(raw []byte, values ...string) bool {
	text := string(raw)
	for _, value := range values {
		if value != "" && strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func deleteRawJSONStatus(t *testing.T, url, token string, wantStatus int) ([]byte, map[string]any) {
	t.Helper()

	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("NewRequest(DELETE %s) error = %v", url, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Delete(%s) error = %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("Delete(%s) status = %d, want %d", url, resp.StatusCode, wantStatus)
	}
	return readRawJSON(t, resp)
}
