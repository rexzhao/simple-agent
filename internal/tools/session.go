package tools

import (
	"context"
	"fmt"
	"strings"
)

// 新建会话工具：在**同一个项目**里开一个子会话。
//
// 用途：把一件独立的事分出去做，不干扰当前会话的上下文。
// 与 mailbox 的区别：邮件是"给已存在的会话递消息"，这里是"开一个新的
// 干活的地方"，新会话从打开那一刻就能看到父会话派它做什么。

func newSessionTool() *ToolDef {
	return &ToolDef{
		Name: "new_session",
		Description: "Start a NEW session in the same project, marked as spawned by this one. " +
			"Use it to hand off independent work that would otherwise clutter this conversation. " +
			"The new session gets its own context; the parent/child link is visible in the sidebar. " +
			"This tool does NOT wait for the child to finish — use mailbox_send / mailbox_read to coordinate.",
		Parameters: schema(map[string]any{
			"title": map[string]any{"type": "string",
				"description": "Short title for the new session (shown in the sidebar). " +
					"Use something the user can recognize at a glance."},
			"prompt": map[string]any{"type": "string",
				"description": "The task for the new session, written as a complete, self-contained " +
					"instruction — it cannot see this conversation. Empty starts an idle session."},
		}, "title"),
		Run: newSessionRun,
	}
}

func newSessionRun(_ context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	host := tc.Sessions
	if host == nil {
		return ToolReturn{}, fmt.Errorf("当前环境不支持新建会话")
	}
	title, _ := args.Str("title")
	title = strings.TrimSpace(title)
	prompt := args.OptStr("prompt")

	id, err := host.NewChildSession(title, prompt)
	if err != nil {
		return ToolReturn{}, err
	}
	output := fmt.Sprintf("已在同一项目里新建会话「%s」（id=%s）。", title, id)
	if prompt != "" {
		output += "\n它已经收到你交代的任务，会自己跑起来。"
	}
	output += "\n它看不到这个会话的内容；要传更多信息或收结果，用 mailbox_send 发给它。"
	return ToolReturn{Output: output}, nil
}
