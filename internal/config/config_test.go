package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadResolvesConfigAndProviderModels(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	wantConfigPath, err := filepath.Abs(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("filepath.Abs() error = %v", err)
	}
	wantConfigPath = filepath.Clean(wantConfigPath)
	if cfg.ConfigPath != wantConfigPath {
		t.Fatalf("ConfigPath = %q, want %q", cfg.ConfigPath, wantConfigPath)
	}
	wantConfigDir := filepath.Dir(wantConfigPath)

	wantProviderDir := filepath.Join(wantConfigDir, "providers")
	if cfg.ProviderDir != wantProviderDir {
		t.Fatalf("ProviderDir = %q, want %q", cfg.ProviderDir, wantProviderDir)
	}

	provider := cfg.Providers["paperhub"]
	if provider.Name != "paperhub" {
		t.Fatalf("provider name = %q, want paperhub", provider.Name)
	}
	if provider.APIKey != "$PAPERHUB_API_KEY" {
		t.Fatalf("APIKey = %q, want $PAPERHUB_API_KEY", provider.APIKey)
	}
	if provider.HTTPProxy != "http://127.0.0.1:7890" {
		t.Fatalf("HTTPProxy = %q, want http://127.0.0.1:7890", provider.HTTPProxy)
	}
	if provider.HTTPSProxy != "https://proxy.example.test:8443" {
		t.Fatalf("HTTPSProxy = %q, want https://proxy.example.test:8443", provider.HTTPSProxy)
	}
	if got := provider.Models["glm-5.2-fast"].ID; got != "glm-5.2" {
		t.Fatalf("fast profile id = %q, want glm-5.2", got)
	}
	if got := provider.Models["glm-5.2-fast"].Parameters["max_tokens"]; got != 2048 {
		t.Fatalf("fast profile max_tokens = %#v, want 2048", got)
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode(%q) = %04o, want %04o", path, got, want)
	}
}

func TestLoadBaseUsesAgentPromptAndRelativeSkillSchema(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "profile")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	configPath := filepath.Join(configDir, "custom.yaml")
	writeFile(t, configPath, `default_provider: custom-provider
default_model: custom-model
agent:
  max_iterations: 6
prompt:
  system_prompt: |
    Review only the assigned scope.
`)

	cfg, err := LoadBase(configPath)
	if err != nil {
		t.Fatalf("LoadBase() error = %v", err)
	}

	if cfg.Agent.MaxIterations != 6 {
		t.Fatalf("Agent.MaxIterations = %d, want 6", cfg.Agent.MaxIterations)
	}
	if cfg.Prompt.SystemPrompt != "Review only the assigned scope.\n" {
		t.Fatalf("Prompt.SystemPrompt = %q, want child prompt", cfg.Prompt.SystemPrompt)
	}
}

func TestLoadUsesExplicitRootConfigFilePath(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "config-root")
	providersDir := filepath.Join(rootDir, "provider-files")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	configPath := filepath.Join(rootDir, "custom-root.yaml")
	writeFile(t, configPath, `default_provider: fake
default_model: default
provider_dir: provider-files

`)
	writeFile(t, filepath.Join(providersDir, "fake.yaml"), `name: fake
base_url: http://127.0.0.1:1
api_key: direct-secret

models:
  default:
    id: model-default
`)

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	wantConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		t.Fatalf("filepath.Abs() error = %v", err)
	}
	wantConfigPath = filepath.Clean(wantConfigPath)
	if cfg.ConfigPath != wantConfigPath {
		t.Fatalf("ConfigPath = %q, want %q", cfg.ConfigPath, wantConfigPath)
	}
	if cfg.ProviderDir != filepath.Join(rootDir, "provider-files") {
		t.Fatalf("ProviderDir = %q", cfg.ProviderDir)
	}
	if _, ok := cfg.Providers["fake"]; !ok {
		t.Fatalf("Providers = %#v, want fake", cfg.Providers)
	}
}

func TestLoadRecognizesAnthropicMessagesProvider(t *testing.T) {
	dir := t.TempDir()
	providersDir := filepath.Join(dir, "providers")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: anthropic
default_model: claude-sonnet-5
provider_dir: providers
`)
	writeFile(t, filepath.Join(providersDir, "anthropic.yaml"), `name: anthropic
base_url: https://api.anthropic.com/v1
api_key: $ANTHROPIC_API_KEY

models:
  claude-sonnet-5:
    id: claude-sonnet-5
    type: anthropic-messages
    max_tokens: 4096
  claude-haiku-4-5:
    id: claude-haiku-4-5
    type: anthropic-messages
    max_tokens: 2048
`)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	profile := cfg.Providers["anthropic"].Models["claude-sonnet-5"]
	if profile.Type != ProviderTypeAnthropicMessages {
		t.Fatalf("profile.Type = %q, want %q", profile.Type, ProviderTypeAnthropicMessages)
	}

	gotModels := cfg.ModelList()
	wantModels := []ModelInfo{
		{Provider: "anthropic", Profile: "claude-haiku-4-5", ID: "claude-haiku-4-5"},
		{Provider: "anthropic", Profile: "claude-sonnet-5", ID: "claude-sonnet-5"},
	}
	if len(gotModels) != len(wantModels) {
		t.Fatalf("len(ModelList()) = %d, want %d: %#v", len(gotModels), len(wantModels), gotModels)
	}
	for i := range wantModels {
		if gotModels[i] != wantModels[i] {
			t.Fatalf("ModelList()[%d] = %#v, want %#v", i, gotModels[i], wantModels[i])
		}
	}

	t.Setenv("ANTHROPIC_API_KEY", "resolved-anthropic-secret")
	resolved, err := cfg.ResolveModel("", "")
	if err != nil {
		t.Fatalf("ResolveModel() error = %v", err)
	}
	if resolved.Type != ProviderTypeAnthropicMessages {
		t.Fatalf("resolved.Type = %q, want %q", resolved.Type, ProviderTypeAnthropicMessages)
	}
	if resolved.ModelID != "claude-sonnet-5" {
		t.Fatalf("resolved.ModelID = %q, want claude-sonnet-5", resolved.ModelID)
	}
	if resolved.Provider.ResolvedAPIKey != "resolved-anthropic-secret" {
		t.Fatalf("resolved API key = %q, want resolved Anthropic API key", resolved.Provider.ResolvedAPIKey)
	}
	if got := resolved.Parameters["max_tokens"]; got != 4096 {
		t.Fatalf("max_tokens = %#v, want 4096", got)
	}
}

func TestLoadRecognizesOpenAIResponsesProvider(t *testing.T) {
	dir := t.TempDir()
	providersDir := filepath.Join(dir, "providers")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: openai
default_model: default
provider_dir: providers
`)
	writeFile(t, filepath.Join(providersDir, "openai.yaml"), `name: openai
base_url: https://api.openai.com/v1
api_key: $OPENAI_API_KEY

models:
  default:
    id: gpt-5.1
    type: openai-responses
    temperature: 0.2
`)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	profile := cfg.Providers["openai"].Models["default"]
	if profile.Type != ProviderTypeOpenAIResponses {
		t.Fatalf("profile.Type = %q, want %q", profile.Type, ProviderTypeOpenAIResponses)
	}

	gotModels := cfg.ModelList()
	wantModels := []ModelInfo{
		{Provider: "openai", Profile: "default", ID: "gpt-5.1"},
	}
	if len(gotModels) != len(wantModels) {
		t.Fatalf("len(ModelList()) = %d, want %d: %#v", len(gotModels), len(wantModels), gotModels)
	}
	for i := range wantModels {
		if gotModels[i] != wantModels[i] {
			t.Fatalf("ModelList()[%d] = %#v, want %#v", i, gotModels[i], wantModels[i])
		}
	}

	t.Setenv("OPENAI_API_KEY", "resolved-openai-secret")
	resolved, err := cfg.ResolveModel("", "")
	if err != nil {
		t.Fatalf("ResolveModel() error = %v", err)
	}
	if resolved.Type != ProviderTypeOpenAIResponses {
		t.Fatalf("resolved.Type = %q, want %q", resolved.Type, ProviderTypeOpenAIResponses)
	}
	if resolved.ModelID != "gpt-5.1" {
		t.Fatalf("resolved.ModelID = %q, want gpt-5.1", resolved.ModelID)
	}
	if resolved.Provider.ResolvedAPIKey != "resolved-openai-secret" {
		t.Fatalf("resolved API key = %q, want resolved OpenAI API key", resolved.Provider.ResolvedAPIKey)
	}
	if got := resolved.Parameters["temperature"]; got != 0.2 {
		t.Fatalf("temperature = %#v, want 0.2", got)
	}
}

func TestLoadRecognizesOpenAICodexProvider(t *testing.T) {
	dir := t.TempDir()
	providersDir := filepath.Join(dir, "providers")
	authDir := filepath.Join(dir, "auth")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(providers) error = %v", err)
	}
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(auth) error = %v", err)
	}

	writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: codex-work
default_model: gpt-5.5
provider_dir: providers
auth_dir: auth
`)
	writeFile(t, filepath.Join(providersDir, "codex-work.yaml"), `name: codex-work
base_url: https://chatgpt.com/backend-api/codex
auth_file: ../auth/codex-work.json

models:
  gpt-5.5:
    id: gpt-5.5
    type: openai-codex
    context_window: 400000
`)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	provider := cfg.Providers["codex-work"]
	if provider.AuthFile != filepath.Join(authDir, "codex-work.json") {
		t.Fatalf("AuthFile = %q, want auth file under auth dir", provider.AuthFile)
	}

	profile := provider.Models["gpt-5.5"]
	if profile.Type != ProviderTypeOpenAICodex {
		t.Fatalf("profile.Type = %q, want %q", profile.Type, ProviderTypeOpenAICodex)
	}
	resolved, err := cfg.ResolveModel("", "")
	if err != nil {
		t.Fatalf("ResolveModel() error = %v", err)
	}
	if resolved.Type != ProviderTypeOpenAICodex {
		t.Fatalf("resolved.Type = %q, want %q", resolved.Type, ProviderTypeOpenAICodex)
	}
	if resolved.Provider.ResolvedAPIKey != "" {
		t.Fatalf("ResolvedAPIKey = %q, want empty for Codex auth provider", resolved.Provider.ResolvedAPIKey)
	}
	if resolved.Provider.AuthFile != filepath.Join(authDir, "codex-work.json") {
		t.Fatalf("resolved AuthFile = %q, want auth file under auth dir", resolved.Provider.AuthFile)
	}
}

func TestNormalizeDeveloperRole(t *testing.T) {
	for _, test := range []struct {
		value any
		want  string
		ok    bool
	}{
		{value: nil, want: "", ok: true},
		{value: "", want: "", ok: true},
		{value: " SYSTEM ", want: "system", ok: true},
		{value: "developer", want: "developer", ok: true},
		{value: "user"},
		{value: 1},
	} {
		got, err := NormalizeDeveloperRole(test.value)
		if (err == nil) != test.ok {
			t.Errorf("NormalizeDeveloperRole(%#v) error = %v, want ok=%t", test.value, err, test.ok)
		}
		if err == nil && got != test.want {
			t.Errorf("NormalizeDeveloperRole(%#v) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestNormalizeModelCompatibility(t *testing.T) {
	for _, test := range []struct {
		value any
		want  string
		ok    bool
	}{
		{value: nil, want: "", ok: true},
		{value: "", want: "", ok: true},
		{value: " KIMI ", want: ModelCompatibilityKimi, ok: true},
		{value: "openai", want: ModelCompatibilityOpenAI, ok: true},
		{value: "moonshot"},
		{value: 1},
	} {
		got, err := NormalizeModelCompatibility(test.value)
		if (err == nil) != test.ok {
			t.Errorf("NormalizeModelCompatibility(%#v) error = %v, want ok=%t", test.value, err, test.ok)
		}
		if err == nil && got != test.want {
			t.Errorf("NormalizeModelCompatibility(%#v) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestLoadRejectsUnknownModelType(t *testing.T) {
	dir := writeConfigFixture(t)
	writeFile(t, filepath.Join(dir, "providers", "unknown.yaml"), `name: unknown
base_url: http://localhost:8080/v1
api_key: direct-secret

models:
  default:
    id: model-default
    type: not-openai
`)

	_, err := Load(rootConfigPath(dir))
	assertErrorContains(t, err, `unknown model type "not-openai"`, "supported provider types: anthropic-messages, openai-codex, openai-chat, openai-responses")
}

func TestLoadRejectsCompatibilityForNonChatModel(t *testing.T) {
	dir := writeConfigFixture(t)
	writeFile(t, filepath.Join(dir, "providers", "incompatible.yaml"), `name: incompatible
base_url: http://localhost:8080/v1
api_key: direct-secret

models:
  default:
    id: model-default
    type: openai-responses
    compatibility: kimi
`)

	_, err := Load(rootConfigPath(dir))
	assertErrorContains(t, err, "compatibility is only supported for openai-chat models")
}

func TestLoadRejectsInvalidProviderRequestTimeout(t *testing.T) {
	dir := writeConfigFixture(t)
	writeFile(t, filepath.Join(dir, "providers", "invalid-timeout.yaml"), `name: invalid-timeout
base_url: http://localhost:8080/v1
request_timeout: immediately

models:
  default:
    id: model-default
`)

	_, err := Load(rootConfigPath(dir))
	assertErrorContains(t, err, "request_timeout must be a positive duration")
}


func TestLoadRejectsInvalidProviderProxyURLs(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
	}{
		{name: "HTTP proxy without scheme", field: "http_proxy", value: "127.0.0.1:7890"},
		{name: "HTTPS proxy without host", field: "https_proxy", value: "https://"},
		{name: "unsupported proxy scheme", field: "https_proxy", value: "socks5://127.0.0.1:7890"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := writeConfigFixture(t)
			writeFile(t, filepath.Join(dir, "providers", "invalid-proxy.yaml"), `name: invalid-proxy
base_url: http://localhost:8080/v1
`+test.field+`: `+test.value+`

models:
  default:
    id: model-default
`)

			_, err := Load(rootConfigPath(dir))
			assertErrorContains(t, err, test.field+" must be an absolute HTTP or HTTPS URL")
		})
	}
}

func TestModelListIsSortedAndIncludesActualIDs(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	got := cfg.ModelList()
	want := []ModelInfo{
		{Provider: "local", Profile: "small", ID: "local-small"},
		{Provider: "paperhub", Profile: "glm-5.2", ID: "glm-5.2"},
		{Provider: "paperhub", Profile: "glm-5.2-fast", ID: "glm-5.2"},
	}

	if len(got) != len(want) {
		t.Fatalf("len(ModelList()) = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ModelList()[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestResolveModelExplicitProviderModel(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	t.Setenv("PAPERHUB_API_KEY", "resolved-paperhub-secret")

	got, err := cfg.ResolveModel("paperhub", "glm-5.2-fast")
	if err != nil {
		t.Fatalf("ResolveModel() error = %v", err)
	}

	if got.ProviderName != "paperhub" {
		t.Fatalf("ProviderName = %q, want paperhub", got.ProviderName)
	}
	if got.Provider.Name != "paperhub" {
		t.Fatalf("Provider.Name = %q, want paperhub", got.Provider.Name)
	}
	if got.Provider.BaseURL != "https://tc-paperhub.diezhi.net/v1" {
		t.Fatalf("Provider.BaseURL = %q, want PaperHub base URL", got.Provider.BaseURL)
	}
	if got.Provider.RequestTimeout != "45s" {
		t.Fatalf("Provider.RequestTimeout = %q, want 45s", got.Provider.RequestTimeout)
	}
	if got.Provider.HTTPProxy != "http://127.0.0.1:7890" || got.Provider.HTTPSProxy != "https://proxy.example.test:8443" {
		t.Fatalf("Provider proxies = %q/%q, want configured HTTP/HTTPS proxies", got.Provider.HTTPProxy, got.Provider.HTTPSProxy)
	}
	if got.Provider.APIKey != "$PAPERHUB_API_KEY" {
		t.Fatalf("Provider.APIKey = %q, want $PAPERHUB_API_KEY", got.Provider.APIKey)
	}
	if got.Provider.ResolvedAPIKey != "resolved-paperhub-secret" {
		t.Fatalf("Provider.ResolvedAPIKey = %q, want resolved API key", got.Provider.ResolvedAPIKey)
	}
	if got.Profile != "glm-5.2-fast" {
		t.Fatalf("Profile = %q, want glm-5.2-fast", got.Profile)
	}
	if got.ModelID != "glm-5.2" {
		t.Fatalf("ModelID = %q, want glm-5.2", got.ModelID)
	}
	if got.Type != ProviderTypeOpenAIChat {
		t.Fatalf("Type = %q, want default %q", got.Type, ProviderTypeOpenAIChat)
	}
	if got.Parameters["temperature"] != 0.2 {
		t.Fatalf("temperature = %#v, want 0.2", got.Parameters["temperature"])
	}
	if got.Parameters["max_tokens"] != 2048 {
		t.Fatalf("max_tokens = %#v, want 2048", got.Parameters["max_tokens"])
	}
}

func TestResolveModelUsesDefaultProviderAndModel(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	t.Setenv("PAPERHUB_API_KEY", "resolved-paperhub-secret")

	got, err := cfg.ResolveModel("", "")
	if err != nil {
		t.Fatalf("ResolveModel() error = %v", err)
	}

	if got.ProviderName != "paperhub" {
		t.Fatalf("ProviderName = %q, want paperhub", got.ProviderName)
	}
	if got.Profile != "glm-5.2" {
		t.Fatalf("Profile = %q, want glm-5.2", got.Profile)
	}
	if got.ModelID != "glm-5.2" {
		t.Fatalf("ModelID = %q, want glm-5.2", got.ModelID)
	}
	if got.Type != ProviderTypeOpenAIChat {
		t.Fatalf("Type = %q, want default %q", got.Type, ProviderTypeOpenAIChat)
	}
	if got.Provider.ResolvedAPIKey != "resolved-paperhub-secret" {
		t.Fatalf("Provider.ResolvedAPIKey = %q, want resolved API key", got.Provider.ResolvedAPIKey)
	}
	if got.Parameters["max_tokens"] != 4096 {
		t.Fatalf("max_tokens = %#v, want 4096", got.Parameters["max_tokens"])
	}
}

func TestResolveModelEnvAPIKeyErrorsWhenEnvIsMissingOrEmpty(t *testing.T) {
	tests := []struct {
		name   string
		env    string
		setEnv bool
	}{
		{name: "missing", env: "SAI_CONFIG_TEST_MISSING_API_KEY"},
		{name: "empty", env: "SAI_CONFIG_TEST_EMPTY_API_KEY", setEnv: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeConfigFixture(t)
			cfg, err := Load(rootConfigPath(dir))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}

			if tt.setEnv {
				t.Setenv(tt.env, "")
			} else {
				unsetEnvForTest(t, tt.env)
			}
			provider := cfg.Providers["paperhub"]
			provider.APIKey = "$" + tt.env
			cfg.Providers["paperhub"] = provider

			_, err = cfg.ResolveModel("paperhub", "glm-5.2")
			assertErrorContains(t, err, `resolve api_key for provider "paperhub"`, `API key environment variable "`+tt.env+`" is not set`)
		})
	}
}

func TestResolveModelDirectAPIKeyIsResolvedAndRedactedInJSON(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	got, err := cfg.ResolveModel("local", "small")
	if err != nil {
		t.Fatalf("ResolveModel() error = %v", err)
	}

	if got.Provider.APIKey != "direct-local-secret" {
		t.Fatalf("Provider.APIKey = %q, want raw direct API key", got.Provider.APIKey)
	}
	if got.Provider.ResolvedAPIKey != "direct-local-secret" {
		t.Fatalf("Provider.ResolvedAPIKey = %q, want direct API key", got.Provider.ResolvedAPIKey)
	}

	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal(cfg) error = %v", err)
	}
	resolvedJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal(resolved) error = %v", err)
	}
	for name, output := range map[string]string{
		"config":   string(cfgJSON),
		"resolved": string(resolvedJSON),
	} {
		if strings.Contains(output, "direct-local-secret") {
			t.Fatalf("%s JSON leaked direct API key: %s", name, output)
		}
		if !strings.Contains(output, "<redacted>") && !strings.Contains(output, `\u003credacted\u003e`) {
			t.Fatalf("%s JSON = %s, want redacted API key", name, output)
		}
	}
}

func TestResolveModelUnknownProviderIncludesChoices(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	_, err = cfg.ResolveModel("missing", "glm-5.2")
	assertErrorContains(t, err, `unknown provider "missing"`, "available providers: local, paperhub")
}

func TestResolveModelUnknownModelIncludesChoices(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	_, err = cfg.ResolveModel("paperhub", "missing")
	assertErrorContains(t, err, `unknown model "missing" for provider "paperhub"`, "available models: glm-5.2 (id glm-5.2), glm-5.2-fast (id glm-5.2)")
}

func TestResolveModelMissingDefaultsIncludesChoices(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	cfg.DefaultProvider = ""
	cfg.DefaultModel = ""

	_, err = cfg.ResolveModel("", "")
	assertErrorContains(t, err, "provider and model are required", "default_provider/default_model", "available models:", "local/small (id local-small)", "paperhub/glm-5.2 (id glm-5.2)")
}

func TestResolveModelInvalidDefaultModelIncludesChoices(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	cfg.DefaultModel = "missing"

	_, err = cfg.ResolveModel("", "")
	assertErrorContains(t, err, `unknown model "missing" for provider "paperhub"`, "available models: glm-5.2 (id glm-5.2), glm-5.2-fast (id glm-5.2)")
}

func TestResolveModelCopiesParameters(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	t.Setenv("PAPERHUB_API_KEY", "resolved-paperhub-secret")

	got, err := cfg.ResolveModel("paperhub", "glm-5.2")
	if err != nil {
		t.Fatalf("ResolveModel() error = %v", err)
	}

	got.Parameters["max_tokens"] = 1
	got.Parameters["new_param"] = true
	providerProfile := got.Provider.Models["glm-5.2"]
	providerProfile.Parameters["temperature"] = 1.0

	original := cfg.Providers["paperhub"].Models["glm-5.2"].Parameters
	if original["max_tokens"] != 4096 {
		t.Fatalf("original max_tokens = %#v, want 4096", original["max_tokens"])
	}
	if _, ok := original["new_param"]; ok {
		t.Fatal("original parameters contain new_param from resolved copy")
	}
	if original["temperature"] != 0.6 {
		t.Fatalf("original temperature = %#v, want 0.6", original["temperature"])
	}
}

func TestLoadMissingConfigMentionsSelectedFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "custom-root.yaml")

	_, err := Load(configPath)
	if err == nil {
		t.Fatal("Load() error = nil, want error")
	}
	if got := err.Error(); !strings.Contains(got, configPath) {
		t.Fatalf("Load() error = %q, want mention %q", got, configPath)
	}
}

func assertErrorContains(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want error")
	}
	got := err.Error()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("error = %q, want contain %q", got, want)
		}
	}
}

func sameStrings(a, b []string) bool {
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

func unsetEnvForTest(t *testing.T, name string) {
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

func writeConfigFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	providersDir := filepath.Join(dir, "providers")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: paperhub
default_model: glm-5.2
provider_dir: providers

agent:
  max_iterations: 8
  stream: true
  show_reasoning: false

tools:
  enabled: []

logging:
  path: logs/sai.jsonl
  level: info
`)

	writeFile(t, filepath.Join(providersDir, "paperhub.yaml"), `name: paperhub
base_url: https://tc-paperhub.diezhi.net/v1
api_key: $PAPERHUB_API_KEY
request_timeout: 45s
http_proxy: http://127.0.0.1:7890
https_proxy: https://proxy.example.test:8443

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

	writeFile(t, filepath.Join(providersDir, "local.yml"), `name: local
base_url: http://localhost:8080/v1
api_key: direct-local-secret

models:
  small:
    id: local-small
`)

	writeFile(t, filepath.Join(providersDir, "ignored.txt"), `name: ignored
`)

	return dir
}

func rootConfigPath(dir string) string {
	return filepath.Join(dir, "sai.yaml")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func TestProviderNameDefaultsFromFileName(t *testing.T) {
	dir := t.TempDir()
	// A minimal profile without an explicit name.
	profile := "base_url: http://127.0.0.1:1/v1\napi_key: k\nmodels:\n  m:\n    id: m\n    type: openai-chat\n"
	if err := os.WriteFile(filepath.Join(dir, "paperhub.yaml"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	providers, err := LoadProviderConfigs(dir)
	if err != nil {
		t.Fatalf("LoadProviderConfigs failed on a nameless profile: %v", err)
	}
	if _, ok := providers["paperhub"]; !ok {
		t.Fatalf("providers = %v, want paperhub derived from the file name", providers)
	}

	// An explicit name still wins.
	named := "name: explicit\nbase_url: http://127.0.0.1:1/v1\napi_key: k\nmodels:\n  m:\n    id: m\n    type: openai-chat\n"
	if err := os.WriteFile(filepath.Join(dir, "other.yaml"), []byte(named), 0o644); err != nil {
		t.Fatal(err)
	}
	providers, err = LoadProviderConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := providers["explicit"]; !ok {
		t.Fatalf("providers = %v, want explicit name preserved", providers)
	}
}

func TestAgentAutoCompactDefaultsOn(t *testing.T) {
	dir := writeConfigFixture(t)

	// Absent agent block: the default survives the unmarshal.
	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Agent.AutoCompact {
		t.Fatal("auto_compact default is not on")
	}

	// An explicit opt-out must stick.
	writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: paperhub
agent:
  auto_compact: false
`)
	cfg, err = Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.AutoCompact {
		t.Fatal("auto_compact: false did not stick")
	}
}
