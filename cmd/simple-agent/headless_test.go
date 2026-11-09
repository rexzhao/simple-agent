package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple-agent/internal/config"
	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
	"simple-agent/internal/wire"
)

// headlessRuntime 每回合直接给一句回答（测试用）。
type headlessRuntime struct{}

func (r *headlessRuntime) OpenStream(_ context.Context, _ providers.Selection, _ providers.ChatRequest) (<-chan providers.Event, error) {
	events := make(chan providers.Event, 4)
	go func() {
		events <- providers.Event{Kind: providers.EventText, Text: "无界面回答"}
		events <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
		close(events)
	}()
	return events, nil
}

func (r *headlessRuntime) RunTools(_ context.Context, _ *tools.ToolContext, _ []tools.Call) []tools.ToolOutcome {
	return nil
}

// 无界面执行端的生命周期：监听 → 展示端发消息 → 停止 → 关闭监听与会话日志。
func TestHeadlessLifecycle(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)
	blobs, err := store.NewBlobs(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	// post 必须串行（真实界面是主线程）；并发执行会让镜像 map 竞态
	post := serialPost()

	local := host.NewLocal(host.Config{
		Catalog: catalog, Blobs: blobs, Runtime: &headlessRuntime{},
		Selection:    providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: post,
	})
	if local.Active() == nil {
		local.NewSession("")
	}
	sess := local.Active().ID

	stop, addr, err := listenHeadless("127.0.0.1:0", local, post)
	if err != nil {
		t.Fatal(err)
	}
	if addr == "" {
		t.Fatal("没有拿到监听地址")
	}

	conn, err := wire.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := host.NewRemote(conn, post)
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	waitFor(t, "展示端连上", func() bool { return viewer.Connected() })

	payload, _ := host.EncodePayload(host.SendPayload{Text: "在吗"})
	if _, err := viewer.Submit(host.Command{SessionID: sess, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	// 会话状态只在该会话自己的线程（这里是 serialPost 队列）上读
	done := make(chan bool, 1)
	for i := 0; i < 500; i++ {
		post(func() {
			session := local.Session(sess)
			done <- session != nil && len(session.Conversation().Entries()) > 0 &&
				session.Busy() == driver.BusyIdle
		})
		if <-done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 收到 SIGTERM 后的顺序：先停回合 → Sync → 关监听 → 退出
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	stop()
	waitFor(t, "展示端收到断开", func() bool { return !viewer.Connected() })

	// 会话日志确实落盘了（Sync 之后可读）
	path := filepath.Join(catalog.SessionsDir(), sess, "session.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("会话日志是空的：没有 Sync")
	}
}

// 新开关的解析：relay 子命令、--relay、--route、--headless。
func TestParseRemoteOptions(t *testing.T) {
	args := []string{"relay", "--listen", "127.0.0.1:7900"}
	opts := parseRemoteOptions(&args)
	if !opts.relayServer || opts.listen != "127.0.0.1:7900" || len(args) != 0 {
		t.Fatalf("relay 子命令解析不对：%+v args=%v", opts, args)
	}

	args = []string{"--headless", "--relay", "127.0.0.1:7900", "--route", "host-1", "--config", "x.yaml"}
	opts = parseRemoteOptions(&args)
	if !opts.headless || opts.relay != "127.0.0.1:7900" || opts.route != "host-1" {
		t.Fatalf("headless/relay 解析不对：%+v", opts)
	}
	if len(args) != 2 || args[0] != "--config" {
		t.Fatalf("其余参数应保留：%v", args)
	}
}

func waitFor(t *testing.T, name string, done func() bool) {
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

// --allow-shell 必须传到 --headless 的执行端（曾经漏传，工具列表里压根没有 bash）。
func TestHeadlessHostConfigCarriesAllowShell(t *testing.T) {
	for _, allow := range []bool{true, false} {
		cfg := &config.Config{
			AllowShell: allow,
			Workspace:  t.TempDir(),
		}
		runtime := &appRuntime{allowShell: allow}
		local := host.NewLocal(headlessHostConfig(cfg, runtime,
			providers.Selection{ProviderID: "fake", Model: "m"}, nil, nil,
			func(f func()) { f() }))
		local.Close()

		// 空会话不会自动创建（没有 catalog），这里断言的是配置透传后
		// 工具上下文与工具列表的可见效果。
		session := local.NewSession("")
		if got := session.ToolContext().AllowShell; got != allow {
			t.Fatalf("AllowShell=%v 时工具上下文是 %v：--allow-shell 在 headless 下丢了", allow, got)
		}
		hasBash := false
		for _, tool := range tools.Builtin(allow) {
			if tool.Name == "bash" {
				hasBash = true
			}
		}
		if hasBash != allow {
			t.Fatalf("AllowShell=%v 时工具列表里的 bash 存在性为 %v", allow, hasBash)
		}
	}
}

// headless 的系统提示与权限必须同源：提示里不能出现"用不了的命令工具"。
//
// 现在的判定比"提到命令执行"更具体：工具指导是**从已注册工具里取的**，
// 所以关掉 shell 时不该出现 bash/pwsh 的指导段落。
func TestHeadlessSystemPromptMatchesAllowShell(t *testing.T) {
	for _, allow := range []bool{true, false} {
		cfg := &config.Config{AllowShell: allow, Workspace: t.TempDir()}
		prompt := systemPrompt(*cfg)

		hasShellGuidance := strings.Contains(prompt, "[exit code: N]")
		if hasShellGuidance != allow {
			t.Fatalf("AllowShell=%v 时命令工具指导存在=%v：\n%s", allow, hasShellGuidance, prompt)
		}
		// 文件工具指导两种情况都该在
		if !strings.Contains(prompt, "Use the read tool") {
			t.Fatalf("AllowShell=%v 时缺少 read 指导：\n%s", allow, prompt)
		}
		// 关掉时要有明确提示（避免模型反复试错）
		mentionsDisabled := strings.Contains(prompt, "命令执行当前**不可用**")
		if mentionsDisabled == allow {
			t.Fatalf("AllowShell=%v 时「不可用」提示存在=%v：\n%s", allow, mentionsDisabled, prompt)
		}
	}
}

// 用户追加的 system_prompt 要排在最后（在工具指导之后）。
func TestSystemPromptAppendsUserPromptLast(t *testing.T) {
	cfg := &config.Config{Workspace: t.TempDir(), SystemPrompt: "回答尽量简短。"}
	prompt := systemPrompt(*cfg)
	guidanceAt := strings.Index(prompt, "Use the read tool")
	userAt := strings.Index(prompt, "回答尽量简短。")
	if guidanceAt < 0 || userAt < 0 {
		t.Fatalf("提示词缺少必要段落：\n%s", prompt)
	}
	if userAt < guidanceAt {
		t.Fatalf("用户追加内容应排在工具指导之后：\n%s", prompt)
	}
}

// 「监听非回环地址」的警告判定。
func TestShouldWarnUnprotected(t *testing.T) {
	cases := []struct {
		addr string
		warn bool
	}{
		{"127.0.0.1:7801", false},
		{"localhost:7801", false},
		{"[::1]:7801", false},
		{":7801", true},        // 空 host = 所有网卡
		{"0.0.0.0:7801", true}, // 所有网卡
		{"192.168.1.2:7801", true},
		{"[::]:7801", true},
	}
	for _, c := range cases {
		if got := shouldWarnUnprotected(c.addr); got != c.warn {
			t.Errorf("%s：期望 warn=%v，实际 %v", c.addr, c.warn, got)
		}
	}
}
