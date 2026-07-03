package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/subagents"
)

func TestModelsListUsesGlobalConfigPathFlag(t *testing.T) {
	dir := writeCLIFixture(t)
	writeCLIFile(t, filepath.Join(dir, "providers", "responses.yaml"), `name: openai
base_url: https://api.openai.com/v1
api_key: $OPENAI_API_KEY

models:
  default:
    id: gpt-5.1
    type: openai-responses
`)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "models", "list"}, &stdout, &stderr, func() (string, error) {
		return "unused", nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"PROVIDER\tPROFILE\tMODEL ID",
		"openai\tdefault\tgpt-5.1",
		"paperhub\tglm-5.2\tglm-5.2",
		"paperhub\tglm-5.2-fast\tglm-5.2",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("models list output missing %q:\n%s", want, out)
		}
	}
}

func TestModelsListAcceptsConfigPathAfterCommand(t *testing.T) {
	dir := writeCLIFixture(t)

	for _, args := range [][]string{
		{"models", "list", "--config", cliConfigPath(dir)},
		{"models", "--config", cliConfigPath(dir), "list"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})

			if code != 0 {
				t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "paperhub\tglm-5.2\tglm-5.2") {
				t.Fatalf("models list output = %s", stdout.String())
			}
		})
	}
}

func TestModelsListMixedHelpDoesNotLoadConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", filepath.Join(t.TempDir(), "missing"), "models", "-h", "list"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !strings.Contains(stdout.String(), "usage: sai models list") {
		t.Fatalf("stdout = %q, want models list usage", stdout.String())
	}
}

func TestModelsListDefaultsConfigPathToAgentsUnderCurrentWorkingDirectory(t *testing.T) {
	projectDir := t.TempDir()
	writeCLIFixtureInDir(t, filepath.Join(projectDir, ".agents"))

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"models", "list"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "paperhub\tglm-5.2\tglm-5.2") {
		t.Fatalf("models list output = %s", stdout.String())
	}
}

func TestModelsListDefaultsConfigPathFromProgramBasename(t *testing.T) {
	projectDir := t.TempDir()
	agentsDir := filepath.Join(projectDir, ".agents")
	writeCLIFixtureInDir(t, agentsDir)
	if err := os.Rename(filepath.Join(agentsDir, "sai.yaml"), filepath.Join(agentsDir, "team-agent.yaml")); err != nil {
		t.Fatalf("Rename(config) error = %v", err)
	}

	for _, program := range []string{
		filepath.Join("bin", "team-agent"),
		filepath.Join("bin", "team-agent.exe"),
	} {
		t.Run(program, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithProgramGetwd(program, []string{"models", "list"}, &stdout, &stderr, func() (string, error) {
				return projectDir, nil
			})

			if code != 0 {
				t.Fatalf("RunWithProgramGetwd() code = %d, stderr = %s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "paperhub\tglm-5.2\tglm-5.2") {
				t.Fatalf("models list output = %s", stdout.String())
			}
		})
	}
}

func TestMCPListShowsConfiguredServersAndEnabledState(t *testing.T) {
	dir := writeCLIFixture(t)
	writeCLIMCPFixture(t, dir)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "mcp", "list"}, &stdout, &stderr, func() (string, error) {
		return "unused", nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"ID\tENABLED",
		"local\ttrue",
		"remote\tfalse",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("mcp list output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "COMMAND") || strings.Contains(out, "example-mcp-server") || strings.Contains(out, "remote-mcp-server") {
		t.Fatalf("mcp list output included command details:\n%s", out)
	}
}

func TestToolsListWritesBuiltInToolsWithoutConfig(t *testing.T) {
	for _, args := range [][]string{
		{"tools", "list"},
		{"tools", "list", "-h"},
		{"tools", "list", "extra", "-h", "--bad"},
		{"help", "tools", "list"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})
			if code != 0 {
				t.Fatalf("RunWithGetwd(%v) code = %d, stderr = %s", args, code, stderr.String())
			}
			if stderr.String() != "" {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			out := stdout.String()
			if containsHelpArg(args) || args[0] == "help" {
				if !strings.Contains(out, "usage: sai tools list") {
					t.Fatalf("stdout = %q, want tools list usage", out)
				}
				return
			}
			if got, want := out, "list_files\nread_file\nglob_files\ngrep_files\nwrite_file\nedit_file\nshell\n"; got != want {
				t.Fatalf("stdout = %q, want %q", got, want)
			}
		})
	}
}

func TestAuthCodexLoginHelpDoesNotLoadConfig(t *testing.T) {
	for _, args := range [][]string{
		{"auth", "codex", "login", "-h"},
		{"help", "auth", "codex", "login"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})
			if code != 0 {
				t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
			}
			if stderr.String() != "" {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			if !strings.Contains(stdout.String(), "usage: sai auth codex login") {
				t.Fatalf("stdout = %q, want auth codex login usage", stdout.String())
			}
		})
	}
}

func TestAuthCodexLoginGeneratesNamedProviderAndAuthFile(t *testing.T) {
	configDir := t.TempDir()
	writeCLIFile(t, filepath.Join(configDir, "sai.yaml"), `default_provider: codex-work
default_model: gpt-5.5
provider_dir: providers
auth_dir: auth
`)
	tokenRequests := make(chan url.Values, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("usercode Content-Type = %q, want application/json", got)
			}
			bodyData, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(usercode body) error = %v", err)
			}
			body := decodeCLIJSON(t, bodyData)
			if body["client_id"] == "" {
				t.Fatalf("usercode body = %#v, want client_id", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_auth_id":"device-auth-123","user_code":"USER-123","verification_uri":"https://example.test/device","interval":"1","expires_in":"600"}`)
		case "/api/accounts/deviceauth/token":
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("device token Content-Type = %q, want application/json", got)
			}
			bodyData, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(device token body) error = %v", err)
			}
			body := decodeCLIJSON(t, bodyData)
			if body["device_auth_id"] != "device-auth-123" || body["user_code"] != "USER-123" {
				t.Fatalf("device token body = %#v, want device_auth_id and user_code", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"authorization_code":"auth-code-123","code_verifier":"verifier-123"}`)
		case "/oauth/token":
			if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
				t.Fatalf("token Content-Type = %q, want application/x-www-form-urlencoded", got)
			}
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm() error = %v", err)
			}
			tokenRequests <- r.PostForm
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"codex-access","refresh_token":"codex-refresh","expires_in":3600,"account_id":"account-123","token_type":"Bearer"}`)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{
		"--config", cliConfigPath(configDir),
		"auth", "codex", "login",
		"--provider", "codex-work",
		"--issuer-url", server.URL,
		"--base-url", "https://codex.example.test/backend",
		"--poll-interval", "1ms",
	}, &stdout, &stderr, func() (string, error) {
		return "unused", nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	for _, want := range []string{"Open https://example.test/device and enter code USER-123", "Saved provider \"codex-work\"", "Saved Codex auth token"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}

	providerData, err := os.ReadFile(filepath.Join(configDir, "providers", "codex-work.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(provider) error = %v", err)
	}
	providerText := string(providerData)
	for _, want := range []string{
		"name: codex-work",
		"base_url: https://codex.example.test/backend",
		"auth_file: ../auth/codex-work.json",
		"type: openai-codex",
		"context_window: 400000",
		"parameters:\n      store: false\n      reasoning:\n        effort: high",
	} {
		if !strings.Contains(providerText, want) {
			t.Fatalf("provider file missing %q:\n%s", want, providerText)
		}
	}

	authData, err := os.ReadFile(filepath.Join(configDir, "auth", "codex-work.json"))
	if err != nil {
		t.Fatalf("ReadFile(auth) error = %v", err)
	}
	var token map[string]any
	if err := json.Unmarshal(authData, &token); err != nil {
		t.Fatalf("Unmarshal(auth) error = %v", err)
	}
	if token["access_token"] != "codex-access" || token["refresh_token"] != "codex-refresh" || token["account_id"] != "account-123" {
		t.Fatalf("auth token = %#v, want generated token values", token)
	}
	if token["token_url"] != server.URL+"/oauth/token" {
		t.Fatalf("token_url = %#v, want fake token URL", token["token_url"])
	}
	form := <-tokenRequests
	if form.Get("grant_type") != "authorization_code" ||
		form.Get("code") != "auth-code-123" ||
		form.Get("code_verifier") != "verifier-123" ||
		form.Get("redirect_uri") != server.URL+"/deviceauth/callback" {
		t.Fatalf("token request form = %#v", form)
	}
}

func TestAuthCodexLoginInterruptCancelsPollingWithoutWritingFiles(t *testing.T) {
	configDir := t.TempDir()
	writeCLIFile(t, filepath.Join(configDir, "sai.yaml"), "default_provider: codex-work\ndefault_model: gpt-5.5\nprovider_dir: providers\nauth_dir: auth\n")
	deviceTokenRequested := make(chan struct{})
	var tokenOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_auth_id":"device-auth-123","user_code":"USER-123","verification_uri":"https://example.test/device","interval":"1","expires_in":"600"}`)
		case "/api/accounts/deviceauth/token":
			tokenOnce.Do(func() { close(deviceTokenRequested) })
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
		case "/oauth/token":
			t.Fatalf("unexpected token exchange after interrupt")
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	interrupts := make(chan struct{}, 1)
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runWithProgramContextAndInterrupts(context.Background(), "sai", []string{
			"--config", cliConfigPath(configDir),
			"auth", "codex", "login",
			"--provider", "codex-work",
			"--issuer-url", server.URL,
			"--base-url", "https://codex.example.test/backend",
			"--poll-interval", "1ms",
		}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
			return "unused", nil
		}, interrupts)
	}()

	select {
	case <-deviceTokenRequested:
	case code := <-done:
		t.Fatalf("auth login returned before polling: code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for device token request; stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	interrupts <- struct{}{}
	code := waitForCode(t, done)
	if code != 1 {
		t.Fatalf("auth login code = %d, want 1", code)
	}
	assertCLIErrorContains(t, stderr.String(), "context canceled")
	if _, err := os.Stat(filepath.Join(configDir, "providers", "codex-work.yaml")); !os.IsNotExist(err) {
		t.Fatalf("provider file stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(configDir, "auth", "codex-work.json")); !os.IsNotExist(err) {
		t.Fatalf("auth file stat error = %v, want not exist", err)
	}
	if !strings.Contains(stdout.String(), "Open https://example.test/device and enter code USER-123") {
		t.Fatalf("stdout = %q, want device flow instruction before cancel", stdout.String())
	}
	if strings.Contains(stdout.String(), "Saved provider") || strings.Contains(stdout.String(), "Saved Codex auth token") {
		t.Fatalf("stdout = %q, want no saved-file messages", stdout.String())
	}
}

func TestToolsListUnknownFlagAfterExtraArgIncludesHelpHint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"tools", "list", "extra", "--bad"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "flag provided but not defined", `Run "sai help tools list" for usage.`)
}

func TestToolsListDelimiterTreatsDashArgAsExtraPositional(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"tools", "list", "--", "--bad"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "usage: sai tools list", `Run "sai help tools list" for usage.`)
	if strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("stderr treated delimiter positional as flag: %s", stderr.String())
	}
}

func TestMCPListEnableMCPOverridesConfiguredEnabledState(t *testing.T) {
	dir := writeCLIFixture(t)
	writeCLIMCPFixture(t, dir)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "mcp", "list", "--enable-mcp", "remote"}, &stdout, &stderr, func() (string, error) {
		return "unused", nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"local\tfalse",
		"remote\ttrue",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("mcp list override output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "COMMAND") || strings.Contains(out, "example-mcp-server") || strings.Contains(out, "remote-mcp-server") {
		t.Fatalf("mcp list override output included command details:\n%s", out)
	}
}

func TestMCPListAcceptsConfigPathAfterCommandAndEnableMCP(t *testing.T) {
	dir := writeCLIFixture(t)
	writeCLIMCPFixture(t, dir)

	for _, args := range [][]string{
		{"mcp", "list", "--enable-mcp", "remote", "--config", cliConfigPath(dir)},
		{"mcp", "--enable-mcp", "remote", "--config", cliConfigPath(dir), "list"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})

			if code != 0 {
				t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
			}
			out := stdout.String()
			for _, want := range []string{
				"local\tfalse",
				"remote\ttrue",
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("mcp list output missing %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestMCPListMixedFlagWithExtraArgIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"mcp", "list", "remote", "--enable-mcp", "local"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "usage: sai mcp list", `Run "sai help mcp list" for usage.`)
}

func TestMCPListRejectsUnknownEnableMCPServer(t *testing.T) {
	dir := writeCLIFixture(t)
	writeCLIMCPFixture(t, dir)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "mcp", "list", "--enable-mcp", "missing"}, &stdout, &stderr, func() (string, error) {
		return "unused", nil
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `unknown MCP server "missing"`, "available MCP servers: local, remote")
}

func TestConfigShowDoesNotPrintAPIKeyValue(t *testing.T) {
	dir := writeCLIFixture(t)
	t.Setenv("PAPERHUB_API_KEY", "env-secret-value")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "config", "show"}, &stdout, &stderr, func() (string, error) {
		return "unused", nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, leaked := range []string{"env-secret-value", "direct-secret-value"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("config show leaked API key value %q:\n%s", leaked, out)
		}
	}
	if !strings.Contains(out, "PAPERHUB_API_KEY") {
		t.Fatalf("config show should include API key env var name:\n%s", out)
	}
	if !strings.Contains(out, "<redacted>") {
		t.Fatalf("config show should include redacted direct API key:\n%s", out)
	}
	if logPaths := sessionLogPaths(t, dir); len(logPaths) != 0 {
		t.Fatalf("config show created session log paths: %#v", logPaths)
	}
}

func TestConfigShowAcceptsConfigPathAfterCommand(t *testing.T) {
	dir := writeCLIFixture(t)

	for _, args := range [][]string{
		{"config", "show", "--config", cliConfigPath(dir)},
		{"config", "--config", cliConfigPath(dir), "show"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})

			if code != 0 {
				t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), `"ConfigPath":`) && !strings.Contains(stdout.String(), `"config_path":`) {
				t.Fatalf("config show output = %s", stdout.String())
			}
		})
	}
}

func TestConfigShowMixedHelpDoesNotLoadConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"config", "show", "--config", filepath.Join(t.TempDir(), "missing"), "-h"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !strings.Contains(stdout.String(), "usage: sai config show") {
		t.Fatalf("stdout = %q, want config show usage", stdout.String())
	}
}

func TestDoctorHelpDoesNotLoadConfig(t *testing.T) {
	for _, args := range [][]string{
		{"doctor", "-h"},
		{"doctor", "--help"},
		{"help", "doctor"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			assertCLIHelpWithoutConfig(t, args, "usage: sai doctor", "provider HTTP requests", "starting MCP servers", "running a model", "printing secrets")
		})
	}
}

func TestDoctorAcceptsConfigPathBeforeAndAfterCommand(t *testing.T) {
	dir := t.TempDir()
	writeCLIRunFixtureInDir(t, dir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")

	for _, args := range [][]string{
		{"--config", cliConfigPath(dir), "doctor"},
		{"doctor", "--config", cliConfigPath(dir)},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})
			if code != 0 {
				t.Fatalf("RunWithGetwd() code = %d, stderr = %s, stdout = %s", code, stderr.String(), stdout.String())
			}
			if stderr.String() != "" {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			out := stdout.String()
			for _, want := range []string{"OK config_path", "OK config_file", "OK provider_files", "OK default_model", "OK api_key"} {
				if !strings.Contains(out, want) {
					t.Fatalf("doctor output = %s, want contain %q", out, want)
				}
			}
			assertCLIErrorOmits(t, out, "direct-secret-value")
		})
	}
}

func TestDoctorSuccessChecksEnabledLocalConfig(t *testing.T) {
	configDir := t.TempDir()
	projectDir := t.TempDir()
	writeCLIRunFixtureInDirWithTools(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat", []string{"list_files", "mcp.local.search"})
	writeCLISkill(t, configDir, "alpha", "skill instructions")
	if err := os.MkdirAll(filepath.Join(configDir, "mcp"), 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp) error = %v", err)
	}
	writeCLIFile(t, filepath.Join(configDir, "mcp", "local.yaml"), `id: local
enabled: true
command: fake-mcp-server
args: []
env: {}
`)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "doctor"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s, stdout = %s", code, stderr.String(), stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"OK config_path",
		"OK config_file",
		"OK provider_files",
		"OK default_model fake/default -> model-default",
		"OK api_key",
		"OK mcp_dir 1 servers loaded",
		"OK enabled_mcp local",
		"OK skill_dirs 1 configured, 1 discovered, 1 loaded",
		"OK loaded_skills alpha",
		"OK enabled_tools list_files,mcp.local.search",
		"OK logging",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output = %s, want contain %q", out, want)
		}
	}
	assertCLIErrorOmits(t, out, "direct-secret-value", "fake-mcp-server")
}

func TestDoctorReportsConfigProviderModelAndAPIKeyErrors(t *testing.T) {
	t.Run("missing config", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing")
		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", missing, "doctor"}, &stdout, &stderr, func() (string, error) {
			return "", errors.New("getwd should not be called")
		})
		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stderr.String() != "" {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		assertCLIOutputContains(t, stdout.String(), "OK config_path", "ERROR config_file")
	})

	t.Run("missing providers", func(t *testing.T) {
		dir := t.TempDir()
		writeCLIRunFixtureInDir(t, dir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
		if err := os.RemoveAll(filepath.Join(dir, "providers")); err != nil {
			t.Fatalf("RemoveAll(providers) error = %v", err)
		}
		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "doctor"}, &stdout, &stderr, func() (string, error) {
			return "", errors.New("getwd should not be called")
		})
		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stderr.String() != "" {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		assertCLIOutputContains(t, stdout.String(), "ERROR provider_files")
	})

	t.Run("invalid default model", func(t *testing.T) {
		dir := t.TempDir()
		writeCLIRunFixtureInDir(t, dir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
		replaceCLIFileText(t, filepath.Join(dir, "sai.yaml"), "default_model: default", "default_model: missing")
		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "doctor"}, &stdout, &stderr, func() (string, error) {
			return "", errors.New("getwd should not be called")
		})
		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stderr.String() != "" {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		assertCLIOutputContains(t, stdout.String(), "ERROR default_model", `unknown model "missing"`)
		assertCLIErrorOmits(t, stdout.String(), "direct-secret-value")
	})

	t.Run("missing api key env", func(t *testing.T) {
		dir := t.TempDir()
		writeCLIRunFixtureInDir(t, dir, "http://127.0.0.1:1", "$SAI_DOCTOR_MISSING_API_KEY", "openai-chat")
		unsetEnvForCLITest(t, "SAI_DOCTOR_MISSING_API_KEY")
		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "doctor"}, &stdout, &stderr, func() (string, error) {
			return "", errors.New("getwd should not be called")
		})
		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stderr.String() != "" {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		assertCLIOutputContains(t, stdout.String(), "ERROR api_key", `API key environment variable "SAI_DOCTOR_MISSING_API_KEY" is not set`)
	})
}

func TestDoctorReportsEnabledToolMCPAndSkillErrors(t *testing.T) {
	t.Run("unknown built-in tool", func(t *testing.T) {
		dir := t.TempDir()
		writeCLIRunFixtureInDirWithTools(t, dir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat", []string{"missing"})
		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "doctor"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})
		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stderr.String() != "" {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		assertCLIOutputContains(t, stdout.String(), "ERROR enabled_tools", `enabled tool "missing" is not registered`)
	})

	t.Run("explicit subagent tool", func(t *testing.T) {
		dir := t.TempDir()
		writeCLIRunFixtureInDirWithTools(t, dir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat", []string{subagents.ToolSubagentStart})
		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "doctor"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})
		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stderr.String() != "" {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		assertCLIOutputContains(t, stdout.String(), "ERROR enabled_tools", `enabled tool "subagent_start" is a subagent tool`, "auto-enabled", "subagents")
	})

	t.Run("malformed skill", func(t *testing.T) {
		dir := t.TempDir()
		writeCLIRunFixtureInDir(t, dir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
		writeCLISkill(t, dir, "bad", "---\nname: [bad\n---\nBad instructions\n")
		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "doctor"}, &stdout, &stderr, func() (string, error) {
			return "", errors.New("getwd should not be called")
		})
		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stderr.String() != "" {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		assertCLIOutputContains(t, stdout.String(), "ERROR skill_dirs", "parse skill frontmatter", "bad", "SKILL.md")
	})

	t.Run("duplicate skill id", func(t *testing.T) {
		dir := t.TempDir()
		writeCLIRunFixtureInDir(t, dir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
		setCLISkillDirs(t, dir, []string{"skills", "team-skills"})
		writeCLISkill(t, dir, "shared", "First instructions\n")
		writeCLISkillInRoot(t, filepath.Join(dir, "team-skills"), "shared", "Second instructions\n")
		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "doctor"}, &stdout, &stderr, func() (string, error) {
			return "", errors.New("getwd should not be called")
		})
		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stderr.String() != "" {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		assertCLIOutputContains(t, stdout.String(), "ERROR skill_dirs", `duplicate skill id "shared"`)
	})

	t.Run("MCP tool server disabled", func(t *testing.T) {
		dir := t.TempDir()
		writeCLIRunFixtureInDirWithTools(t, dir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat", []string{"mcp.local.search"})
		if err := os.MkdirAll(filepath.Join(dir, "mcp"), 0o755); err != nil {
			t.Fatalf("MkdirAll(mcp) error = %v", err)
		}
		writeCLIFile(t, filepath.Join(dir, "mcp", "local.yaml"), `id: local
enabled: false
command: fake-mcp-server
args: []
env: {}
`)
		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "doctor"}, &stdout, &stderr, func() (string, error) {
			return "", errors.New("getwd should not be called")
		})
		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stderr.String() != "" {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		assertCLIOutputContains(t, stdout.String(), "OK enabled_mcp (none)", "ERROR enabled_tools", `references MCP server "local", but it is not enabled`)
	})

	t.Run("invalid MCP config", func(t *testing.T) {
		dir := t.TempDir()
		writeCLIRunFixtureInDir(t, dir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
		if err := os.MkdirAll(filepath.Join(dir, "mcp"), 0o755); err != nil {
			t.Fatalf("MkdirAll(mcp) error = %v", err)
		}
		writeCLIFile(t, filepath.Join(dir, "mcp", "local.yaml"), `id: local
enabled: true
`)
		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "doctor"}, &stdout, &stderr, func() (string, error) {
			return "", errors.New("getwd should not be called")
		})
		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stderr.String() != "" {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		assertCLIOutputContains(t, stdout.String(), "ERROR mcp_dir", `server "local" is missing command`)
	})
}

func TestDoctorLoggingDisabledAndProbeDoesNotCreateSessionLog(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		dir := t.TempDir()
		writeCLIRunFixtureInDir(t, dir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
		setCLILoggingPath(t, dir, `""`)
		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "doctor"}, &stdout, &stderr, func() (string, error) {
			return "", errors.New("getwd should not be called")
		})
		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s, stdout = %s", code, stderr.String(), stdout.String())
		}
		assertCLIOutputContains(t, stdout.String(), "OK logging disabled")
	})

	t.Run("probe only", func(t *testing.T) {
		dir := t.TempDir()
		writeCLIRunFixtureInDir(t, dir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "doctor"}, &stdout, &stderr, func() (string, error) {
			return "", errors.New("getwd should not be called")
		})
		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s, stdout = %s", code, stderr.String(), stdout.String())
		}
		assertCLIOutputContains(t, stdout.String(), "OK logging")
		if logPaths := sessionLogPaths(t, dir); len(logPaths) != 0 {
			t.Fatalf("doctor created session log paths: %#v", logPaths)
		}
		if _, err := os.Stat(filepath.Join(dir, "logs")); err == nil {
			t.Fatalf("doctor left log root behind")
		} else if !os.IsNotExist(err) {
			t.Fatalf("Stat(logs) error = %v", err)
		}
	})
}

func TestDoctorOutputDoesNotLeakSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAI_DOCTOR_API_KEY", "doctor-env-secret")
	t.Setenv("SAI_DOCTOR_MCP_TOKEN", "doctor-mcp-env-secret")
	writeCLIRunFixtureInDirWithTools(t, dir, "http://127.0.0.1:1", "$SAI_DOCTOR_API_KEY", "openai-chat", []string{"mcp.local.search"})
	if err := os.MkdirAll(filepath.Join(dir, "mcp"), 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp) error = %v", err)
	}
	writeCLIFile(t, filepath.Join(dir, "mcp", "local.yaml"), `id: local
enabled: true
command: fake-mcp-server
args:
  - "--token"
  - "doctor-mcp-arg-secret"
env:
  DIRECT_SECRET: doctor-mcp-direct-secret
  TOKEN: $SAI_DOCTOR_MCP_TOKEN
`)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(dir), "doctor"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s, stdout = %s", code, stderr.String(), stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	assertCLIErrorOmits(t, stdout.String(),
		"doctor-env-secret",
		"doctor-mcp-env-secret",
		"doctor-mcp-direct-secret",
		"doctor-mcp-arg-secret",
		"Authorization",
		"Bearer ",
	)
}

func TestVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"version"}, &stdout, &stderr, func() (string, error) {
		return "unused", nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "sai dev\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestRootHelpWritesUsageWithoutConfig(t *testing.T) {
	for _, args := range [][]string{
		{"-h"},
		{"--help"},
		{"--config", filepath.Join(t.TempDir(), "missing"), "--help"},
		{"help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})
			if code != 0 {
				t.Fatalf("RunWithGetwd(%v) code = %d, stderr = %s", args, code, stderr.String())
			}
			out := stdout.String()
			for _, want := range []string{"usage: sai", "chat              Start a chat session", "config show", "models list", "doctor", "tools list", "sessions", "With no command, sai defaults to chat.", `Run "sai help <command>" for command usage.`} {
				if !strings.Contains(out, want) {
					t.Fatalf("stdout = %q, want contain %q", out, want)
				}
			}
			if strings.Contains(out, `run "prompt"`) || strings.Contains(out, "sai run") {
				t.Fatalf("root help still lists run:\n%s", out)
			}
			if stderr.String() != "" {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestRunHelpIsUnsupportedWithoutConfig(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"run", "-h"}, want: `unknown command "run"`},
		{args: []string{"help", "run"}, want: `unknown help topic "run"`},
	}

	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(tt.args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})
			if code != 1 {
				t.Fatalf("RunWithGetwd(%v) code = %d, want 1", tt.args, code)
			}
			if stdout.String() != "" {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			assertCLIErrorContains(t, stderr.String(), tt.want, `Run "sai help" for usage.`)
			if strings.Contains(stderr.String(), "usage: sai run") {
				t.Fatalf("stderr included run usage: %s", stderr.String())
			}
		})
	}
}

func TestChatHelpWritesUsageWithoutConfig(t *testing.T) {
	for _, args := range [][]string{
		{"chat", "-h"},
		{"chat", "--help"},
		{"help", "chat"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			assertCLIHelpWithoutConfig(t, args, "usage: sai chat", "--prompt text | --stdin | --file path", "--save-session", "--resume id | --continue", "full sensitive content")
		})
	}

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"help", "chat"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})
	if code != 0 {
		t.Fatalf("RunWithGetwd(help chat) code = %d, stderr = %s", code, stderr.String())
	}
	for _, removed := range []string{"--enable-skills", "--disable-skills"} {
		if strings.Contains(stdout.String(), removed) {
			t.Fatalf("chat help still includes removed flag %q:\n%s", removed, stdout.String())
		}
	}
}

func TestVersionHelpWritesUsageWithoutConfig(t *testing.T) {
	for _, args := range [][]string{
		{"version", "-h"},
		{"version", "--help"},
		{"help", "version"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			assertCLIHelpWithoutConfig(t, args, "usage: sai version", "Prints the sai version.")
		})
	}
}

func TestGroupHelpWritesUsageWithoutConfig(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		wants []string
	}{
		{
			name:  "config flag short",
			args:  []string{"config", "-h"},
			wants: []string{"usage: sai config <command>", "config show"},
		},
		{
			name:  "config flag long",
			args:  []string{"config", "--help"},
			wants: []string{"usage: sai config <command>", "config show"},
		},
		{
			name:  "config help",
			args:  []string{"help", "config"},
			wants: []string{"usage: sai config <command>", "config show"},
		},
		{
			name:  "models flag short",
			args:  []string{"models", "-h"},
			wants: []string{"usage: sai models <command>", "models list"},
		},
		{
			name:  "models flag long",
			args:  []string{"models", "--help"},
			wants: []string{"usage: sai models <command>", "models list"},
		},
		{
			name:  "models help",
			args:  []string{"help", "models"},
			wants: []string{"usage: sai models <command>", "models list"},
		},
		{
			name:  "tools flag short",
			args:  []string{"tools", "-h"},
			wants: []string{"usage: sai tools <command>", "tools list"},
		},
		{
			name:  "tools flag long",
			args:  []string{"tools", "--help"},
			wants: []string{"usage: sai tools <command>", "tools list"},
		},
		{
			name:  "tools help",
			args:  []string{"help", "tools"},
			wants: []string{"usage: sai tools <command>", "tools list"},
		},
		{
			name:  "mcp flag short",
			args:  []string{"mcp", "-h"},
			wants: []string{"usage: sai mcp <command>", "mcp list"},
		},
		{
			name:  "mcp flag long",
			args:  []string{"mcp", "--help"},
			wants: []string{"usage: sai mcp <command>", "mcp list"},
		},
		{
			name:  "mcp help",
			args:  []string{"help", "mcp"},
			wants: []string{"usage: sai mcp <command>", "mcp list"},
		},
		{
			name:  "sessions flag short",
			args:  []string{"sessions", "-h"},
			wants: []string{"usage: sai sessions <command>", "sessions list", "sessions prune --keep N"},
		},
		{
			name:  "sessions flag long",
			args:  []string{"sessions", "--help"},
			wants: []string{"usage: sai sessions <command>", "sessions list", "sessions prune --keep N"},
		},
		{
			name:  "sessions help",
			args:  []string{"help", "sessions"},
			wants: []string{"usage: sai sessions <command>", "sessions list", "sessions prune --keep N"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertCLIHelpWithoutConfig(t, tt.args, tt.wants...)
		})
	}
}

func TestNestedHelpWritesUsageWithoutConfig(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		wants []string
	}{
		{
			name:  "config show flag",
			args:  []string{"config", "show", "-h"},
			wants: []string{"usage: sai config show", "sensitive values redacted"},
		},
		{
			name:  "config show help",
			args:  []string{"help", "config", "show"},
			wants: []string{"usage: sai config show", "sensitive values redacted"},
		},
		{
			name:  "models list flag",
			args:  []string{"models", "list", "-h"},
			wants: []string{"usage: sai models list", "provider model profiles"},
		},
		{
			name:  "models list help",
			args:  []string{"help", "models", "list"},
			wants: []string{"usage: sai models list", "provider model profiles"},
		},
		{
			name:  "tools list flag",
			args:  []string{"tools", "list", "-h"},
			wants: []string{"usage: sai tools list", "built-in tools"},
		},
		{
			name:  "tools list help",
			args:  []string{"help", "tools", "list"},
			wants: []string{"usage: sai tools list", "built-in tools"},
		},
		{
			name:  "mcp list flag",
			args:  []string{"mcp", "list", "-h"},
			wants: []string{"usage: sai mcp list", "--enable-mcp ids"},
		},
		{
			name:  "mcp list help",
			args:  []string{"help", "mcp", "list"},
			wants: []string{"usage: sai mcp list", "--enable-mcp ids"},
		},
		{
			name:  "sessions list flag",
			args:  []string{"sessions", "list", "-h"},
			wants: []string{"usage: sai sessions list", "messages, prompts, assistant output"},
		},
		{
			name:  "sessions list help",
			args:  []string{"help", "sessions", "list"},
			wants: []string{"usage: sai sessions list", "messages, prompts, assistant output"},
		},
		{
			name:  "sessions show flag",
			args:  []string{"sessions", "show", "-h"},
			wants: []string{"usage: sai sessions show <id>", "full sensitive"},
		},
		{
			name:  "sessions show help",
			args:  []string{"help", "sessions", "show"},
			wants: []string{"usage: sai sessions show <id>", "full sensitive"},
		},
		{
			name:  "sessions delete flag",
			args:  []string{"sessions", "delete", "-h"},
			wants: []string{"usage: sai sessions delete <id>"},
		},
		{
			name:  "sessions delete help",
			args:  []string{"help", "sessions", "delete"},
			wants: []string{"usage: sai sessions delete <id>"},
		},
		{
			name:  "sessions prune flag",
			args:  []string{"--keep", "1", "sessions", "prune", "-h"},
			wants: []string{"usage: sai sessions prune --keep N", "--keep must be provided"},
		},
		{
			name:  "sessions prune help",
			args:  []string{"help", "sessions", "prune"},
			wants: []string{"usage: sai sessions prune --keep N", "--keep must be provided"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertCLIHelpWithoutConfig(t, tt.args, tt.wants...)
		})
	}
}

func TestUnknownCommandIncludesHelpHint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"nope"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `unknown command "nope"`, `Run "sai help" for usage.`)
}

func TestPositionalPromptWithoutCommandIsUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"prompt"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `unknown command "prompt"`, `Run "sai help" for usage.`)
}

func TestUnknownRootFlagBeforeCommandIncludesHelpHint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--bad", "chat"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "flag provided but not defined", `Run "sai help" for usage.`)
}

func TestOldConfigDirFlagIsRejected(t *testing.T) {
	for _, args := range [][]string{
		{"--config-dir", t.TempDir(), "models", "list"},
		{"models", "list", "--config-dir", t.TempDir()},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})

			if code != 1 {
				t.Fatalf("RunWithGetwd() code = %d, want 1", code)
			}
			if stdout.String() != "" {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			assertCLIErrorContains(t, stderr.String(), "flag provided but not defined: -config-dir")
		})
	}
}

func TestRunCommandIsUnsupported(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"run", "Say hi"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `unknown command "run"`, `Run "sai help" for usage.`)
	if strings.Contains(stderr.String(), "usage: sai run") {
		t.Fatalf("stderr included run usage: %s", stderr.String())
	}
}

func TestChatUnknownFlagIncludesHelpHint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "--bad"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "flag provided but not defined", `Run "sai help chat" for usage.`)
}

func TestChatMixedHelpDoesNotLoadConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "ignored", "-h", "--config", filepath.Join(t.TempDir(), "missing")}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !strings.Contains(stdout.String(), "usage: sai chat") {
		t.Fatalf("stdout = %q, want chat usage", stdout.String())
	}
}

func TestChatQuitWithoutPromptIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "--quit requires --prompt, --stdin, or --file", "usage: sai chat", `Run "sai help chat" for usage.`)
}

func TestChatStdinWithQuitRunsOneTurnAndExits(t *testing.T) {
	tests := []struct {
		name string
		args func(configDir string) []string
	}{
		{
			name: "chat command",
			args: func(configDir string) []string {
				return []string{"--config", cliConfigPath(configDir), "chat", "--quit", "--stdin"}
			},
		},
		{
			name: "default chat command",
			args: func(configDir string) []string {
				return []string{"--config", cliConfigPath(configDir), "--quit", "--stdin"}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, requests := newCLIRunServer(t,
				`{"choices":[{"delta":{"content":"one"}}]}`,
				`[DONE]`,
			)
			defer server.Close()

			configDir := t.TempDir()
			writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

			var stdout, stderr bytes.Buffer
			code := RunWithIO(tt.args(configDir), strings.NewReader("stdin prompt\nline two\n"), &stdout, &stderr, func() (string, error) {
				return t.TempDir(), nil
			})

			if code != 0 {
				t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
			}
			if got, want := stdout.String(), "one"; got != want {
				t.Fatalf("stdout = %q, want %q", got, want)
			}
			if stderr.String() != "" {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			messages := requestMessages(t, (<-requests).Body)
			assertMessage(t, messages, 0, "system", builtInBaseInstructions)
			assertMessage(t, messages, 1, "user", "stdin prompt\nline two\n")
			assertNoAdditionalCLIRunRequest(t, requests)
		})
	}
}

func TestChatFileWithQuitRunsOneTurnAndExits(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	promptPath := filepath.Join(t.TempDir(), "prompt.md")
	writeCLIFile(t, promptPath, "file prompt\nline two\n")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--file", promptPath}, strings.NewReader("unused\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	messages := requestMessages(t, (<-requests).Body)
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "file prompt\nline two\n")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatStdinAndFileDoNotExpandJSONLLogContent(t *testing.T) {
	tests := []struct {
		name   string
		args   func(configDir string) []string
		stdin  io.Reader
		prompt string
	}{
		{
			name: "stdin",
			args: func(configDir string) []string {
				return []string{"--config", cliConfigPath(configDir), "chat", "--quit", "--stdin"}
			},
			stdin:  strings.NewReader("stdin prompt secret"),
			prompt: "stdin prompt secret",
		},
		{
			name: "file",
			args: func(configDir string) []string {
				promptPath := filepath.Join(t.TempDir(), "prompt.md")
				writeCLIFile(t, promptPath, "file prompt secret")
				return []string{"--config", cliConfigPath(configDir), "chat", "--quit", "--file", promptPath}
			},
			stdin:  strings.NewReader("unused"),
			prompt: "file prompt secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, requests := newCLIRunServer(t,
				`{"choices":[{"delta":{"content":"streamed response secret"}}]}`,
				`[DONE]`,
			)
			defer server.Close()

			configDir := t.TempDir()
			writeCLIRunFixtureInDir(t, configDir, server.URL, "secret-api-key", "openai-chat")

			var stdout, stderr bytes.Buffer
			code := RunWithIO(tt.args(configDir), tt.stdin, &stdout, &stderr, func() (string, error) {
				return t.TempDir(), nil
			})

			if code != 0 {
				t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
			}
			if got, want := stdout.String(), "streamed response secret"; got != want {
				t.Fatalf("stdout = %q, want %q", got, want)
			}
			assertMessage(t, requestMessages(t, (<-requests).Body), 1, "user", tt.prompt)
			assertNoAdditionalCLIRunRequest(t, requests)

			logPaths := sessionLogPaths(t, configDir)
			if len(logPaths) != 1 {
				t.Fatalf("session log paths = %#v, want one", logPaths)
			}
			logData, err := os.ReadFile(logPaths[0])
			if err != nil {
				t.Fatalf("ReadFile(%q) error = %v", logPaths[0], err)
			}
			logText := string(logData)
			for _, leaked := range []string{tt.prompt, "streamed response secret", "secret-api-key"} {
				if strings.Contains(logText, leaked) {
					t.Fatalf("log leaked %q:\n%s", leaked, logText)
				}
			}
			records := readJSONLRecords(t, logData)
			assertCLILogBaseFields(t, records)
			if !hasCLILogRecord(records, "text_delta", "event", "text_delta") {
				t.Fatalf("chat log records missing text_delta: %#v", records)
			}
		})
	}
}

func TestChatInitialInputSourceValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "stdin without quit", args: []string{"chat", "--stdin"}, want: "--stdin requires --quit"},
		{name: "file without quit", args: []string{"chat", "--file", "prompt.md"}, want: "--file requires --quit"},
		{name: "stdin with prompt", args: []string{"chat", "--quit", "--stdin", "--prompt", "prompt"}, want: "--prompt, --stdin, and --file are mutually exclusive"},
		{name: "file with prompt", args: []string{"chat", "--quit", "--file", "prompt.md", "--prompt", "prompt"}, want: "--prompt, --stdin, and --file are mutually exclusive"},
		{name: "stdin with file", args: []string{"chat", "--quit", "--stdin", "--file", "prompt.md"}, want: "--prompt, --stdin, and --file are mutually exclusive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithIO(tt.args, strings.NewReader("stdin"), &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})

			if code != 1 {
				t.Fatalf("RunWithIO() code = %d, want 1", code)
			}
			if stdout.String() != "" {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			assertCLIErrorContains(t, stderr.String(), tt.want, "usage: sai chat", `Run "sai help chat" for usage.`)
		})
	}
}

func TestChatExitReturnsWithoutModelRequest(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("/exit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "> "; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if logPaths := sessionLogPaths(t, configDir); len(logPaths) != 0 {
		t.Fatalf("session log paths = %#v, want none", logPaths)
	}
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestDefaultChatNoCommandEntersREPL(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	projectDir := t.TempDir()
	configDir := filepath.Join(projectDir, ".agents")
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO(nil, strings.NewReader("/exit\n"), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "> "; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if logPaths := sessionLogPaths(t, configDir); len(logPaths) != 0 {
		t.Fatalf("session log paths = %#v, want none", logPaths)
	}
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestDefaultChatAcceptsConfigPathWithoutCommand(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir)}, strings.NewReader("/exit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "> "; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestDefaultChatInitialPromptWithQuitRunsOneTurnAndExits(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "--prompt", "first", "--quit"}, strings.NewReader("second\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	messages := requestMessages(t, (<-requests).Body)
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "first")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestDefaultChatAcceptsModelAndEnableToolsFlags(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "--model", "fast", "--enable-tools", "read_file", "--prompt", "first", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	request := <-requests
	if got := request.Body["model"]; got != "model-fast" {
		t.Fatalf("model = %#v, want model-fast", got)
	}
	assertMessage(t, requestMessages(t, request.Body), 1, "user", "first")
	assertCLIToolNames(t, request.Body, []string{"read_file"})
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatInitialPromptWithQuitRunsOneTurnAndExits(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "first"}, strings.NewReader("second\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	request := <-requests
	messages := requestMessages(t, request.Body)
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "first")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatAcceptsConfigPathAfterCommand(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "--config", cliConfigPath(configDir), "--quit", "--prompt", "first"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertMessage(t, requestMessages(t, (<-requests).Body), 1, "user", "first")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatDelimiterRejectsHelpAsPositionalPrompt(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "--quit", "--", "--help"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "unexpected positional argument; use --prompt", "usage: sai chat", `Run "sai help chat" for usage.`)
}

func TestChatDelimiterAfterConfigPathRejectsHelpAsPositionalPrompt(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "missing")
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "--config", cliConfigPath(configDir), "--quit", "--", "--help"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "unexpected positional argument; use --prompt", "usage: sai chat", `Run "sai help chat" for usage.`)
}

func TestChatDelimiterRejectsConfigPathAsPositionalPrompt(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "missing")
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "--config", cliConfigPath(configDir), "--quit", "--", "--config"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "unexpected positional argument; use --prompt", "usage: sai chat", `Run "sai help chat" for usage.`)
}

func TestChatInitialPromptAllowsQuitAfterPrompt(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--prompt", "first", "--quit"}, strings.NewReader("second\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	messages := requestMessages(t, (<-requests).Body)
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "first")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatInitialPromptAllowsModelFlagAfterPrompt(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--prompt", "first", "--model", "fast", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	request := <-requests
	if got := request.Body["model"]; got != "model-fast" {
		t.Fatalf("model = %#v, want model-fast", got)
	}
	assertMessage(t, requestMessages(t, request.Body), 1, "user", "first")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatAllowsModelFlagBeforeCommand(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--model", "fast", "chat", "--config", cliConfigPath(configDir), "--prompt", "first", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	request := <-requests
	if got := request.Body["model"]; got != "model-fast" {
		t.Fatalf("model = %#v, want model-fast", got)
	}
	assertMessage(t, requestMessages(t, request.Body), 1, "user", "first")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatInitialPromptAllowsEnableToolsFlagAfterPrompt(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--prompt", "first", "--enable-tools", "read_file", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIToolNames(t, (<-requests).Body, []string{"read_file"})
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatAllowsEnableToolsFlagBeforeCommand(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--enable-tools", "read_file", "chat", "--config", cliConfigPath(configDir), "--prompt", "first", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIToolNames(t, (<-requests).Body, []string{"read_file"})
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatInterspersedArgsRejectPositionalPrompt(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "first", "--quit", "--prompt", "second"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "unexpected positional argument; use --prompt", "usage: sai chat", `Run "sai help chat" for usage.`)
}

func TestChatUnknownFlagAfterPromptIncludesHelpHint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "--prompt", "first", "--bad"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "flag provided but not defined", `Run "sai help chat" for usage.`)
}

func TestChatInitialPromptContinuesIntoREPLWithHistory(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"delta":{"content":"one"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"two"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--prompt", "first"}, strings.NewReader("second\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one\ntwo\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "> > "; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}

	firstRequest := <-requests
	firstMessages := requestMessages(t, firstRequest.Body)
	assertMessage(t, firstMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, firstMessages, 1, "user", "first")

	secondRequest := <-requests
	secondMessages := requestMessages(t, secondRequest.Body)
	if len(secondMessages) != 4 {
		t.Fatalf("len(second request messages) = %d, want 4: %#v", len(secondMessages), secondMessages)
	}
	assertMessage(t, secondMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, secondMessages, 1, "user", "first")
	assertMessage(t, secondMessages, 2, "assistant", "one")
	assertMessage(t, secondMessages, 3, "user", "second")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatTwoTurnsCarryForwardUserAndAssistantHistory(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"delta":{"content":"one"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"two"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("first\n\nsecond\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one\ntwo\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if strings.Contains(stdout.String(), "> ") {
		t.Fatalf("stdout contains prompt: %q", stdout.String())
	}
	if got, want := stderr.String(), "> > > > "; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}

	firstRequest := <-requests
	firstMessages := requestMessages(t, firstRequest.Body)
	assertMessage(t, firstMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, firstMessages, 1, "user", "first")

	secondRequest := <-requests
	secondMessages := requestMessages(t, secondRequest.Body)
	if len(secondMessages) != 4 {
		t.Fatalf("len(second request messages) = %d, want 4: %#v", len(secondMessages), secondMessages)
	}
	assertMessage(t, secondMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, secondMessages, 1, "user", "first")
	assertMessage(t, secondMessages, 2, "assistant", "one")
	assertMessage(t, secondMessages, 3, "user", "second")
	assertNoAdditionalCLIRunRequest(t, requests)

	logPaths := sessionLogPaths(t, configDir)
	if len(logPaths) != 1 {
		t.Fatalf("session log paths = %#v, want one chat session log", logPaths)
	}
	data, err := os.ReadFile(logPaths[0])
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", logPaths[0], err)
	}
	records := readJSONLRecords(t, data)
	assertCLILogBaseFields(t, records)
	if !hasCLILogRecord(records, "text_delta", "event", "text_delta") {
		t.Fatalf("chat log records missing text_delta: %#v", records)
	}
}

func TestChatUsageCommandPrintsSummaryWithoutModelRequest(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLIModelContextWindow(t, configDir, 1234)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("/usage\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	errOut := stderr.String()
	for _, want := range []string{
		"CONTEXT_WINDOW\t1234",
		"CONTEXT_WINDOW_SOURCE\tconfigured",
		"CONTEXT_WARNING_THRESHOLD_PERCENT\t80",
		"CONTEXT_LAST_REQUEST_TOKENS\t0",
		"CONTEXT_LAST_INPUT_TOKENS\t0",
		"CONTEXT_LAST_OUTPUT_TOKENS\t0",
		"CONTEXT_LAST_TOTAL_TOKENS\t0",
		"CONTEXT_LAST_USAGE_SOURCE\t(none)",
	} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr = %q, want contain %q", errOut, want)
		}
	}
	if logPaths := sessionLogPaths(t, configDir); len(logPaths) != 0 {
		t.Fatalf("session log paths = %#v, want none", logPaths)
	}
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatUsageCommandShowsProviderUsageAfterTurn(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":13,"total_tokens":24}}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("first\n/usage\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	errOut := stderr.String()
	for _, want := range []string{
		"CONTEXT_WINDOW\t32000",
		"CONTEXT_WINDOW_SOURCE\testimated",
		"CONTEXT_LAST_INPUT_TOKENS\t11",
		"CONTEXT_LAST_OUTPUT_TOKENS\t13",
		"CONTEXT_LAST_TOTAL_TOKENS\t24",
		"CONTEXT_LAST_USAGE_SOURCE\tprovider",
	} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr = %q, want contain %q", errOut, want)
		}
	}
	assertMessage(t, requestMessages(t, (<-requests).Body), 1, "user", "first")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatUsageCommandShowsEstimatedUsageAfterTurn(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"abcd"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("first\n/usage\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "abcd\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	errOut := stderr.String()
	for _, want := range []string{
		"CONTEXT_LAST_OUTPUT_TOKENS\t4",
		"CONTEXT_LAST_USAGE_SOURCE\testimated",
	} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr = %q, want contain %q", errOut, want)
		}
	}
	if strings.Contains(errOut, "CONTEXT_LAST_INPUT_TOKENS\t0") || strings.Contains(errOut, "CONTEXT_LAST_TOTAL_TOKENS\t0") {
		t.Fatalf("stderr = %q, want nonzero estimated input and total tokens", errOut)
	}
	assertMessage(t, requestMessages(t, (<-requests).Body), 1, "user", "first")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatUsageCommandDoesNotLeakPromptAssistantOrToolContent(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"note.txt\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"final response secret"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":21,"completion_tokens":34,"total_tokens":55}}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "note.txt"), "tool output secret")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "secret-api-key", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--enable-tools", "read_file"}, strings.NewReader("user prompt secret\n/usage\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "final response secret\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	errOut := stderr.String()
	for _, want := range []string{
		"tool: read_file note.txt",
		"CONTEXT_LAST_INPUT_TOKENS\t21",
		"CONTEXT_LAST_OUTPUT_TOKENS\t34",
		"CONTEXT_LAST_TOTAL_TOKENS\t55",
		"CONTEXT_LAST_USAGE_SOURCE\tprovider",
	} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr = %q, want contain %q", errOut, want)
		}
	}
	assertCLIErrorOmits(t, errOut, "user prompt secret", "final response secret", "tool output secret", "secret-api-key")
	<-requests
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatMultilineREPLCollectsOneMessagePreservingNewlines(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	input := "\"\"\"\nfirst\n/usage\n/quit\nsecond\n\"\"\"\n/quit\n"
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader(input), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "> > > > > > > "; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	messages := requestMessages(t, (<-requests).Body)
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "first\n/usage\n/quit\nsecond")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatEmptyMultilineREPLInputIsIgnored(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("\"\"\"\n\"\"\"\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "> > > "; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if logPaths := sessionLogPaths(t, configDir); len(logPaths) != 0 {
		t.Fatalf("session log paths = %#v, want none", logPaths)
	}
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatREPLRecoverableErrorContinuesWithoutFailedHistory(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`not-json`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"two"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("failed\nsecond\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "two\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	errOut := stderr.String()
	if !strings.HasPrefix(errOut, "> sai: ") || !strings.Contains(errOut, "parse OpenAI chat stream") || !strings.HasSuffix(errOut, "\n> > ") {
		t.Fatalf("stderr = %q, want recoverable error followed by next prompts", errOut)
	}

	firstRequest := <-requests
	firstMessages := requestMessages(t, firstRequest.Body)
	assertMessage(t, firstMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, firstMessages, 1, "user", "failed")

	secondRequest := <-requests
	secondMessages := requestMessages(t, secondRequest.Body)
	if len(secondMessages) != 2 {
		t.Fatalf("len(second request messages) = %d, want 2: %#v", len(secondMessages), secondMessages)
	}
	assertMessage(t, secondMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, secondMessages, 1, "user", "second")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatActiveTurnInterruptCancelsTurnAndAllowsNextPrompt(t *testing.T) {
	server, requests := newCancelingFirstThenCLIRunServer(t, []string{
		`{"choices":[{"delta":{"content":"two"}}]}`,
		`[DONE]`,
	})
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	stdinReader, stdinWriter := io.Pipe()
	defer stdinReader.Close()
	interrupts := make(chan struct{}, 2)
	var stdout bytes.Buffer
	stderr := newSignalingWriter(chatInputPrompt)
	done := make(chan int, 1)
	go func() {
		done <- runWithProgramContextAndInterrupts(context.Background(), "sai", []string{"--config", cliConfigPath(configDir), "chat"}, stdinReader, &stdout, stderr, func() (string, error) {
			return t.TempDir(), nil
		}, interrupts)
	}()

	waitForChannel(t, stderr.wrote, "initial prompt")
	if _, err := fmt.Fprintln(stdinWriter, "first"); err != nil {
		t.Fatalf("write first input: %v", err)
	}
	firstRequest := <-requests
	assertMessage(t, requestMessages(t, firstRequest.Body), 1, "user", "first")

	interrupts <- struct{}{}
	go func() {
		_, _ = fmt.Fprintln(stdinWriter, "second")
		_, _ = fmt.Fprintln(stdinWriter, "/quit")
	}()

	code := waitForCode(t, done)
	if code != 0 {
		t.Fatalf("chat code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "two\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	secondRequest := <-requests
	secondMessages := requestMessages(t, secondRequest.Body)
	if len(secondMessages) != 2 {
		t.Fatalf("len(second request messages) = %d, want 2: %#v", len(secondMessages), secondMessages)
	}
	assertMessage(t, secondMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, secondMessages, 1, "user", "second")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatRepeatedActiveTurnInterruptDoesNotExitSession(t *testing.T) {
	server, requests := newCancelingFirstThenCLIRunServer(t, []string{
		`{"choices":[{"delta":{"content":"after"}}]}`,
		`[DONE]`,
	})
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	stdinReader, stdinWriter := io.Pipe()
	defer stdinReader.Close()
	interrupts := make(chan struct{}, 4)
	var stdout bytes.Buffer
	stderr := newSignalingWriter(chatInputPrompt)
	done := make(chan int, 1)
	go func() {
		done <- runWithProgramContextAndInterrupts(context.Background(), "sai", []string{"--config", cliConfigPath(configDir), "chat"}, stdinReader, &stdout, stderr, func() (string, error) {
			return t.TempDir(), nil
		}, interrupts)
	}()

	waitForChannel(t, stderr.wrote, "initial prompt")
	if _, err := fmt.Fprintln(stdinWriter, "first"); err != nil {
		t.Fatalf("write first input: %v", err)
	}
	<-requests

	interrupts <- struct{}{}
	interrupts <- struct{}{}
	go func() {
		_, _ = fmt.Fprintln(stdinWriter, "second")
		_, _ = fmt.Fprintln(stdinWriter, "/quit")
	}()

	code := waitForCode(t, done)
	if code != 0 {
		t.Fatalf("chat code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "after\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	secondMessages := requestMessages(t, (<-requests).Body)
	assertMessage(t, secondMessages, 1, "user", "second")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatIdleInterruptExitsSession(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	stdinReader, _ := io.Pipe()
	defer stdinReader.Close()
	interrupts := make(chan struct{}, 1)
	var stdout bytes.Buffer
	stderr := newSignalingWriter(chatInputPrompt)
	done := make(chan int, 1)
	go func() {
		done <- runWithProgramContextAndInterrupts(context.Background(), "sai", []string{"--config", cliConfigPath(configDir), "chat"}, stdinReader, &stdout, stderr, func() (string, error) {
			return t.TempDir(), nil
		}, interrupts)
	}()

	waitForChannel(t, stderr.wrote, "idle prompt")
	interrupts <- struct{}{}

	code := waitForCode(t, done)
	if code != 1 {
		t.Fatalf("chat code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "sai: context canceled")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatQuitActiveTurnInterruptEndsWithoutSavedAssistantHistory(t *testing.T) {
	server, requests := newCancelingFirstThenCLIRunServer(t)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	interrupts := make(chan struct{}, 1)
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runWithProgramContextAndInterrupts(context.Background(), "sai", []string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--prompt", "first"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		}, interrupts)
	}()

	firstRequest := <-requests
	assertMessage(t, requestMessages(t, firstRequest.Body), 1, "user", "first")
	interrupts <- struct{}{}

	code := waitForCode(t, done)
	if code != 1 {
		t.Fatalf("chat code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if sessions := sessionDirs(t, configDir); len(sessions) != 0 {
		t.Fatalf("session dirs = %#v, want none", sessions)
	}
	assertCLIErrorContains(t, stderr.String(), "sai: context canceled")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatREPLStdoutWriteErrorExitsWithoutNextTurn(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"delta":{"content":"one"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"two"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	stdoutErr := errors.New("stdout write failed")
	var stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("first\nsecond\n/quit\n"), failingWriter{err: stdoutErr}, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	assertCLIErrorContains(t, stderr.String(), "> sai: stdout write failed")
	firstRequest := <-requests
	firstMessages := requestMessages(t, firstRequest.Body)
	assertMessage(t, firstMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, firstMessages, 1, "user", "first")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatToolCallHistoryCarriesIntoNextTurn(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"note.txt\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"done"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"next"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "note.txt"), "tool output")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--enable-tools", "read_file"}, strings.NewReader("Read note\nNext\n/exit\n"), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "done\nnext\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "> \ntool: read_file note.txt\n> > "; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	assertCLIToolStatus(t, stdout.String(), stderr.String(), "read_file note.txt", "tool output")

	<-requests
	<-requests
	thirdRequest := <-requests
	messages := requestMessages(t, thirdRequest.Body)
	if len(messages) != 6 {
		t.Fatalf("len(third request messages) = %d, want 6: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "Read note")
	assertAssistantToolCallMessage(t, messages, 2, "call_1", "read_file", `{"path":"note.txt"}`)
	assertToolMessage(t, messages, 3, "call_1", "tool output")
	assertMessage(t, messages, 4, "assistant", "done")
	assertMessage(t, messages, 5, "user", "Next")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatContextWindowWarningDoesNotLeakSensitiveContent(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"assistant secret"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLIModelContextWindow(t, configDir, 260)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "user prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "assistant secret"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "sai: warning: estimated context usage") || !strings.Contains(errOut, "/260 tokens") || !strings.Contains(errOut, "no context was truncated") {
		t.Fatalf("stderr = %q, want context window warning", errOut)
	}
	assertCLIErrorOmits(t, errOut, "user prompt secret", "assistant secret", "direct-secret-value", builtInBaseInstructions)
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatOverBudgetRejectsBeforeProviderRequestWithoutLeakingContent(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected assistant secret"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "AGENTS.md"), "project developer secret\n")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLIModelContextWindow(t, configDir, 10)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "user prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "context window budget exceeded", "refusing to send provider request", "no context was truncated")
	assertCLIErrorOmits(t, stderr.String(), "user prompt secret", "project developer secret", "unexpected assistant secret", "direct-secret-value", builtInBaseInstructions)
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatContextBudgetDoesNotDropSystemDeveloperOrToolSchemas(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "AGENTS.md"), "project developer secret\n")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLIModelContextWindow(t, configDir, 5000)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "read_file", "--prompt", "user prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	request := <-requests
	messages := requestMessages(t, request.Body)
	if len(messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "developer", "project developer secret\n")
	assertMessage(t, messages, 2, "user", "user prompt secret")
	assertCLIToolNames(t, request.Body, []string{"read_file"})
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatDefaultConfigDoesNotCreateResumableSession(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "first"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)
	if _, err := os.Stat(filepath.Join(configDir, "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sessions dir stat error = %v, want not exist", err)
	}
}

func TestChatSaveSessionFlagWritesFullToolHistory(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"note.txt\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"done"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "note.txt"), "tool output")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--enable-tools", "read_file", "--prompt", "Read note"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	assertResumableSessionNoticeOnce(t, stderr.String())
	assertCLIErrorOmits(t, stderr.String(), "Read note", "done", "tool output")
	<-requests
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)

	session := loadOnlyCLISession(t, filepath.Join(configDir, "sessions"))
	if session.Provider != "fake" || session.ModelProfile != "default" || session.ModelID != "model-default" {
		t.Fatalf("saved model metadata = provider %q profile %q model %q", session.Provider, session.ModelProfile, session.ModelID)
	}
	if session.CWD != projectDir || session.ConfigPath != cliConfigPath(configDir) {
		t.Fatalf("saved paths = cwd %q config %q, want cwd %q config %q", session.CWD, session.ConfigPath, projectDir, cliConfigPath(configDir))
	}
	if !sameStringsForTest(session.EnabledTools, []string{"read_file"}) {
		t.Fatalf("EnabledTools = %#v, want read_file", session.EnabledTools)
	}
	if session.ShowReasoning {
		t.Fatal("ShowReasoning = true, want false")
	}
	if !session.SaveToolResults {
		t.Fatal("SaveToolResults = false, want true")
	}
	assertSavedMessage(t, session.InstructionsSnapshot, 0, model.MessageRoleSystem, builtInBaseInstructions)
	if len(session.Messages) != 5 {
		t.Fatalf("len(saved messages) = %d, want 5: %#v", len(session.Messages), session.Messages)
	}
	assertSavedMessage(t, session.Messages, 0, model.MessageRoleSystem, builtInBaseInstructions)
	assertSavedMessage(t, session.Messages, 1, model.MessageRoleUser, "Read note")
	assertSavedAssistantToolCallMessage(t, session.Messages, 2, "call_1", "read_file", `{"path":"note.txt"}`)
	assertSavedToolMessage(t, session.Messages, 3, "call_1", "tool output")
	assertSavedMessage(t, session.Messages, 4, model.MessageRoleAssistant, "done")
}

func TestChatSaveSessionFlagOverridesDisabledConfigAndPrintsNoticeBeforeProviderRequest(t *testing.T) {
	var stdout bytes.Buffer
	stderr := newSignalingWriter(resumableSessionSaveNoticeText)
	requests := make(chan capturedCLIRunRequest, 1)
	noticeAtRequest := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		noticeAtRequest <- stderr.String()
		requests <- capturedCLIRunRequest{
			Path:             r.URL.Path,
			Authorization:    r.Header.Get("Authorization"),
			XAPIKey:          r.Header.Get("x-api-key"),
			AnthropicVersion: r.Header.Get("anthropic-version"),
			ContentType:      r.Header.Get("Content-Type"),
			RawBody:          body,
			Body:             decodeCLIJSON(t, body),
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--prompt", "first prompt secret"}, strings.NewReader(""), &stdout, stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	atRequest := <-noticeAtRequest
	if !strings.Contains(atRequest, resumableSessionSaveNoticeText) {
		t.Fatalf("stderr at provider request = %q, want session save notice", atRequest)
	}
	assertCLIErrorOmits(t, atRequest, "first prompt secret", "one", "direct-secret-value")
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)
	assertResumableSessionNoticeOnce(t, stderr.String())
	session := loadOnlyCLISession(t, filepath.Join(configDir, "sessions"))
	if len(session.Messages) != 3 {
		t.Fatalf("len(saved messages) = %d, want 3: %#v", len(session.Messages), session.Messages)
	}
}

func TestChatConfiguredSessionsSaveFullMessages(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLISessionsConfig(t, configDir, true, true)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "first"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	assertResumableSessionNoticeOnce(t, stderr.String())
	assertCLIErrorOmits(t, stderr.String(), "first", "one", "direct-secret-value")
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)

	session := loadOnlyCLISession(t, filepath.Join(configDir, "sessions"))
	if len(session.Messages) != 3 {
		t.Fatalf("len(saved messages) = %d, want 3: %#v", len(session.Messages), session.Messages)
	}
	assertSavedMessage(t, session.Messages, 0, model.MessageRoleSystem, builtInBaseInstructions)
	assertSavedMessage(t, session.Messages, 1, model.MessageRoleUser, "first")
	assertSavedMessage(t, session.Messages, 2, model.MessageRoleAssistant, "one")
	if session.Context.ContextWindow != contextwindow.DefaultContextWindowTokens || session.Context.ContextWindowSource != string(contextwindow.WindowSourceEstimated) {
		t.Fatalf("session context window = %d/%q, want default estimated", session.Context.ContextWindow, session.Context.ContextWindowSource)
	}
	if session.Context.LastUsageSource != string(contextwindow.UsageSourceEstimated) {
		t.Fatalf("session LastUsageSource = %q, want estimated", session.Context.LastUsageSource)
	}
	if session.Context.LastRequestTokens <= 0 || session.Context.LastInputTokens <= 0 || session.Context.LastTotalTokens <= 0 {
		t.Fatalf("session context metadata missing usage estimates: %#v", session.Context)
	}
}

func TestChatConfiguredSessionsCanBeDisabledBySaveSessionFalse(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLISessionsConfig(t, configDir, true, true)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--save-session=false", "--quit", "--prompt", "first prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)
	if strings.Contains(stderr.String(), resumableSessionSaveNoticeText) {
		t.Fatalf("stderr = %q, want no session save notice", stderr.String())
	}
	assertCLIErrorOmits(t, stderr.String(), "first prompt secret", "one", "direct-secret-value")
	if _, err := os.Stat(filepath.Join(configDir, "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sessions dir stat error = %v, want not exist", err)
	}
}

func TestChatSaveSessionRecordsProviderUsageMetadata(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":13,"total_tokens":24}}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLIModelContextWindow(t, configDir, 128000)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--prompt", "first"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)

	session := loadOnlyCLISession(t, filepath.Join(configDir, "sessions"))
	if session.Context.ContextWindow != 128000 || session.Context.ContextWindowSource != string(contextwindow.WindowSourceConfigured) {
		t.Fatalf("session context window = %d/%q, want 128000/configured", session.Context.ContextWindow, session.Context.ContextWindowSource)
	}
	if session.Context.LastUsageSource != string(contextwindow.UsageSourceProvider) {
		t.Fatalf("session LastUsageSource = %q, want provider", session.Context.LastUsageSource)
	}
	if session.Context.LastInputTokens != 11 || session.Context.LastOutputTokens != 13 || session.Context.LastTotalTokens != 24 {
		t.Fatalf("session usage = input %d output %d total %d, want 11/13/24", session.Context.LastInputTokens, session.Context.LastOutputTokens, session.Context.LastTotalTokens)
	}
	assertCLIErrorOmits(t, stderr.String(), "first", "one", "direct-secret-value")
}

func TestChatConfiguredSessionNoticePrintsOnceWithoutSensitiveContent(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"delta":{"content":"first assistant secret"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"second assistant secret"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLISessionsConfig(t, configDir, true, true)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("first prompt secret\nsecond prompt secret\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "first assistant secret\nsecond assistant secret\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	<-requests
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)
	errOut := stderr.String()
	assertResumableSessionNoticeOnce(t, errOut)
	if !strings.HasPrefix(errOut, resumableSessionSaveNoticeText+"\n> ") {
		t.Fatalf("stderr = %q, want session notice before first REPL prompt", errOut)
	}
	assertCLIErrorOmits(t, errOut, "first prompt secret", "second prompt secret", "first assistant secret", "second assistant secret", "direct-secret-value")
}

func TestChatResumeSendsRestoredMessagesAndSavesContinuation(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"two"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	sessionRoot := filepath.Join(configDir, "sessions")
	writeCLISession(t, sessionRoot, sessions.Session{
		ID:              "resume-session",
		CreatedAt:       time.Date(2026, 7, 2, 3, 4, 5, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 7, 2, 3, 4, 6, 0, time.UTC),
		Version:         sessions.CurrentVersion,
		Provider:        "fake",
		ModelProfile:    "fast",
		ModelID:         "model-fast",
		ModelParameters: map[string]any{"temperature": 0.2, "max_tokens": 64},
		ConfigPath:      cliConfigPath(configDir),
		InstructionsSnapshot: []model.Message{
			{Role: model.MessageRoleSystem, Content: "saved instructions"},
		},
		Messages: []model.Message{
			{Role: model.MessageRoleSystem, Content: "saved instructions"},
			{Role: model.MessageRoleUser, Content: "first"},
			{Role: model.MessageRoleAssistant, Content: "one"},
		},
		SaveToolResults: true,
	})

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--resume", "resume-session", "--quit", "--prompt", "next"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	assertResumableSessionNoticeOnce(t, stderr.String())
	assertCLIErrorOmits(t, stderr.String(), "first", "one", "next", "two")
	request := <-requests
	if got := request.Body["model"]; got != "model-fast" {
		t.Fatalf("model = %#v, want model-fast", got)
	}
	messages := requestMessages(t, request.Body)
	if len(messages) != 4 {
		t.Fatalf("len(request messages) = %d, want 4: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", "saved instructions")
	assertMessage(t, messages, 1, "user", "first")
	assertMessage(t, messages, 2, "assistant", "one")
	assertMessage(t, messages, 3, "user", "next")
	assertNoAdditionalCLIRunRequest(t, requests)

	session := loadCLISession(t, sessionRoot, "resume-session")
	if len(session.Messages) != 5 {
		t.Fatalf("len(saved messages) = %d, want 5: %#v", len(session.Messages), session.Messages)
	}
	assertSavedMessage(t, session.Messages, 3, model.MessageRoleUser, "next")
	assertSavedMessage(t, session.Messages, 4, model.MessageRoleAssistant, "two")
}

func TestChatContinueUsesLatestSession(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"continued"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	sessionRoot := filepath.Join(configDir, "sessions")
	writeCLISession(t, sessionRoot, sessions.Session{
		ID:              "older-session",
		CreatedAt:       time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 7, 2, 3, 1, 0, 0, time.UTC),
		Version:         sessions.CurrentVersion,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		Messages:        []model.Message{{Role: model.MessageRoleUser, Content: "older"}},
		SaveToolResults: true,
	})
	writeCLISession(t, sessionRoot, sessions.Session{
		ID:              "newer-session",
		CreatedAt:       time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 7, 2, 3, 2, 0, 0, time.UTC),
		Version:         sessions.CurrentVersion,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		Messages:        []model.Message{{Role: model.MessageRoleUser, Content: "newer"}},
		SaveToolResults: true,
	})

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--continue", "--quit", "--prompt", "next"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	assertResumableSessionNoticeOnce(t, stderr.String())
	assertCLIErrorOmits(t, stderr.String(), "newer", "next", "continued")
	messages := requestMessages(t, (<-requests).Body)
	if len(messages) != 2 {
		t.Fatalf("len(request messages) = %d, want 2: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "user", "newer")
	assertMessage(t, messages, 1, "user", "next")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatResumeUsesSavedContextMetadataForBudget(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected assistant secret"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLIModelContextWindow(t, configDir, 5000)
	sessionRoot := filepath.Join(configDir, "sessions")
	writeCLISession(t, sessionRoot, sessions.Session{
		ID:           "small-context-session",
		CreatedAt:    time.Date(2026, 7, 2, 3, 4, 5, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 7, 2, 3, 4, 6, 0, time.UTC),
		Version:      sessions.CurrentVersion,
		Provider:     "fake",
		ModelProfile: "default",
		ModelID:      "model-default",
		Context: contextwindow.Metadata{
			ContextWindow:           10,
			ContextWindowSource:     string(contextwindow.WindowSourceConfigured),
			WarningThresholdPercent: contextwindow.WarningThresholdPercent,
			LastRequestTokens:       9,
		},
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "old prompt secret"},
		},
		SaveToolResults: true,
	})

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--resume", "small-context-session", "--quit", "--prompt", "next prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "context window budget exceeded", "context window 10")
	assertCLIErrorOmits(t, stderr.String(), "old prompt secret", "next prompt secret", "unexpected assistant secret", "direct-secret-value")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatResumeKeepsSavedShowReasoningWhenConfigEnablesIt(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"reasoning_content":"hidden reasoning secret"}}]}`,
		`{"choices":[{"delta":{"content":"visible"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLIAgentShowReasoning(t, configDir, true)
	sessionRoot := filepath.Join(configDir, "sessions")
	writeCLISession(t, sessionRoot, sessions.Session{
		ID:              "reasoning-session",
		CreatedAt:       time.Date(2026, 7, 2, 3, 4, 5, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 7, 2, 3, 4, 6, 0, time.UTC),
		Version:         sessions.CurrentVersion,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		ShowReasoning:   false,
		Messages:        []model.Message{{Role: model.MessageRoleUser, Content: "old prompt secret"}},
		SaveToolResults: true,
	})

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--resume", "reasoning-session", "--quit", "--prompt", "next prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "visible"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)
	assertCLIErrorOmits(t, stdout.String(), "hidden reasoning secret")
	session := loadCLISession(t, sessionRoot, "reasoning-session")
	if session.ShowReasoning {
		t.Fatal("saved ShowReasoning = true, want false from resumed session")
	}
}

func TestChatResumeRejectsConflictingCLISelections(t *testing.T) {
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
	writeCLISession(t, filepath.Join(configDir, "sessions"), sessions.Session{
		ID:              "conflict-session",
		CreatedAt:       time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 7, 2, 3, 1, 0, 0, time.UTC),
		Version:         sessions.CurrentVersion,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		EnabledTools:    []string{"read_file"},
		EnabledMCP:      []string{"local"},
		EnabledSkills:   []string{"alpha"},
		ShowReasoning:   false,
		Messages:        []model.Message{{Role: model.MessageRoleUser, Content: "first"}},
		SaveToolResults: true,
	})

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "provider", args: []string{"--provider", "other"}, want: "--provider"},
		{name: "model", args: []string{"--model", "fast"}, want: "--model"},
		{name: "tools", args: []string{"--enable-tools", "write_file"}, want: "--enable-tools"},
		{name: "mcp", args: []string{"--enable-mcp", "remote"}, want: "--enable-mcp"},
		{name: "reasoning", args: []string{"--show-reasoning"}, want: "--show-reasoning"},
		{name: "save session false", args: []string{"--save-session=false"}, want: "--save-session=false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"--config", cliConfigPath(configDir), "chat", "--resume", "conflict-session", "--quit", "--prompt", "next"}
			args = append(args, tt.args...)
			var stdout, stderr bytes.Buffer
			code := RunWithIO(args, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
				return t.TempDir(), nil
			})
			if code != 1 {
				t.Fatalf("RunWithIO() code = %d, want 1", code)
			}
			if stdout.String() != "" {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			assertCLIErrorContains(t, stderr.String(), "cannot resume session", tt.want)
		})
	}

	writeCLISession(t, filepath.Join(configDir, "sessions"), sessions.Session{
		ID:              "reasoning-enabled-session",
		CreatedAt:       time.Date(2026, 7, 2, 3, 2, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 7, 2, 3, 3, 0, 0, time.UTC),
		Version:         sessions.CurrentVersion,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		ShowReasoning:   true,
		Messages:        []model.Message{{Role: model.MessageRoleUser, Content: "first"}},
		SaveToolResults: true,
	})
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--resume", "reasoning-enabled-session", "--show-reasoning=false", "--quit", "--prompt", "next"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})
	if code != 1 {
		t.Fatalf("RunWithIO(show reasoning false) code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "cannot resume session", "--show-reasoning=false")
}

func TestChatResumeAndContinueAreMutuallyExclusiveWithoutConfigLoad(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "--resume", "some-session", "--continue"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "cannot use --resume with --continue", `Run "sai help chat" for usage.`)
}

func TestChatContinueRejectsConflictingReasoningAndSaveFlags(t *testing.T) {
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
	writeCLISession(t, filepath.Join(configDir, "sessions"), sessions.Session{
		ID:              "latest-session",
		CreatedAt:       time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 7, 2, 3, 1, 0, 0, time.UTC),
		Version:         sessions.CurrentVersion,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		ShowReasoning:   true,
		Messages:        []model.Message{{Role: model.MessageRoleUser, Content: "first prompt secret"}},
		SaveToolResults: true,
	})

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "reasoning false", args: []string{"--show-reasoning=false"}, want: "--show-reasoning=false"},
		{name: "save false", args: []string{"--save-session=false"}, want: "--save-session=false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"--config", cliConfigPath(configDir), "chat", "--continue", "--quit", "--prompt", "next prompt secret"}
			args = append(args, tt.args...)
			var stdout, stderr bytes.Buffer
			code := RunWithIO(args, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
				return t.TempDir(), nil
			})
			if code != 1 {
				t.Fatalf("RunWithIO() code = %d, want 1", code)
			}
			if stdout.String() != "" {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			assertCLIErrorContains(t, stderr.String(), "cannot resume session", tt.want)
			assertCLIErrorOmits(t, stderr.String(), "first prompt secret", "next prompt secret", "direct-secret-value")
		})
	}
}

func TestChatResumeRejectsSessionSavedWithoutReliableToolResults(t *testing.T) {
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
	writeCLISession(t, filepath.Join(configDir, "sessions"), sessions.Session{
		ID:              "partial-session",
		Version:         sessions.CurrentVersion,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		Messages:        []model.Message{{Role: model.MessageRoleUser, Content: "first prompt secret"}},
		SaveToolResults: false,
	})

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--resume", "partial-session", "--quit", "--prompt", "next prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `session "partial-session" cannot be reliably resumed because save_tool_results is false`)
	assertCLIErrorOmits(t, stderr.String(), "first prompt secret", "next prompt secret", "direct-secret-value")
}

func TestChatSaveToolResultsFalseRejectsSaveAndResume(t *testing.T) {
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
	setCLISessionsConfig(t, configDir, false, false)
	writeCLISession(t, filepath.Join(configDir, "sessions"), sessions.Session{
		ID:              "existing-session",
		Version:         sessions.CurrentVersion,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		Messages:        []model.Message{{Role: model.MessageRoleUser, Content: "first"}},
		SaveToolResults: true,
	})

	tests := []struct {
		name string
		args []string
	}{
		{name: "save flag", args: []string{"--save-session", "--quit", "--prompt", "first"}},
		{name: "resume", args: []string{"--resume", "existing-session", "--quit", "--prompt", "next"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"--config", cliConfigPath(configDir), "chat"}, tt.args...)
			var stdout, stderr bytes.Buffer
			code := RunWithIO(args, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
				return t.TempDir(), nil
			})
			if code != 1 {
				t.Fatalf("RunWithIO() code = %d, want 1", code)
			}
			if stdout.String() != "" {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			assertCLIErrorContains(t, stderr.String(), "resumable sessions require sessions.save_tool_results: true")
		})
	}
}

func TestSessionsListShowsMetadataWithoutSensitiveMessagesWhenDisabled(t *testing.T) {
	configDir := writeCLIFixture(t)
	sessionRoot := filepath.Join(configDir, "sessions")
	writeCLISession(t, sessionRoot, sessions.Session{
		ID:           "older-session",
		CreatedAt:    time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 7, 2, 3, 1, 0, 0, time.UTC),
		Version:      sessions.CurrentVersion,
		Provider:     "paperhub",
		ModelProfile: "glm-5.2",
		ModelID:      "glm-5.2",
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "older prompt secret"},
			{Role: model.MessageRoleAssistant, Content: "older assistant secret"},
			{Role: model.MessageRoleTool, ToolCallID: "call_older", Content: "older tool secret"},
		},
		SaveToolResults: true,
	})
	writeCLISession(t, sessionRoot, sessions.Session{
		ID:           "newer-session",
		CreatedAt:    time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 7, 2, 3, 2, 0, 0, time.UTC),
		Version:      sessions.CurrentVersion,
		Provider:     "openai",
		ModelProfile: "default",
		ModelID:      "gpt-5.1",
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "newer prompt secret"},
			{Role: model.MessageRoleAssistant, Content: "newer assistant secret"},
			{Role: model.MessageRoleTool, ToolCallID: "call_newer", Content: "newer tool secret"},
		},
		SaveToolResults: true,
	})

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "sessions", "list"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"ID\tUPDATED\tPROVIDER\tMODEL/PROFILE",
		"newer-session\t2026-07-02T03:02:00Z\topenai\tgpt-5.1/default",
		"older-session\t2026-07-02T03:01:00Z\tpaperhub\tglm-5.2/glm-5.2",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("sessions list output missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "newer-session") > strings.Index(out, "older-session") {
		t.Fatalf("sessions list order = %q, want newest first", out)
	}
	assertCLIErrorOmits(t, out, "older prompt secret", "older assistant secret", "older tool secret", "newer prompt secret", "newer assistant secret", "newer tool secret")
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestSessionsShowPrintsMetadataAndSensitiveContentWarning(t *testing.T) {
	configDir := writeCLIFixture(t)
	writeCLISession(t, filepath.Join(configDir, "sessions"), sessions.Session{
		ID:           "show-session",
		CreatedAt:    time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 7, 2, 3, 1, 0, 0, time.UTC),
		Version:      sessions.CurrentVersion,
		Provider:     "paperhub",
		ModelProfile: "glm-5.2-fast",
		ModelID:      "glm-5.2",
		CWD:          filepath.Join(t.TempDir(), "project"),
		ConfigPath:   cliConfigPath(configDir),
		EnabledTools: []string{"read_file"},
		EnabledMCP:   []string{"local"},
		EnabledSkills: []string{
			"review",
		},
		ShowReasoning: true,
		InstructionsSnapshot: []model.Message{
			{Role: model.MessageRoleSystem, Content: "instruction secret"},
		},
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "prompt body secret"},
			{Role: model.MessageRoleAssistant, Content: "assistant body secret"},
			{Role: model.MessageRoleTool, ToolCallID: "call_show", Content: "tool body secret"},
		},
		Context: contextwindow.Metadata{
			ContextWindow:           128000,
			ContextWindowSource:     string(contextwindow.WindowSourceConfigured),
			WarningThresholdPercent: contextwindow.WarningThresholdPercent,
			LastRequestTokens:       1000,
			LastInputTokens:         900,
			LastOutputTokens:        50,
			LastTotalTokens:         950,
			LastUsageSource:         string(contextwindow.UsageSourceProvider),
		},
		SaveToolResults: true,
	})

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"sessions", "show", "show-session", "--config", cliConfigPath(configDir)}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"WARNING: session files contain full sensitive content",
		"ID\tshow-session",
		"UPDATED\t2026-07-02T03:01:00Z",
		"PROVIDER\tpaperhub",
		"MODEL_PROFILE\tglm-5.2-fast",
		"MODEL_ID\tglm-5.2",
		"CONFIG_PATH\t" + cliConfigPath(configDir),
		"ENABLED_TOOLS\tread_file",
		"ENABLED_MCP\tlocal",
		"ENABLED_SKILLS\treview",
		"SHOW_REASONING\ttrue",
		"CONTEXT_WINDOW\t128000",
		"CONTEXT_WINDOW_SOURCE\tconfigured",
		"CONTEXT_WARNING_THRESHOLD_PERCENT\t80",
		"CONTEXT_LAST_REQUEST_TOKENS\t1000",
		"CONTEXT_LAST_INPUT_TOKENS\t900",
		"CONTEXT_LAST_OUTPUT_TOKENS\t50",
		"CONTEXT_LAST_TOTAL_TOKENS\t950",
		"CONTEXT_LAST_USAGE_SOURCE\tprovider",
		"SAVE_TOOL_RESULTS\ttrue",
		"INSTRUCTION_COUNT\t1",
		"MESSAGE_COUNT\t3",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("sessions show output missing %q:\n%s", want, out)
		}
	}
	assertCLIErrorOmits(t, out, "instruction secret", "prompt body secret", "assistant body secret", "tool body secret")
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestSessionsDeleteRemovesSessionAndReportsMissingSession(t *testing.T) {
	configDir := writeCLIFixture(t)
	sessionRoot := filepath.Join(configDir, "sessions")
	writeCLISession(t, sessionRoot, sessions.Session{
		ID:           "delete-session",
		CreatedAt:    time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 7, 2, 3, 1, 0, 0, time.UTC),
		Version:      sessions.CurrentVersion,
		Provider:     "paperhub",
		ModelProfile: "glm-5.2",
		ModelID:      "glm-5.2",
		Messages:     []model.Message{{Role: model.MessageRoleUser, Content: "delete prompt secret"}},
	})

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "sessions", "delete", "delete-session"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd(delete) code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "deleted session delete-session\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if _, err := sessions.NewStore(sessionRoot).Load("delete-session"); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("Load(deleted) error = %v, want ErrNotFound", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = RunWithGetwd([]string{"--config", cliConfigPath(configDir), "sessions", "delete", "missing-session"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd(missing) code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `resumable session "missing-session" was not found`)
}

func TestSessionsPruneRequiresExplicitKeepWithoutConfigLoad(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing", args: []string{"sessions", "prune"}, want: "--keep must be provided"},
		{name: "negative", args: []string{"sessions", "prune", "--keep", "-1"}, want: "--keep must be 0 or greater"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(tt.args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})

			if code != 1 {
				t.Fatalf("RunWithGetwd() code = %d, want 1", code)
			}
			if stdout.String() != "" {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			assertCLIErrorContains(t, stderr.String(), tt.want, `Run "sai help sessions prune" for usage.`)
		})
	}
}

func TestSessionsPruneKeepsNewestSessionsWithMixedKeepFlag(t *testing.T) {
	tests := []struct {
		name string
		args func(configDir string) []string
	}{
		{
			name: "root keep",
			args: func(configDir string) []string {
				return []string{"--config", cliConfigPath(configDir), "--keep", "1", "sessions", "prune"}
			},
		},
		{
			name: "group keep",
			args: func(configDir string) []string {
				return []string{"sessions", "--keep", "1", "--config", cliConfigPath(configDir), "prune"}
			},
		},
		{
			name: "command keep",
			args: func(configDir string) []string {
				return []string{"sessions", "prune", "--config", cliConfigPath(configDir), "--keep", "1"}
			},
		},
		{
			name: "keep zero",
			args: func(configDir string) []string {
				return []string{"--config", cliConfigPath(configDir), "sessions", "prune", "--keep", "0"}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configDir := writeCLIFixture(t)
			sessionRoot := filepath.Join(configDir, "sessions")
			writeCLIPruneSession(t, sessionRoot, "oldest-session", time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC))
			writeCLIPruneSession(t, sessionRoot, "older-session", time.Date(2026, 7, 2, 3, 1, 0, 0, time.UTC))
			writeCLIPruneSession(t, sessionRoot, "newest-session", time.Date(2026, 7, 2, 3, 2, 0, 0, time.UTC))

			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(tt.args(configDir), &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})

			if code != 0 {
				t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
			}
			out := stdout.String()
			if tt.name == "keep zero" {
				for _, want := range []string{"deleted 3 sessions", "newest-session", "older-session", "oldest-session"} {
					if !strings.Contains(out, want) {
						t.Fatalf("prune output missing %q:\n%s", want, out)
					}
				}
				infos, err := sessions.NewStore(sessionRoot).List()
				if err != nil {
					t.Fatalf("List() after prune error = %v", err)
				}
				if len(infos) != 0 {
					t.Fatalf("remaining sessions = %#v, want none", infos)
				}
				return
			}

			for _, want := range []string{"deleted 2 sessions", "older-session", "oldest-session"} {
				if !strings.Contains(out, want) {
					t.Fatalf("prune output missing %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "newest-session") {
				t.Fatalf("prune output included kept session:\n%s", out)
			}
			infos, err := sessions.NewStore(sessionRoot).List()
			if err != nil {
				t.Fatalf("List() after prune error = %v", err)
			}
			if len(infos) != 1 || infos[0].ID != "newest-session" {
				t.Fatalf("remaining sessions = %#v, want only newest-session", infos)
			}
			if stderr.String() != "" {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestRunWithContextCancelReachesProviderRequest(t *testing.T) {
	requestStarted := make(chan capturedCLIRunRequest, 1)
	requestContextDone := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		requestStarted <- capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}

		<-r.Context().Done()
		close(requestContextDone)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- RunWithContext(ctx, []string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "hello"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})
	}()

	var request capturedCLIRunRequest
	select {
	case request = <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for provider request")
	}
	assertMessage(t, requestMessages(t, request.Body), 1, "user", "hello")

	cancel()
	select {
	case <-requestContextDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for provider request context cancellation")
	}

	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("RunWithContext() code = %d, want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RunWithContext to return")
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "context canceled")
}

func TestRunWithContextCancelFlushesLoggerAfterStreamEvent(t *testing.T) {
	requestStarted := make(chan capturedCLIRunRequest, 1)
	requestContextDone := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		requestStarted <- capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer does not support flush")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"streamed response secret\"}}]}\n\n")
		flusher.Flush()

		<-r.Context().Done()
		close(requestContextDone)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	stdout := newSignalingWriter("streamed response secret")
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- RunWithContext(ctx, []string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "user prompt secret"}, strings.NewReader(""), stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})
	}()

	var request capturedCLIRunRequest
	select {
	case request = <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for provider request")
	}
	assertMessage(t, requestMessages(t, request.Body), 1, "user", "user prompt secret")

	select {
	case <-stdout.wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for streamed text")
	}
	cancel()

	select {
	case <-requestContextDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for provider request context cancellation")
	}
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("RunWithContext() code = %d, want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RunWithContext to return")
	}
	if got, want := stdout.String(), "streamed response secret"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIErrorContains(t, stderr.String(), "context canceled")

	logPaths := sessionLogPaths(t, configDir)
	if len(logPaths) != 1 {
		t.Fatalf("session log paths = %#v, want one canceled session log", logPaths)
	}
	logData, err := os.ReadFile(logPaths[0])
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", logPaths[0], err)
	}
	logText := string(logData)
	for _, leaked := range []string{"user prompt secret", "streamed response secret", "direct-secret-value"} {
		if strings.Contains(logText, leaked) {
			t.Fatalf("log leaked %q:\n%s", leaked, logText)
		}
	}
	records := readJSONLRecords(t, logData)
	assertCLILogBaseFields(t, records)
	if !hasCLILogRecord(records, "text_delta", "event", "text_delta") {
		t.Fatalf("log records missing flushed text_delta: %#v", records)
	}
	if !hasCLILogRecord(records, "error", "message", "read OpenAI chat stream") {
		t.Fatalf("log records missing cancel error: %#v", records)
	}
}

func TestRunUsesDefaultProviderModelAndOutputsTextDelta(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"hello"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL+"/v1", "$SAI_TEST_API_KEY", "openai-chat")
	t.Setenv("SAI_TEST_API_KEY", "env-secret-value")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Say hi"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "hello"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	request := <-requests
	if request.Path != "/v1/chat/completions" {
		t.Fatalf("request path = %q, want /v1/chat/completions", request.Path)
	}
	if request.Authorization != "Bearer env-secret-value" {
		t.Fatalf("Authorization = %q, want env API key", request.Authorization)
	}
	if got := request.Body["model"]; got != "model-default" {
		t.Fatalf("model = %#v, want model-default", got)
	}
	messages := requestMessages(t, request.Body)
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "Say hi")
	assertCLIRequestOmitsKey(t, request.Body, "tools")
}

func TestRunOpenAIResponsesProviderOutputsTextDelta(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"type":"response.output_text.delta","delta":"hello "}`,
		`{"type":"response.output_text.delta","delta":"responses"}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "responses-secret-value", "openai-responses")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Say hi"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "hello responses"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	request := <-requests
	if request.Path != "/responses" {
		t.Fatalf("request path = %q, want /responses", request.Path)
	}
	if request.Authorization != "Bearer responses-secret-value" {
		t.Fatalf("Authorization = %q, want Responses API key", request.Authorization)
	}
	if request.ContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", request.ContentType)
	}
	if got := request.Body["model"]; got != "model-default" {
		t.Fatalf("model = %#v, want model-default", got)
	}
	assertJSONNumber(t, request.Body["temperature"], "0.6")
	assertJSONNumber(t, request.Body["max_output_tokens"], "128")
	assertCLIRequestOmitsKey(t, request.Body, "type")
	assertCLIRequestOmitsKey(t, request.Body, "max_tokens")
	input := requestInput(t, request.Body)
	assertMessage(t, input, 0, "system", builtInBaseInstructions)
	assertMessage(t, input, 1, "user", "Say hi")
	assertCLIRequestOmitsKey(t, request.Body, "tools")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunOpenAICodexProviderForcesStoreFalseForExistingConfig(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"type":"response.output_text.delta","delta":"hello codex"}`,
		`{"type":"response.completed"}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	providersDir := filepath.Join(configDir, "providers")
	authDir := filepath.Join(configDir, "auth")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(providers) error = %v", err)
	}
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(auth) error = %v", err)
	}
	writeCLIFile(t, filepath.Join(configDir, "sai.yaml"), `default_provider: fake
default_model: default
provider_dir: providers
auth_dir: auth
`)
	writeCLIFile(t, filepath.Join(authDir, "fake.json"), fmt.Sprintf(`{
  "access_token": "codex-access-token",
  "refresh_token": "codex-refresh-token",
  "expires_at": %q,
  "account_id": "account-123"
}
`, time.Now().Add(time.Hour).Format(time.RFC3339Nano)))
	writeCLIFile(t, filepath.Join(providersDir, "fake.yaml"), fmt.Sprintf(`name: fake
base_url: %s
auth_file: ../auth/fake.json

models:
  default:
    id: model-default
    type: openai-codex
    context_window: 400000
`, server.URL))

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Say hi"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "hello codex"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	request := <-requests
	if request.Path != "/responses" {
		t.Fatalf("request path = %q, want /responses", request.Path)
	}
	if request.Authorization != "Bearer codex-access-token" {
		t.Fatalf("Authorization = %q, want Codex bearer token", request.Authorization)
	}
	if request.Body["store"] != false {
		t.Fatalf("store = %#v, want false in Codex request body", request.Body["store"])
	}
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunOpenAIResponsesExecutesFunctionCallAndContinuesToFinalText(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"path\":\"note.txt\"}"}`,
			`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"path\":\"note.txt\"}"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"note.txt\"}"}}`,
			`{"type":"response.completed","response":{"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`,
		},
		[]string{
			`{"type":"response.output_text.delta","delta":"done"}`,
			`{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":2,"total_tokens":13}}}`,
		},
	)
	defer server.Close()

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "note.txt"), "tool output")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "responses-secret-value", "openai-responses")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "read_file", "--prompt", "Read note"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "done"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIToolStatus(t, stdout.String(), stderr.String(), "read_file note.txt", "tool output")

	firstRequest := <-requests
	if firstRequest.Path != "/responses" {
		t.Fatalf("first request path = %q, want /responses", firstRequest.Path)
	}
	assertCLIResponsesToolNames(t, firstRequest.Body, []string{"read_file"})

	secondRequest := <-requests
	if secondRequest.Path != "/responses" {
		t.Fatalf("second request path = %q, want /responses", secondRequest.Path)
	}
	assertCLIResponsesToolNames(t, secondRequest.Body, []string{"read_file"})
	input := requestInput(t, secondRequest.Body)
	if len(input) != 4 {
		t.Fatalf("len(second request input) = %d, want 4: %#v", len(input), input)
	}
	assertMessage(t, input, 0, "system", builtInBaseInstructions)
	assertMessage(t, input, 1, "user", "Read note")
	assertResponseFunctionCallInput(t, input, 2, "call_1", "read_file", `{"path":"note.txt"}`)
	assertResponseFunctionCallOutput(t, input, 3, "call_1", "tool output")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunAnthropicMessagesProviderOutputsTextDelta(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello anthropic"}}`,
		`{"type":"message_stop"}`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "anthropic-secret-value", "anthropic-messages")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Say hi"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "hello anthropic"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	request := <-requests
	if request.Path != "/messages" {
		t.Fatalf("request path = %q, want /messages", request.Path)
	}
	if request.XAPIKey != "anthropic-secret-value" {
		t.Fatalf("x-api-key = %q, want Anthropic API key", request.XAPIKey)
	}
	if request.AnthropicVersion != "2023-06-01" {
		t.Fatalf("anthropic-version = %q, want 2023-06-01", request.AnthropicVersion)
	}
	if request.ContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", request.ContentType)
	}
	if got := request.Body["model"]; got != "model-default" {
		t.Fatalf("model = %#v, want model-default", got)
	}
	if got := request.Body["system"]; got != builtInBaseInstructions {
		t.Fatalf("system = %#v, want built-in instructions", got)
	}
	assertJSONNumber(t, request.Body["temperature"], "0.6")
	assertJSONNumber(t, request.Body["max_tokens"], "128")
	assertCLIRequestOmitsKey(t, request.Body, "type")
	messages := requestMessages(t, request.Body)
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "user", "Say hi")
	assertCLIRequestOmitsKey(t, request.Body, "tools")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunAnthropicMessagesProviderExecutesToolUseAndReturnsToolResult(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_read","name":"read_file","input":{}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"note.txt\"}"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_stop"}`,
		},
		[]string{
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
			`{"type":"message_stop"}`,
		},
	)
	defer server.Close()

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "note.txt"), "tool output")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "anthropic-secret-value", "anthropic-messages")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "read_file", "--prompt", "Read note"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "done"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIToolStatus(t, stdout.String(), stderr.String(), "read_file note.txt", "tool output")

	firstRequest := <-requests
	if firstRequest.Path != "/messages" {
		t.Fatalf("first request path = %q, want /messages", firstRequest.Path)
	}
	assertCLIAnthropicToolNames(t, firstRequest.Body, []string{"read_file"})

	secondRequest := <-requests
	if secondRequest.Path != "/messages" {
		t.Fatalf("second request path = %q, want /messages", secondRequest.Path)
	}
	messages := requestMessages(t, secondRequest.Body)
	if len(messages) != 3 {
		t.Fatalf("len(second request messages) = %d, want 3: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "user", "Read note")
	assertAnthropicAssistantToolUseMessage(t, messages, 1, "call_read", "read_file", "path", "note.txt")
	assertAnthropicToolResultMessage(t, messages, 2, "call_read", "tool output")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunExplicitProviderModelSelectsProfileParameters(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--provider", "fake", "--model", "fast", "--prompt", "Use fast"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	request := <-requests
	if request.Authorization != "Bearer direct-secret-value" {
		t.Fatalf("Authorization = %q, want direct API key", request.Authorization)
	}
	if got := request.Body["model"]; got != "model-fast" {
		t.Fatalf("model = %#v, want model-fast", got)
	}
	assertJSONNumber(t, request.Body["temperature"], "0.2")
	assertJSONNumber(t, request.Body["max_tokens"], "64")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunUsesConfiguredEnabledTools(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithTools(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{"read_file", "write_file", "edit_file", "shell"})

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Use configured tools"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIToolNames(t, (<-requests).Body, []string{"read_file", "write_file", "edit_file", "shell"})
}

func TestRunCanExposeDiscoveryTools(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "glob_files,grep_files", "--prompt", "Use discovery tools"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIToolNames(t, (<-requests).Body, []string{"glob_files", "grep_files"})
}

func TestRunDoesNotExposeSubagentToolsWithoutConfig(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "No helpers configured"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIRequestOmitsKey(t, (<-requests).Body, "tools")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunRejectsExplicitSubagentToolInEnabledTools(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithTools(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{subagents.ToolSubagentStart})

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Bad tool config"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `enabled tool "subagent_start" is a subagent tool`, "auto-enabled", "subagents", "tools.enabled")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunDoesNotExposeEditingToolsWhenOnlyNonEditingToolsAreEnabled(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithTools(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{"read_file", "shell"})

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Do not expose tools"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIToolNames(t, (<-requests).Body, []string{"read_file", "shell"})
}

func TestRunEnableToolsOverridesConfiguredTools(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithTools(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{"read_file", "shell"})

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "list_files,write_file,edit_file", "--prompt", "Use CLI tools"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIToolNames(t, (<-requests).Body, []string{"list_files", "write_file", "edit_file"})
}

func TestRunInjectsConfiguredSystemPromptWithPlaceholders(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	projectDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	appendCLIConfig(t, configDir, `
prompt:
  system_prompt: |
    custom cwd={{cwd}}
    custom config={{config_dir}}
`)
	writeCLIFile(t, filepath.Join(projectDir, "AGENTS.md"), "Project instructions\n")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Use configured prompt"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	messages := requestMessages(t, (<-requests).Body)
	if len(messages) != 4 {
		t.Fatalf("len(messages) = %d, want 4: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "developer", "custom cwd="+projectDir+"\ncustom config="+configDir+"\n")
	assertMessage(t, messages, 2, "developer", "Project instructions\n")
	assertMessage(t, messages, 3, "user", "Use configured prompt")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunFailsOnUnknownConfiguredPromptPlaceholder(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	appendCLIConfig(t, configDir, `
prompt:
  system_prompt: "secret={{env.HOME}}"
`)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Use bad prompt"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "render prompt.system_prompt", `unknown prompt placeholder "env.HOME"`, "supported placeholders")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func cliToolCallChunk(t *testing.T, id, name, arguments string) string {
	t.Helper()
	chunk := map[string]any{
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []any{
						map[string]any{
							"index": 0,
							"id":    id,
							"function": map[string]any{
								"name":      name,
								"arguments": arguments,
							},
						},
					},
				},
			},
		},
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("json.Marshal(tool call chunk) error = %v", err)
	}
	return string(data)
}

func cliTextChunk(t *testing.T, text string) string {
	t.Helper()
	chunk := map[string]any{
		"choices": []any{
			map[string]any{
				"delta": map[string]any{
					"content": text,
				},
			},
		},
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("json.Marshal(text chunk) error = %v", err)
	}
	return string(data)
}

func writeCLISSEChunks(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, chunk := range chunks {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
	}
}

func lastCLIUserMessageContent(t *testing.T, body map[string]any) string {
	t.Helper()
	messages := requestMessages(t, body)
	for i := len(messages) - 1; i >= 0; i-- {
		message, ok := messages[i].(map[string]any)
		if !ok {
			t.Fatalf("message[%d] = %T, want object", i, messages[i])
		}
		if message["role"] != "user" {
			continue
		}
		content, ok := message["content"].(string)
		if !ok {
			t.Fatalf("message[%d].content = %T, want string", i, message["content"])
		}
		return content
	}
	t.Fatalf("missing user message in %#v", messages)
	return ""
}

func assertCLIOutputEventuallyContains(t *testing.T, output interface{ String() string }, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if strings.Contains(output.String(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("output did not contain %q before timeout; got %q", want, output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func receiveString(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for string")
		return ""
	}
}

func writeCLIInput(t *testing.T, w io.Writer, text string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.WriteString(w, text)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write input %q error = %v", text, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out writing input %q", text)
	}
}

func writeCLIChildSubagentConfig(t *testing.T, configDir, baseURL string) {
	t.Helper()
	subagentDir := filepath.Join(configDir, "subagents")
	childProvidersDir := filepath.Join(configDir, "child-providers")
	if err := os.MkdirAll(subagentDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(subagents) error = %v", err)
	}
	if err := os.MkdirAll(childProvidersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(child providers) error = %v", err)
	}
	writeCLIFile(t, filepath.Join(subagentDir, "reviewer.yaml"), `default_provider: fake
default_model: default
provider_dir: ../child-providers
skill_dirs: []

agent:
  description: Reviews scoped changes.
  max_turns: 4

logging:
  path: ../child-logs/sai.jsonl
`)
	writeCLIFile(t, filepath.Join(childProvidersDir, "fake.yaml"), fmt.Sprintf(`name: fake
base_url: %s
api_key: child-secret-value

models:
  default:
    id: child-model
    max_tokens: 64
`, baseURL))
}

func TestRunAcceptsParentInputWhileSubagentRunningThenDeliversCompletion(t *testing.T) {
	childRequestStarted := make(chan struct{})
	releaseChild := make(chan struct{})
	var childRequestStartedOnce sync.Once
	childServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		childRequestStartedOnce.Do(func() { close(childRequestStarted) })
		select {
		case <-releaseChild:
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			t.Errorf("timed out waiting to release child")
			return
		}
		writeCLISSEChunks(w,
			cliTextChunk(t, "child finished after parent follow-up"),
			`[DONE]`,
		)
	}))
	defer childServer.Close()

	parentRequests := make(chan capturedCLIRunRequest, 5)
	runningParentTurn := make(chan string, 1)
	completionEvent := make(chan string, 1)
	var parentMu sync.Mutex
	parentRequestCount := 0
	parentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll(parent) error = %v", err)
		}
		request := capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}
		parentRequests <- request

		parentMu.Lock()
		index := parentRequestCount
		parentRequestCount++
		parentMu.Unlock()

		switch index {
		case 0:
			writeCLISSEChunks(w,
				cliToolCallChunk(t, "call_start", subagents.ToolSubagentStart, `{"agent_id":"reviewer","prompt":"long child task","display_name":"Concurrent Review","job_name":"concurrent-review"}`),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			)
		case 1:
			writeCLISSEChunks(w,
				cliTextChunk(t, "started child"),
				`[DONE]`,
			)
		case 2:
			runningParentTurn <- lastCLIUserMessageContent(t, request.Body)
			writeCLISSEChunks(w,
				cliTextChunk(t, "parent answered while child running"),
				`[DONE]`,
			)
		case 3:
			completionEvent <- lastCLIUserMessageContent(t, request.Body)
			writeCLISSEChunks(w,
				cliTextChunk(t, "parent handled child completion"),
				`[DONE]`,
			)
		default:
			http.Error(w, "unexpected parent request", http.StatusInternalServerError)
		}
	}))
	defer parentServer.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, parentServer.URL+"/v1", "parent-secret-value", "openai-chat")
	appendCLIConfig(t, configDir, `
subagents:
  reviewer: subagents/reviewer.yaml
`)
	writeCLIChildSubagentConfig(t, configDir, childServer.URL+"/v1")

	stdinReader, stdinWriter := io.Pipe()
	defer stdinReader.Close()
	defer stdinWriter.Close()
	stdout := newSignalingWriter("unused")
	stderr := newSignalingWriter(chatInputPrompt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- RunWithContext(ctx, []string{"--config", cliConfigPath(configDir), "chat", "--prompt", "delegate"}, stdinReader, stdout, stderr, func() (string, error) {
			return t.TempDir(), nil
		})
	}()

	assertCLIOutputEventuallyContains(t, stdout, "started child\n")
	select {
	case <-childRequestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("child HTTP request did not start")
	}

	writeCLIInput(t, stdinWriter, "parent follow-up while child runs\n")
	assertCLIOutputEventuallyContains(t, stdout, "parent answered while child running")
	if got := receiveString(t, runningParentTurn); got != "parent follow-up while child runs" {
		t.Fatalf("running parent prompt = %q, want follow-up prompt", got)
	}

	close(releaseChild)
	assertCLIOutputEventuallyContains(t, stdout, "parent handled child completion")
	writeCLIInput(t, stdinWriter, "/exit\n")

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("RunWithContext() code = %d, stderr = %s", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("RunWithContext did not exit")
	}

	receiveCLIRunRequest(t, parentRequests)
	receiveCLIRunRequest(t, parentRequests)
	receiveCLIRunRequest(t, parentRequests)
	receiveCLIRunRequest(t, parentRequests)
	assertNoAdditionalCLIRunRequest(t, parentRequests)

	event := receiveString(t, completionEvent)
	for _, want := range []string{
		"Runtime event: subagent job completed",
		"agent_id: reviewer",
		"display_name: Concurrent Review",
		"job_name: concurrent-review",
		"status: completed",
		"output: child finished after parent follow-up",
	} {
		if !strings.Contains(event, want) {
			t.Fatalf("completion event = %q, want substring %q", event, want)
		}
	}
}

func TestRunInjectsConfiguredSubagentListFromChildMetadata(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	appendCLIConfig(t, configDir, `
subagents:
  reviewer: subagents/reviewer.yaml
  researcher: subagents/researcher.yaml
`)
	subagentDir := filepath.Join(configDir, "subagents")
	if err := os.MkdirAll(subagentDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeCLIFile(t, filepath.Join(subagentDir, "reviewer.yaml"), `provider_dir: missing-child-providers
agent:
  description: Reviews scoped changes.
prompt:
  system_prompt: Child prompt is not parent metadata.
`)
	writeCLIFile(t, filepath.Join(subagentDir, "researcher.yaml"), `provider_dir: missing-child-providers
agent:
  description: Looks up facts.
`)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Choose helper"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}

	request := <-requests
	messages := requestMessages(t, request.Body)
	if len(messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "developer", "Configured subagents:\n- researcher: Looks up facts.\n- reviewer: Reviews scoped changes.")
	assertMessage(t, messages, 2, "user", "Choose helper")
	assertCLIToolNames(t, request.Body, []string{
		subagents.ToolSubagentStart,
		subagents.ToolSubagentSend,
		subagents.ToolSubagentStatus,
		subagents.ToolSubagentWait,
		subagents.ToolSubagentCancel,
		subagents.ToolSubagentClose,
	})
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunChildSelfReferentialSubagentConfigDoesNotExposeNestedSubagentRuntime(t *testing.T) {
	childRequests := make(chan capturedCLIRunRequest, 1)
	childServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll(child) error = %v", err)
		}
		childRequests <- capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}
		writeCLISSEChunks(w,
			cliTextChunk(t, "child done"),
			`[DONE]`,
		)
	}))
	defer childServer.Close()

	parentRequests := make(chan capturedCLIRunRequest, 3)
	var parentMu sync.Mutex
	parentRequestCount := 0
	parentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll(parent) error = %v", err)
		}
		parentRequests <- capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}

		parentMu.Lock()
		index := parentRequestCount
		parentRequestCount++
		parentMu.Unlock()

		switch index {
		case 0:
			writeCLISSEChunks(w,
				cliToolCallChunk(t, "call_start", subagents.ToolSubagentStart, `{"agent_id":"reviewer","prompt":"child task"}`),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			)
		case 1:
			writeCLISSEChunks(w,
				cliTextChunk(t, "parent saw start"),
				`[DONE]`,
			)
		case 2:
			writeCLISSEChunks(w,
				cliTextChunk(t, "parent handled completion"),
				`[DONE]`,
			)
		default:
			http.Error(w, "unexpected parent request", http.StatusInternalServerError)
		}
	}))
	defer parentServer.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, parentServer.URL+"/v1", "parent-secret-value", "openai-chat")
	appendCLIConfig(t, configDir, `
subagents:
  reviewer: subagents/reviewer.yaml
`)
	subagentDir := filepath.Join(configDir, "subagents")
	childProvidersDir := filepath.Join(configDir, "child-providers")
	if err := os.MkdirAll(subagentDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(subagents) error = %v", err)
	}
	if err := os.MkdirAll(childProvidersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(child providers) error = %v", err)
	}
	writeCLIFile(t, filepath.Join(subagentDir, "reviewer.yaml"), `default_provider: fake
default_model: default
provider_dir: ../child-providers
skill_dirs: []

agent:
  description: Reviews scoped changes.
  max_turns: 4

tools:
  enabled:
    - list_files

subagents:
  reviewer: reviewer.yaml

logging:
  path: ../child-logs/sai.jsonl
`)
	writeCLIFile(t, filepath.Join(childProvidersDir, "fake.yaml"), fmt.Sprintf(`name: fake
base_url: %s
api_key: child-secret-value

models:
  default:
    id: child-model
    max_tokens: 64
`, childServer.URL+"/v1"))

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "delegate"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}

	firstParent := receiveCLIRunRequest(t, parentRequests)
	assertCLIToolNames(t, firstParent.Body, []string{
		subagents.ToolSubagentStart,
		subagents.ToolSubagentSend,
		subagents.ToolSubagentStatus,
		subagents.ToolSubagentWait,
		subagents.ToolSubagentCancel,
		subagents.ToolSubagentClose,
	})

	child := receiveCLIRunRequest(t, childRequests)
	assertCLIToolNames(t, child.Body, []string{"list_files"})
	for i, raw := range requestMessages(t, child.Body) {
		message, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("child message[%d] = %T, want object", i, raw)
		}
		content, _ := message["content"].(string)
		if strings.Contains(content, "Configured subagents:") {
			t.Fatalf("child message[%d] includes nested subagent prompt: %q", i, content)
		}
	}
	assertNoAdditionalCLIRunRequest(t, childRequests)
}

func TestRunChildUsesChildSkillsAndMCPWithoutInheritingParentCapabilities(t *testing.T) {
	childRequests := make(chan capturedCLIRunRequest, 1)
	childServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll(child) error = %v", err)
		}
		childRequests <- capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}
		writeCLISSEChunks(w,
			cliTextChunk(t, "child done"),
			`[DONE]`,
		)
	}))
	defer childServer.Close()

	parentRequests := make(chan capturedCLIRunRequest, 3)
	var parentMu sync.Mutex
	parentRequestCount := 0
	parentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll(parent) error = %v", err)
		}
		parentRequests <- capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}

		parentMu.Lock()
		index := parentRequestCount
		parentRequestCount++
		parentMu.Unlock()

		switch index {
		case 0:
			writeCLISSEChunks(w,
				cliToolCallChunk(t, "call_start", subagents.ToolSubagentStart, `{"agent_id":"reviewer","prompt":"child task"}`),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			)
		case 1:
			writeCLISSEChunks(w,
				cliTextChunk(t, "parent saw start"),
				`[DONE]`,
			)
		case 2:
			writeCLISSEChunks(w,
				cliTextChunk(t, "parent handled completion"),
				`[DONE]`,
			)
		default:
			http.Error(w, "unexpected parent request", http.StatusInternalServerError)
		}
	}))
	defer parentServer.Close()

	configDir := t.TempDir()
	parentMCPExitFile := filepath.Join(t.TempDir(), "parent-mcp-exited")
	childMCPExitFile := filepath.Join(t.TempDir(), "child-mcp-exited")
	writeCLIRunFixtureInDirWithTools(t, configDir, parentServer.URL+"/v1", "parent-secret-value", "openai-chat", []string{"mcp.remote.search"})
	setCLISkillDirs(t, configDir, []string{"skills"})
	writeCLISkill(t, configDir, "parent-only", "---\nname: Parent Skill\n---\nParent skill instructions.\n")
	appendCLIConfig(t, configDir, `
mcp_dir: parent-mcp
subagents:
  reviewer: subagents/reviewer.yaml
`)
	writeCLIRunMCPServerFixture(t, filepath.Join(configDir, "parent-mcp"), "remote", parentMCPExitFile, true)

	subagentDir := filepath.Join(configDir, "subagents")
	childProvidersDir := filepath.Join(configDir, "child-providers")
	if err := os.MkdirAll(subagentDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(subagents) error = %v", err)
	}
	if err := os.MkdirAll(childProvidersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(child providers) error = %v", err)
	}
	writeCLIFile(t, filepath.Join(subagentDir, "reviewer.yaml"), `default_provider: fake
default_model: default
provider_dir: ../child-providers
skill_dirs:
  - ../child-skills
mcp_dir: ../child-mcp

agent:
  description: Reviews scoped changes.
  max_turns: 4

tools:
  enabled:
    - mcp.local.search

logging:
  path: ../child-logs/sai.jsonl
`)
	writeCLIFile(t, filepath.Join(childProvidersDir, "fake.yaml"), fmt.Sprintf(`name: fake
base_url: %s
api_key: child-secret-value

models:
  default:
    id: child-model
    max_tokens: 64
`, childServer.URL+"/v1"))
	writeCLISkillInRoot(t, filepath.Join(configDir, "child-skills"), "child-only", "---\nname: Child Skill\n---\nChild skill instructions.\n")
	writeCLIRunMCPServerFixture(t, filepath.Join(configDir, "child-mcp"), "local", childMCPExitFile, true)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "delegate"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}

	firstParent := receiveCLIRunRequest(t, parentRequests)
	assertCLIToolNames(t, firstParent.Body, []string{
		"mcp.remote.search",
		subagents.ToolSubagentStart,
		subagents.ToolSubagentSend,
		subagents.ToolSubagentStatus,
		subagents.ToolSubagentWait,
		subagents.ToolSubagentCancel,
		subagents.ToolSubagentClose,
	})
	parentMessageText := string(firstParent.RawBody)
	if !strings.Contains(parentMessageText, "Parent skill instructions.") {
		t.Fatalf("parent request missing parent skill instructions: %s", firstParent.RawBody)
	}

	child := receiveCLIRunRequest(t, childRequests)
	assertCLIToolNames(t, child.Body, []string{"mcp.local.search"})
	childMessageText := string(child.RawBody)
	for _, want := range []string{"Child skill instructions.", "Skill child-only (Child Skill):"} {
		if !strings.Contains(childMessageText, want) {
			t.Fatalf("child request missing %q: %s", want, child.RawBody)
		}
	}
	for _, forbidden := range []string{"Parent skill instructions.", "mcp.remote.search"} {
		if strings.Contains(childMessageText, forbidden) {
			t.Fatalf("child request inherited parent-only capability %q: %s", forbidden, child.RawBody)
		}
	}
	assertNoAdditionalCLIRunRequest(t, childRequests)
	assertCLIFileEventuallyContains(t, childMCPExitFile, "closed")
}

func TestRunDeliversSubagentCompletionAfterParentTurnWithoutWait(t *testing.T) {
	childRequests := make(chan capturedCLIRunRequest, 1)
	childDone := make(chan struct{})
	var childDoneOnce sync.Once
	childServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll(child) error = %v", err)
		}
		childRequests <- capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}
		writeCLISSEChunks(w,
			cliTextChunk(t, "child complete"),
			`[DONE]`,
		)
		childDoneOnce.Do(func() { close(childDone) })
	}))
	defer childServer.Close()

	parentRequests := make(chan capturedCLIRunRequest, 4)
	completionEvent := make(chan string, 1)
	var parentMu sync.Mutex
	parentRequestCount := 0
	parentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll(parent) error = %v", err)
		}
		request := capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}
		parentRequests <- request

		parentMu.Lock()
		index := parentRequestCount
		parentRequestCount++
		parentMu.Unlock()

		switch index {
		case 0:
			writeCLISSEChunks(w,
				cliToolCallChunk(t, "call_start", subagents.ToolSubagentStart, `{"agent_id":"reviewer","prompt":"child task","display_name":"Review UI","job_name":"review-1"}`),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			)
		case 1:
			select {
			case <-childDone:
			case <-time.After(2 * time.Second):
				t.Errorf("timed out waiting for child completion")
			}
			writeCLISSEChunks(w,
				cliTextChunk(t, "parent saw start"),
				`[DONE]`,
			)
		case 2:
			completionEvent <- lastCLIUserMessageContent(t, request.Body)
			writeCLISSEChunks(w,
				cliTextChunk(t, "parent handled child"),
				`[DONE]`,
			)
		default:
			http.Error(w, "unexpected parent request", http.StatusInternalServerError)
		}
	}))
	defer parentServer.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, parentServer.URL+"/v1", "parent-secret-value", "openai-chat")
	appendCLIConfig(t, configDir, `
subagents:
  reviewer: subagents/reviewer.yaml
`)
	writeCLIChildSubagentConfig(t, configDir, childServer.URL+"/v1")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "delegate"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "parent saw start") || !strings.Contains(got, "parent handled child") {
		t.Fatalf("stdout = %q, want parent turn and completion turn output", got)
	}

	receiveCLIRunRequest(t, parentRequests)
	receiveCLIRunRequest(t, parentRequests)
	receiveCLIRunRequest(t, parentRequests)
	assertNoAdditionalCLIRunRequest(t, parentRequests)
	receiveCLIRunRequest(t, childRequests)
	assertNoAdditionalCLIRunRequest(t, childRequests)

	event := receiveString(t, completionEvent)
	for _, want := range []string{
		"Runtime event: subagent job completed",
		"agent_id: reviewer",
		"display_name: Review UI",
		"job_name: review-1",
		"status: completed",
		"output: child complete",
	} {
		if !strings.Contains(event, want) {
			t.Fatalf("completion event = %q, want substring %q", event, want)
		}
	}
	if strings.Contains(strings.ToLower(event), "tool result") {
		t.Fatalf("completion event should not claim tool result: %q", event)
	}

	logPaths := sessionLogPaths(t, configDir)
	if len(logPaths) != 1 {
		t.Fatalf("session log paths = %#v, want one parent log", logPaths)
	}
	logData, err := os.ReadFile(logPaths[0])
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", logPaths[0], err)
	}
	logText := string(logData)
	if strings.Contains(logText, "child complete") {
		t.Fatalf("log leaked child output: %s", logText)
	}
	records := readJSONLRecords(t, logData)
	assertCLILogBaseFields(t, records)
	completionRecord := firstCLILogRecord(t, records, "subagent_completion")
	for key, want := range map[string]any{
		"agent_id":     "reviewer",
		"display_name": "Review UI",
		"job_name":     "review-1",
		"status":       "completed",
	} {
		if got := completionRecord[key]; got != want {
			t.Fatalf("subagent_completion[%q] = %#v, want %#v in %#v", key, got, want, completionRecord)
		}
	}
	if got, ok := completionRecord["job_id"].(string); !ok || got == "" {
		t.Fatalf("subagent_completion job_id = %#v, want non-empty string in %#v", completionRecord["job_id"], completionRecord)
	}
}

func TestRunIdleAutoWakesParentForSubagentCompletion(t *testing.T) {
	releaseChild := make(chan struct{})
	childServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-releaseChild:
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			t.Errorf("timed out waiting to release child")
			return
		}
		writeCLISSEChunks(w,
			cliTextChunk(t, "idle child done"),
			`[DONE]`,
		)
	}))
	defer childServer.Close()

	parentRequests := make(chan capturedCLIRunRequest, 4)
	completionEvent := make(chan string, 1)
	var parentMu sync.Mutex
	parentRequestCount := 0
	parentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll(parent) error = %v", err)
		}
		request := capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}
		parentRequests <- request

		parentMu.Lock()
		index := parentRequestCount
		parentRequestCount++
		parentMu.Unlock()

		switch index {
		case 0:
			writeCLISSEChunks(w,
				cliToolCallChunk(t, "call_start", subagents.ToolSubagentStart, `{"agent_id":"reviewer","prompt":"idle child","display_name":"Idle Review"}`),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			)
		case 1:
			writeCLISSEChunks(w,
				cliTextChunk(t, "started"),
				`[DONE]`,
			)
		case 2:
			completionEvent <- lastCLIUserMessageContent(t, request.Body)
			writeCLISSEChunks(w,
				cliTextChunk(t, "handled idle"),
				`[DONE]`,
			)
		default:
			http.Error(w, "unexpected parent request", http.StatusInternalServerError)
		}
	}))
	defer parentServer.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, parentServer.URL+"/v1", "parent-secret-value", "openai-chat")
	appendCLIConfig(t, configDir, `
subagents:
  reviewer: subagents/reviewer.yaml
`)
	writeCLIChildSubagentConfig(t, configDir, childServer.URL+"/v1")

	stdinReader, stdinWriter := io.Pipe()
	defer stdinReader.Close()
	defer stdinWriter.Close()
	stdout := newSignalingWriter("unused")
	stderr := newSignalingWriter(chatInputPrompt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- RunWithContext(ctx, []string{"--config", cliConfigPath(configDir), "chat", "--prompt", "delegate"}, stdinReader, stdout, stderr, func() (string, error) {
			return t.TempDir(), nil
		})
	}()

	assertCLIOutputEventuallyContains(t, stdout, "started\n")
	select {
	case <-stderr.wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("chat prompt did not render before idle completion")
	}
	close(releaseChild)
	assertCLIOutputEventuallyContains(t, stdout, "handled idle")
	if _, err := stdinWriter.Write([]byte("/exit\n")); err != nil {
		t.Fatalf("write /exit error = %v", err)
	}

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("RunWithContext() code = %d, stderr = %s", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("RunWithContext did not exit")
	}

	receiveCLIRunRequest(t, parentRequests)
	receiveCLIRunRequest(t, parentRequests)
	receiveCLIRunRequest(t, parentRequests)
	assertNoAdditionalCLIRunRequest(t, parentRequests)

	event := receiveString(t, completionEvent)
	for _, want := range []string{
		"Runtime event: subagent job completed",
		"display_name: Idle Review",
		"output: idle child done",
	} {
		if !strings.Contains(event, want) {
			t.Fatalf("completion event = %q, want substring %q", event, want)
		}
	}
	if got := stderr.String(); !strings.Contains(got, chatInputPrompt+"\n"+chatInputPrompt) {
		t.Fatalf("stderr = %q, want idle completion to separate and redraw prompt", got)
	}
}

func TestRunRequeuesSubagentCompletionAfterRecoverableCompletionTurnError(t *testing.T) {
	childRequestStarted := make(chan struct{})
	releaseChild := make(chan struct{})
	var childRequestStartedOnce sync.Once
	childServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		childRequestStartedOnce.Do(func() { close(childRequestStarted) })
		select {
		case <-releaseChild:
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			t.Errorf("timed out waiting to release child")
			return
		}
		writeCLISSEChunks(w,
			cliTextChunk(t, "child output survives failure"),
			`[DONE]`,
		)
	}))
	defer childServer.Close()

	parentRequests := make(chan capturedCLIRunRequest, 5)
	firstCompletionEvent := make(chan string, 1)
	retryParentTurn := make(chan string, 1)
	secondCompletionEvent := make(chan string, 1)
	var parentMu sync.Mutex
	parentRequestCount := 0
	parentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll(parent) error = %v", err)
		}
		request := capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}
		parentRequests <- request

		parentMu.Lock()
		index := parentRequestCount
		parentRequestCount++
		parentMu.Unlock()

		switch index {
		case 0:
			writeCLISSEChunks(w,
				cliToolCallChunk(t, "call_start", subagents.ToolSubagentStart, `{"agent_id":"reviewer","prompt":"retry child","display_name":"Retry Review","job_name":"retry-review"}`),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			)
		case 1:
			writeCLISSEChunks(w,
				cliTextChunk(t, "started retry child"),
				`[DONE]`,
			)
		case 2:
			firstCompletionEvent <- lastCLIUserMessageContent(t, request.Body)
			http.Error(w, "temporary completion turn failure", http.StatusBadRequest)
		case 3:
			retryParentTurn <- lastCLIUserMessageContent(t, request.Body)
			writeCLISSEChunks(w,
				cliTextChunk(t, "parent turn after failure"),
				`[DONE]`,
			)
		case 4:
			secondCompletionEvent <- lastCLIUserMessageContent(t, request.Body)
			writeCLISSEChunks(w,
				cliTextChunk(t, "completion handled after failure"),
				`[DONE]`,
			)
		default:
			http.Error(w, "unexpected parent request", http.StatusInternalServerError)
		}
	}))
	defer parentServer.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, parentServer.URL+"/v1", "parent-secret-value", "openai-chat")
	appendCLIConfig(t, configDir, `
subagents:
  reviewer: subagents/reviewer.yaml
`)
	writeCLIChildSubagentConfig(t, configDir, childServer.URL+"/v1")

	stdinReader, stdinWriter := io.Pipe()
	defer stdinReader.Close()
	defer stdinWriter.Close()
	stdout := newSignalingWriter("unused")
	var stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- RunWithContext(ctx, []string{"--config", cliConfigPath(configDir), "chat", "--prompt", "delegate"}, stdinReader, stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})
	}()

	assertCLIOutputEventuallyContains(t, stdout, "started retry child\n")
	select {
	case <-childRequestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("child HTTP request did not start")
	}
	close(releaseChild)

	firstEvent := receiveString(t, firstCompletionEvent)
	if !strings.Contains(firstEvent, "display_name: Retry Review") || !strings.Contains(firstEvent, "job_name: retry-review") {
		t.Fatalf("first completion event = %q, want job metadata", firstEvent)
	}

	writeCLIInput(t, stdinWriter, "parent retry trigger\n")
	assertCLIOutputEventuallyContains(t, stdout, "parent turn after failure")
	if got := receiveString(t, retryParentTurn); got != "parent retry trigger" {
		t.Fatalf("retry parent prompt = %q, want user retry trigger", got)
	}
	assertCLIOutputEventuallyContains(t, stdout, "completion handled after failure")
	writeCLIInput(t, stdinWriter, "/exit\n")

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("RunWithContext() code = %d, stderr = %s", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("RunWithContext did not exit")
	}

	receiveCLIRunRequest(t, parentRequests)
	receiveCLIRunRequest(t, parentRequests)
	receiveCLIRunRequest(t, parentRequests)
	receiveCLIRunRequest(t, parentRequests)
	receiveCLIRunRequest(t, parentRequests)
	assertNoAdditionalCLIRunRequest(t, parentRequests)

	secondEvent := receiveString(t, secondCompletionEvent)
	for _, want := range []string{
		"Runtime event: subagent job completed",
		"agent_id: reviewer",
		"display_name: Retry Review",
		"job_name: retry-review",
		"status: completed",
		"output: child output survives failure",
	} {
		if !strings.Contains(secondEvent, want) {
			t.Fatalf("second completion event = %q, want substring %q", secondEvent, want)
		}
	}
	if firstEvent != secondEvent {
		t.Fatalf("second completion event = %q, want same pending event as first %q", secondEvent, firstEvent)
	}

	logPaths := sessionLogPaths(t, configDir)
	if len(logPaths) != 1 {
		t.Fatalf("session log paths = %#v, want one parent log", logPaths)
	}
	logData, err := os.ReadFile(logPaths[0])
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", logPaths[0], err)
	}
	if strings.Contains(string(logData), "child output survives failure") {
		t.Fatalf("log leaked child output: %s", logData)
	}
	records := readJSONLRecords(t, logData)
	completionRecords := 0
	for _, record := range records {
		if record["event"] == "subagent_completion" {
			completionRecords++
			for key, want := range map[string]any{
				"agent_id":     "reviewer",
				"display_name": "Retry Review",
				"job_name":     "retry-review",
				"status":       "completed",
			} {
				if got := record[key]; got != want {
					t.Fatalf("subagent_completion[%q] = %#v, want %#v in %#v", key, got, want, record)
				}
			}
		}
	}
	if completionRecords != 1 {
		t.Fatalf("subagent_completion records = %d, want 1 in %#v", completionRecords, records)
	}
}

func TestRunDoesNotAutoResumeWhenNoSubagentCompletions(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--prompt", "hello"}, strings.NewReader("/exit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	request := receiveCLIRunRequest(t, requests)
	assertMessage(t, requestMessages(t, request.Body), 1, "user", "hello")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunRoutesSubagentStartSendWaitAndChildUsesOwnRuntime(t *testing.T) {
	toolCallChunk := func(id, name, arguments string) string {
		t.Helper()
		chunk := map[string]any{
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"index": 0,
								"id":    id,
								"function": map[string]any{
									"name":      name,
									"arguments": arguments,
								},
							},
						},
					},
				},
			},
		}
		data, err := json.Marshal(chunk)
		if err != nil {
			t.Fatalf("json.Marshal(tool call chunk) error = %v", err)
		}
		return string(data)
	}
	textChunk := func(text string) string {
		t.Helper()
		chunk := map[string]any{
			"choices": []any{
				map[string]any{
					"delta": map[string]any{
						"content": text,
					},
				},
			},
		}
		data, err := json.Marshal(chunk)
		if err != nil {
			t.Fatalf("json.Marshal(text chunk) error = %v", err)
		}
		return string(data)
	}
	writeChunks := func(w http.ResponseWriter, chunks ...string) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
	}

	releaseFirstChild := make(chan struct{})
	var releaseFirstChildOnce sync.Once
	childRequests := make(chan capturedCLIRunRequest, 2)
	var childMu sync.Mutex
	childRequestCount := 0
	childServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		childRequests <- capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}

		childMu.Lock()
		index := childRequestCount
		childRequestCount++
		childMu.Unlock()

		switch index {
		case 0:
			select {
			case <-releaseFirstChild:
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Second):
				t.Errorf("timed out waiting for subagent_send before first child response")
				return
			}
			writeChunks(w,
				textChunk("initial child output"),
				`[DONE]`,
			)
		case 1:
			writeChunks(w,
				textChunk("child final after send"),
				`[DONE]`,
			)
		default:
			http.Error(w, "unexpected child request", http.StatusInternalServerError)
		}
	}))
	defer childServer.Close()

	toolResultContent := func(body map[string]any, callID string) string {
		t.Helper()
		for _, raw := range requestMessages(t, body) {
			message, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("message = %T, want object", raw)
			}
			if message["role"] == "tool" && message["tool_call_id"] == callID {
				content, ok := message["content"].(string)
				if !ok {
					t.Fatalf("tool message content = %T, want string", message["content"])
				}
				return content
			}
		}
		t.Fatalf("missing tool result for call %q in %#v", callID, body)
		return ""
	}
	decodeSubagentSnapshot := func(content string) struct {
		OK            bool   `json:"ok"`
		JobID         string `json:"job_id"`
		AgentID       string `json:"agent_id"`
		DisplayName   string `json:"display_name"`
		JobName       string `json:"job_name"`
		Status        string `json:"status"`
		Output        string `json:"output"`
		MessageQueued bool   `json:"message_queued"`
	} {
		t.Helper()
		var snapshot struct {
			OK            bool   `json:"ok"`
			JobID         string `json:"job_id"`
			AgentID       string `json:"agent_id"`
			DisplayName   string `json:"display_name"`
			JobName       string `json:"job_name"`
			Status        string `json:"status"`
			Output        string `json:"output"`
			MessageQueued bool   `json:"message_queued"`
		}
		if err := json.Unmarshal([]byte(content), &snapshot); err != nil {
			t.Fatalf("json.Unmarshal(%q) error = %v", content, err)
		}
		return snapshot
	}

	parentRequests := make(chan capturedCLIRunRequest, 4)
	var parentMu sync.Mutex
	parentRequestCount := 0
	parentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		request := capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}
		parentRequests <- request

		parentMu.Lock()
		index := parentRequestCount
		parentRequestCount++
		parentMu.Unlock()

		switch index {
		case 0:
			writeChunks(w,
				toolCallChunk("call_start", subagents.ToolSubagentStart, `{"agent_id":"reviewer","prompt":"child task","display_name":"Review UI","job_name":"review-1"}`),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			)
		case 1:
			start := decodeSubagentSnapshot(toolResultContent(request.Body, "call_start"))
			if !start.OK || start.JobID == "" || start.AgentID != "reviewer" || start.DisplayName != "Review UI" || start.JobName != "review-1" || start.Status == "" {
				t.Errorf("start snapshot = %#v, want job metadata with display name", start)
			}
			sendArgs, err := json.Marshal(map[string]any{
				"job_id":  start.JobID,
				"message": "follow-up from parent",
			})
			if err != nil {
				t.Errorf("json.Marshal(send args) error = %v", err)
			}
			writeChunks(w,
				toolCallChunk("call_send", subagents.ToolSubagentSend, string(sendArgs)),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			)
		case 2:
			sent := decodeSubagentSnapshot(toolResultContent(request.Body, "call_send"))
			if !sent.MessageQueued || sent.Status != "running" || sent.DisplayName != "Review UI" || sent.JobName != "review-1" {
				t.Errorf("send snapshot = %#v, want running queued message with metadata", sent)
			}
			releaseFirstChildOnce.Do(func() {
				close(releaseFirstChild)
			})
			waitArgs, err := json.Marshal(map[string]any{
				"job_id":     sent.JobID,
				"timeout_ms": 5000,
			})
			if err != nil {
				t.Errorf("json.Marshal(wait args) error = %v", err)
			}
			writeChunks(w,
				toolCallChunk("call_wait", subagents.ToolSubagentWait, string(waitArgs)),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			)
		case 3:
			waited := decodeSubagentSnapshot(toolResultContent(request.Body, "call_wait"))
			if waited.Status != "completed" || waited.Output != "child final after send" || waited.DisplayName != "Review UI" || waited.JobName != "review-1" {
				t.Errorf("wait snapshot = %#v, want completed second-turn child output with metadata", waited)
			}
			writeChunks(w,
				textChunk("parent done"),
				`[DONE]`,
			)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer parentServer.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithTools(t, configDir, parentServer.URL+"/v1", "parent-secret-value", "openai-chat", []string{"read_file"})
	appendCLIConfig(t, configDir, `
subagents:
  reviewer: subagents/reviewer.yaml
`)
	subagentDir := filepath.Join(configDir, "subagents")
	childProvidersDir := filepath.Join(configDir, "child-providers")
	if err := os.MkdirAll(subagentDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(subagents) error = %v", err)
	}
	if err := os.MkdirAll(childProvidersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(child providers) error = %v", err)
	}
	writeCLIFile(t, filepath.Join(subagentDir, "reviewer.yaml"), `default_provider: fake
default_model: default
provider_dir: ../child-providers
skill_dirs: []

agent:
  description: Reviews scoped changes.
  max_turns: 4

tools:
  enabled:
    - list_files

prompt:
  system_prompt: Child configured prompt.

logging:
  path: ../child-logs/sai.jsonl
`)
	writeCLIFile(t, filepath.Join(childProvidersDir, "fake.yaml"), fmt.Sprintf(`name: fake
base_url: %s
api_key: child-secret-value

models:
  default:
    id: child-model
    max_tokens: 64
`, childServer.URL+"/v1"))

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "delegate"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "parent done"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if strings.Contains(stderr.String(), "initial child output") || strings.Contains(stderr.String(), "child final after send") {
		t.Fatalf("stderr leaked child output: %s", stderr.String())
	}

	firstParent := <-parentRequests
	assertCLIToolNames(t, firstParent.Body, []string{
		"read_file",
		subagents.ToolSubagentStart,
		subagents.ToolSubagentSend,
		subagents.ToolSubagentStatus,
		subagents.ToolSubagentWait,
		subagents.ToolSubagentCancel,
		subagents.ToolSubagentClose,
	})
	secondParent := <-parentRequests
	startContent := toolResultContent(secondParent.Body, "call_start")
	if !strings.Contains(startContent, `"display_name":"Review UI"`) || !strings.Contains(startContent, `"job_name":"review-1"`) {
		t.Fatalf("start tool result = %s, want display_name and job_name", startContent)
	}
	thirdParent := <-parentRequests
	sendContent := toolResultContent(thirdParent.Body, "call_send")
	if !strings.Contains(sendContent, `"message_queued":true`) || !strings.Contains(sendContent, `"display_name":"Review UI"`) {
		t.Fatalf("send tool result = %s, want message_queued and display_name", sendContent)
	}
	fourthParent := <-parentRequests
	waitContent := toolResultContent(fourthParent.Body, "call_wait")
	if !strings.Contains(waitContent, `"output":"child final after send"`) || !strings.Contains(waitContent, `"display_name":"Review UI"`) {
		t.Fatalf("wait tool result = %s, want child output and display_name", waitContent)
	}
	assertNoAdditionalCLIRunRequest(t, parentRequests)

	firstChild := <-childRequests
	if firstChild.Authorization != "Bearer child-secret-value" {
		t.Fatalf("first child Authorization = %q, want child API key", firstChild.Authorization)
	}
	if got := firstChild.Body["model"]; got != "child-model" {
		t.Fatalf("first child model = %#v, want child-model", got)
	}
	childMessages := requestMessages(t, firstChild.Body)
	if len(childMessages) != 3 {
		t.Fatalf("len(child messages) = %d, want 3: %#v", len(childMessages), childMessages)
	}
	assertMessage(t, childMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, childMessages, 1, "developer", "Child configured prompt.")
	assertMessage(t, childMessages, 2, "user", "child task")
	assertCLIToolNames(t, firstChild.Body, []string{"list_files"})

	secondChild := <-childRequests
	if secondChild.Authorization != "Bearer child-secret-value" {
		t.Fatalf("second child Authorization = %q, want child API key", secondChild.Authorization)
	}
	if got := secondChild.Body["model"]; got != "child-model" {
		t.Fatalf("second child model = %#v, want child-model", got)
	}
	secondChildMessages := requestMessages(t, secondChild.Body)
	if len(secondChildMessages) != 5 {
		t.Fatalf("len(second child messages) = %d, want 5: %#v", len(secondChildMessages), secondChildMessages)
	}
	assertMessage(t, secondChildMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, secondChildMessages, 1, "developer", "Child configured prompt.")
	assertMessage(t, secondChildMessages, 2, "user", "child task")
	assertMessage(t, secondChildMessages, 3, "assistant", "initial child output")
	assertMessage(t, secondChildMessages, 4, "user", "follow-up from parent")
	assertCLIToolNames(t, secondChild.Body, []string{"list_files"})
	assertNoAdditionalCLIRunRequest(t, childRequests)
}

func TestRunInjectsDiscoveredSkillsInDirectoryOrder(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLISkillDirs(t, configDir, []string{"skills", "team-skills"})
	alphaInstructions := "Alpha instructions.\n"
	betaInstructions := "Beta instructions.\n"
	zetaInstructions := "Zeta instructions.\n"
	writeCLISkill(t, configDir, "alpha", "---\nname: Alpha Skill\ndescription: alpha desc\n---\n"+alphaInstructions)
	writeCLISkill(t, configDir, "zeta", "---\nname: Zeta Skill\ndescription: zeta desc\n---\n"+zetaInstructions)
	writeCLISkillInRoot(t, filepath.Join(configDir, "team-skills"), "beta", "---\nname: Beta Skill\ndescription: beta desc\n---\n"+betaInstructions)
	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "AGENTS.md"), "Project instructions\n")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Use discovered skills"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	messages := requestMessages(t, (<-requests).Body)
	if len(messages) != 6 {
		t.Fatalf("len(messages) = %d, want 6: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "developer", "Project instructions\n")
	assertMessage(t, messages, 2, "developer", "Skill alpha (Alpha Skill):\n"+alphaInstructions)
	assertMessage(t, messages, 3, "developer", "Skill zeta (Zeta Skill):\n"+zetaInstructions)
	assertMessage(t, messages, 4, "developer", "Skill beta (Beta Skill):\n"+betaInstructions)
	assertMessage(t, messages, 5, "user", "Use discovered skills")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunInjectsConfiguredProjectInstructionFilesBeforeSkillsInOrder(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLIInstructionFiles(t, configDir, []string{"$CONFIG/project-first.md", "$CWD/local-second.md"})
	writeCLIFile(t, filepath.Join(configDir, "project-first.md"), "First project instructions\n")
	writeCLISkill(t, configDir, "alpha", "---\nname: Alpha Skill\n---\nSkill instructions\n")

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "local-second.md"), "Second project instructions\n")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--prompt", "Use project instructions"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	messages := requestMessages(t, (<-requests).Body)
	if len(messages) != 5 {
		t.Fatalf("len(messages) = %d, want 5: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "developer", "First project instructions\n")
	assertMessage(t, messages, 2, "developer", "Second project instructions\n")
	assertMessage(t, messages, 3, "developer", "Skill alpha (Alpha Skill):\nSkill instructions\n")
	assertMessage(t, messages, 4, "user", "Use project instructions")
	assertNoAdditionalCLIRunRequest(t, requests)

	session := loadOnlyCLISession(t, filepath.Join(configDir, "sessions"))
	if len(session.InstructionsSnapshot) != 4 {
		t.Fatalf("len(InstructionsSnapshot) = %d, want 4: %#v", len(session.InstructionsSnapshot), session.InstructionsSnapshot)
	}
	assertSavedMessage(t, session.InstructionsSnapshot, 0, model.MessageRoleSystem, builtInBaseInstructions)
	assertSavedMessage(t, session.InstructionsSnapshot, 1, model.MessageRoleDeveloper, "First project instructions\n")
	assertSavedMessage(t, session.InstructionsSnapshot, 2, model.MessageRoleDeveloper, "Second project instructions\n")
	assertSavedMessage(t, session.InstructionsSnapshot, 3, model.MessageRoleDeveloper, "Skill alpha (Alpha Skill):\nSkill instructions\n")
	wantSources := []sessions.InstructionSource{
		{Role: model.MessageRoleSystem, Source: "sai_builtin"},
		{Role: model.MessageRoleDeveloper, Source: "agents_md", Path: filepath.Join(configDir, "project-first.md")},
		{Role: model.MessageRoleDeveloper, Source: "agents_md", Path: filepath.Join(projectDir, "local-second.md")},
		{Role: model.MessageRoleDeveloper, Source: "skill", Path: filepath.Join(configDir, "skills", "alpha", "SKILL.md")},
	}
	if !sameInstructionSourcesForTest(session.InstructionSources, wantSources) {
		t.Fatalf("InstructionSources = %#v, want %#v", session.InstructionSources, wantSources)
	}
}

func TestRunDeduplicatesConfiguredProjectInstructionFilesSilently(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLIInstructionFiles(t, configDir, []string{"$CWD/b*.md", "$CWD/*.md"})

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "b.md"), "B project instructions\n")
	writeCLIFile(t, filepath.Join(projectDir, "a.md"), "A project instructions\n")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--prompt", "Use deduped project instructions"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "warning") {
		t.Fatalf("stderr = %q, want no duplicate warning", stderr.String())
	}

	messages := requestMessages(t, (<-requests).Body)
	if len(messages) != 4 {
		t.Fatalf("len(messages) = %d, want 4: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "developer", "B project instructions\n")
	assertMessage(t, messages, 2, "developer", "A project instructions\n")
	assertMessage(t, messages, 3, "user", "Use deduped project instructions")
	assertNoAdditionalCLIRunRequest(t, requests)

	session := loadOnlyCLISession(t, filepath.Join(configDir, "sessions"))
	if len(session.InstructionsSnapshot) != 3 {
		t.Fatalf("len(InstructionsSnapshot) = %d, want 3: %#v", len(session.InstructionsSnapshot), session.InstructionsSnapshot)
	}
	assertSavedMessage(t, session.InstructionsSnapshot, 0, model.MessageRoleSystem, builtInBaseInstructions)
	assertSavedMessage(t, session.InstructionsSnapshot, 1, model.MessageRoleDeveloper, "B project instructions\n")
	assertSavedMessage(t, session.InstructionsSnapshot, 2, model.MessageRoleDeveloper, "A project instructions\n")
	wantSources := []sessions.InstructionSource{
		{Role: model.MessageRoleSystem, Source: "sai_builtin"},
		{Role: model.MessageRoleDeveloper, Source: "agents_md", Path: filepath.Join(projectDir, "b.md")},
		{Role: model.MessageRoleDeveloper, Source: "agents_md", Path: filepath.Join(projectDir, "a.md")},
	}
	if !sameInstructionSourcesForTest(session.InstructionSources, wantSources) {
		t.Fatalf("InstructionSources = %#v, want %#v", session.InstructionSources, wantSources)
	}
}

func TestRunSkipsDisableModelInvocationSkill(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	writeCLISkill(t, configDir, "hidden", "---\nname: Hidden Skill\ndisable-model-invocation: true\n---\nHidden instructions\n")
	writeCLISkill(t, configDir, "visible", "Visible instructions\n")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--save-session", "--prompt", "Use visible skills"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}

	messages := requestMessages(t, (<-requests).Body)
	if len(messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "developer", "Skill visible (visible):\nVisible instructions\n")
	assertMessage(t, messages, 2, "user", "Use visible skills")
	session := loadOnlyCLISession(t, filepath.Join(configDir, "sessions"))
	if !sameStringsForTest(session.EnabledSkills, []string{"visible"}) {
		t.Fatalf("session.EnabledSkills = %#v, want only visible", session.EnabledSkills)
	}
}

func TestRunFailsOnMalformedDiscoveredSkillFrontmatter(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	writeCLISkill(t, configDir, "valid", "Valid instructions\n")
	writeCLISkill(t, configDir, "bad", "---\nname: [bad\n---\nBad instructions\n")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Use skills"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "load skills: parse skill frontmatter", "bad", "SKILL.md")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunFailsOnDuplicateDiscoveredSkillID(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLISkillDirs(t, configDir, []string{"skills", "team-skills"})
	writeCLISkill(t, configDir, "shared", "First instructions\n")
	writeCLISkillInRoot(t, filepath.Join(configDir, "team-skills"), "shared", "Second instructions\n")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Use skills"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `duplicate skill id "shared"`)
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunRemovedSkillFlagsAreUnknown(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "enable after command", args: []string{"chat", "--quit", "--enable-skills", "alpha", "--prompt", "Use skills"}, want: "flag provided but not defined: -enable-skills"},
		{name: "disable after command", args: []string{"chat", "--quit", "--disable-skills", "--prompt", "Use skills"}, want: "flag provided but not defined: -disable-skills"},
		{name: "enable before command", args: []string{"--enable-skills", "alpha", "chat", "--quit", "--prompt", "Use skills"}, want: "flag provided but not defined: -enable-skills"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(tt.args, &stdout, &stderr, func() (string, error) {
				return t.TempDir(), nil
			})
			if code != 1 {
				t.Fatalf("RunWithGetwd() code = %d, want 1", code)
			}
			if stdout.String() != "" {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			assertCLIErrorContains(t, stderr.String(), tt.want)
		})
	}
}

func TestRunEnableMCPExposesOnlyEnabledMCPSchemas(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	exitFile := filepath.Join(t.TempDir(), "mcp-exited")
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	writeCLIRunMCPFixture(t, configDir, exitFile)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-mcp", "local", "--enable-tools", "list_files,mcp.local.search", "--prompt", "Use mixed tools"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	request := <-requests
	assertCLIToolNames(t, request.Body, []string{"list_files", "mcp.local.search"})
	assertNoAdditionalCLIRunRequest(t, requests)
	assertCLIFileEventuallyContains(t, exitFile, "closed")
}

func TestRunRoutesMCPToolCallAndReturnsResultToModel(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_mcp","function":{"name":"mcp.local.search","arguments":"{\"query\":\"needle\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"final answer"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	exitFile := filepath.Join(t.TempDir(), "mcp-exited")
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	writeCLIRunMCPFixture(t, configDir, exitFile)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-mcp", "local", "--enable-tools", "mcp.local.search", "--prompt", "Use MCP search"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "final answer"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	firstRequest := <-requests
	assertCLIToolNames(t, firstRequest.Body, []string{"mcp.local.search"})

	secondRequest := <-requests
	messages := requestMessages(t, secondRequest.Body)
	if len(messages) != 4 {
		t.Fatalf("len(second request messages) = %d, want 4: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "Use MCP search")
	assertAssistantToolCallMessage(t, messages, 2, "call_mcp", "mcp.local.search", `{"query":"needle"}`)
	assertToolMessage(t, messages, 3, "call_mcp", "mcp result one\nmcp result two")
	assertNoAdditionalCLIRunRequest(t, requests)
	assertCLIFileEventuallyContains(t, exitFile, "closed")
}

func TestRunVerboseWritesDiagnosticsWithoutSensitiveContent(t *testing.T) {
	t.Run("direct API key", func(t *testing.T) {
		server, requests := newCLIRunServer(t,
			`{"choices":[{"delta":{"content":"model response secret"}}]}`,
			`[DONE]`,
		)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--verbose", "--enable-tools", "list_files,read_file", "--prompt", "user prompt secret"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "model response secret"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
		assertCLIVerboseContains(t, stderr.String(),
			"config_path: "+filepath.Clean(cliConfigPath(configDir)),
			"provider: fake",
			"model_profile: default",
			"model_id: model-default",
			"max_turns: 8",
			"enabled_tools: list_files,read_file",
			"show_reasoning: false",
		)
		logPaths := sessionLogPaths(t, configDir)
		if len(logPaths) != 1 {
			t.Fatalf("session log paths = %#v, want one", logPaths)
		}
		assertCLIVerboseContains(t, stderr.String(), "log_path: "+logPaths[0])
		assertCLIErrorOmits(t, stderr.String(), "direct-secret-value", "user prompt secret", "model response secret")
		<-requests
		assertNoAdditionalCLIRunRequest(t, requests)
	})

	t.Run("env API key", func(t *testing.T) {
		server, requests := newCLIRunServer(t,
			`{"choices":[{"delta":{"content":"env response secret"}}]}`,
			`[DONE]`,
		)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "$SAI_VERBOSE_API_KEY", "openai-chat")
		t.Setenv("SAI_VERBOSE_API_KEY", "env-secret-value")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--verbose", "--prompt", "env user prompt secret"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "env response secret"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
		assertCLIVerboseContains(t, stderr.String(),
			"provider: fake",
			"model_id: model-default",
			"enabled_tools: (none)",
		)
		assertCLIErrorOmits(t, stderr.String(), "env-secret-value", "env user prompt secret", "env response secret")
		<-requests
		assertNoAdditionalCLIRunRequest(t, requests)
	})
}

func TestRunVerboseReportsFutureSessionLogPathBeforeFirstEvent(t *testing.T) {
	releaseResponse := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseResponse)
		})
	}

	requests := make(chan capturedCLIRunRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		requests <- capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}

		<-releaseResponse
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	defer release()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--verbose", "--prompt", "hello"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})
	}()

	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first model request")
	}

	futurePath := verboseLogPath(t, stderr.String())
	if futurePath == "(disabled)" {
		t.Fatalf("verbose log_path = %q, want future session path", futurePath)
	}
	if filepath.Base(futurePath) != "sai.jsonl" {
		t.Fatalf("verbose log_path = %q, want sai.jsonl file", futurePath)
	}
	if logPaths := sessionLogPaths(t, configDir); len(logPaths) != 0 {
		t.Fatalf("session log paths before first event = %#v, want none", logPaths)
	}

	release()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for run to finish")
	}
	if got, want := stdout.String(), "ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	logPaths := sessionLogPaths(t, configDir)
	if len(logPaths) != 1 {
		t.Fatalf("session log paths = %#v, want one", logPaths)
	}
	if logPaths[0] != futurePath {
		t.Fatalf("session log path = %q, want verbose future path %q", logPaths[0], futurePath)
	}
}

func TestRunExecutesToolCallAndContinuesToFinalText(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"note.txt\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"done"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "note.txt"), "tool output")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "read_file", "--prompt", "Read note"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "done"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIToolStatus(t, stdout.String(), stderr.String(), "read_file note.txt", "tool output")

	firstRequest := <-requests
	if got := firstRequest.Body["model"]; got != "model-default" {
		t.Fatalf("first request model = %#v, want model-default", got)
	}
	assertCLIToolNames(t, firstRequest.Body, []string{"read_file"})

	secondRequest := <-requests
	if got := secondRequest.Body["model"]; got != "model-default" {
		t.Fatalf("second request model = %#v, want model-default", got)
	}
	messages := requestMessages(t, secondRequest.Body)
	if len(messages) != 4 {
		t.Fatalf("len(second request messages) = %d, want 4: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "Read note")
	assertAssistantToolCallMessage(t, messages, 2, "call_1", "read_file", `{"path":"note.txt"}`)
	assertToolMessage(t, messages, 3, "call_1", "tool output")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunWritesJSONLLogForToolCallWithoutSensitiveContent(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"note.txt\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"final response secret"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":13,"total_tokens":24}}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "note.txt"), "tool output secret")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "secret-api-key", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "read_file", "--prompt", "user prompt secret"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "final response secret"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIToolStatus(t, stdout.String(), stderr.String(), "read_file note.txt", "tool output secret")
	<-requests
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)

	logPaths := sessionLogPaths(t, configDir)
	if len(logPaths) != 1 {
		t.Fatalf("session log paths = %#v, want one", logPaths)
	}
	logPath := logPaths[0]
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", logPath, err)
	}
	logText := string(logData)
	for _, leaked := range []string{"user prompt secret", "final response secret", "tool output secret", "secret-api-key"} {
		if strings.Contains(logText, leaked) {
			t.Fatalf("log leaked %q:\n%s", leaked, logText)
		}
	}

	records := readJSONLRecords(t, logData)
	assertCLILogBaseFields(t, records)
	if !hasCLILogRecord(records, "tool_call_done", "tool_name", "read_file") {
		t.Fatalf("log records missing read_file tool_call_done: %#v", records)
	}
	if !hasCLILogRecord(records, "tool_result", "tool_name", "read_file") || !hasCLILogRecord(records, "tool_result", "is_error", false) {
		t.Fatalf("log records missing successful tool_result metadata: %#v", records)
	}
	usage := firstCLILogRecord(t, records, "usage")["usage"]
	usageMap, ok := usage.(map[string]any)
	if !ok {
		t.Fatalf("usage field = %T(%#v), want object", usage, usage)
	}
	assertJSONNumber(t, usageMap["input_tokens"], "11")
	assertJSONNumber(t, usageMap["output_tokens"], "13")
	assertJSONNumber(t, usageMap["total_tokens"], "24")
}

func TestRunCreatesSeparateSessionLogs(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "secret-api-key", "openai-chat")

	for _, prompt := range []string{"first", "second"} {
		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", prompt}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})
		if code != 0 {
			t.Fatalf("RunWithGetwd(%q) code = %d, stderr = %s", prompt, code, stderr.String())
		}
		if got, want := stdout.String(), "ok"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
		<-requests
	}
	assertNoAdditionalCLIRunRequest(t, requests)

	logPaths := sessionLogPaths(t, configDir)
	if len(logPaths) != 2 {
		t.Fatalf("session log paths = %#v, want two", logPaths)
	}
	if logPaths[0] == logPaths[1] {
		t.Fatalf("two runs wrote the same log path: %q", logPaths[0])
	}
	for _, logPath := range logPaths {
		if filepath.Base(logPath) != "sai.jsonl" {
			t.Fatalf("session log path = %q, want sai.jsonl file", logPath)
		}
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", logPath, err)
		}
		records := readJSONLRecords(t, data)
		assertCLILogBaseFields(t, records)
		firstCLILogRecord(t, records, "usage")
	}
}

func TestRunWithLoggingDisabledDoesNotCreateSessionLog(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "secret-api-key", "openai-chat")
	setCLILoggingPath(t, configDir, `""`)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--verbose", "--prompt", "hello"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIVerboseContains(t, stderr.String(), "log_path: (disabled)")
	if logPaths := sessionLogPaths(t, configDir); len(logPaths) != 0 {
		t.Fatalf("session log paths = %#v, want none", logPaths)
	}
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunEnableToolsRejectsUnknownTool(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "missing", "--prompt", "Use tools"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `enabled tool "missing" is not registered`)
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunEnableMCPRejectsUnknownServer(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-mcp", "missing", "--prompt", "Use MCP"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `unknown MCP server "missing"`, "available MCP servers: (none)")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatAllowsPostPromptModelFlag(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--model", "fast", "--prompt", "Use fast", "--model", "default"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	request := <-requests
	if got := request.Body["model"]; got != "model-default" {
		t.Fatalf("model = %#v, want model-default", got)
	}
	assertMessage(t, requestMessages(t, request.Body), 1, "user", "Use fast")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunIncludesStartupAgentsAndConfigPathDoesNotChangeLookup(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"done"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	projectDir := t.TempDir()
	configDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "AGENTS.md"), "project instructions\n")
	writeCLIFile(t, filepath.Join(configDir, "AGENTS.md"), "config instructions should not be used\n")
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Hello"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}

	request := <-requests
	messages := requestMessages(t, request.Body)
	if len(messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "developer", "project instructions\n")
	assertMessage(t, messages, 2, "user", "Hello")
	if strings.Contains(string(request.RawBody), "config instructions should not be used") {
		t.Fatalf("request included AGENTS.md from config dir: %s", request.RawBody)
	}
}

func TestRunReasoningIsHiddenUnlessShowReasoningIsSet(t *testing.T) {
	t.Run("default hidden", func(t *testing.T) {
		server, _ := newCLIRunServer(t,
			`{"choices":[{"delta":{"reasoning_content":"hidden"}}]}`,
			`{"choices":[{"delta":{"content":"visible"}}]}`,
			`[DONE]`,
		)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Think"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "visible"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("flag shows reasoning", func(t *testing.T) {
		server, _ := newCLIRunServer(t,
			`{"choices":[{"delta":{"reasoning_content":"shown"}}]}`,
			`{"choices":[{"delta":{"content":"visible"}}]}`,
			`[DONE]`,
		)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--show-reasoning", "--prompt", "Think"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "shown\nvisible"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("config shows reasoning", func(t *testing.T) {
		server, _ := newCLIRunServer(t,
			`{"choices":[{"delta":{"reasoning_content":"shown"}}]}`,
			`{"choices":[{"delta":{"content":"visible"}}]}`,
			`[DONE]`,
		)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
		setCLIAgentShowReasoning(t, configDir, true)

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Think"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "shown\nvisible"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("flag false hides configured reasoning", func(t *testing.T) {
		server, _ := newCLIRunServer(t,
			`{"choices":[{"delta":{"reasoning_content":"hidden"}}]}`,
			`{"choices":[{"delta":{"content":"visible"}}]}`,
			`[DONE]`,
		)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
		setCLIAgentShowReasoning(t, configDir, true)

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--show-reasoning=false", "--prompt", "Think"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "visible"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("flag does not duplicate reasoning newline", func(t *testing.T) {
		server, _ := newCLIRunServer(t,
			`{"choices":[{"delta":{"reasoning_content":"shown\n"}}]}`,
			`{"choices":[{"delta":{"content":"visible"}}]}`,
			`[DONE]`,
		)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--show-reasoning", "--prompt", "Think"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "shown\nvisible"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("flag without reasoning does not add newline", func(t *testing.T) {
		server, _ := newCLIRunServer(t,
			`{"choices":[{"delta":{"content":"visible"}}]}`,
			`[DONE]`,
		)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--show-reasoning", "--prompt", "Think"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "visible"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})
}

func TestWriteStreamDoesNotColorReasoningForBufferOutput(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := writeStream(&stdout, &stderr, cliEventStream(
		model.ReasoningDeltaEvent{Text: "shown"},
		model.TextDeltaEvent{Text: "visible"},
	), true, nil)

	if err != nil {
		t.Fatalf("writeStream() error = %v", err)
	}
	if got, want := stdout.String(), "shown\nvisible"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if strings.Contains(stdout.String(), "\x1b[") {
		t.Fatalf("stdout contains ANSI escape sequence: %q", stdout.String())
	}
}

func TestWriteStreamWithOptionsColorsReasoningAndResetsBeforeFinalText(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := writeStreamWithOptions(&stdout, &stderr, cliEventStream(
		model.ReasoningDeltaEvent{Text: "thinking"},
		model.TextDeltaEvent{Text: "final"},
	), true, nil, streamOutputOptions{colorReasoning: true})

	if err != nil {
		t.Fatalf("writeStreamWithOptions() error = %v", err)
	}
	if got, want := stdout.String(), reasoningColorDarkGray+"thinking"+ansiReset+"\nfinal"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestWriteStreamStartsReasoningOnIndependentLine(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := writeStream(&stdout, &stderr, cliEventStream(
		model.TextDeltaEvent{Text: "prefix"},
		model.ReasoningDeltaEvent{Text: "thinking"},
		model.TextDeltaEvent{Text: "final"},
	), true, nil)

	if err != nil {
		t.Fatalf("writeStream() error = %v", err)
	}
	if got, want := stdout.String(), "prefix\nthinking\nfinal"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestWriteStreamWithOptionsResetsAfterReasoningOnly(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := writeStreamWithOptions(&stdout, &stderr, cliEventStream(
		model.ReasoningDeltaEvent{Text: "thinking"},
	), true, nil, streamOutputOptions{colorReasoning: true})

	if err != nil {
		t.Fatalf("writeStreamWithOptions() error = %v", err)
	}
	if got, want := stdout.String(), reasoningColorDarkGray+"thinking"+ansiReset; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestWriteStreamWithOptionsResetsReasoningBeforeToolAndReentersCleanly(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := writeStreamWithOptions(&stdout, &stderr, cliEventStream(
		model.ReasoningDeltaEvent{Text: "thinking"},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "read_file", Arguments: `{"path":"note.txt"}`}},
		model.ReasoningDeltaEvent{Text: "again"},
		model.TextDeltaEvent{Text: "final"},
	), true, nil, streamOutputOptions{colorReasoning: true, colorToolStatus: true})

	if err != nil {
		t.Fatalf("writeStreamWithOptions() error = %v", err)
	}
	if got, want := stdout.String(), reasoningColorDarkGray+"thinking"+ansiReset+"\n"+reasoningColorDarkGray+"again"+ansiReset+"\nfinal"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "\n"+reasoningColorDarkGray+"tool: read_file note.txt"+ansiReset+"\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestWriteStreamWritesToolStatusWithSafeDetailsOnly(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := writeStream(&stdout, &stderr, cliEventStream(
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "read_file", Arguments: `{"path":"docs/checklist.md"}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "list_files", Arguments: `{}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "list_files", Arguments: ``}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "list_files", Arguments: " \n\t "}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "read_file", Arguments: `[`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "list_files", Arguments: `null`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "glob_files", Arguments: `{"pattern":"src/**/*.go","path":"secret-dir","include_hidden":true}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "glob_files", Arguments: `{}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "grep_files", Arguments: `{"query":"secret-query"}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "write_file", Arguments: `{"path":"draft.txt","content":"secret-write-content"}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "edit_file", Arguments: `{"path":"draft.txt","old":"secret-old","new":"secret-new"}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "shell", Arguments: `{"command":"echo secret-command"}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "mcp.local.search", Arguments: `{"query":"secret-query"}`}},
		model.ToolResultEvent{Result: model.ToolResult{Name: "read_file", Content: "secret result body"}},
		model.ToolResultEvent{Result: model.ToolResult{Name: "write_file", Content: "wrote draft.txt (20 bytes)"}},
		model.ToolResultEvent{Result: model.ToolResult{Name: "edit_file", Content: "edited draft.txt (1 replacement)"}},
	), false, nil)

	if err != nil {
		t.Fatalf("writeStream() error = %v", err)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "tool: read_file docs/checklist.md\ntool: list_files .\ntool: list_files .\ntool: list_files .\ntool: read_file\ntool: list_files\ntool: glob_files src/**/*.go\ntool: glob_files\ntool: grep_files\ntool: write_file draft.txt\ntool: edit_file draft.txt\ntool: shell\ntool: mcp.local.search\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	assertCLIErrorOmits(t, stderr.String(), "secret-dir", "include_hidden", "secret-write-content", "secret-old", "secret-new", "secret-command", "secret-query", "secret result body", "wrote draft.txt", "edited draft.txt")
}

func TestWriteStreamWritesSubagentStartStatusWithBriefDetails(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := writeStream(&stdout, &stderr, cliEventStream(
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: subagents.ToolSubagentStart, Arguments: `{"agent_id":"reviewer","prompt":"secret child prompt","display_name":"Review UI","job_name":"review-1"}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: subagents.ToolSubagentStart, Arguments: `{"agent_id":"researcher","prompt":"another secret","job_name":"research-1"}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: subagents.ToolSubagentStart, Arguments: `{"agent_id":"reviewer","prompt":"line secret","display_name":"  Audit\nPass\tOne  "}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: subagents.ToolSubagentStart, Arguments: `{"prompt":"missing agent secret"}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: subagents.ToolSubagentStart, Arguments: `[`}},
	), false, nil)

	if err != nil {
		t.Fatalf("writeStream() error = %v", err)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "tool: subagent_start reviewer Review UI\ntool: subagent_start researcher research-1\ntool: subagent_start reviewer Audit Pass One\ntool: subagent_start\ntool: subagent_start\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	assertCLIErrorOmits(t, stderr.String(), "secret child prompt", "another secret", "line secret", "missing agent secret", "review-1")
}

func TestWriteStreamPutsConsecutiveToolStatusesOnIndependentLinesAfterOutputAndPrompt(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := writeStreamWithOptions(&stdout, &stderr, cliEventStream(
		model.TextDeltaEvent{Text: "partial"},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "shell", Arguments: `{"command":"echo hidden"}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "read_file", Arguments: `{"path":"note.txt"}`}},
	), false, nil, streamOutputOptions{stderrNeedsLeadingBreak: true})

	if err != nil {
		t.Fatalf("writeStreamWithOptions() error = %v", err)
	}
	if got, want := stdout.String(), "partial"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "\ntool: shell\ntool: read_file note.txt\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	assertCLIErrorOmits(t, stderr.String(), "echo hidden")
}

func TestShouldColorizeWriterRequiresTerminalAndHonorsNOColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var buffer bytes.Buffer
	if shouldColorizeWriter(&buffer) {
		t.Fatal("shouldColorizeWriter(bytes.Buffer) = true, want false")
	}

	file, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	defer file.Close()
	if shouldColorizeWriter(file) {
		t.Fatal("shouldColorizeWriter(regular file) = true, want false")
	}

	t.Setenv("NO_COLOR", "1")
	if shouldColorizeWriter(os.Stdout) {
		t.Fatal("shouldColorizeWriter(os.Stdout) = true with NO_COLOR set, want false")
	}
}

func TestRunErrorsDoNotLeakAPIKeyValues(t *testing.T) {
	t.Run("missing API key", func(t *testing.T) {
		server, requests := newCLIRunServer(t, `[DONE]`)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "$SAI_MISSING_API_KEY", "openai-chat")
		t.Setenv("SAI_MISSING_API_KEY", "")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Hello"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stdout.String() != "" {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		assertCLIErrorContains(t, stderr.String(), `API key environment variable "SAI_MISSING_API_KEY" is not set`)
		assertCLIErrorOmits(t, stderr.String(), "Bearer ")
		assertNoAdditionalCLIRunRequest(t, requests)
	})

	t.Run("HTTP failure", func(t *testing.T) {
		requests := make(chan capturedCLIRunRequest, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("ReadAll() error = %v", err)
			}
			requests <- capturedCLIRunRequest{
				Path:          r.URL.Path,
				Authorization: r.Header.Get("Authorization"),
				RawBody:       body,
				Body:          decodeCLIJSON(t, body),
			}
			http.Error(w, "bad request for Authorization: Bearer direct-secret-value", http.StatusBadRequest)
		}))
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Hello"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stdout.String() != "" {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		assertCLIErrorContains(t, stderr.String(), "request model", "OpenAI chat request failed", "400 Bad Request", "bad request")
		assertCLIErrorOmits(t, stderr.String(), "direct-secret-value", "Bearer direct-secret-value")

		request := <-requests
		if request.Authorization != "Bearer direct-secret-value" {
			t.Fatalf("Authorization = %q, want direct API key", request.Authorization)
		}
		logPaths := sessionLogPaths(t, configDir)
		if len(logPaths) != 1 {
			t.Fatalf("session log paths = %#v, want one error log", logPaths)
		}
		data, err := os.ReadFile(logPaths[0])
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", logPaths[0], err)
		}
		records := readJSONLRecords(t, data)
		assertCLILogBaseFields(t, records)
		errorRecord := firstCLILogRecord(t, records, "error")
		if errorRecord["level"] != "error" || errorRecord["message"] != "request model" {
			t.Fatalf("error log record = %#v, want level error with request model message", errorRecord)
		}
		assertNoAdditionalCLIRunRequest(t, requests)
	})

	t.Run("invalid SSE chunk", func(t *testing.T) {
		server, requests := newCLIRunServer(t, `{not-json`)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Hello"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stdout.String() != "" {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		assertCLIErrorContains(t, stderr.String(), "parse OpenAI chat stream", "parse chat completion chunk")
		assertCLIErrorOmits(t, stderr.String(), "direct-secret-value", "Bearer direct-secret-value")
		<-requests
		assertNoAdditionalCLIRunRequest(t, requests)
	})

	t.Run("unknown model", func(t *testing.T) {
		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--model", "missing", "--prompt", "Hello"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		assertCLIErrorContains(t, stderr.String(), `unknown model "missing" for provider "fake"`)
		assertCLIErrorOmits(t, stderr.String(), "direct-secret-value")
	})

	t.Run("unsupported model type", func(t *testing.T) {
		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "not-openai")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Hello"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		assertCLIErrorContains(t, stderr.String(), `unknown model type "not-openai"`, "supported provider types: anthropic-messages, openai-codex, openai-chat, openai-responses")
		assertCLIErrorOmits(t, stderr.String(), "direct-secret-value")
	})
}

func writeCLIFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	writeCLIFixtureInDir(t, dir)
	return dir
}

func cliConfigPath(dir string) string {
	return filepath.Join(dir, "sai.yaml")
}

func writeCLIFixtureInDir(t *testing.T, dir string) {
	t.Helper()

	providersDir := filepath.Join(dir, "providers")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	writeCLIFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: paperhub
default_model: glm-5.2
provider_dir: providers

agent:
  max_turns: 8
  stream: true
  show_reasoning: false

tools:
  enabled: []

logging:
  path: logs/sai.jsonl
  level: info
`)

	writeCLIFile(t, filepath.Join(providersDir, "paperhub.yaml"), `name: paperhub
base_url: https://tc-paperhub.diezhi.net/v1
api_key: $PAPERHUB_API_KEY

models:
  glm-5.2:
    id: glm-5.2
    temperature: 0.6
    max_tokens: 4096
  glm-5.2-fast:
    id: glm-5.2
    temperature: 0.2
    max_tokens: 2048
`)

	writeCLIFile(t, filepath.Join(providersDir, "local.yaml"), `name: local
base_url: http://localhost:8080/v1
api_key: direct-secret-value

models:
  small:
    id: local-small
`)
}

func writeCLIMCPFixture(t *testing.T, dir string) {
	t.Helper()

	mcpDir := filepath.Join(dir, "mcp")
	if err := os.MkdirAll(mcpDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	writeCLIFile(t, filepath.Join(mcpDir, "local.yaml"), `id: local
enabled: true
command: example-mcp-server
args: []
env: {}
`)

	writeCLIFile(t, filepath.Join(mcpDir, "remote.yaml"), `id: remote
enabled: false
command: remote-mcp-server
args: []
env: {}
`)
}

func writeCLIRunMCPFixture(t *testing.T, dir, exitFile string) {
	t.Helper()

	mcpDir := filepath.Join(dir, "mcp")
	if err := os.MkdirAll(mcpDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	writeCLIRunMCPServerFixture(t, mcpDir, "local", exitFile, false)
}

func writeCLIRunMCPServerFixture(t *testing.T, mcpDir, id, exitFile string, enabled bool) {
	t.Helper()
	if err := os.MkdirAll(mcpDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", mcpDir, err)
	}

	writeCLIFile(t, filepath.Join(mcpDir, id+".yaml"), fmt.Sprintf(`id: %s
enabled: %t
command: %q
args:
  - "-test.run=TestCLIMCPHelperProcess"
  - "--"
  - "fake-mcp"
env:
  SAI_CLI_MCP_HELPER_PROCESS: "1"
  SAI_CLI_MCP_EXIT_FILE: %q
`, id, enabled, os.Args[0], exitFile))
}

func writeCLIFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func appendCLIConfig(t *testing.T, configDir, content string) {
	t.Helper()
	configPath := filepath.Join(configDir, "sai.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", configPath, err)
	}
	updated := strings.TrimRight(string(data), "\r\n") + "\n" + strings.TrimLeft(content, "\r\n")
	writeCLIFile(t, configPath, updated)
}

func setCLILoggingPath(t *testing.T, configDir, path string) {
	t.Helper()

	configPath := filepath.Join(configDir, "sai.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", configPath, err)
	}
	updated := strings.Replace(string(data), "  path: logs/sai.jsonl", "  path: "+path, 1)
	if updated == string(data) {
		t.Fatalf("sai.yaml did not contain logging path to replace:\n%s", data)
	}
	writeCLIFile(t, configPath, updated)
}

func setCLISessionsConfig(t *testing.T, configDir string, enabled, saveToolResults bool) {
	t.Helper()

	configPath := filepath.Join(configDir, "sai.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", configPath, err)
	}
	updated := strings.TrimRight(string(data), "\r\n") + fmt.Sprintf(`

sessions:
  enabled: %t
  dir: sessions
  save_tool_results: %t
`, enabled, saveToolResults)
	writeCLIFile(t, configPath, updated)
}

func setCLIAgentShowReasoning(t *testing.T, configDir string, enabled bool) {
	t.Helper()

	configPath := filepath.Join(configDir, "sai.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", configPath, err)
	}
	updated := strings.Replace(string(data), "  show_reasoning: false", fmt.Sprintf("  show_reasoning: %t", enabled), 1)
	if updated == string(data) {
		t.Fatalf("sai.yaml did not contain show_reasoning to replace:\n%s", data)
	}
	writeCLIFile(t, configPath, updated)
}

func setCLIModelContextWindow(t *testing.T, configDir string, tokens int) {
	t.Helper()

	configPath := filepath.Join(configDir, "providers", "fake.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", configPath, err)
	}
	updated := strings.Replace(string(data), "  default:\n    id: model-default\n", fmt.Sprintf("  default:\n    id: model-default\n    context_window: %d\n", tokens), 1)
	if updated == string(data) {
		t.Fatalf("fake.yaml did not contain default model id to replace:\n%s", data)
	}
	writeCLIFile(t, configPath, updated)
}

func writeCLISkill(t *testing.T, configDir, id, content string) {
	t.Helper()
	writeCLISkillInRoot(t, filepath.Join(configDir, "skills"), id, content)
}

func writeCLISkillInRoot(t *testing.T, skillRoot, id, content string) {
	t.Helper()
	skillDir := filepath.Join(skillRoot, id)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", skillDir, err)
	}
	writeCLIFile(t, filepath.Join(skillDir, "SKILL.md"), content)
}

func setCLISkillDirs(t *testing.T, configDir string, dirs []string) {
	t.Helper()
	configPath := filepath.Join(configDir, "sai.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", configPath, err)
	}
	replacement := "provider_dir: providers\nskill_dirs: " + formatTopLevelStringListYAML(dirs) + "\n"
	updated := strings.Replace(string(data), "provider_dir: providers\n", replacement, 1)
	if updated == string(data) {
		t.Fatalf("sai.yaml did not contain provider_dir to replace:\n%s", data)
	}
	writeCLIFile(t, configPath, updated)
}

func setCLIInstructionFiles(t *testing.T, configDir string, files []string) {
	t.Helper()
	configPath := filepath.Join(configDir, "sai.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", configPath, err)
	}
	replacement := "agent:\n  instruction_files:" + formatNestedStringListYAML(files, 4) + "\n  max_turns"
	updated := strings.Replace(string(data), "agent:\n  max_turns", replacement, 1)
	if updated == string(data) {
		t.Fatalf("sai.yaml did not contain agent max_turns to replace:\n%s", data)
	}
	writeCLIFile(t, configPath, updated)
}

func loadOnlyCLISession(t *testing.T, root string) sessions.Session {
	t.Helper()

	store := sessions.NewStore(root)
	infos, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("len(sessions) = %d, want 1: %#v", len(infos), infos)
	}
	return loadCLISession(t, root, infos[0].ID)
}

func loadCLISession(t *testing.T, root, id string) sessions.Session {
	t.Helper()

	session, err := sessions.NewStore(root).Load(id)
	if err != nil {
		t.Fatalf("Load(%q) error = %v", id, err)
	}
	return session
}

func writeCLISession(t *testing.T, root string, session sessions.Session) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(root, session.ID), 0o755); err != nil {
		t.Fatalf("MkdirAll(session %q) error = %v", session.ID, err)
	}
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent(session %q) error = %v", session.ID, err)
	}
	data = append(data, '\n')
	path := filepath.Join(root, session.ID, "session.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func writeCLIPruneSession(t *testing.T, root, id string, updatedAt time.Time) {
	t.Helper()

	writeCLISession(t, root, sessions.Session{
		ID:           id,
		CreatedAt:    updatedAt.Add(-time.Minute),
		UpdatedAt:    updatedAt,
		Version:      sessions.CurrentVersion,
		Provider:     "paperhub",
		ModelProfile: "glm-5.2",
		ModelID:      "glm-5.2",
		Messages:     []model.Message{{Role: model.MessageRoleUser, Content: id + " prompt secret"}},
	})
}

type capturedCLIRunRequest struct {
	Path             string
	Authorization    string
	XAPIKey          string
	AnthropicVersion string
	ContentType      string
	RawBody          []byte
	Body             map[string]any
}

type failingWriter struct {
	err error
}

func (w failingWriter) Write(p []byte) (int, error) {
	return 0, w.err
}

type signalingWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	want  string
	wrote chan struct{}
	once  sync.Once
}

func newSignalingWriter(want string) *signalingWriter {
	return &signalingWriter{
		want:  want,
		wrote: make(chan struct{}),
	}
}

func (w *signalingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.want) {
		w.once.Do(func() { close(w.wrote) })
	}
	return n, err
}

func (w *signalingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func newCLIRunServer(t *testing.T, chunks ...string) (*httptest.Server, <-chan capturedCLIRunRequest) {
	t.Helper()

	requests := make(chan capturedCLIRunRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		requests <- capturedCLIRunRequest{
			Path:             r.URL.Path,
			Authorization:    r.Header.Get("Authorization"),
			XAPIKey:          r.Header.Get("x-api-key"),
			AnthropicVersion: r.Header.Get("anthropic-version"),
			ContentType:      r.Header.Get("Content-Type"),
			RawBody:          body,
			Body:             decodeCLIJSON(t, body),
		}

		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
	}))
	return server, requests
}

func newSequentialCLIRunServer(t *testing.T, responses ...[]string) (*httptest.Server, <-chan capturedCLIRunRequest) {
	t.Helper()

	requests := make(chan capturedCLIRunRequest, len(responses))
	var mu sync.Mutex
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		requests <- capturedCLIRunRequest{
			Path:             r.URL.Path,
			Authorization:    r.Header.Get("Authorization"),
			XAPIKey:          r.Header.Get("x-api-key"),
			AnthropicVersion: r.Header.Get("anthropic-version"),
			ContentType:      r.Header.Get("Content-Type"),
			RawBody:          body,
			Body:             decodeCLIJSON(t, body),
		}

		mu.Lock()
		index := requestCount
		requestCount++
		mu.Unlock()
		if index >= len(responses) {
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range responses[index] {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
	}))
	return server, requests
}

func newCancelingFirstThenCLIRunServer(t *testing.T, responses ...[]string) (*httptest.Server, <-chan capturedCLIRunRequest) {
	t.Helper()

	requests := make(chan capturedCLIRunRequest, len(responses)+1)
	var mu sync.Mutex
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		requests <- capturedCLIRunRequest{
			Path:             r.URL.Path,
			Authorization:    r.Header.Get("Authorization"),
			XAPIKey:          r.Header.Get("x-api-key"),
			AnthropicVersion: r.Header.Get("anthropic-version"),
			ContentType:      r.Header.Get("Content-Type"),
			RawBody:          body,
			Body:             decodeCLIJSON(t, body),
		}

		mu.Lock()
		index := requestCount
		requestCount++
		mu.Unlock()
		if index == 0 {
			<-r.Context().Done()
			return
		}
		responseIndex := index - 1
		if responseIndex >= len(responses) {
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range responses[responseIndex] {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
	}))
	return server, requests
}

func waitForChannel(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitForCode(t *testing.T, ch <-chan int) int {
	t.Helper()

	select {
	case code := <-ch:
		return code
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for chat to finish")
	}
	return -1
}

func writeCLIRunFixtureInDir(t *testing.T, dir, baseURL, apiKey, modelType string) {
	t.Helper()
	writeCLIRunFixtureInDirWithTools(t, dir, baseURL, apiKey, modelType, nil)
}

func writeCLIRunFixtureInDirWithTools(t *testing.T, dir, baseURL, apiKey, modelType string, enabledTools []string) {
	t.Helper()
	writeCLIRunFixtureInDirWithToolList(t, dir, baseURL, apiKey, modelType, enabledTools)
}

func writeCLIRunFixtureInDirWithToolList(t *testing.T, dir, baseURL, apiKey, modelType string, enabledTools []string) {
	t.Helper()

	providersDir := filepath.Join(dir, "providers")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	modelTypeYAML := ""
	if modelType != "openai-chat" {
		modelTypeYAML = fmt.Sprintf("    type: %s\n", modelType)
	}

	writeCLIFile(t, filepath.Join(dir, "sai.yaml"), fmt.Sprintf(`default_provider: fake
default_model: default
provider_dir: providers

agent:
  max_turns: 8
  stream: true
  show_reasoning: false

tools:
  enabled: %s

logging:
  path: logs/sai.jsonl
  level: info
`, formatEnabledToolsYAML(enabledTools)))

	writeCLIFile(t, filepath.Join(providersDir, "fake.yaml"), fmt.Sprintf(`name: fake
base_url: %s
api_key: %s

models:
  default:
    id: model-default
%s    temperature: 0.6
    max_tokens: 128
  fast:
    id: model-fast
%s    temperature: 0.2
    max_tokens: 64
`, baseURL, apiKey, modelTypeYAML, modelTypeYAML))
}

func formatEnabledToolsYAML(enabledTools []string) string {
	if len(enabledTools) == 0 {
		return "[]"
	}

	var out strings.Builder
	out.WriteByte('\n')
	for _, name := range enabledTools {
		fmt.Fprintf(&out, "    - %s\n", name)
	}
	return strings.TrimRight(out.String(), "\n")
}

func formatTopLevelStringListYAML(values []string) string {
	if len(values) == 0 {
		return "[]"
	}

	var out strings.Builder
	out.WriteByte('\n')
	for _, value := range values {
		fmt.Fprintf(&out, "  - %s\n", value)
	}
	return strings.TrimRight(out.String(), "\n")
}

func formatNestedStringListYAML(values []string, indent int) string {
	if len(values) == 0 {
		return " []"
	}

	prefix := strings.Repeat(" ", indent)
	var out strings.Builder
	for _, value := range values {
		fmt.Fprintf(&out, "\n%s- %s", prefix, value)
	}
	return out.String()
}

func decodeCLIJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()

	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode request JSON %q: %v", data, err)
	}
	return value
}

func readJSONLRecords(t *testing.T, data []byte) []map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.UseNumber()
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("decode JSONL line %q: %v", line, err)
		}
		records = append(records, record)
	}
	if len(records) == 0 {
		t.Fatal("JSONL log has no records")
	}
	return records
}

func cliEventStream(events ...model.Event) <-chan model.Event {
	ch := make(chan model.Event, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch
}

func requestMessages(t *testing.T, body map[string]any) []any {
	t.Helper()

	messages, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("messages = %T, want []any", body["messages"])
	}
	return messages
}

func requestInput(t *testing.T, body map[string]any) []any {
	t.Helper()

	input, ok := body["input"].([]any)
	if !ok {
		t.Fatalf("input = %T, want []any", body["input"])
	}
	return input
}

func assertMessage(t *testing.T, messages []any, index int, role, content string) {
	t.Helper()

	if index >= len(messages) {
		t.Fatalf("missing message %d in %#v", index, messages)
	}
	message, ok := messages[index].(map[string]any)
	if !ok {
		t.Fatalf("message[%d] = %T, want object", index, messages[index])
	}
	if got := message["role"]; got != role {
		t.Fatalf("message[%d].role = %#v, want %q", index, got, role)
	}
	if got := message["content"]; got != content {
		t.Fatalf("message[%d].content = %#v, want %q", index, got, content)
	}
}

func assertSavedMessage(t *testing.T, messages []model.Message, index int, role model.MessageRole, content string) {
	t.Helper()

	if index >= len(messages) {
		t.Fatalf("missing saved message %d in %#v", index, messages)
	}
	message := messages[index]
	if message.Role != role {
		t.Fatalf("saved message[%d].Role = %q, want %q", index, message.Role, role)
	}
	if message.Content != content {
		t.Fatalf("saved message[%d].Content = %q, want %q", index, message.Content, content)
	}
}

func assertSavedAssistantToolCallMessage(t *testing.T, messages []model.Message, index int, id, name, arguments string) {
	t.Helper()

	if index >= len(messages) {
		t.Fatalf("missing saved message %d in %#v", index, messages)
	}
	message := messages[index]
	if message.Role != model.MessageRoleAssistant {
		t.Fatalf("saved message[%d].Role = %q, want assistant", index, message.Role)
	}
	if message.Content != "" {
		t.Fatalf("saved message[%d].Content = %q, want empty", index, message.Content)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("len(saved message[%d].ToolCalls) = %d, want 1", index, len(message.ToolCalls))
	}
	toolCall := message.ToolCalls[0]
	if toolCall.ID != id || toolCall.Name != name || toolCall.Arguments != arguments {
		t.Fatalf("saved tool call = %#v, want id %q name %q arguments %q", toolCall, id, name, arguments)
	}
}

func assertSavedToolMessage(t *testing.T, messages []model.Message, index int, toolCallID, content string) {
	t.Helper()

	if index >= len(messages) {
		t.Fatalf("missing saved message %d in %#v", index, messages)
	}
	message := messages[index]
	if message.Role != model.MessageRoleTool {
		t.Fatalf("saved message[%d].Role = %q, want tool", index, message.Role)
	}
	if message.ToolCallID != toolCallID {
		t.Fatalf("saved message[%d].ToolCallID = %q, want %q", index, message.ToolCallID, toolCallID)
	}
	if message.Content != content {
		t.Fatalf("saved message[%d].Content = %q, want %q", index, message.Content, content)
	}
}

func assertAssistantToolCallMessage(t *testing.T, messages []any, index int, id, name, arguments string) {
	t.Helper()

	if index >= len(messages) {
		t.Fatalf("missing message %d in %#v", index, messages)
	}
	message, ok := messages[index].(map[string]any)
	if !ok {
		t.Fatalf("message[%d] = %T, want object", index, messages[index])
	}
	if got := message["role"]; got != "assistant" {
		t.Fatalf("message[%d].role = %#v, want assistant", index, got)
	}
	if got := message["content"]; got != "" {
		t.Fatalf("message[%d].content = %#v, want empty string", index, got)
	}
	toolCalls, ok := message["tool_calls"].([]any)
	if !ok {
		t.Fatalf("message[%d].tool_calls = %T, want []any", index, message["tool_calls"])
	}
	if len(toolCalls) != 1 {
		t.Fatalf("len(message[%d].tool_calls) = %d, want 1", index, len(toolCalls))
	}
	toolCall, ok := toolCalls[0].(map[string]any)
	if !ok {
		t.Fatalf("tool_calls[0] = %T, want object", toolCalls[0])
	}
	if got := toolCall["id"]; got != id {
		t.Fatalf("tool_calls[0].id = %#v, want %q", got, id)
	}
	if got := toolCall["type"]; got != "function" {
		t.Fatalf("tool_calls[0].type = %#v, want function", got)
	}
	function, ok := toolCall["function"].(map[string]any)
	if !ok {
		t.Fatalf("tool_calls[0].function = %T, want object", toolCall["function"])
	}
	if got := function["name"]; got != name {
		t.Fatalf("tool_calls[0].function.name = %#v, want %q", got, name)
	}
	if got := function["arguments"]; got != arguments {
		t.Fatalf("tool_calls[0].function.arguments = %#v, want %q", got, arguments)
	}
}

func assertToolMessage(t *testing.T, messages []any, index int, toolCallID, content string) {
	t.Helper()

	if index >= len(messages) {
		t.Fatalf("missing message %d in %#v", index, messages)
	}
	message, ok := messages[index].(map[string]any)
	if !ok {
		t.Fatalf("message[%d] = %T, want object", index, messages[index])
	}
	if got := message["role"]; got != "tool" {
		t.Fatalf("message[%d].role = %#v, want tool", index, got)
	}
	if got := message["tool_call_id"]; got != toolCallID {
		t.Fatalf("message[%d].tool_call_id = %#v, want %q", index, got, toolCallID)
	}
	if got := message["content"]; got != content {
		t.Fatalf("message[%d].content = %#v, want %q", index, got, content)
	}
}

func assertAnthropicAssistantToolUseMessage(t *testing.T, messages []any, index int, id, name, inputKey, inputValue string) {
	t.Helper()

	if index >= len(messages) {
		t.Fatalf("missing message %d in %#v", index, messages)
	}
	message, ok := messages[index].(map[string]any)
	if !ok {
		t.Fatalf("message[%d] = %T, want object", index, messages[index])
	}
	if got := message["role"]; got != "assistant" {
		t.Fatalf("message[%d].role = %#v, want assistant", index, got)
	}
	blocks, ok := message["content"].([]any)
	if !ok {
		t.Fatalf("message[%d].content = %T, want []any", index, message["content"])
	}
	if len(blocks) != 1 {
		t.Fatalf("len(message[%d].content) = %d, want 1: %#v", index, len(blocks), blocks)
	}
	block, ok := blocks[0].(map[string]any)
	if !ok {
		t.Fatalf("message[%d].content[0] = %T, want object", index, blocks[0])
	}
	if got := block["type"]; got != "tool_use" {
		t.Fatalf("message[%d].content[0].type = %#v, want tool_use", index, got)
	}
	if got := block["id"]; got != id {
		t.Fatalf("message[%d].content[0].id = %#v, want %q", index, got, id)
	}
	if got := block["name"]; got != name {
		t.Fatalf("message[%d].content[0].name = %#v, want %q", index, got, name)
	}
	input, ok := block["input"].(map[string]any)
	if !ok {
		t.Fatalf("message[%d].content[0].input = %T, want object", index, block["input"])
	}
	if got := input[inputKey]; got != inputValue {
		t.Fatalf("message[%d].content[0].input[%q] = %#v, want %q", index, inputKey, got, inputValue)
	}
}

func assertAnthropicToolResultMessage(t *testing.T, messages []any, index int, toolUseID, content string) {
	t.Helper()

	if index >= len(messages) {
		t.Fatalf("missing message %d in %#v", index, messages)
	}
	message, ok := messages[index].(map[string]any)
	if !ok {
		t.Fatalf("message[%d] = %T, want object", index, messages[index])
	}
	if got := message["role"]; got != "user" {
		t.Fatalf("message[%d].role = %#v, want user", index, got)
	}
	blocks, ok := message["content"].([]any)
	if !ok {
		t.Fatalf("message[%d].content = %T, want []any", index, message["content"])
	}
	if len(blocks) != 1 {
		t.Fatalf("len(message[%d].content) = %d, want 1: %#v", index, len(blocks), blocks)
	}
	block, ok := blocks[0].(map[string]any)
	if !ok {
		t.Fatalf("message[%d].content[0] = %T, want object", index, blocks[0])
	}
	if got := block["type"]; got != "tool_result" {
		t.Fatalf("message[%d].content[0].type = %#v, want tool_result", index, got)
	}
	if got := block["tool_use_id"]; got != toolUseID {
		t.Fatalf("message[%d].content[0].tool_use_id = %#v, want %q", index, got, toolUseID)
	}
	if got := block["content"]; got != content {
		t.Fatalf("message[%d].content[0].content = %#v, want %q", index, got, content)
	}
	if got, ok := block["is_error"]; ok && got != false {
		t.Fatalf("message[%d].content[0].is_error = %#v, want absent or false", index, got)
	}
}

func assertCLIRequestOmitsKey(t *testing.T, body map[string]any, key string) {
	t.Helper()

	if _, ok := body[key]; ok {
		t.Fatalf("request body contains unexpected key %q: %#v", key, body[key])
	}
}

func assertCLIAnthropicToolNames(t *testing.T, body map[string]any, want []string) {
	t.Helper()

	toolsValue, ok := body["tools"].([]any)
	if !ok {
		t.Fatalf("tools = %T(%#v), want []any", body["tools"], body["tools"])
	}
	if len(toolsValue) != len(want) {
		t.Fatalf("len(tools) = %d, want %d: %#v", len(toolsValue), len(want), toolsValue)
	}
	for i, wantName := range want {
		tool, ok := toolsValue[i].(map[string]any)
		if !ok {
			t.Fatalf("tools[%d] = %T, want object", i, toolsValue[i])
		}
		if got := tool["name"]; got != wantName {
			t.Fatalf("tools[%d].name = %#v, want %q", i, got, wantName)
		}
		if got := tool["description"]; got == "" {
			t.Fatalf("tools[%d].description is empty", i)
		}
		inputSchema, ok := tool["input_schema"].(map[string]any)
		if !ok {
			t.Fatalf("tools[%d].input_schema = %T, want object", i, tool["input_schema"])
		}
		if got := inputSchema["type"]; got != "object" {
			t.Fatalf("tools[%d].input_schema.type = %#v, want object", i, got)
		}
	}
}

func assertCLIToolNames(t *testing.T, body map[string]any, want []string) {
	t.Helper()

	toolsValue, ok := body["tools"].([]any)
	if !ok {
		t.Fatalf("tools = %T(%#v), want []any", body["tools"], body["tools"])
	}
	if len(toolsValue) != len(want) {
		t.Fatalf("len(tools) = %d, want %d: %#v", len(toolsValue), len(want), toolsValue)
	}
	for i, wantName := range want {
		tool, ok := toolsValue[i].(map[string]any)
		if !ok {
			t.Fatalf("tools[%d] = %T, want object", i, toolsValue[i])
		}
		if got := tool["type"]; got != "function" {
			t.Fatalf("tools[%d].type = %#v, want function", i, got)
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			t.Fatalf("tools[%d].function = %T, want object", i, tool["function"])
		}
		if got := function["name"]; got != wantName {
			t.Fatalf("tools[%d].function.name = %#v, want %q", i, got, wantName)
		}
		if got := function["description"]; got == "" {
			t.Fatalf("tools[%d].function.description is empty", i)
		}
		parameters, ok := function["parameters"].(map[string]any)
		if !ok {
			t.Fatalf("tools[%d].function.parameters = %T, want object", i, function["parameters"])
		}
		if got := parameters["type"]; got != "object" {
			t.Fatalf("tools[%d].function.parameters.type = %#v, want object", i, got)
		}
	}
}

func assertCLIResponsesToolNames(t *testing.T, body map[string]any, want []string) {
	t.Helper()

	toolsValue, ok := body["tools"].([]any)
	if !ok {
		t.Fatalf("tools = %T(%#v), want []any", body["tools"], body["tools"])
	}
	if len(toolsValue) != len(want) {
		t.Fatalf("len(tools) = %d, want %d: %#v", len(toolsValue), len(want), toolsValue)
	}
	for i, wantName := range want {
		tool, ok := toolsValue[i].(map[string]any)
		if !ok {
			t.Fatalf("tools[%d] = %T, want object", i, toolsValue[i])
		}
		if got := tool["type"]; got != "function" {
			t.Fatalf("tools[%d].type = %#v, want function", i, got)
		}
		if got := tool["name"]; got != wantName {
			t.Fatalf("tools[%d].name = %#v, want %q", i, got, wantName)
		}
		if got := tool["description"]; got == "" {
			t.Fatalf("tools[%d].description is empty", i)
		}
		parameters, ok := tool["parameters"].(map[string]any)
		if !ok {
			t.Fatalf("tools[%d].parameters = %T, want object", i, tool["parameters"])
		}
		if got := parameters["type"]; got != "object" {
			t.Fatalf("tools[%d].parameters.type = %#v, want object", i, got)
		}
	}
}

func assertResponseFunctionCallInput(t *testing.T, input []any, index int, callID, name, arguments string) {
	t.Helper()

	if index >= len(input) {
		t.Fatalf("missing input %d in %#v", index, input)
	}
	item, ok := input[index].(map[string]any)
	if !ok {
		t.Fatalf("input[%d] = %T, want object", index, input[index])
	}
	if got := item["type"]; got != "function_call" {
		t.Fatalf("input[%d].type = %#v, want function_call", index, got)
	}
	if got := item["call_id"]; got != callID {
		t.Fatalf("input[%d].call_id = %#v, want %q", index, got, callID)
	}
	if got := item["name"]; got != name {
		t.Fatalf("input[%d].name = %#v, want %q", index, got, name)
	}
	if got := item["arguments"]; got != arguments {
		t.Fatalf("input[%d].arguments = %#v, want %q", index, got, arguments)
	}
}

func assertResponseFunctionCallOutput(t *testing.T, input []any, index int, callID, output string) {
	t.Helper()

	if index >= len(input) {
		t.Fatalf("missing input %d in %#v", index, input)
	}
	item, ok := input[index].(map[string]any)
	if !ok {
		t.Fatalf("input[%d] = %T, want object", index, input[index])
	}
	if got := item["type"]; got != "function_call_output" {
		t.Fatalf("input[%d].type = %#v, want function_call_output", index, got)
	}
	if got := item["call_id"]; got != callID {
		t.Fatalf("input[%d].call_id = %#v, want %q", index, got, callID)
	}
	if got := item["output"]; got != output {
		t.Fatalf("input[%d].output = %#v, want %q", index, got, output)
	}
}

func assertJSONNumber(t *testing.T, value any, want string) {
	t.Helper()

	number, ok := value.(json.Number)
	if !ok {
		t.Fatalf("value = %T(%#v), want json.Number", value, value)
	}
	if got := number.String(); got != want {
		t.Fatalf("number = %q, want %q", got, want)
	}
}

func assertCLIErrorContains(t *testing.T, got string, wants ...string) {
	t.Helper()

	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("stderr = %q, want contain %q", got, want)
		}
	}
}

func assertResumableSessionNoticeOnce(t *testing.T, got string) {
	t.Helper()

	if count := strings.Count(got, resumableSessionSaveNoticeText); count != 1 {
		t.Fatalf("resumable session notice count = %d, want 1 in stderr %q", count, got)
	}
}

func sameStringsForTest(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameInstructionSourcesForTest(a, b []sessions.InstructionSource) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Role != b[i].Role || a[i].Source != b[i].Source || filepath.Clean(a[i].Path) != filepath.Clean(b[i].Path) {
			return false
		}
	}
	return true
}

func assertCLIHelpWithoutConfig(t *testing.T, args []string, wants ...string) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})
	if code != 0 {
		t.Fatalf("RunWithGetwd(%v) code = %d, stderr = %s", args, code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	out := stdout.String()
	if out == "" {
		t.Fatal("stdout is empty")
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want contain %q", out, want)
		}
	}
}

func assertCLIOutputContains(t *testing.T, got string, wants ...string) {
	t.Helper()

	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want contain %q", got, want)
		}
	}
}

func replaceCLIFileText(t *testing.T, path, old, new string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	updated := strings.Replace(string(data), old, new, 1)
	if updated == string(data) {
		t.Fatalf("file %q did not contain %q", path, old)
	}
	writeCLIFile(t, path, updated)
}

func unsetEnvForCLITest(t *testing.T, name string) {
	t.Helper()

	oldValue, hadValue := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("Unsetenv(%q) error = %v", name, err)
	}
	t.Cleanup(func() {
		if hadValue {
			_ = os.Setenv(name, oldValue)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

func assertCLIVerboseContains(t *testing.T, got string, wants ...string) {
	t.Helper()

	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("verbose stderr = %q, want contain %q", got, want)
		}
	}
}

func verboseLogPath(t *testing.T, got string) string {
	t.Helper()

	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "log_path: ") {
			return strings.TrimPrefix(line, "log_path: ")
		}
	}
	t.Fatalf("verbose stderr = %q, want log_path line", got)
	return ""
}

func sessionLogPaths(t *testing.T, configDir string) []string {
	t.Helper()

	logRoot := filepath.Join(configDir, "logs")
	entries, err := os.ReadDir(logRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("ReadDir(%q) error = %v", logRoot, err)
	}

	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(logRoot, entry.Name(), "sai.jsonl")
		if _, err := os.Stat(path); err == nil {
			paths = append(paths, path)
		} else if !os.IsNotExist(err) {
			t.Fatalf("Stat(%q) error = %v", path, err)
		}
	}
	sort.Strings(paths)
	return paths
}

func sessionDirs(t *testing.T, configDir string) []string {
	t.Helper()

	sessionRoot := filepath.Join(configDir, "sessions")
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("ReadDir(%q) error = %v", sessionRoot, err)
	}

	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(sessionRoot, entry.Name()))
		}
	}
	sort.Strings(dirs)
	return dirs
}

func assertCLIToolStatus(t *testing.T, stdout, stderr, statusDetail string, hiddenValues ...string) {
	t.Helper()

	if strings.Contains(stdout, "tool:") || strings.Contains(stdout, statusDetail) {
		t.Fatalf("stdout contains tool status: %q", stdout)
	}
	if !strings.Contains(stderr, "tool: "+statusDetail+"\n") {
		t.Fatalf("stderr = %q, want tool status for %q", stderr, statusDetail)
	}
	for _, value := range hiddenValues {
		if strings.Contains(stderr, value) {
			t.Fatalf("stderr leaked tool detail %q: %s", value, stderr)
		}
	}
}

func assertCLIErrorOmits(t *testing.T, got string, values ...string) {
	t.Helper()

	for _, value := range values {
		if strings.Contains(got, value) {
			t.Fatalf("stderr leaked %q: %s", value, got)
		}
	}
}

func assertCLILogBaseFields(t *testing.T, records []map[string]any) {
	t.Helper()

	for _, record := range records {
		for _, key := range []string{"time", "level", "event", "provider", "model"} {
			if value, ok := record[key].(string); !ok || value == "" {
				t.Fatalf("log record %v missing string field %q", record, key)
			}
		}
		if record["provider"] != "fake" {
			t.Fatalf("log provider = %#v, want fake", record["provider"])
		}
		if record["model"] != "model-default" {
			t.Fatalf("log model = %#v, want model-default", record["model"])
		}
	}
}

func firstCLILogRecord(t *testing.T, records []map[string]any, event string) map[string]any {
	t.Helper()

	for _, record := range records {
		if record["event"] == event {
			return record
		}
	}
	t.Fatalf("missing log event %q in %#v", event, records)
	return nil
}

func hasCLILogRecord(records []map[string]any, event, key string, value any) bool {
	for _, record := range records {
		if record["event"] == event && record[key] == value {
			return true
		}
	}
	return false
}

func assertNoAdditionalCLIRunRequest(t *testing.T, requests <-chan capturedCLIRunRequest) {
	t.Helper()

	select {
	case request := <-requests:
		t.Fatalf("unexpected additional model request: path=%s body=%s", request.Path, request.RawBody)
	default:
	}
}

func receiveCLIRunRequest(t *testing.T, requests <-chan capturedCLIRunRequest) capturedCLIRunRequest {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for model request")
		return capturedCLIRunRequest{}
	}
}

func assertCLIFileEventuallyContains(t *testing.T, path, want string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %q did not contain %q before timeout; last read error = %v, data = %q", path, want, err, data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCLIMCPHelperProcess(t *testing.T) {
	if os.Getenv("SAI_CLI_MCP_HELPER_PROCESS") != "1" {
		return
	}

	code := runCLIFakeMCPServer()
	if code == 0 {
		if exitFile := os.Getenv("SAI_CLI_MCP_EXIT_FILE"); exitFile != "" {
			_ = os.WriteFile(exitFile, []byte("closed"), 0o644)
		}
	}
	os.Exit(code)
}

func runCLIFakeMCPServer() int {
	reader := bufio.NewReader(os.Stdin)

	request, err := readCLIMCPRequest(reader)
	if err != nil {
		return 2
	}
	if request.JSONRPC != "2.0" || request.Method != "initialize" || request.Params.ClientInfo.Name != "sai" {
		return 3
	}
	if err := writeCLIMCPMessage(os.Stdout, map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result": map[string]any{
			"protocolVersion": request.Params.ProtocolVersion,
			"capabilities":    map[string]any{},
			"serverInfo": map[string]any{
				"name":    "cli-fake-mcp",
				"version": "test",
			},
		},
	}); err != nil {
		return 4
	}

	notification, err := readCLIMCPRequest(reader)
	if err != nil {
		return 5
	}
	if notification.JSONRPC != "2.0" || notification.Method != "notifications/initialized" {
		return 6
	}

	for {
		request, err := readCLIMCPRequest(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0
			}
			return 7
		}
		if request.JSONRPC != "2.0" {
			return 8
		}

		switch request.Method {
		case "tools/list":
			if err := writeCLIMCPMessage(os.Stdout, map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{
							"name":        "search",
							"description": "search local fixture data",
							"inputSchema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"query": map[string]any{"type": "string"},
								},
							},
						},
						{
							"name":        "ignored",
							"description": "disabled fixture tool",
							"inputSchema": map[string]any{"type": "object"},
						},
					},
				},
			}); err != nil {
				return 9
			}
		case "tools/call":
			if request.Params.Name != "search" || request.Params.Arguments["query"] != "needle" {
				return 10
			}
			if err := writeCLIMCPMessage(os.Stdout, map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": "mcp result one"},
						{"type": "text", "text": "mcp result two"},
					},
					"isError": false,
				},
			}); err != nil {
				return 11
			}
		default:
			return 12
		}
	}
}

type cliMCPRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"params"`
}

func readCLIMCPRequest(reader *bufio.Reader) (cliMCPRequest, error) {
	payload, err := reader.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return cliMCPRequest{}, io.EOF
		}
		return cliMCPRequest{}, err
	}

	var request cliMCPRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return cliMCPRequest{}, err
	}
	return request, nil
}

func writeCLIMCPMessage(w io.Writer, message any) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	_, err = w.Write(payload)
	return err
}
