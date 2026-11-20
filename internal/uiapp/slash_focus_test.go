package uiapp_test

import (
	"strings"
	"testing"
)

// 输入 "/" 弹提示面板时，输入框**不能失焦**。
//
// 用户报的："对话框输入 / 弹出的提示面板会让输入框失去焦点，体验很差"。
// 根因：候选面板是插在输入卡**前面**的兄弟元素，它出现/消失改变了元素树，
// 焦点随之丢失（同附件条的老问题）。现在按"输入框前面的兄弟数量"比对上帧，
// 一变就下一帧重新聚焦。
func TestSlashHintsKeepComposerFocus(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	pump(tester, p)

	clickComposer(t, tester)
	// 打 "/" → 候选面板出现
	tester.Type("/")
	tester.Frame()
	pump(tester, p)

	// 关键：焦点还在输入框，能继续打字
	tester.Type("co")
	tester.Frame()
	pump(tester, p)

	if got := app.DraftForTest(); got != "/co" {
		t.Fatalf("面板出现后应能继续输入（草稿应为 /co），实际 %q", got)
	}

	// 打全命令名后面板会消失（只剩一个匹配且名字已打全）：
	// 面板消失同样会改变元素树，焦点不能丢
	tester.Type("mpact")
	tester.Frame()
	pump(tester, p)
	if got := app.DraftForTest(); got != "/compact" {
		t.Fatalf("面板消失后应能继续输入，实际 %q", got)
	}
}

// 面板从有到无（输入空格进入参数）也不能丢焦点。
func TestSlashHintsFocusSurvivesPanelClosing(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	pump(tester, p)

	clickComposer(t, tester)
	tester.Type("/con")
	tester.Frame()
	pump(tester, p)

	// 补一个空格：进入参数区，候选面板收起
	tester.Type(" ")
	tester.Frame()
	pump(tester, p)
	// 继续打参数
	tester.Type("x")
	tester.Frame()
	pump(tester, p)

	if got := app.DraftForTest(); got != "/con x" {
		t.Fatalf("面板收起后应能继续输入，实际 %q", got)
	}
}

// /compact 命令要存在，且在没有可压内容时给出**明确原因**（不是静默失败）。
func TestCompactCommandExists(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	pump(tester, p)

	text, ok := app.ExecSlashCommand("/compact")
	if !ok {
		t.Fatal("/compact 应该是可识别的命令")
	}
	if text == "" {
		t.Fatal("应给出提示（成功或失败都要说一句，静默没有任何反馈）")
	}
	// 刚建的会话没有历史：应该明确说"没有可压缩的内容"
	if !strings.Contains(text, "没有") && !strings.Contains(text, "压缩") {
		t.Fatalf("提示应说明情况，实际 %q", text)
	}
}

// /help 里要能看到 compact（用户得知道有这个命令）。
func TestHelpListsCompact(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	pump(tester, p)

	text, ok := app.ExecSlashCommand("/help")
	if !ok {
		t.Fatal("/help 应该可用")
	}
	if !strings.Contains(text, "compact") {
		t.Fatalf("/help 应列出 compact，实际 %q", text)
	}
}