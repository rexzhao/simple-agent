package uistate_test

import (
	"os"
	"path/filepath"
	"testing"

	"simple-agent/internal/uistate"
)

// 没有文件时给默认值且不报错（首次启动）。
func TestLoadMissingFileIsDefault(t *testing.T) {
	state, err := uistate.Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("文件不存在不该报错：%v", err)
	}
	if state.ChatContentWidth != 0 {
		t.Fatalf("默认应没有宽度偏好：%#v", state)
	}
}

// 存了之后能读回来。
func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ui-state.json")
	if err := uistate.Save(path, uistate.State{ChatContentWidth: 820}); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	state, err := uistate.Load(path)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if state.ChatContentWidth != 820 {
		t.Fatalf("宽度没读回来：%#v", state)
	}
}

// **损坏的文件不能让启动失败**：回默认值，且下次保存能覆盖掉它。
func TestLoadCorruptFileFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ui-state.json")
	if err := os.WriteFile(path, []byte("{ 这不是 JSON"), 0o644); err != nil {
		t.Fatal(err)
	}
	state, err := uistate.Load(path)
	if err == nil {
		t.Fatal("损坏文件应给出原因（便于排查），但不该阻止使用")
	}
	if state.ChatContentWidth != 0 {
		t.Fatalf("损坏时应回默认值：%#v", state)
	}

	// 覆盖写之后恢复正常
	if err := uistate.Save(path, uistate.State{ChatContentWidth: 700}); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	state, err = uistate.Load(path)
	if err != nil || state.ChatContentWidth != 700 {
		t.Fatalf("覆盖后应正常：%#v err=%v", state, err)
	}
}

// 手改文件写出非法值（负数、过小）要被夹到合法范围。
func TestLoadClampsIllegalValues(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]int{
		`{"version":1,"chat_content_width":-100}`: 0,
		`{"version":1,"chat_content_width":10}`:   640,
		`{"version":1,"chat_content_width":800}`:  800,
	}
	for content, want := range cases {
		path := filepath.Join(dir, "x.json")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		state, _ := uistate.Load(path)
		if state.ChatContentWidth != want {
			t.Fatalf("%s → 期望 %d，实际 %d", content, want, state.ChatContentWidth)
		}
	}
}

// 不认识的版本按默认值处理（为将来的迁移留出空间）。
func TestLoadUnknownVersionIsDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ui-state.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"chat_content_width":800}`), 0o644); err != nil {
		t.Fatal(err)
	}
	state, err := uistate.Load(path)
	if err != nil {
		t.Fatalf("未知版本不该报错：%v", err)
	}
	if state.ChatContentWidth != 0 {
		t.Fatalf("未知版本应按默认值：%#v", state)
	}
}

// 空路径 = 不持久化：读写都不报错（取不到配置目录时的退化路径）。
func TestEmptyPathIsNoop(t *testing.T) {
	state, err := uistate.Load("")
	if err != nil || state.ChatContentWidth != 0 {
		t.Fatalf("空路径应给默认值：%#v err=%v", state, err)
	}
	if err := uistate.Save("", uistate.State{ChatContentWidth: 800}); err != nil {
		t.Fatalf("空路径保存不该报错：%v", err)
	}
}

// Path 指向系统应用配置目录下的 simple-agent/ui-state.json。
func TestPathIsUnderUserConfigDir(t *testing.T) {
	got := uistate.Path()
	if got == "" {
		t.Skip("这台机器取不到 UserConfigDir")
	}
	if filepath.Base(got) != uistate.FileName {
		t.Fatalf("文件名不对：%s", got)
	}
	if filepath.Base(filepath.Dir(got)) != "simple-agent" {
		t.Fatalf("应放在 simple-agent 目录下：%s", got)
	}
	configDir, err := os.UserConfigDir()
	if err == nil {
		rel, relErr := filepath.Rel(configDir, got)
		if relErr != nil || rel == ".." {
			t.Fatalf("应在 UserConfigDir 之内：%s（配置目录 %s）", got, configDir)
		}
	}
}
