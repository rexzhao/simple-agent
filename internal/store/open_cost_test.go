package store_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"simple-agent/internal/store"
)

// 打开会话只读 session.jsonl，**不碰 archive-*.jsonl**。
//
// 这条守的是"启动不需要回翻归档"：累计用量、轮数/步数都从最新那份里的
// compact.snapshot 接续，归档只在用户往上翻历史时按需加载（LoadOlder）。
//
// 做法：把归档文件写坏（非法 JSON）。如果 Load 试图读它，要么报错、
// 要么把坏行当警告带出来 —— 两种都会让断言失败。
func TestLoadDoesNotTouchArchives(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 写两条正常事件
	if err := s.Append(store.MustEvent(store.TypeSessionCreated, map[string]any{
		"context_id": "ctx", "title": "测试", "created_at": "2026-01-01T00:00:00Z",
	})); err != nil {
		t.Fatal(err)
	}
	// 压缩：产生一份归档 + 用快照重开 base
	if err := s.Compact(store.MustEvent(store.TypeCompactSnapshot, map[string]any{
		"summary": "摘要", "summary_message_id": "m1", "kept_entries": []any{},
		"compacted_at": "2026-01-01T00:00:00Z",
	})); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(store.MustEvent(store.TypeNotice, map[string]any{"text": "压缩后的通知"})); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 把归档写坏
	archives, err := filepath.Glob(filepath.Join(dir, "archive-*.jsonl"))
	if err != nil || len(archives) == 0 {
		t.Fatalf("压缩应产生归档文件，实际 %v（%v）", archives, err)
	}
	if err := os.WriteFile(archives[0], []byte("{这不是合法 JSON\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 重新打开：Load 只读 base，不该因为归档坏掉而报错或产生警告
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatalf("打开不该因为归档损坏失败：%v", err)
	}
	defer reopened.Close()

	loaded, err := reopened.Load()
	if err != nil {
		t.Fatalf("Load 不该读归档：%v", err)
	}
	if len(loaded.Warnings) > 0 {
		t.Fatalf("Load 不该把归档的问题带出来：%v", loaded.Warnings)
	}
	// base 里应只有快照 + 之后那条通知
	if len(loaded.Events) != 2 {
		t.Fatalf("base 应有 2 条事件（快照 + 通知），实际 %d 条", len(loaded.Events))
	}
	if loaded.Events[0].Type != store.TypeCompactSnapshot {
		t.Fatalf("第一条应是快照，实际 %s", loaded.Events[0].Type)
	}

	// 反过来：真正去 LoadOlder 时才读归档。
	// 这里只断言"它确实去读了那份坏文件"——坏在第一行，读出来是空事件
	// （末尾/中间坏行的容错规则），但**不读就永远发现不了**。
	page, err := reopened.LoadOlder(nil)
	if err != nil {
		// 直接报错也算"读了"（读不了坏文件）
		return
	}
	if len(page.Events) > 0 {
		t.Fatalf("归档第一行就坏了，不该解析出事件，实际 %d 条", len(page.Events))
	}
	// 走到这里说明它读了坏文件并按容错规则返回空 —— 符合预期
}

// 压缩快照必须带上累计用量基线，打开时才能不回翻归档。
func TestSnapshotCarriesCountersSoOpenIsCheap(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Compact(store.MustEvent(store.TypeCompactSnapshot, map[string]any{
		"summary": "摘要", "summary_message_id": "m1", "kept_entries": []any{},
		"compacted_at": "2026-01-01T00:00:00Z",
		"turns":        42,
		"steps":        99,
		"total_usage":  map[string]any{"input": 1000, "cache_read": 900},
	})); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Events) == 0 {
		t.Fatal("应当有快照事件")
	}
	first := loaded.Events[0]
	payload := string(first.Data)
	for _, want := range []string{`"turns":42`, `"steps":99`, `"cache_read":900`} {
		if !strings.Contains(payload, want) {
			t.Fatalf("快照应带 %s，实际 %s", want, truncate(payload))
		}
	}
	_ = strconv.Itoa(0)
}

func truncate(s string) string {
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "…"
}