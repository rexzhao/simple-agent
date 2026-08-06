package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/providersettings"
)

func TestProviderSettingsWebSocketResourceUsesSafeDurableProjection(t *testing.T) {
	server, _, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
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
		Name: "provider.update", SchemaVersion: 1, RequestID: "provider-update-request", Arguments: json.RawMessage(`{"provider":"fake","base_url":"http://127.0.0.1:1/v1","api_key":"command-secret","keep_api_key":false,"auth_file":"","request_timeout":"","http_proxy":"","https_proxy":"","max_concurrent_requests":0,"models":[{"profile":"fast","id":"fake-model","type":"","compatibility":"","input":["text","image"],"developer_role":"","context_window":32000,"input_limit":0,"output_limit":0,"parameters":{},"reasoning_config":{"parameter":"","default":"","levels":{}},"pricing":null},{"profile":"precise","id":"changed-model","type":"","compatibility":"","input":[],"developer_role":"","context_window":64000,"input_limit":0,"output_limit":0,"parameters":{},"reasoning_config":{"parameter":"","default":"","levels":{}},"pricing":null}]}`),
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
	if !ok || result.Payload.Status != protocol.CommandStatusSucceeded || strings.Contains(string(result.Payload.Result), "command-secret") {
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
	// Business errors are deliberately safe even when every input field is
	// credential-bearing. In particular, endpoint credentials, auth paths,
	// proxy credentials, and arbitrary model parameters must not cross the
	// command error or protocol logging boundary.
	unsafeErrorArguments := json.RawMessage(`{"provider":"fake","base_url":"https://user:base-error@example.test/v1","api_key":"error-api-key","keep_api_key":false,"auth_file":"/secret/path/provider.json","request_timeout":"","http_proxy":"http://user:proxy-error@proxy.example.test:8080","https_proxy":"","max_concurrent_requests":0,"models":[{"profile":"fast","id":"fake-model","type":"","compatibility":"","input":["text"],"developer_role":"","context_window":32000,"input_limit":0,"output_limit":0,"parameters":{"nested":{"token":"nested-error"}},"reasoning_config":{"parameter":"","default":"","levels":{}},"pricing":null}]}`)
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
