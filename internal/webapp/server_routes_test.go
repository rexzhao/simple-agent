package webapp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestHTTPRouteCleanBreakReturnsJSON404ForRemovedRESTSurface(t *testing.T) {
	server, _ := newWebTestServer(t)
	removed := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/projects", nil},
		{http.MethodPost, "/api/projects", map[string]string{"root": t.TempDir()}},
		{http.MethodGet, "/api/provider-settings", nil},
		{http.MethodPost, "/api/providers", map[string]string{"name": "removed"}},
		{http.MethodGet, "/api/sessions/session-read", nil},
		{http.MethodGet, "/api/sessions/session-read/snapshot", nil},
		{http.MethodPost, "/api/sessions/session-mutation/compact", nil},
		{http.MethodPost, "/api/sessions/session-mutation/runs", map[string]string{"content": "removed"}},
		{http.MethodGet, "/api/runs/active", nil},
		{http.MethodDelete, "/api/runs/run-mutation", nil},
		{http.MethodPost, "/api/runs/run-mutation/prompts", map[string]string{"content": "removed"}},
	}
	for _, test := range removed {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			response := doJSONRequest(t, test.method, server.URL+test.path, test.body)
			assertJSONNotFound(t, response)
		})
	}

	for _, path := range []string{"/api", "/api/not-a-product-route"} {
		t.Run("exact-or-unknown "+path, func(t *testing.T) {
			response := doJSONRequest(t, http.MethodGet, server.URL+path, nil)
			assertJSONNotFound(t, response)
		})
	}
}

func TestHTTPUnauthorizedAPIPathsRemain401(t *testing.T) {
	server, _ := newWebTestServer(t)
	for _, path := range []string{
		"/api", "/api/bootstrap", "/api/ws-ticket", "/api/projects",
		"/api/provider-settings", "/api/sessions/session-id", "/api/runs/active",
		"/api/blobs/blob-id", "/api/unknown",
	} {
		t.Run(path, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status=%d, want 401", response.StatusCode)
			}
			body, _ := io.ReadAll(response.Body)
			if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "html") || strings.Contains(strings.ToLower(string(body)), "<html") {
				t.Fatalf("unauthorized response was HTML: content-type=%q body=%q", response.Header.Get("Content-Type"), body)
			}
		})
	}
}

func TestHTTPAllowlistAndSPAFallbackRemainAvailable(t *testing.T) {
	server, _ := newWebTestServer(t)

	bootstrap := doJSONRequest(t, http.MethodGet, server.URL+"/api/bootstrap", nil)
	if bootstrap.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap status=%d body=%s", bootstrap.StatusCode, readBody(bootstrap))
	}
	_ = readBody(bootstrap)

	ticket := doJSONRequest(t, http.MethodPost, server.URL+"/api/ws-ticket", nil)
	if ticket.StatusCode != http.StatusOK {
		t.Fatalf("ws-ticket status=%d body=%s", ticket.StatusCode, readBody(ticket))
	}
	_ = readBody(ticket)

	for _, path := range []string{"/", "/app/session/unknown"} {
		t.Run("SPA "+path, func(t *testing.T) {
			response, err := http.Get(server.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status=%d, want 200", response.StatusCode)
			}
			body, _ := io.ReadAll(response.Body)
			if !strings.Contains(string(body), "SAI") {
				t.Fatalf("SPA body does not contain application shell: %q", body)
			}
		})
	}
}

func TestHTTPSessionImageReadBoundaryRemainsAvailable(t *testing.T) {
	server, service := newWebTestServer(t)
	project, err := service.CreateProject(t.TempDir(), "image boundary")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{
		DisplayName: "image session", CreatedCWD: project.Project.Root,
		Provider: "fake", ModelProfile: "fast", ModelID: "fake-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	item, err := service.SessionStore().AppendItem(session.ID, sessions.SessionItem{
		ID: "image-item", Kind: sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible, Audience: sessions.ItemAudienceUser,
		Message: &model.Message{Role: model.MessageRoleUser, ContentBlocks: []model.InputContentBlock{{
			Type: "input_image", ImageURL: model.ImageDataURL("image/png", raw), Detail: "auto",
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hash := item.Message.ContentBlocks[0].ImageBlob.Hash
	response := doJSONRequest(t, http.MethodGet, server.URL+"/api/sessions/"+session.ID+"/images/"+hash, nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "image/png" || !bytes.Equal(body, raw) {
		t.Fatalf("image response status=%d content-type=%q body=%x", response.StatusCode, response.Header.Get("Content-Type"), body)
	}
}

func assertJSONNotFound(t *testing.T, response *http.Response) {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", response.StatusCode)
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "application/json") {
		t.Fatalf("content-type=%q, want JSON", response.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(response.Body)
	if strings.Contains(strings.ToLower(string(body)), "<html") {
		t.Fatalf("404 body is HTML: %q", body)
	}
}

func TestHTTPHostGateHonorsAllowNonLoopback(t *testing.T) {
	newApp := func(t *testing.T, allowNonLoopback bool) *httptest.Server {
		t.Helper()
		home := t.TempDir()
		service, err := execution.NewServiceWithOptions(home, execution.ServiceOptions{TurnRunner: webTestRunner{}})
		if err != nil {
			t.Fatalf("NewServiceWithOptions() error = %v", err)
		}
		writeWebTestConfig(t, home)
		app, err := NewServer(ServerOptions{Context: context.Background(), Service: service, Token: testToken, CWD: home, AllowNonLoopback: allowNonLoopback})
		if err != nil {
			t.Fatalf("NewServer() error = %v", err)
		}
		t.Cleanup(app.Close)
		server := httptest.NewServer(app.Handler())
		t.Cleanup(server.Close)
		return server
	}
	hostGateStatus := func(t *testing.T, server *httptest.Server) int {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, server.URL+"/api/bootstrap", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = "example.internal:8080"
		request.Header.Set("Authorization", "Bearer "+testToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("request error = %v", err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}

	if status := hostGateStatus(t, newApp(t, false)); status != http.StatusForbidden {
		t.Fatalf("default server status for non-loopback Host = %d, want 403", status)
	}
	if status := hostGateStatus(t, newApp(t, true)); status != http.StatusOK {
		t.Fatalf("allow-non-loopback server status for non-loopback Host = %d, want 200", status)
	}
}
