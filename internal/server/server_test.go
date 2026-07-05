package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestProcessHealthServerAndShutdown(t *testing.T) {
	startedAt := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	process, err := Start(Options{
		CWD:       t.TempDir(),
		Listen:    "127.0.0.1:0",
		Version:   "test-version",
		AuthToken: "registry-token",
		Now: func() time.Time {
			return startedAt
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- process.Serve(context.Background())
	}()

	baseURL := "http://" + process.Addr()
	health := getJSON(t, baseURL+"/health")
	if health["status"] != "ok" {
		t.Fatalf("health response = %#v, want ok", health)
	}
	if len(health) != 1 {
		t.Fatalf("health response = %#v, want only status", health)
	}
	for _, forbidden := range []string{"token", "auth", "config_path", "cwd", "addr"} {
		if _, ok := health[forbidden]; ok {
			t.Fatalf("health response leaked %q: %#v", forbidden, health)
		}
	}

	_, body := getJSONStatus(t, baseURL+"/server", "", http.StatusForbidden)
	errObj := body["error"].(map[string]any)
	if errObj["code"] != "permission_denied" {
		t.Fatalf("GET /server without token error = %#v, want permission_denied", body)
	}

	info := getJSONWithToken(t, baseURL+"/server", "registry-token")
	for key, want := range map[string]any{
		"base_url":      process.Addr(),
		"version":       "test-version",
		"project_count": float64(0),
		"session_count": float64(0),
		"running_turns": float64(0),
	} {
		if got := info[key]; got != want {
			t.Fatalf("server response %s = %#v, want %#v in %#v", key, got, want, info)
		}
	}
	if info["started_at"] != startedAt.Format(time.RFC3339Nano) {
		t.Fatalf("started_at = %#v, want %q", info["started_at"], startedAt.Format(time.RFC3339Nano))
	}
	if _, ok := info["uptime_seconds"].(float64); !ok {
		t.Fatalf("uptime_seconds = %T(%#v), want number", info["uptime_seconds"], info["uptime_seconds"])
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+"/server/shutdown", nil)
	if err != nil {
		t.Fatalf("NewRequest(shutdown) error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer registry-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Post(shutdown) error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shutdown status = %d, want 200", resp.StatusCode)
	}
	body = decodeJSON(t, resp)
	if body["status"] != "shutting_down" {
		t.Fatalf("shutdown response = %#v, want shutting_down", body)
	}

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve() did not return after shutdown")
	}
}

func TestProcessStructuredErrors(t *testing.T) {
	process, err := Start(Options{Listen: "127.0.0.1:0", AuthToken: "registry-token"})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	serveDone := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		serveDone <- process.Serve(ctx)
	}()
	defer func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Fatal("Serve() did not stop")
		}
	}()

	_, body := getJSONStatus(t, "http://"+process.Addr()+"/server", "", http.StatusForbidden)
	errObj := body["error"].(map[string]any)
	if errObj["code"] != "permission_denied" {
		t.Fatalf("GET /server without token error = %#v, want permission_denied", body)
	}
	_, body = getJSONStatus(t, "http://"+process.Addr()+"/server/anything", "", http.StatusForbidden)
	errObj = body["error"].(map[string]any)
	if errObj["code"] != "permission_denied" {
		t.Fatalf("GET /server/anything without token error = %#v, want permission_denied", body)
	}
	_, body = getJSONStatus(t, "http://"+process.Addr()+"/missing", "", http.StatusForbidden)
	errObj = body["error"].(map[string]any)
	if errObj["code"] != "permission_denied" {
		t.Fatalf("GET /missing without token error = %#v, want permission_denied", body)
	}

	req, err := http.NewRequest(http.MethodPost, "http://"+process.Addr()+"/server", nil)
	if err != nil {
		t.Fatalf("NewRequest(POST /server) error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer registry-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Post(/server) error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /server status = %d, want 405", resp.StatusCode)
	}
	body = decodeJSON(t, resp)
	errObj = body["error"].(map[string]any)
	if errObj["code"] != "method_not_allowed" {
		t.Fatalf("POST /server error = %#v, want method_not_allowed", body)
	}

	_, body = getJSONStatus(t, "http://"+process.Addr()+"/missing", "registry-token", http.StatusNotFound)
	errObj = body["error"].(map[string]any)
	if errObj["code"] != "not_found" {
		t.Fatalf("GET /missing error = %#v, want not_found", body)
	}
}

func TestValidateListenAddressRejectsNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "192.168.1.10:8080", ":8080"} {
		t.Run(addr, func(t *testing.T) {
			err := ValidateListenAddress(addr)
			if err == nil {
				t.Fatal("ValidateListenAddress() error = nil, want error")
			}
			if !strings.Contains(err.Error(), "loopback") {
				t.Fatalf("ValidateListenAddress() error = %q, want loopback", err)
			}
		})
	}
}

func TestValidateListenAddressAcceptsLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "localhost:8080", "[::1]:0"} {
		t.Run(addr, func(t *testing.T) {
			if err := ValidateListenAddress(addr); err != nil {
				t.Fatalf("ValidateListenAddress() error = %v", err)
			}
		})
	}
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()

	_, body := getJSONStatus(t, url, "", http.StatusOK)
	return body
}

func getJSONWithToken(t *testing.T, url, token string) map[string]any {
	t.Helper()

	_, body := getJSONStatus(t, url, token, http.StatusOK)
	return body
}

func getJSONStatus(t *testing.T, url, token string, wantStatus int) ([]byte, map[string]any) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest(%s) error = %v", url, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("Get(%s) status = %d, want %d", url, resp.StatusCode, wantStatus)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll(%s) error = %v", url, err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v; body=%s", url, err, raw)
	}
	return raw, body
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("Decode(%s) error = %v", resp.Request.URL, err)
	}
	return body
}
