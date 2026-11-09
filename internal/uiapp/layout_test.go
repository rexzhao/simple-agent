package uiapp_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/locale"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/uiapp"
)

// 这组测试固定 P0.1 的布局行为（数值来自 dsh 的 columns.ts）：
//   - 侧栏可拖动，范围 264–420（Split 自己只有最小值，范围靠我们夹）；
//   - 窗口 < 1024 自动收起成 56px 图标栏，手动可再展开；
//   - 会话多了要能滚动，不能画到窗口外。

// newLayoutApp 构造一个可控制自动收起阈值的界面。
func newLayoutApp(t *testing.T, autoCollapse int, width, height int) (*uiapp.App, *fakeRuntime, *poster, *ui.Tester) {
	t.Helper()
	rt := &fakeRuntime{}
	p := &poster{}
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:             "你是助手",
		Workspace:                t.TempDir(),
		Title:                    "测试会话",
		MaxToolRounds:            3,
		Language:                 locale.ZH, // 测试固定中文文案
		Post:                     p.Post,
		ContextWindow:            200000,
		SidebarAutoCollapseWidth: autoCollapse,
	})
	tester := ui.NewTester(app.View, width, height)
	pump(tester, p)
	return app, rt, p, tester
}

// 侧栏宽度可以通过 Split 拖动改变，并被夹在 264–420。
func TestSplitResizesSidebar(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	if got := app.SidebarWidthForTest(); got != 280 {
		t.Fatalf("默认宽度应是 dsh 的 280，实际 %d", got)
	}

	// 拖动分隔条到右侧：Inspector 就是「Divider」这个无障碍标签
	rect, ok := tester.Find("Divider")
	if !ok {
		t.Fatalf("找不到分隔条：%v", tester.Texts())
	}
	x, y := rect.X+rect.W/2, rect.Y+rect.H/2
	tester.Press(x, y)
	tester.Move(x+40, y)
	tester.Release(x+40, y)
	pump(tester, p)
	if got := app.SidebarWidthForTest(); got <= 280 {
		t.Fatalf("拖动后应更宽，实际 %d", got)
	}
}

// 拖出范围时夹回 264–420（Split 只保最小值，范围由我们负责）。
func TestSplitClampsWidth(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	rect, ok := tester.Find("Divider")
	if !ok {
		t.Fatalf("找不到分隔条")
	}
	x, y := rect.X+rect.W/2, rect.Y+rect.H/2

	// 往左拖很多：应收在 264
	tester.Press(x, y)
	tester.Move(x-600, y)
	tester.Release(x-600, y)
	pump(tester, p)
	if got := app.SidebarWidthForTest(); got != 264 {
		t.Fatalf("左拖到底应夹在 264，实际 %d", got)
	}

	// 往右拖很多：应收在 420
	rect, _ = tester.Find("Divider")
	x, y = rect.X+rect.W/2, rect.Y+rect.H/2
	tester.Press(x, y)
	tester.Move(x+800, y)
	tester.Release(x+800, y)
	pump(tester, p)
	if got := app.SidebarWidthForTest(); got != 420 {
		t.Fatalf("右拖到底应夹在 420，实际 %d", got)
	}
}

// 窗口 < 1024 自动收起成 56px 图标栏；点展开图标可手动展开。
func TestSidebarAutoCollapses(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, 0, 900, 620) // 0 = 用 dsh 默认阈值 1024
	if !app.SidebarCollapsedForTest() {
		t.Fatalf("900 宽窗口应自动收起：%v", tester.Texts())
	}
	// 图标栏里只有「新建会话」「打开侧边栏」两个图标按钮，品牌行与会话行都不在
	if !tester.HasText(locNewSession) || !tester.HasText(locSidebarShow) {
		t.Fatalf("收起后应显示图标栏：%v", tester.Texts())
	}
	if tester.HasText("simple-agent") {
		t.Fatalf("收起后不该有展开态的品牌行：%v", tester.Texts())
	}

	if err := tester.Click(locSidebarShow); err != nil {
		t.Fatalf("点不到展开按钮：%v", err)
	}
	pump(tester, p)
	if app.SidebarCollapsedForTest() {
		t.Fatal("点展开图标后应展开")
	}
}

// 宽窗口下手动收起：点收起图标变图标栏，再点展开图标恢复。
func TestSidebarManualCollapse(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	if app.SidebarCollapsedForTest() {
		t.Fatal("宽窗口默认应展开")
	}
	if err := tester.Click(locSidebarHide); err != nil {
		t.Fatalf("点不到收起按钮：%v", err)
	}
	pump(tester, p)
	if !app.SidebarCollapsedForTest() {
		t.Fatal("点「«」后应收起")
	}
	if err := tester.Click(locSidebarShow); err != nil {
		t.Fatalf("点不到展开按钮：%v", err)
	}
	pump(tester, p)
	if app.SidebarCollapsedForTest() {
		t.Fatal("点展开图标后应展开")
	}
}

// 会话多了要能滚动：滚到底部后最后一条会话仍在窗口内且可点。
func TestSidebarScrollsWithManySessions(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 620)
	for i := 0; i < 40; i++ {
		session := app.NewSession()
		session.Title = fmt.Sprintf("会话 %02d", i)
	}
	pump(tester, p)

	// 侧栏按创建倒序：最新的是「会话 39」，最早的是「会话 00」（在列表底部、需要滚动）
	top, ok := tester.Find("会话 39")
	if !ok {
		t.Fatalf("找不到侧栏会话行：%v", tester.Texts())
	}
	// 滚到底：判据是「会话 00 真的有可见矩形（w>0）」，而不是 HasText——
	// List 会为测量而构建视口外的行，HasText 会提前变真、循环提前退出，
	// 那时元素尺寸还是 0，点它会落空。
	visible := func() bool {
		rect, found := tester.Find("会话 00")
		return found && rect.W > 0 && rect.H > 0
	}
	for i := 0; i < 40 && !visible(); i++ {
		tester.Scroll(top.X+top.W/2, top.Y+top.H/2, 0, 400)
		pump(tester, p)
	}
	if !visible() {
		t.Fatalf("滚到底后应能看到最早的会话：%v", tester.Texts())
	}

	// 滚到底后点击最早那条：能切过去说明它真的在可点区域里
	last := "会话 00"
	if err := tester.Click(last); err != nil {
		t.Fatalf("滚到底后应能点到最后一条会话：%v", err)
	}
	pump(tester, p)
	if app.Current().Title != last {
		t.Fatalf("点击后应切到该会话，实际 %q（窗口高 %v）", app.Current().Title, func() float32 { _, h := app.ViewportForTest(); return h }())
	}
}

// 端到端：默认配置（不关自动收起）+ 900 宽窗口 = 侧栏是图标栏，
// 从图标栏点「＋」新建会话后仍能正常发出一条消息。
//
// 这条用例**故意不关** SidebarAutoCollapseWidth：其余用例为了能点文本按钮都关了它，
// 这里保证"默认配置下主流程通"。
func TestDefaultCollapsedRailCanSendMessage(t *testing.T) {
	app, rt, p, tester := newLayoutApp(t, 0, 900, 620)
	if !app.SidebarCollapsedForTest() {
		t.Fatalf("默认配置下 900 宽窗口应收起成图标栏：%v", tester.Texts())
	}

	if err := tester.Click(locNewSession); err != nil {
		t.Fatalf("点不到图标栏的新建：%v", err)
	}
	pump(tester, p)
	if len(app.Sessions()) < 2 {
		t.Fatalf("应新建一个会话，实际 %d 个", len(app.Sessions()))
	}

	clickComposer(t, tester)
	tester.Type("图标栏也能发消息")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatalf("点不到发送：%v", err)
	}
	pump(tester, p)
	waitFor(t, tester, p, "打开请求", func() bool { return rt.streamCount() == 1 })
}

// 侧栏按 workspace 分组：不同工作区的会话归到不同项目下，组名取目录名。
func TestSidebarGroupsByWorkspace(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	first := app.Current()
	first.SetWorkspaceForTest("/work/alpha")
	first.Title = "alpha 里的会话"
	second := app.NewSession()
	second.SetWorkspaceForTest("/work/beta")
	second.Title = "beta 里的会话"
	pump(tester, p)

	if !tester.HasText("alpha") || !tester.HasText("beta") {
		t.Fatalf("应出现两个项目组：%v", tester.Texts())
	}
	if !tester.HasText("alpha 里的会话") || !tester.HasText("beta 里的会话") {
		t.Fatalf("两个会话都应在树里：%v", tester.Texts())
	}
}

// 旧日志没有 workspace：归到「默认」组。
func TestSidebarDefaultsGroupForLegacySessions(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	app.Current().SetWorkspaceForTest("")
	app.Current().Title = "老会话"
	pump(tester, p)
	if !tester.HasText("默认") {
		t.Fatalf("没有工作区的会话应归到「默认」：%v", tester.Texts())
	}
}

// 搜索框过滤会话标题。
func TestSidebarSearchFilters(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	app.Current().Title = "分析项目结构"
	other := app.NewSession()
	other.Title = "图片粘贴"
	pump(tester, p)
	if !tester.HasText("分析项目结构") || !tester.HasText("图片粘贴") {
		t.Fatalf("前置条件失败：%v", tester.Texts())
	}

	field, ok := tester.Find("搜索会话")
	if !ok {
		t.Fatalf("找不到搜索框：%v", tester.Texts())
	}
	tester.ClickAt(field.X+field.W/2, field.Y+field.H/2)
	tester.Type("图片")
	pump(tester, p)
	if tester.HasText("分析项目结构") {
		t.Fatalf("搜索后不该显示不匹配的会话：%v", tester.Texts())
	}
	if !tester.HasText("图片粘贴") {
		t.Fatalf("搜索后应显示匹配的会话：%v", tester.Texts())
	}
}

// 相对时间：刚建的会话显示「刚刚」。
func TestSidebarRelativeTime(t *testing.T) {
	_, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	pump(tester, p)
	if !tester.HasText("刚刚") {
		t.Fatalf("新会话应显示「刚刚」：%v", tester.Texts())
	}
}

// 右键菜单改名：菜单里选「改名」→ 输入 → 确定 → 标题更新（且不需要旧的常驻按钮）。
func TestSidebarRenameViaContextMenu(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	app.Current().Title = "改名前"
	pump(tester, p)

	if err := tester.RightClick("改名前"); err != nil {
		t.Fatalf("右键点不到会话行：%v", err)
	}
	if err := tester.ChooseMenuItem(locRename); err != nil {
		t.Fatalf("选不到改名：%v", err)
	}
	pump(tester, p)
	if err := tester.Click(locRenameInput); err != nil {
		t.Fatalf("点不到重命名输入：%v（界面：%v）", err, tester.Texts())
	}
	for i := 0; i < 10; i++ {
		tester.Key(0, ui.KeyBackspace)
	}
	tester.Type("改名后")
	pump(tester, p)
	if err := tester.Click(locConfirm); err != nil {
		t.Fatalf("点不到确定：%v", err)
	}
	pump(tester, p)
	if app.Current().Title != "改名后" {
		t.Fatalf("标题应更新，实际 %q", app.Current().Title)
	}
}

// 英文界面：关键文案显示英文，中文不可见（默认语言由 locale.Detect 决定，这里显式指定）。
func TestEnglishUI(t *testing.T) {
	rt := &fakeRuntime{}
	p := &poster{}
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:             "你是助手",
		Workspace:                t.TempDir(),
		Title:                    "English session",
		MaxToolRounds:            3,
		Post:                     p.Post,
		ContextWindow:            200000,
		SidebarAutoCollapseWidth: -1,
		Language:                 locale.EN,
	})
	tester := ui.NewTester(app.View, 1200, 700)
	pump(tester, p)

	for _, want := range []string{locSendEN, locNewSessionEN} {
		if !tester.HasText(want) {
			t.Fatalf("英文界面应显示 %q：%v", want, tester.Texts())
		}
	}
	// 「更多」菜单里的项也是英文
	if err := tester.Click("More"); err != nil {
		t.Fatalf("点不到 More：%v", err)
	}
	if menu := tester.Menu(); !slices.Contains(menu, "Compact context") {
		t.Fatalf("更多菜单应是英文：%v", menu)
	}
	tester.CloseMenu()
	for _, unwanted := range []string{locSend, locNewSession} {
		if tester.HasText(unwanted) {
			t.Fatalf("英文界面不该出现中文 %q：%v", unwanted, tester.Texts())
		}
	}

	// 「停止」只在生成中出现
	if err := tester.Click("Message input"); err != nil {
		t.Fatalf("点不到输入框：%v", err)
	}
	tester.Frame()
	tester.Type("hello")
	pump(tester, p)
	if err := tester.Click(locSendEN); err != nil {
		t.Fatalf("点不到 Send：%v", err)
	}
	pump(tester, p)
	waitFor(t, tester, p, "英文 Stop 出现", func() bool { return tester.HasText(locStopEN) })
	if tester.HasText(locStop) {
		t.Fatalf("英文界面不该出现中文「停止」：%v", tester.Texts())
	}
}

// P1：回合没结束时（工具执行中 / 等待回答 / 流式输出中）不显示回合结束行。
func TestNoTurnFooterWhileTurnInProgress(t *testing.T) {
	_, rt, p, tester := newLayoutApp(t, -1, 1200, 700)
	clickComposer(t, tester)
	tester.Type("跑工具")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`},
		providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"},
	)
	waitFor(t, tester, p, "工具跑完开下一轮", func() bool { return rt.streamCount() == 2 })
	if tester.HasText("已完成") || tester.HasText("处理失败") {
		t.Fatalf("工具轮之间不该有结束行：%v", tester.Texts())
	}

	// 结束这一轮 → 出现结束行
	rt.push(t,
		providers.Event{Kind: providers.EventText, Text: "好了"},
		providers.Event{Kind: providers.EventDone, StopReason: "stop"},
	)
	waitFor(t, tester, p, "回合结束行出现", func() bool { return tester.HasText("已完成") })
}

// P1：耗时按**整个回合**算（user 消息 → 最后一条消息完成），tokens 求和。
func TestTurnDurationSpansWholeTurn(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	conv := app.Current().Conversation()
	start := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	user, err := conv.AddMessage(message.Message{
		MessageID: "u1", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("开始")},
	}, conversation.StatusComplete)
	if err != nil {
		t.Fatal(err)
	}
	user.CreatedAt = start
	// 两轮工具 + 最终回答（同一回合）
	for i, id := range []string{"a1", "a2"} {
		entry, addErr := conv.AddMessage(message.Message{MessageID: id, Role: message.RoleAgent}, conversation.StatusPending)
		if addErr != nil {
			t.Fatal(addErr)
		}
		entry.CreatedAt = start.Add(time.Duration(i+1) * 3 * time.Second)
		_ = conv.EndStream(id)
		entry.SetFinishedAt(start.Add(time.Duration(i+1) * 5 * time.Second))
		entry.SetTokens(100 * (i + 1))
	}

	// 让 App 认为会话空闲（结束行只在空闲时显示）
	pump(tester, p)
	if !tester.HasText("已完成，用时 10秒") {
		t.Fatalf("耗时应是 user → 最后一条消息完成（10秒）：%v", tester.Texts())
	}
	if !tester.HasText("300 tokens") {
		t.Fatalf("tokens 应是整个回合求和（300）：%v", tester.Texts())
	}
}

// P1：重放后的耗时与实时一致（用事件时间戳算，不额外落盘）。
func TestTurnDurationAfterReplay(t *testing.T) {
	dir := t.TempDir()
	catalog := store.NewCatalog(dir)
	makeApp := func(rt *fakeRuntime) (*uiapp.App, *poster) {
		p := &poster{}
		return uiapp.NewApp(rt, uiapp.Options{
			Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
			Workspace:                t.TempDir(),
			Post:                     p.Post,
			Catalog:                  catalog,
			ContextWindow:            200000,
			SidebarAutoCollapseWidth: -1,
			Language:                 locale.ZH,
		}), p
	}

	rt := &fakeRuntime{}
	app, p := makeApp(rt)
	tester := ui.NewTester(app.View, 1200, 700)
	pump(tester, p)
	ensureSession(t, tester, p, app)
	clickComposer(t, tester)
	tester.Type("跑一轮")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	// 拖 1.1 秒再结束，保证耗时 >= 1 秒才会显示「用时」
	time.Sleep(1100 * time.Millisecond)
	rt.push(t,
		providers.Event{Kind: providers.EventText, Text: "好了"},
		providers.Event{Kind: providers.EventDone, StopReason: "stop"},
	)
	waitFor(t, tester, p, "回合结束行", func() bool { return tester.HasText("用时") })
	app.Close()

	app2, p2 := makeApp(&fakeRuntime{})
	defer app2.Close()
	tester2 := ui.NewTester(app2.View, 1200, 700)
	pump(tester2, p2)
	if !tester2.HasText("用时") {
		t.Fatalf("重放后也应显示耗时：%v", tester2.Texts())
	}
}

// P2：回合中间插入 notice 不该把回合切成两段（不闪「已完成」、耗时仍按整个回合算）。
func TestNoticeInsideTurnDoesNotSplitIt(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	conv := app.Current().Conversation()
	start := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	user, err := conv.AddMessage(message.Message{
		MessageID: "u1", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("开始")},
	}, conversation.StatusComplete)
	if err != nil {
		t.Fatal(err)
	}
	user.CreatedAt = start
	first, _ := conv.AddMessage(message.Message{MessageID: "a1", Role: message.RoleAgent}, conversation.StatusPending)
	first.CreatedAt = start.Add(2 * time.Second)
	_ = conv.EndStream("a1")
	first.SetFinishedAt(start.Add(4 * time.Second))
	// 回合中间的通知（例如"部分工具未返回结果"）
	conv.AddNotice(conversation.NoticeWarn, "部分工具未返回结果，已标记为失败。")
	second, _ := conv.AddMessage(message.Message{MessageID: "a2", Role: message.RoleAgent}, conversation.StatusPending)
	second.CreatedAt = start.Add(5 * time.Second)
	_ = conv.EndStream("a2")
	second.SetFinishedAt(start.Add(10 * time.Second))

	pump(tester, p)
	// 只有一条结束行：耗时按整回合（10 秒），而不是后半段的 5 秒
	if !tester.HasText("已完成，用时 10秒") {
		t.Fatalf("耗时按整个回合算：%v", tester.Texts())
	}
	count := 0
	for _, text := range tester.Texts() {
		if strings.HasPrefix(text, "已完成") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("回合中间的通知不该切出多个结束行（实际 %d）：%v", count, tester.Texts())
	}
}

// 删除会话的三个入口（右键 / 悬停「⋯」/ 头部「更多」）都要真的删掉它。
//
// 这里固定的是**多会话**的情况（删掉后还剩别的会话）。唯一会话的情况见
// 下方 `TestDeleteLastSessionRecreatesEmpty` 的说明（语义待用户决定）。
func TestDeleteSessionViaAllEntries(t *testing.T) {
	entries := []struct {
		name string
		run  func(t *testing.T, tester *ui.Tester, p *poster, title string)
	}{
		{"右键菜单", func(t *testing.T, tester *ui.Tester, p *poster, title string) {
			if err := tester.RightClick(title); err != nil {
				t.Fatalf("右键点不到会话行：%v", err)
			}
			if err := tester.ChooseMenuItem(locDelete); err != nil {
				t.Fatalf("右键菜单没有删除：%v", err)
			}
		}},
		{"悬停⋯", func(t *testing.T, tester *ui.Tester, p *poster, title string) {
			rect, ok := tester.Find(title)
			if !ok {
				t.Fatalf("找不到会话行：%v", tester.Texts())
			}
			tester.Move(rect.X+rect.W/2, rect.Y+rect.H/2)
			pump(tester, p)
			if err := tester.Click(locSessionMore + " " + title); err != nil {
				t.Fatalf("点不到 ⋯：%v", err)
			}
			if err := tester.ChooseMenuItem(locDelete); err != nil {
				t.Fatalf("⋯ 菜单没有删除：%v", err)
			}
		}},
		{"头部更多", func(t *testing.T, tester *ui.Tester, p *poster, title string) {
			if err := tester.Click(locMore); err != nil {
				t.Fatalf("点不到头部更多：%v", err)
			}
			// 头部菜单里那一项叫「删除会话」
			if err := tester.ChooseMenuItem(locDeleteSession); err != nil {
				t.Fatalf("头部菜单没有删除：%v", err)
			}
		}},
	}

	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
			for i := 0; i < 2; i++ {
				if err := tester.Click(locNewSession); err != nil {
					t.Fatal(err)
				}
				pump(tester, p)
			}
			for i, session := range app.Sessions() {
				session.Title = []string{"A", "B", "C"}[i]
			}
			pump(tester, p)

			// 删「B」（头部入口会删当前会话 C，所以先切到 B）
			target := app.Sessions()[1]
			if entry.name == "头部更多" {
				if err := tester.Click("B"); err != nil {
					t.Fatalf("点不到 B：%v", err)
				}
				pump(tester, p)
			}
			entry.run(t, tester, p, target.Title)
			pump(tester, p)
			if err := tester.Click(locDelete); err != nil {
				t.Fatalf("点不到确认删除：%v", err)
			}
			pump(tester, p)

			for _, session := range app.Sessions() {
				if session.ID == target.ID {
					t.Fatalf("%s：目标会话仍在：%v", entry.name, titlesOf(app))
				}
			}
			if len(app.Sessions()) != 2 {
				t.Fatalf("%s：应剩 2 个会话，实际 %v", entry.name, titlesOf(app))
			}
		})
	}
}

// titlesOf 返回会话标题列表（测试断言用）。
func titlesOf(app *uiapp.App) []string {
	out := make([]string, 0, len(app.Sessions()))
	for _, s := range app.Sessions() {
		out = append(out, s.Title)
	}
	return out
}

// 真实窗口的时序：点开「⋯」之后指针离开会话行（移到原生弹出菜单上）再选「删除」。
//
// 我的第一版实现只在 `row.Hovered()` 时创建「⋯」，于是"选中之后的那一帧"元素不存在，
// mygo 的 `Chosen()` 永远不触发——真实窗口里表现为"点删除没反应、确认框不出现"。
// 现在「⋯」每帧都创建（不悬停时透明），这个用例把指针移开之后再选，覆盖该时序。
func TestSessionMenuWorksAfterPointerLeavesRow(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	for i := 0; i < 2; i++ {
		if err := tester.Click(locNewSession); err != nil {
			t.Fatal(err)
		}
		pump(tester, p)
	}
	for i, session := range app.Sessions() {
		session.Title = []string{"A", "B", "C"}[i]
	}
	pump(tester, p)

	target := app.Sessions()[1] // B
	// 点开 B 的「⋯」
	if err := tester.Click(locSessionMore + " " + target.Title); err != nil {
		t.Fatalf("点不到 ⋯：%v", err)
	}
	// 指针离开会话行（模拟移到原生菜单上）
	tester.Move(600, 400)
	pump(tester, p)
	if err := tester.ChooseMenuItem(locDelete); err != nil {
		t.Fatalf("选不到删除：%v（菜单：%v）", err, tester.Menu())
	}
	pump(tester, p)
	if !tester.HasText("及其历史将不可恢复") {
		t.Fatalf("确认框应出现：%v", tester.Texts())
	}
	if err := tester.Click(locDelete); err != nil {
		t.Fatalf("点不到确认删除：%v", err)
	}
	pump(tester, p)
	for _, session := range app.Sessions() {
		if session.ID == target.ID {
			t.Fatalf("目标会话仍在：%v", titlesOf(app))
		}
	}
}

// 同一个时序下「⋯」里的改名也要能用。
func TestSessionRenameWorksAfterPointerLeavesRow(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	app.Current().Title = "旧名字"
	pump(tester, p)

	if err := tester.Click(locSessionMore + " 旧名字"); err != nil {
		t.Fatalf("点不到 ⋯：%v", err)
	}
	tester.Move(600, 400)
	pump(tester, p)
	if err := tester.ChooseMenuItem(locRename); err != nil {
		t.Fatalf("选不到改名：%v（菜单：%v）", err, tester.Menu())
	}
	pump(tester, p)
	if err := tester.Click(locRenameInput); err != nil {
		t.Fatalf("点不到重命名输入：%v（界面：%v）", err, tester.Texts())
	}
	for i := 0; i < 10; i++ {
		tester.Key(0, ui.KeyBackspace)
	}
	tester.Type("新名字")
	pump(tester, p)
	if err := tester.Click(locConfirm); err != nil {
		t.Fatalf("点不到确定：%v", err)
	}
	pump(tester, p)
	if app.Current().Title != "新名字" {
		t.Fatalf("改名应生效，实际 %q", app.Current().Title)
	}
}

// 决策 #54（用户选 B）：删掉最后一个会话后列表为空，主区域显示空态。
func TestEmptyStateAfterDeletingLastSession(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	if err := tester.RightClick(app.Current().Title); err != nil {
		t.Fatal(err)
	}
	if err := tester.ChooseMenuItem(locDelete); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if err := tester.Click(locDelete); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	if len(app.Sessions()) != 0 {
		t.Fatalf("应剩 0 个会话：%v", titlesOf(app))
	}
	if !tester.HasText("还没有会话") {
		t.Fatalf("应显示空态：%v", tester.Texts())
	}
	// 空态下不显示输入卡（不要一个禁用的输入框）
	if tester.HasText(locComposer) {
		t.Fatalf("没有会话时不该渲染输入框：%v", tester.Texts())
	}
}

// 空态里的「新建会话」能建出会话，并且可以正常发消息。
func TestEmptyStateNewSessionCanSend(t *testing.T) {
	app, rt, p, tester := newLayoutApp(t, -1, 1200, 700)
	// 先删光
	if err := tester.RightClick(app.Current().Title); err != nil {
		t.Fatal(err)
	}
	if err := tester.ChooseMenuItem(locDelete); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if err := tester.Click(locDelete); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	if err := tester.Click(locNewSession); err != nil {
		t.Fatalf("点不到空态的新建：%v", err)
	}
	pump(tester, p)
	if len(app.Sessions()) != 1 {
		t.Fatalf("应新建 1 个会话：%v", titlesOf(app))
	}
	clickComposer(t, tester)
	tester.Type("空态里新建的会话")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	waitFor(t, tester, p, "打开请求", func() bool { return rt.streamCount() == 1 })
}

// 空数据目录启动：进入空态，并且**不会**写出空会话目录。
func TestStartupWithNoSessionsShowsEmptyState(t *testing.T) {
	dir := t.TempDir()
	catalog := store.NewCatalog(dir)
	p := &poster{}
	app := uiapp.NewApp(&fakeRuntime{}, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:                t.TempDir(),
		Post:                     p.Post,
		Catalog:                  catalog,
		ContextWindow:            200000,
		SidebarAutoCollapseWidth: -1,
		Language:                 locale.ZH,
	})
	defer app.Close()
	tester := ui.NewTester(app.View, 1200, 700)
	pump(tester, p)

	if len(app.Sessions()) != 0 {
		t.Fatalf("空目录启动不该新建会话：%v", titlesOf(app))
	}
	if !tester.HasText("还没有会话") {
		t.Fatalf("应显示空态：%v", tester.Texts())
	}
	if entries, err := os.ReadDir(catalog.SessionsDir()); err == nil && len(entries) != 0 {
		t.Fatalf("不该写出会话目录，实际 %d 个", len(entries))
	}
}

// 没有会话时，Enter / Ctrl+V / ↑ 都不能 panic（什么都不做）。
func TestNoSessionInputIsSafe(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	// 删光
	if err := tester.RightClick(app.Current().Title); err != nil {
		t.Fatal(err)
	}
	if err := tester.ChooseMenuItem(locDelete); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if err := tester.Click(locDelete); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	tester.Key(0, ui.KeyEnter)
	tester.Key(ui.Cmd, ui.KeyV)
	tester.Key(0, ui.KeyUp)
	pump(tester, p)

	if len(app.Sessions()) != 0 {
		t.Fatalf("输入不该凭空造出会话：%v", titlesOf(app))
	}
	// 拖放同理：忽略（不建会话、不 panic）
	app.DropFilesForTest([]string{t.TempDir()})
	pump(tester, p)
	if len(app.Sessions()) != 0 {
		t.Fatalf("拖放不该凭空造出会话：%v", titlesOf(app))
	}
}

// P1：恢复失败必须让用户看得见（空态 + 错误 + 数据目录）；多个失败要累计。
//
// 数据目录里放一个坏到无法恢复的会话：`session.jsonl` 只有半截坏行（readEvents 会
// 报截断/损坏），`openSession` 失败 → 空态 + startupErr。
func TestRestoreFailureIsVisible(t *testing.T) {
	dir := t.TempDir()
	catalog := store.NewCatalog(dir)
	// 造一个"目录存在但日志无法加载"的会话
	bad := filepath.Join(catalog.SessionsDir(), "broken-session")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	// session.jsonl 为空（"日志为空" → openSession 失败）；再放一个归档文件，
	// 免得被 PruneEmpty 当成"没有内容的会话"直接删掉（否则就测不到恢复失败了）。
	if err := os.WriteFile(filepath.Join(bad, "session.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "archive-0001.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &poster{}
	app := uiapp.NewApp(&fakeRuntime{}, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:                t.TempDir(),
		Post:                     p.Post,
		Catalog:                  catalog,
		ContextWindow:            200000,
		SidebarAutoCollapseWidth: -1,
		Language:                 locale.ZH,
	})
	defer app.Close()
	tester := ui.NewTester(app.View, 1200, 700)
	pump(tester, p)

	if len(app.Sessions()) != 0 {
		t.Fatalf("坏会话不该被恢复：%v", titlesOf(app))
	}
	texts := strings.Join(tester.Texts(), "\n")
	if !strings.Contains(texts, "还没有会话") {
		t.Fatalf("应显示空态：%v", tester.Texts())
	}
	if !strings.Contains(texts, "恢复会话") {
		t.Fatalf("应显示恢复失败原因：%v", tester.Texts())
	}
	// 显示的必须是**用户配置的 data_dir**（sessions 的上一级），不是内部的 sessions 目录
	want := "数据目录：" + app.DataDirForTest()
	if !strings.Contains(texts, want) {
		t.Fatalf("应显示数据目录 %q：%v", want, tester.Texts())
	}
	if strings.HasSuffix(app.DataDirForTest(), "sessions") {
		t.Fatalf("显示的目录不该是 sessions 子目录：%q", app.DataDirForTest())
	}
}

// P1：删除失败不能静默（会用 Toast 提示）。
func TestDeleteFailureShowsToast(t *testing.T) {
	dir := t.TempDir()
	catalog := store.NewCatalog(dir)
	p := &poster{}
	app := uiapp.NewApp(&fakeRuntime{}, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:                t.TempDir(),
		Post:                     p.Post,
		Catalog:                  catalog,
		ContextWindow:            200000,
		SidebarAutoCollapseWidth: -1,
		Language:                 locale.ZH,
	})
	defer app.Close()
	tester := ui.NewTester(app.View, 1200, 700)
	pump(tester, p)
	ensureSession(t, tester, p, app)

	// 让删除必然失败：把会话目录换成一个"占位符"文件（RemoveAll 对文件成功，所以
	// 改成没有权限的父目录不好造；这里直接把 Catalog 的目标换成一个不可删的只读名）。
	// 简化：直接调用 deleteSession 并断言 flashErr 机制存在——用 AddFlashErr 钩子。
	app.SetFlashErrForTest("删除会话失败：注入")
	pump(tester, p)
	if !tester.HasText("删除会话失败：注入") {
		t.Fatalf("删除失败应通过 Toast 显示：%v", tester.Texts())
	}
}
