package execution

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/rexzhao/simple-agent/internal/codexauth"
	"github.com/rexzhao/simple-agent/internal/config"
)

func TestNewServiceWithAgentRunnerCreatesReadyToSignInCodexProvider(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "sai.yaml")
	service, err := NewServiceWithAgentRunner(root, configPath)
	if err != nil {
		t.Fatalf("NewServiceWithAgentRunner() error = %v", err)
	}

	document, err := service.ProviderSettings()
	if err != nil {
		t.Fatalf("ProviderSettings() error = %v", err)
	}
	if document.DefaultProvider != defaultCodexProviderName || document.DefaultModel != defaultCodexProfileName {
		t.Fatalf("defaults = %q/%q, want codex/default", document.DefaultProvider, document.DefaultModel)
	}
	if len(document.Providers) != 1 {
		t.Fatalf("providers = %#v, want one default Codex provider", document.Providers)
	}
	provider := document.Providers[0]
	if provider.Name != defaultCodexProviderName || provider.BaseURL != codexauth.DefaultBaseURL {
		t.Fatalf("provider = %#v, want default Codex endpoint", provider)
	}
	if provider.AuthFile != "../auth/codex.json" || provider.RequestTimeout != defaultCodexTimeout {
		t.Fatalf("auth/timeout = %q/%q, want ../auth/codex.json and 60s", provider.AuthFile, provider.RequestTimeout)
	}
	if provider.CodexAuth == nil || provider.CodexAuth.Status != "signed_out" {
		t.Fatalf("Codex auth = %#v, want signed_out", provider.CodexAuth)
	}
	if len(provider.Models) != 1 {
		t.Fatalf("models = %#v, want one default model", provider.Models)
	}
	model := provider.Models[0]
	if model.Profile != defaultCodexProfileName || model.ID != codexauth.DefaultModelID() || model.Type != config.ProviderTypeOpenAICodex {
		t.Fatalf("model identity = %#v, want default/%s/openai-codex", model, codexauth.DefaultModelID())
	}
	if !reflect.DeepEqual(model.Input, []string{"text", "image"}) {
		t.Fatalf("model input = %#v, want text/image", model.Input)
	}
	if model.ContextWindow != defaultCodexContext || model.InputLimit != defaultCodexInputLimit || model.OutputLimit != defaultCodexOutputLimit {
		t.Fatalf("model limits = %d/%d/%d, want %d/%d/%d", model.ContextWindow, model.InputLimit, model.OutputLimit, defaultCodexContext, defaultCodexInputLimit, defaultCodexOutputLimit)
	}
	if model.ReasoningConfig.Parameter != "reasoning.effort" || model.ReasoningConfig.Default != "xhigh" {
		t.Fatalf("reasoning config = %#v, want reasoning.effort/xhigh", model.ReasoningConfig)
	}
	responses, ok := model.Parameters["responses"].(map[string]any)
	if !ok {
		t.Fatalf("responses parameters = %#v, want map", model.Parameters["responses"])
	}
	compaction, ok := responses["compaction"].(map[string]any)
	if !ok || compaction["mode"] != "responses-compact" {
		t.Fatalf("compaction parameters = %#v, want responses-compact", responses["compaction"])
	}
}

func TestDefaultCodexProvisioningPreservesExistingProviderAndDefaults(t *testing.T) {
	root := t.TempDir()
	providersDir := filepath.Join(root, "providers")
	if err := os.MkdirAll(providersDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(providers) error = %v", err)
	}
	configPath := filepath.Join(root, "sai.yaml")
	if err := os.WriteFile(configPath, []byte("default_provider: existing\ndefault_model: primary\nprovider_dir: providers\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	existingPath := filepath.Join(providersDir, "existing.yaml")
	if err := os.WriteFile(existingPath, []byte("name: existing\nbase_url: https://example.test/v1\napi_key: test-key\nmodels:\n  primary:\n    id: existing-model\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(provider) error = %v", err)
	}

	if _, err := NewServiceWithAgentRunner(root, configPath); err != nil {
		t.Fatalf("NewServiceWithAgentRunner() error = %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DefaultProvider != "existing" || cfg.DefaultModel != "primary" {
		t.Fatalf("defaults = %q/%q, want existing/primary preserved", cfg.DefaultProvider, cfg.DefaultModel)
	}
	if cfg.Providers["existing"].Models["primary"].ID != "existing-model" {
		t.Fatalf("existing provider was changed: %#v", cfg.Providers["existing"])
	}
	if _, ok := cfg.Providers[defaultCodexProviderName]; !ok {
		t.Fatal("default Codex provider was not added")
	}
}

func TestDefaultCodexProvisioningDoesNotOverwriteNamedCodexProvider(t *testing.T) {
	root := t.TempDir()
	providersDir := filepath.Join(root, "providers")
	if err := os.MkdirAll(providersDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(providers) error = %v", err)
	}
	configPath := filepath.Join(root, "sai.yaml")
	if err := os.WriteFile(configPath, []byte("provider_dir: providers\nauth_dir: auth\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	codexPath := filepath.Join(providersDir, "team-codex.yaml")
	providerYAML := "name: codex\nbase_url: https://team.example.test/codex\nauth_file: ../auth/team.json\nmodels:\n  team:\n    id: gpt-5.4\n    type: openai-codex\n"
	if err := os.WriteFile(codexPath, []byte(providerYAML), 0o600); err != nil {
		t.Fatalf("WriteFile(provider) error = %v", err)
	}

	if _, err := NewServiceWithAgentRunner(root, configPath); err != nil {
		t.Fatalf("NewServiceWithAgentRunner() error = %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DefaultProvider != defaultCodexProviderName || cfg.DefaultModel != "team" {
		t.Fatalf("defaults = %q/%q, want codex/team", cfg.DefaultProvider, cfg.DefaultModel)
	}
	provider := cfg.Providers[defaultCodexProviderName]
	if provider.BaseURL != "https://team.example.test/codex" || provider.Models["team"].ID != "gpt-5.4" {
		t.Fatalf("named Codex provider was overwritten: %#v", provider)
	}
	if _, err := os.Stat(filepath.Join(providersDir, "codex.yaml")); !os.IsNotExist(err) {
		t.Fatalf("Stat(auto codex.yaml) error = %v, want file not created", err)
	}
}
