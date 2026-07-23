package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"gopkg.in/yaml.v3"
)

type Config struct {
	ConfigPath      string                     `json:"config_path" yaml:"-"`
	DefaultProvider string                     `json:"default_provider" yaml:"default_provider"`
	DefaultModel    string                     `json:"default_model" yaml:"default_model"`
	ProviderDir     string                     `json:"provider_dir" yaml:"provider_dir"`
	AuthDir         string                     `json:"auth_dir" yaml:"auth_dir"`
	SkillDirs       []string                   `json:"skill_dirs" yaml:"skill_dirs"`
	Subagents       map[string]string          `json:"subagents" yaml:"subagents"`
	Agent           AgentConfig                `json:"agent" yaml:"agent"`
	Prompt          PromptConfig               `json:"prompt" yaml:"prompt"`
	Tools           ToolsConfig                `json:"tools" yaml:"tools"`
	Logging         LoggingConfig              `json:"logging" yaml:"logging"`
	Sessions        SessionsConfig             `json:"sessions" yaml:"sessions"`
	Compaction      CompactionConfig           `json:"compaction" yaml:"compaction"`
	MCPDir          string                     `json:"mcp_dir,omitempty" yaml:"mcp_dir,omitempty"`
	MCPServers      map[string]MCPServerConfig `json:"mcp_servers,omitempty" yaml:"-"`
	Providers       map[string]ProviderConfig  `json:"providers" yaml:"providers"`
}

const (
	ProviderTypeOpenAIChat        = "openai-chat"
	ProviderTypeOpenAIResponses   = "openai-responses"
	ProviderTypeOpenAICodex       = "openai-codex"
	ProviderTypeAnthropicMessages = "anthropic-messages"
)

type AgentConfig struct {
	InstructionFiles []string `json:"instruction_files" yaml:"instruction_files"`
	Name             string   `json:"name,omitempty" yaml:"name,omitempty"`
	Description      string   `json:"description,omitempty" yaml:"description,omitempty"`
	Timeout          string   `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	MaxTurns         int      `json:"max_turns" yaml:"max_turns"`
	Stream           bool     `json:"stream" yaml:"stream"`
	ShowReasoning    bool     `json:"show_reasoning" yaml:"show_reasoning"`
}

type PromptConfig struct {
	SystemPrompt string `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`
}

type ToolsConfig struct {
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

type CompactionConfig struct {
	Enabled          bool   `json:"enabled" yaml:"enabled"`
	ThresholdPercent int    `json:"threshold_percent" yaml:"threshold_percent"`
	SummaryProvider  string `json:"summary_provider" yaml:"summary_provider"`
	SummaryModel     string `json:"summary_model" yaml:"summary_model"`
}

type ProviderConfig struct {
	Name           string                  `json:"name" yaml:"name"`
	BaseURL        string                  `json:"base_url" yaml:"base_url"`
	APIKey         string                  `json:"api_key" yaml:"api_key"`
	AuthFile       string                  `json:"auth_file,omitempty" yaml:"auth_file,omitempty"`
	RequestTimeout string                  `json:"request_timeout,omitempty" yaml:"request_timeout,omitempty"`
	ResolvedAPIKey string                  `json:"-" yaml:"-"`
	Models         map[string]ModelProfile `json:"models" yaml:"models"`
}

type ModelProfile struct {
	ID              string          `json:"id" yaml:"id"`
	Type            string          `json:"type,omitempty" yaml:"type,omitempty"`
	ContextWindow   int             `json:"context_window,omitempty" yaml:"context_window,omitempty"`
	Parameters      map[string]any  `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	ReasoningConfig ReasoningConfig `json:"reasoning_config,omitempty" yaml:"reasoning_config,omitempty"`
}

type ReasoningConfig struct {
	Parameter string         `json:"parameter,omitempty" yaml:"parameter,omitempty"`
	Default   string         `json:"default,omitempty" yaml:"default,omitempty"`
	Levels    map[string]any `json:"levels,omitempty" yaml:"levels,omitempty"`
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
	Type                string
	Parameters          map[string]any
	ContextWindow       int
	ContextWindowSource string
	ReasoningConfig     ReasoningConfig
}

func (p ProviderConfig) MarshalJSON() ([]byte, error) {
	type providerJSON struct {
		Name           string                  `json:"name"`
		BaseURL        string                  `json:"base_url"`
		APIKey         string                  `json:"api_key"`
		AuthFile       string                  `json:"auth_file,omitempty"`
		RequestTimeout string                  `json:"request_timeout,omitempty"`
		Models         map[string]ModelProfile `json:"models"`
	}

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(providerJSON{
		Name:           p.Name,
		BaseURL:        p.BaseURL,
		APIKey:         redactedSecretValue(p.APIKey),
		AuthFile:       p.AuthFile,
		RequestTimeout: p.RequestTimeout,
		Models:         p.Models,
	}); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

func Load(configPath string) (*Config, error) {
	cfg, err := LoadBase(configPath)
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

func LoadBase(configPath string) (*Config, error) {
	if strings.TrimSpace(configPath) == "" {
		return nil, fmt.Errorf("config file is required")
	}

	absConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("resolve config file: %w", err)
	}
	absConfigPath = filepath.Clean(absConfigPath)
	configDir := filepath.Dir(absConfigPath)

	cfg := defaultConfig()
	data, err := os.ReadFile(absConfigPath)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", absConfigPath, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config file %q: %w", absConfigPath, err)
	}
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("validate config file %q: %w", absConfigPath, err)
	}

	cfg.ConfigPath = absConfigPath
	cfg.ProviderDir = resolvePath(configDir, cfg.ProviderDir)
	cfg.AuthDir = resolvePath(configDir, cfg.AuthDir)
	cfg.SkillDirs = resolvePaths(configDir, cfg.SkillDirs)
	cfg.Subagents = resolvePathMap(configDir, cfg.Subagents)
	if cfg.Logging.Path != "" {
		cfg.Logging.Path = resolvePath(configDir, cfg.Logging.Path)
	}
	if cfg.Sessions.Dir != "" {
		cfg.Sessions.Dir = resolvePath(configDir, cfg.Sessions.Dir)
	}
	if cfg.MCPDir != "" {
		cfg.MCPDir = resolvePath(configDir, cfg.MCPDir)
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
	modelType, err := resolveModelType(providerName, modelName, profile.Type)
	if err != nil {
		return ResolvedModel{}, err
	}
	window := contextwindow.ResolveWindow(profile.ContextWindow)

	resolvedProvider := copyProvider(provider)
	if modelType != ProviderTypeOpenAICodex {
		apiKey, err := resolveAPIKey(provider.APIKey)
		if err != nil {
			return ResolvedModel{}, fmt.Errorf("resolve api_key for provider %q: %w", providerName, err)
		}
		resolvedProvider.ResolvedAPIKey = apiKey
	}

	return ResolvedModel{
		ProviderName:        providerName,
		Provider:            resolvedProvider,
		Profile:             modelName,
		ModelID:             profile.ID,
		Type:                modelType,
		Parameters:          copyParameters(profile.Parameters),
		ContextWindow:       window.Tokens,
		ContextWindowSource: string(window.Source),
		ReasoningConfig:     copyReasoningConfig(profile.ReasoningConfig),
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
		profile.ReasoningConfig = copyReasoningConfig(profile.ReasoningConfig)
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

func copyReasoningConfig(reasoning ReasoningConfig) ReasoningConfig {
	reasoning.Levels = copyParameters(reasoning.Levels)
	return reasoning
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
	var structured struct {
		ReasoningConfig ReasoningConfig `yaml:"reasoning_config"`
	}
	if err := value.Decode(&structured); err != nil {
		return err
	}
	m.ReasoningConfig = structured.ReasoningConfig
	delete(fields, "reasoning_config")

	if rawContextWindow, ok := fields["context_window"]; ok {
		contextWindow, err := parseContextWindow(rawContextWindow)
		if err != nil {
			return err
		}
		m.ContextWindow = contextWindow
		delete(fields, "context_window")
	}

	if rawType, ok := fields["type"]; ok {
		switch modelType := rawType.(type) {
		case nil:
			m.Type = ""
		case string:
			m.Type = strings.TrimSpace(modelType)
		default:
			return fmt.Errorf("model profile type must be a string")
		}
		delete(fields, "type")
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

func validateConfig(cfg Config) error {
	if cfg.Compaction.ThresholdPercent <= 0 {
		return fmt.Errorf("compaction.threshold_percent must be positive")
	}
	return nil
}

func defaultConfig() Config {
	return Config{
		ProviderDir: "providers",
		AuthDir:     "auth",
		SkillDirs:   []string{"skills"},
		Agent: AgentConfig{
			InstructionFiles: []string{"$CWD/AGENTS.md"},
			MaxTurns:         defaultAgentMaxTurns,
			Stream:           true,
		},
		Tools: ToolsConfig{
			Enabled: []string{},
		},
		Sessions: SessionsConfig{
			Dir:             "sessions",
			SaveToolResults: true,
		},
		Compaction: CompactionConfig{
			ThresholdPercent: 80,
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
		provider = normalizeProvider(provider, filepath.Dir(path))
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
	if provider.RequestTimeout != "" {
		requestTimeout, err := time.ParseDuration(provider.RequestTimeout)
		if err != nil || requestTimeout <= 0 {
			return fmt.Errorf("provider file %q request_timeout must be a positive duration", path)
		}
	}
	for profileName, profile := range provider.Models {
		if profile.ID == "" {
			return fmt.Errorf("provider file %q model %q is missing id", path, profileName)
		}
		if profile.Type != "" && !isKnownProviderType(profile.Type) {
			return fmt.Errorf("provider file %q model %q has unknown model type %q; supported provider types: %s", path, profileName, profile.Type, formatSupportedProviderTypes())
		}
		if strings.TrimSpace(profile.Type) == ProviderTypeOpenAICodex && strings.TrimSpace(provider.AuthFile) == "" {
			return fmt.Errorf("provider file %q model %q uses %s but auth_file is missing", path, profileName, ProviderTypeOpenAICodex)
		}
		if len(profile.ReasoningConfig.Levels) > 0 && strings.TrimSpace(profile.ReasoningConfig.Parameter) == "" {
			return fmt.Errorf("provider file %q model %q reasoning_config.parameter is required when levels are configured", path, profileName)
		}
		if defaultLevel := strings.TrimSpace(profile.ReasoningConfig.Default); defaultLevel != "" {
			if _, ok := profile.ReasoningConfig.Levels[defaultLevel]; !ok {
				return fmt.Errorf("provider file %q model %q reasoning_config.default %q is not present in levels", path, profileName, defaultLevel)
			}
		}
	}
	return nil
}

func normalizeProvider(provider ProviderConfig, providerFileDir string) ProviderConfig {
	provider.Name = strings.TrimSpace(provider.Name)
	provider.AuthFile = strings.TrimSpace(provider.AuthFile)
	provider.RequestTimeout = strings.TrimSpace(provider.RequestTimeout)
	if provider.AuthFile != "" {
		provider.AuthFile = resolvePath(providerFileDir, provider.AuthFile)
	}
	for profileName, profile := range provider.Models {
		profile.ID = strings.TrimSpace(profile.ID)
		profile.Type = strings.TrimSpace(profile.Type)
		provider.Models[profileName] = profile
	}
	return provider
}

func resolveModelType(providerName, modelName, modelType string) (string, error) {
	modelType = strings.TrimSpace(modelType)
	if modelType == "" {
		return ProviderTypeOpenAIChat, nil
	}
	if !isKnownProviderType(modelType) {
		return "", fmt.Errorf("model %q for provider %q has unknown model type %q; supported provider types: %s", modelName, providerName, modelType, formatSupportedProviderTypes())
	}
	return modelType, nil
}

func isKnownProviderType(providerType string) bool {
	switch providerType {
	case ProviderTypeAnthropicMessages, ProviderTypeOpenAIChat, ProviderTypeOpenAICodex, ProviderTypeOpenAIResponses:
		return true
	default:
		return false
	}
}

func formatSupportedProviderTypes() string {
	return strings.Join([]string{ProviderTypeAnthropicMessages, ProviderTypeOpenAICodex, ProviderTypeOpenAIChat, ProviderTypeOpenAIResponses}, ", ")
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

func resolvePaths(baseDir string, paths []string) []string {
	if paths == nil {
		return nil
	}
	resolved := make([]string, 0, len(paths))
	for _, path := range paths {
		resolved = append(resolved, resolvePath(baseDir, path))
	}
	return resolved
}

func resolvePathMap(baseDir string, paths map[string]string) map[string]string {
	if paths == nil {
		return nil
	}
	resolved := make(map[string]string, len(paths))
	for id, path := range paths {
		resolved[id] = resolvePath(baseDir, path)
	}
	return resolved
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
