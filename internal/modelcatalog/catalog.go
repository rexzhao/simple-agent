// Package modelcatalog provides a cached, read-only view of the public
// models.dev catalog (https://models.dev/api.json). It lets the Web UI search
// third-party model metadata by model ID and fill provider model profiles
// without requiring the browser to reach the public endpoint directly.
//
// The catalog is fetched on demand by the process, cached in memory for a
// bounded TTL, and never stores credentials. Callers treat the data as
// provider-agnostic defaults; a private gateway may differ.
package modelcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultCatalogURL is the public models.dev API endpoint.
const DefaultCatalogURL = "https://models.dev/api.json"

// DefaultMaxBytes bounds a single catalog download.
const DefaultMaxBytes = 16 << 20 // 16 MiB

// DefaultTTL controls how long a fetched catalog is reused.
const DefaultTTL = 24 * time.Hour

// DefaultTimeout bounds each fetch attempt.
const DefaultTimeout = 30 * time.Second

// Catalog is a process-wide cached view of the models.dev catalog.
type Catalog struct {
	url     string
	client  *http.Client
	ttl     time.Duration
	maxBytes int64

	mu    sync.Mutex
	data  *Data
	fetchedAt time.Time
	err   error
}

// Options configures a Catalog instance.
type Options struct {
	URL      string
	HTTPClient *http.Client
	TTL      time.Duration
	MaxBytes int64
}

// New creates a Catalog with the given options, applying defaults.
func New(options Options) *Catalog {
	url := strings.TrimSpace(options.URL)
	if url == "" {
		url = DefaultCatalogURL
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}
	ttl := options.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	maxBytes := options.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Catalog{url: url, client: client, ttl: ttl, maxBytes: maxBytes}
}

// Model is one entry in the catalog. Fields mirror the models.dev schema that
// provider profiles care about; reasoning options are normalized to a bounded
// form so the UI can offer level lists without parsing raw JSON.
type Model struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	Provider      string           `json:"provider"`
	Description   string           `json:"description,omitempty"`
	ContextWindow int              `json:"context_window,omitempty"`
	InputLimit    int              `json:"input_limit,omitempty"`
	OutputLimit   int              `json:"output_limit,omitempty"`
	Input         []string         `json:"input"`
	Reasoning     ReasoningOptions `json:"reasoning,omitempty"`
	Pricing       *Pricing         `json:"pricing,omitempty"`
}

// ReasoningOptions describes the reasoning controls a model exposes.
type ReasoningOptions struct {
	Enabled       bool     `json:"enabled"`
	EffortLevels  []string `json:"effort_levels,omitempty"`
	BudgetMin     *int     `json:"budget_min,omitempty"`
	BudgetMax     *int     `json:"budget_max,omitempty"`
	SupportsToggle bool    `json:"supports_toggle,omitempty"`
}

// Pricing mirrors the models.dev cost structure in USD per 1M tokens.
type Pricing struct {
	Input         float64 `json:"input,omitempty"`
	Output        float64 `json:"output,omitempty"`
	CacheRead     float64 `json:"cache_read,omitempty"`
	CacheWrite    float64 `json:"cache_write,omitempty"`
	LongContextThreshold int `json:"long_context_threshold,omitempty"`
	InputLong     float64 `json:"input_long,omitempty"`
	OutputLong    float64 `json:"output_long,omitempty"`
	CacheReadLong float64 `json:"cache_read_long,omitempty"`
	CacheWriteLong float64 `json:"cache_write_long,omitempty"`
}

// Data is the parsed catalog index.
type Data struct {
	Models []Model `json:"models"`
}

// SearchResult is one match for a model ID query.
type SearchResult struct {
	Model Model `json:"model"`
}

// SearchOptions controls how model ID queries match.
type SearchOptions struct {
	Limit int
}

// Data returns the parsed catalog, fetching and caching it on first use.
func (c *Catalog) Data(ctx context.Context) (*Data, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data != nil && time.Since(c.fetchedAt) < c.ttl {
		return c.data, nil
	}
	if c.err != nil && time.Since(c.fetchedAt) < c.ttl {
		return nil, c.err
	}
	data, err := c.fetch(ctx)
	c.data = data
	c.fetchedAt = time.Now()
	c.err = err
	return data, err
}

// Search matches model IDs by substring (case-insensitive) and returns up to
// Limit matches. Empty queries return an empty result.
func (c *Catalog) Search(ctx context.Context, query string, options SearchOptions) ([]Model, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil, nil
	}
	limit := options.Limit
	if limit <= 0 {
		limit = 25
	}
	data, err := c.Data(ctx)
	if err != nil {
		return nil, err
	}
	matches := make([]Model, 0, limit)
	for _, model := range data.Models {
		if strings.Contains(strings.ToLower(model.ID), query) {
			matches = append(matches, model)
			if len(matches) >= limit {
				break
			}
		}
	}
	return matches, nil
}

func (c *Catalog) fetch(ctx context.Context) (*Data, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("create model catalog request: %w", err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request model catalog: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("model catalog returned %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, c.maxBytes))
	if err != nil {
		return nil, fmt.Errorf("read model catalog: %w", err)
	}
	var raw map[string]providerEntry
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode model catalog: %w", err)
	}
	data := &Data{Models: make([]Model, 0, len(raw)*4)}
	for providerID, entry := range raw {
		for modelID, model := range entry.Models {
			entryModel := Model{
				ID:          modelID,
				Name:        model.Name,
				Provider:    providerID,
				Description: model.Description,
				Input:       normalizeModalities(model.Modalities.Input),
			}
			if model.Limit != nil {
				entryModel.ContextWindow = model.Limit.Context
				entryModel.InputLimit = model.Limit.Input
				entryModel.OutputLimit = model.Limit.Output
			}
			entryModel.Reasoning = normalizeReasoning(model)
			if model.Cost != nil {
				entryModel.Pricing = normalizePricing(model.Cost)
			}
			data.Models = append(data.Models, entryModel)
		}
	}
	sort.Slice(data.Models, func(i, j int) bool {
		if data.Models[i].Provider == data.Models[j].Provider {
			return data.Models[i].ID < data.Models[j].ID
		}
		return data.Models[i].Provider < data.Models[j].Provider
	})
	return data, nil
}

// providerEntry is the top-level shape of models.dev/api.json.
type providerEntry struct {
	Models map[string]catalogModel `json:"models"`
}

type catalogModel struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Reasoning   bool        `json:"reasoning"`
	ReasoningOptions []reasoningOption `json:"reasoning_options"`
	Modalities  modalities  `json:"modalities"`
	Limit       *modelLimit `json:"limit"`
	Cost        *modelCost  `json:"cost"`
}

type reasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
	Min    *int     `json:"min"`
	Max    *int     `json:"max"`
}

type modalities struct {
	Input []string `json:"input"`
}

type modelLimit struct {
	Context int `json:"context"`
	Input   int `json:"input"`
	Output  int `json:"output"`
}

type modelCost struct {
	Input          float64     `json:"input"`
	Output         float64     `json:"output"`
	CacheRead      float64     `json:"cache_read"`
	CacheWrite     float64     `json:"cache_write"`
	ContextOver200K *modelCost `json:"context_over_200k"`
}

func normalizeModalities(input []string) []string {
	// The local provider schema only accepts text and image. Drop audio,
	// video, and pdf so a filled profile saves without a modality error.
	result := make([]string, 0, 2)
	for _, modality := range input {
		switch strings.ToLower(strings.TrimSpace(modality)) {
		case "text", "image":
			result = append(result, strings.ToLower(strings.TrimSpace(modality)))
		}
	}
	if len(result) == 0 {
		result = []string{"text"}
	}
	return result
}

func normalizeReasoning(model catalogModel) ReasoningOptions {
	options := ReasoningOptions{Enabled: model.Reasoning}
	seenEffort := map[string]struct{}{}
	var budgetMin, budgetMax *int
	for _, option := range model.ReasoningOptions {
		switch option.Type {
		case "toggle":
			options.SupportsToggle = true
		case "effort":
			for _, value := range option.Values {
				value = strings.TrimSpace(value)
				if value == "" || value == "null" {
					continue
				}
				if value == "none" {
					value = "off"
				}
				if _, ok := seenEffort[value]; ok {
					continue
				}
				seenEffort[value] = struct{}{}
				options.EffortLevels = append(options.EffortLevels, value)
			}
		case "budget_tokens":
			if option.Min != nil {
				budgetMin = option.Min
			}
			if option.Max != nil {
				budgetMax = option.Max
			}
		}
	}
	options.BudgetMin = budgetMin
	options.BudgetMax = budgetMax
	return options
}

func normalizePricing(cost *modelCost) *Pricing {
	result := &Pricing{
		Input:      cost.Input,
		Output:     cost.Output,
		CacheRead:  cost.CacheRead,
		CacheWrite: cost.CacheWrite,
	}
	if cost.ContextOver200K != nil {
		result.LongContextThreshold = 200_000
		result.InputLong = cost.ContextOver200K.Input
		result.OutputLong = cost.ContextOver200K.Output
		result.CacheReadLong = cost.ContextOver200K.CacheRead
		result.CacheWriteLong = cost.ContextOver200K.CacheWrite
	}
	return result
}

// ErrUnavailable is returned when the catalog cannot be fetched.
var ErrUnavailable = errors.New("model catalog is unavailable")