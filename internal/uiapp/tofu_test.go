package uiapp

import (
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"

	"simple-agent/internal/tlsx"
)

// TOFU：连一台**启用 TLS** 的 relay 时，不传指纹应该先探测、等用户确认，
// 而不是直接失败、也不要用户手抄 64 位十六进制。
//
// 这是"跟 SSH 一样"的核心行为：
//  1. 首次连接 → 拿到指纹 → 弹确认框
//  2. 用户确认 → 保存指纹并连接
//  3. 以后同一台 relay → 直接用存下的指纹，不再询问
func TestRelayTOFUAsksForFingerprint(t *testing.T) {
	addr := startTLSProbeServer(t)
	app := newLocalApp(t)

	message, ok := app.ExecSlashCommand("/connect " + addr + " testtoken")
	if !ok {
		t.Fatal("/connect 应被识别为命令")
	}
	if !strings.Contains(message, "指纹") {
		t.Fatalf("应提示正在获取指纹，实际：%q", message)
	}
	pending := app.pendingFingerprint
	if pending == nil {
		t.Fatal("应进入等待确认状态")
	}
	if pending.fingerprint == "" {
		t.Fatal("确认框里应有指纹")
	}
	if pending.changed {
		t.Fatal("首次连接不该标记为“指纹变了”")
	}
}

// 已确认过的指纹要记住：同一台 relay 再来一次不再询问。
func TestRelayTOFURemembersConfirmed(t *testing.T) {
	addr := startTLSProbeServer(t)
	local, _ := newLocalForCommandTest(t)
	app := newCommandTestApp(t, local, t.TempDir())

	fingerprint, err := tlsx.ProbeFingerprint(addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	a := app
	// 模拟"用户以前确认过这台 relay 的指纹"
	a.saveRelay(savedRelay{Addr: addr, Token: "testtoken", Fingerprint: fingerprint})

	message, _ := a.ExecSlashCommand("/connect " + addr + " testtoken")
	if strings.Contains(message, "指纹") {
		t.Fatalf("已确认过的指纹不该再询问，实际：%q", message)
	}
	if a.pendingFingerprint != nil {
		t.Fatal("不该弹确认框")
	}
}

// 指纹变了（同一地址、不同证书）要单独提示 —— 那可能是换证书，也可能是冒充。
func TestRelayTOFUFlagsChangedFingerprint(t *testing.T) {
	addr := startTLSProbeServer(t)
	local, _ := newLocalForCommandTest(t)
	app := newCommandTestApp(t, local, t.TempDir())

	// 假装以前确认过**另一个**指纹
	app.saveRelay(savedRelay{
		Addr: addr, Token: "testtoken",
		Fingerprint: strings.Repeat("ab", 32),
	})

	message, _ := app.ExecSlashCommand("/connect " + addr + " testtoken")
	if !strings.Contains(message, "指纹") {
		t.Fatalf("应走探测，实际：%q", message)
	}
	pending := app.pendingFingerprint
	if pending == nil {
		t.Fatal("应进入等待确认状态")
	}
	if !pending.changed {
		t.Fatal("指纹与上次不同时应标记为 changed")
	}
}

// startTLSProbeServer 起一个会做 TLS 握手的最小服务器（只为让探测拿到证书）。
func startTLSProbeServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := tlsx.Config{CertFile: dir + "/cert.pem", KeyFile: dir + "/key.pem"}
	if _, err := cfg.EnsureCert(); err != nil {
		t.Fatal(err)
	}
	listener, err := cfg.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// **必须真的完成握手**：直接 Close 会让客户端拿不到证书，
			// 探测于是把对端判成"明文"（这是 tls.Listener 的特性 ——
			// 证书是在握手时才发出去的）。
			go func(c net.Conn) {
				if tlsConn, ok := c.(*tls.Conn); ok {
					_ = tlsConn.Handshake()
				}
				_ = c.Close()
			}(conn)
		}
	}()
	return listener.Addr().String()
}