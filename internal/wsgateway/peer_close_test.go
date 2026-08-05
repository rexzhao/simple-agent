package wsgateway

import (
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayPeerCloseFinishesLifecycle(t *testing.T) {
	observer := newRecordingObserver()
	endpoint := newTestEndpoint(t, Options{Observer: observer})
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	writeHello(t, connection, "tab")
	_ = readProtocol(t, connection)
	if err := connection.Close(websocket.StatusNormalClosure, "client done"); err != nil {
		t.Logf("client close: %v", err)
	}
	select {
	case <-observer.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("peer close did not finish gateway lifecycle")
	}
}
