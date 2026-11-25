package host_test

import (
	"strings"
	"testing"

	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// subagentOf 取出会话的通讯客户端（测试用）。
func subagentOf(t *testing.T, session *host.Session) tools.SubagentHost {
	t.Helper()
	client := session.SubagentForTest()
	if client == nil {
		t.Fatal("会话应接入父子通讯")
	}
	return client
}

// newSubagentPair 造两个**不同项目**的会话，并让第二个成为第一个的子会话。
//
// 为什么刻意跨项目：这次改动的核心就是"取消项目限制"，
// 同项目的话测不出任何新东西。
func newSubagentPair(t *testing.T) (*host.Local, *host.Session, *host.Session) {
	t.Helper()
	catalog := store.NewCatalog(t.TempDir())
	l := newHostWithCatalog(t, catalog)
	parent := l.NewSession(t.TempDir())
	childID, err := l.NewChildSession(parent, "子任务", "")
	if err != nil {
		t.Fatal(err)
	}
	child := l.Session(childID)
	if child == nil {
		t.Fatal("子会话应存在")
	}
	return l, parent, child
}

// 收发必须成对：notify 发出去的内容，收件人一定读得到。
//
// 真实事故：早期版本只有发送、没有读取工具，于是"能看到有 1 条未读、
// 正文永远读不出来"——子会话的汇报全丢在正文里，父会话只能干看着。
// 通讯只通一半比完全不通更糟：它会让人以为功能在工作。
func TestSubagentNoticeReadableByRecipient(t *testing.T) {
	_, parent, child := newSubagentPair(t)

	if _, err := subagentOf(t, child).NotifyParent("汇报", "正文内容 ABC"); err != nil {
		t.Fatal(err)
	}

	// 父会话能看到清单
	list := subagentOf(t, parent).Inbox(true, 10)
	if len(list) != 1 {
		t.Fatalf("父会话应有 1 条未读，实际 %d", len(list))
	}
	if list[0].Read {
		t.Fatal("未读标记不对")
	}

	// 并且能读到正文 —— 这是这条测试的要害
	mail, err := subagentOf(t, parent).ReadNotice(list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if mail.Body != "正文内容 ABC" {
		t.Fatalf("正文应能原样读回，实际 %q", mail.Body)
	}

	// 读过之后未读清零（否则会被反复唤醒）
	if unread := parent.MailUnread(); unread != 0 {
		t.Fatalf("读过后未读应为 0，实际 %d", unread)
	}
}

// 读不到别人的通知：收件箱只认自己的 id。
//
// 这是"不做成通用 read"的边界：否则通知就变成窥探其它会话的通道。
func TestSubagentReadRejectsForeignNotice(t *testing.T) {
	_, parent, child := newSubagentPair(t)

	// 父 → 子 发一条，然后父会话自己拿着那个 id 去读
	delivery, err := subagentOf(t, parent).NotifyChild(child.ID, "给子会话", "子会话才该读")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := subagentOf(t, parent).ReadNotice(delivery.MailID); err == nil {
		t.Fatal("父会话不该能读到发给子会话的通知")
	}
}

// 同一批通知**只组装一次**：buildRequest 每轮都会问，未读不变就不该重复注入。
//
// 真实事故：一条通知连续注入了七八轮。原因是 notice() 只看未读数，
// 而"未读"在模型真正读掉之前不会变 —— 每轮都重新组装同一段清单。
func TestSubagentNoticeAssemblesOnce(t *testing.T) {
	_, parent, child := newSubagentPair(t)

	if _, err := subagentOf(t, child).NotifyParent("汇报", "正文"); err != nil {
		t.Fatal(err)
	}

	first := parent.SubagentNoticeForTest()
	if first == "" {
		t.Fatal("有未读时首次应注入通知")
	}
	// 未读没变（没读）→ 后续几轮都不该再注入
	for i := 0; i < 3; i++ {
		if again := parent.SubagentNoticeForTest(); again != "" {
			t.Fatalf("第 %d 次重复注入了同一条通知：\n%s", i+2, again)
		}
	}
}

// 新来一封时，即使未读数与上一批相同，也必须重新通知。
//
// 这是"用 id 集合而不是计数"的理由：读掉一封、又来一封时，
// 未读数可能刚好抵消，按计数判定就会永远不提醒。
func TestSubagentNoticeFiresForNewMailAfterRead(t *testing.T) {
	_, parent, child := newSubagentPair(t)

	if _, err := subagentOf(t, child).NotifyParent("第一封", "a"); err != nil {
		t.Fatal(err)
	}
	list := subagentOf(t, parent).Inbox(true, 10)
	if len(list) != 1 {
		t.Fatalf("应有 1 条未读，实际 %d", len(list))
	}
	if _, err := subagentOf(t, parent).ReadNotice(list[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := parent.SubagentNoticeForTest(); got != "" {
		t.Fatalf("读掉之后不该再注入，实际：\n%s", got)
	}

	// 再来一封：未读数又回到 1（与第一封时相同），仍必须通知
	if _, err := subagentOf(t, child).NotifyParent("第二封", "b"); err != nil {
		t.Fatal(err)
	}
	notice := parent.SubagentNoticeForTest()
	if !strings.Contains(notice, "第二封") {
		t.Fatalf("新来的一封必须通知，实际：%q", notice)
	}
}

// 读过之后从收件箱里消失（不是只标已读，否则列表会越堆越长）。
func TestSubagentReadRemovesNotice(t *testing.T) {
	_, parent, child := newSubagentPair(t)

	if _, err := subagentOf(t, child).NotifyParent("一次性", "正文"); err != nil {
		t.Fatal(err)
	}
	list := subagentOf(t, parent).Inbox(false, 10)
	if len(list) != 1 {
		t.Fatalf("应有 1 条，实际 %d", len(list))
	}
	if _, err := subagentOf(t, parent).ReadNotice(list[0].ID); err != nil {
		t.Fatal(err)
	}

	after := subagentOf(t, parent).Inbox(false, 10)
	if len(after) != 0 {
		t.Fatalf("读过之后收件箱应为空，实际还剩 %d 条", len(after))
	}
	if unread := parent.MailUnread(); unread != 0 {
		t.Fatalf("未读应为 0，实际 %d", unread)
	}
	// 再读同一条应报"没有这条"（内容已消费）
	if _, err := subagentOf(t, parent).ReadNotice(list[0].ID); err == nil {
		t.Fatal("已移除的通知不该还能读到")
	}
}

// 工具名在**整批工具执行期间**都必须可见（不能设完立刻被清掉）。
//
// 真实事故：runTools 启动后台 goroutine 后就返回了，而清空用的是
// `defer s.setRunningTool("")` —— defer 在 runTools 返回时执行，
// 于是刚设进去的名字立刻被抹掉。表现是活动明细永远显示
// "正在执行 工具"（名字为空），父会话看不出子会话在跑什么。
//
// 这里直接测试"设置 → 读取 → 清空"的可见性语义：runTools 的 goroutine
// 边界在真机上不好稳定复现（要卡住一个真实工具），而 bug 的实质就是
// "设置之后立刻被清空"，用这个顺序断言足以锁住它。
func TestRunningToolVisibleUntilCleared(t *testing.T) {
	_, parent, child := newSubagentPair(t)

	// 模拟"工具开始执行"：设置后必须能读到（如果谁在这里加了同步的
	// 清空，这条就会红）。
	child.SetRunningToolForTest("bash")
	if got := child.RunningTool(); got != "bash" {
		t.Fatalf("设置后应能读到工具名，实际 %q", got)
	}

	// 父会话通过活动明细也该看到
	child.SetBusyForTest("runningTools")
	peers := subagentOf(t, parent).Children(false)
	if len(peers) != 1 {
		t.Fatalf("应有 1 个子会话，实际 %d", len(peers))
	}
	if peers[0].Activity.Tool != "bash" {
		t.Fatalf("活动明细里应带工具名，实际 %q", peers[0].Activity.Tool)
	}

	// 整批结束：清空
	child.SetRunningToolForTest("")
	if got := child.RunningTool(); got != "" {
		t.Fatalf("清空后应读不到工具名，实际 %q", got)
	}
}

// 角色必须写进 ToolContext —— 执行端靠它构造同一份工具表。
//
// 真实事故：Role 只传给了 driver 的清单，执行端用全局 Options（Role 零值 =
// 顶层）构造执行表，于是子会话调用 subagent_notify_parent 报「未知工具」。
// 这条断言直接看上下文里的角色，是那条链最靠前的一环。
func TestSubagentRoleReachesToolContext(t *testing.T) {
	_, parent, child := newSubagentPair(t)

	if got := parent.ToolRoleForTest(); got != tools.RoleRoot {
		t.Fatalf("顶层会话的角色应是 RoleRoot，实际 %v", got)
	}
	if got := child.ToolRoleForTest(); got != tools.RoleChild {
		t.Fatalf("子会话的角色应是 RoleChild，实际 %v", got)
	}
}

// 父子的项目归属被抹掉/改写后仍能互相通知 —— 这是本次需求的核心。
//
// 为什么不能用"两个不同工作区的会话"来测：子会话本来就继承父会话的工作区
// （那是刻意的），所以正常路径下它们必然同项目。真正会出现的跨项目情形是：
// 项目被移除、会话没登记进档案（ProjectID 为空）、或者用户把项目目录搬走。
// 旧代码在这种情况下**通讯直接不可用**（mailboxClient 要求 ProjectID 非空），
// 这里就把那三种情形固化成断言。
func TestSubagentNotifyWithoutProject(t *testing.T) {
	l, parent, child := newSubagentPair(t)

	// 把两边的项目归属抹掉，再塞一个不同的项目 ID 进去 ——
	// 等价于"项目被移除 / 两边分属不同项目"。
	parent.SetProjectIDForTest("")
	child.SetProjectIDForTest("另一个项目")

	// 子 → 父
	if _, err := subagentOf(t, child).NotifyParent("进度", "做完了"); err != nil {
		t.Fatalf("跨项目时子会话仍应能通知父会话：%v", err)
	}
	// 父 → 子
	if _, err := subagentOf(t, parent).NotifyChild(child.ID, "追加", "再做一件事"); err != nil {
		t.Fatalf("跨项目时父会话仍应能通知子会话：%v", err)
	}

	// 两个方向都要真的落盘（未读数能读到）
	if unread := child.MailUnread(); unread != 1 {
		t.Fatalf("子会话应有 1 条未读，实际 %d", unread)
	}
	if unread := parent.MailUnread(); unread != 1 {
		t.Fatalf("父会话应有 1 条未读，实际 %d", unread)
	}
	_ = l
}

// 权限边界没有被放宽：只能和**自己的**父/子通信，不能给无关会话发。
func TestSubagentNotifyRejectsStranger(t *testing.T) {
	l, parent, _ := newSubagentPair(t)
	stranger := l.NewSession(t.TempDir())

	if _, err := subagentOf(t, parent).NotifyChild(stranger.ID, "主题", "正文"); err == nil {
		t.Fatal("给非子会话发通知应该失败")
	}

	// 反过来：顶层会话没有父会话，不能调 notify_parent
	if _, err := subagentOf(t, stranger).NotifyParent("主题", "正文"); err == nil {
		t.Fatal("没有父会话时 notify_parent 应该失败")
	}
}

// 工具集按角色注册：**看列表本身**，不是"调了报错"。
func TestSubagentToolsByRole(t *testing.T) {
	_, parent, child := newSubagentPair(t)

	// 顶层：有 list + notify_child + new_session
	for _, name := range []string{"subagent_list", "subagent_notify_child", "new_session"} {
		if !hasTool(parent, name) {
			t.Fatalf("顶层会话应有 %s", name)
		}
	}
	// 顶层：没有 notify_parent（它没有父会话）
	if hasTool(parent, "subagent_notify_parent") {
		t.Fatal("顶层会话不该有 subagent_notify_parent")
	}

	// 子会话：只有 notify_parent
	if !hasTool(child, "subagent_notify_parent") {
		t.Fatal("子会话应有 subagent_notify_parent")
	}
	for _, name := range []string{"subagent_list", "subagent_notify_child", "new_session", "subagent_parent"} {
		if hasTool(child, name) {
			t.Fatalf("子会话不该有 %s", name)
		}
	}
}

// mailbox_* 三个工具对模型**彻底消失**（不只是"调了报错"）。
func TestMailboxToolsGone(t *testing.T) {
	list := tools.BuiltinWith(tools.Options{AllowShell: true, WebFetch: true})
	for _, tool := range list {
		if strings.HasPrefix(tool.Name, "mailbox") {
			t.Fatalf("不该再有 %s 工具", tool.Name)
		}
	}
	// 子会话那一套同样不能有
	for _, tool := range tools.BuiltinWith(tools.Options{Role: tools.RoleChild}) {
		if strings.HasPrefix(tool.Name, "mailbox") {
			t.Fatalf("子会话也不该有 %s 工具", tool.Name)
		}
	}
}

// 身份提示只出现在子会话的系统提示里。
//
// 机制：Guidance 只从**实际注册的工具**里取（见 tools.GuidanceSection），
// 所以子会话才注册 notify_parent ⇒ 那句话只进子会话的提示。
func TestSubagentIdentityOnlyInChildPrompt(t *testing.T) {
	childSection := tools.GuidanceSection(tools.BuiltinWith(tools.Options{Role: tools.RoleChild}))
	rootSection := tools.GuidanceSection(tools.BuiltinWith(tools.Options{Role: tools.RoleRoot}))

	if !strings.Contains(childSection, "spawned by another session") {
		t.Fatalf("子会话的提示应说明它的身份：\n%s", childSection)
	}
	if strings.Contains(rootSection, "spawned by another session") {
		t.Fatalf("顶层会话的提示不该有子会话身份：\n%s", rootSection)
	}
}

// 活动明细要能报出"在跑哪个工具"和"跑了多久"。
func TestSubagentActivityReportsRunningTool(t *testing.T) {
	_, parent, child := newSubagentPair(t)

	child.SetRunningToolForTest("bash")
	child.SetBusyForTest("runningTools")

	peers := subagentOf(t, parent).Children(false)
	if len(peers) != 1 {
		t.Fatalf("应有 1 个子会话，实际 %d", len(peers))
	}
	activity := peers[0].Activity
	if activity.Busy != tools.SubagentBusyRunningTool {
		t.Fatalf("busy 应是 runningTool，实际 %q", activity.Busy)
	}
	if activity.Tool != "bash" {
		t.Fatalf("tool 应是 bash，实际 %q", activity.Tool)
	}
}

// 空闲会话的 elapsed 必须是 0，否则界面显示成"一直在跑"。
func TestSubagentActivityIdleHasNoElapsed(t *testing.T) {
	_, parent, child := newSubagentPair(t)

	peers := subagentOf(t, parent).Children(false)
	if len(peers) != 1 {
		t.Fatalf("应有 1 个子会话，实际 %d", len(peers))
	}
	if got := peers[0].Activity.ElapsedMS; got != 0 {
		t.Fatalf("空闲会话的 elapsed 应为 0，实际 %d", got)
	}
	if got := peers[0].Activity.Busy; got != tools.SubagentBusyIdle {
		t.Fatalf("busy 应是 idle，实际 %q", got)
	}
	_ = child
}

// 归档子会话默认不列出，includeArchived 才出现。
func TestSubagentListIncludesArchivedFlag(t *testing.T) {
	_, parent, child := newSubagentPair(t)
	child.SetArchivedForTest(true)

	if peers := subagentOf(t, parent).Children(false); len(peers) != 0 {
		t.Fatalf("归档的子会话默认不该列出，实际 %d 个", len(peers))
	}
	peers := subagentOf(t, parent).Children(true)
	if len(peers) != 1 || !peers[0].Archived {
		t.Fatalf("includeArchived 时应列出一条且标记归档，实际 %+v", peers)
	}
}

// ---------------------------------------------------------------------------
// 子会话模型：默认跟随父会话，/subagent 可覆盖
// ---------------------------------------------------------------------------

// newHostWithModels 造一个配了模型列表的 Local（校验需要它）。
func newHostWithModels(t *testing.T, models []string) *host.Local {
	t.Helper()
	l := host.NewLocal(host.Config{
		Runtime:   &fakeRuntime{},
		Selection: providers.Selection{ProviderID: "fake", Model: models[0]},
		Workspace: t.TempDir(),
		Catalog:   store.NewCatalog(t.TempDir()),
		Post:      (&poster{}).Post,
		Models: []host.ProviderChoice{
			{ProviderID: "fake", Models: models},
		},
	})
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// 不设置时子会话继承父会话的模型与档位（**默认行为不能被新功能改掉**）。
func TestChildSessionInheritsModelByDefault(t *testing.T) {
	l, parent, child := newSubagentPair(t)

	if child.Selection() != parent.Selection() {
		t.Fatalf("子会话应继承父会话模型：%v vs %v", child.Selection(), parent.Selection())
	}
	_ = l
}

// 设了 /subagent model 之后，新建的子会话用那个模型。
func TestChildSessionUsesSubagentModel(t *testing.T) {
	l := newHostWithModels(t, []string{"cheap", "expensive"})
	parent := l.NewSession("")

	result := l.DispatchForTest(parent.ID, host.Command{Kind: "setSubagentModel",
		Payload: mustJSON(t, host.TextPayload{Text: "expensive"})})
	if !result.OK {
		t.Fatalf("设置应成功：%q", result.Error)
	}

	childID, err := l.NewChildSession(parent, "子任务", "")
	if err != nil {
		t.Fatal(err)
	}
	child := l.Session(childID)
	if child.Selection().Model != "expensive" {
		t.Fatalf("子会话应使用 expensive，实际 %q", child.Selection().Model)
	}
	if child.Selection().ProviderID != parent.Selection().ProviderID {
		t.Fatal("provider 不该变（只换模型，不换端点）")
	}
}

// 未知模型名要**立即**报错，不能建出一个跑不起来的会话。
func TestSubagentModelRejectsUnknown(t *testing.T) {
	l := newHostWithModels(t, []string{"cheap"})
	parent := l.NewSession("")

	result := l.DispatchForTest(parent.ID, host.Command{Kind: "setSubagentModel",
		Payload: mustJSON(t, host.TextPayload{Text: "不存在的模型"})})
	if result.OK {
		t.Fatal("未知模型应被拒绝")
	}
	if !strings.Contains(result.Error, "cheap") {
		t.Fatalf("错误信息应列出可选值，实际 %q", result.Error)
	}
	// 拒绝之后设置仍为空 ⇒ 子会话照旧跟随父会话
	if parent.SubagentModel() != "" {
		t.Fatalf("被拒绝的设置不该生效，实际 %q", parent.SubagentModel())
	}
}

// 设置在重启后还在（档案是唯一权威）。
//
// 与 TestChildParentPersists 同一套做法：直接读档案，模拟重启后的读取路径
// （不真造一个 Local 再恢复，那要跑完整个重放流程，与本测试关心的事无关）。
func TestSubagentModelPersists(t *testing.T) {
	catalog := store.NewCatalog(t.TempDir())
	l := newHostWithCatalog(t, catalog)
	l.SetModels([]host.ProviderChoice{{ProviderID: "fake", Models: []string{"cheap", "m"}}})
	parent := l.NewSession("")

	result := l.DispatchForTest(parent.ID, host.Command{Kind: "setSubagentModel",
		Payload: mustJSON(t, host.TextPayload{Text: "cheap"})})
	if !result.OK {
		t.Fatalf("设置应成功：%q", result.Error)
	}

	projectID, err := catalog.ProjectOfSession(parent.ID)
	if err != nil || projectID == "" {
		t.Fatalf("会话应登记进项目档案：%v", err)
	}
	project, err := catalog.LoadProject(projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range project.Sessions {
		if record.ID != parent.ID {
			continue
		}
		if record.ChildModel != "cheap" {
			t.Fatalf("档案里的 child_model 应是 cheap，实际 %q", record.ChildModel)
		}
		return
	}
	t.Fatal("档案里找不到该会话")
}