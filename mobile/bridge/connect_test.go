package bridge

import (
	"net"
	"testing"
	"time"

	"simple-agent/internal/relay"
	"simple-agent/internal/wire"
)

// 回归测试：连着的时候再次 Connect 应当"先断开再重连"，而不是报
// "已经连接过了，请先断开"。
//
// 曾经的 bug：连接页没有断开按钮（断开在侧边抽屉里），
// 所以用户点「连接」后卡在一个既连不上也断不开的状态。
//
// 这里用一个假的 relay：任何连接都回"执行端列表为空"，
// 只要能走到"重新拨号"这一步，就说明重复连接被允许了。
func TestConnectTwiceReconnects(t *testing.T) {
	listener, addr := startFakeRelay(t)
	defer listener.Close()

	client := NewClient(discardListener{}, "zh", t.TempDir())
	defer client.Close()

	// 第一次连接：必然失败（没有执行端），但不能报"已经连接过了"
	first := client.Connect(addr, "", "")
	if first == errAlreadyConnected {
		t.Fatalf("第一次连接不该报这个：%s", first)
	}

	// 第二次连接：仍然不该报"已经连接过了"
	second := client.Connect(addr, "", "")
	if second == errAlreadyConnected {
		t.Fatalf("第二次连接仍然被拒绝（重连应当被允许）：%s", second)
	}
}

// 断开之后能重新连接（基本可用性）。
func TestConnectAfterDisconnect(t *testing.T) {
	listener, addr := startFakeRelay(t)
	defer listener.Close()

	client := NewClient(discardListener{}, "zh", t.TempDir())
	defer client.Close()

	_ = client.Connect(addr, "", "")
	client.Disconnect()
	time.Sleep(60 * time.Millisecond)

	if got := client.Connect(addr, "", ""); got == errAlreadyConnected {
		t.Fatal("断开后重新连接被拒绝")
	}
}

// discardListener 忽略所有变更通知（测试不关心 UI 刷新）。
type discardListener struct{}

func (discardListener) OnChange(string, string) {}

// errAlreadyConnected 是那条曾经会卡住用户的错误（改动后不该再出现）。
const errAlreadyConnected = "已经连接过了，请先断开"

// startFakeRelay 起一个只回"空执行端列表"的假 relay（够让首连走到"没有执行端"）。
func startFakeRelay(t *testing.T) (net.Listener, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				packetConn := wire.NewTCPConn(conn)
				for {
					packet, recvErr := packetConn.Recv()
					if recvErr != nil {
						return
					}
					// 对 hello 探测与列表查询都回"空列表"：展示端会据此报"没有执行端"
					if packet.Kind == "hello" || packet.Kind == relay.MsgList {
						_ = packetConn.Send(wire.Packet{
							Kind: relay.MsgExecutors,
							Body: []byte("[]"),
						})
					}
				}
			}()
		}
	}()
	return listener, listener.Addr().String()
}
