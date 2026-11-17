package timeline_test

// 回归测试：Ref.Part() 必须做边界检查。
//
// 真机上 App 反复崩溃，日志里只有一行：
//   panic: runtime error: index out of range [3] with length 3
//   panic: runtime error: index out of range [1] with length 1
//
// 模式很有特征：索引正好等于长度。来源是某条消息的 Parts 在 Ref 生成
// 之后变短了（backfill 把同一批事件重放到另一份 Conversation、流式追加等），
// 而 Ref.Part() 原来是裸下标访问 —— 直接 panic，进程消失。

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/timeline"
)

func TestRefPartOutOfRangeDoesNotPanic(t *testing.T) {
	msg := message.Message{
		MessageID: "m1",
		Role:      message.RoleAgent,
		Parts:     []message.Part{message.NewTextPart("只有一个 part")},
	}
	entry := &conversation.ChatEntry{Message: &msg}

	cases := []struct {
		name  string
		ref   timeline.Ref
		index int
	}{
		{"越界（正好等于长度）", timeline.Ref{Chat: entry, Index: 1}, 1},
		{"越界（远大于长度）", timeline.Ref{Chat: entry, Index: 99}, 99},
		{"负数下标", timeline.Ref{Chat: entry, Index: -1}, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 只要不 panic 就算通过；返回零值 part 是预期行为
			_ = c.ref.Part()
		})
	}

	// 正常下标仍要能取到内容
	ok := timeline.Ref{Chat: entry, Index: 0}
	if text, hasText := ok.Part().AsText(); !hasText || text != "只有一个 part" {
		t.Fatalf("合法下标应能取到 part，实际 %q (ok=%v)", text, hasText)
	}
}

func TestRefPartNilChatDoesNotPanic(t *testing.T) {
	// Chat 为 nil、Message 为 nil 都不该崩
	_ = timeline.Ref{Chat: nil, Index: 0}.Part()
	_ = timeline.Ref{Chat: &conversation.ChatEntry{}, Index: 0}.Part()
}