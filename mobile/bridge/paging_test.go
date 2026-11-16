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

// pageView 是 TimelinePageJSON 的测试镜像（seq 游标版）。
type pageView struct {
	SessionID string              `json:"session_id"`
	Items     []bridge.ItemView   `json:"items"`
	HasMore   bool                `json:"has_more"`
	SeqFrom   uint64              `json:"seq_from"`
	SeqTo     uint64              `json:"seq_to"`
	Runtime   *bridge.RuntimeView `json:"runtime"`
}

// 分页按 seq 切：最后一页给尾部，靠 seq_from 一路往前翻，不重不漏。
func TestTimelinePagingBySeq(t *testing.T) {
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
	// 注意：首次 attach 只下发最近 attachTailEvents（200）条事件，
	// 所以这里能看到的 item 数远少于日志总量——这正是分页存在的理由。
	// 有 50 条以上就足够验证多页。
	if len(full.Items) < 50 {
		t.Skipf("样本太短（%d 条），不足以验证分页", len(full.Items))
	}

	// 第一页
	const limit = 40
	firstJSON := client.TimelinePageJSON(target, 0, limit)
	var page1 pageView
	if err := json.Unmarshal([]byte(firstJSON), &page1); err != nil {
		t.Fatalf("第一页解析失败：%v", err)
	}
	t.Logf("完整：%d KB / %d items；第一页：%d KB / %d items（seq %d..%d）",
		len(fullJSON)/1024, len(full.Items), len(firstJSON)/1024,
		len(page1.Items), page1.SeqFrom, page1.SeqTo)

	if len(page1.Items) == 0 {
		t.Fatal("第一页不该为空")
	}
	if !page1.HasMore {
		t.Fatal("还有更早内容，has_more 应为 true")
	}
	if page1.Runtime == nil {
		t.Fatal("第一页必须带 runtime（当前状态）")
	}
	// 第一页必须是最后若干条
	if got := page1.Items[len(page1.Items)-1].Key; got != full.Items[len(full.Items)-1].Key {
		t.Fatalf("第一页末条应为整个时间线末条，期望 %s 实际 %s",
			full.Items[len(full.Items)-1].Key, got)
	}

	// 第二页：用 seq_from 往前翻
	secondJSON := client.TimelinePageJSON(target, page1.SeqFrom, limit)
	var page2 pageView
	if err := json.Unmarshal([]byte(secondJSON), &page2); err != nil {
		t.Fatalf("第二页解析失败：%v", err)
	}
	if len(page2.Items) == 0 {
		t.Fatal("第二页不该为空（还有更早内容）")
	}
	if page2.Runtime != nil {
		t.Fatal("历史页不该带 runtime（会覆盖当前状态）")
	}
	if page2.SeqTo >= page1.SeqFrom+1 {
		t.Fatalf("第二页的 seq_to（%d）应小于第一页的 seq_from（%d），否则会重复",
			page2.SeqTo, page1.SeqFrom)
	}
	// 两页内容不能重叠
	firstKeys := map[string]bool{}
	for _, item := range page1.Items {
		firstKeys[item.Key] = true
	}
	for _, item := range page2.Items {
		if firstKeys[item.Key] {
			t.Fatalf("第二页与第一页出现重复条目：%s", item.Key)
		}
	}

	// 一路翻到最早：累计条数等于总数、无重复
	seen := map[string]bool{}
	for _, item := range page1.Items {
		seen[item.Key] = true
	}
	cursor := page1.SeqFrom
	pages := 1
	for i := 0; i < 200; i++ {
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
		if !page.HasMore || len(page.Items) == 0 || page.SeqFrom >= cursor {
			break
		}
		cursor = page.SeqFrom
	}
	if len(seen) != len(full.Items) {
		t.Fatalf("翻完所有页应覆盖 %d 条，实际 %d 条（共 %d 页）",
			len(full.Items), len(seen), pages)
	}
	t.Logf("共 %d 页，覆盖全部 %d 条，无重复", pages, len(seen))
}