package uiapp

import (
	"context"
	"testing"

	"simple-agent/internal/providers"
	"simple-agent/internal/relay"
	"simple-agent/internal/tlsx"
	"simple-agent/internal/tools"
	"simple-agent/internal/wire"
)

// startTestRelay 起一个只听本地回环的 relay（仅用于斜杠命令的端到端验证）。
func startTestRelay(t *testing.T) (*relay.Relay, string) {
	t.Helper()
	return startTestRelayTLS(t)
}

// startTestRelayTLS 起一个**启用 TLS** 的最小 relay，返回 (server, 地址, 指纹)。
//
// 为什么要 TLS：加密与明文严格隔离之后，客户端会拒绝明文对端
// （那样攻击者让握手失败就能降级）。所以测试也必须走 TLS，
// 否则测的是"连接被正确拒绝"，而不是"命令驱动了注册逻辑"。
func startTestRelayTLS(t *testing.T) (*relay.Relay, string) {
	t.Helper()
	server, addr, _ := startTestRelayWithFingerprint(t)
	return server, addr
}

// startTestRelayWithFingerprint 同 startTestRelayTLS，另外返回 relay 指纹。
func startTestRelayWithFingerprint(t *testing.T) (*relay.Relay, string, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := tlsx.Config{CertFile: dir + "/cert.pem", KeyFile: dir + "/key.pem"}
	fingerprint, err := cfg.EnsureCert()
	if err != nil {
		t.Fatalf("生成证书失败：%v", err)
	}
	listener, err := cfg.Listen("127.0.0.1:0")
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
	return server, listener.Addr().String(), fingerprint
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
