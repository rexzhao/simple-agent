package uiapp

import (
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/locale"
)

// ExecutorRef 是"可连接的执行端"（经 relay 时可能有多台）。
type ExecutorRef struct {
	HostID string
	Name   string
	Online bool
}

// ExecutorSource 让界面能列出/切换执行端（直连模式下为 nil）。
type ExecutorSource interface {
	Executors() ([]ExecutorRef, error)
	Current() string
	Switch(hostID string) error
}

// fetchExecutors 在后台拉一次执行端列表，结果 post 回界面线程（渲染只读缓存）。
func (a *App) fetchExecutors() {
	if a.executors == nil || a.executorFetching {
		return
	}
	a.executorFetching = true
	go func() {
		refs, err := a.executors.Executors()
		a.post(func() {
			a.executorFetching = false
			a.executorFetched = time.Now()
			if err != nil {
				a.executorErr = err.Error()
				return
			}
			a.executorErr = ""
			a.executorList = refs
		})
	}()
}

// refreshExecutors 打开列表期间每 4 秒刷新一次（渲染路径只做时间判断，不发请求）。
func (a *App) refreshExecutors() {
	if a.executors == nil || a.executorFetching {
		return
	}
	if time.Since(a.executorFetched) < 4*time.Second {
		return
	}
	a.fetchExecutors()
}

// OpenExecutorPicker 打开"选择执行端"列表（仅 relay 模式可用）。
func (a *App) OpenExecutorPicker() {
	if a.executors == nil {
		return
	}
	a.pickerOpen = true
	a.pickerIndex = 0
	a.fetchExecutors()
	refs := a.executorList
	for index, ref := range refs {
		if ref.HostID == a.executors.Current() {
			a.pickerIndex = index
			break
		}
	}
}

// pickExecutor 选中列表里的第 index 项：保存当前执行端的草稿，切换后恢复目标执行端的草稿。
func (a *App) pickExecutor(index int) {
	if a.executors == nil {
		return
	}
	refs := a.executorList
	if index < 0 || index >= len(refs) {
		return
	}
	target := refs[index]
	if target.HostID == a.executors.Current() {
		a.pickerOpen = false
		return
	}
	a.saveDraftForHost()
	if err := a.executors.Switch(target.HostID); err != nil {
		a.flashErr = a.tr("relay.switchFailed", err.Error())
		return
	}
	a.pickerOpen = false
	a.syncSessions()
	a.restoreDraftForHost()
}

// saveDraftForHost 把输入框草稿存进"按执行端 + 会话"的槽位。
//
// 切换执行端之后会话是另一台机器上的，草稿绝不能跟着过去。
func (a *App) saveDraftForHost() {
	if a.executors == nil {
		return
	}
	session := a.Current()
	if session == nil {
		return
	}
	if a.draftsByHost == nil {
		a.draftsByHost = map[string]string{}
	}
	if text := a.draft; text != "" {
		a.draftsByHost[a.executors.Current()+"\x00"+session.ID] = text
	}
	a.draft = ""
}

// restoreDraftForHost 切回来时把该执行端的草稿放回输入框。
func (a *App) restoreDraftForHost() {
	if a.executors == nil || a.draftsByHost == nil {
		return
	}
	session := a.Current()
	if session == nil {
		return
	}
	if text, ok := a.draftsByHost[a.executors.Current()+"\x00"+session.ID]; ok {
		a.draft = text
	}
}

// executorPickerPane 渲染"选择执行端"列表。
func (a *App) executorPickerPane(c *ui.Context) {
	theme := c.Theme()
	a.refreshExecutors()
	refs := a.executorList
	ui.Column(c).Grow(1).FillHeight().MinWidth(centerMinWidth).Center().Gap(10).
		Children(func() {
			ui.Text(c, a.tr("relay.pick.title")).FontSize(15).TextColor(theme.Text)
			ui.Text(c, a.tr("relay.pick.hint")).FontSize(12).TextColor(theme.TextMuted)
			if a.executorErr != "" {
				ui.Text(c, a.tr("relay.pick.failed", a.executorErr)).FontSize(12).
					TextColor(theme.Danger).MaxLines(3)
			} else if len(refs) == 0 && a.executorFetched.IsZero() {
				ui.Text(c, a.tr("relay.pick.loading")).FontSize(12).TextColor(theme.TextMuted)
			}
			ui.Column(c).Gap(6).MinWidth(320).Children(func() {
				if len(refs) == 0 {
					ui.Text(c, a.tr("relay.pick.empty")).FontSize(12).TextColor(theme.TextMuted)
					return
				}
				for index, ref := range refs {
					row := ui.Row(c).Padding(8, 12).Gap(10).Radius(8).
						AlignItems(ui.Center).Cursor(ui.CursorPointer)
					if index == a.pickerIndex {
						row.Background(theme.Selection)
					}
					row.Children(func() {
						name := ref.Name
						if name == "" {
							name = ref.HostID
						}
						ui.Text(c, name).FontSize(13).TextColor(theme.Text).Grow(1)
						status := a.tr("relay.pick.online")
						color := theme.TextMuted
						if !ref.Online {
							status = a.tr("relay.pick.offline")
							color = theme.Danger
						}
						ui.Text(c, status).FontSize(11).TextColor(color)
					})
					if row.Clicked() {
						a.pickExecutor(index)
					}
				}
			})
		})
}

// pickerTitle 是头部按钮/菜单项的文案（直连模式没有）。
var _ = locale.ZH
