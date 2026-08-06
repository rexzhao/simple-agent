package providersettings

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

type providerFixture struct {
	root       string
	configPath string
}

func newProviderFixture(t *testing.T) providerFixture {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"providers", "auth", "mcp"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "sai.yaml")
	if err := os.WriteFile(configPath, []byte("default_provider: alpha\ndefault_model: fast\nprovider_dir: providers\nauth_dir: auth\nmcp_dir: mcp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeProviderFixture(t, root, "alpha", `name: alpha
base_url: "https://user:base-secret@example.test/v1?token=url-secret"
api_key: "api-key-secret"
auth_file: "../auth/alpha.json"
request_timeout: 30s
http_proxy: "http://proxy-user:proxy-secret@proxy.example.test:8080?token=proxy-secret"
models:
  fast:
    id: alpha-fast
    input: [text]
    parameters:
      api_key: parameter-secret
      safe_option: visible
    reasoning_config:
      parameter: effort
      default: medium
      levels:
        low: low
        medium: medium
        budget_tokens: 1234
        max_tokens: true
        no_limit: null
`)
	writeProviderFixture(t, root, "space-provider", `name: "space provider"
base_url: https://space.example.test/v1
models:
  default:
    id: space-default
`)
	return providerFixture{root: root, configPath: configPath}
}

func writeProviderFixture(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "providers", name+".yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadFixtureSnapshot(t *testing.T, fixture providerFixture) ProviderSettingsSnapshot {
	t.Helper()
	cfg, err := config.Load(fixture.configPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotFromConfig(*cfg, fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func openFixture(t *testing.T, provider *Provider, resume *protocol.ResumeToken) (opened syncengine.OpenedResource) {
	t.Helper()
	opened, err := provider.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeProviderSettings, ID: ResourceID}, resume)
	if err != nil {
		t.Fatal(err)
	}
	return opened
}

func TestSnapshotWhitelistDeterministicAndStrict(t *testing.T) {
	fixture := newProviderFixture(t)
	cfg, err := config.Load(fixture.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Providers["space provider"]; !ok {
		t.Fatal("fixture did not prove config.Load accepts internal-space provider name")
	}
	snapshot := loadFixtureSnapshot(t, fixture)
	first, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("provider settings snapshot is not deterministic")
	}
	for _, secret := range []string{"api-key-secret", "base-secret", "url-secret", "proxy-secret", "parameter-secret", "CodexAuth", `"api_key":`, `"parameters":`} {
		if bytes.Contains(first, []byte(secret)) {
			t.Fatalf("snapshot leaked %q: %s", secret, first)
		}
	}
	if !bytes.Contains(first, []byte(`"api_key_configured":true`)) {
		t.Fatalf("safe API-key presence metadata missing: %s", first)
	}
	var wire map[string]any
	if err := json.Unmarshal(first, &wire); err != nil {
		t.Fatal(err)
	}
	levels := wire["providers"].([]any)[0].(map[string]any)["models"].([]any)[0].(map[string]any)["reasoning_config"].(map[string]any)["levels"].([]any)
	levelValues := map[string]any{}
	for _, raw := range levels {
		item := raw.(map[string]any)
		levelValues[item["name"].(string)] = item["value"]
	}
	if levelValues["budget_tokens"] != float64(1234) || levelValues["max_tokens"] != true || levelValues["no_limit"] != nil {
		t.Fatalf("reasoning scalar types were not preserved: %#v", levelValues)
	}
	var decoded ProviderSettingsSnapshot
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("strict snapshot decode: %v", err)
	}
	for _, invalid := range []string{
		strings.TrimSuffix(string(first), "}") + `,"extra":true}`,
		string(first) + ` {}`,
		`{"server_root":"","server_root":"","config_path":"","default_provider":"","default_model":"","providers":[]}`,
		string(bytes.Replace(first, []byte(`"value":"low"`), []byte(`"value":{"nested":true}`), 1)),
	} {
		var value ProviderSettingsSnapshot
		if err := json.Unmarshal([]byte(invalid), &value); err == nil {
			t.Fatalf("strict decoder accepted invalid JSON: %s", invalid)
		}
	}
}

type captureProviderBlobWriter struct{ content []byte }

func (w *captureProviderBlobWriter) Put(_ context.Context, contentType string, content []byte) (protocol.BlobDescriptor, error) {
	w.content = append([]byte(nil), content...)
	digest := sha256.Sum256(content)
	hexDigest := hex.EncodeToString(digest[:])
	return protocol.BlobDescriptor{ID: "provider-settings", URL: "/api/blobs/provider-settings", ContentType: contentType, Size: uint64(len(content)), SHA256: hexDigest, ETag: `"` + hexDigest + `"`, ExpiresAt: "2099-01-01T00:00:00Z"}, nil
}

func TestProviderBlobSnapshotContainsOnlySafeDTO(t *testing.T) {
	fixture := newProviderFixture(t)
	blobs := &captureProviderBlobWriter{}
	provider, err := NewProvider(ProviderOptions{ConfigPath: fixture.configPath, ServerRoot: fixture.root, StreamEpoch: "blob-provider", InlineSnapshotBytes: 1, BlobWriter: blobs})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if err := provider.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened, err := provider.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeProviderSettings, ID: ResourceID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if _, ok := opened.Snapshot.Content.BlobDescriptor(); !ok {
		t.Fatal("provider settings snapshot did not use configured Blob writer")
	}
	for _, secret := range []string{"api-key-secret", "base-secret", "url-secret", "proxy-secret", "parameter-secret"} {
		if bytes.Contains(blobs.content, []byte(secret)) {
			t.Fatalf("Blob snapshot leaked %q", secret)
		}
	}
}

func TestProviderSettingsWireNameAndIntegerContract(t *testing.T) {
	if err := ValidateProviderName("space provider"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProviderName("团队 provider"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{" leading", "trailing ", "a/b", "a\\b", "\x00bad"} {
		if err := ValidateProviderName(name); err == nil {
			t.Fatalf("accepted invalid provider name %q", name)
		}
	}
	valid := ProviderSettingsSnapshot{
		Providers: []ProviderSettings{{
			Name:                  "space provider",
			MaxConcurrentRequests: MaxWireInteger,
			Models: []ProviderModelSettings{{
				Profile:         "default",
				ContextWindow:   MaxWireInteger,
				InputLimit:      0,
				OutputLimit:     0,
				ReasoningConfig: ReasoningMetadata{Levels: []ReasoningLevel{}},
			}},
		}},
	}
	if _, err := json.Marshal(valid); err != nil {
		t.Fatalf("maximum wire integers should encode: %v", err)
	}
	for _, mutate := range []func(*ProviderSettingsSnapshot){
		func(value *ProviderSettingsSnapshot) { value.Providers[0].MaxConcurrentRequests = MaxWireInteger + 1 },
		func(value *ProviderSettingsSnapshot) { value.Providers[0].Models[0].ContextWindow = -1 },
	} {
		value := valid
		value.Providers = append([]ProviderSettings(nil), valid.Providers...)
		value.Providers[0].Models = append([]ProviderModelSettings(nil), valid.Providers[0].Models...)
		mutate(&value)
		if _, err := json.Marshal(value); err == nil {
			t.Fatal("encoded out-of-bound wire integer")
		}
	}
	defaultOperation := Operation{Op: OperationReplaceDefault, Key: "space provider", Default: &DefaultSelection{Provider: "space provider", Model: "default"}}
	if err := defaultOperation.Validate(); err == nil {
		t.Fatal("accepted default operation with non-server key")
	}
}

func TestProviderRuntimePublishesUpsertRemoveDefaultAndNoop(t *testing.T) {
	fixture := newProviderFixture(t)
	provider, err := NewProvider(ProviderOptions{ConfigPath: fixture.configPath, ServerRoot: fixture.root, StreamEpoch: "test-provider", JournalEntries: 2, JournalBytes: 64 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if err := provider.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened, err := provider.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeProviderSettings, ID: ResourceID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if opened.Sequence != 0 || opened.Snapshot.ResourceRevision != "0" {
		t.Fatalf("initial barrier = epoch %q sequence %d revision %q", opened.StreamEpoch, opened.Sequence, opened.Snapshot.ResourceRevision)
	}
	writeProviderFixture(t, fixture.root, "alpha", `name: alpha
base_url: https://example.test/v1
api_key: api-key-secret
models:
  fast:
    id: alpha-fast
  slow:
    id: alpha-slow
`)
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedProviderUpsert, ProviderName: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	upsert := <-opened.Changes
	if upsert.Sequence != 1 || len(upsert.Change.Operations) != 1 {
		t.Fatalf("upsert entry = %#v", upsert)
	}
	if len(upsert.Change.Operations) != 1 || upsert.Change.Operations[0].Op != OperationUpsertDefault {
		t.Fatalf("upsert operations = %#v", upsert.Change.Operations)
	}
	if bytes.Contains(upsert.Change.Operations[0].Raw, []byte("api-key-secret")) {
		t.Fatal("upsert leaked API key")
	}
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedProviderUpsert, ProviderName: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err := provider.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeProviderSettings, ID: ResourceID}, &protocol.ResumeToken{StreamEpoch: opened.StreamEpoch, Sequence: "1"})
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if current.Sequence != 1 || current.Decision.Action != syncengine.SyncActionCurrent {
		t.Fatalf("no-op resume = %#v", current.Decision)
	}

	data, err := os.ReadFile(fixture.configPath)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("default_model: fast"), []byte("default_model: slow"), 1)
	if err := os.WriteFile(fixture.configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedDefaultChanged, DefaultProvider: "alpha", DefaultModel: "slow"}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	defaultChange := <-opened.Changes
	if len(defaultChange.Change.Operations) != 1 || defaultChange.Change.Operations[0].Op != OperationReplaceDefault {
		t.Fatalf("default operations = %#v", defaultChange.Change.Operations)
	}

	if err := os.Remove(filepath.Join(fixture.root, "providers", "alpha.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedProviderRemove, ProviderName: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	remove := <-opened.Changes
	if len(remove.Change.Operations) != 1 || remove.Change.Operations[0].Op != OperationRemove {
		t.Fatalf("remove operations = %#v", remove.Change.Operations)
	}
}

func TestProviderReconnectInvalidationAndJournalOverflow(t *testing.T) {
	fixture := newProviderFixture(t)
	provider, err := NewProvider(ProviderOptions{ConfigPath: fixture.configPath, ServerRoot: fixture.root, StreamEpoch: "overflow-provider", JournalEntries: 2, JournalBytes: 64 * 1024, LiveCapacity: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if err := provider.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened, err := provider.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeProviderSettings, ID: ResourceID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldEpoch := opened.StreamEpoch
	defer opened.Close()

	for index, model := range []string{"one", "two", "three"} {
		content := `name: alpha
base_url: https://example.test/v1
models:
  ` + model + `:
    id: alpha-` + model + `
`
		writeProviderFixture(t, fixture.root, "alpha", content)
		if err := provider.PublishCommitted(CommittedChange{Kind: CommittedProviderUpsert, ProviderName: "alpha"}); err != nil {
			t.Fatal(err)
		}
		if err := provider.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		entry := <-opened.Changes
		if entry.Sequence != uint64(index+1) {
			t.Fatalf("entry sequence = %d, want %d", entry.Sequence, index+1)
		}
	}
	oldResume, err := provider.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeProviderSettings, ID: ResourceID}, &protocol.ResumeToken{StreamEpoch: oldEpoch, Sequence: "0"})
	if err != nil {
		t.Fatal(err)
	}
	defer oldResume.Close()
	if oldResume.Decision.Classification != syncengine.ResumeTooOld || oldResume.Decision.Action != syncengine.SyncActionResync {
		t.Fatalf("overflow decision = %#v", oldResume.Decision)
	}

	if err := provider.Invalidate("test_rebuild"); err != nil {
		t.Fatal(err)
	}
	select {
	case terminal := <-opened.Terminal:
		if terminal.Reason != syncengine.LiveTerminalSequence {
			t.Fatalf("invalidation terminal = %#v", terminal)
		}
	case <-time.After(time.Second):
		t.Fatal("invalidation terminal was not delivered")
	}
	rebuilt, err := provider.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeProviderSettings, ID: ResourceID}, &protocol.ResumeToken{StreamEpoch: oldEpoch, Sequence: "3"})
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	if rebuilt.StreamEpoch == oldEpoch || rebuilt.Sequence != 0 || rebuilt.Decision.Classification != syncengine.ResumeEpochMismatch {
		t.Fatalf("rebuild barrier = epoch %q sequence %d decision %#v", rebuilt.StreamEpoch, rebuilt.Sequence, rebuilt.Decision)
	}
}
