// Package timeline 把会话切分成界面要渲染的条目（正文 / 过程组 / 提示 / 回合结束行）。
//
// 为什么单独成包：这段分组规则（哪些 part 算"过程"、什么情况下算一个回合、
// 回合何时结束、过程组标题怎么拼）是**界面的语义**，桌面（uiapp）与 Android
// （mobile/bridge）必须给出同一个结果，否则"手机和电脑看到的不一样"。
// 所以规则只在这里写一份，两边都调用它；两边各自只负责把结果画出来。
//
// 包本身不依赖任何 UI 库（mygo / Android 都不碰），只依赖 conversation / message /
// render / locale，因此可以随 gomobile 编到手机上去。
package timeline

import (
	"sort"
	"strings"
	"time"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/render"
)

// Kind 是渲染条目的类型。
type Kind string

const (
	// KindNotice 是本地通知（连接状态、日志修复提示等）。
	KindNotice Kind = "notice"
	// KindContent 是正文（用户气泡或助手输出）。
	KindContent Kind = "content"
	// KindProcess 是折叠的过程组（思考 / 工具调用 / 工具结果，可能跨消息）。
	KindProcess Kind = "process"
	// KindRoundFooter 是回合结束行（「已完成，用时 22s」）。
	KindRoundFooter Kind = "footer"
)

// Ref 指向某条消息里的一个 part。
type Ref struct {
	Chat  *conversation.ChatEntry
	Index int
}

// Part 返回这个引用指向的 part。
//
// **必须做边界检查**：Ref 由 timeline.Build 生成，而 ChatEntry 的内容
// 可能在生成之后被改写（流式追加、镜像重放、backfill 把同一批事件
// 重放到另一份 Conversation）。一旦 Parts 变短，裸下标就会
// panic: index out of range —— 真机上表现为 App 直接消失，且日志里
// 只有一行 panic（拿不到栈）。
//
// 返回零值 part 而不是崩溃：上层会把它渲染成空，比进程死掉好得多。
func (r Ref) Part() message.Part {
	if r.Chat == nil || r.Chat.Message == nil {
		return message.Part{}
	}
	if r.Index < 0 || r.Index >= len(r.Chat.Message.Parts) {
		return message.Part{}
	}
	return r.Chat.Message.Parts[r.Index]
}

// Item 是会话列表里的一行。
type Item struct {
	Kind Kind
	// Key 是稳定标识（界面列表的 key / 展开状态的 key）。
	Key    string
	Notice *conversation.Notice
	Refs   []Ref
	// Live 表示这个**过程组自己**还在进行（决定界面是否显示转圈的动画）。
	//
	// 早先是拿"整个会话是否空闲"来当这个标志（`streaming := !idle`），
	// 结果只要会话在跑，几百轮之前的每个历史工具组都跟着转圈。
	// 真正该转圈的只有"最后一组、且回合还没结束、且消息还在流式"的那一个。
	Live bool
	// TurnStart / TurnEntries 只在回合结束行上有值：回合起点与该回合的所有 agent 消息。
	TurnStart   time.Time
	TurnEntries []*conversation.ChatEntry
	// Usage 是本轮的 token 用量（只在结束行上有值；nil 表示拿不到）。
	Usage *Usage
}

// Build 把一份会话条目列表切成渲染条目。
//
// idle 表示会话当前空闲（决定最后一个回合算不算"已结束"：还在流式输出时
// 不该显示「已完成」）。
//
// 分组对齐 dsh 的 turn process：
//   - 思考 / 工具调用 / 工具结果进**过程组**，连续的一段过程合成一组；
//   - 正文**不进组**，它留在会话流里，并且会结束当前组
//     （模型的每段正式输出都是独立内容，不该被折叠掉）。
//
// 于是一个回合可能是「组 → 旁白 → 组 → 回答」的样子。
// Build 把一份会话条目列表切成渲染条目。
//
// idle 表示会话当前空闲（决定最后一个回合算不算"已结束"：还在流式输出时
// 不该显示「已完成」）。
//
// running 表示这一轮**还在进行**（模型在流式，或工具正在执行）。
// 它和 idle 不是简单的取反：工具执行期间会话不空闲，但消息已经不流式了
// —— 只看消息状态会让「● 运行中」这一行**突然消失**，
// 用户以为任务结束了（实际工具还在跑）。三种状态的真实含义：
//
//	idle=true                 → 回合已结束，显示「已完成 + 总计」
//	running=true              → 回合进行中，显示「● 运行中 + 实时数据」
//	两者都 false（如重放历史）→ 当作已结束
//
// usage 是本轮的 token 用量（含缓存字段），用于回合结束行显示缓存命中率。
// 传 nil 表示拿不到（重放的历史会话没有这个数据）——那时不显示缓存率，
// 比显示一个编出来的 0% 诚实。
//
// 分组对齐 dsh 的 turn process：
//   - 思考 / 工具调用 / 工具结果进**过程组**，连续的一段过程合成一组；
//   - 正文**不进组**，它留在会话流里，并且会结束当前组
//     （模型的每段正式输出都是独立内容，不该被折叠掉）。
//
// 于是一个回合可能是「组 → 旁白 → 组 → 回答」的样子。
func Build(entries []conversation.Entry, idle, running bool) []Item {
	var items []Item
	var turn []*conversation.ChatEntry

	roundStart := time.Time{}
	// userStart 是最近一条 user 消息的创建时间：它才是回合起点（耗时从用户发出算起）
	userStart := time.Time{}
	flushTurn := func(closed bool) {
		if len(turn) == 0 {
			return
		}
		items = append(items, roundItems(turn, RoundInfo{
			Closed: closed, StartedAt: roundStart, Running: running && !closed,
		})...)
		turn = nil
		roundStart = time.Time{}
		userStart = time.Time{}
	}
	// flushPartialTurn 只把**已累积的过程段**冲出去（不带结束行），回合继续累积。
	flushPartialTurn := func() {
		if len(turn) == 0 {
			return
		}
		items = append(items, roundItems(turn, RoundInfo{
			Closed: false, StartedAt: roundStart, Running: running,
		})...)
		turn = nil
		// roundStart 保留：整个回合仍然从同一条 user 消息起算
	}

	for _, entry := range entries {
		if entry.Notice != nil {
			// 通知**不结束回合**：回合中间插入的提示不能把前半段当成已结束的回合，
			// 否则会闪出「已完成」并重新计时。
			flushPartialTurn()
			items = append(items, Item{Kind: KindNotice, Key: entry.ID(), Notice: entry.Notice})
			continue
		}
		chat := entry.Message
		if chat == nil {
			continue
		}
		if chat.Message.Role == message.RoleUser {
			flushTurn(true)
			userStart = chat.CreatedAt
			var refs []Ref
			for index := range chat.Message.Parts {
				refs = append(refs, Ref{Chat: chat, Index: index})
			}
			items = append(items, Item{Kind: KindContent, Key: entry.ID(), Refs: refs})
			continue
		}
		if roundStart.IsZero() {
			roundStart = chat.CreatedAt
			if !userStart.IsZero() {
				roundStart = userStart // 耗时从用户发出那条消息算起
			}
		}
		turn = append(turn, chat)
	}
	// 最后一个回合：只有会话空闲（且没有正在流式的消息）才算结束
	flushTurn(idle)
	return items
}

// RoundInfo 是一个回合的边界信息（回合结束行用）。
// RoundInfo 是构建一个回合条目时需要的上下文。
type RoundInfo struct {
	// Closed 表示回合真的结束了（后面已有新的 user 消息，或会话已空闲）。
	// 工具轮之间、等待回答时都还没结束——那时不该显示「已完成」。
	Closed bool
	// StartedAt 是回合开始时间：回合前那条 user 消息的 CreatedAt；没有就用第一条消息的。
	StartedAt time.Time
	// Usage 是本轮的 token 用量（含缓存字段），用于结束行的缓存率。
	// nil 表示拿不到（重放的历史会话）——那时不显示缓存率。
	Usage *Usage
	// Running 表示这一轮**还在进行**（模型在流式，或工具正在执行）。
	//
	// 与 Closed 是两件事：
	//   - Closed 说"这个回合结束了没有"
	//   - Running 说"现在有没有在跑"
	// 工具执行期间两者都是 false/true 的组合：回合没结束、但确实在跑。
	// 只看消息的 streaming 状态会让「运行中」那一行消失（用户以为结束了）。
	Running bool
}

// Usage 是本轮的 token 用量。
//
// 单独定义而不是复用 driver.Usage：timeline 包不依赖 driver
// （它要能编到手机上，依赖面越小越好）。
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	// InputExcludesCache 表示 InputTokens **不含**缓存命中的部分。
	//
	// 两种协议的 usage 语义不同，算命中率时分母也不一样：
	//   - Anthropic：input 只算没命中缓存的，完整输入 = read+write+input
	//   - DeepSeek/OpenAI：prompt_tokens **已含**命中部分，完整输入 = input
	//
	// 混用的后果：DeepSeek 的命中部分被重复计入分母，命中率凭空减半
	// （实测 99.53% 被算成 49.92%）。
	InputExcludesCache bool
}

// CacheHitBasisPoints 返回缓存命中率（**万分比**，0..9999）。
//
// 单位是万分比而不是千分比：显示要保留两位小数，`9993` → `99.93%`。
// 与 uiapp.cacheHitBasisPoints（状态栏累计命中率）同一个口径。
//
// ⚠️ 曾经写成 `read / (read + input)`：对 Anthropic 是对的，
// 但 DeepSeek 的 input 已含命中部分，等于把命中重复计入分母 ——
// 真实的 99.53% 被显示成 49.92%。必须按 InputExcludesCache 分支。
//
// 没有任何输入时返回 -1（调用方据此不显示）。
func (u Usage) CacheHitBasisPoints() int {
	if u.CacheReadTokens <= 0 {
		return -1
	}
	var total int
	if u.InputExcludesCache {
		// Anthropic：三者相加才是完整输入
		total = u.CacheReadTokens + u.CacheWriteTokens + u.InputTokens
	} else {
		// OpenAI 兼容：input 已含命中部分。**不能**再加 read。
		total = u.InputTokens
		if total < u.CacheReadTokens {
			// 防御：个别网关的 input 不含缓存，以较大值为准
			total = u.CacheReadTokens
		}
	}
	if total <= 0 {
		return -1
	}
	points := u.CacheReadTokens * 10_000 / total
	// 上限压到 99.99%：本轮新增的提示词必然要重新计算，
	// 显示 100% 会让人以为"一分钱没花"，反而不可信（与状态栏同一条规则）。
	if points > 9_999 {
		points = 9_999
	}
	return points
}

// TotalTokens 返回本轮的总 token 数（输入 + 输出，含缓存部分）。
func (u Usage) TotalTokens() int {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// roundItems 把一个回合切成按顺序排列的条目：过程组与正文交替。
func roundItems(turn []*conversation.ChatEntry, info RoundInfo) []Item {
	var items []Item
	var pending []Ref

	flushGroup := func(last bool) {
		if len(pending) == 0 {
			return
		}
		first := pending[0]
		items = append(items, Item{
			Kind: KindProcess,
			Key:  "proc:" + first.Chat.Message.MessageID + "#" + itoa(first.Index),
			Refs: pending,
			// 只有最后一个过程组可能"还在进行"：它后面没有正文、
			// 回合还没结束（info.Closed 为假）、且这一组引用的消息
			// 仍处于流式状态。其余情况一律已结束。
			Live: last && !info.Closed && anyRefStreaming(pending),
		})
		pending = nil
	}

	for _, chat := range turn {
		for index, part := range chat.Message.Parts {
			if IsEmptyTextPart(part) {
				continue
			}
			ref := Ref{Chat: chat, Index: index}
			if IsProcessPart(part) {
				pending = append(pending, ref)
				continue
			}
			// 正文：结束当前过程组，正文自己留在组外
			flushGroup(false)
			items = append(items, Item{
				Kind: KindContent,
				Key:  chat.Message.MessageID + "#" + itoa(index),
				Refs: []Ref{ref},
			})
		}
	}
	// 末尾剩下的这一组是本回合的最后一组：只有它可能还在进行。
	flushGroup(true)
	// 回合结束行：dsh 的 turn footer。
	//
	// **进行中也生成**（标记 Live）：用户要在执行过程中看到运行时间、
	// token 和缓存率，而不是等跑完才有这一行。结束后同一个 item
	// 变成"已完成 + 总计"，**始终只有一条**。
	//
	// 判定用 info.Running 而不是消息的 streaming 状态：**工具执行期间
	// 消息已经不流式了**，只看 streaming 会让这一行突然消失，
	// 用户以为任务结束（实际工具还在跑）。
	last := turn[len(turn)-1]
	if last != nil {
		live := info.Running && !info.Closed
		// live：这一轮在跑（模型流式或工具执行中）→ 显示"运行中 + 实时数据"
		// info.Closed：回合已结束 → 显示"已完成 + 本轮总计"
		// 两者都不成立（工具轮之间等）→ 不显示，避免"已完成"一闪一闪
		if live || info.Closed {
			items = append(items, Item{
				Kind:        KindRoundFooter,
				Key:         last.Message.MessageID + "#footer",
				Refs:        []Ref{{Chat: last}},
				TurnStart:   info.StartedAt,
				TurnEntries: turn,
				// 用量从**这一轮自己的消息**汇总，不是会话累计。
				//
				// 以前界面传进来的是 session.Usage()（会话累计值），
				// 于是每个回合的结束行都显示同一个数字 —— 用户一眼看出
				// "所有轮都一样"是错的。现在每轮的用量随消息一起留存，
				// 这里按轮汇总。
				Usage: turnUsage(turn),
				Live:  live,
			})
		}
	}
	return items
}

// turnUsage 汇总这一轮所有消息的用量（没有数据时返回 nil）。
//
// 一轮可能跨多条 agent 消息（工具调用会有多次请求），每条的用量
// 各自留存，这里相加 —— 用户看到的是"这一轮一共用了多少/省了多少"。
//
// 返回 nil 表示拿不到数据（重放的老日志没有 usage 字段）：
// 那时不显示缓存率，比显示一个编出来的 0% 诚实。
func turnUsage(turn []*conversation.ChatEntry) *Usage {
	var total Usage
	found := false
	for _, entry := range turn {
		if entry == nil {
			continue
		}
		used := entry.UsageFor()
		if used.InputTokens == 0 && used.OutputTokens == 0 &&
			used.CacheReadTokens == 0 && used.CacheWriteTokens == 0 {
			continue
		}
		found = true
		total.InputTokens += used.InputTokens
		total.OutputTokens += used.OutputTokens
		total.CacheReadTokens += used.CacheReadTokens
		total.CacheWriteTokens += used.CacheWriteTokens
	}
	if !found {
		return nil
	}
	// Anthropic 会报告 cache_creation，它的 input 不含命中部分；
	// 其余协议（DeepSeek/OpenAI 兼容）的 input 已含命中。
	total.InputExcludesCache = total.CacheWriteTokens > 0
	return &total
}

// anyRefStreaming 报告这一组里是否还有消息在流式。
//
// 过程组可能跨多条消息（模型可以说一句、调个工具、再说一句）。
// 只要组内任意一条还在流式，这一组就还可能长出新内容，界面就该继续转圈。
//
// 用"任意"而不是"最后一条"：正常时间线里消息按顺序追加，两者等价；
// 但数据异常时（例如某条状态没落盘）"任意"不会漏判。
func anyRefStreaming(refs []Ref) bool {
	for _, ref := range refs {
		if ref.Chat == nil {
			continue
		}
		if ref.Chat.Status().IsStreaming() {
			return true
		}
	}
	return false
}

// IsEmptyTextPart 报告 part 是否为"空白的文本"（这类 part 不占位置）。
func IsEmptyTextPart(part message.Part) bool {
	if part.MediaType == render.MediaReasoning || part.MediaType == render.MediaSummary {
		return false
	}
	text, ok := part.AsText()
	return ok && strings.TrimSpace(text) == ""
}

// IsProcessPart 判断 part 是否属于「过程」（默认折叠的内容）。
func IsProcessPart(part message.Part) bool {
	switch part.MediaType {
	case render.MediaReasoning, render.MediaToolUse, render.MediaToolResult:
		return true
	}
	return false
}

// TailMessage 返回回合结束行对应的那条消息（本回合最后一条）。
func TailMessage(item Item) *conversation.ChatEntry {
	if len(item.Refs) > 0 && item.Refs[0].Chat != nil {
		return item.Refs[0].Chat
	}
	return nil
}

// Duration 返回该条目所在**整个回合**的耗时（构建条目时已算好起点与成员）。
func Duration(item Item) time.Duration {
	if len(item.Refs) == 0 {
		return 0
	}
	start := item.TurnStart
	end := item.Refs[0].Chat.FinishedAt()
	if end.IsZero() {
		end = item.Refs[0].Chat.CreatedAt
	}
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start)
}

// Tokens 返回该回合所有消息的输出 token 之和（重放后为 0）。
func Tokens(item Item) int {
	total := 0
	for _, entry := range item.TurnEntries {
		total += entry.Tokens()
	}
	return total
}

// itoa 是 strconv.Itoa 的短包装（避免为了一个调用引入依赖噪音）。
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var buf [20]byte
	pos := len(buf)
	for value > 0 {
		pos--
		buf[pos] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// sortedActivities 按出现次数排序（次数相同按首次出现），返回排序后的分类。
func sortedActivities(counts map[Activity]int, order []Activity) []Activity {
	ranked := append([]Activity(nil), order...)
	sort.SliceStable(ranked, func(i, j int) bool { return counts[ranked[i]] > counts[ranked[j]] })
	return ranked
}
