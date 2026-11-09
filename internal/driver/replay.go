package driver

import (
	"fmt"
	"time"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/render"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// Record 是「把这条事件追加到日志」的 effect。
//
// driver 在每一个会改变持久化状态的地方返回它：用户消息、流开始、流结束、
// 工具结果、提问与作答、切换模型、重命名、以及补齐未配对调用产生的修复。
type Record struct {
	Event store.Event
}

func (Record) effectKind() string { return "record" }

// Replayed 是重放出来的会话状态。
type Replayed struct {
	ContextID string
	Title     string
	// Workspace 是 session.created 里记下的工作区（旧日志为空）。
	Workspace string
	Selection providers.Selection
	// HasSelection 表示日志里有 selection.changed（否则用调用方给的默认值）。
	HasSelection bool
	// ReasoningEffort 是重放出来的思考档位（空表示沿用模型配置）。
	ReasoningEffort string
	// Turns/Steps 是重放出来的累计计数（状态条用，口径见 Counts）。
	//
	// 来源有两部分：压缩快照里的基线 + 快照之后的消息条数。没有快照的会话
	// 直接由全部消息推导，因此「重放 == 在线」在压缩前后都成立。
	Turns int
	Steps int

	Conversation *conversation.Conversation

	// PendingAsks 是重放时仍未作答的提问（面板要重新出现）。
	PendingAsks []*AskGroup
	// Repairs 是**必须补写进日志**的事件（加载流程要在接受新事件之前追加）：
	//   - 只有 appended(streaming) 没有 finalized → 补一条 finalized(cancelled)；
	//   - 有 tool_use 没有结果 → 补一条 tool.results（is_error=「已中断」）。
	// 写完之后再次重放不会产生新的 repairs（幂等）。
	Repairs []store.Event
	// Warnings 记录日志层面被容忍的问题（坏行、截断、被修的中间态）。
	Warnings []string

	// appendedAt/counted 是 ApplyPersisted 增量续传要接着用的内部状态
	// （同一会话的多次 ApplyPersisted 必须共享，不能每次重建）。
	appendedAt map[string]time.Time
	counted    map[string]bool
}

// Replay 把事件流重建成会话状态。
//
// 纯函数：不碰文件、不碰 UI，因此可以和「在线跑一遍 driver」的结果直接比较。
func Replay(events []store.Event) (Replayed, error) {
	st := newReplayState()
	var lastSeq uint64
	for index, event := range events {
		if index > 0 && event.Seq != lastSeq+1 {
			st.out.Warnings = append(st.out.Warnings,
				fmt.Sprintf("事件序号不连续（%d → %d），在此截断", lastSeq, event.Seq))
			break
		}
		lastSeq = event.Seq
		st.apply(event)
	}
	st.finalize()
	return *st.out, nil
}

// ApplyPersisted 把**一条**持久事件应用到已有状态上（展示端增量续传用）。
//
// 与 Replay 共用同一份 apply 逻辑，所以"全量重放"与"增量续传"不可能出现两套语义。
// **不做收尾**（finalize）：收尾会把流式中间态判成"已中断"并产生 repairs，
// 那是加载日志时的动作；展示端增量应用时消息可能正在流式输出，不能被这样处理。
func ApplyPersisted(replayed *Replayed, event store.Event) error {
	st := newReplayStateFrom(replayed)
	st.apply(event)
	return nil
}

// replayState 是重放的累加状态（分给 Replay 与 ApplyPersisted 共用）。
type replayState struct {
	out        *Replayed
	conv       *conversation.Conversation
	appendedAt map[string]time.Time
	// counted 保证同一条消息只记一次数（"appended" 与"finalized 兜底"可能都碰到它）。
	counted map[string]bool
}

func newReplayState() *replayState {
	out := &Replayed{}
	conv := conversation.New("")
	out.Conversation = conv
	return &replayState{
		out:        out,
		conv:       conv,
		appendedAt: map[string]time.Time{},
		counted:    map[string]bool{},
	}
}

// newReplayStateFrom 在已有结果上继续累加（增量续传）。
func newReplayStateFrom(replayed *Replayed) *replayState {
	st := &replayState{
		out:        replayed,
		conv:       replayed.Conversation,
		appendedAt: replayed.appendedAt,
		counted:    replayed.counted,
	}
	if st.appendedAt == nil {
		st.appendedAt = map[string]time.Time{}
		replayed.appendedAt = st.appendedAt
	}
	if st.counted == nil {
		st.counted = map[string]bool{}
		replayed.counted = st.counted
	}
	return st
}

func (st *replayState) countMessage(id string, role message.Role) {
	if id == "" || st.counted[id] {
		return
	}
	st.counted[id] = true
	if role == message.RoleAgent {
		st.out.Steps++
	} else {
		st.out.Turns++
	}
}

// apply 应用**一条**事件（不含收尾）。
func (st *replayState) apply(event store.Event) {
	out, conv := st.out, st.conv

	switch event.Type {
	case store.TypeSessionCreated:
		var data SessionCreatedData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		out.ContextID = data.ContextID
		out.Title = data.Title
		out.Workspace = data.Workspace
		if conv.ContextID() == "" && data.ContextID != "" {
			conv = conversation.New(data.ContextID)
			out.Conversation = conv
			st.conv = conv
		}

	case store.TypeCompactSnapshot:
		// 快照是新文件的第一行：直接用它重建会话（摘要 + kept）
		var data CompactSnapshotData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		rebuilt, buildErr := rebuildFromSnapshot(data)
		if buildErr != nil {
			out.Warnings = append(out.Warnings, buildErr.Error())
			return
		}
		conv = rebuilt
		out.Conversation = conv
		st.conv = conv
		// 清单是会话状态（不在消息里），快照里没带就等于空
		conv.SetTodos(data.Todos)
		// 快照是压缩后的新起点：计数从它记下的基线接续（旧快照没有这两个
		// 字段 → 0，等于只统计快照之后的内容）。
		out.Turns, out.Steps = data.Turns, data.Steps
		if data.Title != "" {
			out.Title = data.Title
		}
		if data.Selection.ProviderID != "" {
			out.Selection = data.Selection
			out.HasSelection = true
		}
		if data.ReasoningEffort != "" {
			out.ReasoningEffort = data.ReasoningEffort
		}
		if data.ContextID != "" {
			out.ContextID = data.ContextID
		}

	case store.TypeMessageAppended:
		var data MessageAppendedData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		added, addErr := conv.AddMessage(data.Message, data.Status)
		if addErr != nil {
			out.Warnings = append(out.Warnings, addErr.Error())
			return
		}
		// 计数：user 消息算一轮，agent 消息算一步（口径见 Counts）。
		// 只在这里加——finalize/status/tool.results 都是给同一条消息补状态。
		st.countMessage(data.Message.MessageID, data.Message.Role)
		// 用事件时间作为消息的创建时间：回合耗时 = 最后一条消息的完成时间 −
		// 回合前那条 user 消息的创建时间，重放与实时因此一致（不再额外写事件）。
		if !event.At.IsZero() {
			added.CreatedAt = event.At
		}
		st.appendedAt[data.Message.MessageID] = event.At
		if data.Status == conversation.StatusFailed {
			_ = conv.SetFailureReason(data.Message.MessageID, data.Reason)
		}

	case store.TypeMessageFinalize:
		var data MessageFinalizedData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		if entry, ok := conv.Message(data.Message.MessageID); ok {
			if err := conv.ReplaceContent(data.Message.MessageID, data.Message,
				data.Status, data.Reason); err != nil {
				out.Warnings = append(out.Warnings, err.Error())
			}
			if !event.At.IsZero() {
				entry.SetFinishedAt(event.At)
			}
		} else if added, err := conv.AddMessage(data.Message, data.Status); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
		} else if !event.At.IsZero() {
			added.CreatedAt = event.At
			added.SetFinishedAt(event.At)
		}
		// 这条消息可能**只经 finalized 出现**（appended 那行损坏/被截断）：
		// countMessage 幂等，这里补一次不会重复计数。
		st.countMessage(data.Message.MessageID, data.Message.Role)

	case store.TypeMessageStatus:
		var data MessageStatusData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		applyStatus(conv, data, out)

	case store.TypeToolResults:
		var data ToolResultsData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		appendToolResults(conv, data)

	case store.TypeAskAnswered:
		var data AskAnsweredData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		// 用最终答案覆盖 pending 占位（tool.results 里已经写过占位）
		settleAsk(conv, data)

	case store.TypeSelectionChange:
		var data SelectionChangedData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		out.Selection = providers.Selection{ProviderID: data.ProviderID, Model: data.Model}
		out.HasSelection = true
		out.ReasoningEffort = data.ReasoningEffort

	case store.TypeSessionRenamed:
		var data SessionRenamedData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		out.Title = data.Title

	case store.TypeNotice:
		var data NoticeData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		_, _ = conv.AddNotice(conversation.NoticeLevel(data.Level), data.Text)

	case store.TypeTodoWrite:
		var data TodoWriteData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		// 整体替换（last-write-wins）：与 dsh 的 `todos` 投影同一语义。
		conv.SetTodos(data.Todos)
	}
}

// finalize 做重放收尾：流式中间态判为 Cancelled 并产生 repair，挂起的提问恢复，
// 未配对的工具调用补结果（也是 repair）。只在**加载完整日志**时调用。
func (st *replayState) finalize() {
	st.out.PendingAsks = restorePending(st.conv, st.out)
}

// applyStatus 应用状态变化。
func applyStatus(conv *conversation.Conversation, data MessageStatusData, out *Replayed) {
	switch data.Status {
	case conversation.StatusComplete:
		_ = conv.EndStream(data.MessageID)
	case conversation.StatusCancelled:
		_ = conv.CancelStream(data.MessageID)
	case conversation.StatusFailed:
		_ = conv.FailStream(data.MessageID, data.Reason)
	case conversation.StatusStreaming, conversation.StatusPending:
		// 日志里不该出现「设为 streaming」的状态事件，忽略
	default:
		out.Warnings = append(out.Warnings, fmt.Sprintf("未知状态：%s", data.Status))
	}
}

// appendToolResults 把工具结果写回消息。
func appendToolResults(conv *conversation.Conversation, data ToolResultsData) {
	entry, ok := conv.Message(data.MessageID)
	if !ok {
		return
	}
	for _, result := range data.Results {
		fields := map[string]any{
			"id":       result.ID,
			"name":     result.Name,
			"output":   result.Output,
			"is_error": result.IsError,
		}
		if len(result.Images) > 0 {
			images := make([]any, 0, len(result.Images))
			for _, image := range result.Images {
				images = append(images, map[string]any{
					"ref":        image.Ref,
					"media_type": image.MediaType,
					"width":      image.Width,
					"height":     image.Height,
				})
			}
			fields["images"] = images
		}
		part := message.Part{
			Content:   message.DataPart{Data: fields},
			MediaType: render.MediaToolResult,
		}
		if result.Pending {
			data := part.Content.(message.DataPart).Data.(map[string]any)
			data["pending"] = true
			if len(result.Questions) > 0 {
				data["questions"] = result.Questions
			}
		}
		entry.AddPart(part)
	}
}

// settleAsk 用最终答案覆盖 pending 占位。
func settleAsk(conv *conversation.Conversation, data AskAnsweredData) {
	entry, ok := conv.Message(data.MessageID)
	if !ok {
		return
	}
	for i, part := range entry.Message.Parts {
		if !isMedia(part, render.MediaToolResult) {
			continue
		}
		partDataMap, ok := partData(part)
		if !ok {
			continue
		}
		id, _ := partDataMap["id"].(string)
		if id != data.CallID {
			continue
		}
		name, _ := partDataMap["name"].(string)
		entry.Message.Parts[i] = message.Part{
			Content: message.DataPart{Data: map[string]any{
				"id":       id,
				"name":     name,
				"output":   data.Output,
				"is_error": false,
			}},
			MediaType: render.MediaToolResult,
		}
		return
	}
}

// restorePending 收尾：中间态判为已中断、补配对结果，并收集仍未作答的提问。
//
// 三件事都会产生 **repair 事件**（调用方必须在接受新事件之前写进日志）：
//  1. 只有 appended(streaming) 没有 finalized → 补 finalized(cancelled)；
//  2. 有 tool_use 没有结果 → 补 tool.results（「已中断」错误结果）；
//  3. 不做其它猜测（内容一律不臆造）。
func restorePending(conv *conversation.Conversation, out *Replayed) []*AskGroup {
	var groups []*AskGroup
	for _, entry := range conv.Entries() {
		if entry.Message == nil {
			continue
		}
		msg := entry.Message
		status := msg.Status()

		if status.IsStreaming() || status == conversation.StatusPending {
			// 进程在生成回复时被杀：保留已写入的内容，标成「已中断」
			_ = conv.CancelStream(msg.Message.MessageID)
			out.Warnings = append(out.Warnings, "有一条消息在生成过程中被中断，已标记为已中止")
			out.Repairs = append(out.Repairs, store.MustEvent(store.TypeMessageFinalize,
				MessageFinalizedData{
					Message: *msg.Message,
					Status:  conversation.StatusCancelled,
				}))
			status = conversation.StatusCancelled
		}
		_ = status

		// 先收集挂起的提问：它们的结果本来就是 pending 占位，属于「合法未完成」
		// （用户在作答之前就关了应用），不能当成「工具没返回结果」补错误结果。
		var messageGroups []*AskGroup
		for _, part := range msg.Message.Parts {
			data, ok := partData(part)
			if !ok {
				continue
			}
			pending, _ := data["pending"].(bool)
			if !pending {
				continue
			}
			id, _ := data["id"].(string)
			messageGroups = append(messageGroups, NewAskGroup(msg.Message.MessageID, id, questionsOf(data)))
		}
		if len(messageGroups) > 0 {
			groups = append(groups, messageGroups...)
			continue // 这条消息在等用户回答，不补「已中断」结果
		}

		// 执行工具时崩溃：agent 消息已经 complete 并带 tool_use，但结果没写
		if providers.SettleUnfinishedToolCalls(msg.Message, "已中断：进程在执行工具时退出。") {
			out.Warnings = append(out.Warnings, "有工具调用没有结果，已补记为「已中断」")
			results := partResultRecords(msg.Message)
			out.Repairs = append(out.Repairs, store.MustEvent(store.TypeToolResults,
				ToolResultsData{MessageID: msg.Message.MessageID, Results: results}))
		}
	}
	return groups
}

// Restore 用重放结果构造一个可以继续工作的 driver。
//
// 若有未作答的提问，状态恢复为 AwaitingAnswer（面板重新出现，提交后继续一轮）。
func Restore(replayed Replayed, opts Options) *Driver {
	d := &Driver{conv: replayed.Conversation, opts: opts, busy: BusyIdle}
	// 标题与选择都从未日志恢复（重命名/切换模型都是事件）
	d.title = replayed.Title
	d.turns = replayed.Turns
	d.steps = replayed.Steps
	d.opts.ReasoningEffort = replayed.ReasoningEffort
	if replayed.HasSelection {
		d.opts.Selection = replayed.Selection
	}
	if len(replayed.PendingAsks) > 0 {
		messageID := replayed.PendingAsks[0].MessageID
		d.turn = &Turn{
			messageID:     messageID,
			buffers:       map[int]*toolBuffer{},
			reasoningPart: -1,
			asks:          replayed.PendingAsks,
		}
		d.busy = BusyAwaitingAnswer
	}
	return d
}

// Load 从事件流恢复 driver（等价于 Replay + Restore）。
func Load(events []store.Event, opts Options) (*Driver, Replayed, error) {
	replayed, err := Replay(events)
	if err != nil {
		return nil, Replayed{}, err
	}
	return Restore(replayed, opts), replayed, nil
}

// AsksOf 便于测试与 UI：返回提问组里的问题。
func AsksOf(groups []*AskGroup) []tools.QuestionSpec {
	var out []tools.QuestionSpec
	for _, group := range groups {
		out = append(out, group.Questions...)
	}
	return out
}
