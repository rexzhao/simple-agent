package execution

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/codexauth"
	"github.com/rexzhao/simple-agent/internal/config"
)

func TestServiceCodexUsageFetchesAndDecodes(t *testing.T) {
	var gotAuth, gotAccountID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Fatalf("path = %q, want /backend-api/wham/usage", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotAccountID = r.Header.Get("ChatGPT-Account-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"user_id": "user-1",
			"account_id": "user-1",
			"email": "test@example.com",
			"plan_type": "pro",
			"rate_limit": {
				"allowed": true,
				"limit_reached": false,
				"primary_window": {"used_percent": 58, "limit_window_seconds": 604800, "reset_after_seconds": 261881, "reset_at": 1786255839},
				"secondary_window": null
			},
			"additional_rate_limits": [
				{"limit_name": "GPT-5.3-Codex-Spark", "metered_feature": "codex_bengalfox",
				 "rate_limit": {"allowed": true, "limit_reached": false,
					"primary_window": {"used_percent": 0, "limit_window_seconds": 604800, "reset_after_seconds": 604800, "reset_at": 1786598758},
					"secondary_window": null}}
			],
			"credits": {"has_credits": false, "unlimited": false, "overage_limit_reached": false, "balance": "0", "approx_local_messages": [0, 0], "approx_cloud_messages": [0, 0]}
		}`))
	}))
	defer server.Close()

	service := newProviderSettingsTestService(t)
	createCodexTestProvider(t, service, server.URL+"/backend-api/codex", "codex-team")

	usage, err := service.CodexUsage(context.Background(), "codex-team")
	if err != nil {
		t.Fatalf("CodexUsage() error = %v", err)
	}
	if usage.PlanType != "pro" || usage.Email != "test@example.com" {
		t.Fatalf("usage = %#v, want plan/email", usage)
	}
	if usage.RateLimit == nil || usage.RateLimit.PrimaryWindow == nil || usage.RateLimit.PrimaryWindow.UsedPercent != 58 {
		t.Fatalf("usage.RateLimit = %#v, want primary window 58%%", usage.RateLimit)
	}
	if usage.RateLimit.SecondaryWindow != nil {
		t.Fatalf("secondary window = %#v, want nil", usage.RateLimit.SecondaryWindow)
	}
	if len(usage.AdditionalRateLimits) != 1 || usage.AdditionalRateLimits[0].LimitName != "GPT-5.3-Codex-Spark" {
		t.Fatalf("additional = %#v, want one Spark limit", usage.AdditionalRateLimits)
	}
	if usage.Credits == nil || usage.Credits.HasCredits || usage.Credits.Balance != "0" {
		t.Fatalf("credits = %#v, want no-credits balance 0", usage.Credits)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Fatalf("Authorization = %q, want bearer", gotAuth)
	}
	if gotAccountID != "acct-123" {
		t.Fatalf("ChatGPT-Account-Id = %q, want acct-123 from JWT claim", gotAccountID)
	}
}

func TestServiceCodexUsageWithoutAccountClaim(t *testing.T) {
	var gotAccountID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccountID = r.Header.Get("ChatGPT-Account-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_after_seconds":100,"reset_at":1786000000},"secondary_window":null}}`))
	}))
	defer server.Close()

	service := newProviderSettingsTestService(t)
	createCodexTestProvider(t, service, server.URL+"/backend-api/codex", "codex-team")

	// A token whose payload has no chatgpt_account_id claim.
	service.SaveCodexAuth("codex-team", codexauth.TokenFile{
		AccessToken: fakeCodexToken(`{"user_id":"user-1"}`),
	})
	if _, err := service.CodexUsage(context.Background(), "codex-team"); err != nil {
		t.Fatalf("CodexUsage() error = %v", err)
	}
	if gotAccountID != "" {
		t.Fatalf("ChatGPT-Account-Id = %q, want empty when claim is absent", gotAccountID)
	}
}

func TestServiceCodexUsageRejectsNonCodexProvider(t *testing.T) {
	service := newProviderSettingsTestService(t)
	_, err := service.CreateProviderSettings(ProviderSettingsInput{
		Name:    "plain",
		BaseURL: "https://provider.example.test/v1",
		Models:  []ProviderModelSettings{{Profile: "main", ID: "model", Type: config.ProviderTypeOpenAIChat}},
	})
	if err != nil {
		t.Fatalf("CreateProviderSettings() error = %v", err)
	}
	if _, err := service.CodexUsage(context.Background(), "plain"); err == nil || !strings.Contains(err.Error(), "openai-codex") {
		t.Fatalf("CodexUsage(plain) error = %v, want openai-codex rejection", err)
	}
}

func TestServiceCodexUsageRedactsTokenOnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Invalid token secret-token-value"}`))
	}))
	defer server.Close()

	service := newProviderSettingsTestService(t)
	createCodexTestProvider(t, service, server.URL+"/backend-api/codex", "codex-team")
	service.SaveCodexAuth("codex-team", codexauth.TokenFile{
		AccessToken: "secret-token-value",
	})
	_, err := service.CodexUsage(context.Background(), "codex-team")
	if err == nil {
		t.Fatal("CodexUsage() error = nil, want Unauthorized error")
	}
	if strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("error leaked token: %s", err.Error())
	}
}

func TestCodexUsageURLDerivation(t *testing.T) {
	tests := []struct {
		base string
		want string
	}{
		{base: "https://chatgpt.com/backend-api/codex", want: "https://chatgpt.com/backend-api/wham/usage"},
		{base: "https://mirror.example.test/backend-api/codex/", want: "https://mirror.example.test/backend-api/wham/usage"},
		{base: "https://api.openai.com/v1", want: codexauth.DefaultUsageURL},
		{base: "", want: codexauth.DefaultUsageURL},
	}
	for _, test := range tests {
		if got := codexUsageURL(test.base); got != test.want {
			t.Fatalf("codexUsageURL(%q) = %q, want %q", test.base, got, test.want)
		}
	}
}

// createCodexTestProvider registers an openai-codex provider whose base URL is
// the fake usage server, saves a signed-in auth token with a chatgpt_account_id
// claim, and returns the provider name.
func createCodexTestProvider(t *testing.T, service *Service, baseURL, name string) {
	t.Helper()
	document, err := service.CreateProviderSettings(ProviderSettingsInput{
		Name:    name,
		BaseURL: baseURL,
		Models: []ProviderModelSettings{{
			Profile: "default",
			ID:      "gpt-5.5",
			Type:    config.ProviderTypeOpenAICodex,
			Input:   []string{"text", "image"},
		}},
	})
	if err != nil {
		t.Fatalf("CreateProviderSettings(codex) error = %v", err)
	}
	if len(document.Providers) != 1 || document.Providers[0].AuthFile == "" {
		t.Fatalf("created Codex provider = %#v, want auth path", document.Providers)
	}
	if err := service.SaveCodexAuth(name, codexauth.TokenFile{
		AccessToken: fakeCodexToken(`{"chatgpt_account_id":"acct-123","user_id":"user-1"}`),
	}); err != nil {
		t.Fatalf("SaveCodexAuth() error = %v", err)
	}
}

// fakeCodexToken builds a three-segment JWT whose payload is the given JSON.
func fakeCodexToken(payload string) string {
	return "header." + strings.TrimRight(base64.RawURLEncoding.EncodeToString([]byte(payload)), "=") + ".signature"
}
