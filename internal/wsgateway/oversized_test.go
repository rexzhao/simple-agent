package wsgateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayRejectsOversizedFirstFrame(t *testing.T) {
	endpoint := newTestEndpoint(t, Options{
		Limits:   Limits{MaxMessageBytes: 128},
		Observer: newRecordingObserver(),
	})
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "test done")
	if err := connection.Write(context.Background(), websocket.MessageText, []byte(strings.Repeat("x", 129))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := connection.Read(ctx)
		if err == nil {
			continue
		}
		got := websocket.CloseStatus(err)
		if got != websocket.StatusMessageTooBig && got != websocket.StatusProtocolError {
			t.Fatalf("oversized frame close code=%v, want message-too-big or protocol-error", got)
		}
		return
	}
}
