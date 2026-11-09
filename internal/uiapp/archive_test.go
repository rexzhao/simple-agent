package uiapp_test

import (
	"testing"

	"simple-agent/internal/host"
	"simple-agent/internal/locale"
	"simple-agent/internal/message"
	"simple-agent/internal/uiapp"
)

func archivePage(texts []string, more bool) host.OlderPage {
	page := host.OlderPage{More: more}
	for _, text := range texts {
		page.Entries = append(page.Entries, host.OlderEntry{
			Message: message.Message{Role: message.RoleUser, Parts: []message.Part{message.NewTextPart(text)}},
		})
	}
	return page
}

// silentHost 吞掉 loadOlder（真实 Local 没有归档时会立刻回空页），
// 让测试自己决定什么时候、按什么内容回结果。
type silentHost struct{ host.Host }

func (s *silentHost) Submit(cmd host.Command) (string, error) {
	if cmd.Kind == "loadOlder" {
		return "silent", nil
	}
	return s.Host.Submit(cmd)
}

// P3: 归档分页结果经 cmdResult 回来后要真正接进界面，且更早的一页排在最前面。
func TestArchivePageAppliedInOrder(t *testing.T) {
	app, _, _ := newAppWithOptions(t, func(opts *uiapp.Options) { opts.Language = locale.ZH })
	app.SetHostForTest(&silentHost{Host: app.Host()})
	app.LoadOlderForTest()

	app.ApplyChangeForTest(host.Change{
		SessionID: app.Current().ID, Kind: "cmdResult", CmdID: "c1",
		Result: host.CmdResult{Kind: "loadOlder", OK: true, Data: archivePage([]string{"第二新的", "最新的"}, true)},
	})
	if got := app.ArchivedForTest(); len(got) != 2 {
		t.Fatalf("第一页应有 2 条：%v", got)
	}
	app.LoadOlderForTest()
	app.ApplyChangeForTest(host.Change{
		SessionID: app.Current().ID, Kind: "cmdResult", CmdID: "c2",
		Result: host.CmdResult{Kind: "loadOlder", OK: true, Data: archivePage([]string{"最老的"}, false)},
	})
	got := app.ArchivedForTest()
	want := []string{"最老的", "第二新的", "最新的"}
	if len(got) != len(want) {
		t.Fatalf("归档条数不对：%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序不对：%v", got)
		}
	}

	// More=false 之后不再请求
	before := len(got)
	app.LoadOlderForTest()
	app.ApplyChangeForTest(host.Change{
		SessionID: app.Current().ID, Kind: "cmdResult", CmdID: "c3",
		Result: host.CmdResult{Kind: "loadOlder", OK: true, Data: archivePage([]string{"不该出现"}, true)},
	})
	if len(app.ArchivedForTest()) != before {
		t.Fatal("读完之后不该再加载归档")
	}
}
