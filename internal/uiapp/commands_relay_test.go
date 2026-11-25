package uiapp

import (
	"strings"
	"testing"
	"time"

	"simple-agent/internal/host"
	"simple-agent/internal/providers"
)

// 端到端：/connect 真的把执行端注册到 relay，/relay 能看到状态，/disconnect 能断开。
//
// 用一个最小 relay（internal/relay）顶替真实服务器：验证的是"命令是否真的驱动了
// 注册逻辑"，网络细节由 relay 包的测试覆盖。
func TestConnectCommandRegistersToRelay(t *testing.T) {
	// 起一个只接受注册的 relay
	relayServer, addr, relayFingerprint := startTestRelayWithFingerprint(t)
	defer relayServer.Close()

	app := newLocalApp(t)

	// 1) /connect 带 token
	message, ok := app.ExecSlashCommand("/connect " + addr + " testtoken " + relayFingerprint)
	if !ok {
		t.Fatal("/connect 应当被识别为命令")
	}
	if !strings.Contains(message, "正在连接") {
		t.Fatalf("/connect 应报告开始连接，实际：%q", message)
	}
	// 注册是异步的：等 relay 确认之后 Relaying() 才会变 true。
	waitRelayOnline(t, app)

	// 2) 等 relay 侧看到注册
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(relayServer.List()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	list := relayServer.List()
	if len(list) == 0 {
		t.Fatal("relay 没有看到执行端注册")
	}
	hostID := list[0].HostID

	// 3) /relay 显示状态（要包含地址与 host_id）
	status, ok := app.ExecSlashCommand("/relay")
	if !ok || !strings.Contains(status, addr) {
		t.Fatalf("/relay 应显示地址，实际：%q", status)
	}
	if !strings.Contains(status, hostID) {
		t.Fatalf("/relay 应显示 host_id=%s，实际：%q", hostID, status)
	}

	// 4) /disconnect 断开，relay 侧要看到执行端消失
	if message, ok := app.ExecSlashCommand("/disconnect"); !ok ||
		!strings.Contains(message, "已断开") {
		t.Fatalf("/disconnect 应报告已断开，实际：%q", message)
	}
	if app.Relaying() {
		t.Fatal("/disconnect 后 Relaying 应为 false")
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(relayServer.List()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("断开后 relay 仍然显示执行端在线")
}

// 重复 /connect：已经在线上时直接返回，不做无谓的重连。
//
// 为什么要这样：注册用 hostID 作路由键，relay 对同名执行端只允许一个在线。
// 无脑"先断再连"会让新连接撞上还没清理完的旧连接，被拒绝后无限退避重试 ——
// 真机日志里就是 30 秒一次的"同名执行端已在线"刷屏。
func TestConnectCommandIsIdempotent(t *testing.T) {
	relayServer, addr, relayFingerprint := startTestRelayWithFingerprint(t)
	defer relayServer.Close()

	app := newLocalApp(t)
	if message, _ := app.ExecSlashCommand("/connect " + addr + " tk " + relayFingerprint); !strings.Contains(message, "正在连接") {
		t.Fatalf("第一次 /connect 应开始连接：%q", message)
	}
	waitRelayOnline(t, app)

	// 等 relay 真正登记上线后再发一次：这一次应当直接返回"无需重连"。
	if message, _ := app.ExecSlashCommand("/connect " + addr + " tk " + relayFingerprint); !strings.Contains(message, "无需重连") {
		t.Fatalf("第二次 /connect 应提示无需重连：%q", message)
	}
	if !app.Relaying() {
		t.Fatal("重复 /connect 之后仍应处于连线状态")
	}
	// relay 那边也应当只有一个在线执行端（没有因为重连产生第二条记录）
	if got := len(relayServer.List()); got != 1 {
		t.Fatalf("relay 在线执行端应恰好 1 个，实际 %d 个", got)
	}
}

// waitRelayOnline 等到 App 报告"已上线"（注册是异步的）。
func waitRelayOnline(t *testing.T, app *App) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if app.Relaying() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待 relay 上线超时")
}

// newLocalApp 构造一个只含本地执行端的最小 App（够跑命令逻辑）。
func newLocalApp(t *testing.T) *App {
	t.Helper()
	rt := &stubRuntime{}
	local := host.NewLocal(host.Config{
		Runtime:       rt,
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Workspace:     t.TempDir(),
		MaxToolRounds: 3,
		ContextWindow: 200_000,
		Post:          func(fn func()) { fn() },
	})
	t.Cleanup(func() { _ = local.Close() })
	return &App{host: local, post: func(fn func()) { fn() }}
}
