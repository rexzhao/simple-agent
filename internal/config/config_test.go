package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
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

	wantSkillDirs := []string{filepath.Join(wantConfigDir, "skills")}
	resolvedSkillDirs, err := cfg.ResolveSkillDirs(dir)
	if err != nil {
		t.Fatalf("ResolveSkillDirs() error = %v", err)
	}
	if !sameStrings(resolvedSkillDirs, wantSkillDirs) {
		t.Fatalf("ResolveSkillDirs() = %#v, want %#v", resolvedSkillDirs, wantSkillDirs)
	}

	wantLogPath := filepath.Join(wantConfigDir, "logs", "sai.jsonl")
	if cfg.Logging.Path != wantLogPath {
		t.Fatalf("Logging.Path = %q, want %q", cfg.Logging.Path, wantLogPath)
	}

	wantSessionsDir := filepath.Join(wantConfigDir, "sessions")
	if cfg.Sessions.Enabled {
		t.Fatal("Sessions.Enabled = true, want false default")
	}
	if cfg.Sessions.Dir != wantSessionsDir {
		t.Fatalf("Sessions.Dir = %q, want %q", cfg.Sessions.Dir, wantSessionsDir)
	}
	if !cfg.Sessions.SaveToolResults {
		t.Fatal("Sessions.SaveToolResults = false, want true default")
	}

	wantMCPDir := filepath.Join(wantConfigDir, "mcp")
	if cfg.MCPDir != wantMCPDir {
		t.Fatalf("MCPDir = %q, want %q", cfg.MCPDir, wantMCPDir)
	}

	if !sameStrings(cfg.Agent.InstructionFiles, []string{"$CWD/AGENTS.md"}) {
		t.Fatalf("Agent.InstructionFiles = %#v, want default $CWD/AGENTS.md", cfg.Agent.InstructionFiles)
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

func TestLoadBaseDisablesDiagnosticLoggingByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sai.yaml")
	writeFile(t, path, "{}\n")
	cfg, err := LoadBase(path)
	if err != nil {
		t.Fatalf("LoadBase() error = %v", err)
	}
	if cfg.Logging.Path != "" {
		t.Fatalf("Logging.Path = %q, want disabled by default", cfg.Logging.Path)
	}
}

func TestEnsureRootConfigCreatesCoreLayoutWithoutLogs(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "custom-agent.yaml")
	if err := EnsureRootConfig(path); err != nil {
		t.Fatalf("EnsureRootConfig() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stat(config) error = %v", err)
	}
	for _, name := range []string{"providers", "auth", "mcp"} {
		if info, err := os.Stat(filepath.Join(root, name)); err != nil || !info.IsDir() {
			t.Fatalf("Stat(%s) = %v/%v, want directory", name, info, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "logs")); !os.IsNotExist(err) {
		t.Fatalf("Stat(logs) error = %v, want no logs directory", err)
	}
	cfg, err := LoadBase(path)
	if err != nil {
		t.Fatalf("LoadBase(created config) error = %v", err)
	}
	if cfg.Logging.Path != "" {
		t.Fatalf("created config logging path = %q, want disabled", cfg.Logging.Path)
	}
}

func TestEnsureRootConfigRestrictsConfigPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not an access-control guarantee on Windows")
	}

	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("Chmod(root) error = %v", err)
	}
	path := filepath.Join(root, "sai.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod(config) error = %v", err)
	}

	if err := EnsureRootConfig(path); err != nil {
		t.Fatalf("EnsureRootConfig() error = %v", err)
	}
	assertFileMode(t, root, 0o700)
	assertFileMode(t, path, 0o600)
	for _, name := range []string{"providers", "auth", "mcp"} {
		assertFileMode(t, filepath.Join(root, name), 0o700)
	}
}

func TestWriteProviderConfigRestrictsExistingPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not an access-control guarantee on Windows")
	}

	root := t.TempDir()
	providers := filepath.Join(root, "providers")
	if err := os.Mkdir(providers, 0o755); err != nil {
		t.Fatalf("Mkdir(providers) error = %v", err)
	}
	path := filepath.Join(providers, "local.yaml")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(provider) error = %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod(provider) error = %v", err)
	}

	if err := WriteProviderConfig(path, ProviderConfig{
		Name:    "local",
		BaseURL: "https://example.test/v1",
		APIKey:  "$LOCAL_API_KEY",
		Models: map[string]ModelProfile{
			"default": {ID: "example-model"},
		},
	}); err != nil {
		t.Fatalf("WriteProviderConfig() error = %v", err)
	}
	assertFileMode(t, providers, 0o700)
	assertFileMode(t, path, 0o600)
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

func TestLoadDefaultsCompactionConfig(t *testing.T) {
	dir := writeConfigFixture(t)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Compaction.Enabled {
		t.Fatal("Compaction.Enabled = true, want false default")
	}
	if cfg.Compaction.ThresholdPercent != 80 {
		t.Fatalf("Compaction.ThresholdPercent = %d, want 80", cfg.Compaction.ThresholdPercent)
	}
	if cfg.Compaction.SummaryProvider != "" {
		t.Fatalf("Compaction.SummaryProvider = %q, want empty default", cfg.Compaction.SummaryProvider)
	}
	if cfg.Compaction.SummaryModel != "" {
		t.Fatalf("Compaction.SummaryModel = %q, want empty default", cfg.Compaction.SummaryModel)
	}

	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal(cfg) error = %v", err)
	}
	var got struct {
		Compaction CompactionConfig `json:"compaction"`
	}
	if err := json.Unmarshal(cfgJSON, &got); err != nil {
		t.Fatalf("Unmarshal(cfg JSON) error = %v", err)
	}
	if got.Compaction != cfg.Compaction {
		t.Fatalf("JSON compaction = %#v, want %#v", got.Compaction, cfg.Compaction)
	}
}

func TestLoadReadsCompactionConfig(t *testing.T) {
	dir := writeConfigFixture(t)
	writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: paperhub
default_model: glm-5.2
provider_dir: providers

compaction:
  enabled: true
  threshold_percent: 65
  reserved: 12000
  summary_provider: ../summary-provider
  summary_model: models/summary
`)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !cfg.Compaction.Enabled {
		t.Fatal("Compaction.Enabled = false, want true")
	}
	if cfg.Compaction.ThresholdPercent != 65 {
		t.Fatalf("Compaction.ThresholdPercent = %d, want 65", cfg.Compaction.ThresholdPercent)
	}
	if cfg.Compaction.Reserved != 12000 {
		t.Fatalf("Compaction.Reserved = %d, want 12000", cfg.Compaction.Reserved)
	}
	if cfg.Compaction.SummaryProvider != "../summary-provider" {
		t.Fatalf("Compaction.SummaryProvider = %q, want path-like value unchanged", cfg.Compaction.SummaryProvider)
	}
	if cfg.Compaction.SummaryModel != "models/summary" {
		t.Fatalf("Compaction.SummaryModel = %q, want path-like value unchanged", cfg.Compaction.SummaryModel)
	}
}

func TestLoadRejectsNegativeCompactionReserved(t *testing.T) {
	dir := writeConfigFixture(t)
	writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: paperhub
default_model: glm-5.2
provider_dir: providers

compaction:
  reserved: -1
`)

	_, err := Load(rootConfigPath(dir))
	assertErrorContains(t, err, "validate config file", "compaction.reserved must not be negative")
}

func TestLoadRejectsInvalidCompactionThreshold(t *testing.T) {
	tests := []struct {
		name      string
		threshold string
	}{
		{name: "zero", threshold: "0"},
		{name: "negative", threshold: "-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeConfigFixture(t)
			writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: paperhub
default_model: glm-5.2
provider_dir: providers

compaction:
  threshold_percent: `+tt.threshold+`
`)

			_, err := Load(rootConfigPath(dir))
			assertErrorContains(t, err, "validate config file", "compaction.threshold_percent must be positive")
		})
	}
}

func TestLoadAllowsEmptyInstructionFiles(t *testing.T) {
	dir := writeConfigFixture(t)
	writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: paperhub
default_model: glm-5.2
provider_dir: providers

agent:
  instruction_files: []
  max_turns: 8
  stream: true
`)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Agent.InstructionFiles == nil {
		t.Fatal("Agent.InstructionFiles = nil, want explicit empty slice")
	}
	if len(cfg.Agent.InstructionFiles) != 0 {
		t.Fatalf("Agent.InstructionFiles = %#v, want empty", cfg.Agent.InstructionFiles)
	}
}

func TestLoadReadsConfiguredInstructionFiles(t *testing.T) {
	dir := writeConfigFixture(t)
	writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: paperhub
default_model: glm-5.2
provider_dir: providers

agent:
  instruction_files:
    - $CONFIG/team.md
    - $CWD/AGENTS.local.md
    - $USER/sai/global.md
    - $REPO/docs/*.md
  max_turns: 8
  stream: true
`)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := []string{
		"$CONFIG/team.md",
		"$CWD/AGENTS.local.md",
		"$USER/sai/global.md",
		"$REPO/docs/*.md",
	}
	if !sameStrings(cfg.Agent.InstructionFiles, want) {
		t.Fatalf("Agent.InstructionFiles = %#v, want %#v", cfg.Agent.InstructionFiles, want)
	}
}

func TestLoadResolvesCustomSkillDirs(t *testing.T) {
	dir := writeConfigFixture(t)
	absSkills := filepath.Join(t.TempDir(), "shared-skills")
	writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: paperhub
default_model: glm-5.2
provider_dir: providers
skill_dirs:
  - local-skills
  - team-skills
  - `+absSkills+`
`)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	wantConfigDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("filepath.Abs() error = %v", err)
	}
	wantSkillDirs := []string{
		filepath.Join(filepath.Clean(wantConfigDir), "local-skills"),
		filepath.Join(filepath.Clean(wantConfigDir), "team-skills"),
		filepath.Clean(absSkills),
	}
	resolvedSkillDirs, err := cfg.ResolveSkillDirs(dir)
	if err != nil {
		t.Fatalf("ResolveSkillDirs() error = %v", err)
	}
	if !sameStrings(resolvedSkillDirs, wantSkillDirs) {
		t.Fatalf("ResolveSkillDirs() = %#v, want %#v", resolvedSkillDirs, wantSkillDirs)
	}
}

func TestResolveSkillDirsExpandsPathPlaceholders(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	configPath := filepath.Join(configDir, "sai.yaml")
	repoDir := filepath.Join(root, "repo")
	cwd := filepath.Join(repoDir, "work", "nested")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(config) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("MkdirAll(cwd) error = %v", err)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}

	configured := []string{
		"$HOME/global-skills",
		"$USER/legacy-skills",
		"$REPO/.agents/skills",
		"$CWD/.agents/skills",
		"$CONFIG/skills",
		"relative-skills",
	}
	writeFile(t, configPath, `skill_dirs:
  - $HOME/global-skills
  - $USER/legacy-skills
  - $REPO/.agents/skills
  - $CWD/.agents/skills
  - $CONFIG/skills
  - relative-skills
`)
	cfg, err := LoadBase(configPath)
	if err != nil {
		t.Fatalf("LoadBase() error = %v", err)
	}
	if !sameStrings(cfg.SkillDirs, configured) {
		t.Fatalf("SkillDirs = %#v, want raw configured values %#v", cfg.SkillDirs, configured)
	}
	got, err := cfg.ResolveSkillDirs(cwd)
	if err != nil {
		t.Fatalf("ResolveSkillDirs() error = %v", err)
	}
	want := []string{
		filepath.Join(homeDir, "global-skills"),
		filepath.Join(homeDir, "legacy-skills"),
		filepath.Join(repoDir, ".agents", "skills"),
		filepath.Join(cwd, ".agents", "skills"),
		filepath.Join(configDir, "skills"),
		filepath.Join(configDir, "relative-skills"),
	}
	if !sameStrings(got, want) {
		t.Fatalf("ResolveSkillDirs() = %#v, want %#v", got, want)
	}
}

func TestLoadAllowsEmptySkillDirs(t *testing.T) {
	dir := writeConfigFixture(t)
	writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: paperhub
default_model: glm-5.2
provider_dir: providers
skill_dirs: []
`)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.SkillDirs == nil {
		t.Fatal("SkillDirs = nil, want explicit empty slice")
	}
	if len(cfg.SkillDirs) != 0 {
		t.Fatalf("SkillDirs = %#v, want empty", cfg.SkillDirs)
	}
}

func TestLoadBaseResolvesSubagentPathsRelativeToConfig(t *testing.T) {
	dir := t.TempDir()
	absSubagent := filepath.Join(t.TempDir(), "absolute-agent.yaml")
	writeFile(t, filepath.Join(dir, "sai.yaml"), `subagents:
  reviewer: subagents/reviewer.yaml
  absolute: `+filepath.ToSlash(absSubagent)+`
`)

	cfg, err := LoadBase(filepath.Join(dir, "sai.yaml"))
	if err != nil {
		t.Fatalf("LoadBase() error = %v", err)
	}

	wantConfigDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("filepath.Abs() error = %v", err)
	}
	wantConfigDir = filepath.Clean(wantConfigDir)
	wantReviewer := filepath.Join(wantConfigDir, "subagents", "reviewer.yaml")
	if cfg.Subagents["reviewer"] != wantReviewer {
		t.Fatalf("Subagents[reviewer] = %q, want %q", cfg.Subagents["reviewer"], wantReviewer)
	}
	if cfg.Subagents["absolute"] != filepath.Clean(absSubagent) {
		t.Fatalf("Subagents[absolute] = %q, want %q", cfg.Subagents["absolute"], filepath.Clean(absSubagent))
	}
}

func TestLoadBaseHandlesMissingAndEmptySubagents(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: fake
`)

		cfg, err := LoadBase(filepath.Join(dir, "sai.yaml"))
		if err != nil {
			t.Fatalf("LoadBase() error = %v", err)
		}
		if cfg.Subagents != nil {
			t.Fatalf("Subagents = %#v, want nil for missing config", cfg.Subagents)
		}
	})

	t.Run("empty", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "sai.yaml"), `subagents: {}
`)

		cfg, err := LoadBase(filepath.Join(dir, "sai.yaml"))
		if err != nil {
			t.Fatalf("LoadBase() error = %v", err)
		}
		if cfg.Subagents == nil {
			t.Fatal("Subagents = nil, want explicit empty map")
		}
		if len(cfg.Subagents) != 0 {
			t.Fatalf("Subagents = %#v, want empty", cfg.Subagents)
		}
	})
}

func TestLoadBaseChildConfigUsesAgentAndPromptSchema(t *testing.T) {
	dir := t.TempDir()
	childDir := filepath.Join(dir, "subagents")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	childPath := filepath.Join(childDir, "reviewer.yaml")
	writeFile(t, childPath, `default_provider: child-provider
default_model: child-model
skill_dirs:
  - child-skills
agent:
  name: Reviewer
  description: Read-only reviewer.
  timeout: 30s
  max_turns: 6
prompt:
  system_prompt: |
    Review only the assigned scope.
`)

	cfg, err := LoadBase(childPath)
	if err != nil {
		t.Fatalf("LoadBase() error = %v", err)
	}

	if cfg.Agent.Name != "Reviewer" {
		t.Fatalf("Agent.Name = %q, want Reviewer", cfg.Agent.Name)
	}
	if cfg.Agent.Description != "Read-only reviewer." {
		t.Fatalf("Agent.Description = %q, want description", cfg.Agent.Description)
	}
	if cfg.Agent.Timeout != "30s" {
		t.Fatalf("Agent.Timeout = %q, want 30s", cfg.Agent.Timeout)
	}
	if cfg.Agent.MaxTurns != 6 {
		t.Fatalf("Agent.MaxTurns = %d, want 6", cfg.Agent.MaxTurns)
	}
	if cfg.Prompt.SystemPrompt != "Review only the assigned scope.\n" {
		t.Fatalf("Prompt.SystemPrompt = %q, want child prompt", cfg.Prompt.SystemPrompt)
	}
	resolvedSkillDirs, err := cfg.ResolveSkillDirs(childDir)
	if err != nil {
		t.Fatalf("ResolveSkillDirs() error = %v", err)
	}
	if !sameStrings(resolvedSkillDirs, []string{filepath.Join(childDir, "child-skills")}) {
		t.Fatalf("ResolveSkillDirs() = %#v, want child-relative skill dir", resolvedSkillDirs)
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
auth_dir: auth-files
skill_dirs:
  - local-skills
mcp_dir: mcp-files

logging:
  path: run-logs/sai.jsonl

sessions:
  dir: saved-sessions
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
	if cfg.AuthDir != filepath.Join(rootDir, "auth-files") {
		t.Fatalf("AuthDir = %q", cfg.AuthDir)
	}
	resolvedSkillDirs, err := cfg.ResolveSkillDirs(rootDir)
	if err != nil {
		t.Fatalf("ResolveSkillDirs() error = %v", err)
	}
	if !sameStrings(resolvedSkillDirs, []string{filepath.Join(rootDir, "local-skills")}) {
		t.Fatalf("ResolveSkillDirs() = %#v", resolvedSkillDirs)
	}
	if cfg.MCPDir != filepath.Join(rootDir, "mcp-files") {
		t.Fatalf("MCPDir = %q", cfg.MCPDir)
	}
	if cfg.Logging.Path != filepath.Join(rootDir, "run-logs", "sai.jsonl") {
		t.Fatalf("Logging.Path = %q", cfg.Logging.Path)
	}
	if cfg.Sessions.Dir != filepath.Join(rootDir, "saved-sessions") {
		t.Fatalf("Sessions.Dir = %q", cfg.Sessions.Dir)
	}
	if _, ok := cfg.Providers["fake"]; !ok {
		t.Fatalf("Providers = %#v, want fake", cfg.Providers)
	}
}

func TestLoadResolvesCustomSessionsDir(t *testing.T) {
	dir := writeConfigFixture(t)
	writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: paperhub
default_model: glm-5.2
provider_dir: providers

sessions:
  enabled: true
  dir: saved-sessions
  save_tool_results: false
`)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	wantConfigDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("filepath.Abs() error = %v", err)
	}
	wantSessionsDir := filepath.Join(filepath.Clean(wantConfigDir), "saved-sessions")
	if !cfg.Sessions.Enabled {
		t.Fatal("Sessions.Enabled = false, want true")
	}
	if cfg.Sessions.Dir != wantSessionsDir {
		t.Fatalf("Sessions.Dir = %q, want %q", cfg.Sessions.Dir, wantSessionsDir)
	}
	if cfg.Sessions.SaveToolResults {
		t.Fatal("Sessions.SaveToolResults = true, want false")
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
	if cfg.AuthDir != authDir {
		t.Fatalf("AuthDir = %q, want %q", cfg.AuthDir, authDir)
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

func TestLoadModelProfileContextWindowAndNestedParameters(t *testing.T) {
	dir := t.TempDir()
	providersDir := filepath.Join(dir, "providers")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	writeFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: fake
default_model: default
provider_dir: providers
`)
	writeFile(t, filepath.Join(providersDir, "fake.yaml"), `name: fake
base_url: http://localhost:8080/v1
api_key: direct-secret

models:
  default:
    id: model-default
    type: anthropic-messages
    context_window: 400000
    input_limit: 272000
    output_limit: 128000
    parameters:
      temperature: 0.2
      max_tokens: 64
  estimated:
    id: model-estimated
    max_tokens: 32
`)

	cfg, err := Load(rootConfigPath(dir))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	profile := cfg.Providers["fake"].Models["default"]
	if profile.ContextWindow != 400000 || profile.InputLimit != 272000 || profile.OutputLimit != 128000 {
		t.Fatalf("model limits = context %d input %d output %d, want 400000/272000/128000", profile.ContextWindow, profile.InputLimit, profile.OutputLimit)
	}
	if got := profile.Parameters["temperature"]; got != 0.2 {
		t.Fatalf("temperature = %#v, want 0.2", got)
	}
	if got := profile.Parameters["max_tokens"]; got != 64 {
		t.Fatalf("max_tokens = %#v, want 64", got)
	}
	if profile.Type != ProviderTypeAnthropicMessages {
		t.Fatalf("Type = %q, want %q", profile.Type, ProviderTypeAnthropicMessages)
	}
	if _, ok := profile.Parameters["type"]; ok {
		t.Fatal("Parameters unexpectedly contains type")
	}
	if _, ok := profile.Parameters["context_window"]; ok {
		t.Fatal("Parameters unexpectedly contains context_window")
	}
	if _, ok := profile.Parameters["input_limit"]; ok {
		t.Fatal("Parameters unexpectedly contains input_limit")
	}
	if _, ok := profile.Parameters["output_limit"]; ok {
		t.Fatal("Parameters unexpectedly contains output_limit")
	}
	if _, ok := profile.Parameters["parameters"]; ok {
		t.Fatal("Parameters unexpectedly contains nested parameters field")
	}

	resolved, err := cfg.ResolveModel("fake", "default")
	if err != nil {
		t.Fatalf("ResolveModel(default) error = %v", err)
	}
	if resolved.ContextWindow != 400000 || resolved.ContextWindowSource != string(contextwindow.WindowSourceConfigured) ||
		resolved.InputLimit != 272000 || resolved.OutputLimit != 128000 {
		t.Fatalf(
			"resolved limits = context %d/%q input %d output %d, want 400000/configured/272000/128000",
			resolved.ContextWindow,
			resolved.ContextWindowSource,
			resolved.InputLimit,
			resolved.OutputLimit,
		)
	}
	if resolved.Type != ProviderTypeAnthropicMessages {
		t.Fatalf("resolved.Type = %q, want %q", resolved.Type, ProviderTypeAnthropicMessages)
	}

	resolved, err = cfg.ResolveModel("fake", "estimated")
	if err != nil {
		t.Fatalf("ResolveModel(estimated) error = %v", err)
	}
	if resolved.ContextWindow != contextwindow.DefaultContextWindowTokens || resolved.ContextWindowSource != string(contextwindow.WindowSourceEstimated) {
		t.Fatalf("resolved context = %d/%q, want default estimated", resolved.ContextWindow, resolved.ContextWindowSource)
	}
	if resolved.Type != ProviderTypeOpenAIChat {
		t.Fatalf("resolved.Type = %q, want default %q", resolved.Type, ProviderTypeOpenAIChat)
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

func TestLoadReadsMCPServerYAMLFiles(t *testing.T) {
	dir := writeConfigFixture(t)
	writeMCPFixture(t, dir)

	cfg, err := Load(rootConfigPath(dir))
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

	cfg, err := Load(rootConfigPath(dir))
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

	cfg, err := Load(rootConfigPath(dir))
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

	cfg, err := Load(rootConfigPath(dir))
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

	cfg, err := Load(rootConfigPath(dir))
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

			_, err := Load(rootConfigPath(dir))
			assertErrorContains(t, err, tt.want)
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
base_url: https://tc-paperhub.diezhi.net/v1
api_key: $PAPERHUB_API_KEY
request_timeout: 45s

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
