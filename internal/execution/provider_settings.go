package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/codexauth"
	"github.com/rexzhao/simple-agent/internal/config"
	"gopkg.in/yaml.v3"
)

type ProviderSettingsDocument struct {
	ProjectID       string             `json:"project_id"`
	ConfigPath      string             `json:"config_path"`
	DefaultProvider string             `json:"default_provider"`
	DefaultModel    string             `json:"default_model"`
	Providers       []ProviderSettings `json:"providers"`
}

type ProviderSettings struct {
	Name             string                  `json:"name"`
	BaseURL          string                  `json:"base_url"`
	APIKey           string                  `json:"api_key,omitempty"`
	APIKeyConfigured bool                    `json:"api_key_configured"`
	AuthFile         string                  `json:"auth_file,omitempty"`
	RequestTimeout   string                  `json:"request_timeout,omitempty"`
	Models           []ProviderModelSettings `json:"models"`
	CodexAuth        *CodexAuthStatus        `json:"codex_auth,omitempty"`
}

type ProviderModelSettings struct {
	Profile         string                 `json:"profile"`
	ID              string                 `json:"id"`
	Type            string                 `json:"type"`
	ContextWindow   int                    `json:"context_window,omitempty"`
	Parameters      map[string]any         `json:"parameters,omitempty"`
	ReasoningConfig config.ReasoningConfig `json:"reasoning_config,omitempty"`
}

type ProviderSettingsInput struct {
	Name           string                  `json:"name"`
	BaseURL        string                  `json:"base_url"`
	APIKey         string                  `json:"api_key,omitempty"`
	KeepAPIKey     bool                    `json:"keep_api_key,omitempty"`
	AuthFile       string                  `json:"auth_file,omitempty"`
	RequestTimeout string                  `json:"request_timeout,omitempty"`
	Models         []ProviderModelSettings `json:"models"`
}

type CodexAuthStatus struct {
	Status      string    `json:"status"`
	AccountID   string    `json:"account_id,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
	Refreshable bool      `json:"refreshable,omitempty"`
	Message     string    `json:"message,omitempty"`
	LoginID     string    `json:"login_id,omitempty"`
	UserCode    string    `json:"user_code,omitempty"`
	VerifyURL   string    `json:"verification_url,omitempty"`
}

func (s *Service) ProviderSettings(projectID string) (ProviderSettingsDocument, error) {
	project, err := s.loadActiveProject(projectID)
	if err != nil {
		return ProviderSettingsDocument{}, err
	}
	configPath := filepath.Join(project.Root, ".agents", "sai.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		return ProviderSettingsDocument{}, err
	}
	document := ProviderSettingsDocument{
		ProjectID:       project.ID,
		ConfigPath:      cfg.ConfigPath,
		DefaultProvider: strings.TrimSpace(cfg.DefaultProvider),
		DefaultModel:    strings.TrimSpace(cfg.DefaultModel),
		Providers:       make([]ProviderSettings, 0, len(cfg.Providers)),
	}
	providerNames := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		providerNames = append(providerNames, name)
	}
	sort.Strings(providerNames)
	for _, name := range providerNames {
		provider := cfg.Providers[name]
		settings := providerSettingsFromConfig(cfg.ProviderDir, provider)
		if providerUsesCodex(provider) {
			status := codexAuthStatus(provider.AuthFile)
			settings.CodexAuth = &status
		}
		document.Providers = append(document.Providers, settings)
	}
	return document, nil
}

func (s *Service) CreateProviderSettings(projectID string, input ProviderSettingsInput) (ProviderSettingsDocument, error) {
	return s.saveProviderSettings(projectID, "", input)
}

func (s *Service) UpdateProviderSettings(projectID, providerName string, input ProviderSettingsInput) (ProviderSettingsDocument, error) {
	return s.saveProviderSettings(projectID, strings.TrimSpace(providerName), input)
}

func (s *Service) saveProviderSettings(projectID, existingName string, input ProviderSettingsInput) (ProviderSettingsDocument, error) {
	project, err := s.loadActiveProject(projectID)
	if err != nil {
		return ProviderSettingsDocument{}, err
	}
	configPath := filepath.Join(project.Root, ".agents", "sai.yaml")
	base, err := config.LoadBase(configPath)
	if err != nil {
		return ProviderSettingsDocument{}, err
	}
	name := strings.TrimSpace(input.Name)
	if err := validateProviderSettingsName(name); err != nil {
		return ProviderSettingsDocument{}, err
	}
	if existingName != "" && existingName != name {
		return ProviderSettingsDocument{}, fmt.Errorf("provider name cannot be changed; create a new provider instead")
	}
	files, err := rawProviderFiles(base.ProviderDir)
	if err != nil {
		return ProviderSettingsDocument{}, err
	}
	existing, exists := files[name]
	if existingName == "" && exists {
		return ProviderSettingsDocument{}, fmt.Errorf("provider %q already exists", name)
	}
	if existingName != "" && !exists {
		return ProviderSettingsDocument{}, fmt.Errorf("provider %q not found", name)
	}
	if len(input.Models) == 0 {
		return ProviderSettingsDocument{}, fmt.Errorf("provider must contain at least one model")
	}
	if strings.TrimSpace(input.BaseURL) == "" {
		return ProviderSettingsDocument{}, fmt.Errorf("provider base_url is required")
	}
	apiKey := strings.TrimSpace(input.APIKey)
	if input.KeepAPIKey && exists {
		apiKey = existing.Provider.APIKey
	}
	authFile := strings.TrimSpace(input.AuthFile)
	if authFile == "" && exists {
		authFile = existing.Provider.AuthFile
	}
	provider := config.ProviderConfig{
		Name:           name,
		BaseURL:        strings.TrimSpace(input.BaseURL),
		APIKey:         apiKey,
		AuthFile:       authFile,
		RequestTimeout: strings.TrimSpace(input.RequestTimeout),
		Models:         make(map[string]config.ModelProfile, len(input.Models)),
	}
	usesCodex := false
	for _, model := range input.Models {
		profile := strings.TrimSpace(model.Profile)
		if profile == "" {
			return ProviderSettingsDocument{}, fmt.Errorf("model profile name is required")
		}
		if _, duplicate := provider.Models[profile]; duplicate {
			return ProviderSettingsDocument{}, fmt.Errorf("duplicate model profile %q", profile)
		}
		modelType := strings.TrimSpace(model.Type)
		usesCodex = usesCodex || modelType == config.ProviderTypeOpenAICodex
		modelProfile := config.ModelProfile{
			ID:              strings.TrimSpace(model.ID),
			Type:            modelType,
			ContextWindow:   model.ContextWindow,
			Parameters:      copyParameterMap(model.Parameters),
			ReasoningConfig: model.ReasoningConfig,
		}
		if len(modelProfile.ReasoningConfig.Levels) == 0 && strings.TrimSpace(modelProfile.ReasoningConfig.Parameter) == "" {
			modelProfile.ReasoningConfig = config.DefaultReasoningConfig(name, provider.BaseURL, modelProfile)
		}
		if modelProfile.Type == config.ProviderTypeAnthropicMessages && len(modelProfile.ReasoningConfig.Levels) > 0 && strings.HasPrefix(modelProfile.ReasoningConfig.Parameter, "output_config.") {
			if modelProfile.Parameters == nil {
				modelProfile.Parameters = make(map[string]any)
			}
			if _, configured := modelProfile.Parameters["thinking"]; !configured {
				modelProfile.Parameters["thinking"] = map[string]any{"type": "adaptive"}
			}
		}
		provider.Models[profile] = modelProfile
	}
	if usesCodex && provider.AuthFile == "" {
		provider.AuthFile = filepath.ToSlash(filepath.Join("..", "auth", name+".json"))
	}
	if provider.AuthFile != "" {
		resolvedAuth := provider.AuthFile
		if !filepath.IsAbs(resolvedAuth) {
			resolvedAuth = filepath.Join(base.ProviderDir, resolvedAuth)
		}
		authRoot := filepath.Join(filepath.Dir(configPath), "auth")
		if !isSameOrAncestorProjectPath(authRoot, resolvedAuth) {
			return ProviderSettingsDocument{}, fmt.Errorf("auth_file must stay inside %q", authRoot)
		}
	}
	path := filepath.Join(base.ProviderDir, name+".yaml")
	if exists {
		path = existing.Path
	}
	if err := config.WriteProviderConfig(path, provider); err != nil {
		return ProviderSettingsDocument{}, err
	}
	return s.ProviderSettings(projectID)
}

func (s *Service) UpdateDefaultProviderModel(projectID, providerName, modelProfile string) (ProviderSettingsDocument, error) {
	project, err := s.loadActiveProject(projectID)
	if err != nil {
		return ProviderSettingsDocument{}, err
	}
	configPath := filepath.Join(project.Root, ".agents", "sai.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		return ProviderSettingsDocument{}, err
	}
	if _, err := cfg.ResolveModel(providerName, modelProfile); err != nil {
		return ProviderSettingsDocument{}, err
	}
	if err := config.UpdateDefaultModel(configPath, strings.TrimSpace(providerName), strings.TrimSpace(modelProfile)); err != nil {
		return ProviderSettingsDocument{}, err
	}
	return s.ProviderSettings(projectID)
}

func (s *Service) CodexAuthStatus(projectID, providerName string) (CodexAuthStatus, error) {
	provider, err := s.codexProvider(projectID, providerName)
	if err != nil {
		return CodexAuthStatus{}, err
	}
	return codexAuthStatus(provider.AuthFile), nil
}

func (s *Service) SaveCodexAuth(projectID, providerName string, token codexauth.TokenFile) error {
	provider, err := s.codexProvider(projectID, providerName)
	if err != nil {
		return err
	}
	return (codexauth.Store{Path: provider.AuthFile}).Save(token)
}

func (s *Service) ClearCodexAuth(projectID, providerName string) error {
	provider, err := s.codexProvider(projectID, providerName)
	if err != nil {
		return err
	}
	if err := os.Remove(provider.AuthFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove Codex auth file: %w", err)
	}
	return nil
}

func (s *Service) DiscoverProviderModels(ctx context.Context, projectID, providerName string) ([]string, error) {
	project, err := s.loadActiveProject(projectID)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(filepath.Join(project.Root, ".agents", "sai.yaml"))
	if err != nil {
		return nil, err
	}
	provider, ok := cfg.Providers[strings.TrimSpace(providerName)]
	if !ok {
		return nil, fmt.Errorf("provider %q not found", providerName)
	}
	if providerUsesCodex(provider) {
		return codexModelIDs(provider), nil
	}
	profiles := make([]string, 0, len(provider.Models))
	for profile := range provider.Models {
		profiles = append(profiles, profile)
	}
	sort.Strings(profiles)
	if len(profiles) == 0 {
		return nil, fmt.Errorf("provider %q has no configured model to determine authentication type", providerName)
	}
	resolved, err := cfg.ResolveModel(providerName, profiles[0])
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(provider.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("create model list request: %w", err)
	}
	if resolved.Type == config.ProviderTypeOpenAICodex {
		token, err := (&codexauth.TokenSource{Store: codexauth.Store{Path: provider.AuthFile}}).AccessToken(ctx)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+token.Token)
		if token.AccountID != "" {
			request.Header.Set("ChatGPT-Account-Id", token.AccountID)
		}
	} else if resolved.Type == config.ProviderTypeAnthropicMessages {
		request.Header.Set("x-api-key", resolved.Provider.ResolvedAPIKey)
		request.Header.Set("anthropic-version", "2023-06-01")
	} else {
		request.Header.Set("Authorization", "Bearer "+resolved.Provider.ResolvedAPIKey)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request provider model list: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("provider model list returned %s", response.Status)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode provider model list: %w", err)
	}
	seen := make(map[string]struct{}, len(payload.Data))
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, id)
	}
	sort.Strings(models)
	return models, nil
}

func codexModelIDs(provider config.ProviderConfig) []string {
	// Pi keeps the Codex catalog explicit instead of depending on a public
	// /models endpoint. Keep the same stable set and include locally configured
	// IDs so custom or older profiles remain selectable.
	known := []string{
		"gpt-5.3-codex-spark",
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-5.5",
		"gpt-5.6-luna",
		"gpt-5.6-sol",
		"gpt-5.6-terra",
	}
	seen := make(map[string]struct{}, len(known)+len(provider.Models))
	models := make([]string, 0, len(known)+len(provider.Models))
	for _, id := range known {
		seen[id] = struct{}{}
		models = append(models, id)
	}
	for _, profile := range provider.Models {
		id := strings.TrimSpace(profile.ID)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, id)
	}
	sort.Strings(models)
	return models
}

type rawProviderFile struct {
	Path     string
	Provider config.ProviderConfig
}

func rawProviderFiles(providerDir string) (map[string]rawProviderFile, error) {
	entries, err := os.ReadDir(providerDir)
	if err != nil {
		return nil, fmt.Errorf("read provider directory %q: %w", providerDir, err)
	}
	result := make(map[string]rawProviderFile)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(providerDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read provider file %q: %w", path, err)
		}
		var provider config.ProviderConfig
		if err := yaml.Unmarshal(data, &provider); err != nil {
			return nil, fmt.Errorf("parse provider file %q: %w", path, err)
		}
		result[strings.TrimSpace(provider.Name)] = rawProviderFile{Path: path, Provider: provider}
	}
	return result, nil
}

func providerSettingsFromConfig(providerDir string, provider config.ProviderConfig) ProviderSettings {
	apiKey := strings.TrimSpace(provider.APIKey)
	visibleAPIKey := apiKey
	if apiKey != "" && !strings.HasPrefix(apiKey, "$") {
		visibleAPIKey = ""
	}
	authFile := ""
	if provider.AuthFile != "" {
		if relative, err := filepath.Rel(providerDir, provider.AuthFile); err == nil {
			authFile = filepath.ToSlash(relative)
		}
	}
	settings := ProviderSettings{
		Name:             provider.Name,
		BaseURL:          provider.BaseURL,
		APIKey:           visibleAPIKey,
		APIKeyConfigured: apiKey != "",
		AuthFile:         authFile,
		RequestTimeout:   provider.RequestTimeout,
		Models:           make([]ProviderModelSettings, 0, len(provider.Models)),
	}
	profiles := make([]string, 0, len(provider.Models))
	for profile := range provider.Models {
		profiles = append(profiles, profile)
	}
	sort.Strings(profiles)
	for _, profile := range profiles {
		model := provider.Models[profile]
		if len(model.ReasoningConfig.Levels) == 0 && strings.TrimSpace(model.ReasoningConfig.Parameter) == "" {
			model.ReasoningConfig = config.DefaultReasoningConfig(provider.Name, provider.BaseURL, model)
		}
		settings.Models = append(settings.Models, ProviderModelSettings{
			Profile:         profile,
			ID:              model.ID,
			Type:            model.Type,
			ContextWindow:   model.ContextWindow,
			Parameters:      copyParameterMap(model.Parameters),
			ReasoningConfig: model.ReasoningConfig,
		})
	}
	return settings
}

func providerUsesCodex(provider config.ProviderConfig) bool {
	for _, model := range provider.Models {
		if model.Type == config.ProviderTypeOpenAICodex {
			return true
		}
	}
	return false
}

func codexAuthStatus(path string) CodexAuthStatus {
	token, err := (codexauth.Store{Path: path}).Load()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CodexAuthStatus{Status: "signed_out"}
		}
		return CodexAuthStatus{Status: "error", Message: err.Error()}
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return CodexAuthStatus{Status: "error", Message: "认证文件中没有 access token"}
	}
	status := "signed_in"
	if !token.ExpiresAt.IsZero() && !token.ExpiresAt.After(time.Now().UTC()) && strings.TrimSpace(token.RefreshToken) == "" {
		status = "expired"
	}
	return CodexAuthStatus{
		Status:      status,
		AccountID:   token.AccountID,
		ExpiresAt:   token.ExpiresAt,
		Refreshable: strings.TrimSpace(token.RefreshToken) != "",
	}
}

func (s *Service) codexProvider(projectID, providerName string) (config.ProviderConfig, error) {
	project, err := s.loadActiveProject(projectID)
	if err != nil {
		return config.ProviderConfig{}, err
	}
	cfg, err := config.Load(filepath.Join(project.Root, ".agents", "sai.yaml"))
	if err != nil {
		return config.ProviderConfig{}, err
	}
	provider, ok := cfg.Providers[strings.TrimSpace(providerName)]
	if !ok {
		return config.ProviderConfig{}, fmt.Errorf("provider %q not found", providerName)
	}
	if !providerUsesCodex(provider) {
		return config.ProviderConfig{}, fmt.Errorf("provider %q does not contain an openai-codex model", providerName)
	}
	if strings.TrimSpace(provider.AuthFile) == "" {
		return config.ProviderConfig{}, fmt.Errorf("provider %q has no auth_file", providerName)
	}
	return provider, nil
}

func validateProviderSettingsName(name string) error {
	if name == "" {
		return fmt.Errorf("provider name is required")
	}
	if name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return fmt.Errorf("provider name must not contain path separators")
	}
	for _, char := range name {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			continue
		}
		return fmt.Errorf("provider name may contain only letters, numbers, '.', '_' and '-'")
	}
	return nil
}
