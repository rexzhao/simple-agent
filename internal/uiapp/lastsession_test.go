package uiapp_test

import (
	"path/filepath"
	"testing"

	"simple-agent/internal/locale"
	"simple-agent/internal/providers"
	"simple-agent/internal/uiapp"
	"simple-agent/internal/uistate"
)

// 切会话时把 ID 记进本机偏好（下次启动切回这里）。
//
// 用户报的："桌面版没有记录用户最后打开的会话，
// 每次打开时候默认会话不确定"。
//
// 存在 ui-state.json 而不是会话档案：同一份会话在电脑和手机上
// 可以各自停在不同位置，不该互相覆盖。
func TestSwitchRecordsLastSessionToUIState(t *testing.T) {
	dir := t.TempDir()
	uiStatePath := filepath.Join(dir, uistate.FileName)

	app := uiapp.NewApp(&fakeRuntime{}, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:                dir,
		Title:                    "会话",
		Language:                 locale.ZH,
		Post:                     (&poster{}).Post,
		UIStatePath:              uiStatePath,
		SidebarAutoCollapseWidth: -1,
	})

	current := app.Current()
	if current == nil {
		t.Fatal("前提：应有一个会话")
	}

	// 切一次会话（侧栏、新建、删除后定位都走这个入口）
	app.SwitchToForTest(0)

	state, err := uistate.Load(uiStatePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastSessionID == "" {
		t.Fatalf("切会话后应写入偏好，实际为空：%+v", state)
	}
	if state.LastSessionID != current.ID {
		t.Fatalf("记录的应是当前会话 %q，实际 %q", current.ID, state.LastSessionID)
	}
}

// 启动时读到偏好里的会话 ID。
func TestLoadsLastSessionFromUIState(t *testing.T) {
	dir := t.TempDir()
	uiStatePath := filepath.Join(dir, uistate.FileName)

	state := uistate.Default()
	state.LastSessionID = "sess-from-prefs"
	if err := uistate.Save(uiStatePath, state); err != nil {
		t.Fatal(err)
	}

	app := uiapp.NewApp(&fakeRuntime{}, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:                dir,
		Title:                    "会话",
		Language:                 locale.ZH,
		Post:                     (&poster{}).Post,
		UIStatePath:              uiStatePath,
		SidebarAutoCollapseWidth: -1,
	})

	if got := app.LastSessionForTest(); got != "sess-from-prefs" {
		t.Fatalf("应读到偏好里的 ID，实际 %q", got)
	}
	// 找不到该会话时不能崩，也不能留空选中
	if app.Current() == nil {
		t.Fatal("偏好指向不存在的会话时，应退化成默认选中")
	}
}
