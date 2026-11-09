package tools

import (
	"context"
	"fmt"
	"strings"
)

func todoWriteTool() *ToolDef {
	return &ToolDef{
		Name: "todo_write",
		Description: "Record and update a task list to plan multi-step work and show progress; " +
			"skip it for trivial single-step tasks. Add one todo per concrete step before you start. " +
			"While work remains, keep exactly one todo `in_progress`. " +
			"Mark each todo `completed` as soon as it is done. " +
			"Content must be unique and non-empty.",
		Parameters: schema(map[string]any{
			"todos": map[string]any{
				"type":        "array",
				"description": "The COMPLETE task list, replacing any previous list.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content": map[string]any{"type": "string",
							"description": "What the task is — a short imperative line."},
						"status": map[string]any{
							"type":        "string",
							"enum":        []string{"pending", "in_progress", "completed"},
							"description": "pending (not started) | in_progress (now) | completed (done).",
						},
					},
					"required": []string{"content", "status"},
				},
			},
		}, "todos"),
		Run: todoWriteRun,
	}
}

// todoWriteRun 校验并整体替换任务清单。
//
// 校验对齐 dsh（`toTodoList`）：content 非空、**不重复**、`in_progress` 最多一个。
// 后两条以前只是"提个醒"照收，现在直接拒绝——理由是照收会让界面呈现与模型
// 认知不一致：重复 content 会让按内容做 key 的列表错乱，多个 in_progress 则
// 让「当前在做哪一步」失去意义。
func todoWriteRun(_ context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	items, err := args.Items("todos")
	if err != nil {
		return ToolReturn{}, err
	}

	todos := make([]TodoItem, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	inProgress := 0
	for i, item := range items {
		content, _ := item["content"].(string)
		content = strings.TrimSpace(content)
		if content == "" {
			return ToolReturn{}, fmt.Errorf("todos[%d].content 不能为空", i)
		}
		if _, duplicate := seen[content]; duplicate {
			return ToolReturn{}, fmt.Errorf("todos[%d].content 与前文重复：%q", i, content)
		}
		seen[content] = struct{}{}

		rawStatus, _ := item["status"].(string)
		var status TodoStatus
		switch TodoStatus(rawStatus) {
		case TodoPending, TodoInProgress, TodoCompleted:
			status = TodoStatus(rawStatus)
		default:
			return ToolReturn{}, fmt.Errorf(
				"todos[%d].status 非法：%q（可选 pending/in_progress/completed）", i, rawStatus)
		}
		if status == TodoInProgress {
			inProgress++
		}
		todos = append(todos, TodoItem{Content: content, Status: status})
	}

	// 整次调用失败、清单保持不变：让模型看到真实原因后自己重发
	if inProgress > 1 {
		return ToolReturn{}, fmt.Errorf(
			"最多只能有一个任务处于 in_progress，实际有 %d 个", inProgress)
	}

	tc.setTodos(todos)

	if len(todos) == 0 {
		return OutputOf("任务列表已清空"), nil
	}

	completed := 0
	for _, todo := range todos {
		if todo.Status == TodoCompleted {
			completed++
		}
	}

	var out strings.Builder
	fmt.Fprintf(&out, "任务列表已更新（%d/%d 完成）：", completed, len(todos))
	for _, todo := range todos {
		fmt.Fprintf(&out, "\n%s %s", todo.Status.glyph(), todo.Content)
	}
	return OutputOf(out.String()), nil
}
