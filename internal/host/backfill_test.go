package host_test

// backfill 的回归测试：首次 attach 只给最近若干条事件，翻页翻到镜像最早时
// 必须能向执行端要到更早的那一段 —— 否则界面会谎报"已经是最早的内容了"，
// 而磁盘上其实还有更多。

import (
	"testing"

	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
)

// persistentHost 造一个**有磁盘日志**的执行端（backfill 只能从 journal 读）。
func persistentHost(t *testing.T) (*host.Local, *host.Session) {
	t.Helper()
	dir := t.TempDir()
	rt := &fakeRuntime{}
	local := host.NewLocal(host.Config{
		Runtime:       rt,
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Workspace:     dir,
		MaxToolRounds: 3,
		ContextWindow: 200_000,
		Post:          func(fn func()) { fn() },
		Catalog:       store.NewCatalog(dir),
	})
	t.Cleanup(func() { _ = local.Close() })
	session := local.NewSession("")
	if session == nil {
		t.Fatal("新建会话失败")
	}
	return local, session
}

// TestBackfillReturnsEarlierEvents 验证 backfill 能取到 attach 没下发的那段。
func TestBackfillReturnsEarlierEvents(t *testing.T) {
	_, session := persistentHost(t)

	for i := 0; i < 20; i++ {
		if err := host.AppendUserMessageForTest(session, "问题"); err != nil {
			t.Fatalf("追加第 %d 条失败：%v", i, err)
		}
	}

	all := host.EventsForTest(session)
	if len(all) < 5 {
		t.Skipf("事件太少（%d 条），不足以验证", len(all))
	}
	mid := all[len(all)/2].Seq

	page, err := host.BackfillForTest(session, mid, 100)
	if err != nil {
		t.Fatalf("backfill 失败：%v", err)
	}
	if len(page.Events) == 0 {
		t.Fatalf("mid=%d 之前应该还有事件，backfill 却返回空", mid)
	}
	for _, event := range page.Events {
		if event.Seq >= mid {
			t.Fatalf("返回了不早于游标的事件：seq=%d >= before=%d", event.Seq, mid)
		}
	}
}

// TestBackfillStopsAtOldest 验证真的到最早时返回空且 HasMore=false。
func TestBackfillStopsAtOldest(t *testing.T) {
	_, session := persistentHost(t)
	for i := 0; i < 3; i++ {
		_ = host.AppendUserMessageForTest(session, "问题")
	}

	// before = 1 表示"只要 seq < 1"，也就是什么都没有
	page, err := host.BackfillForTest(session, 1, 100)
	if err != nil {
		t.Fatalf("backfill 失败：%v", err)
	}
	if len(page.Events) != 0 {
		t.Fatalf("seq<1 不该有事件，实际 %d 条", len(page.Events))
	}
	if page.HasMore {
		t.Fatal("已经到最早，HasMore 应为 false")
	}
}

// TestBackfillLimitsPageSize 验证一次不会倒出全部历史。
func TestBackfillLimitsPageSize(t *testing.T) {
	_, session := persistentHost(t)
	for i := 0; i < 20; i++ {
		_ = host.AppendUserMessageForTest(session, "问题")
	}
	all := host.EventsForTest(session)
	if len(all) < 10 {
		t.Skipf("事件太少（%d 条）", len(all))
	}
	newest := all[len(all)-1].Seq

	page, err := host.BackfillForTest(session, newest, 5)
	if err != nil {
		t.Fatalf("backfill 失败：%v", err)
	}
	if len(page.Events) > 5 {
		t.Fatalf("limit=5 却返回了 %d 条", len(page.Events))
	}
	if !page.HasMore {
		t.Fatal("截断过了，HasMore 应为 true（还能继续往前翻）")
	}
}