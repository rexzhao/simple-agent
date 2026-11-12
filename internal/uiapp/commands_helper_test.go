package uiapp

import (
	"context"
	"net"
	"testing"

	"simple-agent/internal/providers"
	"simple-agent/internal/relay"
	"simple-agent/internal/tools"
	"simple-agent/internal/wire"
)

// startTestRelay 起一个只听本地回环的 relay（仅用于斜杠命令的端到端验证）。
func startTestRelay(t *testing.T) (*relay.Relay, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	server := relay.New()
	go func() {
		_ = server.ServeAll(func() (wire.Conn, error) {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return nil, wire.ErrClosed
			}
			return wire.NewTCPConn(conn), nil
		})
	}()
	t.Cleanup(func() {
		server.Close()
		_ = listener.Close()
	})
	return server, listener.Addr().String()
}

// stubRuntime 只满足 providers 运行时接口，测试里不会真的开流。
type stubRuntime struct{}

func (r *stubRuntime) OpenStream(ctx context.Context, sel providers.Selection, req providers.ChatRequest) (<-chan providers.Event, error) {
	events := make(chan providers.Event)
	close(events)
	return events, nil
}

func (r *stubRuntime) RunTools(ctx context.Context, tc *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
	return nil
}
