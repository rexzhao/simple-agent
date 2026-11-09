package uiapp_test

import (
	"net"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/host"
	"simple-agent/internal/locale"
	"simple-agent/internal/providers"
	"simple-agent/internal/relay"
	"simple-agent/internal/store"
	"simple-agent/internal/uiapp"
	"simple-agent/internal/wire"
)

// serialExec 是执行端的串行 post（真实界面主线程的替代）。
func serialExec() func(func()) {
	queue := make(chan func(), 4096)
	go func() {
		for f := range queue {
			f()
		}
	}()
	return func(f func()) { queue <- f }
}

// 经 relay 的展示端：侧栏要出现执行端的会话（catalog → sessions 变化 → 重建列表）。
func TestRemoteSidebarShowsSessions(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := relay.New()
	defer server.Close()
	go server.ServeAll(func() (wire.Conn, error) {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return nil, wire.ErrClosed
		}
		return wire.NewTCPConn(conn), nil
	})

	local := host.NewLocal(host.Config{
		Catalog: store.NewCatalog(t.TempDir()), Runtime: &fakeRuntime{},
		Selection:    providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: serialExec(),
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("远程会话标题")
	}

	stop := make(chan struct{})
	defer close(stop)
	go relay.RunExecutor(listener.Addr().String(), relay.Executor{HostID: "exec-sidebar"}, stop,
		func(conn wire.Conn) { _ = host.Serve(conn, local, serialExec()) })
	registerDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(registerDeadline) {
		if list, listErr := relay.ListExecutors(listener.Addr().String()); listErr == nil && len(list) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	conn, err := relay.DialViewer(listener.Addr().String(), "exec-sidebar")
	if err != nil {
		t.Fatal(err)
	}
	p0 := &poster{}
	remote, err := host.NewRemote(conn, p0.Post)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { remote.Close() })
	remote.Subscribe(func(change host.Change) {
		if change.Kind == "sessions" {
			t.Logf("收到 sessions 变化，remote 会话数=%d", len(remote.Sessions()))
		}
	})

	app, _, _ := newAppWithOptions(t, func(opts *uiapp.Options) {
		opts.Language = locale.ZH
		opts.Host = remote
		opts.Post = p0.Post
	})
	tester := ui.NewTester(app.View, 900, 600)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p0.Drain()
		tester.Frame()
		if tester.HasText("远程会话标题") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("侧栏没有出现会话来（remote 有 %d 个）：%v", len(remote.Sessions()), tester.Texts())
}
