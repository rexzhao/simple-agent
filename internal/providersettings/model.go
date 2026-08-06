// Package providersettings contains the typed, read-only provider settings
// resource.  It is deliberately separate from execution's HTTP DTOs: the
// latter contain write inputs and, historically, an API key field.
package providersettings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

const ResourceID = "server"

// These are wire-contract bounds, not storage limits. They keep values
// representable by both Go and JavaScript while rejecting nonsensical config
// values before they can enter a snapshot or journal.
const (
	MaxProviderNameBytes    = config.MaxProviderNameBytes
	MaxWireInteger          = 1_000_000_000
	MaxReasoningStringBytes = 256
	MaxReasoningNumberAbs   = 9_007_199_254_740_991.0 // JavaScript MAX_SAFE_INTEGER
)

// ProviderSettingsSnapshot is the complete safe view of provider settings.
// It intentionally does not contain APIKey, ResolvedAPIKey, arbitrary model
// parameters, or Codex authentication status.  The latter can contain
// account identifiers, refresh tokens, and one-time login codes and belongs
// to a separately authorized resource if it is ever needed by the UI.
type ProviderSettingsSnapshot struct {
	ServerRoot      string             `json:"server_root"`
	ConfigPath      string             `json:"config_path"`
	DefaultProvider string             `json:"default_provider"`
	DefaultModel    string             `json:"default_model"`
	Providers       []ProviderSettings `json:"providers"`
}

// ProviderSettings is a safe provider entity. AuthFile is an auth-file
// location relative to the configured auth directory, never the file
// contents. Proxy and base URLs are stripped of user-info, query, and
// fragment components before they reach this DTO.
type ProviderSettings struct {
	Name                  string                  `json:"name"`
	BaseURL               string                  `json:"base_url"`
	APIKeyConfigured      bool                    `json:"api_key_configured"`
	AuthFile              string                  `json:"auth_file"`
	RequestTimeout        string                  `json:"request_timeout"`
	HTTPProxy             string                  `json:"http_proxy"`
	HTTPSProxy            string                  `json:"https_proxy"`
	MaxConcurrentRequests int                     `json:"max_concurrent_requests"`
	Models                []ProviderModelSettings `json:"models"`
}

// ProviderModelSettings contains model selection and display metadata. Raw
// Parameters are purposefully not part of the wire schema: they are an
// unbounded map supplied by provider files and cannot be made a secret-safe
// DTO by copying it wholesale.
type ProviderModelSettings struct {
	Profile         string            `json:"profile"`
	ID              string            `json:"id"`
	Type            string            `json:"type"`
	Compatibility   string            `json:"compatibility"`
	Input           []string          `json:"input"`
	DeveloperRole   string            `json:"developer_role"`
	ContextWindow   int               `json:"context_window"`
	InputLimit      int               `json:"input_limit"`
	OutputLimit     int               `json:"output_limit"`
	ReasoningConfig ReasoningMetadata `json:"reasoning_config"`
	Pricing         *PricingMetadata  `json:"pricing"`
}

// ReasoningMetadata is bounded display metadata. Values are a strict JSON
// scalar union; structured/arbitrary provider extensions are not copied into
// the resource.
type ReasoningMetadata struct {
	Parameter string           `json:"parameter"`
	Default   string           `json:"default"`
	Levels    []ReasoningLevel `json:"levels"`
}

type ReasoningLevel struct {
	Name  string          `json:"name"`
	Value ReasoningScalar `json:"value"`
}

type ReasoningScalarKind string

const (
	ReasoningString  ReasoningScalarKind = "string"
	ReasoningNumber  ReasoningScalarKind = "number"
	ReasoningBoolean ReasoningScalarKind = "boolean"
	ReasoningNull    ReasoningScalarKind = "null"
)

// ReasoningScalar preserves the JSON scalar type and, for numbers, the
// original JSON number spelling. This avoids turning booleans/numbers/null
// into strings and keeps future command round-trips lossless within the
// bounded wire contract.
type ReasoningScalar struct {
	Kind        ReasoningScalarKind
	StringValue string
	NumberValue string
	BoolValue   bool
}

func (s ReasoningScalar) Validate() error {
	switch s.Kind {
	case ReasoningString:
		if !utf8.ValidString(s.StringValue) || len([]byte(s.StringValue)) > MaxReasoningStringBytes {
			return fmt.Errorf("reasoning string exceeds maximum size")
		}
	case ReasoningNumber:
		if err := validateReasoningNumber(s.NumberValue); err != nil {
			return err
		}
	case ReasoningBoolean, ReasoningNull:
		// no additional payload is permitted/needed for these kinds
	default:
		return fmt.Errorf("invalid reasoning scalar kind %q", s.Kind)
	}
	return nil
}

func (s ReasoningScalar) MarshalJSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	switch s.Kind {
	case ReasoningString:
		return json.Marshal(s.StringValue)
	case ReasoningNumber:
		return []byte(s.NumberValue), nil
	case ReasoningBoolean:
		if s.BoolValue {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case ReasoningNull:
		return []byte("null"), nil
	default:
		return nil, fmt.Errorf("invalid reasoning scalar kind %q", s.Kind)
	}
}

func (s *ReasoningScalar) UnmarshalJSON(data []byte) error {
	if s == nil {
		return fmt.Errorf("reasoning scalar is nil")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("decode reasoning scalar: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("decode reasoning scalar: trailing data")
	}
	result := ReasoningScalar{}
	switch typed := value.(type) {
	case nil:
		result.Kind = ReasoningNull
	case string:
		result.Kind, result.StringValue = ReasoningString, typed
	case json.Number:
		result.Kind, result.NumberValue = ReasoningNumber, string(typed)
	case bool:
		result.Kind, result.BoolValue = ReasoningBoolean, typed
	default:
		return fmt.Errorf("reasoning scalar must be string, number, boolean, or null")
	}
	if err := result.Validate(); err != nil {
		return err
	}
	*s = result
	return nil
}

type PricingMetadata struct {
	InputCacheHit        float64              `json:"input_cache_hit"`
	InputCacheMiss       float64              `json:"input_cache_miss"`
	CacheWrite           float64              `json:"cache_write"`
	Output               float64              `json:"output"`
	Currency             string               `json:"currency"`
	LongContextThreshold int                  `json:"long_context_threshold"`
	LongContext          *PricingTierMetadata `json:"long_context"`
}

type PricingTierMetadata struct {
	InputCacheHit  float64 `json:"input_cache_hit"`
	InputCacheMiss float64 `json:"input_cache_miss"`
	CacheWrite     float64 `json:"cache_write"`
	Output         float64 `json:"output"`
}

// DefaultSelection is the collection-level entity used by a default-only
// change. Keeping it as an operation makes default changes observable without
// inventing a second stream or rewriting every provider entity.
type DefaultSelection struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

const (
	OperationUpsert         = "upsert"
	OperationUpsertDefault  = OperationUpsert
	OperationRemove         = "remove"
	OperationReplaceDefault = "default.replace"
	OperationDefaultReplace = OperationReplaceDefault
)

type Operation struct {
	Op      string
	Key     string
	Value   *ProviderSettings
	Default *DefaultSelection
}

func (o Operation) Validate() error {
	if err := ValidateProviderName(o.Key); err != nil {
		return fmt.Errorf("operation key is not a canonical provider name: %w", err)
	}
	switch o.Op {
	case OperationUpsertDefault:
		if o.Value == nil || o.Default != nil {
			return fmt.Errorf("provider upsert requires provider value")
		}
		if o.Value.Name != o.Key {
			return fmt.Errorf("provider upsert key does not match provider name")
		}
		return o.Value.Validate()
	case OperationRemove:
		if o.Value != nil || o.Default != nil {
			return fmt.Errorf("provider remove must not contain a value")
		}
	case OperationReplaceDefault:
		if o.Value != nil || o.Default == nil {
			return fmt.Errorf("default replacement requires default value")
		}
		if o.Key != ResourceID {
			return fmt.Errorf("default replacement key must be %q", ResourceID)
		}
		if err := o.Default.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid provider settings operation %q", o.Op)
	}
	return nil
}

type Change struct {
	ResourceRevision string      `json:"resource_revision"`
	Operations       []Operation `json:"operations"`
}

func (c Change) ToResourceChange() (syncengine.ResourceChange, error) {
	if _, err := protocol.ParseUint64Decimal(c.ResourceRevision); err != nil {
		return syncengine.ResourceChange{}, fmt.Errorf("invalid resource revision: %w", err)
	}
	if len(c.Operations) == 0 {
		return syncengine.ResourceChange{}, fmt.Errorf("provider settings change must contain an operation")
	}
	result := syncengine.ResourceChange{ResourceRevision: protocol.ResourceRevision(c.ResourceRevision), Operations: make([]protocol.ChangeOperation, 0, len(c.Operations))}
	for _, operation := range c.Operations {
		if err := operation.Validate(); err != nil {
			return syncengine.ResourceChange{}, err
		}
		data, err := json.Marshal(operation)
		if err != nil {
			return syncengine.ResourceChange{}, err
		}
		result.Operations = append(result.Operations, protocol.ChangeOperation{Op: operation.Op, Raw: data})
	}
	return result, nil
}

func (o Operation) MarshalJSON() ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	switch o.Op {
	case OperationUpsertDefault:
		return json.Marshal(struct {
			Op    string            `json:"op"`
			Key   string            `json:"key"`
			Value *ProviderSettings `json:"value"`
		}{o.Op, o.Key, o.Value})
	case OperationRemove:
		return json.Marshal(struct {
			Op  string `json:"op"`
			Key string `json:"key"`
		}{o.Op, o.Key})
	default:
		return json.Marshal(struct {
			Op    string            `json:"op"`
			Key   string            `json:"key"`
			Value *DefaultSelection `json:"value"`
		}{o.Op, o.Key, o.Default})
	}
}

type CommittedChangeKind string

const (
	CommittedProviderUpsert  CommittedChangeKind = "provider.upsert"
	CommittedProviderRemove  CommittedChangeKind = "provider.remove"
	CommittedDefaultChanged  CommittedChangeKind = "provider.default"
	CommittedProviderRefresh CommittedChangeKind = "provider.refresh"
)

// CommittedChange is the publication boundary between execution and the
// resource provider. It contains only identifiers and lifecycle metadata, not
// a ProviderConfig or any credential-bearing value. Future provider commands
// should call this boundary only after their durable transaction succeeds.
type CommittedChange struct {
	Kind            CommittedChangeKind
	ProviderName    string
	DefaultProvider string
	DefaultModel    string
}

type ChangeSink interface {
	PublishCommitted(CommittedChange) error
}

type InvalidationSink interface {
	Invalidate(reason string) error
}

// ValidateProviderName is the provider-settings wire contract. It intentionally
// allows internal spaces and Unicode because existing config.Load accepts those
// names. It only rejects ambiguous/path/control boundaries and applies one
// bounded UTF-8 size rule shared by the TypeScript adapter.
func ValidateProviderName(name string) error {
	return config.ValidateProviderName(name)
}

func validateWireInteger(value int, name string) error {
	if value < 0 || value > MaxWireInteger {
		return fmt.Errorf("%s must be between 0 and %d", name, MaxWireInteger)
	}
	return nil
}

func validateReasoningNumber(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("reasoning number is required")
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || math.Abs(parsed) > MaxReasoningNumberAbs {
		return fmt.Errorf("reasoning number is not finite or exceeds the safe precision boundary")
	}
	return nil
}

func (d DefaultSelection) Validate() error {
	if d.Provider != "" {
		if err := ValidateProviderName(d.Provider); err != nil {
			return fmt.Errorf("default provider: %w", err)
		}
	}
	if !utf8.ValidString(d.Model) {
		return fmt.Errorf("default selection must be valid UTF-8")
	}
	return nil
}

// SnapshotFromConfig is the sole config-to-resource mapping. It is also used
// by integration tests so that the production provider and the durable config
// authority exercise the same safety boundary.
func SnapshotFromConfig(cfg config.Config, serverRoot string) (ProviderSettingsSnapshot, error) {
	result := ProviderSettingsSnapshot{
		ServerRoot: strings.TrimSpace(serverRoot), ConfigPath: strings.TrimSpace(cfg.ConfigPath),
		DefaultProvider: strings.TrimSpace(cfg.DefaultProvider), DefaultModel: strings.TrimSpace(cfg.DefaultModel),
		Providers: make([]ProviderSettings, 0, len(cfg.Providers)),
	}
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		provider, err := providerFromConfig(cfg, name, cfg.Providers[name])
		if err != nil {
			return ProviderSettingsSnapshot{}, err
		}
		result.Providers = append(result.Providers, provider)
	}
	if err := result.Validate(); err != nil {
		return ProviderSettingsSnapshot{}, err
	}
	return result, nil
}

func providerFromConfig(cfg config.Config, name string, value config.ProviderConfig) (ProviderSettings, error) {
	profiles := make([]string, 0, len(value.Models))
	for profile := range value.Models {
		profiles = append(profiles, profile)
	}
	sort.Strings(profiles)
	models := make([]ProviderModelSettings, 0, len(profiles))
	for _, profile := range profiles {
		model, err := modelFromConfig(value.Models[profile], profile, name, value.BaseURL)
		if err != nil {
			return ProviderSettings{}, fmt.Errorf("provider %q model %q: %w", name, profile, err)
		}
		models = append(models, model)
	}
	authFile := ""
	if strings.TrimSpace(value.AuthFile) != "" {
		relative, err := filepath.Rel(cfg.AuthDir, value.AuthFile)
		if err == nil && relative != "" && relative != "." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && relative != ".." {
			authFile = filepath.ToSlash(relative)
		} else {
			authFile = filepath.Base(value.AuthFile)
		}
	}
	return ProviderSettings{
		Name: name, BaseURL: safeEndpoint(value.BaseURL), APIKeyConfigured: strings.TrimSpace(value.APIKey) != "" || strings.TrimSpace(value.ResolvedAPIKey) != "",
		AuthFile: authFile, RequestTimeout: strings.TrimSpace(value.RequestTimeout), HTTPProxy: safeEndpoint(value.HTTPProxy), HTTPSProxy: safeEndpoint(value.HTTPSProxy),
		MaxConcurrentRequests: value.MaxConcurrentRequests, Models: models,
	}, nil
}

func modelFromConfig(value config.ModelProfile, profile, providerName, baseURL string) (ProviderModelSettings, error) {
	reasoningConfig := value.ReasoningConfig
	if len(reasoningConfig.Levels) == 0 && strings.TrimSpace(reasoningConfig.Parameter) == "" {
		reasoningConfig = config.DefaultReasoningConfig(providerName, baseURL, value)
	}
	reasoning := ReasoningMetadata{Parameter: strings.TrimSpace(reasoningConfig.Parameter), Default: strings.TrimSpace(reasoningConfig.Default), Levels: []ReasoningLevel{}}
	levelNames := make([]string, 0, len(reasoningConfig.Levels))
	for name := range reasoningConfig.Levels {
		levelNames = append(levelNames, name)
	}
	sort.Strings(levelNames)
	for _, name := range levelNames {
		scalar, ok := safeScalar(reasoningConfig.Levels[name])
		if !ok {
			return ProviderModelSettings{}, fmt.Errorf("reasoning level %q must be a bounded JSON scalar", name)
		}
		reasoning.Levels = append(reasoning.Levels, ReasoningLevel{Name: name, Value: scalar})
	}
	var pricing *PricingMetadata
	if value.Pricing != nil {
		if err := value.Pricing.Validate(); err != nil {
			return ProviderModelSettings{}, err
		}
		pricing = pricingFromConfig(value.Pricing)
	}
	return ProviderModelSettings{Profile: profile, ID: value.ID, Type: value.Type, Compatibility: value.Compatibility, Input: append([]string{}, value.Input...), DeveloperRole: value.DeveloperRole, ContextWindow: value.ContextWindow, InputLimit: value.InputLimit, OutputLimit: value.OutputLimit, ReasoningConfig: reasoning, Pricing: pricing}, nil
}

func pricingFromConfig(value *config.ModelPricing) *PricingMetadata {
	result := &PricingMetadata{InputCacheHit: value.InputCacheHit, InputCacheMiss: value.InputCacheMiss, CacheWrite: value.CacheWrite, Output: value.Output, Currency: strings.ToUpper(strings.TrimSpace(value.Currency)), LongContextThreshold: value.LongContextThreshold}
	if value.LongContext != nil {
		result.LongContext = &PricingTierMetadata{InputCacheHit: value.LongContext.InputCacheHit, InputCacheMiss: value.LongContext.InputCacheMiss, CacheWrite: value.LongContext.CacheWrite, Output: value.LongContext.Output}
	}
	return result
}

func safeEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

func safeScalar(value any) (ReasoningScalar, bool) {
	data, err := json.Marshal(value)
	if err != nil {
		return ReasoningScalar{}, false
	}
	var scalar ReasoningScalar
	if err := json.Unmarshal(data, &scalar); err != nil {
		return ReasoningScalar{}, false
	}
	return scalar, true
}

func (s ProviderSettingsSnapshot) Validate() error {
	if !utf8.ValidString(s.ServerRoot) || !utf8.ValidString(s.ConfigPath) || !utf8.ValidString(s.DefaultProvider) || !utf8.ValidString(s.DefaultModel) {
		return fmt.Errorf("snapshot contains invalid UTF-8")
	}
	if s.DefaultProvider != "" {
		if err := ValidateProviderName(s.DefaultProvider); err != nil {
			return fmt.Errorf("default provider: %w", err)
		}
	}
	seen := make(map[string]struct{}, len(s.Providers))
	for _, provider := range s.Providers {
		if err := provider.Validate(); err != nil {
			return err
		}
		if _, ok := seen[provider.Name]; ok {
			return fmt.Errorf("duplicate provider %q", provider.Name)
		}
		seen[provider.Name] = struct{}{}
	}
	return nil
}

func (p ProviderSettings) Validate() error {
	if err := ValidateProviderName(p.Name); err != nil {
		return err
	}
	for _, value := range []string{p.BaseURL, p.AuthFile, p.RequestTimeout, p.HTTPProxy, p.HTTPSProxy} {
		if !utf8.ValidString(value) {
			return fmt.Errorf("provider %q contains invalid UTF-8", p.Name)
		}
	}
	if err := validateWireInteger(p.MaxConcurrentRequests, "max_concurrent_requests"); err != nil {
		return err
	}
	seen := map[string]struct{}{}
	for _, model := range p.Models {
		if err := model.Validate(); err != nil {
			return err
		}
		if _, ok := seen[model.Profile]; ok {
			return fmt.Errorf("duplicate model profile %q", model.Profile)
		}
		seen[model.Profile] = struct{}{}
	}
	return nil
}

func (m ProviderModelSettings) Validate() error {
	if strings.TrimSpace(m.Profile) == "" || !utf8.ValidString(m.Profile) || !utf8.ValidString(m.ID) {
		return fmt.Errorf("model profile is invalid")
	}
	if err := m.ReasoningConfig.Validate(); err != nil {
		return err
	}
	for name, value := range map[string]int{"context_window": m.ContextWindow, "input_limit": m.InputLimit, "output_limit": m.OutputLimit} {
		if err := validateWireInteger(value, name); err != nil {
			return err
		}
	}
	if m.Pricing != nil {
		if err := m.Pricing.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (r ReasoningMetadata) Validate() error {
	if !boundedReasoningString(r.Parameter) || !boundedReasoningString(r.Default) {
		return fmt.Errorf("reasoning metadata is invalid")
	}
	seen := map[string]struct{}{}
	for _, level := range r.Levels {
		if strings.TrimSpace(level.Name) == "" || !boundedReasoningString(level.Name) {
			return fmt.Errorf("reasoning level is invalid")
		}
		if err := level.Value.Validate(); err != nil {
			return fmt.Errorf("reasoning level %q: %w", level.Name, err)
		}
		if _, ok := seen[level.Name]; ok {
			return fmt.Errorf("duplicate reasoning level %q", level.Name)
		}
		seen[level.Name] = struct{}{}
	}
	return nil
}

func boundedReasoningString(value string) bool {
	return utf8.ValidString(value) && len([]byte(value)) <= MaxReasoningStringBytes
}

func (p PricingMetadata) Validate() error {
	if !utf8.ValidString(p.Currency) {
		return fmt.Errorf("pricing currency is invalid")
	}
	for name, value := range map[string]float64{"input_cache_hit": p.InputCacheHit, "input_cache_miss": p.InputCacheMiss, "cache_write": p.CacheWrite, "output": p.Output} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return fmt.Errorf("pricing %s must be non-negative", name)
		}
	}
	if p.LongContextThreshold < 0 {
		return fmt.Errorf("pricing long context threshold must be non-negative")
	}
	if err := validateWireInteger(p.LongContextThreshold, "long_context_threshold"); err != nil {
		return err
	}
	if p.LongContext != nil {
		for name, value := range map[string]float64{"input_cache_hit": p.LongContext.InputCacheHit, "input_cache_miss": p.LongContext.InputCacheMiss, "cache_write": p.LongContext.CacheWrite, "output": p.LongContext.Output} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return fmt.Errorf("long context pricing %s must be non-negative", name)
			}
		}
	}
	return nil
}

func (d DefaultSelection) MarshalJSON() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}{d.Provider, d.Model})
}

// MarshalJSON sorts all collection members at the serialization boundary, so
// the same durable config produces byte-identical snapshots regardless of map
// iteration order.
func (s ProviderSettingsSnapshot) MarshalJSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	copyValue := cloneSnapshot(s)
	sort.Slice(copyValue.Providers, func(i, j int) bool { return copyValue.Providers[i].Name < copyValue.Providers[j].Name })
	for i := range copyValue.Providers {
		sort.Slice(copyValue.Providers[i].Models, func(a, b int) bool {
			return copyValue.Providers[i].Models[a].Profile < copyValue.Providers[i].Models[b].Profile
		})
		for m := range copyValue.Providers[i].Models {
			sort.Slice(copyValue.Providers[i].Models[m].ReasoningConfig.Levels, func(a, b int) bool {
				return copyValue.Providers[i].Models[m].ReasoningConfig.Levels[a].Name < copyValue.Providers[i].Models[m].ReasoningConfig.Levels[b].Name
			})
		}
	}
	type wire ProviderSettingsSnapshot
	return json.Marshal(wire(copyValue))
}

func (p ProviderSettings) MarshalJSON() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	copyValue := cloneProviderSettings(p)
	sort.Slice(copyValue.Models, func(i, j int) bool { return copyValue.Models[i].Profile < copyValue.Models[j].Profile })
	for i := range copyValue.Models {
		sort.Slice(copyValue.Models[i].ReasoningConfig.Levels, func(a, b int) bool {
			return copyValue.Models[i].ReasoningConfig.Levels[a].Name < copyValue.Models[i].ReasoningConfig.Levels[b].Name
		})
	}
	type wire ProviderSettings
	return json.Marshal(wire(copyValue))
}

func (s *ProviderSettingsSnapshot) UnmarshalJSON(data []byte) error {
	if s == nil {
		return fmt.Errorf("provider settings snapshot is nil")
	}
	fields, err := strictObject(data, "provider settings snapshot", []string{"server_root", "config_path", "default_provider", "default_model", "providers"}, true)
	if err != nil {
		return err
	}
	result := ProviderSettingsSnapshot{}
	if result.ServerRoot, err = requiredString(fields["server_root"], "server_root", true); err != nil {
		return err
	}
	if result.ConfigPath, err = requiredString(fields["config_path"], "config_path", true); err != nil {
		return err
	}
	if result.DefaultProvider, err = requiredString(fields["default_provider"], "default_provider", true); err != nil {
		return err
	}
	if result.DefaultModel, err = requiredString(fields["default_model"], "default_model", true); err != nil {
		return err
	}
	if isNull(fields["providers"]) {
		return fmt.Errorf("providers must be an array")
	}
	if err := json.Unmarshal(fields["providers"], &result.Providers); err != nil {
		return fmt.Errorf("providers: %w", err)
	}
	if result.Providers == nil {
		return fmt.Errorf("providers must be an array")
	}
	if err := result.Validate(); err != nil {
		return err
	}
	*s = result
	return nil
}

func (p *ProviderSettings) UnmarshalJSON(data []byte) error {
	if p == nil {
		return fmt.Errorf("provider settings is nil")
	}
	fields, err := strictObject(data, "provider settings", []string{"name", "base_url", "api_key_configured", "auth_file", "request_timeout", "http_proxy", "https_proxy", "max_concurrent_requests", "models"}, true)
	if err != nil {
		return err
	}
	result := ProviderSettings{}
	if result.Name, err = requiredString(fields["name"], "name", false); err != nil {
		return err
	}
	if result.BaseURL, err = requiredString(fields["base_url"], "base_url", true); err != nil {
		return err
	}
	if err := json.Unmarshal(fields["api_key_configured"], &result.APIKeyConfigured); err != nil || isNull(fields["api_key_configured"]) {
		return fmt.Errorf("api_key_configured must be a boolean")
	}
	if result.AuthFile, err = requiredString(fields["auth_file"], "auth_file", true); err != nil {
		return err
	}
	if result.RequestTimeout, err = requiredString(fields["request_timeout"], "request_timeout", true); err != nil {
		return err
	}
	if result.HTTPProxy, err = requiredString(fields["http_proxy"], "http_proxy", true); err != nil {
		return err
	}
	if result.HTTPSProxy, err = requiredString(fields["https_proxy"], "https_proxy", true); err != nil {
		return err
	}
	if err := json.Unmarshal(fields["max_concurrent_requests"], &result.MaxConcurrentRequests); err != nil || isNull(fields["max_concurrent_requests"]) {
		return fmt.Errorf("max_concurrent_requests must be an integer")
	}
	if isNull(fields["models"]) {
		return fmt.Errorf("models must be an array")
	}
	if err := json.Unmarshal(fields["models"], &result.Models); err != nil {
		return fmt.Errorf("models: %w", err)
	}
	if result.Models == nil {
		return fmt.Errorf("models must be an array")
	}
	if err := result.Validate(); err != nil {
		return err
	}
	*p = result
	return nil
}

func (m *ProviderModelSettings) UnmarshalJSON(data []byte) error {
	if m == nil {
		return fmt.Errorf("provider model settings is nil")
	}
	fields, err := strictObject(data, "provider model settings", []string{"profile", "id", "type", "compatibility", "input", "developer_role", "context_window", "input_limit", "output_limit", "reasoning_config", "pricing"}, true)
	if err != nil {
		return err
	}
	result := ProviderModelSettings{}
	if result.Profile, err = requiredString(fields["profile"], "profile", false); err != nil {
		return err
	}
	if result.ID, err = requiredString(fields["id"], "id", true); err != nil {
		return err
	}
	if result.Type, err = requiredString(fields["type"], "type", true); err != nil {
		return err
	}
	if result.Compatibility, err = requiredString(fields["compatibility"], "compatibility", true); err != nil {
		return err
	}
	if result.Input, err = requiredStringArray(fields["input"], "input"); err != nil {
		return err
	}
	if result.DeveloperRole, err = requiredString(fields["developer_role"], "developer_role", true); err != nil {
		return err
	}
	if err := decodeInt(fields["context_window"], &result.ContextWindow, "context_window"); err != nil {
		return err
	}
	if err := decodeInt(fields["input_limit"], &result.InputLimit, "input_limit"); err != nil {
		return err
	}
	if err := decodeInt(fields["output_limit"], &result.OutputLimit, "output_limit"); err != nil {
		return err
	}
	if isNull(fields["reasoning_config"]) {
		return fmt.Errorf("reasoning_config must be an object")
	}
	if err := json.Unmarshal(fields["reasoning_config"], &result.ReasoningConfig); err != nil {
		return err
	}
	if !isNull(fields["pricing"]) {
		var pricing PricingMetadata
		if err := json.Unmarshal(fields["pricing"], &pricing); err != nil {
			return err
		}
		result.Pricing = &pricing
	}
	if err := result.Validate(); err != nil {
		return err
	}
	*m = result
	return nil
}

func (r *ReasoningMetadata) UnmarshalJSON(data []byte) error {
	if r == nil {
		return fmt.Errorf("reasoning metadata is nil")
	}
	fields, err := strictObject(data, "reasoning metadata", []string{"parameter", "default", "levels"}, true)
	if err != nil {
		return err
	}
	result := ReasoningMetadata{}
	if result.Parameter, err = requiredString(fields["parameter"], "parameter", true); err != nil {
		return err
	}
	if result.Default, err = requiredString(fields["default"], "default", true); err != nil {
		return err
	}
	if isNull(fields["levels"]) {
		return fmt.Errorf("levels must be an array")
	}
	if err := json.Unmarshal(fields["levels"], &result.Levels); err != nil {
		return err
	}
	if result.Levels == nil {
		return fmt.Errorf("levels must be an array")
	}
	if err := result.Validate(); err != nil {
		return err
	}
	*r = result
	return nil
}

func (l *ReasoningLevel) UnmarshalJSON(data []byte) error {
	if l == nil {
		return fmt.Errorf("reasoning level is nil")
	}
	fields, err := strictObject(data, "reasoning level", []string{"name", "value"}, true)
	if err != nil {
		return err
	}
	name, err := requiredString(fields["name"], "name", false)
	if err != nil {
		return err
	}
	var value ReasoningScalar
	if err := json.Unmarshal(fields["value"], &value); err != nil {
		return fmt.Errorf("reasoning level value: %w", err)
	}
	*l = ReasoningLevel{Name: name, Value: value}
	return nil
}

func (p *PricingMetadata) UnmarshalJSON(data []byte) error {
	if p == nil {
		return fmt.Errorf("pricing metadata is nil")
	}
	fields, err := strictObject(data, "pricing metadata", []string{"input_cache_hit", "input_cache_miss", "cache_write", "output", "currency", "long_context_threshold", "long_context"}, true)
	if err != nil {
		return err
	}
	result := PricingMetadata{}
	if err := decodeFloat(fields["input_cache_hit"], &result.InputCacheHit, "input_cache_hit"); err != nil {
		return err
	}
	if err := decodeFloat(fields["input_cache_miss"], &result.InputCacheMiss, "input_cache_miss"); err != nil {
		return err
	}
	if err := decodeFloat(fields["cache_write"], &result.CacheWrite, "cache_write"); err != nil {
		return err
	}
	if err := decodeFloat(fields["output"], &result.Output, "output"); err != nil {
		return err
	}
	if result.Currency, err = requiredString(fields["currency"], "currency", true); err != nil {
		return err
	}
	if err := decodeInt(fields["long_context_threshold"], &result.LongContextThreshold, "long_context_threshold"); err != nil {
		return err
	}
	if !isNull(fields["long_context"]) {
		var tier PricingTierMetadata
		if err := json.Unmarshal(fields["long_context"], &tier); err != nil {
			return err
		}
		result.LongContext = &tier
	}
	if err := result.Validate(); err != nil {
		return err
	}
	*p = result
	return nil
}

func (p *PricingTierMetadata) UnmarshalJSON(data []byte) error {
	if p == nil {
		return fmt.Errorf("pricing tier metadata is nil")
	}
	fields, err := strictObject(data, "pricing tier metadata", []string{"input_cache_hit", "input_cache_miss", "cache_write", "output"}, true)
	if err != nil {
		return err
	}
	result := PricingTierMetadata{}
	if err := decodeFloat(fields["input_cache_hit"], &result.InputCacheHit, "input_cache_hit"); err != nil {
		return err
	}
	if err := decodeFloat(fields["input_cache_miss"], &result.InputCacheMiss, "input_cache_miss"); err != nil {
		return err
	}
	if err := decodeFloat(fields["cache_write"], &result.CacheWrite, "cache_write"); err != nil {
		return err
	}
	if err := decodeFloat(fields["output"], &result.Output, "output"); err != nil {
		return err
	}
	*p = result
	return nil
}

func (d *DefaultSelection) UnmarshalJSON(data []byte) error {
	if d == nil {
		return fmt.Errorf("default selection is nil")
	}
	fields, err := strictObject(data, "default selection", []string{"provider", "model"}, true)
	if err != nil {
		return err
	}
	provider, err := requiredString(fields["provider"], "provider", true)
	if err != nil {
		return err
	}
	model, err := requiredString(fields["model"], "model", true)
	if err != nil {
		return err
	}
	result := DefaultSelection{Provider: provider, Model: model}
	if err := result.Validate(); err != nil {
		return err
	}
	*d = result
	return nil
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func requiredString(raw json.RawMessage, name string, allowEmpty bool) (string, error) {
	if len(raw) == 0 || isNull(raw) {
		return "", fmt.Errorf("%s must be a string", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || (!allowEmpty && strings.TrimSpace(value) == "") {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return value, nil
}

func requiredStringArray(raw json.RawMessage, name string) ([]string, error) {
	if len(raw) == 0 || isNull(raw) {
		return nil, fmt.Errorf("%s must be an array", name)
	}
	var result []string
	if err := json.Unmarshal(raw, &result); err != nil || result == nil {
		return nil, fmt.Errorf("%s must be an array of strings", name)
	}
	return result, nil
}

func decodeInt(raw json.RawMessage, target *int, name string) error {
	if len(raw) == 0 || isNull(raw) {
		return fmt.Errorf("%s must be an integer", name)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value json.Number
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("%s must be an integer", name)
	}
	parsed, err := value.Int64()
	if err != nil || int64(int(parsed)) != parsed {
		return fmt.Errorf("%s must be an integer", name)
	}
	if parsed < 0 || parsed > int64(MaxWireInteger) {
		return fmt.Errorf("%s must be between 0 and %d", name, MaxWireInteger)
	}
	*target = int(parsed)
	return nil
}

func decodeFloat(raw json.RawMessage, target *float64, name string) error {
	if len(raw) == 0 || isNull(raw) {
		return fmt.Errorf("%s must be a number", name)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value json.Number
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("%s must be a number", name)
	}
	parsed, err := value.Float64()
	if err != nil {
		return fmt.Errorf("%s must be a number", name)
	}
	*target = parsed
	return nil
}

func (o *Operation) UnmarshalJSON(data []byte) error {
	fields, err := strictObject(data, "provider settings operation", []string{"op", "key", "value"}, false)
	if err != nil {
		return err
	}
	var op, key string
	if err := json.Unmarshal(fields["op"], &op); err != nil {
		return err
	}
	if err := json.Unmarshal(fields["key"], &key); err != nil {
		return err
	}
	result := Operation{Op: op, Key: key}
	if op == OperationRemove {
		if _, present := fields["value"]; present {
			return fmt.Errorf("remove must not contain value")
		}
	} else if op == OperationUpsertDefault {
		var value ProviderSettings
		if err := json.Unmarshal(fields["value"], &value); err != nil {
			return err
		}
		result.Value = &value
	} else if op == OperationReplaceDefault {
		var value DefaultSelection
		if err := json.Unmarshal(fields["value"], &value); err != nil {
			return err
		}
		result.Default = &value
	} else {
		return fmt.Errorf("invalid provider settings operation %q", op)
	}
	if err := result.Validate(); err != nil {
		return err
	}
	*o = result
	return nil
}

func cloneSnapshot(value ProviderSettingsSnapshot) ProviderSettingsSnapshot {
	result := value
	result.Providers = make([]ProviderSettings, len(value.Providers))
	copy(result.Providers, value.Providers)
	for i := range result.Providers {
		result.Providers[i].Models = make([]ProviderModelSettings, len(value.Providers[i].Models))
		copy(result.Providers[i].Models, value.Providers[i].Models)
		for j := range result.Providers[i].Models {
			result.Providers[i].Models[j].Input = append([]string{}, value.Providers[i].Models[j].Input...)
			result.Providers[i].Models[j].ReasoningConfig.Levels = append([]ReasoningLevel{}, value.Providers[i].Models[j].ReasoningConfig.Levels...)
			if value.Providers[i].Models[j].Pricing != nil {
				pricing := *value.Providers[i].Models[j].Pricing
				if pricing.LongContext != nil {
					tier := *pricing.LongContext
					pricing.LongContext = &tier
				}
				result.Providers[i].Models[j].Pricing = &pricing
			}
		}
	}
	return result
}

func cloneProviderSettings(value ProviderSettings) ProviderSettings {
	result := value
	result.Models = make([]ProviderModelSettings, len(value.Models))
	copy(result.Models, value.Models)
	for i := range result.Models {
		result.Models[i].Input = append([]string{}, value.Models[i].Input...)
		result.Models[i].ReasoningConfig.Levels = append([]ReasoningLevel{}, value.Models[i].ReasoningConfig.Levels...)
		if value.Models[i].Pricing != nil {
			pricing := *value.Models[i].Pricing
			if pricing.LongContext != nil {
				tier := *pricing.LongContext
				pricing.LongContext = &tier
			}
			result.Models[i].Pricing = &pricing
		}
	}
	return result
}

func isJSONObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func strictObject(data []byte, name string, allowed []string, requireAll bool) (map[string]json.RawMessage, error) {
	if !isJSONObject(data) || !utf8.Valid(data) {
		return nil, fmt.Errorf("decode %s: must be a JSON object", name)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, fmt.Errorf("decode %s: must be a JSON object", name)
	}
	allowedSet := map[string]struct{}{}
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	result := map[string]json.RawMessage{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key := keyToken.(string)
		if _, ok := result[key]; ok {
			return nil, fmt.Errorf("decode %s: duplicate field %q", name, key)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		if _, ok := allowedSet[key]; !ok {
			return nil, fmt.Errorf("decode %s: unknown field %q", name, key)
		}
		result[key] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("decode %s: trailing data", name)
	}
	if requireAll {
		for _, key := range allowed {
			if _, ok := result[key]; !ok {
				return nil, fmt.Errorf("decode %s: missing field %q", name, key)
			}
		}
	}
	return result, nil
}
