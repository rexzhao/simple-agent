package wire_test

import (
	"errors"
	"net"
	"testing"
	"time"

	"simple-agent/internal/wire"
)

// 心跳：对端彻底静默（半开连接）时，本端应当自己判定断开，而不是永远卡在 Recv。
func TestHeartbeatDetectsSilentPeer(t *testing.T) {
	wire.SetHeartbeatForTest(20*time.Millisecond, 60*time.Millisecond)
	defer wire.SetHeartbeatForTest(15*time.Second, 30*time.Second)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	// 服务端用裸 socket：既不读也不写（半开/NAT 超时）
	defer server.Close()

	conn := wire.NewTCPConn(client)
	defer conn.Close()

	if err := conn.Send(wire.Packet{Kind: "hello"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for {
			if _, recvErr := conn.Recv(); recvErr != nil {
				done <- recvErr
				return
			}
		}
	}()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, wire.ErrClosed) {
			return // 自己关的也算断开
		}
	case <-time.After(2 * time.Second):
		t.Fatal("对端静默时没有判定断开")
	}
}

// 本端一直发包、对端从不回包：仍然要在 deadAfter 左右判定断开
// （Send 不能刷新 lastSeen，否则半开连接永远检测不出来）。
func TestHeartbeatDetectsSilentPeerWhileSending(t *testing.T) {
	wire.SetHeartbeatForTest(20*time.Millisecond, 120*time.Millisecond)
	defer wire.SetHeartbeatForTest(15*time.Second, 30*time.Second)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	defer server.Close() // 对端只收不回

	conn := wire.NewTCPConn(client)
	defer conn.Close()

	done := make(chan error, 1)
	go func() {
		for {
			if _, recvErr := conn.Recv(); recvErr != nil {
				done <- recvErr
				return
			}
		}
	}()

	// 每秒（这里 10ms）发一个包，对端从不回
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			if err := conn.Send(wire.Packet{Kind: "event", Body: []byte("x")}); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	start := time.Now()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("判定断开太晚：%v（Send 刷新了 lastSeen？）", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("一直发包时反而检测不到静默对端")
	}
}
