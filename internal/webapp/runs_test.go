package webapp

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/execution"
)

func TestRunRegistryLogsUnderlyingFailure(t *testing.T) {
	service, err := execution.NewServiceWithOptions(t.TempDir(), execution.ServiceOptions{TurnRunner: webTestRunner{}})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}

	var logs bytes.Buffer
	registry := newRunRegistry(context.Background(), service, &logs)
	managed, err := registry.start("missing-session", "hello")
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	for {
		_, terminal, changed := managed.snapshot(0)
		if terminal {
			break
		}
		<-changed
	}

	got := logs.String()
	if !strings.Contains(got, "missing-session") || !strings.Contains(got, "session not found") {
		t.Fatalf("failure log = %q, want session id and underlying error", got)
	}
}
