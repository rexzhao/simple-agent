package uistate_test

import (
	"os"
	"path/filepath"
	"testing"

	"simple-agent/internal/uistate"
)

// 上次打开的会话要能存下来、读回去（本机偏好，不写执行端）。
func TestLastSessionRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), uistate.FileName)

	state := uistate.Default()
	state.LastSessionID = "abc123"
	if err := uistate.Save(path, state); err != nil {
		t.Fatal(err)
	}

	loaded, err := uistate.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LastSessionID != "abc123" {
		t.Fatalf("应读回 abc123，实际 %q", loaded.LastSessionID)
	}
}

// 与宽度偏好共存：两个字段都要保留（别因为加了新字段把旧的丢了）。
func TestLastSessionCoexistsWithWidth(t *testing.T) {
	path := filepath.Join(t.TempDir(), uistate.FileName)

	state := uistate.Default()
	state.ChatContentWidth = 900
	state.LastSessionID = "sess-1"
	if err := uistate.Save(path, state); err != nil {
		t.Fatal(err)
	}

	loaded, err := uistate.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ChatContentWidth != 900 {
		t.Fatalf("宽度应保留：%d", loaded.ChatContentWidth)
	}
	if loaded.LastSessionID != "sess-1" {
		t.Fatalf("会话 ID 应保留：%q", loaded.LastSessionID)
	}
}

// 文件不存在时 LastSessionID 为空（首次启动），不能因此报错。
func TestLastSessionMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	state, err := uistate.Load(path)
	if err != nil {
		t.Fatalf("文件不存在不该报错：%v", err)
	}
	if state.LastSessionID != "" {
		t.Fatalf("应是空，实际 %q", state.LastSessionID)
	}
}

// 老版本文件（没有 last_session_id）读出来是空，不报错 —— 升级不能拦住启动。
func TestLastSessionAbsentInOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), uistate.FileName)
	old := `{"version":1,"chat_content_width":800}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	state, err := uistate.Load(path)
	if err != nil {
		t.Fatalf("老文件不该报错：%v", err)
	}
	if state.LastSessionID != "" {
		t.Fatalf("老文件没有该字段，应是空：%q", state.LastSessionID)
	}
	if state.ChatContentWidth != 800 {
		t.Fatalf("老字段要保留：%d", state.ChatContentWidth)
	}
}
