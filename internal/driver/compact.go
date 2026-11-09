package driver

import (
	"fmt"
	"strings"

	"simple-agent/internal/conversation"
	"simple-agent/internal/locale"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/render"
	"simple-agent/internal/store"
)

// 上下文压缩（上下文压缩 = 存储压缩，同一个动作）。
//
// 关键约束：
//   - 只在 Idle 时发生（工具轮中途不压缩，用户已确认）；
//   - 摘要请求不带工具，用当前会话的模型；
//   - 失败或被取消时**什么都不改**（不写快照、不归档、内存不变）；
//   - kept 部分的起点必须是一条 user 消息（本项目里工具调用与结果在同一条
//     消息里，按消息粒度切就不会拆开配对）；
//   - 摘要用 user 角色并放在 history 第一条（Anthropic 也接受）。

// AutoCompactRatio 是自动触发的阈值（占上下文窗口的比例）。
const AutoCompactRatio = 0.80

// compactKeepRatio 是压缩后保留的近期内容占窗口的比例。
const compactKeepRatio = 0.25

// SummaryPrompt 是生成摘要的系统提示。
const SummaryPrompt = "你是对话摘要器。把下面的对话压缩成简洁的中文摘要，" +
	"保留关键事实、已完成的动作、待办事项与用户的偏好；不要加入原文没有的信息。"

// StartSummary 让调用方发起一次不带工具的摘要请求。
type StartSummary struct {
	// MessageID 是本轮（压缩前）最后一条 agent 消息 id，仅用于取消时校验。
	MessageID string
	Request   providers.ChatRequest
}

func (StartSummary) effectKind() string { return "startSummary" }

// CompactSnapshotData 是 compact.snapshot 的载荷。
//
// 与设计文档 §6.2 一致：不含历史消息全文（那是归档文件的职责），
// kept 是压缩后仍进入模型上下文的那部分。
type CompactSnapshotData struct {
	ContextID       string              `json:"context_id,omitempty"`
	Summary         string              `json:"summary"`
	SummaryID       string              `json:"summary_message_id"`
	Kept            []KeptEntry         `json:"kept_entries"`
	Selection       providers.Selection `json:"selection"`
	ReasoningEffort string              `json:"reasoning_effort,omitempty"`
	Title           string              `json:"title,omitempty"`
	CompactedAt     string              `json:"compacted_at"`
	KeptFromSeq     uint64              `json:"kept_from_seq"`
	// Turns/Steps 是压缩**之前**的累计计数基线（状态条用）。
	//
	// 为什么只能存在这里：轮数/步数由会话内容推导（见 Counts），而压缩丢弃了
	// 历史——不含基线的话压缩后计数会掉回「只剩 kept 里的几条」，与压缩前的
	// 界面不一致。旧快照没有这两个字段，读出来是 0（那时也还没有状态条）。
	Turns int `json:"turns,omitempty"`
	Steps int `json:"steps,omitempty"`
	// Todos 是压缩那一刻的任务清单（可选；旧快照没有该字段 → 空）。
	//
	// 清单是会话状态而不是历史消息，压缩不能把它抹掉。
	Todos []conversation.TodoItem `json:"todos,omitempty"`
}

// KeptEntry 是快照里保留的一条消息（带状态与创建时间）。
type KeptEntry struct {
	Message   message.Message          `json:"message"`
	Status    conversation.EntryStatus `json:"status"`
	CreatedAt string                   `json:"created_at"`
}

// UsageEstimate 估算一段历史的 token 用量。
//
// 有真实 usage 时用它（最近一次），再加上之后新增内容的估算：
// CJK 字符按 1 token/字，其余按字节数/4。
// imageTokens 是每张图片的固定估算值。
//
// 图片的真实 token 数取决于分辨率与模型的分块方式，本地算不出来；按一个
// 固定值估算（用户选的方案）足以让自动压缩及时触发，也不会离谱地高。
const imageTokens = 1000

func estimateTokens(messages []message.Message) int {
	total := 0
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if isImagePart(part) {
				total += imageTokens
				continue
			}
			switch content := part.Content.(type) {
			case message.TextPart:
				total += textTokens(content.Text)
			case message.DataPart:
				total += textTokens(fmt.Sprint(content.Data))
				// 工具结果里的图片（read_image）按张计：只看 fmt.Sprint 的引用
				// 字符串会严重低估，连续读几张图时自动压缩就会来得太晚。
				total += imageCount(content.Data) * imageTokens
			}
		}
	}
	return total
}

// imageCount 数出工具结果 data 里挂了几张图片。
func imageCount(data any) int {
	fields, ok := data.(map[string]any)
	if !ok {
		return 0
	}
	list, _ := fields["images"].([]any)
	return len(list)
}

func textTokens(text string) int {
	cjk, other := 0, 0
	for _, r := range text {
		if r >= 0x2E80 && r <= 0x9FFF || r >= 0xFF00 && r <= 0xFFEF {
			cjk++
			continue
		}
		other += len(string(r))
	}
	return cjk + other/4
}

// SummaryPromptForTest 暴露摘要提示构造（测试用）。
func SummaryPromptForTest(messages []message.Message) string { return buildSummaryPrompt(messages) }

// EstimateTokensForTest 暴露估算函数（测试用）。
func EstimateTokensForTest(messages []message.Message) int { return estimateTokens(messages) }

// DefaultContextWindow 是窗口未知时使用的保守值（32k）。
const DefaultContextWindow = 32_000

// EffectiveContextWindow 返回**实际生效**的窗口大小（未知时为 DefaultContextWindow）。
//
// 自动压缩判定、保留量计算、以及界面的「x / y（n%）」都必须用它，而不是默认模型
// 的配置值：会话的模型可能被切换过，或来自重放时与默认模型不同。
func (d *Driver) EffectiveContextWindow() int { return d.effectiveWindow() }

// effectiveWindow 返回用于判定的窗口大小。
func (d *Driver) effectiveWindow() int {
	if d.opts.ContextWindow > 0 {
		return d.opts.ContextWindow
	}
	return DefaultContextWindow
}

// TokensInUse 估算当前上下文的用量。
//
// 有真实 usage 时用它作基线（input+output），只估算之后新增的消息；
// 没有 usage（第一轮还没结束）时回退到「历史 + system + 工具 schema」的估算——
// 后两项常常有好几千 token，漏掉会明显低估。
func (d *Driver) TokensInUse() int {
	history := d.conv.History()
	if d.hasUsage && d.historyLenAtUsage <= len(history) {
		return d.usageBase + estimateTokens(history[d.historyLenAtUsage:])
	}
	total := estimateTokens(history)
	total += textTokens(d.opts.SystemPrompt)
	for _, tool := range d.opts.Tools {
		total += textTokens(tool.Name + tool.Description)
		total += estimateTokens([]message.Message{{Parts: []message.Part{
			{Content: message.DataPart{Data: tool.Parameters}},
		}}})
	}
	return total
}

// recordUsage 记住最近一次请求的真实用量。
func (d *Driver) recordUsage(usage Usage) {
	if usage.InputTokens <= 0 && usage.OutputTokens <= 0 {
		return
	}
	d.usageBase = usage.InputTokens + usage.OutputTokens
	d.historyLenAtUsage = len(d.conv.History())
	d.hasUsage = true
}

// NeedsDefaultWindowNotice 报告是否该提示「窗口未知，按默认值处理」（只提示一次）。
func (d *Driver) NeedsDefaultWindowNotice() (string, bool) {
	if d.opts.ContextWindow > 0 || d.warnedDefaultWindow {
		return "", false
	}
	d.warnedDefaultWindow = true
	return locale.Tr("notice.defaultWindow"), true
}

// ShouldCompact 报告是否该自动压缩（仅在 Idle 时由调用方询问）。
func (d *Driver) ShouldCompact() bool {
	return float64(d.TokensInUse()) >= float64(d.effectiveWindow())*AutoCompactRatio
}

// BeginCompact 进入压缩状态并返回摘要请求。
func (d *Driver) BeginCompact() ([]Effect, error) {
	if d.busy != BusyIdle {
		return nil, ErrBusy
	}
	history := d.conv.History()
	if len(history) == 0 {
		return nil, fmt.Errorf("没有可压缩的内容")
	}

	keepIndex := chooseBoundary(history, d.keepBudget())
	if keepIndex <= 0 {
		// 预算宽裕时 chooseBoundary 会保留全部。但「服务端说超窗」的情况下
		// 必须压缩，所以退一步：至少折叠到最后一轮问答之前。
		keepIndex = lastUserIndex(history)
	}
	if keepIndex <= 0 {
		// 只有一轮问答：没有更早的内容可折叠
		return nil, fmt.Errorf("上下文还很短，不需要压缩")
	}

	// 摘要对象是「将被折叠」的那部分；输入必须压到预算内，否则摘要请求自己超窗
	older := clipForSummary(history[:keepIndex], d.summaryBudget())
	prompt := buildSummaryPrompt(older)

	d.busy = BusyCompacting
	d.pendingCompact = &pendingCompact{
		keepIndex: keepIndex,
		history:   history,
	}
	return []Effect{StartSummary{Request: providers.ChatRequest{
		Model:    d.opts.Selection.Model,
		System:   SummaryPrompt,
		Messages: []message.Message{{Role: message.RoleUser, Parts: []message.Part{message.NewTextPart(prompt)}}},
	}}}, nil
}

// keepBudget 是压缩后保留的 token 预算。
func (d *Driver) keepBudget() int {
	return int(float64(d.effectiveWindow()) * compactKeepRatio)
}

// summaryBudget 是摘要请求输入的上限（窗口的 60%）。
//
// 否则会死在这里：需要压缩时历史本来就接近窗口上限，把整段历史塞进摘要请求
// 很可能直接 400，然后无限重试。
func (d *Driver) summaryBudget() int {
	return int(float64(d.effectiveWindow()) * 0.6)
}

// clipForSummary 把待摘要的消息压到 token 预算内。
//
// 保留「最早的那条」——如果它本身就是上一次压缩的摘要，就等于保留更早的上下文——
// 以及最近的部分，中间用一行省略标记代替。
func clipForSummary(messages []message.Message, budget int) []message.Message {
	if budget <= 0 || estimateTokens(messages) <= budget {
		return messages
	}
	if len(messages) <= 2 {
		return messages
	}
	head := messages[:1]
	tail := messages[1:]
	kept := append([]message.Message{}, head...)
	used := estimateTokens(head)
	start := len(tail)
	for start > 0 {
		candidate := tail[start-1]
		cost := estimateTokens([]message.Message{candidate})
		if used+cost > budget {
			break
		}
		used += cost
		start--
	}
	omitted := start
	if omitted > 0 {
		kept = append(kept, message.Message{
			Role: message.RoleUser,
			Parts: []message.Part{message.NewTextPart(
				fmt.Sprintf("…（此处省略了 %d 条较早的消息）…", omitted))},
		})
	}
	kept = append(kept, tail[start:]...)
	return kept
}

// buildSummaryPrompt 把待折叠的消息拼成摘要请求的正文。
func buildSummaryPrompt(messages []message.Message) string {
	var builder strings.Builder
	builder.WriteString("以下是需要压缩的对话：\n\n")
	for _, msg := range messages {
		role := "用户"
		if msg.Role == message.RoleAgent {
			role = "助手"
		}
		for _, part := range msg.Parts {
			// 图片不把 blob 引用写进摘要提示（模型看不懂 hash，也读不到文件）：
			// 统一写成「[图片]」，让摘要知道"这里有一张图"即可。
			if isImagePart(part) {
				fmt.Fprintf(&builder, "%s：（一张图片）\n", role)
				continue
			}
			if text, ok := part.AsText(); ok && text != "" {
				if part.MediaType == render.MediaReasoning {
					continue // 推理不进摘要
				}
				fmt.Fprintf(&builder, "%s：%s\n", role, text)
			}
			if data, ok := partData(part); ok {
				name, _ := data["name"].(string)
				if part.MediaType == render.MediaToolUse {
					fmt.Fprintf(&builder, "（调用工具 %s）\n", name)
				}
				if part.MediaType == render.MediaToolResult {
					output, _ := data["output"].(string)
					if runes := []rune(output); len(runes) > 500 {
						output = string(runes[:500]) + "…"
					}
					fmt.Fprintf(&builder, "（工具 %s 结果：%s）\n", name, output)
				}
			}
		}
	}
	return builder.String()
}

// chooseBoundary 选 kept 部分的起点：**必须是一条 user 消息**。
//
// 从预算允许的位置往前找最近的 user 消息；找不到就退到最后一条 user 消息
// （至少保留最后一次用户输入，否则压缩后的上下文里没有"用户说了什么"）。
func chooseBoundary(history []message.Message, budget int) int {
	if len(history) == 0 {
		return 0
	}
	// 从末尾往前累加，找到预算内的起点
	total := 0
	start := len(history)
	for i := len(history) - 1; i >= 0; i-- {
		total += estimateTokens(history[i : i+1])
		if budget > 0 && total > budget && start != len(history) {
			break
		}
		start = i
	}
	// 往前挪到 user 消息
	for i := start; i < len(history); i++ {
		if history[i].Role == message.RoleUser {
			return i
		}
	}
	// 没找到：退到最后一条 user 消息
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == message.RoleUser {
			return i
		}
	}
	return len(history)
}

// lastUserIndex 返回最后一条 user 消息的下标（没有则 0）。
func lastUserIndex(history []message.Message) int {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == message.RoleUser {
			return i
		}
	}
	return 0
}

// pendingCompact 是压缩过程中记住的信息。
type pendingCompact struct {
	keepIndex int
	history   []message.Message
}

// ApplyCompact 用摘要完成压缩：重建内存会话并返回快照事件。
func (d *Driver) ApplyCompact(summary string) ([]Effect, error) {
	if d.busy != BusyCompacting || d.pendingCompact == nil {
		return nil, ErrBusy
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		// 空摘要等同失败：什么都不改，回到空闲
		d.pendingCompact = nil
		d.busy = BusyIdle
		return nil, fmt.Errorf("摘要为空，压缩已放弃")
	}
	pending := d.pendingCompact
	d.pendingCompact = nil

	kept := pending.history[pending.keepIndex:]
	summaryID, err := conversation.NewMessageID()
	if err != nil {
		d.busy = BusyIdle
		return nil, err
	}
	summaryMessage := message.Message{
		MessageID: summaryID,
		Role:      message.RoleUser,
		Parts: []message.Part{{
			Content:   message.TextPart{Text: "以下是此前对话的摘要：\n\n" + summary},
			MediaType: render.MediaSummary,
		}},
	}

	// 重建内存会话：摘要 + kept（旧内容只留在归档里）
	rebuilt := conversation.New(d.conv.ContextID())
	if _, err := rebuilt.AddMessage(summaryMessage, conversation.StatusComplete); err != nil {
		d.busy = BusyIdle
		return nil, err
	}
	keptEntries := make([]KeptEntry, 0, len(kept))
	for _, msg := range kept {
		if _, err := rebuilt.AddMessage(msg, conversation.StatusComplete); err != nil {
			d.busy = BusyIdle
			return nil, err
		}
		keptEntries = append(keptEntries, KeptEntry{Message: msg, Status: conversation.StatusComplete})
	}

	d.conv = rebuilt
	d.busy = BusyIdle
	// 压缩改变了历史长度：重新建立 usage 基线（旧基线已失效）
	d.hasUsage = false
	d.usageBase = 0
	d.historyLenAtUsage = 0
	// 本轮已经压缩过，不再自动重试
	d.autoCompactUsed = true

	snapshot := CompactSnapshotData{
		ContextID:       d.conv.ContextID(),
		Summary:         summary,
		SummaryID:       summaryID,
		Kept:            keptEntries,
		Selection:       d.opts.Selection,
		ReasoningEffort: d.opts.ReasoningEffort,
		Title:           d.title,
		CompactedAt:     nowUTC(),
		KeptFromSeq:     0,
		// 计数**不因压缩而减少**：把累计值写进快照当基线，重放据此接续。
		// 摘要消息本身是合成的，不算一次用户输入（turns 里已包含它替代的那些）。
		Turns: d.turns,
		Steps: d.steps,
		// 任务清单也要进快照：它是会话状态，压缩丢历史不能把它一起丢掉
		// （否则压一次清单就消失，而界面上一秒还看着它）。
		Todos: d.conv.Todos(),
	}
	effects := []Effect{record(store.TypeCompactSnapshot, snapshot)}
	// 压缩后仍接近上限：给一条提示，不再自动重试（下一回合才允许再压）
	effects = append(effects, d.afterCompactNotice()...)
	// 压缩是"回合的终点"之一：自动压缩发生在回合结束的边界上，排队的插话
	// 必须在这里投递，否则回合一压缩完就停在 Idle，队列会一直挂着。
	// 快照已经先写进 effects，落盘顺序因此是「快照 → 插话消息 → 新流」。
	if delivery, delivered := d.deliverQueued(); delivered {
		effects = append(effects, delivery...)
	}
	return append(effects, Changed{}), nil
}

// FailCompact 放弃压缩：**什么都不改**（不写快照、不归档、内存状态不变）。
func (d *Driver) FailCompact(reason string) []Effect {
	if d.busy != BusyCompacting {
		return nil
	}
	d.pendingCompact = nil
	d.busy = BusyIdle
	// 失败之后也不再自动重试（等下一个用户回合）
	d.autoCompactUsed = true
	// 排队的插话**保留**：压缩失败不是"停止"，队列等下次 Send 时一起投递
	// （见 CancelCompact 与决策 #18/#19）。
	return []Effect{Changed{}}
}

// CancelCompact 放弃压缩（用户在压缩中点停止）。
//
// 与 FailCompact 的区别只有一处，但语义关键：**停止要清空插话队列**。
// 停下来的意思是"什么都别做了"，此时再把队列投递出去等于立刻又开一轮请求。
func (d *Driver) CancelCompact(reason string) []Effect {
	if d.busy != BusyCompacting {
		return d.Stop()
	}
	effects := d.FailCompact(reason)
	if cleared := d.ClearQueued(); len(cleared) > 0 {
		effects = append(effects, cleared...)
	}
	return effects
}

// Compacting 报告是否正在压缩。
func (d *Driver) Compacting() bool { return d.busy == BusyCompacting }

// DisplayNotice 是**只用于显示**的提示（不写日志）。
type DisplayNotice struct {
	Text string
}

func (DisplayNotice) effectKind() string { return "displayNotice" }

// autoCompactEffects 是「回合正常结束后是否要自动压缩」的唯一决策点。
//
// 策略放在 driver 里（而不是 UI）才能被测试，也能避免这些坑：
//   - 压缩失败后回到 Idle 又触发一次 → 无限重试、持续烧 token；
//   - 压缩成功后仍高于阈值 → 每回一次 Idle 就再压一次；
//   - 用户按了停止 → 立刻又开始压缩，和意图相反。
//
// 因此：只有**用户回合正常结束**才检查；每轮最多自动压缩一次；压缩后仍超阈值
// 只给一条提示，不重试。
// force 为真时跳过阈值判断：服务端说「超出窗口」本身就说明本地估算偏低，
// 这时候按估算值决定要不要压缩，会导致用户重试多少次都失败。
func (d *Driver) autoCompactEffects(force bool) []Effect {
	if d.opts.DisableAutoCompact || d.autoCompactUsed {
		return nil
	}
	if !force && !d.ShouldCompact() {
		return nil
	}
	effects, err := d.BeginCompact()
	if err != nil {
		// 没有可压的内容：不再尝试
		d.autoCompactUsed = true
		return nil
	}
	d.autoCompactUsed = true
	return effects
}

// afterCompactNotice 在「压缩后仍然接近上限」时给一条提示（不再重试）。
func (d *Driver) afterCompactNotice() []Effect {
	if !d.ShouldCompact() {
		return nil
	}
	text := d.tr("notice.stillNearLimit")
	_, _ = d.conv.AddNotice(conversation.NoticeWarn, text)
	return []Effect{recordNotice(conversation.NoticeWarn, text)}
}

// rebuildFromSnapshot 用快照重建内存会话（重放用）。
func rebuildFromSnapshot(data CompactSnapshotData) (*conversation.Conversation, error) {
	conv := conversation.New("")
	summaryID := data.SummaryID
	if summaryID == "" {
		generated, err := conversation.NewMessageID()
		if err != nil {
			return nil, err
		}
		summaryID = generated
	}
	summary := message.Message{
		MessageID: summaryID,
		Role:      message.RoleUser,
		Parts: []message.Part{{
			Content:   message.TextPart{Text: "以下是此前对话的摘要：\n\n" + data.Summary},
			MediaType: render.MediaSummary,
		}},
	}
	if _, err := conv.AddMessage(summary, conversation.StatusComplete); err != nil {
		return nil, err
	}
	for _, entry := range data.Kept {
		status := entry.Status
		if status == "" {
			status = conversation.StatusComplete
		}
		if _, err := conv.AddMessage(entry.Message, status); err != nil {
			return nil, err
		}
	}
	return conv, nil
}
