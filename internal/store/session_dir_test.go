package store_test

import (
	"path/filepath"
	"testing"

	"simple-agent/internal/store"
)

// 新建会话时："先算路径登记" 与 "之后 Open 真正打开" 必须落在**同一个目录**。
//
// 这是 NewSession 立刻登记的配套条件：登记时用 SessionDirFor 算路径写进
// 档案的 dir；之后 ensurePersisted 调 Catalog.Create → Open，走的也是
// sessionLocation。两者不一致的话，日志会写到另一个地方，
// 或者 Open 建了个新目录而档案指向旧位置。
func TestSessionDirForMatchesOpen(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)

	const id = "ctx-consistency"
	// 1) 模拟 NewSession：先算路径、登记
	dir, err := catalog.SessionDirFor(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.AddSession(id, "新会话", `F:\work\alpha`, dir); err != nil {
		t.Fatal(err)
	}

	// 2) 模拟 ensurePersisted：真正打开
	opened, err := catalog.Create(id)
	if err != nil {
		t.Fatalf("打开会话失败：%v", err)
	}
	defer opened.Close()

	if filepath.Clean(opened.Dir()) != filepath.Clean(dir) {
		t.Fatalf("登记时算的路径与真正打开的路径不一致：\n  登记=%q\n  实际=%q\n"+
			"（这会让日志写到别处，或档案指向不存在的目录）", dir, opened.Dir())
	}

	// 3) 目录也应该确实在老布局下（sessions/<id>），不是项目目录里
	wantLegacy := filepath.Join(dataDir, "sessions", id)
	if filepath.Clean(opened.Dir()) != filepath.Clean(wantLegacy) {
		t.Fatalf("新会话应落在老布局 %q，实际 %q", wantLegacy, opened.Dir())
	}
}