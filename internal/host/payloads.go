package host

import (
	"encoding/json"

	"simple-agent/internal/providers"
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
}

// LoadOlderPayload 是 "loadOlder" 命令的参数：展示端自己的翻页游标。
type LoadOlderPayload struct {
	Cursor string `json:"cursor,omitempty"`
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
