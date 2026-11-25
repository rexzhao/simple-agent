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
	// SubagentNotice 在**组装请求前**被调用，返回一条要追加到本次请求末尾的
	// 用户消息（空串表示没有）。
	//
	// 为什么在组装前而不是走 Enqueue：
	//   - Enqueue 是"用户插话"的队列，每条都开一个 user 消息并真正落盘；
	//     通知可能一次来好几条，堆进去会在历史里留下多条噪声消息。
	//   - 这里**不写 Conversation、不落盘**：只在发给模型的 Messages 里追加一条。
	//     历史不变 ⇒ 缓存前缀不变（除了最后那条通知本身），也不会重复注入。
	//
	// 由 host 注入：它知道未读通知与"哪些已经通知过"。父子会话**都要挂**——
	// 父会话也要能被自己的子会话通知。
	SubagentNotice func() string
}

// Usage 是本轮 token 用量。
type Usage struct {
	InputTokens  int
	OutputTokens int
	// CacheReadTokens 是命中缓存的输入 token（没花钱重算的那部分）。
	CacheReadTokens int
	// CacheWriteTokens 是写入缓存的输入 token（Anthropic 的 cache_creation）。
	CacheWriteTokens int
}

// toolBuffer 累积一个工具调用的流式分片。
type toolBuffer struct {
	id    string
	name  string
	args  strings.Builder
	ended bool
	order int
}

// Round 是一轮对话（用户一次提问 → 全部工具执行完毕）的进行中状态。
//
// 注意：这里的 Round 是**大回合**；一次 Round 内会发起多次 LLM 请求
// （每次是一个 Turn），见 docs/session-log-model.md。
type Round struct {
	messageID string
	// id 是本轮的 roundId（全局自增，见 Driver.roundSeq）。
	//
	// 落盘到 round.started/finished 与各内容事件上，供展示端按轮分页与缓存。
	id int
	// turn 是本轮内当前进行到第几个 Turn（从 0 开始）。
	//
	// 一次 Round 内每发起一次 LLM 请求就是一个新 Turn（工具续轮也算）。
	// 0 表示还在第一个 Turn，尚未进入工具续轮。
	turn int
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
	round *Round
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
	// lastUsage 是最近一次真实 usage 的**完整**记录（含缓存字段）。
	//
	// 为什么不能只从 d.round.usage 取：回合结束时先 reset() 再 recordUsage()，
	// round 已经被清空；界面在回合结束后（以及重新打开会话后）也要显示用量，
	// 所以必须留一份独立于 round 的副本。
	//
	// 重放历史会话时它同样为「空」——用量不落盘（只有 turn 边界事件），
	// 旧会话要等下一次真实请求才会有数据。
	lastUsage Usage
	// totalUsage 是**会话累计**用量（所有已完成的请求之和）。
	//
	// 为什么要有它：命中率要看整体趋势，单次请求的数字抖得厉害 ——
	// 一轮新建缓存（0%）紧接一轮全命中（99.9%），看单次会以为缓存时好时坏。
	// 累计值稳定得多，也更符合"这个会话省了多少"的直觉。
	//
	// 压缩时会写进 compact.snapshot（total_input 等字段），重放直接从快照
	// 接着算，不必回翻归档日志。
	totalUsage Usage
	// autoCompactUsed 表示本轮用户输入已经自动压缩过一次，不再重试。
	autoCompactUsed bool
	// overflowRetryArmed 表示"压缩是因为超限触发的，压缩完要自动重试一次"。
	//
	// 对齐 pi 的 _overflowRecoveryAttempted：撞到上下文/体积上限后，
	// 只压缩不重试的话用户还得自己再点一次发送 —— 而这时他刚刚才发过，
	// 很容易以为是程序卡住了。压缩完直接替他重发一次，失败再多说。
	//
	// 用单独的字段而不是复用 autoCompactUsed：后者是"这一轮已经压过了"的
	// 防重入标志，语义不同；两层判断各有各的用处。
	overflowRetryArmed bool
	// overflowRetryDone 表示已经自动重试过一次，防止无限循环。
	//
	// 压缩后仍超限（比如单条工具结果本身就超过体积上限）时，
	// 不能一直 "压缩→重发→还超限→再压缩"。
	overflowRetryDone bool
	// warnedDefaultWindow 表示已经提示过「窗口未知，按默认值处理」。
	warnedDefaultWindow bool

	// turns/steps 是会话级累计计数（状态条显示用），口径见 Counts。
	//
	// **不单独落盘**：它们可以从会话内容推导（turns = user 消息条数，
	// steps = agent 消息条数），重放因此天然一致；唯一会丢失历史的动作是
	// 压缩，所以压缩快照里带上当时的累计值当基线（与 kept 一起重建，见 apply).
	turns int
	steps int

	// roundSeq 是已经分配过的 Round 总数（= 最后一个 roundId）。
	//
	// 与 turns 的区别：turns 是"用户输入条数"（插话也各算一条），
	// roundSeq 是"Round 个数"（一次提问算一个，插话不新增 Round）。
	// 落盘到 round.started/finished 的 roundId 上。
	roundSeq int
	// roundStartedAt 是当前 Round 的开始时间（round.finished 算总耗时用）。
	//
	// 不能用 Round.startedAt：那个结构体每个 Turn 重建一次，只代表当前 Turn。
	roundStartedAt time.Time
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

// AddDisplayMessage 插入一条**只用于展示**的消息（不进模型上下文）。
//
// 场景：界面要显示某样东西（比如 display_image 的图），但模型不该看到它。
// 消息必须带 message.MetadataDisplayOnly 标记，组装请求时整条跳过。
//
// 会写日志（recordMessageOutput），所以重放后照样出现在界面里。
// 状态用 StatusComplete：它是一条"已经完成"的展示消息，没有流式过程。
func (d *Driver) AddDisplayMessage(msg message.Message) (*conversation.ChatEntry, error) {
	if !msg.DisplayOnly() {
		// 防御：调用方忘了打标记就等于"往上下文里塞了条没人认识的消息"
		return nil, errors.New("AddDisplayMessage 需要 display_only 标记")
	}
	entry, err := d.conv.AddMessage(msg, conversation.StatusComplete)
	if err != nil {
		return nil, err
	}
	entry.SetFinishedAt(time.Now())
	d.pendingRecords = append(d.pendingRecords, recordMessageOutput(
		0, 0, *entry.Message, conversation.StatusComplete, ""))
	return entry, nil
}

// TakePendingRecords 取走积压的事件（没有 effect 返回值的操作用）。
func (d *Driver) TakePendingRecords() []Effect {
	out := d.pendingRecords
	d.pendingRecords = nil
	return out
}

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

// SetSubagentNotice 设置"未读通知"的取值函数（见 Options.SubagentNotice）。
//
// 传 nil 表示关闭（例如展示端镜像）。
func (d *Driver) SetSubagentNotice(fn func() string) { d.opts.SubagentNotice = fn }

// RoundElapsed 返回当前回合已运行时长；空闲时返回 0。
//
// 用途：父会话用 subagent_list 看子会话"跑了多久"，判断它是卡住了还是在干活。
//
// **口径**：用 Driver.roundStartedAt（Send/Retry/SendWithAttachments 三个入口都设它），
// 而不是 Round.startedAt（那个只给回合结束行算耗时）。
//
// 空闲时必须返回 0：否则界面上会显示成"一直在跑"。
func (d *Driver) RoundElapsed() time.Duration {
	if d.busy == BusyIdle || d.roundStartedAt.IsZero() {
		return 0
	}
	return time.Since(d.roundStartedAt)
}

// Tools 返回当前注册的工具列表。
//
// 用途：宿主/测试要确认"某个工具到底在不在列表里"——例如子会话不该有
// new_session（不注册，而不是注册了报错）。
func (d *Driver) Tools() []*tools.ToolDef { return d.opts.Tools }

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
	return d.round != nil && d.round.messageID == messageID
}

// CurrentMessageID 返回当前轮的消息 id（无进行中的轮时为空）。
func (d *Driver) CurrentMessageID() string {
	if d.round == nil {
		return ""
	}
	return d.round.messageID
}

// Usage 返回最近一次请求的用量。
//
// 取值顺序很关键：**先看 round 有没有真实数据，没有就回退 lastUsage**。
//
// 不能只写 `if d.round != nil { return d.round.usage }`：新一轮请求刚开始时
// round 已经建好、但 usage 事件还没到，那份 usage 是全 0 —— 直接返回它会让
// 状态条从"缓存 99%"闪成"缓存 0%"或整行消失（用户看到的就是"显示一下又没了"）。
//
// 反过来，round 里有值时必须用它（流式期间要实时更新），不能一直用上一轮的旧值。
func (d *Driver) Usage() Usage {
	if d.round != nil {
		if usage := d.round.usage; usage.InputTokens > 0 || usage.OutputTokens > 0 ||
			usage.CacheReadTokens > 0 || usage.CacheWriteTokens > 0 {
			return usage
		}
	}
	return d.lastUsage
}

// TotalUsage 返回会话累计用量（所有已完成请求之和）。
//
// 与 Usage() 的区别：那个是「最近一次请求」，这个是「整个过程」。
// 命中率用累计口径才有意义 —— 单次请求里新建缓存的那一轮是 0%，
// 紧接着全命中的一轮是 99.9%，看单次会以为缓存时好时坏。
func (d *Driver) TotalUsage() Usage { return d.totalUsage }

// addUsage 把一次请求的用量累加进整体。
func addUsage(total, one Usage) Usage {
	return Usage{
		InputTokens:      total.InputTokens + one.InputTokens,
		OutputTokens:     total.OutputTokens + one.OutputTokens,
		CacheReadTokens:  total.CacheReadTokens + one.CacheReadTokens,
		CacheWriteTokens: total.CacheWriteTokens + one.CacheWriteTokens,
	}
}

// StreamFailed 表示这一轮因"重发可能就好"的原因失败（网络、超时、5xx）。
//
// 决策在 driver（它知道失败原因），执行在宿主层（它管退避与次数上限）。
// 宿主层收到后用 Retry() 重发 —— **不是** Send("继续")：
// 后者会多一条用户消息，弄脏历史并让缓存前缀失效。
type StreamFailed struct {
	Reason string
}

func (StreamFailed) effectKind() string { return "streamFailed" }

// retryableFailure 报告这个失败原因**重发有没有意义**。
//
// 与界面 isRetryable（uiapp）同一套判断，但放在 driver：
// 自动重试是执行端行为，不该依赖界面层。两处的关键词保持同步。
func retryableFailure(reason string) bool {
	if reason == "" {
		return false
	}
	lower := strings.ToLower(reason)
	// 先排除"重发无用"的
	for _, hopeless := range []string{
		"401", "403", "402", "invalid api key", "unauthorized",
		"authentication", "permission", "insufficient", "balance",
		"context_length_exceeded", "maximum context length",
		"prompt is too long", "too many tokens", "max_tokens",
		"400", "404", "prefill", // 以 assistant 结尾这类请求构造错误
	} {
		if strings.Contains(lower, hopeless) {
			return false
		}
	}
	for _, retryable := range []string{
		"timeout", "deadline exceeded", "connection refused",
		"no such host", "network", "eof", "reset by peer",
		"broken pipe", "stream", "502", "503", "504", "429",
		"rate limit", "quota", "unexpected",
		// 断流：StreamClosed 用的原因文本是"连接意外中断（未收到结束标记）"，
		// 含中文关键词，不加这条自动重试永远触发不了。
		"中断", "未收到结束标记",
	} {
		if strings.Contains(lower, retryable) {
			return true
		}
	}
	return false
}

// toConversationUsage 把用量转成 conversation 包的类型。
//
// 两个包各自定义了一份（conversation 更底层，不能依赖 driver），
// 字段名一致，转换是逐字段赋值 —— 不复制聚合逻辑，只有类型转换。
func toConversationUsage(u Usage) conversation.Usage {
	return conversation.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
	}
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
	// 空闲时没有"插话"这回事：直接当普通消息发出去。
	//
	// 真实踩到的：手机端新会话刚建好，状态数据还没拉回来（canSend=false），
	// 于是走了 enqueue 这条路径；而 idle 会话 CanSteer() 为 false，
	// 返回 ErrBusy（"当前状态不支持该操作"）—— 用户看到的就是
	// "点了发送没反应"。
	//
	// 客户端判断 canSend 依赖一次状态拉取，有时序窗口；后端兜住更可靠，
	// 而且语义也对：空闲时候队等于立刻发送。
	if d.busy == BusyIdle {
		return d.SendWithAttachments(text, nil)
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

	// 新的 Round：**先分配 roundId**，再追加用户消息——
	// message.input 事件要带上 roundId，所以不能等 startTurn 时才分配。
	// 插话（Enqueue → deliverQueued）不走这里，复用当前 roundId。
	d.roundSeq++
	d.roundStartedAt = time.Now()

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
	turnEffects, err := d.startTurn(0)
	if err != nil {
		return nil, err
	}
	return append(append(effects, cleared...), turnEffects...), nil
}

// Retry 重新发起**这一轮请求**：不追加任何用户消息，直接开新一轮。
//
// 与 Send 的区别（这是为什么要单独做一个方法）：
//   - Send("继续") 会在对话里**多出一条用户消息**。那既弄脏历史
//     （模型会看到一句莫名的话），也会**改变缓存前缀** —— 整段缓存作废。
//   - Retry 沿用历史里最后那条用户消息，模型看到的前缀完全一样，
//     缓存照常命中。
//
// 用在"网络失败/断流"这类**重发就能好**的场合（界面的失败行给按钮）。
// 真正的错误（401、上下文超限）重发也没用，调用方不该给这个按钮。
func (d *Driver) Retry() ([]Effect, error) {
	if d.busy != BusyIdle {
		return nil, ErrBusy
	}
	// 必须有可重发的起点：历史里得有一条用户消息。
	// （没有的话说明状态异常，直接拒绝比发一个空请求好。）
	if !d.hasUserMessage() {
		return nil, fmt.Errorf("没有可重发的消息")
	}
	d.roundSeq++
	d.roundStartedAt = time.Now()
	d.queued = nil
	d.depth = 0
	d.autoCompactUsed = false
	return d.startTurn(0)
}

// hasUserMessage 报告历史里有没有用户消息。
func (d *Driver) hasUserMessage() bool {
	for _, entry := range d.conv.Entries() {
		if entry.Message != nil && entry.Message.Message != nil &&
			entry.Message.Message.Role == message.RoleUser {
			return true
		}
	}
	return false
}

// SendWithAttachments 追加一条"文字 + 图片"的 user 消息并开始新一轮。
//
// 图片以 `blob:sha256:` 引用进入消息（字节已经在 blob 存储里），请求组装时才
// 读成字节。模型不支持图片时直接拒绝：这是界面的兜底守卫，不能只靠按钮禁用。
func (d *Driver) SendWithAttachments(text string, attachments []message.Part) ([]Effect, error) {
	if d.busy != BusyIdle {
		// 带上当前状态：只说"当前状态不接受该操作"，用户和排查的人都不知道
		// 到底卡在哪（是还在生成？在等回答？在压缩？）
		return nil, fmt.Errorf("%w（当前状态：%s）", ErrBusy, d.busy)
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
	// 同 Send：先分配 roundId（message.input 要带上它），再追加用户消息
	d.roundSeq++
	d.roundStartedAt = time.Now()

	effects, err := d.appendUserTextsWithLastParts(all, attachments)
	if err != nil {
		return nil, err
	}
	// 同 Send：用户的新输入清空上一轮的任务清单
	cleared := d.clearTodos()
	d.queued = nil
	d.depth = 0
	d.autoCompactUsed = false
	turnEffects, err := d.startTurn(0)
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
	// 用户输入归属当前 Round 的第 0 个 Turn（插话时是当前 turn）
	return []Effect{recordMessageInput(d.roundSeq, d.currentTurnIndex(), user)}, nil
}

// currentTurnIndex 返回用户消息应归属的 Turn 序号。
//
// 新 Round 里用户提问属于 Turn 0；插话发生在某个 Turn 进行中，
// 归属那个正在进行的 Turn（d.round.turn）。
func (d *Driver) currentTurnIndex() int {
	if d.round == nil {
		return 0
	}
	return d.round.turn
}

// toolResultOrigin 返回"发起这次工具调用"的那个 (round, turn)。
//
// **不能用 d.round**：工具是异步执行的，写结果时 driver 的当前 round/turn
// 可能已经进入下一轮或被重置。归属应由发起调用的那条消息决定，
// 而 d.round.messageID 记的正是它——但前提是它还在。
func (d *Driver) toolResultOrigin(messageID string) (int, int) {
	if d.round != nil && d.round.messageID == messageID {
		return d.round.id, d.round.turn
	}
	// driver 已经进入下一轮/重置：退回"当前 Round 的当前 Turn"，
	// 重放时由 call_id 兜底配对（见 replay.applyToolResult）。
	if d.round != nil {
		return d.round.id, d.round.turn
	}
	return d.roundSeq, 0
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
	next, err := d.startTurn(d.nextTurnIndex())
	if err != nil {
		return append(effects, Changed{}), true
	}
	return append(effects, next...), true
}

// nextTurnIndex 返回当前 Round 内的下一个 Turn 序号。
//
// 续轮（工具执行完 / 插话投递完）都用它：同一个 Round 内 Turn 从 0 递增。
// 当前没有进行中的 Round 时返回 0（理论上不会走到，防御性处理）。
func (d *Driver) nextTurnIndex() int {
	if d.round == nil {
		return 0
	}
	return d.round.turn + 1
}

// startTurn 新建 agent 消息并发起流。
//
// turnIndex 是这个 Turn 在所属 Round 内的序号（从 0 开始）。
// 工具续轮/插话续轮会再次调用它，序号递增；新 Round 从 0 重新开始。
func (d *Driver) startTurn(turnIndex int) ([]Effect, error) {
	agentID, err := conversation.NewMessageID()
	if err != nil {
		return nil, err
	}
	agent := message.Message{MessageID: agentID, Role: message.RoleAgent}
	entry, err := d.conv.AddMessage(agent, conversation.StatusPending)
	if err != nil {
		return nil, err
	}

	round := &Round{
		messageID:     entry.Message.MessageID,
		id:            d.roundSeq,
		turn:          turnIndex,
		startedAt:     time.Now(),
		buffers:       map[int]*toolBuffer{},
		toolParts:     map[int]int{},
		reasoningPart: -1,
		depth:         d.depth + 1,
	}
	d.round = round
	d.depth = round.depth
	d.busy = BusyStreaming
	// 每发起一次请求算一步（工具续轮、插话续轮都各算一步，见 Counts）
	d.steps++

	request := d.buildRequest()
	var effects []Effect
	// 第一个 Turn 开始时才发 round.started：一次 Round 内有多个 Turn，
	// 只有最开始那个需要标记 Round 的起点。
	if round.turn == 0 {
		effects = append(effects, recordRoundStarted(round.id))
	}
	effects = append(effects,
		// Turn 边界：显式标记"这一轮请求开始了"。
		// 崩溃恢复靠它判定"哪些 Turn 没走完"（没有 turn.finished 即中断）。
		//
		// **不再写 streaming 空壳**：内容只在确定后写一次，
		// 崩溃时这一轮直接丢弃，从上一个 turn 继续
		//（见 docs/session-log-model.md §2.4、§3）。
		recordTurnStarted(round.id, round.turn),
		StartStream{MessageID: round.messageID, Request: request},
	)
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
	// 未读通知：追加成**最后一条 user 消息**，但不写进 Conversation。
	//
	// 三个要点：
	//  1. 只在这里追加 ⇒ 历史不变 ⇒ 下次请求不会重复带上它
	//     （host 侧还会记录"哪些已通知过"，避免同一条反复提醒）
	//  2. 追加在末尾（user 角色）⇒ 两家协议都接受，不会出现"以 assistant 结尾"
	//  3. 内容只列清单、不含正文：正文让模型自己决定怎么取，
	//     这样通知正文不会长期占据上下文
	if d.opts.SubagentNotice != nil {
		if notice := strings.TrimSpace(d.opts.SubagentNotice()); notice != "" {
			history = append(history, message.Message{
				MessageID: "subagent-notice",
				Role:      message.RoleUser,
				Parts:     []message.Part{message.NewTextPart(notice)},
			})
		}
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
	round := d.round

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
		if round.reasoningPart < 0 {
			round.reasoningPart = entry.AddPart(message.Part{
				Content:   message.TextPart{Text: ev.Text},
				MediaType: render.MediaReasoning,
			})
			if err := d.conv.MarkStreaming(messageID); err != nil {
				return nil, err
			}
			return []Effect{Changed{}}, nil
		}
		// 用按 part 追加的接口：不能挪用 streamingPart，否则正文与推理会互相串
		if err := d.conv.AppendToPart(messageID, round.reasoningPart, ev.Text); err != nil {
			return nil, err
		}
		return []Effect{Changed{}}, nil

	case providers.EventToolStart:
		buf := round.buffer(ev.Index)
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
			d.ensureToolPart(entry, round, ev.Index)
		}
		return []Effect{Changed{}}, nil

	case providers.EventToolArgs:
		buf := round.buffer(ev.Index)
		buf.args.WriteString(ev.Delta)
		// 增量也落盘：标题要能像 dsh 一样边收边显示"正在读取 docs/plan.md"。
		//
		// 只落**前一部分**（界面摘要用到的 key 都在开头）：整段重写会让
		// 复制量变成 O(n²)——write 写 100KB 文件时几千个增量累计几百 MB。
		if entry, ok := d.conv.Message(messageID); ok {
			partIndex := d.ensureToolPart(entry, round, ev.Index)
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
		buf := round.buffer(ev.Index)
		buf.ended = true
		return []Effect{Changed{}}, nil

	case providers.EventUsage:
		if ev.InputTokens > 0 {
			round.usage.InputTokens = ev.InputTokens
		}
		if ev.OutputTokens > 0 {
			round.usage.OutputTokens = ev.OutputTokens
		}
		// 缓存字段：**带了才覆盖**，靠 CacheReported 判断。
		//
		// Anthropic 分两个事件报用量：message_start 带 input 与缓存字段，
		// message_delta 只带 output。若按"每次都覆盖"处理（以前的做法），
		// 第二个事件会把已经拿到的缓存数抹成 0 —— 实测就是这样：
		// 网关明明回了 cached=298240，界面却显示 0%。
		//
		// 为什么不能简单按"非 0 才写"：那会让"这轮真没命中（确实 0）"
		// 沿用上一轮的旧值，显示成假的命中率。所以需要一个显式的
		// "这条事件带了缓存字段"信号。
		if ev.CacheReported {
			round.usage.CacheReadTokens = ev.CacheReadTokens
			round.usage.CacheWriteTokens = ev.CacheWriteTokens
		}
		// **同时更新 lastUsage**：有工具调用的回合里 round 会跨多次请求存活，
		// 但下一轮请求一开始 round 就被换掉了；只在"回合彻底结束"时记一份
		// 会让状态条在中间那几轮读到旧的（或空的）值。
		// 每次真实 usage 到达即刷新，状态条任何时候都是最近一次的真实数据。
		d.lastUsage = round.usage
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
	round := d.round
	switch ev.Kind {
	case providers.EventDone:
		round.sawStop = true
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
func (d *Driver) ensureToolPart(entry *conversation.ChatEntry, round *Round, index int) int {
	if partIndex, ok := round.toolParts[index]; ok {
		return partIndex
	}
	buf := round.buffer(index)
	// stream_index 是 **provider 给的下标**（Anthropic 是 content block 下标，
	// 不是"第几个工具"）。镜像中途接管时要靠它把增量对回正确的 part，
	// 不能按出现顺序重新编号——前面有一个 text 块时就会错位。
	data := map[string]any{
		"id": buf.id, "name": buf.name, "args": map[string]any{},
		"stream_index": index,
		// started_at 是工具**开始执行**的时刻（Unix 毫秒）。
		//
		// 为什么要它：折叠行要显示"已经跑了多久"。没有起点就只能显示
		// 一个静止的"运行中"——用户看到转圈半天也不知道是 3 秒还是 3 分钟，
		// 这正是"是不是卡住了"的焦虑来源。
		//
		// 只给界面用（providers 组装请求时不读它），重放时保留 ——
		// 重启后看历史也能知道那次工具跑了多久。
		"started_at": time.Now().UnixMilli(),
	}
	partIndex := entry.AddPart(message.Part{
		Content:   message.DataPart{Data: data},
		MediaType: render.MediaToolUse,
	})
	round.toolParts[index] = partIndex
	return partIndex
}

// buffer 取（或新建）指定 index 的工具缓冲。
func (t *Round) buffer(index int) *toolBuffer {
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
	round := d.round
	entry, ok := d.conv.Message(messageID)
	if !ok {
		d.reset()
		return []Effect{Changed{}}
	}

	calls, parts := d.collectToolCalls(round)
	for _, part := range parts {
		entry.AddPart(part)
	}

	// 无论是否要跑工具，本轮这条消息都已完成（工具结果是它的后续 part）
	if err := d.conv.EndStream(messageID); err != nil && !errors.Is(err, conversation.ErrNotStreaming) {
		d.reset()
		return []Effect{Changed{}}
	}

	// 按轮留存完整用量：回合结束行要显示**这一轮**的缓存命中率，
	// 而不是整个会话的平均（以前界面拿会话累计值，每轮显示都一样）。
	//
	// 放在这里而不是各分支里：有工具调用与没工具调用两条路都要记。
	entry.SetUsage(toConversationUsage(round.usage))

	// 最终内容一次性写入（流式增量不落日志；参数非法产生的错误结果也在其中）
	effects := []Effect{
		recordMessageOutput(round.id, round.turn, *entry.Message, conversation.StatusComplete, ""),
		// Turn 边界：这一次请求结束了（后面可能还有工具执行与续轮）
		recordTurnFinished(round.id, round.turn, time.Since(round.startedAt), round.usage),
	}
	// 累计这次请求的用量。
	//
	// **必须在这里累加**，不能放在 EventUsage 里：Anthropic 的 input 与 output
	// 分两个事件发（message_start / message_delta），而缓存字段各自覆盖 ——
	// 在事件里累加会把同一个请求算两遍。finishStream 是「一次请求结束、用量
	// 已是最终值」的唯一确定时点，一个 turn 恰好累加一次。
	d.totalUsage = addUsage(d.totalUsage, round.usage)

	if len(calls) == 0 {
		usage := round.usage
		entry.SetFinishedAt(time.Now())
		entry.SetTokens(usage.OutputTokens)
		// 没有工具调用 → 整个 Round 结束
		effects = append(effects, recordRoundFinished(round.id, time.Since(d.roundStartedAt)))
		d.reset()
		// 正常结束：记住真实用量，并检查是否该自动压缩（driver 决策，UI 只执行）
		d.recordUsage(usage)
		// **静默溢出**：网关没报错就接受了超限请求，但真实用量已经吃满窗口。
		//
		// 对齐 pi 的 Case 2：z.ai 这类网关超限不报错，而是默默接受，
		// 等到下一轮才突然失败（甚至开始静默截断上下文）。
		// 判据用**服务端回的真实 usage**（不是本地估算）：
		// input + cacheRead 就是这次实际喂进去的量。
		silentOverflow := d.effectiveWindow() > 0 &&
			usage.InputTokens+usage.CacheReadTokens > d.effectiveWindow()
		compact := d.autoCompactEffects(silentOverflow)
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
	d.round.pendingToolResults = len(calls)
	return append(effects,
		RunTools{MessageID: messageID, Calls: calls},
		Changed{},
	)
}

// collectToolCalls 把缓冲转成调用与对应的 tool_use part。
//
// 参数是分片 JSON，必须累积到结束才解析；解析失败保留原文并在这里就标成
// 带错误的调用（由调用方拿到错误结果，模型可自行纠正）。
func (d *Driver) collectToolCalls(round *Round) ([]tools.Call, []message.Part) {
	if len(round.buffers) == 0 {
		return nil, nil
	}

	order := make([]int, 0, len(round.buffers))
	for index := range round.buffers {
		order = append(order, index)
	}
	slices.Sort(order)

	calls := make([]tools.Call, 0, len(order))
	parts := make([]message.Part, 0, len(order)*2)

	for _, index := range order {
		buf := round.buffers[index]
		name := buf.name
		if name == "" {
			// 没有名字的调用无法执行：保留下来让历史配对完整
			name = "unknown"
		}
		id := buf.id
		if id == "" {
			id = fmt.Sprintf("call_%s_%d", round.messageID, index)
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
		if entry, ok := d.conv.Message(round.messageID); ok {
			partIndex := d.ensureToolPart(entry, round, index)
			final := message.Part{
				Content:   message.DataPart{Data: data},
				MediaType: render.MediaToolUse,
			}
			if err := d.conv.UpdatePart(round.messageID, partIndex, final); err != nil {
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
		if d.round != nil {
			// 推理 part 在流式过程中已经落盘；这里只补上已收到的工具调用
			_, parts := d.collectToolCalls(d.round)
			for _, part := range parts {
				entry.AddPart(part)
			}
		}
		providers.SettleUnfinishedToolCalls(entry.Message, "已中止："+reason)
		_ = d.conv.FailStream(messageID, reason)
		entry.SetFinishedAt(time.Now())
		// 最终内容一次性写入（已含补齐的结果）
		effects := []Effect{recordMessageOutput(d.round.id, d.round.turn, *entry.Message, conversation.StatusFailed, reason)}
		// 失败也是终点：Turn 与 Round 都要收尾，否则崩溃恢复会把它当成"未完成"
		if d.round != nil {
			effects = append(effects,
				recordTurnFinished(d.round.id, d.round.turn, time.Since(d.round.startedAt), d.round.usage),
				recordRoundFinished(d.round.id, time.Since(d.roundStartedAt)),
			)
		}
		overflow := isContextOverflow(reason)
		d.reset()
		if overflow {
			// 回合中途因超窗/超体积失败 → 自动压缩一次，压缩完**自动重试**。
			// 以**服务端的判断为准**强制压缩（本地估算偏低正是超限的原因）。
			if compactEffects := d.autoCompactEffects(true); len(compactEffects) > 0 {
				// 压缩完成后替我重发一次：用户刚刚才发过，让他再点一次
				// 很容易被当成"卡住了"。
				if !d.overflowRetryDone {
					d.overflowRetryArmed = true
				}
				text := d.tr("notice.overflowCompact")
				_, _ = d.conv.AddNotice(conversation.NoticeInfo, text)
				effects = append(effects, recordNotice(conversation.NoticeInfo, text))
				effects = append(effects, compactEffects...)
			}
		} else if retryableFailure(reason) {
			// 网络/超时/5xx 这类"重发就可能好"的失败：告诉宿主层可以自动重试。
			//
			// 决策放在 driver（它知道 reason），执行放在宿主层（它管退避与次数）。
			// 超限走上面的压缩路径，不在这里重试 —— 两者不能同时来。
			effects = append(effects, StreamFailed{Reason: reason})
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
	if d.round != nil {
		d.round.pendingToolResults = 0
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
	// 结构化附加字段全部走一个入口：
	//   diff / exec / image_info / web / search / read / todo
	// 它们**只给界面**渲染，组装请求时不会发给模型（providers 只取
	// output/is_error）。字段多，逐个写在这里容易漏，收成一个函数。
	attachToolExtras(data, outcome)
	if len(outcome.Images) > 0 {
		images := make([]any, 0, len(outcome.Images))
		for _, image := range outcome.Images {
			entry := map[string]any{
				"ref":        image.Ref,
				"media_type": image.MediaType,
				"width":      image.Width,
				"height":     image.Height,
			}
			// caption 只有 display_image（给用户看）会带；空值不写，
			// 免得历史里多出一堆空字符串。
			if image.Caption != "" {
				entry["caption"] = image.Caption
			}
			// display_only：这张图只给用户看，不发模型（组装请求时跳过）。
			if image.DisplayOnly {
				entry["display_only"] = true
			}
			images = append(images, entry)
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

	resultRound, resultTurn := d.toolResultOrigin(messageID)
	effects := append(recordToolResults(resultRound, resultTurn, resultRecords([]tools.ToolOutcome{outcome})), Changed{})

	if d.round != nil && d.round.pendingToolResults > 0 {
		d.round.pendingToolResults--
		if d.round.pendingToolResults > 0 {
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
	d.round.asks = d.collectAsks(entry.Message)
	if len(d.round.asks) > 0 {
		for _, group := range d.round.asks {
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
		// recordToolResults 返回 []Effect，必须先展开再追加后面的提示
		// （Go 只允许展开参数出现在 append 的最后，所以分两步）
		resultRound, resultTurn := d.toolResultOrigin(messageID)
		effects = append(effects, recordToolResults(resultRound, resultTurn, partResultRecords(entry.Message))...)
		effects = append(effects, recordNotice(conversation.NoticeWarn, "部分工具未返回结果，已标记为失败。"))
	}
	next, err := d.continueAfterTools()
	if err != nil {
		return effects
	}
	return append(effects, next...)
}

// AskGroups 返回本轮等待回答的提问组（同一轮可能有多个）。
func (d *Driver) AskGroups() []*AskGroup {
	if d.busy != BusyAwaitingAnswer || d.round == nil {
		return nil
	}
	return d.round.asks
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
	if d.busy != BusyAwaitingAnswer || d.round == nil {
		return nil, nil
	}
	entry, ok := d.conv.Message(d.round.messageID)
	if !ok {
		d.reset()
		return nil, nil
	}

	outputs := make(map[string]string, len(d.round.asks))
	infos := make(map[string]*tools.AskInfo, len(d.round.asks))
	for _, group := range d.round.asks {
		outputs[group.CallID] = group.Output(action, custom)
		// 结构化问答**只给界面**：Output 是 JSON，摊出来用户看不懂。
		infos[group.CallID] = group.Info(action == AskDismiss, custom)
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
		result := map[string]any{
			"id":       id,
			"name":     name,
			"output":   output,
			"is_error": action == AskDismiss,
		}
		if info := infos[id]; info != nil {
			result[extraAsk] = info
		}
		entry.Message.Parts[i] = message.Part{
			Content:   message.DataPart{Data: result},
			MediaType: render.MediaToolResult,
		}
	}

	var effects []Effect
	for _, group := range d.round.asks {
		effects = append(effects, recordAskAnswered(d.round.messageID, group.CallID,
			group.Output(action, custom), group.Info(action == AskDismiss, custom)))
	}
	d.round.asks = nil
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
	effects, err := d.startTurn(d.nextTurnIndex())
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
	if d.round != nil {
		messageID = d.round.messageID
	}
	d.queued = nil

	var effects []Effect
	if entry, ok := d.conv.Message(messageID); ok {
		// 先把已收到的工具调用落成 part（流式中停止时它们还在缓冲区里），
		// 否则「未配对调用」根本不存在，补齐就成了空转——重放出来历史仍然带病。
		if d.round != nil && d.busy == BusyStreaming {
			_, parts := d.collectToolCalls(d.round)
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
			effects = append(effects, recordMessageOutput(d.round.id, d.round.turn, *entry.Message, conversation.StatusCancelled, ""))
			// 用户主动停止也是终点：Turn 与 Round 都要收尾
			if d.round != nil {
				effects = append(effects,
					recordTurnFinished(d.round.id, d.round.turn, time.Since(d.round.startedAt), d.round.usage),
					recordRoundFinished(d.round.id, time.Since(d.roundStartedAt)),
				)
			}
		case BusyRunningTools, BusyAwaitingAnswer:
			// 这条消息在工具执行前已经 Complete 并落过日志，只需补结果
			if changed {
				resultRound, resultTurn := d.toolResultOrigin(messageID)
				effects = append(effects, recordToolResults(resultRound, resultTurn, partResultRecords(entry.Message))...)
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
	d.round = nil
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
		caption, _ := obj["caption"].(string)
		displayOnly, _ := obj["display_only"].(bool)
		out = append(out, tools.ToolImageRef{
			Ref: ref, MediaType: mediaType, Width: width, Height: height,
			Caption: caption, DisplayOnly: displayOnly,
		})
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

// isContextOverflow 报告失败原因是否是「上下文超出窗口 / 请求体太大」。
//
// 两种说法本质相同：都是"发给模型的内容太多了。各家措辞：
//   - OpenAI：context_length_exceeded
//   - Anthropic："prompt is too long" / 413 request_too_large
//   - 兼容实现：400 + "maximum context length"
//   - 部分网关（含实测的 paperhub）：413 + "Entry too big" / EntryTooBig
//   - GitHub Copilot：413 "failed to parse request"（请求体字节超限）
//
// **HTTP 413 一律算体积溢出**，不看 body 文本 —— 对齐 pi 的
// packages/ai/src/utils/overflow.ts：不同网关对同一个限制有十几种写法，
// 逐个枚举必然漏；而 413 语义明确（Payload Too Large），没有歧义。
//
// 先查"非溢出"再查"溢出"：限流信息里常带 "too many tokens" 之类的字样
// （如实测到的 Bedrock："ThrottlingException: Too many tokens, please wait"），
// 不排除的话会白白压缩一次。
func isContextOverflow(reason string) bool {
	lower := strings.ToLower(reason)
	// 先排除：这些是限流/服务端错误，跟"内容太多"无关
	for _, marker := range nonOverflowMarkers {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	for _, marker := range overflowMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	// HTTP 413 = Payload Too Large，不看 body
	if hasStatus413(reason) {
		return true
	}
	return false
}

// overflowMarkers 是"内容太多"的各种说法（小写匹配）。
var overflowMarkers = []string{
	// token 超窗口
	"context_length_exceeded",
	"context length exceeded",
	"maximum context length",
	"prompt is too long",
	"prompt too long",
	"context window exceeded",
	"exceeds the context window",
	"exceeded model token limit",
	"too many tokens",
	"token limit exceeded",
	"reduce the length of the messages",
	"input is too long",
	"exceeds the available context size",
	"greater than the context length",
	// 请求体过大（按字节，不是 token）
	"entry too big",
	"entrytoobig",
	"request entity too large",
	"request_too_large",
	"payload too large",
	"request too large",
}

// nonOverflowMarkers 是**不能**当成溢出的情况（限流、服务不可用）。
//
// 必须放在溢出判定之前：这些报错里常夹带 "too many tokens" 之类的措辞，
// 而它们要的是"等一会儿重试"，压缩历史毫无帮助、还会白白丢掉上下文。
var nonOverflowMarkers = []string{
	"rate limit",
	"ratelimit",
	"too many requests",
	"throttling",
	"throttlingexception",
	"slow down",
	"quota exceeded",
	"service unavailable",
}

// hasStatus413 报告错误里是否以 HTTP 413 的形式出现。
//
// 只看"状态码位置"，不能简单 contains("413")：
// request id、token 数里都可能出现 413，误判会白白压缩一次。
// 接受的形态（对齐 pi 的 /^(?:[\w -]+ API error \(413\):|413(?:\s|:|$))/）：
//
//	http (HTTP 413): ...      ← 我们自己的 FailStreamWith 前缀
//	some-provider API error (413): ...
//	413 {"error":...}
//	... 413（结尾）
func hasStatus413(reason string) bool {
	lower := strings.ToLower(reason)
	// "(413)" 覆盖 "API error (413)"；
	// "(http 413)" 覆盖我们自己的 "http (HTTP 413): ..." 前缀
	// —— 注意这里 413 前面是空格不是括号，不能用同一个子串匹配。
	if strings.Contains(lower, "(413)") || strings.Contains(lower, "http 413") {
		return true
	}
	trimmed := strings.TrimSpace(lower)
	if strings.HasPrefix(trimmed, "413") {
		// 要求 413 后面不是数字（4130 不是 413）
		rest := trimmed[3:]
		if rest == "" {
			return true
		}
		switch rest[0] {
		case ' ', ':', '(', '{', '-':
			return true
		}
	}
	return false
}

// IsContextOverflowForTest 暴露溢出判定（测试用）。
func IsContextOverflowForTest(reason string) bool { return isContextOverflow(reason) }

// RetryableFailureForTest 暴露"这个失败值得自动重试吗"（测试用）。
func RetryableFailureForTest(reason string) bool { return retryableFailure(reason) }

// SetBusyForTest 强行改变编排状态（测试用）。
//
// 用途：造"别的会话看到我正卡在工具里"这种场面 —— 真跑一个慢工具
// 既慢又不稳。**不要在生产路径里用**：状态由流/工具边界推进。
func (d *Driver) SetBusyForTest(busy Busy) { d.busy = busy }

// WorkspaceForTest 返回 driver 当前记录的工作区（测试用）。
func (d *Driver) WorkspaceForTest() string { return d.opts.Workspace }

// SetWorkspace 设置会话的工作区。
//
// 用途：host 打开会话时先用启动目录构造 driver（那时还没解析出日志里
// 记的工作区），解析完再把真值写回来。不回写的话，下一次压缩会拿
// 启动目录当工作区写进快照，把错误固化下来。
func (d *Driver) SetWorkspace(workspace string) { d.opts.Workspace = workspace }

// isTruncated 报告停止原因是否是「达到长度上限」。
func isTruncated(reason string) bool {
	switch strings.ToLower(reason) {
	case "length", "max_tokens":
		return true
	}
	return false
}
