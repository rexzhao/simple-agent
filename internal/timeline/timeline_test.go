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
	items := timeline.Build(conv.Entries(), true, false)

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
	items := timeline.Build(conv.Entries(), false, false)
	last := items[len(items)-1]
	if last.Kind == timeline.KindRoundFooter {
		t.Fatal("会话还没空闲，不该出现回合结束行")
	}
}

// 过程组标题：进行中显示当前活动，已完成按次数拼句子。
func TestGroupTitle(t *testing.T) {
	conv := buildConversation(t)
	items := timeline.Build(conv.Entries(), true, false)
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
	items := timeline.Build(conv.Entries(), true, false)
	footer := items[len(items)-1]
	if footer.Kind != timeline.KindRoundFooter {
		t.Fatalf("最后一条应是回合结束行，实际 %s", footer.Kind)
	}
	if got := timeline.Duration(footer); got < 2*time.Second {
		t.Fatalf("耗时应接近 3 秒，实际 %v", got)
	}
	if got := timeline.Tokens(footer); got != 0 {
		t.Fatalf("没有 token 记录时应为 0，实际 %d", got)
	}
}

// 只有「最后一组、回合未结束、消息仍在流式」的过程组才算进行中。
//
// 回归测试：以前 mobile/bridge 用 `streaming := !idle`（整个会话是否空闲）
// 来决定每个过程组是否显示转圈，结果会话一跑起来，几百轮之前的历史工具组
// 全都跟着转圈。
func TestOnlyLastProcessGroupIsLive(t *testing.T) {
	conv := conversation.New("ctx")

	// 第一轮：用户提问 + 两段过程（中间被正文隔开）—— 已成历史，不该 live
	if _, err := conv.AddMessage(message.Message{
		MessageID: "u1", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("第一次")},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			message.NewTextPart("想一想").WithMediaType(render.MediaReasoning),
			message.NewDataPart(map[string]any{"id": "c1", "name": "read"}).
				WithMediaType(render.MediaToolUse),
			message.NewDataPart(map[string]any{"id": "c1", "name": "read", "output": "ok"}).
				WithMediaType(render.MediaToolResult),
			message.NewTextPart("中间回答"),
			message.NewDataPart(map[string]any{"id": "c2", "name": "grep"}).
				WithMediaType(render.MediaToolUse),
			message.NewDataPart(map[string]any{"id": "c2", "name": "grep", "output": "hit"}).
				WithMediaType(render.MediaToolResult),
		},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	// 第二轮：用户又提问（第一轮因此 Closed），然后正在流式输出
	if _, err := conv.AddMessage(message.Message{
		MessageID: "u2", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("第二次")},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a2", Role: message.RoleAgent,
		Parts: []message.Part{
			message.NewTextPart("再想想").WithMediaType(render.MediaReasoning),
			message.NewDataPart(map[string]any{"id": "c3", "name": "shell"}).
				WithMediaType(render.MediaToolUse),
		},
	}, conversation.StatusStreaming); err != nil {
		t.Fatal(err)
	}

	// idle=false：会话正在跑（这正是触发 bug 的条件）
	items := timeline.Build(conv.Entries(), false, false)

	var liveKeys []string
	var processCount int
	for _, item := range items {
		if item.Kind != timeline.KindProcess {
			continue
		}
		processCount++
		if item.Live {
			liveKeys = append(liveKeys, item.Key)
		}
	}
	if processCount != 3 {
		t.Fatalf("应有 3 个过程组（历史 2 + 当前 1），实际 %d", processCount)
	}
	if len(liveKeys) != 1 {
		t.Fatalf("只有最后一个过程组该转圈，实际 %d 个在转：%v", len(liveKeys), liveKeys)
	}

	// 活着的那一组必须是最后那个（含 c3 工具调用的第二轮）。
	//
	// 注意：流式期间**也会有一条回合行**（Live，显示运行时间/用量），
	// 所以不能假设"最后一条是过程组"——要找最后一条**过程组**。
	var lastProcess *timeline.Item
	for index := range items {
		if items[index].Kind == timeline.KindProcess {
			lastProcess = &items[index]
		}
	}
	if lastProcess == nil {
		t.Fatal("应有过程组")
	}
	if !lastProcess.Live {
		t.Fatal("最后一个过程组应标记为进行中")
	}
	if len(lastProcess.Refs) == 0 || lastProcess.Refs[len(lastProcess.Refs)-1].Chat.Message.MessageID != "a2" {
		t.Fatal("活着的应该是第二轮的过程组")
	}

	// 会话空闲后，一个都不该转圈
	for _, item := range timeline.Build(conv.Entries(), true, false) {
		if item.Kind == timeline.KindProcess && item.Live {
			t.Fatalf("会话空闲后不该有过程组仍在转圈：%s", item.Key)
		}
	}
}
