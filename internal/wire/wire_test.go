package wire_test

import (
	"bytes"
	"testing"

	"simple-agent/internal/wire"
)

// 进程内连接：收发的包保序、内容一致。
func TestPipePair(t *testing.T) {
	viewer, executor := wire.PipePair()
	go func() {
		for i := 0; i < 3; i++ {
			packet, err := executor.Recv()
			if err != nil {
				return
			}
			_ = executor.Send(wire.Packet{Conn: packet.Conn, Kind: "echo", Body: packet.Body})
		}
	}()
	for i := 0; i < 3; i++ {
		body, _ := wire.Encode(map[string]int{"i": i})
		if err := viewer.Send(wire.Packet{Kind: "ping", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		packet, err := viewer.Recv()
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]int
		if err := wire.Decode(packet.Body, &value); err != nil {
			t.Fatal(err)
		}
		if value["i"] != i {
			t.Fatalf("顺序不对：%d != %d", value["i"], i)
		}
	}
}

// 超大载荷按包上限分块，拼回后字节完全一致；篡改会校验失败。
func TestFragmentsRoundTrip(t *testing.T) {
	viewer, executor := wire.PipePair()
	payload := bytes.Repeat([]byte("远程状态同步 payload "), 20000) // ~ 500 KB
	go func() {
		if err := wire.SendLarge(executor, 7, "msg-1", "log.events", payload, 4096); err != nil {
			t.Error(err)
		}
	}()

	reassembler := wire.NewReassembler()
	for {
		packet, err := viewer.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if packet.Kind != "fragment" {
			continue
		}
		if packet.Conn != 7 {
			t.Fatalf("逻辑通道 id 丢了：%d", packet.Conn)
		}
		var fragment wire.Fragment
		if err := wire.Decode(packet.Body, &fragment); err != nil {
			t.Fatal(err)
		}
		if len(packet.Body) > 4096+512 {
			t.Fatalf("单包超过上限：%d", len(packet.Body))
		}
		kind, data, done, err := reassembler.Add(fragment)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			if kind != "log.events" {
				t.Fatalf("重组后的逻辑类型丢了：%q", kind)
			}
			if !bytes.Equal(data, payload) {
				t.Fatal("拼回来的内容不一致")
			}
			return
		}
	}
}

// 分块校验：内容被改过就报错，不会静默拼成坏数据。
func TestFragmentChecksum(t *testing.T) {
	reassembler := wire.NewReassembler()
	fragment := wire.Fragment{MsgID: "m", Index: 0, Count: 1, Total: 3, SHA256: "deadbeef", Data: []byte("abc")}
	if _, _, done, err := reassembler.Add(fragment); err == nil || done {
		t.Fatalf("sha256 不匹配应报错：done=%v err=%v", done, err)
	}
}
