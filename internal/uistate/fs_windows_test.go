//go:build windows

package uistate_test

import (
	"os"
	"path/filepath"
	"testing"

	"simple-agent/internal/uistate"
)

// 目标文件正被别的进程打开时，保存仍然要成功。
//
// 这是用户实际踩到的场景：`%APPDATA%\Roaming` 是漫游目录，同步服务/索引器/
// 杀毒软件会短暂持有 ui-state.json，于是 `os.Rename` 报
// `Access is denied`。Save 必须重试并在必要时退化为覆盖写。
func TestSaveWhileTargetIsOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ui-state.json")
	if err := uistate.Save(path, uistate.State{ChatContentWidth: 700}); err != nil {
		t.Fatal(err)
	}

	// 用「不共享删除」的方式打开目标：这会让 rename 覆盖它失败，
	// 与 Windows 上同步服务的占用行为一致。
	holder, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	if err := uistate.Save(path, uistate.State{ChatContentWidth: 860}); err != nil {
		t.Fatalf("目标被打开时也应保存成功：%v", err)
	}

	// 至少不能损坏已有内容：能被正常读回
	state, err := uistate.Load(path)
	if err != nil {
		t.Fatalf("保存后应仍可读：%v", err)
	}
	if state.ChatContentWidth != 860 && state.ChatContentWidth != 700 {
		t.Fatalf("宽度应是旧值或新值，实际 %d", state.ChatContentWidth)
	}
}

// 目标文件只读时也要能保存（漫游目录里常见）。
func TestSaveOverReadOnlyTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ui-state.json")
	if err := uistate.Save(path, uistate.State{ChatContentWidth: 700}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	if err := uistate.Save(path, uistate.State{ChatContentWidth: 820}); err != nil {
		t.Fatalf("只读目标也应保存成功：%v", err)
	}
	state, err := uistate.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.ChatContentWidth != 820 {
		t.Fatalf("应写入新值，实际 %d", state.ChatContentWidth)
	}
}

// 连续保存不留下临时文件（重试路径不能泄漏 tmp）。
func TestSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ui-state.json")
	for i := 0; i < 5; i++ {
		if err := uistate.Save(path, uistate.State{ChatContentWidth: 700 + i}); err != nil {
			t.Fatalf("第 %d 次保存失败：%v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "ui-state.json" {
			t.Fatalf("残留文件：%s", entry.Name())
		}
	}
}
