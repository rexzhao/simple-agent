package driver_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// SetWorkspace 要让后续压缩写对工作区。
//
// 真实踩到的链条：
//  1. host 打开会话时先拿启动目录建 driver（那时还没解析日志里的工作区）
//  2. 解析出真值后没回写 → driver 里仍是启动目录
//  3. 用户压缩一次 → 快照把启动目录固化下来
//  4. 从此这个会话永远归到启动目录那个分组（wow-viewer 的会话变成 simple-agent）
//
// 第 2 步的回写就是 SetWorkspace 的职责。
func TestSetWorkspaceAffectsCompact(t *testing.T) {
	conv := conversation.New("ctx-ws-set")
	for i := 0; i < 20; i++ {
		conv.AddMessage(message.Message{
			MessageID: "u" + itoa(i), Role: message.RoleUser,
			Parts: []message.Part{message.NewTextPart("问题")},
		}, conversation.StatusComplete)
		conv.AddMessage(message.Message{
			MessageID: "a" + itoa(i), Role: message.RoleAgent,
			Parts: []message.Part{message.NewTextPart("回答，写长一点好让压缩有东西可折。")},
		}, conversation.StatusComplete)
	}
	d := driver.New(conv, driver.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Tools:         tools.Builtin(false),
		ContextWindow: 2000,
		// 模拟 host 打开会话时的初始值：启动目录，不是真值
		Workspace: `F:\work\simple-agent`,
	})

	// host 解析出真值后回写
	d.SetWorkspace(`F:\wow-viewer`)

	effects, err := d.Send("再来")
	if err != nil {
		t.Fatal(err)
	}
	messageID := findStartStream(effects)
	if messageID == "" {
		t.Fatal("没有 StartStream")
	}
	// 让这一轮流结束（超限失败会触发压缩路径，状态回到 Idle）
	overflowEffects := d.FailStreamWith(messageID,
		`http (HTTP 413): {"error":{"message":"Entry too big"}}`)
	if !hasStartSummary(overflowEffects) {
		t.Fatalf("应触发压缩，实际 effect 数：%d", len(overflowEffects))
	}
	after, err := d.ApplyCompact("摘要")
	if err != nil {
		t.Fatalf("压缩失败：%v", err)
	}

	var snapshot store.Event
	for _, effect := range after {
		if rec, ok := effect.(driver.Record); ok && rec.Event.Type == store.TypeCompactSnapshot {
			snapshot = rec.Event
		}
	}
	if snapshot.Type == "" {
		t.Fatal("没有产出快照")
	}
	var data struct {
		Workspace string `json:"workspace"`
	}
	if err := snapshot.Decode(&data); err != nil {
		t.Fatal(err)
	}
	if data.Workspace != `F:\wow-viewer` {
		t.Fatalf("压缩应写回写后的工作区 F:\\wow-viewer，实际 %q（说明 SetWorkspace 没生效）", data.Workspace)
	}
}
