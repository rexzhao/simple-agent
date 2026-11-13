package bridge

import (
	"time"

	"simple-agent/internal/message"
)

// 本文件定义**跨进程的视图模型**：Go 把会话整理成这份结构，Kotlin 只负责画。
//
// 设计取舍：字段刻意用 string / int / bool / []T，不用任何 Go 特有的类型
// （interface、map[string]any 之外的嵌套都避免），这样 JSON 稳定、Kotlin 侧
// 的 @Serializable 也简单。语义（分组、标题、失败原因）都已经在 Go 里算好。

// SessionSummary 是侧栏里的一行。
type SessionSummary struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Workspace string `json:"workspace,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

// RuntimeView 是状态条 / 输入卡要用的瞬时状态。
type RuntimeView struct {
	Busy   string   `json:"busy"`
	Queued []string `json:"queued,omitempty"`

	// 用量与上下文
	InputTokens   int `json:"input_tokens,omitempty"`
	OutputTokens  int `json:"output_tokens,omitempty"`
	TokensInUse   int `json:"tokens_in_use,omitempty"`
	ContextWindow int `json:"context_window,omitempty"`
	Turns         int `json:"turns,omitempty"`
	Steps         int `json:"steps,omitempty"`

	// 模型选择
	ProviderID  string `json:"provider_id,omitempty"`
	Model       string `json:"model,omitempty"`
	ModelLabel  string `json:"model_label,omitempty"`
	Effort      string `json:"effort,omitempty"`
	EffortLabel string `json:"effort_label,omitempty"`
	Vision      bool   `json:"vision,omitempty"`

	Compacting bool `json:"compacting,omitempty"`
	CanSend    bool `json:"can_send"`
	CanSteer   bool `json:"can_steer"`

	// AllowShell 来自执行端（展示端不读本地配置）。
	AllowShell bool `json:"allow_shell"`
	// Connected 是展示端到执行端的连接状态。
	Connected bool `json:"connected"`
}

// MessageView 是一条消息（user 或 agent）的词渲染结果。
type MessageView struct {
	ID     string `json:"id"`
	Role   string `json:"role"`   // "user" / "agent"
	Status string `json:"status"` // complete / cancelled / failed / streaming
	Reason string `json:"reason,omitempty"`
	At     int64  `json:"at,omitempty"`
	// Parts 是这条消息里**不属于过程组**的内容（正文、图片等）。
	Parts []PartView `json:"parts,omitempty"`
}

// PartView 是一个内容块的视图。
type PartView struct {
	Kind string `json:"kind"` // markdown / code / image / file / text / json
	// Text 是文本类内容；Code 是代码块内容。
	Text string `json:"text,omitempty"`
	Lang string `json:"lang,omitempty"`
	// Image 非空时按图片渲染。
	Image *ImageView `json:"image,omitempty"`
	Name  string     `json:"name,omitempty"`
	// Blocks 是 markdown 的块结构（Kind=markdown 时给，Kotlin 直接照着排版）。
	Blocks []BlockView `json:"blocks,omitempty"`
}

// BlockView 是一个 markdown 块（段落 / 标题 / 代码块 / 列表 / 引用 / 分隔线 / 表格）。
type BlockView struct {
	Kind    string         `json:"kind"`
	Level   int            `json:"level,omitempty"`
	Lang    string         `json:"lang,omitempty"`
	Code    string         `json:"code,omitempty"`
	Spans   []SpanView     `json:"spans,omitempty"`
	Blocks  []BlockView    `json:"blocks,omitempty"`
	Items   [][]BlockView  `json:"items,omitempty"`
	Ordered bool           `json:"ordered,omitempty"`
	Start   int            `json:"start,omitempty"`
	Header  [][]SpanView   `json:"header,omitempty"`
	Rows    [][][]SpanView `json:"rows,omitempty"`
	Aligns  []string       `json:"aligns,omitempty"`
}

// SpanView 是行内的一段文本（粗体/斜体/代码/链接）。
type SpanView struct {
	Text     string `json:"text"`
	Bold     bool   `json:"bold,omitempty"`
	Italic   bool   `json:"italic,omitempty"`
	Strike   bool   `json:"strike,omitempty"`
	Code     bool   `json:"code,omitempty"`
	Link     string `json:"link,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// ImageView 指向一张图片：Blob 是内容寻址 hash（经 host.Blobs 取字节），
// URL 是远程地址。两者互斥。
type ImageView struct {
	Blob      string `json:"blob,omitempty"`
	URL       string `json:"url,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
}

// ProcessView 是一个折叠的过程组。
type ProcessView struct {
	// Key 是展开状态的稳定 key。
	Key string `json:"key"`
	// Title 是标题（"已完成分析并读取文件" / "正在读取文件 · a.md"）。
	Title string `json:"title"`
	// Activity 是图标分类（thinking / read / commands / …）。
	Activity string `json:"activity"`
	// Streaming 表示这一组还在进行（决定是否显示进行中的动画）。
	Streaming bool `json:"streaming"`
	// StepCount 是这一组有多少步（**折叠时也要知道**：界面显示"思考 · 5 步"）。
	//
	// 有了它，折叠状态下 Steps 可以不传，界面仍然能显示步数。
	StepCount int `json:"step_count"`
	// Detail 表示 Steps 是否已带上。
	//
	// **懒加载的核心**：默认折叠的过程组不带 Steps（思考全文、工具参数与输出
	// 全文往往几十 KB，一屏十几个组就是上 MB）。用户点开时才通过
	// ProcessDetailJSON 单独取这一个组。
	Detail bool `json:"detail"`
	// Steps 是展开后的内容（Detail 为真时才有值）。
	Steps []ProcessStep `json:"steps,omitempty"`
}

// ProcessStep 是过程组里的一项（思考或一次工具调用）。
type ProcessStep struct {
	Kind string `json:"kind"` // reasoning / tool
	// Text 是思考内容（kind=reasoning）。
	Text string `json:"text,omitempty"`
	// 工具调用
	ToolID     string `json:"tool_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolTitle  string `json:"tool_title,omitempty"`
	Args       string `json:"args,omitempty"`
	Output     string `json:"output,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	Pending    bool   `json:"pending,omitempty"`
	Done       bool   `json:"done,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// NoticeView 是一条本地提示。
type NoticeView struct {
	ID    string `json:"id,omitempty"`
	Level string `json:"level"` // info / warn / error
	Text  string `json:"text"`
}

// AskView 是一个待回答的提问（面板要画的内容）。
type AskView struct {
	MessageID string         `json:"message_id"`
	CallID    string         `json:"call_id"`
	Questions []QuestionView `json:"questions"`
	// Answered 表示这一组里每题都已作答（按钮可以点亮）。
	Answered bool `json:"answered,omitempty"`
}

// QuestionView 是提问里的一道题。
//
// Selected / Custom 是**当前的选择状态**：它存在 Go 的 AskGroup 里（单选互斥、
// 写文字就取消选项这些规则都在 driver 里），Kotlin 只按它画勾。
type QuestionView struct {
	ID          string   `json:"id"`
	Question    string   `json:"question"`
	Header      string   `json:"header,omitempty"`
	MultiSelect bool     `json:"multi_select,omitempty"`
	Options     []string `json:"options,omitempty"`
	// Selected 是已选中的选项下标。
	Selected []int `json:"selected,omitempty"`
	// Custom 是用户手写的答案（可能还没生效——见 CustomActive）。
	Custom string `json:"custom,omitempty"`
	// CustomActive 表示这一题此刻以手写文字作答（此时 Selected 必为空）。
	CustomActive bool `json:"custom_active,omitempty"`
	// Answered 表示这一题已作答。
	Answered bool `json:"answered,omitempty"`
}

// ArchiveView 是一页「更早的历史」。
type ArchiveView struct {
	// Entries 按时间先后排列（界面直接插到列表顶部之后）。
	Entries []ArchivedMessage `json:"entries,omitempty"`
	// Next 是下一页游标；空表示没有更早的了。
	Next string `json:"next,omitempty"`
	More bool   `json:"more,omitempty"`
	// Error 非空表示这一页没取到（归档损坏等）。
	Error string `json:"error,omitempty"`
}

// ArchivedMessage 是归档里的一条消息（只用于显示）。
type ArchivedMessage struct {
	ID     string     `json:"id"`
	Role   string     `json:"role"`
	Status string     `json:"status"`
	Reason string     `json:"reason,omitempty"`
	At     int64      `json:"at,omitempty"`
	Parts  []PartView `json:"parts,omitempty"`
}

// TimelineView 是一个会话的完整可渲染内容。
type TimelineView struct {
	SessionID string     `json:"session_id"`
	Title     string     `json:"title,omitempty"`
	Items     []ItemView `json:"items,omitempty"`
	// Todos 是任务清单（可能在正文之外单独显示）。
	Todos []TodoView `json:"todos,omitempty"`
	// Asks 是待回答的提问（非空时界面要弹面板）。
	Asks    []AskView   `json:"asks,omitempty"`
	Runtime RuntimeView `json:"runtime"`
	// Notices 是本次同步里新增的提示（增量，按 id 去重）。
	Notices []NoticeView `json:"notices,omitempty"`
}

// ItemView 是时间线上的一行（正文 / 过程组 / 提示 / 回合结束行）。
type ItemView struct {
	Kind string `json:"kind"` // content / process / notice / footer
	Key  string `json:"key"`
	// content
	Message *MessageView `json:"message,omitempty"`
	// process
	Process *ProcessView `json:"process,omitempty"`
	// notice
	Notice *NoticeView `json:"notice,omitempty"`
	// footer
	Footer *FooterView `json:"footer,omitempty"`
}

// FooterView 是回合结束行。
type FooterView struct {
	Status string `json:"status"` // complete / cancelled / failed
	Reason string `json:"reason,omitempty"`
	// DurationMS 是回合耗时（0 表示未知或不足 1 秒）。
	DurationMS int64 `json:"duration_ms,omitempty"`
	Tokens     int   `json:"tokens,omitempty"`
}

// TodoView 是任务清单一项。
type TodoView struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// ---- 内部辅助 ----

// roleOf 把 message.Role 转成小写字符串（Kotlin 侧比较方便）。
func roleOf(role message.Role) string {
	if role == message.RoleUser {
		return "user"
	}
	return "agent"
}

// unixMillis 把时间转成毫秒（0 表示零值）。
func unixMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
