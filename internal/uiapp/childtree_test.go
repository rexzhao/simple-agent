package uiapp_test

import (
	"encoding/json"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/host"
	"simple-agent/internal/locale"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/uiapp"
)

// 子会话创建后，界面的会话列表要带上 parentID（侧栏靠它折叠）。
//
// 注意必须给 Catalog：父子关系存在项目档案里，没有档案就没有"项目"，
// 也就无从判断子会话该建在哪。
func TestChildSessionAppearsInSidebarTree(t *testing.T) {
	catalog := store.NewCatalog(t.TempDir())
	p := &poster{}
	app := uiapp.NewApp(&fakeRuntime{}, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:             "你是助手",
		Workspace:                t.TempDir(),
		Title:                    "父会话",
		MaxToolRounds:            3,
		SidebarAutoCollapseWidth: -1,
		Language:                 locale.ZH,
		Post:                     p.Post,
		ContextWindow:            testContextWindow,
		Catalog:                  catalog,
	})
	tester := ui.NewTester(app.View, 900, 620)
	tester.Frame()
	p.Drain()

	// 先建一个顶层会话当父会话（带 Catalog 时启动可能一个会话都没有）
	if app.Current() == nil {
		payload, _ := json.Marshal(host.NewSessionPayload{Title: "父会话"})
		app.SubmitForTest(host.Command{Kind: "newSession", Payload: payload})
		for i := 0; i < 60 && app.Current() == nil; i++ {
			pump(tester, p)
		}
	}
	if app.Current() == nil {
		t.Fatal("应有父会话")
	}
	parentID := app.Current().ID
	payload, _ := json.Marshal(host.NewSessionPayload{Title: "子会话", ParentID: parentID})
	app.SubmitForTest(host.Command{Kind: "newSession", Payload: payload})

	// 命令是异步的：要靠 pump（排空 post 队列 + 渲染）把它跑完
	for i := 0; i < 60; i++ {
		pump(tester, p)
		if len(app.SessionIDsForTest()) >= 2 {
			break
		}
	}

	if got := len(app.SessionIDsForTest()); got != 2 {
		t.Fatalf("应有父会话与子会话，实际 %d（%v）", got, app.SessionIDsForTest())
	}
	if children := app.ChildCountForTest(parentID); children != 1 {
		t.Fatalf("父会话应有 1 个子会话，实际 %d", children)
	}
}
