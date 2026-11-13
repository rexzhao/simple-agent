package bridge_test

import (
	"context"
	json "encoding/json/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
	"simple-agent/internal/wire"
	"simple-agent/mobile/bridge"
)

// fakeRuntime 让执行端能"流式输出"，测试手动推事件。
type fakeRuntime struct {
	mu      sync.Mutex
	streams []chan providers.Event
}

func (r *fakeRuntime) OpenStream(ctx context.Context, sel providers.Selection, req providers.ChatRequest) (<-chan providers.Event, error) {
	events := make(chan providers.Event, 64)
	r.mu.Lock()
	r.streams = append(r.streams, events)
	r.mu.Unlock()
	return events, nil
}

func (r *fakeRuntime) RunTools(ctx context.Context, tc *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
	out := make([]tools.ToolOutcome, 0, len(calls))
	for _, call := range calls {
		out = append(out, tools.ToolOutcome{ID: call.ID, Name: call.Name, Output: "ok"})
	}
	return out
}

// listener 记录 bridge 推给界面的变更通知。
type listener struct {
	mu     sync.Mutex
	events []string
}

func (l *listener) OnChange(kind string, sessionID string) {
	l.mu.Lock()
	l.events = append(l.events, kind+"|"+sessionID)
	l.mu.Unlock()
}

// TestTimelineJSON 验证 Android 侧拿到的视图模型：
// 会话列表、时间线条目类型、过程组标题、markdown 块、运行时状态。
func TestTimelineJSON(t *testing.T) {
	poster := &serialPoster{}
	rt := &fakeRuntime{}
	local := host.NewLocal(host.Config{
		Runtime:       rt,
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Workspace:     t.TempDir(),
		MaxToolRounds: 3,
		ContextWindow: 200_000,
		Post:          poster.post,
		Models: []host.ProviderChoice{{
			ProviderID: "fake", Label: "测试", Models: []string{"m"},
			Labels: map[string]string{"m": "模型 M"},
		}},
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	sessionID := local.Active().ID

	// 起一个展示端（进程内管道），再包一层 bridge.Client
	viewerConn, execConn := wire.PipePair()
	go func() { _ = host.Serve(execConn, local, poster.post) }()
	_ = viewerConn

	changes := &listener{}
	client := bridge.NewClient(changes, "zh", t.TempDir())
	t.Cleanup(client.Close)

	// 直接注入连接（Android 上是 Connect 自己拨号；测试里用管道避免端口）
	remote, err := host.NewRemote(viewerConn, poster.post)
	if err != nil {
		t.Fatal(err)
	}
	bridge.AttachForTest(client, remote)

	waitFor(t, poster, func() bool {
		var sessions []bridge.SessionSummary
		if err := json.Unmarshal([]byte(client.SessionsJSON()), &sessions); err != nil {
			return false
		}
		return len(sessions) == 1 && sessions[0].ID == sessionID
	})

	// 发一条消息并流式输出 markdown
	payload, _ := host.EncodePayload(host.SendPayload{Text: "你好"})
	if _, err := remote.Submit(host.Command{SessionID: sessionID, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	// 命令要在执行端主线程上跑：先把 post 队列泵空
	poster.pump()
	stream := waitStream(t, poster, rt, 1)
	stream <- providers.Event{Kind: providers.EventText, Text: "# 标题\n\n- 一\n- 二"}
	stream <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}

	var timeline bridge.TimelineView
	waitFor(t, poster, func() bool {
		if err := json.Unmarshal([]byte(client.TimelineJSON(sessionID)), &timeline); err != nil {
			return false
		}
		// 等到"回合真的结束"：snapshot 说 idle 之后 Build 才会补回合结束行
		return len(timeline.Items) >= 2 && timeline.Runtime.Busy == "idle"
	})

	// 第一条正文应是用户的，最后一条是回合结束行（中间可能夹本地提示，跳过）
	var userItem *bridge.ItemView
	for index := range timeline.Items {
		if timeline.Items[index].Kind == "content" && timeline.Items[index].Message != nil {
			userItem = &timeline.Items[index]
			break
		}
	}
	if userItem == nil {
		t.Fatalf("时间线里没有正文条目：%+v", timeline.Items)
	}
	if userItem.Message.Role != "user" {
		t.Fatalf("第一条正文应是 user，实际 %q", userItem.Message.Role)
	}
	footer := timeline.Items[len(timeline.Items)-1]
	if footer.Kind != "footer" || footer.Footer == nil {
		t.Fatalf("最后一条应是回合结束行，实际 %+v", footer)
	}
	if footer.Footer.Status != "complete" {
		t.Fatalf("回合状态应为 complete，实际 %q", footer.Footer.Status)
	}

	// agent 消息里的 markdown 应带**块结构**（Kotlin 照着排版）
	var markdownPart *bridge.PartView
	for _, item := range timeline.Items {
		if item.Message == nil || item.Message.Role != "agent" {
			continue
		}
		for index := range item.Message.Parts {
			if item.Message.Parts[index].Kind == "markdown" {
				markdownPart = &item.Message.Parts[index]
			}
		}
	}
	if markdownPart == nil {
		t.Fatal("agent 消息里没有 markdown part")
	}
	if len(markdownPart.Blocks) < 2 {
		t.Fatalf("markdown 块结构没解析出来：%+v", markdownPart.Blocks)
	}
	if markdownPart.Blocks[0].Kind != "heading" || markdownPart.Blocks[0].Level != 1 {
		t.Fatalf("第一块应是 h1，实际 %+v", markdownPart.Blocks[0])
	}
	if markdownPart.Blocks[1].Kind != "list" || len(markdownPart.Blocks[1].Items) != 2 {
		t.Fatalf("第二块应是有两项的列表，实际 %+v", markdownPart.Blocks[1])
	}

	// 运行时状态：模型显示名来自执行端公布的列表
	if timeline.Runtime.ModelLabel != "模型 M" {
		t.Fatalf("模型显示名应来自执行端，实际 %q", timeline.Runtime.ModelLabel)
	}
	if !timeline.Runtime.CanSend {
		t.Fatal("回合结束后应可以发送")
	}

	// 变更通知至少推过 sessions / conversation / runtime。
	// conversation 是高频事件，会被合并窗口延迟（见 bridge.emit），
	// 所以这里要等一小会儿再断言——这是有意的行为改变，不是丢通知。
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		changes.mu.Lock()
		joined := strings.Join(changes.events, ",")
		changes.mu.Unlock()
		if strings.Contains(joined, "conversation|") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	changes.mu.Lock()
	joined := strings.Join(changes.events, ",")
	changes.mu.Unlock()
	for _, want := range []string{"sessions|", "conversation|"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("没有收到 %q 通知，实际：%s", want, joined)
		}
	}
}

// serialPoster 把回调投到测试线程（等价于桌面的主线程）。
type serialPoster struct {
	mu    sync.Mutex
	queue []func()
}

func (p *serialPoster) post(fn func()) {
	p.mu.Lock()
	p.queue = append(p.queue, fn)
	p.mu.Unlock()
}

func (p *serialPoster) pump() {
	for {
		p.mu.Lock()
		if len(p.queue) == 0 {
			p.mu.Unlock()
			return
		}
		batch := p.queue
		p.queue = nil
		p.mu.Unlock()
		for _, fn := range batch {
			fn()
		}
	}
}

func waitFor(t *testing.T, p *serialPoster, done func() bool) {
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

func waitStream(t *testing.T, p *serialPoster, rt *fakeRuntime, n int) chan providers.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		rt.mu.Lock()
		if len(rt.streams) >= n {
			channel := rt.streams[n-1]
			rt.mu.Unlock()
			return channel
		}
		rt.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("第 %d 条流没有打开", n)
	return nil
}
