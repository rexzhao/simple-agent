package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ConfigDir       string                    `json:"config_dir" yaml:"config_dir"`
	DefaultProvider string                    `json:"default_provider" yaml:"default_provider"`
	DefaultModel    string                    `json:"default_model" yaml:"default_model"`
	ProviderDir     string                    `json:"provider_dir" yaml:"provider_dir"`
	Agent           AgentConfig               `json:"agent" yaml:"agent"`
	Tools           ToolsConfig               `json:"tools" yaml:"tools"`
	Logging         LoggingConfig             `json:"logging" yaml:"logging"`
	MCPDir          string                    `json:"mcp_dir,omitempty" yaml:"mcp_dir,omitempty"`
	Providers       map[string]ProviderConfig `json:"providers" yaml:"providers"`
}

type AgentConfig struct {
	MaxTurns      int  `json:"max_turns" yaml:"max_turns"`
	Stream        bool `json:"stream" yaml:"stream"`
	ShowReasoning bool `json:"show_reasoning" yaml:"show_reasoning"`
}

type ToolsConfig struct {
	Enabled []string `json:"enabled" yaml:"enabled"`
}

type LoggingConfig struct {
	Path  string `json:"path" yaml:"path"`
	Level string `json:"level" yaml:"level"`
}

type ProviderConfig struct {
	Name      string                  `json:"name" yaml:"name"`
	Type      string                  `json:"type" yaml:"type"`
	BaseURL   string                  `json:"base_url" yaml:"base_url"`
	APIKeyEnv string                  `json:"api_key_env" yaml:"api_key_env"`
	Models    map[string]ModelProfile `json:"models" yaml:"models"`
}

type ModelProfile struct {
	ID         string         `json:"id" yaml:"id"`
	Parameters map[string]any `json:"parameters,omitempty" yaml:"parameters,omitempty"`
}

type ModelInfo struct {
	Provider string
	Profile  string
	ID       string
}

func Load(configDir string) (*Config, error) {
	if strings.TrimSpace(configDir) == "" {
		return nil, fmt.Errorf("config directory is required")
	}

	absConfigDir, err := filepath.Abs(configDir)
	if err != nil {
		return nil, fmt.Errorf("resolve config directory: %w", err)
	}
	absConfigDir = filepath.Clean(absConfigDir)

	cfg := defaultConfig()
	configPath := filepath.Join(absConfigDir, "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config.yaml: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config.yaml: %w", err)
	}

	cfg.ConfigDir = absConfigDir
	cfg.ProviderDir = resolvePath(absConfigDir, cfg.ProviderDir)
	if cfg.Logging.Path != "" {
		cfg.Logging.Path = resolvePath(absConfigDir, cfg.Logging.Path)
	}
	if cfg.MCPDir != "" {
		cfg.MCPDir = resolvePath(absConfigDir, cfg.MCPDir)
	}

	providers, err := loadProviders(cfg.ProviderDir)
	if err != nil {
		return nil, err
	}
	cfg.Providers = providers
	return &cfg, nil
}

func (c *Config) ModelList() []ModelInfo {
	providerNames := make([]string, 0, len(c.Providers))
	for name := range c.Providers {
		providerNames = append(providerNames, name)
	}
	sort.Strings(providerNames)

	var models []ModelInfo
	for _, providerName := range providerNames {
		provider := c.Providers[providerName]
		profileNames := make([]string, 0, len(provider.Models))
		for profileName := range provider.Models {
			profileNames = append(profileNames, profileName)
		}
		sort.Strings(profileNames)
		for _, profileName := range profileNames {
			models = append(models, ModelInfo{
				Provider: providerName,
				Profile:  profileName,
				ID:       provider.Models[profileName].ID,
			})
		}
	}
	return models
}

func (m *ModelProfile) UnmarshalYAML(value *yaml.Node) error {
	var fields map[string]any
	if err := value.Decode(&fields); err != nil {
		return err
	}

	id, ok := fields["id"]
	if !ok {
		return fmt.Errorf("model profile is missing id")
	}
	idString, ok := id.(string)
	if !ok || idString == "" {
		return fmt.Errorf("model profile id must be a non-empty string")
	}
	delete(fields, "id")

	m.ID = idString
	m.Parameters = fields
	return nil
}

func defaultConfig() Config {
	return Config{
		ProviderDir: "providers",
		Agent: AgentConfig{
			Stream: true,
		},
		Tools: ToolsConfig{
			Enabled: []string{},
		},
		Logging: LoggingConfig{
			Path: "logs/sai.jsonl",
		},
		Providers: map[string]ProviderConfig{},
	}
}

func loadProviders(providerDir string) (map[string]ProviderConfig, error) {
	entries, err := os.ReadDir(providerDir)
	if err != nil {
		return nil, fmt.Errorf("read provider_dir %q: %w", providerDir, err)
	}

	providers := make(map[string]ProviderConfig)
	for _, entry := range entries {
		if entry.IsDir() || !isYAMLFile(entry.Name()) {
			continue
		}

		path := filepath.Join(providerDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read provider file %q: %w", path, err)
		}

		var provider ProviderConfig
		if err := yaml.Unmarshal(data, &provider); err != nil {
			return nil, fmt.Errorf("parse provider file %q: %w", path, err)
		}
		if err := validateProvider(path, provider); err != nil {
			return nil, err
		}
		if _, exists := providers[provider.Name]; exists {
			return nil, fmt.Errorf("duplicate provider name %q", provider.Name)
		}
		providers[provider.Name] = provider
	}
	return providers, nil
}

func validateProvider(path string, provider ProviderConfig) error {
	if provider.Name == "" {
		return fmt.Errorf("provider file %q is missing name", path)
	}
	for profileName, profile := range provider.Models {
		if profile.ID == "" {
			return fmt.Errorf("provider file %q model %q is missing id", path, profileName)
		}
	}
	return nil
}

func isYAMLFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

func resolvePath(baseDir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(baseDir, path))
}
