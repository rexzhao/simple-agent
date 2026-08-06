package webapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/execution"
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

	if _, err := service.UpdateProviderSettings("fake", execution.ProviderSettingsInput{Name: "fake", BaseURL: "http://127.0.0.1:1/v1", KeepAPIKey: true, Models: []execution.ProviderModelSettings{{Profile: "fast", ID: "fake-model"}, {Profile: "precise", ID: "changed-model"}}}); err != nil {
		t.Fatal(err)
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
}
