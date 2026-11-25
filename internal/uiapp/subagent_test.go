package uiapp

import (
	"strings"
	"testing"

	"simple-agent/internal/host"
)

// newSubagentCommandApp 造一个带一条会话、且配了模型列表的最小 App。
//
// 模型列表要**两处都设**，这是生产环境的真实形态：
//   - 执行端 Config.Models ← executor.go 的 providerChoices(registry)（校验用）
//   - 界面 opts.Providers   ← 同一个注册表（显示可选值用）
//
// 两处来自同一个注册表，所以生产上不会不一致；测试里各自注入，就得手动对齐，
// 否则会测出"界面能列、后端不认"的假象。
func newSubagentCommandApp(t *testing.T) *App {
	t.Helper()
	models := []host.ProviderChoice{
		{ProviderID: "fake", Models: []string{"cheap", "expensive", "m"}},
	}
	app := newLocalApp(t)
	local := app.host.(*host.Local)
	local.SetModels(models)
	app.opts.Providers = models
	app.syncSessions()
	app.switchTo(0)
	if app.Current() == nil {
		t.Fatal("应有一条可用的会话")
	}
	return app
}

// /subagent 命令必须存在且出现在帮助里（否则用户根本不知道有它）。
func TestSubagentCommandRegistered(t *testing.T) {
	if _, ok := lookupCommand("subagent"); !ok {
		t.Fatal("应有 /subagent 命令")
	}
	if !strings.Contains(buildHelpText(), "/subagent") {
		t.Fatalf("帮助里应列出 /subagent：\n%s", buildHelpText())
	}
}

// 不带参数时说明当前设置。没设置过就是"跟随本会话"。
func TestSubagentCommandShowsFollowingByDefault(t *testing.T) {
	app := newSubagentCommandApp(t)

	message, ok := app.ExecSlashCommand("/subagent")
	if !ok {
		t.Fatal("/subagent 应被识别为命令")
	}
	if !strings.Contains(message, "跟随本会话") {
		t.Fatalf("默认应说明跟随本会话，实际：%q", message)
	}
}

// 不带 model 参数时列出可选模型 —— 用户照着敲，不需要另做一个选择器。
func TestSubagentCommandListsModels(t *testing.T) {
	app := newSubagentCommandApp(t)

	message, _ := app.ExecSlashCommand("/subagent model")
	for _, want := range []string{"cheap", "expensive"} {
		if !strings.Contains(message, want) {
			t.Fatalf("应列出模型 %s，实际：%q", want, message)
		}
	}
}

// 设置成功后，同一会话再查应显示已设置的模型。
func TestSubagentCommandSetsModel(t *testing.T) {
	app := newSubagentCommandApp(t)

	if _, ok := app.ExecSlashCommand("/subagent model cheap"); !ok {
		t.Fatal("设置命令应被识别")
	}
	message, _ := app.ExecSlashCommand("/subagent")
	if !strings.Contains(message, "cheap") {
		t.Fatalf("应显示已设置的模型，实际：%q", message)
	}
}

// off 恢复为跟随本会话。
func TestSubagentCommandOffRestoresInheritance(t *testing.T) {
	app := newSubagentCommandApp(t)

	if _, ok := app.ExecSlashCommand("/subagent model cheap"); !ok {
		t.Fatal("设置命令应被识别")
	}
	if _, ok := app.ExecSlashCommand("/subagent model off"); !ok {
		t.Fatal("off 应被识别")
	}
	message, _ := app.ExecSlashCommand("/subagent")
	if !strings.Contains(message, "跟随本会话") {
		t.Fatalf("off 之后应回到跟随本会话，实际：%q", message)
	}
}

// 子命令写错时给出用法，而不是静默什么都不做。
func TestSubagentCommandUnknownSubcommand(t *testing.T) {
	app := newSubagentCommandApp(t)

	message, ok := app.ExecSlashCommand("/subagent 乱写")
	if !ok {
		t.Fatal("应被识别为命令")
	}
	if !strings.Contains(message, "用法") {
		t.Fatalf("应给出用法，实际：%q", message)
	}
}