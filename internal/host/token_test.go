package host_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
	"simple-agent/internal/wire"
)

// 执行端的 token（--listen 直连）与 relay 的 token 是两套：这里验证执行端侧。
//
// 三种情况：
//   - 执行端没配 token：谁都能连（保持原来的开放行为）；
//   - 配了 token：不带/带错的握手被拒，且**拿不到任何会话数据**；
//   - 带对 token：正常同步会话。
func TestExecutorTokenAuth(t *testing.T) {
	for _, tc := range []struct {
		name      string
		execToken string
		viewToken string
		wantOK    bool
	}{
		{"执行端无 token，展示端也不带", "", "", true},
		{"执行端有 token，展示端带对", "s3cret", "s3cret", true},
		{"执行端有 token，展示端不带", "s3cret", "", false},
		{"执行端有 token，展示端带错", "s3cret", "wrong", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			poster := newTokenPoster()
			rt := &tokenRuntime{}
			local := host.NewLocal(host.Config{
				Runtime:       rt,
				Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
				SystemPrompt:  "你是助手",
				Workspace:     t.TempDir(),
				MaxToolRounds: 3,
				ContextWindow: 200_000,
				Post:          poster.Post,
				Token:         tc.execToken,
			})
			t.Cleanup(func() { _ = local.Close() })
			if local.Active() == nil {
				local.NewSession("")
			}

			viewerConn, execConn := wire.PipePair()
			go func() { _ = host.Serve(execConn, local, poster.Post) }()

			remote, err := host.NewRemoteToken(viewerConn, poster.Post, tc.viewToken)
			if err != nil {
				t.Fatalf("NewRemoteToken：%v", err)
			}
			t.Cleanup(func() { _ = remote.Close() })

			if tc.wantOK {
				pumpTokenUntil(t, poster, func() bool { return len(remote.Sessions()) == 1 })
				return
			}
			// 被拒：pump 一段时间后仍然拿不到任何会话（也不能看到数据）
			deadline := time.Now().Add(600 * time.Millisecond)
			for time.Now().Before(deadline) {
				poster.pump()
				if len(remote.Sessions()) > 0 {
					t.Fatalf("token 不对却同步到了会话：%+v", remote.Sessions())
				}
				time.Sleep(2 * time.Millisecond)
			}
			if got := remote.Sessions(); len(got) != 0 {
				t.Fatalf("token 不对却拿到会话：%+v", got)
			}
		})
	}
}

// token 校验失败时，执行端要回一条带原因的 welcome（展示端能显示"token 不正确"，
// 而不是干等超时）。
func TestExecutorTokenRejectionHasReason(t *testing.T) {
	poster := newTokenPoster()
	local := host.NewLocal(host.Config{
		Runtime:       &tokenRuntime{},
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Workspace:     t.TempDir(),
		MaxToolRounds: 3,
		ContextWindow: 200_000,
		Post:          poster.Post,
		Token:         "s3cret",
	})
	t.Cleanup(func() { _ = local.Close() })

	// 走原始连接，自己读第一条回包。
	//
	// 注意：执行端的处理是在 post 队列上执行的，读回包的同时必须 pump，
	// 否则会自己等自己（这条测试就是被这个坑卡住过一次）。
	viewerConn, execConn := wire.PipePair()
	go func() { _ = host.Serve(execConn, local, poster.Post) }()
	defer viewerConn.Close()

	hello, _ := wire.Encode(wire.Hello{Protocol: wire.Protocol, Role: "viewer", Purpose: "control"})
	if err := viewerConn.Send(wire.Packet{Kind: "hello", Body: hello}); err != nil {
		t.Fatal(err)
	}
	type recvResult struct {
		packet wire.Packet
		err    error
	}
	recvCh := make(chan recvResult, 1)
	go func() {
		packet, recvErr := viewerConn.Recv()
		recvCh <- recvResult{packet: packet, err: recvErr}
	}()
	var reply wire.Packet
	for {
		poster.pump()
		select {
		case result := <-recvCh:
			if result.err != nil {
				t.Fatalf("没收到回包：%v", result.err)
			}
			reply = result.packet
		default:
			time.Sleep(2 * time.Millisecond)
			continue
		}
		break
	}
	if reply.Kind != "welcome" {
		t.Fatalf("期望 welcome，实际 %s", reply.Kind)
	}
	var welcome wire.Welcome
	if err := wire.Decode(reply.Body, &welcome); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(welcome.Error, "token") {
		t.Fatalf("拒绝原因里应说明 token 问题，实际：%q", welcome.Error)
	}
}

// tokenRuntime 只满足接口，测试里不会真的开流。
type tokenRuntime struct{}

func (r *tokenRuntime) OpenStream(ctx context.Context, sel providers.Selection, req providers.ChatRequest) (<-chan providers.Event, error) {
	ch := make(chan providers.Event)
	close(ch)
	return ch, nil
}

func (r *tokenRuntime) RunTools(ctx context.Context, tc *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
	return nil
}

// tokenPoster 把回调投到测试线程（等价于桌面的主线程）。
type tokenPoster struct {
	mu    sync.Mutex
	queue []func()
}

func newTokenPoster() *tokenPoster { return &tokenPoster{} }

func (p *tokenPoster) Post(fn func()) {
	p.mu.Lock()
	p.queue = append(p.queue, fn)
	p.mu.Unlock()
}

func (p *tokenPoster) pump() {
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

func pumpTokenUntil(t *testing.T, p *tokenPoster, done func() bool) {
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
