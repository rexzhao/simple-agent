package host

import (
	"testing"
	"time"

	"simple-agent/internal/conversation"
	"simple-agent/internal/store"
	"simple-agent/internal/wire"
)

// 快照说"已空闲"时，镜像里残留的 streaming 条目要就地收尾
// （终结事件在断线/切换时丢了，否则工具卡片永远显示 running）。
func TestIdleSnapshotClosesStaleStreaming(t *testing.T) {
	viewer, executor := wire.PipePair()
	queue := make(chan func(), 1024)
	drain := func() {
		for {
			select {
			case fn := <-queue:
				fn()
			default:
				return
			}
		}
	}
	post := func(f func()) { queue <- f }
	remote, err := NewRemote(viewer, post)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	send := func(kind string, payload any) {
		body, encodeErr := wire.Encode(payload)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		if err := executor.Send(wire.Packet{Kind: kind, Body: body}); err != nil {
			t.Fatal(err)
		}
	}

	send(msgCatalog, []SessionRef{{ID: "s1", Title: "会话"}})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && remote.Session("s1") == nil {
		drain()
		time.Sleep(5 * time.Millisecond)
	}
	drain()
	session := remote.Session("s1")
	if session == nil {
		t.Fatal("没有会话")
	}

	event, err := store.NewEvent(store.TypeMessageAppended, map[string]any{
		"message": map[string]any{"messageId": "m1", "role": "ROLE_AGENT", "parts": []any{}},
		"status":  string(conversation.StatusStreaming),
	})
	if err != nil {
		t.Fatal(err)
	}
	event.Seq = 1
	send(msgEvent, map[string]any{"session_id": "s1", "event": event})

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		drain()
		if entry, ok := session.Conversation().Message("m1"); ok && entry.Status() == conversation.StatusStreaming {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	send(msgState, map[string]any{
		"session_id": "s1",
		"state":      ViewState{Busy: "idle"},
	})
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		drain()
		entry, ok := session.Conversation().Message("m1")
		if ok && entry.Status() != conversation.StatusStreaming {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	entry, ok := session.Conversation().Message("m1")
	if !ok {
		t.Fatal("消息不见了")
	}
	if entry.Status() == conversation.StatusStreaming {
		t.Fatal("空闲快照之后仍然是 streaming（工具会一直 running）")
	}
}
