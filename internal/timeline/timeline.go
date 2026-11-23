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
func Build(entries []conversation.Entry, idle bool, usage *Usage) []Item {
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
			Closed: closed, StartedAt: roundStart, Usage: usage,
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
		items = append(items, roundItems(turn, RoundInfo{Closed: false, StartedAt: roundStart})...)
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
}

// CacheHitBasisPoints 返回缓存命中率（千分比，0..1000）。
//
// 口径：命中 / (命中 + 未命中)。
//   - 命中 = CacheReadTokens（读缓存的部分，按 10% 计价）
//   - 未命中 = InputTokens（真正重新计算的部分）
//
// **不把 CacheWriteTokens 算进分母**：它是"写缓存的量"，费用和普通输入不同，
// 混进命中率里会让第一次请求（全写入）显示成 0%，看着像缓存没用。
//
// 没有任何输入时返回 -1（调用方据此不显示）。
func (u Usage) CacheHitBasisPoints() int {
	if u.CacheReadTokens <= 0 {
		return -1
	}
	total := u.CacheReadTokens + u.InputTokens
	if total <= 0 {
		return -1
	}
	return u.CacheReadTokens * 1000 / total
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
	// **流式期间也生成**（标记 Live）：用户要在执行过程中看到
	// 运行时间、token 和缓存率，而不是等跑完才有这一行。
	// 结束后同一个 item 变成"已完成 + 总计"，**始终只有一条**。
	last := turn[len(turn)-1]
	if last != nil {
		streaming := last.Status().IsStreaming()
		// 非流式时要求回合已闭合（后面已有新 user 消息，或会话空闲）：
		// 工具轮之间、等待回答时回合还没结束，显示「已完成」会一闪一闪。
		// 流式时无条件显示——那时它表达的是"正在跑"。
		if streaming || info.Closed {
			items = append(items, Item{
				Kind:        KindRoundFooter,
				Key:         last.Message.MessageID + "#footer",
				Refs:        []Ref{{Chat: last}},
				TurnStart:   info.StartedAt,
				TurnEntries: turn,
				Usage:       info.Usage,
				Live:        streaming,
			})
		}
	}
	return items
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
