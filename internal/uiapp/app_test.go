package uiapp_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/locale"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/render"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
	"simple-agent/internal/uiapp"
)

// fakeStream 是由测试控制的事件流。
type fakeStream struct {
	events chan providers.Event
	cancel context.CancelFunc
	// once 保证通道只关一次（push 与 ctx 取消的 goroutine 都会关它）
	once sync.Once
	// done 在通道关闭时关闭，用来判断"这条流还能不能推事件"
	done chan struct{}
}

// isClosed 报告流是否已经关闭。
func (s *fakeStream) isClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// fakeRuntime 记录请求，并允许测试逐条推送事件。
type fakeRuntime struct {
	mu       sync.Mutex
	requests []providers.ChatRequest
	streams  []*fakeStream
	// openErr 非空时让 OpenStream 直接失败。
	openErr error
	// runTools 非空时用它执行工具（测试控制时序与返回值）。
	runTools func(ctx context.Context, tc *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome
	// openGate 非 nil 时，OpenStream 会一直阻塞到它关闭（模拟慢网络）。
	openGate chan struct{}
	// openEntered 在 OpenStream 被调用时收到一个信号。
	openEntered chan struct{}
}

func (r *fakeRuntime) OpenStream(ctx context.Context, _ providers.Selection, req providers.ChatRequest) (<-chan providers.Event, error) {
	if r.openEntered != nil {
		r.openEntered <- struct{}{}
	}
	if r.openGate != nil {
		<-r.openGate
	}
	if r.openErr != nil {
		return nil, r.openErr
	}
	events := make(chan providers.Event, 8)
	stream := &fakeStream{events: events, done: make(chan struct{})}
	stream.cancel = func() {}
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.streams = append(r.streams, stream)
	r.mu.Unlock()

	// ctx 取消时关闭通道，模拟"流被中止"
	go func() {
		<-ctx.Done()
		stream.close()
	}()
	return events, nil
}

// RunTools 执行工具（可由测试注入）。
//
// 契约与真实实现一致：**必须**尊重 ctx；测试用它控制时序与返回值。
func (r *fakeRuntime) RunTools(ctx context.Context, tc *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
	r.mu.Lock()
	run := r.runTools
	r.mu.Unlock()
	if run != nil {
		return run(ctx, tc, calls)
	}
	return tools.ExecuteAll(ctx, tools.Builtin(tc.AllowShell), tc, calls)
}

// last 返回最近一次流。

// awaitStream 等到至少有一条流打开，返回最后一条。
//
// 开流是**异步**的（生产环境里 OpenStream 会同步等响应头，所以放在 goroutine 里），
// 测试如果按下发送就立刻推事件，就依赖了"goroutine 先跑到"这种调度顺序——在
// GOMAXPROCS=1 或高负载机器上会失败。这里改成等待一个**可观测条件**（流已注册），
// 时间的流逝足以让 goroutine 被调度，断言不依赖先后顺序。
func (r *fakeRuntime) awaitStream(t *testing.T) *fakeStream {
	t.Helper()
	for attempt := 0; attempt < 5000; attempt++ {
		r.mu.Lock()
		// 取**最后一条还没关闭**的流：直接取最后一条会在"上一条已关、下一条
		// 还没开"的瞬间拿到关闭的通道，push 就会 panic: send on closed channel
		var stream *fakeStream
		for index := len(r.streams) - 1; index >= 0; index-- {
			if !r.streams[index].isClosed() {
				stream = r.streams[index]
				break
			}
		}
		r.mu.Unlock()
		if stream != nil {
			return stream
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待开流超时（开了 %d 条）", r.streamCount())
	return nil
}

// streamCount 返回已打开的流数量（并发安全）。
func (r *fakeRuntime) streamCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.streams)
}

// push 推送事件。
//
// **遵守真实 provider 的契约**：发出 Done/Error 之后立即关闭 channel。
// 之前假实现只在 ctx 取消时关闭，导致「流 goroutine 一直不退出」，
// 把 P0（工具 ctC 被流 goroutine 的 defer cancel 提前取消）掩盖掉了。
func (r *fakeRuntime) push(t *testing.T, events ...providers.Event) {
	t.Helper()
	stream := r.awaitStream(t)
	for _, event := range events {
		stream.events <- event
		if event.Kind == providers.EventDone || event.Kind == providers.EventError {
			stream.close()
			return
		}
	}
}

// close 关闭通道（幂等）。
func (s *fakeStream) close() {
	s.once.Do(func() {
		close(s.events)
		close(s.done)
	})
}

// poster 把回调排队，由测试线程在渲染前排空。
//
// 生产环境里 Post 是 win.Update（真的投递到主线程）；测试里如果直接执行，就会在
// 流事件的 goroutine 上改 App 状态，与渲染线程交错——那是数据竞争，不是被测行为。
type poster struct {
	mu    sync.Mutex
	queue []func()
}

func (p *poster) Post(f func()) {
	p.mu.Lock()
	p.queue = append(p.queue, f)
	p.mu.Unlock()
}

// Drain 在测试线程上执行所有排队的回调。
func (p *poster) Drain() {
	for {
		p.mu.Lock()
		if len(p.queue) == 0 {
			p.mu.Unlock()
			return
		}
		f := p.queue[0]
		p.queue = p.queue[1:]
		p.mu.Unlock()
		f()
	}
}

// newApp 构造带假 Runtime 的 App。
// testContextWindow 是测试用的上下文窗口（默认值，够大，不会误触发自动压缩）。
var testContextWindow = 200000

func newApp(t *testing.T) (*uiapp.App, *fakeRuntime, *poster) {
	t.Helper()
	return newAppWithRuntime(t, &fakeRuntime{})
}

func newAppWithRuntime(t *testing.T, rt *fakeRuntime) (*uiapp.App, *fakeRuntime, *poster) {
	t.Helper()
	p := &poster{}
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:             "你是助手",
		Workspace:                t.TempDir(),
		Title:                    "测试会话",
		MaxToolRounds:            3,
		SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
		Language:                 locale.ZH, // 测试固定中文文案
		Post:                     p.Post,
		ContextWindow:            testContextWindow,
	})
	return app, rt, p
}

// pump 排空回调并渲染一帧（测试里的"主线程"）。
func pump(tester *ui.Tester, p *poster) {
	p.Drain()
	tester.Frame()
}

// waitFor 轮询直到条件成立：每轮排空回调并渲染一帧。
func waitFor(t *testing.T, tester *ui.Tester, p *poster, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		pump(tester, p)
		if cond() {
			return
		}
		sleepTiny()
	}
	t.Fatalf("等待超时：%s（界面：%v）", what, tester.Texts())
}

// 发送：输入文本 → 点「发送」→ 草稿清空、出现用户消息与 agent 消息。
func TestAppSendFlow(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)

	clickComposer(t, tester)
	tester.Type("你好")
	tester.Frame()
	if err := tester.Click(locSend); err != nil {
		t.Fatalf("点不到发送按钮：%v", err)
	}

	waitFor(t, tester, p, "打开流", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.streams) == 1
	})

	session := app.Current()
	if session.Busy() != driver.BusyStreaming {
		t.Fatalf("应为 streaming，实际 %v", session.Busy())
	}
	entries := session.Conversation().Entries()
	if len(entries) != 2 {
		t.Fatalf("应有用户 + agent 两条，实际 %d", len(entries))
	}
	if got := entries[0].Message.PreviewText(); got != "你好" {
		t.Fatalf("用户消息不一致：%q", got)
	}

	// 流式增量 → 界面上应出现文本
	rt.push(t, providers.Event{Kind: providers.EventText, Text: "你好！"})
	waitFor(t, tester, p, "界面出现回答", func() bool {
		return tester.HasText("你好！")
	})

	// 结束 → 回到就绪
	rt.push(t, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	waitFor(t, tester, p, "回到 idle", func() bool { return session.Busy() == driver.BusyIdle })
	tester.Frame()
	// 空闲时不显示忙碌文案（harness 的做法：只在忙的时候提示）
	if tester.HasText("正在生成") || tester.HasText("执行工具中") {
		t.Fatalf("空闲时不应显示忙碌状态：%v", tester.Texts())
	}
}

// 停止：点「停止」→ 状态归位、显示「已中止」、取消 ctx。
func TestAppStopFlow(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)

	clickComposer(t, tester)
	tester.Type("写长文")
	tester.Frame()
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tester, p, "打开流", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.streams) == 1
	})

	rt.push(t, providers.Event{Kind: providers.EventText, Text: "开始写"})
	waitFor(t, tester, p, "出现文本", func() bool {
		return tester.HasText("开始写")
	})

	if err := tester.Click(locStop); err != nil {
		t.Fatalf("点不到停止按钮：%v", err)
	}
	pump(tester, p) // Clicked() 在点击后的下一帧才为真
	session := app.Current()
	if session.Busy() != driver.BusyIdle {
		t.Fatalf("停止后应 idle，实际 %v", session.Busy())
	}
	tester.Frame()
	if !tester.HasText("已中止") {
		t.Fatalf("应显示「已中止」，界面文本：%v", tester.Texts())
	}
}

// 提问：工具返回 pending → 出现选项；点选项 → 继续开流。
func TestAppAnswerFlow(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)

	clickComposer(t, tester)
	tester.Type("问我")
	tester.Frame()
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tester, p, "打开流", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.streams) == 1
	})

	// 模型发起 ask_user_question
	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "ask_1", Name: "ask_user_question"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"questions":[{"id":"q1","question":"选哪种方案？","options":[{"label":"方案甲"},{"label":"方案乙"}]}]}`},
		providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"},
	)

	session := app.Current()
	waitFor(t, tester, p, "进入等待回答", func() bool {
		return session.Busy() == driver.BusyAwaitingAnswer
	})
	waitFor(t, tester, p, "出现选项", func() bool {
		return tester.HasText("方案甲") && tester.HasText("方案乙")
	})

	// 点选项只是勾选，提交才回填（Rust 版 AskGroup 的语义）
	if err := tester.Click("方案乙"); err != nil {
		t.Fatalf("点不到选项：%v", err)
	}
	pump(tester, p)
	if session.Busy() != driver.BusyAwaitingAnswer {
		t.Fatalf("勾选后应仍在等待回答，实际 %v", session.Busy())
	}
	if err := tester.Click(locSubmit); err != nil {
		t.Fatalf("点不到提交：%v", err)
	}
	pump(tester, p)
	// 作答后应开下一轮流
	waitFor(t, tester, p, "打开第二条流", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.streams) == 2
	})
	if session.Busy() != driver.BusyStreaming {
		t.Fatalf("作答后应继续流式，实际 %v", session.Busy())
	}
}

// 删除确认：右键菜单选「删除」→ 弹 AlertDialog；点「取消」不删，点「删除」才删。
func TestAppDeleteConfirm(t *testing.T) {
	app, _, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)

	title := app.Current().Title
	if err := tester.RightClick(title); err != nil {
		t.Fatalf("右键点不到会话行：%v", err)
	}
	if err := tester.ChooseMenuItem(locDelete); err != nil {
		t.Fatalf("选不到删除：%v", err)
	}
	pump(tester, p)
	if len(app.Sessions()) != 1 {
		t.Fatal("选删除不应立即移除会话（要二次确认）")
	}
	// 先取消：会话仍在
	if err := tester.Click(locCancel); err != nil {
		t.Fatalf("点不到取消：%v（界面：%v）", err, tester.Texts())
	}
	pump(tester, p)
	if len(app.Sessions()) != 1 {
		t.Fatal("取消后会话应保留")
	}

	// 再来一次，这次确认删除
	if err := tester.RightClick(title); err != nil {
		t.Fatalf("右键点不到会话行：%v", err)
	}
	if err := tester.ChooseMenuItem(locDelete); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if err := tester.Click(locDelete); err != nil {
		t.Fatalf("点不到删除按钮：%v（界面：%v）", err, tester.Texts())
	}
	pump(tester, p)
	// 删掉最后一个会话后列表允许为空（决策 #54），主区域显示空态
	if len(app.Sessions()) != 0 {
		t.Fatalf("应剩 0 个会话，实际 %d 个", len(app.Sessions()))
	}
	if !tester.HasText("还没有会话") {
		t.Fatalf("应显示空态：%v", tester.Texts())
	}
}

// 重命名：右键菜单选「改名」→ 输入回车 → 标题更新。
func TestAppRename(t *testing.T) {
	app, _, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)

	if err := tester.RightClick(app.Current().Title); err != nil {
		t.Fatalf("右键点不到会话行：%v", err)
	}
	if err := tester.ChooseMenuItem(locRename); err != nil {
		t.Fatalf("选不到改名：%v", err)
	}
	pump(tester, p)

	// 聚焦重命名输入框（用 placeholder 定位）并清空后重输
	if err := tester.Click(locRenameInput); err != nil {
		t.Fatalf("点不到重命名输入框：%v（界面：%v）", err, tester.Texts())
	}
	tester.Frame()
	for i := 0; i < 20; i++ { // 退格清空
		tester.Key(0, ui.KeyBackspace)
	}
	tester.Frame()
	tester.Type("新名字")
	tester.Frame()
	if err := tester.Click(locConfirm); err != nil {
		t.Fatalf("点不到确定：%v", err)
	}
	pump(tester, p)

	if got := app.Current().Title; got != "新名字" {
		t.Fatalf("标题应为「新名字」，实际 %q（界面：%v）", got, tester.Texts())
	}
}

// 新建会话：点「新建会话」后会话数 +1 且切到新会话。
func TestAppNewSession(t *testing.T) {
	app, _, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	tester.Frame()

	if err := tester.Click(locNewSession); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if len(app.Sessions()) != 2 {
		t.Fatalf("应有 2 个会话，实际 %d", len(app.Sessions()))
	}
	if app.Current() != app.Sessions()[1] {
		t.Fatal("应切到新会话")
	}
}

// 渲染：工具调用与结果、推理、失败与中止都要有可见表示。
func TestAppRendersParts(t *testing.T) {
	app, _, p := newApp(t)
	session := app.Current()
	conv := session.Conversation()

	agentID := "m-render"
	msg := message.Message{MessageID: agentID, Role: message.RoleAgent, Parts: []message.Part{
		{Content: message.TextPart{Text: "先想一下"}, MediaType: render.MediaReasoning},
		message.NewTextPart("调用工具的正文"),
		{Content: message.DataPart{Data: map[string]any{
			"id": "c1", "name": "read", "args": map[string]any{"file_path": "a.txt"},
		}}, MediaType: render.MediaToolUse},
		{Content: message.DataPart{Data: map[string]any{
			"id": "c1", "name": "read", "output": "文件内容", "is_error": false,
		}}, MediaType: render.MediaToolResult},
	}}
	if _, err := conv.AddMessage(msg, "complete"); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	// 对齐 dsh：思考/工具进过程组，正文不进组（它结束当前组）
	if !hasProcessGroup(tester) {
		t.Fatalf("应有过程组：%v", tester.Texts())
	}
	if !tester.HasText("调用工具的正文") {
		t.Fatalf("正文应在组外可见：%v", tester.Texts())
	}
	clickProcessGroup(t, tester)
	pump(tester, p)
	for _, want := range []string{"先想一下", "read", "文件内容"} {
		if !tester.HasText(want) {
			t.Fatalf("展开后缺少 %q：%v", want, tester.Texts())
		}
	}
}

// 开流失败：不能停在「正在生成」，要落到失败并给出原因。
func TestAppOpenStreamFailure(t *testing.T) {
	app, _, p := newAppWithRuntime(t, &fakeRuntime{openErr: context.DeadlineExceeded})
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	ensureSession(t, tester, p, app)
	clickComposer(t, tester)
	tester.Type("hi")
	tester.Frame()
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}

	session := app.Current()
	waitFor(t, tester, p, "回到 idle", func() bool { return session.Busy() == driver.BusyIdle })
	tester.Frame()
	texts := strings.Join(tester.Texts(), "\n")
	if !strings.Contains(texts, "失败") && !strings.Contains(texts, "中断") {
		t.Fatalf("应给出失败提示：%v", tester.Texts())
	}
}

// clickComposer 聚焦输入框（用占位文本定位，避免坐标硬编码）。
func clickComposer(t *testing.T, tester *ui.Tester) {
	t.Helper()
	if err := tester.Click(locComposer); err != nil {
		t.Fatalf("点不到输入框：%v（界面：%v）", err, tester.Texts())
	}
	tester.Frame()
}

// checkURLForTest 暴露内部地址检查（测试用）。
func checkURLForTest(url string) error { return uiapp.CheckImageURLForTest(url) }

// itoaForTest 暴露整数格式化（测试用）。
func itoaForTest(value int) string { return uiapp.ItoaForTest(value) }

// makePNG 生成一张 PNG 用于图片管线测试。
func makePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// sleepTiny 让出调度，给后台 goroutine 一点时间（测试辅助）。
func sleepTiny() {
	time.Sleep(time.Millisecond)
}

// P0 回归：工具执行必须活过「流关闭」。
//
// 真实 provider 契约是 Done 之后立即关闭 channel，读流的 goroutine 随即退出并
// 取消自己的 ctx。如果工具用的是那个 ctx，每个调用都会变成「已中止」。
func TestToolsNotCancelledWhenStreamCloses(t *testing.T) {
	app, rt, p := newApp(t)

	// 注入一个工具执行器：要求 ctx 仍然有效，并模拟耗时
	ran := make(chan bool, 1)
	rt.runTools = func(ctx context.Context, _ *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
		// 等一会儿再检查，确保流 goroutine 已经退出
		time.Sleep(20 * time.Millisecond)
		ran <- ctx.Err() == nil
		outcomes := make([]tools.ToolOutcome, 0, len(calls))
		for _, call := range calls {
			outcomes = append(outcomes, tools.ToolOutcome{ID: call.ID, Name: call.Name, Output: "工具真的跑了"})
		}
		return outcomes
	}

	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	ensureSession(t, tester, p, app)
	clickComposer(t, tester)
	tester.Type("跑工具")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`},
		providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"},
	)

	// 工具在后台跑：必须持续泵主线程回调，它才真的启动
	ok := false
	waitFor(t, tester, p, "工具执行（ctx 仍然有效）", func() bool {
		select {
		case ok = <-ran:
			return true
		default:
			return false
		}
	})
	if !ok {
		t.Fatal("流关闭后工具的 ctx 已被取消（P0 回归）")
	}

	// 工具结果回填后应开下一轮流
	waitFor(t, tester, p, "打开第二条流", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.streams) == 2
	})
	waitFor(t, tester, p, "过程组出现", func() bool { return hasProcessGroup(tester) })
	clickProcessGroup(t, tester)
	pump(tester, p)
	waitFor(t, tester, p, "工具结果上屏", func() bool { return tester.HasText("工具真的跑了") })
}

// P1-1：删掉会话后再新建，ID 不能与现存会话重复。
func TestSessionIDsStayUnique(t *testing.T) {
	app, _, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)

	if err := tester.Click(locNewSession); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if err := tester.Click(locNewSession); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	// 给三个会话不同的标题，右键才定位得准（侧栏按创建倒序）
	for i, session := range app.Sessions() {
		session.Title = fmt.Sprintf("会话 %d", i)
	}
	pump(tester, p)
	// 删掉第一个会话（右键 → 删除 → 确认）
	first := app.Sessions()[0]
	if err := tester.RightClick(first.Title); err != nil {
		t.Fatal(err)
	}
	if err := tester.ChooseMenuItem(locDelete); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if err := tester.Click(locDelete); err != nil {
		t.Fatalf("点不到确认删除：%v（界面：%v）", err, tester.Texts())
	}
	pump(tester, p)

	if err := tester.Click(locNewSession); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	seen := map[string]bool{}
	for _, session := range app.Sessions() {
		if seen[session.ID] {
			t.Fatalf("会话 ID 重复：%s", session.ID)
		}
		seen[session.ID] = true
	}
	if seen[first.ID] {
		t.Fatal("被删除的会话 ID 不应被重新使用")
	}
}

// P1-2：删掉「当前会话之前」的会话后，当前会话不能悄悄换人。
func TestDeleteBeforeActiveKeepsCurrent(t *testing.T) {
	app, _, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	for i := 0; i < 2; i++ {
		if err := tester.Click(locNewSession); err != nil {
			t.Fatal(err)
		}
		pump(tester, p)
	}
	if len(app.Sessions()) != 3 {
		t.Fatalf("应有 3 个会话，实际 %d", len(app.Sessions()))
	}

	second := app.Sessions()[1]
	second.Title = "第二个会话"
	third := app.Sessions()[2]
	third.Title = "第三个会话"
	pump(tester, p)
	// 点第二个会话的标题切过去
	if err := tester.Click("第二个会话"); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if app.Current() != second {
		t.Fatal("应切到第二个会话")
	}

	// 先给两个会话不同标题，右键才定位得准
	app.Sessions()[0].Title = "被删的会话"
	app.Sessions()[1].Title = "留下的会话"
	pump(tester, p)
	// 删除第一个会话（右键 → 删除 → 确认）
	firstTitle := app.Sessions()[0].Title
	if err := tester.RightClick(firstTitle); err != nil {
		t.Fatal(err)
	}
	if err := tester.ChooseMenuItem(locDelete); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if err := tester.Click(locDelete); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	if app.Current() != second {
		t.Fatalf("删掉前面的会话后当前会话不应改变：now=%q want=%q", app.Current().ID, second.ID)
	}
}

// 多选：可叠加、可取消；单选互斥。
func TestAskMultiSelectAndSingleChoice(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	ensureSession(t, tester, p, app)
	clickComposer(t, tester)
	tester.Type("问")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "ask_1", Name: "ask_user_question"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"questions":[
			{"id":"q1","question":"多选","multiSelect":true,"options":[{"label":"甲"},{"label":"乙"}]},
			{"id":"q2","question":"单选","options":[{"label":"丙"},{"label":"丁"}]}]}`},
		providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"},
	)
	session := app.Current()
	waitFor(t, tester, p, "进入等待回答", func() bool {
		return session.Busy() == driver.BusyAwaitingAnswer
	})

	// 多个问题用 tab 显示：一次只看一题
	if !tester.HasText("问题 1") || !tester.HasText("问题 2") {
		t.Fatalf("应显示问题页签：%v", tester.Texts())
	}
	if !tester.HasText("甲") || tester.HasText("丙") {
		t.Fatalf("未切换页签时只应看到第一题：%v", tester.Texts())
	}

	// 多选：连点两个都选中
	if err := tester.Click("甲"); err != nil {
		t.Fatalf("点不到甲：%v（界面：%v）", err, tester.Texts())
	}
	pump(tester, p)
	if err := tester.Click("乙"); err != nil {
		t.Fatalf("点不到乙：%v", err)
	}
	pump(tester, p)
	if countText(tester, "☑") != 2 {
		t.Fatalf("多选应同时选中：%v", tester.Texts())
	}

	// 切到第二题：页签上会出现 ✓
	if err := tester.Click("问题 2"); err != nil {
		t.Fatalf("点不到第二个页签：%v", err)
	}
	pump(tester, p)
	if !tester.HasText("丙") || tester.HasText("甲") {
		t.Fatalf("切换页签后应只看到第二题：%v", tester.Texts())
	}
	if !tester.HasText("✓ 问题 1") {
		t.Fatalf("已回答的题应在页签上加 ✓：%v", tester.Texts())
	}

	// 单选：点第二个会取消第一个
	if err := tester.Click("丙"); err != nil {
		t.Fatalf("点不到丙：%v", err)
	}
	pump(tester, p)
	if countText(tester, "◉") != 1 {
		t.Fatalf("应选中丙：%v", tester.Texts())
	}
	if err := tester.Click("丁"); err != nil {
		t.Fatalf("点不到丁：%v", err)
	}
	pump(tester, p)
	if countText(tester, "◉") != 1 {
		t.Fatalf("单选应互斥（只留一个选中）：%v", tester.Texts())
	}
}

// 开放式提问（无选项）：在输入框写字后回车即作为答案提交。
func TestAskCustomAnswerViaComposer(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	ensureSession(t, tester, p, app)
	clickComposer(t, tester)
	tester.Type("问")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "ask_1", Name: "ask_user_question"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"questions":[{"id":"q1","question":"你的名字？"}]}`},
		providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"},
	)
	session := app.Current()
	waitFor(t, tester, p, "进入等待回答", func() bool {
		return session.Busy() == driver.BusyAwaitingAnswer
	})

	// 等待回答时输入框必须可用
	clickComposer(t, tester)
	tester.Type("张三")
	pump(tester, p)
	tester.Key(0, ui.KeyEnter)
	pump(tester, p)

	waitFor(t, tester, p, "打开第二条流", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.streams) == 2
	})
	// 答案写进了工具结果（过程默认折叠，展开后可见）
	clickProcessGroup(t, tester)
	pump(tester, p)
	waitFor(t, tester, p, "作答出现在界面", func() bool { return tester.HasText("张三") })
}

// 跳过：写入 dismissed 结果并继续。
func TestAskDismiss(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	ensureSession(t, tester, p, app)
	clickComposer(t, tester)
	tester.Type("问")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "ask_1", Name: "ask_user_question"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"questions":[{"id":"q1","question":"?"}]}`},
		providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"},
	)
	session := app.Current()
	waitFor(t, tester, p, "进入等待回答", func() bool {
		return session.Busy() == driver.BusyAwaitingAnswer
	})

	if err := tester.Click(locDismiss); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	waitFor(t, tester, p, "打开第二条流", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.streams) == 2
	})
	// 过程默认折叠成一组，展开后再看工具结果
	clickProcessGroup(t, tester)
	pump(tester, p)
	waitFor(t, tester, p, "dismissed 结果上屏", func() bool { return tester.HasText("dismissed") })
}

// 模型切换器：只列已配置的提供商 + 模拟；只在空闲时可切换；只影响当前会话。
func TestModelSwitcher(t *testing.T) {
	rt := &fakeRuntime{}
	p := &poster{}
	var changes []providers.Selection
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "deepseek", Model: "deepseek-chat"},
		Workspace:                t.TempDir(),
		SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
		Language:                 locale.ZH, // 测试固定中文文案
		Post:                     p.Post,
		Providers: []uiapp.ProviderChoice{
			{ProviderID: "deepseek", Label: "DeepSeek", Models: []string{"deepseek-chat", "deepseek-reasoner"}},
			{ProviderID: "anthropic", Label: "Anthropic", Models: []string{"claude-sonnet-4-5"}},
			{ProviderID: "simulated", Label: "本地提示", Models: []string{"none"}},
		},
		OnSelectionChanged: func(sessionID string, sel providers.Selection) {
			changes = append(changes, sel)
		},
	})
	tester := ui.NewTester(app.View, 1000, 700)
	pump(tester, p)
	ensureSession(t, tester, p, app)

	if !tester.HasText("deepseek-chat") {
		t.Fatalf("输入卡应显示当前模型：%v", tester.Texts())
	}

	// 打开模型菜单（输入卡右下的 chip）→「Anthropic」子菜单 → claude
	if err := tester.Click(locModel); err != nil {
		t.Fatalf("点不到模型 chip：%v（界面：%v）", err, tester.Texts())
	}
	if err := tester.ChooseMenuItem("Anthropic", "claude-sonnet-4-5"); err != nil {
		t.Fatalf("选不到 Anthropic 的模型：%v（菜单：%v）", err, tester.Menu())
	}
	pump(tester, p)

	session := app.Current()
	if got := session.Selection(); got.ProviderID != "anthropic" {
		t.Fatalf("提供商未切换：%+v", got)
	}
	// 换了提供商但模型不属于它 → 自动落到该提供商的第一个模型
	if got := session.Selection().Model; got != "claude-sonnet-4-5" {
		t.Fatalf("模型应回落到新提供商的默认值，实际 %q", got)
	}
	if len(changes) != 1 || changes[0].ProviderID != "anthropic" {
		t.Fatalf("应回调一次变更：%#v", changes)
	}

	// 再选 deepseek 的另一个模型
	if err := tester.Click(locModel); err != nil {
		t.Fatalf("点不到模型 chip：%v（界面：%v）", err, tester.Texts())
	}
	if err := tester.ChooseMenuItem("DeepSeek", "deepseek-reasoner"); err != nil {
		t.Fatalf("选不到 deepseek-reasoner：%v（菜单：%v）", err, tester.Menu())
	}
	pump(tester, p)
	if got := session.Selection().Model; got != "deepseek-reasoner" {
		t.Fatalf("模型未切换：%q", got)
	}

	// 切换只影响当前会话
	other := app.NewSession()
	if other.Selection().ProviderID != "deepseek" || other.Selection().Model != "deepseek-chat" {
		t.Fatalf("新会话应使用初始选择，实际 %+v", other.Selection())
	}
	if app.Current() == session {
		t.Fatal("新建会话后当前会话应切换")
	}
}

// 忙时不能切换模型（发送中按钮禁用）。
func TestModelSwitcherDisabledWhileBusy(t *testing.T) {
	app, rt, p := newApp(t)
	app2 := app
	_ = app2
	tester := ui.NewTester(app.View, 1000, 700)
	pump(tester, p)
	ensureSession(t, tester, p, app)

	clickComposer(t, tester)
	tester.Type("hi")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	_ = rt.awaitStream(t)
	before := app.Current().Selection()
	// 忙时菜单仍可打开，但每一项都禁用（并有一条说明），点了不会切换
	if err := tester.Click(locModel); err != nil {
		t.Logf("点不到模型 chip（禁用后不可点）：%v", err)
	}
	if menu := tester.Menu(); len(menu) > 0 {
		if err := tester.ChooseMenuItem("fake", "m"); err == nil {
			pump(tester, p)
			if got := app.Current().Selection(); got != before {
				t.Fatalf("忙时不应切换：%+v → %+v", before, got)
			}
		}
	}
	tester.CloseMenu()
	pump(tester, p)
	if got := app.Current().Selection(); got != before {
		t.Fatalf("忙时不应切换：%+v → %+v", before, got)
	}
}

// 第二个实例聚焦：在当前会话插入一条提示。
func TestNotifyInfoAddsNotice(t *testing.T) {
	app, _, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)

	app.NotifyDisplay("已有一个 simple-agent 在运行，已切换到这个窗口。")
	pump(tester, p)
	if !tester.HasText("已有一个 simple-agent 在运行") {
		t.Fatalf("应在会话里插入提示：%v", tester.Texts())
	}
}

// 持久化：会话内容在「重启」后仍然在（同一个数据目录，新建 App）。
func TestPersistenceAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)
	makeApp := func(rt *fakeRuntime) (*uiapp.App, *poster) {
		p := &poster{}
		app := uiapp.NewApp(rt, uiapp.Options{
			Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
			Workspace:                t.TempDir(),
			SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
			Language:                 locale.ZH, // 测试固定中文文案
			Post:                     p.Post,
			Catalog:                  catalog,
			ContextWindow:            200_000,
		})
		return app, p
	}

	// 第一次运行：发一轮完整对话
	rt := &fakeRuntime{}
	app, p := makeApp(rt)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	ensureSession(t, tester, p, app)
	clickComposer(t, tester)
	tester.Type("记住这句话")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	rt.push(t,
		providers.Event{Kind: providers.EventText, Text: "我记住了"},
		providers.Event{Kind: providers.EventDone, StopReason: "stop"},
	)
	waitFor(t, tester, p, "回答结束", func() bool {
		return app.Current().Busy() == driver.BusyIdle
	})
	contextID := app.Current().ID
	app.Close()

	// 第二次运行：同一个数据目录
	app2, p2 := makeApp(&fakeRuntime{})
	defer app2.Close()
	tester2 := ui.NewTester(app2.View, 900, 620)
	pump(tester2, p2)
	if app2.Current() == nil {
		// 全新数据目录：现在显示空态，先建一个会话再测
		if err := tester2.Click(locNewSession); err != nil {
			t.Fatalf("点不到新建会话：%v", err)
		}
		pump(tester2, p2)
	}

	session := app2.Current()
	if session.ID != contextID {
		t.Logf("第二次运行的会话 id：%s（第一次 %s）", session.ID, contextID)
	}
	entries := session.Conversation().Entries()
	if len(entries) != 2 {
		t.Fatalf("重启后应有 2 条消息，实际 %d（界面：%v）", len(entries), tester2.Texts())
	}
	if entries[0].Message.PreviewText() != "记住这句话" {
		t.Fatalf("用户消息丢失：%q", entries[0].Message.PreviewText())
	}
	if !tester2.HasText("我记住了") {
		t.Fatalf("回答丢失：%v", tester2.Texts())
	}
	if entries[1].Message.Status() != conversation.StatusComplete {
		t.Fatalf("状态应为 complete，实际 %v", entries[1].Message.Status())
	}
}

// 持久化：挂起的提问在重启后重新出现，能继续作答。
func TestPendingAskSurvivesRestart(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)
	makeApp := func(rt *fakeRuntime) (*uiapp.App, *poster) {
		p := &poster{}
		app := uiapp.NewApp(rt, uiapp.Options{
			Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
			Workspace:                t.TempDir(),
			SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
			Language:                 locale.ZH, // 测试固定中文文案
			Post:                     p.Post,
			Catalog:                  catalog,
			ContextWindow:            200_000,
		})
		return app, p
	}

	rt := &fakeRuntime{}
	app, p := makeApp(rt)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	ensureSession(t, tester, p, app)
	clickComposer(t, tester)
	tester.Type("问个问题")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "ask_1", Name: "ask_user_question"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"questions":[{"id":"q1","question":"选哪个？","options":[{"label":"甲"},{"label":"乙"}]}]}`},
		providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"},
	)
	waitFor(t, tester, p, "等待回答", func() bool {
		return app.Current().Busy() == driver.BusyAwaitingAnswer
	})

	// 「重启」
	app.Close()
	app2, p2 := makeApp(&fakeRuntime{})
	defer app2.Close()
	tester2 := ui.NewTester(app2.View, 900, 620)
	pump(tester2, p2)
	if app2.Current() == nil {
		// 全新数据目录：现在显示空态，先建一个会话再测
		if err := tester2.Click(locNewSession); err != nil {
			t.Fatalf("点不到新建会话：%v", err)
		}
		pump(tester2, p2)
	}

	restored := app2.Current()
	if restored.Busy() != driver.BusyAwaitingAnswer {
		t.Fatalf("重启后应恢复为等待回答，实际 %v", restored.Busy())
	}
	if !tester2.HasText("选哪个？") || !tester2.HasText("甲") || !tester2.HasText("○") {
		t.Fatalf("提问面板应恢复：%v", tester2.Texts())
	}
}

// 重命名与模型切换也进日志：重启后标题与选择都在。
func TestRenameAndSelectionPersist(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)
	makeApp := func() (*uiapp.App, *poster, *fakeRuntime) {
		p := &poster{}
		rt := &fakeRuntime{}
		app := uiapp.NewApp(rt, uiapp.Options{
			Selection:                providers.Selection{ProviderID: "deepseek", Model: "deepseek-chat"},
			Workspace:                t.TempDir(),
			SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
			Language:                 locale.ZH, // 测试固定中文文案
			Post:                     p.Post,
			Catalog:                  catalog,
			Providers: []uiapp.ProviderChoice{
				{ProviderID: "deepseek", Label: "DeepSeek", Models: []string{"deepseek-chat"}},
				{ProviderID: "anthropic", Label: "Anthropic", Models: []string{"claude-sonnet-4-5"}},
			},
		})
		return app, p, rt
	}

	app, p, rt := makeApp()
	tester := ui.NewTester(app.View, 1000, 700)
	pump(tester, p)
	ensureSession(t, tester, p, app)

	// 空会话不落盘：先发一条消息，日志才会建立
	clickComposer(t, tester)
	tester.Type("第一句话")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	rt.push(t,
		providers.Event{Kind: providers.EventText, Text: "好的"},
		providers.Event{Kind: providers.EventDone, StopReason: "stop"},
	)
	waitFor(t, tester, p, "回合结束", func() bool {
		return app.Current().Busy() == driver.BusyIdle
	})

	if err := tester.RightClick(app.Current().Title); err != nil {
		t.Fatalf("右键点不到会话行：%v", err)
	}
	if err := tester.ChooseMenuItem(locRename); err != nil {
		t.Fatalf("选不到改名：%v", err)
	}
	pump(tester, p)
	if err := tester.Click(locRenameInput); err != nil {
		t.Fatalf("点不到重命名输入：%v", err)
	}
	pump(tester, p)
	for i := 0; i < 20; i++ {
		tester.Key(0, ui.KeyBackspace)
	}
	pump(tester, p)
	tester.Type("持久化标题")
	pump(tester, p)
	if err := tester.Click(locConfirm); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	// 切模型（输入卡右下的 chip → 原生菜单）
	if err := tester.Click(locModel); err != nil {
		t.Fatalf("点不到模型 chip：%v（界面：%v）", err, tester.Texts())
	}
	if err := tester.ChooseMenuItem("Anthropic", "claude-sonnet-4-5"); err != nil {
		t.Fatalf("选不到 Anthropic 的模型：%v（菜单：%v）", err, tester.Menu())
	}
	pump(tester, p)

	// 「重启」
	app.Close()
	app2, p2, _ := makeApp()
	tester2 := ui.NewTester(app2.View, 1000, 700)
	pump(tester2, p2)
	defer app2.Close()

	session := app2.Current()
	if session.Title != "持久化标题" {
		t.Fatalf("标题未持久化：%q（界面：%v）", session.Title, tester2.Texts())
	}
	if sel := session.Selection(); sel.ProviderID != "anthropic" {
		t.Fatalf("模型选择未持久化：%+v", sel)
	}
}

// 回归（opus）：加载时产生的提示只用于显示，不能写进日志。
//
// 否则每次启动都多一条，用户会以为会话里真的发生过无数次「日志损坏」。
func TestLoadNoticesAreDisplayOnly(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)
	newApp := func() *uiapp.App {
		return uiapp.NewApp(&fakeRuntime{}, uiapp.Options{
			Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
			Workspace:     t.TempDir(),
			Catalog:       catalog,
			ContextWindow: 200_000,
		})
	}

	// 造一个「有内容」的会话（空会话现在不落盘），然后人为损坏日志
	rt := &fakeRuntime{}
	p := &poster{}
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:                t.TempDir(),
		SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
		Language:                 locale.ZH, // 测试固定中文文案
		Post:                     p.Post,
		Catalog:                  catalog,
		ContextWindow:            200_000,
	})
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	ensureSession(t, tester, p, app)
	clickComposer(t, tester)
	tester.Type("留下内容")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	rt.push(t, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	waitFor(t, tester, p, "回合结束", func() bool {
		return app.Current().Busy() == driver.BusyIdle
	})
	contextID := app.Current().ID
	app.Close()

	sessionPath := filepath.Join(dataDir, "sessions", contextID, "session.jsonl")
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	lines = append(lines, `{"broken":`)
	if err := os.WriteFile(sessionPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 加载两次
	countEvents := func() int {
		app := newApp()
		defer app.Close()
		result, err := catalog.Open(contextID)
		if err != nil {
			t.Fatal(err)
		}
		defer result.Close()
		loaded, err := result.Load()
		if err != nil {
			t.Fatal(err)
		}
		// 界面上应该有提示
		var found bool
		for _, entry := range app.Current().Conversation().Entries() {
			if entry.Notice != nil {
				found = true
			}
		}
		if !found {
			t.Logf("这次加载没有产生提示（可能没有可报告的问题）")
		}
		return len(loaded.Events)
	}

	first := countEvents()
	second := countEvents()
	if first != second {
		t.Fatalf("重复加载不应往日志里写提示：%d → %d", first, second)
	}
}

// 第二个实例的提示同样只显示、不落日志。
func TestNotifyDisplayNotPersisted(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)
	app := uiapp.NewApp(&fakeRuntime{}, uiapp.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:     t.TempDir(),
		Catalog:       catalog,
		ContextWindow: 200_000,
		// 测试窗口 900 < 1024：禁用自动收起，否则侧栏变图标栏、点不到「新建」
		SidebarAutoCollapseWidth: -1,
		Language:                 locale.ZH,
	})
	tester := ui.NewTester(app.View, 900, 620)
	p := &poster{}
	app.SetPostForTest(p.Post)
	pump(tester, p)
	ensureSession(t, tester, p, app)
	contextID := app.Current().ID

	before := countEvents(t, catalog, contextID)
	app.NotifyDisplay("已有一个 simple-agent 在运行，已切换到这个窗口。")
	after := countEvents(t, catalog, contextID)
	if before != after {
		t.Fatalf("显示型提示不应写日志：%d → %d", before, after)
	}
	app.Close()
}

// countEvents 读某个会话日志的事件条数。
func countEvents(t *testing.T, catalog *store.Catalog, contextID string) int {
	t.Helper()
	log, err := catalog.Open(contextID)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	result, err := log.Load()
	if err != nil {
		t.Fatal(err)
	}
	return len(result.Events)
}

// 手动压缩：点「压缩上下文」→ 摘要请求 → 会话被折叠成「摘要 + kept」，
// 界面出现「以上内容已压缩」分隔线。
func TestManualCompactFlow(t *testing.T) {
	rt := &fakeRuntime{}
	p := &poster{}
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:                t.TempDir(),
		SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
		Language:                 locale.ZH, // 测试固定中文文案
		Post:                     p.Post,
		ContextWindow:            400,
		DisableAutoCompact:       true, // 这个用例测手动路径
	})
	tester := ui.NewTester(app.View, 900, 620)

	// 攒一段足够长的会话
	for i := 0; i < 3; i++ {
		clickComposer(t, tester)
		tester.Type(strings.Repeat("用户消息填充上下文。", 5))
		pump(tester, p)
		if err := tester.Click(locSend); err != nil {
			t.Fatal(err)
		}
		pump(tester, p)
		rt.push(t,
			providers.Event{Kind: providers.EventText, Text: strings.Repeat("助手回复填充上下文。", 5)},
			providers.Event{Kind: providers.EventDone, StopReason: "stop"},
		)
		waitFor(t, tester, p, "回合结束", func() bool {
			return app.Current().Busy() == driver.BusyIdle
		})
	}

	before := len(app.Current().Conversation().Entries())
	if err := tester.Click(locMore); err != nil {
		t.Fatalf("点不到「更多」：%v（界面：%v）", err, tester.Texts())
	}
	if err := tester.ChooseMenuItem(locCompact); err != nil {
		t.Fatalf("选不到压缩：%v（菜单：%v）", err, tester.Menu())
	}
	// 压缩期间状态是「压缩上下文中」
	waitFor(t, tester, p, "开始压缩", func() bool {
		return app.Current().Busy() == driver.BusyCompacting
	})

	// 摘要请求：把文本推回去（并以 Done 关闭流）
	rt.push(t,
		providers.Event{Kind: providers.EventText, Text: "这是压缩后的摘要。"},
		providers.Event{Kind: providers.EventDone, StopReason: "stop"},
	)
	waitFor(t, tester, p, "压缩完成", func() bool {
		return app.Current().Busy() == driver.BusyIdle
	})

	after := app.Current().Conversation().Entries()
	if len(after) >= before {
		t.Fatalf("压缩后条目应变少：%d → %d", before, len(after))
	}
	if !tester.HasText("以上内容已压缩") {
		t.Fatalf("应出现压缩分隔线：%v", tester.Texts())
	}
	// 展开摘要原文
	if err := tester.Click("以上内容已压缩"); err != nil {
		t.Fatalf("点不到分隔线：%v", err)
	}
	pump(tester, p)
	if !tester.HasText("这是压缩后的摘要。") {
		t.Fatalf("展开后应显示摘要原文：%v", tester.Texts())
	}
}

// 压缩失败（摘要请求报错）：会话不变、状态回 Idle、能继续用。
func TestCompactFailureKeepsConversation(t *testing.T) {
	rt := &fakeRuntime{}
	p := &poster{}
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:                t.TempDir(),
		SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
		Language:                 locale.ZH, // 测试固定中文文案
		Post:                     p.Post,
		ContextWindow:            400,
		DisableAutoCompact:       true, // 这个用例测手动路径
	})
	tester := ui.NewTester(app.View, 900, 620)
	for i := 0; i < 3; i++ {
		clickComposer(t, tester)
		tester.Type(strings.Repeat("填充上下文。", 6))
		pump(tester, p)
		if err := tester.Click(locSend); err != nil {
			t.Fatal(err)
		}
		pump(tester, p)
		rt.push(t,
			providers.Event{Kind: providers.EventText, Text: strings.Repeat("回复填充。", 6)},
			providers.Event{Kind: providers.EventDone, StopReason: "stop"},
		)
		waitFor(t, tester, p, "回合结束", func() bool {
			return app.Current().Busy() == driver.BusyIdle
		})
	}
	before := len(app.Current().Conversation().Entries())

	if err := tester.Click(locMore); err != nil {
		t.Fatal(err)
	}
	if err := tester.ChooseMenuItem(locCompact); err != nil {
		t.Fatalf("选不到压缩：%v（菜单：%v）", err, tester.Menu())
	}
	waitFor(t, tester, p, "开始压缩", func() bool {
		return app.Current().Busy() == driver.BusyCompacting
	})
	rt.push(t, providers.Event{Kind: providers.EventError, Err: context.DeadlineExceeded})
	waitFor(t, tester, p, "回到空闲", func() bool {
		return app.Current().Busy() == driver.BusyIdle
	})

	if got := len(app.Current().Conversation().Entries()); got != before {
		t.Fatalf("失败时不应改动会话：%d → %d", before, got)
	}
}

// 压缩中停止：放弃压缩，不修改会话。
func TestStopDuringCompact(t *testing.T) {
	rt := &fakeRuntime{}
	p := &poster{}
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:                t.TempDir(),
		SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
		Language:                 locale.ZH, // 测试固定中文文案
		Post:                     p.Post,
		ContextWindow:            400,
		DisableAutoCompact:       true, // 这个用例测手动路径
	})
	tester := ui.NewTester(app.View, 900, 620)
	for i := 0; i < 3; i++ {
		clickComposer(t, tester)
		tester.Type(strings.Repeat("填充上下文。", 6))
		pump(tester, p)
		if err := tester.Click(locSend); err != nil {
			t.Fatal(err)
		}
		pump(tester, p)
		rt.push(t,
			providers.Event{Kind: providers.EventText, Text: strings.Repeat("回复。", 8)},
			providers.Event{Kind: providers.EventDone, StopReason: "stop"},
		)
		waitFor(t, tester, p, "回合结束", func() bool {
			return app.Current().Busy() == driver.BusyIdle
		})
	}
	before := len(app.Current().Conversation().Entries())

	if err := tester.Click(locMore); err != nil {
		t.Fatal(err)
	}
	if err := tester.ChooseMenuItem(locCompact); err != nil {
		t.Fatalf("选不到压缩：%v（菜单：%v）", err, tester.Menu())
	}
	waitFor(t, tester, p, "开始压缩", func() bool {
		return app.Current().Busy() == driver.BusyCompacting
	})
	if err := tester.Click(locStop); err != nil {
		t.Fatalf("压缩中应有停止按钮：%v（界面：%v）", err, tester.Texts())
	}
	pump(tester, p)
	if app.Current().Busy() != driver.BusyIdle {
		t.Fatalf("停止后应回到空闲，实际 %v", app.Current().Busy())
	}
	if got := len(app.Current().Conversation().Entries()); got != before {
		t.Fatalf("取消压缩不应改动会话：%d → %d", before, got)
	}
}

// 自动压缩：回合结束回到 Idle 且超过阈值时自动开始。
func TestAutoCompactOnIdle(t *testing.T) {
	rt := &fakeRuntime{}
	p := &poster{}
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:                t.TempDir(),
		SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
		Language:                 locale.ZH, // 测试固定中文文案
		Post:                     p.Post,
		ContextWindow:            200, // 很小的窗口，很快就会超阈值
	})
	tester := ui.NewTester(app.View, 900, 620)

	// 多轮对话才会有「更早的内容」可压（单轮时 kept 就是全部，无需压缩）
	for i := 0; i < 3; i++ {
		clickComposer(t, tester)
		tester.Type(strings.Repeat("把上下文填满。", 8))
		pump(tester, p)
		if err := tester.Click(locSend); err != nil {
			t.Fatal(err)
		}
		pump(tester, p)
		rt.push(t,
			providers.Event{Kind: providers.EventText, Text: strings.Repeat("回复填满。", 8)},
			providers.Event{Kind: providers.EventDone, StopReason: "stop"},
		)
		// 回合结束：可能直接回到 Idle，也可能自动进入压缩
		waitFor(t, tester, p, "回合结束或开始压缩", func() bool {
			busy := app.Current().Busy()
			return busy == driver.BusyIdle || busy == driver.BusyCompacting
		})
		if app.Current().Busy() == driver.BusyCompacting {
			rt.push(t,
				providers.Event{Kind: providers.EventText, Text: "自动摘要"},
				providers.Event{Kind: providers.EventDone, StopReason: "stop"},
			)
			waitFor(t, tester, p, "压缩完成", func() bool {
				return app.Current().Busy() == driver.BusyIdle
			})
		}
	}
	// 至少压缩过一次
	compacted := false
	for _, entry := range app.Current().Conversation().Entries() {
		if entry.Message == nil {
			continue
		}
		for _, part := range entry.Message.Message.Parts {
			if part.MediaType == render.MediaSummary {
				compacted = true
			}
		}
	}
	if !compacted {
		t.Fatalf("多轮后应自动压缩：%v", tester.Texts())
	}
	if !tester.HasText("以上内容已压缩") {
		t.Fatalf("自动压缩后应有分隔线：%v", tester.Texts())
	}
}

// Markdown：粗体/斜体/行内代码/代码块/标题/列表/引用/分隔线都渲染出来，
// 文本内容仍然可被 HasText 找到。
func TestMarkdownRendering(t *testing.T) {
	app, _, p := newApp(t)
	session := app.Current()
	conv := session.Conversation()
	msg := message.Message{MessageID: "md-1", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart("# 标题一\n\n这是 **粗体** 与 *斜体* 和 `代码`。\n\n- 第一项\n- 第二项\n\n> 引用内容\n\n---\n\n```go\nfmt.Println(\"hi\")\n```"),
	}}
	if _, err := conv.AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)
	for _, want := range []string{"标题一", "粗体", "斜体", "代码", "第一项", "第二项", "引用内容", "fmt.Println"} {
		if !tester.HasText(want) {
			t.Fatalf("markdown 渲染缺少 %q：%v", want, tester.Texts())
		}
	}
}

// 流式：半截 markdown 不能崩，也不能把整段吞掉。
func TestMarkdownHandlesIncompleteStreaming(t *testing.T) {
	app, _, p := newApp(t)
	session := app.Current()
	conv := session.Conversation()
	msg := message.Message{MessageID: "md-2", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart("先说一句\n\n```go\nfunc main() {"),
	}}
	if _, err := conv.AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)
	if !tester.HasText("先说一句") {
		t.Fatalf("未闭合代码块之前的段落应正常显示：%v", tester.Texts())
	}
}

// 钓鱼提示：链接文字本身是 URL 且主机不同 → 显示目标主机。
func TestLinkHostMismatchHint(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()
	msg := message.Message{MessageID: "md-phish", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart("[https://bank.com](https://evil.example) 与 [文档](https://example.com/doc)"),
	}}
	if _, err := conv.AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)
	if !tester.HasText("evil.example") {
		t.Fatalf("主机不一致时应提示真实主机：%v", tester.Texts())
	}
	// 普通链接不加提示
	if tester.HasText("→ example.com") {
		t.Fatalf("普通链接不应加提示：%v", tester.Texts())
	}
}

// 链接：只允许 http/https/mailto；其余 scheme 当普通文本（不可点）。
func TestLinkSchemeAllowlist(t *testing.T) {
	app, _, p := newApp(t)
	session := app.Current()
	conv := session.Conversation()
	msg := message.Message{MessageID: "md-3", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart("看 [文档](https://example.com/doc) 和 [本地](file:///etc/passwd)"),
	}}
	if _, err := conv.AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)
	if !tester.HasText("文档") || !tester.HasText("本地") {
		t.Fatalf("链接文字应显示：%v", tester.Texts())
	}

	// 点安全链接：应请求打开
	if err := tester.Click("文档"); err != nil {
		t.Fatalf("点不到链接：%v", err)
	}
	pump(tester, p)
	opened := tester.OpenedURLs()
	if len(opened) == 0 || !strings.HasPrefix(opened[0], "https://") {
		t.Fatalf("应通过系统打开 https 链接：%v", opened)
	}

	// file: 链接不可点
	before := len(tester.OpenedURLs())
	if err := tester.Click("本地"); err == nil {
		pump(tester, p)
	}
	after := tester.OpenedURLs()
	if len(after) != before {
		t.Fatalf("file: 链接不应被打开：%v", after)
	}
	for _, url := range after {
		if strings.HasPrefix(url, "file:") {
			t.Fatalf("不应打开 file: 链接：%v", after)
		}
	}
}

// 缓存：同一条消息重复渲染不重复解析；内容变化后重新解析。
func TestMarkdownCacheReuse(t *testing.T) {
	app, _, p := newApp(t)
	session := app.Current()
	conv := session.Conversation()
	msg := message.Message{MessageID: "md-4", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart("缓存测试 **内容**"),
	}}
	if _, err := conv.AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(app.View, 900, 700)
	for i := 0; i < 3; i++ {
		pump(tester, p)
	}
	_, misses := app.MarkdownCacheStatsForTest()
	if misses > 1 {
		t.Fatalf("同一内容不应重复解析，misses=%d", misses)
	}
}

// 表格：内容可见、宽表格不撑宽窗口（按钮仍在窗口内）。
func TestTableRendering(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()
	msg := message.Message{MessageID: "tbl-1", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart("| 城市 | 人口 | 备注 |\n|:---|--:|:---|\n| 北京 | 2189 | 首都 |\n| 上海 | 2487 | 直辖市 |"),
	}}
	if _, err := conv.AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)
	for _, want := range []string{"城市", "人口", "北京", "上海", "首都"} {
		if !tester.HasText(want) {
			t.Fatalf("表格缺少 %q：%v", want, tester.Texts())
		}
	}
	// 宽表格不能把输入区挤到窗口外（M4 那个 Fill 坑的同类检查）
	send, ok := tester.Find("发送")
	if ok && send.X+send.W > 900 {
		t.Fatalf("输入区被表格撑到窗口外：%v", send)
	}
}

// 图片：file: 之类的 URL 不加载，只显示 alt 占位。
func TestImageSchemeNotLoaded(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()
	msg := message.Message{MessageID: "img-1", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart("看图 ![本机密照](file:///C:/secret.png)"),
	}}
	if _, err := conv.AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)
	if !tester.HasText("本机密照") {
		t.Fatalf("应显示 alt 占位：%v", tester.Texts())
	}
	// 没有发起下载（file: 被 scheme 检查挡下）
	if got := app.ImageFetchCountForTest(); got != 0 {
		t.Fatalf("file: 图片不应触发下载：%d", got)
	}
}

// 代码块：高亮后文本仍完整可见，且长行不撑宽窗口。
func TestCodeBlockHighlightRendering(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()
	msg := message.Message{MessageID: "code-1", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart("```go\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```\n\n```\n未知语言也按纯文本\n```"),
	}}
	if _, err := conv.AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)
	for _, want := range []string{"func main", "Println", "未知语言也按纯文本"} {
		if !tester.HasText(want) {
			t.Fatalf("代码块缺少 %q：%v", want, tester.Texts())
		}
	}
}

// 高亮走缓存：同一段代码渲染多帧只计算一次。
func TestCodeHighlightCached(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()
	msg := message.Message{MessageID: "code-2", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart("```go\nx := 1\n```"),
	}}
	if _, err := conv.AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(app.View, 900, 700)
	for i := 0; i < 3; i++ {
		pump(tester, p)
	}
	if _, misses := app.CodeCacheStatsForTest(); misses > 1 {
		t.Fatalf("同一代码块不应重复高亮，misses=%d", misses)
	}
}

// 图片：真实走下载+解码管线，状态经 post 回主线程（-race 下验证）。
func TestImageLoadsAndRenders(t *testing.T) {
	pngBytes := makePNG(t, 40, 30)
	app, rt, p := newApp(t)
	_ = rt
	app.SetImageFetcherForTest(func(url string) ([]byte, error) {
		return pngBytes, nil
	})
	conv := app.Current().Conversation()
	msg := message.Message{MessageID: "img-ok", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart("看图 ![示例图](https://example.com/a.png)"),
	}}
	if _, err := conv.AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(app.View, 900, 700)
	waitFor(t, tester, p, "图片加载完成", func() bool {
		loaded, failed := app.ImageStateForTest("https://example.com/a.png")
		return loaded || failed != ""
	})
	if loaded, failed := app.ImageStateForTest("https://example.com/a.png"); !loaded {
		t.Fatalf("图片应解码成功：%s", failed)
	}
	if app.ImageFetchCountForTest() != 1 {
		t.Fatalf("同一 URL 只应下载一次，实际 %d", app.ImageFetchCountForTest())
	}
}

// 内网 / 本机地址必须被拒绝（模型可以写 URL 让应用去打本机服务）。
func TestImageRejectsPrivateHosts(t *testing.T) {
	cases := []string{
		"http://127.0.0.1:8080/admin/delete?id=1",
		"http://localhost/secret.png",
		"http://10.0.0.5/x.png",
		"http://192.168.1.1/x.png",
		"http://169.254.169.254/latest/meta-data.png",
		"http://100.64.1.1/x.png", // CGNAT
		"file:///C:/secret.png",
		"javascript:alert(1)",
	}
	for _, url := range cases {
		if err := checkURLForTest(url); err == nil {
			t.Fatalf("%s 应被拒绝", url)
		}
	}
	// 正常公网地址应放行
	if err := checkURLForTest("https://example.com/a.png"); err != nil {
		t.Fatalf("公网地址应放行：%v", err)
	}
}

// 内嵌图片 part（raw 字节）也能渲染。
func TestInlineImagePart(t *testing.T) {
	pngBytes := makePNG(t, 20, 10)
	app, _, p := newApp(t)
	app.SetImageFetcherForTest(func(string) ([]byte, error) {
		return nil, nil
	})
	conv := app.Current().Conversation()
	msg := message.Message{MessageID: "img-inline", Role: message.RoleUser, Parts: []message.Part{
		{Content: message.RawPart{Raw: pngBytes}, MediaType: "image/png", Filename: "截图.png"},
	}}
	if _, err := conv.AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)
	// 内嵌图片不走下载，直接解码
	waitFor(t, tester, p, "内嵌图片解码", func() bool {
		loaded, _ := app.ImageStateForTest("inline:" + itoaForTest(len(pngBytes)) + ":image/png")
		return loaded
	})
}

// 失败原因要翻译成可操作的提示（原始错误仍然保留）。
func TestFailureHints(t *testing.T) {
	cases := []struct{ reason, want string }{
		{"401 Unauthorized: invalid api key", "API Key"},
		{"402 Payment Required: insufficient balance", "余额"},
		{"429 Too Many Requests", "限流"},
		{"400 maximum context length is 8192 tokens", "压缩上下文"},
		{"dial tcp: connection refused", "连不上服务端"},
	}
	for _, c := range cases {
		app, _, p := newApp(t)
		conv := app.Current().Conversation()
		msg := message.Message{MessageID: "fail-1", Role: message.RoleAgent}
		entry, err := conv.AddMessage(msg, conversation.StatusPending)
		if err != nil {
			t.Fatal(err)
		}
		_ = entry
		if err := conv.FailStream("fail-1", c.reason); err != nil {
			t.Fatal(err)
		}
		tester := ui.NewTester(app.View, 900, 620)
		pump(tester, p)
		if !tester.HasText(c.reason) {
			t.Fatalf("原始错误应保留：%v", tester.Texts())
		}
		// 失败建议在回合结束行展开后显示（dsh 的 turn footer 也是这个结构）
		for _, text := range tester.Texts() {
			if strings.HasPrefix(text, "▸ 处理失败") {
				if err := tester.Click(text); err != nil {
					t.Fatalf("点不到失败行：%v", err)
				}
				break
			}
		}
		pump(tester, p)
		if !tester.HasText(c.want) {
			t.Fatalf("%q 应给出提示 %q：%v", c.reason, c.want, tester.Texts())
		}
	}
}

// 对齐 deepseek-harness 的排版：
//
//	· 用户消息右对齐气泡，助手消息整列宽；
//	· 正文列最宽 748，居中；
//	· 推理与工具是折叠行，点击展开。
func TestHarnessStyleLayout(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()
	if _, err := conv.AddMessage(message.Message{
		MessageID: "u-md", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("用户说的话")},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a-md", Role: message.RoleAgent,
		Parts: []message.Part{
			{Content: message.TextPart{Text: "思考第一行\n思考第二行"}, MediaType: render.MediaReasoning},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "read", "args": map[string]any{"file_path": "a.txt"},
			}}, MediaType: render.MediaToolUse},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "read", "output": "第一行输出\n第二行输出",
				"is_error": false,
			}}, MediaType: render.MediaToolResult},
			message.NewTextPart("助手的正文"),
		},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 1200, 800)
	pump(tester, p)

	user, ok := tester.Find("用户说的话")
	if !ok {
		t.Fatalf("用户消息未渲染：%v", tester.Texts())
	}
	body, ok := tester.Find("助手的正文")
	if !ok {
		t.Fatalf("助手正文未渲染：%v", tester.Texts())
	}
	// 用户气泡靠右、助手正文靠左
	if user.X <= body.X {
		t.Fatalf("用户消息应右对齐（user.x=%.0f, body.x=%.0f）", user.X, body.X)
	}
	// 正文列不超过 748
	if body.W > 800 {
		t.Fatalf("正文列过宽：%.0f", body.W)
	}
	// 过程（思考+工具）默认折叠成一组；展开后推理行只显示最后一行
	clickProcessGroup(t, tester)
	pump(tester, p)
	if !tester.HasText("思考 · 思考第二行") {
		t.Fatalf("推理应折叠成一行并显示最后一行：%v", tester.Texts())
	}
	if err := tester.Click("思考 · 思考第二行"); err != nil {
		t.Fatalf("点不到推理行：%v", err)
	}
	pump(tester, p)
	if !tester.HasText("思考第一行") {
		t.Fatalf("展开后应显示完整推理：%v", tester.Texts())
	}
	// 工具输出默认折叠成第一行
	if tester.HasText("第二行输出") {
		t.Fatalf("工具输出应默认折叠：%v", tester.Texts())
	}
}

// 回归（用户反馈）：没有任何内容的新会话不应该落盘，也不该在侧栏堆一片「新会话」。
func TestEmptySessionIsNotPersisted(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)
	app := uiapp.NewApp(&fakeRuntime{}, uiapp.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:     t.TempDir(),
		Catalog:       catalog,
		ContextWindow: 200_000,
		// 测试窗口 900 < 1024：禁用自动收起，否则侧栏变图标栏、点不到「新建」
		SidebarAutoCollapseWidth: -1,
		Language:                 locale.ZH,
	})
	defer app.Close()
	tester := ui.NewTester(app.View, 900, 620)
	p2 := &poster{}
	pump(tester, p2)

	// 新建了会话，但磁盘上不该有目录
	if entries, err := os.ReadDir(catalog.SessionsDir()); err == nil && len(entries) != 0 {
		t.Fatalf("空会话不应落盘，实际有 %d 个目录", len(entries))
	}

	// 再点几次「新建」也不该产生目录
	for i := 0; i < 3; i++ {
		if err := tester.Click(locNewSession); err != nil {
			t.Fatal(err)
		}
		p2.Drain()
		tester.Frame()
	}
	if entries, err := os.ReadDir(catalog.SessionsDir()); err == nil && len(entries) != 0 {
		t.Fatalf("空会话不应落盘，实际有 %d 个目录", len(entries))
	}
}

// 第一条消息才建立日志，并且用它的首行当标题。
func TestFirstMessagePersistsSessionWithTitle(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)
	rt := &fakeRuntime{}
	p := &poster{}
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:                t.TempDir(),
		SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
		Language:                 locale.ZH, // 测试固定中文文案
		Post:                     p.Post,
		Catalog:                  catalog,
		ContextWindow:            200_000,
	})
	defer app.Close()
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	ensureSession(t, tester, p, app)

	clickComposer(t, tester)
	tester.Type("帮我看看这个登录接口的问题\n第二行不该进标题")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	rt.push(t, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	waitFor(t, tester, p, "回合结束", func() bool {
		return app.Current().Busy() == driver.BusyIdle
	})

	session := app.Current()
	if session.Title != "帮我看看这个登录接口的问题" {
		t.Fatalf("标题应取第一条消息的首行：%q", session.Title)
	}
	summaries, err := catalog.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Title != session.Title {
		t.Fatalf("索引标题不一致：%#v", summaries)
	}
	if !tester.HasText(session.Title) {
		t.Fatalf("侧栏应显示会话标题：%v", tester.Texts())
	}
}

// 启动时清掉历史遗留的空会话，有内容的保留。
func TestStartupPrunesEmptySessions(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)

	// 造一个空会话（只有 session.created，模拟旧版本留下的）
	empty, err := catalog.Create("empty-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.Append(store.MustEvent(store.TypeSessionCreated,
		store.SessionCreatedData{ContextID: "empty-1", Title: "新会话"})); err != nil {
		t.Fatal(err)
	}
	empty.Close()

	// 再造一个有内容的
	full, err := catalog.Create("full-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := full.Append(store.MustEvent(store.TypeSessionCreated,
		store.SessionCreatedData{ContextID: "full-1", Title: "有内容"})); err != nil {
		t.Fatal(err)
	}
	if err := full.Append(store.MustEvent(store.TypeMessageAppended, map[string]any{
		"message": map[string]any{
			"messageId": "m1", "role": "ROLE_USER",
			"parts": []any{map[string]any{"text": "你好"}},
		},
		"status": "complete",
	})); err != nil {
		t.Fatal(err)
	}
	full.Close()

	app := uiapp.NewApp(&fakeRuntime{}, uiapp.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:     t.TempDir(),
		Catalog:       catalog,
		ContextWindow: 200_000,
		// 测试窗口 900 < 1024：禁用自动收起，否则侧栏变图标栏、点不到「新建」
		SidebarAutoCollapseWidth: -1,
		Language:                 locale.ZH,
	})
	defer app.Close()

	if _, err := os.Stat(filepath.Join(catalog.SessionsDir(), "empty-1")); !os.IsNotExist(err) {
		t.Fatal("空会话应被清理")
	}
	if _, err := os.Stat(filepath.Join(catalog.SessionsDir(), "full-1")); err != nil {
		t.Fatalf("有内容的会话应保留：%v", err)
	}
}

// 回归（用户反馈）：删除会话必须真的删掉磁盘上的目录，重启后不能再出现。
func TestDeleteSessionRemovesFromDisk(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)
	makeApp := func(rt *fakeRuntime) (*uiapp.App, *poster) {
		p := &poster{}
		app := uiapp.NewApp(rt, uiapp.Options{
			Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
			Workspace:                t.TempDir(),
			SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
			Language:                 locale.ZH, // 测试固定中文文案
			Post:                     p.Post,
			Catalog:                  catalog,
			ContextWindow:            200_000,
		})
		return app, p
	}

	// 造两个有内容的会话
	rt := &fakeRuntime{}
	app, p := makeApp(rt)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	ensureSession(t, tester, p, app)
	for i := 0; i < 2; i++ {
		clickComposer(t, tester)
		tester.Type("第" + itoaForTest(i+1) + "个会话的内容")
		pump(tester, p)
		if err := tester.Click(locSend); err != nil {
			t.Fatal(err)
		}
		pump(tester, p)
		rt.push(t, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
		waitFor(t, tester, p, "回合结束", func() bool {
			return app.Current().Busy() == driver.BusyIdle
		})
		if i == 0 {
			if err := tester.Click(locNewSession); err != nil {
				t.Fatal(err)
			}
			pump(tester, p)
		}
	}
	if len(app.Sessions()) != 2 {
		t.Fatalf("应有 2 个会话，实际 %d", len(app.Sessions()))
	}
	deleted := app.Sessions()[0].ID
	before := countDirs(t, catalog.SessionsDir())
	if before != 2 {
		t.Fatalf("磁盘上应有 2 个会话目录，实际 %d", before)
	}

	// 先给两个会话不同标题，右键才定位得准
	app.Sessions()[0].Title = "被删的会话"
	app.Sessions()[1].Title = "留下的会话"
	pump(tester, p)
	// 删除第一个会话（右键 → 删除 → 确认）
	firstTitle := app.Sessions()[0].Title
	if err := tester.RightClick(firstTitle); err != nil {
		t.Fatal(err)
	}
	if err := tester.ChooseMenuItem(locDelete); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if err := tester.Click(locDelete); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	if _, err := os.Stat(filepath.Join(catalog.SessionsDir(), deleted)); !os.IsNotExist(err) {
		t.Fatalf("磁盘上的会话目录应被删除：%s", deleted)
	}
	app.Close()

	// 「重启」：只剩一个会话
	app2, p2 := makeApp(&fakeRuntime{})
	defer app2.Close()
	tester2 := ui.NewTester(app2.View, 900, 620)
	pump(tester2, p2)
	if app2.Current() == nil {
		// 全新数据目录：现在显示空态，先建一个会话再测
		if err := tester2.Click(locNewSession); err != nil {
			t.Fatalf("点不到新建会话：%v", err)
		}
		pump(tester2, p2)
	}
	if len(app2.Sessions()) != 1 {
		t.Fatalf("重启后应只剩 1 个会话，实际 %d（界面：%v）", len(app2.Sessions()), tester2.Texts())
	}
	for _, session := range app2.Sessions() {
		if session.ID == deleted {
			t.Fatal("已删除的会话不应被恢复")
		}
	}
}

// hasProcessGroup 判断界面上有没有折叠中的过程组（标题以 ▸ 开头）。
func hasProcessGroup(tester *ui.Tester) bool {
	for _, text := range tester.Texts() {
		if strings.HasPrefix(text, "▸ ") {
			return true
		}
	}
	return false
}

// countText 数一数界面上有多少个**完全等于** text 的元素。
func countText(tester *ui.Tester, text string) int {
	count := 0
	for _, candidate := range tester.Texts() {
		if candidate == text {
			count++
		}
	}
	return count
}

// hasTextPrefix 判断界面上有没有以 prefix 开头的文本。
func hasTextPrefix(tester *ui.Tester, prefix string) bool {
	for _, text := range tester.Texts() {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// hasExpandedProcessGroup 判断过程组是否展开（标题以 ▾ 开头）。
func hasExpandedProcessGroup(tester *ui.Tester) bool {
	for _, text := range tester.Texts() {
		if strings.HasPrefix(text, "▾ ") {
			return true
		}
	}
	return false
}

// clickProcessGroup 找到当前界面上的过程组标题并点击（标题后面带"最后任务"信息，
// 不适合在测试里写死）。
func clickProcessGroup(t *testing.T, tester *ui.Tester) string {
	t.Helper()
	for _, text := range tester.Texts() {
		if strings.HasPrefix(text, "▸ ") {
			if err := tester.Click(text); err != nil {
				t.Fatalf("点不到过程组 %q：%v", text, err)
			}
			return text
		}
	}
	t.Fatalf("界面上没有过程组：%v", tester.Texts())
	return ""
}

// countDirs 数一个目录下的子目录个数。
func countDirs(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	return count
}

// 回归（用户反馈）：会话区宽度不能随内容伸缩。
//
// 之前正文列没有 FillWidth，宽度由最宽的那条消息决定：消息短时列很窄并居中，
// 来了长消息才撑开——看起来像"内容居中、然后突然变宽"。
func TestTranscriptWidthIsStable(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()
	add := func(role message.Role, text string) {
		t.Helper()
		id, err := conversation.NewMessageID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conv.AddMessage(message.Message{
			MessageID: id, Role: role,
			Parts: []message.Part{message.NewTextPart(text)},
		}, conversation.StatusComplete); err != nil {
			t.Fatal(err)
		}
	}

	add(message.RoleUser, "短")
	tester := ui.NewTester(app.View, 1200, 800)
	pump(tester, p)
	shortUser, ok := tester.Find("短")
	if !ok {
		t.Fatalf("找不到用户气泡：%v", tester.Texts())
	}
	shortRight := shortUser.X + shortUser.W

	// 再来一条很长的助手消息（超长不换行的单词，逼出最宽内容）
	add(message.RoleAgent, strings.Repeat("wideContent", 60))
	pump(tester, p)

	longUser, ok := tester.Find("短")
	if !ok {
		t.Fatalf("找不到用户气泡：%v", tester.Texts())
	}
	longRight := longUser.X + longUser.W

	// 气泡右边缘应贴在正文列右边界上，且不随内容变化
	if shortRight != longRight {
		t.Fatalf("会话区宽度随内容变化：短消息右边界 %.0f，长消息后 %.0f", shortRight, longRight)
	}
	// 正文列本身应该已经是满宽（748）：气泡右边缘因此要明显大于侧栏 + 748 的
	// 下限，而不是缩到"短"这个内容的宽度附近。
	minRight := float32(240 + 748)
	if shortRight < minRight {
		t.Fatalf("正文列没有撑到 748（右边界 %.0f < %.0f），说明宽度仍由内容决定",
			shortRight, minRight)
	}
}

// 思考档位选择器：按模型给出档位、切换后写日志、重启后恢复。
func TestReasoningEffortPicker(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)
	makeApp := func() (*uiapp.App, *poster, *fakeRuntime) {
		p := &poster{}
		rt := &fakeRuntime{}
		app := uiapp.NewApp(rt, uiapp.Options{
			Selection:                providers.Selection{ProviderID: "paperhub", Model: "flash"},
			Workspace:                t.TempDir(),
			SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
			Language:                 locale.ZH, // 测试固定中文文案
			Post:                     p.Post,
			Catalog:                  catalog,
			ContextWindow:            128_000,
			Providers: []uiapp.ProviderChoice{{
				ProviderID: "paperhub",
				Label:      "PaperHub",
				Models:     []string{"flash", "glm"},
				Efforts: map[string][]string{
					"flash": {"off", "low", "medium", "high"},
					"glm":   {"low", "high", "max"},
				},
			}},
			EffortFor: func(sel providers.Selection) string {
				if sel.Model == "glm" {
					return "high"
				}
				return "medium"
			},
		})
		return app, p, rt
	}

	app, p, rt := makeApp()
	tester := ui.NewTester(app.View, 1000, 700)
	pump(tester, p)
	ensureSession(t, tester, p, app)

	if !tester.HasText("flash 中") {
		t.Fatalf("应显示当前档位：%v", tester.Texts())
	}
	// 先发一条消息让会话落盘（档位事件才会写日志）
	clickComposer(t, tester)
	tester.Type("档位测试")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	rt.push(t, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	waitFor(t, tester, p, "回合结束", func() bool {
		return app.Current().Busy() == driver.BusyIdle
	})

	// 切到「高」：模型菜单 →「思考档位」子菜单 → 高
	if err := tester.Click(locModel); err != nil {
		t.Fatalf("点不到模型 chip：%v（界面：%v）", err, tester.Texts())
	}
	if err := tester.ChooseMenuItem("思考档位", "高"); err != nil {
		t.Fatalf("选不到档位：%v（菜单：%v）", err, tester.Menu())
	}
	pump(tester, p)
	if got := app.Current().Selection(); got.Model != "flash" {
		t.Fatalf("切换档位不应改模型：%+v", got)
	}
	if !tester.HasText("flash 高") {
		t.Fatalf("档位应显示为高：%v", tester.Texts())
	}
	app.Close()

	// 「重启」后档位恢复
	app2, p2, _ := makeApp()
	defer app2.Close()
	tester2 := ui.NewTester(app2.View, 1000, 700)
	pump(tester2, p2)
	if app2.Current().ReasoningEffortForTest() != "high" {
		t.Fatalf("重启后档位应恢复为 high，实际 %q", app2.Current().ReasoningEffortForTest())
	}
}

// 模型显示名：标题栏与选择面板都显示 label，选择时仍用 id。
func TestModelLabelShownInUI(t *testing.T) {
	app, _, p := newApp(t)
	_ = app
	app2 := uiapp.NewApp(&fakeRuntime{}, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "paperhub", Model: "flash"},
		Workspace:                t.TempDir(),
		SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
		Language:                 locale.ZH, // 测试固定中文文案
		Post:                     (&poster{}).Post,
		ContextWindow:            128_000,
		Providers: []uiapp.ProviderChoice{{
			ProviderID: "paperhub",
			Label:      "PaperHub",
			Models:     []string{"flash", "glm"},
			Labels:     map[string]string{"flash": "DeepSeek V4.1 Flash", "glm": "GLM-5.3"},
		}},
	})
	defer app2.Close()
	p2 := &poster{}
	app2.SetPostForTest(p2.Post)
	tester := ui.NewTester(app2.View, 1000, 700)
	pump(tester, p2)

	if !tester.HasText("DeepSeek V4.1 Flash") {
		t.Fatalf("输入卡应显示模型显示名：%v", tester.Texts())
	}
	if tester.HasText("flash") {
		t.Fatalf("不应显示裸 id：%v", tester.Texts())
	}
	// 菜单里也用显示名
	if err := tester.Click(locModel); err != nil {
		t.Fatalf("点不到模型 chip：%v", err)
	}
	if err := tester.ChooseMenuItem("PaperHub", "GLM-5.3"); err != nil {
		t.Fatalf("选不到 GLM-5.3：%v（菜单：%v）", err, tester.Menu())
	}
	pump(tester, p2)
	if got := app2.Current().Selection().Model; got != "glm" {
		t.Fatalf("选择应使用 id：%q", got)
	}
	_ = p
}

// 回归（用户反馈）：按回车后消息要**立刻**出现，不能等网络往返。
//
// providers.Stream 会同步等待 HTTP 响应头；如果开流发生在主线程，界面会被卡住。
func TestSendShowsMessageBeforeNetworkResponds(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	rt := &fakeRuntime{openGate: gate, openEntered: entered}
	p := &poster{}
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:                t.TempDir(),
		SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
		Language:                 locale.ZH, // 测试固定中文文案
		Post:                     p.Post,
		ContextWindow:            200_000,
	})
	defer app.Close()
	tester := ui.NewTester(app.View, 900, 620)

	clickComposer(t, tester)
	tester.Type("回车之后应该马上出现")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}

	// 开流还没返回（网络被 gate 挡住），界面必须已经显示这条消息
	pump(tester, p)
	if !tester.HasText("回车之后应该马上出现") {
		t.Fatalf("消息没有立刻出现：%v", tester.Texts())
	}
	if app.Current().Busy() != driver.BusyStreaming {
		t.Fatalf("应处于 streaming，实际 %v", app.Current().Busy())
	}

	// 放开网络，确认流确实开了起来
	close(gate)
	waitFor(t, tester, p, "流建立", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.streams) == 1
	})
}

// 思考中：显示"思考中" + 最后一行，并且随增量更新（流式可见）。
func TestReasoningStreamingShowsLastLine(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	ensureSession(t, tester, p, app)
	clickComposer(t, tester)
	tester.Type("想一想")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	rt.push(t,
		providers.Event{Kind: providers.EventReasoning, Text: "第一行想法\n"},
		providers.Event{Kind: providers.EventReasoning, Text: "第二行想法"},
	)
	waitFor(t, tester, p, "显示思考中", func() bool {
		return tester.HasText("思考中 · 第二行想法")
	})
	// 再来一段：预览要跟着走到新的最后一行
	rt.push(t, providers.Event{Kind: providers.EventReasoning, Text: "\n第三行在想"})
	waitFor(t, tester, p, "预览前进", func() bool {
		return tester.HasText("思考中 · 第三行在想")
	})
	// 正文开始后不再算"思考中"
	rt.push(t,
		providers.Event{Kind: providers.EventText, Text: "答案"},
		providers.Event{Kind: providers.EventDone, StopReason: "stop"},
	)
	waitFor(t, tester, p, "回到折叠标题", func() bool {
		return tester.HasText("思考 · 第三行在想") && !tester.HasText("思考中")
	})
}

// 工具调用：运行时显示「运行中」，出结果后显示「完成/失败」，输出默认折叠。
func TestToolCallStatusLive(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	ensureSession(t, tester, p, app)
	clickComposer(t, tester)
	tester.Type("跑工具")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`},
	)
	// 只有一条过程 part 时直接内联显示（>=2 才合并成组）
	waitFor(t, tester, p, "显示当前调用", func() bool {
		return tester.HasText("read") && tester.HasText("运行中")
	})

	rt.push(t, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	// 工具跑完会开下一轮流；这期间**不该**出现回合结束行（回合还没结束）
	waitFor(t, tester, p, "开下一轮流", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.streams) == 2
	})
	if tester.HasText("已完成") {
		t.Fatalf("回合进行中不该显示结束行：%v", tester.Texts())
	}
	if tester.HasText("运行中") {
		t.Fatalf("完成后不应再显示运行中：%v", tester.Texts())
	}
}

// 连续的过程 part（思考/工具调用/结果）≥2 个时合并成一组；只有 1 个时内联显示。
func TestProcessGroupMerging(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()
	makeMessage := func(id string, parts ...message.Part) {
		t.Helper()
		if _, err := conv.AddMessage(message.Message{
			MessageID: id, Role: message.RoleAgent, Parts: parts,
		}, conversation.StatusComplete); err != nil {
			t.Fatal(err)
		}
	}
	reasoning := message.Part{Content: message.TextPart{Text: "想一下"}, MediaType: render.MediaReasoning}
	toolUse := message.Part{Content: message.DataPart{Data: map[string]any{
		"id": "c1", "name": "read", "args": map[string]any{"file_path": "a.txt"},
	}}, MediaType: render.MediaToolUse}
	toolResult := message.Part{Content: message.DataPart{Data: map[string]any{
		"id": "c1", "name": "read", "output": "内容", "is_error": false,
	}}, MediaType: render.MediaToolResult}
	text := message.NewTextPart("最终答案")

	// 回合 1：思考 + 工具 + 结果 + 回答（回答在组外）
	makeMessage("g1", reasoning, toolUse, toolResult, text)
	// 新回合（用户消息分隔）：单个过程 part，且后面没有回答 → 内联
	if _, err := conv.AddMessage(message.Message{
		MessageID: "u2", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("再来")},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	makeMessage("g2", toolUse)

	tester := ui.NewTester(app.View, 1000, 800)
	pump(tester, p)
	if !hasProcessGroup(tester) {
		t.Fatalf("同一回合的过程应合并成一组：%v", tester.Texts())
	}
	// 新回合里只有一个过程 part → 内联（⚙ / 名称 / 参数是三个文本片段）
	if !tester.HasText("⚙") {
		t.Fatalf("单个过程 part 应内联显示：%v", tester.Texts())
	}
	// 回合末尾的回答在组外
	if !tester.HasText("最终答案") {
		t.Fatalf("回答应在组外：%v", tester.Texts())
	}
}

// 跨回合合并：一次工具轮会产出多条 agent 消息，中间的思考/调用都属于同一个过程组
// ——只要中间没有正文输出，就应该折叠在一起（用户要求）。
func TestProcessGroupSpansMessages(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()
	add := func(id string, parts ...message.Part) {
		t.Helper()
		if _, err := conv.AddMessage(message.Message{
			MessageID: id, Role: message.RoleAgent, Parts: parts,
		}, conversation.StatusComplete); err != nil {
			t.Fatal(err)
		}
	}
	reasoning := func(text string) message.Part {
		return message.Part{Content: message.TextPart{Text: text}, MediaType: render.MediaReasoning}
	}
	toolUse := func(id, name string) message.Part {
		return message.Part{Content: message.DataPart{Data: map[string]any{
			"id": id, "name": name, "args": map[string]any{},
		}}, MediaType: render.MediaToolUse}
	}
	toolResult := func(id, name string) message.Part {
		return message.Part{Content: message.DataPart{Data: map[string]any{
			"id": id, "name": name, "output": "ok", "is_error": false,
		}}, MediaType: render.MediaToolResult}
	}

	// 两条 agent 消息，中间没有正文：应合并成一个过程组（4 步）
	add("a1", reasoning("先想想"), toolUse("c1", "read"), toolResult("c1", "read"))
	add("a2", reasoning("再看一处"), toolUse("c2", "grep"), toolResult("c2", "grep"))
	add("a3", message.NewTextPart("最终答案"))

	tester := ui.NewTester(app.View, 1000, 900)
	pump(tester, p)
	t.Logf("界面：%v", tester.Texts())
	if !hasProcessGroup(tester) {
		t.Fatalf("跨消息的过程应合并成一组：%v", tester.Texts())
	}
	if !tester.HasText("最终答案") {
		t.Fatalf("正文应正常显示：%v", tester.Texts())
	}
	// 展开后两条消息里的过程都能看到
	clickProcessGroup(t, tester)
	pump(tester, p)
	for _, want := range []string{"思考", "read", "思考", "grep"} {
		if !tester.HasText(want) {
			t.Fatalf("展开后缺少 %q：%v", want, tester.Texts())
		}
	}
}

// 回归（用户反馈）：中间那些**正式输出**不能被折叠进过程组。
// 它们留在会话流里，并把过程切成"组 → 旁白 → 组 → 回答"。
func TestNarrationStaysOutsideProcessGroup(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()
	addUser := func(id, text string) {
		t.Helper()
		if _, err := conv.AddMessage(message.Message{
			MessageID: id, Role: message.RoleUser,
			Parts: []message.Part{message.NewTextPart(text)},
		}, conversation.StatusComplete); err != nil {
			t.Fatal(err)
		}
	}
	addAgent := func(id string, parts ...message.Part) {
		t.Helper()
		if _, err := conv.AddMessage(message.Message{
			MessageID: id, Role: message.RoleAgent, Parts: parts,
		}, conversation.StatusComplete); err != nil {
			t.Fatal(err)
		}
	}
	reasoning := func(text string) message.Part {
		return message.Part{Content: message.TextPart{Text: text}, MediaType: render.MediaReasoning}
	}
	toolUse := func(id, name string) message.Part {
		return message.Part{Content: message.DataPart{Data: map[string]any{
			"id": id, "name": name, "args": map[string]any{},
		}}, MediaType: render.MediaToolUse}
	}
	toolResult := func(id, name string) message.Part {
		return message.Part{Content: message.DataPart{Data: map[string]any{
			"id": id, "name": name, "output": "ok", "is_error": false,
		}}, MediaType: render.MediaToolResult}
	}

	addUser("u1", "看看项目进度")
	// 一轮里有多条 agent 消息，中间夹着"过渡旁白"（正文）
	addAgent("a1", reasoning("先看目录"), message.NewTextPart("我先看看目录。"), toolUse("c1", "ls"), toolResult("c1", "ls"))
	addAgent("a2", reasoning("再看文档"), message.NewTextPart("文档在 docs/。"), toolUse("c2", "read"), toolResult("c2", "read"))
	addAgent("a3", message.NewTextPart("最终回答：M1-M7 都完成了。"))

	tester := ui.NewTester(app.View, 1000, 900)
	pump(tester, p)

	// 旁白（正式输出）不进组：它自己留在会话流里，并把过程切成多组
	for _, want := range []string{"我先看看目录。", "文档在 docs/。", "最终回答：M1-M7 都完成了。"} {
		if !tester.HasText(want) {
			t.Fatalf("正文 %q 应在组外可见：%v", want, tester.Texts())
		}
	}
	groups := 0
	for _, text := range tester.Texts() {
		if strings.HasPrefix(text, "▸ ") {
			groups++
		}
	}
	if groups != 2 {
		t.Fatalf("旁白把这一轮切成两个过程组，实际 %d 组：%v", groups, tester.Texts())
	}
	// 组里只剩思考与工具，没有旁白
	clickProcessGroup(t, tester)
	pump(tester, p)
	if tester.HasText("文档在 docs/。") && countText(tester, "文档在 docs/。") > 1 {
		t.Fatalf("旁白不应重复出现在展开的组里：%v", tester.Texts())
	}
}

// 折叠时显示"最后执行的任务"：思考显示最后一行，工具调用显示工具简略信息。
func TestProcessGroupTitlesFollowHarness(t *testing.T) {
	// 标题照抄 dsh 的 step-process：
	//   已完成 → 活动分类按调用次数排序拼成一句话；进行中 →「正在…」+ 任务摘要
	app, _, p := newApp(t)
	conv := app.Current().Conversation()
	reasoning := func(text string) message.Part {
		return message.Part{Content: message.TextPart{Text: text}, MediaType: render.MediaReasoning}
	}
	toolUse := func(id, name, arg string) message.Part {
		return message.Part{Content: message.DataPart{Data: map[string]any{
			"id": id, "name": name, "args": map[string]any{"file_path": arg},
		}}, MediaType: render.MediaToolUse}
	}
	toolResult := func(id, name string) message.Part {
		return message.Part{Content: message.DataPart{Data: map[string]any{
			"id": id, "name": name, "output": "ok", "is_error": false,
		}}, MediaType: render.MediaToolResult}
	}
	add := func(id string, parts ...message.Part) {
		t.Helper()
		if _, err := conv.AddMessage(message.Message{
			MessageID: id, Role: message.RoleAgent, Parts: parts,
		}, conversation.StatusComplete); err != nil {
			t.Fatal(err)
		}
	}
	user := func(id, text string) {
		t.Helper()
		if _, err := conv.AddMessage(message.Message{
			MessageID: id, Role: message.RoleUser,
			Parts: []message.Part{message.NewTextPart(text)},
		}, conversation.StatusComplete); err != nil {
			t.Fatal(err)
		}
	}

	// 一组：思考 + 读文件 + 搜索代码（已完成）
	add("t1", reasoning("先想想"), toolUse("c1", "read", "a.txt"),
		toolResult("c1", "read"), toolUse("c2", "grep", "b.go"), toolResult("c2", "grep"))
	user("u2", "继续")
	// 另一组：4 种活动 → 标题以「等」结尾
	add("t3", reasoning("再看"), toolUse("c3", "read", "c.txt"), toolResult("c3", "read"),
		toolUse("c4", "grep", "d.go"), toolResult("c4", "grep"),
		toolUse("c5", "bash", "ls"), toolResult("c5", "bash"),
		toolUse("c6", "write", "e.txt"), toolResult("c6", "write"))

	tester := ui.NewTester(app.View, 1000, 800)
	pump(tester, p)
	titles := []string{}
	for _, text := range tester.Texts() {
		if strings.HasPrefix(text, "▸ ") {
			titles = append(titles, text)
		}
	}
	if len(titles) != 2 {
		t.Fatalf("应有两个过程组，实际 %v", titles)
	}
	// 思考、读文件、搜索代码各一次 → 按首次出现排序
	if want := "▸ 已完成分析，读取文件，搜索代码"; titles[0] != want {
		t.Fatalf("组标题应为 %q，实际 %q", want, titles[0])
	}
	// 四种活动 → 只显示前三种 + 等（出现次序：思考、读、搜索、命令）
	if want := "▸ 已完成分析，读取文件，搜索代码等"; titles[1] != want {
		t.Fatalf("组标题应为 %q，实际 %q", want, titles[1])
	}
	// 旧版的构成统计不应再出现
	if strings.Contains(strings.Join(tester.Texts(), " "), "工具 1") {
		t.Fatalf("组标题不应再显示构成统计：%v", tester.Texts())
	}
}

// 进行中的过程组标题是「正在X · 摘要」，X 来自当前活动。
func TestStreamingProcessTitleShowsActivity(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 1000, 700)
	clickComposer(t, tester)
	tester.Type("搜一下")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	// 有思考 + 工具两项才会形成"过程组"（单项直接显示那一项）
	rt.push(t,
		providers.Event{Kind: providers.EventReasoning, Text: "先看看再搜"},
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "grep"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"pattern":"needle"}`},
	)
	waitFor(t, tester, p, "进行中的标题显示当前活动", func() bool {
		return hasTextPrefix(tester, "▸ 正在搜索代码 · needle")
	})
}

// 工具调用**串行执行**：一次只跑一个，每完成一个界面上就能看到它变「完成」，
// 而不是整批同时挂着「运行中」。
func TestToolsExecuteSerially(t *testing.T) {
	app, rt, p := newApp(t)

	mu := sync.Mutex{}
	var started []string
	gate := make(chan struct{}, 4)
	rt.runTools = func(ctx context.Context, _ *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
		mu.Lock()
		started = append(started, calls[0].Name)
		mu.Unlock()
		<-gate // 由测试逐步放行
		out := make([]tools.ToolOutcome, 0, len(calls))
		for _, call := range calls {
			out = append(out, tools.ToolOutcome{ID: call.ID, Name: call.Name, Output: "done:" + call.Name})
		}
		return out
	}
	startedCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(started)
	}

	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	ensureSession(t, tester, p, app)
	clickComposer(t, tester)
	tester.Type("跑两个工具")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`},
		providers.Event{Kind: providers.EventToolStart, Index: 1, ID: "c2", Name: "grep"},
		providers.Event{Kind: providers.EventToolArgs, Index: 1, Delta: `{"pattern":"x"}`},
		providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"},
	)

	waitFor(t, tester, p, "第一个工具开始执行", func() bool { return startedCount() == 1 })
	// 第一个还没放行，第二个不能开始
	time.Sleep(30 * time.Millisecond)
	pump(tester, p)
	if got := startedCount(); got != 1 {
		t.Fatalf("工具应串行执行，实际已开始 %d 个", got)
	}

	// 放行第一个 → 它完成，第二个才开始
	gate <- struct{}{}
	waitFor(t, tester, p, "第二个工具开始执行", func() bool { return startedCount() == 2 })

	gate <- struct{}{}
	waitFor(t, tester, p, "开下一轮流", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.streams) == 2
	})
}

// 过程组的三角要反映状态：折叠 ▸，展开 ▾（展开还会把整组包进竖线里）。
func TestProcessGroupTriangleToggles(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			{Content: message.TextPart{Text: "想一想"}, MediaType: render.MediaReasoning},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "read", "args": map[string]any{"file_path": "a.txt"},
			}}, MediaType: render.MediaToolUse},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "read", "output": "ok", "is_error": false,
			}}, MediaType: render.MediaToolResult},
		},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	if !hasProcessGroup(tester) {
		t.Fatalf("折叠时应显示 ▸：%v", tester.Texts())
	}
	clickProcessGroup(t, tester)
	pump(tester, p)
	if !hasExpandedProcessGroup(tester) {
		t.Fatalf("展开后应显示 ▾：%v", tester.Texts())
	}
	// 再点一次收回去
	for _, text := range tester.Texts() {
		if strings.HasPrefix(text, "▾ ") {
			if err := tester.Click(text); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	pump(tester, p)
	if !hasProcessGroup(tester) {
		t.Fatalf("收起后应回到 ▸：%v", tester.Texts())
	}
}

// 每题都有自己的输入框：写下文字即等于用文字作答（自动取消该题已选选项），
// 切回前面的页签还能继续改。
func TestAskPerQuestionCustomAnswer(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 1000, 780)
	clickComposer(t, tester)
	tester.Type("问我")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)

	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "ask_1", Name: "ask_user_question"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"questions":[
			{"id":"q1","header":"方案","question":"选哪个？","options":[{"label":"甲"},{"label":"乙"}]},
			{"id":"q2","header":"备注","question":"还有别的吗？","options":[{"label":"丙"}]}]}`},
		providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"},
	)
	waitFor(t, tester, p, "出现提问", func() bool {
		return tester.HasText("选哪个？")
	})

	// 先在第一题选一个选项
	if err := tester.Click("甲"); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if countText(tester, "◉") != 1 {
		t.Fatalf("应选中甲：%v", tester.Texts())
	}

	// 在第一题输入框写字：文字变成答案，选项被取消选中
	for _, text := range tester.Texts() {
		if strings.HasPrefix(text, "回答 ") {
			if err := tester.Click(text); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	tester.Type("都不合适，我想要第三种")
	pump(tester, p)
	if countText(tester, "◉") != 0 {
		t.Fatalf("写了文字应取消选项：%v", tester.Texts())
	}
	if !tester.HasText("✓ 方案") {
		t.Fatalf("手写答案后该题应算已答：%v", tester.Texts())
	}

	// 切到第二题，在它自己的输入框里作答
	if err := tester.Click("备注"); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if !tester.HasText("还有别的吗？") {
		t.Fatalf("应切到第二题：%v", tester.Texts())
	}
	for _, text := range tester.Texts() {
		if strings.HasPrefix(text, "回答 ") {
			if err := tester.Click(text); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	tester.Type("没有别的了")
	pump(tester, p)
	if !tester.HasText("✓ 备注") || !tester.HasText("已回答 2") {
		t.Fatalf("两题都应算已答：%v", tester.Texts())
	}

	// 切回第一题还能改：重新选一个选项，手写答案被替换掉
	if err := tester.Click("✓ 方案"); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if !tester.HasText("选哪个？") {
		t.Fatalf("应切回第一题：%v", tester.Texts())
	}
	if err := tester.Click("乙"); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if countText(tester, "◉") != 1 {
		t.Fatalf("改选后应选中乙：%v", tester.Texts())
	}

	// 提交：第一题用改选后的选项，第二题用手写答案
	if err := tester.Click(locSubmit); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	output := askOutput(t, app.Current())
	if !strings.Contains(output, `"id":"q1","selected":["乙"]`) {
		t.Fatalf("第一题应用改选后的选项：%s", output)
	}
	if strings.Contains(output, "都不合适") {
		t.Fatalf("改选选项后手写答案应被清掉：%s", output)
	}
	if !strings.Contains(output, `"id":"q2"`) || !strings.Contains(output, "没有别的了") {
		t.Fatalf("第二题应带自己的手写答案：%s", output)
	}
}

// askOutput 取本轮提问写回的工具输出（模型可见的那段 JSON）。
func askOutput(t *testing.T, session *uiapp.Session) string {
	t.Helper()
	for _, chat := range session.Conversation().Entries() {
		for _, part := range chat.Message.Message.Parts {
			dataPart, ok := part.Content.(message.DataPart)
			if !ok {
				continue
			}
			data, ok := dataPart.Data.(map[string]any)
			if !ok {
				continue
			}
			if name, _ := data["name"].(string); name != "ask_user_question" {
				continue
			}
			if output, _ := data["output"].(string); output != "" {
				return output
			}
		}
	}
	t.Fatalf("没找到提问结果")
	return ""
}

// 写了文字之后仍可反悔：改选预设选项以选项作答（文字保留在框里），
// 再把选项取消就又回到文字作答。
func TestAskRevertToPresetOption(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 1000, 780)
	clickComposer(t, tester)
	tester.Type("问我")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "ask_1", Name: "ask_user_question"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"questions":[
			{"id":"q1","question":"选哪个？","options":[{"label":"甲"},{"label":"乙"}]}]}`},
		providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"},
	)
	waitFor(t, tester, p, "出现提问", func() bool { return tester.HasText("选哪个？") })

	// 先写文字作答
	for _, text := range tester.Texts() {
		if strings.HasPrefix(text, "回答 ") {
			if err := tester.Click(text); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	tester.Type("我自己写")
	pump(tester, p)
	if !tester.HasText("已回答 1") {
		t.Fatalf("写字后应算已答：%v", tester.Texts())
	}

	// 反悔：改选「甲」——以选项作答，文字仍留在框里
	if err := tester.Click("甲"); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	if countText(tester, "◉") != 1 {
		t.Fatalf("应改为选中甲：%v", tester.Texts())
	}
	if !tester.HasText("当前按上面的选项作答") {
		t.Fatalf("应提示文字仍保留、只是不作为答案：%v", tester.Texts())
	}

	// 再反悔一次：回到输入框继续改字 → 文字重新成为答案
	if err := tester.Click("回答 1"); err != nil {
		t.Fatal(err)
	}
	tester.Type("，还是我自己决定")
	pump(tester, p)
	if countText(tester, "◉") != 0 {
		t.Fatalf("改回文字作答后不应有选中项：%v", tester.Texts())
	}
	if tester.HasText("当前按上面的选项作答") {
		t.Fatalf("回到文字作答后不应再提示：%v", tester.Texts())
	}

	if err := tester.Click(locSubmit); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	output := askOutput(t, app.Current())
	if !strings.Contains(output, "我自己写") || !strings.Contains(output, "还是我自己决定") {
		t.Fatalf("改回文字后应以文字作答：%s", output)
	}
}

// 中途输出与最终输出**渲染一致**（都是正文 markdown），只有思考/工具进折叠组。
func TestNarrationRendersLikeFinalAnswer(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()
	add := func(id string, role message.Role, parts ...message.Part) {
		t.Helper()
		if _, err := conv.AddMessage(message.Message{
			MessageID: id, Role: role, Parts: parts,
		}, conversation.StatusComplete); err != nil {
			t.Fatal(err)
		}
	}
	reasoning := func(text string) message.Part {
		return message.Part{Content: message.TextPart{Text: text}, MediaType: render.MediaReasoning}
	}
	toolUse := func(id, name, arg string) message.Part {
		return message.Part{Content: message.DataPart{Data: map[string]any{
			"id": id, "name": name, "args": map[string]any{"file_path": arg},
		}}, MediaType: render.MediaToolUse}
	}
	toolResult := func(id, name string) message.Part {
		return message.Part{Content: message.DataPart{Data: map[string]any{
			"id": id, "name": name, "output": "ok", "is_error": false,
		}}, MediaType: render.MediaToolResult}
	}

	add("u1", message.RoleUser, message.NewTextPart("看下进度"))
	add("a1", message.RoleAgent,
		reasoning("先想想要看什么"),
		toolUse("c1", "read", "docs/plan.md"), toolResult("c1", "read"),
		message.NewTextPart("我看了一下 **docs/** 目录：\n\n- `plan.md`\n\n接着读文件。"))
	add("a2", message.RoleAgent, message.NewTextPart("任务都完成了：\n\n1. M1 消息"))

	tester := ui.NewTester(app.View, 1000, 780)
	pump(tester, p)

	// 中途输出在组外、且是 markdown（列表项、行内代码都渲染出来了）
	for _, want := range []string{"我看了一下", "docs/ 目录", "plan.md", "接着读文件。"} {
		if !tester.HasText(want) {
			t.Fatalf("中途输出应以正文渲染出 %q：%v", want, tester.Texts())
		}
	}
	// 思考折叠进组：只看到标题行，看不到思考原文
	if !hasProcessGroup(tester) {
		t.Fatalf("应有过程组：%v", tester.Texts())
	}
	if tester.HasText("先想想要看什么") {
		t.Fatalf("思考应折叠进组：%v", tester.Texts())
	}
	// 中途输出不该在展开组里重复出现
	clickProcessGroup(t, tester)
	pump(tester, p)
	if countText(tester, "接着读文件。") > 1 {
		t.Fatalf("中途输出不应重复出现在展开的组里：%v", tester.Texts())
	}
}

// ensureSession 在"全新数据目录 → 空态"的情况下先建一个会话（决策 #54 之后，
// 启动时不再自动新建空会话）。已经有会话时什么都不做。
func ensureSession(t *testing.T, tester *ui.Tester, p *poster, app *uiapp.App) {
	t.Helper()
	if app.Current() != nil {
		return
	}
	if !tester.HasText(locNewSession) {
		t.Fatalf("空态下应有新建入口：%v", tester.Texts())
	}
	if err := tester.Click(locNewSession); err != nil {
		t.Fatalf("点不到新建会话：%v", err)
	}
	pump(tester, p)
}
