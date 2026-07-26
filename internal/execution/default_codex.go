package execution

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rexzhao/simple-agent/internal/codexauth"
	"github.com/rexzhao/simple-agent/internal/config"
)

const (
	defaultCodexProviderName = "codex"
	defaultCodexProfileName  = "default"
	defaultCodexTimeout      = "60s"
	defaultCodexContext      = 400000
	defaultCodexInputLimit   = 272000
	defaultCodexOutputLimit  = 128000
)

// ensureDefaultCodexProvider gives a new or upgraded Server Root a ready to
// sign in Codex profile. Existing providers and a configured default model are
// never replaced.
func ensureDefaultCodexProvider(configPath string) error {
	base, err := config.LoadBase(configPath)
	if err != nil {
		return err
	}
	providers, err := config.LoadProviderConfigs(base.ProviderDir)
	if err != nil {
		return err
	}

	codexProvider, exists := providers[defaultCodexProviderName]
	if !exists {
		codexProvider, err = newDefaultCodexProvider(base)
		if err != nil {
			return err
		}
		path := filepath.Join(base.ProviderDir, defaultCodexProviderName+".yaml")
		if err := config.WriteProviderConfig(path, codexProvider); err != nil {
			return fmt.Errorf("create default Codex provider: %w", err)
		}
	}

	if strings.TrimSpace(base.DefaultProvider) != "" && strings.TrimSpace(base.DefaultModel) != "" {
		return nil
	}
	profile := preferredCodexProfile(codexProvider)
	if profile == "" {
		// Preserve a user-owned provider named "codex" even when it is not a
		// usable Codex provider; the settings UI can still be used to repair it.
		return nil
	}
	if err := config.UpdateDefaultModel(base.ConfigPath, defaultCodexProviderName, profile); err != nil {
		return fmt.Errorf("set default Codex model: %w", err)
	}
	return nil
}

func newDefaultCodexProvider(base *config.Config) (config.ProviderConfig, error) {
	authPath := filepath.Join(base.AuthDir, defaultCodexProviderName+".json")
	relativeAuthPath, err := filepath.Rel(base.ProviderDir, authPath)
	if err != nil {
		return config.ProviderConfig{}, fmt.Errorf("resolve default Codex auth path: %w", err)
	}
	profile := config.ModelProfile{
		ID:            codexauth.DefaultModelID(),
		Type:          config.ProviderTypeOpenAICodex,
		Input:         []string{"text", "image"},
		ContextWindow: defaultCodexContext,
		InputLimit:    defaultCodexInputLimit,
		OutputLimit:   defaultCodexOutputLimit,
		Parameters: map[string]any{
			"responses": map[string]any{
				"compaction": map[string]any{"mode": "responses-compact"},
			},
		},
	}
	profile.ReasoningConfig = config.DefaultReasoningConfig(defaultCodexProviderName, codexauth.DefaultBaseURL, profile)
	return config.ProviderConfig{
		Name:           defaultCodexProviderName,
		BaseURL:        codexauth.DefaultBaseURL,
		AuthFile:       filepath.ToSlash(relativeAuthPath),
		RequestTimeout: defaultCodexTimeout,
		Models: map[string]config.ModelProfile{
			defaultCodexProfileName: profile,
		},
	}, nil
}

func preferredCodexProfile(provider config.ProviderConfig) string {
	if profile, ok := provider.Models[defaultCodexProfileName]; ok && profile.Type == config.ProviderTypeOpenAICodex {
		return defaultCodexProfileName
	}
	profiles := make([]string, 0, len(provider.Models))
	for name, profile := range provider.Models {
		if profile.Type == config.ProviderTypeOpenAICodex {
			profiles = append(profiles, name)
		}
	}
	sort.Strings(profiles)
	if len(profiles) == 0 {
		return ""
	}
	return profiles[0]
}
