package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/providersettings"
)

func TestProviderSettingsWebSocketResourceUsesSafeDurableProjection(t *testing.T) {
	server, service, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
	connection := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), "http://"+strings.TrimPrefix(server.URL, "http://"))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, connection)
	if _, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}
	message := protocol.SubscribeMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "provider-settings-subscribe"}, Payload: protocol.SubscribePayload{SubscriptionID: "provider-settings", Resource: protocol.ResourceKey{Type: protocol.ResourceTypeProviderSettings, ID: providersettings.ResourceID}}}
	encoded, err := protocol.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := readWebAppMessage(t, connection).(protocol.SubscribedMessage); !ok {
		t.Fatal("provider settings subscribed missing")
	}
	snapshotMessage, ok := readWebAppMessage(t, connection).(protocol.SnapshotMessage)
	if !ok {
		t.Fatal("provider settings snapshot missing")
	}
	if bytes := snapshotMessage.Payload.Content.Inline; strings.Contains(string(bytes), "test-key") || strings.Contains(string(bytes), `"api_key":`) {
		t.Fatalf("provider settings snapshot leaked secret: %s", bytes)
	}
	var snapshot providersettings.ProviderSettingsSnapshot
	if err := json.Unmarshal(snapshotMessage.Payload.Content.Inline, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Providers) != 1 || !snapshot.Providers[0].APIKeyConfigured {
		t.Fatalf("safe provider snapshot = %#v", snapshot)
	}

	command := protocol.CommandMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "provider-update"}, Payload: protocol.CommandPayload{
		Name: "provider.update", SchemaVersion: 1, RequestID: "provider-update-request", Arguments: json.RawMessage(`{"provider":"fake","base_url":"http://127.0.0.1:1/v1","base_url_mode":"replace","api_key":"command-secret","keep_api_key":false,"auth_file":"","auth_file_mode":"replace","request_timeout":"","http_proxy":"","http_proxy_mode":"replace","https_proxy":"","https_proxy_mode":"replace","max_concurrent_requests":0,"models":[{"profile":"fast","id":"fake-model","type":"","compatibility":"","input":["text","image"],"developer_role":"","context_window":32000,"input_limit":0,"output_limit":0,"parameters_mode":"replace","parameters":{},"reasoning_config":{"parameter":"","default":"","levels":{}},"pricing":null},{"profile":"precise","id":"changed-model","type":"","compatibility":"","input":[],"developer_role":"","context_window":64000,"input_limit":0,"output_limit":0,"parameters_mode":"replace","parameters":{},"reasoning_config":{"parameter":"","default":"","levels":{}},"pricing":null}]}`),
	}}
	encoded, err = protocol.EncodeMessage(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := readWebAppMessage(t, connection).(protocol.CommandAcceptedMessage); !ok {
		t.Fatal("provider.update accepted missing")
	}
	result, ok := readWebAppMessage(t, connection).(protocol.CommandResultMessage)
	if !ok || result.Payload.Status != protocol.CommandStatusSucceeded || !strings.Contains(string(result.Payload.Result), `"changed":true`) || strings.Contains(string(result.Payload.Result), "command-secret") {
		t.Fatalf("provider.update result = %#v, API key must not be returned", result.Payload)
	}
	change, ok := readWebAppMessage(t, connection).(protocol.ChangeMessage)
	if !ok {
		t.Fatal("provider settings change missing")
	}
	if len(change.Payload.Operations) != 1 || change.Payload.Operations[0].Op != providersettings.OperationUpsertDefault {
		t.Fatalf("provider settings operations = %#v", change.Payload.Operations)
	}
	if strings.Contains(string(change.Payload.Operations[0].Raw), "test-key") {
		t.Fatal("provider settings change leaked API key")
	}
	// Replaying the already durable target reports changed=false and must not
	// create a second LocalReplica publication.
	noOpCommand := command
	noOpCommand.Envelope.ID = "provider-update-noop"
	noOpCommand.Payload.RequestID = "provider-update-noop-request"
	encoded, err = protocol.EncodeMessage(noOpCommand)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := readWebAppMessage(t, connection).(protocol.CommandAcceptedMessage); !ok {
		t.Fatal("provider.update no-op accepted missing")
	}
	noOpResult, ok := readWebAppMessage(t, connection).(protocol.CommandResultMessage)
	if !ok || noOpResult.Payload.Status != protocol.CommandStatusSucceeded || string(noOpResult.Payload.Result) != `{"provider":"fake","status":"applied","changed":false}` {
		t.Fatalf("provider.update no-op result = %#v", noOpResult.Payload)
	}

	createBaseURL := "https://create-base-user:create-base-pass@example.test/v1?create_query=create-query-secret"
	createAPIKey := "create-api-key"
	createAuthFile := "../auth/create-provider-auth.json"
	createHTTPProxy := "http://create-http-user:create-http-pass@proxy.example.test:8080"
	createHTTPSProxy := "https://create-https-user:create-https-pass@proxy.example.test:8443"
	createNestedSecret := "create-nested-secret"
	createArguments := json.RawMessage(fmt.Sprintf(`{"operation_id":"operation-created-provider","provider":"created-provider","base_url":%q,"base_url_mode":"replace","api_key":%q,"keep_api_key":false,"auth_file":%q,"auth_file_mode":"replace","request_timeout":"","http_proxy":%q,"http_proxy_mode":"replace","https_proxy":%q,"https_proxy_mode":"replace","max_concurrent_requests":0,"models":[{"profile":"fast","id":"created-model","type":"","compatibility":"","input":["text"],"developer_role":"","context_window":32000,"input_limit":0,"output_limit":0,"parameters_mode":"replace","parameters":{"nested":%q},"reasoning_config":{"parameter":"","default":"","levels":{}},"pricing":null}]}`, createBaseURL, createAPIKey, createAuthFile, createHTTPProxy, createHTTPSProxy, createNestedSecret))
	createSensitiveValues := []string{createBaseURL, "create-base-user", "create-base-pass", "create-query-secret", createAuthFile, createHTTPProxy, "create-http-user", "create-http-pass", createHTTPSProxy, "create-https-user", "create-https-pass", createAPIKey, createNestedSecret}
	assertNoCreateSecrets := func(label, payload string, values ...string) {
		for _, secret := range values {
			if strings.Contains(payload, secret) {
				t.Fatalf("provider.create %s leaked %q", label, secret)
			}
		}
	}
	createCommand := protocol.CommandMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "provider-create"}, Payload: protocol.CommandPayload{
		Name: "provider.create", SchemaVersion: 1, RequestID: "provider-create-request", Arguments: createArguments,
	}}
	encoded, err = protocol.EncodeMessage(createCommand)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := readWebAppMessage(t, connection).(protocol.CommandAcceptedMessage); !ok {
		t.Fatal("provider.create accepted missing")
	}
	createdResult, ok := readWebAppMessage(t, connection).(protocol.CommandResultMessage)
	if !ok || createdResult.Payload.Status != protocol.CommandStatusSucceeded || string(createdResult.Payload.Result) != `{"operation_id":"operation-created-provider","provider":"created-provider","status":"applied","changed":true}` {
		t.Fatalf("provider.create result = %#v", createdResult.Payload)
	}
	createdChange, ok := readWebAppMessage(t, connection).(protocol.ChangeMessage)
	if !ok || len(createdChange.Payload.Operations) != 1 || createdChange.Payload.Operations[0].Op != providersettings.OperationUpsertDefault {
		t.Fatalf("provider.create authoritative change = %#v", createdChange)
	}
	assertNoCreateSecrets("result", string(createdResult.Payload.Result), createSensitiveValues...)
	assertNoCreateSecrets("authoritative change", string(createdChange.Payload.Operations[0].Raw), createSensitiveValues...)
	persistedProvider, err := os.ReadFile(filepath.Join(service.ServerRoot(), "providers", "created-provider.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, persisted := range []string{createBaseURL, createAPIKey, createHTTPProxy, createHTTPSProxy, createNestedSecret} {
		if !strings.Contains(string(persistedProvider), persisted) {
			t.Fatalf("provider.create did not persist target value %q: %s", persisted, persistedProvider)
		}
	}

	// The request-id cache is the only same-epoch dedupe authority for create.
	// An exact retry returns the bounded acknowledgement without a second file
	// operation; changing the target under that request ID is a conflict.
	createRetry := createCommand
	createRetry.Envelope.ID = "provider-create-retry"
	encoded, err = protocol.EncodeMessage(createRetry)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
	retryResult, ok := readWebAppMessage(t, connection).(protocol.CommandResultMessage)
	if !ok || retryResult.Payload.Status != protocol.CommandStatusSucceeded || string(retryResult.Payload.Result) != string(createdResult.Payload.Result) {
		t.Fatalf("provider.create exact retry = %#v", retryResult.Payload)
	}
	createConflict := createCommand
	createConflict.Envelope.ID = "provider-create-conflict"
	createConflict.Payload.Arguments = json.RawMessage(strings.Replace(string(createArguments), "create-api-key", "other-create-api-key", 1))
	encoded, err = protocol.EncodeMessage(createConflict)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
	conflictResult, ok := readWebAppMessage(t, connection).(protocol.CommandResultMessage)
	if !ok || conflictResult.Payload.Status != protocol.CommandStatusFailed || conflictResult.Payload.Error == nil || conflictResult.Payload.Error.Code != "idempotency_conflict" {
		t.Fatalf("provider.create conflicting retry = %#v", conflictResult.Payload)
	}
	assertNoCreateSecrets("conflict result", string(conflictResult.Payload.Result), createSensitiveValues...)
	assertNoCreateSecrets("conflict error", fmt.Sprint(conflictResult.Payload.Error), createSensitiveValues...)
	assertNoCreateSecrets("conflict result", string(conflictResult.Payload.Result), "other-create-api-key")
	assertNoCreateSecrets("conflict error", fmt.Sprint(conflictResult.Payload.Error), "other-create-api-key")
	duplicate := createCommand
	duplicate.Envelope.ID = "provider-create-duplicate"
	duplicate.Payload.RequestID = "provider-create-duplicate-request"
	encoded, err = protocol.EncodeMessage(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := readWebAppMessage(t, connection).(protocol.CommandAcceptedMessage); !ok {
		t.Fatal("provider.create duplicate accepted missing")
	}
	duplicateResult, ok := readWebAppMessage(t, connection).(protocol.CommandResultMessage)
	if !ok || duplicateResult.Payload.Status != protocol.CommandStatusFailed || duplicateResult.Payload.Error == nil || duplicateResult.Payload.Error.Code != "provider_command_failed" {
		t.Fatalf("provider.create duplicate result = %#v", duplicateResult.Payload)
	}
	assertNoCreateSecrets("duplicate result", string(duplicateResult.Payload.Result), createSensitiveValues...)
	assertNoCreateSecrets("duplicate error", fmt.Sprint(duplicateResult.Payload.Error), createSensitiveValues...)
	invalidName := createCommand
	invalidName.Envelope.ID = "provider-create-invalid-name"
	invalidName.Payload.RequestID = "provider-create-invalid-name-request"
	invalidName.Payload.Arguments = json.RawMessage(strings.Replace(strings.Replace(string(createArguments), "operation-created-provider", "operation-invalid-name", 1), `"provider":"created-provider"`, `"provider":"CON"`, 1))
	encoded, err = protocol.EncodeMessage(invalidName)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := readWebAppMessage(t, connection).(protocol.CommandAcceptedMessage); !ok {
		t.Fatal("provider.create invalid-name accepted missing")
	}
	invalidNameResult, ok := readWebAppMessage(t, connection).(protocol.CommandResultMessage)
	if !ok || invalidNameResult.Payload.Status != protocol.CommandStatusFailed || invalidNameResult.Payload.Error == nil || invalidNameResult.Payload.Error.Code != "provider_command_failed" {
		t.Fatalf("provider.create invalid-name result = %#v", invalidNameResult.Payload)
	}
	assertNoCreateSecrets("invalid-name result", string(invalidNameResult.Payload.Result), createSensitiveValues...)
	assertNoCreateSecrets("invalid-name error", fmt.Sprint(invalidNameResult.Payload.Error), createSensitiveValues...)
	// Business errors are deliberately safe even when every input field is
	// credential-bearing. In particular, endpoint credentials, auth paths,
	// proxy credentials, and arbitrary model parameters must not cross the
	// command error or protocol logging boundary.
	unsafeErrorArguments := json.RawMessage(`{"provider":"fake","base_url":"https://user:base-error@example.test/v1","base_url_mode":"replace","api_key":"error-api-key","keep_api_key":false,"auth_file":"/secret/path/provider.json","auth_file_mode":"replace","request_timeout":"","http_proxy":"http://user:proxy-error@proxy.example.test:8080","http_proxy_mode":"replace","https_proxy":"","https_proxy_mode":"replace","max_concurrent_requests":0,"models":[{"profile":"fast","id":"fake-model","type":"anthropic-messages","compatibility":"openai","input":["text"],"developer_role":"","context_window":32000,"input_limit":0,"output_limit":0,"parameters_mode":"replace","parameters":{"nested":{"token":"nested-error"}},"reasoning_config":{"parameter":"","default":"","levels":{}},"pricing":null}]}`)
	unsafeCommand := protocol.CommandMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "provider-update-error"}, Payload: protocol.CommandPayload{
		Name: "provider.update", SchemaVersion: 1, RequestID: "provider-update-error-request", Arguments: unsafeErrorArguments,
	}}
	encoded, err = protocol.EncodeMessage(unsafeCommand)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := readWebAppMessage(t, connection).(protocol.CommandAcceptedMessage); !ok {
		t.Fatal("provider.update error accepted missing")
	}
	failed, ok := readWebAppMessage(t, connection).(protocol.CommandResultMessage)
	if !ok || failed.Payload.Status != protocol.CommandStatusFailed || failed.Payload.Error == nil {
		t.Fatalf("provider.update safe error = %#v", failed)
	}
	for _, secret := range []string{"error-api-key", "base-error", "/secret/path/provider.json", "proxy-error", "nested-error"} {
		if strings.Contains(string(failed.Payload.Result), secret) || strings.Contains(fmt.Sprint(failed.Payload.Error), secret) {
			t.Fatalf("provider.update error leaked %q: %#v", secret, failed.Payload)
		}
	}

	defaultCommand := protocol.CommandMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "provider-default"}, Payload: protocol.CommandPayload{
		Name: "provider.set_default", SchemaVersion: 1, RequestID: "provider-default-request", Arguments: json.RawMessage(`{"provider":"fake","model":"precise"}`),
	}}
	encoded, err = protocol.EncodeMessage(defaultCommand)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := readWebAppMessage(t, connection).(protocol.CommandAcceptedMessage); !ok {
		t.Fatal("provider.set_default accepted missing")
	}
	defaultResult, ok := readWebAppMessage(t, connection).(protocol.CommandResultMessage)
	if !ok || defaultResult.Payload.Status != protocol.CommandStatusSucceeded || string(defaultResult.Payload.Result) != `{"provider":"fake","model":"precise","status":"applied"}` {
		t.Fatalf("provider.set_default result = %#v", defaultResult.Payload)
	}
	defaultChange, ok := readWebAppMessage(t, connection).(protocol.ChangeMessage)
	if !ok || len(defaultChange.Payload.Operations) != 1 || defaultChange.Payload.Operations[0].Op != providersettings.OperationReplaceDefault {
		t.Fatalf("provider.set_default authoritative change = %#v", defaultChange)
	}
}
