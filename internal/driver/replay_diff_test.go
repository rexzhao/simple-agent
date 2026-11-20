package driver_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/render"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// edit 的 diff 要能**重放回来**：写进日志 → 重新加载 → 界面仍能画 diff。
//
// 真实踩到的：编辑过的文件，重启后详情里只剩一句
// "已编辑 xxx（+1 -1）"，红绿行不见了 —— 重放构造 part 时漏了 diff 字段。
func TestToolResultDiffSurvivesReplay(t *testing.T) {
	diff := tools.BuildLineDiff("panel.lua",
		"a\nb\nc\n", "a\nc\n", tools.LineDiffOptions{})
	if diff.Removed != 1 {
		t.Fatalf("前提不成立：应 -1，实际 -%d", diff.Removed)
	}

	replayed, err := driver.Replay(withSeq([]store.Event{
		store.MustEvent(store.TypeSessionCreated, store.SessionCreatedData{
			ContextID: "ctx", Title: "测试",
		}),
		store.MustEvent(store.TypeMessageInput, driver.MessageEventData{
			Message: message.Message{
				MessageID: "u1", Role: message.RoleUser,
				Parts: []message.Part{message.NewTextPart("改一下")},
			},
			Status: conversation.StatusComplete,
		}),
		store.MustEvent(store.TypeMessageOutput, driver.MessageEventData{
			Message: message.Message{
				MessageID: "a1", Role: message.RoleAgent,
				// 必须先有 tool_use，重放才找得到配对的消息
				Parts: []message.Part{{
					Content: message.DataPart{Data: map[string]any{
						"id": "c1", "name": "edit",
						"args": map[string]any{"file_path": "panel.lua"},
					}},
					MediaType: render.MediaToolUse,
				}},
			},
			Status: conversation.StatusComplete,
		}),
		// 工具结果事件里带着 diff（落盘时就该有）
		store.MustEvent(store.TypeToolResult, driver.ToolResultData{
			CallID: "c1",
			ToolResultRecord: driver.ToolResultRecord{
				ID: "c1", Name: "edit",
				Output: "已编辑 panel.lua（+0 -1）",
				Diff:   diff,
			},
		}),
	}))
	if err != nil {
		t.Fatal(err)
	}

	// 重放后要能从 part 里重新拿到 diff
	found := false
	for _, entry := range replayed.Conversation.Entries() {
		if entry.Message == nil || entry.Message.Message == nil {
			continue
		}
		for _, part := range entry.Message.Message.Parts {
			if part.MediaType != render.MediaToolResult {
				continue
			}
			data, ok := partContent(part)
			if !ok {
				continue
			}
			if data["diff"] == nil {
				t.Fatalf("重放后 part 里缺少 diff：%+v", data)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("没找到工具结果 part")
	}
}

// partContent 取出 data part 的内容（测试内联，避免依赖未导出实现）。
func partContent(part message.Part) (map[string]any, bool) {
	data, ok := part.Content.(message.DataPart)
	if !ok {
		return nil, false
	}
	fields, ok := data.Data.(map[string]any)
	return fields, ok
}