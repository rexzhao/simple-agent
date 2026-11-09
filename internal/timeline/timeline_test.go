package timeline_test

import (
	"strings"
	"testing"
	"time"

	"simple-agent/internal/conversation"
	"simple-agent/internal/locale"
	"simple-agent/internal/message"
	"simple-agent/internal/render"
	"simple-agent/internal/timeline"
)

// buildConversation 造一份「用户 → 过程（思考 + 工具）→ 正文」的会话。
func buildConversation(t *testing.T) *conversation.Conversation {
	t.Helper()
	conv := conversation.New("ctx")
	user := message.Message{
		MessageID: "u1", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("你好")},
	}
	if _, err := conv.AddMessage(user, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	agent := message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			message.NewTextPart("想一想").WithMediaType(render.MediaReasoning),
			message.NewDataPart(map[string]any{"id": "c1", "name": "read"}).
				WithMediaType(render.MediaToolUse),
			message.NewDataPart(map[string]any{"id": "c1", "name": "read", "output": "ok"}).
				WithMediaType(render.MediaToolResult),
			message.NewTextPart("回答"),
		},
	}
	entry, err := conv.AddMessage(agent, conversation.StatusComplete)
	if err != nil {
		t.Fatal(err)
	}
	// 回合耗时 = 最后一条消息的 finishedAt − 回合前那条 user 消息的 CreatedAt，
	// 所以两条时间都要设。
	now := time.Now()
	entry.SetFinishedAt(now)
	for _, item := range conv.Entries() {
		if item.Message != nil && item.Message.Message.MessageID == "u1" {
			item.Message.CreatedAt = now.Add(-3 * time.Second)
		}
	}
	return conv
}

// 分组规则：过程进组、正文在组外、回合结束时补一行 footer。
func TestBuildSplitsProcessAndContent(t *testing.T) {
	conv := buildConversation(t)
	items := timeline.Build(conv.Entries(), true)

	var kinds []string
	for _, item := range items {
		kinds = append(kinds, string(item.Kind))
	}
	want := []string{"content", "process", "content", "footer"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("条目类型不对：%v（期望 %v）", kinds, want)
	}
	// 过程组应包含思考 + 工具调用 + 工具结果三个 part
	if got := len(items[1].Refs); got != 3 {
		t.Fatalf("过程组应有 3 个 part，实际 %d", got)
	}
}

// 会话还在忙时，最后一个回合不算结束（不显示「已完成」）。
func TestBuildNoFooterWhileBusy(t *testing.T) {
	conv := buildConversation(t)
	items := timeline.Build(conv.Entries(), false)
	last := items[len(items)-1]
	if last.Kind == timeline.KindTurnFooter {
		t.Fatal("会话还没空闲，不该出现回合结束行")
	}
}

// 过程组标题：进行中显示当前活动，已完成按次数拼句子。
func TestGroupTitle(t *testing.T) {
	conv := buildConversation(t)
	items := timeline.Build(conv.Entries(), true)
	var group timeline.Item
	for _, item := range items {
		if item.Kind == timeline.KindProcess {
			group = item
		}
	}
	done := timeline.GroupTitle(group.Refs, false, locale.ZH)
	// 中文文案是「已完成分析并读取文件」：思考 → "分析"，读取 → "读取文件"
	if !strings.Contains(done, "分析") || !strings.Contains(done, "读取") {
		t.Fatalf("已完成的标题应包含分析（思考）与读取，实际：%q", done)
	}
	live := timeline.GroupTitle(group.Refs, true, locale.ZH)
	if !strings.Contains(live, "正在") {
		t.Fatalf("进行中的标题应是「正在…」，实际：%q", live)
	}
}

// 空文本 part 不占位置（流式时常见）。
func TestEmptyTextPartSkipped(t *testing.T) {
	if !timeline.IsEmptyTextPart(message.NewTextPart("   \n\t")) {
		t.Fatal("空白文本应被跳过")
	}
	if timeline.IsEmptyTextPart(message.NewTextPart("x")) {
		t.Fatal("非空文本不该被跳过")
	}
	reasoning := message.NewTextPart("  ").WithMediaType(render.MediaReasoning)
	if timeline.IsEmptyTextPart(reasoning) {
		t.Fatal("推理 part 即使空白也要保留（它有预览作用）")
	}
}

// 工具参数摘要要能从「半截 JSON」里抠出来（流式时参数还没收完）。
func TestToolArgSummaryFromPartialJSON(t *testing.T) {
	data := map[string]any{"raw_args": `{"path": "docs/pl`}
	if got := timeline.ToolArgSummary(data); got != "docs/pl" {
		t.Fatalf("半截参数没解析出来：%q", got)
	}
	full := map[string]any{"args": map[string]any{"command": "go test ./..."}}
	if got := timeline.ToolArgSummary(full); got != "go test ./..." {
		t.Fatalf("完整参数没解析出来：%q", got)
	}
}

// 回合耗时与 token 汇总。
func TestDurationAndTokens(t *testing.T) {
	conv := buildConversation(t)
	items := timeline.Build(conv.Entries(), true)
	footer := items[len(items)-1]
	if footer.Kind != timeline.KindTurnFooter {
		t.Fatalf("最后一条应是回合结束行，实际 %s", footer.Kind)
	}
	if got := timeline.Duration(footer); got < 2*time.Second {
		t.Fatalf("耗时应接近 3 秒，实际 %v", got)
	}
	if got := timeline.Tokens(footer); got != 0 {
		t.Fatalf("没有 token 记录时应为 0，实际 %d", got)
	}
}
