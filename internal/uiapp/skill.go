package uiapp

import (
	"strings"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/skills"
)

// /skill 命令：选一个 skill，把它的完整说明注入当前会话的一轮。
//
// **纯被动**（用户明确的要求）：会话启动时不加载任何 skill，系统提示里
// 不放 skill 列表，请求组装也不碰它。只有用户在这里显式选了，才把
// SKILL.md 全文作为一条消息发出去 —— 用不到的时候一个 token 都不花。
//
// 为什么不做成"启动时把 skill 概述放进系统提示"：那样每个会话、每一轮
// 都要为一个可能用不上的能力付 token，还会让系统提示随项目内容变化
// （破坏缓存前缀）。pin 式的显式选择反而更适合这个工具。

// skillPickerOpen 记录选择器是否打开。
type skillPickerState struct {
	open  bool
	index int
	// list 是打开时扫到的 skill（打开期间不重扫，避免每帧读盘）。
	list []skills.Skill
	// err 是扫描/读取失败时的提示。
	err string
}

// runSkill 是 `/skill` 命令的入口。
//
// 带参数时直接注入同名 skill（`/skill pdf-tools`），
// 不带参数时打开选择器。
func runSkill(a *App, args []string) string {
	if len(args) > 0 {
		return a.injectSkillByName(strings.Join(args, " "))
	}
	a.OpenSkillPicker()
	return ""
}

// OpenSkillPicker 打开 skill 选择器。
func (a *App) OpenSkillPicker() {
	session := a.Current()
	if session == nil {
		return
	}
	sources := skills.DefaultSources(session.Workspace)
	a.skillPicker.list = skills.Discover(sources)
	a.skillPicker.index = 0
	a.skillPicker.err = ""
	a.skillPicker.open = true
}

// CloseSkillPicker 关闭选择器。
func (a *App) CloseSkillPicker() {
	a.skillPicker.open = false
}

// injectSkillByName 按名字直接注入（`/skill <name>` 的快捷路径）。
func (a *App) injectSkillByName(name string) string {
	session := a.Current()
	if session == nil {
		return a.tr("skill.noSession")
	}
	list := skills.Discover(skills.DefaultSources(session.Workspace))
	skill, ok := skills.Find(list, strings.TrimSpace(name))
	if !ok {
		if len(list) == 0 {
			return a.tr("skill.none")
		}
		names := make([]string, 0, len(list))
		for _, item := range list {
			names = append(names, item.Name)
		}
		return a.tr("skill.unknown", name, strings.Join(names, "、"))
	}
	return a.injectSkill(skill)
}

// injectSkill 把 skill 全文作为一条用户消息发给当前会话。
//
// 走 Send（和用户自己打字发消息同一条路径）：这一轮会正常跑起来，
// 模型拿到完整说明后按它做事。**不写系统提示、不改工具列表** ——
// 注入只影响当前这一轮之后的对话，下一条新消息不会重复带上它（除非用户
// 再选一次），这也符合"纯被动"的语义。
func (a *App) injectSkill(skill skills.Skill) string {
	session := a.Current()
	if session == nil {
		return a.tr("skill.noSession")
	}
	prompt, err := skill.Prompt()
	if err != nil {
		return err.Error()
	}
	// 通过统一的发送路径提交（和用户自己打字发消息完全一样）：
	// 这一轮会正常跑起来，模型拿到完整说明后按它做事。
	//
	// 不写系统提示、不改工具列表 —— 注入只影响这一轮之后的对话，
	// 下一条新消息不会重复带上它（除非用户再选一次），符合"纯被动"。
	//
	// 注意不动用户的草稿：skill 内容是程序生成的，塞进草稿框会把用户
	// 正在写的东西冲掉。
	if cmdID := a.submitSend(session, prompt, nil); cmdID == "" {
		return a.tr("skill.sendFailed")
	}
	a.skillPicker.open = false
	return a.tr("skill.injected", skill.Name)
}

// RunCommandForTest 执行一条斜杠命令（测试用）。
//
// 直接走 lookupCommand + Run，与输入框提交命令的路径一致。
func (a *App) RunCommandForTest(name string, args []string) string {
	command, ok := lookupCommand(name)
	if !ok {
		return "没有这条命令：" + name
	}
	return command.Run(a, args)
}

// SkillPickerOpenForTest 报告 skill 选择器是否打开（测试用）。
func (a *App) SkillPickerOpenForTest() bool { return a.skillPicker.open }

// skillListMaxHeight 是选择器列表的最大高度（再多就滚动）。
//
// 必须限高：skill 可能很多（这台机器上光用户级就有 8 个），不限高会把
// 输入框挤出屏幕，用户连「取消」都点不到。
const skillListMaxHeight = 420

// skillPickerPane 渲染 skill 选择器。
func (a *App) skillPickerPane(c *ui.Context) {
	theme := c.Theme()
	list := a.skillPicker.list
	ui.Column(c).Grow(1).FillHeight().MinWidth(centerMinWidth).Center().Gap(10).
		Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, a.tr("skill.pick.title")).FontSize(15).TextColor(theme.Text)
				ui.Text(c, a.tr("skill.pick.scope")).FontSize(11).TextColor(theme.TextMuted)
			})
			if a.skillPicker.err != "" {
				ui.Text(c, a.skillPicker.err).FontSize(12).
					TextColor(theme.Danger).MaxLines(3)
			}
			if len(list) == 0 {
				ui.Text(c, a.tr("skill.pick.empty")).
					FontSize(12).TextColor(theme.TextMuted).MaxLines(4)
			}
			ui.Scroll(c).FillWidth().MaxHeight(skillListMaxHeight).Children(func() {
				ui.Column(c).Gap(6).MinWidth(360).Children(func() {
					for index, skill := range list {
						a.skillPickerRow(c, index, skill, theme)
					}
				})
			})
			ui.Text(c, a.tr("skill.pick.hint")).FontSize(11).TextColor(theme.TextMuted)
			if textButton(c, a.tr("common.cancel"), 12, theme.TextMuted) {
				a.CloseSkillPicker()
			}
		})
}

// skillPickerRow 渲染一行 skill。
func (a *App) skillPickerRow(c *ui.Context, index int, skill skills.Skill, theme *ui.Theme) {
	row := ui.Row(c).Padding(8, 12).Gap(10).Radius(8).
		AlignItems(ui.Center).Cursor(ui.CursorPointer)
	if index == a.skillPicker.index {
		row.Background(theme.Selection)
	}
	row.Children(func() {
		ui.Column(c).Grow(1).Gap(2).Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, skill.Name).FontSize(13).TextColor(theme.Text)
				// 来源标注：同名时项目版覆盖用户版，显示出来用户才知道
				// 用的是哪一份。
				if skill.Scope != "" {
					ui.Text(c, skill.Scope).FontSize(10).TextColor(theme.TextMuted)
				}
			})
			if skill.Description != "" {
				ui.Text(c, skill.Description).FontSize(11).
					TextColor(theme.TextMuted).MaxLines(2)
			}
		})
	})
	if row.Clicked() {
		a.skillPicker.index = index
		a.injectSkill(skill)
	}
}
