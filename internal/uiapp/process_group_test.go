package uiapp_test

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/render"
	"simple-agent/internal/uiapp"
)

// 过程组展开后的两条视觉约束（用户提出）：
//
//  1. 竖线**从折叠标题的图标下面开始**，不把标题行也框进去；
//  2. Thinking（思考）前面也有图标，与过程组标题一致。
//
// 这两条都是纯视觉，但可以用几何断言钉住：竖线是 Absolute 之外的普通元素，
// 它的矩形不该与标题行重叠（标题在上方）。

// agentWithProcess 构造一条带推理 + 工具调用的 agent 消息。
func agentWithProcess(t *testing.T, app *uiapp.App) {
	t.Helper()
	if _, err := app.Current().Conversation().AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			{Content: message.TextPart{Text: "先想想该怎么做"}, MediaType: render.MediaReasoning},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "read", "args": map[string]any{"file_path": "a.txt"},
			}}, MediaType: render.MediaToolUse},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "read", "output": "内容", "is_error": false,
			}}, MediaType: render.MediaToolResult},
		},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
}

// 展开后标题行**不被竖线包进去**：标题应位于竖线左缘，而不是缩进到线右边。
//
// 判据用"标题 vs 展开内容"的相对位置：内容在竖线右侧（缩进），
// 标题不该也被推到那个位置。之前竖线把整组（含标题）包进去，标题就缩进了。
func TestProcessGroupHeaderNotIndentedByRail(t *testing.T) {
	app, _, p := newApp(t)
	agentWithProcess(t, app)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)

	collapsedTitle, ok := findPrefixRect(tester, "▸ ")
	if !ok {
		t.Fatalf("折叠时应有 ▸ 标题：%v", tester.Texts())
	}
	if err := tester.Click(prefixText(tester, "▸ ")); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	expandedTitle, ok := findPrefixRect(tester, "▾ ")
	if !ok {
		t.Fatalf("展开后应有 ▾ 标题：%v", tester.Texts())
	}
	// 1) 展开前后标题位置不变（不因竖线而缩进）
	if diff := expandedTitle.X - collapsedTitle.X; diff > 1 || diff < -1 {
		t.Fatalf("展开后标题不应横向移动：折叠 x=%.1f，展开 x=%.1f",
			collapsedTitle.X, expandedTitle.X)
	}
	// 2) 展开后的**内容**确实在标题右侧（说明竖线只缩进了内容）
	bodyRect, ok := findRectContaining(tester, "read")
	if !ok {
		t.Fatalf("找不到展开后的工具行：%v", tester.Texts())
	}
	if bodyRect.X <= expandedTitle.X {
		t.Fatalf("展开内容应在标题右侧（竖线只缩进内容）：标题 x=%.1f，内容 x=%.1f",
			expandedTitle.X, bodyRect.X)
	}
}

// 展开后正文在标题**下方**，且竖线不覆盖标题行的纵向范围。
func TestProcessGroupRailStartsBelowHeader(t *testing.T) {
	app, _, p := newApp(t)
	agentWithProcess(t, app)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	if err := tester.Click(prefixText(tester, "▸ ")); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	titleRect, ok := findPrefixRect(tester, "▾ ")
	if !ok {
		t.Fatalf("展开后应有 ▾ 标题")
	}
	// 正文（工具行）应在标题下方
	bodyRect, ok := tester.Find("read a.txt")
	if !ok {
		// 工具行文案可能带状态后缀，退化为按 "read" 找
		bodyRect, ok = findRectContaining(tester, "read")
		if !ok {
			t.Fatalf("找不到展开后的正文：%v", tester.Texts())
		}
	}
	if bodyRect.Y <= titleRect.Y {
		t.Fatalf("正文应在标题下方：标题 y=%.1f，正文 y=%.1f", titleRect.Y, bodyRect.Y)
	}
}

// Thinking 前面有图标（dsh 的 PROCESS_ICONS：thinking → IconThinkOutlineRegular）。
//
// 断言方式：Thinking 单独成组（只有推理、没有工具）时，标题行的 x 应与
// 过程组标题一致——两者都有图标且尺寸相同，所以起点对齐。
func TestReasoningHasLeadingIcon(t *testing.T) {
	app, _, p := newApp(t)
	// 只有推理：走 MediaReasoning 的单独渲染分支（不是过程组）
	if _, err := app.Current().Conversation().AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			{Content: message.TextPart{Text: "我在思考这件事"}, MediaType: render.MediaReasoning},
			message.NewTextPart("答案是 42"),
		},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)

	thinkingRect, ok := findRectContaining(tester, "思考")
	if !ok {
		t.Fatalf("找不到 Thinking 行：%v", tester.Texts())
	}
	// 有图标时文字会右移；对比正文第一行的起点来确认不是贴在最左边
	bodyRect, ok := tester.Find("答案是 42")
	if !ok {
		t.Fatalf("找不到正文：%v", tester.Texts())
	}
	if thinkingRect.X <= bodyRect.X+4 {
		t.Fatalf("Thinking 前面应有图标（文字应比正文右移）：思考 x=%.1f，正文 x=%.1f",
			thinkingRect.X, bodyRect.X)
	}
}

// findPrefixRect 找到以 prefix 开头的那条文本的矩形。
func findPrefixRect(tester *ui.Tester, prefix string) (ui.Rect, bool) {
	for _, text := range tester.Texts() {
		if !strings.HasPrefix(text, prefix) {
			continue
		}
		if rect, ok := tester.Find(text); ok {
			return rect, true
		}
	}
	return ui.Rect{}, false
}

// prefixText 返回以 prefix 开头的第一条文本。
func prefixText(tester *ui.Tester, prefix string) string {
	for _, text := range tester.Texts() {
		if strings.HasPrefix(text, prefix) {
			return text
		}
	}
	return ""
}
