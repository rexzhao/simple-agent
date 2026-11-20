package uiapp

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/render"
	"simple-agent/internal/timeline"
)

// 步数口径统一：标题、「上面还有 N 步」、「展开全部 N 步」必须是同一个数。
//
// 用户报的：标题「6 步」，但「上面还有 10 步」「展开全部 13 步」——
// 三个数字互相打架。原因是标题只数 tool_use，另两个数的是 part 数
// （tool_result 也被算进去，一个调用凭空变两步）。
func TestStepCountExcludesToolResult(t *testing.T) {
	// 3 组（思考 + 调用 + 结果）：真实步骤 6，part 数 9。
	// 差的 3 个就是 tool_result。
	chat := &conversation.ChatEntry{Message: &message.Message{
		MessageID: "a1", Role: message.RoleAgent,
	}}
	for i := 0; i < 3; i++ {
		chat.Message.Parts = append(chat.Message.Parts,
			message.Part{Content: message.TextPart{Text: "想"},
				MediaType: render.MediaReasoning},
			message.Part{Content: message.DataPart{Data: map[string]any{
				"id": "c", "name": "read",
			}}, MediaType: render.MediaToolUse},
			message.Part{Content: message.DataPart{Data: map[string]any{
				"id": "c", "name": "read", "output": "ok",
			}}, MediaType: render.MediaToolResult},
		)
	}

	// uiapp 侧：只留下会渲染成行的 part
	refs := make([]partRef, 0, len(chat.Message.Parts))
	for i := range chat.Message.Parts {
		refs = append(refs, partRef{Chat: chat, Index: i})
	}
	if got := len(stepRefs(refs)); got != 6 {
		t.Fatalf("3 组（思考+调用+结果）应是 6 步，实际 %d（part 数 %d）",
			got, len(chat.Message.Parts))
	}

	// timeline 侧：标题用的 StepCount 必须是同一个数
	trs := make([]timeline.Ref, 0, len(chat.Message.Parts))
	for i := range chat.Message.Parts {
		trs = append(trs, timeline.Ref{Chat: chat, Index: i})
	}
	if got := timeline.StepCount(trs); got != 6 {
		t.Fatalf("StepCount 也应是 6，实际 %d", got)
	}
}