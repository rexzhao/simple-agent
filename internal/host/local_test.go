package host_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
)

// fakeRuntime 模拟模型：返回一个可以手动推事件的流。
type fakeRuntime struct {
	mu      sync.Mutex
	streams []chan providers.Event
}

func (r *fakeRuntime) OpenStream(ctx context.Context, sel providers.Selection, req providers.ChatRequest) (<-chan providers.Event, error) {
	events := make(chan providers.Event, 64)
	r.mu.Lock()
	r.streams = append(r.streams, events)
	r.mu.Unlock()
	go func() {
		<-ctx.Done()
	}()
	return events, nil
}

func (r *fakeRuntime) RunTools(ctx context.Context, tc *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
	out := make([]tools.ToolOutcome, 0, len(calls))
	for _, call := range calls {
		out = append(out, tools.ToolOutcome{ID: call.ID, Name: call.Name, Output: "ok"})
	}
	return out
}

func (r *fakeRuntime) last() chan providers.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streams[len(r.streams)-1]
}

func (r *fakeRuntime) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.streams)
}

// poster 模拟主线程：Post 进来的回调先排队，由测试 pump 依次执行。
type poster struct {
	mu    sync.Mutex
	queue []func()
}

func (p *poster) Post(fn func()) {
	p.mu.Lock()
	p.queue = append(p.queue, fn)
	p.mu.Unlock()
}

func (p *poster) pump() {
	for {
		p.mu.Lock()
		queue := p.queue
		p.queue = nil
		p.mu.Unlock()
		if len(queue) == 0 {
			return
		}
		for _, fn := range queue {
			fn()
		}
	}
}

func newHost(t *testing.T) (*host.Local, *fakeRuntime) {
	t.Helper()
	l, rt, _ := newHostWithPoster(t)
	return l, rt
}

func newHostWithPoster(t *testing.T) (*host.Local, *fakeRuntime, *poster) {
	t.Helper()
	rt := &fakeRuntime{}
	p := &poster{}
	l := host.NewLocal(host.Config{
		Runtime:       rt,
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Workspace:     t.TempDir(),
		MaxToolRounds: 3,
		ContextWindow: 200_000,
		Post:          p.Post,
	})
	t.Cleanup(func() { _ = l.Close() })
	return l, rt, p
}

// send 提交一条 send 命令（异步接口，这里等地跑完当前命令）。
func send(t *testing.T, l *host.Local, sessionID, text string) string {
	t.Helper()
	payload, _ := json.Marshal(host.SendPayload{Text: text})
	cmdID, err := l.Submit(host.Command{SessionID: sessionID, Kind: "send", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return cmdID
}

// P1 验收：同一串操作，直接跑 driver 与经 localHost 的结果完全一致。
func TestLocalHostMatchesDirectDriver(t *testing.T) {
	// 1) 直接跑 driver
	conv := conversation.New("ctx")
	drv := driver.New(conv, driver.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Tools:         tools.Builtin(false),
		ContextWindow: 200_000,
	})
	directEffects, err := drv.Send("你好")
	if err != nil {
		t.Fatal(err)
	}
	directStream := firstStartStream(t, directEffects)
	drv.ApplyEvent(directStream.MessageID, providers.Event{Kind: providers.EventText, Text: "你好！"})
	drv.ApplyEvent(directStream.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})

	// 2) 经 localHost（同一串操作）
	l, rt2, post := newHostWithPoster(t)
	session := l.NewSession("")
	send(t, l, session.ID, "你好")
	deadline := time.Now().Add(2 * time.Second)
	for rt2.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if rt2.count() == 0 {
		t.Fatal("应打开一条流")
	}
	live := rt2.last()
	live <- providers.Event{Kind: providers.EventText, Text: "你好！"}
	live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	// 事件由流 goroutine 投递到 Post：这里当作主线程把它们执行掉
	deadline2 := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline2) {
		post.pump()
		if session.Busy() == driver.BusyIdle && len(session.Conversation().Entries()) >= 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	waitIdle(t, l, session.ID)

	// 3) 比较会话内容（消息 id 随机，比较角色/状态/parts）
	want := digest(t, conv)
	got := digest(t, session.Conversation())
	if got != want {
		t.Fatalf("localHost 与直接 driver 不一致：\nhost=%s\ndirect=%s", got, want)
	}
}

func firstStartStream(t *testing.T, effects []driver.Effect) driver.StartStream {
	t.Helper()
	for _, effect := range effects {
		if stream, ok := effect.(driver.StartStream); ok {
			return stream
		}
	}
	t.Fatalf("没有 StartStream：%#v", effects)
	return driver.StartStream{}
}

func waitIdle(t *testing.T, l *host.Local, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		session := l.Session(sessionID)
		if session != nil && session.Busy() == driver.BusyIdle && len(session.Conversation().Entries()) >= 2 {
			// 让主线程把事件应用完再返回
			time.Sleep(10 * time.Millisecond)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("会话没有回到 idle：%v", l.Session(sessionID).Busy())
}

func digest(t *testing.T, conv *conversation.Conversation) string {
	t.Helper()
	type row struct {
		Role   message.Role
		Status conversation.EntryStatus
		Parts  []message.Part
	}
	rows := make([]row, 0, len(conv.Entries()))
	for _, entry := range conv.Entries() {
		if entry.Message == nil {
			continue
		}
		rows = append(rows, row{
			Role:   entry.Message.Message.Role,
			Status: entry.Message.Status(),
			Parts:  entry.Message.Message.Parts,
		})
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// P1 验收：命令是异步语义 —— Submit 立即返回 CmdID，结果随后以 cmdResult 回到订阅者。
func TestCommandIsAsync(t *testing.T) {
	l, _ := newHost(t)
	session := l.NewSession("")

	type result struct {
		cmdID  string
		result host.CmdResult
	}
	results := make(chan result, 4)
	l.Subscribe(func(change host.Change) {
		if change.Kind == "cmdResult" {
			results <- result{cmdID: change.CmdID, result: change.Result}
		}
	})

	// 一个必然失败的命令（未知会话）：Submit 本身不返回执行错误
	cmdID, err := l.Submit(host.Command{SessionID: session.ID, Kind: "rename", Payload: []byte(`{"text":""}`)})
	if err != nil {
		t.Fatalf("Submit 不该返回执行错误：%v", err)
	}
	if cmdID == "" {
		t.Fatal("Submit 应返回 CmdID")
	}
	select {
	case got := <-results:
		if got.cmdID != cmdID {
			t.Fatalf("cmdResult 的 CmdID 不对：%q vs %q", got.cmdID, cmdID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("没有收到 cmdResult")
	}

	// 空标题的 rename 不做任何事，会话状态不应被改坏
	if got := l.Session(session.ID).Title; got == "" {
		t.Fatalf("标题不应被清空")
	}
}
