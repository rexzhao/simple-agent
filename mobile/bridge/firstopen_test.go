package bridge_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/wire"
	"simple-agent/mobile/bridge"
)

// 量化"首次打开会话"的端到端耗时与数据量。
//
// 用户反馈：打开长会话时白屏/转圈很久。这里分三段量：
//  1. 执行端侧 attach 出多少数据（log.events 的体积）
//  2. 展示端重建 timeline 要多久
//  3. TimelineJSON 跨 JNI 的体积
//
// 用真实会话文件：SIMPLE_AGENT_PERF_SESSION=/path/to/session.jsonl
func TestFirstOpenCost(t *testing.T) {
	if testing.Short() {
		t.Skip("手写计时，短模式跳过")
	}
	path := os.Getenv("SIMPLE_AGENT_PERF_SESSION")
	if path == "" {
		t.Skip("没有指定真实会话文件（SIMPLE_AGENT_PERF_SESSION）")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("读不到 %s：%v", path, err)
	}
	t.Logf("会话文件：%.2f MB", float64(len(content))/(1024*1024))

	// 复制到临时 data_dir
	dataDir := t.TempDir()
	sessionID := filepath.Base(filepath.Dir(path))
	dstDir := filepath.Join(dataDir, "sessions", sessionID)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "session.jsonl"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	local := host.NewLocal(host.Config{
		Runtime:       &fakeRuntime{},
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Workspace:     dataDir,
		MaxToolRounds: 4,
		ContextWindow: 200_000,
		Catalog:       store.NewCatalog(dataDir),
		Post:          func(fn func()) { fn() },
	})
	t.Cleanup(func() { _ = local.Close() })

	sessions := local.Sessions()
	if len(sessions) == 0 {
		t.Fatal("没有恢复出会话")
	}
	target := sessions[0].ID
	entries := local.Session(target).Conversation().Entries()
	t.Logf("会话条目数：%d", len(entries))

	// 段 1：执行端 attach 下发的数据量
	start := time.Now()
	attachResult, ok := local.Attach(target, "", ^uint64(0), 0)
	attachCost := time.Since(start)
	if !ok {
		t.Fatal("attach 失败")
	}
	eventsJSON, _ := wire.Encode(map[string]any{"events": attachResult.Events})
	t.Logf("attach：%d 条事件，%.1f KB，耗时 %v",
		len(attachResult.Events), float64(len(eventsJSON))/1024, attachCost)

	// 段 2+3：接一个展示端，量首次 TimelineJSON
	poster := &serialPoster{}
	viewerConn, execConn := wire.PipePair()
	go func() { _ = host.Serve(execConn, local, poster.post) }()
	remote, err := host.NewRemote(viewerConn, poster.post)
	if err != nil {
		t.Fatal(err)
	}
	client := bridge.NewClient(&listener{}, "zh", t.TempDir())
	t.Cleanup(client.Close)
	bridge.AttachForTest(client, remote)

	// 等首次同步完成
	deadline := time.Now().Add(30 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		poster.pump()
		time.Sleep(5 * time.Millisecond)
		body = client.TimelineJSON(target)
		if len(body) > 2048 {
			break
		}
	}
	t.Logf("首次 TimelineJSON：%.1f KB", float64(len(body))/1024)

	const rounds = 10
	start = time.Now()
	for i := 0; i < rounds; i++ {
		_ = client.TimelineJSON(target)
	}
	t.Logf("单次 TimelineJSON：%v", time.Since(start)/rounds)

	fmt.Println()
}