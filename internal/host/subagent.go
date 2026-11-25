package host

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"simple-agent/internal/driver"
	"simple-agent/internal/mailbox"
	"simple-agent/internal/message"
	"simple-agent/internal/tools"
)

// 父子会话通讯（subagent 组）在 host 侧的接线。
//
// 分层：
//   - internal/mailbox —— 存储（JSONL、已读状态），只认"某个目录 + 收件人"
//   - internal/tools   —— 三个工具，只依赖 SubagentHost 接口
//   - 这里             —— 把"哪些会话是父子、投递到哪、要不要唤醒"接起来
//
// 与旧的"项目内邮件"相比只有一条规则变了：**不再要求同一项目**。
// 父子关系本来就可以跨目录（一个会话在任何工作区都能派生任务），
// 按项目隔离会让跨项目父子根本收不到彼此的消息，与功能语义冲突。
//
// 其余规则保持不变：归档会话不能收信、不能给自己发、唤醒有冷却与去抖。

// sessionByID 按 ID 找会话（找不到返回 nil）。
func (l *Local) sessionByID(id string) *Session {
	if id == "" {
		return nil
	}
	for _, session := range l.sessions {
		if session != nil && session.ID == id {
			return session
		}
	}
	return nil
}

// subagentClient 返回某个会话的通讯客户端（nil 表示不可用）。
//
// 不可用的情况：没有 data_dir（纯内存后端，例如测试与嵌入用法）。
// **不再要求 ProjectID**：跨项目父子要能通信，这是这次改动的核心。
func (l *Local) subagentClient(session *Session) tools.SubagentHost {
	if session == nil || l.cfg.Catalog == nil {
		return nil
	}
	dir := l.cfg.Catalog.GlobalMailboxDir()
	if dir == "" {
		return nil
	}
	return &subagentHost{local: l, session: session, dir: dir}
}

// subagentHost 把 host 的会话世界翻译成 tools 要的那几件事。
type subagentHost struct {
	local   *Local
	session *Session
	dir     string
}

// Children 返回自己的子会话（内存里有的优先，档案兜底）。
//
// 为什么要有档案兜底：当前进程可能没加载某个子会话（它属于另一个项目、
// 或者用户在别的执行端建的）。只查内存的话，父会话会看到"我明明派生过
// 它就没了"。
func (h *subagentHost) Children(includeArchived bool) []tools.SubagentPeer {
	seen := map[string]bool{}
	var out []tools.SubagentPeer

	// 1) 内存里的会话：有忙碌状态等实时信息，优先用它们。
	for _, other := range h.local.sessions {
		if other == nil || other.ID == h.session.ID {
			continue
		}
		if other.parentID != h.session.ID {
			continue
		}
		seen[other.ID] = true
		if !includeArchived && other.Archived() {
			continue
		}
		out = append(out, tools.SubagentPeer{
			ID:       other.ID,
			Title:    other.TitleValue(),
			Archived: other.Archived(),
			Activity: h.activityOf(other),
		})
	}

	// 2) 档案里有、内存里没有的：只能给出静态信息（id/标题/归档），
	//    活动明细留零值（Busy 为空 = 未知）。
	if h.local.cfg.Catalog != nil {
		for _, record := range h.local.cfg.Catalog.ChildSessions(h.session.ID) {
			if seen[record.ID] {
				continue
			}
			archived := record.ArchivedAt != ""
			if !includeArchived && archived {
				continue
			}
			out = append(out, tools.SubagentPeer{
				ID:       record.ID,
				Title:    record.Title,
				Archived: archived,
			})
		}
	}
	return out
}

// Inbox 返回发给**本会话**的通知（新→旧）。
//
// 只读自己的收件箱：不接受"看别人收到了什么"，那会让通知变成窥探通道。
func (h *subagentHost) Inbox(unreadOnly bool, limit int) []tools.SubagentNotice {
	box := h.local.openBox(h.dir, h.session.ID)
	list := box.List(unreadOnly, limit)
	out := make([]tools.SubagentNotice, 0, len(list))
	for _, mail := range list {
		out = append(out, toToolNotice(mail))
	}
	return out
}

// ReadNotice 读一条通知的正文，然后**把它从收件箱里移除**。
//
// 为什么读完就删：通知是**一次性**的协作信号 —— 一个 agent 向另一个
// agent 汇报/请求，收件人读过并据此行动之后，它就没有价值了。
// 留在收件箱里有两个坏处（都真实发生了）：
//   - 未读数居高不下，每轮请求都重复注入同一条通知（见 notice 的"只组装一次"）
//   - 列表越来越长，"哪些还没处理"分不清
//
// 标记已读只是中间态（保证同一条不会被重复唤醒），删除才是终点。
func (h *subagentHost) ReadNotice(id string) (tools.SubagentNotice, error) {
	box := h.local.openBox(h.dir, h.session.ID)
	id = strings.TrimSpace(id)
	mail, err := box.Get(id)
	if err != nil {
		if errors.Is(err, mailbox.ErrNotFound) {
			return tools.SubagentNotice{}, fmt.Errorf(
				"收件箱里没有 id 为 %s 的通知（用不带参数的 subagent_read 看列表）", id)
		}
		return tools.SubagentNotice{}, err
	}
	// 删除失败不让读失败：内容已经拿到了，用户/模型能继续处理。
	// 最坏情况是这条下次还在列表里，重新读一遍即可。
	if err := box.Drop(id); err != nil {
		_ = err
	}
	mail.Read = true
	return toToolNotice(mail), nil
}

// toToolNotice 把存储层的通知转成工具层视图。
func toToolNotice(mail mailbox.Mail) tools.SubagentNotice {
	return tools.SubagentNotice{
		ID:        mail.ID,
		From:      mail.From,
		FromTitle: mail.FromTitle,
		Subject:   mail.Subject,
		Body:      mail.Body,
		Read:      mail.Read,
	}
}

// NotifyParent 给自己父会话发一条通知。
func (h *subagentHost) NotifyParent(subject, body string) (tools.SubagentDelivery, error) {
	parentID := h.session.parentID
	if parentID == "" {
		return tools.SubagentDelivery{}, fmt.Errorf(
			"当前会话没有父会话（它不是由别的会话派生的），无法通知父会话")
	}
	target := h.local.sessionByID(parentID)
	if target == nil {
		return tools.SubagentDelivery{}, fmt.Errorf("父会话不存在：%s", parentID)
	}
	return h.deliver(target, subject, body, false)
}

// NotifyChild 给**一个**指定子会话发通知。
func (h *subagentHost) NotifyChild(to, subject, body string) (tools.SubagentDelivery, error) {
	to = strings.TrimSpace(to)
	if to == "" {
		return tools.SubagentDelivery{}, fmt.Errorf("缺少收件会话 id（to 参数）")
	}
	target := h.local.sessionByID(to)
	if target == nil {
		return tools.SubagentDelivery{}, fmt.Errorf("会话不存在：%s", to)
	}
	// 权限边界：只能发给自己派生的子会话。
	//
	// 刻意不用"同项目"当判据——那正是这次要放开的；父子关系是唯一依据。
	if target.parentID != h.session.ID {
		return tools.SubagentDelivery{}, fmt.Errorf(
			"「%s」不是你派生的子会话，只能和自己的子会话通信", target.TitleValue())
	}
	return h.deliver(target, subject, body, true)
}

// deliver 落盘一封通知，必要时唤醒收件会话。
//
// withActivity 决定返回里要不要带对方的实时活动（父→子要，子→父不要：
// 子会话不该依赖父会话的状态）。
func (h *subagentHost) deliver(
	target *Session, subject, body string, withActivity bool,
) (tools.SubagentDelivery, error) {
	if target.ID == h.session.ID {
		return tools.SubagentDelivery{}, fmt.Errorf("不能给自己发通知")
	}
	if target.Archived() {
		return tools.SubagentDelivery{}, fmt.Errorf("「%s」已归档，不能收通知", target.TitleValue())
	}

	box := mailbox.Open(h.dir, target.ID)
	mail, err := box.Add(mailbox.Mail{
		From:      h.session.ID,
		FromTitle: h.session.TitleValue(),
		Subject:   strings.TrimSpace(subject),
		Body:      body,
	})
	if err != nil {
		return tools.SubagentDelivery{}, err
	}

	// 先记下"能不能叫醒"的判定依据，再决定要不要唤醒：
	// 唤醒会让对方真的花 token 跑一轮，得谨慎（见 wakeFor）。
	delivery := tools.SubagentDelivery{
		ID:     target.ID,
		Title:  target.TitleValue(),
		MailID: mail.ID,
	}
	if withActivity {
		activity := h.activityOf(target)
		delivery.Activity = &activity
	}
	delivery.Woken = h.local.wakeFor(target)
	if !delivery.Woken && target.Busy() != driver.BusyIdle {
		delivery.Skipped = "它正在运行，通知已落盘，下一轮会自己看到"
	}
	return delivery, nil
}

// activityOf 组装"这个会话现在在干什么"。
//
// 四个字段都来自已有的运行时信息，没有新增状态（见文档 §3.7）。
func (h *subagentHost) activityOf(s *Session) tools.SubagentActivity {
	return tools.SubagentActivity{
		Busy:      mapSubagentBusy(s.Busy()),
		Tool:      s.RunningTool(),
		ElapsedMS: s.roundElapsed().Milliseconds(),
		Queued:    s.queuedCount(),
	}
}

// mapSubagentBusy 把 driver 的编排状态映射成工具层的枚举。
//
// 映射放在 host 而不是 tools：tools 不能依赖 driver（会成环）。
func mapSubagentBusy(busy driver.Busy) string {
	switch busy {
	case driver.BusyStreaming:
		return tools.SubagentBusyStreaming
	case driver.BusyRunningTools:
		return tools.SubagentBusyRunningTool
	case driver.BusyAwaitingAnswer:
		return tools.SubagentBusyAwaiting
	case driver.BusyCompacting:
		return tools.SubagentBusyCompacting
	default:
		return tools.SubagentBusyIdle
	}
}

// roundElapsed 返回某会话当前回合已运行时长。
//
// **展示端（镜像）返回 0**：耗时是"执行端进程内"的信息，刻意不上 wire
// （见文档 §3.7），镜像会话也就没有这个值。侧栏只需要"在忙"，那由 Busy 给。
func (s *Session) roundElapsed() time.Duration {
	if s.view != nil || s.drv == nil {
		return 0
	}
	return s.drv.RoundElapsed()
}

// queuedCount 返回排队中的用户插话条数。
func (s *Session) queuedCount() int {
	if s.drv == nil {
		return 0
	}
	return len(s.drv.Queued())
}

// attachSubagent 给会话接上父子通讯：填项目 ID、挂工具、挂请求注入钩子。
//
// **父子都挂**：父会话要能列/通知子会话，子会话要能通知父会话。
// 与 attachSessionTools（新建子会话的能力）分开：后者只给顶层会话。
//
// 幂等：可以重复调用（恢复流程里会话被重新登记时会再走一遍）。
func (l *Local) attachSubagent(session *Session) {
	if session == nil || session.drv == nil {
		return
	}
	// 项目 ID：刚创建的会话还没有，从项目档案里查（AddSessionUnder 刚登记过）。
	//
	// 这一步**不能省**：attachSessionTools 要求有项目才能派生（父子关系要记在
	// 档案里），界面侧栏也要按它分组。原先它和挂工具写在同一个函数里，
	// 正是为了避免"只做一半"。
	if session.ProjectID == "" && l.cfg.Catalog != nil {
		if projectID, err := l.cfg.Catalog.ProjectOfSession(session.ID); err == nil {
			session.ProjectID = projectID
		}
	}
	client := l.subagentClient(session)
	if client == nil {
		return
	}
	if session.toolCtx != nil {
		session.toolCtx.Subagent = client
		// 角色写进工具上下文：执行端（runtime.RunTools）靠它构造同一份工具表。
		// 不写的话执行端会按"顶层"处理，子会话调 subagent_notify_parent 就报
		// "未知工具"—— 清单里有、执行不认识，正是这条链最典型的翻车方式。
		if session.parentID != "" {
			session.toolCtx.SetRole(tools.RoleChild)
		} else {
			session.toolCtx.SetRole(tools.RoleRoot)
		}
	}
	host, ok := client.(*subagentHost)
	if !ok {
		return
	}
	// 请求注入：每轮组装前问一次"有没有未读要通知"。
	session.drv.SetSubagentNotice(func() string { return host.notice() })
}

// wakeFor 决定要不要把收件会话叫起来，返回是否真的安排了唤醒。
//
// 分类讨论（这三种情况的处理方式不同，不能混）：
//
//	运行中（流式/跑工具） → 什么都不做。它下一个请求会自动带上未读通知
//	                        （buildRequest 的 SubagentNotice 钩子），
//	                        打断它没有意义，只会让当前这轮跑得更乱
//	空闲（Idle）          → **唤醒**：发一条通知消息让它跑起来
//	忙碌但不可插话        → 不唤醒。等它自己回到 Idle
//	                        （等回答、压缩中：这两种状态下发消息会失败）
//
// 唤醒的代价必须说清：它真的会让那个会话花 token 跑一整轮。所以额外加两道闸：
//   - 同一会话短时间内最多唤醒一次（wakeCooldown）
//   - 连续来信合并成一次（wakeDelay 去抖窗口）
func (l *Local) wakeFor(target *Session) bool {
	if target == nil || target.Archived() {
		return false
	}
	// 通知界面刷新（侧栏徽标）
	l.emit(Change{SessionID: target.ID, Kind: "mail"})

	// 只看"是否空闲"：运行中的会话不需要唤醒，它自己会看到通知。
	if target.drv == nil || target.drv.Busy() != driver.BusyIdle {
		return false
	}
	if !target.allowWakeScheduling(time.Now()) {
		return false
	}

	// **延迟唤醒（去抖）**：连续来信合并成一次。
	//
	// 为什么需要：一个 agent 经常连着发好几条（"我发现了 A"、"还有 B"、
	// "补充一下"）。每条都唤醒的话，收件人要跑好几轮，既慢又费钱。
	// 等一小段时间让这一批落定，再一次性告诉它"你有 N 条未读"。
	//
	// 延迟期间如果又有新通知，wakeFor 会被再次调用：
	// allowWakeScheduling 发现已有待执行的唤醒就直接返回，定时器不会重置 ——
	// 也就是说唤醒最多推迟 wakeDelay，不会被连续来信无限拖后。
	target.wakeTimer = time.AfterFunc(wakeDelay, func() {
		target.post(func() { l.performWake(target) })
	})
	return true
}

// wakeDelay 是唤醒前的等待时间（去抖窗口）。
//
// 取 3 秒：足够合并"一口气发几条"的常见情形，又不会让人等太久
// （这类协作本来就是异步的，3 秒几乎无感）。
const wakeDelay = 3 * time.Second

// allowWakeScheduling 判断是否可以安排一次唤醒（已有待执行的就不再排）。
func (s *Session) allowWakeScheduling(now time.Time) bool {
	if s.wakeTimer != nil {
		return false // 已经有待执行的唤醒，等它跑
	}
	return s.allowWake(now)
}

// performWake 真正执行唤醒（延迟结束后调用）。
//
// 放在单独函数里而不是塞进 AfterFunc：需要重新检查状态 ——
// 延迟期间会话可能已经被用户唤醒、开始跑、或被归档。
func (l *Local) performWake(target *Session) {
	target.wakeTimer = nil
	if target.Archived() || target.drv == nil || target.drv.Busy() != driver.BusyIdle {
		return
	}
	// 会话可能已被删除（延迟期间用户删掉了它）：确认它还在列表里。
	if l.sessionByID(target.ID) == nil {
		return
	}
	notice := target.subagentWakeNotice()
	if notice == "" {
		return
	}
	effects, err := target.drv.Send(notice)
	if err != nil {
		return
	}
	l.applyEffects(target, effects)
}

// allowWake 判断现在是否可以唤醒（并记录本次唤醒时间）。
func (s *Session) allowWake(now time.Time) bool {
	if !s.lastWakeAt.IsZero() && now.Sub(s.lastWakeAt) < wakeCooldown {
		return false
	}
	s.lastWakeAt = now
	return true
}

// wakeCooldown 是同一会话两次唤醒之间的最小间隔。
//
// 取 30 秒：足够覆盖"一次工具调用 + 一次回复"的时间尺度，
// 又不至于让通知积压太久。注意它**不阻止投递**，只决定要不要主动叫醒。
const wakeCooldown = 30 * time.Second

// subagentWakeNotice 组装唤醒用的那条消息。
//
// 与 buildRequest 注入的通知内容一致（都从未读状态算），
// 但这条**会真的进入对话历史**（它是 Send，不是注入）。
//
// 之所以接受它进历史：模型必须有"我为什么被叫醒"的线索，否则它会在
// 一个空白提示下自由发挥。控制成本的办法是让文本尽量短、且只在
// 确实有未读时才发（空邮箱根本不唤醒）。
func (s *Session) subagentWakeNotice() string {
	if s.host == nil {
		return ""
	}
	client, ok := s.host.subagentClient(s).(*subagentHost)
	if !ok {
		return ""
	}
	unread, _ := mailbox.Open(client.dir, s.ID).Unread()
	if unread == 0 {
		return ""
	}
	// 已经因为同一批通知唤醒过就不再重复（只有新的才再叫一次）。
	if unread <= s.wokenUnread && s.wokenUnread > 0 {
		return ""
	}
	s.wokenUnread = unread
	return client.notice()
}

// subagentUnread 返回某会话的未读数（供界面徽标与请求注入共用）。
//
// 每次新建 Box（内部懒加载）：通知量小、频率低，比起在 host 里维护一份
// 缓存并处理失效，现读更简单也更不容易出错。
func (l *Local) subagentUnread(session *Session) int {
	if session == nil || l.cfg.Catalog == nil {
		return 0
	}
	dir := l.cfg.Catalog.GlobalMailboxDir()
	if dir == "" {
		return 0
	}
	count, _ := l.openBox(dir, session.ID).Unread()
	return count
}

// openBox 打开某会话的收件箱（含旧项目目录的只读兜底）。
//
// 旧目录按**发件会话所属项目**反查不可行（收件人自己可能不属于任何项目），
// 所以这里把**所有**项目目录都作为候选：多读几个不存在的文件代价很小
// （一次 stat），而漏读会让用户的旧未读彻底看不见。
func (l *Local) openBox(dir, sessionID string) *mailbox.Box {
	return mailbox.OpenWithLegacy(dir, sessionID, l.legacyMailboxDirs())
}

// legacyMailboxDirs 返回所有项目的旧邮箱目录（只读兜底用）。
func (l *Local) legacyMailboxDirs() []string {
	if l.cfg.Catalog == nil {
		return nil
	}
	projects, err := l.cfg.Catalog.ListProjects()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(projects))
	for _, project := range projects {
		if dir := l.cfg.Catalog.MailboxDir(project.ID); dir != "" {
			out = append(out, dir)
		}
	}
	return out
}

// SubagentUnread 是 subagentUnread 的公开版本（界面徽标用）。
//
// 字段名沿用 MailUnread（wire 协议与 Android 端都在用），语义变成
// "未读通知数"。
func (s *Session) MailUnread() int {
	if s == nil || s.host == nil {
		return 0
	}
	return s.host.subagentUnread(s)
}

// notice 组装"未读通知"（注入请求用）。
//
// 内容刻意不含正文：
//   - 正文进了请求就会一直留在上下文里占 token，而收件人可能只关心其中一条
//   - 让模型按需决定怎么处理（subagent_read）
//
// **同一批通知只组装一次**：buildRequest 每轮都会调用这里，而"未读"这个
// 状态在模型真正读掉之前不会变。不记住已通知过哪些，就会每轮重复注入同一段
// 清单 —— 用户看到的是"同一条通知反复出现"，模型也会被反复提示同一件事
// （真实踩过：一条通知连续注入七八轮）。
//
// 判定用 **id 集合**而不是未读数：未读数相等不代表是同一批（读掉一封又来一封
// 时数量可能刚好相同，那样新信就永远不提醒了）。
func (h *subagentHost) notice() string {
	box := h.local.openBox(h.dir, h.session.ID)
	list := box.List(true, 10)
	if len(list) == 0 {
		h.session.notifiedMu.Lock()
		h.session.notifiedIDs = nil
		h.session.notifiedMu.Unlock()
		return ""
	}

	// 挑出**还没通知过**的那些。
	h.session.notifiedMu.Lock()
	fresh := make([]mailbox.Mail, 0, len(list))
	for _, mail := range list {
		if h.session.notifiedIDs[mail.ID] {
			continue
		}
		fresh = append(fresh, mail)
	}
	if len(fresh) == 0 {
		h.session.notifiedMu.Unlock()
		return "" // 这一批已经说过了，本轮不再注入
	}
	if h.session.notifiedIDs == nil {
		h.session.notifiedIDs = map[string]bool{}
	}
	for _, mail := range fresh {
		h.session.notifiedIDs[mail.ID] = true
	}
	h.session.notifiedMu.Unlock()

	senders := make([]string, 0, len(fresh))
	seen := map[string]bool{}
	for _, mail := range fresh {
		name := mail.FromTitle
		if name == "" {
			name = mail.From
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		senders = append(senders, name)
	}

	var lines []string
	lines = append(lines, fmt.Sprintf(
		"你有 %d 条未读通知（来自「%s」）：", len(fresh), strings.Join(senders, "」「")))
	for _, mail := range fresh {
		lines = append(lines, fmt.Sprintf("- [%s] %s", mail.ID, mail.Subject))
	}
	if unread, _ := box.Unread(); unread > len(fresh) {
		lines = append(lines, fmt.Sprintf("（还有 %d 条未列出）", unread-len(fresh)))
	}
	lines = append(lines, "",
		"这只是通知的清单，正文不在这里。",
		"用 subagent_read 读正文（不带参数看列表，带 id 读某一封）。读完会自动移除。",
		"如果不需要你做任何事，不必回信 —— 每次回信都会唤醒对方。")
	return strings.Join(lines, "\n")
}

// 编译期确认：subagentHost 满足 tools 的接口。
var _ tools.SubagentHost = (*subagentHost)(nil)

// SubagentForTest 返回会话的通讯客户端（测试用）。
func (s *Session) SubagentForTest() tools.SubagentHost {
	if s == nil || s.host == nil {
		return nil
	}
	return s.host.subagentClient(s)
}

// SubagentNoticeForTest 返回当前会组装出的未读通知（测试用）。
func (s *Session) SubagentNoticeForTest() string {
	if s == nil || s.host == nil {
		return ""
	}
	client, ok := s.host.subagentClient(s).(*subagentHost)
	if !ok {
		return ""
	}
	return client.notice()
}

// SetArchivedForTest 设置归档状态（测试用）。
//
// 生产路径的归档要落盘（项目档案是权威），这里只改内存标志：
// 测试关注的是"归档会话能不能收通知"，不关心持久化。
func (s *Session) SetArchivedForTest(archived bool) { s.archived = archived }

// SetProjectIDForTest 设置会话的项目归属（测试用）。
//
// 用途：造"项目被移除 / 两边分属不同项目"的情形，
// 验证父子通讯不再以同项目为前提。
func (s *Session) SetProjectIDForTest(projectID string) { s.ProjectID = projectID }

// ToolRoleForTest 返回写进工具上下文的角色（测试用）。
//
// 用途：验证"角色传到了执行端看得见的地方"——子会话的 subagent 工具
// 依赖它（漏传的表现是调用报"未知工具"）。
func (s *Session) ToolRoleForTest() tools.SubagentRole {
	if s == nil || s.toolCtx == nil {
		return tools.RoleRoot
	}
	return s.toolCtx.RoleOf()
}

// SetRunningToolForTest 设置"正在跑的工具"（测试用）。
//
// 生产路径由 runTools 在主线程维护；测试要造"某个子会话卡在 bash 里"
// 这种状态，没法真跑一个工具。
func (s *Session) SetRunningToolForTest(name string) { s.setRunningTool(name) }

// SetBusyForTest 强行改变会话的忙碌状态（测试用）。
//
// 只能用于展示"别人看到的状态"：真实状态由 driver 在流/工具边界上推进。
func (s *Session) SetBusyForTest(busy string) {
	if s == nil || s.drv == nil {
		return
	}
	s.drv.SetBusyForTest(driverBusy(busy))
}

// driverBusy 把字符串转成 driver.Busy（只在测试辅助里用）。
func driverBusy(busy string) driver.Busy { return driver.Busy(busy) }

// ToolsForTest 返回会话当前的工具列表（测试用）。
// 用途：验证"子会话的工具列表里没有 new_session / subagent_list"——
// 那是**不注册**，而不是"注册了但调用失败"，所以必须看列表本身。
func (s *Session) ToolsForTest() []*tools.ToolDef {
	if s == nil || s.drv == nil {
		return nil
	}
	return s.drv.Tools()
}

// LastUserTextForTest 返回最后一条用户消息的文本（测试用）。
//
// 用途：验证唤醒注入的那条消息内容（不含正文、说了什么）。
func (s *Session) LastUserTextForTest() string {
	if s == nil || s.drv == nil {
		return ""
	}
	entries := s.drv.Conversation().Entries()
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index].Message
		if entry == nil || entry.Message == nil {
			continue
		}
		if entry.Message.Role != message.RoleUser {
			continue
		}
		var texts []string
		for _, part := range entry.Message.Parts {
			if text, ok := part.Content.(message.TextPart); ok {
				texts = append(texts, text.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}
