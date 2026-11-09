package wire_test

import (
	"testing"

	"simple-agent/internal/wire"
)

// 一条底层连接上开两条逻辑通道：互不串包。
func TestMuxSeparatesChannels(t *testing.T) {
	left, right := wire.PipePair()
	client := wire.NewMux(left)
	defer client.Close()
	server := wire.NewMux(right)
	defer server.Close()

	a := client.Open(1)
	b := client.Open(2)
	if err := a.Send(wire.Packet{Kind: "x", Body: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	if err := b.Send(wire.Packet{Kind: "y", Body: []byte("b")}); err != nil {
		t.Fatal(err)
	}

	gotA, err := server.Accept()
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := server.Accept()
	if err != nil {
		t.Fatal(err)
	}
	packetA, _ := gotA.Recv()
	packetB, _ := gotB.Recv()
	if string(packetA.Body) != "a" || packetB.Body[0] != 'b' {
		t.Fatalf("通道串包：%q %q", packetA.Body, packetB.Body)
	}

	// 反向也走各自的通道
	if err := gotA.Send(wire.Packet{Kind: "reply", Body: []byte("ra")}); err != nil {
		t.Fatal(err)
	}
	reply, err := a.Recv()
	if err != nil || string(reply.Body) != "ra" {
		t.Fatalf("回包不对：%q %v", reply.Body, err)
	}
}
