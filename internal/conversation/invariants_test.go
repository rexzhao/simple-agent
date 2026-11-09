package conversation_test

import (
	"errors"
	"strings"
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
)

// 这组用例只覆盖**状态机的不变量**：哪些调用在什么状态下必须被拒绝，
// 以及「增量只并入流式目标」这类容易被后续改动破坏的约束。
// 覆盖率数字本身不是目标。

func newConv(t *testing.T) (*conversation.Conversation, *conversation.ChatEntry) {
	t.Helper()
	conv := conversation.New("ctx")
	entry, err := conv.AddMessage(message.Message{
		MessageID: "m1",
		Role:      message.RoleAgent,
		Parts:     []message.Part{message.NewTextPart("hello")},
	}, conversation.StatusPending)
	if err != nil {
		t.Fatal(err)
	}
	return conv, entry
}

func TestAppendToStreamRequiresStreaming(t *testing.T) {
	conv, _ := newConv(t)
	// Pending 允许（第一次增量会把状态推进到 streaming）
	if err := conv.AppendToStream("m1", "!"); err != nil {
		t.Fatalf("pending 时应允许追加：%v", err)
	}
	if err := conv.EndStream("m1"); err != nil {
		t.Fatal(err)
	}
	// 结束后再追加必须被拒绝
	if err := conv.AppendToStream("m1", "more"); !errors.Is(err, conversation.ErrNotStreaming) {
		t.Fatalf("complete 后追加应返回 ErrNotStreaming，实际 %v", err)
	}
	// 不存在的 id
	if err := conv.AppendToStream("nope", "x"); !errors.Is(err, conversation.ErrMessageNotFound) {
		t.Fatalf("未知 id 应返回 ErrMessageNotFound，实际 %v", err)
	}
}

func TestAppendToPartRequiresStreamingAndText(t *testing.T) {
	conv, entry := newConv(t)
	// pending 也算流式（与 AppendToStream 一致）
	if err := conv.AppendToPart("m1", 0, "+"); err != nil {
		t.Fatalf("pending 时应允许按 part 追加：%v", err)
	}
	if got := entry.PreviewText(); got != "hello+" {
		t.Fatalf("内容不一致：%q", got)
	}

	// 下标越界
	if err := conv.AppendToPart("m1", 9, "x"); !errors.Is(err, conversation.ErrPartNotFound) {
		t.Fatalf("越界应返回 ErrPartNotFound，实际 %v", err)
	}
	// 非文本 part
	toolPart := message.NewDataPart(map[string]any{"id": "c1"}).
		WithMediaType("application/vnd.simple-agent.tool-use+json")
	index := entry.AddPart(toolPart)
	if err := conv.AppendToPart("m1", index, "x"); !errors.Is(err, conversation.ErrNotTextPart) {
		t.Fatalf("非文本 part 应返回 ErrNotTextPart，实际 %v", err)
	}

	// 结束之后不能再追加
	if err := conv.EndStream("m1"); err != nil {
		t.Fatal(err)
	}
	if err := conv.AppendToPart("m1", 0, "x"); !errors.Is(err, conversation.ErrNotStreaming) {
		t.Fatalf("complete 后应返回 ErrNotStreaming，实际 %v", err)
	}
}

func TestFinishRequiresStreaming(t *testing.T) {
	conv, _ := newConv(t)
	if err := conv.EndStream("m1"); err != nil {
		t.Fatal(err)
	}
	for _, finish := range []func(string) error{
		conv.EndStream,
		conv.CancelStream,
		func(id string) error { return conv.FailStream(id, "x") },
	} {
		if err := finish("m1"); !errors.Is(err, conversation.ErrNotStreaming) {
			t.Fatalf("重复结束应返回 ErrNotStreaming，实际 %v", err)
		}
	}
	if err := conv.EndStream("nope"); !errors.Is(err, conversation.ErrMessageNotFound) {
		t.Fatalf("未知 id 应返回 ErrMessageNotFound，实际 %v", err)
	}
}

func TestAddMessageRejectsEmptyAndDuplicate(t *testing.T) {
	conv := conversation.New("ctx")
	if _, err := conv.AddMessage(message.Message{Role: message.RoleUser}, conversation.StatusComplete); !errors.Is(err, conversation.ErrEmptyID) {
		t.Fatalf("空 id 应被拒绝，实际 %v", err)
	}
	msg := message.Message{MessageID: "dup", Role: message.RoleUser}
	if _, err := conv.AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	if _, err := conv.AddMessage(msg, conversation.StatusComplete); !errors.Is(err, conversation.ErrDuplicateID) {
		t.Fatalf("重复 id 应被拒绝，实际 %v", err)
	}
}

// 关键不变量：AddPart 之后，增量不能再滲进「恰好位于末尾」的文本 part。
//
// 否则工具调用后面的正文会写进工具卡片里。
func TestAppendAfterAddPartStartsNewTextPart(t *testing.T) {
	conv, entry := newConv(t)
	conv.AppendToStream("m1", "正文一")
	toolPart := message.NewDataPart(map[string]any{"id": "c1"}).
		WithMediaType("application/vnd.simple-agent.tool-use+json")
	entry.AddPart(toolPart)

	conv.AppendToStream("m1", "正文二")

	// 注意第一个增量也新建了 part：AddMessage 的 streamingPart 是 -1，
	// 只有后续增量才并入同一个目标。driver 里的 agent 消息初始为空，
	// 所以真实链路是「空消息 + 增量 → 一个 part」。
	parts := entry.Message.Parts
	if len(parts) != 4 {
		t.Fatalf("应新增两个文本 part，实际 %d 个", len(parts))
	}
	if text, ok := parts[0].AsText(); !ok || text != "hello" {
		t.Fatalf("初始内容不应被改动：%#v", parts[0])
	}
	if text, ok := parts[1].AsText(); !ok || text != "正文一" {
		t.Fatalf("第一个增量应新建 part：%#v", parts[1])
	}
	if parts[2].MediaType != toolPart.MediaType {
		t.Fatalf("工具 part 位置不对：%#v", parts[2])
	}
	if text, ok := parts[3].AsText(); !ok || text != "正文二" {
		t.Fatalf("AddPart 之后的增量应写进新的文本 part：%#v", parts[3])
	}
}

// ReplaceContent 只用于重放：保留顺序与创建时间，替换内容与状态。
func TestReplaceContentKeepsPosition(t *testing.T) {
	conv := conversation.New("ctx")
	if _, err := conv.AddMessage(message.Message{MessageID: "m1", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("hi")}}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	entry, err := conv.AddMessage(message.Message{MessageID: "m2", Role: message.RoleAgent},
		conversation.StatusStreaming)
	if err != nil {
		t.Fatal(err)
	}
	created := entry.CreatedAt

	replacement := message.Message{MessageID: "m2", Role: message.RoleAgent,
		Parts: []message.Part{message.NewTextPart("最终内容")}}
	if err := conv.ReplaceContent("m2", replacement, conversation.StatusComplete, ""); err != nil {
		t.Fatal(err)
	}
	if entry.Status() != conversation.StatusComplete {
		t.Fatalf("状态应更新：%v", entry.Status())
	}
	if entry.PreviewText() != "最终内容" {
		t.Fatalf("内容应替换：%q", entry.PreviewText())
	}
	if !entry.CreatedAt.Equal(created) {
		t.Fatal("创建时间不应被替换")
	}
	if entries := conv.Entries(); len(entries) != 2 || entries[1].ID() != "m2" {
		t.Fatalf("顺序不应改变：%#v", entries)
	}
	if err := conv.ReplaceContent("nope", replacement, conversation.StatusComplete, ""); !errors.Is(err, conversation.ErrMessageNotFound) {
		t.Fatalf("未知 id 应被拒绝，实际 %v", err)
	}
}

// SetFailureReason 只允许用在已经 failed 的条目上。
func TestSetFailureReasonRequiresFailed(t *testing.T) {
	conv, _ := newConv(t)
	if err := conv.SetFailureReason("m1", "boom"); !errors.Is(err, conversation.ErrNotFailed) {
		t.Fatalf("非 failed 状态应被拒绝，实际 %v", err)
	}
	if err := conv.FailStream("m1", "boom"); err != nil {
		t.Fatal(err)
	}
	if err := conv.SetFailureReason("m1", "更详细的原因"); err != nil {
		t.Fatalf("failed 状态应允许补原因：%v", err)
	}
	entry, _ := conv.Message("m1")
	if !strings.Contains(entry.FailureReason(), "更详细") {
		t.Fatalf("原因未更新：%q", entry.FailureReason())
	}
}

// History 只含 complete；Cancelled/Failed 的半截回复不进上下文。
func TestHistoryOnlyIncludesCompleteEntries(t *testing.T) {
	conv := conversation.New("ctx")
	statuses := []conversation.EntryStatus{
		conversation.StatusComplete,
		conversation.StatusCancelled,
		conversation.StatusFailed,
		conversation.StatusStreaming,
	}
	for index, status := range statuses {
		id := "m" + string(rune('1'+index))
		if _, err := conv.AddMessage(message.Message{MessageID: id, Role: message.RoleAgent,
			Parts: []message.Part{message.NewTextPart(id)}}, status); err != nil {
			t.Fatal(err)
		}
	}
	history := conv.History()
	if len(history) != 1 || history[0].MessageID != "m1" {
		t.Fatalf("只有 complete 应进历史：%#v", history)
	}
}

// Entries 返回的是克隆：调用方改动不会影响会话内部。
func TestEntriesAreCloned(t *testing.T) {
	conv, _ := newConv(t)
	entries := conv.Entries()
	entries[0] = conversation.Entry{}
	if again := conv.Entries(); again[0].Message == nil {
		t.Fatal("Entries 应返回克隆")
	}
}
