package driver

import (
	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/render"
	"simple-agent/internal/store"
)

// Mirror 是展示端的**只读镜像**：它复用同一份 Conversation 与同一套"应用增量"的
// 代码，但绝不做任何决策。
//
// 为什么需要单独的入口：`ApplyEvent` 在 Done/Error 时会直接改 Conversation
// （结束消息、补工具结果、投递插话、加通知…），执行端随后还会把同样的变化作为
// 持久事件发出来。展示端如果也跑一遍，就会出现两条 user 消息、重复的工具结果、
// 翻倍的通知——丢掉 effect 拦不住，因为那些动作不是 effect，是直接改状态。
//
// 所以镜像只做两件事：
//   - ApplyStreamDelta：把模型已经说出来的内容写进 part（文字/推理/tool args）；
//   - ApplyPersisted：把执行端的持久事件应用到状态上（与 Replay 同一份逻辑）。
//
// 消息的结束、工具结果、插话产生的新消息、显示型提示、Busy 状态，一律由执行端
// 的持久事件与 state.delta 决定，镜像不自己推断。
type Mirror struct {
	drv      *Driver
	replayed *Replayed
}

// NewMirror 用重放结果构造镜像。
func NewMirror(replayed *Replayed, opts Options) *Mirror {
	drv := &Driver{
		conv:   replayed.Conversation,
		opts:   opts,
		mirror: true,
	}
	return &Mirror{drv: drv, replayed: replayed}
}

// Conversation 返回镜像内容（只读）。
//
// 取的是**当前**会话：增量应用可能因为 session.created / compact.snapshot 而替换
// 整个 Conversation 对象（见 replayState.apply），缓存旧指针会读到过期内容。
func (m *Mirror) Conversation() *conversation.Conversation {
	if m.replayed != nil && m.replayed.Conversation != nil {
		m.drv.conv = m.replayed.Conversation
	}
	return m.drv.conv
}

// ApplyStreamDelta 应用一条**增量** provider 事件（文字/推理/tool args）。
//
// Done/Error 不转发给镜像（见设计 §3.3.1），因此这里不需要收尾逻辑；
// 传进来也只会被忽略。
func (m *Mirror) ApplyStreamDelta(messageID string, ev providers.Event) error {
	m.drv.beginMirrorTurn(messageID)
	_, err := m.drv.ApplyStreamDelta(messageID, ev)
	return err
}

// ApplyPersisted 应用一条持久事件（与 Replay 共用同一份 apply 逻辑）。
func (m *Mirror) ApplyPersisted(event store.Event) error {
	return ApplyPersisted(m.replayed, event)
}

// beginMirrorTurn 为镜像准备一个"轮"（增量写入需要 reasoning/tool part 的下标）。
//
// 镜像的轮在会话里**已经存在**（日志里那条 streaming 空壳，或 state.snapshot 里的
// Inflight 内容），所以这里只补一个 Turn 记录，不新建消息。镜像经常在**流的中途**
// 接管（中途连上、断线重连），因此必须从已有 part 完整恢复增量元数据：
//
//   - 推理 part 的下标；
//   - 每个工具 part 对应的 **provider 下标**（Anthropic 是 content block 下标，
//     不是"第几个工具"；按出现顺序编号会在"前面有个 text 块"时错位）；
//   - 每个工具的缓冲区（id / name / 已经收到的参数前缀），否则接管后的第一个
//     参数增量会用空 id/name 覆盖整个 part，工具卡片当场变成空白。
func (d *Driver) beginMirrorTurn(messageID string) {
	if d.turn != nil && d.turn.messageID == messageID {
		return
	}
	turn := &Turn{
		messageID:     messageID,
		buffers:       map[int]*toolBuffer{},
		toolParts:     map[int]int{},
		reasoningPart: -1,
	}
	if entry, ok := d.conv.Message(messageID); ok {
		sequential := 0
		for index, part := range entry.Message.Parts {
			switch part.MediaType {
			case render.MediaReasoning:
				turn.reasoningPart = index
			case render.MediaToolUse:
				data, ok := partData(part)
				if !ok {
					continue
				}
				streamIndex, ok := toInt(data["stream_index"])
				if !ok {
					// 旧日志/旧数据没有 stream_index：按出现顺序兜底
					streamIndex = sequential
				}
				sequential++
				turn.toolParts[streamIndex] = index
				buf := turn.buffer(streamIndex)
				buf.id, _ = data["id"].(string)
				buf.name, _ = data["name"].(string)
				if raw, _ := data["raw_args"].(string); raw != "" {
					// part 里的 raw_args 已经是**截断后的前缀**（clipRawArgs），
					// 与执行端缓冲区的前缀一致：接着追加、再截一次，结果相同。
					buf.args.WriteString(raw)
				}
			}
		}
	}
	d.turn = turn
}

// ReplaceInflight 用 snapshot 里的完整内容替换正在流式输出的消息 parts。
//
// 中途连上（或重连）时，日志里只有一条 streaming 空壳，之前已经输出的文字/推理/
// tool args 不在日志里；执行端在 state.snapshot 里带上当前完整 parts，这里整体替换，
// 之后再接 seq 更大的增量（见设计 §3.3.1）。
func (m *Mirror) ReplaceInflight(messageID string, parts []message.Part, status conversation.EntryStatus) error {
	// 整份覆盖后，这一轮的增量元数据（reasoning/tool 下标与缓冲区）要重建；
	// 先丢掉旧的 Turn，下一次 ApplyStreamDelta 会从新的 parts 里恢复。
	m.drv.turn = nil
	if _, ok := m.drv.conv.Message(messageID); ok {
		// 重放收尾会把 streaming 空壳判成「已中断」，这里整份覆盖并恢复 streaming
		return m.drv.conv.SetStreamingParts(messageID, parts)
	}
	_, err := m.drv.conv.AddMessage(message.Message{
		MessageID: messageID,
		Role:      message.RoleAgent,
		Parts:     parts,
	}, status)
	return err
}
