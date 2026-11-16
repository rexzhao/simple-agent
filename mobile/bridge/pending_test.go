package bridge

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/render"
	"simple-agent/internal/timeline"
)

// toolPendingItem 造一个"有 tool_use 但没有 tool_result"的过程组。
func toolPendingItem(t *testing.T) timeline.Item {
	t.Helper()
	conv := conversation.New("ctx")
	entry, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			message.NewDataPart(map[string]any{"id": "c1", "name": "shell"}).
				WithMediaType(render.MediaToolUse),
		},
	}, conversation.StatusComplete)
	if err != nil {
		t.Fatal(err)
	}
	return timeline.Item{
		Kind: timeline.KindProcess,
		Key:  "proc:a1#0",
		Refs: []timeline.Ref{{Chat: entry, Index: 0}},
	}
}

// 工具正在执行时，那个还没结果的调用必须标成 Pending（界面显示"等待结果"）。
//
// 回归测试：工具执行期间模型已经把消息标成 complete，所以 item.Live
// （"这一组还在流式"）是 false。上一版只看 item.Live，于是整个执行期间
// 界面上不转圈也不显示"等待结果"，看起来像卡死了。
func TestToolPendingWhileRunningTools(t *testing.T) {
	item := toolPendingItem(t)
	client := &Client{}

	// 会话正忙着跑工具：消息不是流式（streaming=false），但工具在跑
	steps := client.processSteps(item, false, false, driver.BusyRunningTools)
	if len(steps) != 1 {
		t.Fatalf("应有 1 个步骤，实际 %d", len(steps))
	}
	if !steps[0].Pending {
		t.Fatal("工具执行期间，没有结果的调用应标 Pending（界面显示\"等待结果\"）")
	}
	if steps[0].Done {
		t.Fatal("还没有结果，不该标 Done")
	}
}

// 工具执行中，组标题也要显示转圈（process.streaming=true）。
//
// 否则用户看到的是一个不动的折叠标题 + 不动的详情，以为程序卡住了。
func TestProcessViewStreamingWhileRunningTools(t *testing.T) {
	item := toolPendingItem(t)
	client := &Client{lang: ""}

	view := client.processView(item, false, false, false, driver.BusyRunningTools)
	if view.Streaming != true {
		t.Fatal("工具执行中，过程组应显示进行中（转圈）")
	}
	// 而且必须带详情：Kotlin 侧看到 streaming 为 true 会跳过拉详情，
	// 这里不带的话展开后是空的。
	if !view.Detail || len(view.Steps) == 0 {
		t.Fatal("工具执行中的组必须自带 Steps（否则展开一片空白）")
	}
}

// 会话彻底空闲时，没有结果的调用按数据异常处理，不该谎称还在等。
func TestToolNotPendingWhenIdle(t *testing.T) {
	item := toolPendingItem(t)
	client := &Client{}

	steps := client.processSteps(item, false, true, driver.BusyIdle)
	if len(steps) != 1 {
		t.Fatalf("应有 1 个步骤，实际 %d", len(steps))
	}
	if steps[0].Pending {
		t.Fatal("会话已空闲，没有结果属于数据异常，不该标 Pending")
	}
}

// 模型还在流式输出时，没有结果的调用同样要标 Pending。
func TestToolPendingWhileStreaming(t *testing.T) {
	item := toolPendingItem(t)
	client := &Client{}

	steps := client.processSteps(item, true, false, driver.BusyStreaming)
	if len(steps) != 1 || !steps[0].Pending {
		t.Fatal("流式期间没有结果的调用应标 Pending")
	}
}

// 有结果的调用不该标 Pending（对照组，防止条件写宽了）。
func TestToolWithResultNotPending(t *testing.T) {
	conv := conversation.New("ctx")
	entry, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			message.NewDataPart(map[string]any{"id": "c1", "name": "shell"}).
				WithMediaType(render.MediaToolUse),
			message.NewDataPart(map[string]any{
				"id": "c1", "name": "shell", "output": "ok",
			}).WithMediaType(render.MediaToolResult),
		},
	}, conversation.StatusComplete)
	if err != nil {
		t.Fatal(err)
	}
	item := timeline.Item{
		Kind: timeline.KindProcess,
		Key:  "proc:a1#0",
		Refs: []timeline.Ref{
			{Chat: entry, Index: 0},
			{Chat: entry, Index: 1},
		},
	}

	client := &Client{}
	steps := client.processSteps(item, false, false, driver.BusyRunningTools)
	if len(steps) != 1 {
		t.Fatalf("use+result 应合成 1 张卡片，实际 %d", len(steps))
	}
	if steps[0].Pending {
		t.Fatal("已经有结果了，不该标 Pending")
	}
	if !steps[0].Done {
		t.Fatal("有结果的调用应标 Done")
	}
}