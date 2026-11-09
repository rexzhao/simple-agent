package bridge

import (
	"strings"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/markdown"
	"simple-agent/internal/message"
	"simple-agent/internal/render"
	"simple-agent/internal/timeline"
)

// 本文件把「执行端的会话状态」转成 Android 能直接画的视图模型。
//
// 关键点：**语义全部在这里算**（分组、标题、工具卡片状态、markdown 结构），
// Kotlin 只负责把结构画出来。这样 Android 与桌面不会出现"同一段对话两种样子"。

// buildTimeline 生成一个会话的完整可渲染内容。
func (c *Client) buildTimeline(sessionID string) TimelineView {
	session := c.currentSession(sessionID)
	if session == nil {
		return TimelineView{SessionID: sessionID}
	}
	conv := session.Conversation()
	idle := session.Busy() == driver.BusyIdle

	view := TimelineView{
		SessionID: session.ID,
		Title:     session.Title,
		Runtime:   c.runtimeView(session),
	}
	for _, todo := range session.Todos() {
		view.Todos = append(view.Todos, TodoView{Content: todo.Content, Status: todo.Status})
	}
	view.Asks = c.asksOf(session)

	for _, item := range timeline.Build(conv.Entries(), idle) {
		view.Items = append(view.Items, c.itemView(item, conv, idle))
	}
	return view
}

// asksOf 把一个会话的待回答提问转成视图（含选中态）。
//
// 选中态存在 Go 的 AskGroup 里：单选互斥、写文字就取消选项这些规则在 driver 里，
// 桌面与 Android 共用。客户端不重新实现，只按这里给的结果画勾。
func (c *Client) asksOf(session *host.Session) []AskView {
	var out []AskView
	for _, group := range session.AskGroups() {
		ask := AskView{MessageID: group.MessageID, CallID: group.CallID}
		allAnswered := len(group.Questions) > 0
		for index, question := range group.Questions {
			item := QuestionView{
				ID: question.ID, Question: question.Question,
				Header: question.Header, MultiSelect: question.MultiSelect,
				Custom:       group.Custom(index),
				CustomActive: group.CustomActive(index),
				Answered:     group.Answered(index),
				Selected:     selectedOf(group, index),
			}
			for _, option := range question.Options {
				item.Options = append(item.Options, option.Label)
			}
			if !item.Answered {
				allAnswered = false
			}
			ask.Questions = append(ask.Questions, item)
		}
		ask.Answered = allAnswered
		out = append(out, ask)
	}
	return out
}

// buildAsks 返回一个会话的提问视图（供 ToggleAskOption 等直接回包）。
func (c *Client) buildAsks(sessionID string) []AskView {
	session := c.currentSession(sessionID)
	if session == nil {
		return nil
	}
	return c.asksOf(session)
}

// findAskGroup 按 callID 找本地提问组（找不到返回 nil）。
func findAskGroup(c *Client, sessionID, callID string) *driver.AskGroup {
	session := c.currentSession(sessionID)
	if session == nil {
		return nil
	}
	for _, group := range session.AskGroups() {
		if group.CallID == callID {
			return group
		}
	}
	return nil
}

// selectedOf 返回某题已选中的选项下标（按选项顺序，便于稳定显示与断言）。
func selectedOf(group *driver.AskGroup, questionIndex int) []int {
	if questionIndex < 0 || questionIndex >= len(group.Questions) {
		return nil
	}
	var picked []int
	for optionIndex := range group.Questions[questionIndex].Options {
		if group.Selected(questionIndex, optionIndex) {
			picked = append(picked, optionIndex)
		}
	}
	return picked
}

// itemView 把一条时间线条目转成视图。
func (c *Client) itemView(item timeline.Item, conv *conversation.Conversation, idle bool) ItemView {
	view := ItemView{Key: item.Key}
	switch item.Kind {
	case timeline.KindNotice:
		view.Kind = "notice"
		if item.Notice != nil {
			view.Notice = &NoticeView{
				ID: item.Notice.ID, Level: string(item.Notice.Level), Text: item.Notice.Text,
			}
		}
	case timeline.KindProcess:
		view.Kind = "process"
		view.Process = c.processView(item, !idle)
	case timeline.KindTurnFooter:
		view.Kind = "footer"
		view.Footer = footerOf(item)
	case timeline.KindContent:
		view.Kind = "content"
		view.Message = c.messageView(item, conv, idle)
	}
	return view
}

// footerOf 生成回合结束行。
func footerOf(item timeline.Item) *FooterView {
	chat := timeline.TailMessage(item)
	if chat == nil {
		return nil
	}
	footer := &FooterView{
		Status:     string(chat.Status()),
		Reason:     chat.FailureReason(),
		DurationMS: timeline.Duration(item).Milliseconds(),
		Tokens:     timeline.Tokens(item),
	}
	if footer.DurationMS < 1000 {
		// 不足 1 秒不显示耗时（与桌面一致：别显示"用时 0 秒"）
		footer.DurationMS = 0
	}
	return footer
}

// messageView 生成一条消息（正文 part + 图片等）。
//
// 过程类 part（思考/工具）不进这里——它们已经在过程组里了。
func (c *Client) messageView(item timeline.Item, conv *conversation.Conversation, idle bool) *MessageView {
	if len(item.Refs) == 0 {
		return nil
	}
	chat := item.Refs[0].Chat
	view := &MessageView{
		ID:     chat.Message.MessageID,
		Role:   roleOf(chat.Message.Role),
		Status: string(chat.Status()),
		Reason: chat.FailureReason(),
		At:     unixMillis(chat.CreatedAt),
	}
	for _, ref := range item.Refs {
		part := ref.Part()
		// 用户消息里的图片附件也要显示（它们不是过程）
		if partView, ok := c.partView(part); ok {
			view.Parts = append(view.Parts, partView)
		}
	}
	return view
}

// partView 把一个 part 转成视图（只处理"正文类"内容）。
func (c *Client) partView(part message.Part) (PartView, bool) {
	classified := render.Classify(part)
	switch classified.Kind {
	case render.KindMarkdown:
		text := strings.TrimSpace(classified.Text)
		if text == "" {
			return PartView{}, false
		}
		// markdown 的**块结构**由 Go 解析好交给 Kotlin：Kotlin 只负责排版，
		// 不必自己实现一套 markdown（两套实现必然会不一致）。
		return PartView{Kind: "markdown", Text: classified.Text, Blocks: c.blocks(classified.Text)}, true
	case render.KindPlainText:
		if strings.TrimSpace(classified.Text) == "" {
			return PartView{}, false
		}
		return PartView{Kind: "text", Text: classified.Text}, true
	case render.KindCode:
		return PartView{Kind: "code", Text: classified.Text, Lang: classified.Language}, true
	case render.KindSummary:
		return PartView{Kind: "summary", Text: classified.Text}, true
	case render.KindImage:
		if classified.Image == nil {
			return PartView{}, false
		}
		image := &ImageView{
			Blob:      classified.Image.Blob,
			URL:       classified.Image.URL,
			MediaType: classified.Image.MediaType,
		}
		return PartView{Kind: "image", Image: image, Name: classified.Name}, true
	case render.KindFile:
		return PartView{Kind: "file", Name: classified.Name}, true
	case render.KindJSON:
		return PartView{}, false // 正文里的裸 JSON 不显示（工具参数走过程组）
	}
	return PartView{}, false
}

// processView 生成一个过程组（折叠面板）。
func (c *Client) processView(item timeline.Item, streaming bool) *ProcessView {
	view := &ProcessView{
		Key:       item.Key,
		Title:     timeline.GroupTitle(item.Refs, streaming, c.lang),
		Activity:  activityName(timeline.GroupActivity(item.Refs, streaming)),
		Streaming: streaming,
	}
	// 工具结果按 id 索引，便于把 use/result 合成一张卡片
	results := map[string]timeline.Ref{}
	for _, ref := range item.Refs {
		part := ref.Part()
		if part.MediaType != render.MediaToolResult {
			continue
		}
		data, ok := partData(part)
		if !ok {
			continue
		}
		results[stringField(data, "id")] = ref
	}
	seen := map[string]bool{}
	for _, ref := range item.Refs {
		part := ref.Part()
		switch part.MediaType {
		case render.MediaReasoning:
			text, _ := part.AsText()
			if strings.TrimSpace(text) == "" {
				continue
			}
			view.Steps = append(view.Steps, ProcessStep{Kind: "reasoning", Text: text, Done: true})
		case render.MediaToolUse:
			data, ok := partData(part)
			if !ok {
				continue
			}
			id := stringField(data, "id")
			step := ProcessStep{
				Kind: "tool", ToolID: id, ToolName: stringField(data, "name"),
				ToolTitle: toolTitle(stringField(data, "name")),
				Args:      toolArgsText(data),
			}
			if result, ok := results[id]; ok {
				seen[id] = true
				applyToolResult(&step, result.Part())
			} else {
				step.Pending = !streaming
			}
			view.Steps = append(view.Steps, step)
		case render.MediaToolResult:
			data, ok := partData(part)
			if !ok {
				continue
			}
			id := stringField(data, "id")
			if seen[id] {
				continue
			}
			// 没有对应 tool_use 的结果（例如重放修复补的）：单独成一项
			seen[id] = true
			step := ProcessStep{
				Kind: "tool", ToolID: id, ToolName: stringField(data, "name"),
				ToolTitle: toolTitle(stringField(data, "name")),
			}
			applyToolResult(&step, part)
			view.Steps = append(view.Steps, step)
		}
	}
	return view
}

// applyToolResult 把结果写进工具卡片。
func applyToolResult(step *ProcessStep, part message.Part) {
	data, ok := partData(part)
	if !ok {
		return
	}
	step.Output, _ = data["output"].(string)
	if isError, ok := data["is_error"].(bool); ok {
		step.IsError = isError
	}
	if pending, ok := data["pending"].(bool); ok && pending {
		step.Pending = true
		return
	}
	step.Done = true
}

// toolArgsText 把工具参数转成可读文本（对象则逐行 key: value）。
func toolArgsText(data map[string]any) string {
	if args, ok := data["args"].(map[string]any); ok && len(args) > 0 {
		return joinArgs(args)
	}
	if raw := stringField(data, "raw_args"); raw != "" {
		return raw
	}
	return ""
}

// joinArgs 把参数对象拼成 "key: value" 的多行文本（键顺序稳定，便于阅读）。
func joinArgs(args map[string]any) string {
	var builder strings.Builder
	for _, key := range sortedKeys(args) {
		value := args[key]
		text, ok := value.(string)
		if !ok {
			encoded, err := encodeJSON(value)
			if err != nil {
				continue
			}
			text = string(encoded)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(key)
		builder.WriteString(": ")
		builder.WriteString(text)
	}
	return builder.String()
}

// toolTitle 返回工具的显示名（Android 侧做卡片标题）。
func toolTitle(name string) string {
	if name == "" {
		return "tool"
	}
	return name
}

// runtimeView 生成状态条与输入卡要用的信息。
func (c *Client) runtimeView(session *host.Session) RuntimeView {
	selection := session.Selection()
	effort := session.Effort()
	return RuntimeView{
		Busy:          string(session.Busy()),
		Queued:        session.Queued(),
		InputTokens:   session.Usage().InputTokens,
		OutputTokens:  session.Usage().OutputTokens,
		TokensInUse:   session.TokensInUse(),
		ContextWindow: session.EffectiveContextWindow(),
		Turns:         session.Counts().Turns,
		Steps:         session.Counts().Steps,
		ProviderID:    selection.ProviderID,
		Model:         selection.Model,
		ModelLabel:    c.modelLabel(selection.ProviderID, selection.Model),
		Effort:        effort,
		EffortLabel:   c.effortLabel(effort),
		Vision:        session.Vision(),
		Compacting:    session.Compacting(),
		CanSend:       session.CanSend(),
		CanSteer:      session.CanSteer(),
		AllowShell:    c.allowShell,
		Connected:     c.connected(),
	}
}

// modelLabel 查执行端公布的模型显示名。
func (c *Client) modelLabel(providerID, model string) string {
	for _, choice := range c.providers() {
		if choice.ProviderID != providerID {
			continue
		}
		if label, ok := choice.Labels[model]; ok && label != "" {
			return label
		}
	}
	return model
}

// effortLabel 返回思考档位的显示名。
func (c *Client) effortLabel(effort string) string {
	switch effort {
	case "":
		return ""
	case "off":
		return c.tr("effort.off")
	case "low":
		return c.tr("effort.low")
	case "medium":
		return c.tr("effort.medium")
	case "high":
		return c.tr("effort.high")
	case "max":
		return c.tr("effort.max")
	}
	return effort
}

// activityName 把活动分类转成稳定的字符串（Kotlin 侧据此选图标）。
func activityName(activity timeline.Activity) string {
	switch activity {
	case timeline.ActivityThinking:
		return "thinking"
	case timeline.ActivityRead:
		return "read"
	case timeline.ActivityReadImage:
		return "read_image"
	case timeline.ActivitySearch:
		return "search"
	case timeline.ActivityWrite:
		return "write"
	case timeline.ActivityEdit:
		return "edit"
	case timeline.ActivityCommands:
		return "commands"
	case timeline.ActivityCode:
		return "code"
	case timeline.ActivityWebSearch:
		return "web_search"
	case timeline.ActivityWebFetch:
		return "web_fetch"
	case timeline.ActivitySubagents:
		return "subagents"
	case timeline.ActivityPlan:
		return "plan"
	case timeline.ActivityQuestions:
		return "questions"
	}
	return "tools"
}

// parseMarkdown 用共享解析器解析（带缓存：长会话每帧都解析会很慢）。
func (c *Client) parseMarkdown(text string) []markdown.Block {
	c.mu.Lock()
	parser := c.parser
	c.mu.Unlock()
	if parser == nil {
		return nil
	}
	return parser.Parse(text)
}
