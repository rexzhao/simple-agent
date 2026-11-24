package tools

import (
	"context"
	"fmt"
	"strings"
)

// 会话通讯工具：同一项目内的 agent 之间互发邮件。
//
// 为什么做成三个工具而不是一个多功能工具：模型对"发/列/读"三类动作的
// 用途很明确，拆开后每个描述都可以很短；塞进一个工具会让描述变成
// "根据 action 参数做不同的事"那种难读的形态。

func mailboxSendTool() *ToolDef {
	return &ToolDef{
		Name: "mailbox_send",
		Description: "Send a message to another session in THIS project. " +
			"The recipient is woken up (or notified at its next model call) and can read it with mailbox_read. " +
			"Use this to hand off a finding, ask a peer to change something, or reply to earlier mail. " +
			"Only send what the peer actually needs — every message costs it a turn.",
		Parameters: schema(map[string]any{
			"to": map[string]any{"type": "string",
				"description": "Recipient: either an exact session id, or a keyword from its title " +
					"(e.g. \"修复特效\"). If a keyword matches several sessions the send is NOT performed " +
					"and the candidates are returned — resend with one of their ids."},
			"subject": map[string]any{"type": "string",
				"description": "One-line subject (max 200 chars). The recipient sees this first."},
			"body": map[string]any{"type": "string",
				"description": "Full message body (max 64 KiB). Include the concrete details the peer needs; " +
					"it cannot see your conversation."},
			"refs": map[string]any{
				"type":        "array",
				"description": "Optional mail ids this message refers to (when replying, include the original id).",
				"items":       map[string]any{"type": "string"},
			},
		}, "to", "subject", "body"),
		Run: mailboxSendRun,
	}
}

func mailboxListTool() *ToolDef {
	return &ToolDef{
		Name: "mailbox_list",
		Description: "List mail sent to THIS session, newest first. " +
			"The result is an INDEX ONLY (subject lines): read each message with mailbox_read before acting on it — " +
			"a subject alone is not enough to decide what to do.",
		Parameters: schema(map[string]any{
			"unreadOnly": map[string]any{"type": "boolean",
				"description": "Only list unread mail. Defaults to false."},
			"limit": map[string]any{"type": "integer",
				"description": "Maximum number of messages to return. Defaults to 20."},
		}, "limit"),
		Run: mailboxListRun,
	}
}

func mailboxReadTool() *ToolDef {
	return &ToolDef{
		Name: "mailbox_read",
		Description: "Read one message by id and mark it as read. " +
			"Get ids from mailbox_list or from the unread-mail notice.",
		Parameters: schema(map[string]any{
			"id": map[string]any{"type": "string",
				"description": "Mail id, as shown by mailbox_list."},
		}, "id"),
		Run: mailboxReadRun,
	}
}

// argStrings 取字符串数组参数（缺省或类型不对时返回 nil）。
func argStrings(args Args, key string) []string {
	raw, ok := args[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, text)
		}
	}
	return out
}

func mailboxSendRun(_ context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	host := tc.Mailbox
	if host == nil {
		return ToolReturn{}, fmt.Errorf("会话通讯不可用")
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
	refs := argStrings(args, "refs")

	target, candidates, err := resolveRecipient(host.MailboxPeers(), to)
	if err != nil {
		return ToolReturn{}, err
	}
	if target == nil {
		// 命中多个：**不发**，把候选交给模型自己挑。
		//
		// 为什么不让工具自己选一个（比如取第一个）：发错人比发不出去更糟，
		// 而且模型看到候选后能结合上下文判断，比任何启发式都准。
		var lines []string
		for _, peer := range candidates {
			lines = append(lines, fmt.Sprintf("- %s  %s", peer.ID, peer.Title))
		}
		// 返回 error 而不是普通输出：ToolDef.Run 的 err 会成为工具结果里的
		// 错误标记，模型能明确知道"这次没发出去"。
		return ToolReturn{}, fmt.Errorf("有 %d 个会话匹配 %q，请用完整 id 重新发送：\n%s",
			len(candidates), to, strings.Join(lines, "\n"))
	}

	id, err := host.MailboxDeliver(target.ID, subject, body, refs)
	if err != nil {
		return ToolReturn{}, err
	}
	return ToolReturn{
		Output: fmt.Sprintf("已发送给「%s」（%s），邮件 id=%s。",
			target.Title, target.ID, id),
	}, nil
}

// resolveRecipient 把"id 或标题关键词"解析成一个目标会话。
//
// 返回 (target, candidates, err)：
//   - target 非 nil：唯一命中，可以发送
//   - target 为 nil 且 candidates 非空：命中多个，调用方应让模型重选
//   - 都为空：没命中（err 里给出可发清单）
func resolveRecipient(peers []MailboxPeer, to string) (*MailboxPeer, []MailboxPeer, error) {
	query := strings.TrimSpace(to)
	if query == "" {
		return nil, nil, fmt.Errorf("收件人不能为空")
	}
	// 1) 精确 ID 优先：模型从候选里挑过之后就是走这条。
	//
	// 忽略大小写：会话 ID 是小写十六进制，但用户复制/模型转述时可能变成
	// 大写 —— 那不该成为"找不到这个会话"。
	lower := strings.ToLower(query)
	for index := range peers {
		if strings.ToLower(peers[index].ID) == lower {
			return &peers[index], nil, nil
		}
	}
	// 2) 标题包含关键词
	var hits []MailboxPeer
	for _, peer := range peers {
		if strings.Contains(strings.ToLower(peer.Title), lower) {
			hits = append(hits, peer)
		}
	}
	if len(hits) == 1 {
		return &hits[0], nil, nil
	}
	if len(hits) > 1 {
		return nil, hits, nil
	}
	// 3) 没命中：把可发清单给模型，让它自己修正
	if len(peers) == 0 {
		return nil, nil, fmt.Errorf("本项目没有其它会话可以收信")
	}
	var lines []string
	for _, peer := range peers {
		lines = append(lines, fmt.Sprintf("- %s  %s", peer.ID, peer.Title))
	}
	return nil, nil, fmt.Errorf("找不到会话 %q。本项目可收信的会话：\n%s",
		query, strings.Join(lines, "\n"))
}

func mailboxListRun(_ context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	host := tc.Mailbox
	if host == nil {
		return ToolReturn{}, fmt.Errorf("会话通讯不可用")
	}
	unreadOnly := args.Bool("unreadOnly")
	limit := 20
	if value, ok := args.Int("limit"); ok && value > 0 {
		limit = value
	}
	mails := host.MailboxInbox(unreadOnly, limit)
	if len(mails) == 0 {
		if unreadOnly {
			return ToolReturn{Output: "没有未读邮件。"}, nil
		}
		return ToolReturn{Output: "邮箱是空的。"}, nil
	}

	var lines []string
	for _, mail := range mails {
		flag := " "
		if !mail.Read {
			flag = "●"
		}
		from := mail.FromTitle
		if from == "" {
			from = mail.From
		}
		lines = append(lines,
			fmt.Sprintf("%s [%s] %s — %s（%s）",
				flag, mail.ID, mail.Subject, from, mail.At.Format("01-02 15:04")))
	}
	return ToolReturn{
		Output: fmt.Sprintf("%d 封邮件（● 未读）：\n%s\n\n读正文请用 mailbox_read。",
			len(mails), strings.Join(lines, "\n")),
	}, nil
}

func mailboxReadRun(_ context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	host := tc.Mailbox
	if host == nil {
		return ToolReturn{}, fmt.Errorf("会话通讯不可用")
	}
	id, err := args.Str("id")
	if err != nil {
		return ToolReturn{}, err
	}
	mail, err := host.MailboxReadMail(strings.TrimSpace(id))
	if err != nil {
		return ToolReturn{}, err
	}
	from := mail.FromTitle
	if from == "" {
		from = mail.From
	}
	output := fmt.Sprintf("主题：%s\n发件人：%s（%s）\n时间：%s\n",
		mail.Subject, from, mail.From, mail.At.Format("2006-01-02 15:04"))
	if len(mail.Refs) > 0 {
		output += fmt.Sprintf("引用：%s\n", strings.Join(mail.Refs, ", "))
	}
	output += "\n" + mail.Body
	return ToolReturn{Output: output}, nil
}
