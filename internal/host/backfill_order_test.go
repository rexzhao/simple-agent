package host_test

// 回归测试：backfill 补拉进来的更早事件必须排在**前面**，
// 不能追加到末尾。
//
// 曾经的 bug：直接用 driver.Mirror.ApplyPersisted 把补拉结果注入镜像，
// 那是"追加到会话末尾"的语义。于是时间线顺序变成
// 「新的 → 老的历史 → 新的」，分页的 seq 区间也随之错乱
// （真机日志里出现过 seq=3333..18，from 比 to 大）。

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/message"
	"simple-agent/internal/store"
)

// makeInput 造一条 message.input 事件（seq 由调用方给定）。
func makeInput(t *testing.T, seq uint64, id, text string) store.Event {
	t.Helper()
	event, err := store.NewEvent(store.TypeMessageInput, driver.MessageEventData{
		Round: 1, Turn: 1, Role: string(message.RoleUser),
		Message: message.Message{
			MessageID: id,
			Role:      message.RoleUser,
			Parts:     []message.Part{message.NewTextPart(text)},
		},
		Status: conversation.StatusComplete,
	})
	if err != nil {
		t.Fatal(err)
	}
	event.Seq = seq
	return event
}

// TestHistoryPrependsNotAppends 验证补拉的内容排在已有内容之前。
func TestHistoryPrependsNotAppends(t *testing.T) {
	_, session := persistentHost(t)

	// 当前内容（seq 大）
	for i := 0; i < 3; i++ {
		if err := host.AppendUserMessageForTest(session, "现在的对话"); err != nil {
			t.Fatalf("追加失败：%v", err)
		}
	}
	current := session.EntriesWithHistory()
	if len(current) == 0 {
		t.Fatal("追加后应有条目")
	}
	// 记下当前最早的 seq
	var currentOldest uint64
	for _, entry := range current {
		if entry.Message == nil {
			continue
		}
		if first, _ := entry.Message.SeqRange(); first > 0 {
			currentOldest = first
			break
		}
	}

	// 补一段**更早**的历史（seq 比现有都小）
	older := []store.Event{
		makeInput(t, 1, "old-1", "很早的问题"),
		makeInput(t, 2, "old-2", "更早的问题"),
	}
	applied := session.ApplyHistory(older)
	if applied != len(older) {
		t.Fatalf("应接受 %d 条，实际 %d", len(older), applied)
	}

	entries := session.EntriesWithHistory()
	if len(entries) <= len(current) {
		t.Fatalf("补拉后条目应变多：%d -> %d", len(current), len(entries))
	}

	// 关键断言：补进来的老内容必须**排在最前**
	if entries[0].Message == nil || entries[0].Message.Message.MessageID != "old-1" {
		got := "(nil)"
		if entries[0].Message != nil {
			got = entries[0].Message.Message.MessageID
		}
		t.Fatalf("补拉的历史应排在最前，实际第一条是 %q", got)
	}
	if entries[1].Message == nil || entries[1].Message.Message.MessageID != "old-2" {
		t.Fatal("第二条应是 old-2")
	}

	// 整体 seq 必须单调不减 —— 追加到末尾会在这里失败
	var prev uint64
	for index, entry := range entries {
		if entry.Message == nil {
			continue
		}
		first, _ := entry.Message.SeqRange()
		if first == 0 {
			continue
		}
		if prev != 0 && first < prev {
			t.Fatalf("第 %d 条 seq=%d < 前一条 %d —— 顺序错了（历史被追加到末尾）",
				index, first, prev)
		}
		prev = first
		_ = currentOldest
	}
}

// 补拉后 OldestContentSeq 应该变小（能看到的范围往前扩了）。
func TestOldestSeqShrinksAfterBackfill(t *testing.T) {
	_, session := persistentHost(t)
	for i := 0; i < 5; i++ {
		_ = host.AppendUserMessageForTest(session, "对话")
	}
	oldOldest := session.OldestContentSeq()
	if oldOldest == 0 {
		t.Skip("没有内容事件")
	}
	if oldOldest <= 2 {
		t.Skipf("现有最早 seq 太小（%d），造不出更早的", oldOldest)
	}

	session.ApplyHistory([]store.Event{
		makeInput(t, 1, "ancient", "远古问题"),
	})

	newOldest := session.OldestContentSeq()
	if newOldest == 0 {
		t.Fatal("补拉后不该是 0")
	}
	if newOldest >= oldOldest {
		t.Fatalf("补拉更早内容后，最早 seq 应变小：%d -> %d", oldOldest, newOldest)
	}
}