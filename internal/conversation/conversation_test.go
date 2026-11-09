package conversation_test

import (
	"errors"
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
)

// 外部测试包：只能通过导出 API 驱动，由编译器保证（G6：不靠自觉）。

func addMessage(t *testing.T, c *conversation.Conversation, id string, role message.Role, status conversation.EntryStatus) *conversation.ChatEntry {
	t.Helper()
	entry, err := c.AddMessage(message.Message{MessageID: id, Role: role}, status)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// 回归：流式增量必须合并进同一个 part（token 级不会产生上千个 part）。
func TestStreamingMergeKeepsPartCountStable(t *testing.T) {
	c := conversation.New("ctx")
	addMessage(t, c, "m1", message.RoleAgent, conversation.StatusPending)

	for _, delta := range []string{"# ", "静", "夜", "思", "\n", "床前明月光"} {
		if err := c.AppendToStream("m1", delta); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.EndStream("m1"); err != nil {
		t.Fatal(err)
	}

	entry, ok := c.Message("m1")
	if !ok {
		t.Fatal("找不到消息")
	}
	if entry.Status() != conversation.StatusComplete {
		t.Fatalf("状态应为 complete，实际 %v", entry.Status())
	}
	if entry.PartCount() != 1 {
		t.Fatalf("增量应合并为 1 个 part，实际 %d", entry.PartCount())
	}
	if got := entry.PreviewText(); got != "# 静夜思\n床前明月光" {
		t.Fatalf("文本不一致：%q", got)
	}
}

// 回归：附件类 part 不得被流式增量污染（AddPart 要清空合并目标）。
func TestAttachmentPartNotMerged(t *testing.T) {
	c := conversation.New("ctx")
	addMessage(t, c, "m1", message.RoleAgent, conversation.StatusStreaming)
	if err := c.AppendToStream("m1", "先看这段代码"); err != nil {
		t.Fatal(err)
	}

	entry, _ := c.Message("m1")
	// 中间到达一个附件（文本 part，带 filename）
	entry.AddPart(message.NewTextPart("fn main() {}").WithFilename("main.rs"))

	if err := c.AppendToStream("m1", "，解释如下"); err != nil {
		t.Fatal(err)
	}

	if entry.PartCount() != 3 {
		t.Fatalf("应新建 part，实际 %d 个", entry.PartCount())
	}
	if text, _ := entry.Message.Parts[1].AsText(); text != "fn main() {}" {
		t.Fatalf("附件被污染：%q", text)
	}
	if text, _ := entry.Message.Parts[2].AsText(); text != "，解释如下" {
		t.Fatalf("新 part 内容错误：%q", text)
	}
}

// 回归：ID 唯一性是硬约束（列表 key、流定位都依赖它）。
func TestDuplicateMessageIDRejected(t *testing.T) {
	c := conversation.New("ctx")
	addMessage(t, c, "dup", message.RoleUser, conversation.StatusComplete)
	_, err := c.AddMessage(message.Message{MessageID: "dup", Role: message.RoleUser}, conversation.StatusComplete)
	if !errors.Is(err, conversation.ErrDuplicateID) {
		t.Fatalf("重复 ID 应返回 ErrDuplicateID，实际：%v", err)
	}
	if len(c.Entries()) != 1 {
		t.Fatalf("不应插入第二条，实际 %d 条", len(c.Entries()))
	}
}

// 空 ID 直接拒绝（否则第二条空 ID 会报让人困惑的 duplicate）。
func TestEmptyMessageIDRejected(t *testing.T) {
	c := conversation.New("ctx")
	_, err := c.AddMessage(message.Message{Role: message.RoleUser}, conversation.StatusComplete)
	if !errors.Is(err, conversation.ErrEmptyID) {
		t.Fatalf("空 ID 应返回 ErrEmptyID，实际：%v", err)
	}
}

func TestStreamErrorsAreActionable(t *testing.T) {
	c := conversation.New("ctx")
	if err := c.AppendToStream("nope", "x"); !errors.Is(err, conversation.ErrMessageNotFound) {
		t.Fatalf("未知 ID 应返回 ErrMessageNotFound，实际：%v", err)
	}
	addMessage(t, c, "u1", message.RoleUser, conversation.StatusComplete)
	if err := c.AppendToStream("u1", "x"); !errors.Is(err, conversation.ErrNotStreaming) {
		t.Fatalf("非流式消息应返回 ErrNotStreaming，实际：%v", err)
	}
	if err := c.EndStream("u1"); !errors.Is(err, conversation.ErrNotStreaming) {
		t.Fatalf("结束非流式消息应返回 ErrNotStreaming，实际：%v", err)
	}
}

// 用户停止：状态为 Cancelled，已收内容保留，且不进模型历史。
func TestCancelKeepsContentButExcludedFromHistory(t *testing.T) {
	c := conversation.New("ctx")
	userMsg := message.Message{
		MessageID: "u1",
		Role:      message.RoleUser,
		Parts:     []message.Part{message.NewTextPart("写长文")},
	}
	if _, err := c.AddMessage(userMsg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	addMessage(t, c, "a1", message.RoleAgent, conversation.StatusStreaming)
	if err := c.AppendToStream("a1", "已经写了一半"); err != nil {
		t.Fatal(err)
	}
	if err := c.CancelStream("a1"); err != nil {
		t.Fatal(err)
	}

	entry, _ := c.Message("a1")
	if entry.Status() != conversation.StatusCancelled {
		t.Fatalf("应为 cancelled，实际 %v", entry.Status())
	}
	if got := entry.PreviewText(); got != "已经写了一半" {
		t.Fatalf("已收内容应保留，实际 %q", got)
	}

	history := c.History()
	if len(history) != 1 || history[0].MessageID != "u1" {
		t.Fatalf("Cancelled 不应进历史，实际 %d 条：%#v", len(history), history)
	}
}

func TestFailStreamRecordsReason(t *testing.T) {
	c := conversation.New("ctx")
	addMessage(t, c, "a1", message.RoleAgent, conversation.StatusStreaming)
	if err := c.AppendToStream("a1", "部分内容"); err != nil {
		t.Fatal(err)
	}
	if err := c.FailStream("a1", "connection reset"); err != nil {
		t.Fatal(err)
	}
	entry, _ := c.Message("a1")
	if entry.Status() != conversation.StatusFailed || entry.FailureReason() != "connection reset" {
		t.Fatalf("状态/原因不一致：%v / %q", entry.Status(), entry.FailureReason())
	}
	if entry.PreviewText() != "部分内容" {
		t.Fatal("失败时已收内容应保留")
	}
}

// 回归：Failed 也是终态（M5 按 IsTerminal 决定是否落盘 finalize）。
func TestTerminalAndHistoryStatuses(t *testing.T) {
	tests := []struct {
		status    conversation.EntryStatus
		terminal  bool
		inHistory bool
	}{
		{conversation.StatusPending, false, false},
		{conversation.StatusStreaming, false, false},
		{conversation.StatusComplete, true, true},
		{conversation.StatusCancelled, true, false},
		{conversation.StatusFailed, true, false},
	}
	for _, tc := range tests {
		if got := tc.status.IsTerminal(); got != tc.terminal {
			t.Fatalf("%v IsTerminal 应为 %v，实际 %v", tc.status, tc.terminal, got)
		}
		if got := tc.status.InHistory(); got != tc.inHistory {
			t.Fatalf("%v InHistory 应为 %v，实际 %v", tc.status, tc.inHistory, got)
		}
	}
}

func TestNoticesAreSkippedByMessageLookup(t *testing.T) {
	c := conversation.New("ctx")
	addMessage(t, c, "u1", message.RoleUser, conversation.StatusComplete)
	notice, err := c.AddNotice(conversation.NoticeWarn, "已断开")
	if err != nil {
		t.Fatal(err)
	}
	if notice.ID == "" {
		t.Fatal("通知 ID 应自动生成")
	}

	last, ok := c.LastMessage()
	if !ok || last.Message.MessageID != "u1" {
		t.Fatalf("LastMessage 应跳过通知，实际 %#v", last)
	}
	entries := c.Entries()
	if len(entries) != 2 || entries[1].ID() != notice.ID {
		t.Fatalf("通知应保留在列表里，实际 %d 条", len(entries))
	}
}

// Parts 必须在入会话时克隆：调用方之后改自己的切片不应影响会话。
func TestPartsAreClonedOnAdd(t *testing.T) {
	c := conversation.New("ctx")
	parts := []message.Part{message.NewTextPart("原始")}
	if _, err := c.AddMessage(message.Message{MessageID: "m1", Role: message.RoleUser, Parts: parts}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	// 改调用方手里的切片内容
	parts[0] = message.NewTextPart("被篡改")

	entry, _ := c.Message("m1")
	if text, _ := entry.Message.Parts[0].AsText(); text != "原始" {
		t.Fatalf("会话内容不应随调用方切片变化，实际 %q", text)
	}
}

// Entries() 返回副本：调用方改返回的切片不影响会话。
func TestEntriesReturnsCopy(t *testing.T) {
	c := conversation.New("ctx")
	addMessage(t, c, "m1", message.RoleUser, conversation.StatusComplete)

	entries := c.Entries()
	entries[0] = conversation.Entry{}

	if got := c.Entries()[0].ID(); got != "m1" {
		t.Fatalf("Entries 应是副本，实际 %q", got)
	}
}

func TestContextIDIsStable(t *testing.T) {
	c := conversation.New("ctx-42")
	addMessage(t, c, "m", message.RoleUser, conversation.StatusComplete)
	if c.ContextID() != "ctx-42" {
		t.Fatalf("contextID 不一致：%q", c.ContextID())
	}
}

func TestHistoryOnlyComplete(t *testing.T) {
	c := conversation.New("ctx")
	statuses := []conversation.EntryStatus{
		conversation.StatusComplete, conversation.StatusStreaming,
		conversation.StatusFailed, conversation.StatusCancelled, conversation.StatusPending,
	}
	for i, st := range statuses {
		id := "m" + string(rune('0'+i))
		addMessage(t, c, id, message.RoleAgent, st)
	}
	history := c.History()
	if len(history) != 1 || history[0].MessageID != "m0" {
		t.Fatalf("只有 complete 进历史，实际：%#v", history)
	}
}

// Pending 收到首个增量后应转为 Streaming。
func TestPendingBecomesStreamingOnFirstDelta(t *testing.T) {
	c := conversation.New("ctx")
	addMessage(t, c, "a1", message.RoleAgent, conversation.StatusPending)
	if err := c.AppendToStream("a1", "x"); err != nil {
		t.Fatal(err)
	}
	entry, _ := c.Message("a1")
	if entry.Status() != conversation.StatusStreaming {
		t.Fatalf("首个增量后应为 streaming，实际 %v", entry.Status())
	}
}
