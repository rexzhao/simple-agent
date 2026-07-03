package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestProcessHealthServerAndShutdown(t *testing.T) {
	startedAt := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	process, err := Start(Options{
		CWD:        t.TempDir(),
		ConfigPath: "config.yaml",
		Listen:     "127.0.0.1:0",
		Version:    "test-version",
		AuthToken:  "registry-token",
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
	if health["status"] != "ok" || health["version"] != "test-version" {
		t.Fatalf("health response = %#v, want ok test-version", health)
	}
	if _, ok := health["pid"].(float64); !ok {
		t.Fatalf("health pid = %T(%#v), want number", health["pid"], health["pid"])
	}

	info := getJSON(t, baseURL+"/server")
	for key, want := range map[string]any{
		"config_path":   "config.yaml",
		"addr":          process.Addr(),
		"version":       "test-version",
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
	body := decodeJSON(t, resp)
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
	process, err := Start(Options{Listen: "127.0.0.1:0"})
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

	resp, err := http.Post("http://"+process.Addr()+"/server", "application/json", nil)
	if err != nil {
		t.Fatalf("Post(/server) error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /server status = %d, want 405", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	errObj := body["error"].(map[string]any)
	if errObj["code"] != "method_not_allowed" {
		t.Fatalf("POST /server error = %#v, want method_not_allowed", body)
	}

	resp, err = http.Get("http://" + process.Addr() + "/missing")
	if err != nil {
		t.Fatalf("Get(/missing) error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /missing status = %d, want 404", resp.StatusCode)
	}
	body = decodeJSON(t, resp)
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

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Get(%s) status = %d, want 200", url, resp.StatusCode)
	}
	return decodeJSON(t, resp)
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("Decode(%s) error = %v", resp.Request.URL, err)
	}
	return body
}
