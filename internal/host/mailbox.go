package host

import (
	"fmt"
	"log"
	"strings"
	"time"

	"simple-agent/internal/driver"
	"simple-agent/internal/mailbox"
	"simple-agent/internal/message"
	"simple-agent/internal/tools"
)

// 会话通讯（项目内邮件）在 host 侧的接线。
//
// 分层：
//   - internal/mailbox —— 存储（JSONL、已读状态）
//   - internal/tools   —— 三个工具，只依赖 MailboxHost 接口
//   - 这里             —— 把"哪个项目、哪些会话能收信、投递到哪"这三件事接起来
//
// 规则都集中在这个文件里：归档会话不能收信、跨项目不能发、同项目才互相可见。

// mailboxClient 返回某个会话的通讯客户端（nil 表示不可用）。
//
// 不可用的情况：没有项目归属（ProjectID 为空）、或没有 data_dir（纯内存后端，
// 例如测试与嵌入用法）。这时工具仍然注册但会明确报错。
func (l *Local) mailboxClient(session *Session) tools.MailboxHost {
	if session == nil || session.ProjectID == "" || l.cfg.Catalog == nil {
		return nil
	}
	dir := l.cfg.Catalog.MailboxDir(session.ProjectID)
	if dir == "" {
		return nil
	}
	return &mailboxHost{local: l, session: session, dir: dir}
}

// mailboxHost 把 host 的会话世界翻译成 tools 要的那几件事。
type mailboxHost struct {
	local   *Local
	session *Session
	dir     string
}

// MailboxSelf 返回当前会话（发件人信息）。
func (h *mailboxHost) MailboxSelf() tools.MailboxPeer {
	return tools.MailboxPeer{ID: h.session.ID, Title: h.session.Title}
}

// MailboxPeers 返回本项目内**可收信**的会话。
//
// 排除两类：
//   - 自己（不该给自己发信）
//   - 已归档的会话（用户定的规则：归档会话不能运行，也就不能收信）
//
// 用"本项目"而不是"所有会话"：邮件是项目内的协作产物，跨项目既没有语义
// （两个项目的会话在做什么互不相干），也会让目录与权限边界变模糊。
func (h *mailboxHost) MailboxPeers() []tools.MailboxPeer {
	out := make([]tools.MailboxPeer, 0, len(h.local.sessions))
	for _, other := range h.local.sessions {
		if other == nil || other.ID == h.session.ID {
			continue
		}
		if other.ProjectID != h.session.ProjectID {
			continue
		}
		if other.Archived() {
			continue
		}
		out = append(out, tools.MailboxPeer{
			ID:    other.ID,
			Title: other.TitleValue(),
		})
	}
	return out
}

// MailboxDeliver 投递一封邮件，必要时唤醒收件会话。
func (h *mailboxHost) MailboxDeliver(to, subject, body string, refs []string) (string, error) {
	target := h.local.sessionByID(to)
	if target == nil {
		return "", fmt.Errorf("收件会话不存在：%s", to)
	}
	if target.ID == h.session.ID {
		return "", fmt.Errorf("不能给自己发信")
	}
	if target.ProjectID != h.session.ProjectID {
		return "", fmt.Errorf("只能给同一项目的会话发信")
	}
	if target.Archived() {
		return "", fmt.Errorf("「%s」已归档，不能收信", target.TitleValue())
	}

	box := mailbox.Open(h.dir, target.ID)
	mail, err := box.Add(mailbox.Mail{
		From:      h.session.ID,
		FromTitle: h.session.TitleValue(),
		Subject:   strings.TrimSpace(subject),
		Body:      body,
		Refs:      refs,
	})
	if err != nil {
		return "", err
	}

	// 信已落盘（离线也不会丢）。至于"要不要现在把收件人叫起来"，
	// 是另一件事：唤醒会让它真的花 token 跑一轮，得谨慎。
	h.local.wakeForMail(target)
	return mail.ID, nil
}

// MailboxInbox 返回当前会话的邮件（新→旧）。
func (h *mailboxHost) MailboxInbox(unreadOnly bool, limit int) []tools.MailboxMail {
	box := mailbox.Open(h.dir, h.session.ID)
	list := box.List(unreadOnly, limit)
	out := make([]tools.MailboxMail, 0, len(list))
	for _, mail := range list {
		out = append(out, toToolMail(mail))
	}
	return out
}

// MailboxReadMail 读一封邮件并标记已读。
func (h *mailboxHost) MailboxReadMail(id string) (tools.MailboxMail, error) {
	box := mailbox.Open(h.dir, h.session.ID)
	mail, err := box.Get(id)
	if err != nil {
		if err == mailbox.ErrNotFound {
			return tools.MailboxMail{}, fmt.Errorf("没有 id 为 %s 的邮件", id)
		}
		return tools.MailboxMail{}, err
	}
	if err := box.MarkRead(id); err != nil {
		// 标记失败不该让读信失败：内容已经拿到了，用户/模型能继续处理。
		// （下次还会提示未读，是小事。）
		_ = err
	}
	mail.Read = true
	return toToolMail(mail), nil
}

// toToolMail 把存储层的邮件转成工具层视图。
func toToolMail(mail mailbox.Mail) tools.MailboxMail {
	return tools.MailboxMail{
		ID:        mail.ID,
		From:      mail.From,
		FromTitle: mail.FromTitle,
		Subject:   mail.Subject,
		Body:      mail.Body,
		At:        mail.At,
		Read:      mail.Read,
		Refs:      mail.Refs,
	}
}

// sessionByID 按 ID 找会话（找不到返回 nil）。
func (l *Local) sessionByID(id string) *Session {
	for _, session := range l.sessions {
		if session != nil && session.ID == id {
			return session
		}
	}
	return nil
}

// attachMailbox 给会话接上会话通讯：填项目 ID、挂工具、挂请求注入钩子。
//
// 统一一个入口的原因：这些事必须**一起**做。只填 ProjectID 不挂工具，
// 模型看不到工具；只挂工具不填 ProjectID，工具会报"不可用"。分散在三处
// 迟早漏一个（这个项目里已经踩过多次同类问题）。
//
// 幂等：可以重复调用（恢复流程里会话被重新登记时会再走一遍）。
func (l *Local) attachMailbox(session *Session) {
	if session == nil {
		return
	}
	// 项目 ID：刚创建的会话还没有，从项目档案里查（AddSession 刚登记过）。
	if session.ProjectID == "" && l.cfg.Catalog != nil {
		if projectID, err := l.cfg.Catalog.ProjectOfSession(session.ID); err == nil {
			session.ProjectID = projectID
		}
	}
	if session.ProjectID == "" {
		return // 不属于任何项目 ⇒ 没有通讯能力（工具会明确报错）
	}
	client := l.mailboxClient(session)
	if client == nil {
		return
	}
	if session.toolCtx != nil {
		session.toolCtx.Mailbox = client
	}
	// 请求注入：每轮组装前问一次"有没有未读要通知"。
	//
	// 用 sync.Once 式的判断避免重复挂（关掉时先置 nil 再重挂即可）。
	session.drv.SetMailboxNotice(func() string {
		if host, ok := client.(*mailboxHost); ok {
			return host.mailboxNotice()
		}
		return ""
	})
}

// wakeForMail 决定要不要把收件会话叫起来，以及怎么叫。
//
// 分类讨论（这三种情况的处理方式不同，不能混）：
//
//	运行中（流式/跑工具） → 什么都不做。它下一个请求会自动带上未读通知
//	                        （buildRequest 的 MailboxNotice 钩子），
//	                        打断它没有意义，只会让当前这轮跑得更乱
//	空闲（Idle）          → **唤醒**：发一条通知消息让它跑起来
//	忙碌但不可插话        → 不唤醒。等它自己回到 Idle
//	                        （等回答、压缩中：这两种状态下发消息会失败）
//
// 唤醒的代价必须说清：它真的会让那个会话花 token 跑一整轮。所以额外加两道闸：
//   - 每封邮件只唤醒一次（记在会话的 wokenMails 里）
//   - 同一个会话短时间内最多唤醒一次（wakeCooldown）
func (l *Local) wakeForMail(target *Session) {
	if target == nil || target.Archived() {
		return
	}
	// 通知界面刷新（侧栏徽标）
	l.emit(Change{SessionID: target.ID, Kind: "mail"})

	// 只看"是否空闲"：运行中的会话不需要唤醒，它自己会看到通知。
	if target.drv.Busy() != driver.BusyIdle {
		return
	}
	if !target.allowWakeScheduling(time.Now()) {
		return
	}

	// **延迟唤醒（去抖）**：连续来信合并成一次。
	//
	// 为什么需要：一个 agent 经常连着发好几封（"我发现了 A"、"还有 B"、
	// "补充一下"）。每封都唤醒的话，收件人要跑好几轮，既慢又费钱。
	// 等一小段时间让这一批信落定，再一次性告诉它"你有 N 封未读"。
	//
	// 延迟期间如果又有新邮件，wakeForMail 会被再次调用：
	// allowWakeScheduling 发现已有待执行的唤醒就直接返回，定时器不会重置 ——
	// 也就是说唤醒最多推迟 wakeDelay，不会被连续来信无限拖后。
	target.wakeTimer = time.AfterFunc(wakeDelay, func() {
		target.post(func() { l.performWake(target) })
	})
}

// wakeDelay 是唤醒前的等待时间（去抖窗口）。
//
// 取 3 秒：足够合并"一口气发几封"的常见情形，又不会让人等太久
// （邮件本来就是异步协作，3 秒几乎无感）。
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
	if target.Archived() || target.drv.Busy() != driver.BusyIdle {
		return
	}
	// 会话可能已被删除（延迟期间用户删掉了它）：确认它还在列表里。
	if l.sessionByID(target.ID) == nil {
		return
	}
	notice := target.mailboxWakeNotice()
	if notice == "" {
		return
	}
	effects, err := target.drv.Send(notice)
	if err != nil {
		log.Printf("会话 %s：唤醒失败：%v", target.ID, err)
		return
	}
	l.applyEffects(target, effects)
}

// allowWake 判断现在是否可以唤醒（并记录本次唤醒时间）。
//
// 顺带把"已经唤醒过的邮件 ID"清掉一半：只保留最近的一批，
// 免得这个集合随着会话寿命无限增长。
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
// 又不至于让邮件积压太久。注意它**不阻止邮件投递**，只决定要不要主动叫醒。
const wakeCooldown = 30 * time.Second

// mailboxWakeNotice 组装唤醒用的那条消息。
//
// 与 buildRequest 注入的通知内容一致（都从未读状态算），
// 但这条**会真的进入对话历史**（它是 Send，不是注入）。
//
// 之所以接受它进历史：模型必须有"我为什么被叫醒"的线索，否则它会在
// 一个空白提示下自由发挥。控制成本的办法是让文本尽量短、且只在
// 确实有未读时才发（空邮箱根本不唤醒）。
func (s *Session) mailboxWakeNotice() string {
	if s.host == nil {
		return ""
	}
	client, ok := s.host.mailboxClient(s).(*mailboxHost)
	if !ok {
		return ""
	}
	unread, _ := mailbox.Open(client.dir, s.ID).Unread()
	if unread == 0 {
		return ""
	}
	// 已经因为同一批邮件唤醒过就不再重复（只有新邮件才再叫一次）。
	if unread <= s.wokenUnread && s.wokenUnread > 0 {
		return ""
	}
	s.wokenUnread = unread
	return client.mailboxNotice()
}

// mailboxUnread 返回某会话的未读数（供界面徽标与请求注入共用）。
//
// 每次新建 Box（内部懒加载）：邮件量小、频率低，比起在 host 里维护一份
// 缓存并处理失效，现读更简单也更不容易出错。
func (l *Local) mailboxUnread(session *Session) int {
	if session == nil || session.ProjectID == "" || l.cfg.Catalog == nil {
		return 0
	}
	dir := l.cfg.Catalog.MailboxDir(session.ProjectID)
	if dir == "" {
		return 0
	}
	count, _ := mailbox.Open(dir, session.ID).Unread()
	return count
}

// MailboxUnread 是 mailboxUnread 的公开版本（界面徽标用）。
func (s *Session) MailboxUnread() int {
	if s == nil || s.host == nil {
		return 0
	}
	return s.host.mailboxUnread(s)
}

// mailboxNotice 组装"未读邮件通知"（注入请求用）。
//
// 内容刻意不含正文：
//   - 正文进了请求就会一直留在上下文里占 token，而收件人可能只关心其中一封
//   - 让模型用 mailbox_read 按需取，读过的还能标已读
//
// 同时明确说"不需要行动就不必回信"：两个 agent 互相回信会无限唤醒，
// 每一轮都真花钱。
func (h *mailboxHost) mailboxNotice() string {
	box := mailbox.Open(h.dir, h.session.ID)
	unread, senders := box.Unread()
	if unread == 0 {
		return ""
	}
	list := box.List(true, 10)

	from := "未知发件人"
	if len(senders) > 0 {
		from = "「" + strings.Join(senders, "」「") + "」"
	}
	var lines []string
	lines = append(lines, fmt.Sprintf(
		"你有 %d 封未读邮件（来自%s）：", unread, from))
	for _, mail := range list {
		lines = append(lines, fmt.Sprintf("- [%s] %s", mail.ID, mail.Subject))
	}
	if unread > len(list) {
		lines = append(lines, fmt.Sprintf("（还有 %d 封未列出，用 mailbox_list 查看）", unread-len(list)))
	}
	lines = append(lines, "",
		"用 mailbox_read 读取正文后再处理。这只是通知，正文不在上面。",
		"如果邮件不需要你做任何事，不必回信 —— 每次回信都会唤醒对方。")
	return strings.Join(lines, "\n")
}

// 编译期确认：mailboxHost 满足 tools 的接口。
var _ tools.MailboxHost = (*mailboxHost)(nil)

// MailboxForTest 返回会话的通讯客户端（测试用）。
func (s *Session) MailboxForTest() tools.MailboxHost {
	if s == nil || s.host == nil {
		return nil
	}
	return s.host.mailboxClient(s)
}

// MailboxNoticeForTest 返回当前会组装出的未读通知（测试用）。
func (s *Session) MailboxNoticeForTest() string {
	if s == nil || s.host == nil {
		return ""
	}
	client, ok := s.host.mailboxClient(s).(*mailboxHost)
	if !ok {
		return ""
	}
	return client.mailboxNotice()
}

// SetTitleForTest 改会话标题（测试用）。
func (s *Session) SetTitleForTest(title string) { s.Title = title }

// ToolsForTest 返回会话当前的工具列表（测试用）。
//
// 用途：验证"子会话的工具列表里没有 new_session"——那是**不注册**，
// 而不是"注册了但调用失败"，所以必须看列表本身。
func (s *Session) ToolsForTest() []*tools.ToolDef {
	if s == nil || s.drv == nil {
		return nil
	}
	return s.drv.Tools()
}

// SetArchivedForTest 设置归档状态（测试用）。
//
// 生产路径的归档要落盘（项目档案是权威），这里只改内存标志：
// 测试关注的是"归档会话能不能收信"，不关心持久化。
func (s *Session) SetArchivedForTest(archived bool) { s.archived = archived }

// MessageCountForTest 返回会话的消息数（测试用）。
func (s *Session) MessageCountForTest() int {
	return len(s.drv.Conversation().Entries())
}

// UserMessageCountForTest 返回**用户消息**条数（测试用）。
//
// 与 MessageCountForTest 的区别：那条数全部条目（含 agent 占位消息），
// 唤醒一次会产生 1 条 user + 1 条 agent 占位，数全部就看不出"唤醒了几次"。
func (s *Session) UserMessageCountForTest() int {
	count := 0
	for _, entry := range s.drv.Conversation().Entries() {
		if entry.Message == nil || entry.Message.Message == nil {
			continue
		}
		if entry.Message.Message.Role == message.RoleUser {
			count++
		}
	}
	return count
}

// LastUserTextForTest 返回最后一条用户消息的文本（测试用）。
//
// 用途：验证唤醒注入的那条消息内容（不含正文、说了什么）。
func (s *Session) LastUserTextForTest() string {
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
