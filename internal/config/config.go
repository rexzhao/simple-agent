package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"gopkg.in/yaml.v3"
)

type Config struct {
	ConfigDir       string                     `json:"config_dir" yaml:"config_dir"`
	DefaultProvider string                     `json:"default_provider" yaml:"default_provider"`
	DefaultModel    string                     `json:"default_model" yaml:"default_model"`
	ProviderDir     string                     `json:"provider_dir" yaml:"provider_dir"`
	SkillDir        string                     `json:"skill_dir" yaml:"skill_dir"`
	Agent           AgentConfig                `json:"agent" yaml:"agent"`
	Tools           ToolsConfig                `json:"tools" yaml:"tools"`
	Skills          SkillsConfig               `json:"skills" yaml:"skills"`
	Logging         LoggingConfig              `json:"logging" yaml:"logging"`
	Sessions        SessionsConfig             `json:"sessions" yaml:"sessions"`
	MCPDir          string                     `json:"mcp_dir,omitempty" yaml:"mcp_dir,omitempty"`
	MCPServers      map[string]MCPServerConfig `json:"mcp_servers,omitempty" yaml:"-"`
	Providers       map[string]ProviderConfig  `json:"providers" yaml:"providers"`
}

const (
	ProviderTypeOpenAIChat        = "openai-chat"
	ProviderTypeOpenAIResponses   = "openai-responses"
	ProviderTypeAnthropicMessages = "anthropic-messages"
)

type AgentConfig struct {
	MaxTurns      int  `json:"max_turns" yaml:"max_turns"`
	Stream        bool `json:"stream" yaml:"stream"`
	ShowReasoning bool `json:"show_reasoning" yaml:"show_reasoning"`
}

type ToolsConfig struct {
	Enabled []string `json:"enabled" yaml:"enabled"`
}

type SkillsConfig struct {
	Enabled []string `json:"enabled" yaml:"enabled"`
}

type LoggingConfig struct {
	Path  string `json:"path" yaml:"path"`
	Level string `json:"level" yaml:"level"`
}

type SessionsConfig struct {
	Enabled         bool   `json:"enabled" yaml:"enabled"`
	Dir             string `json:"dir" yaml:"dir"`
	SaveToolResults bool   `json:"save_tool_results" yaml:"save_tool_results"`
}

type ProviderConfig struct {
	Name           string                  `json:"name" yaml:"name"`
	Type           string                  `json:"type" yaml:"type"`
	BaseURL        string                  `json:"base_url" yaml:"base_url"`
	APIKey         string                  `json:"api_key" yaml:"api_key"`
	ResolvedAPIKey string                  `json:"-" yaml:"-"`
	Models         map[string]ModelProfile `json:"models" yaml:"models"`
}

type ModelProfile struct {
	ID            string         `json:"id" yaml:"id"`
	ContextWindow int            `json:"context_window,omitempty" yaml:"context_window,omitempty"`
	Parameters    map[string]any `json:"parameters,omitempty" yaml:"parameters,omitempty"`
}

type ModelInfo struct {
	Provider string
	Profile  string
	ID       string
}

type ResolvedModel struct {
	ProviderName        string
	Provider            ProviderConfig
	Profile             string
	ModelID             string
	Parameters          map[string]any
	ContextWindow       int
	ContextWindowSource string
}

func (p ProviderConfig) MarshalJSON() ([]byte, error) {
	type providerJSON struct {
		Name    string                  `json:"name"`
		Type    string                  `json:"type"`
		BaseURL string                  `json:"base_url"`
		APIKey  string                  `json:"api_key"`
		Models  map[string]ModelProfile `json:"models"`
	}

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(providerJSON{
		Name:    p.Name,
		Type:    p.Type,
		BaseURL: p.BaseURL,
		APIKey:  redactedSecretValue(p.APIKey),
		Models:  p.Models,
	}); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

func Load(configDir string) (*Config, error) {
	cfg, err := LoadBase(configDir)
	if err != nil {
		return nil, err
	}

	providers, err := LoadProviderConfigs(cfg.ProviderDir)
	if err != nil {
		return nil, err
	}
	cfg.Providers = providers

	mcpServers, err := LoadMCPServerConfigs(cfg.MCPDir)
	if err != nil {
		return nil, err
	}
	cfg.MCPServers = mcpServers
	return cfg, nil
}

func LoadBase(configDir string) (*Config, error) {
	if strings.TrimSpace(configDir) == "" {
		return nil, fmt.Errorf("config directory is required")
	}

	absConfigDir, err := filepath.Abs(configDir)
	if err != nil {
		return nil, fmt.Errorf("resolve config directory: %w", err)
	}
	absConfigDir = filepath.Clean(absConfigDir)

	cfg := defaultConfig()
	configPath := filepath.Join(absConfigDir, "sai.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read sai.yaml: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse sai.yaml: %w", err)
	}

	cfg.ConfigDir = absConfigDir
	cfg.ProviderDir = resolvePath(absConfigDir, cfg.ProviderDir)
	cfg.SkillDir = resolvePath(absConfigDir, cfg.SkillDir)
	if cfg.Logging.Path != "" {
		cfg.Logging.Path = resolvePath(absConfigDir, cfg.Logging.Path)
	}
	if cfg.Sessions.Dir != "" {
		cfg.Sessions.Dir = resolvePath(absConfigDir, cfg.Sessions.Dir)
	}
	if cfg.MCPDir != "" {
		cfg.MCPDir = resolvePath(absConfigDir, cfg.MCPDir)
	}

	return &cfg, nil
}

func (c *Config) ResolveModel(providerName, modelName string) (ResolvedModel, error) {
	providerName = strings.TrimSpace(providerName)
	modelName = strings.TrimSpace(modelName)
	if providerName == "" {
		providerName = strings.TrimSpace(c.DefaultProvider)
	}
	if modelName == "" {
		modelName = strings.TrimSpace(c.DefaultModel)
	}

	if providerName == "" && modelName == "" {
		return ResolvedModel{}, fmt.Errorf("provider and model are required; set default_provider/default_model or pass provider/model; available models: %s", c.formatModelChoices())
	}
	if providerName == "" {
		return ResolvedModel{}, fmt.Errorf("provider is required; set default_provider or pass provider; available providers: %s", c.formatProviderChoices())
	}

	provider, ok := c.Providers[providerName]
	if !ok {
		return ResolvedModel{}, fmt.Errorf("unknown provider %q; available providers: %s", providerName, c.formatProviderChoices())
	}
	if modelName == "" {
		return ResolvedModel{}, fmt.Errorf("model is required for provider %q; set default_model or pass model; available models: %s", providerName, formatProviderModelChoices(provider))
	}

	profile, ok := provider.Models[modelName]
	if !ok {
		return ResolvedModel{}, fmt.Errorf("unknown model %q for provider %q; available models: %s", modelName, providerName, formatProviderModelChoices(provider))
	}
	window := contextwindow.ResolveWindow(profile.ContextWindow)

	resolvedProvider := copyProvider(provider)
	apiKey, err := resolveAPIKey(provider.APIKey)
	if err != nil {
		return ResolvedModel{}, fmt.Errorf("resolve api_key for provider %q: %w", providerName, err)
	}
	resolvedProvider.ResolvedAPIKey = apiKey

	return ResolvedModel{
		ProviderName:        providerName,
		Provider:            resolvedProvider,
		Profile:             modelName,
		ModelID:             profile.ID,
		Parameters:          copyParameters(profile.Parameters),
		ContextWindow:       window.Tokens,
		ContextWindowSource: string(window.Source),
	}, nil
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

func (c *Config) formatProviderChoices() string {
	names := make([]string, 0, len(c.Providers))
	for name := range c.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

func (c *Config) formatModelChoices() string {
	models := c.ModelList()
	if len(models) == 0 {
		return "(none)"
	}

	choices := make([]string, 0, len(models))
	for _, model := range models {
		choices = append(choices, fmt.Sprintf("%s/%s (id %s)", model.Provider, model.Profile, model.ID))
	}
	return strings.Join(choices, ", ")
}

func formatProviderModelChoices(provider ProviderConfig) string {
	names := make([]string, 0, len(provider.Models))
	for name := range provider.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(none)"
	}

	choices := make([]string, 0, len(names))
	for _, name := range names {
		choices = append(choices, fmt.Sprintf("%s (id %s)", name, provider.Models[name].ID))
	}
	return strings.Join(choices, ", ")
}

func copyProvider(provider ProviderConfig) ProviderConfig {
	provider.Models = copyModels(provider.Models)
	return provider
}

func copyModels(models map[string]ModelProfile) map[string]ModelProfile {
	if models == nil {
		return nil
	}

	copied := make(map[string]ModelProfile, len(models))
	for name, profile := range models {
		profile.Parameters = copyParameters(profile.Parameters)
		copied[name] = profile
	}
	return copied
}

func copyParameters(parameters map[string]any) map[string]any {
	if parameters == nil {
		return nil
	}

	copied := make(map[string]any, len(parameters))
	for name, value := range parameters {
		copied[name] = value
	}
	return copied
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

	if rawContextWindow, ok := fields["context_window"]; ok {
		contextWindow, err := parseContextWindow(rawContextWindow)
		if err != nil {
			return err
		}
		m.ContextWindow = contextWindow
		delete(fields, "context_window")
	}

	parameters := map[string]any{}
	if rawParameters, ok := fields["parameters"]; ok {
		decoded, ok := rawParameters.(map[string]any)
		if !ok {
			return fmt.Errorf("model profile parameters must be a map")
		}
		for key, value := range decoded {
			parameters[key] = value
		}
		delete(fields, "parameters")
	}
	for key, value := range fields {
		parameters[key] = value
	}

	m.ID = idString
	m.Parameters = parameters
	return nil
}

func parseContextWindow(value any) (int, error) {
	tokens, ok := value.(int)
	if !ok {
		return 0, fmt.Errorf("model profile context_window must be a positive integer")
	}
	if tokens <= 0 {
		return 0, fmt.Errorf("model profile context_window must be a positive integer")
	}
	return tokens, nil
}

func defaultConfig() Config {
	return Config{
		ProviderDir: "providers",
		SkillDir:    "skills",
		Agent: AgentConfig{
			MaxTurns: defaultAgentMaxTurns,
			Stream:   true,
		},
		Tools: ToolsConfig{
			Enabled: []string{},
		},
		Skills: SkillsConfig{
			Enabled: []string{},
		},
		Logging: LoggingConfig{
			Path: "logs/sai.jsonl",
		},
		Sessions: SessionsConfig{
			Dir:             "sessions",
			SaveToolResults: true,
		},
		MCPDir:     "mcp",
		Providers:  map[string]ProviderConfig{},
		MCPServers: map[string]MCPServerConfig{},
	}
}

const defaultAgentMaxTurns = 8

func LoadProviderConfigs(providerDir string) (map[string]ProviderConfig, error) {
	return loadProviders(providerDir)
}

func LoadMCPServerConfigs(mcpDir string) (map[string]MCPServerConfig, error) {
	return loadMCPServers(mcpDir)
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
		provider.Name = strings.TrimSpace(provider.Name)
		provider.Type = strings.TrimSpace(provider.Type)
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
	if provider.Type == "" {
		return fmt.Errorf("provider file %q provider %q is missing type", path, provider.Name)
	}
	if !isKnownProviderType(provider.Type) {
		return fmt.Errorf("provider file %q provider %q has unknown provider type %q; supported provider types: %s", path, provider.Name, provider.Type, formatSupportedProviderTypes())
	}
	for profileName, profile := range provider.Models {
		if profile.ID == "" {
			return fmt.Errorf("provider file %q model %q is missing id", path, profileName)
		}
	}
	return nil
}

func isKnownProviderType(providerType string) bool {
	switch providerType {
	case ProviderTypeAnthropicMessages, ProviderTypeOpenAIChat, ProviderTypeOpenAIResponses:
		return true
	default:
		return false
	}
}

func formatSupportedProviderTypes() string {
	return strings.Join([]string{ProviderTypeAnthropicMessages, ProviderTypeOpenAIChat, ProviderTypeOpenAIResponses}, ", ")
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

func resolveAPIKey(value string) (string, error) {
	return resolveSensitiveValue("API key", value, os.LookupEnv)
}

func resolveSensitiveValue(name, value string, lookup func(string) (string, bool)) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if !strings.HasPrefix(value, "$") {
		return value, nil
	}

	envName := strings.TrimPrefix(value, "$")
	if strings.TrimSpace(envName) == "" {
		return "", fmt.Errorf("%s environment variable name is required", name)
	}
	resolved, ok := lookup(envName)
	if !ok || strings.TrimSpace(resolved) == "" {
		return "", fmt.Errorf("%s environment variable %q is not set", name, envName)
	}
	return resolved, nil
}

func redactedSecretValue(value string) string {
	if value == "" || strings.HasPrefix(value, "$") {
		return value
	}
	return "<redacted>"
}
