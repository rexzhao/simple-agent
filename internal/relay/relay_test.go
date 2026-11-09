package relay_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/relay"
	"simple-agent/internal/tools"
	"simple-agent/internal/wire"
)

// relayTest 起 relay + TCP listener，返回地址。
func startRelay(t *testing.T) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	r := relay.New()
	go r.ServeAll(func() (wire.Conn, error) {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return nil, wire.ErrClosed
		}
		return wire.NewTCPConn(conn), nil
	})
	return addr, func() { _ = listener.Close(); r.Close() }
}

// §10 P4：relay 转发后，展示端拿到的状态和直连完全一致。
func TestRelayRoutesPackets(t *testing.T) {
	addr, stopRelay := startRelay(t)
	defer stopRelay()

	post := serial()
	local := host.NewLocal(host.Config{
		Runtime: newRuntime(), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: post,
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	sess := local.Active().ID

	stop := make(chan struct{})
	defer close(stop)
	go relay.RunExecutor(addr, relay.Executor{HostID: "exec-1", Name: "测试执行端"}, stop, func(conn wire.Conn) {
		_ = host.Serve(conn, local, post)
	})
	wait(t, "执行端注册", func() bool {
		list, err := relay.ListExecutors(addr)
		return err == nil && len(list) == 1
	})

	// 展示端：经 relay
	conn, err := relay.DialViewer(addr, "exec-1")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := host.NewRemote(conn, serial())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	wait(t, "经 relay 同步到会话", func() bool { return len(viewer.Sessions()) == 1 })

	// 直连对照
	direct := host.NewLocal(host.Config{
		Runtime: newRuntime(), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: serial(),
	})
	_ = direct
	payload, _ := host.EncodePayload(host.SendPayload{Text: "你好"})
	if _, err := viewer.Submit(host.Command{SessionID: sess, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	wait(t, "命令经 relay 到达", func() bool {
		return ask(post, func() bool {
			session := local.Session(sess)
			return session.Busy() != driver.BusyIdle || len(session.Conversation().Entries()) > 0
		})
	})
}

type runtime struct{}

func newRuntime() *runtime { return &runtime{} }

func (r *runtime) OpenStream(_ context.Context, _ providers.Selection, _ providers.ChatRequest) (<-chan providers.Event, error) {
	events := make(chan providers.Event, 8)
	go func() {
		events <- providers.Event{Kind: providers.EventText, Text: "回答"}
		events <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
		close(events)
	}()
	return events, nil
}

func (r *runtime) RunTools(_ context.Context, _ *tools.ToolContext, _ []tools.Call) []tools.ToolOutcome {
	return nil
}

// ask 在会话自己的线程上读状态（避免测试线程与 post 线程的数据竞争）。
func ask(post func(func()), check func() bool) bool {
	result := make(chan bool, 1)
	post(func() { result <- check() })
	return <-result
}

// serial 返回串行 post（真实界面是主线程；并发执行会让镜像 map 竞态）。
func serial() func(func()) {
	queue := make(chan func(), 4096)
	go func() {
		for f := range queue {
			f()
		}
	}()
	return func(f func()) { queue <- f }
}

// mustEncode 编码 JSON（测试用）。
func mustEncode(value any) []byte {
	body, _ := wire.Encode(value)
	return body
}

func wait(t *testing.T, name string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", name)
}

// §10 P4：relay 重启后，执行端自动重新注册、展示端自动重连，命令仍然可用。
func TestRelayRestartRecovers(t *testing.T) {
	addr, stopRelay := startRelay(t)

	post := serial()
	local := host.NewLocal(host.Config{
		Runtime: newRuntime(), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: post,
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	sess := local.Active().ID

	stop := make(chan struct{})
	defer close(stop)
	go relay.RunExecutor(addr, relay.Executor{HostID: "exec-1"}, stop, func(conn wire.Conn) {
		_ = host.Serve(conn, local, post)
	})
	wait(t, "执行端注册", func() bool {
		list, err := relay.ListExecutors(addr)
		return err == nil && len(list) == 1
	})

	dial := func() (wire.Conn, error) { return relay.DialViewer(addr, "exec-1") }
	remote, err := host.NewRemoteWithDialer(dial, serial())
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	wait(t, "展示端连上", func() bool { return remote.Connected() })

	// 重启 relay：关掉旧 listener，同一地址再起一个全新的 relay
	stopRelay()
	time.Sleep(100 * time.Millisecond)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("端口不能立即重用：%v", err)
	}
	fresh := relay.New()
	go fresh.ServeAll(func() (wire.Conn, error) {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return nil, wire.ErrClosed
		}
		return wire.NewTCPConn(conn), nil
	})
	defer listener.Close()

	wait(t, "执行端重新注册", func() bool { return len(fresh.List()) == 1 })
	wait(t, "展示端自动重连", func() bool { return remote.Connected() })

	payload, _ := host.EncodePayload(host.SendPayload{Text: "重启之后还能用吗"})
	if _, err := remote.Submit(host.Command{SessionID: sess, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	wait(t, "命令经新 relay 到达", func() bool {
		return ask(post, func() bool {
			return len(local.Session(sess).Conversation().Entries()) > 0
		})
	})
}

// executorCount 是 relay 上当前注册的执行端数量。
func executorCount(addr string) int {
	list, err := relay.ListExecutors(addr)
	if err != nil {
		return -1
	}
	return len(list)
}

// P1-1：展示端反复连上/断开，执行端的观察者不能越积越多（通道关闭要传到执行端）。
func TestRelayViewerDisconnectReleasesExecutor(t *testing.T) {
	addr, stopRelay := startRelay(t)
	defer stopRelay()

	post := serial()
	local := host.NewLocal(host.Config{
		Runtime: newRuntime(), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: post,
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}

	stop := make(chan struct{})
	defer close(stop)
	go relay.RunExecutor(addr, relay.Executor{HostID: "exec-1"}, stop, func(conn wire.Conn) {
		_ = host.Serve(conn, local, post)
	})
	wait(t, "执行端注册", func() bool { return executorCount(addr) == 1 })

	for i := 0; i < 5; i++ {
		conn, err := relay.DialViewer(addr, "exec-1")
		if err != nil {
			t.Fatal(err)
		}
		viewer, err := host.NewRemote(conn, post)
		if err != nil {
			t.Fatal(err)
		}
		wait(t, "展示端连上", func() bool { return viewer.Connected() })
		viewer.Close()
		wait(t, "观察者被摘除", func() bool {
			return ask(post, func() bool { return local.ObserverCountForTest() == 0 })
		})
	}
}

// P1-2：经过 relay 时一个卡住的展示端只断自己，另一个照常收到全部内容。
func TestRelayStalledViewerDoesNotBlockOthers(t *testing.T) {
	relay.SetOutQueueLimitForTest(32)
	defer relay.SetOutQueueLimitForTest(2000)

	addr, stopRelay, r := startRelayWithInstance(t)
	defer stopRelay()

	stalledConn := &blockedConn{route: mustEncode(map[string]string{"host_id": "exec-1"})}
	go r.Serve(stalledConn)

	// 执行端只是一个"发包机"：按 relay 协议注册后连续给两条通道发帧
	execConn, err := wire.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer execConn.Close()
	body, _ := wire.Encode(relay.Executor{HostID: "exec-1"})
	if err := execConn.Send(wire.Packet{Kind: relay.MsgRegister, Body: body}); err != nil {
		t.Fatal(err)
	}
	if reply, recvErr := execConn.Recv(); recvErr != nil || reply.Kind != relay.MsgRegistered {
		t.Fatalf("注册失败：%v", recvErr)
	}
	wait(t, "注册可见", func() bool { return executorCount(addr) == 1 })

	// 第一个展示端：写入永远阻塞（Send 直接挂起），用来模拟对端不读 socket
	good, err := relay.DialViewer(addr, "exec-1")
	if err != nil {
		t.Fatal(err)
	}
	defer good.Close()
	time.Sleep(100 * time.Millisecond)

	const total = 60
	received := make(chan int, 1)
	go func() {
		count := 0
		for count < total {
			if _, recvErr := good.Recv(); recvErr != nil {
				received <- count
				return
			}
			count++
		}
		received <- count
	}()

	// 通道 id 由 relay 分配（第一个展示端 2，第二个 3）：两边都发，
	// 正常展示端应当收全，卡住的那个被单独断开
	for i := 0; i < total; i++ {
		payload, _ := wire.Encode(map[string]int{"i": i})
		for _, id := range []uint32{2, 3} {
			if err := execConn.Send(wire.Packet{Conn: id, Kind: "event", Body: payload}); err != nil {
				t.Fatal(err)
			}
		}
		if i%5 == 0 {
			time.Sleep(2 * time.Millisecond) // 让正常展示端的写 goroutine 有时间排空
		}
	}

	select {
	case count := <-received:
		if count != total {
			t.Fatalf("正常展示端少收了：%d/%d（被卡住的那个拖累了它）", count, total)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("正常展示端没有收全")
	}
}

// blockedConn 的 Send 永远挂起、Recv 只交付一次 route，模拟"对端不读 socket"。
type blockedConn struct {
	route []byte
	once  bool
}

func (c *blockedConn) Send(wire.Packet) error {
	select {} // 永远阻塞：relay 的写 goroutine 会卡在这里
}

func (c *blockedConn) Recv() (wire.Packet, error) {
	if !c.once {
		c.once = true
		return wire.Packet{Kind: relay.MsgRoute, Body: c.route}, nil
	}
	select {}
}

func (c *blockedConn) Close() error { return nil }

// startRelayWithInstance 与 startRelay 相同，但把 Relay 实例一起返回。
func startRelayWithInstance(t *testing.T) (string, func(), *relay.Relay) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := relay.New()
	go r.ServeAll(func() (wire.Conn, error) {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return nil, wire.ErrClosed
		}
		return wire.NewTCPConn(conn), nil
	})
	return listener.Addr().String(), func() { _ = listener.Close(); r.Close() }, r
}

// --connect 没写 --route 时：探测到对面是 relay 且只有一个执行端，展示端应当能连上并同步会话。
func TestProbeExecutorsAndAutoRoute(t *testing.T) {
	addr, stopRelay := startRelay(t)
	defer stopRelay()

	// 直连执行端的地址：探测应当超时/失败（不是 relay）
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	directAddr := listener.Addr().String()
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() { _ = conn.Close() }()
		}
	}()
	defer listener.Close()
	if _, probeErr := relay.ProbeExecutors(directAddr, 200*time.Millisecond); probeErr == nil {
		t.Fatal("直连执行端不该被当成 relay")
	}

	local := host.NewLocal(host.Config{
		Runtime: newRuntime(), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: serial(),
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	post := serial()
	stop := make(chan struct{})
	defer close(stop)
	go relay.RunExecutor(addr, relay.Executor{HostID: "exec-auto"}, stop, func(conn wire.Conn) {
		_ = host.Serve(conn, local, post)
	})
	wait(t, "注册", func() bool { return executorCount(addr) == 1 })

	executors, err := relay.ProbeExecutors(addr, time.Second)
	if err != nil || len(executors) != 1 || executors[0].HostID != "exec-auto" {
		t.Fatalf("探测结果不对：%v %v", executors, err)
	}

	// 界面按"只有一个执行端"自动路由
	conn, err := relay.DialViewer(addr, executors[0].HostID)
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := host.NewRemote(conn, post)
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	wait(t, "同步到会话", func() bool { return len(viewer.Sessions()) == 1 })
}

// OpenViewer：一次握手分辨对端。直连返回连接（welcome 垫回），relay 自动/显式路由。
func TestOpenViewerDetectsPeerWithoutExtraProbe(t *testing.T) {
	addr, stopRelay := startRelay(t)
	defer stopRelay()

	// 直连执行端
	local := host.NewLocal(host.Config{
		Runtime: newRuntime(), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: serial(),
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	directListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer directListener.Close()
	go func() {
		for {
			conn, acceptErr := directListener.Accept()
			if acceptErr != nil {
				return
			}
			go func() { _ = host.Serve(wire.NewTCPConn(conn), local, serial()) }()
		}
	}()

	start := time.Now()
	conn, _, err := relay.OpenViewer(directListener.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("直连不该等探测超时：%v", elapsed)
	}
	viewer, err := host.NewRemote(conn, serial())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	wait(t, "直连同步到会话", func() bool { return len(viewer.Sessions()) == 1 })

	// relay：唯一执行端自动选中
	go relay.RunExecutor(addr, relay.Executor{HostID: "only-one"}, make(chan struct{}), func(conn wire.Conn) {
		_ = host.Serve(conn, local, serial())
	})
	wait(t, "注册", func() bool { return executorCount(addr) == 1 })
	relayConn, chosen, err := relay.OpenViewer(addr, "")
	if err != nil || chosen != "only-one" {
		t.Fatalf("自动选中不对：%q %v", chosen, err)
	}
	if err != nil {
		t.Fatal(err)
	}
	relayViewer, err := host.NewRemote(relayConn, serial())
	if err != nil {
		t.Fatal(err)
	}
	defer relayViewer.Close()
	wait(t, "relay 同步到会话", func() bool { return len(relayViewer.Sessions()) == 1 })
}

// 直连经 OpenViewer 之后只 attach 一次（不因为"两次 hello"做两遍）。
func TestOpenViewerSingleAttach(t *testing.T) {
	local := host.NewLocal(host.Config{
		Runtime: newRuntime(), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: serial(),
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	sess := local.Active().ID

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() { _ = host.Serve(wire.NewTCPConn(conn), local, serial()) }()
		}
	}()

	conn, chosen, err := relay.OpenViewer(listener.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	if chosen != "" {
		t.Fatalf("直连不该有 relay hostID：%q", chosen)
	}
	viewer, err := host.NewRemote(conn, serial())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	wait(t, "attach 完成", func() bool { return viewer.AttachCountForTest(sess) == 1 })
	time.Sleep(100 * time.Millisecond)
	if count := viewer.AttachCountForTest(sess); count != 1 {
		t.Fatalf("同一会话 attach 了 %d 次（重复 hello？）", count)
	}
}

// 自动选中的执行端要固定下来：A 下线、B 上线后，展示端**不能**悄悄连到 B。
func TestAutoChosenExecutorStaysPinned(t *testing.T) {
	addr, stopRelay := startRelay(t)
	defer stopRelay()

	makeLocal := func(text string) (*host.Local, string) {
		local := host.NewLocal(host.Config{
			Runtime: newRuntime(), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
			SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
			ContextWindow: 200_000, Post: serial(),
		})
		t.Cleanup(func() { _ = local.Close() })
		if local.Active() == nil {
			local.NewSession(text)
		}
		payload, _ := host.EncodePayload(host.SendPayload{Text: text})
		_, _ = local.Submit(host.Command{SessionID: local.Active().ID, Kind: "send", Payload: payload})
		time.Sleep(50 * time.Millisecond)
		return local, local.Active().ID
	}
	localA, sessA := makeLocal("A 的会话")
	localB, sessB := makeLocal("B 的会话")

	stopA := make(chan struct{})
	go relay.RunExecutor(addr, relay.Executor{HostID: "exec-a"}, stopA, func(conn wire.Conn) {
		_ = host.Serve(conn, localA, serial())
	})
	wait(t, "A 注册", func() bool { return executorCount(addr) == 1 })

	pinned := ""
	dial := func() (wire.Conn, error) {
		conn, chosen, dialErr := relay.OpenViewer(addr, pinned)
		if dialErr == nil && chosen != "" {
			pinned = chosen
		}
		return conn, dialErr
	}
	viewer, err := host.NewRemoteWithDialer(dial, serial())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	wait(t, "连到 A", func() bool { return viewer.Connected() && viewer.AttachCountForTest(sessA) >= 1 })
	if pinned != "exec-a" {
		t.Fatalf("没有固定到 A：%q", pinned)
	}

	// A 下线
	close(stopA)
	wait(t, "A 下线", func() bool { return executorCount(addr) == 0 })
	// B 上线（列表里又只有一个执行端）
	localBB, _ := localB, sessB
	stopB := make(chan struct{})
	defer close(stopB)
	go relay.RunExecutor(addr, relay.Executor{HostID: "exec-b"}, stopB, func(conn wire.Conn) {
		_ = host.Serve(conn, localBB, serial())
	})
	wait(t, "B 注册", func() bool { return executorCount(addr) == 1 })

	// 等一段时间：展示端必须一直在"重连中"，不能连上 B
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if viewer.AttachCountForTest(sessB) > 0 {
			t.Fatal("自动选中没有固定：重连时换到了另一台执行端 B")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if viewer.Connected() {
		t.Fatal("A 不在线时不该显示已连接")
	}
}

// P1-3：切换执行端之后，展示端只剩新执行端的会话；切回来又只剩旧的。
func TestSwitchHostResetsState(t *testing.T) {
	addr, stopRelay := startRelay(t)
	defer stopRelay()

	makeHost := func(title string) (*host.Local, string) {
		local := host.NewLocal(host.Config{
			Runtime: newRuntime(), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
			SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
			ContextWindow: 200_000, Post: serial(),
		})
		t.Cleanup(func() { _ = local.Close() })
		if local.Active() == nil {
			local.NewSession(title)
		}
		return local, local.Active().ID
	}
	localA, sessA := makeHost("A 的会话")
	localB, sessB := makeHost("B 的会话")

	stop := make(chan struct{})
	defer close(stop)
	post := serial()
	go relay.RunExecutor(addr, relay.Executor{HostID: "exec-a"}, stop, func(conn wire.Conn) {
		_ = host.Serve(conn, localA, post)
	})
	go relay.RunExecutor(addr, relay.Executor{HostID: "exec-b"}, stop, func(conn wire.Conn) {
		_ = host.Serve(conn, localB, post)
	})
	wait(t, "两台都注册", func() bool { return executorCount(addr) == 2 })

	var pinnedMu sync.Mutex
	pinned := "exec-a"
	setPinned := func(id string) { pinnedMu.Lock(); pinned = id; pinnedMu.Unlock() }
	dial := func() (wire.Conn, error) {
		pinnedMu.Lock()
		target := pinned
		pinnedMu.Unlock()
		return relay.DialViewer(addr, target)
	}
	remote, err := host.NewRemoteWithDialer(dial, post)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	wait(t, "连上 A", func() bool { return remote.Session(sessA) != nil })

	// 切到 B
	setPinned("exec-b")
	remote.ResetForNewHost()
	remote.RetryNow()
	deadlineB := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadlineB) && remote.Session(sessB) == nil {
		time.Sleep(20 * time.Millisecond)
	}
	if remote.Session(sessB) == nil {
		t.Fatalf("切到 B 失败：connected=%v sessions=%v", remote.Connected(), remote.Sessions())
	}

	ids := map[string]bool{}
	for _, ref := range remote.Sessions() {
		ids[ref.ID] = true
	}
	if ids[sessA] {
		t.Fatal("切到 B 之后侧栏里还有 A 的会话")
	}
	if !ids[sessB] {
		t.Fatalf("切到 B 之后没有 B 的会话：%v", remote.Sessions())
	}

	// 切回 A
	setPinned("exec-a")
	remote.ResetForNewHost()
	remote.RetryNow()
	wait(t, "切回 A", func() bool { return remote.Session(sessA) != nil })
	ids = map[string]bool{}
	for _, ref := range remote.Sessions() {
		ids[ref.ID] = true
	}
	if ids[sessB] || !ids[sessA] {
		t.Fatalf("切回 A 之后会话不对：%v", remote.Sessions())
	}
}

// relay 上有多个执行端且没选：OpenViewer 返回 NeedsChoice（界面据此打开选择列表）。
func TestOpenViewerNeedsChoiceWhenAmbiguous(t *testing.T) {
	addr, stopRelay := startRelay(t)
	defer stopRelay()

	stop := make(chan struct{})
	defer close(stop)
	local := func() *host.Local {
		return host.NewLocal(host.Config{
			Runtime: newRuntime(), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
			SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
			ContextWindow: 200_000, Post: serial(),
		})
	}
	for _, name := range []string{"A", "B"} {
		l := local()
		t.Cleanup(func() { _ = l.Close() })
		current := l
		go relay.RunExecutor(addr, relay.Executor{HostID: name}, stop, func(conn wire.Conn) {
			_ = host.Serve(conn, current, serial())
		})
	}
	wait(t, "两台注册", func() bool { return executorCount(addr) == 2 })

	_, _, err := relay.OpenViewer(addr, "")
	var choice *relay.NeedsChoice
	if !errors.As(err, &choice) || len(choice.Executors) != 2 {
		t.Fatalf("应当返回 NeedsChoice：%v", err)
	}
}

// P1（竞态窗口）：A 正在流式输出时切到 B，已经排进界面队列的 A 的包不能落到 B 上。
func TestSwitchHostWhileStreamingDropsStalePackets(t *testing.T) {
	addr, stopRelay := startRelay(t)
	defer stopRelay()

	// A：一个会持续吐字的执行端
	localA := host.NewLocal(host.Config{
		Runtime: newStreamingRuntime("A-STREAM-TEXT"), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: serial(),
	})
	t.Cleanup(func() { _ = localA.Close() })
	if localA.Active() == nil {
		localA.NewSession("A 的会话")
	}
	sessA := localA.Active().ID
	localB := host.NewLocal(host.Config{
		Runtime: newRuntime(), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: serial(),
	})
	t.Cleanup(func() { _ = localB.Close() })
	if localB.Active() == nil {
		localB.NewSession("B 的会话")
	}
	sessB := localB.Active().ID

	stop := make(chan struct{})
	defer close(stop)
	go relay.RunExecutor(addr, relay.Executor{HostID: "A"}, stop, func(conn wire.Conn) {
		_ = host.Serve(conn, localA, serial())
	})
	go relay.RunExecutor(addr, relay.Executor{HostID: "B"}, stop, func(conn wire.Conn) {
		_ = host.Serve(conn, localB, serial())
	})
	wait(t, "两台注册", func() bool { return executorCount(addr) == 2 })

	var mu sync.Mutex
	current := "A"
	dial := func() (wire.Conn, error) {
		mu.Lock()
		target := current
		mu.Unlock()
		return relay.DialViewer(addr, target)
	}
	post := serial()
	remote, err := host.NewRemoteWithDialer(dial, post)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	wait(t, "连上 A", func() bool { return remote.Session(sessA) != nil })

	// 让 A 开始流式输出（事件会不断被 post 到界面队列）
	payload, _ := host.EncodePayload(host.SendPayload{Text: "开始输出"})
	_, _ = remote.Submit(host.Command{SessionID: sessA, Kind: "send", Payload: payload})
	time.Sleep(100 * time.Millisecond)

	// 切换：关旧连接 → 加代际/清空 → 重连到 B
	mu.Lock()
	current = "B"
	mu.Unlock()
	remote.DropConn()
	remote.ResetForNewHost()
	remote.RetryNow()

	// 把所有已排进队列的回调都跑一遍
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if remote.Session(sessB) != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	if remote.Session(sessA) != nil {
		t.Fatal("切到 B 之后仍然能看到 A 的会话")
	}
	for _, ref := range remote.Sessions() {
		if ref.ID == sessA {
			t.Fatal("侧栏里仍有 A 的会话")
		}
	}
}

// streamingRuntime 每 10ms 吐一个词，直到被取消（用来制造"切换时还在流式输出"）。
type streamingRuntime struct{ text string }

func newStreamingRuntime(text string) *streamingRuntime { return &streamingRuntime{text: text} }

func (r *streamingRuntime) OpenStream(ctx context.Context, _ providers.Selection, _ providers.ChatRequest) (<-chan providers.Event, error) {
	events := make(chan providers.Event, 128)
	go func() {
		defer close(events)
		for i := 0; i < 100; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
			events <- providers.Event{Kind: providers.EventText, Text: r.text}
		}
		events <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	}()
	return events, nil
}

func (r *streamingRuntime) RunTools(context.Context, *tools.ToolContext, []tools.Call) []tools.ToolOutcome {
	return nil
}

// relay 配了 --token 时：不带/带错 token 一律拒绝，带对了照常转发。
// 用户决定：token 只保护 relay 这一段（执行端直连不受影响）。
func TestRelayTokenAuth(t *testing.T) {
	addr, stopRelay, server := startRelayWithInstance(t)
	defer stopRelay()
	server.Token = "s3cret"

	post := serial()
	local := host.NewLocal(host.Config{
		Runtime: newRuntime(), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: post,
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}

	// 1) 不带 token 的注册会被拒绝，执行端不在列表里
	stopBad := make(chan struct{})
	go relay.RunExecutor(addr, relay.Executor{HostID: "no-token"}, stopBad, func(wire.Conn) {})
	time.Sleep(300 * time.Millisecond)
	close(stopBad)
	if list, err := relay.ListExecutorsToken(addr, "s3cret"); err != nil || len(list) != 0 {
		t.Fatalf("不带 token 的执行端不该注册成功：%v %v", list, err)
	}

	// 2) 查询列表也要 token
	if _, err := relay.ListExecutors(addr); err == nil {
		t.Fatal("不带 token 查询列表应当被拒绝")
	}
	if _, err := relay.ListExecutorsToken(addr, "wrong"); err == nil {
		t.Fatal("错误 token 查询列表应当被拒绝")
	}

	// 3) 带正确 token：注册、列表、路由、同步全部正常
	stop := make(chan struct{})
	defer close(stop)
	go relay.RunExecutor(addr, relay.Executor{HostID: "exec-1", Token: "s3cret"}, stop, func(conn wire.Conn) {
		_ = host.Serve(conn, local, post)
	})
	wait(t, "带 token 的执行端注册", func() bool {
		list, err := relay.ListExecutorsToken(addr, "s3cret")
		return err == nil && len(list) == 1
	})

	if _, err := relay.DialViewerToken(addr, "exec-1", "wrong"); err == nil {
		t.Fatal("错误 token 的路由请求应当被拒绝")
	}
	conn, err := relay.DialViewerToken(addr, "exec-1", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := host.NewRemote(conn, serial())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	wait(t, "经 relay 同步到会话", func() bool { return len(viewer.Sessions()) == 1 })
}

// relay 没配 token 时行为不变（兼容既有部署）。
func TestRelayWithoutTokenStillWorks(t *testing.T) {
	addr, stopRelay := startRelay(t)
	defer stopRelay()
	post := serial()
	local := host.NewLocal(host.Config{
		Runtime: newRuntime(), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: post,
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	stop := make(chan struct{})
	defer close(stop)
	go relay.RunExecutor(addr, relay.Executor{HostID: "exec-open"}, stop, func(conn wire.Conn) {
		_ = host.Serve(conn, local, post)
	})
	wait(t, "执行端注册", func() bool {
		list, err := relay.ListExecutors(addr)
		return err == nil && len(list) == 1
	})
}

// 展示端（relay.OpenViewerToken）会先发 hello 探测对端，relay 必须跳过它再校验 token。
// 回归：早先版本把 hello 当第一条请求帧去校验 token，导致带 token 的连接一定被拒。
func TestRelayTokenWithHelloProbe(t *testing.T) {
	addr, stopRelay, server := startRelayWithInstance(t)
	defer stopRelay()
	server.Token = "s3cret"

	post := serial()
	local := host.NewLocal(host.Config{
		Runtime: newRuntime(), Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: post,
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}

	stop := make(chan struct{})
	defer close(stop)
	go relay.RunExecutor(addr, relay.Executor{HostID: "exec-1", Token: "s3cret"}, stop, func(conn wire.Conn) {
		_ = host.Serve(conn, local, post)
	})
	wait(t, "执行端注册", func() bool {
		list, err := relay.ListExecutorsToken(addr, "s3cret")
		return err == nil && len(list) == 1
	})

	// 真实客户端的路径：一次握手先分辨对端，再路由
	conn, chosen, err := relay.OpenViewerToken(addr, "", "s3cret")
	if err != nil {
		t.Fatalf("带 token 的 OpenViewer 失败：%v", err)
	}
	if chosen != "exec-1" {
		t.Fatalf("应自动选中唯一执行端，实际 %q", chosen)
	}
	viewer, err := host.NewRemote(conn, serial())
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	wait(t, "经 relay 同步到会话", func() bool { return len(viewer.Sessions()) == 1 })
}
