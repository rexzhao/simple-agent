package host_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
	"simple-agent/internal/wire"
)

// p2Runtime 记录打开的流，测试手动推事件。
type p2Runtime struct {
	mu      sync.Mutex
	streams []chan providers.Event
}

func newP2Runtime() *p2Runtime { return &p2Runtime{} }

func (r *p2Runtime) OpenStream(ctx context.Context, sel providers.Selection, req providers.ChatRequest) (<-chan providers.Event, error) {
	events := make(chan providers.Event, 64)
	r.mu.Lock()
	r.streams = append(r.streams, events)
	r.mu.Unlock()
	return events, nil
}

func (r *p2Runtime) RunTools(ctx context.Context, tc *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
	out := make([]tools.ToolOutcome, 0, len(calls))
	for _, call := range calls {
		out = append(out, tools.ToolOutcome{ID: call.ID, Name: call.Name, Output: "ok"})
	}
	return out
}

func (r *p2Runtime) stream(n int) chan providers.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streams[n-1]
}

// await 等到第 n 条流打开，返回它。
func (r *p2Runtime) await(t *testing.T, n int) chan providers.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		if len(r.streams) >= n {
			channel := r.streams[n-1]
			r.mu.Unlock()
			return channel
		}
		r.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("第 %d 条流没有打开", n)
	return nil
}

func (r *p2Runtime) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.streams)
}

// p2Pair 起一个执行端 + 一个展示端（进程内管道）。
type p2Pair struct {
	local  *host.Local
	remote *host.Remote
	post   *poster
	sessID string
	rt     *p2Runtime
	viewer wire.Conn
}

func newP2Pair(t *testing.T, withCatalog bool) *p2Pair {
	t.Helper()
	return newP2PairWith(t, withCatalog, nil)
}

func newP2PairWith(t *testing.T, withCatalog bool, mutate func(*host.Config)) *p2Pair {
	t.Helper()
	rt := newP2Runtime()
	p := &poster{}
	cfg := host.Config{
		Runtime:       rt,
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Workspace:     t.TempDir(),
		MaxToolRounds: 3,
		ContextWindow: 200_000,
		Post:          p.Post,
	}
	if withCatalog {
		cfg.Catalog = store.NewCatalog(t.TempDir())
	}
	if mutate != nil {
		mutate(&cfg)
	}
	local := host.NewLocal(cfg)
	t.Cleanup(func() { _ = local.Close() })

	viewer, executor := wire.PipePair()
	go func() { _ = host.Serve(executor, local, p.Post) }()
	remote, err := host.NewRemote(viewer, p.Post)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })

	if local.Active() == nil {
		// 有 Catalog 的执行端在"没有任何会话"时是空态（用户选 B），测试里显式建一个
		local.NewSession("")
	}
	pair := &p2Pair{local: local, remote: remote, post: p, rt: rt, sessID: local.Active().ID, viewer: viewer}
	pair.pumpUntil(t, func() bool { return len(remote.Sessions()) == 1 })
	return pair
}

func (p *p2Pair) pumpUntil(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.post.pump()
		if done() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}

func (p *p2Pair) send(t *testing.T, text string) {
	t.Helper()
	payload, _ := host.EncodePayload(host.SendPayload{Text: text})
	if _, err := p.local.Submit(host.Command{SessionID: p.sessID, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

// P2 验收 1：执行端流式输出，展示端逐帧文本一致。
func TestViewerSeesStreamingText(t *testing.T) {
	pair := newP2Pair(t, false)
	pair.send(t, "你好")
	live := pair.rt.await(t, 1)
	live <- providers.Event{Kind: providers.EventText, Text: "你好"}
	live <- providers.Event{Kind: providers.EventText, Text: "！"}
	pair.pumpUntil(t, func() bool {
		session := pair.remote.Session(pair.sessID)
		return session != nil && strings.Contains(lastAgentText(session), "你好！")
	})
	live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	pair.pumpUntil(t, func() bool {
		session := pair.remote.Session(pair.sessID)
		return session != nil && session.Busy() == driver.BusyIdle
	})
}

// P2 验收 2：中途连上时，snapshot 的 Inflight 补齐已经输出的内容。
func TestViewerSeesInflightText(t *testing.T) {
	pair := newP2Pair(t, false)
	pair.send(t, "你好")
	live := pair.rt.await(t, 1)
	live <- providers.Event{Kind: providers.EventText, Text: "已经输出的一半"}
	pair.pumpUntil(t, func() bool {
		session := pair.remote.Session(pair.sessID)
		return session != nil && len(session.Conversation().Entries()) >= 2
	})

	// 第二个展示端在输出中途连上
	viewer2, executor2 := wire.PipePair()
	go func() { _ = host.Serve(executor2, pair.local, pair.post.Post) }()
	remote2, err := host.NewRemote(viewer2, pair.post.Post)
	if err != nil {
		t.Fatal(err)
	}
	defer remote2.Close()
	pair.pumpUntil(t, func() bool {
		session := remote2.Session(pair.sessID)
		return session != nil && strings.Contains(lastAgentText(session), "已经输出的一半")
	})
}

// P2 验收 3：断线期间执行端继续产生事件，重连后只补增量且**两端一致**。
func TestViewerReconnectResumes(t *testing.T) {
	pair := newP2Pair(t, true)
	pair.send(t, "你好")
	live := pair.rt.await(t, 1)
	live <- providers.Event{Kind: providers.EventText, Text: "第一段"}
	live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	pair.pumpUntil(t, func() bool {
		session := pair.remote.Session(pair.sessID)
		return session != nil && session.Busy() == driver.BusyIdle
	})

	// 断开连接（执行端会注销这个观察者）
	_ = pair.viewer.Close()
	pair.post.pump()
	time.Sleep(20 * time.Millisecond)

	// **断线期间**再跑完整一轮：新 user 消息 + 流式输出 + 工具调用
	pair.send(t, "断线期间的问题")
	live2 := pair.rt.await(t, 2)
	live2 <- providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"}
	live2 <- providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"x"}`}
	live2 <- providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"}
	pair.pumpUntil(t, func() bool { return pair.rt.count() >= 3 || pair.local.Active().Busy() == driver.BusyIdle })
	if pair.rt.count() >= 3 {
		live3 := pair.rt.await(t, 3)
		live3 <- providers.Event{Kind: providers.EventText, Text: "断线期间的回答"}
		live3 <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	}
	local := pair.local.Active()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && local.Busy() != driver.BusyIdle {
		pair.post.pump()
		time.Sleep(2 * time.Millisecond)
	}

	// 重连：换一条新管道，带游标
	viewer2, executor2 := wire.PipePair()
	go func() { _ = host.Serve(executor2, pair.local, pair.post.Post) }()
	if err := pair.remote.Reconnect(viewer2); err != nil {
		t.Fatal(err)
	}
	pair.pumpUntil(t, func() bool { return pair.remote.AttachCountForTest(pair.sessID) >= 2 })
	pair.pumpUntil(t, func() bool {
		remote := pair.remote.Session(pair.sessID)
		return remote != nil && strings.Contains(lastAgentText(remote), "断线期间的回答")
	})
	if pair.remote.LastAttachTruncated(pair.sessID) {
		t.Fatalf("ring 覆盖得到时不该发全量")
	}
	if got, want := digestOf(t, pair.remote.Session(pair.sessID)), digestOf(t, pair.local.Active()); got != want {
		t.Fatalf("重连后两端不一致：remote=%s local=%s", got, want)
	}
}

// P2 验收 4：ring 覆盖不到时退回全量（truncated=true），两端一致。
func TestViewerSnapshotFallback(t *testing.T) {
	// ring 只留 1 条：跑完两轮后断线游标必然覆盖不到
	pair := newP2PairWith(t, true, func(cfg *host.Config) { cfg.RingLimit = 1 })
	pair.send(t, "第一轮")
	live := pair.rt.await(t, 1)
	live <- providers.Event{Kind: providers.EventText, Text: "回答一"}
	live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	pair.pumpUntil(t, func() bool {
		session := pair.remote.Session(pair.sessID)
		return session != nil && session.Busy() == driver.BusyIdle
	})

	_ = pair.viewer.Close()
	pair.post.pump()
	time.Sleep(20 * time.Millisecond)

	// 断线期间再跑一轮（产生多条事件，把 ring 挤爆）
	pair.send(t, "第二轮")
	live2 := pair.rt.await(t, 2)
	live2 <- providers.Event{Kind: providers.EventText, Text: "回答二"}
	live2 <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	local := pair.local.Active()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && local.Busy() != driver.BusyIdle {
		pair.post.pump()
		time.Sleep(2 * time.Millisecond)
	}

	viewer2, executor2 := wire.PipePair()
	go func() { _ = host.Serve(executor2, pair.local, pair.post.Post) }()
	if err := pair.remote.Reconnect(viewer2); err != nil {
		t.Fatal(err)
	}
	pair.pumpUntil(t, func() bool { return pair.remote.AttachCountForTest(pair.sessID) >= 2 })
	pair.pumpUntil(t, func() bool {
		remote := pair.remote.Session(pair.sessID)
		return remote != nil && strings.Contains(lastAgentText(remote), "回答二")
	})
	if !pair.remote.LastAttachTruncated(pair.sessID) {
		t.Fatalf("ring 覆盖不到时应走全量")
	}
	if got, want := digestOf(t, pair.remote.Session(pair.sessID)), digestOf(t, local); got != want {
		t.Fatalf("全量重建后两端不一致：remote=%s local=%s", got, want)
	}
}

// P2 验收 5：工具调用与结果在展示端同样可见（过程组标题靠它渲染）。
func TestViewerSeesToolRun(t *testing.T) {
	pair := newP2Pair(t, false)
	pair.send(t, "读文件")
	live := pair.rt.await(t, 1)
	live <- providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"}
	live <- providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`}
	live <- providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"}

	// 第二轮（工具结果回填后模型继续）
	pair.pumpUntil(t, func() bool { return pair.rt.count() >= 2 || hasToolResult(pair.remote.Session(pair.sessID)) })
	if pair.rt.count() >= 2 {
		live2 := pair.rt.await(t, 2)
		live2 <- providers.Event{Kind: providers.EventText, Text: "读完了"}
		live2 <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	}
	pair.pumpUntil(t, func() bool {
		session := pair.remote.Session(pair.sessID)
		return session != nil && hasToolResult(session) && session.Busy() == driver.BusyIdle
	})
}

// hasToolResult 报告会话里是否有已完成、非错误的工具结果。
func hasToolResult(session *host.Session) bool {
	if session == nil {
		return false
	}
	for _, entry := range session.Conversation().Entries() {
		if entry.Message == nil || entry.Message.Message == nil {
			continue
		}
		for _, part := range entry.Message.Message.Parts {
			if part.MediaType != "application/vnd.simple-agent.tool-result+json" {
				continue
			}
			data, ok := part.AsData()
			if !ok {
				continue
			}
			fields, ok := data.(map[string]any)
			if !ok {
				continue
			}
			if failed, _ := fields["is_error"].(bool); !failed && fields["output"] == "ok" {
				return true
			}
		}
	}
	return false
}

// lastAgentText returns the last agent message's visible text.
func lastAgentText(session *host.Session) string {
	if session == nil {
		return ""
	}
	var text string
	for _, entry := range session.Conversation().Entries() {
		if entry.Message == nil || entry.Message.Message == nil {
			continue
		}
		if entry.Message.Message.Role != message.RoleAgent {
			continue
		}
		var builder strings.Builder
		for _, part := range entry.Message.Message.Parts {
			if value, ok := part.AsText(); ok {
				builder.WriteString(value)
			}
		}
		text = builder.String()
	}
	return text
}

// hasToolResult reports a completed, non-error tool result.

// digestOf serializes a conversation into a stable string for deep comparison.
//
// Notices 单独排序比较：显示型提示不落盘，重连/后连上来的端会把它放在
// 不同的位置（执行端在事件之前，补同步时在事件之后），位置差异不代表状态不同。
func digestOf(t *testing.T, session *host.Session) string {
	t.Helper()
	type row struct {
		Role   string
		Status string
		Parts  []message.Part
		Notice string
	}
	rows := make([]row, 0)
	for _, entry := range session.Conversation().Entries() {
		if entry.Notice != nil {
			// "未启用持久化"是**本端**的合成提示（每个展示端各有一条，可能重复合并），
			// 不属于执行端会话状态，比较时忽略；真实提示仍按顺序比较。
			if strings.Contains(entry.Notice.Text, "未启用持久化") {
				continue
			}
			rows = append(rows, row{Notice: entry.Notice.Text})
			continue
		}
		if entry.Message == nil || entry.Message.Message == nil {
			continue
		}
		rows = append(rows, row{
			Role:   string(entry.Message.Message.Role),
			Status: string(entry.Message.Status()),
			Parts:  entry.Message.Message.Parts,
		})
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// P2: two viewers both receive; closing one does not affect the other.
func TestTwoViewersBothReceive(t *testing.T) {
	pair := newP2Pair(t, false)

	viewer2, executor2 := wire.PipePair()
	go func() { _ = host.Serve(executor2, pair.local, pair.post.Post) }()
	second, err := host.NewRemote(viewer2, pair.post.Post)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	pair.pumpUntil(t, func() bool { return len(second.Sessions()) == 1 })

	pair.send(t, "两个展示端")
	live := pair.rt.await(t, 1)
	live <- providers.Event{Kind: providers.EventText, Text: "都该看到"}
	live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	pair.pumpUntil(t, func() bool {
		first := pair.remote.Session(pair.sessID)
		other := second.Session(pair.sessID)
		return first != nil && other != nil &&
			strings.Contains(lastAgentText(first), "都该看到") &&
			strings.Contains(lastAgentText(other), "都该看到")
	})
	if got, want := digestOf(t, second.Session(pair.sessID)), digestOf(t, pair.local.Active()); got != want {
		t.Fatalf("第二个展示端与执行端不一致：remote=%s local=%s", got, want)
	}

	// close the first viewer: the second must keep receiving
	_ = pair.viewer.Close()
	pair.post.pump()
	time.Sleep(10 * time.Millisecond)

	pair.send(t, "第一个断开之后")
	live2 := pair.rt.await(t, 2)
	live2 <- providers.Event{Kind: providers.EventText, Text: "第二个还在"}
	live2 <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	pair.pumpUntil(t, func() bool {
		other := second.Session(pair.sessID)
		return other != nil && strings.Contains(lastAgentText(other), "第二个还在")
	})
}

// P2: a compaction happened while disconnected -> generation changes -> full rebuild.
func TestViewerReconnectAfterCompact(t *testing.T) {
	pair := newP2Pair(t, true)
	pair.send(t, "第一轮")
	live := pair.rt.await(t, 1)
	live <- providers.Event{Kind: providers.EventText, Text: "回答一"}
	live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	pair.pumpUntil(t, func() bool {
		session := pair.remote.Session(pair.sessID)
		return session != nil && session.Busy() == driver.BusyIdle
	})

	_ = pair.viewer.Close()
	pair.post.pump()
	time.Sleep(10 * time.Millisecond)

	// compact while offline: needs a summary stream
	if _, err := pair.local.Submit(host.Command{SessionID: pair.sessID, Kind: "compact"}); err != nil {
		t.Fatal(err)
	}
	pair.pumpUntil(t, func() bool { return pair.rt.count() >= 2 || pair.local.Active().Busy() == driver.BusyIdle })
	if pair.rt.count() >= 2 {
		summaryStream := pair.rt.await(t, 2)
		summaryStream <- providers.Event{Kind: providers.EventText, Text: "摘要"}
		summaryStream <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	}
	local := pair.local.Active()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && local.Busy() != driver.BusyIdle {
		pair.post.pump()
		time.Sleep(2 * time.Millisecond)
	}

	viewer2, executor2 := wire.PipePair()
	go func() { _ = host.Serve(executor2, pair.local, pair.post.Post) }()
	if err := pair.remote.Reconnect(viewer2); err != nil {
		t.Fatal(err)
	}
	pair.pumpUntil(t, func() bool { return pair.remote.AttachCountForTest(pair.sessID) >= 2 })
	pair.pumpUntil(t, func() bool {
		remote := pair.remote.Session(pair.sessID)
		return remote != nil && remote.Busy() == driver.BusyIdle
	})
	if got, want := digestOf(t, pair.remote.Session(pair.sessID)), digestOf(t, local); got != want {
		t.Fatalf("压缩后重连两端不一致：remote=%s local=%s", got, want)
	}
}

// P2: a display-only notice produced before the viewer attached shows up after attach.
func TestViewerSeesDisplayNotice(t *testing.T) {
	// 需要持久化后端：重连时要能从日志重建（内存后端只能给增量）
	pair := newP2Pair(t, true)
	_ = pair.viewer.Close()
	pair.post.pump()
	time.Sleep(10 * time.Millisecond)

	// 先产生两条消息，再发提示：提示的位置应该在这两条之后（位置锚点）
	payload, _ := host.EncodePayload(host.SendPayload{Text: "先聊一句"})
	if _, err := pair.local.Submit(host.Command{SessionID: pair.sessID, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	live := pair.rt.await(t, 1)
	live <- providers.Event{Kind: providers.EventText, Text: "回答"}
	live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && pair.local.Active().Busy() != driver.BusyIdle {
		pair.post.pump()
		time.Sleep(2 * time.Millisecond)
	}
	pair.local.NotifyDisplay(pair.sessID, "窗口未知，按 32k 处理")

	viewer2, executor2 := wire.PipePair()
	go func() { _ = host.Serve(executor2, pair.local, pair.post.Post) }()
	if err := pair.remote.Reconnect(viewer2); err != nil {
		t.Fatal(err)
	}
	pair.pumpUntil(t, func() bool {
		remote := pair.remote.Session(pair.sessID)
		if remote == nil {
			return false
		}
		for _, entry := range remote.Conversation().Entries() {
			if entry.Notice != nil && strings.Contains(entry.Notice.Text, "按 32k 处理") {
				return true
			}
		}
		return false
	})
	// 顺序也要一致：提示在两条消息之后，而不是被追加到"看起来一样"的位置
	if got, want := digestOf(t, pair.remote.Session(pair.sessID)), digestOf(t, pair.local.Active()); got != want {
		t.Fatalf("提示位置不一致：remote=%s local=%s", got, want)
	}
}

// P2: attaching mid-stream yields exactly the executor's text (no dup, no loss).
func TestStreamAfterMidAttachNoDupNoLoss(t *testing.T) {
	pair := newP2Pair(t, false)
	pair.send(t, "流到一半")
	live := pair.rt.await(t, 1)
	live <- providers.Event{Kind: providers.EventText, Text: "A"}
	pair.pumpUntil(t, func() bool {
		session := pair.remote.Session(pair.sessID)
		return session != nil && strings.Contains(lastAgentText(session), "A")
	})

	// second viewer attaches mid-stream
	viewer2, executor2 := wire.PipePair()
	go func() { _ = host.Serve(executor2, pair.local, pair.post.Post) }()
	second, err := host.NewRemote(viewer2, pair.post.Post)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	pair.pumpUntil(t, func() bool {
		session := second.Session(pair.sessID)
		return session != nil && strings.Contains(lastAgentText(session), "A")
	})

	live <- providers.Event{Kind: providers.EventText, Text: "B"}
	live <- providers.Event{Kind: providers.EventText, Text: "C"}
	live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	pair.pumpUntil(t, func() bool {
		first := pair.remote.Session(pair.sessID)
		other := second.Session(pair.sessID)
		return first != nil && other != nil &&
			first.Busy() == driver.BusyIdle && other.Busy() == driver.BusyIdle
	})
	if got := lastAgentText(second.Session(pair.sessID)); got != "ABC" {
		t.Fatalf("中途接上的展示端文本应正好是 ABC，实际 %q", got)
	}
	if got := lastAgentText(pair.remote.Session(pair.sessID)); got != "ABC" {
		t.Fatalf("原展示端文本应正好是 ABC，实际 %q", got)
	}
}

// P2: full sync of an already-restored session must not poison the cursor with
// the journal file's seq; otherwise later resume silently drops offline events.
func TestViewerResumeAfterRestoredSession(t *testing.T) {
	dataDir := t.TempDir()

	// first executor: create a session with a full round (journal gets many events)
	rt := newP2Runtime()
	p := &poster{}
	makeLocal := func() *host.Local {
		return host.NewLocal(host.Config{
			Runtime: rt, Selection: providers.Selection{ProviderID: "fake", Model: "m"},
			SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
			ContextWindow: 200_000, Post: p.Post, Catalog: store.NewCatalog(dataDir),
		})
	}
	first := makeLocal()
	sessID := first.Active()
	if sessID == nil {
		first.NewSession("")
		sessID = first.Active()
	}
	payload, _ := host.EncodePayload(host.SendPayload{Text: "第一轮"})
	if _, err := first.Submit(host.Command{SessionID: sessID.ID, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	live := rt.await(t, 1)
	live <- providers.Event{Kind: providers.EventText, Text: "回答一"}
	live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && sessID.Busy() != driver.BusyIdle {
		p.pump()
		time.Sleep(2 * time.Millisecond)
	}
	_ = first.Close()

	// second executor: restores the session from disk (eventSeq starts from 0)
	second := makeLocal()
	t.Cleanup(func() { _ = second.Close() })
	restored := second.Sessions()
	if len(restored) != 1 {
		t.Fatalf("应从磁盘恢复 1 个会话：%v", restored)
	}
	id := restored[0].ID

	viewer, executor := wire.PipePair()
	go func() { _ = host.Serve(executor, second, p.Post) }()
	remote, err := host.NewRemote(viewer, p.Post)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		if session := remote.Session(id); session != nil && strings.Contains(lastAgentText(session), "回答一") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !remote.LastAttachTruncated(id) {
		t.Fatalf("首次同步应是全量")
	}

	// disconnect, run another round offline, reconnect: must resume from the ring
	_ = viewer.Close()
	p.pump()
	time.Sleep(10 * time.Millisecond)
	payload2, _ := host.EncodePayload(host.SendPayload{Text: "断线期间"})
	if _, err := second.Submit(host.Command{SessionID: id, Kind: "send", Payload: payload2}); err != nil {
		t.Fatal(err)
	}
	offline := rt.await(t, 2)
	offline <- providers.Event{Kind: providers.EventText, Text: "离线回答"}
	offline <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && second.Session(id).Busy() != driver.BusyIdle {
		p.pump()
		time.Sleep(2 * time.Millisecond)
	}

	viewer2, executor2 := wire.PipePair()
	go func() { _ = host.Serve(executor2, second, p.Post) }()
	if err := remote.Reconnect(viewer2); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		if remote.AttachCountForTest(id) >= 2 && strings.Contains(lastAgentText(remote.Session(id)), "离线回答") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if remote.LastAttachTruncated(id) {
		t.Fatalf("游标应落在 eventSeq 上，重连应走增量而不是全量")
	}
	if got, want := digestOf(t, remote.Session(id)), digestOf(t, second.Session(id)); got != want {
		t.Fatalf("重连后两端不一致：remote=%s local=%s", got, want)
	}
}

// P2: executor restart changes epoch -> viewer must fall back to a full sync.
func TestViewerReconnectAfterExecutorRestart(t *testing.T) {
	dataDir := t.TempDir()
	rt := newP2Runtime()
	p := &poster{}
	makeLocal := func() *host.Local {
		return host.NewLocal(host.Config{
			Runtime: rt, Selection: providers.Selection{ProviderID: "fake", Model: "m"},
			SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
			ContextWindow: 200_000, Post: p.Post, Catalog: store.NewCatalog(dataDir),
		})
	}
	first := makeLocal()
	session := first.Active()
	if session == nil {
		first.NewSession("")
		session = first.Active()
	}
	payload, _ := host.EncodePayload(host.SendPayload{Text: "第一轮"})
	if _, err := first.Submit(host.Command{SessionID: session.ID, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	live := rt.await(t, 1)
	live <- providers.Event{Kind: providers.EventText, Text: "回答一"}
	live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && session.Busy() != driver.BusyIdle {
		p.pump()
		time.Sleep(2 * time.Millisecond)
	}

	// connect once (so the viewer has a cursor with the first epoch)
	viewer, executor := wire.PipePair()
	go func() { _ = host.Serve(executor, first, p.Post) }()
	remote, err := host.NewRemote(viewer, p.Post)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		if session := remote.Session(session.ID); session != nil && strings.Contains(lastAgentText(session), "回答一") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	oldEpoch := remote.EpochForTest()
	_ = viewer.Close()
	_ = first.Close()
	p.pump()
	time.Sleep(10 * time.Millisecond)

	// restart the executor on the same data dir: new epoch, eventSeq starts over
	second := makeLocal()
	t.Cleanup(func() { _ = second.Close() })
	if second.Epoch() == oldEpoch {
		t.Fatalf("重启后 epoch 应该变化")
	}
	viewer2, executor2 := wire.PipePair()
	go func() { _ = host.Serve(executor2, second, p.Post) }()
	if err := remote.Reconnect(viewer2); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		if remote.AttachCountForTest(session.ID) >= 2 &&
			strings.Contains(lastAgentText(remote.Session(session.ID)), "回答一") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !remote.LastAttachTruncated(session.ID) {
		t.Fatalf("执行端重启后必须走全量（epoch 不同）")
	}
	if got, want := digestOf(t, remote.Session(session.ID)), digestOf(t, second.Session(session.ID)); got != want {
		t.Fatalf("重启后两端不一致：remote=%s local=%s", got, want)
	}
}

// P2: a memory-only executor must tell the viewer that older history cannot sync.
func TestViewerSeesNoPersistenceNotice(t *testing.T) {
	pair := newP2Pair(t, false)
	pair.pumpUntil(t, func() bool {
		remote := pair.remote.Session(pair.sessID)
		if remote == nil {
			return false
		}
		for _, entry := range remote.Conversation().Entries() {
			if entry.Notice != nil && strings.Contains(entry.Notice.Text, "未启用持久化") {
				return true
			}
		}
		return false
	})
}

// P1: a viewer that never reads must not block the executor's main thread,
// must not stall other viewers, and must be disconnected on queue overflow.
func TestStalledViewerDoesNotBlockExecutor(t *testing.T) {
	pair := newP2Pair(t, false)

	// a "stalled" viewer: connected, but its receive side is never read.
	// 只给这条连接很小的发送队列（健康展示端保持默认 2000）。
	stalledViewer, stalledExecutor := wire.PipePair()
	stalledDone := make(chan struct{})
	go func() {
		_ = host.ServeLimit(stalledExecutor, pair.local, pair.post.Post, 8)
		close(stalledDone)
	}()
	pair.post.pump()

	// the healthy viewer must keep receiving
	pair.send(t, "卡住的展示端不该拖住我")
	live := pair.rt.await(t, 1)
	text := strings.Repeat("内容", 400) // enough packets to overflow the stalled queue
	for i := 0; i < 40; i++ {
		live <- providers.Event{Kind: providers.EventText, Text: text}
	}
	live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}

	pair.pumpUntil(t, func() bool {
		remote := pair.remote.Session(pair.sessID)
		return remote != nil && remote.Busy() == driver.BusyIdle &&
			strings.Contains(lastAgentText(remote), "内容")
	})

	// the stalled connection is closed by the executor (queue overflow)
	select {
	case <-stalledDone:
	case <-time.After(5 * time.Second):
		t.Fatal("卡住的展示端应被断开")
	}
	_ = stalledViewer
}

// P3: the viewer reconnects automatically after the executor goes away and
// comes back (cursors preserved: offline events are caught up).
func TestViewerAutoReconnect(t *testing.T) {
	local := host.NewLocal(host.Config{
		Runtime: newP2Runtime(), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: (&poster{}).Post,
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	rt := &p2Runtime{}
	_ = rt

	// dialer hands out a fresh pipe each time, plugged into the same executor
	var mu sync.Mutex
	var pendingViewer wire.Conn
	stop := make(chan struct{})
	dial := func() (wire.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		return pendingViewer, nil
	}

	// first connection
	post := (&poster{})
	_ = post
	firstViewer, firstExecutor := wire.PipePair()
	go func() { _ = host.Serve(firstExecutor, local, func(f func()) { f() }) }()
	pendingViewer = firstViewer
	remote, err := host.NewRemoteWithDialer(dial, func(f func()) { f() })
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	time.Sleep(50 * time.Millisecond)
	if len(remote.Sessions()) != 1 {
		t.Fatalf("应同步到 1 个会话：%v", remote.Sessions())
	}

	// executor "goes away": close the first connection
	_ = firstViewer.Close()
	_ = firstExecutor.Close()
	time.Sleep(100 * time.Millisecond)

	// executor "comes back": new pipe served by the same Local
	secondViewer, secondExecutor := wire.PipePair()
	mu.Lock()
	pendingViewer = secondViewer
	mu.Unlock()
	go func() { _ = host.Serve(secondExecutor, local, func(f func()) { f() }) }()

	// the auto-reconnect loop sleeps 3s before the first retry
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if remote.Connected() && remote.AttachCountForTest(remote.Sessions()[0].ID) >= 2 {
			close(stop)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("展示端没有自动重连")
}

// P1: while disconnected, Submit fails immediately and the command is NOT
// replayed after reconnect (the user may no longer want it).
func TestSubmitFailsWhileDisconnected(t *testing.T) {
	pair := newP2Pair(t, true)
	pair.pumpUntil(t, func() bool { return pair.remote.AttachCountForTest(pair.sessID) >= 1 })
	_ = pair.viewer.Close()
	pair.pumpUntil(t, func() bool { return !pair.remote.Connected() })

	payload, _ := host.EncodePayload(host.SendPayload{Text: "断线时候发的"})
	if _, err := pair.remote.Submit(host.Command{SessionID: pair.sessID, Kind: "send", Payload: payload}); err == nil {
		t.Fatal("断线时 Submit 应立即失败")
	}

	viewer2, executor2 := wire.PipePair()
	go func() { _ = host.Serve(executor2, pair.local, pair.post.Post) }()
	if err := pair.remote.Reconnect(viewer2); err != nil {
		t.Fatal(err)
	}
	pair.pumpUntil(t, func() bool { return pair.remote.Connected() && pair.remote.AttachCountForTest(pair.sessID) >= 2 })

	// 重连后用一条**新**命令证明链路恢复
	payload2, _ := host.EncodePayload(host.SendPayload{Text: "重连之后发的"})
	if _, err := pair.remote.Submit(host.Command{SessionID: pair.sessID, Kind: "send", Payload: payload2}); err != nil {
		t.Fatalf("重连后提交应成功：%v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pair.post.pump()
		if strings.Contains(digestOf(t, pair.local.Active()), "重连之后发的") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 断线期间那条绝不能被补发
	for _, entry := range pair.local.Active().Conversation().Entries() {
		if entry.Message == nil || entry.Message.Message == nil {
			continue
		}
		for _, part := range entry.Message.Message.Parts {
			if text, ok := part.AsText(); ok && strings.Contains(text, "断线时候发的") {
				t.Fatalf("断线期间的命令不该在重连后被补发")
			}
		}
	}
}

// P1: rapid disconnect/reconnect cycles interleaved with submits and executor
// events must converge (no lost or duplicated state).
func TestRapidReconnectCycles(t *testing.T) {
	pair := newP2Pair(t, true)
	pair.pumpUntil(t, func() bool { return pair.remote.AttachCountForTest(pair.sessID) >= 1 })
	for cycle := 0; cycle < 10; cycle++ {
		_ = pair.viewer.Close()
		// 等管理器发现断线（Connected 变 false）再断言 Submit
		pair.pumpUntil(t, func() bool { return !pair.remote.Connected() })

		// offline: a submit must fail, and the executor keeps producing events
		payload, _ := host.EncodePayload(host.SendPayload{Text: "断线期"})
		if _, err := pair.remote.Submit(host.Command{SessionID: pair.sessID, Kind: "send", Payload: payload}); err == nil {
			t.Fatalf("第 %d 轮：断线时 Submit 应失败", cycle)
		}
		build, _ := host.EncodePayload(host.SendPayload{Text: "执行端事件"})
		if _, err := pair.local.Submit(host.Command{SessionID: pair.sessID, Kind: "send", Payload: build}); err != nil {
			t.Fatal(err)
		}
		live := pair.rt.await(t, cycle+1)
		live <- providers.Event{Kind: providers.EventText, Text: "回答"}
		live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && pair.local.Active().Busy() != driver.BusyIdle {
			pair.post.pump()
			time.Sleep(2 * time.Millisecond)
		}

		// reconnect with a fresh pipe
		nextViewer, nextExecutor := wire.PipePair()
		go func() { _ = host.Serve(nextExecutor, pair.local, pair.post.Post) }()
		if err := pair.remote.Reconnect(nextViewer); err != nil {
			t.Fatal(err)
		}
		pair.viewer = nextViewer
		pair.pumpUntil(t, func() bool {
			return pair.remote.Connected() && pair.remote.AttachCountForTest(pair.sessID) >= cycle+2
		})
	}
	pair.pumpUntil(t, func() bool {
		if got, want := digestOf(t, pair.remote.Session(pair.sessID)), digestOf(t, pair.local.Active()); got != want {
			return false
		}
		return true
	})
}

// fakeConn 是测试用的可控连接：Send 可阻塞、Recv 可关闭。
type fakeConn struct {
	mu       sync.Mutex
	sent     []wire.Packet
	sendGate chan struct{} // 非 nil 时 Send 会阻塞直到它关闭
	closed   chan struct{}
	once     sync.Once
}

func newFakeConn(gate bool) *fakeConn {
	conn := &fakeConn{closed: make(chan struct{})}
	if gate {
		conn.sendGate = make(chan struct{})
	}
	return conn
}

func (c *fakeConn) Send(packet wire.Packet) error {
	if c.sendGate != nil {
		<-c.sendGate
	}
	select {
	case <-c.closed:
		return wire.ErrClosed
	default:
	}
	c.mu.Lock()
	c.sent = append(c.sent, packet)
	c.mu.Unlock()
	return nil
}

func (c *fakeConn) Recv() (wire.Packet, error) {
	<-c.closed
	return wire.Packet{}, wire.ErrClosed
}

func (c *fakeConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// (a) 已经收到结果的命令，断线时不该再收到失败回执。
func TestNoFailureForCompletedCommandsOnDisconnect(t *testing.T) {
	pair := newP2Pair(t, true)
	results := make(chan host.CmdResult, 16)
	pair.remote.Subscribe(func(change host.Change) {
		if change.Kind == "cmdResult" {
			results <- change.Result
		}
	})
	ids := map[string]bool{}
	for i := 0; i < 3; i++ {
		payload, _ := host.EncodePayload(host.TextPayload{Text: "改名" + fmt.Sprint(i)})
		id, err := pair.remote.Submit(host.Command{SessionID: pair.sessID, Kind: "rename", Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		ids[id] = true
	}
	got := 0
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && got < 3 {
		pair.post.pump()
		select {
		case result := <-results:
			if _, ok := ids[result.CmdID]; ok {
				got++
			}
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	if got != 3 {
		t.Fatalf("应收到 3 条结果，实际 %d", got)
	}

	_ = pair.viewer.Close()
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		pair.post.pump()
		select {
		case result := <-results:
			if ids[result.CmdID] {
				t.Fatalf("已完成的命令不该再收到失败回执：%+v", result)
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// (b) 已写出但没收到结果：回 Unknown，界面不还原草稿。
func TestUnknownForSentCommandOnDisconnect(t *testing.T) {
	conn := newFakeConn(false)
	remote, err := host.NewRemote(conn, func(f func()) { f() })
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	results := make(chan host.CmdResult, 8)
	remote.Subscribe(func(change host.Change) {
		if change.Kind == "cmdResult" {
			results <- change.Result
		}
	})

	payload, _ := host.EncodePayload(host.TextPayload{Text: "x"})
	if _, err := remote.Submit(host.Command{SessionID: "s", Kind: "rename", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	// 等写 goroutine 把它写出去（fakeConn 记录），再断线
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn.mu.Lock()
		count := len(conn.sent)
		conn.mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	// 等管理器把"已写出"回执吃掉（写 goroutine 与读 goroutine 之间存在调度差）
	time.Sleep(50 * time.Millisecond)
	_ = conn.Close()

	select {
	case result := <-results:
		if !result.Unknown {
			t.Fatalf("已写出的命令应回 Unknown：%+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("没有收到 Unknown 回执")
	}
}

// (c) 还在队列里（没写出去）：回"未送达"（界面据此还原草稿）。
func TestNotSentForQueuedCommandOnDisconnect(t *testing.T) {
	conn := newFakeConn(true) // Send 阻塞：命令写不出去
	remote, err := host.NewRemote(conn, func(f func()) { f() })
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	results := make(chan host.CmdResult, 8)
	remote.Subscribe(func(change host.Change) {
		if change.Kind == "cmdResult" {
			results <- change.Result
		}
	})
	payload, _ := host.EncodePayload(host.TextPayload{Text: "x"})
	if _, err := remote.Submit(host.Command{SessionID: "s", Kind: "rename", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	select {
	case result := <-results:
		if result.Unknown || !strings.Contains(result.Error, "未送达") {
			t.Fatalf("未写出的命令应回「未送达」：%+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("没有收到未送达回执")
	}
}

// scriptedConn 允许测试在"发送之后"注入一条回来的 cmd.result，然后再制造断线。
type scriptedConn struct {
	recv chan wire.Packet
	done chan struct{}
	once sync.Once
}

func newScriptedConn() *scriptedConn {
	return &scriptedConn{recv: make(chan wire.Packet, 8), done: make(chan struct{})}
}

func (c *scriptedConn) Send(packet wire.Packet) error { return nil }

func (c *scriptedConn) Recv() (wire.Packet, error) {
	select {
	case packet := <-c.recv:
		return packet, nil
	case <-c.done:
		return wire.Packet{}, wire.ErrClosed
	}
}

func (c *scriptedConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

// P2: a cmd.result already received in the inbox must not be dropped when the
// connection fails right after it (it would turn OK into "unknown").
func TestResultInInboxNotDroppedOnDisconnect(t *testing.T) {
	conn := newScriptedConn()
	remote, err := host.NewRemote(conn, func(f func()) { f() })
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	results := make(chan host.CmdResult, 4)
	remote.Subscribe(func(change host.Change) {
		if change.Kind == "cmdResult" {
			results <- change.Result
		}
	})

	payload, _ := host.EncodePayload(host.TextPayload{Text: "x"})
	cmdID, err := remote.Submit(host.Command{SessionID: "s", Kind: "rename", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	// 执行端的 OK 结果先到，紧接着连接断掉
	body, _ := wire.Encode(host.CmdResult{CmdID: cmdID, OK: true})
	conn.recv <- wire.Packet{Kind: "cmd.result", Body: body}
	time.Sleep(20 * time.Millisecond)
	_ = conn.Close()

	select {
	case result := <-results:
		if !result.OK {
			t.Fatalf("已回结果的命令不该变成失败/未知：%+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("没有收到结果")
	}
}

// P3: an image pasted on the viewer reaches the executor's blob store, and the
// viewer can pull it back by hash (bidirectional).
func TestViewerPasteImageReachesExecutor(t *testing.T) {
	blobs, err := store.NewBlobs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt := newP2Runtime()
	p := &poster{}
	local := host.NewLocal(host.Config{
		Runtime: rt, Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: p.Post, Blobs: blobs,
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	viewer, executor := wire.PipePair()
	go func() { _ = host.Serve(executor, local, p.Post) }()
	remote, err := host.NewRemote(viewer, p.Post)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	image := bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 4096) // 16KB，多分块
	type putResult struct {
		hash string
		err  error
	}
	putCh := make(chan putResult, 1)
	go func() {
		hash, putErr := remote.Blobs().Put(image)
		putCh <- putResult{hash: hash, err: putErr}
	}()
	var hash string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		select {
		case result := <-putCh:
			if result.err != nil {
				t.Fatalf("上传失败：%v", result.err)
			}
			hash = result.hash
		default:
		}
		if hash != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if hash == "" {
		t.Fatal("上传没有返回")
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		if data, openErr := blobs.Open(hash); openErr == nil {
			if !bytes.Equal(data, image) {
				t.Fatalf("执行端图片内容不对：%d 字节", len(data))
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("执行端没有收到图片：hash=%s", hash)
}

// P3: an image produced on the executor is fetched on demand by the viewer.
func TestViewerFetchesExecutorImage(t *testing.T) {
	blobs, err := store.NewBlobs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	image := []byte("executor-side-image")
	hash, err := blobs.Put(image)
	if err != nil {
		t.Fatal(err)
	}
	rt := newP2Runtime()
	p := &poster{}
	local := host.NewLocal(host.Config{
		Runtime: rt, Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: p.Post, Blobs: blobs,
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	viewer, executor := wire.PipePair()
	go func() { _ = host.Serve(executor, local, p.Post) }()
	remote, err := host.NewRemote(viewer, p.Post)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	got := make(chan []byte, 1)
	go func() {
		data, openErr := remote.Blobs().Open(hash)
		if openErr == nil {
			got <- data
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		select {
		case data := <-got:
			if !bytes.Equal(data, image) {
				t.Fatalf("拉回来的图片不对：%q", data)
			}
			return
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("展示端没有拉到执行端的图片")
}

// failingBlobs 让执行端入库失败（磁盘满/校验不过），用于验证 Put 会把失败报回去。
type failingBlobs struct{}

func (failingBlobs) Put([]byte) (string, error)  { return "", errors.New("磁盘已满") }
func (failingBlobs) Open(string) ([]byte, error) { return nil, errors.New("缺") }
func (failingBlobs) Has(string) bool             { return false }

// P1-1: 执行端入库失败时 Put 必须报错（不能"入队即成功"）。
func TestPutFailsWhenExecutorCommitFails(t *testing.T) {
	rt := newP2Runtime()
	p := &poster{}
	local := host.NewLocal(host.Config{
		Runtime: rt, Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: p.Post,
	})
	t.Cleanup(func() { _ = local.Close() })
	local.SetBlobs(failingBlobs{})
	if local.Active() == nil {
		local.NewSession("")
	}
	viewer, executor := wire.PipePair()
	go func() { _ = host.Serve(executor, local, p.Post) }()
	remote, err := host.NewRemote(viewer, p.Post)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	done := make(chan error, 1)
	go func() {
		_, putErr := remote.Blobs().Put([]byte("x"))
		done <- putErr
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		select {
		case putErr := <-done:
			if putErr == nil {
				t.Fatal("执行端入库失败，Put 却成功了")
			}
			return
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Put 没有返回")
}

// P1-2: 断线时挂着的 Open 要被唤醒报错，而不是永远等下去。
func TestOpenFailsOnDisconnect(t *testing.T) {
	conn := newScriptedConn()
	remote, err := host.NewRemote(conn, func(f func()) { f() })
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	done := make(chan error, 1)
	go func() {
		_, openErr := remote.Blobs().Open("missing-hash")
		done <- openErr
	}()
	time.Sleep(30 * time.Millisecond)
	_ = conn.Close()

	select {
	case openErr := <-done:
		if openErr == nil {
			t.Fatal("断线后 Open 应该报错")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("断线后 Open 没有被唤醒")
	}
}

// P1-2: after reconnect the same blob can be fetched again (failure is not final).
func TestOpenWorksAfterReconnect(t *testing.T) {
	pair := newP2Pair(t, true)
	pair.pumpUntil(t, func() bool { return pair.remote.AttachCountForTest(pair.sessID) >= 1 })

	blobs, err := store.NewBlobs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	image := []byte("after-reconnect")
	hash, err := blobs.Put(image)
	if err != nil {
		t.Fatal(err)
	}
	pair.local.SetBlobs(host.LocalBlobs{Store: blobs})

	_ = pair.viewer.Close()
	pair.pumpUntil(t, func() bool { return !pair.remote.Connected() })
	if _, openErr := pair.remote.Blobs().Open(hash); openErr == nil {
		t.Fatal("断线时 Open 应该失败")
	}

	viewer2, executor2 := wire.PipePair()
	go func() { _ = host.Serve(executor2, pair.local, pair.post.Post) }()
	if err := pair.remote.Reconnect(viewer2); err != nil {
		t.Fatal(err)
	}
	pair.pumpUntil(t, func() bool { return pair.remote.Connected() })

	got := make(chan []byte, 1)
	go func() {
		data, openErr := pair.remote.Blobs().Open(hash)
		if openErr == nil {
			got <- data
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pair.post.pump()
		select {
		case data := <-got:
			if !bytes.Equal(data, image) {
				t.Fatalf("重连后拉到的图片不对：%q", data)
			}
			return
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("重连后仍然拉不到图片")
}

// P3: 归档分页结果跨进程回来后仍是 host.OlderPage（Data 是 any，走 JSON 会变成 map）。
func TestLoadOlderResultStaysTypedOverWire(t *testing.T) {
	pair := newP2Pair(t, true)
	pair.pumpUntil(t, func() bool { return pair.remote.AttachCountForTest(pair.sessID) >= 1 })

	results := make(chan host.CmdResult, 4)
	pair.remote.Subscribe(func(change host.Change) {
		if change.Kind == "cmdResult" {
			results <- change.Result
		}
	})
	cmdID, err := pair.remote.Submit(host.Command{SessionID: pair.sessID, Kind: "loadOlder"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pair.post.pump()
		select {
		case result := <-results:
			if result.CmdID != cmdID || !result.OK {
				t.Fatalf("结果不对：%+v", result)
			}
			if _, ok := result.Data.(host.OlderPage); !ok {
				t.Fatalf("归档页没有还原成 host.OlderPage：%T", result.Data)
			}
			return
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("没有收到归档页结果")
}

// attachSecondViewer 建第二个展示端并同步到同一会话（P3 多端验收公用）。
func attachSecondViewer(t *testing.T, local *host.Local, post *poster, sessID string) *host.Remote {
	t.Helper()
	viewer, executor := wire.PipePair()
	go func() { _ = host.Serve(executor, local, post.Post) }()
	remote, err := host.NewRemote(viewer, post.Post)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { remote.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		post.pump()
		if session := remote.Session(sessID); session != nil && session.Busy() != driver.BusyIdle {
			return remote
		}
		time.Sleep(5 * time.Millisecond)
	}
	post.pump()
	return remote
}

// §10 P3: 两个展示端先后发消息，第一轮还在跑，第二个进插话队列；两端都看到队列。
func TestTwoViewersSteerInOrder(t *testing.T) {
	pair := newP2Pair(t, true)
	pair.pumpUntil(t, func() bool { return pair.remote.AttachCountForTest(pair.sessID) >= 1 })
	second := attachSecondViewer(t, pair.local, pair.post, pair.sessID)

	firstPayload, _ := host.EncodePayload(host.SendPayload{Text: "第一条"})
	if _, err := pair.remote.Submit(host.Command{SessionID: pair.sessID, Kind: "send", Payload: firstPayload}); err != nil {
		t.Fatal(err)
	}
	var stream chan providers.Event
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pair.post.pump()
		if pair.rt.count() >= 1 {
			stream = pair.rt.stream(1)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if stream == nil {
		t.Fatal("回合没有开始")
	}
	stream <- providers.Event{Kind: providers.EventText, Text: "在处理"}
	pair.pumpUntil(t, func() bool { return pair.local.Session(pair.sessID).Busy() == driver.BusyStreaming })

	// 忙时界面走"插话"命令（与本地语义一致：send 只在空闲时用）
	steerPayload, _ := host.EncodePayload(host.TextPayload{Text: "插一句"})
	if _, err := second.Submit(host.Command{SessionID: pair.sessID, Kind: "enqueue", Payload: steerPayload}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pair.post.pump()
		queuedA := pair.local.Session(pair.sessID).Queued()
		sessionB := second.Session(pair.sessID)
		if len(queuedA) == 1 && queuedA[0] == "插一句" && sessionB != nil && len(sessionB.Queued()) == 1 {
			// 结束这一轮：插话在安全边界投递，之后才能验证"顺序"本身
			stream <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
			deadline = time.Now().Add(5 * time.Second)
			var users []string
			for time.Now().Before(deadline) {
				pair.post.pump()
				local := pair.local.Session(pair.sessID)
				if len(local.Queued()) != 0 {
					time.Sleep(2 * time.Millisecond)
					continue
				}
				// 投递后：两条 user 消息按到达顺序出现，两端与执行端一致
				users = users[:0]
				for _, item := range local.Conversation().Entries() {
					if item.Message.Message.Role == message.RoleUser {
						users = append(users, item.Message.Message.PreviewText())
					}
				}
				if len(users) >= 2 && users[0] == "第一条" && users[1] == "插一句" {
					// 第二轮流完再比：投递瞬间镜像可能正好差一个增量
					if pair.rt.count() >= 2 {
						secondStream := pair.rt.stream(2)
						secondStream <- providers.Event{Kind: providers.EventText, Text: "收到插话"}
						secondStream <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
					}
					deadline2 := time.Now().Add(5 * time.Second)
					for time.Now().Before(deadline2) && local.Busy() != driver.BusyIdle {
						pair.post.pump()
						time.Sleep(2 * time.Millisecond)
					}
					want := digestOf(t, local)
					if got := digestOf(t, pair.remote.Session(pair.sessID)); got != want {
						t.Fatalf("插话投递后 A 端不一致")
					}
					if sessionB := second.Session(pair.sessID); sessionB != nil {
						if got := digestOf(t, sessionB); got != want {
							t.Fatalf("插话投递后 B 端不一致")
						}
					}
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
			t.Fatalf("插话没有按顺序投递：users=%v entries=%d busy=%v queued=%v",
				users, len(pair.local.Session(pair.sessID).Conversation().Entries()),
				pair.local.Session(pair.sessID).Busy(), pair.local.Session(pair.sessID).Queued())
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("插话队列不一致：执行端=%v", pair.local.Session(pair.sessID).Queued())
}

// askRuntime 让模型"调用提问工具"，用于多端抢答的验收。
type askRuntime struct{ *p2Runtime }

func (r *askRuntime) RunTools(ctx context.Context, tc *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
	out := make([]tools.ToolOutcome, 0, len(calls))
	for _, call := range calls {
		out = append(out, tools.ToolOutcome{
			ID: call.ID, Name: "ask_user_question",
			Pending: []tools.QuestionSpec{{ID: "q1", Question: "继续吗？",
				Options: []tools.QuestionOption{{Label: "继续"}, {Label: "停"}}}},
		})
	}
	return out
}

// §10 P3: A、B 同时回答同一提问，B 收到「已被其他端回答」。
func TestAskAnsweredElsewhere(t *testing.T) {
	rt := &askRuntime{newP2Runtime()}
	p := &poster{}
	local := host.NewLocal(host.Config{
		Runtime: rt, Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: p.Post,
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	sessID := local.Active().ID

	viewerA, execA := wire.PipePair()
	go func() { _ = host.Serve(execA, local, p.Post) }()
	remoteA, err := host.NewRemote(viewerA, p.Post)
	if err != nil {
		t.Fatal(err)
	}
	defer remoteA.Close()
	viewerB, execB := wire.PipePair()
	go func() { _ = host.Serve(execB, local, p.Post) }()
	remoteB, err := host.NewRemote(viewerB, p.Post)
	if err != nil {
		t.Fatal(err)
	}
	defer remoteB.Close()

	payload, _ := host.EncodePayload(host.SendPayload{Text: "问我"})
	if _, err := remoteA.Submit(host.Command{SessionID: sessID, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	var live chan providers.Event
	for i := 0; i < 200 && live == nil; i++ {
		p.pump()
		if rt.count() >= 1 {
			live = rt.stream(1)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if live == nil {
		t.Fatal("回合没有开始")
	}
	live <- providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "ask_user_question"}
	live <- providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"questions":[]}`}
	live <- providers.Event{Kind: providers.EventToolEnd, Index: 0}
	live <- providers.Event{Kind: providers.EventDone, StopReason: "tool_use"}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		if local.Session(sessID).Busy() == driver.BusyAwaitingAnswer {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if local.Session(sessID).Busy() != driver.BusyAwaitingAnswer {
		t.Fatalf("没有进入等待回答：%v", local.Session(sessID).Busy())
	}

	results := make(chan host.CmdResult, 4)
	remoteB.Subscribe(func(change host.Change) {
		if change.Kind == "cmdResult" {
			results <- change.Result
		}
	})
	answer, _ := host.EncodePayload(host.AnswerPayload{Custom: "继续"})
	aid, err := remoteA.Submit(host.Command{SessionID: sessID, Kind: "answer", Payload: answer})
	if err != nil {
		t.Fatal(err)
	}
	// 先等 A 的回答真正落地，再让 B 回答（"同时"在协议里只会有一个赢家，
	// 但测试要的是可复现：后到的那个必须收到提示）
	for time.Now().Before(deadline) {
		p.pump()
		settled := false
		select {
		case result := <-results:
			if result.CmdID == aid {
				settled = result.OK
			}
		default:
		}
		if settled {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	bid, err := remoteB.Submit(host.Command{SessionID: sessID, Kind: "answer", Payload: answer})
	if err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline) {
		p.pump()
		select {
		case result := <-results:
			if result.CmdID != bid {
				continue // A 的回答结果（OK）先到，正常
			}
			if result.Notice == "" {
				t.Fatalf("后到的回答应收到提示，实际 %+v", result)
			}
			return
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("B 没有收到「已被其他端回答」")
}

// §10 P3: A 端停止，B 端（以及 A 自己）都回到 idle。
func TestStopFromViewerStopsAll(t *testing.T) {
	pair := newP2Pair(t, true)
	pair.pumpUntil(t, func() bool { return pair.remote.AttachCountForTest(pair.sessID) >= 1 })
	second := attachSecondViewer(t, pair.local, pair.post, pair.sessID)

	payload, _ := host.EncodePayload(host.SendPayload{Text: "跑起来"})
	if _, err := pair.remote.Submit(host.Command{SessionID: pair.sessID, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	var live chan providers.Event
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && live == nil {
		pair.post.pump()
		if pair.rt.count() >= 1 {
			live = pair.rt.stream(1)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if live == nil {
		t.Fatal("回合没有开始")
	}
	live <- providers.Event{Kind: providers.EventText, Text: "输出中"}
	pair.pumpUntil(t, func() bool { return pair.local.Session(pair.sessID).Busy() != driver.BusyIdle })

	// 第二个展示端按停止
	if _, err := second.Submit(host.Command{SessionID: pair.sessID, Kind: "stop"}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pair.post.pump()
		idleA := pair.local.Session(pair.sessID).Busy() == driver.BusyIdle
		sessionB := second.Session(pair.sessID)
		idleB := sessionB != nil && sessionB.Busy() == driver.BusyIdle
		self := pair.remote.Session(pair.sessID)
		idleSelf := self != nil && self.Busy() == driver.BusyIdle
		if idleA && idleB && idleSelf {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("停止没有传到所有端：A=%v", pair.local.Session(pair.sessID).Busy())
}

// §10 P3: 归档按需分页——执行端压缩两次产生两份归档，两个展示端各自翻到底，
// 序列与执行端本地 LoadOlder 完全一致，且不会重复出现 kept 部分的消息。
func TestViewerOlderArchive(t *testing.T) {
	pair := newP2Pair(t, true)
	pair.pumpUntil(t, func() bool { return pair.remote.AttachCountForTest(pair.sessID) >= 1 })
	second := attachSecondViewer(t, pair.local, pair.post, pair.sessID)

	streamIndex := 0
	runRound := func(text, answer string) {
		t.Helper()
		streamIndex++
		pair.send(t, text)
		live := streamAt(t, pair.rt, pair.post, streamIndex)
		live <- providers.Event{Kind: providers.EventText, Text: answer}
		live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
		pair.pumpUntil(t, func() bool {
			session := pair.local.Session(pair.sessID)
			return session != nil && session.Busy() == driver.BusyIdle
		})
	}
	compact := func() {
		t.Helper()
		streamIndex++
		if _, err := pair.local.Submit(host.Command{SessionID: pair.sessID, Kind: "compact"}); err != nil {
			t.Fatal(err)
		}
		summary := streamAt(t, pair.rt, pair.post, streamIndex)
		summary <- providers.Event{Kind: providers.EventText, Text: "摘要"}
		// 摘要流靠**关闭通道**结束（driver 在 range 结束后才 ApplyCompact）
		close(summary)
		pair.pumpUntil(t, func() bool {
			session := pair.local.Session(pair.sessID)
			return session != nil && session.Busy() == driver.BusyIdle
		})
	}

	// 每次压缩至少要有一轮"更早的"问答可折叠：两轮一压，共压两次 = 两份归档
	runRound("第一轮问题", "第一轮回答")
	runRound("第二轮问题", "第二轮回答")
	compact()
	runRound("第三轮问题", "第三轮回答")
	runRound("第四轮问题", "第四轮回答")
	compact()

	flip := func(remote *host.Remote) []string {
		t.Helper()
		var seen []string
		cursor := ""
		for page := 0; page < 10; page++ {
			payload, _ := host.EncodePayload(host.LoadOlderPayload{Cursor: cursor})
			results := make(chan host.CmdResult, 2)
			cancel := remote.Subscribe(func(change host.Change) {
				if change.Kind == "cmdResult" {
					results <- change.Result
				}
			})
			cmdID, err := remote.Submit(host.Command{SessionID: pair.sessID, Kind: "loadOlder", Payload: payload})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			var page host.OlderPage
			deadline := time.Now().Add(5 * time.Second)
			got := false
			for time.Now().Before(deadline) && !got {
				pair.post.pump()
				select {
				case result := <-results:
					if result.CmdID != cmdID {
						continue
					}
					if result.Error != "" {
						cancel()
						t.Fatalf("加载归档失败：%s", result.Error)
					}
					typed, ok := result.Data.(host.OlderPage)
					if !ok {
						cancel()
						t.Fatalf("归档页类型不对：%T", result.Data)
					}
					page = typed
					got = true
				default:
				}
				time.Sleep(2 * time.Millisecond)
			}
			cancel()
			if !got {
				t.Fatal("没有收到归档页")
			}
			if len(page.Entries) == 0 {
				t.Fatal("归档页是空的（压缩没有产生归档？）")
			}
			for _, entry := range page.Entries {
				seen = append(seen, entry.Message.MessageID+"/"+string(entry.Message.Role)+"/"+entry.Message.PreviewText())
			}
			if !page.More {
				return seen
			}
			cursor = page.Next
		}
		t.Fatal("归档页数超过预期，可能没有前进")
		return nil
	}

	fromA := flip(pair.remote)
	fromB := flip(second)
	if len(fromA) == 0 {
		t.Fatal("没有读到归档条目")
	}
	if strings.Join(fromA, "|") != strings.Join(fromB, "|") {
		t.Fatalf("两个展示端归档不一致：A=%v B=%v", fromA, fromB)
	}

	// 与执行端本地直接翻页的结果一致
	var local []string
	cursor := ""
	for page := 0; page < 10; page++ {
		got, err := pair.local.Session(pair.sessID).LoadOlderForTest(cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range got.Entries {
			local = append(local, entry.Message.MessageID+"/"+string(entry.Message.Role)+"/"+entry.Message.PreviewText())
		}
		if !got.More {
			break
		}
		cursor = got.Next
	}
	if strings.Join(fromA, "|") != strings.Join(local, "|") {
		t.Fatalf("展示端与执行端归档不一致：远端=%v 本地=%v", fromA, local)
	}

	// D2：已经保留在当前会话里的消息不能再出现在归档里
	current := map[string]bool{}
	for _, item := range pair.local.Session(pair.sessID).Conversation().Entries() {
		current[item.Message.Message.MessageID] = true
	}
	for _, key := range fromA {
		id := strings.SplitN(key, "/", 2)[0]
		if current[id] {
			t.Fatalf("归档里出现了当前会话已有的消息：%s", id)
		}
	}
}

// §10 P3: 忙时带附件的发送被拒，队列里不会出现附件（与本地语义一致）。
func TestViewerSendWithAttachmentWhileBusyRejected(t *testing.T) {
	pair := newP2Pair(t, true)
	pair.pumpUntil(t, func() bool { return pair.remote.AttachCountForTest(pair.sessID) >= 1 })

	payload, _ := host.EncodePayload(host.SendPayload{Text: "第一轮"})
	if _, err := pair.remote.Submit(host.Command{SessionID: pair.sessID, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	var live chan providers.Event
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && live == nil {
		pair.post.pump()
		if pair.rt.count() >= 1 {
			live = pair.rt.stream(1)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if live == nil {
		t.Fatal("回合没有开始")
	}
	live <- providers.Event{Kind: providers.EventText, Text: "忙"}
	pair.pumpUntil(t, func() bool { return pair.local.Session(pair.sessID).Busy() != driver.BusyIdle })

	results := make(chan host.CmdResult, 4)
	pair.remote.Subscribe(func(change host.Change) {
		if change.Kind == "cmdResult" {
			results <- change.Result
		}
	})
	withImage, _ := host.EncodePayload(host.SendPayload{
		Text: "带图插话", Attachments: []host.AttachmentRef{{Hash: "abc", MediaType: "image/png"}},
	})
	cmdID, err := pair.remote.Submit(host.Command{SessionID: pair.sessID, Kind: "send", Payload: withImage})
	if err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline) {
		pair.post.pump()
		select {
		case result := <-results:
			if result.CmdID != cmdID {
				continue
			}
			if result.Error == "" {
				t.Fatal("忙时带附件的发送应被拒绝")
			}
			if queued := pair.local.Session(pair.sessID).Queued(); len(queued) != 0 {
				t.Fatalf("队列里不该有内容：%v", queued)
			}
			return
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("没有收到拒绝结果")
}

// P3 补充：翻页到一半断线——命令失败或结果未知，重连后能从同一个 cursor 继续。
func TestOlderArchiveSurvivesDisconnect(t *testing.T) {
	pair := newP2Pair(t, true)
	pair.pumpUntil(t, func() bool { return pair.remote.AttachCountForTest(pair.sessID) >= 1 })

	streamIndex := 0
	runRound := func(text, answer string) {
		t.Helper()
		streamIndex++
		pair.send(t, text)
		live := streamAt(t, pair.rt, pair.post, streamIndex)
		live <- providers.Event{Kind: providers.EventText, Text: answer}
		live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
		pair.pumpUntil(t, func() bool {
			session := pair.local.Session(pair.sessID)
			return session != nil && session.Busy() == driver.BusyIdle
		})
	}
	compact := func() {
		t.Helper()
		streamIndex++
		if _, err := pair.local.Submit(host.Command{SessionID: pair.sessID, Kind: "compact"}); err != nil {
			t.Fatal(err)
		}
		summary := streamAt(t, pair.rt, pair.post, streamIndex)
		summary <- providers.Event{Kind: providers.EventText, Text: "摘要"}
		close(summary)
		pair.pumpUntil(t, func() bool {
			session := pair.local.Session(pair.sessID)
			return session != nil && session.Busy() == driver.BusyIdle
		})
	}
	runRound("a1", "a2")
	runRound("a3", "a4")
	compact()
	runRound("b1", "b2")
	runRound("b3", "b4")
	compact()

	// 先正常翻一页，拿到 cursor
	load := func(cursor string) (host.OlderPage, error) {
		t.Helper()
		results := make(chan host.CmdResult, 4)
		cancel := pair.remote.Subscribe(func(change host.Change) {
			if change.Kind == "cmdResult" {
				results <- change.Result
			}
		})
		defer cancel()
		payload, _ := host.EncodePayload(host.LoadOlderPayload{Cursor: cursor})
		cmdID, err := pair.remote.Submit(host.Command{SessionID: pair.sessID, Kind: "loadOlder", Payload: payload})
		if err != nil {
			return host.OlderPage{}, err
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			pair.post.pump()
			select {
			case result := <-results:
				if result.CmdID != cmdID {
					continue
				}
				if result.Error != "" {
					return host.OlderPage{}, errors.New(result.Error)
				}
				page, ok := result.Data.(host.OlderPage)
				if !ok {
					return host.OlderPage{}, errors.New("类型不对")
				}
				return page, nil
			default:
			}
			time.Sleep(2 * time.Millisecond)
		}
		return host.OlderPage{}, errors.New("超时")
	}

	first, err := load("")
	if err != nil {
		t.Fatalf("第一页没读到：%v", err)
	}
	if len(first.Entries) == 0 {
		t.Fatal("第一页是空的")
	}

	// 断线期间再翻页：必须立刻失败（不挂住）
	_ = pair.viewer.Close()
	pair.pumpUntil(t, func() bool { return !pair.remote.Connected() })
	if _, err := load(first.Next); err == nil {
		t.Fatal("断线时翻页应失败")
	}

	// 重连后从同一个 cursor 继续，结果与执行端本地一致
	viewer2, executor2 := wire.PipePair()
	go func() { _ = host.Serve(executor2, pair.local, pair.post.Post) }()
	if err := pair.remote.Reconnect(viewer2); err != nil {
		t.Fatal(err)
	}
	pair.pumpUntil(t, func() bool { return pair.remote.Connected() })

	next, err := load(first.Next)
	if err != nil {
		t.Fatalf("重连后翻页失败：%v", err)
	}
	local, err := pair.local.Session(pair.sessID).LoadOlderForTest(first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Entries) == 0 || len(next.Entries) != len(local.Entries) {
		t.Fatalf("重连后的页不一致：远端=%d 本地=%d", len(next.Entries), len(local.Entries))
	}
}

// streamAt 等第 n 条流打开并返回它（测试里流是按顺序产生的，不能用"最新一条"，
// 否则新开的摘要/下一轮流会把事件送给错的流）。
func streamAt(t *testing.T, rt *p2Runtime, post *poster, n int) chan providers.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		post.pump()
		if rt.count() >= n {
			return rt.stream(n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("第 %d 条流没有打开", n)
	return nil
}

// RetryNow：不等退避，立刻发起一次重连（切换执行端/手动重试要用）。
func TestRetryNowReconnectsImmediately(t *testing.T) {
	pair := newP2Pair(t, true)
	pair.pumpUntil(t, func() bool { return pair.remote.AttachCountForTest(pair.sessID) >= 1 })

	_ = pair.viewer.Close()
	pair.pumpUntil(t, func() bool { return !pair.remote.Connected() })

	viewer2, executor2 := wire.PipePair()
	go func() { _ = host.Serve(executor2, pair.local, pair.post.Post) }()
	// 换一条连接，然后要求"立刻重连"（正常情况下要等退避）
	if err := pair.remote.Reconnect(viewer2); err != nil {
		t.Fatal(err)
	}
	pair.pumpUntil(t, func() bool { return pair.remote.Connected() })
}

// 展示端收到 catalog 必须发 "sessions" 变化，否则侧栏永远不会刷新。
func TestCatalogEmitsSessionsChange(t *testing.T) {
	pair := newP2Pair(t, true)
	changes := make(chan string, 16)
	pair.remote.Subscribe(func(change host.Change) {
		if change.Kind == "sessions" {
			changes <- change.Kind
		}
	})
	if _, err := pair.remote.Submit(host.Command{Kind: "newSession"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pair.post.pump()
		select {
		case <-changes:
			if len(pair.remote.Sessions()) == 2 {
				return
			}
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("没有收到 sessions 变化（会话数 %d）", len(pair.remote.Sessions()))
}

// 经 relay 新建会话后，展示端在拿到快照之前也必须按"空闲"处理：
// 否则 Stop 按钮常亮、模型选择被禁用。
func TestNewSessionStartsIdleOnViewer(t *testing.T) {
	pair := newP2Pair(t, true)
	pair.pumpUntil(t, func() bool { return pair.remote.AttachCountForTest(pair.sessID) >= 1 })

	results := make(chan host.CmdResult, 4)
	pair.remote.Subscribe(func(change host.Change) {
		if change.Kind == "cmdResult" {
			results <- change.Result
		}
	})
	if _, err := pair.remote.Submit(host.Command{Kind: "newSession"}); err != nil {
		t.Fatal(err)
	}
	var newID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && newID == "" {
		pair.post.pump()
		select {
		case result := <-results:
			if data, ok := result.Data.(host.NewSessionData); ok {
				newID = data.SessionID
			}
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	if newID == "" {
		t.Fatal("没有拿到新会话 id")
	}
	pair.pumpUntil(t, func() bool { return pair.remote.Session(newID) != nil })
	session := pair.remote.Session(newID)
	if session.Busy() != driver.BusyIdle {
		t.Fatalf("新会话在展示端不是空闲：%v", session.Busy())
	}
	if !session.CanSend() {
		t.Fatal("新会话在展示端不能发送（模型也切不了）")
	}
}

// 握手必须把执行端的 allow_shell 告诉展示端（展示端不读执行端配置）。
func TestViewerLearnsAllowShellFromHandshake(t *testing.T) {
	pair := newP2PairWith(t, true, func(cfg *host.Config) { cfg.AllowShell = true })
	pair.pumpUntil(t, func() bool { return pair.remote.Connected() })
	if !pair.remote.Info().AllowShell {
		t.Fatal("展示端没有从握手学到 allow_shell=true")
	}

	pair2 := newP2Pair(t, true)
	pair2.pumpUntil(t, func() bool { return pair2.remote.Connected() })
	if pair2.remote.Info().AllowShell {
		t.Fatal("执行端没开命令执行，展示端却显示已开启")
	}
}

// 展示端要能看到提问并作答：选择会回传执行端（执行端据此继续回合）。
func TestViewerAnswersAsk(t *testing.T) {
	rt := &askRuntime{newP2Runtime()}
	post := &poster{}
	local := host.NewLocal(host.Config{
		Runtime: rt, Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: post.Post,
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	sessID := local.Active().ID

	viewer, executor := wire.PipePair()
	go func() { _ = host.Serve(executor, local, post.Post) }()
	remote, err := host.NewRemote(viewer, post.Post)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	pumpUntilPost(t, post, func() bool { return len(remote.Sessions()) == 1 })

	// 发消息并让模型"调用提问工具"
	payload, _ := host.EncodePayload(host.SendPayload{Text: "问我"})
	if _, err := remote.Submit(host.Command{SessionID: sessID, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	live := streamAt(t, rt.p2Runtime, post, 1)
	live <- providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "ask_user_question"}
	live <- providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"questions":[]}`}
	live <- providers.Event{Kind: providers.EventToolEnd, Index: 0}
	live <- providers.Event{Kind: providers.EventDone, StopReason: "tool_use"}
	pumpUntilPost(t, post, func() bool {
		return local.Session(sessID).Busy() == driver.BusyAwaitingAnswer
	})

	// 展示端应当拿到提问组（以前固定返回 nil）
	var groups []*driver.AskGroup
	pumpUntilPost(t, post, func() bool {
		groups = remote.Session(sessID).AskGroups()
		return len(groups) > 0
	})
	if len(groups[0].Questions) == 0 {
		t.Fatal("展示端提问组没有题目")
	}

	// 在展示端选中第一个选项，并把答案回传
	groups[0].Toggle(0, 0)
	answer := host.AskAnswer{CallID: groups[0].CallID, Selected: map[string][]int{}}
	for index, question := range groups[0].Questions {
		for option := range question.Options {
			if groups[0].Selected(index, option) {
				answer.Selected[question.ID] = append(answer.Selected[question.ID], option)
			}
		}
	}
	body, _ := host.EncodePayload(host.AnswerPayload{Answers: []host.AskAnswer{answer}})
	if _, err := remote.Submit(host.Command{SessionID: sessID, Kind: "answer", Payload: body}); err != nil {
		t.Fatal(err)
	}

	// 执行端应用了选择并提交：回合继续（开下一条流）
	next := streamAt(t, rt.p2Runtime, post, 2)
	next <- providers.Event{Kind: providers.EventText, Text: "收到答案"}
	next <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	pumpUntilPost(t, post, func() bool { return local.Session(sessID).Busy() == driver.BusyIdle })
}

// pumpUntilPost 反复 pump 直到条件成立（独立 poster 版本）。
func pumpUntilPost(t *testing.T, p *poster, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		if done() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}

// 展示端必须用**执行端**公布的模型列表：API Key 与模型配置只在执行端，
// 按本机配置显示会让菜单指向执行端根本不存在的模型。
func TestViewerReceivesExecutorModels(t *testing.T) {
	models := []host.ProviderChoice{{
		ProviderID: "gw", Label: "网关", Models: []string{"a", "b"},
		Labels:  map[string]string{"a": "模型 A"},
		Efforts: map[string][]string{"a": {"low", "high"}},
	}}
	pair := newP2PairWith(t, false, func(cfg *host.Config) { cfg.Models = models })
	pair.pumpUntil(t, func() bool { return len(pair.remote.Providers()) == 1 })

	got := pair.remote.Providers()[0]
	if got.ProviderID != "gw" || got.Label != "网关" || len(got.Models) != 2 {
		t.Fatalf("模型列表没有正确同步：%+v", got)
	}
	if got.Labels["a"] != "模型 A" {
		t.Fatalf("模型显示名丢了：%+v", got.Labels)
	}
	if len(got.Efforts["a"]) != 2 {
		t.Fatalf("思考档位丢了：%+v", got.Efforts)
	}
	// 执行端自己也要能给出同一份（桌面本地模式与 headless 共用）
	if local := pair.local.Providers(); len(local) != 1 || local[0].ProviderID != "gw" {
		t.Fatalf("执行端 Providers() 不一致：%+v", local)
	}
}
