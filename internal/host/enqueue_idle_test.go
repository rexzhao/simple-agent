package host_test

import (
	"testing"

	"simple-agent/internal/driver"
	"simple-agent/internal/host"

	"simple-agent/internal/store"
)

// 新会话"状态还没拉到"时发消息：**必须能发出去**。
//
// 用户报的：手机端新建会话后点发送没反应，提示"当前状态不支持该操作"。
//
// 链条：客户端根据上一次拉到的 runtime.canSend 决定调 send 还是 enqueue；
// 新会话刚建、状态还没回来时 canSend=false → 走 enqueue；而空闲会话
// 不能插话，Enqueue 返回 ErrBusy。后端兜底：空闲时 enqueue 降级为发送。
func TestEnqueueOnIdleSessionSendsInsteadOfFailing(t *testing.T) {
	catalog := store.NewCatalog(t.TempDir())
	l := newHostWithCatalog(t, catalog)
	session := l.NewSession(`F:\work\alpha`)

	// 直接走 enqueue（模拟客户端 canSend=false 的那条路径）
	result := l.DispatchForTest(session.ID, host.Command{
		SessionID: session.ID, Kind: "enqueue",
		Payload: mustJSON(t, host.TextPayload{Text: "第一条消息"}),
	})
	if !result.OK {
		t.Fatalf("空闲会话的 enqueue 应降级为发送，实际报错：%q", result.Error)
	}

	// 真的开流了
	if got := l.Session(session.ID).Busy(); got != driver.BusyStreaming {
		t.Fatalf("应进入 streaming，实际 %v", got)
	}

	// 且立刻登记进项目档案（不会出现"另一个同名项目"）
	projects, err := catalog.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || len(projects[0].Sessions) != 1 {
		t.Fatalf("应有 1 个项目 1 个会话，实际 %+v", projects)
	}
}