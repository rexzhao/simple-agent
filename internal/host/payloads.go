package host

import (
	"encoding/json"

	"simple-agent/internal/providers"
	"simple-agent/internal/store"
)

// 命令载荷是**跨进程**的（展示端 → 执行端），所以每个字段都必须有 json tag：
// 没有 tag 时 Go 会用字段名（`Hash`、`MediaType`…），非 Go 的展示端（Android 的
// bridge 用 snake_case）按小写发送，解码后就全是空值——附件 hash 会变成空串，
// 消息里只剩 `blob:sha256:`。桌面端的展示端也是 Go，字段名恰好对得上，
// 所以这个坑只在手机端暴露（已经踩过一次）。

// SendPayload 是 "send" 命令的载荷（文字 + 已上传到执行端的图片引用）。
type SendPayload struct {
	Text        string          `json:"text"`
	Attachments []AttachmentRef `json:"attachments,omitempty"`
}

// AttachmentRef 是已经存进 blob 存储的一张图片。
type AttachmentRef struct {
	Hash      string `json:"hash"`
	MediaType string `json:"media_type,omitempty"`
	Name      string `json:"name,omitempty"`
}

// TextPayload 是只带一段文字的命令载荷（enqueue / setEffort / rename）。
type TextPayload struct {
	Text string `json:"text"`
}

// IndexPayload 是带下标的命令载荷（dropQueued）。
type IndexPayload struct {
	Index int `json:"index"`
}

// AnswerPayload 是回答提问的载荷。
// AskAnswer 是某一次提问调用在界面上做出的选择（跨展示端提交用）。
type AskAnswer struct {
	CallID   string            `json:"call_id"`
	Selected map[string][]int  `json:"selected,omitempty"` // questionID → 选项下标
	Custom   map[string]string `json:"custom,omitempty"`   // questionID → 手写答案
}

type AnswerPayload struct {
	Custom string `json:"custom,omitempty"`
	// Answers 非空表示"由展示端作答"：执行端要先把这些选择应用到自己的提问组。
	Answers []AskAnswer `json:"answers,omitempty"`
}

// SelectPayload 是切换模型的载荷。
type SelectPayload struct {
	Selection providers.Selection `json:"selection"`
	Effort    string              `json:"effort,omitempty"`
}

// NewSessionPayload 是新建会话的载荷。
type NewSessionPayload struct {
	Workspace string `json:"workspace,omitempty"`
	Title     string `json:"title,omitempty"`
	// ParentID 非空表示"以这个会话为父建子会话"。
	//
	// 让界面也能建子会话（不只是 new_session 工具）：展示端与执行端走
	// 同一条命令，Android 端将来也能用。空值 = 普通顶层会话。
	ParentID string `json:"parent_id,omitempty"`
	// Prompt 非空时新会话立刻收到这条消息（子会话的初始任务）。
	Prompt string `json:"prompt,omitempty"`
}

// LoadOlderPayload 是 "loadOlder" 命令的参数：展示端自己的翻页游标。
type LoadOlderPayload struct {
	Cursor string `json:"cursor,omitempty"`
}

// BackfillPayload 是 "backfill" 命令的参数：要 **seq < Before** 的更早事件。
//
// 为什么需要它：首次 attach 只下发最近 [attachTailEvents] 条事件（为了让
// 长会话首屏不卡）。展示端在本地镜像里翻页翻到最早时，其实磁盘上还有更多，
// 必须回头向执行端要 —— 否则界面会谎报"已经是最早的内容了"。
type BackfillPayload struct {
	// Before 是本次要取的区间的**上界（不含）**：返回 seq < Before 的事件。
	// 用展示端当前的 firstSeq，天然不重不漏（seq 单调递增且不重置）。
	Before uint64 `json:"before"`
	// Limit 是最多返回多少条（按事件计）。执行端可以再截断。
	Limit int `json:"limit,omitempty"`
}

// BackfillData 是 "backfill" 命令的结果。
//
// 直接传原始 store.Event：展示端用 driver.Mirror.ApplyPersisted 把它注入
// 镜像，与 attach 下发的历史走同一条路径 —— 不必再定义一套平行结构，
// 也不会出现"两个地方各自解析事件、字段理解不一致"的问题。
type BackfillData struct {
	// Events 是读到的那一段（按 seq 升序）。
	Events []store.Event `json:"events"`
	// HasMore 表示 Before 之前是否还有更多（false 表示真的到最早了）。
	HasMore bool `json:"has_more"`
	// OldestSeq 是执行端日志里最早的内容事件 seq（用于判定到底）。
	OldestSeq uint64 `json:"oldest_seq,omitempty"`
}

// NewSessionData 是 "newSession" 命令的结果：新会话的 id。
//
// 命令是异步的，界面不能"提交后立刻查列表"来猜哪个是新会话（Deferred/远程下
// 这时它还不存在）；必须用结果里的 id 去切换。
type NewSessionData struct {
	SessionID string `json:"session_id"`
}

// EncodePayload 是测试与 remote 提交命令时用的编码辅助。
func EncodePayload(value any) ([]byte, error) { return json.Marshal(value) }
