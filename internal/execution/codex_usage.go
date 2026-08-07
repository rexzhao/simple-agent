package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rexzhao/simple-agent/internal/codexauth"
)

// CodexUsage mirrors the ChatGPT wham/usage response. Only the fields the UI
// renders today are modeled; unknown fields are ignored so schema drift stays
// tolerant.
type CodexUsage struct {
	UserID    string `json:"user_id"`
	AccountID string `json:"account_id"` // user id, not chatgpt_account_id
	Email     string `json:"email"`
	PlanType  string `json:"plan_type"`

	RateLimit            *CodexUsageWindowSet   `json:"rate_limit"`
	AdditionalRateLimits []CodexUsageAdditional `json:"additional_rate_limits"`

	Credits *CodexUsageCredits `json:"credits"`
}

type CodexUsageWindowSet struct {
	Allowed         bool              `json:"allowed"`
	LimitReached    bool              `json:"limit_reached"`
	PrimaryWindow   *CodexUsageWindow `json:"primary_window"`
	SecondaryWindow *CodexUsageWindow `json:"secondary_window"`
}

type CodexUsageWindow struct {
	UsedPercent        int64 `json:"used_percent"`
	LimitWindowSeconds int64 `json:"limit_window_seconds"`
	ResetAfterSeconds  int64 `json:"reset_after_seconds"`
	ResetAt            int64 `json:"reset_at"` // unix seconds
}

type CodexUsageAdditional struct {
	LimitName      string               `json:"limit_name"`
	MeteredFeature string               `json:"metered_feature"`
	RateLimit      *CodexUsageWindowSet `json:"rate_limit"`
}

type CodexUsageCredits struct {
	HasCredits          bool   `json:"has_credits"`
	Unlimited           bool   `json:"unlimited"`
	OverageLimitReached bool   `json:"overage_limit_reached"`
	Balance             string `json:"balance"`
	ApproxLocalMessages []int  `json:"approx_local_messages"`
	ApproxCloudMessages []int  `json:"approx_cloud_messages"`
}

// CodexUsage fetches the current Codex usage/quota for the named provider.
// The token is resolved through the existing refresh-aware TokenSource, and
// the (optional) ChatGPT-Account-Id header is derived from the access token's
// JWT claims on the fly rather than persisted.
func (s *Service) CodexUsage(ctx context.Context, providerName string) (CodexUsage, error) {
	provider, err := s.codexProvider(providerName)
	if err != nil {
		return CodexUsage{}, err
	}
	client, err := providerHTTPClient(provider)
	if err != nil {
		return CodexUsage{}, fmt.Errorf("provider %q: %w", providerName, err)
	}
	token, err := (&codexauth.TokenSource{Store: codexauth.Store{Path: provider.AuthFile}, HTTPClient: client}).AccessToken(ctx)
	if err != nil {
		return CodexUsage{}, fmt.Errorf("resolve Codex access token: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL(provider.BaseURL), nil)
	if err != nil {
		return CodexUsage{}, fmt.Errorf("create Codex usage request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token.Token)
	request.Header.Set("User-Agent", "codex-cli")
	request.Header.Set("Accept", "application/json")
	if claims, err := codexauth.DecodeClaims(token.Token); err == nil && strings.TrimSpace(claims.ChatGPTAccountID) != "" {
		request.Header.Set("ChatGPT-Account-Id", claims.ChatGPTAccountID)
	}

	httpClient := client
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return CodexUsage{}, fmt.Errorf("request Codex usage: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body := readRedactedResponseBody(response.Body, token.Token)
		return CodexUsage{}, fmt.Errorf("fetch Codex usage: %s: %s", response.Status, body)
	}
	var usage CodexUsage
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil {
		return CodexUsage{}, fmt.Errorf("decode Codex usage response: %w", err)
	}
	return usage, nil
}

// codexUsageURL derives the usage endpoint from the provider base URL by
// swapping the trailing /backend-api/codex for /backend-api/wham/usage, which
// keeps mirrored/proxied base URLs working. It falls back to the default
// ChatGPT endpoint when the base URL does not follow the expected shape.
func codexUsageURL(baseURL string) string {
	const codexSuffix = "/backend-api/codex"
	const usagePath = "/backend-api/wham/usage"
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(trimmed, codexSuffix) {
		return strings.TrimSuffix(trimmed, codexSuffix) + usagePath
	}
	return codexauth.DefaultUsageURL
}

// readRedactedResponseBody reads a bounded response body, redacting any
// occurrence of the given secret so tokens never leak into error messages.
func readRedactedResponseBody(body io.Reader, secret string) string {
	data, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil {
		return "read response body: " + err.Error()
	}
	message := strings.TrimSpace(string(data))
	if message == "" {
		return "empty response body"
	}
	if strings.TrimSpace(secret) != "" {
		message = strings.ReplaceAll(message, secret, "<redacted>")
	}
	return message
}
