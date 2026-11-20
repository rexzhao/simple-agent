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

// 压缩快照必须带上工作区，否则压缩后会话会掉回"启动目录"那个分组。
//
// 真实踩到的：某个 wow-viewer 的会话压缩过之后，重放时读不到工作区
// （session.created 已经被归档掉了，而工作区只记在那条事件里），
// 于是 host 回退到配置值 —— 而配置没设 workspace，最终用启动目录，
// 会话就跑到 simple-agent 下面去了。
//
// 用户的话是："那个会话怎么变成 simple-agent 下面的了？"
func TestSnapshotCarriesWorkspace(t *testing.T) {
	// 新快照带 workspace → 重放能读回来
	snapshot := store.MustEvent(store.TypeCompactSnapshot, map[string]any{
		"context_id":         "ctx-ws",
		"summary":            "摘要",
		"summary_message_id": "s1",
		"kept_entries":       []any{},
		"compacted_at":       "2026-01-01T00:00:00Z",
		"workspace":          `F:\wow-viewer`,
	})
	replayed, err := driver.Replay([]store.Event{snapshot})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Workspace != `F:\wow-viewer` {
		t.Fatalf("应从快照读出工作区，实际 %q", replayed.Workspace)
	}

	// 恢复到 driver 的 opts 里（下次压缩要用它写新快照）
	d, _, err := driver.Load([]store.Event{snapshot}, driver.Options{
		Selection:    providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手",
		Tools:        tools.Builtin(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.WorkspaceForTest(); got != `F:\wow-viewer` {
		t.Fatalf("driver 应恢复工作区，实际 %q", got)
	}
}

// 旧快照（没有 workspace 字段）不能报错，也不能编一个值出来。
//
// 这种情况仍需 host 按旧行为回退（配置 → 启动目录）。
// 如实为空比造一个假工作区诚实：那个会话本来就没记住自己在哪。
func TestOldSnapshotWithoutWorkspace(t *testing.T) {
	snapshot := store.MustEvent(store.TypeCompactSnapshot, map[string]any{
		"context_id":         "ctx-old",
		"summary":            "摘要",
		"summary_message_id": "s1",
		"kept_entries":       []any{},
		"compacted_at":       "2026-01-01T00:00:00Z",
	})
	replayed, err := driver.Replay([]store.Event{snapshot})
	if err != nil {
		t.Fatalf("旧快照不该解析失败：%v", err)
	}
	if replayed.Workspace != "" {
		t.Fatalf("旧快照没有工作区时应为空，实际 %q", replayed.Workspace)
	}
}

// 压缩产出的快照里要真的写进 workspace（不只是能读）。
func TestCompactWritesWorkspace(t *testing.T) {
	conv := conversation.New("ctx-ws-compact")
	for i := 0; i < 20; i++ {
		conv.AddMessage(message.Message{
			MessageID: "u" + itoa(i), Role: message.RoleUser,
			Parts: []message.Part{message.NewTextPart("问题")},
		}, conversation.StatusComplete)
		conv.AddMessage(message.Message{
			MessageID: "a" + itoa(i), Role: message.RoleAgent,
			Parts: []message.Part{message.NewTextPart("回答，写长一些好让压缩有东西可折。")},
		}, conversation.StatusComplete)
	}
	d := driver.New(conv, driver.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Tools:         tools.Builtin(false),
		ContextWindow: 2000,
		Workspace:     `F:\wow-viewer`,
	})
	effects, err := d.Send("再来")
	if err != nil {
		t.Fatal(err)
	}
	messageID := findStartStream(effects)
	if messageID == "" {
		t.Fatal("没有 StartStream")
	}

	effects = d.FailStreamWith(messageID,
		`http (HTTP 413): {"error":{"message":"Entry too big"}}`)
	if !hasStartSummary(effects) {
		t.Fatal("应触发压缩")
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
		t.Fatal("压缩没有产出快照")
	}
	var data struct {
		Workspace string `json:"workspace"`
	}
	if err := snapshot.Decode(&data); err != nil {
		t.Fatal(err)
	}
	if data.Workspace != `F:\wow-viewer` {
		t.Fatalf("快照里应写入工作区，实际 %q", data.Workspace)
	}
}