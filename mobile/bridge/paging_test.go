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

// pageView 是 TimelinePageJSON 的测试镜像。
type pageView struct {
	SessionID string              `json:"session_id"`
	Items     []bridge.ItemView   `json:"items"`
	HasMore   bool                `json:"has_more"`
	FirstKey  string              `json:"first_key"`
	Runtime   *bridge.RuntimeView `json:"runtime"`
}

// 分页：最后一页只给尾部，且能靠 first_key 一路往前翻，不重不漏。
func TestTimelinePaging(t *testing.T) {
	if testing.Short() {
		t.Skip("需要真实会话文件")
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

	// 接展示端，等同步完成
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

	fullJSON := client.TimelineJSON(target)
	var full bridge.TimelineView
	if err := json.Unmarshal([]byte(fullJSON), &full); err != nil {
		t.Fatal(err)
	}
	if len(full.Items) < 100 {
		t.Skipf("样本太短（%d 条），不足以验证分页", len(full.Items))
	}

	// 第一页：最后 40 条
	const limit = 40
	firstPageJSON := client.TimelinePageJSON(target, "", limit)
	var page1 pageView
	if err := json.Unmarshal([]byte(firstPageJSON), &page1); err != nil {
		t.Fatalf("第一页解析失败：%v", err)
	}
	t.Logf("完整：%d KB / %d items；第一页：%d KB / %d items",
		len(fullJSON)/1024, len(full.Items), len(firstPageJSON)/1024, len(page1.Items))

	if len(page1.Items) != limit {
		t.Fatalf("第一页应有 %d 条，实际 %d", limit, len(page1.Items))
	}
	if !page1.HasMore {
		t.Fatal("第一页之后还有内容，has_more 应为 true")
	}
	if page1.Runtime == nil {
		t.Fatal("第一页必须带 runtime（当前状态）")
	}
	// 第一页必须是**最后** limit 条
	for i := 0; i < limit; i++ {
		want := full.Items[len(full.Items)-limit+i].Key
		if page1.Items[i].Key != want {
			t.Fatalf("第一页第 %d 条 key 不符：期望 %s 实际 %s", i, want, page1.Items[i].Key)
		}
	}

	// 第二页：靠 first_key 往前翻，必须接上
	secondPageJSON := client.TimelinePageJSON(target, page1.FirstKey, limit)
	var page2 pageView
	if err := json.Unmarshal([]byte(secondPageJSON), &page2); err != nil {
		t.Fatalf("第二页解析失败：%v", err)
	}
	if len(page2.Items) != limit {
		t.Fatalf("第二页应有 %d 条，实际 %d", limit, len(page2.Items))
	}
	if page2.Runtime != nil {
		t.Fatal("历史页不该带 runtime（会覆盖当前状态）")
	}
	// 第二页的最后一条必须紧邻第一页第一条
	firstOfPage1 := full.Items[len(full.Items)-limit].Key
	if got := page2.Items[len(page2.Items)-1].Key; got != full.Items[len(full.Items)-2*limit].Key {
		t.Fatalf("第二页末尾应紧邻第一页开头：期望 %s 实际 %s", full.Items[len(full.Items)-2*limit].Key, got)
	}
	if firstOfPage1 == "" {
		t.Fatal("first_key 不该为空")
	}

	// 一路翻到最早：累计条数必须等于总数，且无重复
	seen := map[string]bool{}
	for _, item := range page1.Items {
		seen[item.Key] = true
	}
	cursor := page1.FirstKey
	pages := 1
	for i := 0; i < 100; i++ {
		body := client.TimelinePageJSON(target, cursor, limit)
		var page pageView
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatalf("翻页解析失败：%v", err)
		}
		pages++
		for _, item := range page.Items {
			if seen[item.Key] {
				t.Fatalf("翻页出现重复 item：%s", item.Key)
			}
			seen[item.Key] = true
		}
		if !page.HasMore || len(page.Items) == 0 {
			break
		}
		cursor = page.FirstKey
	}
	if len(seen) != len(full.Items) {
		t.Fatalf("翻完所有页应覆盖 %d 条，实际 %d 条（共 %d 页）", len(full.Items), len(seen), pages)
	}
	t.Logf("共 %d 页，覆盖全部 %d 条，无重复", pages, len(seen))
}