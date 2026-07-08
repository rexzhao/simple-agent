package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/model"
	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/sessionprojector"
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

func TestCustomProgramBasenameInHelpVersionAndUsageError(t *testing.T) {
	program := filepath.Join(t.TempDir(), "custom-agent.exe")
	serverRoot := t.TempDir()
	getwd := func() (string, error) {
		return "", errors.New("getwd should not be called")
	}

	for _, tt := range []struct {
		name  string
		args  []string
		wants []string
	}{
		{
			name: "root help",
			args: []string{"--server-root", serverRoot, "help"},
			wants: []string{
				"usage: custom-agent.exe [--server-root dir]",
				"With no command, custom-agent.exe auto-creates a project for the current directory",
				"needed, then starts a pending session.",
				`Run "custom-agent.exe help <command>" for command usage.`,
			},
		},
		{
			name:  "nested help",
			args:  []string{"--server-root", serverRoot, "help", "project", "create"},
			wants: []string{"usage: custom-agent.exe project create [--cwd path] [--name name]"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithProgramGetwd(program, tt.args, &stdout, &stderr, getwd)
			if code != 0 {
				t.Fatalf("RunWithProgramGetwd(%v) code = %d, stderr = %s", tt.args, code, stderr.String())
			}
			if stderr.String() != "" {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			out := stdout.String()
			for _, want := range tt.wants {
				if !strings.Contains(out, want) {
					t.Fatalf("stdout = %q, want contain %q", out, want)
				}
			}
			assertCLIErrorOmits(t, out, filepath.Dir(program), "usage: sai", `Run "sai help`)
		})
	}

	t.Run("version", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := RunWithProgramGetwd(program, []string{"--server-root", serverRoot, "version"}, &stdout, &stderr, getwd)
		if code != 0 {
			t.Fatalf("RunWithProgramGetwd(version) code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "custom-agent.exe dev\n"; got != want {
			t.Fatalf("version output = %q, want %q", got, want)
		}
		if stderr.String() != "" {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
	})

	t.Run("usage error", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := RunWithProgramGetwd(program, []string{"--server-root", serverRoot, "nope"}, &stdout, &stderr, getwd)
		if code != 1 {
			t.Fatalf("RunWithProgramGetwd(unknown) code = %d, want 1", code)
		}
		if stdout.String() != "" {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		assertCLIErrorContains(t, stderr.String(), `custom-agent.exe: unknown command "nope"`, `Run "custom-agent.exe help" for usage.`)
		assertCLIErrorOmits(t, stderr.String(), filepath.Dir(program), `Run "sai help"`)
	})

	t.Run("dynamic command named sai", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := RunWithProgramGetwd(program, []string{"--server-root", serverRoot, "sai"}, &stdout, &stderr, getwd)
		if code != 1 {
			t.Fatalf("RunWithProgramGetwd(unknown sai) code = %d, want 1", code)
		}
		if stdout.String() != "" {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		assertCLIErrorContains(t, stderr.String(), `custom-agent.exe: unknown command "sai"`, `Run "custom-agent.exe help" for usage.`)
		assertCLIErrorOmits(t, stderr.String(), `unknown command "custom-agent.exe"`, `Run "sai help"`)
	})
}

func TestCustomProgramBasenameInProjectGuidanceError(t *testing.T) {
	program := filepath.Join(t.TempDir(), "custom-agent.exe")
	serverRoot := t.TempDir()
	projectDir := t.TempDir()
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithProgramGetwd(program, []string{"--server-root", serverRoot, "session", "create"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 1 {
		t.Fatalf("session create code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "custom-agent.exe: no registered project found from", `run "custom-agent.exe project create"`)
	assertCLIErrorOmits(t, stderr.String(), filepath.Dir(program), `run "sai project create"`)
}

func TestRenderCommandTextPreservesNonCommandSaiIdentifiers(t *testing.T) {
	got := renderCommandText(`usage: sai config show
sai: warning: example
config=sai.yaml
log=sai.jsonl
source=sai_builtin
`, "custom-agent.exe")
	want := `usage: custom-agent.exe config show
custom-agent.exe: warning: example
config=sai.yaml
log=sai.jsonl
source=sai_builtin
`
	if got != want {
		t.Fatalf("renderCommandText() = %q, want %q", got, want)
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
			for _, want := range []string{"usage: sai", "project           Manage registered projects", "session           Manage explicit sessions", "config show", "models list", "doctor", "tools list", "With no command, sai auto-creates a project for the current directory", "then starts a pending session.", `Run "sai help <command>" for command usage.`} {
				if !strings.Contains(out, want) {
					t.Fatalf("stdout = %q, want contain %q", out, want)
				}
			}
			for _, omit := range []string{"attach            Attach to a session", "send              Send one prompt to a session", "server            Manage the selected local HTTP server", "status            Show nearest server status", "stop              Stop nearest server", "servers list", "sessions           Alias", "send              Send one prompt to a server-owned session"} {
				if strings.Contains(out, omit) {
					t.Fatalf("root help still lists removed command %q:\n%s", omit, out)
				}
			}
			if strings.Contains(out, "chat") {
				t.Fatalf("root help still lists chat:\n%s", out)
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

func TestChatCommandIsUnsupportedWithoutConfig(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"chat"}, want: `unknown command "chat"`},
		{args: []string{"chat", "-h"}, want: `unknown command "chat"`},
		{args: []string{"chat", "--help"}, want: `unknown command "chat"`},
		{args: []string{"chat", "--bad"}, want: `unknown command "chat"`},
		{args: []string{"chat", "--quit", "--prompt", "hi"}, want: `unknown command "chat"`},
		{args: []string{"help", "chat"}, want: `unknown help topic "chat"`},
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
			if strings.Contains(stderr.String(), "usage: sai chat") {
				t.Fatalf("stderr included chat usage: %s", stderr.String())
			}
		})
	}
}

func TestServerCommandIsUnsupportedWithoutConfig(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"server"}, want: `unknown command "server"`},
		{args: []string{"server", "-h"}, want: `unknown command "server"`},
		{args: []string{"server", "start", "-h"}, want: `unknown command "server"`},
		{args: []string{"server", "status"}, want: `unknown command "server"`},
		{args: []string{"server", "stop"}, want: `unknown command "server"`},
		{args: []string{"help", "server"}, want: `unknown help topic "server"`},
		{args: []string{"help", "server", "start"}, want: `unknown help topic "server start"`},
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
			assertCLIErrorOmits(t, stderr.String(), "usage: sai server")
		})
	}
}

func TestProjectHelpWritesUsageWithoutConfig(t *testing.T) {
	for _, tt := range []struct {
		args  []string
		wants []string
	}{
		{args: []string{"project", "-h"}, wants: []string{"usage: sai project <command>", "project create", "project list", "project show", "project rename", "project archive", "project remove"}},
		{args: []string{"help", "project"}, wants: []string{"usage: sai project <command>", "project create", "project list", "project show", "project rename", "project archive", "project remove"}},
		{args: []string{"project", "create", "-h"}, wants: []string{"usage: sai project create", "--cwd path", "--name name"}},
		{args: []string{"help", "project", "create"}, wants: []string{"usage: sai project create", "--cwd path", "--name name"}},
		{args: []string{"project", "list", "-h"}, wants: []string{"usage: sai project list [--archived]", "Lists active registered projects"}},
		{args: []string{"help", "project", "list"}, wants: []string{"usage: sai project list [--archived]", "Lists active registered projects"}},
		{args: []string{"project", "show", "-h"}, wants: []string{"usage: sai project show [project-id]", "nearest registered ancestor"}},
		{args: []string{"help", "project", "show"}, wants: []string{"usage: sai project show [project-id]", "nearest registered ancestor"}},
		{args: []string{"project", "rename", "-h"}, wants: []string{"usage: sai project rename [project-id] <name>", "Renames an active project"}},
		{args: []string{"help", "project", "rename"}, wants: []string{"usage: sai project rename [project-id] <name>", "Renames an active project"}},
		{args: []string{"project", "archive", "-h"}, wants: []string{"usage: sai project archive [project-id]", "Archives an active project"}},
		{args: []string{"help", "project", "archive"}, wants: []string{"usage: sai project archive [project-id]", "Archives an active project"}},
		{args: []string{"project", "remove", "-h"}, wants: []string{"usage: sai project remove [project-id]", "Removes an archived project"}},
		{args: []string{"help", "project", "remove"}, wants: []string{"usage: sai project remove [project-id]", "Removes an archived project"}},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(tt.args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})
			if code != 0 {
				t.Fatalf("RunWithGetwd(%v) code = %d, stderr = %s", tt.args, code, stderr.String())
			}
			if stderr.String() != "" {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			out := stdout.String()
			for _, want := range tt.wants {
				if !strings.Contains(out, want) {
					t.Fatalf("stdout = %q, want contain %q", out, want)
				}
			}
		})
	}
}

func TestSessionHelpWritesUsageWithoutConfig(t *testing.T) {
	for _, tt := range []struct {
		args  []string
		wants []string
		omits []string
	}{
		{
			args:  []string{"session", "-h"},
			wants: []string{"usage: sai session <command>", "session create", "session resume", "session list", "session show", "session rename", "session archive", "session remove"},
			omits: []string{"session config", "session update", "session mutate", "session set"},
		},
		{
			args:  []string{"help", "session"},
			wants: []string{"usage: sai session <command>", "session create", "session resume", "session list", "session show", "session rename", "session archive", "session remove"},
			omits: []string{"session config", "session update", "session mutate", "session set"},
		},
		{args: []string{"session", "create", "-h"}, wants: []string{"usage: sai session create", "--cwd path"}},
		{args: []string{"help", "session", "create"}, wants: []string{"usage: sai session create", "--cwd path"}},
		{args: []string{"session", "resume", "-h"}, wants: []string{"usage: sai session resume <session-id>", "resumes the session interactively"}},
		{args: []string{"help", "session", "resume"}, wants: []string{"usage: sai session resume <session-id>", "resumes the session interactively"}},
		{args: []string{"session", "list", "-h"}, wants: []string{"usage: sai session list", "--project project-id", "--all-projects", "--archived"}},
		{args: []string{"help", "session", "list"}, wants: []string{"usage: sai session list", "--project project-id", "--all-projects", "--archived"}},
		{args: []string{"session", "show", "-h"}, wants: []string{"usage: sai session show <session-id>", "explicit global session id"}},
		{args: []string{"help", "session", "show"}, wants: []string{"usage: sai session show <session-id>", "explicit global session id"}},
		{args: []string{"session", "rename", "-h"}, wants: []string{"usage: sai session rename <session-id> <name>", "display name"}},
		{args: []string{"help", "session", "rename"}, wants: []string{"usage: sai session rename <session-id> <name>", "display name"}},
		{args: []string{"session", "archive", "-h"}, wants: []string{"usage: sai session archive <session-id>", "Archives a session"}},
		{args: []string{"help", "session", "archive"}, wants: []string{"usage: sai session archive <session-id>", "Archives a session"}},
		{args: []string{"session", "remove", "-h"}, wants: []string{"usage: sai session remove <session-id>", "Removes an archived session"}},
		{args: []string{"help", "session", "remove"}, wants: []string{"usage: sai session remove <session-id>", "Removes an archived session"}},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			out := assertCLIHelpWithoutConfig(t, tt.args, tt.wants...)
			for _, omit := range tt.omits {
				if strings.Contains(out, omit) {
					t.Fatalf("session help mentioned unexpected config mutation command %q:\n%s", omit, out)
				}
			}
		})
	}
}

func TestProjectCreateUsesExecutionServiceAndPrintsMetadata(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	canonicalRoot, err := projectstore.CanonicalRoot(projectDir)
	if err != nil {
		t.Fatalf("CanonicalRoot(%q) error = %v", projectDir, err)
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "project", "create", "--name", "Current Repo"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd(project create) code = %d, stderr = %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	projects, err := cliProjectStore(t, home).List()
	if err != nil {
		t.Fatalf("project store List() error = %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("stored projects = %#v, want one project", projects)
	}
	project := projects[0]
	if project.Root != canonicalRoot || project.DisplayName != "Current Repo" {
		t.Fatalf("stored project = %#v, want canonical root and display name", project)
	}
	assertCLIOutputContains(t, stdout.String(),
		"STATUS\tcreated\n",
		"ID\t"+project.ID+"\n",
		"ROOT\t"+canonicalRoot+"\n",
		"NAME\tCurrent Repo\n",
		"ARCHIVED\tfalse\n",
		"CREATED_AT\t"+formatSessionTimestamp(project.CreatedAt)+"\n",
		"UPDATED_AT\t"+formatSessionTimestamp(project.UpdatedAt)+"\n",
	)
}

func TestProjectCreateWithCWDPrintsDuplicateExistingMetadata(t *testing.T) {
	home := t.TempDir()
	baseDir := t.TempDir()
	projectDir := filepath.Join(baseDir, "repo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(projectDir) error = %v", err)
	}
	canonicalRoot, err := projectstore.CanonicalRoot(projectDir)
	if err != nil {
		t.Fatalf("CanonicalRoot(%q) error = %v", projectDir, err)
	}
	store := cliProjectStore(t, home)
	existing, created, err := store.Create(projectDir, "Existing Repo")
	if err != nil {
		t.Fatalf("Create(existing) error = %v", err)
	}
	if !created {
		t.Fatal("Create(existing) created = false, want true")
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "project", "create", "--cwd", "repo", "--name", "Ignored Duplicate Name"}, &stdout, &stderr, func() (string, error) {
		return baseDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd(project create duplicate) code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIOutputContains(t, stdout.String(),
		"STATUS\texisting\n",
		"ID\t"+existing.ID+"\n",
		"ROOT\t"+canonicalRoot+"\n",
		"NAME\tExisting Repo\n",
	)
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestProjectListAndShowUseExecutionService(t *testing.T) {
	home := t.TempDir()
	parentDir := t.TempDir()
	childDir := filepath.Join(parentDir, "child")
	leafDir := filepath.Join(childDir, "leaf")
	if err := os.MkdirAll(leafDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(leafDir) error = %v", err)
	}
	store := cliProjectStore(t, home)
	parent, _, err := store.Create(parentDir, "Parent")
	if err != nil {
		t.Fatalf("Create(parent) error = %v", err)
	}
	child, _, err := store.Create(childDir, "Child")
	if err != nil {
		t.Fatalf("Create(child) error = %v", err)
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "project", "list"}, &stdout, &stderr, func() (string, error) {
		return leafDir, nil
	})
	if code != 0 {
		t.Fatalf("project list code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIOutputContains(t, stdout.String(),
		"ID\tNAME\tROOT\tARCHIVED\tCREATED_AT\tUPDATED_AT\n",
		parent.ID+"\tParent\t"+parent.Root+"\tfalse\t"+formatSessionTimestamp(parent.CreatedAt),
		child.ID+"\tChild\t"+child.Root+"\tfalse\t"+formatSessionTimestamp(child.CreatedAt),
	)

	stdout.Reset()
	stderr.Reset()
	code = RunWithGetwd([]string{"--server-root", home, "project", "show", child.ID}, &stdout, &stderr, func() (string, error) {
		return leafDir, nil
	})
	if code != 0 {
		t.Fatalf("project show child code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIOutputContains(t, stdout.String(), "ID\t"+child.ID+"\n", "ROOT\t"+child.Root+"\n", "NAME\tChild\n")

	stdout.Reset()
	stderr.Reset()
	code = RunWithGetwd([]string{"--server-root", home, "project", "show"}, &stdout, &stderr, func() (string, error) {
		return leafDir, nil
	})
	if code != 0 {
		t.Fatalf("project show nearest code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIOutputContains(t, stdout.String(), "ID\t"+child.ID+"\n", "ROOT\t"+child.Root+"\n", "NAME\tChild\n")
	if strings.Contains(stdout.String(), parent.ID) {
		t.Fatalf("project show nearest chose parent instead of child:\n%s", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestProjectExplicitSelectorsDoNotRequireCWDDiscovery(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	store := cliProjectStore(t, home)
	project, _, err := store.Create(projectDir, "Explicit Project")
	if err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	forbidCLIBackgroundStart(t)
	getwd := func() (string, error) {
		return "", errors.New("getwd should not be called")
	}

	tests := []struct {
		name  string
		args  []string
		wants []string
	}{
		{
			name:  "list",
			args:  []string{"--server-root", home, "project", "list"},
			wants: []string{"ID\tNAME\tROOT\tARCHIVED\tCREATED_AT\tUPDATED_AT", project.ID + "\tExplicit Project\t" + project.Root},
		},
		{
			name:  "show",
			args:  []string{"--server-root", home, "project", "show", project.ID},
			wants: []string{"ID\t" + project.ID, "NAME\tExplicit Project", "ROOT\t" + project.Root},
		},
		{
			name:  "rename",
			args:  []string{"--server-root", home, "project", "rename", project.ID, "Renamed Project"},
			wants: []string{"STATUS\trenamed", "ID\t" + project.ID, "NAME\tRenamed Project"},
		},
		{
			name:  "archive",
			args:  []string{"--server-root", home, "project", "archive", project.ID},
			wants: []string{"STATUS\tarchived", "ID\t" + project.ID},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(tt.args, &stdout, &stderr, getwd)
			if code != 0 {
				t.Fatalf("RunWithGetwd(%v) code = %d, stderr = %s", tt.args, code, stderr.String())
			}
			assertCLIOutputContains(t, stdout.String(), tt.wants...)
			if stderr.String() != "" {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}

	removeRoot := t.TempDir()
	removeProject, _, err := store.Create(removeRoot, "Remove Project")
	if err != nil {
		t.Fatalf("Create(remove project) error = %v", err)
	}
	if _, err := store.Archive(removeProject.ID); err != nil {
		t.Fatalf("Archive(remove project) error = %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "project", "remove", removeProject.ID}, &stdout, &stderr, getwd)
	if code != 0 {
		t.Fatalf("RunWithGetwd(project remove explicit) code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIOutputContains(t, stdout.String(), "STATUS\tremoved", "ID\t"+removeProject.ID, "REMOVED_SESSIONS\t0")
}

func TestProjectShowRejectsCWD(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"project", "show", "--cwd", t.TempDir()}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("project show --cwd code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "flag provided but not defined: -cwd", `Run "sai help project show" for usage.`)
}

func TestProjectRemoveUsesNearestArchivedProject(t *testing.T) {
	home := t.TempDir()
	parentDir := t.TempDir()
	childDir := filepath.Join(parentDir, "child")
	leafDir := filepath.Join(childDir, "leaf")
	if err := os.MkdirAll(leafDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(leafDir) error = %v", err)
	}
	store := cliProjectStore(t, home)
	parent, _, err := store.Create(parentDir, "Parent")
	if err != nil {
		t.Fatalf("Create(parent) error = %v", err)
	}
	child, _, err := store.Create(childDir, "Child")
	if err != nil {
		t.Fatalf("Create(child) error = %v", err)
	}
	if _, err := store.Archive(child.ID); err != nil {
		t.Fatalf("Archive(child) error = %v", err)
	}
	sessionStore := cliSessionStore(t, home)
	for _, session := range []sessions.SessionV2{
		{ID: "session-child-a", ProjectID: child.ID},
		{ID: "session-child-b", ProjectID: child.ID},
		{ID: "session-parent", ProjectID: parent.ID},
	} {
		if _, err := sessionStore.SaveMetadata(session); err != nil {
			t.Fatalf("SaveMetadata(%s) error = %v", session.ID, err)
		}
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "project", "remove"}, &stdout, &stderr, func() (string, error) {
		return leafDir, nil
	})
	if code != 0 {
		t.Fatalf("project remove code = %d, stderr = %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	assertCLIOutputContains(t, stdout.String(),
		"STATUS\tremoved\n",
		"ID\t"+child.ID+"\n",
		"REMOVED_SESSIONS\t2\n",
	)
	if _, err := store.Load(child.ID); !errors.Is(err, projectstore.ErrNotFound) {
		t.Fatalf("Load(child after remove) error = %v, want ErrNotFound", err)
	}
	if _, err := store.Load(parent.ID); err != nil {
		t.Fatalf("Load(parent after remove) error = %v, want retained", err)
	}
	for _, id := range []string{"session-child-a", "session-child-b"} {
		if _, err := sessionStore.Load(id); !errors.Is(err, sessions.ErrNotFound) {
			t.Fatalf("Load(%s) error = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := sessionStore.Load("session-parent"); err != nil {
		t.Fatalf("Load(parent session) error = %v, want retained", err)
	}
}

func TestProjectRemoveWithExplicitProjectID(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	store := cliProjectStore(t, home)
	project, _, err := store.Create(projectDir, "Explicit Project")
	if err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	if _, err := store.Archive(project.ID); err != nil {
		t.Fatalf("Archive(project) error = %v", err)
	}
	sessionStore := cliSessionStore(t, home)
	for _, id := range []string{"session-explicit-a", "session-explicit-b", "session-explicit-c"} {
		if _, err := sessionStore.SaveMetadata(sessions.SessionV2{ID: id, ProjectID: project.ID}); err != nil {
			t.Fatalf("SaveMetadata(%s) error = %v", id, err)
		}
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "project", "remove", project.ID}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})
	if code != 0 {
		t.Fatalf("project remove project-explicit code = %d, stderr = %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	assertCLIOutputContains(t, stdout.String(),
		"STATUS\tremoved\n",
		"ID\t"+project.ID+"\n",
		"REMOVED_SESSIONS\t3\n",
	)
}

func TestProjectRemoveRejectsCWD(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"project", "remove", "--cwd", t.TempDir()}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("project remove --cwd code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "flag provided but not defined: -cwd", `Run "sai help project remove" for usage.`)
}

func TestProjectRemoveDoesNotAddCompatibilityAliases(t *testing.T) {
	for _, tt := range []struct {
		args  []string
		wants []string
	}{
		{
			args:  []string{"project", "delete", "project-one"},
			wants: []string{"usage: sai project <create|list|show|rename|archive|remove>", `Run "sai help project" for usage.`},
		},
		{
			args:  []string{"project", "remove", "--delete-data", "project-one"},
			wants: []string{"flag provided but not defined: -delete-data", `Run "sai help project remove" for usage.`},
		},
	} {
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
			assertCLIErrorContains(t, stderr.String(), tt.wants...)
		})
	}
}

func TestProjectCommandsDoNotStartBackgroundServer(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "project", "create", "--name", "No Server"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})
	if code != 0 {
		t.Fatalf("project create code = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "SERVER_ADDR") {
		t.Fatalf("project create stdout = %q, want no unexpected startup output", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = RunWithGetwd([]string{"--server-root", home, "project", "list"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})
	if code != 0 {
		t.Fatalf("project list code = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "SERVER_ADDR") {
		t.Fatalf("project list stdout = %q, want no unexpected startup output", stdout.String())
	}
	assertCLIOutputContains(t, stdout.String(), "ID\tNAME\tROOT\tARCHIVED\tCREATED_AT\tUPDATED_AT")
}

func TestSessionCreateUsesExecutionServiceWithMetadata(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	configDir := filepath.Join(projectDir, ".agents")
	writeCLIRunFixtureInDirWithTools(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat", []string{"read_file"})
	writeCLIMCPFixture(t, configDir)
	setCLIAgentShowReasoning(t, configDir, true)
	skillDir := filepath.Join(configDir, "skills", "visible")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(skillDir) error = %v", err)
	}
	writeCLIFile(t, filepath.Join(skillDir, "SKILL.md"), "---\nname: Visible Skill\n---\nVisible skill instructions\n")
	hiddenSkillDir := filepath.Join(configDir, "skills", "hidden")
	if err := os.MkdirAll(hiddenSkillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(hiddenSkillDir) error = %v", err)
	}
	writeCLIFile(t, filepath.Join(hiddenSkillDir, "SKILL.md"), "---\ndisable-model-invocation: true\n---\nHidden skill instructions\n")

	canonicalRoot, err := projectstore.CanonicalRoot(projectDir)
	if err != nil {
		t.Fatalf("CanonicalRoot(%q) error = %v", projectDir, err)
	}
	configPath := filepath.Join(configDir, "sai.yaml")
	project, _, err := cliProjectStore(t, home).Create(projectDir, "Current")
	if err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "session", "create"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("session create code = %d, stderr = %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	infos, err := cliSessionStore(t, home).ListWithOptions(sessions.V2ListOptions{})
	if err != nil {
		t.Fatalf("session store ListWithOptions() error = %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("stored sessions = %#v, want one session", infos)
	}
	session, err := cliSessionStore(t, home).Load(infos[0].ID)
	if err != nil {
		t.Fatalf("Load(created session) error = %v", err)
	}
	for key, want := range map[string]string{
		"created_cwd":   canonicalRoot,
		"config_path":   configPath,
		"provider":      "fake",
		"model_profile": "default",
		"model_id":      "model-default",
	} {
		var got string
		switch key {
		case "created_cwd":
			got = session.CreatedCWD
		case "config_path":
			got = session.RootConfigPath()
		case "provider":
			got = session.Provider
		case "model_profile":
			got = session.ModelProfile
		case "model_id":
			got = session.ModelID
		}
		if got != want {
			t.Fatalf("stored session %s = %q, want %q; session=%#v", key, got, want, session)
		}
	}
	if session.ProjectID != project.ID {
		t.Fatalf("stored session ProjectID = %q, want %q", session.ProjectID, project.ID)
	}
	if got := fmt.Sprint(session.ModelParameters["max_tokens"]); got != "128" {
		t.Fatalf("model_parameters.max_tokens = %s, want 128; session=%#v", got, session)
	}
	if !reflect.DeepEqual(session.EnabledTools, []string{"read_file"}) {
		t.Fatalf("enabled_tools = %#v, want read_file", session.EnabledTools)
	}
	if !reflect.DeepEqual(session.EnabledMCP, []string{"local"}) {
		t.Fatalf("enabled_mcp = %#v, want local", session.EnabledMCP)
	}
	if !reflect.DeepEqual(session.EnabledSkills, []string{"visible"}) {
		t.Fatalf("enabled_skills = %#v, want visible", session.EnabledSkills)
	}
	if !session.ShowReasoning {
		t.Fatalf("show_reasoning = false, want true; session=%#v", session)
	}
	if !session.SaveToolResults {
		t.Fatalf("save_tool_results = false, want true; session=%#v", session)
	}
	if session.Context.ContextWindow != 32000 || session.Context.ContextWindowSource != string(contextwindow.WindowSourceEstimated) {
		t.Fatalf("context = %#v, want estimated 32000", session.Context)
	}
	out := stdout.String()
	assertCLIOutputContains(t, out,
		"STATUS\tcreated",
		"ID\t"+session.ID,
		"PROJECT_ID\t"+project.ID,
		"CREATED_CWD\t"+canonicalRoot,
		"ARCHIVED\tfalse",
		"LAST_USED_AT\t"+formatSessionTimestamp(session.LastUsedAt),
	)
	if strings.Contains(out, "Hidden skill instructions") || strings.Contains(out, "direct-secret-value") {
		t.Fatalf("session create output leaked hidden data:\n%s", out)
	}
}

func TestSessionCreateFailsWithoutRegisteredNearestProject(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "session", "create"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 1 {
		t.Fatalf("session create code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "no registered project found from", `run "sai project create"`)
}

func TestSessionListDefaultUsesNearestProjectScope(t *testing.T) {
	home := t.TempDir()
	parentDir := t.TempDir()
	childDir := filepath.Join(parentDir, "child")
	leafDir := filepath.Join(childDir, "leaf")
	if err := os.MkdirAll(leafDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(leafDir) error = %v", err)
	}
	projectStore := cliProjectStore(t, home)
	parent, _, err := projectStore.Create(parentDir, "Parent")
	if err != nil {
		t.Fatalf("Create(parent) error = %v", err)
	}
	child, _, err := projectStore.Create(childDir, "Child")
	if err != nil {
		t.Fatalf("Create(child) error = %v", err)
	}
	sessionStore := cliSessionStore(t, home)
	updatedAt := time.Date(2026, 7, 4, 6, 1, 0, 0, time.UTC)
	if _, err := sessionStore.SaveMetadata(sessions.SessionV2{
		ID:           "parent-session",
		ProjectID:    parent.ID,
		CreatedCWD:   parent.Root,
		LastUsedAt:   updatedAt.Add(time.Minute),
		Provider:     "fake",
		ModelProfile: "default",
		ModelID:      "model-default",
	}); err != nil {
		t.Fatalf("SaveMetadata(parent session) error = %v", err)
	}
	if _, err := sessionStore.SaveMetadata(sessions.SessionV2{
		ID:           "child-session",
		ProjectID:    child.ID,
		CreatedCWD:   child.Root,
		LastUsedAt:   updatedAt,
		Provider:     "fake",
		ModelProfile: "default",
		ModelID:      "model-default",
	}); err != nil {
		t.Fatalf("SaveMetadata(child session) error = %v", err)
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "session", "list"}, &stdout, &stderr, func() (string, error) {
		return leafDir, nil
	})

	if code != 0 {
		t.Fatalf("session list code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIOutputContains(t, stdout.String(),
		"ID\tPROJECT_ID\tNAME\tCREATED_CWD\tARCHIVED\tLAST_USED_AT",
		"child-session\t"+child.ID+"\t\t"+child.Root+"\tfalse\t2026-07-04T06:01:00Z",
	)
	if strings.Contains(stdout.String(), "parent-session") {
		t.Fatalf("session list included parent session outside nearest project:\n%s", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestSessionListArchivedUsesProjectScopedFilter(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	project, _, err := cliProjectStore(t, home).Create(projectDir, "Current")
	if err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	sessionStore := cliSessionStore(t, home)
	updatedAt := time.Date(2026, 7, 4, 6, 30, 0, 0, time.UTC)
	lastUsedAt := updatedAt.Add(5 * time.Minute)
	if _, err := sessionStore.SaveMetadata(sessions.SessionV2{
		ID:           "active-session",
		ProjectID:    project.ID,
		CreatedCWD:   project.Root,
		LastUsedAt:   updatedAt,
		Provider:     "fake",
		ModelProfile: "default",
		ModelID:      "model-default",
	}); err != nil {
		t.Fatalf("SaveMetadata(active session) error = %v", err)
	}
	if _, err := sessionStore.SaveMetadata(sessions.SessionV2{
		ID:           "archived-session",
		ProjectID:    project.ID,
		CreatedCWD:   project.Root,
		LastUsedAt:   lastUsedAt,
		DisplayName:  "Old Thread",
		Archived:     true,
		Provider:     "fake",
		ModelProfile: "default",
		ModelID:      "model-default",
	}); err != nil {
		t.Fatalf("SaveMetadata(archived session) error = %v", err)
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "session", "list", "--archived"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("session list --archived code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIOutputContains(t, stdout.String(),
		"ID\tPROJECT_ID\tNAME\tCREATED_CWD\tARCHIVED\tLAST_USED_AT",
		"archived-session\t"+project.ID+"\tOld Thread\t"+project.Root+"\ttrue\t2026-07-04T06:35:00Z",
	)
	if strings.Contains(stdout.String(), "active-session") {
		t.Fatalf("session list --archived included active session:\n%s", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestSessionListProjectUsesExecutionServiceWithoutCWDDiscovery(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	project, _, err := cliProjectStore(t, home).Create(projectDir, "Direct Project")
	if err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	updatedAt := time.Date(2026, 7, 4, 7, 1, 0, 0, time.UTC)
	if _, err := cliSessionStore(t, home).SaveMetadata(sessions.SessionV2{
		ID:           "direct-session",
		ProjectID:    project.ID,
		CreatedCWD:   project.Root,
		LastUsedAt:   updatedAt,
		Provider:     "fake",
		ModelProfile: "default",
		ModelID:      "model-default",
	}); err != nil {
		t.Fatalf("SaveMetadata(direct session) error = %v", err)
	}
	forbidCLIBackgroundStart(t)
	getwd := func() (string, error) {
		return "", errors.New("getwd should not be called")
	}

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "session", "list", "--project", project.ID}, &stdout, &stderr, getwd)

	if code != 0 {
		t.Fatalf("session list --project code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIOutputContains(t, stdout.String(),
		"ID\tPROJECT_ID\tNAME\tCREATED_CWD\tARCHIVED\tLAST_USED_AT",
		"direct-session\t"+project.ID+"\t\t"+project.Root+"\tfalse\t2026-07-04T07:01:00Z",
	)
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = RunWithGetwd([]string{"--server-root", home, "session", "list", "--project", "missing-project"}, &stdout, &stderr, getwd)
	if code != 1 {
		t.Fatalf("session list missing project code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "project not found: missing-project")

	archivedProjectRoot := t.TempDir()
	archivedProject, _, err := cliProjectStore(t, home).Create(archivedProjectRoot, "Archived Project")
	if err != nil {
		t.Fatalf("Create(archived project) error = %v", err)
	}
	if _, err := cliProjectStore(t, home).Archive(archivedProject.ID); err != nil {
		t.Fatalf("Archive(project) error = %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = RunWithGetwd([]string{"--server-root", home, "session", "list", "--project", archivedProject.ID}, &stdout, &stderr, getwd)
	if code != 1 {
		t.Fatalf("session list archived project code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "project is archived")
}

func TestSessionListAllProjectsUsesExecutionServiceAndRejectsProjectCombination(t *testing.T) {
	home := t.TempDir()
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	projectStore := cliProjectStore(t, home)
	first, _, err := projectStore.Create(firstRoot, "First")
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	second, _, err := projectStore.Create(secondRoot, "Second")
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	sessionStore := cliSessionStore(t, home)
	updatedAt := time.Date(2026, 7, 4, 8, 1, 0, 0, time.UTC)
	for _, session := range []sessions.SessionV2{
		{ID: "global-first-active", ProjectID: first.ID, CreatedCWD: first.Root, LastUsedAt: updatedAt.Add(2 * time.Minute), Provider: "fake", ModelProfile: "default", ModelID: "model-default"},
		{ID: "global-second-active", ProjectID: second.ID, CreatedCWD: second.Root, LastUsedAt: updatedAt.Add(time.Minute), Provider: "fake", ModelProfile: "default", ModelID: "model-default"},
		{ID: "global-first-archived", ProjectID: first.ID, CreatedCWD: first.Root, LastUsedAt: updatedAt.Add(3 * time.Minute), Archived: true, Provider: "fake", ModelProfile: "default", ModelID: "model-default"},
	} {
		if _, err := sessionStore.SaveMetadata(session); err != nil {
			t.Fatalf("SaveMetadata(%s) error = %v", session.ID, err)
		}
	}
	forbidCLIBackgroundStart(t)
	getwd := func() (string, error) {
		return "", errors.New("getwd should not be called")
	}

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "session", "list", "--all-projects"}, &stdout, &stderr, getwd)

	if code != 0 {
		t.Fatalf("session list --all-projects code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIOutputContains(t, stdout.String(),
		"ID\tPROJECT_ID\tNAME\tCREATED_CWD\tARCHIVED\tLAST_USED_AT",
		"global-first-active\t"+first.ID+"\t\t"+first.Root+"\tfalse\t2026-07-04T08:03:00Z",
		"global-second-active\t"+second.ID+"\t\t"+second.Root+"\tfalse\t2026-07-04T08:02:00Z",
	)
	if strings.Contains(stdout.String(), "global-first-archived") {
		t.Fatalf("session list --all-projects included archived session without --archived:\n%s", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = RunWithGetwd([]string{"--server-root", home, "session", "list", "--all-projects", "--archived"}, &stdout, &stderr, getwd)
	if code != 0 {
		t.Fatalf("session list --all-projects --archived code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIOutputContains(t, stdout.String(),
		"ID\tPROJECT_ID\tNAME\tCREATED_CWD\tARCHIVED\tLAST_USED_AT",
		"global-first-archived\t"+first.ID+"\t\t"+first.Root+"\ttrue\t2026-07-04T08:04:00Z",
	)
	if strings.Contains(stdout.String(), "global-first-active") || strings.Contains(stdout.String(), "global-second-active") {
		t.Fatalf("session list --all-projects --archived included active sessions:\n%s", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = RunWithGetwd([]string{"--server-root", home, "session", "list", "--project", first.ID, "--all-projects"}, &stdout, &stderr, getwd)
	if code != 1 {
		t.Fatalf("session list conflicting flags code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "--project cannot be combined with --all-projects", `Run "sai help session list" for usage.`)
}

func TestSessionMetadataCommandsRejectConfigBeforeDiscovery(t *testing.T) {
	home := t.TempDir()
	missingConfig := filepath.Join(t.TempDir(), "missing.yaml")
	tests := []struct {
		name     string
		args     []string
		helpHint string
	}{
		{
			name:     "session list",
			args:     []string{"--config", missingConfig, "session", "list"},
			helpHint: `Run "sai help session list" for usage.`,
		},
		{
			name:     "session show",
			args:     []string{"--config", missingConfig, "session", "show", "show-session"},
			helpHint: `Run "sai help session show" for usage.`,
		},
		{
			name:     "session rename",
			args:     []string{"session", "rename", "rename-session", "New Name", "--config", missingConfig},
			helpHint: `Run "sai help session rename" for usage.`,
		},
		{
			name:     "session archive",
			args:     []string{"session", "archive", "archive-session", "--config", missingConfig},
			helpHint: `Run "sai help session archive" for usage.`,
		},
		{
			name:     "session remove",
			args:     []string{"session", "remove", "remove-session", "--config", missingConfig},
			helpHint: `Run "sai help session remove" for usage.`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			forbidCLIBackgroundStart(t)

			var stdout, stderr bytes.Buffer
			args := append([]string{"--server-root", home}, tt.args...)
			code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})

			if code != 1 {
				t.Fatalf("RunWithGetwd(%v) code = %d, want 1", args, code)
			}
			if stdout.String() != "" {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			assertCLIErrorContains(t, stderr.String(), "--config can only be used when creating a new session", tt.helpHint)
			assertCLIErrorOmits(t, stderr.String(), missingConfig, "getwd should not be called")
		})
	}
}

func TestSessionShowUsesExecutionServiceAndRejectsCWD(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	configPath := filepath.Join(projectDir, ".agents", "sai.yaml")
	updatedAt := time.Date(2026, 7, 4, 9, 1, 0, 0, time.UTC)
	if _, err := cliSessionStore(t, home).SaveMetadata(sessions.SessionV2{
		ID:              "show-session",
		CreatedAt:       updatedAt,
		LastUsedAt:      updatedAt,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		ProjectID:       "project-current",
		CreatedCWD:      projectDir,
		CWD:             projectDir,
		ConfigPath:      configPath,
		ShowReasoning:   false,
		SaveToolResults: true,
	}); err != nil {
		t.Fatalf("SaveMetadata(show session) error = %v", err)
	}
	forbidCLIBackgroundStart(t)
	getwd := func() (string, error) {
		return "", errors.New("getwd should not be called")
	}

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "session", "show", "show-session"}, &stdout, &stderr, getwd)

	if code != 0 {
		t.Fatalf("session show code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIOutputContains(t, stdout.String(),
		"ID\tshow-session",
		"PROJECT_ID\tproject-current",
		"CREATED_CWD\t"+projectDir,
		"CONFIG_PATH\t"+configPath,
		"SAVE_TOOL_RESULTS\ttrue",
	)
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = RunWithGetwd([]string{"--server-root", home, "session", "show", "--cwd", projectDir, "show-session"}, &stdout, &stderr, getwd)
	if code != 1 {
		t.Fatalf("session show --cwd code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "flag provided but not defined: -cwd", `Run "sai help session show" for usage.`)
}

func TestSessionRenameAndArchiveUseExecutionService(t *testing.T) {
	home := t.TempDir()
	updatedAt := time.Date(2026, 7, 4, 9, 30, 0, 0, time.UTC)
	lastUsedAt := updatedAt.Add(-10 * time.Minute)
	sessionStore := cliSessionStore(t, home)
	if _, err := sessionStore.SaveMetadata(sessions.SessionV2{
		ID:              "rename-session",
		CreatedAt:       updatedAt,
		LastUsedAt:      lastUsedAt,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		ProjectID:       "project-current",
		ShowReasoning:   false,
		SaveToolResults: true,
	}); err != nil {
		t.Fatalf("SaveMetadata(rename session) error = %v", err)
	}
	if _, err := sessionStore.SaveMetadata(sessions.SessionV2{
		ID:              "archive-session",
		CreatedAt:       updatedAt,
		LastUsedAt:      lastUsedAt,
		DisplayName:     "Old Session",
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		ProjectID:       "project-current",
		ShowReasoning:   false,
		SaveToolResults: true,
	}); err != nil {
		t.Fatalf("SaveMetadata(archive session) error = %v", err)
	}
	forbidCLIBackgroundStart(t)
	getwd := func() (string, error) {
		return "", errors.New("getwd should not be called")
	}

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "session", "rename", "rename-session", "Renamed Session"}, &stdout, &stderr, getwd)
	if code != 0 {
		t.Fatalf("session rename code = %d, stderr = %s", code, stderr.String())
	}
	renamed, err := sessionStore.Load("rename-session")
	if err != nil {
		t.Fatalf("Load(rename session) error = %v", err)
	}
	if renamed.DisplayName != "Renamed Session" || renamed.Archived {
		t.Fatalf("renamed session = %#v, want renamed active session", renamed)
	}
	assertCLIOutputContains(t, stdout.String(),
		"STATUS\trenamed",
		"ID\trename-session",
		"LAST_USED_AT\t2026-07-04T09:20:00Z",
		"NAME\tRenamed Session",
		"ARCHIVED\tfalse",
	)
	if stderr.String() != "" {
		t.Fatalf("rename stderr = %q, want empty", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = RunWithGetwd([]string{"--server-root", home, "session", "archive", "archive-session"}, &stdout, &stderr, getwd)
	if code != 0 {
		t.Fatalf("session archive code = %d, stderr = %s", code, stderr.String())
	}
	archived, err := sessionStore.Load("archive-session")
	if err != nil {
		t.Fatalf("Load(archive session) error = %v", err)
	}
	if !archived.Archived || archived.DisplayName != "Old Session" {
		t.Fatalf("archived session = %#v, want archived existing name", archived)
	}
	assertCLIOutputContains(t, stdout.String(),
		"STATUS\tarchived",
		"ID\tarchive-session",
		"NAME\tOld Session",
		"ARCHIVED\ttrue",
	)
	if stderr.String() != "" {
		t.Fatalf("archive stderr = %q, want empty", stderr.String())
	}
}

func TestSessionRemoveUsesExecutionService(t *testing.T) {
	home := t.TempDir()
	sessionStore := cliSessionStore(t, home)
	if _, err := sessionStore.SaveMetadata(sessions.SessionV2{
		ID:           "remove-active",
		ProjectID:    "project-current",
		Provider:     "fake",
		ModelProfile: "default",
		ModelID:      "model-default",
	}); err != nil {
		t.Fatalf("SaveMetadata(active session) error = %v", err)
	}
	if _, err := sessionStore.SaveMetadata(sessions.SessionV2{
		ID:           "remove-archived",
		ProjectID:    "project-current",
		Archived:     true,
		Provider:     "fake",
		ModelProfile: "default",
		ModelID:      "model-default",
	}); err != nil {
		t.Fatalf("SaveMetadata(archived session) error = %v", err)
	}
	forbidCLIBackgroundStart(t)
	getwd := func() (string, error) {
		return "", errors.New("getwd should not be called")
	}

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "session", "remove", "remove-active"}, &stdout, &stderr, getwd)
	if code != 1 {
		t.Fatalf("session remove active code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "archive session before removing it")

	stdout.Reset()
	stderr.Reset()
	code = RunWithGetwd([]string{"--server-root", home, "session", "remove", "remove-archived"}, &stdout, &stderr, getwd)
	if code != 0 {
		t.Fatalf("session remove archived code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIOutputContains(t, stdout.String(),
		"STATUS\tremoved\n",
		"ID\tremove-archived\n",
	)
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if _, err := sessionStore.Load("remove-archived"); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("Load(remove-archived) error = %v, want ErrNotFound", err)
	}
	if _, err := sessionStore.Load("remove-active"); err != nil {
		t.Fatalf("Load(remove-active) error = %v, want retained active session", err)
	}
}

func TestSessionRenameAndArchiveUsageErrors(t *testing.T) {
	for _, tt := range []struct {
		name  string
		args  []string
		wants []string
	}{
		{name: "rename missing name", args: []string{"session", "rename", "session-id"}, wants: []string{"usage: sai session rename <session-id> <name>", `Run "sai help session rename" for usage.`}},
		{name: "rename blank name", args: []string{"session", "rename", "session-id", "   "}, wants: []string{"session display name must be a non-empty string", `Run "sai help session rename" for usage.`}},
		{name: "archive extra arg", args: []string{"session", "archive", "session-id", "extra"}, wants: []string{"usage: sai session archive <session-id>", `Run "sai help session archive" for usage.`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
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
			assertCLIErrorContains(t, stderr.String(), tt.wants...)
		})
	}
}

func TestSessionCommandsDoNotStartBackgroundServer(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	writeCLIRunFixtureInDir(t, filepath.Join(projectDir, ".agents"), "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
	project, _, err := cliProjectStore(t, home).Create(projectDir, "No Server")
	if err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--server-root", home, "session", "create"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})
	if code != 0 {
		t.Fatalf("session create code = %d, stderr = %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if strings.Contains(stdout.String(), "SERVER_ADDR") {
		t.Fatalf("stdout = %q, want no SERVER_ADDR", stdout.String())
	}
	assertCLIOutputContains(t, stdout.String(), "STATUS\tcreated", "PROJECT_ID\t"+project.ID)

	stdout.Reset()
	stderr.Reset()
	code = RunWithGetwd([]string{"--server-root", home, "session", "list", "--all-projects"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})
	if code != 0 {
		t.Fatalf("session list --all-projects code = %d, stderr = %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if strings.Contains(stdout.String(), "SERVER_ADDR") {
		t.Fatalf("stdout = %q, want no SERVER_ADDR", stdout.String())
	}
	assertCLIOutputContains(t, stdout.String(), "ID\tPROJECT_ID\tNAME\tCREATED_CWD\tARCHIVED\tLAST_USED_AT")
}

func TestBareDefaultAttachPendingSessionWithoutDurableRequests(t *testing.T) {
	isolateCLIUserRegistry(t)
	home := cliDefaultServerRoot(t)
	projectDir := t.TempDir()
	writeCLIFixtureInDir(t, filepath.Join(projectDir, ".agents"))
	if _, _, err := cliProjectStore(t, home).Create(projectDir, "Current"); err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO(nil, strings.NewReader("/quit\n"), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("bare attach code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if strings.Contains(stderr.String(), "SERVER_ADDR") {
		t.Fatalf("stderr = %q, want no SERVER_ADDR", stderr.String())
	}
	if strings.Contains(stderr.String(), "attached to session") {
		t.Fatalf("stderr = %q, want no durable attach notice", stderr.String())
	}
	infos, err := cliSessionStore(t, home).ListWithOptions(sessions.V2ListOptions{All: true})
	if err != nil {
		t.Fatalf("ListWithOptions() error = %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("sessions = %#v, want none before first prompt", infos)
	}
}

func TestPendingAttachNoIDAllowsConfigAndCWDImmediateQuit(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "child")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(childDir) error = %v", err)
	}
	missingConfig := filepath.Join(t.TempDir(), "custom.yaml")
	if _, _, err := cliProjectStore(t, home).Create(projectDir, "Current"); err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--server-root", home, "--config", missingConfig, "--cwd", childDir}, strings.NewReader("/quit\n"), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("pending attach code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	infos, err := cliSessionStore(t, home).ListWithOptions(sessions.V2ListOptions{All: true})
	if err != nil {
		t.Fatalf("ListWithOptions() error = %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("sessions = %#v, want none before first prompt", infos)
	}
	assertCLIErrorOmits(t, stderr.String(), "attached to session")
}

func TestServersCommandRemoved(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"servers", "list"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})
	if code != 1 {
		t.Fatalf("servers list code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `unknown command "servers"`, `Run "sai help" for usage.`)
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

func TestSendCommandIsUnsupported(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"send", "-h"}, want: `unknown command "send"`},
		{args: []string{"send", "--help"}, want: `unknown command "send"`},
		{args: []string{"send", "--prompt", "hello"}, want: `unknown command "send"`},
		{args: []string{"help", "send"}, want: `unknown help topic "send"`},
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
			assertCLIErrorOmits(t, stderr.String(), "usage: sai send", "server-owned session")
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertCLIHelpWithoutConfig(t, tt.args, tt.wants...)
		})
	}
}

func TestPluralSessionsCommandIsRemoved(t *testing.T) {
	tests := []struct {
		args []string
		want string
		hint string
	}{
		{args: []string{"sessions"}, want: `unknown command "sessions"`, hint: `Run "sai help" for usage.`},
		{args: []string{"sessions", "-h"}, want: `unknown command "sessions"`, hint: `Run "sai help" for usage.`},
		{args: []string{"sessions", "list"}, want: `unknown command "sessions"`, hint: `Run "sai help" for usage.`},
		{args: []string{"sessions", "list", "-h"}, want: `unknown command "sessions"`, hint: `Run "sai help" for usage.`},
		{args: []string{"help", "sessions"}, want: `unknown help topic "sessions"`, hint: `Run "sai help" for usage.`},
		{args: []string{"help", "sessions", "list"}, want: `unknown help topic "sessions list"`, hint: `Run "sai help" for usage.`},
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
			assertCLIErrorContains(t, stderr.String(), tt.want, tt.hint)
			for _, forbidden := range []string{"usage: sai sessions", `Alias for "sai session list"`} {
				if strings.Contains(stderr.String(), forbidden) {
					t.Fatalf("removed sessions command printed %q:\n%s", forbidden, stderr.String())
				}
			}
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
	code := runInProcessRuntimeWithIO([]string{"chat", "--bad"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"chat", "ignored", "-h", "--config", filepath.Join(t.TempDir(), "missing")}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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

func TestAttachCommandIsUnsupported(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"attach", "-h"}, want: `unknown command "attach"`},
		{args: []string{"attach", "--help"}, want: `unknown command "attach"`},
		{args: []string{"attach", "existing-session"}, want: `unknown command "attach"`},
		{args: []string{"help", "attach"}, want: `unknown help topic "attach"`},
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
			assertCLIErrorOmits(t, stderr.String(), "usage: sai attach", "server-owned session")
		})
	}
}

func TestBareNewFlagIsUnsupported(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--new"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd(--new) code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "flag provided but not defined: -new", `Run "sai help" for usage.`)
}

func TestChatQuitWithoutPromptIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithIO([]string{"chat", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
			code := runInProcessRuntimeWithIO(tt.args(configDir), strings.NewReader("stdin prompt\nline two\n"), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--file", promptPath}, strings.NewReader("unused\n"), &stdout, &stderr, func() (string, error) {
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
			code := runInProcessRuntimeWithIO(tt.args(configDir), tt.stdin, &stdout, &stderr, func() (string, error) {
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
			code := runInProcessRuntimeWithIO(tt.args, strings.NewReader("stdin"), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("/exit\n"), &stdout, &stderr, func() (string, error) {
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

func TestChatAcceptsModelAndEnableToolsFlagsBeforeCommand(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "--model", "fast", "--enable-tools", "read_file", "chat", "--prompt", "first", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "first"}, strings.NewReader("second\n"), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"chat", "--config", cliConfigPath(configDir), "--quit", "--prompt", "first"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"chat", "--quit", "--", "--help"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"chat", "--config", cliConfigPath(configDir), "--quit", "--", "--help"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"chat", "--config", cliConfigPath(configDir), "--quit", "--", "--config"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--prompt", "first", "--quit"}, strings.NewReader("second\n"), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--prompt", "first", "--model", "fast", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--model", "fast", "chat", "--config", cliConfigPath(configDir), "--prompt", "first", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--prompt", "first", "--enable-tools", "read_file", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--enable-tools", "read_file", "chat", "--config", cliConfigPath(configDir), "--prompt", "first", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"chat", "first", "--quit", "--prompt", "second"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"chat", "--prompt", "first", "--bad"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--prompt", "first"}, strings.NewReader("second\n/quit\n"), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("first\n\nsecond\n/quit\n"), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("/usage\n/quit\n"), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("first\n/usage\n/quit\n"), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("first\n/usage\n/quit\n"), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--enable-tools", "read_file"}, strings.NewReader("user prompt secret\n/usage\n/quit\n"), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader(input), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("\"\"\"\n\"\"\"\n/quit\n"), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("failed\nsecond\n/quit\n"), &stdout, &stderr, func() (string, error) {
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
		done <- runInProcessRuntimeWithInterrupts(context.Background(), "sai", []string{"--config", cliConfigPath(configDir), "chat"}, stdinReader, &stdout, stderr, func() (string, error) {
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
		done <- runInProcessRuntimeWithInterrupts(context.Background(), "sai", []string{"--config", cliConfigPath(configDir), "chat"}, stdinReader, &stdout, stderr, func() (string, error) {
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
		done <- runInProcessRuntimeWithInterrupts(context.Background(), "sai", []string{"--config", cliConfigPath(configDir), "chat"}, stdinReader, &stdout, stderr, func() (string, error) {
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
		done <- runInProcessRuntimeWithInterrupts(context.Background(), "sai", []string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--prompt", "first"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	session := loadOnlyCLISession(t, filepath.Join(configDir, "sessions"))
	if session.RunningTurnID != "" {
		t.Fatalf("RunningTurnID = %q, want cleared after interrupt", session.RunningTurnID)
	}
	if session.InterruptedTurnID == "" {
		t.Fatalf("InterruptedTurnID is empty, want interrupted turn metadata: %#v", session)
	}
	if len(session.Items) != 2 {
		t.Fatalf("len(session.Items) = %d, want runtime context plus user prompt: %#v", len(session.Items), session.Items)
	}
	active := activeCLIMessages(t, session)
	if len(active) != 2 {
		t.Fatalf("len(active messages) = %d, want runtime context plus user prompt: %#v", len(active), active)
	}
	assertSavedMessage(t, active, 0, model.MessageRoleSystem, builtInBaseInstructions)
	assertSavedMessage(t, active, 1, model.MessageRoleUser, "first")
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("first\nsecond\n/quit\n"), failingWriter{err: stdoutErr}, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--enable-tools", "read_file"}, strings.NewReader("Read note\nNext\n/exit\n"), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "user prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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

func TestCustomProgramBasenameInContextWindowWarning(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"assistant secret"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	program := filepath.Join(t.TempDir(), "custom-agent.exe")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLIModelContextWindow(t, configDir, 260)

	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithInterrupts(context.Background(), program, []string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "user prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	}, nil)

	if code != 0 {
		t.Fatalf("RunWithProgram(chat) code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "assistant secret"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	errOut := stderr.String()
	assertCLIErrorContains(t, errOut, "custom-agent.exe: warning: estimated context usage", "/260 tokens", "no context was truncated")
	assertCLIErrorOmits(t, errOut, "sai: warning:", filepath.Dir(program), "user prompt secret", "assistant secret", "direct-secret-value")
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestCustomProgramBasenameInInstructionFileWarning(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"assistant"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	program := filepath.Join(t.TempDir(), "custom-agent.exe")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLIInstructionFiles(t, configDir, []string{"$REPO/AGENTS.md"})

	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithInterrupts(context.Background(), program, []string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "hello"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	}, nil)

	if code != 0 {
		t.Fatalf("RunWithProgram(chat) code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "assistant"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	errOut := stderr.String()
	assertCLIErrorContains(t, errOut, "custom-agent.exe: warning: skipping instruction file entry", "$REPO could not be resolved")
	assertCLIErrorOmits(t, errOut, "sai: warning:", filepath.Dir(program), "direct-secret-value")
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "user prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "read_file", "--prompt", "user prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "first"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--enable-tools", "read_file", "--prompt", "Read note"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	sessionDir := filepath.Join(configDir, "sessions", session.ID)
	if _, err := os.Stat(filepath.Join(sessionDir, "meta.json")); err != nil {
		t.Fatalf("meta.json stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessionDir, "segments")); err != nil {
		t.Fatalf("segments stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessionDir, "session.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session.json stat error = %v, want not exist", err)
	}
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
	messages := activeCLIMessages(t, session)
	if len(messages) != 5 {
		t.Fatalf("len(saved messages) = %d, want 5: %#v", len(messages), messages)
	}
	assertSavedMessage(t, messages, 0, model.MessageRoleSystem, builtInBaseInstructions)
	assertSavedMessage(t, messages, 1, model.MessageRoleUser, "Read note")
	assertSavedAssistantToolCallMessage(t, messages, 2, "call_1", "read_file", `{"path":"note.txt"}`)
	assertSavedToolMessage(t, messages, 3, "call_1", "tool output")
	assertSavedMessage(t, messages, 4, model.MessageRoleAssistant, "done")
	var toolItems []sessions.SessionItem
	for _, item := range session.Items {
		if item.Message != nil && item.Message.Role == model.MessageRoleTool {
			toolItems = append(toolItems, item)
		}
	}
	if len(toolItems) != 1 {
		t.Fatalf("tool item count = %d, want 1: %#v", len(toolItems), session.Items)
	}
	if toolItems[0].Status != sessions.ItemStatusCompleted {
		t.Fatalf("tool item status = %q, want completed: %#v", toolItems[0].Status, toolItems[0])
	}
	if session.RunningTurnID != "" {
		t.Fatalf("RunningTurnID = %q, want cleared", session.RunningTurnID)
	}
	events, err := sessions.NewV2Store(filepath.Join(configDir, "sessions")).PersistedEventsAfter(session.ID, 0)
	if err != nil {
		t.Fatalf("PersistedEventsAfter() error = %v", err)
	}
	var updatedEvents int
	for _, event := range events {
		if event.Type == sessions.RecordTypeItemUpdated && event.ItemID == toolItems[0].ID {
			updatedEvents++
		}
	}
	if updatedEvents != 1 {
		t.Fatalf("tool item.updated events = %d, want 1: %#v", updatedEvents, events)
	}
}

func TestChatSaveSessionFlagWritesMultiToolHistoryIncrementally(t *testing.T) {
	firstCommand := blockingShellCommandForCLIReleaseFile("release-one.txt", "first tool output")
	secondCommand := blockingShellCommandForCLIReleaseFile("release-two.txt", "second tool output")
	firstArgs, err := json.Marshal(map[string]any{
		"command":    firstCommand,
		"timeout_ms": 5000,
	})
	if err != nil {
		t.Fatalf("Marshal(first shell args) error = %v", err)
	}
	secondArgs, err := json.Marshal(map[string]any{
		"command":    secondCommand,
		"timeout_ms": 5000,
	})
	if err != nil {
		t.Fatalf("Marshal(second shell args) error = %v", err)
	}
	toolChunk := fmt.Sprintf(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"shell","arguments":%q}},{"index":1,"id":"call_2","function":{"name":"shell","arguments":%q}}]}}]}`,
		string(firstArgs),
		string(secondArgs),
	)
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			toolChunk,
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
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	type runOutcome struct {
		code   int
		stdout string
		stderr string
	}
	done := make(chan runOutcome, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--enable-tools", "shell", "--prompt", "Run both tools"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
			return projectDir, nil
		})
		done <- runOutcome{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}()

	firstRequest := receiveCLIRunRequest(t, requests)
	assertCLIToolNames(t, firstRequest.Body, []string{"shell"})
	sessionRoot := filepath.Join(configDir, "sessions")
	session := waitForOnlyCLISession(t, sessionRoot)
	store := sessions.NewV2Store(sessionRoot)
	pendingTools := waitForCLISessionToolStatusCount(t, store, session.ID, sessions.ItemStatusPending, 2)
	if got := toolCallIDsForSessionItems(pendingTools); !reflect.DeepEqual(got, []string{"call_1", "call_2"}) {
		t.Fatalf("pending tool call IDs = %#v, want call_1/call_2", got)
	}

	writeCLIFile(t, filepath.Join(projectDir, "release-one.txt"), "go")
	writeCLIFile(t, filepath.Join(projectDir, "release-two.txt"), "go")
	secondRequest := receiveCLIRunRequest(t, requests)
	messages := requestMessages(t, secondRequest.Body)
	if got := toolMessagesByCallID(messages); !strings.Contains(got["call_1"], "first tool output") || !strings.Contains(got["call_2"], "second tool output") {
		t.Fatalf("second request tool messages = %#v, want both tool outputs", got)
	}

	var outcome runOutcome
	select {
	case outcome = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for CLI run")
	}
	if outcome.code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", outcome.code, outcome.stderr)
	}
	if got, want := outcome.stdout, "done"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertNoAdditionalCLIRunRequest(t, requests)

	loaded := loadCLISession(t, sessionRoot, session.ID)
	if loaded.RunningTurnID != "" {
		t.Fatalf("RunningTurnID = %q, want cleared", loaded.RunningTurnID)
	}
	completedTools := toolItemsByCallID(loaded.Items)
	for callID, wantContent := range map[string]string{
		"call_1": "first tool output",
		"call_2": "second tool output",
	} {
		item, ok := completedTools[callID]
		if !ok || item.Status != sessions.ItemStatusCompleted || item.Message == nil || !strings.Contains(item.Message.Content, wantContent) {
			t.Fatalf("%s completed item = %#v, ok %v, want completed content %q", callID, item, ok, wantContent)
		}
	}
	materialized, err := store.MaterializeActiveHistory(loaded)
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if err := validateActiveHistoryToolExchanges(loaded.ID, materialized); err != nil {
		t.Fatalf("active history validation error = %v; messages=%#v", err, materialized)
	}
	events, err := store.PersistedEventsAfter(loaded.ID, 0)
	if err != nil {
		t.Fatalf("PersistedEventsAfter() error = %v", err)
	}
	updatedItemIDs := make(map[string]struct{}, 2)
	for _, item := range completedTools {
		updatedItemIDs[item.ID] = struct{}{}
	}
	var updatedEvents int
	for _, event := range events {
		if event.Type != sessions.RecordTypeItemUpdated {
			continue
		}
		if _, ok := updatedItemIDs[event.ItemID]; ok {
			updatedEvents++
		}
	}
	if updatedEvents != 2 {
		t.Fatalf("tool item.updated events = %d, want 2: %#v", updatedEvents, events)
	}
}

func TestChatSaveSessionAssistantReadyPublishFailureAborts(t *testing.T) {
	argumentSecret := "publish failure argument secret"
	args, err := json.Marshal(map[string]any{
		"command":    "echo " + argumentSecret,
		"timeout_ms": 1000,
	})
	if err != nil {
		t.Fatalf("Marshal(shell args) error = %v", err)
	}
	toolChunk := fmt.Sprintf(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_dup","function":{"name":"shell","arguments":%q}},{"index":1,"id":"call_dup","function":{"name":"shell","arguments":%q}}]}}]}`,
		string(args),
		string(args),
	)
	server, requests := newSequentialCLIRunServer(t, []string{
		toolChunk,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	})
	defer server.Close()

	projectDir := t.TempDir()
	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithTools(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{"shell"})

	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--enable-tools", "shell", "--prompt", "publish failure prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code == 0 {
		t.Fatalf("RunWithIO() code = 0, want publish failure; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "persist assistant") {
		t.Fatalf("stderr = %q, want generic persist assistant failure", stderr.String())
	}
	assertCLIErrorOmits(t, stderr.String(), "publish failure prompt secret", argumentSecret, "direct-secret-value", builtInBaseInstructions)
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)

	sessionRoot := filepath.Join(configDir, "sessions")
	session := loadOnlyCLISession(t, sessionRoot)
	if session.RunningTurnID != "" || session.InterruptedTurnID == "" || session.InterruptedAt.IsZero() {
		t.Fatalf("turn metadata = running %q interrupted %q at %s, want interrupted turn", session.RunningTurnID, session.InterruptedTurnID, session.InterruptedAt)
	}
	messages := activeCLIMessages(t, session)
	if len(messages) != 2 {
		t.Fatalf("materialized messages = %#v, want instructions plus user prompt", messages)
	}
	assertSavedMessage(t, messages, 0, model.MessageRoleSystem, builtInBaseInstructions)
	assertSavedMessage(t, messages, 1, model.MessageRoleUser, "publish failure prompt secret")
	for _, item := range session.Items {
		if item.Message != nil && item.Message.Role == model.MessageRoleAssistant {
			t.Fatalf("unexpected assistant item after AssistantReady publish failure: %#v", item)
		}
		if item.Message != nil && item.Message.Role == model.MessageRoleTool {
			t.Fatalf("unexpected tool item after AssistantReady publish failure: %#v", item)
		}
		if item.Status == sessions.ItemStatusPending {
			t.Fatalf("unexpected pending item after AssistantReady publish failure: %#v", item)
		}
	}
	events, err := sessions.NewV2Store(sessionRoot).PersistedEventsAfter(session.ID, 0)
	if err != nil {
		t.Fatalf("PersistedEventsAfter() error = %v", err)
	}
	for _, event := range events {
		if event.Type == sessions.RecordTypeItemUpdated {
			t.Fatalf("unexpected item.updated event without pending tools: %#v", events)
		}
	}
}

func TestCrashResumeKeepsCompletedToolsAndSynthesizesPendingTools(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"resumed final"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	projectDir := t.TempDir()
	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithTools(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{"shell"})
	sessionRoot := filepath.Join(configDir, "sessions")
	store := sessions.NewV2Store(sessionRoot)
	session, err := store.SaveMetadata(sessions.SessionV2{
		ID:                   "crash-resume-tools",
		Version:              sessions.VersionV2,
		Provider:             "fake",
		ModelProfile:         "default",
		ModelID:              "model-default",
		CWD:                  projectDir,
		CreatedCWD:           projectDir,
		ConfigPath:           cliConfigPath(configDir),
		EnabledTools:         []string{"shell"},
		InstructionsSnapshot: []model.Message{{Role: model.MessageRoleSystem, Content: builtInBaseInstructions}},
		SaveToolResults:      true,
	})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	projector, err := sessionprojector.New(store, session)
	if err != nil {
		t.Fatalf("sessionprojector.New() error = %v", err)
	}
	bus := eventbus.NewBus(projector.Handler())
	publish := func(event eventbus.Event) {
		t.Helper()
		if err := bus.Publish(event); err != nil {
			t.Fatalf("Publish(%T) error = %v", event, err)
		}
	}
	publish(eventbus.TurnStarted{TurnID: "turn-crash"})
	publish(eventbus.TurnInputReady{TurnID: "turn-crash", Message: model.Message{Role: model.MessageRoleUser, Content: "run two tools"}})
	publish(eventbus.AssistantReady{
		TurnID: "turn-crash",
		Message: model.Message{
			Role: model.MessageRoleAssistant,
			ToolCalls: []model.ToolCall{
				{ID: "call-a", Name: "shell", Arguments: `{"command":"echo a"}`},
				{ID: "call-b", Name: "shell", Arguments: `{"command":"echo b"}`},
			},
		},
	})
	publish(eventbus.ToolResultReady{TurnID: "turn-crash", Result: model.ToolResult{ToolCallID: "call-a", Name: "shell", Content: "completed output"}})
	bus.Close()
	if err := projector.Close(); err != nil {
		t.Fatalf("Projector.Close() error = %v", err)
	}

	if marked, err := store.MarkRunningTurnsInterrupted(); err != nil {
		t.Fatalf("MarkRunningTurnsInterrupted() error = %v", err)
	} else if len(marked) != 1 || marked[0].ID != session.ID {
		t.Fatalf("MarkRunningTurnsInterrupted() = %#v, want recovered session", marked)
	}
	recovered, err := store.Load(session.ID)
	if err != nil {
		t.Fatalf("Load(recovered) error = %v", err)
	}
	if recovered.RunningTurnID != "" || recovered.InterruptedTurnID != "turn-crash" {
		t.Fatalf("turn metadata = running %q interrupted %q, want interrupted turn-crash", recovered.RunningTurnID, recovered.InterruptedTurnID)
	}
	toolsByCallID := toolItemsByCallID(recovered.Items)
	callA := toolsByCallID["call-a"]
	callB := toolsByCallID["call-b"]
	if callA.Message == nil || callA.Status != sessions.ItemStatusCompleted || callA.Message.Content != "completed output" {
		t.Fatalf("call-a item = %#v, want completed real output", callA)
	}
	if callB.Message == nil || callB.Status != sessions.ItemStatusPending {
		t.Fatalf("call-b item = %#v, want pending disk fallback", callB)
	}
	materialized, err := store.MaterializeActiveHistory(recovered)
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if err := validateActiveHistoryToolExchanges(recovered.ID, materialized); err != nil {
		t.Fatalf("active history validation error = %v; messages=%#v", err, materialized)
	}
	if !materializedToolMessageContains(materialized, "call-a", "completed output") {
		t.Fatalf("materialized messages missing completed call-a output: %#v", materialized)
	}
	var synthesizedCallB bool
	for _, message := range materialized {
		if message.Role == model.MessageRoleTool && message.ToolCallID == "call-b" && message.Content == "[tool execution interrupted]" && message.IsError {
			synthesizedCallB = true
		}
	}
	if !synthesizedCallB {
		t.Fatalf("materialized messages missing synthesized interrupted call-b: %#v", materialized)
	}

	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--resume", session.ID, "--quit", "--prompt", "continue after crash"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})
	if code != 0 {
		t.Fatalf("resume RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "resumed final"; got != want {
		t.Fatalf("resume stdout = %q, want %q", got, want)
	}
	resumeRequest := <-requests
	assertNoAdditionalCLIRunRequest(t, requests)
	resumeMessages := requestMessages(t, resumeRequest.Body)
	if len(resumeMessages) != 6 {
		t.Fatalf("len(resume messages) = %d, want recovered history plus new user: %#v", len(resumeMessages), resumeMessages)
	}
	assertMessage(t, resumeMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, resumeMessages, 1, "user", "run two tools")
	assistant, ok := resumeMessages[2].(map[string]any)
	if !ok || assistant["role"] != "assistant" {
		t.Fatalf("resume message[2] = %#v, want assistant", resumeMessages[2])
	}
	toolCalls, ok := assistant["tool_calls"].([]any)
	if !ok || len(toolCalls) != 2 {
		t.Fatalf("resume assistant tool_calls = %#v, want call-a/call-b", assistant["tool_calls"])
	}
	for i, want := range []struct {
		id        string
		arguments string
	}{
		{id: "call-a", arguments: `{"command":"echo a"}`},
		{id: "call-b", arguments: `{"command":"echo b"}`},
	} {
		toolCall, ok := toolCalls[i].(map[string]any)
		if !ok {
			t.Fatalf("tool_calls[%d] = %#v, want object", i, toolCalls[i])
		}
		function, ok := toolCall["function"].(map[string]any)
		if !ok {
			t.Fatalf("tool_calls[%d].function = %#v, want object", i, toolCall["function"])
		}
		if toolCall["id"] != want.id || function["name"] != "shell" || function["arguments"] != want.arguments {
			t.Fatalf("tool_calls[%d] = %#v, want id %q shell args %q", i, toolCall, want.id, want.arguments)
		}
	}
	assertToolMessage(t, resumeMessages, 3, "call-a", "completed output")
	assertToolMessage(t, resumeMessages, 4, "call-b", "[tool execution interrupted]")
	assertMessage(t, resumeMessages, 5, "user", "continue after crash")

	afterResume := loadCLISession(t, sessionRoot, session.ID)
	if afterResume.RunningTurnID != "" {
		t.Fatalf("RunningTurnID after resume = %q, want cleared", afterResume.RunningTurnID)
	}
	afterToolsByCallID := toolItemsByCallID(afterResume.Items)
	if len(afterToolsByCallID) != 2 {
		t.Fatalf("tool items after resume = %#v, want only original call-a/call-b", afterToolsByCallID)
	}
	if afterToolsByCallID["call-b"].Status != sessions.ItemStatusPending {
		t.Fatalf("call-b after resume = %#v, want still pending on disk", afterToolsByCallID["call-b"])
	}
	persisted, err := store.PersistedEventsAfter(session.ID, 0)
	if err != nil {
		t.Fatalf("PersistedEventsAfter() error = %v", err)
	}
	var callBUpdates int
	for _, event := range persisted {
		if event.Type == sessions.RecordTypeItemUpdated && event.ItemID == callB.ID {
			callBUpdates++
		}
	}
	if callBUpdates != 0 {
		t.Fatalf("call-b item.updated events = %d, want 0: %#v", callBUpdates, persisted)
	}
}

func TestChatSaveSessionCancelAfterCompletedToolKeepsCompletedResult(t *testing.T) {
	firstCommand := shellOutputCommandForCLI("first tool output")
	secondReleaseFile := "release-cancel-second.txt"
	secondCommand := blockingShellCommandForCLIReleaseFile(secondReleaseFile, "second tool output")
	firstArgs, err := json.Marshal(map[string]any{
		"command":    firstCommand,
		"timeout_ms": 5000,
	})
	if err != nil {
		t.Fatalf("Marshal(first shell args) error = %v", err)
	}
	secondArgs, err := json.Marshal(map[string]any{
		"command":    secondCommand,
		"timeout_ms": 10000,
	})
	if err != nil {
		t.Fatalf("Marshal(second shell args) error = %v", err)
	}
	toolChunk := fmt.Sprintf(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"shell","arguments":%q}},{"index":1,"id":"call_2","function":{"name":"shell","arguments":%q}}]}}]}`,
		string(firstArgs),
		string(secondArgs),
	)
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			toolChunk,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"unexpected final"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	projectDir := t.TempDir()
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	interrupts := make(chan struct{}, 1)
	type runOutcome struct {
		code   int
		stdout string
		stderr string
	}
	done := make(chan runOutcome, 1)
	runFinished := make(chan struct{})
	go func() {
		defer close(runFinished)
		var stdout, stderr bytes.Buffer
		code := runInProcessRuntimeWithInterrupts(context.Background(), "sai", []string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--enable-tools", "shell", "--prompt", "Run until cancel"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
			return projectDir, nil
		}, interrupts)
		done <- runOutcome{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}()
	t.Cleanup(func() {
		select {
		case <-runFinished:
			return
		default:
		}
		select {
		case interrupts <- struct{}{}:
		default:
		}
		_ = os.WriteFile(filepath.Join(projectDir, secondReleaseFile), []byte("go"), 0o600)
		select {
		case <-runFinished:
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for canceled CLI run cleanup")
		}
	})

	firstRequest := receiveCLIRunRequest(t, requests)
	assertCLIToolNames(t, firstRequest.Body, []string{"shell"})
	sessionRoot := filepath.Join(configDir, "sessions")
	session := waitForOnlyCLISession(t, sessionRoot)
	store := sessions.NewV2Store(sessionRoot)
	completed := waitForCLISessionToolCallStatus(t, store, session.ID, "call_1", sessions.ItemStatusCompleted)
	if completed.Message == nil || !strings.Contains(completed.Message.Content, "first tool output") {
		t.Fatalf("completed call_1 item = %#v, want first tool output", completed)
	}

	interrupts <- struct{}{}
	var outcome runOutcome
	select {
	case outcome = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for canceled CLI run")
	}
	if outcome.code == 0 {
		t.Fatalf("RunWithInterrupts() code = 0, want cancellation failure; stdout=%q stderr=%q", outcome.stdout, outcome.stderr)
	}
	assertCLIErrorContains(t, outcome.stderr, "context canceled")
	assertNoAdditionalCLIRunRequest(t, requests)

	loaded := loadCLISession(t, sessionRoot, session.ID)
	if loaded.RunningTurnID != "" {
		t.Fatalf("RunningTurnID = %q, want cleared", loaded.RunningTurnID)
	}
	if loaded.InterruptedTurnID == "" {
		t.Fatal("InterruptedTurnID = empty, want canceled turn recorded")
	}
	toolsByCallID := toolItemsByCallID(loaded.Items)
	call1, ok := toolsByCallID["call_1"]
	if !ok || call1.Status != sessions.ItemStatusCompleted || call1.Message == nil || !strings.Contains(call1.Message.Content, "first tool output") {
		t.Fatalf("call_1 item after cancel = %#v, ok %v, want completed first output", call1, ok)
	}
	materialized, err := store.MaterializeActiveHistory(loaded)
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if err := validateActiveHistoryToolExchanges(loaded.ID, materialized); err != nil {
		t.Fatalf("active history validation error = %v; messages=%#v", err, materialized)
	}
	if !materializedToolMessageContains(materialized, "call_1", "first tool output") {
		t.Fatalf("materialized active history missing completed call_1 output: %#v", materialized)
	}
	events, err := store.PersistedEventsAfter(loaded.ID, 0)
	if err != nil {
		t.Fatalf("PersistedEventsAfter() error = %v", err)
	}
	var call1Updates int
	for _, event := range events {
		if event.Type == sessions.RecordTypeItemUpdated && event.ItemID == call1.ID {
			call1Updates++
		}
	}
	if call1Updates != 1 {
		t.Fatalf("call_1 item.updated events = %d, want 1: %#v", call1Updates, events)
	}
}

func TestChatSaveSessionProcessKillKeepsCompletedToolResult(t *testing.T) {
	firstCommand := shellOutputCommandForCLI("first tool output")
	secondStartedFile := "started-kill-second.txt"
	secondReleaseFile := "release-kill-second.txt"
	secondDoneFile := "done-kill-second.txt"
	secondCommand := blockingShellCommandForCLIStartedReleaseDoneFiles(secondStartedFile, secondReleaseFile, secondDoneFile)
	firstArgs, err := json.Marshal(map[string]any{
		"command":    firstCommand,
		"timeout_ms": 5000,
	})
	if err != nil {
		t.Fatalf("Marshal(first shell args) error = %v", err)
	}
	secondArgs, err := json.Marshal(map[string]any{
		"command":    secondCommand,
		"timeout_ms": 10000,
	})
	if err != nil {
		t.Fatalf("Marshal(second shell args) error = %v", err)
	}
	toolChunk := fmt.Sprintf(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"shell","arguments":%q}},{"index":1,"id":"call_2","function":{"name":"shell","arguments":%q}}]}}]}`,
		string(firstArgs),
		string(secondArgs),
	)
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			toolChunk,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"unexpected final"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	projectDir := t.TempDir()
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	args := []string{
		"-test.run=TestCLIChatSaveSessionHelperProcess",
		"--",
		"--config", cliConfigPath(configDir),
		"chat",
		"--save-session",
		"--quit",
		"--enable-tools", "shell",
		"--prompt", "Run until killed",
	}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(),
		"SAI_CLI_CHAT_HELPER_PROCESS=1",
		"SAI_CLI_CHAT_HELPER_CWD="+projectDir,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start(helper CLI) error = %v", err)
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	childDone := false
	secondShellStarted := false
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(projectDir, secondReleaseFile), []byte("go"), 0o600)
		if secondShellStarted {
			waitForFile(t, filepath.Join(projectDir, secondDoneFile))
		}
		if childDone {
			return
		}
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-waitCh:
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for helper CLI cleanup; stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})

	firstRequest := receiveCLIRunRequest(t, requests)
	assertCLIToolNames(t, firstRequest.Body, []string{"shell"})
	sessionRoot := filepath.Join(configDir, "sessions")
	session := waitForOnlyCLISession(t, sessionRoot)
	store := sessions.NewV2Store(sessionRoot)
	completed := waitForCLISessionToolCallStatus(t, store, session.ID, "call_1", sessions.ItemStatusCompleted)
	if completed.Message == nil || !strings.Contains(completed.Message.Content, "first tool output") {
		t.Fatalf("completed call_1 item = %#v, want first tool output before process kill", completed)
	}
	waitForFile(t, filepath.Join(projectDir, secondStartedFile))
	secondShellStarted = true

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill(helper CLI) error = %v", err)
	}
	_ = os.WriteFile(filepath.Join(projectDir, secondReleaseFile), []byte("go"), 0o600)
	waitForFile(t, filepath.Join(projectDir, secondDoneFile))
	select {
	case <-waitCh:
		childDone = true
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for killed helper CLI; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	assertNoAdditionalCLIRunRequest(t, requests)

	killed := loadCLISession(t, sessionRoot, session.ID)
	toolsByCallID := toolItemsByCallID(killed.Items)
	call1, ok := toolsByCallID["call_1"]
	if !ok || call1.Status != sessions.ItemStatusCompleted || call1.Message == nil || !strings.Contains(call1.Message.Content, "first tool output") {
		t.Fatalf("call_1 item after process kill = %#v, ok %v, want completed first output", call1, ok)
	}
	call2, ok := toolsByCallID["call_2"]
	if !ok || call2.Status != sessions.ItemStatusPending {
		t.Fatalf("call_2 item after process kill = %#v, ok %v, want pending crash fallback", call2, ok)
	}

	if marked, err := store.MarkRunningTurnsInterrupted(); err != nil {
		t.Fatalf("MarkRunningTurnsInterrupted() error = %v", err)
	} else if len(marked) != 1 || marked[0].ID != session.ID {
		t.Fatalf("MarkRunningTurnsInterrupted() = %#v, want killed session", marked)
	}
	recovered := loadCLISession(t, sessionRoot, session.ID)
	materialized, err := store.MaterializeActiveHistory(recovered)
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if err := validateActiveHistoryToolExchanges(recovered.ID, materialized); err != nil {
		t.Fatalf("active history validation error = %v; messages=%#v", err, materialized)
	}
	if !materializedToolMessageContains(materialized, "call_1", "first tool output") {
		t.Fatalf("materialized active history missing completed call_1 output: %#v", materialized)
	}
	if !materializedToolMessageContains(materialized, "call_2", "[tool execution interrupted]") {
		t.Fatalf("materialized active history missing synthesized interrupted call_2: %#v", materialized)
	}

	events, err := store.PersistedEventsAfter(session.ID, 0)
	if err != nil {
		t.Fatalf("PersistedEventsAfter() error = %v", err)
	}
	var call1Updates, call2Updates int
	for _, event := range events {
		switch {
		case event.Type == sessions.RecordTypeItemUpdated && event.ItemID == call1.ID:
			call1Updates++
		case event.Type == sessions.RecordTypeItemUpdated && event.ItemID == call2.ID:
			call2Updates++
		}
	}
	if call1Updates != 1 || call2Updates != 0 {
		t.Fatalf("item.updated counts after process kill = call1:%d call2:%d, want 1/0: %#v", call1Updates, call2Updates, events)
	}
}

func TestCLIChatSaveSessionHelperProcess(t *testing.T) {
	if os.Getenv("SAI_CLI_CHAT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		os.Exit(2)
	}
	args = args[1:]
	cwd := os.Getenv("SAI_CLI_CHAT_HELPER_CWD")
	if len(args) == 0 || strings.TrimSpace(cwd) == "" {
		os.Exit(2)
	}
	code := runInProcessRuntimeWithIO(args, strings.NewReader(""), os.Stdout, os.Stderr, func() (string, error) {
		return cwd, nil
	})
	os.Exit(code)
}

func TestChatSaveSessionWithCompactionEnabledUsesProjectorPath(t *testing.T) {
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
	setCLICompactionConfig(t, configDir, true, "", "")

	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--enable-tools", "read_file", "--prompt", "Read note"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	<-requests
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)

	root := filepath.Join(configDir, "sessions")
	session := loadOnlyCLISession(t, root)
	messages := activeCLIMessages(t, session)
	if len(messages) != 5 {
		t.Fatalf("len(saved messages) = %d, want 5: %#v", len(messages), messages)
	}
	var toolItems []sessions.SessionItem
	for _, item := range session.Items {
		if item.Message != nil && item.Message.Role == model.MessageRoleTool {
			toolItems = append(toolItems, item)
		}
	}
	if len(toolItems) != 1 {
		t.Fatalf("tool item count = %d, want 1: %#v", len(toolItems), session.Items)
	}
	if toolItems[0].Status != sessions.ItemStatusCompleted {
		t.Fatalf("tool item status = %q, want completed projector status", toolItems[0].Status)
	}
	events, err := sessions.NewV2Store(root).PersistedEventsAfter(session.ID, 0)
	if err != nil {
		t.Fatalf("PersistedEventsAfter() error = %v", err)
	}
	var updatedEvents int
	for _, event := range events {
		if event.Type == sessions.RecordTypeItemUpdated && event.ItemID == toolItems[0].ID {
			updatedEvents++
		}
	}
	if updatedEvents != 1 {
		t.Fatalf("tool item.updated events = %d, want 1: %#v", updatedEvents, events)
	}
}

func TestChatLargePromptSaveSessionStoresBlobBackedContent(t *testing.T) {
	requests := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		requests <- struct{}{}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLIModelContextWindow(t, configDir, 30000000)

	oversizedPrompt := strings.Repeat("x", 17*1024*1024) + "OVERSIZED-PROMPT-TAIL"
	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--prompt", oversizedPrompt}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, want 0; stderr:\n%s", code, stderr.String())
	}
	if got := stdout.String(); got != "one" {
		t.Fatalf("stdout = %q, want streamed provider output", got)
	}
	<-requests
	assertCLIErrorOmits(t, stderr.String(), "direct-secret-value")

	store := sessions.NewV2Store(filepath.Join(configDir, "sessions"))
	session := loadOnlyCLISession(t, filepath.Join(configDir, "sessions"))
	messages, err := store.MaterializeActiveHistory(session)
	if err != nil {
		t.Fatalf("MaterializeActiveHistory(%q) error = %v", session.ID, err)
	}
	if len(messages) != 3 {
		t.Fatalf("len(saved messages) = %d, want 3", len(messages))
	}
	assertSavedMessage(t, messages, 0, model.MessageRoleSystem, builtInBaseInstructions)
	if messages[1].Role != model.MessageRoleUser || messages[1].Content != oversizedPrompt {
		t.Fatalf("saved prompt role/length/tail = %q/%d/%t, want user/%d/true", messages[1].Role, len(messages[1].Content), strings.HasSuffix(messages[1].Content, "OVERSIZED-PROMPT-TAIL"), len(oversizedPrompt))
	}
	assertSavedMessage(t, messages, 2, model.MessageRoleAssistant, "one")

	userItem := session.Items[1]
	if userItem.Message == nil || userItem.Message.Content != "" || userItem.Content == nil || userItem.Content.Blob == nil {
		t.Fatalf("saved user item = %#v, want blob-backed content", userItem)
	}
	segmentRaw, err := os.ReadFile(filepath.Join(configDir, "sessions", session.ID, "segments", "000001.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile(segment) error = %v", err)
	}
	if bytes.Contains(segmentRaw, []byte("OVERSIZED-PROMPT-TAIL")) {
		t.Fatalf("segment stored raw prompt tail")
	}
	if !bytes.Contains(segmentRaw, []byte(userItem.Content.Blob.Hash)) {
		t.Fatalf("segment does not contain blob hash %s", userItem.Content.Blob.Hash)
	}
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

	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--prompt", "first prompt secret"}, strings.NewReader(""), &stdout, stderr, func() (string, error) {
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
	if messages := activeCLIMessages(t, session); len(messages) != 3 {
		t.Fatalf("len(saved messages) = %d, want 3: %#v", len(messages), messages)
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "first"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	messages := activeCLIMessages(t, session)
	if len(messages) != 3 {
		t.Fatalf("len(saved messages) = %d, want 3: %#v", len(messages), messages)
	}
	assertSavedMessage(t, messages, 0, model.MessageRoleSystem, builtInBaseInstructions)
	assertSavedMessage(t, messages, 1, model.MessageRoleUser, "first")
	assertSavedMessage(t, messages, 2, model.MessageRoleAssistant, "one")
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

func TestPrepareSessionProjectorMetadataCreatesMetadataForNewSession(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	cwd := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "sai.yaml")
	systemMessage := model.Message{Role: model.MessageRoleSystem, Content: "system instructions"}
	developerMessage := model.Message{Role: model.MessageRoleDeveloper, Content: "developer instructions"}
	runtime := &agentRuntime{
		cwd:                   cwd,
		configPath:            configPath,
		providerName:          "fake",
		modelProfile:          "fast",
		modelID:               "model-fast",
		parameters:            map[string]any{"temperature": 0.2},
		enabledTools:          []string{"read_file"},
		enabledMCP:            []string{"local"},
		enabledSkills:         []string{"skill-a"},
		showReasoning:         true,
		baseMessages:          []model.Message{systemMessage, developerMessage},
		instructionSources:    []sessions.InstructionSource{{Role: model.MessageRoleSystem, Source: "builtin"}, {Role: model.MessageRoleDeveloper, Source: "file", Path: filepath.Join(cwd, "AGENTS.md")}},
		resumableSessionStore: store,
		saveSessions:          true,
		contextTracker:        contextwindow.NewTracker(contextwindow.Window{Tokens: 12345, Source: contextwindow.WindowSourceConfigured}, contextwindow.Metadata{}),
	}

	session, err := runtime.prepareSessionProjectorMetadata()
	if err != nil {
		t.Fatalf("prepareSessionProjectorMetadata() error = %v", err)
	}
	if strings.TrimSpace(session.ID) == "" {
		t.Fatal("session.ID is empty, want generated id")
	}
	if runtime.resumableSession.ID != session.ID {
		t.Fatalf("runtime session ID = %q, want %q", runtime.resumableSession.ID, session.ID)
	}
	if len(runtime.activeItemIDs) != 0 {
		t.Fatalf("runtime activeItemIDs = %#v, want empty", runtime.activeItemIDs)
	}
	if len(session.Items) != 0 || len(session.ActiveHistory) != 0 || session.LastSeq != 0 {
		t.Fatalf("session replay state = items %#v active %#v lastSeq %d, want empty", session.Items, session.ActiveHistory, session.LastSeq)
	}
	if session.Provider != "fake" || session.ModelProfile != "fast" || session.ModelID != "model-fast" {
		t.Fatalf("session model metadata = provider %q profile %q id %q", session.Provider, session.ModelProfile, session.ModelID)
	}
	if got := session.ModelParameters["temperature"]; fmt.Sprint(got) != "0.2" {
		t.Fatalf("temperature = %#v, want 0.2", got)
	}
	if session.CWD != cwd || session.ConfigPath != configPath {
		t.Fatalf("session paths = cwd %q config %q, want %q/%q", session.CWD, session.ConfigPath, cwd, configPath)
	}
	if !reflect.DeepEqual(session.EnabledTools, []string{"read_file"}) || !reflect.DeepEqual(session.EnabledMCP, []string{"local"}) || !reflect.DeepEqual(session.EnabledSkills, []string{"skill-a"}) {
		t.Fatalf("enabled metadata = tools %#v mcp %#v skills %#v", session.EnabledTools, session.EnabledMCP, session.EnabledSkills)
	}
	if !session.ShowReasoning || !session.SaveToolResults {
		t.Fatalf("session flags = showReasoning %t saveToolResults %t, want true/true", session.ShowReasoning, session.SaveToolResults)
	}
	if !reflect.DeepEqual(session.InstructionsSnapshot, []model.Message{systemMessage, developerMessage}) {
		t.Fatalf("InstructionsSnapshot = %#v, want base messages", session.InstructionsSnapshot)
	}
	if len(session.InstructionSources) != 2 || session.InstructionSources[1].Path != filepath.Join(cwd, "AGENTS.md") {
		t.Fatalf("InstructionSources = %#v, want copied sources", session.InstructionSources)
	}
	if session.Context.ContextWindow != 12345 || session.Context.ContextWindowSource != string(contextwindow.WindowSourceConfigured) {
		t.Fatalf("session context = %#v, want configured 12345", session.Context)
	}

	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatalf("Load(%q) error = %v", session.ID, err)
	}
	if loaded.ID != session.ID || len(loaded.Items) != 0 || len(loaded.ActiveHistory) != 0 || loaded.LastSeq != 0 {
		t.Fatalf("loaded session = %#v, want metadata-only replay state", loaded)
	}
}

func TestPrepareSessionProjectorMetadataPreservesReplayedState(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	oldSystemMessage := model.Message{Role: model.MessageRoleSystem, Content: "saved instructions"}
	userMessage := model.Message{Role: model.MessageRoleUser, Content: "first"}
	initial := sessions.SessionV2{
		ID:                   "projector-existing",
		Version:              sessions.VersionV2,
		Provider:             "old",
		ModelProfile:         "old-profile",
		ModelID:              "old-model",
		CWD:                  t.TempDir(),
		ConfigPath:           filepath.Join(t.TempDir(), "old.yaml"),
		InstructionsSnapshot: []model.Message{oldSystemMessage},
		SaveToolResults:      true,
	}
	saved, err := store.SaveTurn(initial, []sessions.SessionItem{
		sessions.SessionItemFromMessage("runtime-000001", oldSystemMessage),
		sessions.SessionItemFromMessage("msg-000002", userMessage),
	}, []string{"runtime-000001", "msg-000002"})
	if err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	runtime := &agentRuntime{
		cwd:                   t.TempDir(),
		configPath:            filepath.Join(t.TempDir(), "sai.yaml"),
		providerName:          "new",
		modelProfile:          "default",
		modelID:               "model-default",
		parameters:            map[string]any{"max_tokens": float64(64)},
		baseMessages:          []model.Message{{Role: model.MessageRoleSystem, Content: "current instructions"}},
		resumableSession:      saved,
		resumableSessionStore: store,
		activeItemIDs:         copyStringSlice(saved.ActiveHistory),
		saveSessions:          true,
	}

	session, err := runtime.prepareSessionProjectorMetadata()
	if err != nil {
		t.Fatalf("prepareSessionProjectorMetadata() error = %v", err)
	}
	if session.ID != saved.ID {
		t.Fatalf("session.ID = %q, want %q", session.ID, saved.ID)
	}
	if len(session.Items) != len(saved.Items) || session.LastSeq != saved.LastSeq {
		t.Fatalf("session replay = len(items) %d lastSeq %d, want %d/%d", len(session.Items), session.LastSeq, len(saved.Items), saved.LastSeq)
	}
	if !reflect.DeepEqual(session.ActiveHistory, saved.ActiveHistory) || !reflect.DeepEqual(runtime.activeItemIDs, saved.ActiveHistory) {
		t.Fatalf("active history = session %#v runtime %#v, want %#v", session.ActiveHistory, runtime.activeItemIDs, saved.ActiveHistory)
	}
	if session.Provider != "new" || session.ModelProfile != "default" || session.ModelID != "model-default" {
		t.Fatalf("session model metadata = provider %q profile %q id %q", session.Provider, session.ModelProfile, session.ModelID)
	}
	if got := session.ModelParameters["max_tokens"]; fmt.Sprint(got) != "64" {
		t.Fatalf("max_tokens = %#v, want 64", got)
	}
	if !reflect.DeepEqual(session.InstructionsSnapshot, []model.Message{oldSystemMessage}) {
		t.Fatalf("InstructionsSnapshot = %#v, want existing snapshot preserved", session.InstructionsSnapshot)
	}
}

func TestPrepareSessionProjectorMetadataRequiresSaveSessionStore(t *testing.T) {
	runtime := &agentRuntime{}
	if _, err := runtime.prepareSessionProjectorMetadata(); err == nil || !strings.Contains(err.Error(), "resumable session saving is not enabled") {
		t.Fatalf("prepareSessionProjectorMetadata() error = %v, want save-session disabled error", err)
	}

	runtime.saveSessions = true
	if _, err := runtime.prepareSessionProjectorMetadata(); err == nil || !strings.Contains(err.Error(), "session store is not configured") {
		t.Fatalf("prepareSessionProjectorMetadata() error = %v, want missing store error", err)
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--save-session=false", "--quit", "--prompt", "first prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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

func TestChatManualCompactReplacesActiveHistoryWithoutStartingUserTurn(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"delta":{"content":"one"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"two"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"three"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"# Context Checkpoint\n\n## Goal\nContinue.\n\n## Current Progress\nThird turn is current.\n\n## Decisions Made\nNone.\n\n## Constraints / User Preferences\nKeep concise.\n\n## Relevant Files / APIs / Commands\nNone.\n\n## Tool State / Environment State\nNo tools.\n\n## Open Questions\nNone.\n\n## Next Steps\nAnswer fourth."}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"four"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithTools(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{"read_file"})
	setCLISessionsConfig(t, configDir, true, true)
	setCLICompactionConfig(t, configDir, true, "", "fast")

	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("first\nsecond\nthird\n/compact\nfourth\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one\ntwo\nthree\nfour\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIErrorContains(t, stderr.String(), "sai: compacted session context")
	assertCLIErrorOmits(t, stderr.String(), "first", "second", "third", "fourth", "one", "two", "three", "four", "direct-secret-value")

	firstRequest := <-requests
	secondRequest := <-requests
	thirdRequest := <-requests
	summaryRequest := <-requests
	nextRequest := <-requests
	assertNoAdditionalCLIRunRequest(t, requests)

	assertMessage(t, requestMessages(t, firstRequest.Body), 1, "user", "first")
	assertMessage(t, requestMessages(t, secondRequest.Body), 3, "user", "second")
	assertMessage(t, requestMessages(t, thirdRequest.Body), 5, "user", "third")
	if got := summaryRequest.Body["model"]; got != "model-fast" {
		t.Fatalf("summary model = %#v, want model-fast", got)
	}
	if _, ok := summaryRequest.Body["tools"]; ok {
		t.Fatalf("summary request included tools: %#v", summaryRequest.Body["tools"])
	}
	summaryMessages := requestMessages(t, summaryRequest.Body)
	if len(summaryMessages) != 2 {
		t.Fatalf("len(summary messages) = %d, want 2: %#v", len(summaryMessages), summaryMessages)
	}
	assertMessageContentContains(t, summaryMessages, 0, "system", "Create a concise handoff checkpoint")
	if strings.Contains(string(summaryRequest.RawBody), "/compact") {
		t.Fatalf("summary request included slash command as user turn: %s", summaryRequest.RawBody)
	}

	nextMessages := requestMessages(t, nextRequest.Body)
	if len(nextMessages) != 7 {
		t.Fatalf("len(next request messages) = %d, want replacement history plus new user: %#v", len(nextMessages), nextMessages)
	}
	assertMessage(t, nextMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, nextMessages, 1, "user", "second")
	assertMessage(t, nextMessages, 2, "assistant", "two")
	assertMessage(t, nextMessages, 3, "user", "third")
	assertMessage(t, nextMessages, 4, "assistant", "three")
	assertMessageContentContains(t, nextMessages, 5, "developer", "<compaction_summary>")
	assertMessage(t, nextMessages, 6, "user", "fourth")
	if requestMessagesContainExactContent(nextMessages, "first") || requestMessagesContainExactContent(nextMessages, "one") || requestMessagesContainExactContent(nextMessages, "/compact") {
		t.Fatalf("next request included old exact message content: %#v", nextMessages)
	}
	assertCLIToolNames(t, nextRequest.Body, []string{"read_file"})

	session := loadOnlyCLISession(t, filepath.Join(configDir, "sessions"))
	if len(session.Compactions) != 1 {
		t.Fatalf("len(Compactions) = %d, want 1: %#v", len(session.Compactions), session.Compactions)
	}
	checkpoint := session.Compactions[0]
	if checkpoint.Reason != "user_requested" || checkpoint.Phase != "manual" || checkpoint.Trigger != "manual" {
		t.Fatalf("checkpoint reason/phase/trigger = %q/%q/%q, want manual user request", checkpoint.Reason, checkpoint.Phase, checkpoint.Trigger)
	}
	if checkpoint.SummaryProvider != "fake" || checkpoint.SummaryModel != "fast" {
		t.Fatalf("checkpoint summary model = %q/%q, want fake/fast", checkpoint.SummaryProvider, checkpoint.SummaryModel)
	}
	summaryItem := sessionItemByID(t, session, checkpoint.SummaryItemID)
	if summaryItem.Visibility != sessions.ItemVisibilityHidden || summaryItem.Audience != sessions.ItemAudienceModel {
		t.Fatalf("summary item visibility/audience = %q/%q, want hidden/model", summaryItem.Visibility, summaryItem.Audience)
	}
	if summaryItem.Message == nil || summaryItem.Message.Role != model.MessageRoleDeveloper || !strings.Contains(summaryItem.Message.Content, "<compaction_summary>") {
		t.Fatalf("summary item message = %#v, want hidden developer summary", summaryItem.Message)
	}
	if !sessionContainsMessageContent(session, "first") || !sessionContainsMessageContent(session, "one") {
		t.Fatalf("session items dropped old visible messages: %#v", session.Items)
	}
	if sessionContainsExactMessageContent(session, "/compact") {
		t.Fatalf("session items included /compact as a user turn: %#v", session.Items)
	}
	active := activeCLIMessages(t, session)
	if len(active) != 8 {
		t.Fatalf("len(active messages) = %d, want compacted history plus third turn: %#v", len(active), active)
	}
	assertSavedMessage(t, active, 1, model.MessageRoleUser, "second")
	assertSavedMessage(t, active, 2, model.MessageRoleAssistant, "two")
	assertSavedMessage(t, active, 3, model.MessageRoleUser, "third")
	assertSavedMessage(t, active, 4, model.MessageRoleAssistant, "three")
	assertSavedMessageContentContains(t, active, 5, model.MessageRoleDeveloper, "<compaction_summary>")
	assertSavedMessage(t, active, 6, model.MessageRoleUser, "fourth")
	assertSavedMessage(t, active, 7, model.MessageRoleAssistant, "four")
}

func TestChatResumeAfterCompletedCompactSendsOnlyMaterializedActiveHistory(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"delta":{"content":"old assistant alpha"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"middle assistant beta"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"recent assistant gamma"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"# Context Checkpoint\n\n## Goal\nContinue the compacted session.\n\n## Current Progress\nRecent gamma is current.\n\n## Decisions Made\nUse the saved compacted active history.\n\n## Constraints / User Preferences\nKeep concise.\n\n## Relevant Files / APIs / Commands\nNone.\n\n## Tool State / Environment State\nNo tools.\n\n## Open Questions\nNone.\n\n## Next Steps\nAnswer delta."}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"next assistant delta"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLISessionsConfig(t, configDir, true, true)
	setCLICompactionConfig(t, configDir, true, "", "")

	var firstStdout, firstStderr bytes.Buffer
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("old user alpha\nmiddle user beta\nrecent user gamma\n/compact\n/quit\n"), &firstStdout, &firstStderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("first RunWithIO() code = %d, stderr = %s", code, firstStderr.String())
	}
	if got, want := firstStdout.String(), "old assistant alpha\nmiddle assistant beta\nrecent assistant gamma\n"; got != want {
		t.Fatalf("first stdout = %q, want %q", got, want)
	}
	firstRequest := <-requests
	secondRequest := <-requests
	thirdRequest := <-requests
	summaryRequest := <-requests
	assertMessage(t, requestMessages(t, firstRequest.Body), 1, "user", "old user alpha")
	assertMessage(t, requestMessages(t, secondRequest.Body), 3, "user", "middle user beta")
	assertMessage(t, requestMessages(t, thirdRequest.Body), 5, "user", "recent user gamma")
	if _, ok := summaryRequest.Body["tools"]; ok {
		t.Fatalf("summary request included tools: %#v", summaryRequest.Body["tools"])
	}

	sessionRoot := filepath.Join(configDir, "sessions")
	session := loadOnlyCLISession(t, sessionRoot)
	for _, persisted := range []string{
		"old user alpha",
		"old assistant alpha",
		"middle user beta",
		"middle assistant beta",
		"recent user gamma",
		"recent assistant gamma",
	} {
		if !sessionContainsExactMessageContent(session, persisted) {
			t.Fatalf("session items dropped pre-compact visible message %q after compact: %#v", persisted, session.Items)
		}
	}
	if len(session.Compactions) != 1 {
		t.Fatalf("len(Compactions) = %d, want 1: %#v", len(session.Compactions), session.Compactions)
	}
	summaryItem := sessionItemByID(t, session, session.Compactions[0].SummaryItemID)
	if summaryItem.Visibility != sessions.ItemVisibilityHidden || summaryItem.Audience != sessions.ItemAudienceModel {
		t.Fatalf("summary item visibility/audience = %q/%q, want hidden/model", summaryItem.Visibility, summaryItem.Audience)
	}
	if summaryItem.Message == nil || summaryItem.Message.Role != model.MessageRoleDeveloper || !strings.Contains(summaryItem.Message.Content, "<compaction_summary>") {
		t.Fatalf("summary item message = %#v, want hidden developer summary", summaryItem.Message)
	}

	var resumeStdout, resumeStderr bytes.Buffer
	code = runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--resume", session.ID, "--quit", "--prompt", "next user delta"}, strings.NewReader(""), &resumeStdout, &resumeStderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("resume RunWithIO() code = %d, stderr = %s", code, resumeStderr.String())
	}
	if got, want := resumeStdout.String(), "next assistant delta"; got != want {
		t.Fatalf("resume stdout = %q, want %q", got, want)
	}
	resumeRequest := <-requests
	assertNoAdditionalCLIRunRequest(t, requests)

	resumeMessages := requestMessages(t, resumeRequest.Body)
	if len(resumeMessages) != 7 {
		t.Fatalf("len(resume messages) = %d, want compacted active history plus new user: %#v", len(resumeMessages), resumeMessages)
	}
	assertMessage(t, resumeMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, resumeMessages, 1, "user", "middle user beta")
	assertMessage(t, resumeMessages, 2, "assistant", "middle assistant beta")
	assertMessage(t, resumeMessages, 3, "user", "recent user gamma")
	assertMessage(t, resumeMessages, 4, "assistant", "recent assistant gamma")
	assertMessageContentContains(t, resumeMessages, 5, "developer", "<compaction_summary>")
	assertMessage(t, resumeMessages, 6, "user", "next user delta")
	for _, leaked := range []string{"old user alpha", "old assistant alpha"} {
		if strings.Contains(string(resumeRequest.RawBody), leaked) {
			t.Fatalf("resume request included inactive content %q: %s", leaked, resumeRequest.RawBody)
		}
	}
	if requestMessagesContainExactContent(resumeMessages, "/compact") {
		t.Fatalf("resume request included compact slash command as a message: %#v", resumeMessages)
	}

	session = loadCLISession(t, sessionRoot, session.ID)
	for _, persisted := range []string{
		"old user alpha",
		"old assistant alpha",
		"middle user beta",
		"middle assistant beta",
		"recent user gamma",
		"recent assistant gamma",
	} {
		if !sessionContainsExactMessageContent(session, persisted) {
			t.Fatalf("session items dropped pre-compact visible message %q after resume: %#v", persisted, session.Items)
		}
	}
	active := activeCLIMessages(t, session)
	if len(active) != 8 {
		t.Fatalf("len(active messages) = %d, want resumed compacted history plus continuation: %#v", len(active), active)
	}
	assertSavedMessageContentContains(t, active, 5, model.MessageRoleDeveloper, "<compaction_summary>")
	assertSavedMessage(t, active, 6, model.MessageRoleUser, "next user delta")
	assertSavedMessage(t, active, 7, model.MessageRoleAssistant, "next assistant delta")
}

func TestValidateCompactionReplacementHistoryRejectsIllegalToolExchangeBeforeWrite(t *testing.T) {
	summaryMessage := model.Message{Role: model.MessageRoleDeveloper, Content: "<compaction_summary>\nsummary\n</compaction_summary>"}
	summaryItem := sessions.SessionItem{
		ID:         "summary-1",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityHidden,
		Audience:   sessions.ItemAudienceModel,
		Message:    &summaryMessage,
	}
	session := sessions.SessionV2{
		ID: "bad-replacement-session",
		Items: []sessions.SessionItem{
			sessions.SessionItemFromMessage("runtime-1", model.Message{Role: model.MessageRoleSystem, Content: "runtime"}),
			sessions.SessionItemFromMessage("user-1", model.Message{Role: model.MessageRoleUser, Content: "ask"}),
			sessions.SessionItemFromMessage("assistant-tool-1", model.Message{
				Role: model.MessageRoleAssistant,
				ToolCalls: []model.ToolCall{
					{ID: "call_1", Name: "read_file", Arguments: `{"path":"note.txt"}`},
				},
			}),
		},
	}

	_, err := validateCompactionReplacementHistory(session, summaryItem, []string{"runtime-1", "user-1", "assistant-tool-1", "summary-1"})

	if err == nil {
		t.Fatal("validateCompactionReplacementHistory() error = nil, want illegal tool exchange")
	}
	if !strings.Contains(err.Error(), "appears before tool results for call_1") {
		t.Fatalf("validateCompactionReplacementHistory() error = %q, want pending tool result detail", err)
	}
}

func TestChatMultilineCompactIsUserText(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLISessionsConfig(t, configDir, true, true)
	setCLICompactionConfig(t, configDir, true, "", "")

	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("\"\"\"\n/compact\n\"\"\"\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "compacted session context") {
		t.Fatalf("stderr = %q, want no compaction status", stderr.String())
	}
	messages := requestMessages(t, (<-requests).Body)
	assertMessage(t, messages, 1, "user", "/compact")
	assertNoAdditionalCLIRunRequest(t, requests)

	session := loadOnlyCLISession(t, filepath.Join(configDir, "sessions"))
	if len(session.Compactions) != 0 {
		t.Fatalf("Compactions = %#v, want none", session.Compactions)
	}
	if !sessionContainsMessageContent(session, "/compact") {
		t.Fatalf("session items do not contain multiline /compact user text: %#v", session.Items)
	}
}

func TestChatInitialPromptCompactIsUserText(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLISessionsConfig(t, configDir, true, true)
	setCLICompactionConfig(t, configDir, true, "", "")

	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "/compact"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "compacted session context") {
		t.Fatalf("stderr = %q, want no compaction status", stderr.String())
	}
	messages := requestMessages(t, (<-requests).Body)
	assertMessage(t, messages, 1, "user", "/compact")
	assertNoAdditionalCLIRunRequest(t, requests)

	session := loadOnlyCLISession(t, filepath.Join(configDir, "sessions"))
	if len(session.Compactions) != 0 {
		t.Fatalf("Compactions = %#v, want none", session.Compactions)
	}
	if !sessionContainsExactMessageContent(session, "/compact") {
		t.Fatalf("session items do not contain initial /compact user text: %#v", session.Items)
	}
}

func TestChatManualCompactDisabledOrNotSavingDoesNotRequestModel(t *testing.T) {
	tests := []struct {
		name              string
		sessionsEnabled   bool
		compactionEnabled bool
		want              string
	}{
		{name: "disabled", sessionsEnabled: true, compactionEnabled: false, want: "compaction is disabled"},
		{name: "not saving", sessionsEnabled: false, compactionEnabled: true, want: "compaction requires a saved or resumed session"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, requests := newCLIRunServer(t,
				`{"choices":[{"delta":{"content":"unexpected"}}]}`,
				`[DONE]`,
			)
			defer server.Close()

			configDir := t.TempDir()
			writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
			setCLISessionsConfig(t, configDir, tt.sessionsEnabled, true)
			setCLICompactionConfig(t, configDir, tt.compactionEnabled, "", "")

			var stdout, stderr bytes.Buffer
			code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("/compact\n/quit\n"), &stdout, &stderr, func() (string, error) {
				return t.TempDir(), nil
			})

			if code != 0 {
				t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
			}
			if stdout.String() != "" {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			assertCLIErrorContains(t, stderr.String(), "sai: compact failed", tt.want)
			assertNoAdditionalCLIRunRequest(t, requests)
		})
	}
}

func TestChatManualCompactSummaryFailureLeavesStateUnchanged(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"delta":{"content":"one"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"two"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{not-json`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"three"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLISessionsConfig(t, configDir, true, true)
	setCLICompactionConfig(t, configDir, true, "", "")

	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("first\nsecond\n/compact\nthird\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one\ntwo\nthree\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIErrorContains(t, stderr.String(), "sai: compact failed", "parse OpenAI chat stream")
	assertCLIErrorOmits(t, stderr.String(), "first", "second", "third", "one", "two", "three", "direct-secret-value")

	<-requests
	<-requests
	<-requests
	nextRequest := <-requests
	assertNoAdditionalCLIRunRequest(t, requests)
	nextMessages := requestMessages(t, nextRequest.Body)
	if len(nextMessages) != 6 {
		t.Fatalf("len(next request messages) = %d, want unchanged full history plus new user: %#v", len(nextMessages), nextMessages)
	}
	assertMessage(t, nextMessages, 1, "user", "first")
	assertMessage(t, nextMessages, 2, "assistant", "one")
	assertMessage(t, nextMessages, 3, "user", "second")
	assertMessage(t, nextMessages, 4, "assistant", "two")
	assertMessage(t, nextMessages, 5, "user", "third")

	session := loadOnlyCLISession(t, filepath.Join(configDir, "sessions"))
	if len(session.Compactions) != 0 {
		t.Fatalf("Compactions = %#v, want none", session.Compactions)
	}
	if sessionContainsMessageContent(session, "<compaction_summary>") {
		t.Fatalf("session contains summary after failed compaction: %#v", session.Items)
	}
	active := activeCLIMessages(t, session)
	if len(active) != 7 {
		t.Fatalf("len(active messages) = %d, want original history plus third turn: %#v", len(active), active)
	}
	assertSavedMessage(t, active, 1, model.MessageRoleUser, "first")
	assertSavedMessage(t, active, 3, model.MessageRoleUser, "second")
	assertSavedMessage(t, active, 5, model.MessageRoleUser, "third")
}

func TestChatAutoCompactTriggersBeforeMainModelWhenThresholdExceeded(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"delta":{"content":"one"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"# Context Checkpoint\n\n## Goal\nContinue.\n\n## Current Progress\nFirst turn is current.\n\n## Decisions Made\nNone.\n\n## Constraints / User Preferences\nKeep concise.\n\n## Relevant Files / APIs / Commands\nNone.\n\n## Tool State / Environment State\nNo tools.\n\n## Open Questions\nNone.\n\n## Next Steps\nAnswer second."}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"two"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithTools(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{"read_file"})
	setCLISessionsConfig(t, configDir, true, true)
	setCLICompactionConfigWithThreshold(t, configDir, true, 1, "", "")
	setCLIModelContextWindow(t, configDir, 10000)

	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("first\nsecond\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one\ntwo\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	firstRequest := <-requests
	summaryRequest := <-requests
	secondRequest := <-requests
	assertNoAdditionalCLIRunRequest(t, requests)

	assertMessage(t, requestMessages(t, firstRequest.Body), 1, "user", "first")
	if _, ok := summaryRequest.Body["tools"]; ok {
		t.Fatalf("summary request included tools: %#v", summaryRequest.Body["tools"])
	}
	summaryMessages := requestMessages(t, summaryRequest.Body)
	if len(summaryMessages) != 2 {
		t.Fatalf("len(summary messages) = %d, want 2: %#v", len(summaryMessages), summaryMessages)
	}
	assertMessageContentContains(t, summaryMessages, 0, "system", "Create a concise handoff checkpoint")
	assertMessageContentContains(t, summaryMessages, 1, "user", "first")
	if strings.Contains(string(summaryRequest.RawBody), "second") {
		t.Fatalf("summary request included pending user message: %s", summaryRequest.RawBody)
	}

	secondMessages := requestMessages(t, secondRequest.Body)
	if len(secondMessages) != 5 {
		t.Fatalf("len(second request messages) = %d, want compacted history plus pending user: %#v", len(secondMessages), secondMessages)
	}
	assertMessage(t, secondMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, secondMessages, 1, "user", "first")
	assertMessage(t, secondMessages, 2, "assistant", "one")
	assertMessageContentContains(t, secondMessages, 3, "developer", "<compaction_summary>")
	assertMessage(t, secondMessages, 4, "user", "second")
	assertCLIToolNames(t, secondRequest.Body, []string{"read_file"})

	sessionRoot := filepath.Join(configDir, "sessions")
	session := loadOnlyCLISession(t, sessionRoot)
	if len(session.Compactions) != 1 {
		t.Fatalf("len(Compactions) = %d, want 1: %#v", len(session.Compactions), session.Compactions)
	}
	checkpoint := session.Compactions[0]
	if checkpoint.Reason != "context_limit" || checkpoint.Phase != "pre_turn" || checkpoint.Trigger != "auto" {
		t.Fatalf("checkpoint reason/phase/trigger = %q/%q/%q, want auto pre-turn context limit", checkpoint.Reason, checkpoint.Phase, checkpoint.Trigger)
	}
	if !sessionContainsExactMessageContent(session, "first") || !sessionContainsExactMessageContent(session, "one") {
		t.Fatalf("session items dropped old visible messages: %#v", session.Items)
	}
	active := activeCLIMessages(t, session)
	if len(active) != 6 {
		t.Fatalf("len(active messages) = %d, want compacted history plus second turn: %#v", len(active), active)
	}
	assertSavedMessageContentContains(t, active, 3, model.MessageRoleDeveloper, "<compaction_summary>")
	assertSavedMessage(t, active, 4, model.MessageRoleUser, "second")
	assertSavedMessage(t, active, 5, model.MessageRoleAssistant, "two")

	events, err := sessions.NewV2Store(sessionRoot).PersistedEventsAfter(session.ID, 0)
	if err != nil {
		t.Fatalf("PersistedEventsAfter() error = %v", err)
	}
	var compactionSeq int64
	for _, event := range events {
		if event.Type == sessions.RecordTypeCompactionCreated && event.CompactionID == checkpoint.ID {
			compactionSeq = event.Seq
			break
		}
	}
	if compactionSeq == 0 {
		t.Fatalf("compaction.created event for %q not found: %#v", checkpoint.ID, events)
	}
	secondItem := sessionItemWithExactContent(t, session, "second")
	if secondItem.Seq <= compactionSeq {
		t.Fatalf("second user item seq = %d, want after compaction seq %d", secondItem.Seq, compactionSeq)
	}
}

func TestChatAutoCompactDoesNotTriggerBelowThreshold(t *testing.T) {
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
	setCLISessionsConfig(t, configDir, true, true)
	setCLICompactionConfig(t, configDir, true, "", "")
	setCLIModelContextWindow(t, configDir, 100000)

	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("first\nsecond\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	firstRequest := <-requests
	secondRequest := <-requests
	assertNoAdditionalCLIRunRequest(t, requests)

	assertMessage(t, requestMessages(t, firstRequest.Body), 1, "user", "first")
	secondMessages := requestMessages(t, secondRequest.Body)
	if len(secondMessages) != 4 {
		t.Fatalf("len(second request messages) = %d, want direct full history: %#v", len(secondMessages), secondMessages)
	}
	assertMessage(t, secondMessages, 1, "user", "first")
	assertMessage(t, secondMessages, 2, "assistant", "one")
	assertMessage(t, secondMessages, 3, "user", "second")
	if strings.Contains(string(secondRequest.RawBody), "<compaction_summary>") {
		t.Fatalf("second request included compaction summary: %s", secondRequest.RawBody)
	}

	session := loadOnlyCLISession(t, filepath.Join(configDir, "sessions"))
	if len(session.Compactions) != 0 {
		t.Fatalf("Compactions = %#v, want none", session.Compactions)
	}
}

func TestChatAutoCompactFailureLeavesTurnUnchangedAndREPLContinues(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"delta":{"content":"one"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{not-json`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"# Context Checkpoint\n\n## Goal\nContinue.\n\n## Current Progress\nFirst turn is current.\n\n## Decisions Made\nNone.\n\n## Constraints / User Preferences\nKeep concise.\n\n## Relevant Files / APIs / Commands\nNone.\n\n## Tool State / Environment State\nNo tools.\n\n## Open Questions\nNone.\n\n## Next Steps\nAnswer third."}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"three"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLISessionsConfig(t, configDir, true, true)
	setCLICompactionConfigWithThreshold(t, configDir, true, 1, "", "")
	setCLIModelContextWindow(t, configDir, 10000)

	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("first\nsecond\nthird\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one\nthree\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIErrorContains(t, stderr.String(), "sai: auto compact failed", "parse OpenAI chat stream")

	firstRequest := <-requests
	failedSummaryRequest := <-requests
	retrySummaryRequest := <-requests
	thirdRequest := <-requests
	assertNoAdditionalCLIRunRequest(t, requests)
	assertMessage(t, requestMessages(t, firstRequest.Body), 1, "user", "first")
	if _, ok := failedSummaryRequest.Body["tools"]; ok {
		t.Fatalf("failed summary request included tools: %#v", failedSummaryRequest.Body["tools"])
	}
	if strings.Contains(string(failedSummaryRequest.RawBody), "second") {
		t.Fatalf("failed summary request included pending failed user message: %s", failedSummaryRequest.RawBody)
	}
	if strings.Contains(string(retrySummaryRequest.RawBody), "third") {
		t.Fatalf("retry summary request included pending later user message: %s", retrySummaryRequest.RawBody)
	}
	thirdMessages := requestMessages(t, thirdRequest.Body)
	assertMessageContentContains(t, thirdMessages, 3, "developer", "<compaction_summary>")
	assertMessage(t, thirdMessages, 4, "user", "third")
	if requestMessagesContainExactContent(thirdMessages, "second") {
		t.Fatalf("third request included failed pending user message: %#v", thirdMessages)
	}

	session := loadOnlyCLISession(t, filepath.Join(configDir, "sessions"))
	if len(session.Compactions) != 1 {
		t.Fatalf("len(Compactions) = %d, want only later successful auto compaction: %#v", len(session.Compactions), session.Compactions)
	}
	if sessionContainsExactMessageContent(session, "second") {
		t.Fatalf("session items contain failed pending user message: %#v", session.Items)
	}
	if !sessionContainsExactMessageContent(session, "first") || !sessionContainsExactMessageContent(session, "third") {
		t.Fatalf("session items missing successful turns: %#v", session.Items)
	}
}

func TestServerAgentTurnRunnerDisablesSubagentsForSingleRequestRuntime(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"server assistant"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	appendCLIConfig(t, configDir, `
subagents:
  reviewer: subagents/reviewer.yaml
`)
	writeCLIChildSubagentConfig(t, configDir, server.URL)
	sessionRoot := filepath.Join(configDir, "sessions")
	projectDir := t.TempDir()
	store := sessions.NewV2Store(sessionRoot)
	session, err := store.SaveMetadata(sessions.SessionV2{
		ID:              "server-subagents-disabled",
		Version:         sessions.VersionV2,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		CWD:             projectDir,
		CreatedCWD:      projectDir,
		ConfigPath:      cliConfigPath(configDir),
		SaveToolResults: true,
	})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}

	result, err := runServerAgentTurnWithProjectorForTest(t, context.Background(), store, session, "turn-subagents-disabled", "server prompt")
	if err != nil {
		t.Fatalf("RunSessionTurn() error = %v", err)
	}
	assertIncrementalSessionTurnResult(t, result)

	request := receiveCLIRunRequest(t, requests)
	assertCLIRequestOmitsKey(t, request.Body, "tools")
	messages := requestMessages(t, request.Body)
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "server prompt")
	if got := countCLIRequestMessagesWithContent(t, messages, "user", "server prompt"); got != 1 {
		t.Fatalf("provider request prompt count = %d, want one; messages=%#v", got, messages)
	}
	for _, leaked := range []string{"Configured subagents", subagents.ToolSubagentStart} {
		if strings.Contains(string(request.RawBody), leaked) {
			t.Fatalf("server runner request exposed subagent runtime %q: %s", leaked, request.RawBody)
		}
	}
	assertNoAdditionalCLIRunRequest(t, requests)
	persisted, err := store.Load(session.ID)
	if err != nil {
		t.Fatalf("Load(%q) error = %v", session.ID, err)
	}
	if got := countSessionItemsWithExactContent(persisted, "server prompt"); got != 1 {
		t.Fatalf("persisted prompt count = %d, want one; items=%#v", got, persisted.Items)
	}
}

func TestServerAgentTurnRunnerUsesCreatedCWDForInstructions(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"server assistant"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	sessionRoot := filepath.Join(configDir, "sessions")
	createdCWD := t.TempDir()
	staleCWD := t.TempDir()
	writeCLIFile(t, filepath.Join(createdCWD, "AGENTS.md"), "created cwd instructions\n")
	writeCLIFile(t, filepath.Join(staleCWD, "AGENTS.md"), "stale cwd instructions\n")
	session := sessions.SessionV2{
		ID:              "server-created-cwd",
		Version:         sessions.VersionV2,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		CWD:             staleCWD,
		CreatedCWD:      createdCWD,
		ConfigPath:      cliConfigPath(configDir),
		SaveToolResults: true,
	}
	writeCLISessionV2(t, sessionRoot, session)
	loaded := loadCLISession(t, sessionRoot, session.ID)

	store := sessions.NewV2Store(sessionRoot)
	result, err := runServerAgentTurnWithProjectorForTest(t, context.Background(), store, loaded, "turn-created-cwd", "server prompt")
	if err != nil {
		t.Fatalf("RunSessionTurn() error = %v", err)
	}
	assertIncrementalSessionTurnResult(t, result)

	request := receiveCLIRunRequest(t, requests)
	assertNoAdditionalCLIRunRequest(t, requests)
	messages := requestMessages(t, request.Body)
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "developer", "created cwd instructions\n")
	assertMessage(t, messages, 2, "user", "server prompt")
	if strings.Contains(string(request.RawBody), "stale cwd instructions") {
		t.Fatalf("server runner request used stale cwd instructions: %s", request.RawBody)
	}
	if got, want := result.Session.CWD, createdCWD; got != want {
		t.Fatalf("result session CWD = %q, want created_cwd %q", got, want)
	}
	if got, want := result.Session.CreatedCWD, createdCWD; got != want {
		t.Fatalf("result session CreatedCWD = %q, want %q", got, want)
	}
	if got, want := result.Session.ConfigPath, cliConfigPath(configDir); got != want {
		t.Fatalf("result session ConfigPath = %q, want %q", got, want)
	}
}

func TestServerAgentTurnRunnerRequiresCreatedCWD(t *testing.T) {
	bus := eventbus.NewBus(func(eventbus.Event) error { return nil })
	defer bus.Close()

	_, err := (executionAgentTurnRunner{program: "sai"}).RunSessionTurn(context.Background(), execution.SessionTurnRequest{
		Session: sessions.SessionV2{
			ID:              "server-missing-created-cwd",
			Version:         sessions.VersionV2,
			Provider:        "fake",
			ModelProfile:    "default",
			ModelID:         "model-default",
			CWD:             t.TempDir(),
			ConfigPath:      filepath.Join(t.TempDir(), "sai.yaml"),
			SaveToolResults: true,
		},
		TurnID:    "turn-missing-created-cwd",
		Content:   "server prompt",
		Publisher: bus,
	})
	if err == nil {
		t.Fatal("RunSessionTurn() error = nil, want missing created_cwd error")
	}
	if !strings.Contains(err.Error(), "session created_cwd is required") {
		t.Fatalf("RunSessionTurn() error = %v, want missing created_cwd error", err)
	}
}

func TestServerAgentTurnRunnerRequiresIncrementalPublisherAndTurnID(t *testing.T) {
	_, err := (executionAgentTurnRunner{program: "sai"}).RunSessionTurn(context.Background(), execution.SessionTurnRequest{})
	if err == nil {
		t.Fatal("RunSessionTurn() error = nil, want missing publisher error")
	}
	if !strings.Contains(err.Error(), "session turn publisher is required") {
		t.Fatalf("RunSessionTurn() error = %v, want missing publisher error", err)
	}

	bus := eventbus.NewBus(func(eventbus.Event) error { return nil })
	defer bus.Close()

	_, err = (executionAgentTurnRunner{program: "sai"}).RunSessionTurn(context.Background(), execution.SessionTurnRequest{
		Publisher: bus,
	})
	if err == nil {
		t.Fatal("RunSessionTurn() error = nil, want missing turn id error")
	}
	if !strings.Contains(err.Error(), "session turn id is required") {
		t.Fatalf("RunSessionTurn() error = %v, want missing turn id error", err)
	}
}

func runServerAgentTurnWithProjectorForTest(t *testing.T, ctx context.Context, store *sessions.V2Store, session sessions.SessionV2, turnID, prompt string) (execution.SessionTurnResult, error) {
	t.Helper()

	projector, err := sessionprojector.New(store, session)
	if err != nil {
		t.Fatalf("sessionprojector.New() error = %v", err)
	}
	defer projector.Close()
	bus := eventbus.NewBus(projector.Handler())
	defer bus.Close()

	if err := bus.Publish(eventbus.TurnStarted{TurnID: turnID}); err != nil {
		t.Fatalf("Publish(TurnStarted) error = %v", err)
	}
	if err := bus.Publish(eventbus.TurnInputReady{TurnID: turnID, Message: model.Message{Role: model.MessageRoleUser, Content: prompt}}); err != nil {
		t.Fatalf("Publish(TurnInputReady) error = %v", err)
	}

	result, err := (executionAgentTurnRunner{program: "sai"}).RunSessionTurn(ctx, execution.SessionTurnRequest{
		Session:      session,
		SessionStore: store,
		TurnID:       turnID,
		Content:      prompt,
		Publisher:    bus,
	})
	if err != nil {
		_ = bus.Publish(eventbus.TurnInterrupted{TurnID: turnID})
		return result, err
	}
	if err := bus.Publish(eventbus.TurnCompleted{TurnID: turnID}); err != nil {
		return result, err
	}
	return result, nil
}

func assertIncrementalSessionTurnResult(t *testing.T, result execution.SessionTurnResult) {
	t.Helper()
	if !result.Incremental {
		t.Fatalf("RunSessionTurn() Incremental = false, want true")
	}
	if len(result.Items) != 0 || len(result.ActiveHistory) != 0 || result.Compaction != nil {
		t.Fatalf("incremental result returned legacy save plan: %#v", result)
	}
}

func TestPublishCLIInterruptedTurnFallsBackToStore(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	session, err := store.SaveMetadata(sessions.SessionV2{
		ID:              "cli-interrupt-fallback",
		Version:         sessions.VersionV2,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		SaveToolResults: true,
	})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	if _, err := store.MarkTurnRunning(session.ID, "turn-1"); err != nil {
		t.Fatalf("MarkTurnRunning() error = %v", err)
	}
	publisher := &failingCLIInterruptPublisher{err: errors.New("publish failed")}

	publishCLIInterruptedTurn(publisher, store, session.ID, "turn-1")

	if len(publisher.events) != 1 {
		t.Fatalf("publisher events = %d, want 1", len(publisher.events))
	}
	if event, ok := publisher.events[0].(eventbus.TurnInterrupted); !ok || event.TurnID != "turn-1" {
		t.Fatalf("publisher event = %#v, want TurnInterrupted turn-1", publisher.events[0])
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.RunningTurnID != "" {
		t.Fatalf("RunningTurnID = %q, want cleared", loaded.RunningTurnID)
	}
	if loaded.InterruptedTurnID != "turn-1" {
		t.Fatalf("InterruptedTurnID = %q, want turn-1", loaded.InterruptedTurnID)
	}
	if loaded.InterruptedAt.IsZero() {
		t.Fatal("InterruptedAt is zero, want timestamp")
	}
}

type failingCLIInterruptPublisher struct {
	err    error
	events []eventbus.Event
}

func (p *failingCLIInterruptPublisher) Publish(event eventbus.Event) error {
	p.events = append(p.events, event)
	return p.err
}

func TestSessionProjectorKeepsActiveHistoryValidAfterEachHook(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	session, err := store.SaveMetadata(sessions.SessionV2{
		ID:              "active-history-hooks",
		Version:         sessions.VersionV2,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		SaveToolResults: true,
	})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	projector, err := sessionprojector.New(store, session)
	if err != nil {
		t.Fatalf("sessionprojector.New() error = %v", err)
	}
	defer projector.Close()
	bus := eventbus.NewBus(projector.Handler())
	defer bus.Close()

	publish := func(event eventbus.Event) {
		t.Helper()
		if err := bus.Publish(event); err != nil {
			t.Fatalf("Publish(%T) error = %v", event, err)
		}
	}
	assertValid := func(label string) sessions.SessionV2 {
		t.Helper()
		loaded, err := store.Load(session.ID)
		if err != nil {
			t.Fatalf("%s: Load() error = %v", label, err)
		}
		messages, err := store.MaterializeActiveHistory(loaded)
		if err != nil {
			t.Fatalf("%s: MaterializeActiveHistory() error = %v", label, err)
		}
		if err := validateActiveHistoryToolExchanges(loaded.ID, messages); err != nil {
			t.Fatalf("%s: active history validation error = %v; messages=%#v", label, err, messages)
		}
		return loaded
	}
	activeToolStatuses := func(session sessions.SessionV2) map[string]string {
		itemsByID := make(map[string]sessions.SessionItem, len(session.Items))
		for _, item := range session.Items {
			itemsByID[item.ID] = item
		}
		statuses := map[string]string{}
		for _, id := range session.ActiveHistory {
			item := itemsByID[id]
			if item.Message == nil || item.Message.Role != model.MessageRoleTool {
				continue
			}
			statuses[item.Message.ToolCallID] = item.Status
		}
		return statuses
	}

	assertValid("initial")
	publish(eventbus.TurnStarted{TurnID: "turn-1"})
	assertValid("after TurnStarted")
	publish(eventbus.TurnInputReady{TurnID: "turn-1", Message: model.Message{Role: model.MessageRoleUser, Content: "use tools"}})
	assertValid("after TurnInputReady")

	publish(eventbus.AssistantReady{
		TurnID: "turn-1",
		Message: model.Message{
			Role: model.MessageRoleAssistant,
			ToolCalls: []model.ToolCall{
				{ID: "call-a", Name: "read", Arguments: "{}"},
				{ID: "call-b", Name: "write", Arguments: "{}"},
			},
		},
	})
	afterAssistant := assertValid("after AssistantReady")
	if got, want := activeToolStatuses(afterAssistant), map[string]string{
		"call-a": sessions.ItemStatusPending,
		"call-b": sessions.ItemStatusPending,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active tool statuses after AssistantReady = %#v, want %#v", got, want)
	}

	publish(eventbus.ToolResultReady{TurnID: "turn-1", Result: model.ToolResult{ToolCallID: "call-a", Content: "alpha"}})
	afterFirstResult := assertValid("after first ToolResultReady")
	if got, want := activeToolStatuses(afterFirstResult), map[string]string{
		"call-a": sessions.ItemStatusCompleted,
		"call-b": sessions.ItemStatusPending,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active tool statuses after first ToolResultReady = %#v, want %#v", got, want)
	}

	publish(eventbus.ToolResultReady{TurnID: "turn-1", Result: model.ToolResult{ToolCallID: "call-b", Content: "bravo"}})
	afterSecondResult := assertValid("after second ToolResultReady")
	if got, want := activeToolStatuses(afterSecondResult), map[string]string{
		"call-a": sessions.ItemStatusCompleted,
		"call-b": sessions.ItemStatusCompleted,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active tool statuses after second ToolResultReady = %#v, want %#v", got, want)
	}

	publish(eventbus.AssistantReady{
		TurnID:  "turn-1",
		Message: model.Message{Role: model.MessageRoleAssistant, Content: "done"},
	})
	assertValid("after final AssistantReady")
	publish(eventbus.TurnCompleted{TurnID: "turn-1"})
	assertValid("after TurnCompleted")
}

func TestServerAgentTurnRunnerSupportsIncrementalSessionTurnWithCompaction(t *testing.T) {
	t.Run("compaction disabled", func(t *testing.T) {
		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
		store := sessions.NewV2Store(filepath.Join(configDir, "sessions"))
		projectDir := t.TempDir()
		session, err := store.SaveMetadata(sessions.SessionV2{
			ID:              "incremental-supported",
			Version:         sessions.VersionV2,
			Provider:        "fake",
			ModelProfile:    "default",
			ModelID:         "model-default",
			CWD:             projectDir,
			CreatedCWD:      projectDir,
			ConfigPath:      cliConfigPath(configDir),
			SaveToolResults: true,
		})
		if err != nil {
			t.Fatalf("SaveMetadata() error = %v", err)
		}

		supported, err := (executionAgentTurnRunner{program: "sai"}).SupportsIncrementalSessionTurn(context.Background(), execution.SessionTurnRequest{
			Session:      session,
			SessionStore: store,
			Content:      "hello",
		})
		if err != nil {
			t.Fatalf("SupportsIncrementalSessionTurn() error = %v", err)
		}
		if !supported {
			t.Fatal("SupportsIncrementalSessionTurn() = false, want true")
		}
	})

	t.Run("compaction enabled", func(t *testing.T) {
		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
		appendCLIConfig(t, configDir, `
compaction:
  enabled: true
  threshold_percent: 80
`)
		store := sessions.NewV2Store(filepath.Join(configDir, "sessions"))
		projectDir := t.TempDir()
		session, err := store.SaveMetadata(sessions.SessionV2{
			ID:              "incremental-supported-compaction",
			Version:         sessions.VersionV2,
			Provider:        "fake",
			ModelProfile:    "default",
			ModelID:         "model-default",
			CWD:             projectDir,
			CreatedCWD:      projectDir,
			ConfigPath:      cliConfigPath(configDir),
			SaveToolResults: true,
		})
		if err != nil {
			t.Fatalf("SaveMetadata() error = %v", err)
		}

		supported, err := (executionAgentTurnRunner{program: "sai"}).SupportsIncrementalSessionTurn(context.Background(), execution.SessionTurnRequest{
			Session:      session,
			SessionStore: store,
			Content:      "hello",
		})
		if err != nil {
			t.Fatalf("SupportsIncrementalSessionTurn() error = %v", err)
		}
		if !supported {
			t.Fatal("SupportsIncrementalSessionTurn() = false, want true for compaction-enabled runtime")
		}
	})
}

func TestServerAgentTurnRunnerPublishesIncrementalEventsToPublisher(t *testing.T) {
	shellCommand := blockingShellCommandForCLIIncrementalTest()
	args, err := json.Marshal(map[string]any{
		"command":    shellCommand,
		"timeout_ms": 5000,
	})
	if err != nil {
		t.Fatalf("Marshal(shell args) error = %v", err)
	}
	toolChunk := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"shell","arguments":%q}}]}}]}`, string(args))
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			toolChunk,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"server final"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithTools(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{"shell"})
	sessionRoot := filepath.Join(configDir, "sessions")
	projectDir := t.TempDir()
	store := sessions.NewV2Store(sessionRoot)
	session, err := store.SaveMetadata(sessions.SessionV2{
		ID:              "server-incremental-runner",
		Version:         sessions.VersionV2,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		CWD:             projectDir,
		CreatedCWD:      projectDir,
		ConfigPath:      cliConfigPath(configDir),
		EnabledTools:    []string{"shell"},
		SaveToolResults: true,
	})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	projector, err := sessionprojector.New(store, session)
	if err != nil {
		t.Fatalf("sessionprojector.New() error = %v", err)
	}
	defer projector.Close()
	bus := eventbus.NewBus(projector.Handler())
	defer bus.Close()
	turnID := "turn-incremental"
	if err := bus.Publish(eventbus.TurnStarted{TurnID: turnID}); err != nil {
		t.Fatalf("Publish(TurnStarted) error = %v", err)
	}
	if err := bus.Publish(eventbus.TurnInputReady{TurnID: turnID, Message: model.Message{Role: model.MessageRoleUser, Content: "server prompt"}}); err != nil {
		t.Fatalf("Publish(TurnInputReady) error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type runResult struct {
		result execution.SessionTurnResult
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		result, err := (executionAgentTurnRunner{program: "sai"}).RunSessionTurn(ctx, execution.SessionTurnRequest{
			Session:      session,
			SessionStore: store,
			TurnID:       turnID,
			Content:      "server prompt",
			Publisher:    bus,
		})
		done <- runResult{result: result, err: err}
	}()

	firstRequest := receiveCLIRunRequest(t, requests)
	assertCLIToolNames(t, firstRequest.Body, []string{"shell"})
	pendingTool := waitForCLISessionToolStatus(t, store, session.ID, sessions.ItemStatusPending)
	if pendingTool.Message == nil || pendingTool.Message.ToolCallID != "call_1" {
		t.Fatalf("pending tool item = %#v, want call_1", pendingTool)
	}
	writeCLIFile(t, filepath.Join(projectDir, "release.txt"), "go")

	var outcome runResult
	select {
	case outcome = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for incremental runner")
	}
	if outcome.err != nil {
		_ = bus.Publish(eventbus.TurnInterrupted{TurnID: turnID})
		t.Fatalf("RunSessionTurn() error = %v", outcome.err)
	}
	if !outcome.result.Incremental {
		t.Fatalf("RunSessionTurn() Incremental = false, want true")
	}
	if len(outcome.result.Items) != 0 || len(outcome.result.ActiveHistory) != 0 || outcome.result.Compaction != nil {
		t.Fatalf("incremental result returned legacy save plan: %#v", outcome.result)
	}
	if err := bus.Publish(eventbus.TurnCompleted{TurnID: turnID}); err != nil {
		t.Fatalf("Publish(TurnCompleted) error = %v", err)
	}

	secondRequest := receiveCLIRunRequest(t, requests)
	messages := requestMessages(t, secondRequest.Body)
	toolMessage, ok := messages[len(messages)-1].(map[string]any)
	if !ok {
		t.Fatalf("last request message = %T(%#v), want object", messages[len(messages)-1], messages[len(messages)-1])
	}
	if toolMessage["role"] != "tool" || toolMessage["tool_call_id"] != "call_1" || !strings.Contains(fmt.Sprint(toolMessage["content"]), "blocked tool output") {
		t.Fatalf("last request message = %#v, want shell tool output", toolMessage)
	}
	assertNoAdditionalCLIRunRequest(t, requests)

	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatalf("Load(%s) error = %v", session.ID, err)
	}
	if loaded.RunningTurnID != "" {
		t.Fatalf("RunningTurnID = %q, want cleared", loaded.RunningTurnID)
	}
	var completedTool sessions.SessionItem
	for _, item := range loaded.Items {
		if item.Message != nil && item.Message.ToolCallID == "call_1" {
			completedTool = item
			break
		}
	}
	if completedTool.ID == "" || completedTool.Status != sessions.ItemStatusCompleted || !strings.Contains(completedTool.Message.Content, "blocked tool output") {
		t.Fatalf("completed tool item = %#v, want completed shell output", completedTool)
	}
	materialized, err := store.MaterializeActiveHistory(loaded)
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if err := validateActiveHistoryToolExchanges(loaded.ID, materialized); err != nil {
		t.Fatalf("active history validation error = %v; messages=%#v", err, materialized)
	}
}

func blockingShellCommandForCLIIncrementalTest() string {
	return blockingShellCommandForCLIReleaseFile("release.txt", "blocked tool output")
}

func shellOutputCommandForCLI(output string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("Write-Output '%s'", output)
	}
	return fmt.Sprintf("printf '%%s\\n' '%s'", output)
}

func blockingShellCommandForCLIReleaseFile(filename, output string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("while (!(Test-Path -LiteralPath '%s')) { Start-Sleep -Milliseconds 50 }; Write-Output '%s'", filename, output)
	}
	return fmt.Sprintf("while [ ! -f '%s' ]; do sleep 0.05; done; printf '%%s\\n' '%s'", filename, output)
}

func blockingShellCommandForCLIStartedReleaseDoneFiles(startedFilename, releaseFilename, doneFilename string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("Set-Content -LiteralPath '%s' -Value 'started'; while (!(Test-Path -LiteralPath '%s')) { Start-Sleep -Milliseconds 50 }; Set-Content -LiteralPath '%s' -Value 'done'", startedFilename, releaseFilename, doneFilename)
	}
	return fmt.Sprintf(": > '%s'; while [ ! -f '%s' ]; do sleep 0.05; done; : > '%s'", startedFilename, releaseFilename, doneFilename)
}

func waitForOnlyCLISession(t *testing.T, root string) sessions.SessionV2 {
	t.Helper()

	store := sessions.NewV2Store(root)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		infos, err := store.List()
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(infos) == 1 {
			return loadCLISession(t, root, infos[0].ID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	infos, err := store.List()
	if err != nil {
		t.Fatalf("List() after timeout error = %v", err)
	}
	t.Fatalf("timed out waiting for one CLI session; sessions=%#v", infos)
	return sessions.SessionV2{}
}

func waitForCLISessionToolStatus(t *testing.T, store *sessions.V2Store, sessionID, status string) sessions.SessionItem {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		session, err := store.Load(sessionID)
		if err != nil {
			t.Fatalf("Load(%s) error = %v", sessionID, err)
		}
		for _, item := range session.Items {
			if item.Message != nil && item.Message.Role == model.MessageRoleTool && item.Status == status {
				return item
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	session, err := store.Load(sessionID)
	if err != nil {
		t.Fatalf("Load(%s) after timeout error = %v", sessionID, err)
	}
	t.Fatalf("timed out waiting for tool status %q; items=%#v", status, session.Items)
	return sessions.SessionItem{}
}

func waitForCLISessionToolStatusCount(t *testing.T, store *sessions.V2Store, sessionID, status string, count int) []sessions.SessionItem {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		session, err := store.Load(sessionID)
		if err != nil {
			t.Fatalf("Load(%s) error = %v", sessionID, err)
		}
		items := sessionToolItemsWithStatus(session.Items, status)
		if len(items) == count {
			return items
		}
		time.Sleep(10 * time.Millisecond)
	}
	session, err := store.Load(sessionID)
	if err != nil {
		t.Fatalf("Load(%s) after timeout error = %v", sessionID, err)
	}
	t.Fatalf("timed out waiting for %d tool items with status %q; items=%#v", count, status, session.Items)
	return nil
}

func waitForCLISessionToolCallStatus(t *testing.T, store *sessions.V2Store, sessionID, toolCallID, status string) sessions.SessionItem {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		session, err := store.Load(sessionID)
		if err != nil {
			t.Fatalf("Load(%s) error = %v", sessionID, err)
		}
		for _, item := range session.Items {
			if item.Message != nil && item.Message.Role == model.MessageRoleTool && item.Message.ToolCallID == toolCallID && item.Status == status {
				return item
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	session, err := store.Load(sessionID)
	if err != nil {
		t.Fatalf("Load(%s) after timeout error = %v", sessionID, err)
	}
	t.Fatalf("timed out waiting for tool call %q with status %q; items=%#v", toolCallID, status, session.Items)
	return sessions.SessionItem{}
}

func sessionToolItemsWithStatus(items []sessions.SessionItem, status string) []sessions.SessionItem {
	var matches []sessions.SessionItem
	for _, item := range items {
		if item.Message != nil && item.Message.Role == model.MessageRoleTool && item.Status == status {
			matches = append(matches, item)
		}
	}
	return matches
}

func toolCallIDsForSessionItems(items []sessions.SessionItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item.Message != nil {
			ids = append(ids, item.Message.ToolCallID)
		}
	}
	return ids
}

func toolItemsByCallID(items []sessions.SessionItem) map[string]sessions.SessionItem {
	byCallID := make(map[string]sessions.SessionItem)
	for _, item := range items {
		if item.Message != nil && item.Message.Role == model.MessageRoleTool {
			byCallID[item.Message.ToolCallID] = item
		}
	}
	return byCallID
}

func materializedToolMessageContains(messages []model.Message, toolCallID, content string) bool {
	for _, message := range messages {
		if message.Role == model.MessageRoleTool && message.ToolCallID == toolCallID && strings.Contains(message.Content, content) {
			return true
		}
	}
	return false
}

func TestServerAgentTurnRunnerUsesProvidedSessionOutsideConfigStore(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"server assistant"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	appendCLIConfig(t, configDir, `
sessions:
  dir: config-only-sessions
  save_tool_results: true
`)
	homeRoot, err := sessions.RootForHome(t.TempDir())
	if err != nil {
		t.Fatalf("RootForHome() error = %v", err)
	}
	createdCWD := t.TempDir()
	session := sessions.SessionV2{
		ID:              "server-home-only-session",
		Version:         sessions.VersionV2,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		CWD:             createdCWD,
		CreatedCWD:      createdCWD,
		ConfigPath:      cliConfigPath(configDir),
		SaveToolResults: true,
	}
	homeStore := sessions.NewV2Store(homeRoot)
	saved, err := homeStore.SaveMetadata(session)
	if err != nil {
		t.Fatalf("SaveMetadata(home session) error = %v", err)
	}
	loaded, err := homeStore.Load(saved.ID)
	if err != nil {
		t.Fatalf("Load(home session) error = %v", err)
	}
	configSessionRoot := filepath.Join(configDir, "config-only-sessions")
	if _, err := sessions.NewV2Store(configSessionRoot).Load(saved.ID); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("Load(config sessions.dir session) error = %v, want not found", err)
	}

	result, err := runServerAgentTurnWithProjectorForTest(t, context.Background(), homeStore, loaded, "turn-home-only", "server prompt")
	if err != nil {
		t.Fatalf("RunSessionTurn() error = %v", err)
	}
	assertIncrementalSessionTurnResult(t, result)

	request := receiveCLIRunRequest(t, requests)
	assertNoAdditionalCLIRunRequest(t, requests)
	messages := requestMessages(t, request.Body)
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "server prompt")
	if result.Session.ID != saved.ID {
		t.Fatalf("result session id = %q, want %q", result.Session.ID, saved.ID)
	}
}

func TestServerAgentTurnRunnerHydratesBlobBackedActiveHistoryFromProvidedStore(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"server assistant"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	appendCLIConfig(t, configDir, `
sessions:
  dir: config-only-sessions
  save_tool_results: true
`)
	homeRoot, err := sessions.RootForHome(t.TempDir())
	if err != nil {
		t.Fatalf("RootForHome() error = %v", err)
	}
	createdCWD := t.TempDir()
	homeStore := sessions.NewV2Store(homeRoot)
	session, err := homeStore.SaveMetadata(sessions.SessionV2{
		ID:              "server-home-blob-session",
		Version:         sessions.VersionV2,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		CWD:             createdCWD,
		CreatedCWD:      createdCWD,
		ConfigPath:      cliConfigPath(configDir),
		SaveToolResults: true,
	})
	if err != nil {
		t.Fatalf("SaveMetadata(home session) error = %v", err)
	}
	largePrior := strings.Repeat("prior server-owned blob content ", 300) + "PRIOR-BLOB-TAIL"
	saved, err := homeStore.SaveTurn(session, []sessions.SessionItem{{
		ID:         "prior-large",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: largePrior},
	}}, []string{"prior-large"})
	if err != nil {
		t.Fatalf("SaveTurn(home session) error = %v", err)
	}
	if saved.Items[0].Content == nil || saved.Items[0].Content.Blob == nil || saved.Items[0].Message.Content != "" {
		t.Fatalf("saved prior item = %#v, want blobified active history item", saved.Items[0])
	}
	loaded, err := homeStore.Load(saved.ID)
	if err != nil {
		t.Fatalf("Load(home session) error = %v", err)
	}
	if _, err := loaded.MaterializeActiveHistory(); !errors.Is(err, sessions.ErrCorruptedSession) {
		t.Fatalf("loaded.MaterializeActiveHistory() error = %v, want store-backed materialization required", err)
	}
	configSessionRoot := filepath.Join(configDir, "config-only-sessions")
	if _, err := sessions.NewV2Store(configSessionRoot).Load(saved.ID); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("Load(config sessions.dir session) error = %v, want not found", err)
	}

	result, err := runServerAgentTurnWithProjectorForTest(t, context.Background(), homeStore, loaded, "turn-home-blob", "server prompt")
	if err != nil {
		t.Fatalf("RunSessionTurn() error = %v", err)
	}
	assertIncrementalSessionTurnResult(t, result)

	request := receiveCLIRunRequest(t, requests)
	assertNoAdditionalCLIRunRequest(t, requests)
	messages := requestMessages(t, request.Body)
	if len(messages) != 2 {
		t.Fatalf("len(request messages) = %d, want prior active history plus pending prompt: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "user", largePrior)
	assertMessage(t, messages, 1, "user", "server prompt")
	if result.Session.ID != saved.ID {
		t.Fatalf("result session id = %q, want %q", result.Session.ID, saved.ID)
	}
}

func TestServerAgentTurnRunnerReloadsSessionConfigPathEachTurn(t *testing.T) {
	firstServer, firstRequests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"first assistant"}}]}`,
		`[DONE]`,
	)
	defer firstServer.Close()
	secondServer, secondRequests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"second assistant"}}]}`,
		`[DONE]`,
	)
	defer secondServer.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, firstServer.URL, "direct-secret-value", "openai-chat")
	sessionRoot := filepath.Join(configDir, "sessions")
	createdCWD := t.TempDir()
	session := sessions.SessionV2{
		ID:              "server-reload-config",
		Version:         sessions.VersionV2,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		CWD:             createdCWD,
		CreatedCWD:      createdCWD,
		ConfigPath:      cliConfigPath(configDir),
		SaveToolResults: true,
	}
	writeCLISessionV2(t, sessionRoot, session)
	loaded := loadCLISession(t, sessionRoot, session.ID)

	store := sessions.NewV2Store(sessionRoot)
	firstResult, err := runServerAgentTurnWithProjectorForTest(t, context.Background(), store, loaded, "turn-reload-first", "first prompt")
	if err != nil {
		t.Fatalf("first RunSessionTurn() error = %v", err)
	}
	assertIncrementalSessionTurnResult(t, firstResult)
	firstRequest := receiveCLIRunRequest(t, firstRequests)
	assertNoAdditionalCLIRunRequest(t, secondRequests)
	assertMessage(t, requestMessages(t, firstRequest.Body), 1, "user", "first prompt")
	saved, err := store.Load(loaded.ID)
	if err != nil {
		t.Fatalf("Load(first result) error = %v", err)
	}

	setCLIProviderBaseURL(t, configDir, secondServer.URL)
	secondResult, err := runServerAgentTurnWithProjectorForTest(t, context.Background(), store, saved, "turn-reload-second", "second prompt")
	if err != nil {
		t.Fatalf("second RunSessionTurn() error = %v", err)
	}
	assertIncrementalSessionTurnResult(t, secondResult)

	secondRequest := receiveCLIRunRequest(t, secondRequests)
	assertNoAdditionalCLIRunRequest(t, firstRequests)
	secondMessages := requestMessages(t, secondRequest.Body)
	if len(secondMessages) != 3 {
		t.Fatalf("len(second request messages) = %d, want saved first turn plus pending prompt: %#v", len(secondMessages), secondMessages)
	}
	assertMessage(t, secondMessages, 0, "user", "first prompt")
	assertMessage(t, secondMessages, 1, "assistant", "first assistant")
	assertMessage(t, secondMessages, 2, "user", "second prompt")
	if got, want := secondResult.Session.ConfigPath, cliConfigPath(configDir); got != want {
		t.Fatalf("second result ConfigPath = %q, want %q", got, want)
	}
}

func TestServerAgentTurnRunnerPlansManualCompactionWithoutPersisting(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"delta":{"content":"# Context Checkpoint\n\n## Goal\nContinue.\n\n## Current Progress\nServer compact command is current.\n\n## Decisions Made\nThe HTTP handler owns persistence.\n\n## Constraints / User Preferences\nDo not leak session content.\n\n## Relevant Files / APIs / Commands\nPOST /sessions/{id}/commands/compact.\n\n## Tool State / Environment State\nNo tools.\n\n## Open Questions\nNone.\n\n## Next Steps\nCommit the checkpoint."}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLICompactionConfig(t, configDir, true, "", "")
	sessionRoot := filepath.Join(configDir, "sessions")
	projectDir := t.TempDir()
	systemMessage := model.Message{Role: model.MessageRoleSystem, Content: builtInBaseInstructions}
	userMessage := model.Message{Role: model.MessageRoleUser, Content: "first"}
	assistantMessage := model.Message{Role: model.MessageRoleAssistant, Content: "one"}
	session := sessions.SessionV2{
		ID:                   "server-manual-compact",
		CreatedAt:            time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC),
		UpdatedAt:            time.Date(2026, 7, 3, 12, 1, 0, 0, time.UTC),
		Version:              sessions.VersionV2,
		Provider:             "fake",
		ModelProfile:         "default",
		ModelID:              "model-default",
		CWD:                  projectDir,
		CreatedCWD:           projectDir,
		ConfigPath:           cliConfigPath(configDir),
		InstructionsSnapshot: []model.Message{systemMessage},
		Items: []sessions.SessionItem{
			sessions.SessionItemFromMessage("runtime-000001", systemMessage),
			sessions.SessionItemFromMessage("msg-000002", userMessage),
			sessions.SessionItemFromMessage("msg-000003", assistantMessage),
		},
		ActiveHistory:   []string{"runtime-000001", "msg-000002", "msg-000003"},
		Context:         contextwindow.Metadata{ContextWindow: 10000, ContextWindowSource: string(contextwindow.WindowSourceConfigured)},
		SaveToolResults: true,
	}
	writeCLISessionV2(t, sessionRoot, session)
	loaded := loadCLISession(t, sessionRoot, session.ID)

	result, err := (executionAgentTurnRunner{program: "sai"}).PlanSessionCompaction(context.Background(), execution.SessionCompactionRequest{
		Session: loaded,
	})
	if err != nil {
		t.Fatalf("PlanSessionCompaction() error = %v", err)
	}

	summaryRequest := receiveCLIRunRequest(t, requests)
	assertNoAdditionalCLIRunRequest(t, requests)
	if _, ok := summaryRequest.Body["tools"]; ok {
		t.Fatalf("summary request included tools: %#v", summaryRequest.Body["tools"])
	}
	messages := requestMessages(t, summaryRequest.Body)
	assertMessageContentContains(t, messages, 0, "system", "Create a concise handoff checkpoint")
	assertMessageContentContains(t, messages, 1, "user", "first")
	if result.Compaction.SummaryItem.ID == "" || result.Compaction.Checkpoint.ID == "" {
		t.Fatalf("compaction IDs missing: %#v", result.Compaction)
	}
	if result.Compaction.Checkpoint.Trigger != "manual" || result.Compaction.Checkpoint.Phase != "manual" || result.Compaction.Checkpoint.Reason != "user_requested" {
		t.Fatalf("checkpoint trigger metadata = %#v, want manual user_requested", result.Compaction.Checkpoint)
	}
	if result.Compaction.SummaryItem.Message == nil || !strings.Contains(result.Compaction.SummaryItem.Message.Content, "<compaction_summary>") {
		t.Fatalf("summary item = %#v, want hidden compaction summary", result.Compaction.SummaryItem)
	}

	after := loadCLISession(t, sessionRoot, session.ID)
	if len(after.Compactions) != 0 {
		t.Fatalf("len(Compactions) = %d, want no runner persistence: %#v", len(after.Compactions), after.Compactions)
	}
	if sessionContainsMessageContent(after, "<compaction_summary>") {
		t.Fatalf("session persisted planned compaction summary: %#v", after.Items)
	}
	if !reflect.DeepEqual(after.ActiveHistory, session.ActiveHistory) {
		t.Fatalf("ActiveHistory = %#v, want unchanged %#v", after.ActiveHistory, session.ActiveHistory)
	}
}

func TestServerAgentTurnRunnerPlansAutoCompactionWithoutPersisting(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"delta":{"content":"# Context Checkpoint\n\n## Goal\nContinue.\n\n## Current Progress\nServer auto compact is current.\n\n## Decisions Made\nThe HTTP handler owns persistence.\n\n## Constraints / User Preferences\nDo not leak pending prompt.\n\n## Relevant Files / APIs / Commands\nPOST /sessions/{id}/messages.\n\n## Tool State / Environment State\nNo tools.\n\n## Open Questions\nNone.\n\n## Next Steps\nCommit the checkpoint."}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	setCLICompactionConfigWithThreshold(t, configDir, true, 1, "", "")
	setCLIModelContextWindow(t, configDir, 10000)
	sessionRoot := filepath.Join(configDir, "sessions")
	store := sessions.NewV2Store(sessionRoot)
	projectDir := t.TempDir()
	systemMessage := model.Message{Role: model.MessageRoleSystem, Content: builtInBaseInstructions}
	userMessage := model.Message{Role: model.MessageRoleUser, Content: "first"}
	assistantMessage := model.Message{Role: model.MessageRoleAssistant, Content: "one"}
	session := sessions.SessionV2{
		ID:                   "server-auto-compact-plan",
		CreatedAt:            time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC),
		UpdatedAt:            time.Date(2026, 7, 3, 12, 1, 0, 0, time.UTC),
		Version:              sessions.VersionV2,
		Provider:             "fake",
		ModelProfile:         "default",
		ModelID:              "model-default",
		CWD:                  projectDir,
		CreatedCWD:           projectDir,
		ConfigPath:           cliConfigPath(configDir),
		InstructionsSnapshot: []model.Message{systemMessage},
		Items: []sessions.SessionItem{
			sessions.SessionItemFromMessage("runtime-000001", systemMessage),
			sessions.SessionItemFromMessage("msg-000002", userMessage),
			sessions.SessionItemFromMessage("msg-000003", assistantMessage),
		},
		ActiveHistory:   []string{"runtime-000001", "msg-000002", "msg-000003"},
		Context:         contextwindow.Metadata{ContextWindow: 10000, ContextWindowSource: string(contextwindow.WindowSourceConfigured)},
		SaveToolResults: true,
	}
	writeCLISessionV2(t, sessionRoot, session)
	loaded := loadCLISession(t, sessionRoot, session.ID)

	result, err := (executionAgentTurnRunner{program: "sai"}).PlanSessionTurnCompaction(context.Background(), execution.SessionTurnRequest{
		Session:      loaded,
		SessionStore: store,
		Content:      "second",
	})
	if err != nil {
		t.Fatalf("PlanSessionTurnCompaction() error = %v", err)
	}

	summaryRequest := receiveCLIRunRequest(t, requests)
	assertNoAdditionalCLIRunRequest(t, requests)
	if strings.Contains(string(summaryRequest.RawBody), "second") {
		t.Fatalf("summary request included pending user message: %s", summaryRequest.RawBody)
	}
	if _, ok := summaryRequest.Body["tools"]; ok {
		t.Fatalf("summary request included tools: %#v", summaryRequest.Body["tools"])
	}
	messages := requestMessages(t, summaryRequest.Body)
	assertMessageContentContains(t, messages, 0, "system", "Create a concise handoff checkpoint")
	assertMessageContentContains(t, messages, 1, "user", "first")
	if result.Compaction.SummaryItem.ID == "" || result.Compaction.Checkpoint.ID == "" {
		t.Fatalf("compaction IDs missing: %#v", result.Compaction)
	}
	if result.Compaction.Checkpoint.Trigger != "auto" || result.Compaction.Checkpoint.Phase != "pre_turn" || result.Compaction.Checkpoint.Reason != "context_limit" {
		t.Fatalf("checkpoint trigger metadata = %#v, want auto pre_turn context_limit", result.Compaction.Checkpoint)
	}
	if result.Compaction.SummaryItem.Message == nil || !strings.Contains(result.Compaction.SummaryItem.Message.Content, "<compaction_summary>") {
		t.Fatalf("summary item = %#v, want hidden compaction summary", result.Compaction.SummaryItem)
	}

	after := loadCLISession(t, sessionRoot, session.ID)
	if len(after.Compactions) != 0 {
		t.Fatalf("len(Compactions) = %d, want no runner persistence: %#v", len(after.Compactions), after.Compactions)
	}
	if sessionContainsExactMessageContent(after, "second") {
		t.Fatalf("session persisted pending user prompt: %#v", after.Items)
	}
	if sessionContainsMessageContent(after, "<compaction_summary>") {
		t.Fatalf("session persisted planned compaction summary: %#v", after.Items)
	}
	if !reflect.DeepEqual(after.ActiveHistory, session.ActiveHistory) {
		t.Fatalf("ActiveHistory = %#v, want unchanged %#v", after.ActiveHistory, session.ActiveHistory)
	}
}

func TestAutoCompactionThresholdUsesStrictExceedsBoundary(t *testing.T) {
	if autoCompactionThresholdExceeded(80, 100, 80) {
		t.Fatal("autoCompactionThresholdExceeded(80, 100, 80) = true, want false at threshold")
	}
	if !autoCompactionThresholdExceeded(81, 100, 80) {
		t.Fatal("autoCompactionThresholdExceeded(81, 100, 80) = false, want true above threshold")
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--prompt", "first"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat"}, strings.NewReader("first prompt secret\nsecond prompt secret\n/quit\n"), &stdout, &stderr, func() (string, error) {
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

func TestChatResumeSendsOnlyActiveHistoryAndSavesContinuation(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"two"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	sessionRoot := filepath.Join(configDir, "sessions")
	savedInstruction := model.Message{Role: model.MessageRoleSystem, Content: "saved instructions"}
	oldVisible := model.Message{Role: model.MessageRoleUser, Content: "old visible item secret"}
	activeUser := model.Message{Role: model.MessageRoleUser, Content: "first"}
	activeAssistant := model.Message{Role: model.MessageRoleAssistant, Content: "one"}
	writeCLISessionV2(t, sessionRoot, sessions.SessionV2{
		ID:              "resume-session",
		CreatedAt:       time.Date(2026, 7, 2, 3, 4, 5, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 7, 2, 3, 4, 6, 0, time.UTC),
		Version:         sessions.VersionV2,
		Provider:        "fake",
		ModelProfile:    "fast",
		ModelID:         "model-fast",
		ModelParameters: map[string]any{"temperature": 0.2, "max_tokens": 64},
		ConfigPath:      cliConfigPath(configDir),
		InstructionsSnapshot: []model.Message{
			savedInstruction,
		},
		Items: []sessions.SessionItem{
			sessions.SessionItemFromMessage("runtime-saved", savedInstruction),
			sessions.SessionItemFromMessage("old-visible", oldVisible),
			sessions.SessionItemFromMessage("active-user", activeUser),
			sessions.SessionItemFromMessage("active-assistant", activeAssistant),
		},
		ActiveHistory:   []string{"runtime-saved", "active-user", "active-assistant"},
		SaveToolResults: true,
	})
	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "AGENTS.md"), "current project instruction secret\n")

	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--resume", "resume-session", "--quit", "--prompt", "next"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	assertResumableSessionNoticeOnce(t, stderr.String())
	assertCLIErrorOmits(t, stderr.String(), "first", "one", "next", "two", "old visible item secret", "current project instruction secret")
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
	for _, leaked := range []string{"old visible item secret", "current project instruction secret"} {
		if strings.Contains(string(request.RawBody), leaked) {
			t.Fatalf("resume request included inactive/current content %q: %s", leaked, request.RawBody)
		}
	}
	assertNoAdditionalCLIRunRequest(t, requests)

	session := loadCLISession(t, sessionRoot, "resume-session")
	active := activeCLIMessages(t, session)
	if len(active) != 5 {
		t.Fatalf("len(saved messages) = %d, want 5: %#v", len(active), active)
	}
	assertSavedMessage(t, active, 3, model.MessageRoleUser, "next")
	assertSavedMessage(t, active, 4, model.MessageRoleAssistant, "two")
	if len(session.Items) != 6 {
		t.Fatalf("len(session.Items) = %d, want inactive item plus appended active items: %#v", len(session.Items), session.Items)
	}
}

func TestChatResumeRejectsCorruptedActiveHistory(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected assistant secret"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	tests := []struct {
		name    string
		session sessions.SessionV2
		want    string
	}{
		{
			name: "missing item ref",
			session: sessions.SessionV2{
				ID:              "missing-ref-session",
				Version:         sessions.VersionV2,
				Provider:        "fake",
				ModelProfile:    "default",
				ModelID:         "model-default",
				ActiveHistory:   []string{"missing"},
				SaveToolResults: true,
			},
			want: `active history references missing item "missing"`,
		},
		{
			name: "orphan tool result",
			session: sessions.SessionV2{
				ID:           "orphan-tool-session",
				Version:      sessions.VersionV2,
				Provider:     "fake",
				ModelProfile: "default",
				ModelID:      "model-default",
				Items: []sessions.SessionItem{
					sessions.SessionItemFromMessage("tool-only", model.Message{Role: model.MessageRoleTool, ToolCallID: "call_1", Content: "tool result secret"}),
				},
				ActiveHistory:   []string{"tool-only"},
				SaveToolResults: true,
			},
			want: `references unresolved tool call "call_1"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configDir := t.TempDir()
			writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
			writeCLISessionV2(t, filepath.Join(configDir, "sessions"), tt.session)

			var stdout, stderr bytes.Buffer
			code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--resume", tt.session.ID, "--quit", "--prompt", "next prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
				return t.TempDir(), nil
			})

			if code != 1 {
				t.Fatalf("RunWithIO() code = %d, want 1", code)
			}
			if stdout.String() != "" {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			assertCLIErrorContains(t, stderr.String(), "corrupted session", tt.want)
			assertCLIErrorOmits(t, stderr.String(), "next prompt secret", "tool result secret", "unexpected assistant secret", "direct-secret-value")
			assertNoAdditionalCLIRunRequest(t, requests)
		})
	}
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--continue", "--quit", "--prompt", "next"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--resume", "small-context-session", "--quit", "--prompt", "next prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--resume", "reasoning-session", "--quit", "--prompt", "next prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
			code := runInProcessRuntimeWithIO(args, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--resume", "reasoning-enabled-session", "--show-reasoning=false", "--quit", "--prompt", "next"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"chat", "--resume", "some-session", "--continue"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
			code := runInProcessRuntimeWithIO(args, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--resume", "partial-session", "--quit", "--prompt", "next prompt secret"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
			code := runInProcessRuntimeWithIO(args, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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

func TestPluralSessionsRejectsLegacyCommandsAndCWD(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "show", args: []string{"sessions", "show", "show-session"}},
		{name: "show help flag", args: []string{"sessions", "show", "-h"}},
		{name: "delete", args: []string{"sessions", "delete", "delete-session"}},
		{name: "prune", args: []string{"sessions", "prune", "--keep", "1"}},
		{name: "group cwd", args: []string{"sessions", "--cwd", t.TempDir()}},
		{name: "list cwd", args: []string{"sessions", "list", "--cwd", t.TempDir()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
			assertCLIErrorContains(t, stderr.String(), `unknown command "sessions"`, `Run "sai help" for usage.`)
			for _, forbidden := range []string{
				"usage: sai sessions",
				"usage: sai sessions show",
				"usage: sai sessions delete",
				"usage: sai sessions prune",
				`Alias for "sai session list"`,
				"Deletes one resumable session",
				"--keep must be provided",
			} {
				if strings.Contains(stderr.String(), forbidden) {
					t.Fatalf("sessions legacy rejection printed %q:\n%s", forbidden, stderr.String())
				}
			}
		})
	}

	for _, args := range [][]string{
		{"help", "sessions"},
		{"help", "sessions", "list"},
		{"help", "sessions", "show"},
		{"help", "sessions", "delete"},
		{"help", "sessions", "prune"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})
			if code != 1 {
				t.Fatalf("RunWithGetwd(%v) code = %d, want 1", args, code)
			}
			if stdout.String() != "" {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			assertCLIErrorContains(t, stderr.String(), "unknown help topic", `Run "sai help" for usage.`)
			if strings.Contains(stderr.String(), "usage: sai sessions show") ||
				strings.Contains(stderr.String(), "usage: sai sessions delete") ||
				strings.Contains(stderr.String(), "usage: sai sessions prune") {
				t.Fatalf("legacy help topic printed old usage:\n%s", stderr.String())
			}
		})
	}
}

func TestSessionResumeUsesExecutionServiceWithoutCWDDiscovery(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	configDir := filepath.Join(projectDir, ".agents")
	writeCLIRunFixtureInDir(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
	saveCLIExecutionSession(t, home, "existing-session", projectDir, configDir)
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--server-root", home, "session", "resume", "existing-session"}, strings.NewReader("/quit\n"), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("session resume code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIOutputContains(t, stderr.String(), "sai: attached to session existing-session")
	assertCLIErrorOmits(t, stderr.String(), "direct-secret-value")
}

func TestSessionResumeOmitsCWDDiscovery(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	configDir := filepath.Join(projectDir, ".agents")
	writeCLIRunFixtureInDir(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
	saveCLIExecutionSession(t, home, "existing-session", projectDir, configDir)
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--server-root", home, "session", "resume", "existing-session"}, strings.NewReader("/quit\n"), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("session resume code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIOutputContains(t, stderr.String(), "sai: attached to session existing-session")
	assertCLIErrorOmits(t, stderr.String(), "direct-secret-value")
}

func TestSessionResumeRendersSnapshotAndStreamsAfterNewestSeq(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	configDir := filepath.Join(projectDir, ".agents")
	writeCLIRunFixtureInDir(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")
	saveCLIExecutionSession(t, home, "existing-session", projectDir, configDir)
	if _, err := cliSessionStore(t, home).AppendItemsAndReplaceActiveHistory("existing-session", []sessions.SessionItem{
		{
			ID:         "user-visible",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "hello snapshot"},
		},
		{
			ID:         "assistant-visible",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant},
			Content:    &sessions.StoredContent{Preview: "assistant preview"},
		},
		{
			ID:         "hidden-summary",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityHidden,
			Audience:   sessions.ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "hidden summary secret"},
		},
		{
			ID:         "debug-record",
			Kind:       sessions.ItemKindRuntimeContext,
			Visibility: sessions.ItemVisibilityDebug,
			Audience:   sessions.ItemAudienceInternal,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "debug secret"},
		},
	}, []string{"user-visible", "assistant-visible"}); err != nil {
		t.Fatalf("AppendItemsAndReplaceActiveHistory() error = %v", err)
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--server-root", home, "session", "resume", "existing-session"}, strings.NewReader("/quit\n"), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("session resume code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "user: hello snapshot\nassistant: assistant preview\n"; got != want {
		t.Fatalf("stdout = %q, want snapshot %q", got, want)
	}
	assertCLIOutputContains(t, stderr.String(), "sai: attached to session existing-session")
	assertCLIErrorOmits(t, stderr.String(), "hidden summary secret", "debug secret", "direct-secret-value")
}

func TestSessionResumeRejectsCWDAndConfigBeforeDiscovery(t *testing.T) {
	projectDir := t.TempDir()
	missingConfig := filepath.Join(t.TempDir(), "missing.yaml")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "explicit id cwd",
			args: []string{"session", "resume", "existing-session", "--cwd", projectDir},
			want: "--cwd cannot be used when resuming an existing session",
		},
		{
			name: "explicit id config",
			args: []string{"--config", missingConfig, "session", "resume", "existing-session"},
			want: "--config cannot be used when resuming an existing session",
		},
		{
			name: "explicit id config after command",
			args: []string{"session", "resume", "existing-session", "--config", missingConfig},
			want: "--config cannot be used when resuming an existing session",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(tt.args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})
			if code != 1 {
				t.Fatalf("session resume code = %d, want 1", code)
			}
			if stdout.String() != "" {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			assertCLIErrorContains(t, stderr.String(), tt.want, `Run "sai help session resume" for usage.`)
			assertCLIErrorOmits(t, stderr.String(), "getwd should not be called", "no healthy sai server found")
		})
	}
}

func TestBareDefaultAttachPendingFirstPromptCreatesStreamsAndSends(t *testing.T) {
	runPendingAttachFirstPrompt(t, nil)
}

func runPendingAttachFirstPrompt(t *testing.T, args []string) {
	t.Helper()
	home := t.TempDir()
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "child")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(childDir) error = %v", err)
	}
	configDir := filepath.Join(childDir, ".agents")
	modelServer, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"pending response"}}]}`,
		`[DONE]`,
	)
	defer modelServer.Close()
	writeCLIRunFixtureInDir(t, configDir, modelServer.URL, "direct-secret-value", "openai-chat")
	canonicalChild, err := projectstore.CanonicalRoot(childDir)
	if err != nil {
		t.Fatalf("CanonicalRoot(childDir) error = %v", err)
	}
	configPath := filepath.Join(configDir, "sai.yaml")
	prompt := "hello pending"
	project, _, err := cliProjectStore(t, home).Create(childDir, "Current")
	if err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	forbidCLIBackgroundStart(t)
	if args == nil {
		args = []string{"--server-root", home}
	} else {
		args = append([]string{"--server-root", home}, args...)
	}

	var stdout, stderr bytes.Buffer
	code := RunWithIO(args, strings.NewReader(prompt+"\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return childDir, nil
	})

	if code != 0 {
		t.Fatalf("pending attach code = %d, stderr = %s", code, stderr.String())
	}
	request := receiveCLIRunRequest(t, requests)
	if got := countCLIRequestMessagesWithContent(t, requestMessages(t, request.Body), "user", prompt); got != 1 {
		t.Fatalf("request user prompt count = %d, want 1", got)
	}
	assertNoAdditionalCLIRunRequest(t, requests)
	infos, err := cliSessionStore(t, home).ListWithOptions(sessions.V2ListOptions{})
	if err != nil {
		t.Fatalf("ListWithOptions() error = %v", err)
	}
	if len(infos) != 1 || infos[0].ProjectID != project.ID {
		t.Fatalf("stored sessions = %#v, want one project session", infos)
	}
	session, err := cliSessionStore(t, home).Load(infos[0].ID)
	if err != nil {
		t.Fatalf("Load(created session) error = %v", err)
	}
	if session.CreatedCWD != canonicalChild || session.RootConfigPath() != configPath {
		t.Fatalf("created session cwd/config = %q/%q, want %q/%q", session.CreatedCWD, session.RootConfigPath(), canonicalChild, configPath)
	}
	if got, want := stdout.String(), "pending response\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIOutputContains(t, stderr.String(), "sai: attached to session "+session.ID)
	assertCLIErrorOmits(t, stderr.String(), prompt, "direct-secret-value")
}

func TestPendingAttachNoIDUsesConfigAndCWDOverridesOnFirstPrompt(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "child")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(childDir) error = %v", err)
	}
	configDir := t.TempDir()
	modelServer, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"override response"}}]}`,
		`[DONE]`,
	)
	defer modelServer.Close()
	writeCLIRunFixtureInDir(t, configDir, modelServer.URL, "direct-secret-value", "openai-chat")
	configPath := filepath.Join(configDir, "custom.yaml")
	if err := os.Rename(filepath.Join(configDir, "sai.yaml"), configPath); err != nil {
		t.Fatalf("Rename(config) error = %v", err)
	}
	canonicalChild, err := projectstore.CanonicalRoot(childDir)
	if err != nil {
		t.Fatalf("CanonicalRoot(%q) error = %v", childDir, err)
	}
	prompt := "override prompt"
	project, _, err := cliProjectStore(t, home).Create(projectDir, "Current")
	if err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--server-root", home, "--config", configPath, "--cwd", childDir}, strings.NewReader(prompt+"\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("pending override attach code = %d, stderr = %s", code, stderr.String())
	}
	request := receiveCLIRunRequest(t, requests)
	if got := countCLIRequestMessagesWithContent(t, requestMessages(t, request.Body), "user", prompt); got != 1 {
		t.Fatalf("request user prompt count = %d, want 1", got)
	}
	assertNoAdditionalCLIRunRequest(t, requests)
	infos, err := cliSessionStore(t, home).ListWithOptions(sessions.V2ListOptions{})
	if err != nil {
		t.Fatalf("ListWithOptions() error = %v", err)
	}
	if len(infos) != 1 || infos[0].ProjectID != project.ID {
		t.Fatalf("stored sessions = %#v, want one project session", infos)
	}
	session, err := cliSessionStore(t, home).Load(infos[0].ID)
	if err != nil {
		t.Fatalf("Load(created session) error = %v", err)
	}
	if session.CreatedCWD != canonicalChild || session.RootConfigPath() != configPath {
		t.Fatalf("created session cwd/config = %q/%q, want %q/%q", session.CreatedCWD, session.RootConfigPath(), canonicalChild, configPath)
	}
	if got, want := stdout.String(), "override response\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIOutputContains(t, stderr.String(), "sai: attached to session "+session.ID)
	assertCLIErrorOmits(t, stderr.String(), prompt, "direct-secret-value")
}

func TestBareDefaultAttachAutoCreatesProjectBeforeFirstPrompt(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--server-root", home}, strings.NewReader("/quit\n"), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("bare attach code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorOmits(t, stderr.String(), "attached to session", "no registered project found")
	projects, err := cliProjectStore(t, home).ListWithOptions(projectstore.ListOptions{})
	if err != nil {
		t.Fatalf("ListWithOptions(projects) error = %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects = %#v, want one auto-created project", projects)
	}
	canonicalRoot, err := projectstore.CanonicalRoot(projectDir)
	if err != nil {
		t.Fatalf("CanonicalRoot(%q) error = %v", projectDir, err)
	}
	if projects[0].Root != canonicalRoot {
		t.Fatalf("auto-created project root = %q, want %q", projects[0].Root, canonicalRoot)
	}
	infos, err := cliSessionStore(t, home).ListWithOptions(sessions.V2ListOptions{All: true})
	if err != nil {
		t.Fatalf("ListWithOptions(sessions) error = %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("sessions = %#v, want none before first prompt", infos)
	}
}

func TestBareDefaultAttachImmediateQuitIgnoresExistingProjectSessions(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	project, _, err := cliProjectStore(t, home).Create(projectDir, "Current")
	if err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	if _, err := cliSessionStore(t, home).SaveMetadata(sessions.SessionV2{ID: "existing-project-session", ProjectID: project.ID}); err != nil {
		t.Fatalf("SaveMetadata(existing project session) error = %v", err)
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--server-root", home}, strings.NewReader("/quit\n"), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("attach code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if strings.Contains(stderr.String(), "attached to session") {
		t.Fatalf("stderr = %q, want no durable attach notice", stderr.String())
	}
}

func TestBareDefaultAttachCreatesSessionStreamsAndSendsPrompts(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	configDir := filepath.Join(projectDir, ".agents")
	modelServer, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"hello "}}]}`,
		`{"choices":[{"delta":{"content":"world"}}]}`,
		`[DONE]`,
	)
	defer modelServer.Close()
	writeCLIRunFixtureInDirWithTools(t, configDir, modelServer.URL, "direct-secret-value", "openai-chat", []string{"read_file"})
	mcpExitFile := filepath.Join(t.TempDir(), "mcp-exited")
	writeCLIRunMCPServerFixture(t, filepath.Join(configDir, "mcp"), "local", mcpExitFile, true)
	setCLIAgentShowReasoning(t, configDir, true)
	skillDir := filepath.Join(configDir, "skills", "visible")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(skillDir) error = %v", err)
	}
	writeCLIFile(t, filepath.Join(skillDir, "SKILL.md"), "---\nname: Visible Skill\n---\nVisible skill instructions\n")
	hiddenSkillDir := filepath.Join(configDir, "skills", "hidden")
	if err := os.MkdirAll(hiddenSkillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(hiddenSkillDir) error = %v", err)
	}
	writeCLIFile(t, filepath.Join(hiddenSkillDir, "SKILL.md"), "---\ndisable-model-invocation: true\n---\nHidden skill instructions\n")
	canonicalRoot, err := projectstore.CanonicalRoot(projectDir)
	if err != nil {
		t.Fatalf("CanonicalRoot(%q) error = %v", projectDir, err)
	}
	configPath := filepath.Join(configDir, "sai.yaml")
	prompt := "hello attach"
	project, _, err := cliProjectStore(t, home).Create(projectDir, "Current")
	if err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--server-root", home, "--cwd", projectDir}, strings.NewReader(prompt+"\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("bare attach code = %d, stderr = %s", code, stderr.String())
	}
	request := receiveCLIRunRequest(t, requests)
	if got := countCLIRequestMessagesWithContent(t, requestMessages(t, request.Body), "user", prompt); got != 1 {
		t.Fatalf("request user prompt count = %d, want 1", got)
	}
	assertNoAdditionalCLIRunRequest(t, requests)
	infos, err := cliSessionStore(t, home).ListWithOptions(sessions.V2ListOptions{})
	if err != nil {
		t.Fatalf("ListWithOptions() error = %v", err)
	}
	if len(infos) != 1 || infos[0].ProjectID != project.ID {
		t.Fatalf("stored sessions = %#v, want one project session", infos)
	}
	session, err := cliSessionStore(t, home).Load(infos[0].ID)
	if err != nil {
		t.Fatalf("Load(created session) error = %v", err)
	}
	if session.CreatedCWD != canonicalRoot || session.RootConfigPath() != configPath {
		t.Fatalf("created session cwd/config = %q/%q, want %q/%q", session.CreatedCWD, session.RootConfigPath(), canonicalRoot, configPath)
	}
	if !reflect.DeepEqual(session.EnabledTools, []string{"read_file"}) || !reflect.DeepEqual(session.EnabledMCP, []string{"local"}) || !reflect.DeepEqual(session.EnabledSkills, []string{"visible"}) {
		t.Fatalf("created session enabled metadata = tools %#v mcp %#v skills %#v", session.EnabledTools, session.EnabledMCP, session.EnabledSkills)
	}
	if !session.ShowReasoning || !session.SaveToolResults {
		t.Fatalf("created session booleans = show %v save %v, want true/true", session.ShowReasoning, session.SaveToolResults)
	}
	if got, want := stdout.String(), "hello world\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIOutputContains(t, stderr.String(), "sai: attached to session "+session.ID)
	assertCLIErrorOmits(t, stderr.String(), prompt, "direct-secret-value", "Hidden skill instructions")
	assertCLIFileEventuallyContains(t, mcpExitFile, "closed")
}

func TestBareDefaultAttachPendingImmediateQuitCreatesNoSession(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	if _, _, err := cliProjectStore(t, home).Create(projectDir, "Current"); err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--server-root", home, "--cwd", projectDir}, strings.NewReader("/quit\n"), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("bare attach code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if strings.Contains(stderr.String(), "attached to session") {
		t.Fatalf("stderr = %q, want no durable attach notice", stderr.String())
	}
	infos, err := cliSessionStore(t, home).ListWithOptions(sessions.V2ListOptions{All: true})
	if err != nil {
		t.Fatalf("ListWithOptions() error = %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("sessions = %#v, want none before first prompt", infos)
	}
}

func TestPendingAttachCompactBeforeFirstMessageDoesNotCreateSession(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	if _, _, err := cliProjectStore(t, home).Create(projectDir, "Current"); err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--server-root", home}, strings.NewReader("/compact\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("pending compact code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "compact requires a session", "send a message first")
	infos, err := cliSessionStore(t, home).ListWithOptions(sessions.V2ListOptions{All: true})
	if err != nil {
		t.Fatalf("ListWithOptions() error = %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("sessions = %#v, want none before first prompt", infos)
	}
}

func TestSessionResumeWaitsForTerminalStreamEventBeforeQuit(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	configDir := filepath.Join(projectDir, ".agents")
	prompt := "delayed prompt"
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		if got := countCLIRequestMessagesWithContent(t, requestMessages(t, decodeCLIJSON(t, body)), "user", prompt); got != 1 {
			t.Errorf("request user prompt count = %d, want 1", got)
		}
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"delayed text\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer modelServer.Close()
	writeCLIRunFixtureInDir(t, configDir, modelServer.URL, "direct-secret-value", "openai-chat")
	saveCLIExecutionSession(t, home, "existing-session", projectDir, configDir)
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--server-root", home, "session", "resume", "existing-session"}, strings.NewReader(prompt+"\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("session resume delayed code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "delayed text\n"; got != want {
		t.Fatalf("stdout = %q, want delayed terminal stream text %q", got, want)
	}
	assertCLIErrorOmits(t, stderr.String(), prompt, "direct-secret-value")
}

func TestSessionResumeFailedTurnWaitsForDelayedTurnFailed(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	configDir := filepath.Join(projectDir, ".agents")
	prompt := "prompt before failed turn"
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "provider leaked prompt before failed turn and assistant secret", http.StatusInternalServerError)
	}))
	defer modelServer.Close()
	writeCLIRunFixtureInDir(t, configDir, modelServer.URL, "direct-secret-value", "openai-chat")
	saveCLIExecutionSession(t, home, "existing-session", projectDir, configDir)
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--server-root", home, "session", "resume", "existing-session"}, strings.NewReader(prompt+"\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("session resume failed turn code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIOutputContains(t, stderr.String(), "sai: turn failed")
	assertCLIErrorOmits(t, stderr.String(), "send failed", prompt, "assistant secret", "direct-secret-value")

	logPaths := sessionLogPaths(t, configDir)
	if len(logPaths) != 1 {
		t.Fatalf("session log paths = %#v, want one failed turn log", logPaths)
	}
	data, err := os.ReadFile(logPaths[0])
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", logPaths[0], err)
	}
	records := readJSONLRecords(t, data)
	errorRecord := firstCLILogRecord(t, records, "error")
	errorDetail, ok := errorRecord["error"].(string)
	if errorRecord["message"] != "request model" || !ok || !strings.Contains(errorDetail, "500 Internal Server Error") {
		t.Fatalf("error log record = %#v, want failed provider status detail", errorRecord)
	}
	for _, leaked := range []string{prompt, "assistant secret", "direct-secret-value", "Bearer direct-secret-value"} {
		if strings.Contains(string(data), leaked) {
			t.Fatalf("error log leaked %q: %s", leaked, string(data))
		}
	}
}

func TestSessionResumeMultilineCompactIsSentAsPrompt(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	configDir := filepath.Join(projectDir, ".agents")
	modelServer, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"sent compact text"}}]}`,
		`[DONE]`,
	)
	defer modelServer.Close()
	writeCLIRunFixtureInDir(t, configDir, modelServer.URL, "direct-secret-value", "openai-chat")
	saveCLIExecutionSession(t, home, "existing-session", projectDir, configDir)
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--server-root", home, "session", "resume", "existing-session"}, strings.NewReader("\"\"\"\n/compact\n\"\"\"\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("session resume multiline compact code = %d, stderr = %s", code, stderr.String())
	}
	request := receiveCLIRunRequest(t, requests)
	assertMessage(t, requestMessages(t, request.Body), 1, "user", "/compact")
	assertNoAdditionalCLIRunRequest(t, requests)
	if got, want := stdout.String(), "sent compact text\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if strings.Contains(stderr.String(), "compacted session context") {
		t.Fatalf("stderr = %q, want no compaction status", stderr.String())
	}
}

func TestSessionResumeCompactAndNormalMessageContainingCompact(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	configDir := filepath.Join(projectDir, ".agents")
	prompt := "normal /compact text"
	modelServer, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"delta":{"content":"manual compact summary"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"sent"}}]}`,
			`[DONE]`,
		},
	)
	defer modelServer.Close()
	writeCLIRunFixtureInDir(t, configDir, modelServer.URL, "direct-secret-value", "openai-chat")
	setCLICompactionConfig(t, configDir, true, "", "")
	saveCLIExecutionSession(t, home, "existing-session", projectDir, configDir)
	seedCompleteVisibleTurns(t, cliSessionStore(t, home), "existing-session")
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--server-root", home, "session", "resume", "existing-session"}, strings.NewReader("/compact\n"+prompt+"\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("session resume code = %d, stderr = %s", code, stderr.String())
	}
	summaryRequest := receiveCLIRunRequest(t, requests)
	assertMessageContentContains(t, requestMessages(t, summaryRequest.Body), 0, "system", "Create a concise handoff checkpoint")
	sendRequest := receiveCLIRunRequest(t, requests)
	if got := countCLIRequestMessagesWithContent(t, requestMessages(t, sendRequest.Body), "user", prompt); got != 1 {
		t.Fatalf("request user prompt count = %d, want 1", got)
	}
	assertNoAdditionalCLIRunRequest(t, requests)
	if got, want := stdout.String(), seedCompleteVisibleTurnsSnapshot()+"sent\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIOutputContains(t, stderr.String(), "sai: compacted session context")
	assertCLIErrorOmits(t, stderr.String(), prompt, "direct-secret-value")
	session, err := cliSessionStore(t, home).Load("existing-session")
	if err != nil {
		t.Fatalf("Load(existing-session) error = %v", err)
	}
	if len(session.Compactions) != 1 {
		t.Fatalf("Compactions = %#v, want one manual compaction", session.Compactions)
	}
}

func TestSessionResumeCompactWaitsWithoutDefaultHTTPTimeout(t *testing.T) {
	home := t.TempDir()
	projectDir := t.TempDir()
	configDir := filepath.Join(projectDir, ".agents")
	requests := make(chan capturedCLIRunRequest, 1)
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		requests <- capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}
		time.Sleep(650 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"slow compact summary\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer modelServer.Close()
	writeCLIRunFixtureInDir(t, configDir, modelServer.URL, "direct-secret-value", "openai-chat")
	setCLICompactionConfig(t, configDir, true, "", "")
	saveCLIExecutionSession(t, home, "existing-session", projectDir, configDir)
	seedCompleteVisibleTurns(t, cliSessionStore(t, home), "existing-session")
	forbidCLIBackgroundStart(t)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--server-root", home, "session", "resume", "existing-session"}, strings.NewReader("/compact\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("session resume slow compact code = %d, stderr = %s", code, stderr.String())
	}
	_ = receiveCLIRunRequest(t, requests)
	assertNoAdditionalCLIRunRequest(t, requests)
	if got, want := stdout.String(), seedCompleteVisibleTurnsSnapshot(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIOutputContains(t, stderr.String(), "sai: compacted session context")
	assertCLIErrorOmits(t, stderr.String(), "direct-secret-value")
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
		done <- runInProcessRuntimeWithContext(ctx, []string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "hello"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
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
		done <- runInProcessRuntimeWithContext(ctx, []string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "user prompt secret"}, strings.NewReader(""), stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Say hi"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Say hi"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Say hi"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "read_file", "--prompt", "Read note"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Say hi"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "read_file", "--prompt", "Read note"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--provider", "fake", "--model", "fast", "--prompt", "Use fast"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Use configured tools"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "glob_files,grep_files", "--prompt", "Use discovery tools"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "No helpers configured"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Bad tool config"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Do not expose tools"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "list_files,write_file,edit_file", "--prompt", "Use CLI tools"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Use configured prompt"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Use bad prompt"}, &stdout, &stderr, func() (string, error) {
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
		done <- runInProcessRuntimeWithContext(ctx, []string{"--config", cliConfigPath(configDir), "chat", "--prompt", "delegate"}, stdinReader, stdout, stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Choose helper"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "delegate"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "delegate"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "delegate"}, &stdout, &stderr, func() (string, error) {
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

func TestSubagentCompletionPromptFormatsSingleAndMultipleCompletions(t *testing.T) {
	first := subagents.JobSnapshot{
		JobID:       "job-1",
		AgentID:     "reviewer",
		DisplayName: "Review UI",
		JobName:     "review-1",
		Status:      subagents.StatusCompleted,
		Output:      "first output",
	}
	second := subagents.JobSnapshot{
		JobID:   "job-2",
		AgentID: "researcher",
		Status:  subagents.StatusFailed,
		Error:   "second error",
	}

	if got, want := subagentCompletionPrompt([]subagents.JobSnapshot{first}), formatSubagentCompletionEvent(first); got != want {
		t.Fatalf("single completion prompt = %q, want exact event %q", got, want)
	}

	got := subagentCompletionPrompt([]subagents.JobSnapshot{first, second})
	want := formatSubagentCompletionEvent(first) + "\n\n" + formatSubagentCompletionEvent(second)
	if got != want {
		t.Fatalf("multi completion prompt = %q, want %q", got, want)
	}
}

func TestRunPersistsSubagentCompletionWithSessionProjector(t *testing.T) {
	childDone := make(chan struct{})
	var childDoneOnce sync.Once
	childServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
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
	setCLISessionsConfig(t, configDir, true, true)
	appendCLIConfig(t, configDir, `
subagents:
  reviewer: subagents/reviewer.yaml
`)
	writeCLIChildSubagentConfig(t, configDir, childServer.URL+"/v1")

	var stdout, stderr bytes.Buffer
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "delegate"}, &stdout, &stderr, func() (string, error) {
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

	sessionRoot := filepath.Join(configDir, "sessions")
	store := sessions.NewV2Store(sessionRoot)
	session := loadOnlyCLISession(t, sessionRoot)
	messages, err := store.MaterializeActiveHistory(session)
	if err != nil {
		t.Fatalf("MaterializeActiveHistory(%q) error = %v", session.ID, err)
	}
	if !messagesContainRoleContent(messages, model.MessageRoleUser, "Runtime event: subagent job completed") {
		t.Fatalf("saved messages missing completion user event: %#v", messages)
	}
	if !messagesContainRoleContent(messages, model.MessageRoleAssistant, "parent handled child") {
		t.Fatalf("saved messages missing completion assistant response: %#v", messages)
	}

	completionUserID := sessionItemIDWithRoleContent(t, session.Items, model.MessageRoleUser, "Runtime event: subagent job completed")
	completionAssistantID := sessionItemIDWithRoleContent(t, session.Items, model.MessageRoleAssistant, "parent handled child")
	records := readCLISessionJSONLRecords(t, sessionRoot, session.ID)
	userTxID := appendedSessionItemTxID(t, records, completionUserID)
	assistantTxID := appendedSessionItemTxID(t, records, completionAssistantID)
	if userTxID == "" || assistantTxID == "" {
		t.Fatalf("completion item tx ids = user %q assistant %q, want transaction ids", userTxID, assistantTxID)
	}
	if userTxID == assistantTxID {
		t.Fatalf("completion user item and assistant item were born in same transaction %q, want projector turn-input and assistant transactions", userTxID)
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
		done <- runInProcessRuntimeWithContext(ctx, []string{"--config", cliConfigPath(configDir), "chat", "--prompt", "delegate"}, stdinReader, stdout, stderr, func() (string, error) {
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
		done <- runInProcessRuntimeWithContext(ctx, []string{"--config", cliConfigPath(configDir), "chat", "--prompt", "delegate"}, stdinReader, stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithIO([]string{"--config", cliConfigPath(configDir), "chat", "--prompt", "hello"}, strings.NewReader("/exit\n"), &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "delegate"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Use discovered skills"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--prompt", "Use project instructions"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--save-session", "--quit", "--prompt", "Use deduped project instructions"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--save-session", "--prompt", "Use visible skills"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Use skills"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Use skills"}, &stdout, &stderr, func() (string, error) {
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
			code := runInProcessRuntimeWithGetwd(tt.args, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-mcp", "local", "--enable-tools", "list_files,mcp.local.search", "--prompt", "Use mixed tools"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-mcp", "local", "--enable-tools", "mcp.local.search", "--prompt", "Use MCP search"}, &stdout, &stderr, func() (string, error) {
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
		code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--verbose", "--enable-tools", "list_files,read_file", "--prompt", "user prompt secret"}, &stdout, &stderr, func() (string, error) {
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
		code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--verbose", "--prompt", "env user prompt secret"}, &stdout, &stderr, func() (string, error) {
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
		done <- runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--verbose", "--prompt", "hello"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "read_file", "--prompt", "Read note"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "read_file", "--prompt", "user prompt secret"}, &stdout, &stderr, func() (string, error) {
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
		code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", prompt}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--verbose", "--prompt", "hello"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-tools", "missing", "--prompt", "Use tools"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--enable-mcp", "missing", "--prompt", "Use MCP"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--model", "fast", "--prompt", "Use fast", "--model", "default"}, &stdout, &stderr, func() (string, error) {
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
	code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Hello"}, &stdout, &stderr, func() (string, error) {
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
		code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Think"}, &stdout, &stderr, func() (string, error) {
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
		code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--show-reasoning", "--prompt", "Think"}, &stdout, &stderr, func() (string, error) {
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
		code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Think"}, &stdout, &stderr, func() (string, error) {
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
		code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--show-reasoning=false", "--prompt", "Think"}, &stdout, &stderr, func() (string, error) {
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
		code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--show-reasoning", "--prompt", "Think"}, &stdout, &stderr, func() (string, error) {
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
		code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--show-reasoning", "--prompt", "Think"}, &stdout, &stderr, func() (string, error) {
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

func TestModelEventsFromBusForwardsOnlyModelEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	bus := eventbus.NewBus(func(eventbus.Event) error { return nil })
	sub := bus.SubscribeLossless(4)
	if err := bus.Publish(eventbus.TurnStarted{TurnID: "turn-1"}); err != nil {
		t.Fatalf("Publish(TurnStarted) error = %v", err)
	}
	if err := bus.Publish(eventbus.ModelEvent{Event: model.TextDeltaEvent{Text: "hello"}}); err != nil {
		t.Fatalf("Publish(ModelEvent) error = %v", err)
	}
	bus.Close()

	var got []model.Event
	for event := range modelEventsFromBus(ctx, sub) {
		got = append(got, event)
	}
	if len(got) != 1 {
		t.Fatalf("len(model events) = %d, want 1: %#v", len(got), got)
	}
	text, ok := got[0].(model.TextDeltaEvent)
	if !ok || text.Text != "hello" {
		t.Fatalf("model event = %#v, want text delta hello", got[0])
	}
}

func TestModelEventBusBridgeRendersAllEventsAndCloses(t *testing.T) {
	const eventCount = 96

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	bus := eventbus.NewBus(nil)
	defer bus.Close()
	source := make(chan model.Event)
	pumpDone := publishModelEventsToBus(ctx, bus, source)
	events := modelEventsFromBusUntil(ctx, bus.SubscribeLossless(1), pumpDone)
	go func() {
		defer close(source)
		for i := 0; i < eventCount; i++ {
			source <- model.TextDeltaEvent{Text: fmt.Sprintf("%02d,", i)}
		}
	}()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := writeStreamWithOptions(&stdout, &stderr, events, false, nil, streamOutputOptions{}); err != nil {
		t.Fatalf("writeStreamWithOptions() error = %v", err)
	}
	select {
	case <-pumpDone:
	case <-time.After(time.Second):
		t.Fatal("event pump did not stop after source close")
	}
	var want strings.Builder
	for i := 0; i < eventCount; i++ {
		fmt.Fprintf(&want, "%02d,", i)
	}
	if got := stdout.String(); got != want.String() {
		t.Fatalf("stdout = %q, want %q", got, want.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
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
		code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Hello"}, &stdout, &stderr, func() (string, error) {
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
		code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Hello"}, &stdout, &stderr, func() (string, error) {
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
		errorDetail, ok := errorRecord["error"].(string)
		if !ok || !strings.Contains(errorDetail, "400 Bad Request") {
			t.Fatalf("error log record = %#v, want HTTP status error detail", errorRecord)
		}
		for _, leaked := range []string{"direct-secret-value", "Bearer direct-secret-value"} {
			if strings.Contains(string(data), leaked) {
				t.Fatalf("error log leaked %q: %s", leaked, string(data))
			}
		}
		assertNoAdditionalCLIRunRequest(t, requests)
	})

	t.Run("invalid SSE chunk", func(t *testing.T) {
		server, requests := newCLIRunServer(t, `{not-json`)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Hello"}, &stdout, &stderr, func() (string, error) {
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
		code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--model", "missing", "--prompt", "Hello"}, &stdout, &stderr, func() (string, error) {
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
		code := runInProcessRuntimeWithGetwd([]string{"--config", cliConfigPath(configDir), "chat", "--quit", "--prompt", "Hello"}, &stdout, &stderr, func() (string, error) {
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

func setCLICompactionConfig(t *testing.T, configDir string, enabled bool, summaryProvider, summaryModel string) {
	t.Helper()

	setCLICompactionConfigWithThreshold(t, configDir, enabled, 80, summaryProvider, summaryModel)
}

func setCLICompactionConfigWithThreshold(t *testing.T, configDir string, enabled bool, thresholdPercent int, summaryProvider, summaryModel string) {
	t.Helper()

	configPath := filepath.Join(configDir, "sai.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", configPath, err)
	}
	updated := strings.TrimRight(string(data), "\r\n") + fmt.Sprintf(`

compaction:
  enabled: %t
  threshold_percent: %d
  summary_provider: %q
  summary_model: %q
`, enabled, thresholdPercent, summaryProvider, summaryModel)
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

func setCLIProviderBaseURL(t *testing.T, configDir, baseURL string) {
	t.Helper()

	configPath := filepath.Join(configDir, "providers", "fake.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", configPath, err)
	}
	lines := strings.Split(string(data), "\n")
	replaced := false
	for i, line := range lines {
		if strings.HasPrefix(line, "base_url: ") {
			lines[i] = "base_url: " + baseURL
			replaced = true
			break
		}
	}
	if !replaced {
		t.Fatalf("fake.yaml did not contain base_url to replace:\n%s", data)
	}
	writeCLIFile(t, configPath, strings.Join(lines, "\n"))
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

func loadOnlyCLISession(t *testing.T, root string) sessions.SessionV2 {
	t.Helper()

	store := sessions.NewV2Store(root)
	infos, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("len(sessions) = %d, want 1: %#v", len(infos), infos)
	}
	return loadCLISession(t, root, infos[0].ID)
}

func loadCLISession(t *testing.T, root, id string) sessions.SessionV2 {
	t.Helper()

	session, err := sessions.NewV2Store(root).Load(id)
	if err != nil {
		t.Fatalf("Load(%q) error = %v", id, err)
	}
	return session
}

func activeCLIMessages(t *testing.T, session sessions.SessionV2) []model.Message {
	t.Helper()

	messages, err := session.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory(%q) error = %v", session.ID, err)
	}
	return messages
}

func messagesContainRoleContent(messages []model.Message, role model.MessageRole, content string) bool {
	for _, message := range messages {
		if message.Role == role && strings.Contains(message.Content, content) {
			return true
		}
	}
	return false
}

func sessionItemIDWithRoleContent(t *testing.T, items []sessions.SessionItem, role model.MessageRole, content string) string {
	t.Helper()
	for _, item := range items {
		if item.Message == nil {
			continue
		}
		if item.Message.Role == role && strings.Contains(item.Message.Content, content) {
			return item.ID
		}
	}
	t.Fatalf("missing session item role %q containing %q in %#v", role, content, items)
	return ""
}

func sessionItemByID(t *testing.T, session sessions.SessionV2, id string) sessions.SessionItem {
	t.Helper()

	for _, item := range session.Items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("session item %q not found in %#v", id, session.Items)
	return sessions.SessionItem{}
}

func sessionContainsMessageContent(session sessions.SessionV2, content string) bool {
	for _, item := range session.Items {
		if item.Message != nil && strings.Contains(item.Message.Content, content) {
			return true
		}
	}
	return false
}

func countSessionItemsWithExactContent(session sessions.SessionV2, content string) int {
	count := 0
	for _, item := range session.Items {
		if item.Message != nil && item.Message.Content == content {
			count++
		}
	}
	return count
}

func sessionContainsExactMessageContent(session sessions.SessionV2, content string) bool {
	for _, item := range session.Items {
		if item.Message != nil && item.Message.Content == content {
			return true
		}
	}
	return false
}

func sessionItemWithExactContent(t *testing.T, session sessions.SessionV2, content string) sessions.SessionItem {
	t.Helper()

	for _, item := range session.Items {
		if item.Message != nil && item.Message.Content == content {
			return item
		}
	}
	t.Fatalf("session item with content %q not found in %#v", content, session.Items)
	return sessions.SessionItem{}
}

func writeCLISession(t *testing.T, root string, session sessions.Session) {
	t.Helper()

	v2 := sessions.SessionV2{
		ID:                   session.ID,
		CreatedAt:            session.CreatedAt,
		UpdatedAt:            session.UpdatedAt,
		Version:              sessions.VersionV2,
		Provider:             session.Provider,
		ModelProfile:         session.ModelProfile,
		ModelID:              session.ModelID,
		ModelParameters:      session.ModelParameters,
		CWD:                  session.CWD,
		ConfigPath:           session.ConfigPath,
		ConfigDir:            session.ConfigDir,
		EnabledTools:         session.EnabledTools,
		EnabledMCP:           session.EnabledMCP,
		EnabledSkills:        session.EnabledSkills,
		ShowReasoning:        session.ShowReasoning,
		InstructionsSnapshot: session.InstructionsSnapshot,
		InstructionSources:   session.InstructionSources,
		Context:              session.Context,
		SaveToolResults:      session.SaveToolResults,
	}
	for _, message := range session.Messages {
		id := sessions.NextSessionItemID(sessions.SessionItemIDs(v2.Items), message)
		v2.Items = append(v2.Items, sessions.SessionItemFromMessage(id, message))
		v2.ActiveHistory = append(v2.ActiveHistory, id)
	}
	writeCLISessionV2(t, root, v2)
}

func writeCLISessionV2(t *testing.T, root string, session sessions.SessionV2) {
	t.Helper()

	if session.Version == 0 || session.Version == sessions.CurrentVersion {
		session.Version = sessions.VersionV2
	}
	if err := os.MkdirAll(filepath.Join(root, session.ID, "segments"), 0o755); err != nil {
		t.Fatalf("MkdirAll(session %q) error = %v", session.ID, err)
	}
	metadata := session
	metadata.Items = nil
	metadata.ActiveHistory = nil
	metadata.Compactions = nil
	metadata.LastSeq = 0
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent(session %q) error = %v", session.ID, err)
	}
	data = append(data, '\n')
	path := filepath.Join(root, session.ID, "meta.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}

	segmentPath := filepath.Join(root, session.ID, "segments", "000001.jsonl")
	segment := &strings.Builder{}
	seq := int64(0)
	for _, item := range session.Items {
		seq++
		item.Seq = seq
		if item.CreatedAt.IsZero() {
			item.CreatedAt = session.CreatedAt
			if item.CreatedAt.IsZero() {
				item.CreatedAt = session.UpdatedAt
			}
		}
		writeCLISessionV2Record(t, segment, map[string]any{
			"seq":  seq,
			"type": sessions.RecordTypeItemAppended,
			"item": item,
		})
	}
	if session.ActiveHistory != nil {
		seq++
		writeCLISessionV2Record(t, segment, map[string]any{
			"seq":      seq,
			"type":     sessions.RecordTypeActiveHistoryReplaced,
			"item_ids": session.ActiveHistory,
		})
	}
	if segment.Len() > 0 {
		if err := os.WriteFile(segmentPath, []byte(segment.String()), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", segmentPath, err)
		}
	}
}

func writeCLISessionV2Record(t *testing.T, out *strings.Builder, record map[string]any) {
	t.Helper()

	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("Marshal(session record) error = %v", err)
	}
	out.Write(data)
	out.WriteByte('\n')
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

type concurrentCLIRunResult struct {
	stdout string
	stderr string
	code   int
}

func receiveConcurrentCLIRunResult(t *testing.T, results <-chan concurrentCLIRunResult) concurrentCLIRunResult {
	t.Helper()

	select {
	case result := <-results:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for concurrent CLI run")
	}
	return concurrentCLIRunResult{}
}

func twoCallerGetwd(t *testing.T, dir string) func() (string, error) {
	t.Helper()

	var mu sync.Mutex
	calls := 0
	bothCalled := make(chan struct{})
	var closeBoth sync.Once
	return func() (string, error) {
		mu.Lock()
		calls++
		if calls >= 2 {
			closeBoth.Do(func() { close(bothCalled) })
		}
		mu.Unlock()

		select {
		case <-bothCalled:
			return dir, nil
		case <-time.After(2 * time.Second):
			return "", fmt.Errorf("timed out waiting for concurrent getwd")
		}
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for file %s", path)
}

func assertCLIStringSliceContains(t *testing.T, values []string, want string) {
	t.Helper()

	for _, value := range values {
		if value == want {
			return
		}
	}
	t.Fatalf("values = %#v, want contain %q", values, want)
}

func assertCLIStringSliceOmits(t *testing.T, values []string, omitted ...string) {
	t.Helper()

	for _, value := range values {
		for _, omit := range omitted {
			if value == omit {
				t.Fatalf("values = %#v, want omit %q", values, omit)
			}
		}
	}
}

func assertCLIFlagValue(t *testing.T, args []string, flagName, want string) {
	t.Helper()

	for i := 0; i < len(args)-1; i++ {
		if args[i] == flagName {
			if got := args[i+1]; got != want {
				t.Fatalf("%s value = %q in %#v, want %q", flagName, got, args, want)
			}
			return
		}
	}
	t.Fatalf("args = %#v, want %s %q", args, flagName, want)
}

func mustCLICanonicalPath(t *testing.T, path string) string {
	t.Helper()

	canonical, err := canonicalPath(path)
	if err != nil {
		t.Fatalf("CanonicalPath(%q) error = %v", path, err)
	}
	return canonical
}

func isolateCLIUserRegistry(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg-config"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	path, err := resolveStorageRoot("sai", "")
	if err != nil {
		t.Fatalf("resolveStorageRoot() error = %v", err)
	}
	return path
}

func cliDefaultServerRoot(t *testing.T) string {
	t.Helper()

	root, err := resolveStorageRoot("sai", "")
	if err != nil {
		t.Fatalf("resolveStorageRoot() error = %v", err)
	}
	return root
}

func cliProjectStore(t *testing.T, home string) *projectstore.Store {
	t.Helper()

	root, err := projectstore.RootForHome(home)
	if err != nil {
		t.Fatalf("Project RootForHome(%q) error = %v", home, err)
	}
	return projectstore.NewStore(root)
}

func cliSessionStore(t *testing.T, home string) *sessions.V2Store {
	t.Helper()

	root, err := sessions.RootForHome(home)
	if err != nil {
		t.Fatalf("Session RootForHome(%q) error = %v", home, err)
	}
	return sessions.NewV2Store(root)
}

func saveCLIExecutionSession(t *testing.T, home, id, projectDir, configDir string) sessions.SessionV2 {
	t.Helper()

	session := sessions.SessionV2{
		ID:              id,
		CreatedCWD:      projectDir,
		CWD:             projectDir,
		ConfigPath:      filepath.Join(configDir, "sai.yaml"),
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		SaveToolResults: true,
	}
	saved, err := cliSessionStore(t, home).SaveMetadata(session)
	if err != nil {
		t.Fatalf("SaveMetadata(%s) error = %v", id, err)
	}
	return saved
}

func seedCompleteVisibleTurns(t *testing.T, store *sessions.V2Store, sessionID string) {
	t.Helper()

	items := []sessions.SessionItem{
		sessions.SessionItemFromMessage("seed-user-1", model.Message{Role: model.MessageRoleUser, Content: "first"}),
		sessions.SessionItemFromMessage("seed-assistant-1", model.Message{Role: model.MessageRoleAssistant, Content: "one"}),
		sessions.SessionItemFromMessage("seed-user-2", model.Message{Role: model.MessageRoleUser, Content: "second"}),
		sessions.SessionItemFromMessage("seed-assistant-2", model.Message{Role: model.MessageRoleAssistant, Content: "two"}),
		sessions.SessionItemFromMessage("seed-user-3", model.Message{Role: model.MessageRoleUser, Content: "third"}),
		sessions.SessionItemFromMessage("seed-assistant-3", model.Message{Role: model.MessageRoleAssistant, Content: "three"}),
	}
	activeHistory := make([]string, 0, len(items))
	for _, item := range items {
		activeHistory = append(activeHistory, item.ID)
	}
	if _, err := store.AppendItemsAndReplaceActiveHistory(sessionID, items, activeHistory); err != nil {
		t.Fatalf("AppendItemsAndReplaceActiveHistory(%s) error = %v", sessionID, err)
	}
}

func seedCompleteVisibleTurnsSnapshot() string {
	return "user: first\nassistant: one\nuser: second\nassistant: two\nuser: third\nassistant: three\n"
}

func forbidCLIBackgroundStart(t *testing.T) {
	t.Helper()
}

func waitForCLIStreamReady(t *testing.T, ready <-chan struct{}) {
	t.Helper()

	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for attach stream")
	}
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

func writeCLIJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeCLIEmptySessionItems(t *testing.T, w http.ResponseWriter, r *http.Request, authSeen chan<- string) {
	t.Helper()

	if r.Method != http.MethodGet {
		t.Fatalf("session items method = %s, want GET", r.Method)
	}
	if got := r.URL.Query().Get("view"); got != "chat" {
		t.Fatalf("session items view = %q, want chat", got)
	}
	if got := r.URL.Query().Get("limit"); got != "50" {
		t.Fatalf("session items limit = %q, want 50", got)
	}
	if authSeen != nil {
		authSeen <- r.Header.Get("Authorization")
	}
	writeCLIJSON(w, http.StatusOK, map[string]any{
		"items":           []any{},
		"oldest_seq":      0,
		"newest_seq":      0,
		"has_more_before": false,
		"has_more_after":  false,
	})
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

func readCLISessionJSONLRecords(t *testing.T, root, sessionID string) []map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, sessionID, "segments", "*.jsonl"))
	if err != nil {
		t.Fatalf("Glob(session segments) error = %v", err)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		t.Fatalf("session %q has no segment records under %s", sessionID, root)
	}
	records := make([]map[string]any, 0)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", path, err)
		}
		records = append(records, readJSONLRecords(t, data)...)
	}
	return records
}

func appendedSessionItemTxID(t *testing.T, records []map[string]any, itemID string) string {
	t.Helper()
	for _, record := range records {
		if record["type"] != sessions.RecordTypeItemAppended {
			continue
		}
		item, ok := record["item"].(map[string]any)
		if !ok {
			t.Fatalf("item.appended record item = %T, want object in %#v", record["item"], record)
		}
		if item["id"] != itemID {
			continue
		}
		txID, _ := record["tx_id"].(string)
		return txID
	}
	t.Fatalf("missing item.appended record for item %q in %#v", itemID, records)
	return ""
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

func countCLIRequestMessagesWithContent(t *testing.T, messages []any, role, content string) int {
	t.Helper()
	count := 0
	for i, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("message[%d] = %T, want object", i, raw)
		}
		if message["role"] == role && message["content"] == content {
			count++
		}
	}
	return count
}

func toolMessagesByCallID(messages []any) map[string]string {
	byCallID := make(map[string]string)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || message["role"] != "tool" {
			continue
		}
		toolCallID, _ := message["tool_call_id"].(string)
		byCallID[toolCallID] = fmt.Sprint(message["content"])
	}
	return byCallID
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

func assertMessageContentContains(t *testing.T, messages []any, index int, role, content string) {
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
	got, ok := message["content"].(string)
	if !ok {
		t.Fatalf("message[%d].content = %T, want string", index, message["content"])
	}
	if !strings.Contains(got, content) {
		t.Fatalf("message[%d].content = %q, want to contain %q", index, got, content)
	}
}

func requestMessagesContainExactContent(messages []any, content string) bool {
	for _, message := range messages {
		item, ok := message.(map[string]any)
		if !ok {
			continue
		}
		if item["content"] == content {
			return true
		}
	}
	return false
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

func assertSavedMessageContentContains(t *testing.T, messages []model.Message, index int, role model.MessageRole, content string) {
	t.Helper()

	if index >= len(messages) {
		t.Fatalf("missing saved message %d in %#v", index, messages)
	}
	message := messages[index]
	if message.Role != role {
		t.Fatalf("saved message[%d].Role = %q, want %q", index, message.Role, role)
	}
	if !strings.Contains(message.Content, content) {
		t.Fatalf("saved message[%d].Content = %q, want to contain %q", index, message.Content, content)
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

func assertCLIHelpWithoutConfig(t *testing.T, args []string, wants ...string) string {
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
	return out
}

func assertCLIOutputContains(t *testing.T, got string, wants ...string) {
	t.Helper()

	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want contain %q", got, want)
		}
	}
}

func valueFromCLIKeyValueOutput(t *testing.T, got, key string) string {
	t.Helper()

	prefix := key + "\t"
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), prefix); ok {
			return value
		}
	}
	t.Fatalf("output = %q, want key %q", got, key)
	return ""
}

func assertCLILastSeqPresent(t *testing.T, got string) {
	t.Helper()

	value := valueFromCLIKeyValueOutput(t, got, "LAST_SEQ")
	if strings.TrimSpace(value) == "" || value == "0" {
		t.Fatalf("output = %q, want non-zero LAST_SEQ", got)
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

func runInProcessRuntimeWithIO(args []string, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error)) int {
	return runInProcessRuntimeWithContext(context.Background(), args, stdin, stdout, stderr, getwd)
}

func runInProcessRuntimeWithGetwd(args []string, stdout, stderr io.Writer, getwd func() (string, error)) int {
	return runInProcessRuntimeWithContext(context.Background(), args, strings.NewReader(""), stdout, stderr, getwd)
}

func runInProcessRuntimeWithContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error)) int {
	return runInProcessRuntimeWithInterrupts(ctx, "sai", args, stdin, stdout, stderr, getwd, nil)
}

const inProcessRuntimeUsageText = `usage: sai chat [--provider name] [--model profile] [--prompt text | --stdin | --file path] [--show-reasoning] [--verbose] [--enable-tools names] [--enable-mcp ids] [--save-session] [--resume id | --continue] [--quit]

Test-only in-process runtime harness.
`

func printInProcessRuntimeUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(inProcessRuntimeUsageText, command))
}

func runInProcessRuntimeWithInterrupts(ctx context.Context, program string, args []string, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error), interrupts <-chan struct{}) int {
	rootArgs, err := splitRootArgs(args)
	if err == nil && rootArgs.command != "chat" {
		err = usageError(fmt.Sprintf("in-process runtime test helper requires chat command, got %q", rootArgs.command), "", "sai help")
	}
	if err == nil {
		err = runInProcessRuntimeForTest(ctx, rootArgs.commandArgs, rootArgs.configPath, stdin, stdout, stderr, getwd, program, interrupts)
	}
	if err != nil {
		if errors.Is(err, errSilentExit) {
			return 1
		}
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	return 0
}

func runInProcessRuntimeForTest(ctx context.Context, args []string, configPath string, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error), program string, interrupts <-chan struct{}) (runtimeErr error) {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai chat", flag.ContinueOnError)
	var options agentCommandFlags
	registerAgentCommandFlags(flags, &options)
	quit := flags.Bool("quit", false, "exit after the initial prompt turn")
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printInProcessRuntimeUsage, displayCommand, "sai help chat")
	if done || err != nil {
		return err
	}
	flags.Visit(func(flag *flag.Flag) {
		switch flag.Name {
		case "show-reasoning":
			options.showReasoningSet = true
		case "save-session":
			options.saveSessionSet = true
		}
	})
	if err := options.validate("sai help chat"); err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("unexpected positional argument; use --prompt for the initial prompt", inProcessRuntimeUsageText, "sai help chat")
	}
	initialSources := testRuntimeInitialInputSourceCount(options)
	if initialSources > 1 {
		return usageError("--prompt, --stdin, and --file are mutually exclusive", inProcessRuntimeUsageText, "sai help chat")
	}
	if *quit && initialSources == 0 {
		return usageError("--quit requires --prompt, --stdin, or --file", inProcessRuntimeUsageText, "sai help chat")
	}
	if !*quit && options.stdin {
		return usageError("--stdin requires --quit", inProcessRuntimeUsageText, "sai help chat")
	}
	if !*quit && options.file.set {
		return usageError("--file requires --quit", inProcessRuntimeUsageText, "sai help chat")
	}

	runtime, err := prepareAgentRuntimeWithOptions(ctx, configPath, options, stderr, getwd, program, runtimePreparationOptions{
		enableSubagents: true,
	})
	if err != nil {
		return err
	}
	defer func() {
		runtimeErr = errors.Join(runtimeErr, runtime.Close())
	}()
	if err := runtime.writeSessionSaveNotice(stderr); err != nil {
		return err
	}

	messages := runtime.initialMessages()
	initialPrompt, hasInitialPrompt, err := readInitialPromptForTest(options, stdin)
	if err != nil {
		return err
	}
	if hasInitialPrompt {
		updated, err := runChatTurnAndCompletions(ctx, runtime, messages, initialPrompt, stdout, stderr, !*quit, false, interrupts)
		if err != nil {
			if *quit {
				return err
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if !isRecoverableTurnError(err) {
				return err
			}
			if _, printErr := fmt.Fprintf(stderr, "%s: %v\n", displayCommand, err); printErr != nil {
				return printErr
			}
		} else {
			messages = updated
		}
		if *quit {
			messages, err = runCompletionTurnsWithOptionalWait(ctx, runtime, messages, stdout, stderr, false, false, subagentCompletionExitWait, interrupts)
			if err != nil {
				return err
			}
			return nil
		}
	}

	scanner := bufio.NewScanner(stdin)
	var inputCh <-chan chatInputEvent
	for {
		if inputCh == nil {
			inputCh = startChatInputRead(ctx, scanner, stderr)
		}

		select {
		case input := <-inputCh:
			inputCh = nil
			if input.err != nil {
				return input.err
			}
			if !input.ok {
				return nil
			}

			command := strings.TrimSpace(input.line)
			if command == "" {
				continue
			}
			if !input.multiline && (command == "/exit" || command == "/quit") {
				return nil
			}
			if !input.multiline && command == "/usage" {
				if err := runtime.writeUsageSummary(stderr); err != nil {
					return err
				}
				continue
			}
			if !input.multiline && command == "/compact" {
				updated, err := runtime.compactSession(ctx, stderr)
				if err != nil {
					if _, printErr := fmt.Fprintf(stderr, "%s: compact failed: %v\n", displayCommand, err); printErr != nil {
						return printErr
					}
					continue
				}
				messages = updated
				continue
			}

			updated, err := runChatTurnAndCompletions(ctx, runtime, messages, input.line, stdout, stderr, true, true, interrupts)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				if !isRecoverableTurnError(err) {
					return err
				}
				if _, printErr := fmt.Fprintf(stderr, "%s: %v\n", displayCommand, err); printErr != nil {
					return printErr
				}
				continue
			}
			messages = updated
		case <-interrupts:
			return context.Canceled
		case <-runtime.subagentCompletionSignal():
			redrawPrompt := inputCh != nil
			if redrawPrompt {
				if _, err := fmt.Fprint(stderr, "\n"); err != nil {
					return err
				}
			}
			updated, err := runAvailableCompletionTurns(ctx, runtime, messages, stdout, stderr, true, true, interrupts)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				if !isRecoverableTurnError(err) {
					return err
				}
				if _, printErr := fmt.Fprintf(stderr, "%s: %v\n", displayCommand, err); printErr != nil {
					return printErr
				}
				if redrawPrompt {
					if _, printErr := fmt.Fprint(stderr, chatInputPrompt); printErr != nil {
						return printErr
					}
				}
				continue
			}
			messages = updated
			if redrawPrompt {
				if _, err := fmt.Fprint(stderr, chatInputPrompt); err != nil {
					return err
				}
			}
		}
	}
}

func testRuntimeInitialInputSourceCount(options agentCommandFlags) int {
	count := 0
	if options.prompt.set {
		count++
	}
	if options.stdin {
		count++
	}
	if options.file.set {
		count++
	}
	return count
}

func readInitialPromptForTest(options agentCommandFlags, stdin io.Reader) (string, bool, error) {
	switch {
	case options.prompt.set:
		return options.prompt.text, true, nil
	case options.stdin:
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", false, fmt.Errorf("read stdin prompt: %w", err)
		}
		return string(data), true, nil
	case options.file.set:
		data, err := os.ReadFile(options.file.path)
		if err != nil {
			return "", false, fmt.Errorf("read prompt file %q: %w", options.file.path, err)
		}
		return string(data), true, nil
	default:
		return "", false, nil
	}
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
