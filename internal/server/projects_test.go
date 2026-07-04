package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	projectstore "github.com/rexzhao/simple-agent/internal/projects"
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
