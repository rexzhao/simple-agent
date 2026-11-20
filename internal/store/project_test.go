package store_test

import (
	"os"
	"path/filepath"
	"testing"

	json "encoding/json/v2"

	"simple-agent/internal/store"
)

// 项目档案：目录用 UUID，project.json 记工作区与会话。
//
// 用户的要求："项目目录应该用 uuid，防止出现字符编码问题，里面的
// project.json 记录目录"。项目名可能含中文/空格/重名，做目录名不安全。
func TestProjectUsesUUIDDirectory(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)

	id, err := catalog.EnsureProject(`F:\work\我的项目`)
	if err != nil {
		t.Fatal(err)
	}
	// 目录名是 UUID（只含十六进制与连字符），不含中文
	if !isUUIDLike(id) {
		t.Fatalf("项目 ID 应是 UUID 形式，实际 %q", id)
	}
	for _, r := range id {
		if !(r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("项目 ID 不该含 %q（会有编码问题）：%q", r, id)
		}
	}
	// 档案文件在 projects/<uuid>/project.json，里面记录真实路径
	path := filepath.Join(dataDir, "projects", id, "project.json")
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("档案文件应存在：%v", statErr)
	}
	project, err := catalog.LoadProject(id)
	if err != nil {
		t.Fatal(err)
	}
	if project.Workspace != `F:\work\我的项目` {
		t.Fatalf("档案里应记真实工作区，实际 %q", project.Workspace)
	}
	if project.Name != "我的项目" {
		t.Fatalf("显示名应是目录末级，实际 %q", project.Name)
	}
}

// 同一个工作区重复登记不该产生第二个项目（重新添加 = 恢复显示）。
func TestEnsureProjectIsStable(t *testing.T) {
	catalog := store.NewCatalog(t.TempDir())

	first, err := catalog.EnsureProject(`F:\work\alpha`)
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.EnsureProject(`F:/work/alpha/`) // 斜杠与尾斜杠不同写法
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("同一工作区应复用项目：%q != %q", first, second)
	}
	// Windows 大小写不敏感
	third, err := catalog.EnsureProject(`f:/WORK/ALPHA`)
	if err != nil {
		t.Fatal(err)
	}
	if first != third {
		t.Fatalf("大小写不同也算同一项目：%q != %q", first, third)
	}
}

// 移除项目只标记 hidden，**不删任何文件**；重新添加即恢复。
//
// 用户的场景：移除后不显示，也不再打开它的会话；添加回来时恢复显示。
func TestRemoveProjectHidesButKeepsFiles(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)

	id, err := catalog.EnsureProject(`F:\work\alpha`)
	if err != nil {
		t.Fatal(err)
	}
	// 建一个会话
	opened, err := catalog.Create("ctx-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Append(store.MustEvent(store.TypeSessionCreated,
		store.SessionCreatedData{ContextID: "ctx-a", Title: "会话 A"})); err != nil {
		t.Fatal(err)
	}
	dir := opened.Dir()
	opened.Close()
	if err := catalog.AddSession("ctx-a", "会话 A", `F:\work\alpha`, dir); err != nil {
		t.Fatal(err)
	}

	// 可见时有 1 个项目、1 个会话
	visible, err := catalog.VisibleProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 {
		t.Fatalf("应有 1 个可见项目，实际 %d", len(visible))
	}
	summaries, err := catalog.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("应有 1 个会话，实际 %d", len(summaries))
	}

	// 移除项目
	if err := catalog.SetProjectHidden(id, true); err != nil {
		t.Fatal(err)
	}
	visible, err = catalog.VisibleProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 0 {
		t.Fatalf("移除后不该有可见项目，实际 %d", len(visible))
	}
	summaries, err = catalog.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Fatalf("移除后不该列出它的会话（也不再读文件），实际 %d", len(summaries))
	}
	// **文件必须还在**（移除不等于删除）
	if _, statErr := os.Stat(filepath.Join(dir, "session.jsonl")); statErr != nil {
		t.Fatalf("移除项目不该删会话文件：%v", statErr)
	}

	// 重新添加同一工作区 → 恢复显示
	restored, err := catalog.EnsureProject(`F:\work\alpha`)
	if err != nil {
		t.Fatal(err)
	}
	if restored != id {
		t.Fatalf("重新添加应复用原项目：%q != %q", restored, id)
	}
	// 还需要把 hidden 置回 false（添加动作的一部分）
	if err := catalog.SetProjectHidden(restored, false); err != nil {
		t.Fatal(err)
	}
	visible, err = catalog.VisibleProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 {
		t.Fatalf("恢复后应有 1 个可见项目，实际 %d", len(visible))
	}
	summaries, err = catalog.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].ContextID != "ctx-a" {
		t.Fatalf("恢复后应列出原会话，实际 %+v", summaries)
	}
}

// 会话档案里的 dir 是**父目录**（相对 project.json），不是会话自己的目录。
//
// 会话目录 = dir/ID。这样一组会话共享一条路径：老会话都记成
// `../../sessions`，档案一眼能看出"这些还留在老位置"。
//
// 用户的原话："dir 应该指向 session 所在的目录，而不是 session 会话自己的
// 目录，是相对于 project.json 的路径"。
func TestSessionDirIsParentRelativeToProject(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)

	opened, err := catalog.Create("ctx-rel")
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Append(store.MustEvent(store.TypeSessionCreated,
		store.SessionCreatedData{ContextID: "ctx-rel", Title: "相对路径"})); err != nil {
		t.Fatal(err)
	}
	sessionDir := opened.Dir()
	opened.Close()
	if err := catalog.AddSession("ctx-rel", "相对路径", "", sessionDir); err != nil {
		t.Fatal(err)
	}

	projects, err := catalog.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || len(projects[0].Sessions) != 1 {
		t.Fatalf("应有 1 个项目 1 个会话，实际 %+v", projects)
	}
	recorded := projects[0].Sessions[0].Dir
	if filepath.IsAbs(filepath.FromSlash(recorded)) {
		t.Fatalf("dir 应是相对路径，实际 %q", recorded)
	}
	// **关键**：dir 指向父目录，拼上 ID 才是会话目录
	base := filepath.Join(dataDir, "projects", projects[0].ID)
	parent := filepath.Join(base, filepath.FromSlash(recorded))
	wantDir := filepath.Join(filepath.Dir(sessionDir), "ctx-rel")
	if filepath.Clean(parent) != filepath.Clean(filepath.Dir(sessionDir)) {
		t.Fatalf("dir 应指向会话的父目录：\n  dir=%q → %q\n  期望 %q",
			recorded, filepath.Clean(parent), filepath.Dir(sessionDir))
	}
	// 能通过档案找回会话并打开
	reopened, err := catalog.Open("ctx-rel")
	if err != nil {
		t.Fatalf("应能按档案打开会话：%v", err)
	}
	if filepath.Clean(reopened.Dir()) != filepath.Clean(wantDir) {
		t.Fatalf("打开的应是 %q，实际 %q", wantDir, reopened.Dir())
	}
	defer reopened.Close()
}

// 老布局（sessions/<id>/）的会话只登记、不搬动。
func TestMigrateLegacySessionsKeepsFilesInPlace(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)

	// 手工造一个老布局会话
	legacyDir := filepath.Join(dataDir, "sessions", "old-1")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writer, err := store.Open(legacyDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(store.MustEvent(store.TypeSessionCreated,
		store.SessionCreatedData{
			ContextID: "old-1", Title: "老会话", Workspace: `F:\work\legacy`,
		})); err != nil {
		t.Fatal(err)
	}
	writer.Close()

	migrated, err := catalog.MigrateLegacySessions()
	if err != nil {
		t.Fatal(err)
	}
	if migrated != 1 {
		t.Fatalf("应登记 1 个老会话，实际 %d", migrated)
	}
	// 文件必须还在原位
	if _, statErr := os.Stat(filepath.Join(legacyDir, "session.jsonl")); statErr != nil {
		t.Fatalf("迁移不该移动文件：%v", statErr)
	}
	// 档案里记的是老位置
	summaries, err := catalog.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].ContextID != "old-1" {
		t.Fatalf("应列出老会话，实际 %+v", summaries)
	}
	if summaries[0].Title != "老会话" {
		t.Fatalf("标题应来自日志，实际 %q", summaries[0].Title)
	}
	if summaries[0].Workspace != `F:\work\legacy` {
		t.Fatalf("工作区应来自日志，实际 %q", summaries[0].Workspace)
	}
	// 幂等：再跑一次不重复登记
	again, err := catalog.MigrateLegacySessions()
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("重复迁移不该再登记，实际 %d", again)
	}
}

// 压缩过的老会话：标题和工作区要从**归档**里找回来。
//
// 真实踩到的：压缩后首行是 compact.snapshot，session.created 已在
// archive-0001.jsonl 里。之前的登记把标题退回成会话 id，侧栏里就成了
// 一串看不出内容的十六进制。
func TestMigrateReadsTitleFromArchive(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)

	// 手工造一个"压缩过"的老会话：归档里有 session.created，
	// base 里只有 compact.snapshot。
	legacyDir := filepath.Join(dataDir, "sessions", "compacted-1")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writer, err := store.Open(legacyDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(store.MustEvent(store.TypeSessionCreated,
		store.SessionCreatedData{
			ContextID: "compacted-1", Title: "原始标题", Workspace: `F:\work\archived`,
		})); err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(store.MustEvent(store.TypeSessionRenamed,
		store.SessionRenamedData{Title: "改名后的标题"})); err != nil {
		t.Fatal(err)
	}
	// 压缩：把已有内容归档，用快照开新文件
	if err := writer.Compact(store.MustEvent(store.TypeCompactSnapshot, map[string]any{
		"summary": "摘要", "summary_message_id": "s1", "kept_entries": []any{},
		"compacted_at": "2026-01-01T00:00:00Z",
	})); err != nil {
		t.Fatal(err)
	}
	writer.Close()

	migrated, err := catalog.MigrateLegacySessions()
	if err != nil {
		t.Fatal(err)
	}
	if migrated != 1 {
		t.Fatalf("应登记 1 个会话，实际 %d", migrated)
	}
	summaries, err := catalog.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("应列出 1 个会话，实际 %d", len(summaries))
	}
	got := summaries[0]
	// 标题要来自归档（不是会话 id）
	if got.Title == "compacted-1" {
		t.Fatalf("标题不该退回成会话 id（说明没读归档）：%q", got.Title)
	}
	if got.Title != "改名后的标题" {
		t.Fatalf("标题应取归档里最后一次改名，实际 %q", got.Title)
	}
	// 工作区同理：只在 session.created 里
	if got.Workspace != `F:\work\archived` {
		t.Fatalf("工作区应来自归档，实际 %q", got.Workspace)
	}
}

// 项目与会话的显示顺序**跟档案走**，用户可调整。
//
// 用户的要求："界面显示顺序按照 project.json 里面记录的顺序，
// 用户最好可以调整顺序"。所以 List/ListProjects 不能自己重排。
func TestProjectAndSessionOrderIsRespected(t *testing.T) {
	catalog := store.NewCatalog(t.TempDir())

	// 建三个项目：zeta、alpha、beta（名字顺序与创建顺序故意不同）
	var ids []string
	for _, ws := range []string{`F:\work\zeta`, `F:\work\alpha`, `F:\work\beta`} {
		id, err := catalog.EnsureProject(ws)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	// 初始顺序 = 创建顺序（新项目排在最后）
	projects, err := catalog.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range projects {
		got = append(got, p.Name)
	}
	want := []string{"zeta", "alpha", "beta"}
	if len(got) != len(want) {
		t.Fatalf("应有 %d 个项目，实际 %d：%v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("初始应按创建顺序 %v，实际 %v", want, got)
		}
	}

	// 调整成 alpha、beta、zeta
	if err := catalog.ReorderProjects([]string{ids[1], ids[2], ids[0]}); err != nil {
		t.Fatal(err)
	}
	projects, err = catalog.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	for _, p := range projects {
		got = append(got, p.Name)
	}
	want = []string{"alpha", "beta", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("调整后应按新顺序 %v，实际 %v", want, got)
		}
	}
}

// 会话的显示顺序同样跟档案走，**不按更新时间重排**。
func TestSessionOrderIsRespected(t *testing.T) {
	catalog := store.NewCatalog(t.TempDir())

	id, err := catalog.EnsureProject(`F:\work\order`)
	if err != nil {
		t.Fatal(err)
	}
	// 建三个会话（后建的更新时间更晚）
	for _, ctx := range []string{"s1", "s2", "s3"} {
		opened, openErr := catalog.Create(ctx)
		if openErr != nil {
			t.Fatal(openErr)
		}
		if err := opened.Append(store.MustEvent(store.TypeSessionCreated,
			store.SessionCreatedData{ContextID: ctx, Title: ctx})); err != nil {
			t.Fatal(err)
		}
		dir := opened.Dir()
		opened.Close()
		if err := catalog.AddSession(ctx, ctx, `F:\work\order`, dir); err != nil {
			t.Fatal(err)
		}
	}

	// 档案顺序 = 登记顺序
	summaries, err := catalog.List()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range summaries {
		got = append(got, s.ContextID)
	}
	if len(got) != 3 || got[0] != "s1" || got[1] != "s2" || got[2] != "s3" {
		t.Fatalf("应按登记顺序 [s1 s2 s3]，实际 %v", got)
	}

	// 调整成 s3、s1、s2
	if err := catalog.ReorderSessions(id, []string{"s3", "s1", "s2"}); err != nil {
		t.Fatal(err)
	}
	summaries, err = catalog.List()
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	for _, s := range summaries {
		got = append(got, s.ContextID)
	}
	want := []string{"s3", "s1", "s2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("调整后应按 [s3 s1 s2]，实际 %v", got)
		}
	}
}

// projects.json 是**唯一权威**：顺序由数组保证，移除 = 从数组删除。
//
// 用户的要求："最外层也应该有个 projects.json 引用到所有的 project，
// 这样删除项目时候只需要从 projects.json 中移除就可以了，而且可以保证顺序"。
func TestProjectsIndexIsAuthoritative(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)

	// 建三个项目
	var ids []string
	for _, ws := range []string{`F:\work\one`, `F:\work\two`, `F:\work\three`} {
		id, err := catalog.EnsureProject(ws)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	// 索引文件应存在，且顺序 = 创建顺序
	indexPath := filepath.Join(dataDir, "projects.json")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("应有 projects.json：%v", err)
	}
	var index struct {
		Projects []string `json:"projects"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Projects) != 3 {
		t.Fatalf("索引里应有 3 个项目，实际 %d：%v", len(index.Projects), index.Projects)
	}
	for i, want := range ids {
		if index.Projects[i] != want {
			t.Fatalf("索引顺序应与创建顺序一致：第 %d 个是 %q，应为 %q", i, index.Projects[i], want)
		}
	}

	// 移除中间那个：只改索引
	if err := catalog.RemoveProject(ids[1]); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Projects) != 2 {
		t.Fatalf("移除后索引应剩 2 个，实际 %d", len(index.Projects))
	}
	if index.Projects[0] != ids[0] || index.Projects[1] != ids[2] {
		t.Fatalf("移除后顺序应保持 [one three]，实际 %v", index.Projects)
	}
	// **目录还在**（移除不删文件）
	if _, statErr := os.Stat(filepath.Join(dataDir, "projects", ids[1], "project.json")); statErr != nil {
		t.Fatalf("移除项目不该删档案：%v", statErr)
	}
	// 列表里也没有它了
	projects, err := catalog.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 {
		t.Fatalf("列表应只剩 2 个，实际 %d", len(projects))
	}

	// 重新添加同一工作区 → 回到列表（追加到末尾）
	restored, err := catalog.EnsureProject(`F:\work\two`)
	if err != nil {
		t.Fatal(err)
	}
	if restored != ids[1] {
		t.Fatalf("应复用原项目 ID：%q != %q", restored, ids[1])
	}
	projects, err = catalog.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 3 {
		t.Fatalf("恢复后应有 3 个项目，实际 %d", len(projects))
	}
	if projects[2].ID != ids[1] {
		t.Fatalf("恢复的项目应追加到末尾，实际顺序 %+v", projects)
	}
}

// 索引文件缺失时从目录重建（兼容只有 project.json 的数据）。
func TestProjectsIndexRebuiltWhenMissing(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)

	id, err := catalog.EnsureProject(`F:\work\keep`)
	if err != nil {
		t.Fatal(err)
	}
	// 删掉索引，模拟早期版本/手工拷贝
	if err := os.Remove(filepath.Join(dataDir, "projects.json")); err != nil {
		t.Fatal(err)
	}

	projects, err := catalog.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].ID != id {
		t.Fatalf("应从目录重建出 1 个项目，实际 %+v", projects)
	}
	// 重建后索引文件应被补上
	if _, statErr := os.Stat(filepath.Join(dataDir, "projects.json")); statErr != nil {
		t.Fatalf("应补建 projects.json：%v", statErr)
	}
}

// isUUIDLike 判断是不是 UUID 形式（8-4-4-4-12，只含十六进制与连字符）。
func isUUIDLike(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}