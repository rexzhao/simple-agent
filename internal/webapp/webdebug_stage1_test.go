package webapp

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/webdebug"
)

func TestProductionWebDebugAssemblyUsesConfigAndSessionStore(t *testing.T) {
	t.Run("default disabled target session", func(t *testing.T) {
		service, appServer, httpServer := newStage1ProductionApp(t, false)
		saveStage1Session(t, service, "stage1-disabled-session", webdebug.TargetProjectID)
		connection := openStage1Connection(t, httpServer)
		defer connection.Close(websocket.StatusNormalClosure, "done")
		message := sendStage1Register(t, connection, "stage1-disabled-session")
		assertStage1Error(t, message, webdebug.ErrorCodeDisabled)
		if _, err := appServer.webDebugBroker.Current(); !errors.Is(err, webdebug.ErrNotConnected) {
			t.Fatalf("disabled broker Current() error = %v, want ErrNotConnected", err)
		}
	})

	t.Run("enabled target session", func(t *testing.T) {
		service, appServer, httpServer := newStage1ProductionApp(t, true)
		saveStage1Session(t, service, "stage1-target-session", webdebug.TargetProjectID)
		connection := openStage1Connection(t, httpServer)
		defer connection.Close(websocket.StatusNormalClosure, "done")
		message := sendStage1Register(t, connection, "stage1-target-session")
		registered, ok := message.(protocol.DebugRegisteredMessage)
		if !ok {
			t.Fatalf("registration response = %T, want DebugRegisteredMessage", message)
		}
		if registered.Payload.SessionID != "stage1-target-session" {
			t.Fatalf("registered session = %q", registered.Payload.SessionID)
		}
		identity, err := appServer.webDebugBroker.Current()
		if err != nil {
			t.Fatal(err)
		}
		if identity.SessionID != "stage1-target-session" {
			t.Fatalf("current identity = %#v", identity)
		}
		if err := service.SessionStore().Delete("stage1-target-session"); err != nil {
			t.Fatal(err)
		}
		if identity, err := appServer.webDebugBroker.Acquire(context.Background()); !errors.Is(err, webdebug.ErrNotConnected) || identity != (webdebug.LeaseIdentity{}) {
			t.Fatalf("Acquire() after session deletion = %#v, %v, want empty ErrNotConnected", identity, err)
		}
	})

	t.Run("enabled rejects non-target and missing sessions generically", func(t *testing.T) {
		service, appServer, httpServer := newStage1ProductionApp(t, true)
		saveStage1Session(t, service, "stage1-wrong-project", "project-not-target")
		for _, sessionID := range []string{"stage1-wrong-project", "stage1-missing-session"} {
			connection := openStage1Connection(t, httpServer)
			message := sendStage1Register(t, connection, sessionID)
			assertStage1Error(t, message, webdebug.ErrorCodeNotEligible)
			if errMessage := message.(protocol.ErrorMessage); len(errMessage.Payload.Details) != 0 || errMessage.Payload.Message != webdebug.ErrorCodeNotEligible {
				t.Fatalf("authority rejection leaked details: %#v", errMessage.Payload)
			}
			connection.Close(websocket.StatusNormalClosure, "done")
			if _, err := appServer.webDebugBroker.Current(); !errors.Is(err, webdebug.ErrNotConnected) {
				t.Fatalf("rejected registration created lease: %v", err)
			}
		}
	})
}

func TestProductionWebDebugConfigFailureDoesNotFailOpen(t *testing.T) {
	home := t.TempDir()
	service, err := execution.NewService(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.ConfigPath(), []byte("debug:\n  web_eval_enabled: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := NewServer(ServerOptions{Context: context.Background(), Service: service, Token: testToken, CWD: home})
	if err == nil || app != nil {
		if app != nil {
			app.Close()
		}
		t.Fatalf("malformed config assembly app=%#v err=%v, want startup error", app, err)
	}
}

func newStage1ProductionApp(t *testing.T, enabled bool) (*execution.Service, *Server, *httptest.Server) {
	t.Helper()
	home := t.TempDir()
	service, err := execution.NewServiceWithOptions(home, execution.ServiceOptions{TurnRunner: webTestRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	writeWebTestConfig(t, home)
	if enabled {
		config, err := os.ReadFile(service.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		config = append(config, []byte("debug:\n  web_eval_enabled: true\n")...)
		if err := os.WriteFile(service.ConfigPath(), config, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	app, err := NewServer(ServerOptions{Context: context.Background(), Service: service, Token: testToken, CWD: home})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(app.Handler())
	t.Cleanup(func() {
		httpServer.Close()
		app.Close()
	})
	return service, app, httpServer
}

func saveStage1Session(t *testing.T, service *execution.Service, sessionID, projectID string) {
	t.Helper()
	_, err := service.SessionStore().SaveMetadata(sessions.SessionV2{
		ID: sessionID, ProjectID: projectID, CreatedCWD: t.TempDir(), ConfigPath: service.ConfigPath(),
		Provider: "fake", ModelProfile: "fast", ModelID: "fake-model",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func openStage1Connection(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	connection := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), "http://"+server.Listener.Addr().String())
	writeWebAppHello(t, connection)
	if welcome, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok || welcome.Payload.SelectedVersion != 1 {
		t.Fatalf("websocket handshake response = %#v, want V1 welcome", welcome)
	}
	return connection
}

func sendStage1Register(t *testing.T, connection *websocket.Conn, sessionID string) protocol.Message {
	t.Helper()
	message := protocol.DebugRegisterMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeDebugRegister, ID: "stage1-register"},
		Payload: protocol.DebugRegisterPayload{
			PageID: "stage1-page", PageEpoch: "stage1-epoch", SessionID: sessionID, Focused: true,
		},
	}
	payload, err := protocol.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	return readWebAppMessage(t, connection)
}

func assertStage1Error(t *testing.T, message protocol.Message, wantCode string) {
	t.Helper()
	errMessage, ok := message.(protocol.ErrorMessage)
	if !ok {
		t.Fatalf("response = %T, want ErrorMessage", message)
	}
	if errMessage.Payload.Code != wantCode {
		t.Fatalf("error code = %q, want %q", errMessage.Payload.Code, wantCode)
	}
}
