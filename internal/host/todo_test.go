package host_test

import (
	"context"
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
	"simple-agent/internal/wire"
)

// todoRuntime 让模型发起一次 todo_write，然后收尾。
//
// 用它而不是假的 RunTools：清单的同步发生在**真实工具执行之后**
// （host/session.go 读 ToolContext.Todos()），假实现会绕开这条链路，
// 测试就测不到真正会出问题的地方。
type todoRuntime struct {
	*p2Runtime
	todos []map[string]any
}

func (r *todoRuntime) RunTools(ctx context.Context, tc *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
	out := make([]tools.ToolOutcome, 0, len(calls))
	for _, call := range calls {
		if call.Name != "todo_write" {
			out = append(out, tools.ToolOutcome{ID: call.ID, Name: call.Name, Output: "ok"})
			continue
		}
		tool, ok := tools.Find(tools.Builtin(false), "todo_write")
		if !ok {
			out = append(out, tools.ToolOutcome{ID: call.ID, Name: call.Name, Output: "缺工具", IsError: true})
			continue
		}
		// 参数要包成 []any：工具层的 Args.Items 只认这个类型（JSON 解出来的形态），
		// 直接给 []map[string]any 会被判成"必须是数组"。
		raw := make([]any, 0, len(r.todos))
		for _, todo := range r.todos {
			raw = append(raw, todo)
		}
		result, err := tool.Run(ctx, tools.Args{"todos": raw}, tc)
		output := result.Output
		isError := false
		if err != nil {
			output, isError = err.Error(), true
		}
		out = append(out, tools.ToolOutcome{ID: call.ID, Name: call.Name, Output: output, IsError: isError})
	}
	return out
}

// toolUseEvents 构造一轮"模型要求调用 todo_write"的事件序列。
func todoToolCalls(callID string, args string) []providers.Event {
	return []providers.Event{
		{Kind: providers.EventToolStart, Index: 0, ID: callID, Name: "todo_write"},
		{Kind: providers.EventToolArgs, Index: 0, Delta: args},
		{Kind: providers.EventDone, StopReason: "tool_calls"},
	}
}

// 执行端执行 todo_write 之后，展示端能从快照看到清单。
//
// 这是清单在远程模式唯一能到界面的路径：清单不是消息，`log.events` 里只有
// todo.write 事件，中途连上时必须靠 state.snapshot 带过去。
func TestViewerSeesTodos(t *testing.T) {
	rt := &todoRuntime{p2Runtime: newP2Runtime()}
	rt.todos = []map[string]any{
		{"content": "读需求", "status": "completed"},
		{"content": "写代码", "status": "in_progress"},
		{"content": "跑测试", "status": "pending"},
	}
	pair := newP2PairWithRuntime(t, rt)

	pair.send(t, "开始干活")
	live := rt.await(t, 1)
	for _, event := range todoToolCalls("call-1", `{"todos":[]}`) {
		live <- event
	}

	// 工具执行完会**开第二轮**（模型接着说话）：等它出现再喂完，
	// 会话才能回到 Idle。
	pair.pumpUntil(t, func() bool { return rt.count() >= 2 })
	live2 := rt.await(t, 2)
	live2 <- providers.Event{Kind: providers.EventText, Text: "清单已更新"}
	live2 <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}

	// 执行端先拿到清单，再看展示端（两者分别对应"同步"与"快照"两条链路）
	pair.pumpUntil(t, func() bool {
		session := pair.local.Session(pair.sessID)
		return session != nil && len(session.Todos()) == 3
	})
	pair.pumpUntil(t, func() bool {
		session := pair.remote.Session(pair.sessID)
		return session != nil && len(session.Todos()) == 3
	})

	// 执行端与展示端看到的是同一份
	executor := pair.local.Session(pair.sessID)
	viewer := pair.remote.Session(pair.sessID)
	if len(executor.Todos()) != len(viewer.Todos()) {
		t.Fatalf("两端清单长度不一致：%d vs %d", len(executor.Todos()), len(viewer.Todos()))
	}
	for index, todo := range viewer.Todos() {
		if todo != executor.Todos()[index] {
			t.Fatalf("第 %d 项不一致：%#v vs %#v", index, todo, executor.Todos()[index])
		}
	}
	if viewer.Todos()[1].Status != conversation.TodoInProgress {
		t.Fatalf("状态没同步过来：%#v", viewer.Todos())
	}
}

// 用户发新消息时清单清空，且展示端也看到清空。
func TestViewerSeesTodosClearedOnNewMessage(t *testing.T) {
	rt := &todoRuntime{p2Runtime: newP2Runtime()}
	rt.todos = []map[string]any{{"content": "只剩这一项", "status": "pending"}}
	pair := newP2PairWithRuntime(t, rt)

	pair.send(t, "第一轮")
	live := rt.await(t, 1)
	for _, event := range todoToolCalls("call-1", `{"todos":[]}`) {
		live <- event
	}
	// 工具轮之后是**新的**流（不是把第一条继续用完）
	pair.pumpUntil(t, func() bool { return rt.count() >= 2 })
	live2 := rt.await(t, 2)
	live2 <- providers.Event{Kind: providers.EventText, Text: "好了"}
	live2 <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	pair.pumpUntil(t, func() bool {
		session := pair.local.Session(pair.sessID)
		return session != nil && session.Busy() == driver.BusyIdle && len(session.Todos()) == 1
	})
	pair.pumpUntil(t, func() bool {
		session := pair.remote.Session(pair.sessID)
		return session != nil && len(session.Todos()) == 1
	})

	// 新消息 → 清单清空
	pair.send(t, "第二轮")
	pair.pumpUntil(t, func() bool {
		session := pair.local.Session(pair.sessID)
		return session != nil && len(session.Todos()) == 0
	})
	pair.pumpUntil(t, func() bool {
		session := pair.remote.Session(pair.sessID)
		return session != nil && len(session.Todos()) == 0
	})
}

// newP2PairWithRuntime 用给定 runtime 起执行端 + 展示端。
func newP2PairWithRuntime(t *testing.T, rt host.Runtime) *p2Pair {
	t.Helper()
	post := &poster{}
	cfg := host.Config{
		Runtime:       rt,
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Workspace:     t.TempDir(),
		MaxToolRounds: 5,
		ContextWindow: 200_000,
		Post:          post.Post,
	}
	local := host.NewLocal(cfg)
	t.Cleanup(func() { _ = local.Close() })

	viewerConn, executor := wire.PipePair()
	go func() { _ = host.Serve(executor, local, post.Post) }()
	remote, err := host.NewRemote(viewerConn, post.Post)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })

	if local.Active() == nil {
		local.NewSession("")
	}
	pair := &p2Pair{local: local, remote: remote, post: post, sessID: local.Active().ID}
	pair.pumpUntil(t, func() bool { return len(remote.Sessions()) == 1 })
	return pair
}
