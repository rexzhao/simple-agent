package driver

import (
	"encoding/json"
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

// MessageEventData 是 message.input / message.output 的载荷。
//
// 一条事件 = 一条完整的消息（不拆 part 级）：
//   - message.input：用户输入，status 恒为 complete
//   - message.output：LLM 的一次完整输出（含 thinking / text / tool_use）
//
// 流式过程中的增量**不落盘**：崩溃时这一轮直接丢弃，从上一个 turn 继续
// （见 docs/session-log-model.md §3）。
type MessageEventData struct {
	// Round/Turn 标明这条消息属于哪个回合的哪一次请求。
	Round int `json:"round"`
	Turn  int `json:"turn"`
	// Role 是 user / agent（与 message.Message.Role 一致，方便只读日志时判断）。
	Role    string                   `json:"role"`
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
	// Diff 是文件修改的结构化差异（edit/write 用），**只给界面**。
	// 旧日志没有该字段，重放后为 nil —— 界面退回只显示 Output。
	Diff *tools.FileDiff `json:"diff,omitempty"`
	// Exec 是命令执行的元信息（bash/pwsh），**只给界面**。
	Exec *tools.ExecInfo `json:"exec,omitempty"`
	// ImageInfo 是图片工具的摘要，**只给界面**（折叠行一行人话）。
	ImageInfo *tools.ImageInfo `json:"image_info,omitempty"`
	// Web / Search / Read / Todo 同理：都是只给界面的结构化摘要。
	Web    *tools.WebInfo    `json:"web,omitempty"`
	Search *tools.SearchInfo `json:"search,omitempty"`
	Read   *tools.ReadInfo   `json:"read,omitempty"`
	Todo   *tools.TodoInfo   `json:"todo,omitempty"`
}

// ToolResultData 是 tool.result 的载荷：**单条**工具结果。
//
// 取代旧的批量 tool.results：一条事件一条结果，写入后不再变更，
// 展示端可按条缓存、分页只需按 seq 切。
// 它与 tool_use 的关联靠 CallID（与 LLM 协议一致，并行工具完成顺序不影响配对）。
type ToolResultData struct {
	Round  int `json:"round"`
	Turn   int `json:"turn"`
	CallID string `json:"call_id"`
	ToolResultRecord
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

// 以下四个是边界事件的载荷（见 docs/session-log-model.md §2.2）。

// RoundEventData 是 round.started / round.finished 的载荷。
type RoundEventData struct {
	Round int `json:"round"`
	// DurationMs 只出现在 round.finished 上（该轮总耗时）。
	DurationMs int64 `json:"duration_ms,omitempty"`
}

// TurnEventData 是 turn.started / turn.finished 的载荷。
type TurnEventData struct {
	Round int `json:"round"`
	Turn  int `json:"turn"`
	// DurationMs 只出现在 turn.finished 上（这一次请求的耗时）。
	DurationMs int64 `json:"duration_ms,omitempty"`
	// Usage 是这一次请求的真实用量（只有 turn.finished 会带）。
	//
	// **必须落盘**：否则打开一个没压缩过的旧会话，累计用量是 0，
	// 状态条上的缓存命中率显示不出来（用户报的："刚打开会话时不显示
	// 缓存命中率"）。以前只有压缩快照里存过累计值，没压缩过的会话
	// 就永远读不到任何用量。
	//
	// 重放时逐条累加得到会话累计（见 Replayed.TotalUsage）。
	Usage *UsageData `json:"usage,omitempty"`
}

// recordTurnFinished 记录 Turn 结束（带本次请求耗时与用量）。
func recordTurnFinished(roundID, turn int, duration time.Duration, usage Usage) Effect {
	data := TurnEventData{
		Round:      roundID,
		Turn:       turn,
		DurationMs: duration.Milliseconds(),
	}
	// 全零不写：省日志空间，也让"这条没有用量"和"用量是 0"区分开
	if usage != (Usage{}) {
		converted := toUsageData(usage)
		data.Usage = &converted
	}
	return record(store.TypeTurnFinished, data)
}

// recordRoundStarted 记录 Round 开始。
func recordRoundStarted(roundID int) Effect {
	return record(store.TypeRoundStarted, RoundEventData{Round: roundID})
}

// recordRoundFinished 记录 Round 结束（带总耗时）。
func recordRoundFinished(roundID int, duration time.Duration) Effect {
	return record(store.TypeRoundFinished, RoundEventData{
		Round:      roundID,
		DurationMs: duration.Milliseconds(),
	})
}

// recordTurnStarted 记录 Turn 开始（请求即将发出）。
func recordTurnStarted(roundID, turn int) Effect {
	return record(store.TypeTurnStarted, TurnEventData{Round: roundID, Turn: turn})
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

// recordMessageInput 记录用户输入（一条消息一条记录）。
func recordMessageInput(roundID, turn int, msg message.Message) Effect {
	return record(store.TypeMessageInput, MessageEventData{
		Round: roundID, Turn: turn, Role: "user",
		Message: msg, Status: conversation.StatusComplete,
	})
}

// recordMessageOutput 记录 LLM 的一次完整输出（含 thinking / text / tool_use）。
//
// 只在内容确定后写一次：流式增量不落盘，因此不会再出现"同一条消息两条事件"。
func recordMessageOutput(roundID, turn int, msg message.Message, status conversation.EntryStatus, reason string) Effect {
	return record(store.TypeMessageOutput, MessageEventData{
		Round: roundID, Turn: turn, Role: "agent",
		Message: msg, Status: status, Reason: reason,
	})
}

// recordToolResults 把一批工具结果拆成**单条**事件（含补齐的合成结果）。
//
// 拆成单条的理由：每条写入后不再变更，展示端可按条缓存；
// 分页按 seq 切时不会出现"半批结果"。
//
// round/turn 由调用方按**发起这次工具调用的那条消息**给出（见 driver 侧），
// 不能用 driver 的当前 round/turn——工具是异步执行的，写结果时状态可能已经变了。
func recordToolResults(roundID, turn int, results []ToolResultRecord) []Effect {
	effects := make([]Effect, 0, len(results))
	for _, result := range results {
		effects = append(effects, record(store.TypeToolResult, ToolResultData{
			Round: roundID, Turn: turn, CallID: result.ID, ToolResultRecord: result,
		}))
	}
	return effects
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
			Diff:      outcome.Diff,
			Exec:      outcome.Exec,
			ImageInfo: outcome.Image,
			Web:       outcome.Web,
			Search:    outcome.Search,
			Read:      outcome.Read,
			Todo:      outcome.Todo,
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
			Diff:      diffOf(data),
			Exec:      execOf(data),
			ImageInfo: imageInfoOf(data),
			Web:       webOf(data),
			Search:    searchOf(data),
			Read:      readOf(data),
			Todo:      todoOf(data),
		})
	}
	return out
}

// toolExtraKeys 是工具结果里**只给界面**的结构化字段。
//
// 集中在这里的理由：这些字段要经过两条路径 ——
//  1. 实时：ToolResult() 把 outcome 写进 part
//  2. 重放：applyToolResult() 从日志重建 part
//
// 两份代码各自写一遍，加字段时极容易只改一边（已经踩过两次：
// diff 和 exec 都曾"刚编辑完能看到、重启后就没了"）。
// 现在两条路都调 attachToolExtras，漏改的可能性降到最低。
const (
	extraDiff      = "diff"
	extraExec      = "exec"
	extraImageInfo = "image_info"
	extraWeb       = "web"
	extraSearch    = "search"
	extraRead      = "read"
	extraTodo      = "todo"
)

// attachToolExtras 把 outcome 里的结构化附加字段挂到 part 数据上。
//
// 实时路径（ToolResult）与重放路径（applyToolResult）**共用这一个函数**。
// 这些字段都不发给模型：providers 组装请求时只取 output/is_error。
func attachToolExtras(data map[string]any, outcome tools.ToolOutcome) {
	if outcome.Diff != nil {
		data[extraDiff] = outcome.Diff
	}
	if outcome.Exec != nil {
		data[extraExec] = outcome.Exec
	}
	if outcome.Image != nil {
		data[extraImageInfo] = outcome.Image
	}
	if outcome.Web != nil {
		data[extraWeb] = outcome.Web
	}
	if outcome.Search != nil {
		data[extraSearch] = outcome.Search
	}
	if outcome.Read != nil {
		data[extraRead] = outcome.Read
	}
	if outcome.Todo != nil {
		data[extraTodo] = outcome.Todo
	}
}

// attachRecordExtras 是重放侧的对应版本：从 ToolResultRecord 把同样这些
// 字段写回 part。
func attachRecordExtras(data map[string]any, record ToolResultRecord) {
	if record.Diff != nil {
		data[extraDiff] = record.Diff
	}
	if record.Exec != nil {
		data[extraExec] = record.Exec
	}
	if record.ImageInfo != nil {
		data[extraImageInfo] = record.ImageInfo
	}
	if record.Web != nil {
		data[extraWeb] = record.Web
	}
	if record.Search != nil {
		data[extraSearch] = record.Search
	}
	if record.Read != nil {
		data[extraRead] = record.Read
	}
	if record.Todo != nil {
		data[extraTodo] = record.Todo
	}
}

// diffOf 读取 part 里的结构化差异（edit 用）。
//
// 内存里是 *tools.FileDiff 指针；重放时是 map[string]any（JSON 解出来的）。
// 两条路都要认 —— 只认一种的话，重启后 diff 就没了。
func diffOf(data map[string]any) *tools.FileDiff {
	switch value := data["diff"].(type) {
	case *tools.FileDiff:
		return value
	case map[string]any:
		raw, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		var diff tools.FileDiff
		if err := json.Unmarshal(raw, &diff); err != nil {
			return nil
		}
		return &diff
	}
	return nil
}

// decodeExtra 把 part 里的结构化字段还原成具体类型。
//
// 内存里是 *T 指针，重放后是 map[string]any（JSON 解出来的）——
// 两条路都要认。diff/exec/image/of 都是一样的形状，所以收成一个泛型函数，
// 免得每加一个字段就复制一遍 12 行。
func decodeExtra[T any](data map[string]any, key string) *T {
	switch value := data[key].(type) {
	case *T:
		return value
	case map[string]any:
		raw, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		var out T
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil
		}
		return &out
	}
	return nil
}

// webOf / searchOf / readOf / todoOf 读取各工具的结构化摘要。
func webOf(data map[string]any) *tools.WebInfo    { return decodeExtra[tools.WebInfo](data, extraWeb) }
func searchOf(data map[string]any) *tools.SearchInfo { return decodeExtra[tools.SearchInfo](data, extraSearch) }
func readOf(data map[string]any) *tools.ReadInfo  { return decodeExtra[tools.ReadInfo](data, extraRead) }
func todoOf(data map[string]any) *tools.TodoInfo  { return decodeExtra[tools.TodoInfo](data, extraTodo) }

// imageInfoOf 读取 part 里的图片摘要（read_image / display_image）。
//
// 与 diffOf/execOf 同理：内存里是 *tools.ImageInfo，重放后是 map[string]any。
func imageInfoOf(data map[string]any) *tools.ImageInfo {
	switch value := data["image_info"].(type) {
	case *tools.ImageInfo:
		return value
	case map[string]any:
		raw, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		var info tools.ImageInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			return nil
		}
		return &info
	}
	return nil
}

// execOf 读取 part 里的命令元信息（bash/pwsh）。
//
// 与 diffOf 同样的道理：内存里是 *tools.ExecInfo，重放后是 map[string]any。
func execOf(data map[string]any) *tools.ExecInfo {
	switch value := data["exec"].(type) {
	case *tools.ExecInfo:
		return value
	case map[string]any:
		raw, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		var info tools.ExecInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			return nil
		}
		return &info
	}
	return nil
}
