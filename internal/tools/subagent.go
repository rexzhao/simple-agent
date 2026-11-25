package tools

import (
	"context"
	"fmt"
	"strings"
)

// 父子会话通讯工具（subagent 组）。
//
// 按角色**只注册该有的**（见 Options.Role）：
//
//	父会话（顶层）：subagent_list + subagent_notify_child + subagent_read
//	子会话：        subagent_notify_parent + subagent_read
//
// 为什么不注册成一个通用的 send：模型一看到"发给任意会话"就会试着乱发，
// 而这里的语义边界（只和父/子通信）恰恰是隐私与成本的关键。
// 不注册比"注册了报错"更干净 —— 模型不会白读一段描述、也不会试错一次。
//
// **subagent_read 两个角色都要**：通知是双向的，能发就必须能收。
// 早期版本只有发送、没有读取，结果是"能看到有 1 条未读、正文永远读不出来"
// —— 通讯只通一半，比完全不通更糟（子会话的汇报全在正文里）。

func subagentListTool() *ToolDef {
	return &ToolDef{
		Name: "subagent_list",
		Description: "List YOUR child sessions (you are their parent), including what each is " +
			"doing right now: streaming, which tool it is running, and how long the current " +
			"round has been going. Use it to decide whether it is worth interrupting a child.",
		Guidance: "These tools talk to the child sessions you spawned. Check subagent_list first: " +
			"a child that is streaming or running a tool will read your notice at its next " +
			"turn, not immediately — so notifying it now costs nothing but changes nothing " +
			"until it finishes.",
		Parameters: schema(map[string]any{
			"includeArchived": map[string]any{"type": "boolean",
				"description": "Also list archived children. Defaults to false — archived " +
					"sessions cannot receive notices."},
		}, "includeArchived"),
		Run: subagentListRun,
	}
}

func subagentNotifyParentTool() *ToolDef {
	return &ToolDef{
		Name: "subagent_notify_parent",
		Description: "You are a CHILD session spawned by another session. Use this to report " +
			"a result, ask for a decision, or hand back a finding. The parent is woken by this " +
			"(or sees it at its next turn). Do not notify for pleasantries — every notify " +
			"costs the parent a turn.",
		Guidance: "You were spawned by another session and normally cannot see its " +
			"conversation. Use subagent_notify_parent to report results, ask for a decision, " +
			"or escalate a blocker. When your task is done, notify the parent instead of just " +
			"ending — otherwise it is left waiting for you.",
		Parameters: schema(map[string]any{
			"subject": map[string]any{"type": "string",
				"description": "One-line subject (max 200 chars). The parent sees this first " +
					"(the unread notice only shows subjects, not the body)."},
			"body": map[string]any{"type": "string",
				"description": "Full message body (max 64 KiB). Include the concrete details " +
					"the parent needs; it cannot see your conversation."},
		}, "subject", "body"),
		Run: subagentNotifyParentRun,
	}
}

func subagentNotifyChildTool() *ToolDef {
	return &ToolDef{
		Name: "subagent_notify_child",
		Description: "Notify ONE of your child sessions. Check subagent_list first — if it is " +
			"already streaming or running a tool, the message is queued for its next turn " +
			"instead of waking it. One target per call; notify several by calling several times.",
		Guidance: "These tools talk to the child sessions you spawned. Check subagent_list first: " +
			"a child that is streaming or running a tool will read your notice at its next " +
			"turn, not immediately — so notifying it now costs nothing but changes nothing " +
			"until it finishes.",
		Parameters: schema(map[string]any{
			"to": map[string]any{"type": "string",
				"description": "Session id of the child to notify (get ids from subagent_list). " +
					"Exactly one target per call."},
			"subject": map[string]any{"type": "string",
				"description": "One-line subject (max 200 chars). The child sees this first."},
			"body": map[string]any{"type": "string",
				"description": "Full message body (max 64 KiB). Include the concrete details " +
					"it needs; it cannot see your conversation."},
		}, "to", "subject", "body"),
		Run: subagentNotifyChildRun,
	}
}

func subagentReadTool() *ToolDef {
	return &ToolDef{
		Name: "subagent_read",
		Description: "Read notices from your parent or child sessions. Without an id it lists " +
			"what is in your inbox (ids + subjects + sender); with an id it returns that " +
			"notice's full body and REMOVES it from the inbox. The unread notice on its own " +
			"only shows subjects — always call this before acting on one.",
		Guidance: "The unread notice tells you a message arrived but not what it says: " +
			"call subagent_read (no arguments) to see the list, then subagent_read with an id " +
			"to read the body — reading it also removes it from your inbox, so it cannot " +
			"pile up. Do this before deciding whether any action is needed.",
		Parameters: schema(map[string]any{
			"id": map[string]any{"type": "string",
				"description": "Notice id from the unread notice or from a previous " +
					"subagent_read listing. Omit to list the inbox instead."},
		}, "id"),
		Run: subagentReadRun,
	}
}

func subagentListRun(_ context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	host := tc.Subagent
	if host == nil {
		return ToolReturn{}, fmt.Errorf("父子会话通讯不可用")
	}
	peers := host.Children(args.Bool("includeArchived"))
	if len(peers) == 0 {
		return ToolReturn{Output: "你没有子会话。"}, nil
	}
	lines := make([]string, 0, len(peers))
	for _, peer := range peers {
		line := fmt.Sprintf("- %s  %s", peer.ID, peer.Title)
		if peer.Archived {
			line += "（已归档，不能收通知）"
		}
		if detail := describeActivity(peer.Activity); detail != "" {
			line += "  " + detail
		}
		lines = append(lines, line)
	}
	return ToolReturn{
		Output: fmt.Sprintf("你有 %d 个子会话：\n%s", len(peers), strings.Join(lines, "\n")),
	}, nil
}

// describeActivity 把活动明细压成一句人能读的短语。
//
// 空 Busy（档案兜底来的会话，当前进程没加载）返回空串：
// 说"未知"比编一个"空闲"更诚实。
func describeActivity(activity SubagentActivity) string {
	switch activity.Busy {
	case "":
		return ""
	case SubagentBusyIdle:
		return "空闲"
	case SubagentBusyStreaming:
		return "正在生成回答" + elapsedSuffix(activity.ElapsedMS)
	case SubagentBusyRunningTool:
		name := activity.Tool
		if name == "" {
			name = "工具"
		}
		return "正在执行 " + name + elapsedSuffix(activity.ElapsedMS)
	case SubagentBusyAwaiting:
		return "等待用户回答" + elapsedSuffix(activity.ElapsedMS)
	case SubagentBusyCompacting:
		return "正在压缩上下文" + elapsedSuffix(activity.ElapsedMS)
	default:
		return activity.Busy
	}
}

// elapsedSuffix 返回"（已运行 12 秒）"这种后缀（0 时返回空）。
func elapsedSuffix(milliseconds int64) string {
	if milliseconds <= 0 {
		return ""
	}
	seconds := milliseconds / 1000
	if seconds < 60 {
		return fmt.Sprintf("（已运行 %d 秒）", seconds)
	}
	return fmt.Sprintf("（已运行 %d 分 %d 秒）", seconds/60, seconds%60)
}

func subagentReadRun(_ context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	host := tc.Subagent
	if host == nil {
		return ToolReturn{}, fmt.Errorf("父子会话通讯不可用")
	}
	id := strings.TrimSpace(args.OptStr("id"))
	if id == "" {
		list := host.Inbox(false, 20)
		if len(list) == 0 {
			return ToolReturn{Output: "你的收件箱是空的。"}, nil
		}
		lines := make([]string, 0, len(list))
		for _, mail := range list {
			flag := " "
			if !mail.Read {
				flag = "●"
			}
			from := mail.FromTitle
			if from == "" {
				from = mail.From
			}
			lines = append(lines, fmt.Sprintf("%s [%s] %s — %s",
				flag, mail.ID, mail.Subject, from))
		}
		return ToolReturn{Output: fmt.Sprintf(
			"%d 条通知（● 未读）：\n%s\n\n用 subagent_read 带 id 读正文。",
			len(list), strings.Join(lines, "\n"))}, nil
	}
	mail, err := host.ReadNotice(id)
	if err != nil {
		return ToolReturn{}, err
	}
	from := mail.FromTitle
	if from == "" {
		from = mail.From
	}
	output := fmt.Sprintf("主题：%s\n发件人：%s（%s）\n\n%s",
		mail.Subject, from, mail.From, mail.Body)
	return ToolReturn{Output: output}, nil
}

func subagentNotifyParentRun(_ context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	host := tc.Subagent
	if host == nil {
		return ToolReturn{}, fmt.Errorf("父子会话通讯不可用")
	}
	subject, err := args.Str("subject")
	if err != nil {
		return ToolReturn{}, err
	}
	body, err := args.Str("body")
	if err != nil {
		return ToolReturn{}, err
	}
	delivery, err := host.NotifyParent(subject, body)
	if err != nil {
		return ToolReturn{}, err
	}
	output := fmt.Sprintf("已通知父会话「%s」（邮件 id=%s）。", delivery.Title, delivery.MailID)
	if delivery.Woken {
		output += "\n它当时空闲，已被唤醒去处理。"
	} else if delivery.Skipped != "" {
		output += "\n" + delivery.Skipped
	}
	return ToolReturn{Output: output}, nil
}

func subagentNotifyChildRun(_ context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	host := tc.Subagent
	if host == nil {
		return ToolReturn{}, fmt.Errorf("父子会话通讯不可用")
	}
	to, err := args.Str("to")
	if err != nil {
		return ToolReturn{}, err
	}
	subject, err := args.Str("subject")
	if err != nil {
		return ToolReturn{}, err
	}
	body, err := args.Str("body")
	if err != nil {
		return ToolReturn{}, err
	}
	delivery, err := host.NotifyChild(strings.TrimSpace(to), subject, body)
	if err != nil {
		return ToolReturn{}, err
	}
	output := fmt.Sprintf("已通知子会话「%s」（邮件 id=%s）。", delivery.Title, delivery.MailID)
	if delivery.Activity != nil {
		if detail := describeActivity(*delivery.Activity); detail != "" {
			output += "\n它当时：" + detail + "。"
		}
	}
	if delivery.Woken {
		output += "\n它已被唤醒去处理。"
	} else if delivery.Skipped != "" {
		output += "\n" + delivery.Skipped
	}
	return ToolReturn{Output: output}, nil
}
