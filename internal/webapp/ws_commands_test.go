package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/codexauth"
	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/protocol"
)

func TestSessionCommandSchemasAreStrict(t *testing.T) {
	tests := []struct {
		name     string
		validate func(json.RawMessage) error
		valid    json.RawMessage
		invalid  []json.RawMessage
	}{
		{
			name: "mark_read", validate: validateSessionMarkReadArguments,
			valid: json.RawMessage(`{"session_id":"s","run_id":"r","project_id":"p"}`),
			invalid: []json.RawMessage{
				json.RawMessage(`{"session_id":"s","run_id":"r","project_id":null}`),
				json.RawMessage(`{"session_id":"s","run_id":"r","unknown":true}`),
				json.RawMessage(`{"session_id":"s","run_id":"r"} {}`),
				json.RawMessage(`{"session_id":"s","run_id":"r","session_id":"other"}`),
			},
		},
		{
			name: "create", validate: validateSessionCreateArguments,
			valid: json.RawMessage(`{"session_id":"session_s","project_id":"project_p","display_name":"new","full_access":false}`),
			invalid: []json.RawMessage{
				json.RawMessage(`{"session_id":"session_s","project_id":"project_p","unknown":true}`),
				json.RawMessage(`{"session_id":"session_s","project_id":"project_p","session_id":"other"}`),
				json.RawMessage(`{"session_id":"../escape","project_id":"project_p"}`),
				json.RawMessage(`{"session_id":"session_s","project_id":"project_p"} trailing`),
			},
		},
		{
			name: "rename", validate: validateSessionRenameArguments,
			valid: json.RawMessage(`{"session_id":"s","display_name":"new name"}`),
			invalid: []json.RawMessage{
				json.RawMessage(`{"session_id":"s","display_name":"new","unknown":1}`),
				json.RawMessage(`{"session_id":"s","display_name":1}`),
				json.RawMessage(`{"session_id":"","display_name":"new"}`),
				json.RawMessage(`{"session_id":"s","display_name":"new"} {}`),
			},
		},
		{
			name: "archive", validate: func(raw json.RawMessage) error { return validateSessionIDArguments(raw, "session.archive") },
			valid:   json.RawMessage(`{"session_id":"s"}`),
			invalid: []json.RawMessage{json.RawMessage(`{"session_id":"s","extra":true}`), json.RawMessage(`{"session_id":null}`), json.RawMessage(`{"session_id":9007199254740993}`)},
		},
		{
			name: "restore", validate: func(raw json.RawMessage) error { return validateSessionIDArguments(raw, "session.restore") },
			valid:   json.RawMessage(`{"session_id":"s"}`),
			invalid: []json.RawMessage{json.RawMessage(`{"session_id":"s","extra":true}`), json.RawMessage(`{"session_id":" "}`)},
		},
		{
			name: "delete", validate: func(raw json.RawMessage) error { return validateSessionIDArguments(raw, "session.delete") },
			valid:   json.RawMessage(`{"session_id":"s"}`),
			invalid: []json.RawMessage{json.RawMessage(`{"session_id":"s","session_id":"other"}`), json.RawMessage(`{"session_id":"s","extra":true}`), json.RawMessage(`{"session_id":"../escape"}`), json.RawMessage(`{"session_id":1}`), json.RawMessage(`{"session_id":"s"} trailing`)},
		},
		{
			name: "compact", validate: func(raw json.RawMessage) error { return validateSessionIDArguments(raw, "session.compact") },
			valid:   json.RawMessage(`{"session_id":"s"}`),
			invalid: []json.RawMessage{json.RawMessage(`{"session_id":""}`), json.RawMessage(`{"session_id":"."}`), json.RawMessage(`{"session_id":"s","unknown":false}`), json.RawMessage(`{"session_id":"s"}{}`)},
		},
		{
			name: "history_read", validate: validateSessionHistoryReadArguments,
			valid: json.RawMessage(`{"session_id":"s","cursor":12,"direction":"before","limit":20,"align_turn":true}`),
			invalid: []json.RawMessage{
				json.RawMessage(`{"session_id":"s","unknown":true}`),
				json.RawMessage(`{"session_id":"s","session_id":"other"}`),
				json.RawMessage(`{"session_id":"s","cursor":1}`),
				json.RawMessage(`{"session_id":"s","direction":"after"}`),
				json.RawMessage(`{"session_id":"s","cursor":0,"direction":"before"}`),
				json.RawMessage(`{"session_id":"s","cursor":1,"direction":"sideways"}`),
				json.RawMessage(`{"session_id":"s","limit":0}`),
				json.RawMessage(`{"session_id":"s","limit":201}`),
				json.RawMessage(`{"session_id":"s","align_turn":"yes"}`),
				json.RawMessage(`{"session_id":"s"} trailing`),
				json.RawMessage(`{"session_id":""}`),
			},
		},
		{
			name: "full_access", validate: validateSessionFullAccessArguments,
			valid: json.RawMessage(`{"session_id":"s","full_access":false}`),
			invalid: []json.RawMessage{
				json.RawMessage(`{"session_id":"s","full_access":1}`),
				json.RawMessage(`{"session_id":"s","full_access":null}`),
				json.RawMessage(`{"session_id":"s","full_access":true,"extra":false}`),
			},
		},
		{
			name: "debug", validate: validateSessionDebugArguments,
			valid:   json.RawMessage(`{"session_id":"s","request_bodies":true}`),
			invalid: []json.RawMessage{json.RawMessage(`{"session_id":"s","request_bodies":9007199254740993}`), json.RawMessage(`{"session_id":"s","request_bodies":null}`), json.RawMessage(`{"session_id":"s","request_bodies":true} trailing`)},
		},
		{
			name: "run_cancel", validate: validateRunCancelArguments,
			valid:   json.RawMessage(`{"run_id":"run"}`),
			invalid: []json.RawMessage{json.RawMessage(`{"run_id":""}`), json.RawMessage(`{"run_id":"run","extra":true}`), json.RawMessage(`{"run_id":"run"}{}`)},
		},
		{
			name: "run_start", validate: validateRunStartArguments,
			valid:   json.RawMessage(`{"session_id":"session","run_id":"run","content":"hello"}`),
			invalid: []json.RawMessage{json.RawMessage(`{"session_id":"session","run_id":"run","content":""}`), json.RawMessage(`{"session_id":"session","run_id":"run","content":"hello","images":[]}`), json.RawMessage(`{"session_id":"session","run_id":"run","content":"hello"} trailing`), json.RawMessage(`{"session_id":"session","run_id":"run","content":"hello","content":"again"}`), json.RawMessage(`{"session_id":"session","run_id":"run","content":"` + strings.Repeat("x", maxRunStartContentBytes+1) + `"}`)},
		},
		{
			name: "run_continue", validate: validateRunContinueArguments,
			valid:   json.RawMessage(`{"session_id":"session","run_id":"run"}`),
			invalid: []json.RawMessage{json.RawMessage(`{"session_id":"session","run_id":"run","content":"new"}`), json.RawMessage(`{"session_id":"session","run_id":"run","blob":null}`), json.RawMessage(`{"session_id":"session","run_id":"run"} trailing`), json.RawMessage(`{"session_id":"session","run_id":"run","run_id":"other"}`), json.RawMessage(`{"session_id":"session","run_id":1}`), json.RawMessage(`{"session_id":"session","run_id":"run","content":null}`)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.validate(test.valid); err != nil {
				t.Fatalf("valid arguments rejected: %v", err)
			}
			for _, invalid := range test.invalid {
				if err := test.validate(invalid); err == nil {
					t.Fatalf("invalid arguments accepted: %s", invalid)
				}
			}
		})
	}
}

func providerUpdateTestArguments() json.RawMessage {
	return json.RawMessage(`{"provider":"空 白 provider","base_url":"https://example.test/v1","base_url_mode":"replace","api_key":"command-secret","keep_api_key":false,"auth_file":"","auth_file_mode":"replace","request_timeout":"30s","http_proxy":"","http_proxy_mode":"replace","https_proxy":"","https_proxy_mode":"replace","max_concurrent_requests":3,"models":[{"profile":"主 模型","id":"model-主","type":"","compatibility":"","input":["text"],"developer_role":"","context_window":1000,"input_limit":900,"output_limit":100,"parameters_mode":"replace","parameters":{"temperature":0.2,"enabled":true,"nested":{"values":[null,2]}},"reasoning_config":{"parameter":"reasoning_effort","default":"medium","levels":{"low":"low","medium":2,"off":false,"none":null}},"pricing":{"input_cache_hit":0.1,"input_cache_miss":0.2,"cache_write":0.3,"output":0.4,"currency":"USD","long_context_threshold":10000,"long_context":null}}]}`)
}

func providerCreateTestArguments() json.RawMessage {
	update := providerUpdateTestArguments()
	return json.RawMessage(`{"operation_id":"operation-provider-create",` + string(update[1:]))
}

func TestProviderCommandSchemasAreStrictAndBounded(t *testing.T) {
	valid := providerUpdateTestArguments()
	if err := validateProviderUpdateArguments(valid); err != nil {
		t.Fatalf("valid provider.update arguments rejected: %v", err)
	}
	preserve := json.RawMessage(`{"provider":"p","base_url":"https://example.test","base_url_mode":"preserve","auth_file_mode":"preserve","http_proxy_mode":"preserve","https_proxy_mode":"preserve","models":[{"profile":"renamed","parameters_mode":"preserve","parameters_source_profile":"old"}]}`)
	decodedPreserve, err := decodeProviderUpdateArguments(preserve)
	if err != nil || decodedPreserve.Input.Models[0].ParametersMode != execution.ProviderWritePreserve || decodedPreserve.Input.Models[0].ParametersSourceProfile != "old" || decodedPreserve.Input.Models[0].Parameters != nil {
		t.Fatalf("preserve provider target = %#v, err=%v", decodedPreserve, err)
	}
	emptyEndpoint := json.RawMessage(`{"provider":"p","base_url":"","base_url_mode":"preserve","auth_file_mode":"preserve","http_proxy_mode":"preserve","https_proxy_mode":"preserve","models":[{"profile":"m","parameters_mode":"preserve","parameters_source_profile":"m"}]}`)
	if decoded, decodeErr := decodeProviderUpdateArguments(emptyEndpoint); decodeErr != nil || decoded.Input.BaseURL != "" || decoded.Input.BaseURLMode != execution.ProviderWritePreserve {
		t.Fatalf("empty safe endpoint preserve target = %#v, err=%v", decoded, decodeErr)
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","parameters_mode":"preserve","parameters":{}}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","parameters_mode":"replace"}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","parameters_mode":"invalid","parameters":{}}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","base_url_mode":"replace","auth_file_mode":"replace","http_proxy_mode":"replace","https_proxy_mode":"replace","models":[{"profile":"m","parameters":{}}]}`),
	} {
		if err := validateProviderUpdateArguments(raw); err == nil {
			t.Fatalf("provider.update accepted unsafe parameter write intent: %s", raw)
		}
	}
	decoded, err := decodeProviderUpdateArguments(valid)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Provider != "空 白 provider" || len(decoded.Input.Models) != 1 || decoded.Input.Models[0].ID != "model-主" {
		t.Fatalf("provider update identity/model was not preserved: %#v", decoded.Input.Models[0])
	}
	model := decoded.Input.Models[0]
	if got, ok := model.Parameters["temperature"].(float64); !ok || got != 0.2 {
		t.Fatalf("parameter scalar = %#v, want float64 0.2", model.Parameters["temperature"])
	}
	if got, ok := model.ReasoningConfig.Levels["medium"].(int64); !ok || got != 2 {
		t.Fatalf("reasoning scalar = %#v, want int64 2", model.ReasoningConfig.Levels["medium"])
	}
	if model.ReasoningConfig.Levels["off"] != false || model.ReasoningConfig.Levels["none"] != nil {
		t.Fatalf("reasoning scalar union was not preserved: %#v", model.ReasoningConfig.Levels)
	}
	validSurrogate := json.RawMessage(`{"provider":"p","base_url":"https://example.test","base_url_mode":"replace","auth_file_mode":"replace","http_proxy_mode":"replace","https_proxy_mode":"replace","models":[{"profile":"m","parameters_mode":"replace","parameters":{"\ud83d\ude00":"\ud83d\ude00","nested":[1,0.25,1e-3]}}]}`)
	surrogateDecoded, err := decodeProviderUpdateArguments(validSurrogate)
	if err != nil || surrogateDecoded.Input.Models[0].Parameters["😀"] != "😀" {
		t.Fatalf("valid surrogate pair was rejected or changed: %#v / %v", surrogateDecoded, err)
	}
	validNumbers := json.RawMessage(`{"provider":"p","base_url":"https://example.test","base_url_mode":"replace","auth_file_mode":"replace","http_proxy_mode":"replace","https_proxy_mode":"replace","models":[{"profile":"m","parameters_mode":"replace","parameters":{"max":9007199254740991,"min":-9007199254740991,"fraction":0.125,"exponent":1e-3,"replacement":"\ufffd"},"reasoning_config":{"parameter":"","default":"","levels":{"max":9007199254740991,"fraction":0.5}}}]}`)
	numberDecoded, err := decodeProviderUpdateArguments(validNumbers)
	if err != nil {
		t.Fatalf("valid provider number boundaries rejected: %v", err)
	}
	parameters := numberDecoded.Input.Models[0].Parameters
	if got, ok := parameters["max"].(int64); !ok || got != 9007199254740991 {
		t.Fatalf("maximum safe integer parameter = %#v, want int64 MAX_SAFE_INTEGER", parameters["max"])
	}
	if got, ok := parameters["min"].(int64); !ok || got != -9007199254740991 {
		t.Fatalf("minimum safe integer parameter = %#v, want int64 -MAX_SAFE_INTEGER", parameters["min"])
	}
	if got, ok := parameters["exponent"].(float64); !ok || got != 1e-3 {
		t.Fatalf("finite exponent parameter = %#v, want float64 1e-3", parameters["exponent"])
	}
	if got, ok := numberDecoded.Input.Models[0].ReasoningConfig.Levels["max"].(int64); !ok || got != 9007199254740991 {
		t.Fatalf("nested reasoning integer = %#v, want int64 MAX_SAFE_INTEGER", numberDecoded.Input.Models[0].ReasoningConfig.Levels["max"])
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"provider":"空 白 provider","unknown":true}`),
		json.RawMessage(`{"provider":"空 白 provider","provider":"other"}`),
		json.RawMessage(append([]byte(string(valid)), []byte(` trailing`)...)),
		json.RawMessage(`{"provider":"空 白 provider","base_url":"https://example.test","models":[{"profile":"m","parameters":{"x":1,"x":2}}]}`),
		json.RawMessage(`{"provider":" 空 白 provider","base_url":"https://example.test","models":[{"profile":"m"}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","parameters":{"\ud800":1}}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","parameters":{"x":"\udc00"}}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","parameters":{"x":"\ud83d\u0041"}}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","parameters":{"x":9007199254740992}}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","parameters":{"x":-9007199254740992}}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","parameters":{"x":1e1000000}}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","parameters":{"x":1e-400}}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","parameters":{"x":-1e-400}}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","parameters":{"x":9007199254740990.5}}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","parameters":{"x":-9007199254740990.5}}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","api_key":null,"models":[{"profile":"m"}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","max_concurrent_requests":null,"models":[{"profile":"m"}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","context_window":null}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","parameters":null}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","reasoning_config":null}]}`),
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","pricing":{"input_cache_hit":null}}]}`),
		json.RawMessage{0xff, '{', '}'},
	} {
		if err := validateProviderUpdateArguments(raw); err == nil {
			t.Fatalf("provider.update accepted invalid arguments: %q", raw)
		}
	}
	deep := []byte(`{"provider":"p","base_url":"https://example.test","models":[{"profile":"m","parameters":`)
	deep = append(deep, []byte(strings.Repeat(`{"x":`, maxCommandJSONDepth+2))...)
	deep = append(deep, []byte(`null`+strings.Repeat(`}`, maxCommandJSONDepth+2)+`}]}`)...)
	if err := validateProviderUpdateArguments(deep); err == nil {
		t.Fatal("provider.update accepted over-depth parameters")
	}
	if err := validateProviderDefaultArguments(json.RawMessage(`{"provider":"空 白 provider","model":"主 模型"}`)); err != nil {
		t.Fatalf("valid provider.set_default rejected: %v", err)
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"provider":"空 白 provider","model":"主 模型","extra":true}`),
		json.RawMessage(`{"provider":"空 白 provider","model":"主 模型","model":"other"}`),
		json.RawMessage(`{"provider":"空 白 provider","model":"主 模型"} trailing`),
	} {
		if err := validateProviderDefaultArguments(raw); err == nil {
			t.Fatalf("provider.set_default accepted invalid arguments: %s", raw)
		}
	}
	if err := validateProviderDiscoverArguments(json.RawMessage(`{"provider":"空 白 provider"}`)); err != nil {
		t.Fatalf("valid provider.discover_models rejected: %v", err)
	}
	redacted := redactProviderUpdateArguments(valid)
	if string(redacted) != `{}` {
		t.Fatalf("provider update cache tombstone = %s, want {}", redacted)
	}
	for _, secret := range []string{"command-secret", "auth_file", "base_url", "parameters"} {
		if strings.Contains(string(redacted), secret) {
			t.Fatalf("redacted command cache contains %q: %s", secret, redacted)
		}
	}

	createValid := providerCreateTestArguments()
	if err := validateProviderCreateArguments(createValid); err != nil {
		t.Fatalf("valid provider.create arguments rejected: %v", err)
	}
	created, err := decodeProviderCreateArguments(createValid)
	if err != nil {
		t.Fatal(err)
	}
	if created.OperationID != "operation-provider-create" || created.Provider != "空 白 provider" || len(created.Input.Models) != 1 {
		t.Fatalf("provider create identity/target was not preserved: %#v", created)
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"provider":"p","base_url":"https://example.test","api_key":"secret","keep_api_key":false,"auth_file":"","request_timeout":"","http_proxy":"","https_proxy":"","max_concurrent_requests":0,"models":[{"profile":"m"}]}`),
		json.RawMessage(`{"operation_id":null,"provider":"p","base_url":"https://example.test","api_key":"secret","keep_api_key":false,"auth_file":"","request_timeout":"","http_proxy":"","https_proxy":"","max_concurrent_requests":0,"models":[{"profile":"m"}]}`),
		json.RawMessage(`{"operation_id":"../escape","provider":"p","base_url":"https://example.test","api_key":"secret","keep_api_key":false,"auth_file":"","request_timeout":"","http_proxy":"","https_proxy":"","max_concurrent_requests":0,"models":[{"profile":"m"}]}`),
		json.RawMessage(`{"operation_id":"operation-provider-create","provider":"p","base_url":"https://example.test","api_key":"secret","keep_api_key":false,"auth_file":"","request_timeout":"","http_proxy":"","https_proxy":"","max_concurrent_requests":0,"models":[{"profile":"m"}],"unknown":true}`),
		json.RawMessage(`{"operation_id":"operation-provider-create","operation_id":"other","provider":"p","base_url":"https://example.test","api_key":"secret","keep_api_key":false,"auth_file":"","request_timeout":"","http_proxy":"","https_proxy":"","max_concurrent_requests":0,"models":[{"profile":"m"}]}`),
		json.RawMessage(append([]byte(string(createValid)), []byte(` trailing`)...)),
		json.RawMessage(strings.Replace(string(createValid), `"temperature":0.2`, `"temperature":"\ud800"`, 1)),
		json.RawMessage(strings.Replace(string(createValid), `"temperature":0.2`, `"temperature":9007199254740992`, 1)),
	} {
		if err := validateProviderCreateArguments(raw); err == nil {
			t.Fatalf("provider.create accepted invalid arguments: %q", raw)
		}
	}
	createRedacted := redactProviderCreateArguments(createValid)
	if string(createRedacted) != `{}` || strings.Contains(string(createRedacted), "command-secret") {
		t.Fatalf("provider create cache tombstone = %s, secrets must be redacted", createRedacted)
	}
}

func TestProviderWriteIntentFingerprintIsStableAndConflictingModesDiffer(t *testing.T) {
	preserveA := json.RawMessage(`{"provider":"p","base_url":"https://example.test","base_url_mode":"preserve","models":[{"profile":"m","parameters_mode":"preserve","parameters_source_profile":"m"}]}`)
	preserveB := json.RawMessage(`{"models":[{"parameters_source_profile":"m","parameters_mode":"preserve","profile":"m"}],"base_url_mode":"preserve","base_url":"https://example.test","provider":"p"}`)
	replace := json.RawMessage(`{"provider":"p","base_url":"https://example.test","base_url_mode":"preserve","models":[{"profile":"m","parameters_mode":"replace","parameters":{}}]}`)
	left, err := commands.Fingerprint(commands.CommandRequest{Name: "provider.update", SchemaVersion: 1, RequestID: "request", Arguments: preserveA})
	if err != nil {
		t.Fatal(err)
	}
	right, err := commands.Fingerprint(commands.CommandRequest{Name: "provider.update", SchemaVersion: 1, RequestID: "request", Arguments: preserveB})
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("preserve fingerprint changed with object order: %q != %q", left, right)
	}
	conflict, err := commands.Fingerprint(commands.CommandRequest{Name: "provider.update", SchemaVersion: 1, RequestID: "request", Arguments: replace})
	if err != nil {
		t.Fatal(err)
	}
	if left == conflict {
		t.Fatal("preserve and replace write intents shared a fingerprint")
	}
}

type providerCommandTestBlobWriter struct {
	content  []byte
	override *protocol.BlobDescriptor
}

func (w *providerCommandTestBlobWriter) Put(_ context.Context, _ string, content []byte) (protocol.BlobDescriptor, error) {
	w.content = append([]byte(nil), content...)
	if w.override != nil {
		return *w.override, nil
	}
	digest := sha256.Sum256(content)
	return protocol.BlobDescriptor{ID: "provider-models", URL: "/api/blobs/provider-models", ContentType: "application/json", Size: uint64(len(content)), SHA256: hex.EncodeToString(digest[:]), ETag: `"provider-models"`, ExpiresAt: "2099-01-01T00:00:00Z"}, nil
}

func TestProviderDiscoverCommandUsesBlobBoundaryAndRejectsOverage(t *testing.T) {
	modelCount := 4000
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		items := make([]map[string]string, modelCount)
		for index := range items {
			items[index] = map[string]string{"id": fmt.Sprintf("model-%04d-%s", index, strings.Repeat("x", 24))}
		}
		payload, err := json.Marshal(map[string]any{"data": items})
		if err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer providerServer.Close()
	server, service, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
	defer server.Close()
	if _, err := service.CreateProviderSettings(execution.ProviderSettingsInput{
		Name: "discover", BaseURL: providerServer.URL + "/v1", APIKey: "discover-secret",
		Models: []execution.ProviderModelSettings{{Profile: "main", ID: "configured", Type: config.ProviderTypeOpenAIChat}},
	}); err != nil {
		t.Fatal(err)
	}
	writer := &providerCommandTestBlobWriter{}
	registry, err := newSessionCommandRegistry(service, nil, sessionCommandRegistryOptions{HistoryWriter: writer})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := registry.Definition("provider.discover_models", 1)
	if err != nil {
		t.Fatal(err)
	}
	result, err := definition.Execute(context.Background(), commands.CommandRequest{Arguments: json.RawMessage(`{"provider":"discover"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result), "discover-secret") || writer.content == nil || len(writer.content) <= maxProviderDiscoverInline {
		t.Fatalf("discover result/blob boundary invalid: result=%s blobBytes=%d", result, len(writer.content))
	}
	var blobResult providerDiscoverBlobResult
	if err := json.Unmarshal(result, &blobResult); err != nil || blobResult.Blob == nil || blobResult.Blob.ContentType != "application/json" {
		t.Fatalf("discover blob result=%s err=%v", result, err)
	}
	digest := sha256.Sum256(writer.content)
	if blobResult.Blob.Size != uint64(len(writer.content)) || blobResult.Blob.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("discover descriptor integrity = %#v, bytes=%d", blobResult.Blob, len(writer.content))
	}
	var models []string
	if err := json.Unmarshal(writer.content, &models); err != nil || len(models) != modelCount || models[0] >= models[len(models)-1] {
		t.Fatalf("discover blob models len=%d err=%v", len(models), err)
	}
	modelCount = 4000
	validDescriptor := *blobResult.Blob
	faults := []struct {
		name string
		edit func(*protocol.BlobDescriptor)
	}{
		{name: "content type", edit: func(value *protocol.BlobDescriptor) { value.ContentType = "text/plain" }},
		{name: "size", edit: func(value *protocol.BlobDescriptor) { value.Size++ }},
		{name: "hash", edit: func(value *protocol.BlobDescriptor) { value.SHA256 = strings.Repeat("a", 64) }},
		{name: "missing id", edit: func(value *protocol.BlobDescriptor) { value.ID = "" }},
		{name: "missing URL", edit: func(value *protocol.BlobDescriptor) { value.URL = "" }},
		{name: "invalid expiry", edit: func(value *protocol.BlobDescriptor) { value.ExpiresAt = "not-a-timestamp" }},
	}
	for _, fault := range faults {
		t.Run(fault.name, func(t *testing.T) {
			bad := validDescriptor
			fault.edit(&bad)
			writer.override = &bad
			defer func() { writer.override = nil }()
			if _, err := definition.Execute(context.Background(), commands.CommandRequest{Arguments: json.RawMessage(`{"provider":"discover"}`)}); err == nil || strings.Contains(err.Error(), "discover-secret") {
				t.Fatalf("faulty descriptor accepted or leaked detail: %v", err)
			}
		})
	}
	modelCount = maxProviderDiscoverModels + 1
	if _, err := definition.Execute(context.Background(), commands.CommandRequest{Arguments: json.RawMessage(`{"provider":"discover"}`)}); err == nil {
		t.Fatal("discover command accepted a model count over its bound")
	}
}

func TestProjectCommandSchemasAreStrict(t *testing.T) {
	validCreate := json.RawMessage(`{"operation_id":"operation_project_1","root":"/tmp/project","display_name":"Project"}`)
	if err := validateProjectCreateArguments(validCreate); err != nil {
		t.Fatalf("valid project.create rejected: %v", err)
	}
	if err := validateProjectCreateArguments(json.RawMessage(`{"operation_id":"operation_project_empty","root":"/tmp/project-empty","display_name":""}`)); err != nil {
		t.Fatalf("empty optional project display name rejected: %v", err)
	}
	invalidCreates := []json.RawMessage{
		json.RawMessage(`{"operation_id":"operation_project_1","root":"/tmp/project","display_name":"Project","unknown":true}`),
		json.RawMessage(`{"operation_id":"operation_project_1","root":"/tmp/project","display_name":"Project","operation_id":"other"}`),
		json.RawMessage(`{"operation_id":"operation_project_1","root":null,"display_name":"Project"}`),
		json.RawMessage(`{"operation_id":"operation_project_1","root":"/tmp/project"}`),
		json.RawMessage(`{"operation_id":"../escape","root":"/tmp/project","display_name":"Project"}`),
		json.RawMessage(`{"operation_id":"operation_project_1","root":"/tmp/project","display_name":"Project"} trailing`),
	}
	for _, raw := range invalidCreates {
		if err := validateProjectCreateArguments(raw); err == nil {
			t.Fatalf("invalid project.create accepted: %s", raw)
		}
	}

	for _, test := range []struct {
		name     string
		validate func(json.RawMessage) error
		valid    json.RawMessage
		invalid  []json.RawMessage
	}{
		{name: "rename", validate: validateProjectRenameArguments, valid: json.RawMessage(`{"project_id":"project_1","display_name":"Renamed"}`), invalid: []json.RawMessage{
			json.RawMessage(`{"project_id":"project_1","display_name":1}`),
			json.RawMessage(`{"project_id":"project_1","display_name":"Renamed","extra":true}`),
		}},
		{name: "archive", validate: func(raw json.RawMessage) error { return validateProjectIDArguments(raw, "project.archive") }, valid: json.RawMessage(`{"project_id":"project_1"}`), invalid: []json.RawMessage{
			json.RawMessage(`{"project_id":null}`), json.RawMessage(`{"project_id":"project_1","extra":false}`),
		}},
		{name: "restore", validate: func(raw json.RawMessage) error { return validateProjectIDArguments(raw, "project.restore") }, valid: json.RawMessage(`{"project_id":"project_1"}`), invalid: []json.RawMessage{
			json.RawMessage(`{"project_id":""}`), json.RawMessage(`{"project_id":"project_1"}{}`),
		}},
		{name: "delete", validate: func(raw json.RawMessage) error { return validateProjectIDArguments(raw, "project.delete") }, valid: json.RawMessage(`{"project_id":"project_1"}`), invalid: []json.RawMessage{
			json.RawMessage(`{"project_id":"project_1","project_id":"other"}`), json.RawMessage(`{"project_id":1}`),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.validate(test.valid); err != nil {
				t.Fatalf("valid arguments rejected: %v", err)
			}
			for _, raw := range test.invalid {
				if err := test.validate(raw); err == nil {
					t.Fatalf("invalid arguments accepted: %s", raw)
				}
			}
		})
	}
}

func TestProjectCommandLifecycleUsesTypedExecutionRules(t *testing.T) {
	service, err := execution.NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := newSessionCommandRegistry(service, nil, sessionCommandRegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	execute := func(name string, arguments any) json.RawMessage {
		t.Helper()
		definition, err := registry.Definition(name, 1)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		result, err := definition.Execute(context.Background(), commands.CommandRequest{Name: name, SchemaVersion: 1, Arguments: raw})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	var created projectCreateResult
	if err := json.Unmarshal(execute("project.create", map[string]string{
		"operation_id": "operation_lifecycle", "root": root, "display_name": "Lifecycle",
	}), &created); err != nil {
		t.Fatal(err)
	}
	if !created.Created || created.ProjectID == "" || created.OperationID != "operation_lifecycle" {
		t.Fatalf("project.create result = %#v", created)
	}
	var emptyCreated projectCreateResult
	if err := json.Unmarshal(execute("project.create", map[string]string{
		"operation_id": "operation_empty_name", "root": t.TempDir(), "display_name": "",
	}), &emptyCreated); err != nil {
		t.Fatalf("empty-name project.create result decode: %v", err)
	}
	if !emptyCreated.Created || emptyCreated.ProjectID == "" || emptyCreated.OperationID != "operation_empty_name" {
		t.Fatalf("empty-name project.create result = %#v", emptyCreated)
	}

	var renamed projectRenameResult
	if err := json.Unmarshal(execute("project.rename", map[string]string{"project_id": created.ProjectID, "display_name": "Renamed"}), &renamed); err != nil {
		t.Fatal(err)
	}
	if renamed.ProjectID != created.ProjectID || renamed.DisplayName != "Renamed" {
		t.Fatalf("project.rename result = %#v", renamed)
	}
	// Rename to the same value is a no-op domain operation and therefore safe
	// to replay across a server epoch.
	execute("project.rename", map[string]string{"project_id": created.ProjectID, "display_name": "Renamed"})

	session, err := service.CreateSession(created.ProjectID, execution.SessionCreateMetadata{CreatedCWD: root})
	if err != nil {
		t.Fatal(err)
	}
	archive := projectArchiveResult{}
	if err := json.Unmarshal(execute("project.archive", map[string]string{"project_id": created.ProjectID}), &archive); err != nil {
		t.Fatal(err)
	}
	if !archive.Archived || archive.ProjectID != created.ProjectID {
		t.Fatalf("project.archive result = %#v", archive)
	}
	var restored projectArchiveResult
	if err := json.Unmarshal(execute("project.restore", map[string]string{"project_id": created.ProjectID}), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Archived {
		t.Fatalf("project.restore result = %#v", restored)
	}
	// Re-archive is idempotent; the session is idle, so this also verifies the
	// archive/busy rule is delegated to execution rather than the command.
	execute("project.archive", map[string]string{"project_id": created.ProjectID})
	var removed projectDeleteResult
	if err := json.Unmarshal(execute("project.delete", map[string]string{"project_id": created.ProjectID}), &removed); err != nil {
		t.Fatal(err)
	}
	if removed.ProjectID != created.ProjectID || removed.Status != "removed" || removed.RemovedSessions != 1 || session.ID == "" {
		t.Fatalf("project.delete result = %#v", removed)
	}
}

func TestRunStartPreservesExactContentAndUsesUTF8ByteLimit(t *testing.T) {
	exactContent := "  hello\n"
	raw, err := json.Marshal(map[string]string{
		"session_id": "session-content-boundary",
		"run_id":     "run-content-boundary",
		"content":    exactContent,
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := decodeRunStartArguments(raw)
	if err != nil {
		t.Fatalf("decodeRunStartArguments() error = %v", err)
	}
	if arguments.Content != exactContent {
		t.Fatalf("decoded content = %q, want exact wire value %q", arguments.Content, exactContent)
	}
	roundTrip, err := json.Marshal(arguments.Content)
	if err != nil || string(roundTrip) != `"  hello\n"` {
		t.Fatalf("content round trip = %s/%v, want preserved whitespace", roundTrip, err)
	}

	request := commands.CommandRequest{Name: "run.start", SchemaVersion: 1, Arguments: raw}
	fingerprint, err := runStartFingerprint(request, arguments)
	if err != nil {
		t.Fatalf("runStartFingerprint() error = %v", err)
	}
	trimmedRaw, err := json.Marshal(map[string]string{
		"session_id": "session-content-boundary",
		"run_id":     "run-content-boundary",
		"content":    "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	trimmedArguments, err := decodeRunStartArguments(trimmedRaw)
	if err != nil {
		t.Fatalf("trimmed decode error = %v", err)
	}
	trimmedFingerprint, err := runStartFingerprint(request, trimmedArguments)
	if err != nil {
		t.Fatalf("trimmed runStartFingerprint() error = %v", err)
	}
	if fingerprint == trimmedFingerprint {
		t.Fatal("content whitespace was lost from the durable fingerprint")
	}

	whitespaceOnly, err := json.Marshal(map[string]string{
		"session_id": "session-content-boundary",
		"run_id":     "run-content-whitespace",
		"content":    " \n\t\u2003",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRunStartArguments(whitespaceOnly); err == nil {
		t.Fatal("pure whitespace content was accepted")
	}

	unit := "界"
	exactBytes := strings.Repeat(unit, maxRunStartContentBytes/len(unit)) + "x"
	if len(exactBytes) != maxRunStartContentBytes {
		t.Fatalf("test exact-boundary content bytes = %d, want %d", len(exactBytes), maxRunStartContentBytes)
	}
	exactBoundary, err := json.Marshal(map[string]string{
		"session_id": "session-content-boundary",
		"run_id":     "run-content-exact-bytes",
		"content":    exactBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := decodeRunStartArguments(exactBoundary); err != nil || len(parsed.Content) != maxRunStartContentBytes {
		t.Fatalf("exact UTF-8 byte boundary decode = %d/%v, want accepted %d bytes", len(parsed.Content), err, maxRunStartContentBytes)
	}
	overBoundary, err := json.Marshal(map[string]string{
		"session_id": "session-content-boundary",
		"run_id":     "run-content-over-bytes",
		"content":    exactBytes + "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRunStartArguments(overBoundary); err == nil {
		t.Fatal("content over the UTF-8 byte boundary was accepted")
	}
}

func TestRunContinueFingerprintNormalizesOnlyWireOperationAndSeparatesRunStart(t *testing.T) {
	request := commands.CommandRequest{Name: "run.continue", SchemaVersion: 1, Arguments: json.RawMessage(`{"session_id":"session","run_id":"run-a"}`)}
	first, err := runContinueFingerprint(request, runContinueArguments{SessionID: "session", RunID: "run-a"})
	if err != nil {
		t.Fatalf("runContinueFingerprint() error = %v", err)
	}
	second, err := runContinueFingerprint(request, runContinueArguments{SessionID: "session", RunID: "run-b"})
	if err != nil {
		t.Fatalf("runContinueFingerprint(second) error = %v", err)
	}
	if first != second {
		t.Fatalf("continue fingerprint depends on stable run identity: %q != %q", first, second)
	}
	start, err := runStartFingerprint(commands.CommandRequest{Name: "run.start", SchemaVersion: 1}, runStartArguments{SessionID: "session", RunID: "run-a", Content: ""})
	if err != nil {
		t.Fatalf("runStartFingerprint() error = %v", err)
	}
	if first == start {
		t.Fatal("run.start and run.continue share an idempotency fingerprint")
	}
}

func TestRunPromptAppendArgumentsAreStrictAndPreserveContentBytes(t *testing.T) {
	content := "\n  keep both edges  \t"
	arguments, err := decodeRunPromptAppendArguments(json.RawMessage(`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":"\n  keep both edges  \t"}`))
	if err != nil {
		t.Fatalf("valid append arguments rejected: %v", err)
	}
	if arguments.Content != content {
		t.Fatalf("content=%q, want exact whitespace=%q", arguments.Content, content)
	}
	invalid := []string{
		`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":"ok","extra":true}`,
		`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","operation_id":"other","content":"ok"}`,
		`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":"ok"} {}`,
		`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":[]}`,
		`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":"   \t"}`,
		`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":"ok","images":[]}`,
		`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":"ok","blob":"not-supported"}`,
	}
	for _, raw := range invalid {
		if _, err := decodeRunPromptAppendArguments(json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid append arguments accepted: %s", raw)
		}
	}
	tooLarge := `{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":"` + strings.Repeat("x", maxRunPromptAppendContentBytes+1) + `"}`
	if _, err := decodeRunPromptAppendArguments(json.RawMessage(tooLarge)); err == nil {
		t.Fatal("append content over byte bound was accepted")
	}
}

func TestActiveRunControlCommandSchemasAreStrict(t *testing.T) {
	tests := []struct {
		name     string
		validate func(json.RawMessage) error
		valid    string
		invalid  []string
	}{
		{
			name: "remove", validate: validateRunPromptRemoveArguments,
			valid: `{"session_id":"session","run_id":"run","prompt_id":"ap-1"}`,
			invalid: []string{
				`{"session_id":"session","run_id":"run"}`,
				`{"session_id":"session","run_id":"run","prompt_id":""}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","unknown":true}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","prompt_id":"ap-2"}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1"} trailing`,
				`{"session_id":"session","run_id":1,"prompt_id":"ap-1"}`,
			},
		},
		{
			name: "steer", validate: validateRunPromptSteerArguments,
			valid: `{"session_id":"session","run_id":"run","prompt_id":"ap-1","steer":false}`,
			invalid: []string{
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1"}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","steer":1}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","steer":false,"extra":null}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","prompt_id":"ap-2","steer":true}`,
			},
		},
		{
			name: "move", validate: validateRunPromptMoveArguments,
			valid: `{"session_id":"session","run_id":"run","prompt_id":"ap-1","delta":-1}`,
			invalid: []string{
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","delta":1.5}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","delta":65}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","delta":null}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","delta":0,"direction":"up"}`,
			},
		},
		{
			name: "tool_cancel", validate: validateRunToolCancelArguments,
			valid: `{"session_id":"session","run_id":"run","tool_call_id":"call-1"}`,
			invalid: []string{
				`{"session_id":"session","run_id":"run","tool_call_id":""}`,
				`{"session_id":"session","run_id":"run","tool_call_id":false}`,
				`{"session_id":"session","run_id":"run","tool_call_id":"call-1","tool_call_id":"call-2"}`,
				`{"session_id":"session","run_id":"run","tool_call_id":"call-1"}{}`,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.validate(json.RawMessage(test.valid)); err != nil {
				t.Fatalf("valid arguments rejected: %v", err)
			}
			for _, raw := range test.invalid {
				if err := test.validate(json.RawMessage(raw)); err == nil {
					t.Fatalf("invalid arguments accepted: %s", raw)
				}
			}
		})
	}
}

func TestSessionCommandRegistryIsClosedAndFlagsAreExplicit(t *testing.T) {
	registry, err := newSessionCommandRegistry(nil, nil, sessionCommandRegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"codex_login.clear", "codex_login.start", "project.archive", "project.create", "project.delete", "project.rename", "project.restore", "provider.create", "provider.discover_models", "provider.set_default", "provider.update", "run.cancel", "run.continue", "run.prompt.append", "run.prompt.move", "run.prompt.remove", "run.prompt.steer", "run.start", "run.tool.cancel", "session.archive", "session.compact", "session.create", "session.delete", "session.history.read", "session.mark_read", "session.rename", "session.restore", "session.set_debug", "session.set_full_access"}
	if got := registry.Names(); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("registry names=%v, want %v", got, wantNames)
	}
	for _, name := range wantNames {
		definition, err := registry.Definition(name, 1)
		if err != nil {
			t.Fatal(err)
		}
		if definition.SupportsExpectedRevision {
			t.Fatalf("%s unexpectedly supports expected_revision", name)
		}
		if name == "session.history.read" || name == "provider.discover_models" {
			if definition.CachePolicy != commands.ResultCacheVolatile {
				t.Fatalf("%s must retain a volatile-result policy", name)
			}
		} else if definition.CachePolicy != commands.ResultCacheDurable {
			t.Fatalf("%s unexpectedly has a volatile-result policy", name)
		}
		if name == "provider.create" || name == "codex_login.start" || name == "run.cancel" || name == "run.prompt.move" || name == "run.prompt.remove" || name == "run.prompt.steer" || name == "run.tool.cancel" || name == "session.compact" || name == "session.delete" || name == "project.delete" {
			if definition.CrossEpochRetrySafe {
				t.Fatalf("%s must remain cross-epoch unsafe", name)
			}
		} else if !definition.CrossEpochRetrySafe {
			t.Fatalf("%s must be cross-epoch safe", name)
		}
	}
	createDefinition, err := registry.Definition("provider.create", 1)
	if err != nil || createDefinition.RedactArguments == nil || string(createDefinition.RedactArguments(providerCreateTestArguments())) != `{}` {
		t.Fatalf("provider.create must retain only a minimal cache tombstone: %#v", createDefinition)
	}
}

func TestCodexLoginCommandSchemaIsStrictAndBounded(t *testing.T) {
	registry, err := newSessionCommandRegistry(nil, nil, sessionCommandRegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		valid     string
		invalid   []string
		crossSafe bool
	}{
		{name: "codex_login.start", valid: `{"provider":"codex"}`, invalid: []string{
			`{}`, `null`, `{"provider":null}`, `{"provider":""}`, `{"provider":"bad/provider"}`,
			`{"provider":"codex","extra":true}`, `{"provider":"codex","provider":"other"}`,
			`{"provider":"codex"}{}`, `{"provider":"\ud800"}`,
		}, crossSafe: false},
		{name: "codex_login.clear", valid: `{"provider":"codex"}`, invalid: []string{
			`{}`, `{"provider":null}`, `{"provider":false}`, `{"provider":"codex","extra":true}`,
			`{"provider":"codex"} trailing`, `{"provider":"\ud800"}`,
		}, crossSafe: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition, err := registry.Definition(test.name, 1)
			if err != nil {
				t.Fatal(err)
			}
			if definition.CrossEpochRetrySafe != test.crossSafe || definition.RedactArguments == nil || string(definition.RedactArguments(json.RawMessage(test.valid))) != `{}` {
				t.Fatalf("definition=%#v", definition)
			}
			if err := registry.Validate(definition, json.RawMessage(test.valid)); err != nil {
				t.Fatalf("valid arguments rejected: %v", err)
			}
			for _, raw := range test.invalid {
				if err := registry.Validate(definition, json.RawMessage(raw)); err == nil {
					t.Fatalf("invalid arguments accepted: %s", raw)
				}
			}
		})
	}
}

func TestCodexLoginCommandsUseFakeDeviceFlowAndSafeResults(t *testing.T) {
	var polls atomic.Int32
	deviceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/usercode":
			_, _ = fmt.Fprint(w, `{"device_auth_id":"device-1","user_code":"USER-123","verification_uri":"https://example.test/device","interval":1,"expires_in":600}`)
		case "/device-token":
			polls.Add(1)
			_, _ = fmt.Fprint(w, `{"authorization_code":"auth-code","code_verifier":"verifier"}`)
		case "/oauth-token":
			_, _ = fmt.Fprint(w, `{"access_token":"access-token-secret","refresh_token":"refresh-token-secret","expires_in":3600,"account_id":"account-1"}`)
		default:
			t.Fatalf("unexpected fake device path %q", r.URL.Path)
		}
	}))
	defer deviceServer.Close()

	server, service, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
	defer server.Close()
	if _, err := service.CreateProviderSettings(execution.ProviderSettingsInput{
		Name: "codex-command", BaseURL: "https://example.test/codex",
		Models: []execution.ProviderModelSettings{{Profile: "default", ID: "gpt", Type: config.ProviderTypeOpenAICodex}},
	}); err != nil {
		t.Fatal(err)
	}
	registryState := newCodexLoginRegistry(context.Background(), service)
	registryState.startDeviceLogin = func(ctx context.Context, _ string) (codexauth.PendingDeviceLogin, error) {
		return codexauth.StartDeviceLogin(ctx, codexauth.DeviceLoginOptions{
			UserCodeURL: deviceServer.URL + "/usercode", DeviceTokenURL: deviceServer.URL + "/device-token",
			TokenURL: deviceServer.URL + "/oauth-token", RedirectURI: deviceServer.URL + "/callback",
			HTTPClient: deviceServer.Client(), PollInterval: time.Millisecond,
			Sleep: func(context.Context, time.Duration) error { return nil },
		})
	}
	registry, err := newSessionCommandRegistry(service, nil, sessionCommandRegistryOptions{CodexLogins: registryState})
	if err != nil {
		t.Fatal(err)
	}
	startDefinition, err := registry.Definition("codex_login.start", 1)
	if err != nil {
		t.Fatal(err)
	}
	result, err := startDefinition.Execute(context.Background(), commands.CommandRequest{Arguments: json.RawMessage(`{"provider":"codex-command"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `{"provider":"codex-command","status":"accepted"}` || strings.Contains(string(result), "token") {
		t.Fatalf("start result=%s", result)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, statusErr := registryState.status("codex-command")
		if statusErr == nil && status.Status == "signed_in" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	status, err := registryState.status("codex-command")
	if err != nil || status.Status != "signed_in" || polls.Load() == 0 {
		t.Fatalf("completed status=%#v polls=%d err=%v", status, polls.Load(), err)
	}
	if err := registryState.clear("codex-command"); err != nil {
		t.Fatal(err)
	}
	status, err = registryState.status("codex-command")
	if err != nil || status.Status != "signed_out" {
		t.Fatalf("cleared status=%#v err=%v", status, err)
	}
	clearDefinition, err := registry.Definition("codex_login.clear", 1)
	if err != nil {
		t.Fatal(err)
	}
	clearResult, err := clearDefinition.Execute(context.Background(), commands.CommandRequest{Arguments: json.RawMessage(`{"provider":"codex-command"}`)})
	if err != nil || string(clearResult) != `{"provider":"codex-command","status":"cleared"}` {
		t.Fatalf("signed-out clear result=%s err=%v", clearResult, err)
	}

	// An external failure is mapped to a fixed error and never echoes its
	// diagnostic into the command result.
	failing := newCodexLoginRegistry(context.Background(), service)
	failing.startDeviceLogin = func(context.Context, string) (codexauth.PendingDeviceLogin, error) {
		return codexauth.PendingDeviceLogin{}, errors.New("access-token-secret /raw/auth-file")
	}
	failingRegistry, err := newSessionCommandRegistry(service, nil, sessionCommandRegistryOptions{CodexLogins: failing})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := failingRegistry.Definition("codex_login.start", 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = definition.Execute(context.Background(), commands.CommandRequest{Arguments: json.RawMessage(`{"provider":"codex-command"}`)})
	if err == nil || !strings.Contains(err.Error(), "Codex login could not be started") || strings.Contains(err.Error(), "access-token-secret") {
		t.Fatalf("failure=%v", err)
	}
}

func TestCodexLoginClearBlocksStaleCompletion(t *testing.T) {
	completionStarted := make(chan struct{})
	completionFinished := make(chan struct{})
	var startedOnce sync.Once
	var finishedOnce sync.Once
	deviceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/usercode":
			_, _ = fmt.Fprint(w, `{"device_auth_id":"device-1","user_code":"USER-123","verification_uri":"https://example.test/device"}`)
		default:
			t.Fatalf("unexpected fake device path %q", r.URL.Path)
		}
	}))
	defer deviceServer.Close()

	server, service, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
	defer server.Close()
	if _, err := service.CreateProviderSettings(execution.ProviderSettingsInput{
		Name: "codex-race", BaseURL: "https://example.test/codex",
		Models: []execution.ProviderModelSettings{{Profile: "default", ID: "gpt", Type: config.ProviderTypeOpenAICodex}},
	}); err != nil {
		t.Fatal(err)
	}
	registry := newCodexLoginRegistry(context.Background(), service)
	registry.startDeviceLogin = func(ctx context.Context, _ string) (codexauth.PendingDeviceLogin, error) {
		return codexauth.StartDeviceLogin(ctx, codexauth.DeviceLoginOptions{
			UserCodeURL: deviceServer.URL + "/usercode", DeviceTokenURL: deviceServer.URL + "/device-token",
			TokenURL: deviceServer.URL + "/oauth-token", RedirectURI: deviceServer.URL + "/callback",
			HTTPClient: deviceServer.Client(), PollInterval: time.Millisecond,
			Sleep: func(context.Context, time.Duration) error { return nil },
		})
	}
	registry.completeDeviceLogin = func(ctx context.Context, _ codexauth.PendingDeviceLogin) (codexauth.DeviceLoginResult, error) {
		startedOnce.Do(func() { close(completionStarted) })
		<-ctx.Done()
		finishedOnce.Do(func() { close(completionFinished) })
		return codexauth.DeviceLoginResult{}, ctx.Err()
	}
	if _, err := registry.start("codex-race"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-completionStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("fake device flow did not reach completion")
	}
	if err := registry.clear("codex-race"); err != nil {
		t.Fatal(err)
	}
	status, err := service.CodexAuthStatus("codex-race")
	if err != nil || status.Status != "signed_out" {
		t.Fatalf("status after clear=%#v err=%v", status, err)
	}
	// Wait for the completion hook to observe cancellation. This is the
	// deterministic point at which the old completion can no longer proceed to
	// token persistence; no fixed-duration sleep is used to prove the barrier.
	select {
	case <-completionFinished:
	case <-time.After(2 * time.Second):
		t.Fatal("stale completion did not observe clear cancellation")
	}
	status, err = service.CodexAuthStatus("codex-race")
	if err != nil || status.Status != "signed_out" {
		t.Fatalf("stale completion rewrote auth state: %#v err=%v", status, err)
	}
}

func TestPromptAppendOutcomeErrorsRemainTypedFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
		want string
	}{
		{name: "not applied", err: execution.ErrPromptAppendNotApplied, code: "operation_not_applied", want: "was not applied"},
		{name: "outcome unknown", err: execution.ErrPromptAppendOutcomeUnknown, code: "operation_outcome_unknown", want: "may already have been applied"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mapped := sessionCommandError(test.err)
			var domain *commands.DomainError
			if !errors.As(mapped, &domain) || domain == nil || domain.Code != test.code || !strings.Contains(domain.Message, test.want) {
				t.Fatalf("mapped error=%#v, want %s containing %q", mapped, test.code, test.want)
			}
		})
	}
}

func TestSessionCompactCommandErrorsRemainTyped(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "busy", err: execution.ErrSessionBusy, code: "session_busy"},
		{name: "planner cancellation", err: context.Canceled, code: "cancelled"},
		{name: "planner failure", err: execution.ErrTurnFailed, code: "compact_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mapped := sessionCompactCommandError(test.err)
			var domain *commands.DomainError
			if !errors.As(mapped, &domain) || domain == nil || domain.Code != test.code {
				t.Fatalf("mapped error=%#v, want code %q", mapped, test.code)
			}
		})
	}
}
