package webapp

import (
	"context"

	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/webdebug"
)

// webEvalBrokerAdapter is the only webapp-to-execution bridge. It exposes the
// broker's narrow execution method, never its connection or page selector.
type webEvalBrokerAdapter struct {
	broker *webdebug.Broker
}

func (a webEvalBrokerAdapter) Execute(ctx context.Context, code string, timeoutMS int) (protocol.DebugExecutionResultPayload, error) {
	if a.broker == nil {
		return protocol.DebugExecutionResultPayload{}, webdebug.ErrClosed
	}
	return a.broker.Execute(ctx, code, timeoutMS)
}

func (a webEvalBrokerAdapter) Enabled() bool {
	return a.broker != nil && a.broker.Enabled()
}
