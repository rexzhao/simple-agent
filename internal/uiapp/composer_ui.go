package uiapp

import (
	"github.com/egoist/mygo/ui"

	"simple-agent/internal/driver"
	"simple-agent/internal/providers"
)

// 输入卡右下角的模型 chip、左下的权限展示、`+` 附件与圆形发送按钮。
//
// 对照 dsh：
//   - 模型 chip / 权限下拉在 `ui-chat/src/client/chat/` 的 composer 组件里；
//   - 权限文案来自 `ui-permission-presets/src/client/locales.ts`（`preset.fullAccess` /
//     `preset.workspaceWrite`、`title`）；
//   - 圆形发送按钮用 `ui-primitives` 的 `IconSendOutlineRegular`。

// modelChip 渲染「模型 档位 ▾」并弹出原生菜单：提供商 → 模型，再一个「思考」子菜单。
//
// 忙时整份菜单禁用并给一条说明（用户点了没反应是最糟的），与 dsh 一致。
func (a *App) modelChip(c *ui.Context, session *Session, theme *ui.Theme) {
	idle := session.Busy() == driver.BusyIdle
	label := a.modelLabel(session.ProviderID(), session.Model())
	if effort := a.effortLabel(session.Effort()); effort != "" {
		label += " " + effort
	}
	chip := ui.Row(c).Gap(4).Padding(2, 6).Radius(6).Cursor(ui.CursorPointer).
		Label(a.tr("composer.model"))
	if chip.Hovered() {
		chip.Background(dsh.SurfaceHover)
	}
	chip.Children(func() {
		ui.Text(c, label).FontSize(11).TextColor(theme.TextMuted)
		ui.Icon(c, iconChevronDownSVG).Size(10, 10).TextColor(theme.TextMuted)
	})
	chip.Menu(func(m *ui.Menu) {
		if !idle {
			m.Item(a.tr("composer.switchBusy")).Disabled(true)
			m.Separator()
		}
		for _, choice := range a.providers() {
			choice := choice
			m.Submenu(choice.Label, func(sub *ui.Menu) {
				for _, model := range choice.Models {
					model := model
					item := sub.Item(a.modelLabel(choice.ProviderID, model)).
						Checked(choice.ProviderID == session.ProviderID() && model == session.Model()).
						Disabled(!idle)
					if item.Chosen() {
						a.selectModel(session, providers.Selection{ProviderID: choice.ProviderID, Model: model}, "")
					}
				}
			}).Disabled(!idle)
		}
		efforts := a.effortsFor(providers.Selection{ProviderID: session.ProviderID(), Model: session.Model()})
		if len(efforts) > 0 {
			m.Separator()
			m.Submenu(a.tr("composer.effort"), func(sub *ui.Menu) {
				for _, effort := range efforts {
					effort := effort
					item := sub.Item(a.effortLabel(effort)).Checked(effort == session.Effort()).Disabled(!idle)
					if item.Chosen() {
						a.setEffort(session, effort)
					}
				}
			}).Disabled(!idle)
		}
	})
}

// permissionChip 只读展示当前访问模式（dsh 的 Full access / Workspace Write 那一档）。
//
// 我们的权限是**启动期全局开关** `allow_shell`，不支持按会话切换，所以这里只展示，
// 点击没有动作，靠 Tooltip 说明去哪里改（避免"这里能改"的错觉）。
//
// 取值必须来自**执行端**（`host.Info()`），不能用界面自己的 opts：远程模式下执行端
// 可能开了命令执行而展示端没配（--connect 甚至根本不读本地配置），按本地猜会把
// 「完全权限」显示成「工作区内修改」。
func (a *App) permissionChip(c *ui.Context, session *Session, theme *ui.Theme) {
	label := a.tr("permission.workspaceWrite")
	if a.allowShell() {
		label = a.tr("permission.fullAccess")
	}
	ui.Row(c).Gap(4).Padding(2, 6).AlignItems(ui.Center).
		Label(a.tr("permission.title")).
		Tooltip(a.tr("permission.hint") + "\n" + a.tr("permission.mode", label)).
		Children(func() {
			ui.Text(c, label).FontSize(11).TextColor(theme.TextMuted)
		})
}

// allowShell 返回**执行端**的命令执行权限（展示端以握手结果为准确）。
func (a *App) allowShell() bool {
	if a.host == nil {
		return a.opts.AllowShell
	}
	return a.host.Info().AllowShell
}

// sendCircleButton 渲染圆形发送按钮（dsh 的 ↑）。
//
// 可用时用 dsh 的主按钮色（近黑 `ButtonSolid`，见 §0.5.1 的说明），禁用时灰底；
// 有需要抢占的插话时标签用「插话」。
func (a *App) sendCircleButton(c *ui.Context, session *Session, steerable bool, theme *ui.Theme) {
	disabled := a.sendDisabled(session)
	size := float32(26)
	label := a.tr("composer.send")
	if steerable {
		label = a.tr("composer.steer")
	}
	button := ui.Row(c).Size(size, size).Radius(size / 2).Center().Label(label)
	if disabled {
		button.Background(theme.SurfaceHover)
	} else {
		button.Background(dsh.ButtonSolid).Cursor(ui.CursorPointer)
	}
	if !disabled && button.Hovered() {
		button.Background(dsh.BrandBlueLo)
	}
	button.Children(func() {
		color := theme.TextMuted
		if !disabled {
			color = dsh.BgBase
		}
		ui.Icon(c, iconSendSVG).Size(14, 14).TextColor(color)
	})
	if !disabled && button.Clicked() {
		a.Send()
	}
}
