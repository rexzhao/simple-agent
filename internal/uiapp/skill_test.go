package uiapp_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/locale"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/uiapp"
)

// newAppIn 造一个工作区在指定目录的 App（skill 要在工作区里找）。
func newAppIn(t *testing.T, workspace string) (*uiapp.App, *fakeRuntime, *poster) {
	t.Helper()
	rt := &fakeRuntime{}
	p := &poster{}
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:             "你是助手",
		Workspace:                workspace,
		Title:                    "测试会话",
		MaxToolRounds:            3,
		SidebarAutoCollapseWidth: -1,
		Language:                 locale.ZH,
		Post:                     p.Post,
		ContextWindow:            testContextWindow,
	})
	return app, rt, p
}

// 造一个带 skill 的工作区。
func withSkill(t *testing.T, workspace, name, body string) {
	t.Helper()
	dir := filepath.Join(workspace, ".agents", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: 测试技能\n---\n\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// **纯被动**：会话启动时不加载任何 skill。
//
// 用户明确的定调："会话启动时候不需要自动加载 skill 概述，也就是说纯被动"。
// 这条测试锁住它：即使项目里有 skill，启动后的会话历史与系统提示里
// 都不该出现 skill 的任何内容。
func TestSkillsNotLoadedOnStartup(t *testing.T) {
	workspace := t.TempDir()
	withSkill(t, workspace, "pdf-tools", "这是 skill 的正文标记 UNIQUE_SKILL_BODY")

	app, _, p := newAppIn(t, workspace)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)

	// 历史里没有任何消息
	conv := app.Current().Conversation()
	if entries := conv.Entries(); len(entries) != 0 {
		t.Fatalf("启动后不该有任何消息：%d 条", len(entries))
	}
	// 界面文本里也不该出现 skill 内容
	for _, text := range tester.Texts() {
		if strings.Contains(text, "UNIQUE_SKILL_BODY") || strings.Contains(text, "pdf-tools") {
			t.Fatalf("启动时不该出现 skill 内容：%q", text)
		}
	}
}

// `/skill <名字>` 直接把全文注入当前会话一轮。
func TestSkillCommandInjectsBody(t *testing.T) {
	workspace := t.TempDir()
	withSkill(t, workspace, "pdf-tools", "读 references/formats.md 再转换。")

	app, _, p := newAppIn(t, workspace)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)

	app.RunCommandForTest("skill", []string{"pdf-tools"})
	pump(tester, p)

	// 应有一条用户消息，内容是 skill 全文
	conv := app.Current().Conversation()
	entries := conv.Entries()
	if len(entries) == 0 {
		t.Fatal("注入后应有一条消息")
	}
	text := lastUserMessageText(t, entries)
	if !strings.Contains(text, "读 references/formats.md 再转换。") {
		t.Fatalf("应包含 skill 正文：%q", text)
	}
	// 要带目录（相对路径的基准）
	if !strings.Contains(text, filepath.Join(".agents", "skills", "pdf-tools")) {
		t.Fatalf("应带上 skill 目录：%q", text)
	}
}

// 没有同名 skill 时给友好提示，且**不发送**任何东西。
func TestSkillCommandUnknownName(t *testing.T) {
	workspace := t.TempDir()
	withSkill(t, workspace, "pdf-tools", "正文")

	app, _, p := newAppIn(t, workspace)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)

	msg := app.RunCommandForTest("skill", []string{"不存在"})
	if !strings.Contains(msg, "没有名为") {
		t.Fatalf("应提示找不到：%q", msg)
	}
	if entries := app.Current().Conversation().Entries(); len(entries) != 0 {
		t.Fatalf("找不到时不该发送任何消息：%d 条", len(entries))
	}
}

// 不带参数打开选择器，界面上列出 skill。
func TestSkillPickerListsSkills(t *testing.T) {
	workspace := t.TempDir()
	withSkill(t, workspace, "pdf-tools", "正文")

	app, _, p := newAppIn(t, workspace)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)

	app.RunCommandForTest("skill", nil)
	pump(tester, p)

	all := strings.Join(tester.Texts(), "\n")
	if !strings.Contains(all, "pdf-tools") {
		t.Fatalf("选择器应列出 skill：%v", tester.Texts())
	}
	if !strings.Contains(all, "测试技能") {
		t.Fatalf("选择器应显示描述：%v", tester.Texts())
	}
}

// lastUserMessageText 取最后一条用户消息的文本。
func lastUserMessageText(t *testing.T, entries []conversation.Entry) string {
	t.Helper()
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.Message == nil || entry.Message.Message == nil {
			continue
		}
		if entry.Message.Message.Role != message.RoleUser {
			continue
		}
		var texts []string
		for _, part := range entry.Message.Message.Parts {
			if text, ok := part.Content.(message.TextPart); ok {
				texts = append(texts, text.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}
