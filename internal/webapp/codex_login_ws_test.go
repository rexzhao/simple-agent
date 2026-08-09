package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/codexauth"
	"github.com/rexzhao/simple-agent/internal/codexlogin"
	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

// TestCodexLoginWebSocketEndToEnd deliberately exercises the assembled HTTP
// ticket boundary, real WebSocket hello/subscribe/command dispatch, the
// execution-owned login registry, and the codex_login sync provider. The
// device endpoints are local fake HTTP handlers; no real network is used.
func TestCodexLoginWebSocketEndToEnd(t *testing.T) {
	const providerName = "codex-ws"
	forbidden := []string{
		"access-token-secret", "refresh-token-secret", "account-secret",
		"/raw/auth-file", "auth-file-secret",
	}

	var startCalls atomic.Int32
	var pollCalls atomic.Int32
	releasePoll := make(chan struct{})
	var releaseOnce sync.Once
	unexpectedPath := make(chan string, 4)
	deviceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/usercode":
			_, _ = fmt.Fprint(w, `{"device_auth_id":"device-ws","user_code":"USER-WS-123","verification_uri":"https://example.test/device"}`)
		case "/device-token":
			pollCalls.Add(1)
			select {
			case <-releasePoll:
				_, _ = fmt.Fprint(w, `{"authorization_code":"authorization-secret","code_verifier":"verifier-secret"}`)
			case <-r.Context().Done():
				return
			}
		case "/oauth-token":
			_, _ = fmt.Fprint(w, `{"access_token":"access-token-secret","refresh_token":"refresh-token-secret","expires_in":3600,"account_id":"account-secret"}`)
		default:
			select {
			case unexpectedPath <- r.URL.Path:
			default:
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer deviceServer.Close()
	defer releaseOnce.Do(func() { close(releasePoll) })

	server, service, appServer := newWebTestAppServerWithRunner(t, webTestRunner{})
	if _, err := service.CreateProviderSettings(execution.ProviderSettingsInput{
		Name: providerName, BaseURL: "https://example.test/codex",
		Models: []execution.ProviderModelSettings{{Profile: "default", ID: "gpt", Type: config.ProviderTypeOpenAICodex}},
	}); err != nil {
		t.Fatal(err)
	}
	appServer.codexLogins.startDeviceLogin = func(ctx context.Context, provider string) (codexauth.PendingDeviceLogin, error) {
		if provider != providerName {
			return codexauth.PendingDeviceLogin{}, fmt.Errorf("unexpected provider")
		}
		startCalls.Add(1)
		return codexauth.StartDeviceLogin(ctx, codexauth.DeviceLoginOptions{
			UserCodeURL:    deviceServer.URL + "/usercode",
			DeviceTokenURL: deviceServer.URL + "/device-token",
			TokenURL:       deviceServer.URL + "/oauth-token",
			RedirectURI:    deviceServer.URL + "/callback",
			HTTPClient:     deviceServer.Client(),
			PollInterval:   time.Millisecond,
			Sleep:          func(context.Context, time.Duration) error { return nil },
		})
	}

	first := openCodexLoginConnection(t, server.URL, forbidden)
	writeCodexLoginSubscribe(t, first, "codex-login-first", providerName, nil)
	firstSubscribed := readCodexSubscribed(t, first, forbidden)
	if firstSubscribed.Payload.Resource.Type != protocol.ResourceTypeCodexLogin || firstSubscribed.Payload.Resource.ID != providerName {
		t.Fatalf("initial subscription=%#v", firstSubscribed.Payload)
	}
	firstSnapshot := readCodexSnapshot(t, first, forbidden)
	if firstSnapshot.Status != codexlogin.StatusSignedOut || firstSnapshot.Provider != providerName {
		t.Fatalf("initial Codex login snapshot=%#v", firstSnapshot)
	}

	startResult, startChanges, accepted := sendCodexLoginCommand(t, first, "codex_login.start", "codex-start-request", providerName, forbidden)
	if !accepted || startResult.Payload.Status != protocol.CommandStatusSucceeded || startResult.Payload.Error != nil {
		t.Fatalf("start result=%#v accepted=%t", startResult.Payload, accepted)
	}
	assertCodexLoginResult(t, startResult, providerName, "accepted")

	var pendingChange *codexLoginWSChange
	for _, change := range startChanges {
		if change.Snapshot.Status == codexlogin.StatusPending && change.Snapshot.UserCode != "" && change.Snapshot.VerificationURL != "" {
			candidate := change
			pendingChange = &candidate
			break
		}
	}
	if pendingChange == nil {
		change := readCodexLoginChange(t, first, forbidden)
		if change.Snapshot.Status != codexlogin.StatusPending || change.Snapshot.UserCode != "USER-WS-123" || change.Snapshot.VerificationURL != "https://example.test/device" {
			t.Fatalf("pending device resource change=%#v", change.Snapshot)
		}
		pendingChange = &change
	}
	if pendingChange.Snapshot.UserCode != "USER-WS-123" || pendingChange.Snapshot.VerificationURL != "https://example.test/device" {
		t.Fatalf("pending device capabilities=%#v", pendingChange.Snapshot)
	}
	if pendingChange.Sequence == "" || pendingChange.StreamEpoch == "" {
		t.Fatalf("pending resource cursor=%#v", pendingChange)
	}

	// Keep the fake poll blocked until the pending capability change has
	// crossed the real WebSocket. This makes the required pending publication
	// deterministic rather than relying on goroutine scheduling.
	releaseOnce.Do(func() { close(releasePoll) })

	var signedIn *codexLoginWSChange
	for signedIn == nil {
		change := readCodexLoginChange(t, first, forbidden)
		if change.Snapshot.Status == codexlogin.StatusSignedIn {
			signedIn = &change
		}
	}
	if pollCalls.Load() == 0 || startCalls.Load() != 1 {
		t.Fatalf("device flow calls start=%d poll=%d", startCalls.Load(), pollCalls.Load())
	}
	if signedIn.Snapshot.UserCode != "" || signedIn.Snapshot.VerificationURL != "" {
		t.Fatalf("signed-in resource retained device capability=%#v", signedIn.Snapshot)
	}

	// Reconnect with the cursor immediately before signed-in. The assembled
	// journal must replay the signed-in change, not silently fall back to a
	// snapshot or lose the transition.
	resume := &protocol.ResumeToken{StreamEpoch: pendingChange.StreamEpoch, Sequence: pendingChange.Sequence}
	if err := first.Close(websocket.StatusNormalClosure, "reconnect"); err != nil {
		t.Fatal(err)
	}
	second := openCodexLoginConnection(t, server.URL, forbidden)
	defer second.Close(websocket.StatusNormalClosure, "done")
	writeCodexLoginSubscribe(t, second, "codex-login-replay", providerName, resume)
	replaySubscribed := readCodexSubscribed(t, second, forbidden)
	if replaySubscribed.Payload.StreamEpoch != pendingChange.StreamEpoch {
		t.Fatalf("replay epoch=%q, want %q", replaySubscribed.Payload.StreamEpoch, pendingChange.StreamEpoch)
	}
	replayed := readCodexLoginChange(t, second, forbidden)
	if replayed.Snapshot.Status != codexlogin.StatusSignedIn || replayed.Sequence != signedIn.Sequence {
		t.Fatalf("replayed signed-in change=%#v, want sequence %s", replayed.Snapshot, signedIn.Sequence)
	}

	// The same request ID on a new real WebSocket joins the server-epoch
	// dispatcher cache. It must return the bounded acknowledgement without
	// invoking the device flow a second time or publishing a new change.
	cachedResult, cachedChanges, cachedAccepted := sendCodexLoginCommand(t, second, "codex_login.start", "codex-start-request", providerName, forbidden)
	if cachedAccepted || len(cachedChanges) != 0 || cachedResult.Payload.Status != protocol.CommandStatusSucceeded {
		t.Fatalf("cached start accepted=%t changes=%d result=%#v", cachedAccepted, len(cachedChanges), cachedResult.Payload)
	}
	assertCodexLoginResult(t, cachedResult, providerName, "accepted")
	if startCalls.Load() != 1 {
		t.Fatalf("cached start launched another device flow: calls=%d", startCalls.Load())
	}

	// Invalid and ahead cursors both use the real dispatcher/resource recovery
	// path: resync_required followed by a fresh safe snapshot.
	writeCodexLoginSubscribe(t, second, "codex-login-wrong-epoch", providerName, &protocol.ResumeToken{StreamEpoch: "wrong-epoch", Sequence: pendingChange.Sequence})
	assertCodexResyncSnapshot(t, second, "codex-login-wrong-epoch", providerName, "epoch_mismatch", forbidden)
	writeCodexLoginSubscribe(t, second, "codex-login-ahead-cursor", providerName, &protocol.ResumeToken{StreamEpoch: pendingChange.StreamEpoch, Sequence: "999999"})
	assertCodexResyncSnapshot(t, second, "codex-login-ahead-cursor", providerName, "ahead", forbidden)
	if err := second.Close(websocket.StatusNormalClosure, "clear connection"); err != nil {
		t.Fatal(err)
	}

	clearConnection := openCodexLoginConnection(t, server.URL, forbidden)
	defer clearConnection.Close(websocket.StatusNormalClosure, "done")
	writeCodexLoginSubscribe(t, clearConnection, "codex-login-clear", providerName, nil)
	readCodexSubscribed(t, clearConnection, forbidden)
	clearSnapshot := readCodexSnapshot(t, clearConnection, forbidden)
	if clearSnapshot.Status != codexlogin.StatusSignedIn {
		t.Fatalf("clear baseline=%#v", clearSnapshot)
	}

	clearResult, clearChanges, accepted := sendCodexLoginCommand(t, clearConnection, "codex_login.clear", "codex-clear-request", providerName, forbidden)
	if !accepted || clearResult.Payload.Status != protocol.CommandStatusSucceeded || len(clearChanges) > 1 {
		t.Fatalf("clear result=%#v accepted=%t changes=%d", clearResult.Payload, accepted, len(clearChanges))
	}
	assertCodexLoginResult(t, clearResult, providerName, "cleared")
	var cleared *codexLoginWSChange
	for _, change := range clearChanges {
		if change.Snapshot.Status == codexlogin.StatusSignedOut {
			candidate := change
			cleared = &candidate
		}
	}
	if cleared == nil {
		change := readCodexLoginChange(t, clearConnection, forbidden)
		cleared = &change
	}
	if cleared.Snapshot.Status != codexlogin.StatusSignedOut {
		t.Fatalf("clear resource change=%#v", cleared.Snapshot)
	}

	// A distinct request ID executes clear again, while an exact retry also
	// exercises dispatcher cache semantics. Neither is allowed to append a
	// second signed_out resource change.
	repeatedResult, repeatedChanges, repeatedAccepted := sendCodexLoginCommand(t, clearConnection, "codex_login.clear", "codex-clear-repeat", providerName, forbidden)
	if !repeatedAccepted || repeatedResult.Payload.Status != protocol.CommandStatusSucceeded || len(repeatedChanges) != 0 {
		t.Fatalf("repeated clear accepted=%t changes=%d result=%#v", repeatedAccepted, len(repeatedChanges), repeatedResult.Payload)
	}
	assertCodexLoginResult(t, repeatedResult, providerName, "cleared")
	cachedClear, cachedClearChanges, cachedClearAccepted := sendCodexLoginCommand(t, clearConnection, "codex_login.clear", "codex-clear-request", providerName, forbidden)
	if cachedClearAccepted || len(cachedClearChanges) != 0 || cachedClear.Payload.Status != protocol.CommandStatusSucceeded {
		t.Fatalf("cached clear accepted=%t changes=%d result=%#v", cachedClearAccepted, len(cachedClearChanges), cachedClear.Payload)
	}
	assertCodexLoginResult(t, cachedClear, providerName, "cleared")

	unknown := sendCodexLoginCommandExpectFailure(t, clearConnection, "codex_login.start", "codex-unknown-provider", "missing-provider", forbidden)
	if unknown.Payload.Error == nil || unknown.Payload.Error.Code != "codex_provider_not_found" || unknown.Payload.Error.Message != "Codex provider was not found" {
		t.Fatalf("unknown provider error=%#v", unknown.Payload.Error)
	}
	nonCodex := sendCodexLoginCommandExpectFailure(t, clearConnection, "codex_login.start", "codex-non-codex-provider", "fake", forbidden)
	if nonCodex.Payload.Error == nil || nonCodex.Payload.Error.Code != "codex_provider_invalid" || nonCodex.Payload.Error.Message != "Provider is not configured for Codex" {
		t.Fatalf("non-Codex provider error=%#v", nonCodex.Payload.Error)
	}

	select {
	case path := <-unexpectedPath:
		t.Fatalf("fake device flow received unexpected path %q", path)
	default:
	}
}

func TestCodexLoginRejectsUnsafeDeviceCapabilitiesAtEveryBoundary(t *testing.T) {
	for _, test := range []struct {
		name      string
		userCode  string
		verifyURL string
		forbidden string
	}{
		{name: "query URL", userCode: "SAFE-CODE", verifyURL: "https://example.test/device?unsafe-capability=secret", forbidden: "unsafe-capability=secret"},
		{name: "oversized user code", userCode: strings.Repeat("U", codexlogin.MaxUserCodeBytes+1), verifyURL: "https://example.test/device", forbidden: strings.Repeat("U", codexlogin.MaxUserCodeBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			deviceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/usercode" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"device_auth_id":"unsafe-device","user_code":%q,"verification_uri":%q}`, test.userCode, test.verifyURL)
			}))
			defer deviceServer.Close()

			server, service, appServer := newWebTestAppServerWithRunner(t, webTestRunner{})
			const providerName = "codex-unsafe"
			if _, err := service.CreateProviderSettings(execution.ProviderSettingsInput{
				Name: providerName, BaseURL: "https://example.test/codex",
				Models: []execution.ProviderModelSettings{{Profile: "default", ID: "gpt", Type: config.ProviderTypeOpenAICodex}},
			}); err != nil {
				t.Fatal(err)
			}
			appServer.codexLogins.startDeviceLogin = func(ctx context.Context, _ string) (codexauth.PendingDeviceLogin, error) {
				return codexauth.StartDeviceLogin(ctx, codexauth.DeviceLoginOptions{
					UserCodeURL: deviceServer.URL + "/usercode", DeviceTokenURL: deviceServer.URL + "/unused-device-token",
					TokenURL: deviceServer.URL + "/unused-token", RedirectURI: deviceServer.URL + "/callback",
					HTTPClient: deviceServer.Client(),
				})
			}

			registry, err := newSessionCommandRegistry(service, nil, sessionCommandRegistryOptions{CodexLogins: appServer.codexLogins})
			if err != nil {
				t.Fatal(err)
			}
			definition, err := registry.Definition("codex_login.start", 1)
			if err != nil {
				t.Fatal(err)
			}
			_, err = definition.Execute(context.Background(), commands.CommandRequest{
				Arguments: json.RawMessage(`{"provider":"` + providerName + `"}`),
			})
			if err == nil || !strings.Contains(err.Error(), "Codex login could not be started") || strings.Contains(err.Error(), test.forbidden) {
				t.Fatalf("unsafe device command error=%v", err)
			}

			key := protocol.ResourceKey{Type: protocol.ResourceTypeCodexLogin, ID: providerName}
			if err := appServer.codexLogin.Authorize(context.Background(), syncengine.Principal{ID: "capability"}, key); err != nil {
				t.Fatal(err)
			}
			opened, err := appServer.codexLogin.Open(context.Background(), key, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			var resourceSnapshot codexlogin.Snapshot
			if err := json.Unmarshal(opened.Snapshot.Content.InlineBytes(), &resourceSnapshot); err != nil {
				t.Fatal(err)
			}
			if resourceSnapshot.Status != codexlogin.StatusError || resourceSnapshot.UserCode != "" || resourceSnapshot.VerificationURL != "" || strings.Contains(string(opened.Snapshot.Content.InlineBytes()), test.forbidden) {
				t.Fatalf("unsafe resource snapshot=%s", opened.Snapshot.Content.InlineBytes())
			}

			response := doJSONRequest(t, http.MethodPost, server.URL+"/api/providers/"+providerName+"/codex-login", map[string]string{})
			body := readBody(response)
			if response.StatusCode != http.StatusNotFound || strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "html") || strings.Contains(body, "<html") || strings.Contains(body, test.forbidden) || strings.Contains(body, "raw/auth-file") {
				t.Fatalf("unsafe REST start status=%d body=%s", response.StatusCode, body)
			}
			statusResponse := doJSONRequest(t, http.MethodGet, server.URL+"/api/providers/"+providerName+"/codex-login", nil)
			statusBody := readBody(statusResponse)
			if statusResponse.StatusCode != http.StatusNotFound || strings.Contains(strings.ToLower(statusResponse.Header.Get("Content-Type")), "html") || strings.Contains(statusBody, "<html") || strings.Contains(statusBody, test.forbidden) || strings.Contains(statusBody, "raw/auth-file") {
				t.Fatalf("unsafe REST status status=%d body=%s", statusResponse.StatusCode, statusBody)
			}
		})
	}
}

type codexLoginWSChange struct {
	Sequence    protocol.Sequence
	StreamEpoch string
	Snapshot    codexlogin.Snapshot
}

func openCodexLoginConnection(t *testing.T, serverURL string, forbidden []string) *websocket.Conn {
	t.Helper()
	connection := dialWebApp(t, serverURL, issueWebSocketTicket(t, serverURL), "http://"+strings.TrimPrefix(serverURL, "http://"))
	writeWebAppHello(t, connection)
	message := readCodexWebSocketMessage(t, connection, forbidden)
	if _, ok := message.(protocol.WelcomeMessage); !ok {
		connection.Close(websocket.StatusProtocolError, "welcome missing")
		t.Fatalf("welcome=%#v", message)
	}
	return connection
}

func writeCodexLoginSubscribe(t *testing.T, connection *websocket.Conn, subscriptionID, provider string, resume *protocol.ResumeToken) {
	t.Helper()
	message := protocol.SubscribeMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "codex-subscribe-" + subscriptionID},
		Payload: protocol.SubscribePayload{
			SubscriptionID: subscriptionID,
			Resource:       protocol.ResourceKey{Type: protocol.ResourceTypeCodexLogin, ID: provider},
			Resume:         resume,
		},
	}
	payload, err := protocol.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

func readCodexSubscribed(t *testing.T, connection *websocket.Conn, forbidden []string) protocol.SubscribedMessage {
	t.Helper()
	message := readCodexWebSocketMessage(t, connection, forbidden)
	subscribed, ok := message.(protocol.SubscribedMessage)
	if !ok {
		t.Fatalf("subscribed=%#v", message)
	}
	return subscribed
}

func readCodexSnapshot(t *testing.T, connection *websocket.Conn, forbidden []string) codexlogin.Snapshot {
	t.Helper()
	message := readCodexWebSocketMessage(t, connection, forbidden)
	snapshot, ok := message.(protocol.SnapshotMessage)
	if !ok {
		t.Fatalf("snapshot=%#v", message)
	}
	var value codexlogin.Snapshot
	if err := json.Unmarshal(snapshot.Payload.Content.Inline, &value); err != nil {
		t.Fatal(err)
	}
	if err := value.Validate(); err != nil {
		t.Fatalf("snapshot validation: %v", err)
	}
	return value
}

func readCodexLoginChange(t *testing.T, connection *websocket.Conn, forbidden []string) codexLoginWSChange {
	t.Helper()
	for {
		message := readCodexWebSocketMessage(t, connection, forbidden)
		change, ok := message.(protocol.ChangeMessage)
		if !ok {
			continue
		}
		if change.Payload.Resource.Type != protocol.ResourceTypeCodexLogin {
			t.Fatalf("unexpected resource change=%#v", change.Payload.Resource)
		}
		if len(change.Payload.Operations) != 1 {
			t.Fatalf("Codex login operation count=%d", len(change.Payload.Operations))
		}
		var operation struct {
			Op    string              `json:"op"`
			Key   string              `json:"key"`
			Value codexlogin.Snapshot `json:"value"`
		}
		if err := json.Unmarshal(change.Payload.Operations[0].Raw, &operation); err != nil {
			t.Fatal(err)
		}
		if operation.Op != codexlogin.OperationReplace || operation.Key != change.Payload.Resource.ID {
			t.Fatalf("Codex login operation=%#v", operation)
		}
		if err := operation.Value.Validate(); err != nil {
			t.Fatalf("Codex login change validation: %v", err)
		}
		return codexLoginWSChange{Sequence: change.Payload.Sequence, StreamEpoch: change.Payload.StreamEpoch, Snapshot: operation.Value}
	}
}

func sendCodexLoginCommand(t *testing.T, connection *websocket.Conn, name, requestID, provider string, forbidden []string) (protocol.CommandResultMessage, []codexLoginWSChange, bool) {
	t.Helper()
	message := protocol.CommandMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "codex-command-" + requestID},
		Payload: protocol.CommandPayload{
			Name: name, SchemaVersion: 1, RequestID: requestID,
			Arguments: json.RawMessage(`{"provider":"` + provider + `"}`),
		},
	}
	payload, err := protocol.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	var changes []codexLoginWSChange
	accepted := false
	for {
		incoming := readCodexWebSocketMessage(t, connection, forbidden)
		switch value := incoming.(type) {
		case protocol.CommandAcceptedMessage:
			if value.Payload.RequestID == requestID {
				accepted = true
			}
		case protocol.CommandResultMessage:
			if value.Payload.RequestID == requestID {
				return value, changes, accepted
			}
		case protocol.ChangeMessage:
			decoded := decodeCodexLoginChange(t, value)
			changes = append(changes, decoded)
		}
	}
}

func sendCodexLoginCommandExpectFailure(t *testing.T, connection *websocket.Conn, name, requestID, provider string, forbidden []string) protocol.CommandResultMessage {
	t.Helper()
	result, _, accepted := sendCodexLoginCommand(t, connection, name, requestID, provider, forbidden)
	if !accepted || result.Payload.Status != protocol.CommandStatusFailed || result.Payload.Error == nil {
		t.Fatalf("failed command accepted=%t result=%#v", accepted, result.Payload)
	}
	return result
}

func assertCodexLoginResult(t *testing.T, result protocol.CommandResultMessage, provider, status string) {
	t.Helper()
	if string(result.Payload.Result) != fmt.Sprintf(`{"provider":%q,"status":%q}`, provider, status) {
		t.Fatalf("command result=%s, want bounded %s/%s", result.Payload.Result, provider, status)
	}
}

func assertCodexResyncSnapshot(t *testing.T, connection *websocket.Conn, subscriptionID, provider, reason string, forbidden []string) {
	t.Helper()
	if subscribed := readCodexSubscribed(t, connection, forbidden); subscribed.Payload.SubscriptionID != subscriptionID {
		t.Fatalf("resync subscribed=%#v", subscribed.Payload)
	}
	message := readCodexWebSocketMessage(t, connection, forbidden)
	resync, ok := message.(protocol.ResyncRequiredMessage)
	if !ok || resync.Payload.SubscriptionID != subscriptionID || resync.Payload.Resource.ID != provider || resync.Payload.Reason != reason {
		t.Fatalf("resync=%#v, want reason %q", message, reason)
	}
	if snapshot := readCodexSnapshot(t, connection, forbidden); snapshot.Provider != provider || snapshot.Status != codexlogin.StatusSignedIn {
		t.Fatalf("resync snapshot=%#v", snapshot)
	}
}

func decodeCodexLoginChange(t *testing.T, message protocol.ChangeMessage) codexLoginWSChange {
	t.Helper()
	if message.Payload.Resource.Type != protocol.ResourceTypeCodexLogin {
		t.Fatalf("unexpected change resource=%#v", message.Payload.Resource)
	}
	if len(message.Payload.Operations) != 1 {
		t.Fatalf("Codex login operation count=%d", len(message.Payload.Operations))
	}
	var operation struct {
		Op    string              `json:"op"`
		Key   string              `json:"key"`
		Value codexlogin.Snapshot `json:"value"`
	}
	if err := json.Unmarshal(message.Payload.Operations[0].Raw, &operation); err != nil {
		t.Fatal(err)
	}
	if operation.Op != codexlogin.OperationReplace || operation.Key != message.Payload.Resource.ID {
		t.Fatalf("Codex login operation=%#v", operation)
	}
	if err := operation.Value.Validate(); err != nil {
		t.Fatalf("Codex login change validation: %v", err)
	}
	return codexLoginWSChange{Sequence: message.Payload.Sequence, StreamEpoch: message.Payload.StreamEpoch, Snapshot: operation.Value}
}

func readCodexWebSocketMessage(t *testing.T, connection *websocket.Conn, forbidden []string) protocol.Message {
	t.Helper()
	message := readWebAppMessage(t, connection)
	payload, err := protocol.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range forbidden {
		if strings.Contains(string(payload), value) {
			t.Fatalf("WebSocket payload leaked forbidden value %q: %s", value, payload)
		}
	}
	return message
}
