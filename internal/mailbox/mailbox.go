// Package mailbox 实现**项目内**的会话通讯（agent 之间的邮件）。
//
// 设计取舍（与 pi-mailbox 的差异，都是刻意的）：
//   - 没有守护进程：我们的执行端本来就一个进程管所有会话，不需要跨进程投递
//   - 没有命名邮箱/多登录：会话 ID 本身就是唯一身份，不存在抢占与归属问题
//   - 不跨项目、不跨机器：按项目隔离，邮件存在项目目录下与会话日志同源
//   - 邮件不可编辑、不可删除：只追加（已读状态也是追加一个新事件）
//
// 存储：每个**收件会话**一个 JSONL：
//
//	<data_dir>/projects/<projectID>/mailbox/<targetSessionID>.jsonl
//
// 为什么按收件人分文件：读某人收件箱只需读一个文件；删除会话时删掉它的
// 文件即可，不牵连别人。全局一个文件则每次都要扫描并过滤。
package mailbox

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"simple-agent/internal/store"
)

// 事件类型（JSONL 里 Type 字段的值）。
const (
	// TypeSent 是一封新邮件（对外导出：投递层要靠它判断"这封是否已通知过"）。
	TypeSent = "mail.sent"
	// TypeRead 标记某封邮件已读（后写的覆盖先写的）。
	TypeRead = "mail.read"
)

// 大小上限：与请求体量保持同一量级，避免一封邮件把上下文撑爆。
const (
	// MaxSubject 是主题字数上限。
	MaxSubject = 200
	// MaxBody 是正文字节上限。
	MaxBody = 64 << 10 // 64 KiB
	// maxRefs 是一封邮件最多引用几封旧邮件。
	maxRefs = 16
)

// ErrNotFound 表示邮件不存在。
var ErrNotFound = errors.New("未找到")

// Mail 是一封邮件。
type Mail struct {
	// ID 是邮件标识（在本收件箱内唯一）。
	ID string `json:"id"`
	// From 是发件会话 ID，FromTitle 是发件会话标题（显示用）。
	//
	// 存两份的原因：标题会改，但当时发信人叫什么对收件人是重要上下文；
	// 而且发件人可能已经从项目里删掉，之后就查不到标题了。
	From      string `json:"from"`
	FromTitle string `json:"from_title,omitempty"`
	// To 是收件会话 ID（与文件名一致，便于校验）。
	To string `json:"to"`
	// Subject 与 Body 是内容。
	Subject string `json:"subject"`
	Body    string `json:"body"`
	// Refs 引用更早的邮件（"回复"就是引用原邮件）。
	Refs []string `json:"refs,omitempty"`
	// At 是发送时间。
	At time.Time `json:"at"`
	// Read 表示已读（由 TypeRead 事件覆盖而来）。
	Read bool `json:"-"`
}

// Box 是一个收件会话的邮箱。
//
// 并发：写入来自工具 goroutine，读取来自界面线程与请求组装，所以内部加锁。
type Box struct {
	mu     sync.Mutex
	dir    string
	owner  string // 收件会话 ID
	mails  []Mail // 按写入顺序（旧→新）
	loaded bool
}

// Open 打开某个收件会话的邮箱（不读盘，首次访问时懒加载）。
//
// targetID 是收件会话 ID；dir 是项目下的 mailbox 目录。
func Open(dir, targetID string) *Box {
	return &Box{dir: dir, owner: targetID}
}

// path 返回某收件会话的邮箱文件路径。
func (b *Box) path() string {
	return filepath.Join(b.dir, safeName(b.owner)+".jsonl")
}

// ensureLoaded 首次访问时把事件重放成邮件列表。
//
// 懒加载的理由：绝大多数会话从未收过信，每次构造都 stat + read 一遍文件
// 是白费；而 buildRequest 每轮都会碰它一次。
//
// 调用方必须已持有 b.mu（或确认无并发）。
func (b *Box) ensureLoadedLocked() error {
	if b.loaded {
		return nil
	}
	b.loaded = true
	file := b.path()
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil // 从没收到过信
	}
	if err != nil {
		return fmt.Errorf("读取邮箱失败：%w", err)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev store.Event
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue // 坏行跳过：一封读不出来不该让整个邮箱不可用
		}
		switch ev.Type {
		case TypeSent:
			var mail Mail
			if ev.Decode(&mail) != nil {
				continue
			}
			if mail.To == "" {
				mail.To = b.owner
			}
			b.mails = append(b.mails, mail)
		case TypeRead:
			var data struct {
				ID string `json:"id"`
			}
			if ev.Decode(&data) != nil {
				continue
			}
			for index := range b.mails {
				if b.mails[index].ID == data.ID {
					b.mails[index].Read = true
				}
			}
		}
	}
	return nil
}

// safeName 把会话 ID 变成安全文件名。
//
// 会话 ID 是十六进制字符串，本来就安全；这里仍过滤一遍，防止未来 ID 规则
// 变化引入路径分隔符（宁可多一层）。
func safeName(id string) string {
	var out strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out.WriteRune(r)
		default:
			out.WriteByte('_')
		}
	}
	if out.Len() == 0 {
		return "_"
	}
	return out.String()
}

// NewID 生成邮件 ID。
//
// 短、可读、在收件箱内唯一即可：它只用于 mailbox_read 与 refs 引用，
// 不需要全局唯一（跨收件箱的同号邮件互不相干）。
func NewID(at time.Time) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	n := uint64(at.UnixNano())
	var buf [7]byte
	for i := range buf {
		buf[i] = alphabet[n%uint64(len(alphabet))]
		n /= uint64(len(alphabet))
	}
	return string(buf[:])
}

// Add 追加一封邮件并落盘，返回它的 ID。
//
// 只追加不改写：已读状态也是新事件，重放时后写覆盖先写。
func (b *Box) Add(mail Mail) (Mail, error) {
	// 先补全收件人再校验：调用方只关心"发给谁的信"，To 由邮箱自己填，
	// 不该让它传（传错还会和文件名对不上）。
	mail.To = b.owner
	if err := validate(mail); err != nil {
		return Mail{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureLoadedLocked(); err != nil {
		return Mail{}, err
	}

	if mail.At.IsZero() {
		mail.At = time.Now()
	}
	// ID 在**本邮箱内**唯一即可，但要防撞（同纳秒两次发送理论上可能）。
	for mail.ID == "" || b.hasIDLocked(mail.ID) {
		mail.ID = NewID(mail.At.Add(time.Duration(len(b.mails)) * time.Microsecond))
	}

	if err := b.appendEventLocked(TypeSent, mail); err != nil {
		return Mail{}, err
	}
	b.mails = append(b.mails, mail)
	return mail, nil
}

// hasIDLocked 报告邮箱里是否已有这个 ID。调用方须持有锁。
func (b *Box) hasIDLocked(id string) bool {
	for _, mail := range b.mails {
		if mail.ID == id {
			return true
		}
	}
	return false
}

// appendEventLocked 往邮箱文件追加一条事件。调用方须持有锁。
func (b *Box) appendEventLocked(kind string, data any) error {
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		return fmt.Errorf("创建邮箱目录失败：%w", err)
	}
	file, err := os.OpenFile(b.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("打开邮箱文件失败：%w", err)
	}
	defer file.Close()
	event, err := store.NewEvent(kind, data)
	if err != nil {
		return err
	}
	return store.AppendEvent(file, event)
}

// MarkRead 标记某封邮件已读。
func (b *Box) MarkRead(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureLoadedLocked(); err != nil {
		return err
	}
	for index := range b.mails {
		if b.mails[index].ID != id {
			continue
		}
		if b.mails[index].Read {
			return nil // 已经读过：不重复追加事件
		}
		if err := b.appendEventLocked(TypeRead, map[string]string{"id": id}); err != nil {
			return err
		}
		b.mails[index].Read = true
		return nil
	}
	return ErrNotFound
}

// List 返回邮件（新→旧）。unreadOnly 只看未读；limit<=0 表示不限制。
func (b *Box) List(unreadOnly bool, limit int) []Mail {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureLoadedLocked(); err != nil {
		return nil
	}
	out := make([]Mail, 0, len(b.mails))
	for index := len(b.mails) - 1; index >= 0; index-- {
		mail := b.mails[index]
		if unreadOnly && mail.Read {
			continue
		}
		out = append(out, mail)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Get 按 ID 取一封邮件。
func (b *Box) Get(id string) (Mail, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureLoadedLocked(); err != nil {
		return Mail{}, err
	}
	for _, mail := range b.mails {
		if mail.ID == id {
			return mail, nil
		}
	}
	return Mail{}, ErrNotFound
}

// Unread 返回未读数与未读邮件的发件人标题（去重，保持出现顺序）。
//
// 一起返回的理由：投递层两个都要（数字给徽标/通知正文，发件人给"来自「X」"），
// 分两次调用就要遍历两遍。
func (b *Box) Unread() (int, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureLoadedLocked(); err != nil {
		return 0, nil
	}
	count := 0
	seen := map[string]bool{}
	var senders []string
	for _, mail := range b.mails {
		if mail.Read {
			continue
		}
		count++
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
	return count, senders
}

// Total 返回邮件总数。
func (b *Box) Total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureLoadedLocked(); err != nil {
		return 0
	}
	return len(b.mails)
}

// validate 检查一封邮件是否合法。
func validate(mail Mail) error {
	if strings.TrimSpace(mail.Subject) == "" {
		return fmt.Errorf("主题不能为空")
	}
	if len([]rune(mail.Subject)) > MaxSubject {
		return fmt.Errorf("主题过长（最多 %d 字）", MaxSubject)
	}
	if len(mail.Body) > MaxBody {
		return fmt.Errorf("正文过长（最多 %d 字节）", MaxBody)
	}
	if len(mail.Refs) > maxRefs {
		return fmt.Errorf("引用过多（最多 %d 条）", maxRefs)
	}
	if strings.TrimSpace(mail.To) == "" {
		return fmt.Errorf("收件人不能为空")
	}
	return nil
}
