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
	// TotalUsage 是重放出来的会话累计用量（状态条算缓存命中率用）。
	//
	// 来源：压缩快照里的基线（total_usage 字段）。**没有快照、或快照是
	// 老版本（没有该字段）时为零值** —— 那等于按 0/0 开始算，只统计之后
	// 的真实请求。旧日志里本来就没有准确的累计数字，编不出来，
	// 如实从零算比造一个假基线诚实。
	TotalUsage Usage

	// RoundSeq 是日志里出现过的最大 roundId。
	//
	// 重放后从它继续自增，保证 roundId 在同一会话内单调不复用。
	RoundSeq int
	// CrashedRound 是"开始了但没有 finished 的 Round"（0 表示没有）。
	//
	// 崩溃恢复用它判定"要不要显示上次异常结束"并让用户选择重发。
	CrashedRound int
	// OpenTurn 是"开始了但没有 finished 的 Turn"（零值表示没有）。
	//
	// 与 CrashedRound 配合：Turn 中断意味着这次请求没走完；
	// 若该 Turn 已经有 message.output，还需要补齐 tool.result。
	OpenTurn TurnEventData

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
	// turnMessages 是 (round, turn) → agent 消息 ID 的索引。
	//
	// tool.result 只带 (round, turn, call_id)，重放时要靠它找回归属消息。
	// 与 appendedAt/counted 一样，增量续传必须共享同一份。
	turnMessages map[turnKey]string
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
	// turnMessages 记录每个 (round, turn) 对应的 agent 消息 ID。
	//
	// tool.result 事件只带 (round, turn, call_id)，要靠它找回归属的消息——
	// 工具的产出 belongs to 发起它的那次请求。
	turnMessages map[turnKey]string
}

// turnKey 是 (round, turn) 的复合键。
type turnKey struct {
	Round int
	Turn  int
}

func newReplayState() *replayState {
	out := &Replayed{}
	conv := conversation.New("")
	out.Conversation = conv
	// 索引挂到 out 上：restorePending 读的是 out.turnMessages，
	// 而 replayState 与 Replayed 必须共享同一份（增量续传才不会丢）。
	out.turnMessages = map[turnKey]string{}
	out.appendedAt = map[string]time.Time{}
	out.counted = map[string]bool{}
	return &replayState{
		out:          out,
		conv:         conv,
		appendedAt:   out.appendedAt,
		counted:      out.counted,
		turnMessages: out.turnMessages,
	}
}

// newReplayStateFrom 在已有结果上继续累加（增量续传）。
func newReplayStateFrom(replayed *Replayed) *replayState {
	st := &replayState{
		out:          replayed,
		conv:         replayed.Conversation,
		appendedAt:   replayed.appendedAt,
		counted:      replayed.counted,
		turnMessages: replayed.turnMessages,
	}
	if st.appendedAt == nil {
		st.appendedAt = map[string]time.Time{}
		replayed.appendedAt = st.appendedAt
	}
	if st.counted == nil {
		st.counted = map[string]bool{}
		replayed.counted = st.counted
	}
	if st.turnMessages == nil {
		st.turnMessages = map[turnKey]string{}
		replayed.turnMessages = st.turnMessages
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
		// 摘要与 kept 消息都「来自」这条快照：给它们记上快照的 seq。
		// 没有 seq 的条目在展示端分页时无法定位（切页、判断「更早」都靠它）。
		for _, entry := range conv.Entries() {
			if entry.Message != nil {
				entry.Message.SetSeqRange(event.Seq, event.Seq)
			}
		}
		// 清单是会话状态（不在消息里），快照里没带就等于空
		conv.SetTodos(data.Todos)
		// 快照是压缩后的新起点：计数从它记下的基线接续（旧快照没有这两个
		// 字段 → 0，等于只统计快照之后的内容）。
		out.Turns, out.Steps = data.Turns, data.Steps
		// 累计用量同理。**旧快照没有 total_usage** → 零值 →
		// 命中率按 0/0 算（不显示），直到这个会话再跑一轮真实的请求。
		out.TotalUsage = data.TotalUsage.toUsage()
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
		// 工作区也进快照：session.created 被归档后，它是唯一的来源。
		// 旧快照没有该字段 → 保持空 → 调用方按旧行为回退。
		if data.Workspace != "" {
			out.Workspace = data.Workspace
		}

	case store.TypeMessageInput:
		var data MessageEventData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		added, addErr := conv.AddMessage(data.Message, data.Status)
		if addErr != nil {
			out.Warnings = append(out.Warnings, addErr.Error())
			return
		}
		// 计数：user 消息算一轮（口径见 Counts）
		st.countMessage(data.Message.MessageID, data.Message.Role)
		// 用事件时间作为创建时间：回合耗时 = 最后一条消息完成时间 −
		// 回合前那条 user 消息的创建时间，重放与实时因此一致。
		if !event.At.IsZero() {
			added.CreatedAt = event.At
		}
		// 记录 seq 区间：分页游标用它（日志不可变，seq 稳定）
		added.SetSeqRange(event.Seq, event.Seq)
		st.appendedAt[data.Message.MessageID] = event.At

	case store.TypeMessageOutput:
		var data MessageEventData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		// 记下 (round, turn) → 消息 ID：后续 tool.result 靠它找归属
		st.turnMessages[turnKey{Round: data.Round, Turn: data.Turn}] = data.Message.MessageID

		if entry, ok := conv.Message(data.Message.MessageID); ok {
			// 已存在（理论上不该有，因为不再写空壳）：覆盖内容
			if err := conv.ReplaceContent(data.Message.MessageID, data.Message,
				data.Status, data.Reason); err != nil {
				out.Warnings = append(out.Warnings, err.Error())
			}
			if !event.At.IsZero() {
				entry.SetFinishedAt(event.At)
			}
			entry.SetSeqRange(event.Seq, event.Seq)
		} else if added, err := conv.AddMessage(data.Message, data.Status); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
		} else {
			if !event.At.IsZero() {
				added.CreatedAt = event.At
				added.SetFinishedAt(event.At)
			}
			added.SetSeqRange(event.Seq, event.Seq)
		}
		st.countMessage(data.Message.MessageID, data.Message.Role)
		st.appendedAt[data.Message.MessageID] = event.At
		if data.Status == conversation.StatusFailed {
			_ = conv.SetFailureReason(data.Message.MessageID, data.Reason)
		}

	case store.TypeMessageStatus:
		var data MessageStatusData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		applyStatus(conv, data, out)

	case store.TypeToolResult:
		var data ToolResultData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		st.applyToolResult(data, event.Seq)

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

	case store.TypeRoundStarted:
		var data RoundEventData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		// 记下最大的 roundId：后续新 Round 从这里继续自增，
		// 保证同一会话内的 roundId 单调不复用。
		if data.Round > out.RoundSeq {
			out.RoundSeq = data.Round
		}
		out.CrashedRound = data.Round

	case store.TypeRoundFinished:
		var data RoundEventData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		if data.Round > out.RoundSeq {
			out.RoundSeq = data.Round
		}
		// 这一轮正常收尾了：清掉"崩溃中的 Round"标记
		if out.CrashedRound == data.Round {
			out.CrashedRound = 0
		}

	case store.TypeTurnStarted:
		var data TurnEventData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		// 有 turn.started 但没有对应的 turn.finished → 这个 Turn 被中断
		out.OpenTurn = data

	case store.TypeTurnFinished:
		var data TurnEventData
		if err := event.Decode(&data); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			return
		}
		if out.OpenTurn.Round == data.Round && out.OpenTurn.Turn == data.Turn {
			out.OpenTurn = TurnEventData{} // 正常收尾
		}
		// 逐条累加真实用量：没压缩过的会话靠这个还原累计值
		// （以前只有压缩快照存累计，于是"刚打开旧会话看不到命中率"）。
		if data.Usage != nil {
			out.TotalUsage = addUsage(out.TotalUsage, data.Usage.toUsage())
		}

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

// applyToolResult 把**单条**工具结果写回它所属的消息。
//
// 归属优先靠 call_id：工具结果一定带着它，而且 tool_use 就在目标消息里。
// (round, turn) 只作为"日志里已经有了明确标注"时的快速路径——
// 不能用它作为唯一依据，因为工具执行是异步的，写结果时 driver 的当前
// round/turn 可能已经变了（续轮、重置）。
func (st *replayState) applyToolResult(data ToolResultData, seq uint64) {
	messageID := st.findMessageByCallID(data.CallID)
	if messageID == "" {
		// 兜底：按 (round, turn) 找（call_id 缺失或 tool_use 已被压缩掉）
		messageID = st.turnMessages[turnKey{Round: data.Round, Turn: data.Turn}]
	}
	if messageID == "" {
		return
	}
	entry, ok := st.conv.Message(messageID)
	if !ok {
		return
	}

	result := data.ToolResultRecord
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
	if result.Pending {
		fields["pending"] = true
		if len(result.Questions) > 0 {
			fields["questions"] = result.Questions
		}
	}
	// edit 的结构化差异：**持久化在日志里**（ToolResultRecord.Diff），
	// 重放时必须带回 part，否则重启后界面就画不出 diff 了
	// （真实踩到：编辑过的文件记录，重启后只剩一句"已编辑 xxx（+1 -1）"）。
	// 结构化附加字段（diff/exec/image_info/web/search/read/todo）：
	// 走与实时路径**共用的** attachRecordExtras。
	//
	// 这里是 bug 的高发区 —— 曾经只补了实时路径、漏了这里，导致
	// "刚编辑完能看到 diff，重启后就没了"。改成共用函数后，
	// 新增字段只要在 attachToolExtras/attachRecordExtras 里各加一行。
	attachRecordExtras(fields, result)
	entry.AddPart(message.Part{
		Content:   message.DataPart{Data: fields},
		MediaType: render.MediaToolResult,
	})
	// 工具结果也属于这条消息：扩展它的 seq 区间（分页时整条消息不能被切开）
	entry.SetSeqRange(seq, seq)
}

// findMessageByCallID 在所有消息里找带指定 tool_use 的那一条（兜底用）。
func (st *replayState) findMessageByCallID(callID string) string {
	if callID == "" {
		return ""
	}
	for _, entry := range st.conv.Entries() {
		if entry.Message == nil {
			continue
		}
		for _, part := range entry.Message.Message.Parts {
			if !isMedia(part, render.MediaToolUse) {
				continue
			}
			payload, ok := partData(part)
			if !ok {
				continue
			}
			if id, _ := payload["id"].(string); id == callID {
				return entry.Message.Message.MessageID
			}
		}
	}
	return ""
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

// restorePending 收尾重放出来的中间态。
//
// 新模型（docs/session-log-model.md §3）：崩溃判定看**有没有 turn.finished**，
// 而不是看消息 status。Replayed.OpenTurn / CrashedRound 已经记录了
// "最后一个 turn.finished 之后还剩什么"，这里据此决定补什么。
//
//	已落盘的内容                                动作
//	──────────────────────────────────────────  ──────────────────────────
//	有 output（含 tool_use），缺 tool.result     补 tool.result（is_error）
//	只有 input，没有 output                      不补内容，标记待重发
//	有 output 但缺 turn.finished                 补 turn.finished
//	全部都有 turn.finished                       什么都不做
//
// **补 tool.result 与 turn.finished 无关**：只要 tool_use 缺配对结果，
// 下次请求就会被 LLM 协议拒收，必须补。
//
// 所有修复都产生 **repair 事件**（调用方必须在接受新事件之前写进日志）。
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
			//
			// 新模型下不会再写 streaming 空壳，所以这条路径只对**旧日志**生效；
			// 新日志的中断由 turn.finished 缺失来判定（见 reclaimOpenTurn）。
			_ = conv.CancelStream(msg.Message.MessageID)
			out.Warnings = append(out.Warnings, "有一条消息在生成过程中被中断，已标记为已中止")
			out.Repairs = append(out.Repairs, store.MustEvent(store.TypeMessageOutput,
				MessageEventData{
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
			// 拆成单条 tool.result 事件（与 recordToolResults 的写法一致）
			for _, result := range partResultRecords(msg.Message) {
				out.Repairs = append(out.Repairs, store.MustEvent(store.TypeToolResult,
					ToolResultData{CallID: result.ID, ToolResultRecord: result}))
			}
		}
	}

	// 有 output 但缺 turn.finished：内容已经完整，只差边界标记。
	// 补上它，否则下次重放还会把它当成"中断"。
	if out.OpenTurn.Round > 0 && turnHasOutput(conv, out.turnMessages, out.OpenTurn) {
		out.Repairs = append(out.Repairs, store.MustEvent(store.TypeTurnFinished,
			TurnEventData{Round: out.OpenTurn.Round, Turn: out.OpenTurn.Turn}))
		out.Warnings = append(out.Warnings, "有一次请求已产出内容但缺少结束标记，已补记")
		out.OpenTurn = TurnEventData{}
	}

	return groups
}

// turnHasOutput 判断某个 (round, turn) 是否已经有 message.output 落盘。
func turnHasOutput(conv *conversation.Conversation, index map[turnKey]string, open TurnEventData) bool {
	messageID := index[turnKey{Round: open.Round, Turn: open.Turn}]
	if messageID == "" {
		return false
	}
	_, ok := conv.Message(messageID)
	return ok
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
	// 累计用量从快照接续。旧日志没有这条基线 → 零值，命中率按 0/0 算。
	//
	// 顺带把它也塞进 lastUsage：界面在"这个会话还没跑过新请求"时
	// 也该看到一点数字（否则状态条上的用量整块是空的）。
	d.totalUsage = replayed.TotalUsage
	d.lastUsage = replayed.TotalUsage
	// roundId 从日志里的最大值继续，保证不复用
	d.roundSeq = replayed.RoundSeq
	d.opts.ReasoningEffort = replayed.ReasoningEffort
	if replayed.HasSelection {
		d.opts.Selection = replayed.Selection
	}
	// 工作区从日志/快照恢复（提交时要用它对下次压缩写快照）。
	// 恢复不出来就保持调用方给的值（host 会回退到配置）。
	if replayed.Workspace != "" {
		d.opts.Workspace = replayed.Workspace
	}
	if len(replayed.PendingAsks) > 0 {
		messageID := replayed.PendingAsks[0].MessageID
		d.round = &Round{
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
