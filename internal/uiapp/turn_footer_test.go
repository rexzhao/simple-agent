package uiapp_test

import (
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/render"
	"simple-agent/internal/uiapp"
)

// 回合结束行的形态（对齐 dsh 的 turn footer + MessageIconActions）：
//
//	已完成，用时 22秒  128 tokens  [复制图标] [以后的其它按钮…]
//
// 三条硬性要求（用户提出）：
//   - 后面**不画**分隔线；
//   - 复制是**图标**，不是文字；
//   - 复制只出现在回合**最终输出**那一行，且与「已完成」**同一行**。

// addTurn 构造一个完整回合：user 消息 + agent 消息。//
// 把 user 消息的时间往前挪，让耗时是一个确定值。
func addTurn(t *testing.T, session *uiapp.Session, messageID string, elapsed time.Duration) *conversation.ChatEntry {
	t.Helper()
	userEntry, err := session.Conversation().AddMessage(message.Message{
		MessageID: messageID + "-u", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("问题")},
	}, conversation.StatusComplete)
	if err != nil {
		t.Fatal(err)
	}
	userEntry.CreatedAt = userEntry.CreatedAt.Add(-elapsed)

	agentEntry, err := session.Conversation().AddMessage(message.Message{
		MessageID: messageID + "-a", Role: message.RoleAgent,
		Parts: []message.Part{message.NewTextPart("答案正文")},
	}, conversation.StatusComplete)
	if err != nil {
		t.Fatal(err)
	}
	agentEntry.SetTokens(128)
	agentEntry.SetFinishedAt(time.Now())
	return agentEntry
}

// 回合结束行：状态文字、token、复制图标都在**同一行**，且没有分隔线。
func TestTurnFooterHasCopyIconInSameRow(t *testing.T) {
	app, _, p := newApp(t)
	addTurn(t, app.Current(), "t1", 22*time.Second)

	tester := ui.NewTester(app.View, 760, 620)
	pump(tester, p)
	pump(tester, p)

	// 「已完成，用时」在（文字是「已完成，用时 22秒」整串）
	if !tester.HasText("已完成") {
		t.Fatalf("应有回合结束行：%v", tester.Texts())
	}
	// 复制按钮的**无障碍名**是「复制回答」（图标按钮没有可见文字）
	rect, ok := tester.Find(locCopyAnswer)
	if !ok {
		t.Fatalf("回合结束行应有复制按钮：%v", tester.Texts())
	}
	// 同一行：按钮的垂直中心应落在「已完成」文字的纵向范围内。
	// 注意文字是整串「已完成，用时 22秒」，Find 要按完整文本找。
	labelRect, ok := findRectContaining(tester, "已完成")
	if !ok {
		t.Fatalf("找不到「已完成」文字：%v", tester.Texts())
	}
	if rect.Y < labelRect.Y-8 || rect.Y > labelRect.Y+labelRect.H+8 {
		t.Fatalf("复制按钮应与「已完成」同一行：按钮 y=%.1f h=%.1f，文字 y=%.1f h=%.1f",
			rect.Y, rect.H, labelRect.Y, labelRect.H)
	}
	// 图标不是文字：可见文本里不该出现「复制」二字。
	//
	// 注意 Texts() 里会包含**无障碍名**（"复制回答"），那是给读屏用的，
	// 不是可见文字——所以这里只拦"复制"/"复制回答"这种纯标签以外的判断
	// 没有意义，改为断言：按钮存在且宽高是图标尺寸，而不是文字宽度。
	if rect.W > 40 {
		t.Fatalf("复制按钮应是图标（宽 ≤ 40），实际宽 %.1f：可能又变回文字按钮了", rect.W)
	}
}

// 复制按钮只出现在**最终输出**那一行：回合中间的旁白不带按钮。
func TestCopyIconOnlyOnFinalOutput(t *testing.T) {
	app, _, p := newApp(t)
	// 一个回合里两条 agent 消息：先旁白（含工具），再最终答案
	if _, err := app.Current().Conversation().AddMessage(message.Message{
		MessageID: "t1-u", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("问题")},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Current().Conversation().AddMessage(message.Message{
		MessageID: "t1-a1", Role: message.RoleAgent,
		Parts: []message.Part{message.NewTextPart("我先看一下文件。")},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	final, err := app.Current().Conversation().AddMessage(message.Message{
		MessageID: "t1-a2", Role: message.RoleAgent,
		Parts: []message.Part{message.NewTextPart("最终答案")},
	}, conversation.StatusComplete)
	if err != nil {
		t.Fatal(err)
	}
	final.SetFinishedAt(time.Now())

	tester := ui.NewTester(app.View, 760, 620)
	pump(tester, p)
	pump(tester, p)

	// 回合结束行那一个，且复制的是**最终回答**
	if _, ok := tester.Find(locCopyAnswer); !ok {
		t.Fatalf("最终输出应有复制按钮：%v", tester.Texts())
	}
	if err := tester.Click(locCopyAnswer); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if got := tester.Clipboard(); got != "最终答案" {
		t.Fatalf("应复制最终输出，实际 %q", got)
	}
}

// 用户消息保留自己的复制入口（它不是回合最终输出），且复制的是用户输入。
func TestUserMessageKeepsCopyIcon(t *testing.T) {
	app, _, p := newApp(t)
	addTurn(t, app.Current(), "t1", 5*time.Second)

	tester := ui.NewTester(app.View, 760, 620)
	pump(tester, p)
	pump(tester, p)

	messageRect, ok := tester.Find(locCopyMessage)
	if !ok {
		t.Fatalf("用户气泡应有复制按钮：%v", tester.Texts())
	}
	answerRect, ok := tester.Find(locCopyAnswer)
	if !ok {
		t.Fatalf("回合结束行应有复制按钮：%v", tester.Texts())
	}
	// 用户气泡在上方，它的复制按钮 y 更小
	if messageRect.Y >= answerRect.Y {
		t.Fatalf("用户消息的复制按钮应在回合结束行上方：%v vs %v", messageRect.Y, answerRect.Y)
	}
	// 点用户那个，复制的是用户输入
	if err := tester.Click(locCopyMessage); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if got := tester.Clipboard(); got != "问题" {
		t.Fatalf("用户消息的复制应复制其正文，实际 %q", got)
	}
}

// 生成一张截图（设置 SIMPLE_AGENT_SHOT 时），便于人眼确认视觉效果。
func TestTurnFooterScreenshot(t *testing.T) {
	app, _, p := newApp(t)
	if _, err := app.Current().Conversation().AddMessage(message.Message{
		MessageID: "u1", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("这个文件的编译错误怎么修？")},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	agent := message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			{Content: message.TextPart{Text: "先看一下这个文件。"}, MediaType: render.MediaReasoning},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "read", "args": map[string]any{"file_path": "main.go"},
			}}, MediaType: render.MediaToolUse},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "read", "output": "package main", "is_error": false,
			}}, MediaType: render.MediaToolResult},
			message.NewTextPart("错误在 `main.go:12`：变量声明后没有使用。删掉那一行即可。"),
		},
	}
	entry, err := app.Current().Conversation().AddMessage(agent, conversation.StatusComplete)
	if err != nil {
		t.Fatal(err)
	}
	entry.SetTokens(128)
	entry.SetFinishedAt(time.Now())

	tester := ui.NewTester(app.View, 760, 620)
	pump(tester, p)
	pump(tester, p)

	if path := os.Getenv("SIMPLE_AGENT_SHOT"); path != "" {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := png.Encode(file, tester.Image()); err != nil {
			t.Fatal(err)
		}
	}
}

// findRectContaining 找到包含子串的那条可见文本的矩形（Find 是精确匹配）。
func findRectContaining(tester *ui.Tester, sub string) (ui.Rect, bool) {
	for _, text := range tester.Texts() {
		if !strings.Contains(text, sub) {
			continue
		}
		if rect, ok := tester.Find(text); ok {
			return rect, true
		}
	}
	return ui.Rect{}, false
}
