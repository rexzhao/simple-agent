package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"simple-agent/internal/store"
)

// 档案里指向"不存在的目录"的记录要被清掉。
//
// 真实踩到的：移动端新建几个会话，桌面侧栏出现四组（已修），
// 但重启后弹出 4 条 "failed to restore ... The log is empty"。
// 原因是新会话立刻登记进档案，而空会话**不落盘** —— 目录根本不存在，
// 档案里却留着记录，启动时按它去打开就报"日志为空"。
func TestPruneMissingDropsRecordsWithoutDir(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)

	// 一个真实存在的会话
	opened, err := catalog.Create("real-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Append(store.MustEvent(store.TypeSessionCreated,
		store.SessionCreatedData{ContextID: "real-1", Title: "真会话"})); err != nil {
		t.Fatal(err)
	}
	dir := opened.Dir()
	opened.Close()
	if err := catalog.AddSession("real-1", "真会话", `F:\work\alpha`, dir); err != nil {
		t.Fatal(err)
	}

	// 一个只登记、没落盘的（目录不存在）
	ghostDir, err := catalog.SessionDirFor("ghost-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.AddSession("ghost-1", "空会话", `F:\work\alpha`, ghostDir); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "sessions", "ghost-1")); statErr == nil {
		t.Fatal("前提不成立：ghost 会话不该有目录")
	}

	removed, err := catalog.PruneMissing()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "ghost-1" {
		t.Fatalf("应只移除 ghost-1，实际 %v", removed)
	}

	// 档案里只剩真会话
	summaries, err := catalog.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].ContextID != "real-1" {
		t.Fatalf("档案应只剩 real-1，实际 %+v", summaries)
	}
}

// 没有缺失记录时不该改动档案（避免无谓写盘）。
func TestPruneMissingNoopWhenAllPresent(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)

	opened, err := catalog.Create("only-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Append(store.MustEvent(store.TypeSessionCreated,
		store.SessionCreatedData{ContextID: "only-1", Title: "唯一"})); err != nil {
		t.Fatal(err)
	}
	dir := opened.Dir()
	opened.Close()
	if err := catalog.AddSession("only-1", "唯一", `F:\work\alpha`, dir); err != nil {
		t.Fatal(err)
	}

	removed, err := catalog.PruneMissing()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("不该移除任何记录，实际 %v", removed)
	}
}