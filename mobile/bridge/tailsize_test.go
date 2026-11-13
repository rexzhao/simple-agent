package bridge_test

import (
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/wire"
	"simple-agent/mobile/bridge"
)

// 量一下"只传最后 N 条 item"能省多少，用来判断是否值得做 timeline 分页。
func TestTailItemSizes(t *testing.T) {
	if testing.Short() {
		t.Skip("手写计时，短模式跳过")
	}
	path := os.Getenv("SIMPLE_AGENT_PERF_SESSION")
	if path == "" {
		t.Skip("没有指定真实会话文件")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("读不到：%v", err)
	}
	dataDir := t.TempDir()
	sessionID := filepath.Base(filepath.Dir(path))
	dst := filepath.Join(dataDir, "sessions", sessionID)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "session.jsonl"), content, 0o644); err != nil {
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

	deadline := 0
	for deadline < 6000 {
		poster.pump()
		deadline++
		if len(client.TimelineJSON(target)) > 2048 {
			break
		}
	}

	full := client.TimelineJSON(target)
	t.Logf("完整 timeline：%.1f KB", float64(len(full))/1024)

	// 按 item 估算：取 JSON 里每个 item 的平均体积 × 常见一屏条数
	var view bridge.TimelineView
	_ = json.Unmarshal([]byte(full), &view)
	t.Logf("item 总数：%d", len(view.Items))
	if len(view.Items) > 0 {
		avg := float64(len(full)) / float64(len(view.Items))
		t.Logf("平均每个 item：%.1f KB", avg/1024)
		for _, n := range []int{20, 40, 60} {
			t.Logf("最后 %d 条估算：%.1f KB", n, avg*float64(n)/1024)
		}
	}
}
