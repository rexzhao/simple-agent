package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelsListUsesGlobalConfigDirFlag(t *testing.T) {
	dir := writeCLIFixture(t)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", dir, "models", "list"}, &stdout, &stderr, func() (string, error) {
		return "unused", nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"PROVIDER\tPROFILE\tMODEL ID",
		"paperhub\tglm-5.2\tglm-5.2",
		"paperhub\tglm-5.2-fast\tglm-5.2",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("models list output missing %q:\n%s", want, out)
		}
	}
}

func TestModelsListDefaultsConfigDirToAgentsUnderCurrentWorkingDirectory(t *testing.T) {
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

func TestConfigShowDoesNotPrintAPIKeyValue(t *testing.T) {
	dir := writeCLIFixture(t)
	t.Setenv("PAPERHUB_API_KEY", "env-secret-value")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", dir, "config", "show"}, &stdout, &stderr, func() (string, error) {
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
	code := RunWithGetwd([]string{"--config-dir", configDir, "run", "Say hi"}, &stdout, &stderr, func() (string, error) {
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
	code := RunWithGetwd([]string{"--config-dir", configDir, "run", "--provider", "fake", "--model", "fast", "Use fast"}, &stdout, &stderr, func() (string, error) {
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

func TestRunRejectsPostPromptModelFlagInsteadOfSwitching(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"run", "--model", "fast", "Use fast", "--model", "default"}, &stdout, &stderr, func() (string, error) {
		return "unused", nil
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `usage: sai run`)
}

func TestRunIncludesStartupAgentsAndConfigDirDoesNotChangeLookup(t *testing.T) {
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
	code := RunWithGetwd([]string{"--config-dir", configDir, "run", "Hello"}, &stdout, &stderr, func() (string, error) {
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
		code := RunWithGetwd([]string{"--config-dir", configDir, "run", "Think"}, &stdout, &stderr, func() (string, error) {
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
		code := RunWithGetwd([]string{"--config-dir", configDir, "run", "--show-reasoning", "Think"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "shown\nvisible"; got != want {
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
		code := RunWithGetwd([]string{"--config-dir", configDir, "run", "--show-reasoning", "Think"}, &stdout, &stderr, func() (string, error) {
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
		code := RunWithGetwd([]string{"--config-dir", configDir, "run", "--show-reasoning", "Think"}, &stdout, &stderr, func() (string, error) {
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

func TestRunErrorsDoNotLeakAPIKeyValues(t *testing.T) {
	t.Run("missing API key", func(t *testing.T) {
		server, _ := newCLIRunServer(t, `[DONE]`)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "$SAI_MISSING_API_KEY", "openai-chat")
		t.Setenv("SAI_MISSING_API_KEY", "")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config-dir", configDir, "run", "Hello"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		assertCLIErrorContains(t, stderr.String(), `API key environment variable "SAI_MISSING_API_KEY" is not set`)
	})

	t.Run("unknown model", func(t *testing.T) {
		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config-dir", configDir, "run", "--model", "missing", "Hello"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		assertCLIErrorContains(t, stderr.String(), `unknown model "missing" for provider "fake"`)
		assertCLIErrorOmits(t, stderr.String(), "direct-secret-value")
	})

	t.Run("unsupported provider type", func(t *testing.T) {
		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "not-openai")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config-dir", configDir, "run", "Hello"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		assertCLIErrorContains(t, stderr.String(), `unsupported provider type "not-openai"`)
		assertCLIErrorOmits(t, stderr.String(), "direct-secret-value")
	})
}

func writeCLIFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	writeCLIFixtureInDir(t, dir)
	return dir
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
type: openai-chat
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
type: openai-chat
base_url: http://localhost:8080/v1
api_key: direct-secret-value

models:
  small:
    id: local-small
`)
}

func writeCLIFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

type capturedCLIRunRequest struct {
	Path          string
	Authorization string
	RawBody       []byte
	Body          map[string]any
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
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}

		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
	}))
	return server, requests
}

func writeCLIRunFixtureInDir(t *testing.T, dir, baseURL, apiKey, providerType string) {
	t.Helper()

	providersDir := filepath.Join(dir, "providers")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	writeCLIFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: fake
default_model: default
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

	writeCLIFile(t, filepath.Join(providersDir, "fake.yaml"), fmt.Sprintf(`name: fake
type: %s
base_url: %s
api_key: %s

models:
  default:
    id: model-default
    temperature: 0.6
    max_tokens: 128
  fast:
    id: model-fast
    temperature: 0.2
    max_tokens: 64
`, providerType, baseURL, apiKey))
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

func requestMessages(t *testing.T, body map[string]any) []any {
	t.Helper()

	messages, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("messages = %T, want []any", body["messages"])
	}
	return messages
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

func assertCLIErrorOmits(t *testing.T, got string, values ...string) {
	t.Helper()

	for _, value := range values {
		if strings.Contains(got, value) {
			t.Fatalf("stderr leaked %q: %s", value, got)
		}
	}
}

func assertNoAdditionalCLIRunRequest(t *testing.T, requests <-chan capturedCLIRunRequest) {
	t.Helper()

	select {
	case request := <-requests:
		t.Fatalf("unexpected additional model request: path=%s body=%s", request.Path, request.RawBody)
	default:
	}
}
