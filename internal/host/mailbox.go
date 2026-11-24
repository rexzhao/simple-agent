package host

import (
	"fmt"
	"strings"

	"simple-agent/internal/mailbox"
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

	h.local.notifyMail(target)
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

// notifyMail 在收到新邮件时通知收件会话。
//
// 这里**只发一个界面通知 + 记未读**，不碰对话历史。真正把"你有未读邮件"
// 送进模型的是 driver 的 MailboxNotice 钩子（在组装请求时注入一条 user
// 消息，不落盘）。两者配合的结果：
//   - 收件会话空闲时：徽标 +1；等它下一次被唤醒或用户发消息时，模型才看到通知
//   - 收件会话运行中：不打断，下一个请求自动带上通知
func (l *Local) notifyMail(target *Session) {
	if target == nil {
		return
	}
	l.emit(Change{SessionID: target.ID, Kind: "mail"})
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

// SetArchivedForTest 设置归档状态（测试用）。
//
// 生产路径的归档要落盘（项目档案是权威），这里只改内存标志：
// 测试关注的是"归档会话能不能收信"，不关心持久化。
func (s *Session) SetArchivedForTest(archived bool) { s.archived = archived }
