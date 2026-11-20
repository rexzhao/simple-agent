package host_test

import (
	"encoding/json"
	"testing"

	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
)

// 归档的会话**不能再运行**：发消息、压缩、改名一律拒绝。
//
// 用户的要求："归档的会话无法再运行，想运行必须恢复"。
// 拦截放在 dispatch 里统一做（白名单式），这里验证几个代表性命令。
func TestArchivedSessionRejectsRunCommands(t *testing.T) {
	l, _, _ := newHostWithPoster(t)
	session := l.NewSession("")
	target := l.Session(session.ID)
	if err := target.SetArchived(true); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		kind    string
		payload []byte
		name    string
	}{
		{"send", mustJSON(t, host.SendPayload{Text: "你好"}), "发消息"},
		{"compact", nil, "压缩"},
		{"rename", mustJSON(t, host.TextPayload{Text: "新名字"}), "改名"},
	}
	for _, c := range cases {
		result := l.DispatchForTest(session.ID, host.Command{
			SessionID: session.ID, Kind: c.kind, Payload: c.payload,
		})
		if result.OK {
			t.Fatalf("归档的会话不该接受 %s（%s）", c.name, c.kind)
		}
		// 只说"被拒绝"：具体文案走 locale，测试不该锁死中文
		if result.Error == "" {
			t.Fatalf("%s 应给出拒绝原因", c.name)
		}
	}
}

// 恢复之后又能正常运行了。
func TestRestoredSessionCanRunAgain(t *testing.T) {
	l, _, _ := newHostWithPoster(t)
	session := l.NewSession("")
	target := l.Session(session.ID)

	if err := target.SetArchived(true); err != nil {
		t.Fatal(err)
	}
	blocked := l.DispatchForTest(session.ID, host.Command{
		SessionID: session.ID, Kind: "rename",
		Payload: mustJSON(t, host.TextPayload{Text: "改不了"}),
	})
	if blocked.OK {
		t.Fatal("归档时不该能改名")
	}

	if err := target.SetArchived(false); err != nil {
		t.Fatal(err)
	}
	allowed := l.DispatchForTest(session.ID, host.Command{
		SessionID: session.ID, Kind: "rename",
		Payload: mustJSON(t, host.TextPayload{Text: "能改了"}),
	})
	if !allowed.OK {
		t.Fatalf("恢复后应能改名：%v", allowed.Error)
	}
	if got := l.Session(session.ID).Title; got != "能改了" {
		t.Fatalf("标题应更新，实际 %q", got)
	}
}

// 归档落在**项目档案**里：重启后读回来仍是归档态。
//
// 不落盘的话，重启后归档的会话又能跑了 —— 状态自相矛盾。
func TestArchiveStatePersistsInProjectRecord(t *testing.T) {
	dir := t.TempDir()
	catalog := store.NewCatalog(dir)
	l := newHostWithCatalog(t, catalog)

	session := l.NewSession("")
	// 手工登记进档案（不跑真实对话：那要手动驱动假模型的事件流，
	// 与这里想验证的"状态位落盘"无关）
	if err := catalog.AddSession(session.ID, "测试会话", "", ""); err != nil {
		t.Fatal(err)
	}

	result := l.DispatchForTest(session.ID, host.Command{
		SessionID: session.ID, Kind: "archiveSession",
	})
	if !result.OK {
		t.Fatalf("归档应成功：%v", result.Error)
	}

	archived, found := catalog.SessionArchived(session.ID)
	if !found {
		t.Fatal("会话应已登记进项目档案")
	}
	if !archived {
		t.Fatal("档案里应记录为已归档")
	}

	// 恢复后再查一次
	if r := l.DispatchForTest(session.ID, host.Command{
		SessionID: session.ID, Kind: "restoreSession",
	}); !r.OK {
		t.Fatalf("恢复应成功：%v", r.Error)
	}
	if archived, _ := catalog.SessionArchived(session.ID); archived {
		t.Fatal("恢复后档案里不该再是归档态")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// newHostWithCatalog 造一个带项目档案的 Local（归档状态要靠它落盘）。
func newHostWithCatalog(t *testing.T, catalog *store.Catalog) *host.Local {
	t.Helper()
	l := host.NewLocal(host.Config{
		Runtime:       &fakeRuntime{},
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Workspace:     t.TempDir(),
		MaxToolRounds: 3,
		ContextWindow: 200_000,
		Catalog:       catalog,
		Post:          (&poster{}).Post,
	})
	t.Cleanup(func() { _ = l.Close() })
	return l
}