// Package driver 是会话编排状态机：把用户操作与提供商事件翻译成会话变化，
// 并把"接下来该做什么"以 Effect 形式交给调用方（uiapp）。
//
// 三条硬约束（方案 §4 / §9）：
//
//  1. **状态只在调用方的主线程改**：driver 自己不起 goroutine、不碰 UI。需要
//     异步的工作（发请求、跑工具）由调用方执行，结果再经 ApplyEvent /
//     ToolResults 回到这里。因此 driver 可以被普通 go test 直接驱动。
//  2. **迟到的流事件必须被丢弃**：按当前 turn 的 messageID 校验（ownsStream），
//     否则上一轮的 fail/closed 会污染下一轮。
//  3. **停止时补全未配对的 tool_use**：给每个没有结果的调用写一条 is_error 结果，
//     否则后续每条请求都会被两家 provider 以"tool_use 无配对结果"拒绝，会话报废。
package driver

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	json "encoding/json/v2"

	"simple-agent/internal/conversation"
	"simple-agent/internal/locale"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/render"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// Busy 是编排状态。
type Busy string

const (
	// BusyIdle 空闲：可以发送。
	BusyIdle Busy = "idle"
	// BusyStreaming 正在接收模型流。
	BusyStreaming Busy = "streaming"
	// BusyRunningTools 正在执行工具（此状态禁止发送）。
	BusyRunningTools Busy = "runningTools"
	// BusyAwaitingAnswer 等待用户回答问题。
	BusyAwaitingAnswer Busy = "awaitingAnswer"
	// BusyCompacting 正在压缩上下文。
	BusyCompacting Busy = "compacting"
)

// ErrBusy 表示当前状态不接受该操作。
var ErrBusy = errors.New("当前状态不接受该操作")

// Effect 是 driver 交给调用方执行的后续动作。
type Effect interface{ effectKind() string }

// StartStream 让调用方发起一次流式请求（goroutine 由调用方管理）。
type StartStream struct {
	// MessageID 是本轮 agent 消息 id；回传事件时要用它校验。
	MessageID string
	Request   providers.ChatRequest
}

func (StartStream) effectKind() string { return "startStream" }

// RunTools 让调用方执行工具调用。
type RunTools struct {
	MessageID string
	Calls     []tools.Call
}

func (RunTools) effectKind() string { return "runTools" }

// Changed 表示状态有变化，需要重绘。
type Changed struct{}

func (Changed) effectKind() string { return "changed" }

// Options 是 driver 的运行配置。
type Options struct {
	Selection    providers.Selection
	SystemPrompt string
	Tools        []*tools.ToolDef
	Temperature  *float64
	MaxTokens    int
	// ContextWindow 是当前模型的上下文窗口（token）。0 表示未知（按 32k 处理）。
	ContextWindow int
	// MaxOutputTokens 是单次回复的最大输出 token 数（0 = 不限制）。
	MaxOutputTokens int
	// Blobs 把 `blob:sha256:<hex>` 引用读成字节。请求在异步 goroutine 里组装，
	// 所以读取发生在那里（不在主线程）；nil 或读失败时降级成「[图片丢失]」。
	Blobs providers.BlobReader
	// Vision 表示当前模型是否支持图片（配置里 `vision: true` 才算支持）。
	Vision bool
	// Workspace 是这条会话使用的工具工作区：写进 session.created，侧栏按它分组。
	Workspace string
	// Tr 查界面文案（通知等写进日志的用户可见文字按当前界面语言写入）。
	// nil 时用进程级默认语言（locale.Tr）。
	Tr func(key string, args ...any) string
	// ReasoningEffort 是当前思考档位（"" 沿用模型配置，"off" 显式关闭）。
	ReasoningEffort string
	// DisableAutoCompact 关闭自动压缩（只留手动按钮）。
	// 给「不希望后台花 token」的用户，也让测试能单独驱动压缩流程。
	DisableAutoCompact bool
	// MaxToolRounds 限制一次用户输入内最多进行几轮工具调用（0 = 不限）。
	//
	// 这是安全阀：没有上限时模型可能无限「调用工具 → 再调用」，一次用户输入
	// 能跑掉几十轮、烧掉大量 token。达到上限时给一条通知并回到 Idle
	// （Rust 阶段修过的 P0 就是这么处理的）。
	MaxToolRounds int
}

// Usage 是本轮 token 用量。
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// toolBuffer 累积一个工具调用的流式分片。
type toolBuffer struct {
	id    string
	name  string
	args  strings.Builder
	ended bool
	order int
}

// Turn 是一轮对话的进行中状态。
type Turn struct {
	messageID string
	// startedAt 是本轮开始的时间（回合结束行算耗时用）。
	startedAt time.Time
	buffers   map[int]*toolBuffer
	nextOrder int
	// toolParts 是各工具调用对应的 part 下标（index → part）。
	toolParts map[int]int
	// reasoningPart 是推理 part 的下标（-1 表示还没有）。
	//
	// 推理必须**边收边落盘**：UI 从 Conversation 渲染，缓冲到结束才落盘会让
	// 用户在整个思考阶段看到空白（deepseek-reasoner 这类模型能思考几十秒）。
	reasoningPart int
	depth         int
	// asks 是本轮等待回答的提问组（同一轮可能有多个 ask 调用）。
	asks []*AskGroup
	// pendingToolResults 是本批还剩几个工具调用没返回结果。
	// 工具串行执行，每完成一个就回填一个，界面能看到逐个完成。
	pendingToolResults int
	usage              Usage
	// sawStop 记录收到过 Done（区分正常结束与连接中断）。
	sawStop bool
}

// Driver 是会话编排器。
type Driver struct {
	// mirror 为真时表示这是展示端的只读镜像：只应用增量，不做任何决策
	// （不结束消息、不补结果、不投递插话、不写日志）。
	mirror bool
	conv   *conversation.Conversation
	opts   Options

	title string
	busy  Busy
	turn  *Turn
	// queued 是回合进行中用户插话排队的内容（steer）。
	//
	// **不进 Conversation**：忙时若立刻追加 user 消息，会话顺序会变成
	// [user1, agent1(streaming), user2]——界面会把正在流式的回答塞进折叠组，
	// 重放出的顺序也与实际发生顺序不符。因此排队只放在这里，投递时才走
	// appendUserText（与 Send 共用同一条构造/落盘路径）。
	//
	// 队列**不落盘**：它的语义是"还没说出口的话"，与草稿同级。进程崩了就没了，
	// 这是有意的取舍（写事件会改动 §6.2 的事件清单与重放语义）。
	queued []string
	// visionNotified 记录「哪些模型已经提示过『不支持图片』」：同一个会话里
	// 同一个模型只提示一次（进程内，不落盘）。
	visionNotified map[string]bool
	// pendingNotice 是下一次返回 effects 时要带出去的显示型提示。
	pendingNotice *DisplayNotice
	// depth 是当前回话已经进行的工具轮数（回合内递增）。
	depth int
	// pendingRecords 是等待调用方取走的事件（没有 effect 返回值的操作用）。
	pendingRecords []Effect
	// pendingCompact 是正在进行的压缩（BeginCompact 之后、ApplyCompact 之前）。
	pendingCompact *pendingCompact

	// usageBase/historyLenAtUsage 记录「最近一次真实 usage」以及它对应的历史长度。
	// 之后新增的消息再估算，避免每次都把整段历史重新估算（也避免低估 system+tools）。
	usageBase         int
	historyLenAtUsage int
	hasUsage          bool
	// autoCompactUsed 表示本轮用户输入已经自动压缩过一次，不再重试。
	autoCompactUsed bool
	// warnedDefaultWindow 表示已经提示过「窗口未知，按默认值处理」。
	warnedDefaultWindow bool

	// turns/steps 是会话级累计计数（状态条显示用），口径见 Counts。
	//
	// **不单独落盘**：它们可以从会话内容推导（turns = user 消息条数，
	// steps = agent 消息条数），重放因此天然一致；唯一会丢失历史的动作是
	// 压缩，所以压缩快照里带上当时的累计值当基线（与 kept 一起重建，见 apply).
	turns int
	steps int
}

// Counts 是会话级累计计数（底部状态条显示用）。
//
// 口径（与重放推导完全一致，测试守着）：
//   - Turns = **用户输入条数**（每次 Send 一条；一次投递的多条插话算多条）；
//   - Steps = **发给模型的请求次数**（每次 startTurn 一条，工具续轮也各算一次）。
//
// 之所以按"消息条数"而不是"用户按了几次发送"定义：压缩会丢弃历史，
// 能重放的只有消息本身；口径与重放一致才谈得上「重放 == 在线」。
type Counts struct {
	Turns int
	Steps int
}

// Counts 返回会话级累计计数。
func (d *Driver) Counts() Counts { return Counts{Turns: d.turns, Steps: d.steps} }

// Todos 返回当前任务清单（空清单返回 nil）。
func (d *Driver) Todos() []conversation.TodoItem { return d.conv.Todos() }

// SyncTodos 用工具执行后的清单同步会话状态，并在**真的变了**时记一条事件。
//
// 为什么由这里出面而不是让工具直接改会话：工具层拿不到会话，且清单的持久化是
// driver 的职责（重放要能重建）。比较后再写，避免每次工具轮都刷一条无变化事件。
func (d *Driver) SyncTodos(todos []conversation.TodoItem) []Effect {
	if slices.Equal(d.conv.Todos(), todos) {
		return nil
	}
	d.conv.SetTodos(todos)
	return []Effect{recordTodos(d.conv.Todos()), Changed{}}
}

// clearTodos 清空任务清单并记录（用户发新消息时调用，对齐 dsh 在 `turn/start`
// 清空的语义：上一轮的计划不跨轮）。
//
// 只在**用户输入**这个边界清：工具续轮也走 startTurn，在那里清会把同一轮的清单
// 中途抹掉（dsh 的 turn/end 也是保留清单可见的）。
func (d *Driver) clearTodos() []Effect {
	if len(d.conv.Todos()) == 0 {
		return nil
	}
	d.conv.SetTodos(nil)
	return []Effect{recordTodos(nil)}
}

// New 构造 driver（初始 Idle）。
func New(conv *conversation.Conversation, opts Options) *Driver {
	return &Driver{conv: conv, opts: opts, busy: BusyIdle}
}

// Conversation 返回会话（只读用途）。
func (d *Driver) Conversation() *conversation.Conversation { return d.conv }

// Busy 返回当前状态。
func (d *Driver) Busy() Busy { return d.busy }

// SetContextWindow 更新当前模型的上下文窗口（切换模型后由调用方同步）。
func (d *Driver) SetContextWindow(window int) { d.opts.ContextWindow = window }

// tr 查文案：优先用调用方注入的 Tr，其次进程级默认语言。
func (d *Driver) tr(key string, args ...any) string {
	if d.opts.Tr != nil {
		return d.opts.Tr(key, args...)
	}
	return locale.Tr(key, args...)
}

// ContextWindow 返回当前使用的上下文窗口。
func (d *Driver) ContextWindow() int { return d.opts.ContextWindow }

// Vision 报告当前模型是否支持图片。
func (d *Driver) Vision() bool { return d.opts.Vision }

// SetVision 更新当前模型是否支持图片（切换模型后由调用方同步）。
func (d *Driver) SetVision(vision bool) { d.opts.Vision = vision }

// SetBlobs 更新 blob 读取器（启动时注入）。
func (d *Driver) SetBlobs(blobs providers.BlobReader) { d.opts.Blobs = blobs }

// SetMaxOutputTokens 更新当前模型的最大输出 token 数（切换模型后由调用方同步）。
func (d *Driver) SetMaxOutputTokens(limit int) { d.opts.MaxOutputTokens = limit }

// SetReasoningEffort 更新思考档位并记录事件（重放后仍然生效）。
func (d *Driver) SetReasoningEffort(effort string) []Effect {
	if d.opts.ReasoningEffort == effort {
		return nil
	}
	d.opts.ReasoningEffort = effort
	return []Effect{recordSelectionWithEffort(d.opts.Selection, effort)}
}

// ReasoningEffort 返回当前思考档位。
func (d *Driver) ReasoningEffort() string { return d.opts.ReasoningEffort }

// Selection 返回当前模型选择。
func (d *Driver) Selection() providers.Selection { return d.opts.Selection }

// SetSelection 切换模型（仅空闲时允许）。
func (d *Driver) SetSelection(sel providers.Selection) error {
	if d.busy != BusyIdle {
		return ErrBusy
	}
	d.opts.Selection = sel
	d.pendingRecords = append(d.pendingRecords, recordSelectionWithEffort(sel, d.opts.ReasoningEffort))
	return nil
}

// TakeRecords 取出待写日志的事件（SetSelection 这类没有 effect 返回值的操作用）。
func (d *Driver) TakeRecords() []Effect {
	out := d.pendingRecords
	d.pendingRecords = nil
	return out
}

// OwnsStream 报告 messageID 是否属于当前进行中的流。
//
// 迟到的流事件必须用它过滤（方案 §9 第 3 条）。
func (d *Driver) OwnsStream(messageID string) bool {
	return d.turn != nil && d.turn.messageID == messageID
}

// CurrentMessageID 返回当前轮的消息 id（无进行中的轮时为空）。
func (d *Driver) CurrentMessageID() string {
	if d.turn == nil {
		return ""
	}
	return d.turn.messageID
}

// Usage 返回本轮用量。
func (d *Driver) Usage() Usage {
	if d.turn == nil {
		return Usage{}
	}
	return d.turn.usage
}

// CanSend 报告现在能否发送。
func (d *Driver) CanSend() bool { return d.busy == BusyIdle }

// CanSteer 报告现在能否插话（回合进行中追加一条用户消息）。
//
// 两个状态可以插话：
//   - BusyStreaming：模型正在生成，投递点是本轮结束后的下一个安全边界；
//   - BusyRunningTools：工具正在执行，投递点是本批结果齐了之后。
//
// BusyAwaitingAnswer 不行：那时输入框里的文字是「自定义答案」的语义，
// 插话会与作答打架。BusyCompacting 同理（压缩期间不改会话内容）。
func (d *Driver) CanSteer() bool {
	return d.busy == BusyStreaming || d.busy == BusyRunningTools
}

// Enqueue 记下一条插话，等下一个安全边界投递给模型。
//
// 只改队列，**不动 Conversation、不落盘、不开流**：这就是它与 Send 的核心区别。
// 消息在真正投递之前，历史与界面都不该看到它。
func (d *Driver) Enqueue(text string) ([]Effect, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("消息为空")
	}
	if !d.CanSteer() {
		return nil, ErrBusy
	}
	d.queued = append(d.queued, text)
	return []Effect{Changed{}}, nil
}

// Queued 返回排队中的插话（副本）。
func (d *Driver) Queued() []string { return slices.Clone(d.queued) }

// HasQueued 报告是否有排队的插话。
func (d *Driver) HasQueued() bool { return len(d.queued) > 0 }

// ClearQueued 清空队列（用户停止生成时调用，与「停下来就不再自动动作」一致）。
func (d *Driver) ClearQueued() []Effect {
	if len(d.queued) == 0 {
		return nil
	}
	d.queued = nil
	return []Effect{Changed{}}
}

// DropQueued 撤回排队中的第 index 条插话（下标越界时忽略）。
func (d *Driver) DropQueued(index int) []Effect {
	if index < 0 || index >= len(d.queued) {
		return nil
	}
	d.queued = append(d.queued[:index], d.queued[index+1:]...)
	return []Effect{Changed{}}
}

// Send 追加一条用户消息并开始新一轮。
//
// 原子操作：忙时既不追加消息也不开流（方案 §9 第 13 条）。
//
// 空闲时若队列里还留着插话（回合失败后保留的那种），会**先按顺序把它们作为
// user 消息追加**，再追加本次输入，然后只开一轮流。这样历史顺序是
// 「插话A → 插话B → 这次输入」，而不是等下一轮结束后再补发。
func (d *Driver) Send(text string) ([]Effect, error) {
	text = strings.TrimSpace(text)
	if d.busy != BusyIdle {
		return nil, ErrBusy
	}
	queued := d.queued
	if text == "" && len(queued) == 0 {
		return nil, fmt.Errorf("消息为空")
	}
	all := slices.Clone(queued)
	if text != "" {
		all = append(all, text)
	}

	effects, err := d.appendUserTexts(all)
	if err != nil {
		return nil, err
	}
	// 用户的**新输入**是清单的生命周期边界（对齐 dsh 在 turn/start 清空）：
	// 上一轮的计划不该带到这一轮。插话（steer）不走这里，所以同轮中途插话
	// 不会把正在用的清单抹掉。
	cleared := d.clearTodos()
	d.queued = nil
	d.depth = 0
	d.autoCompactUsed = false
	turnEffects, err := d.startTurn()
	if err != nil {
		return nil, err
	}
	return append(append(effects, cleared...), turnEffects...), nil
}

// SendWithAttachments 追加一条"文字 + 图片"的 user 消息并开始新一轮。
//
// 图片以 `blob:sha256:` 引用进入消息（字节已经在 blob 存储里），请求组装时才
// 读成字节。模型不支持图片时直接拒绝：这是界面的兜底守卫，不能只靠按钮禁用。
func (d *Driver) SendWithAttachments(text string, attachments []message.Part) ([]Effect, error) {
	if d.busy != BusyIdle {
		return nil, ErrBusy
	}
	if len(attachments) > 0 && !d.opts.Vision {
		return nil, fmt.Errorf("当前模型不支持图片")
	}
	text = strings.TrimSpace(text)
	queued := d.queued
	if text == "" && len(queued) == 0 && len(attachments) == 0 {
		return nil, fmt.Errorf("消息为空")
	}

	all := slices.Clone(queued)
	if text != "" || len(attachments) > 0 {
		all = append(all, text)
	}
	effects, err := d.appendUserTextsWithLastParts(all, attachments)
	if err != nil {
		return nil, err
	}
	// 同 Send：用户的新输入清空上一轮的任务清单
	cleared := d.clearTodos()
	d.queued = nil
	d.depth = 0
	d.autoCompactUsed = false
	turnEffects, err := d.startTurn()
	if err != nil {
		return nil, err
	}
	return append(append(effects, cleared...), turnEffects...), nil
}

// appendUserTextsWithLastParts 追加多条 user 消息，最后一条额外带 parts。
func (d *Driver) appendUserTextsWithLastParts(texts []string, extra []message.Part) ([]Effect, error) {
	ids := make([]string, len(texts))
	for index := range texts {
		id, err := conversation.NewMessageID()
		if err != nil {
			return nil, err
		}
		ids[index] = id
	}
	var effects []Effect
	for index, text := range texts {
		var parts []message.Part
		if index == len(texts)-1 {
			parts = extra
		}
		recorded, err := d.addUserMessageWithParts(ids[index], text, parts)
		if err != nil {
			return effects, err
		}
		effects = append(effects, recorded...)
	}
	return effects, nil
}

// appendUserTexts 依次追加多条 user 消息。
//
// 先把 id 全部生成好再动会话：中途失败不会留下"追加了一半"的状态，
// 队列也因此能原样保留（失败的调用方负责把队列放回去）。
func (d *Driver) appendUserTexts(texts []string) ([]Effect, error) {
	ids := make([]string, len(texts))
	for index := range texts {
		id, err := conversation.NewMessageID()
		if err != nil {
			return nil, err
		}
		ids[index] = id
	}
	var effects []Effect
	for index, text := range texts {
		recorded, err := d.addUserMessage(ids[index], text)
		if err != nil {
			return effects, err
		}
		effects = append(effects, recorded...)
	}
	return effects, nil
}

// appendUserText 追加一条 user 消息并返回它的落盘事件。
//
// Send 与插话投递共用这一处：两条路径都以同一种方式构造消息、记录事件，
// 否则很容易漏掉落盘或漏掉 clone 语义。
func (d *Driver) appendUserText(text string) ([]Effect, error) {
	userID, err := conversation.NewMessageID()
	if err != nil {
		return nil, err
	}
	return d.addUserMessage(userID, text)
}

// addUserMessage 用给定 id 追加一条 user 消息。
func (d *Driver) addUserMessage(userID, text string) ([]Effect, error) {
	return d.addUserMessageWithParts(userID, text, nil)
}

// addUserMessageWithParts 追加一条 user 消息（文本可空，extra 追加在后面）。
func (d *Driver) addUserMessageWithParts(userID, text string, extra []message.Part) ([]Effect, error) {
	parts := make([]message.Part, 0, len(extra)+1)
	if text != "" || len(extra) == 0 {
		parts = append(parts, message.NewTextPart(text))
	}
	parts = append(parts, extra...)
	user := message.Message{
		MessageID: userID,
		Role:      message.RoleUser,
		Parts:     parts,
	}
	if _, err := d.conv.AddMessage(user, conversation.StatusComplete); err != nil {
		return nil, err
	}
	// 用户输入条数即轮数（状态条口径见 Counts）
	d.turns++
	return []Effect{recordMessage(user, conversation.StatusComplete, "")}, nil
}

// deliverQueued 把排队的插话一次性投递出去（每条一条 user 消息），然后开下一轮流。
//
// 用户决定：**同一时刻的多条插话作为多个 user 消息一起投递**，不拼成一条。
// 因此历史里会出现连续的 user 消息（tool_result 也是 user 角色）。
// OpenAI 接受连续 user；Anthropic 官方说明会把相邻同角色消息合并成一个 turn，
// 所以两家都能跑。**这里依赖了 Anthropic 的这个宽容度**——同角色合并没有在
// ToAnthropicMessages 里实现（那是适配器自己的职责，另行处理）。
//
// 投递点必须满足"工具配对完整"（见 continueAfterTools/finishStream/ApplyCompact
// 三处调用），否则两家协议都会以「tool_use 无配对结果」拒收。
func (d *Driver) deliverQueued() ([]Effect, bool) {
	if len(d.queued) == 0 {
		return nil, false
	}
	pending := d.queued
	d.queued = nil

	var effects []Effect
	for index, text := range pending {
		appended, err := d.appendUserText(text)
		if err != nil {
			// 生成 id 失败这类极端情况：把**这条以及它后面的**都放回队列，
			// 别静默吞掉用户的话（之前的写法只放回当前这一条，后面全丢了）。
			d.queued = append(slices.Clone(pending[index:]), d.queued...)
			return append(effects, Changed{}), true
		}
		effects = append(effects, appended...)
	}
	// 插话是**新的用户输入**：工具轮数上限按输入计，必须归零，
	// 否则长回合末尾插一句话会立刻撞上 max_tool_rounds；
	// 自动压缩的额度也随新输入重置（与 Send 的口径保持一致）。
	d.depth = 0
	d.autoCompactUsed = false
	next, err := d.startTurn()
	if err != nil {
		return append(effects, Changed{}), true
	}
	return append(effects, next...), true
}

// startTurn 新建 agent 消息并发起流。
func (d *Driver) startTurn() ([]Effect, error) {
	agentID, err := conversation.NewMessageID()
	if err != nil {
		return nil, err
	}
	agent := message.Message{MessageID: agentID, Role: message.RoleAgent}
	entry, err := d.conv.AddMessage(agent, conversation.StatusPending)
	if err != nil {
		return nil, err
	}

	turn := &Turn{
		messageID:     entry.Message.MessageID,
		startedAt:     time.Now(),
		buffers:       map[int]*toolBuffer{},
		toolParts:     map[int]int{},
		reasoningPart: -1,
		depth:         d.depth + 1,
	}
	d.turn = turn
	d.depth = turn.depth
	d.busy = BusyStreaming
	// 每发起一次请求算一步（工具续轮、插话续轮都各算一步，见 Counts）
	d.steps++

	request := d.buildRequest()
	effects := []Effect{
		// 流开始时写一条 streaming 空壳：进程若在生成中途被杀，重放会把它
		// 标成「已中断」占位（用户明确要求的行为）
		recordMessage(agent, conversation.StatusStreaming, ""),
		StartStream{MessageID: turn.messageID, Request: request},
	}
	// buildRequest 可能因为"模型不支持图片"而挂起一条一次性提示
	effects = append(effects, d.takeDisplayNotice()...)
	return append(effects, Changed{}), nil
}

// buildRequest 构造请求（历史只含正常完成、且不含 pending 占位的消息）。
// liveRawArgsLimit 是流式期间落进 part 的参数前缀长度。
//
// 界面只从中取一个摘要值（file_path/command/pattern…），都在开头；完整参数在
// 回合结束时由 collectToolCalls 从缓冲区一次性解析，因此截断不影响执行。
const liveRawArgsLimit = 2048

// clipRawArgs 截取流式参数的可用前缀。
func clipRawArgs(raw string) string {
	if len(raw) <= liveRawArgsLimit {
		return strings.TrimSpace(raw)
	}
	// 直接在字节边界截断可能切在多字节字符中间：退回到最后一个完整 rune
	cut := liveRawArgsLimit
	for cut > 0 && !utf8.RuneStart(raw[cut]) {
		cut--
	}
	return strings.TrimSpace(raw[:cut])
}

func (d *Driver) buildRequest() providers.ChatRequest {
	// 请求会交给**另一个 goroutine** 去 marshal（开流是异步的），而主线程随后
	// 还会继续改会话。History() 只做浅拷贝（DataPart 的 map 与条目共享），
	// 因此这里必须深拷贝一份，否则 -race 能抓到真实的数据竞争。
	history := deepCopyMessages(providers.HistoryForRequest(d.conv.History()))
	history, replacedImages := replaceImagesForModel(history, d.opts.Vision)
	history, strippedToolImages := stripToolResultImages(history, d.opts.Vision)
	if strippedToolImages > 0 {
		replacedImages += strippedToolImages
	}
	if replacedImages > 0 {
		d.noteVisionImages(replacedImages)
	}
	req := providers.ChatRequest{
		Model:       d.opts.Selection.Model,
		System:      d.opts.SystemPrompt,
		Messages:    history,
		Temperature: d.opts.Temperature,
		MaxTokens:   d.opts.MaxTokens,
	}
	// 模型自己的输出上限优先于全局配置
	if d.opts.MaxOutputTokens > 0 {
		req.MaxTokens = d.opts.MaxOutputTokens
	}
	// 图片在异步开流的 goroutine 里读（这里只传引用）
	req.Blobs = d.opts.Blobs
	req.ReasoningEffort = d.opts.ReasoningEffort
	for _, tool := range d.opts.Tools {
		req.Tools = append(req.Tools, providers.ToolSpec{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.Parameters,
		})
	}
	return req
}

// stripToolResultImages 在模型不支持图片时，去掉工具结果里挂的图片（read_image）。
//
// 图片藏在 tool-result 的 data 里（`images: [...]`），不是独立的 part，所以
// replaceImagesForModel 看不到它们；不剥掉就会把图片发给不收图片的模型 → 400。
func stripToolResultImages(history []message.Message, vision bool) ([]message.Message, int) {
	if vision {
		return history, 0
	}
	stripped := 0
	for index, msg := range history {
		for partIndex, part := range msg.Parts {
			if part.MediaType != render.MediaToolResult {
				continue
			}
			data, ok := partData(part)
			if !ok {
				continue
			}
			images, _ := data["images"].([]any)
			if len(images) == 0 {
				continue
			}
			delete(data, "images")
			output, _ := data["output"].(string)
			data["output"] = output + "\n（图片已省略：当前模型不支持图片输入）"
			history[index].Parts[partIndex] = message.NewDataPart(data).WithMediaType(render.MediaToolResult)
			stripped++
		}
	}
	return history, stripped
}

// replaceImagesForModel 在模型不支持图片时，把历史里的图片换成文字「[图片]」。
//
// 只看 part 的**类型**，不读 blob（读文件在异步开流里做），所以主线程没有 IO。
func replaceImagesForModel(history []message.Message, vision bool) ([]message.Message, int) {
	if vision {
		return history, 0
	}
	replaced := 0
	for index, msg := range history {
		for partIndex, part := range msg.Parts {
			if !isImagePart(part) {
				continue
			}
			history[index].Parts[partIndex] = message.NewTextPart("[图片]")
			replaced++
		}
	}
	return history, replaced
}

// isImagePart 判断一个 part 是不是图片（不读字节）。
func isImagePart(part message.Part) bool {
	switch part.Content.(type) {
	case message.URLPart, message.RawPart:
	default:
		return false
	}
	view := render.Classify(part)
	return view.Kind == render.KindImage
}

// noteVisionImages 记下"这一轮有图片被替换成文字"，由 startTurn 一次性提示。
func (d *Driver) noteVisionImages(count int) {
	if d.visionNotified == nil {
		d.visionNotified = map[string]bool{}
	}
	if d.visionNotified[d.opts.Selection.Model] {
		return
	}
	d.visionNotified[d.opts.Selection.Model] = true
	d.pendingNotice = &DisplayNotice{Text: d.tr("notice.visionImages", count)}
}

// takeDisplayNotice 取出挂起的一次性提示。
func (d *Driver) takeDisplayNotice() []Effect {
	if d.pendingNotice == nil {
		return nil
	}
	notice := *d.pendingNotice
	d.pendingNotice = nil
	return []Effect{notice}
}

// deepCopyMessages 深拷贝消息列表（含 part 里的 map / slice）。
func deepCopyMessages(messages []message.Message) []message.Message {
	out := make([]message.Message, len(messages))
	for index, msg := range messages {
		out[index] = msg
		parts := make([]message.Part, len(msg.Parts))
		for partIndex, part := range msg.Parts {
			parts[partIndex] = deepCopyPart(part)
		}
		out[index].Parts = parts
	}
	return out
}

// deepCopyPart 深拷贝一个 part。
func deepCopyPart(part message.Part) message.Part {
	switch content := part.Content.(type) {
	case message.DataPart:
		part.Content = message.DataPart{Data: deepCopyValue(content.Data)}
	case message.RawPart:
		if content.Raw != nil {
			part.Content = message.RawPart{Raw: slices.Clone(content.Raw)}
		}
	}
	return part
}

// deepCopyValue 递归复制 map / slice（JSON 风格的数据结构）。
func deepCopyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copied := make(map[string]any, len(typed))
		for key, item := range typed {
			copied[key] = deepCopyValue(item)
		}
		return copied
	case []any:
		copied := make([]any, len(typed))
		for index, item := range typed {
			copied[index] = deepCopyValue(item)
		}
		return copied
	default:
		return value
	}
}

// ApplyEvent 应用一条流事件。
//
// messageID 不属于当前轮时直接丢弃（返回 nil，无副作用）。
func (d *Driver) ApplyEvent(messageID string, ev providers.Event) ([]Effect, error) {
	// 拆两半：增量（模型已经说出来的内容）+ 终态（回合推进）。
	// 拆开是为了让展示端的镜像只应用增量那一半——终态会直接改 Conversation
	// （补工具结果、投递插话、加通知…），两侧各做一遍就会重复（见设计 §3.3.2）。
	effects, err := d.ApplyStreamDelta(messageID, ev)
	if err != nil {
		return effects, err
	}
	if !d.OwnsStream(messageID) || d.busy != BusyStreaming {
		return effects, nil
	}
	switch ev.Kind {
	case providers.EventDone, providers.EventError:
		return append(effects, d.FinishStream(messageID, ev)...), nil
	}
	return effects, nil
}

// ApplyStreamDelta 只应用"模型已经说出来的内容"：文字、推理、工具调用的
// name 与参数增量。**不碰** Busy、不补工具结果、不加通知、不写日志、不投递插话。
//
// 展示端的镜像直接用它（见 Mirror）；执行端由 ApplyEvent 调用。
func (d *Driver) ApplyStreamDelta(messageID string, ev providers.Event) ([]Effect, error) {
	if !d.OwnsStream(messageID) || (d.busy != BusyStreaming && !d.mirror) {
		return nil, nil
	}
	turn := d.turn

	switch ev.Kind {
	case providers.EventText:
		if ev.Text == "" {
			return nil, nil
		}
		if err := d.conv.AppendToStream(messageID, ev.Text); err != nil {
			return nil, err
		}
		return []Effect{Changed{}}, nil

	case providers.EventReasoning:
		if ev.Text == "" {
			return nil, nil
		}
		entry, ok := d.conv.Message(messageID)
		if !ok {
			return nil, nil
		}
		if turn.reasoningPart < 0 {
			turn.reasoningPart = entry.AddPart(message.Part{
				Content:   message.TextPart{Text: ev.Text},
				MediaType: render.MediaReasoning,
			})
			if err := d.conv.MarkStreaming(messageID); err != nil {
				return nil, err
			}
			return []Effect{Changed{}}, nil
		}
		// 用按 part 追加的接口：不能挪用 streamingPart，否则正文与推理会互相串
		if err := d.conv.AppendToPart(messageID, turn.reasoningPart, ev.Text); err != nil {
			return nil, err
		}
		return []Effect{Changed{}}, nil

	case providers.EventToolStart:
		buf := turn.buffer(ev.Index)
		// 同一 index 的 ToolStart 可能重复出现（有些网关每个分片都带 id/name），
		// 按 index 合并而不是新建调用。
		if ev.ID != "" && buf.id == "" {
			buf.id = ev.ID
		}
		if ev.Name != "" && buf.name == "" {
			buf.name = ev.Name
		}
		// 立刻落一个占位 part：界面上要能马上看到"正在调用什么"，
		// 而不是等整轮结束才出现。
		if entry, ok := d.conv.Message(messageID); ok {
			d.ensureToolPart(entry, turn, ev.Index)
		}
		return []Effect{Changed{}}, nil

	case providers.EventToolArgs:
		buf := turn.buffer(ev.Index)
		buf.args.WriteString(ev.Delta)
		// 增量也落盘：标题要能像 dsh 一样边收边显示"正在读取 docs/plan.md"。
		//
		// 只落**前一部分**（界面摘要用到的 key 都在开头）：整段重写会让
		// 复制量变成 O(n²)——write 写 100KB 文件时几千个增量累计几百 MB。
		if entry, ok := d.conv.Message(messageID); ok {
			partIndex := d.ensureToolPart(entry, turn, ev.Index)
			live := message.Part{
				Content: message.DataPart{Data: map[string]any{
					"id":           buf.id,
					"name":         buf.name,
					"raw_args":     clipRawArgs(buf.args.String()),
					"stream_index": ev.Index,
				}},
				MediaType: render.MediaToolUse,
			}
			// 更新失败只可能是 part 不存在（理论上不会），静默跳过即可
			_ = d.conv.UpdatePart(messageID, partIndex, live)
		}
		return []Effect{Changed{}}, nil

	case providers.EventToolEnd:
		buf := turn.buffer(ev.Index)
		buf.ended = true
		return []Effect{Changed{}}, nil

	case providers.EventUsage:
		if ev.InputTokens > 0 {
			turn.usage.InputTokens = ev.InputTokens
		}
		if ev.OutputTokens > 0 {
			turn.usage.OutputTokens = ev.OutputTokens
		}
		return nil, nil

	}

	return nil, nil
}

// FinishStream 做回合推进：结束/失败当前消息、补工具结果、投递插话、
// 判断自动压缩、记用量。**只有执行端调用**。
func (d *Driver) FinishStream(messageID string, ev providers.Event) []Effect {
	if !d.OwnsStream(messageID) || d.busy != BusyStreaming {
		return nil
	}
	turn := d.turn
	switch ev.Kind {
	case providers.EventDone:
		turn.sawStop = true
		effects := d.finishStream(messageID, "")
		if isTruncated(ev.StopReason) {
			// 截断是用户必须知道的：否则只会看到一句没写完的回答
			text := d.tr("notice.truncated")
			_, _ = d.conv.AddNotice(conversation.NoticeWarn, text)
			effects = append(effects, recordNotice(conversation.NoticeWarn, "回答因长度上限被截断。"))
		}
		return effects
	case providers.EventError:
		reason := "模型请求失败"
		if ev.Err != nil {
			reason = ev.Err.Error()
		}
		return d.failStream(messageID, reason)
	}
	return nil
}

// ensureToolPart 确保某个工具调用已经有对应的 part（没有就新建占位）。
func (d *Driver) ensureToolPart(entry *conversation.ChatEntry, turn *Turn, index int) int {
	if partIndex, ok := turn.toolParts[index]; ok {
		return partIndex
	}
	buf := turn.buffer(index)
	// stream_index 是 **provider 给的下标**（Anthropic 是 content block 下标，
	// 不是"第几个工具"）。镜像中途接管时要靠它把增量对回正确的 part，
	// 不能按出现顺序重新编号——前面有一个 text 块时就会错位。
	data := map[string]any{
		"id": buf.id, "name": buf.name, "args": map[string]any{},
		"stream_index": index,
	}
	partIndex := entry.AddPart(message.Part{
		Content:   message.DataPart{Data: data},
		MediaType: render.MediaToolUse,
	})
	turn.toolParts[index] = partIndex
	return partIndex
}

// buffer 取（或新建）指定 index 的工具缓冲。
func (t *Turn) buffer(index int) *toolBuffer {
	buf, ok := t.buffers[index]
	if !ok {
		buf = &toolBuffer{order: t.nextOrder}
		t.nextOrder++
		t.buffers[index] = buf
	}
	return buf
}

// FailStreamWith 用给定原因把当前流判为失败（开流阶段就失败时用）。
func (d *Driver) FailStreamWith(messageID, reason string) []Effect {
	if !d.OwnsStream(messageID) || d.busy != BusyStreaming {
		return nil
	}
	return d.failStream(messageID, reason)
}

// StreamClosed 报告流在没有终止事件的情况下关闭（连接中断）。
//
// 调用方在 channel 关闭且 driver 仍处于本轮流式状态时调用它，把这一轮判为失败，
// 而不是让它永远停在「正在生成」。
func (d *Driver) StreamClosed(messageID string) []Effect {
	if !d.OwnsStream(messageID) || d.busy != BusyStreaming {
		return nil
	}
	return d.failStream(messageID, "连接意外中断（未收到结束标记）")
}

// finishStream 正常结束：落盘推理与工具调用，决定下一步。
func (d *Driver) finishStream(messageID, _ string) []Effect {
	turn := d.turn
	entry, ok := d.conv.Message(messageID)
	if !ok {
		d.reset()
		return []Effect{Changed{}}
	}

	calls, parts := d.collectToolCalls(turn)
	for _, part := range parts {
		entry.AddPart(part)
	}

	// 无论是否要跑工具，本轮这条消息都已完成（工具结果是它的后续 part）
	if err := d.conv.EndStream(messageID); err != nil && !errors.Is(err, conversation.ErrNotStreaming) {
		d.reset()
		return []Effect{Changed{}}
	}

	// 最终内容一次性写入（流式增量不落日志；参数非法产生的错误结果也在其中）
	effects := []Effect{recordFinalized(*entry.Message, conversation.StatusComplete, "")}

	if len(calls) == 0 {
		usage := turn.usage
		entry.SetFinishedAt(time.Now())
		entry.SetTokens(usage.OutputTokens)
		d.reset()
		// 正常结束：记住真实用量，并检查是否该自动压缩（driver 决策，UI 只执行）
		d.recordUsage(usage)
		compact := d.autoCompactEffects(false)
		if len(compact) > 0 {
			// 回合结束的边界也是插话的投递点，但**自动压缩优先**：
			// 排队的插话留在队列里，压缩完成后由 ApplyCompact 再投递。
			// 若这里直接投递，回合永远回不到 Idle，自动压缩就再也没机会执行。
			effects = append(effects, compact...)
			effects = append(effects, Changed{})
			return effects
		}
		if delivery, delivered := d.deliverQueued(); delivered {
			effects = append(effects, delivery...)
			effects = append(effects, Changed{})
			return effects
		}
		effects = append(effects, Changed{})
		return effects
	}

	entry.SetFinishedAt(time.Now())
	d.busy = BusyRunningTools
	d.turn.pendingToolResults = len(calls)
	return append(effects,
		RunTools{MessageID: messageID, Calls: calls},
		Changed{},
	)
}

// collectToolCalls 把缓冲转成调用与对应的 tool_use part。
//
// 参数是分片 JSON，必须累积到结束才解析；解析失败保留原文并在这里就标成
// 带错误的调用（由调用方拿到错误结果，模型可自行纠正）。
func (d *Driver) collectToolCalls(turn *Turn) ([]tools.Call, []message.Part) {
	if len(turn.buffers) == 0 {
		return nil, nil
	}

	order := make([]int, 0, len(turn.buffers))
	for index := range turn.buffers {
		order = append(order, index)
	}
	slices.Sort(order)

	calls := make([]tools.Call, 0, len(order))
	parts := make([]message.Part, 0, len(order)*2)

	for _, index := range order {
		buf := turn.buffers[index]
		name := buf.name
		if name == "" {
			// 没有名字的调用无法执行：保留下来让历史配对完整
			name = "unknown"
		}
		id := buf.id
		if id == "" {
			id = fmt.Sprintf("call_%s_%d", turn.messageID, index)
		}

		raw := strings.TrimSpace(buf.args.String())
		var args map[string]any
		var parseErr error
		if raw == "" {
			args = map[string]any{}
		} else if err := json.Unmarshal([]byte(raw), &args); err != nil {
			parseErr = err
			args = map[string]any{}
		}

		data := map[string]any{"id": id, "name": name}
		if raw != "" {
			data["args"] = args
			if len(args) == 0 {
				data["raw_args"] = raw
			}
		} else {
			data["args"] = map[string]any{}
		}
		// part 在 ToolStart 时已经建好；这里只把最终参数补进去（也兼容没有
		// ToolStart 的网关）。
		if entry, ok := d.conv.Message(turn.messageID); ok {
			partIndex := d.ensureToolPart(entry, turn, index)
			final := message.Part{
				Content:   message.DataPart{Data: data},
				MediaType: render.MediaToolUse,
			}
			if err := d.conv.UpdatePart(turn.messageID, partIndex, final); err != nil {
				parts = append(parts, final)
			}
		} else {
			parts = append(parts, message.Part{
				Content:   message.DataPart{Data: data},
				MediaType: render.MediaToolUse,
			})
		}

		if parseErr != nil {
			// 参数坏掉的调用**不执行**：拿 {} 去跑只会得到「缺少 xxx 参数」，
			// 把模型引向错误方向（常见原因是回答被长度上限截断）。
			// 直接给一条写明真实原因的结果。
			parts = append(parts, message.Part{
				Content: message.DataPart{Data: map[string]any{
					"id":   id,
					"name": name,
					"output": fmt.Sprintf("参数不是合法 JSON：%v；原始参数：%s",
						parseErr, clipText(raw, 400)),
					"is_error": true,
				}},
				MediaType: render.MediaToolResult,
			})
			continue
		}

		calls = append(calls, tools.Call{ID: id, Name: name, Args: tools.Args(args)})
	}
	return calls, parts
}

// clipText 截断过长文本（工具结果里的原文回显）。
func clipText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// failStream 把本轮判为失败。
func (d *Driver) failStream(messageID, reason string) []Effect {
	if entry, ok := d.conv.Message(messageID); ok {
		// 已经产生的内容保留：推理与已收到的工具调用也要落盘，界面上才看得到
		// 「模型打算做什么」。落盘后立刻补合成结果，避免历史里出现孤立的 tool_use。
		if d.turn != nil {
			// 推理 part 在流式过程中已经落盘；这里只补上已收到的工具调用
			_, parts := d.collectToolCalls(d.turn)
			for _, part := range parts {
				entry.AddPart(part)
			}
		}
		providers.SettleUnfinishedToolCalls(entry.Message, "已中止："+reason)
		_ = d.conv.FailStream(messageID, reason)
		entry.SetFinishedAt(time.Now())
		// 最终内容一次性写入（已含补齐的结果）
		effects := []Effect{recordFinalized(*entry.Message, conversation.StatusFailed, reason)}
		overflow := isContextOverflow(reason)
		d.reset()
		if overflow {
			// 用户的决定：回合中途因超窗失败 → 回到 Idle 后自动压缩一次，并提示重试。
			// 以**服务端的判断为准**强制压缩（本地估算偏低正是超窗的原因）。
			if compactEffects := d.autoCompactEffects(true); len(compactEffects) > 0 {
				text := d.tr("notice.overflowCompact")
				_, _ = d.conv.AddNotice(conversation.NoticeInfo, text)
				effects = append(effects, recordNotice(conversation.NoticeInfo, text))
				effects = append(effects, compactEffects...)
			}
		}
		return append(effects, Changed{})
	}
	d.reset()
	return []Effect{Changed{}}
}

// ToolResults 回填工具执行结果并继续。
//
// 结果必须与 tool_use 配对；若有等待用户回答的调用，进入 BusyAwaitingAnswer，
// 等 Answer 后再继续。
func (d *Driver) ToolResults(messageID string, outcomes []tools.ToolOutcome) ([]Effect, error) {
	var effects []Effect
	for _, outcome := range outcomes {
		part, err := d.ToolResult(messageID, outcome)
		if err != nil {
			return nil, err
		}
		effects = append(effects, part...)
	}
	return append(effects, d.ToolBatchDone(messageID)...), nil
}

// ToolBatchDone 表示这一批工具调用**都已回填完毕**（可能少了几个，比如执行器
// 被打断）。缺的结果会补成「已中断」错误结果，然后推进回合。
func (d *Driver) ToolBatchDone(messageID string) []Effect {
	if !d.OwnsStream(messageID) || d.busy != BusyRunningTools {
		return nil
	}
	entry, ok := d.conv.Message(messageID)
	if !ok {
		d.reset()
		return nil
	}
	if d.turn != nil {
		d.turn.pendingToolResults = 0
	}
	return d.finishToolBatch(messageID, entry)
}

// ToolResult 回填**一个**工具结果。
//
// 工具是串行执行的：调用方每完成一个就回填一个，界面因而能看到
// 「运行中 → 完成 → 下一个运行中」。整批结果齐了才示意继续下一轮流。
func (d *Driver) ToolResult(messageID string, outcome tools.ToolOutcome) ([]Effect, error) {
	if !d.OwnsStream(messageID) || d.busy != BusyRunningTools {
		// 迟到或重复的结果：丢弃（会话可能已被删除/停止）
		return nil, nil
	}
	entry, ok := d.conv.Message(messageID)
	if !ok {
		d.reset()
		return nil, nil
	}

	data := map[string]any{
		"id":       outcome.ID,
		"name":     outcome.Name,
		"output":   outcome.Output,
		"is_error": outcome.IsError,
	}
	if len(outcome.Images) > 0 {
		images := make([]any, 0, len(outcome.Images))
		for _, image := range outcome.Images {
			images = append(images, map[string]any{
				"ref":        image.Ref,
				"media_type": image.MediaType,
				"width":      image.Width,
				"height":     image.Height,
			})
		}
		data["images"] = images
	}
	if outcome.IsPending() {
		// 占位结果：pending 标记让 providers.HistoryForRequest 把这轮排除
		data["pending"] = true
		data["questions"] = outcome.Pending
	}
	entry.AddPart(message.Part{
		Content:   message.DataPart{Data: data},
		MediaType: render.MediaToolResult,
	})

	effects := []Effect{recordToolResults(messageID, resultRecords([]tools.ToolOutcome{outcome})), Changed{}}

	if d.turn != nil && d.turn.pendingToolResults > 0 {
		d.turn.pendingToolResults--
		if d.turn.pendingToolResults > 0 {
			// 还有工具在跑：不推进回合，让界面先显示这一条已完成
			return effects, nil
		}
	}
	return append(effects, d.finishToolBatch(messageID, entry)...), nil
}

// finishToolBatch 在整批工具结果齐了之后推进回合。
func (d *Driver) finishToolBatch(messageID string, entry *conversation.ChatEntry) []Effect {
	var effects []Effect

	// 有 pending 提问：进入等待回答
	d.turn.asks = d.collectAsks(entry.Message)
	if len(d.turn.asks) > 0 {
		for _, group := range d.turn.asks {
			effects = append(effects, recordAskRaised(messageID, group.CallID, group.Questions))
		}
		d.busy = BusyAwaitingAnswer
		return effects
	}

	// 结果少于调用时（执行器被打断/bug）补一条错误结果，否则下一轮请求会
	// 因为「tool_use 无配对结果」被直接拒收。
	if providers.SettleUnfinishedToolCalls(entry.Message, "工具未返回结果（执行被中断）") {
		text := d.tr("notice.unpairedTools")
		_, _ = d.conv.AddNotice(conversation.NoticeWarn, text)
		effects = append(effects,
			recordToolResults(messageID, partResultRecords(entry.Message)),
			recordNotice(conversation.NoticeWarn, "部分工具未返回结果，已标记为失败。"),
		)
	}
	next, err := d.continueAfterTools()
	if err != nil {
		return effects
	}
	return append(effects, next...)
}

// AskGroups 返回本轮等待回答的提问组（同一轮可能有多个）。
func (d *Driver) AskGroups() []*AskGroup {
	if d.busy != BusyAwaitingAnswer || d.turn == nil {
		return nil
	}
	return d.turn.asks
}

// collectAsks 从消息里收集 pending 的调用，构造提问组。
func (d *Driver) collectAsks(msg *message.Message) []*AskGroup {
	var groups []*AskGroup
	for _, part := range msg.Parts {
		data, ok := partData(part)
		if !ok {
			continue
		}
		if pending, _ := data["pending"].(bool); !pending {
			continue
		}
		id, _ := data["id"].(string)
		groups = append(groups, NewAskGroup(msg.MessageID, id, questionsOf(data)))
	}
	return groups
}

// SubmitAsks 提交全部提问的答案并继续。
//
// custom 非空时作为「第一个没有选中选项的问题」的答案；如果所有问题都选了，
// 它会被放进 note（不静默丢弃）。按 Rust 版 AskGroup 的语义实现。
func (d *Driver) SubmitAsks(custom string) ([]Effect, error) {
	return d.settleAsks(AskCustom, custom)
}

// DismissAsks 跳过全部提问（写入 dismissed 结果）并继续。
func (d *Driver) DismissAsks() ([]Effect, error) {
	return d.settleAsks(AskDismiss, "")
}

// settleAsks 把每个提问组的输出写回对应 pending 占位结果，然后继续。
func (d *Driver) settleAsks(action AskAction, custom string) ([]Effect, error) {
	if d.busy != BusyAwaitingAnswer || d.turn == nil {
		return nil, nil
	}
	entry, ok := d.conv.Message(d.turn.messageID)
	if !ok {
		d.reset()
		return nil, nil
	}

	outputs := make(map[string]string, len(d.turn.asks))
	for _, group := range d.turn.asks {
		outputs[group.CallID] = group.Output(action, custom)
	}

	for i, part := range entry.Message.Parts {
		data, ok := partData(part)
		if !ok {
			continue
		}
		if pending, _ := data["pending"].(bool); !pending {
			continue
		}
		id, _ := data["id"].(string)
		output, ok := outputs[id]
		if !ok {
			continue
		}
		name, _ := data["name"].(string)
		entry.Message.Parts[i] = message.Part{
			Content: message.DataPart{Data: map[string]any{
				"id":       id,
				"name":     name,
				"output":   output,
				"is_error": action == AskDismiss,
			}},
			MediaType: render.MediaToolResult,
		}
	}

	var effects []Effect
	for _, group := range d.turn.asks {
		effects = append(effects, recordAskAnswered(d.turn.messageID, group.CallID, group.Output(action, custom)))
	}
	d.turn.asks = nil
	next, err := d.continueAfterTools()
	if err != nil {
		return nil, err
	}
	return append(effects, next...), nil
}

// continueAfterTools 开下一轮流（工具结果已齐全）。
func (d *Driver) continueAfterTools() ([]Effect, error) {
	// 插话优先于轮数上限：用户刚说的话必须送到模型面前，不能因为上一个
	// 回合用完了工具轮数就被丢掉。deliverQueued 会把 depth 归零，
	// 所以插话之后再按新的用户输入重新计数。
	if delivery, delivered := d.deliverQueued(); delivered {
		return delivery, nil
	}
	// 安全阀：一次用户输入内的工具轮数上限
	if d.opts.MaxToolRounds > 0 && d.depth >= d.opts.MaxToolRounds {
		text := d.tr("notice.roundLimit", d.opts.MaxToolRounds)
		_, _ = d.conv.AddNotice(conversation.NoticeWarn, text)
		d.reset()
		return []Effect{recordNotice(conversation.NoticeWarn, text), Changed{}}, nil
	}
	effects, err := d.startTurn()
	if err != nil {
		d.reset()
		return nil, err
	}
	return effects, nil
}

// Stop 停止当前生成（用户点停止）。
//
// 顺序（方案 §4）：先让 driver 收尾（补全 tool_use 结果、状态归位），调用方随后
// cancel ctx。这样 goroutine 因取消而投递的迟到事件会被 OwnsStream 挡掉。
//
// 排队的插话**一并清空**（用户决定）：按下停止就是"停下来"，此时再自动把队列
// 投递出去会立刻开一轮新请求，与用户的意图相反（与决策 #14 同源）。
func (d *Driver) Stop() []Effect {
	messageID := ""
	if d.turn != nil {
		messageID = d.turn.messageID
	}
	d.queued = nil

	var effects []Effect
	if entry, ok := d.conv.Message(messageID); ok {
		// 先把已收到的工具调用落成 part（流式中停止时它们还在缓冲区里），
		// 否则「未配对调用」根本不存在，补齐就成了空转——重放出来历史仍然带病。
		if d.turn != nil && d.busy == BusyStreaming {
			_, parts := d.collectToolCalls(d.turn)
			for _, part := range parts {
				entry.AddPart(part)
			}
		}
		// 未配对的调用必须有结果，否则整条会话之后都无法再发请求
		changed := providers.SettleUnfinishedToolCalls(entry.Message, "已中止：用户停止了生成。")
		switch d.busy {
		case BusyStreaming:
			_ = d.conv.CancelStream(messageID)
			entry.SetFinishedAt(time.Now())
			// 最终内容一次性写入（已含补齐的结果，不要再单独记 tool.results）
			effects = append(effects, recordFinalized(*entry.Message, conversation.StatusCancelled, ""))
		case BusyRunningTools, BusyAwaitingAnswer:
			// 这条消息在工具执行前已经 Complete 并落过日志，只需补结果
			if changed {
				effects = append(effects, recordToolResults(messageID, partResultRecords(entry.Message)))
			}
		}
	}
	d.reset()
	return append(effects, Changed{})
}

// Created 返回会话开始时应该写入的一次性事件（会话 id、初始选择）。
//
// 与 Send 分开：用户可能打开应用就关掉，此时也要有 session.created 与
// selection.changed，重放才知道这是哪个会话、用什么模型。
func (d *Driver) Created(title string) []Effect {
	d.title = title
	return []Effect{
		recordCreated(d.conv.ContextID(), title, d.opts.Workspace),
		recordSelection(d.opts.Selection),
	}
}

// Rename 重命名会话并记录事件。
func (d *Driver) Rename(title string) []Effect {
	d.title = title
	return []Effect{record(store.TypeSessionRenamed, SessionRenamedData{Title: title})}
}

// Title 返回会话标题（重命名后由 driver 记着）。
func (d *Driver) Title() string { return d.title }

// DeleteCurrentTurn 丢弃当前轮（会话被删除时调用，不做状态收尾）。
func (d *Driver) DeleteCurrentTurn() {
	d.queued = nil
	d.reset()
}

func (d *Driver) reset() {
	d.turn = nil
	d.depth = 0
	d.pendingCompact = nil
	d.busy = BusyIdle
}

// partData 取 part 的 data 对象（两值断言：历史里的 part 不一定自洽）。
func partData(part message.Part) (map[string]any, bool) {
	dataPart, ok := part.Content.(message.DataPart)
	if !ok {
		return nil, false
	}
	data, ok := dataPart.Data.(map[string]any)
	return data, ok
}

func isMedia(part message.Part, want string) bool {
	essence := part.MediaType
	if i := strings.IndexByte(essence, ';'); i >= 0 {
		essence = essence[:i]
	}
	return strings.EqualFold(strings.TrimSpace(essence), want)
}

// imagesOf 从工具结果的 data 里读回图片引用（补写日志时用）。
func imagesOf(data map[string]any) []tools.ToolImageRef {
	list, _ := data["images"].([]any)
	if len(list) == 0 {
		return nil
	}
	out := make([]tools.ToolImageRef, 0, len(list))
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		ref, _ := obj["ref"].(string)
		mediaType, _ := obj["media_type"].(string)
		width, _ := toInt(obj["width"])
		height, _ := toInt(obj["height"])
		out = append(out, tools.ToolImageRef{Ref: ref, MediaType: mediaType, Width: width, Height: height})
	}
	return out
}

func toInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	}
	return 0, false
}

func questionsOf(data map[string]any) []tools.QuestionSpec {
	raw, ok := data["questions"].([]tools.QuestionSpec)
	if ok {
		return raw
	}
	// 从 JSON 反序列化回来的形式
	if list, ok := data["questions"].([]any); ok {
		out := make([]tools.QuestionSpec, 0, len(list))
		for _, item := range list {
			if obj, ok := item.(map[string]any); ok {
				out = append(out, questionFromMap(obj))
			}
		}
		return out
	}
	return nil
}

func questionFromMap(obj map[string]any) tools.QuestionSpec {
	id, _ := obj["id"].(string)
	question, _ := obj["question"].(string)
	header, _ := obj["header"].(string)
	multi, _ := obj["multiSelect"].(bool)
	var options []tools.QuestionOption
	if list, ok := obj["options"].([]any); ok {
		for _, item := range list {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			label, _ := entry["label"].(string)
			description, _ := entry["description"].(string)
			options = append(options, tools.QuestionOption{Label: label, Description: description})
		}
	}
	return tools.QuestionSpec{ID: id, Question: question, Header: header, Options: options, MultiSelect: multi}
}

// nowUTC 返回当前 UTC 时间（便于测试替换）。
var nowUTC = func() string { return time.Now().UTC().Format(time.RFC3339) }

// isContextOverflow 报告失败原因是否是「上下文超出窗口」。
//
// 三家常见说法：OpenAI 的 context_length_exceeded、Anthropic 的
// "prompt is too long"、兼容实现的 400 + "maximum context length"。
func isContextOverflow(reason string) bool {
	lower := strings.ToLower(reason)
	for _, marker := range []string{
		"context_length_exceeded",
		"maximum context length",
		"prompt is too long",
		"context length",
		"too many tokens",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// isTruncated 报告停止原因是否是「达到长度上限」。
func isTruncated(reason string) bool {
	switch strings.ToLower(reason) {
	case "length", "max_tokens":
		return true
	}
	return false
}
