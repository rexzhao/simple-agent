package config

import (
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

	wantLogPath := filepath.Join(wantConfigDir, "logs", "sai.jsonl")
	if cfg.Logging.Path != wantLogPath {
		t.Fatalf("Logging.Path = %q, want %q", cfg.Logging.Path, wantLogPath)
	}

	provider := cfg.Providers["paperhub"]
	if provider.Name != "paperhub" {
		t.Fatalf("provider name = %q, want paperhub", provider.Name)
	}
	if provider.APIKeyEnv != "PAPERHUB_API_KEY" {
		t.Fatalf("APIKeyEnv = %q, want PAPERHUB_API_KEY", provider.APIKeyEnv)
	}
	if got := provider.Models["glm-5.2-fast"].ID; got != "glm-5.2" {
		t.Fatalf("fast profile id = %q, want glm-5.2", got)
	}
	if got := provider.Models["glm-5.2-fast"].Parameters["max_tokens"]; got != 2048 {
		t.Fatalf("fast profile max_tokens = %#v, want 2048", got)
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
api_key_env: PAPERHUB_API_KEY

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
api_key_env: LOCAL_API_KEY

models:
  small:
    id: local-small
`)

	writeFile(t, filepath.Join(providersDir, "ignored.txt"), `name: ignored
`)

	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
