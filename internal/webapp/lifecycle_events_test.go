package webapp

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/execution"
)

func TestLifecycleEventsEndpointRequiresAuthAndEmitsFetchSSE(t *testing.T) {
	server, service, _ := newWebTestAppServerWithRunner(t, webTestRunner{})

	unauthorized, err := http.Get(server.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET unauthenticated lifecycle events error = %v", err)
	}
	if unauthorized.StatusCode != http.StatusUnauthorized || responseErrorCode(t, unauthorized) != "unauthorized" {
		t.Fatalf("GET unauthenticated lifecycle events = %d, want 401 unauthorized", unauthorized.StatusCode)
	}
	unauthorized.Body.Close()

	project, err := service.CreateProject(t.TempDir(), "events")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET lifecycle events error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("GET lifecycle events status/content-type = %d/%q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(response.Body)
	initial := make([]string, 0, 3)
	for len(initial) < 3 {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("read lifecycle SSE handshake: %v", readErr)
		}
		initial = append(initial, line)
	}
	if !strings.Contains(strings.Join(initial, ""), ": connected") {
		t.Fatalf("lifecycle SSE handshake = %q, want connected comment", initial)
	}

	session, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{CreatedCWD: project.Project.Root})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	readResult := make(chan string, 1)
	go func() {
		var body strings.Builder
		for {
			line, readErr := reader.ReadString('\n')
			body.WriteString(line)
			if strings.Contains(body.String(), "event: session.created") && strings.Contains(body.String(), `"id":"`+session.ID+`"`) {
				readResult <- body.String()
				cancel()
				return
			}
			if readErr != nil {
				return
			}
		}
	}()
	select {
	case body := <-readResult:
		if !strings.Contains(body, "event: session.created\n") || !strings.Contains(body, "data: {") {
			t.Fatalf("lifecycle SSE event = %q, want event/data frame", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for session.created SSE event")
	}
}
