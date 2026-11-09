// Package store 是会话事件日志的持久化层。
//
// 设计要点（docs/persistence-design.md）：
//   - 一行一个事件：`{"v":1,"seq":12,"at":"...","type":"...","data":{...}}`；
//   - `seq` 单调递增，用于检测截断与重排（只在**同一个文件内**连续）；
//   - `session.jsonl`（不带后缀）永远是最新的一份，加载只读它；
//   - 压缩时滚动：把当前内容复制成 `archive-NNNN.jsonl`，再用快照开一个新文件。
//
// 包只负责「事件进、事件出」，不认识 conversation / message 的语义——重放由
// driver 做。这样两种后端共用同一份重放逻辑，也便于各自单测。
package store

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"strings"
	"time"
)

// Version 是日志格式版本。
const Version = 1

// 事件类型（与设计文档 §3.2 一致）。
const (
	TypeSessionCreated  = "session.created"
	TypeMessageAppended = "message.appended"
	TypeMessageFinalize = "message.finalized"
	TypeMessageStatus   = "message.status"
	TypeToolResults     = "tool.results"
	TypeAskRaised       = "ask.raised"
	TypeAskAnswered     = "ask.answered"
	TypeSelectionChange = "selection.changed"
	TypeSessionRenamed  = "session.renamed"
	TypeCompactSnapshot = "compact.snapshot"
	// TypeNotice 记录本地通知（例如「回答被截断」「达到轮数上限」）。
	// 设计文档的事件清单里没有它；加了它是因为通知也会显示在会话里，
	// 不写日志的话重放后界面上会少一条，与"重放 == 在线状态"矛盾。
	TypeNotice = "notice.appended"
	// TypeTodoWrite 记录任务清单的一次整体替换（对齐 dsh 的 `todo/write`）。
	//
	// 为什么必须落盘：清单只存在 `ToolContext` 里的话，重启就没了，而界面上
	// 还挂着旧的一份——重放必须能重建出同样的清单。语义是**整体替换**
	// （last-write-wins），不是增量。
	TypeTodoWrite = "todo.write"
)

// SessionCreatedData 与 SessionRenamedData 由 catalog 读取（driver 那边也有同名类型，
// 这里重新定义一份是为了让 store 不依赖 driver）。
type SessionCreatedData struct {
	ContextID string    `json:"context_id"`
	Title     string    `json:"title,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Workspace 是会话的工作区（侧栏按它分组）；旧日志没有该字段，读出来是空。
	Workspace string `json:"workspace,omitempty"`
}

// SessionRenamedData 是 session.renamed 的载荷。
type SessionRenamedData struct {
	Title string `json:"title"`
}

// Event 是一条日志记录。
type Event struct {
	Version int            `json:"v"`
	Seq     uint64         `json:"seq"`
	At      time.Time      `json:"at"`
	Type    string         `json:"type"`
	Data    jsontext.Value `json:"data,omitempty"`
}

// NewEvent 构造事件（Seq 由 store 分配）。
//
// 非法 UTF-8 不算失败：工具输出可能是 GBK/二进制（中文 Windows 上很常见），
// `json/v2` 会拒绝这种字符串。这里把这种字节换成 U+FFFD 后重试，绝不 panic 掉
// 整个界面线程——见 `SanitizeUTF8` 的注释。
func NewEvent(kind string, data any) (Event, error) {
	raw, err := json.Marshal(data)
	if err != nil && isUTF8Rejection(err) {
		raw, err = json.Marshal(sanitizeValue(data))
	}
	if err != nil {
		return Event{}, fmt.Errorf("编码事件 %s 失败：%w", kind, err)
	}
	return Event{Version: Version, At: time.Now().UTC(), Type: kind, Data: raw}, nil
}

// isUTF8Rejection 判断编码错误是不是"字符串里有非法 UTF-8"。
func isUTF8Rejection(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "invalid UTF-8") || strings.Contains(text, "invalid utf-8")
}

// MustEvent 构造事件，编码失败直接 panic（只用于值必然可编码的调用点）。
func MustEvent(kind string, data any) Event {
	event, err := NewEvent(kind, data)
	if err != nil {
		panic(err)
	}
	return event
}

// Decode 把事件数据解成目标类型。
func (e Event) Decode(target any) error {
	if len(e.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(e.Data, target); err != nil {
		return fmt.Errorf("解析事件 %s 失败：%w", e.Type, err)
	}
	return nil
}

// LoadResult 是一次加载的结果。
type LoadResult struct {
	Events []Event
	// Warnings 记录坏行/序号不连续等被容忍的问题（界面可以提示用户）。
	Warnings []string
	// Truncated 表示文件末尾有半行（上次写入被中断）。
	Truncated bool
}

// ArchiveCursor 指向一份归档文件。
type ArchiveCursor struct {
	// Name 是归档文件名（如 archive-0002.jsonl）。
	Name string
}

// OlderPage 是一次「加载更早历史」的结果。
type OlderPage struct {
	Events []Event
	// Warnings 是归档文件里的坏行等问题（界面按显示型提示给出即可）。
	Warnings []string
	// Next 为 nil 表示没有更早的归档了。
	Next *ArchiveCursor
}

// SessionStore 是会话日志的后端。
type SessionStore interface {
	// Append 追加一个事件（Seq 由实现分配）。
	Append(event Event) error
	// Load 读取最新的一份日志（内存后端返回全部事件）。
	Load() (LoadResult, error)
	// Compact 滚动日志：把当前内容归档，并用 snapshot 开一份新文件。
	Compact(snapshot Event) error
	// LoadOlder 按需加载更早的归档；cursor 为 nil 表示从最近的归档开始。
	LoadOlder(cursor *ArchiveCursor) (OlderPage, error)
	// Sync 把缓冲刷到磁盘（回合结束、滚动、退出时调用）。
	Sync() error
	// Close 关闭底层文件。
	Close() error
}
