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

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	wantConfigDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("filepath.Abs() error = %v", err)
	}
	wantConfigDir = filepath.Clean(wantConfigDir)
	if cfg.ConfigDir != wantConfigDir {
		t.Fatalf("ConfigDir = %q, want %q", cfg.ConfigDir, wantConfigDir)
	}

	wantProviderDir := filepath.Join(wantConfigDir, "providers")
	if cfg.ProviderDir != wantProviderDir {
		t.Fatalf("ProviderDir = %q, want %q", cfg.ProviderDir, wantProviderDir)
	}

	wantSkillDir := filepath.Join(wantConfigDir, "skills")
	if cfg.SkillDir != wantSkillDir {
		t.Fatalf("SkillDir = %q, want %q", cfg.SkillDir, wantSkillDir)
	}

	wantLogPath := filepath.Join(wantConfigDir, "logs", "sai.jsonl")
	if cfg.Logging.Path != wantLogPath {
		t.Fatalf("Logging.Path = %q, want %q", cfg.Logging.Path, wantLogPath)
	}

	wantMCPDir := filepath.Join(wantConfigDir, "mcp")
	if cfg.MCPDir != wantMCPDir {
		t.Fatalf("MCPDir = %q, want %q", cfg.MCPDir, wantMCPDir)
	}

	provider := cfg.Providers["paperhub"]
	if provider.Name != "paperhub" {
		t.Fatalf("provider name = %q, want paperhub", provider.Name)
	}
	if provider.APIKey != "$PAPERHUB_API_KEY" {
		t.Fatalf("APIKey = %q, want $PAPERHUB_API_KEY", provider.APIKey)
	}
	if got := provider.Models["glm-5.2-fast"].ID; got != "glm-5.2" {
		t.Fatalf("fast profile id = %q, want glm-5.2", got)
	}
	if got := provider.Models["glm-5.2-fast"].Parameters["max_tokens"]; got != 2048 {
		t.Fatalf("fast profile max_tokens = %#v, want 2048", got)
	}
}

func TestLoadResolvesCustomSkillDir(t *testing.T) {
	dir := writeConfigFixture(t)
	writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: paperhub
default_model: glm-5.2
provider_dir: providers
skill_dir: local-skills
`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	wantConfigDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("filepath.Abs() error = %v", err)
	}
	wantSkillDir := filepath.Join(filepath.Clean(wantConfigDir), "local-skills")
	if cfg.SkillDir != wantSkillDir {
		t.Fatalf("SkillDir = %q, want %q", cfg.SkillDir, wantSkillDir)
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
type: anthropic-messages
base_url: https://api.anthropic.com/v1
api_key: $ANTHROPIC_API_KEY

models:
  claude-sonnet-5:
    id: claude-sonnet-5
    max_tokens: 4096
  claude-haiku-4-5:
    id: claude-haiku-4-5
    max_tokens: 2048
`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	provider := cfg.Providers["anthropic"]
	if provider.Type != ProviderTypeAnthropicMessages {
		t.Fatalf("provider.Type = %q, want %q", provider.Type, ProviderTypeAnthropicMessages)
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
	if resolved.Provider.Type != ProviderTypeAnthropicMessages {
		t.Fatalf("resolved.Provider.Type = %q, want %q", resolved.Provider.Type, ProviderTypeAnthropicMessages)
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
type: openai-responses
base_url: https://api.openai.com/v1
api_key: $OPENAI_API_KEY

models:
  default:
    id: gpt-5.1
    temperature: 0.2
`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	provider := cfg.Providers["openai"]
	if provider.Type != ProviderTypeOpenAIResponses {
		t.Fatalf("provider.Type = %q, want %q", provider.Type, ProviderTypeOpenAIResponses)
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
	if resolved.Provider.Type != ProviderTypeOpenAIResponses {
		t.Fatalf("resolved.Provider.Type = %q, want %q", resolved.Provider.Type, ProviderTypeOpenAIResponses)
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

func TestLoadRejectsUnknownProviderType(t *testing.T) {
	dir := writeConfigFixture(t)
	writeFile(t, filepath.Join(dir, "providers", "unknown.yaml"), `name: unknown
type: not-openai
base_url: http://localhost:8080/v1
api_key: direct-secret

models:
  default:
    id: model-default
`)

	_, err := Load(dir)
	assertErrorContains(t, err, `unknown provider type "not-openai"`, "supported provider types: anthropic-messages, openai-chat, openai-responses")
}

func TestLoadReadsMCPServerYAMLFiles(t *testing.T) {
	dir := writeConfigFixture(t)
	writeMCPFixture(t, dir)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(cfg.MCPServers) != 2 {
		t.Fatalf("len(MCPServers) = %d, want 2: %#v", len(cfg.MCPServers), cfg.MCPServers)
	}

	local := cfg.MCPServers["local"]
	if local.ID != "local" {
		t.Fatalf("local.ID = %q, want local", local.ID)
	}
	if !local.Enabled {
		t.Fatal("local.Enabled = false, want true")
	}
	if local.Command != "example-mcp-server" {
		t.Fatalf("local.Command = %q, want example-mcp-server", local.Command)
	}
	if got, want := strings.Join(local.Args, ","), "--root,."; got != want {
		t.Fatalf("local.Args = %q, want %q", got, want)
	}
	if local.Env["MODE"] != "test" {
		t.Fatalf("local.Env[MODE] = %q, want test", local.Env["MODE"])
	}
	if local.Env["SECRET"] != "direct-mcp-secret" {
		t.Fatalf("local.Env[SECRET] = %q, want direct MCP secret", local.Env["SECRET"])
	}

	remote := cfg.MCPServers["remote"]
	if remote.ID != "remote" {
		t.Fatalf("remote.ID = %q, want remote", remote.ID)
	}
	if remote.Enabled {
		t.Fatal("remote.Enabled = true, want false")
	}
	if remote.Command != "remote-mcp-server" {
		t.Fatalf("remote.Command = %q, want remote-mcp-server", remote.Command)
	}
	if _, ok := cfg.MCPServers["ignored"]; ok {
		t.Fatal("MCPServers contains ignored non-YAML file")
	}

	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal(cfg) error = %v", err)
	}
	output := string(cfgJSON)
	if strings.Contains(output, "direct-mcp-secret") {
		t.Fatalf("config JSON leaked direct MCP env value: %s", output)
	}
	if !strings.Contains(output, "MCP_TOKEN") {
		t.Fatalf("config JSON should include MCP env var name: %s", output)
	}
	if !strings.Contains(output, "<redacted>") && !strings.Contains(output, `\u003credacted\u003e`) {
		t.Fatalf("config JSON = %s, want redacted MCP env value", output)
	}
}

func TestSelectedMCPServersUsesEnabledByDefault(t *testing.T) {
	dir := writeConfigFixture(t)
	writeMCPFixture(t, dir)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	got, err := cfg.SelectedMCPServers(nil, false)
	if err != nil {
		t.Fatalf("SelectedMCPServers() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "local" {
		t.Fatalf("SelectedMCPServers() = %#v, want only local", got)
	}
}

func TestSelectedMCPServersDefaultsMissingEnabledToTrue(t *testing.T) {
	dir := writeConfigFixture(t)
	mcpDir := filepath.Join(dir, "mcp")
	if err := os.MkdirAll(mcpDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeFile(t, filepath.Join(mcpDir, "local.yaml"), `id: local
command: example-mcp-server
`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !cfg.MCPServers["local"].Enabled {
		t.Fatal("MCPServers[local].Enabled = false, want true when enabled is omitted")
	}
	got, err := cfg.SelectedMCPServers(nil, false)
	if err != nil {
		t.Fatalf("SelectedMCPServers() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "local" {
		t.Fatalf("SelectedMCPServers() = %#v, want default-enabled local", got)
	}
}

func TestSelectedMCPServersOverrideIgnoresEnabledFields(t *testing.T) {
	dir := writeConfigFixture(t)
	writeMCPFixture(t, dir)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	got, err := cfg.SelectedMCPServers([]string{"remote"}, true)
	if err != nil {
		t.Fatalf("SelectedMCPServers() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "remote" {
		t.Fatalf("SelectedMCPServers() = %#v, want only remote", got)
	}

	got, err = cfg.SelectedMCPServers([]string{}, true)
	if err != nil {
		t.Fatalf("SelectedMCPServers(empty override) error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("SelectedMCPServers(empty override) = %#v, want none", got)
	}
}

func TestSelectedMCPServersRejectsUnknownID(t *testing.T) {
	dir := writeConfigFixture(t)
	writeMCPFixture(t, dir)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	_, err = cfg.SelectedMCPServers([]string{"missing"}, true)
	assertErrorContains(t, err, `unknown MCP server "missing"`, "available MCP servers: local, remote")
}

func TestLoadMCPServerConfigErrors(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name: "duplicate id",
			files: map[string]string{
				"first.yaml": "id: local\nenabled: true\ncommand: first\n",
				"second.yml": "id: local\nenabled: false\ncommand: second\n",
			},
			want: `duplicate MCP server id "local"`,
		},
		{
			name: "missing id",
			files: map[string]string{
				"local.yaml": "enabled: true\ncommand: example-mcp-server\n",
			},
			want: "is missing id",
		},
		{
			name: "missing command",
			files: map[string]string{
				"local.yaml": "id: local\nenabled: true\n",
			},
			want: `server "local" is missing command`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeConfigFixture(t)
			mcpDir := filepath.Join(dir, "mcp")
			if err := os.MkdirAll(mcpDir, 0o755); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			for name, content := range tt.files {
				writeFile(t, filepath.Join(mcpDir, name), content)
			}

			_, err := Load(dir)
			assertErrorContains(t, err, tt.want)
		})
	}
}

func TestModelListIsSortedAndIncludesActualIDs(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(dir)
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

	cfg, err := Load(dir)
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
	if got.Parameters["temperature"] != 0.2 {
		t.Fatalf("temperature = %#v, want 0.2", got.Parameters["temperature"])
	}
	if got.Parameters["max_tokens"] != 2048 {
		t.Fatalf("max_tokens = %#v, want 2048", got.Parameters["max_tokens"])
	}
}

func TestResolveModelUsesDefaultProviderAndModel(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(dir)
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
			cfg, err := Load(dir)
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

	cfg, err := Load(dir)
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

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	_, err = cfg.ResolveModel("missing", "glm-5.2")
	assertErrorContains(t, err, `unknown provider "missing"`, "available providers: local, paperhub")
}

func TestResolveModelUnknownModelIncludesChoices(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	_, err = cfg.ResolveModel("paperhub", "missing")
	assertErrorContains(t, err, `unknown model "missing" for provider "paperhub"`, "available models: glm-5.2 (id glm-5.2), glm-5.2-fast (id glm-5.2)")
}

func TestResolveModelMissingDefaultsIncludesChoices(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(dir)
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

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	cfg.DefaultModel = "missing"

	_, err = cfg.ResolveModel("", "")
	assertErrorContains(t, err, `unknown model "missing" for provider "paperhub"`, "available models: glm-5.2 (id glm-5.2), glm-5.2-fast (id glm-5.2)")
}

func TestResolveModelCopiesParameters(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(dir)
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

func TestLoadMissingConfigMentionsSaiYAML(t *testing.T) {
	dir := t.TempDir()

	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load() error = nil, want error")
	}
	if got := err.Error(); !strings.Contains(got, "sai.yaml") {
		t.Fatalf("Load() error = %q, want mention sai.yaml", got)
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
  max_turns: 8
  stream: true
  show_reasoning: false

tools:
  enabled: []

logging:
  path: logs/sai.jsonl
  level: info
`)

	writeFile(t, filepath.Join(providersDir, "paperhub.yaml"), `name: paperhub
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

	writeFile(t, filepath.Join(providersDir, "local.yml"), `name: local
type: openai-chat
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

func writeMCPFixture(t *testing.T, dir string) {
	t.Helper()

	mcpDir := filepath.Join(dir, "mcp")
	if err := os.MkdirAll(mcpDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	writeFile(t, filepath.Join(mcpDir, "local.yaml"), `id: local
enabled: true
command: example-mcp-server
args:
  - --root
  - .
env:
  MODE: test
  SECRET: direct-mcp-secret
  TOKEN: $MCP_TOKEN
`)

	writeFile(t, filepath.Join(mcpDir, "remote.yml"), `id: remote
enabled: false
command: remote-mcp-server
args: []
env: {}
`)

	writeFile(t, filepath.Join(mcpDir, "ignored.txt"), `id: ignored
enabled: true
command: ignored-mcp-server
`)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
