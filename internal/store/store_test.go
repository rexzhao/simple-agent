package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"simple-agent/internal/store"
)

func mustEvent(t *testing.T, kind string, data any) store.Event {
	t.Helper()
	event, err := store.NewEvent(kind, data)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

// 打开 → 追加 → 重新打开 → 事件都在，seq 连续。
func TestJsonlAppendAndReload(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Append(mustEvent(t, store.TypeSessionCreated, map[string]any{"i": i})); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	result, err := s2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 3 {
		t.Fatalf("应有 3 条事件，实际 %d", len(result.Events))
	}
	for i, event := range result.Events {
		if event.Seq != uint64(i) {
			t.Fatalf("第 %d 条 seq 应为 %d，实际 %d", i, i, event.Seq)
		}
		if event.Version != store.Version {
			t.Fatalf("版本应补全：%d", event.Version)
		}
	}

	// 重新打开后继续追加，seq 接着走
	if err := s2.Append(mustEvent(t, store.TypeSessionRenamed, map[string]any{"title": "x"})); err != nil {
		t.Fatal(err)
	}
	result, _ = s2.Load()
	if last := result.Events[len(result.Events)-1]; last.Seq != 3 {
		t.Fatalf("续写 seq 应接上，实际 %d", last.Seq)
	}
}

// 末尾半行（写入被中断）→ 丢弃该行，其余完整。
func TestToleratesTruncatedLastLine(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Append(mustEvent(t, store.TypeMessageAppended, map[string]any{"i": i})); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	// 人为截断最后一行
	path := filepath.Join(dir, "session.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)-8], 0o644); err != nil {
		t.Fatal(err)
	}

	s2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("半行不应阻止打开：%v", err)
	}
	defer s2.Close()
	result, err := s2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 2 {
		t.Fatalf("应保留 2 条完整事件，实际 %d", len(result.Events))
	}
	// 修复发生在打开时：加载到的是修好的文件，问题通过 warnings 告知
	if len(result.Warnings) == 0 {
		t.Fatal("应提示末尾半行已被截掉")
	}
}

// 中间行损坏 → 记警告并在损坏点截断。
func TestCorruptMiddleLineTruncates(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := s.Append(mustEvent(t, store.TypeMessageAppended, map[string]any{"i": i})); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	path := filepath.Join(dir, "session.jsonl")
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	lines[1] = `{"v":1,"seq":1,"type":` // 坏行
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("应在损坏点截断（只留第 1 条），实际 %d", len(result.Events))
	}
	if len(result.Warnings) == 0 || !strings.Contains(strings.Join(result.Warnings, " "), "损坏") {
		t.Fatalf("中间坏行应产生警告：%v", result.Warnings)
	}
	// 原文件已留档
	entries, _ := os.ReadDir(dir)
	backup := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "session.corrupt-") {
			backup = true
		}
	}
	if !backup {
		t.Fatal("损坏的原文件应留档")
	}
	// 截断之后仍可继续写（seq 接着最后一条有效事件）
	if err := s2.Append(mustEvent(t, store.TypeNotice, map[string]any{"text": "hi"})); err != nil {
		t.Fatal(err)
	}
	s2.Close()
}

// seq 跳变 → 截断并警告。
func TestSeqGapDetected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	content := `{"v":1,"seq":0,"at":"2026-10-27T00:00:00Z","type":"session.created","data":{}}
{"v":1,"seq":5,"at":"2026-10-27T00:00:01Z","type":"session.renamed","data":{}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	result, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("序号跳变应截断到 1 条，实际 %d", len(result.Events))
	}
	if len(result.Warnings) == 0 {
		t.Fatalf("应提示序号问题：%v", result.Warnings)
	}
}

// 滚动：归档存在、当前文件只剩快照、编号递增。
func TestCompactRollsArchive(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 3; i++ {
		if err := s.Append(mustEvent(t, store.TypeMessageAppended, map[string]any{"i": i})); err != nil {
			t.Fatal(err)
		}
	}

	snapshot := mustEvent(t, store.TypeCompactSnapshot, map[string]any{"summary": "摘要"})
	if err := s.Compact(snapshot); err != nil {
		t.Fatalf("滚动失败：%v", err)
	}

	// 归档包含滚动之前的全部事件
	page, err := s.LoadOlder(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 3 {
		t.Fatalf("归档应有 3 条，实际 %d", len(page.Events))
	}
	if page.Next != nil {
		t.Fatal("只有一份归档时不应有更早的")
	}

	// 当前文件只有快照，且 seq 从 0 开始
	result, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 || result.Events[0].Type != store.TypeCompactSnapshot {
		t.Fatalf("当前文件应只剩快照：%#v", result.Events)
	}

	// 之后追加从 seq=1 开始
	if err := s.Append(mustEvent(t, store.TypeMessageAppended, map[string]any{"after": true})); err != nil {
		t.Fatal(err)
	}
	result, _ = s.Load()
	if last := result.Events[len(result.Events)-1]; last.Seq != 1 {
		t.Fatalf("压缩后 seq 应从 1 开始，实际 %d", last.Seq)
	}

	// 第二次滚动：编号 0002，并可以分页往回读
	if err := s.Compact(mustEvent(t, store.TypeCompactSnapshot, map[string]any{"summary": "第二份"})); err != nil {
		t.Fatal(err)
	}
	page, _ = s.LoadOlder(nil)
	if page.Next == nil {
		t.Fatal("应能继续往前读")
	}
	older, _ := s.LoadOlder(page.Next)
	if len(older.Events) != 3 {
		t.Fatalf("更早的归档应是第一次滚动的内容，实际 %d 条", len(older.Events))
	}
	if older.Next != nil {
		t.Fatal("读到头时 Next 应为 nil")
	}
}

// 崩溃点：tmp 残留 → 启动时清理，且当前文件仍是合法的旧内容。
func TestStartupCleansStaleTmp(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(mustEvent(t, store.TypeMessageAppended, map[string]any{"i": 0})); err != nil {
		t.Fatal(err)
	}
	s.Close()

	tmp := filepath.Join(dir, "session.jsonl.tmp")
	if err := os.WriteFile(tmp, []byte(`{"v":1,"seq":0,"type":"compact.snapshot","data":{"summary":"x"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("残留 tmp 应被清理")
	}
	result, _ := s2.Load()
	if len(result.Events) != 1 || result.Events[0].Type != store.TypeMessageAppended {
		t.Fatalf("当前文件应保持旧内容：%#v", result.Events)
	}
}

// 归档已存在时滚动要幂等（不覆盖已有归档）。
func TestCompactReusesExistingArchive(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(mustEvent(t, store.TypeMessageAppended, map[string]any{"i": 0})); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(mustEvent(t, store.TypeCompactSnapshot, map[string]any{"summary": "一"})); err != nil {
		t.Fatal(err)
	}
	// 手工把归档序号占住（模拟「1 之后崩溃，序号已存在」）
	s.Close()

	// 再打开时 scanArchiveSeq 会看到 0001，下次滚动用 0002
	s2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.Append(mustEvent(t, store.TypeMessageAppended, map[string]any{"i": 1})); err != nil {
		t.Fatal(err)
	}
	if err := s2.Compact(mustEvent(t, store.TypeCompactSnapshot, map[string]any{"summary": "二"})); err != nil {
		t.Fatal(err)
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var archives []string
	for _, entry := range names {
		if strings.HasPrefix(entry.Name(), "archive-") {
			archives = append(archives, entry.Name())
		}
	}
	if len(archives) != 2 {
		t.Fatalf("应有两份归档，实际 %v", archives)
	}
}

// 内存后端与文件后端对同一事件序列给出同样的加载结果。
func TestMemoryStoreMatchesJsonl(t *testing.T) {
	dir := t.TempDir()
	file, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	memory := store.NewMemory()

	kinds := []string{
		store.TypeSessionCreated,
		store.TypeMessageAppended,
		store.TypeMessageStatus,
		store.TypeToolResults,
		store.TypeSelectionChange,
	}
	for _, kind := range kinds {
		event := mustEvent(t, kind, map[string]any{"k": kind})
		if err := file.Append(event); err != nil {
			t.Fatal(err)
		}
		if err := memory.Append(event); err != nil {
			t.Fatal(err)
		}
	}

	fromFile, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	fromMemory, err := memory.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(fromFile.Events) != len(fromMemory.Events) {
		t.Fatalf("两种后端事件数不同：%d vs %d", len(fromFile.Events), len(fromMemory.Events))
	}
	for i := range fromFile.Events {
		if fromFile.Events[i].Type != fromMemory.Events[i].Type {
			t.Fatalf("第 %d 条类型不同", i)
		}
		if string(fromFile.Events[i].Data) != string(fromMemory.Events[i].Data) {
			t.Fatalf("第 %d 条数据不同：%s vs %s", i, fromFile.Events[i].Data, fromMemory.Events[i].Data)
		}
	}
}

// 数据目录锁：第二个获取者失败，释放后可以再拿到。
func TestDirLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	first, err := store.AcquireDirLock(dir)
	if err != nil {
		t.Fatalf("第一个锁应成功：%v", err)
	}
	if _, err := store.AcquireDirLock(dir); err == nil {
		t.Fatal("第二个锁应失败")
	} else {
		var locked *store.LockedError
		if !asLockedError(err, &locked) {
			t.Fatalf("应为 LockedError，实际 %T", err)
		}
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := store.AcquireDirLock(dir)
	if err != nil {
		t.Fatalf("释放后应能重新获取：%v", err)
	}
	_ = again.Release()
}

func asLockedError(err error, target **store.LockedError) bool {
	if e, ok := err.(*store.LockedError); ok {
		*target = e
		return true
	}
	return false
}

// Catalog：扫描数据目录，按更新时间倒序列出会话，标题取最后一次重命名。
func TestCatalogListsSessions(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)

	first, err := catalog.Create("ctx-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Append(store.MustEvent(store.TypeSessionCreated,
		store.SessionCreatedData{ContextID: "ctx-1", Title: "第一"})); err != nil {
		t.Fatal(err)
	}
	if err := first.Append(store.MustEvent(store.TypeSessionRenamed,
		store.SessionRenamedData{Title: "改名了"})); err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := catalog.Create("ctx-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Append(store.MustEvent(store.TypeSessionCreated,
		store.SessionCreatedData{ContextID: "ctx-2", Title: "第二"})); err != nil {
		t.Fatal(err)
	}
	second.Close()

	summaries, err := catalog.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("应列出 2 个会话，实际 %d", len(summaries))
	}
	titles := map[string]string{}
	for _, summary := range summaries {
		titles[summary.ContextID] = summary.Title
	}
	if titles["ctx-1"] != "改名了" {
		t.Fatalf("标题应取最后一次重命名：%q", titles["ctx-1"])
	}
	if titles["ctx-2"] != "第二" {
		t.Fatalf("第二个会话标题不一致：%q", titles["ctx-2"])
	}

	// 打开已有会话可以继续追加
	reopened, err := catalog.Open("ctx-2")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err := reopened.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("应读到 1 条事件，实际 %d", len(result.Events))
	}
}

// 数据目录不存在时 List 返回空而不是报错。
func TestCatalogMissingDir(t *testing.T) {
	catalog := store.NewCatalog(t.TempDir())
	summaries, err := catalog.List()
	if err != nil {
		t.Fatalf("目录不存在不应报错：%v", err)
	}
	if len(summaries) != 0 {
		t.Fatalf("应为空：%v", summaries)
	}
}

// 回归（opus P0-1）：崩溃留下半行之后，再追加的事件不能丢。
//
// 之前不修文件：新事件接在半行后面，每次加载都被当成「中间坏行」截断，
// 崩溃之后写入的内容永久丢失。
func TestAppendAfterTruncatedLineKeepsNewEvents(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Append(mustEvent(t, store.TypeMessageAppended, map[string]any{"i": i})); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	// 模拟写到一半崩溃：追加半行（没有换行结尾）
	path := filepath.Join(dir, "session.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"v":1,"seq":2,"type":"notice.app`); err != nil {
		t.Fatal(err)
	}
	file.Close()

	// 重新打开 → 追加 3 条 → 再打开加载
	s2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s2.Append(mustEvent(t, store.TypeNotice, map[string]any{"i": i})); err != nil {
			t.Fatal(err)
		}
	}
	s2.Close()

	s3, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	result, err := s3.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 5 {
		t.Fatalf("修好半行后应有 5 条事件（2 + 3），实际 %d（warnings=%v）",
			len(result.Events), result.Warnings)
	}
	// 序号连续
	for i, event := range result.Events {
		if event.Seq != uint64(i) {
			t.Fatalf("第 %d 条 seq 应为 %d，实际 %d", i, i, event.Seq)
		}
	}
}

// 回归（opus P0-1）：中间行损坏之后，后续追加的事件也不会被反复截断。
func TestAppendAfterCorruptMiddleKeepsNewEvents(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Append(mustEvent(t, store.TypeMessageAppended, map[string]any{"i": i})); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	path := filepath.Join(dir, "session.jsonl")
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	lines[1] = `{"broken":` // 中间行损坏
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 重新打开会留档损坏文件并用合法前缀重写
	s2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Append(mustEvent(t, store.TypeNotice, map[string]any{"after": true})); err != nil {
		t.Fatal(err)
	}
	s2.Close()

	s3, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	result, err := s3.Load()
	if err != nil {
		t.Fatal(err)
	}
	// 保留第 1 条 + 新追加的 1 条
	if len(result.Events) != 2 {
		t.Fatalf("应有 2 条（1 条合法前缀 + 1 条新事件），实际 %d", len(result.Events))
	}
	last := result.Events[len(result.Events)-1]
	if last.Type != store.TypeNotice {
		t.Fatalf("最后一条应是新追加的事件：%s", last.Type)
	}
}

// 回归（opus P1-1）：复制归档之后、rename 之前崩溃 → 重启不能留下过期归档。
func TestOrphanArchiveDropped(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Append(mustEvent(t, store.TypeMessageAppended, map[string]any{"i": i})); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	// 模拟崩溃现场：归档已写出（内容 = 当前 base 的副本），但 base 还没换成快照
	data, err := os.ReadFile(filepath.Join(dir, "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "archive-0001.jsonl"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	s2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 孤儿归档被清掉，下次压缩重用 0001
	if _, statErr := os.Stat(filepath.Join(dir, "archive-0001.jsonl")); !os.IsNotExist(statErr) {
		t.Fatal("孤儿归档应被删除")
	}
	if err := s2.Append(mustEvent(t, store.TypeNotice, map[string]any{"after": true})); err != nil {
		t.Fatal(err)
	}
	snapshot := mustEvent(t, store.TypeCompactSnapshot, map[string]any{"summary": "压缩"})
	if err := s2.Compact(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "archive-0001.jsonl")); err != nil {
		t.Fatalf("压缩后应生成 archive-0001：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "archive-0002.jsonl")); !os.IsNotExist(err) {
		t.Fatal("不应产生第二份归档（序号被错误推进）")
	}

	// 归档里应该正好是 base 的全部内容（4 条：3 + 1），不重不漏
	page, err := s2.LoadOlder(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 4 {
		t.Fatalf("归档应有 4 条事件，实际 %d", len(page.Events))
	}
	s2.Close()
}

// 归档文件损坏时，LoadOlder 要给出警告（界面按显示型提示处理）。
func TestLoadOlderReportsCorruption(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Append(mustEvent(t, store.TypeMessageAppended, map[string]any{"i": i})); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Compact(mustEvent(t, store.TypeCompactSnapshot, map[string]any{"summary": "x"})); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(mustEvent(t, store.TypeNotice, map[string]any{"i": 1})); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// 破坏归档文件（第一行）
	path := filepath.Join(dir, "archive-0001.jsonl")
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	lines[0] = `{"broken":`
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	page, err := s2.LoadOlder(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Warnings) == 0 {
		t.Fatalf("归档损坏应给出警告：%#v", page)
	}
}

// 快照要记录 kept_from_seq（按需加载归档时用它截断，避免与 kept 重复）。
func TestCompactRecordsKeptFromSeq(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 3; i++ {
		if err := s.Append(mustEvent(t, store.TypeMessageAppended, map[string]any{"i": i})); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Compact(mustEvent(t, store.TypeCompactSnapshot, map[string]any{"summary": "x"})); err != nil {
		t.Fatal(err)
	}
	result, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := store.KeptFromSeq(result.Events[0].Data); got != 2 {
		t.Fatalf("kept_from_seq 应是压缩前最后一条事件的序号（2），实际 %d", got)
	}
}

// PruneEmpty 只删"真的什么都没有"的会话目录：目录里只要有主日志之外的
// 文件（归档、session.corrupt-*.jsonl 备份），就不能动。
func TestPruneEmptyKeepsDirectoriesWithBackups(t *testing.T) {
	root := t.TempDir()
	catalog := store.NewCatalog(root)

	// 1) 空目录（没有主日志、没有别的文件）→ 删
	empty := filepath.Join(catalog.SessionsDir(), "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	// 2) 只有 corrupt 备份 → 保留
	corrupt := filepath.Join(catalog.SessionsDir(), "corrupt")
	if err := os.MkdirAll(corrupt, 0o755); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(corrupt, "session.corrupt-20260101.jsonl")
	if err := os.WriteFile(backup, []byte(`{"type":"message.appended"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 3) 主日志为空但有归档 → 保留
	archived := filepath.Join(catalog.SessionsDir(), "archived")
	if err := os.MkdirAll(archived, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archived, "session.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archived, "archive-0001.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := catalog.PruneEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "empty" {
		t.Fatalf("只应删掉空目录，实际删了 %v", removed)
	}
	for _, dir := range []string{corrupt, archived} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s 不应被删除：%v", dir, err)
		}
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("损坏日志的备份不应被删除：%v", err)
	}
}

// Blob 存储：内容寻址、去重、原子写、严格的 hash 校验。
func TestBlobsPutOpenAndDedupe(t *testing.T) {
	dataDir := t.TempDir()
	blobs, err := store.NewBlobs(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := blobs.Put([]byte("图片内容"))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Fatalf("hash 应是 64 位 hex，实际 %q", first)
	}
	// 同一份内容只存一份
	second, err := blobs.Put([]byte("图片内容"))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("相同内容应得到相同 hash：%s vs %s", first, second)
	}
	// 两级分片
	if path := blobs.Path(first); filepath.Base(filepath.Dir(path)) != first[:2] {
		t.Fatalf("blob 应放在两级分片目录里：%s", path)
	}
	got, err := blobs.Open(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "图片内容" {
		t.Fatalf("内容不一致：%q", got)
	}
	if size, err := blobs.Stat(first); err != nil || size != int64(len("图片内容")) {
		t.Fatalf("Stat 不一致：%d %v", size, err)
	}
	if !blobs.Has(first) {
		t.Fatal("Has 应为 true")
	}
	if blobs.Has(strings.Repeat("0", 64)) {
		t.Fatal("不存在的 blob 不该 Has")
	}
}

// 非法引用必须被拒绝：它们可能来自被改过的日志或模型输出。
func TestBlobsRejectBadHash(t *testing.T) {
	blobs, err := store.NewBlobs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	valid, err := blobs.Put([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	bad := []string{
		"",
		"../../boot.ini",
		"../" + valid,
		strings.ToUpper(valid),
		valid[:63],
		valid + "0",
		valid[:32] + "/" + valid[32:],
		strings.Repeat("g", 64),
	}
	for _, hash := range bad {
		if _, err := blobs.Open(hash); err == nil {
			t.Fatalf("非法 hash 应被拒绝：%q", hash)
		}
		if _, err := blobs.Stat(hash); err == nil {
			t.Fatalf("非法 hash 的 Stat 应失败：%q", hash)
		}
	}

	// 引用解析也要严格
	if _, ok := store.BlobHash("blob:sha256:" + valid); !ok {
		t.Fatal("合法引用应能解析")
	}
	for _, url := range []string{
		"blob:sha256:../x",
		"blob:sha256:" + strings.ToUpper(valid),
		"blob:sha256:" + valid[:10],
		"http://example.com/a.png",
		"blob:md5:" + valid,
	} {
		if _, ok := store.BlobHash(url); ok {
			t.Fatalf("非法引用应被拒绝：%q", url)
		}
	}
}

// 内容被篡改（hash 对不上）时按"丢失"处理，不能当成正确内容发出去。
func TestBlobsDetectCorruption(t *testing.T) {
	blobs, err := store.NewBlobs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash, err := blobs.Put([]byte("原始内容"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobs.Path(hash), []byte("被改过"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Open(hash); err == nil {
		t.Fatal("内容与 hash 不一致时应报错")
	}
}

// 用户实测崩溃的回归：工具输出里有非法 UTF-8（GBK 文件内容）时，
// NewEvent / MustEvent 不能 panic，写出来的日志必须是合法 UTF-8。
func TestEventWithInvalidUTF8DoesNotPanic(t *testing.T) {
	// GBK 的「中文」：0xD6 0xD0 0xCE 0xC4，不是合法 UTF-8
	gbk := string([]byte{0xd6, 0xd0, 0xce, 0xc4})
	data := map[string]any{
		"results": []any{
			map[string]any{"call_id": "c1", "status": "ok", "output": "内容：" + gbk},
		},
	}
	event := store.MustEvent(store.TypeToolResults, data) // 以前这里 panic
	if len(event.Data) == 0 {
		t.Fatal("事件数据为空")
	}
	if !utf8.Valid(event.Data) {
		t.Fatalf("事件数据不是合法 UTF-8：%q", event.Data)
	}
	var decoded struct {
		Results []struct {
			Output string `json:"output"`
		} `json:"results"`
	}
	if err := event.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	// ToValidUTF8 把**连续**的非法字节合成一个替换字符
	if got := decoded.Results[0].Output; got != "内容：\uFFFD" {
		t.Fatalf("非法字节应换成替换字符：%q", got)
	}
}

// 合法 UTF-8 的事件不受影响（兜底路径只在编码失败时才走）。
func TestEventWithValidUTF8Unchanged(t *testing.T) {
	event, err := store.NewEvent("x", map[string]string{"output": "中文 ok"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := event.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["output"] != "中文 ok" {
		t.Fatalf("内容被改动：%q", decoded["output"])
	}
}
