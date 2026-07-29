package execution

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/codexauth"
	"github.com/rexzhao/simple-agent/internal/config"
)

func TestServiceProviderSettingsLifecycleAndModelDiscovery(t *testing.T) {
	type observedRequest struct {
		Path          string
		Authorization string
	}
	requests := make(chan observedRequest, 1)
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- observedRequest{Path: r.URL.Path, Authorization: r.Header.Get("Authorization")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"zeta"},{"id":"alpha"},{"id":"alpha"},{"id":"  "}]}`))
	}))
	defer providerServer.Close()

	service := newProviderSettingsTestService(t)
	input := ProviderSettingsInput{
		Name:                  "remote",
		BaseURL:               providerServer.URL + "/v1/",
		APIKey:                "discovery-secret",
		RequestTimeout:        "45s",
		MaxConcurrentRequests: 3,
		Models: []ProviderModelSettings{
			{Profile: "main", ID: "configured-main", Type: config.ProviderTypeOpenAIChat, Input: []string{"text", "image"}, DeveloperRole: "developer"},
			{Profile: "text", ID: "configured-text", Type: config.ProviderTypeOpenAIChat, Input: []string{"text"}},
		},
	}
	document, err := service.CreateProviderSettings(input)
	if err != nil {
		t.Fatalf("CreateProviderSettings() error = %v", err)
	}
	if len(document.Providers) != 1 {
		t.Fatalf("CreateProviderSettings() providers = %#v, want one", document.Providers)
	}
	created := document.Providers[0]
	if created.Name != input.Name || created.APIKey != "" || !created.APIKeyConfigured || len(created.Models) != 2 {
		t.Fatalf("created provider = %#v, want hidden configured API key and two models", created)
	}
	if created.MaxConcurrentRequests != 3 {
		t.Fatalf("created provider MaxConcurrentRequests = %d, want 3", created.MaxConcurrentRequests)
	}

	input.APIKey = ""
	input.KeepAPIKey = true
	input.Models[0].ID = "updated-main"
	document, err = service.UpdateProviderSettings("remote", input)
	if err != nil {
		t.Fatalf("UpdateProviderSettings() error = %v", err)
	}
	if document.Providers[0].Models[0].ID != "updated-main" || !document.Providers[0].APIKeyConfigured {
		t.Fatalf("updated provider = %#v, want updated model and retained API key", document.Providers[0])
	}
	if document.Providers[0].MaxConcurrentRequests != 3 {
		t.Fatalf("updated provider MaxConcurrentRequests = %d, want 3", document.Providers[0].MaxConcurrentRequests)
	}

	document, err = service.UpdateDefaultProviderModel("remote", "main")
	if err != nil {
		t.Fatalf("UpdateDefaultProviderModel() error = %v", err)
	}
	if document.DefaultProvider != "remote" || document.DefaultModel != "main" {
		t.Fatalf("updated defaults = %q/%q, want remote/main", document.DefaultProvider, document.DefaultModel)
	}

	models, err := service.DiscoverProviderModels(context.Background(), "remote")
	if err != nil {
		t.Fatalf("DiscoverProviderModels() error = %v", err)
	}
	if !reflect.DeepEqual(models, []string{"alpha", "zeta"}) {
		t.Fatalf("DiscoverProviderModels() = %#v, want sorted unique IDs", models)
	}
	select {
	case request := <-requests:
		if request.Path != "/v1/models" || request.Authorization != "Bearer discovery-secret" {
			t.Fatalf("model discovery request = %#v, want authenticated /v1/models", request)
		}
	case <-time.After(time.Second):
		t.Fatal("provider did not receive model discovery request")
	}

	data, err := os.ReadFile(filepath.Join(service.ServerRoot(), "providers", "remote.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(remote provider) error = %v", err)
	}
	if !strings.Contains(string(data), "api_key: discovery-secret") || !strings.Contains(string(data), "id: updated-main") {
		t.Fatalf("persisted provider = %s, want retained secret and updated model", data)
	}
	if !strings.Contains(string(data), "max_concurrent_requests: 3") {
		t.Fatalf("persisted provider = %s, want max_concurrent_requests: 3", data)
	}
	if _, err := service.CreateProviderSettings(input); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("CreateProviderSettings(duplicate) error = %v, want duplicate rejection", err)
	}
}

func TestServiceProviderSettingsCodexAuthLifecycle(t *testing.T) {
	service := newProviderSettingsTestService(t)
	document, err := service.CreateProviderSettings(ProviderSettingsInput{
		Name:    "codex-team",
		BaseURL: "https://chatgpt.example.test/backend-api/codex",
		Models: []ProviderModelSettings{{
			Profile: "default",
			ID:      "gpt-5.4",
			Type:    config.ProviderTypeOpenAICodex,
			Input:   []string{"text", "image"},
		}},
	})
	if err != nil {
		t.Fatalf("CreateProviderSettings(codex) error = %v", err)
	}
	if len(document.Providers) != 1 || document.Providers[0].AuthFile == "" || document.Providers[0].CodexAuth == nil || document.Providers[0].CodexAuth.Status != "signed_out" {
		t.Fatalf("created Codex provider = %#v, want auth path and signed_out status", document.Providers)
	}

	expiresAt := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	err = service.SaveCodexAuth("codex-team", codexauth.TokenFile{
		AccessToken:  "access-secret",
		RefreshToken: "refresh-secret",
		AccountID:    "account-123",
		ExpiresAt:    expiresAt,
	})
	if err != nil {
		t.Fatalf("SaveCodexAuth() error = %v", err)
	}
	status, err := service.CodexAuthStatus("codex-team")
	if err != nil {
		t.Fatalf("CodexAuthStatus() error = %v", err)
	}
	if status.Status != "signed_in" || status.AccountID != "account-123" || !status.Refreshable || !status.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("CodexAuthStatus() = %#v, want signed-in refreshable account", status)
	}
	if err := service.ClearCodexAuth("codex-team"); err != nil {
		t.Fatalf("ClearCodexAuth() error = %v", err)
	}
	status, err = service.CodexAuthStatus("codex-team")
	if err != nil {
		t.Fatalf("CodexAuthStatus(after clear) error = %v", err)
	}
	if status.Status != "signed_out" {
		t.Fatalf("CodexAuthStatus(after clear) = %#v, want signed_out", status)
	}
	models, err := service.DiscoverProviderModels(context.Background(), "codex-team")
	if err != nil {
		t.Fatalf("DiscoverProviderModels(codex) error = %v", err)
	}
	if !containsString(models, "gpt-5.4") || !containsString(models, "gpt-5.5") {
		t.Fatalf("DiscoverProviderModels(codex) = %#v, want configured and built-in IDs", models)
	}
}

func TestServiceProviderSettingsRejectsInvalidDocuments(t *testing.T) {
	service := newProviderSettingsTestService(t)
	valid := func() ProviderSettingsInput {
		return ProviderSettingsInput{
			Name:    "valid",
			BaseURL: "https://provider.example.test/v1",
			Models:  []ProviderModelSettings{{Profile: "main", ID: "model", Type: config.ProviderTypeOpenAIChat}},
		}
	}
	tests := []struct {
		name       string
		mutate     func(*ProviderSettingsInput)
		wantErrSub string
	}{
		{name: "path traversal name", mutate: func(input *ProviderSettingsInput) { input.Name = "../escape" }, wantErrSub: "path separators"},
		{name: "missing base URL", mutate: func(input *ProviderSettingsInput) { input.BaseURL = "" }, wantErrSub: "base_url is required"},
		{name: "missing models", mutate: func(input *ProviderSettingsInput) { input.Models = nil }, wantErrSub: "at least one model"},
		{name: "duplicate model profile", mutate: func(input *ProviderSettingsInput) { input.Models = append(input.Models, input.Models[0]) }, wantErrSub: "duplicate model profile"},
		{name: "unsupported model input", mutate: func(input *ProviderSettingsInput) { input.Models[0].Input = []string{"audio"} }, wantErrSub: "unsupported modality"},
		{name: "invalid developer role", mutate: func(input *ProviderSettingsInput) { input.Models[0].DeveloperRole = "admin" }, wantErrSub: "developer_role"},
		{name: "incompatible compatibility", mutate: func(input *ProviderSettingsInput) {
			input.Models[0].Type = config.ProviderTypeAnthropicMessages
			input.Models[0].Compatibility = "openai"
		}, wantErrSub: "compatibility is only supported"},
		{name: "auth file outside auth root", mutate: func(input *ProviderSettingsInput) { input.AuthFile = "../outside.json" }, wantErrSub: "auth_file must stay inside"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := valid()
			test.mutate(&input)
			_, err := service.CreateProviderSettings(input)
			if err == nil || !strings.Contains(err.Error(), test.wantErrSub) {
				t.Fatalf("CreateProviderSettings() error = %v, want substring %q", err, test.wantErrSub)
			}
		})
	}
}

func newProviderSettingsTestService(t *testing.T) *Service {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "providers"), 0o700); err != nil {
		t.Fatalf("MkdirAll(providers) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "auth"), 0o700); err != nil {
		t.Fatalf("MkdirAll(auth) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sai.yaml"), []byte("provider_dir: providers\nauth_dir: auth\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	service, err := NewService(root)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
