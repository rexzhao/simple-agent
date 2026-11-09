// Package conversation 是会话的本地状态层（不依赖 UI）。
//
// 与 wire 层的分工：
//   - message.Message 是协议数据，会发给模型/服务端；
//   - 本包的时间戳、流式状态、本地通知**不上 wire**。
//
// 不变量（均有测试覆盖，且尽量由类型/封装保证而非调用方自觉）：
//   - 消息 ID 唯一且非空（列表 key、流定位都依赖它）；
//   - 流式增量只并入 streamingPart；AddPart（附件、工具卡片）会清空该目标，
//     后续增量另起文本 part，不会污染附件；
//   - 状态只能通过本包的方法修改（status 不导出）；
//   - entries 私有，Entries() 返回副本；
//   - contextID 创建后不可变（异步回调按它定位会话）。
package conversation

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"simple-agent/internal/message"
)

// EntryStatus 是条目的本地生命周期状态。
type EntryStatus string

const (
	// StatusPending 表示流式已开启但还没收到内容。
	StatusPending EntryStatus = "pending"
	// StatusStreaming 表示正在接收增量。
	StatusStreaming EntryStatus = "streaming"
	// StatusComplete 表示正常结束。
	StatusComplete EntryStatus = "complete"
	// StatusCancelled 表示用户主动停止（不是错误，已收内容保留）。
	StatusCancelled EntryStatus = "cancelled"
	// StatusFailed 表示失败。
	StatusFailed EntryStatus = "failed"
)

// IsStreaming 报告条目是否仍处于可追加的流式状态。
func (s EntryStatus) IsStreaming() bool {
	return s == StatusPending || s == StatusStreaming
}

// IsTerminal 报告条目是否已进入终态（不能再追加内容）。
//
// 含 Failed：失败同样是终态，M5 落盘 finalize 事件时按它判断。
func (s EntryStatus) IsTerminal() bool {
	return s == StatusComplete || s == StatusCancelled || s == StatusFailed
}

// InHistory 报告该状态的消息是否进入模型上下文（只有正常完成的算）。
func (s EntryStatus) InHistory() bool { return s == StatusComplete }

// NoticeLevel 是本地通知级别。
type NoticeLevel string

const (
	NoticeInfo  NoticeLevel = "info"
	NoticeWarn  NoticeLevel = "warn"
	NoticeError NoticeLevel = "error"
)

// Notice 是本地系统提示（非协议消息）：连接状态、日志损坏修复提示等。
type Notice struct {
	ID        string
	Level     NoticeLevel
	Text      string
	CreatedAt time.Time
}

// ChatEntry 是一条协议消息 + 本地状态。
//
// status 不导出：只能通过 Conversation 的方法修改，避免绕过状态机校验
// （G6 纪律：正确性不靠调用方自觉）。
type ChatEntry struct {
	Message       *message.Message
	CreatedAt     time.Time
	status        EntryStatus
	failureReason string
	// finishedAt 是这条消息**完成**的时间（finalize/cancel/fail 时打点）。
	// 回合耗时 = 最后一条消息的 finishedAt − 回合前那条 user 消息的 CreatedAt，
	// 所以内存与重放（用 message.finalized 事件的 at）走同一套计算。
	finishedAt time.Time
	// tokens 是这条消息的输出 token 数（重放时为 0：事件里没有 usage）。
	tokens int
	// streamingPart 是流式增量合并的目标 part 下标；-1 表示应新建。
	streamingPart int
}

// FinishedAt 返回这条消息完成的时间（零值表示未知）。
func (e *ChatEntry) FinishedAt() time.Time { return e.finishedAt }

// SetFinishedAt 记录这条消息完成的时间。
func (e *ChatEntry) SetFinishedAt(t time.Time) { e.finishedAt = t }

// Tokens 返回这一轮输出的 token 数（0 表示未知）。
func (e *ChatEntry) Tokens() int { return e.tokens }

// SetTokens 记录这一轮输出的 token 数。
func (e *ChatEntry) SetTokens(n int) { e.tokens = n }

// Status 返回条目状态。
func (e *ChatEntry) Status() EntryStatus { return e.status }

// FailureReason 返回失败原因（仅 Status()==StatusFailed 时有值）。
func (e *ChatEntry) FailureReason() string { return e.failureReason }

// PartCount 返回 part 数量。
func (e *ChatEntry) PartCount() int { return len(e.Message.Parts) }

// PreviewText 返回有损文本预览。
func (e *ChatEntry) PreviewText() string { return e.Message.PreviewText() }

// appendText 把增量并入 streamingPart；没有目标时新建文本 part。
//
// 不导出：调用方必须走 Conversation.AppendToStream（带状态校验）。
// 不并入“恰好位于末尾”的任意文本 part——那可能是刚 AddPart 的附件。
func (e *ChatEntry) appendText(delta string) {
	if e.streamingPart >= 0 && e.streamingPart < len(e.Message.Parts) {
		if text, ok := e.Message.Parts[e.streamingPart].AsText(); ok {
			e.Message.Parts[e.streamingPart].Content = message.TextPart{Text: text + delta}
			return
		}
	}
	e.Message.Parts = append(e.Message.Parts, message.NewTextPart(delta))
	e.streamingPart = len(e.Message.Parts) - 1
}

// AddPart 追加一个 part（附件、工具调用/结果等），并清空流式合并目标。
func (e *ChatEntry) AddPart(p message.Part) int {
	e.streamingPart = -1
	e.Message.Parts = append(e.Message.Parts, p)
	return len(e.Message.Parts) - 1
}

// Entry 是会话列表的一项：消息或本地通知。
type Entry struct {
	Message *ChatEntry
	Notice  *Notice
}

// ID 返回稳定标识（列表 key 用）。
func (e Entry) ID() string {
	if e.Message != nil {
		return e.Message.Message.MessageID
	}
	if e.Notice != nil {
		return e.Notice.ID
	}
	return ""
}

// CreatedAt 返回创建时间。
func (e Entry) CreatedAt() time.Time {
	if e.Message != nil {
		return e.Message.CreatedAt
	}
	if e.Notice != nil {
		return e.Notice.CreatedAt
	}
	return time.Time{}
}

// Conversation 是一个会话（对应 A2A 的 context）。
type Conversation struct {
	contextID string
	entries   []Entry
	// ids 是 ID 索引：重放长会话时避免 O(n²) 查重。
	ids map[string]struct{}
	// todos 是当前任务清单（对齐 dsh 的 `todos` 投影语义）。
	//
	// 存在会话上而不是 ToolContext 里：它要能被重放重建、能被展示端通过快照看到，
	// 而 ToolContext 是执行端进程内的临时状态。
	todos []TodoItem
}

// TodoItem 是任务清单里的一条（字段与 dsh 的 TodoItem 一致）。
//
// 刻意不 import tools：conversation 是更底层的包，而且工具层的 TodoItem
// 是给模型看的参数结构，两边的演进节奏不同（这里多了「写进日志/跨进程传输」
// 的约束）。
type TodoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// TodoEmpty / TodoInProgress / TodoCompleted 是清单项的状态取值（与工具层一致）。
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"
)

// New 创建会话。contextID 创建后不可变。
func New(contextID string) *Conversation {
	return &Conversation{contextID: contextID, ids: make(map[string]struct{})}
}

// Todos 返回当前任务清单的副本（空清单返回 nil）。
func (c *Conversation) Todos() []TodoItem {
	if len(c.todos) == 0 {
		return nil
	}
	return slices.Clone(c.todos)
}

// SetTodos 整体替换任务清单（对应 dsh 的 `todo/write`：last-write-wins）。
//
// 传 nil 或空切片就是清空——用户发下一条消息时用它（对齐 dsh 在 `turn/start`
// 清空的语义）。
func (c *Conversation) SetTodos(todos []TodoItem) {
	if len(todos) == 0 {
		c.todos = nil
		return
	}
	c.todos = slices.Clone(todos)
}

// ContextID 返回会话身份。
func (c *Conversation) ContextID() string { return c.contextID }

// Entries 返回条目副本（顺序即展示顺序；勿按 CreatedAt 排序，墙钟可能回拨）。
func (c *Conversation) Entries() []Entry { return slices.Clone(c.entries) }

var (
	// ErrDuplicateID 表示消息/通知 ID 已存在。
	ErrDuplicateID = errors.New("duplicate id")
	// ErrEmptyID 表示 ID 为空。
	ErrEmptyID = errors.New("empty id")
	// ErrMessageNotFound 表示找不到指定消息。
	ErrMessageNotFound = errors.New("message not found")
	// ErrNotStreaming 表示目标消息不处于流式状态。
	ErrNotStreaming = errors.New("message is not streaming")
	// ErrPartNotFound 表示 part 下标越界。
	ErrPartNotFound = errors.New("part not found")
	// ErrNotTextPart 表示目标 part 不是文本。
	ErrNotTextPart = errors.New("part is not text")
	// ErrNotFailed 表示目标消息不是失败状态。
	ErrNotFailed = errors.New("message is not failed")
)

// AddMessage 追加一条消息（状态由调用方指定）。
//
// Parts 会被克隆：调用方之后改动自己手里的切片不会影响会话内容。
func (c *Conversation) AddMessage(msg message.Message, status EntryStatus) (*ChatEntry, error) {
	if msg.MessageID == "" {
		return nil, ErrEmptyID
	}
	if _, ok := c.ids[msg.MessageID]; ok {
		return nil, fmt.Errorf("%w: %s", ErrDuplicateID, msg.MessageID)
	}
	msg.Parts = slices.Clone(msg.Parts)
	entry := &ChatEntry{
		Message:       &msg,
		CreatedAt:     time.Now(),
		status:        status,
		streamingPart: -1,
	}
	c.entries = append(c.entries, Entry{Message: entry})
	c.ids[msg.MessageID] = struct{}{}
	return entry, nil
}

// SetStreamingParts 用给定 parts 覆盖消息内容，并把状态设回 streaming。
//
// 用途：展示端中途连上（或重连）时，日志里只有一条 streaming 空壳，而重放收尾
// 会把它判成「已中断」。`state.snapshot` 带着这条消息的当前完整 parts，这里整体
// 覆盖并恢复 streaming 状态，之后再接 seq 更大的增量（见设计 §3.3.1）。
func (c *Conversation) SetStreamingParts(id string, parts []message.Part) error {
	entry, ok := c.Message(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrMessageNotFound, id)
	}
	entry.Message.Parts = slices.Clone(parts)
	entry.status = StatusStreaming
	entry.streamingPart = -1
	return nil
}

// InsertNoticeAfter 在指定条目之后插入一条通知（afterID 找不到时追加到末尾）。
//
// 用途：展示端合并快照里的显示型提示时，要放回它**发生的位置**，不能一律追加到
// 末尾——否则后连上来的端看到的顺序与执行端不同。
func (c *Conversation) InsertNoticeAfter(level NoticeLevel, text, afterID string) (*Notice, error) {
	notice, err := c.AddNotice(level, text)
	if err != nil {
		return nil, err
	}
	if afterID == "" {
		// 发生在最前面：插到第一条之前
		c.entries = append(c.entries[:0], append([]Entry{{Notice: notice}}, c.entries[:len(c.entries)-1]...)...)
		return notice, nil
	}
	index := -1
	for i, entry := range c.entries {
		if entry.ID() == afterID {
			index = i
			break
		}
	}
	if index < 0 {
		return notice, nil // 锚点找不到：保持在末尾
	}
	last := len(c.entries) - 1
	copy(c.entries[index+2:], c.entries[index+1:last])
	c.entries[index+1] = Entry{Notice: notice}
	return notice, nil
}

// AddNotice 追加一条本地通知；ID 自动生成。
func (c *Conversation) AddNotice(level NoticeLevel, text string) (*Notice, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	notice := &Notice{ID: id, Level: level, Text: text, CreatedAt: time.Now()}
	c.entries = append(c.entries, Entry{Notice: notice})
	c.ids[id] = struct{}{}
	return notice, nil
}

// NewMessageID 生成一个新的消息 id。
//
// 消息 id 由调用方（driver）在构造消息时填入：Conversation 不替调用方编 id，
// 因为它需要的是「id 唯一」这一不变式，而不是「id 从哪来」。
func NewMessageID() (string, error) { return newID() }

func newID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成 ID 失败：%w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// Message 按 ID 查找消息条目（从后往前：活跃流几乎都在末尾）。
func (c *Conversation) Message(id string) (*ChatEntry, bool) {
	if _, ok := c.ids[id]; !ok {
		return nil, false
	}
	for i := len(c.entries) - 1; i >= 0; i-- {
		if e := c.entries[i].Message; e != nil && e.Message.MessageID == id {
			return e, true
		}
	}
	return nil, false
}

// LastMessage 返回最后一条消息条目（跳过通知）。
func (c *Conversation) LastMessage() (*ChatEntry, bool) {
	for i := len(c.entries) - 1; i >= 0; i-- {
		if e := c.entries[i].Message; e != nil {
			return e, true
		}
	}
	return nil, false
}

// MarkStreaming 把 Pending 推进到 Streaming（不新增 part）。
//
// 用于"收到第一段推理"这类情况：状态要变，但没有正文增量可追加。
// 之前用 AppendToStream(id, "") 代替，会留下一个空的文本 part——
// 那个空 part 会把界面上的过程组错误地打断。
func (c *Conversation) MarkStreaming(id string) error {
	entry, ok := c.Message(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrMessageNotFound, id)
	}
	if !entry.status.IsStreaming() {
		return fmt.Errorf("%w: %s", ErrNotStreaming, id)
	}
	if entry.status == StatusPending {
		entry.status = StatusStreaming
	}
	return nil
}

// AppendToStream 把增量并入指定消息；非流式或不存在时返回错误。
func (c *Conversation) AppendToStream(id, delta string) error {
	entry, ok := c.Message(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrMessageNotFound, id)
	}
	if !entry.status.IsStreaming() {
		return fmt.Errorf("%w: %s", ErrNotStreaming, id)
	}
	if entry.status == StatusPending {
		entry.status = StatusStreaming
	}
	entry.appendText(delta)
	return nil
}

// AppendToPart 把增量并入指定下标的文本 part。
//
// 与 AppendToStream 的区别：它**不碰** streamingPart，因此正文与推理可以交替
// 流入而不互相串（推理先显示、正文随后，二者都是累加）。只允许流式状态下调用。
func (c *Conversation) AppendToPart(id string, partIndex int, delta string) error {
	entry, ok := c.Message(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrMessageNotFound, id)
	}
	if !entry.status.IsStreaming() {
		return fmt.Errorf("%w: %s", ErrNotStreaming, id)
	}
	if partIndex < 0 || partIndex >= len(entry.Message.Parts) {
		return fmt.Errorf("%w: %d", ErrPartNotFound, partIndex)
	}
	text, ok := entry.Message.Parts[partIndex].AsText()
	if !ok {
		return fmt.Errorf("%w: %d", ErrNotTextPart, partIndex)
	}
	entry.Message.Parts[partIndex].Content = message.TextPart{Text: text + delta}
	return nil
}

// UpdatePart 原位替换某个 part（流式过程中工具调用的参数就是这样补全的）。
func (c *Conversation) UpdatePart(id string, partIndex int, part message.Part) error {
	entry, ok := c.Message(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrMessageNotFound, id)
	}
	if partIndex < 0 || partIndex >= len(entry.Message.Parts) {
		return fmt.Errorf("%w: %d", ErrPartNotFound, partIndex)
	}
	entry.Message.Parts[partIndex] = part
	return nil
}

// ReplaceContent 用最终内容替换一条已存在的消息（重放 message.finalized 用）。
//
// 保留条目本身的创建时间与顺序：重放出来的界面上，「已中断」的占位与最终消息
// 是同一条，不能变成两条。
func (c *Conversation) ReplaceContent(id string, msg message.Message, status EntryStatus, reason string) error {
	entry, ok := c.Message(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrMessageNotFound, id)
	}
	entry.Message.Parts = slices.Clone(msg.Parts)
	if msg.Role != "" {
		entry.Message.Role = msg.Role
	}
	entry.status = status
	entry.failureReason = reason
	entry.streamingPart = -1
	return nil
}

// SetFailureReason 给已经是 failed 的条目补上原因。
//
// 重放时消息是「一次性带着最终状态」写入的，AddMessage 只带状态码，
// 原因由这个入口补上（与流式路径的 FailStream 分开，避免放宽状态机）。
func (c *Conversation) SetFailureReason(id, reason string) error {
	entry, ok := c.Message(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrMessageNotFound, id)
	}
	if entry.status != StatusFailed {
		return fmt.Errorf("%w: %s", ErrNotFailed, id)
	}
	entry.failureReason = reason
	return nil
}

// EndStream 正常结束。
func (c *Conversation) EndStream(id string) error { return c.finish(id, StatusComplete, "") }

// CancelStream 用户主动停止。
func (c *Conversation) CancelStream(id string) error { return c.finish(id, StatusCancelled, "") }

// FailStream 失败结束。
func (c *Conversation) FailStream(id, reason string) error {
	return c.finish(id, StatusFailed, reason)
}

func (c *Conversation) finish(id string, status EntryStatus, reason string) error {
	entry, ok := c.Message(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrMessageNotFound, id)
	}
	if !entry.status.IsStreaming() {
		return fmt.Errorf("%w: %s", ErrNotStreaming, id)
	}
	entry.status = status
	entry.failureReason = reason
	return nil
}

// History 返回可发给模型的消息（只含正常完成的条目）。
//
// Cancelled/Failed 的半截回复**不进历史**：它们是本地状态，不是有效上下文。
func (c *Conversation) History() []message.Message {
	out := make([]message.Message, 0, len(c.entries))
	for _, e := range c.entries {
		if e.Message != nil && e.Message.status.InHistory() {
			out = append(out, *e.Message.Message)
		}
	}
	return out
}
