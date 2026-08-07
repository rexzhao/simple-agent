package execution

import (
	"context"
	"encoding/json"
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
	"github.com/rexzhao/simple-agent/internal/providersettings"
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

func TestServiceProviderSettingsWriteIntentsPreserveOpaqueFields(t *testing.T) {
	service := newProviderSettingsTestService(t)
	originalParameters := map[string]any{"responses": map[string]any{"temperature": 0.2}, "opaque": "must-survive"}
	if _, err := service.CreateProviderSettings(ProviderSettingsInput{
		Name: "opaque", BaseURL: "https://user:password@example.test/v1?token=old#fragment", APIKey: "keep-me",
		AuthFile: "../auth/opaque.json", HTTPProxy: "http://proxy-user:proxy-pass@proxy.example.test:8080?token=old",
		Models: []ProviderModelSettings{{Profile: "primary", ID: "model", Parameters: originalParameters}},
	}); err != nil {
		t.Fatal(err)
	}

	// A safe projection is a write intent, not a complete durable document.
	if _, err := service.UpdateProviderSettings("opaque", ProviderSettingsInput{
		Name: "opaque", BaseURL: "https://example.test/v1", BaseURLMode: ProviderWritePreserve,
		KeepAPIKey: true, AuthFile: "opaque.json", AuthFileMode: ProviderWritePreserve,
		HTTPProxy: "http://proxy.example.test:8080", HTTPProxyMode: ProviderWritePreserve,
		HTTPSProxyMode: ProviderWritePreserve, RequestTimeout: "45s",
		Models: []ProviderModelSettings{{Profile: "primary", ID: "model", ParametersMode: ProviderWritePreserve, ParametersSourceProfile: "primary"}},
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(service.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	updated := cfg.Providers["opaque"]
	if updated.BaseURL != "https://user:password@example.test/v1?token=old#fragment" || updated.HTTPProxy != "http://proxy-user:proxy-pass@proxy.example.test:8080?token=old" || updated.AuthFile != filepath.Join(service.ServerRoot(), "auth", "opaque.json") {
		t.Fatalf("preserve update changed opaque provider fields: %#v", updated)
	}
	if !reflect.DeepEqual(updated.Models["primary"].Parameters, originalParameters) || updated.RequestTimeout != "45s" {
		t.Fatalf("preserve update parameters/timeout = %#v, want parameters retained and timeout changed", updated)
	}

	if _, err := service.UpdateProviderSettings("opaque", ProviderSettingsInput{
		Name: "opaque", BaseURL: "https://new.example.test/v2", BaseURLMode: ProviderWriteReplace,
		KeepAPIKey: true, AuthFile: "opaque.json", AuthFileMode: ProviderWritePreserve,
		HTTPProxy: "", HTTPProxyMode: ProviderWriteReplace, HTTPSProxy: "", HTTPSProxyMode: ProviderWriteReplace,
		RequestTimeout: "45s", Models: []ProviderModelSettings{{Profile: "primary", ID: "model", ParametersMode: ProviderWriteReplace, Parameters: map[string]any{"replacement": true}}},
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(service.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	replaced := cfg.Providers["opaque"]
	if replaced.BaseURL != "https://new.example.test/v2" || replaced.HTTPProxy != "" || !reflect.DeepEqual(replaced.Models["primary"].Parameters, map[string]any{"replacement": true}) {
		t.Fatalf("replace update = %#v, want explicit endpoint/parameter replacement", replaced)
	}
}

type providerSettingsRecordingSink struct {
	changes []providersettings.CommittedChange
}

func (s *providerSettingsRecordingSink) PublishCommitted(change providersettings.CommittedChange) error {
	s.changes = append(s.changes, change)
	return nil
}

func TestServiceProviderSettingsPublishesTypedPostCommitChanges(t *testing.T) {
	service := newProviderSettingsTestService(t)
	sink := &providerSettingsRecordingSink{}
	registration := service.RegisterProviderSettingsChangeSink(sink)
	if registration == nil {
		t.Fatal("provider settings registration is nil")
	}
	defer registration.Unregister()
	if _, err := service.CreateProviderSettings(ProviderSettingsInput{Name: "remote", BaseURL: "https://provider.example.test/v1", APIKey: "must-not-cross-boundary", Models: []ProviderModelSettings{{Profile: "main", ID: "model"}}}); err != nil {
		t.Fatal(err)
	}
	if len(sink.changes) != 1 || sink.changes[0].Kind != providersettings.CommittedProviderUpsert || sink.changes[0].ProviderName != "remote" {
		t.Fatalf("create publication = %#v", sink.changes)
	}
	// Repeating the exact target is a durable no-op. It must not rewrite the
	// credential-bearing file or publish a second provider resource change.
	if _, err := service.UpdateProviderSettings("remote", ProviderSettingsInput{
		Name: "remote", BaseURL: "https://provider.example.test/v1", APIKey: "", KeepAPIKey: true,
		Models: []ProviderModelSettings{{Profile: "main", ID: "model"}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(sink.changes) != 1 {
		t.Fatalf("identical provider update publication = %#v, want no additional change", sink.changes)
	}
	if _, err := service.UpdateDefaultProviderModel("remote", "main"); err != nil {
		t.Fatal(err)
	}
	if len(sink.changes) != 2 || sink.changes[1].Kind != providersettings.CommittedDefaultChanged || sink.changes[1].DefaultProvider != "remote" || sink.changes[1].DefaultModel != "main" {
		t.Fatalf("default publication = %#v", sink.changes)
	}
	if _, err := service.UpdateDefaultProviderModel("remote", "main"); err != nil {
		t.Fatal(err)
	}
	if len(sink.changes) != 2 {
		t.Fatalf("identical default update publication = %#v, want no additional change", sink.changes)
	}
}

func TestServiceProviderSettingsChangedAcknowledgementTracksHiddenKeyReplacement(t *testing.T) {
	service := newProviderSettingsTestService(t)
	sink := &providerSettingsRecordingSink{}
	registration := service.RegisterProviderSettingsChangeSink(sink)
	defer registration.Unregister()
	created, err := service.CreateProviderSettingsWithResult(ProviderSettingsInput{
		Name: "key-authority", BaseURL: "https://example.test/v1", APIKey: "old-secret",
		Models: []ProviderModelSettings{{Profile: "main", ID: "model"}},
	})
	if err != nil || !created.Changed || len(sink.changes) != 1 {
		t.Fatalf("create result = %#v, changes = %#v, err = %v", created, sink.changes, err)
	}
	target := ProviderSettingsInput{Name: "key-authority", BaseURL: "https://example.test/v1", APIKey: "new-secret", Models: []ProviderModelSettings{{Profile: "main", ID: "model"}}}
	replaced, err := service.UpdateProviderSettingsWithResult("key-authority", target)
	if err != nil || !replaced.Changed || len(sink.changes) != 2 || !sink.changes[1].PublicationExpected {
		t.Fatalf("replacement result = %#v, changes = %#v, err = %v", replaced, sink.changes, err)
	}
	encoded, marshalErr := json.Marshal(sink.changes)
	if marshalErr != nil || strings.Contains(string(encoded), "old-secret") || strings.Contains(string(encoded), "new-secret") {
		t.Fatalf("typed publication leaked API key: %s", encoded)
	}
	noOp, err := service.UpdateProviderSettingsWithResult("key-authority", target)
	if err != nil || noOp.Changed || len(sink.changes) != 2 {
		t.Fatalf("same replacement retry = %#v, changes = %#v, err = %v", noOp, sink.changes, err)
	}
	cleared, err := service.UpdateProviderSettingsWithResult("key-authority", ProviderSettingsInput{
		Name: "key-authority", BaseURL: "https://example.test/v1", Models: []ProviderModelSettings{{Profile: "main", ID: "model"}},
	})
	if err != nil || !cleared.Changed || len(sink.changes) != 3 {
		t.Fatalf("clear result = %#v, changes = %#v, err = %v", cleared, sink.changes, err)
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
		{name: "path traversal name", mutate: func(input *ProviderSettingsInput) { input.Name = "../escape" }, wantErrSub: "path separator"},
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

func TestServiceProviderSettingsMutationNameContract(t *testing.T) {
	service := newProviderSettingsTestService(t)
	providerDir := filepath.Join(service.ServerRoot(), "providers")

	// These names are valid durable provider identities. Updating them must use
	// the existing resolved path instead of applying the create-only filename
	// policy or falling back to the old ASCII-only validator.
	for _, name := range []string{"space provider", "供应商"} {
		path := filepath.Join(providerDir, name+".yaml")
		if err := config.WriteProviderConfig(path, config.ProviderConfig{
			Name:    name,
			BaseURL: "https://provider.example.test/v1",
			Models:  map[string]config.ModelProfile{"main": {ID: "before"}},
		}); err != nil {
			t.Fatalf("WriteProviderConfig(%q) error = %v", name, err)
		}

		document, err := service.ProviderSettings()
		if err != nil {
			t.Fatalf("ProviderSettings(%q) error = %v", name, err)
		}
		if !containsProvider(document, name) {
			t.Fatalf("ProviderSettings() did not expose provider %q: %#v", name, document.Providers)
		}

		updated, err := service.UpdateProviderSettings(name, ProviderSettingsInput{
			Name:    name,
			BaseURL: "https://provider.example.test/v2",
			Models:  []ProviderModelSettings{{Profile: "main", ID: "after"}},
		})
		if err != nil {
			t.Fatalf("UpdateProviderSettings(%q) error = %v", name, err)
		}
		provider := findProvider(updated, name)
		if provider == nil || provider.Models[0].ID != "after" {
			t.Fatalf("updated provider %q = %#v, want model after", name, provider)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", path, err)
		}
		if !strings.Contains(string(data), "id: after") {
			t.Fatalf("updated provider file %q = %s, want existing path updated", path, data)
		}
	}

	beforeEntries, err := os.ReadDir(providerDir)
	if err != nil {
		t.Fatalf("ReadDir(before invalid creates) error = %v", err)
	}
	for _, name := range []string{"CON", "bad:name", "bad.", "bad "} {
		_, err := service.CreateProviderSettings(ProviderSettingsInput{
			Name:    name,
			BaseURL: "https://provider.example.test/v1",
			Models:  []ProviderModelSettings{{Profile: "main", ID: "model"}},
		})
		if err == nil {
			t.Fatalf("CreateProviderSettings(%q) succeeded, want cross-platform filename rejection", name)
		}
		entries, readErr := os.ReadDir(providerDir)
		if readErr != nil {
			t.Fatalf("ReadDir(after invalid create %q) error = %v", name, readErr)
		}
		for _, entry := range entries {
			if entry.Name() == name+".yaml" {
				t.Fatalf("invalid create %q left a provider file", name)
			}
		}
	}
	afterEntries, err := os.ReadDir(providerDir)
	if err != nil {
		t.Fatalf("ReadDir(after invalid creates) error = %v", err)
	}
	if len(afterEntries) != len(beforeEntries) {
		t.Fatalf("invalid creates changed provider directory: before=%v after=%v", beforeEntries, afterEntries)
	}

	// Ordinary ASCII creation remains the normal path and publishes only the
	// typed post-commit identity, not the write DTO.
	sink := &providerSettingsRecordingSink{}
	registration := service.RegisterProviderSettingsChangeSink(sink)
	if registration == nil {
		t.Fatal("provider settings registration is nil")
	}
	defer registration.Unregister()
	if _, err := service.CreateProviderSettings(ProviderSettingsInput{
		Name:    "ordinary",
		BaseURL: "https://provider.example.test/v1",
		Models:  []ProviderModelSettings{{Profile: "main", ID: "model"}},
	}); err != nil {
		t.Fatalf("CreateProviderSettings(ordinary) error = %v", err)
	}
	if len(sink.changes) != 1 || sink.changes[0].Kind != providersettings.CommittedProviderUpsert || sink.changes[0].ProviderName != "ordinary" {
		t.Fatalf("ordinary create publication = %#v", sink.changes)
	}
}

func containsProvider(document ProviderSettingsDocument, name string) bool {
	return findProvider(document, name) != nil
}

func findProvider(document ProviderSettingsDocument, name string) *ProviderSettings {
	for index := range document.Providers {
		if document.Providers[index].Name == name {
			return &document.Providers[index]
		}
	}
	return nil
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
