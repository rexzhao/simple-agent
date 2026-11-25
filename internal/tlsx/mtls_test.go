package tlsx_test

import (
	"crypto/tls"
	"net"
	"testing"
	"time"

	"simple-agent/internal/tlsx"
)

// 端到端：服务端要求客户端证书时，客户端确实会出示它。
//
// 这是 mTLS 的核心前提，踩过两个坑：
//  1. 服务端没设 ClientAuth → 根本不发 CertificateRequest，客户端不发证书
//  2. 调用方在 accept 后立刻查 ConnectionState() → 握手还没做，证书是空的
//
// 这条测试覆盖第 1 个（第 2 个在 cmd 层，靠 Handshake 显式触发）。
func TestServerRequestsClientCertificate(t *testing.T) {
	serverDir := t.TempDir()
	clientDir := t.TempDir()

	serverCfg := tlsx.Config{
		CertFile: serverDir + "/cert.pem",
		KeyFile:  serverDir + "/key.pem",
	}
	serverFingerprint, err := serverCfg.EnsureCert()
	if err != nil {
		t.Fatal(err)
	}

	// 客户端自己的证书（与执行端共用 DataDir 的机制一致）
	clientCertCfg := tlsx.Config{DataDir: clientDir}
	if _, err := clientCertCfg.EnsureCert(); err != nil {
		t.Fatal(err)
	}

	listener, err := serverCfg.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	// 服务端：接受连接并读对端证书
	peerCert := make(chan string, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		tlsConn, ok := conn.(*tls.Conn)
		if !ok {
			peerCert <- ""
			return
		}
		// 必须显式握手：懒握手时 ConnectionState 里还没有对端证书
		if handshakeErr := tlsConn.Handshake(); handshakeErr != nil {
			peerCert <- ""
			return
		}
		state := tlsConn.ConnectionState()
		if len(state.PeerCertificates) == 0 {
			peerCert <- ""
			return
		}
		peerCert <- "presented"
	}()

	// 客户端：连上去，出示自己的证书
	clientCfg := tlsx.Config{
		ClientTLSEnabled: true,
		DataDir:          clientDir,
		Fingerprint:      serverFingerprint,
	}
	conn, err := clientCfg.Dial(listener.Addr().String())
	if err != nil {
		t.Fatalf("客户端连接失败：%v", err)
	}
	defer conn.Close()

	select {
	case result := <-peerCert:
		if result != "presented" {
			t.Fatal("服务端没有收到客户端证书 —— ClientAuth 没生效，或客户端没出示")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("服务端没有响应")
	}
}

// 服务端配了 ClientAuth 但客户端没有证书时，连接仍然建立（RequestClientCert
// 是"请求"而非"强制"），只是对端证书为空 —— 由应用层决定是否放行。
//
// 这个行为是刻意的：验证逻辑在 peerverify（按指纹查信任列表），
// 用 CA 链强制校验会与自签证书模型冲突。
func TestServerToleratesClientWithoutCertificate(t *testing.T) {
	serverDir := t.TempDir()
	serverCfg := tlsx.Config{
		CertFile: serverDir + "/cert.pem",
		KeyFile:  serverDir + "/key.pem",
	}
	fingerprint, err := serverCfg.EnsureCert()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := serverCfg.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	done := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			if tlsConn, ok := conn.(*tls.Conn); ok {
				_ = tlsConn.Handshake()
			}
			conn.Close()
		}
		close(done)
	}()

	// 客户端没有任何证书（DataDir 为空）
	clientCfg := tlsx.Config{ClientTLSEnabled: true, Fingerprint: fingerprint}
	conn, err := clientCfg.Dial(listener.Addr().String())
	if err != nil {
		// 服务端用 RequestClientCert（非强制），握手应当成功；
		// 这里失败说明用错了 ClientAuth 级别
		t.Fatalf("无客户端证书时握手不该失败（应为 RequestClientCert）：%v", err)
	}
	conn.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("服务端没有响应")
	}
}

var _ net.Conn