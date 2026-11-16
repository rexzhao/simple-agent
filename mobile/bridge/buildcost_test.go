package bridge_test

import (
	json "encoding/json/v2"
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

// 量化阶段 8 要解决的问题：Build（构建）与序列化各占多少。
//
// 如果 Build 占比很小，说明增量改造不值得（瓶颈在传输，已由分页解决）。
func TestBuildVsEncodeCost(t *testing.T) {
	path := os.Getenv("SIMPLE_AGENT_PERF_SESSION")
	if path == "" {
		t.Skip("没有指定 SIMPLE_AGENT_PERF_SESSION")
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
	for i := 0; i < 6000; i++ {
		poster.pump()
		if len(client.TimelineJSON(target)) > 2048 {
			break
		}
	}

	// 完整时间线：构建 + 序列化
	full := client.TimelineJSON(target)
	var view bridge.TimelineView
	if err := json.Unmarshal([]byte(full), &view); err != nil {
		t.Fatal(err)
	}
	t.Logf("完整：%d items，JSON %.1f KB", len(view.Items), float64(len(full))/1024)

	const rounds = 10
	// A：完整路径（构建 + 序列化）
	start := time.Now()
	for i := 0; i < rounds; i++ {
		_ = client.TimelineJSON(target)
	}
	fullCost := time.Since(start) / rounds

	// B：分页路径（构建 + 序列化 60 条）
	start = time.Now()
	for i := 0; i < rounds; i++ {
		_ = client.TimelinePageJSON(target, 0, 60)
	}
	pageCost := time.Since(start) / rounds

	// C：只序列化已有视图（不含构建）—— 用分页结果反推
	small := client.TimelinePageJSON(target, 0, 60)
	start = time.Now()
	for i := 0; i < rounds; i++ {
		_, _ = json.Marshal(view)
	}
	encodeOnly := time.Since(start) / rounds

	t.Logf("完整 TimelineJSON：%v", fullCost)
	t.Logf("分页 60 条：%v（%.1f KB）", pageCost, float64(len(small))/1024)
	t.Logf("仅序列化完整视图：%v", encodeOnly)
	t.Logf("推算构建耗时：%v", fullCost-encodeOnly)
}