package relay

import (
	"net"
	"testing"

	"simple-agent/internal/wire"
)

// 回归测试：限流必须能拿到来源 IP。
//
// 曾经的真实 bug：tcpConn/heartbeatConn 没有暴露 RemoteAddr，
// remoteIP 永远返回空字符串，于是按 IP 限流完全没生效——
// 日志里表现为 "认证失败 ip= 连续失败=0"，而单测因为直接用管道连接
// （本来就没有地址）完全测不出来。
func TestRemoteIPFromTCPConn(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	clientRaw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("拨号失败：%v", err)
	}
	defer clientRaw.Close()

	serverRaw := <-accepted
	defer serverRaw.Close()

	// 用 wire 的真实实现（NewTCPConn 会套上心跳包装）：
	// 验证接口断言能穿透两层包装拿到 RemoteAddr
	packetConn := wire.NewTCPConn(serverRaw)
	ip := remoteIP(packetConn)
	if ip == "" {
		t.Fatal("remoteIP 拿不到地址：按 IP 限流会完全失效")
	}
	if ip != "127.0.0.1" {
		t.Fatalf("期望 127.0.0.1，实际 %q", ip)
	}
}

// 管道连接没有地址：返回空是预期行为（限流对那种连接不生效，也不该生效）。
func TestRemoteIPEmptyForPipe(t *testing.T) {
	left, right := wire.PipePair()
	defer left.Close()
	defer right.Close()
	if ip := remoteIP(left); ip != "" {
		t.Fatalf("管道连接不该有 IP，实际 %q", ip)
	}
}
