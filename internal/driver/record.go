package driver

import (
	"time"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// 本文件定义事件的 data 载荷，以及「把状态变化翻译成事件」的构造器。
//
// 事件由 **driver 产生**（Effect 里的 Record），不是 uiapp 根据状态变化自己推断。
// 原因：任何「必须靠 UI 自觉调用」的持久化都会漏（attach_stream / dispatch 那几次
// 已经吃过教训）。重放消费的是同一套事件类型，用同一组公开方法重建状态，
// 所以「重放 == 在线状态」可以被直接测出来。

// SessionCreatedData 是 session.created 的载荷。
type SessionCreatedData struct {
	ContextID string    `json:"context_id"`
	Title     string    `json:"title,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Workspace 是这条会话使用的工具工作区（侧栏按它分组）。
	// 可选：旧日志没有这个字段 → 空 → 归到「默认」项目。
	Workspace string `json:"workspace,omitempty"`
}

// MessageAppendedData 是 message.appended 的载荷。
type MessageAppendedData struct {
	Message message.Message          `json:"message"`
	Status  conversation.EntryStatus `json:"status"`
	Reason  string                   `json:"reason,omitempty"`
}

// MessageFinalizedData 是 message.finalized 的载荷：携带最终的完整 parts。
//
// 与 message.appended 的分工：开始流时写 appended(status=streaming)，结束时写
// finalized。若只有 appended 而没有 finalized，说明进程在生成回复时崩了——
// 重放要显示「已中断」占位（用户明确要求的行为），而不是当作什么都没发生。
type MessageFinalizedData struct {
	Message message.Message          `json:"message"`
	Status  conversation.EntryStatus `json:"status"`
	Reason  string                   `json:"reason,omitempty"`
}

// MessageStatusData 是 message.status 的载荷。
type MessageStatusData struct {
	MessageID string                   `json:"message_id"`
	Status    conversation.EntryStatus `json:"status"`
	Reason    string                   `json:"reason,omitempty"`
}

// ToolResultRecord 是工具结果在日志里的形态（与 ToolOutcome 同构）。
type ToolResultRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Output  string `json:"output"`
	IsError bool   `json:"is_error"`
	// Images 是工具返回的图片引用（read_image）；旧日志没有该字段。
	Images    []tools.ToolImageRef `json:"images,omitempty"`
	Pending   bool                 `json:"pending,omitempty"`
	Questions []tools.QuestionSpec `json:"questions,omitempty"`
}

// ToolResultsData 是 tool.results 的载荷。
type ToolResultsData struct {
	MessageID string             `json:"message_id"`
	Results   []ToolResultRecord `json:"results"`
}

// AskRaisedData 是 ask.raised 的载荷。
type AskRaisedData struct {
	MessageID string               `json:"message_id"`
	CallID    string               `json:"call_id"`
	Questions []tools.QuestionSpec `json:"questions"`
}

// AskAnsweredData 是 ask.answered 的载荷。
type AskAnsweredData struct {
	MessageID string `json:"message_id"`
	CallID    string `json:"call_id"`
	Output    string `json:"output"`
}

// SelectionChangedData 是 selection.changed 的载荷。
type SelectionChangedData struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
	// ReasoningEffort 是思考档位（"" 沿用模型配置，"off" 关闭）。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// SessionRenamedData 是 session.renamed 的载荷。
type SessionRenamedData struct {
	Title string `json:"title"`
}

// NoticeData 是 notice.appended 的载荷。
type NoticeData struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

// TodoWriteData 是 todo.write 的载荷：**完整的**清单（整体替换，不是增量）。
type TodoWriteData struct {
	Todos []conversation.TodoItem `json:"todos"`
}

// record 构造一条 Record effect。
func record(kind string, data any) Effect {
	return Record{Event: store.MustEvent(kind, data)}
}

// recordCreated 是会话开始时写的第一条事件。
func recordCreated(contextID, title, workspace string) Effect {
	return record(store.TypeSessionCreated, SessionCreatedData{
		ContextID: contextID,
		Title:     title,
		CreatedAt: time.Now().UTC(),
		Workspace: workspace,
	})
}

// recordMessage 记录一条消息（用户消息、新建的 agent 消息、补齐后的结果）。
func recordMessage(msg message.Message, status conversation.EntryStatus, reason string) Effect {
	return record(store.TypeMessageAppended, MessageAppendedData{
		Message: msg,
		Status:  status,
		Reason:  reason,
	})
}

// recordFinalized 记录一条消息的最终内容与状态。
func recordFinalized(msg message.Message, status conversation.EntryStatus, reason string) Effect {
	return record(store.TypeMessageFinalize, MessageFinalizedData{
		Message: msg,
		Status:  status,
		Reason:  reason,
	})
}

// recordToolResults 记录工具结果（含补齐的合成结果）。
func recordToolResults(messageID string, results []ToolResultRecord) Effect {
	return record(store.TypeToolResults, ToolResultsData{MessageID: messageID, Results: results})
}

// recordAskRaised 记录提问。
func recordAskRaised(messageID, callID string, questions []tools.QuestionSpec) Effect {
	return record(store.TypeAskRaised, AskRaisedData{
		MessageID: messageID,
		CallID:    callID,
		Questions: questions,
	})
}

// recordAskAnswered 记录作答/跳过。
func recordAskAnswered(messageID, callID, output string) Effect {
	return record(store.TypeAskAnswered, AskAnsweredData{
		MessageID: messageID,
		CallID:    callID,
		Output:    output,
	})
}

// recordSelection 记录模型切换。
func recordSelection(sel providers.Selection) Effect {
	return recordSelectionWithEffort(sel, "")
}

// recordSelectionWithEffort 记录模型与思考档位。
func recordSelectionWithEffort(sel providers.Selection, effort string) Effect {
	return record(store.TypeSelectionChange, SelectionChangedData{
		ProviderID:      sel.ProviderID,
		Model:           sel.Model,
		ReasoningEffort: effort,
	})
}

// recordNotice 记录本地通知。
func recordNotice(level conversation.NoticeLevel, text string) Effect {
	return record(store.TypeNotice, NoticeData{Level: string(level), Text: text})
}

// recordTodos 记录任务清单的整体替换。
//
// 空清单也要写：模型可能把清单清空（`todos: []`），不写的话重放会保留上一份，
// 界面与在线状态就不一致了。
func recordTodos(todos []conversation.TodoItem) Effect {
	return record(store.TypeTodoWrite, TodoWriteData{Todos: todos})
}

// resultRecords 把 ToolOutcome 转成日志形态。
func resultRecords(outcomes []tools.ToolOutcome) []ToolResultRecord {
	out := make([]ToolResultRecord, 0, len(outcomes))
	for _, outcome := range outcomes {
		out = append(out, ToolResultRecord{
			ID:        outcome.ID,
			Name:      outcome.Name,
			Output:    outcome.Output,
			IsError:   outcome.IsError,
			Images:    outcome.Images,
			Pending:   outcome.IsPending(),
			Questions: outcome.Pending,
		})
	}
	return out
}

// partResultRecords 从消息里取出全部工具结果（补齐后落盘用）。
//
// 补齐（SettleUnfinishedToolCalls）产生的合成结果也要写进日志，否则重放出来的
// 历史会缺配对结果，用户下次打开会话就发不出请求。
func partResultRecords(msg *message.Message) []ToolResultRecord {
	var out []ToolResultRecord
	for _, part := range msg.Parts {
		if !isMedia(part, "application/vnd.simple-agent.tool-result+json") {
			continue
		}
		data, ok := partData(part)
		if !ok {
			continue
		}
		id, _ := data["id"].(string)
		name, _ := data["name"].(string)
		output, _ := data["output"].(string)
		isError, _ := data["is_error"].(bool)
		pending, _ := data["pending"].(bool)
		out = append(out, ToolResultRecord{
			ID:        id,
			Name:      name,
			Output:    output,
			IsError:   isError,
			Images:    imagesOf(data),
			Pending:   pending,
			Questions: questionsOf(data),
		})
	}
	return out
}
