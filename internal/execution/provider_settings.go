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
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/rexzhao/simple-agent/internal/codexauth"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/providersettings"
	"gopkg.in/yaml.v3"
)

type ProviderSettingsDocument struct {
	ServerRoot      string             `json:"server_root"`
	ConfigPath      string             `json:"config_path"`
	DefaultProvider string             `json:"default_provider"`
	DefaultModel    string             `json:"default_model"`
	Providers       []ProviderSettings `json:"providers"`
}

type ProviderSettings struct {
	Name                  string                  `json:"name"`
	BaseURL               string                  `json:"base_url"`
	APIKey                string                  `json:"api_key,omitempty"`
	APIKeyConfigured      bool                    `json:"api_key_configured"`
	AuthFile              string                  `json:"auth_file,omitempty"`
	RequestTimeout        string                  `json:"request_timeout,omitempty"`
	HTTPProxy             string                  `json:"http_proxy,omitempty"`
	HTTPSProxy            string                  `json:"https_proxy,omitempty"`
	MaxConcurrentRequests int                     `json:"max_concurrent_requests,omitempty"`
	Models                []ProviderModelSettings `json:"models"`
	CodexAuth             *CodexAuthStatus        `json:"codex_auth,omitempty"`
}

type ProviderModelSettings struct {
	Profile         string                 `json:"profile"`
	ID              string                 `json:"id"`
	Type            string                 `json:"type"`
	Compatibility   string                 `json:"compatibility,omitempty"`
	Input           []string               `json:"input,omitempty"`
	DeveloperRole   string                 `json:"developer_role,omitempty"`
	ContextWindow   int                    `json:"context_window,omitempty"`
	InputLimit      int                    `json:"input_limit,omitempty"`
	OutputLimit     int                    `json:"output_limit,omitempty"`
	Parameters      map[string]any         `json:"parameters,omitempty"`
	ReasoningConfig config.ReasoningConfig `json:"reasoning_config,omitempty"`
	Pricing         *config.ModelPricing   `json:"pricing,omitempty"`
}

type ProviderSettingsInput struct {
	Name                  string                  `json:"name"`
	BaseURL               string                  `json:"base_url"`
	APIKey                string                  `json:"api_key,omitempty"`
	KeepAPIKey            bool                    `json:"keep_api_key,omitempty"`
	AuthFile              string                  `json:"auth_file,omitempty"`
	RequestTimeout        string                  `json:"request_timeout,omitempty"`
	HTTPProxy             string                  `json:"http_proxy,omitempty"`
	HTTPSProxy            string                  `json:"https_proxy,omitempty"`
	MaxConcurrentRequests int                     `json:"max_concurrent_requests,omitempty"`
	Models                []ProviderModelSettings `json:"models"`
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

// These errors are intentionally small and stable.  Callers which expose the
// Codex login command/resource must not forward the detailed configuration or
// filesystem errors returned by the lower-level provider helpers.
var (
	ErrCodexProviderNotFound   = errors.New("codex provider was not found")
	ErrCodexProviderNotCodex   = errors.New("provider is not configured for Codex")
	ErrCodexProviderNoAuthFile = errors.New("Codex provider has no auth file")
)

func (s *Service) ProviderSettings() (ProviderSettingsDocument, error) {
	configPath := s.ConfigPath()
	cfg, err := config.Load(configPath)
	if err != nil {
		return ProviderSettingsDocument{}, err
	}
	document := ProviderSettingsDocument{
		ServerRoot:      s.ServerRoot(),
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

func (s *Service) CreateProviderSettings(input ProviderSettingsInput) (ProviderSettingsDocument, error) {
	return s.saveProviderSettings("", input)
}

func (s *Service) UpdateProviderSettings(providerName string, input ProviderSettingsInput) (ProviderSettingsDocument, error) {
	return s.saveProviderSettings(providerName, input)
}

func (s *Service) saveProviderSettings(existingName string, input ProviderSettingsInput) (ProviderSettingsDocument, error) {
	s.providerConfigMu.Lock()
	defer s.providerConfigMu.Unlock()

	configPath := s.ConfigPath()
	base, err := config.LoadBase(configPath)
	if err != nil {
		return ProviderSettingsDocument{}, err
	}
	name := input.Name
	if err := config.ValidateProviderName(name); err != nil {
		return ProviderSettingsDocument{}, fmt.Errorf("provider name: %w", err)
	}
	if existingName != "" {
		if err := config.ValidateProviderName(existingName); err != nil {
			return ProviderSettingsDocument{}, fmt.Errorf("existing provider name: %w", err)
		}
	} else if err := validateProviderSettingsCreateFilename(name); err != nil {
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
		Name:                  name,
		BaseURL:               strings.TrimSpace(input.BaseURL),
		APIKey:                apiKey,
		AuthFile:              authFile,
		RequestTimeout:        strings.TrimSpace(input.RequestTimeout),
		HTTPProxy:             strings.TrimSpace(input.HTTPProxy),
		HTTPSProxy:            strings.TrimSpace(input.HTTPSProxy),
		MaxConcurrentRequests: input.MaxConcurrentRequests,
		Models:                make(map[string]config.ModelProfile, len(input.Models)),
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
		compatibility, err := config.NormalizeModelCompatibility(model.Compatibility)
		if err != nil {
			return ProviderSettingsDocument{}, fmt.Errorf("model profile %q: %w", profile, err)
		}
		if compatibility != "" && modelType != "" && modelType != config.ProviderTypeOpenAIChat {
			return ProviderSettingsDocument{}, fmt.Errorf("model profile %q: compatibility is only supported for %s models", profile, config.ProviderTypeOpenAIChat)
		}
		modelInput := append([]string(nil), model.Input...)
		if len(modelInput) > 0 {
			modelInput, err = config.NormalizeModelInput(modelInput)
			if err != nil {
				return ProviderSettingsDocument{}, fmt.Errorf("model profile %q: %w", profile, err)
			}
		}
		developerRole, err := config.NormalizeDeveloperRole(model.DeveloperRole)
		if err != nil {
			return ProviderSettingsDocument{}, fmt.Errorf("model profile %q: %w", profile, err)
		}
		modelProfile := config.ModelProfile{
			ID:              strings.TrimSpace(model.ID),
			Type:            modelType,
			Compatibility:   compatibility,
			Input:           modelInput,
			DeveloperRole:   developerRole,
			ContextWindow:   model.ContextWindow,
			InputLimit:      model.InputLimit,
			OutputLimit:     model.OutputLimit,
			Parameters:      copyParameterMap(model.Parameters),
			ReasoningConfig: model.ReasoningConfig,
			Pricing:         copyModelPricing(model.Pricing),
		}
		if modelProfile.Pricing != nil {
			if err := modelProfile.Pricing.Validate(); err != nil {
				return ProviderSettingsDocument{}, fmt.Errorf("model profile %q: %w", profile, err)
			}
			if strings.TrimSpace(modelProfile.Pricing.Currency) == "" {
				modelProfile.Pricing.Currency = "USD"
			}
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
		authPath := filepath.Join(base.AuthDir, name+".json")
		relative, err := filepath.Rel(base.ProviderDir, authPath)
		if err != nil {
			return ProviderSettingsDocument{}, fmt.Errorf("resolve provider auth path: %w", err)
		}
		provider.AuthFile = filepath.ToSlash(relative)
	}
	if provider.AuthFile != "" {
		resolvedAuth := provider.AuthFile
		if !filepath.IsAbs(resolvedAuth) {
			resolvedAuth = filepath.Join(base.ProviderDir, resolvedAuth)
		}
		authRoot := base.AuthDir
		if !isSameOrAncestorProjectPath(authRoot, resolvedAuth) {
			return ProviderSettingsDocument{}, fmt.Errorf("auth_file must stay inside %q", authRoot)
		}
	}
	// This is a target-state operation, not a write-intent operation. A
	// cross-epoch retry may arrive after the original command has committed;
	// avoid rewriting the file or publishing a second resource change when the
	// durable provider already equals the requested target. The comparison is
	// confined to this execution boundary and never serializes credentials.
	if exists && providerConfigsEqual(existing.Provider, provider) {
		return s.ProviderSettings()
	}
	path := filepath.Join(base.ProviderDir, name+".yaml")
	if exists {
		path = existing.Path
	}
	if err := config.WriteProviderConfig(path, provider); err != nil {
		return ProviderSettingsDocument{}, err
	}
	document, readErr := s.ProviderSettings()
	// The provider file is durable at this point. Publish only the typed
	// identity; the WebSocket projection reloads the safe snapshot from the
	// same config authority and never receives this write DTO.
	s.publishProviderSettingsChange(providersettings.CommittedChange{
		Kind: providersettings.CommittedProviderUpsert, ProviderName: name,
	})
	return document, readErr
}

func (s *Service) UpdateDefaultProviderModel(providerName, modelProfile string) (ProviderSettingsDocument, error) {
	s.providerConfigMu.Lock()
	defer s.providerConfigMu.Unlock()

	configPath := s.ConfigPath()
	cfg, err := config.Load(configPath)
	if err != nil {
		return ProviderSettingsDocument{}, err
	}
	providerName = strings.TrimSpace(providerName)
	modelProfile = strings.TrimSpace(modelProfile)
	if _, err := cfg.ResolveModel(providerName, modelProfile); err != nil {
		return ProviderSettingsDocument{}, err
	}
	if strings.TrimSpace(cfg.DefaultProvider) == providerName && strings.TrimSpace(cfg.DefaultModel) == modelProfile {
		return s.ProviderSettings()
	}
	if err := config.UpdateDefaultModel(configPath, strings.TrimSpace(providerName), strings.TrimSpace(modelProfile)); err != nil {
		return ProviderSettingsDocument{}, err
	}
	document, readErr := s.ProviderSettings()
	s.publishProviderSettingsChange(providersettings.CommittedChange{
		Kind:            providersettings.CommittedDefaultChanged,
		DefaultProvider: strings.TrimSpace(providerName), DefaultModel: strings.TrimSpace(modelProfile),
	})
	return document, readErr
}

func (s *Service) CodexAuthStatus(providerName string) (CodexAuthStatus, error) {
	provider, err := s.codexProvider(providerName)
	if err != nil {
		return CodexAuthStatus{}, err
	}
	return codexAuthStatus(provider.AuthFile), nil
}

// ValidateCodexProvider checks only provider identity/configuration.  It does
// not read the auth file and never returns credentials.  The detailed
// codexProvider helper remains the compatibility path for the existing REST
// handlers; this method is the bounded command/resource boundary.
func (s *Service) ValidateCodexProvider(providerName string) error {
	cfg, err := config.Load(s.ConfigPath())
	if err != nil {
		return err
	}
	provider, ok := cfg.Providers[strings.TrimSpace(providerName)]
	if !ok {
		return ErrCodexProviderNotFound
	}
	if !providerUsesCodex(provider) {
		return ErrCodexProviderNotCodex
	}
	if strings.TrimSpace(provider.AuthFile) == "" {
		return ErrCodexProviderNoAuthFile
	}
	return nil
}

func (s *Service) SaveCodexAuth(providerName string, token codexauth.TokenFile) error {
	provider, err := s.codexProvider(providerName)
	if err != nil {
		return err
	}
	return (codexauth.Store{Path: provider.AuthFile}).Save(token)
}

func (s *Service) StartCodexDeviceLogin(ctx context.Context, providerName string) (codexauth.PendingDeviceLogin, error) {
	provider, err := s.codexProvider(providerName)
	if err != nil {
		return codexauth.PendingDeviceLogin{}, err
	}
	client, err := providerHTTPClient(provider)
	if err != nil {
		return codexauth.PendingDeviceLogin{}, fmt.Errorf("provider %q: %w", providerName, err)
	}
	return codexauth.StartDeviceLogin(ctx, codexauth.DeviceLoginOptions{HTTPClient: client})
}

func (s *Service) ClearCodexAuth(providerName string) error {
	provider, err := s.codexProvider(providerName)
	if err != nil {
		return err
	}
	if err := os.Remove(provider.AuthFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove Codex auth file: %w", err)
	}
	return nil
}

func (s *Service) DiscoverProviderModels(ctx context.Context, providerName string) ([]string, error) {
	cfg, err := config.Load(s.ConfigPath())
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
	client, err := providerHTTPClient(provider)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", providerName, err)
	}
	if client == nil {
		client = &http.Client{}
	}
	client.Timeout = 20 * time.Second
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
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20+1))
	if err != nil {
		return nil, fmt.Errorf("read provider model list: %w", err)
	}
	if len(body) > 4<<20 {
		return nil, fmt.Errorf("provider model list is too large")
	}
	if err := json.Unmarshal(body, &payload); err != nil {
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

func providerConfigsEqual(left, right config.ProviderConfig) bool {
	// YAML's empty mapping and an omitted mapping have the same provider
	// execution meaning for top-level model parameters/reasoning levels. Treat
	// those representations as one target so a retry after the first durable
	// write does not create a spurious change. Parameter numbers are compared
	// through JSON so yaml.v3's int and the command decoder's int64 are the
	// same target value.
	if left.Name != right.Name || left.BaseURL != right.BaseURL || left.APIKey != right.APIKey || left.AuthFile != right.AuthFile || left.RequestTimeout != right.RequestTimeout || left.HTTPProxy != right.HTTPProxy || left.HTTPSProxy != right.HTTPSProxy || left.MaxConcurrentRequests != right.MaxConcurrentRequests || len(left.Models) != len(right.Models) {
		return false
	}
	for profile, leftModel := range left.Models {
		rightModel, ok := right.Models[profile]
		if !ok {
			return false
		}
		leftParameters, rightParameters := leftModel.Parameters, rightModel.Parameters
		leftLevels, rightLevels := leftModel.ReasoningConfig.Levels, rightModel.ReasoningConfig.Levels
		leftModel.Parameters, rightModel.Parameters = nil, nil
		leftModel.ReasoningConfig.Levels, rightModel.ReasoningConfig.Levels = nil, nil
		if len(leftModel.Input) == 0 {
			leftModel.Input = nil
		}
		if len(rightModel.Input) == 0 {
			rightModel.Input = nil
		}
		if reflect.DeepEqual(leftModel, rightModel) && canonicalProviderJSON(leftParameters) == canonicalProviderJSON(rightParameters) && canonicalProviderJSON(leftLevels) == canonicalProviderJSON(rightLevels) {
			continue
		}
		return false
	}
	return true
}

func canonicalProviderJSON(value any) string {
	if value == nil {
		return "null"
	}
	if reflected := reflect.ValueOf(value); reflected.Kind() == reflect.Map && reflected.Len() == 0 {
		return "null"
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "<invalid>"
	}
	return string(data)
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
		Name:                  provider.Name,
		BaseURL:               provider.BaseURL,
		APIKey:                visibleAPIKey,
		APIKeyConfigured:      apiKey != "",
		AuthFile:              authFile,
		RequestTimeout:        provider.RequestTimeout,
		HTTPProxy:             provider.HTTPProxy,
		HTTPSProxy:            provider.HTTPSProxy,
		MaxConcurrentRequests: provider.MaxConcurrentRequests,
		Models:                make([]ProviderModelSettings, 0, len(provider.Models)),
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
			Compatibility:   model.Compatibility,
			Input:           append([]string(nil), model.Input...),
			DeveloperRole:   model.DeveloperRole,
			ContextWindow:   model.ContextWindow,
			InputLimit:      model.InputLimit,
			OutputLimit:     model.OutputLimit,
			Parameters:      copyParameterMap(model.Parameters),
			ReasoningConfig: model.ReasoningConfig,
			Pricing:         copyModelPricing(model.Pricing),
		})
	}
	return settings
}

func copyModelPricing(pricing *config.ModelPricing) *config.ModelPricing {
	if pricing == nil {
		return nil
	}
	copied := *pricing
	copied.Currency = strings.ToUpper(strings.TrimSpace(copied.Currency))
	if copied.Currency == "" {
		copied.Currency = "USD"
	}
	if pricing.LongContext != nil {
		longContext := *pricing.LongContext
		copied.LongContext = &longContext
	}
	return &copied
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
		return CodexAuthStatus{Status: "error", Message: "Codex authentication status is unavailable"}
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return CodexAuthStatus{Status: "error", Message: "Codex authentication file is invalid"}
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

func (s *Service) codexProvider(providerName string) (config.ProviderConfig, error) {
	cfg, err := config.Load(s.ConfigPath())
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

// validateProviderSettingsCreateFilename is deliberately narrower than the
// provider identity contract. Provider names are durable identities and may
// contain internal spaces and Unicode; only creates need an additional
// cross-platform filename check because they derive <name>.yaml. Updates use
// the existing resolved path and therefore do not reinterpret the identity as
// a new filename.
func validateProviderSettingsCreateFilename(name string) error {
	const extension = ".yaml"
	const maxFilenameBytes = 255

	if len([]byte(name))+len(extension) > maxFilenameBytes {
		return fmt.Errorf("provider name is too long for a cross-platform provider filename")
	}
	characters := []rune(name)
	if strings.HasSuffix(name, ".") || (len(characters) > 0 && unicode.IsSpace(characters[len(characters)-1])) {
		return fmt.Errorf("provider name cannot end with a dot or space when creating a provider file")
	}
	if strings.ContainsAny(name, `<>:"|?*`) {
		return fmt.Errorf("provider name contains a character that is invalid in a cross-platform filename")
	}

	// Windows device names remain reserved even when an extension is added.
	// Check the portion before the first dot, matching the Win32 filename
	// normalization rules (CON.yaml, for example, is still reserved).
	deviceName := name
	if dot := strings.IndexByte(deviceName, '.'); dot >= 0 {
		deviceName = deviceName[:dot]
	}
	switch strings.ToUpper(deviceName) {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return fmt.Errorf("provider name is reserved on Windows and cannot be used as a provider filename")
	}
	return nil
}
