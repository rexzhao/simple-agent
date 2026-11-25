package bridge

import (
	"encoding/json"
	"strconv"
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
	view, _ := c.buildTimelineWithRaw(sessionID)
	return view
}

// buildTimelineWithRaw 同时返回渲染视图与**原始条目**。
//
// 分页需要原始条目上的 seq 区间（渲染后的 ItemView 不含 seq），
// 所以两者一起返回，避免调用方重复 Build 一次。
func (c *Client) buildTimelineWithRaw(sessionID string) (TimelineView, []timeline.Item) {
	session := c.currentSession(sessionID)
	if session == nil {
		return TimelineView{SessionID: sessionID}, nil
	}
	conv := session.Conversation()
	busy := session.Busy()
	idle := busy == driver.BusyIdle
	// 用"补拉历史 + 镜像"拼接后的 entries：镜像里只有最近 attachTailEvents 条，
	// 翻页补拉回来的更早内容不在 conv 里，直接读 conv 会看不到它们。
	entries := session.EntriesWithHistory()

	// **缓存**：流式输出时每 60ms 就会重建一次，但很多次 entries 根本没变
	// （同一条消息内部的 part 在长，条数不变）。
	//
	// 指纹必须覆盖**所有会影响输出的状态**：entries 结构、idle、
	// 以及 asks/todos（它们变了但 message 没变，遗漏会导致界面不更新）。
	fingerprint := timelineFingerprint(entries, idle, session)
	c.mu.Lock()
	cached := c.timelineCache[sessionID]
	c.mu.Unlock()
	if cached != nil && cached.fingerprint == fingerprint {
		return cached.view, cached.raw
	}

	view := TimelineView{
		SessionID: session.ID,
		Title:     session.Title,
		Runtime:   c.runtimeView(session),
	}
	for _, todo := range session.Todos() {
		view.Todos = append(view.Todos, TodoView{Content: todo.Content, Status: todo.Status})
	}
	view.Asks = c.asksOf(session)

	// running 单独传：**工具执行期间会话不空闲、但消息已不流式**，
	// 只看 idle 会让「运行中」那一行消失（与桌面端同一个 bug）。
	//
	// 用量不再传：timeline 从**每一轮自己的消息**汇总（以前传的是
	// 会话累计值，导致每个回合显示同一个数字）。
	raw := timeline.Build(entries, idle, busy != driver.BusyIdle)
	for _, item := range raw {
		view.Items = append(view.Items, c.itemView(item, conv, idle, busy))
	}

	// 缓存游标：手机端据此判断本地 SQLite 缓存还能不能用、从哪里续。
	view.Sync = c.syncCursorOf(session, raw...)

	c.mu.Lock()
	if c.timelineCache == nil {
		c.timelineCache = map[string]*timelineCacheEntry{}
	}
	c.timelineCache[sessionID] = &timelineCacheEntry{
		fingerprint: fingerprint, view: view, raw: raw,
	}
	c.mu.Unlock()
	return view, raw
}

// syncCursorOf 生成某个会话的同步游标。
//
// 三样东西各有用途（缺一个就会错）：
//   - Epoch：执行端重启后 seq 从 0 重新计数，旧缓存必须整体丢弃
//   - Generation：压缩会把历史折叠，缓存的行数对不上
//   - LastSeq：这份内容覆盖到的最大事件序号 —— 下次只要它之后的部分
//
// raw 为空时 LastSeq 退化成"当前已知的最大序号"：单独问游标（不构建时间线）
// 的路径没有条目可反推，那只关心 epoch/generation。
func (c *Client) syncCursorOf(session *host.Session, raw ...timeline.Item) SyncCursor {
	var lastSeq uint64
	for _, item := range raw {
		if _, last, ok := itemSeqRange(item); ok && last > lastSeq {
			lastSeq = last
		}
	}
	return SyncCursor{
		Epoch:      session.SyncEpoch(),
		Generation: session.Generation(),
		LastSeq:    lastSeq,
	}
}

// timelineCacheEntry 是一次构建结果的缓存。
type timelineCacheEntry struct {
	fingerprint string
	view        TimelineView
	raw         []timeline.Item
}

// timelineFingerprint 生成一个能反映"时间线输出是否变化"的廉价指纹。
//
// 必须覆盖所有影响输出的状态，否则界面会停在旧数据上：
//   - 条目数变化 → 结构变了
//   - 最后两条的 part 数变化 → 那几条的渲染会变（流式增长、新增内容）
//   - idle 变化 → 回合结束行可能出现/消失
//   - asks / todos 变化 → 它们不体现在 message 里，但会改变输出
func timelineFingerprint(entries []conversation.Entry, idle bool, session *host.Session) string {
	var builder strings.Builder
	builder.WriteString(strconv.Itoa(len(entries)))
	builder.WriteByte('|')
	if idle {
		builder.WriteByte('1')
	} else {
		builder.WriteByte('0')
	}
	// 最后两条的 part 数：足够发现"最后一条在流式增长"或"刚追加了新条目"
	for index := len(entries) - 1; index >= 0 && index >= len(entries)-2; index-- {
		entry := entries[index]
		if entry.Message == nil {
			if entry.Notice != nil {
				builder.WriteString("|n")
			}
			continue
		}
		builder.WriteByte('|')
		builder.WriteString(strconv.Itoa(len(entry.Message.Message.Parts)))
	}
	// asks：待回答提问的**选中态**变化不会改 message，但界面必须更新
	builder.WriteString("|a")
	builder.WriteString(strconv.Itoa(len(session.AskGroups())))
	for _, group := range session.AskGroups() {
		builder.WriteByte(':')
		builder.WriteString(group.CallID)
		for index, question := range group.Questions {
			// 每个问题：已答 + 自写内容 + 每个选项的选中态
			if group.Answered(index) {
				builder.WriteByte('1')
			} else {
				builder.WriteByte('0')
			}
			if group.CustomActive(index) {
				builder.WriteByte('c')
			}
			builder.WriteString(group.Custom(index))
			for optionIndex := range question.Options {
				if group.Selected(index, optionIndex) {
					builder.WriteByte('s')
				} else {
					builder.WriteByte('_')
				}
			}
		}
	}
	// todos：任务清单变化同样不改 message
	builder.WriteString("|t")
	builder.WriteString(strconv.Itoa(len(session.Todos())))
	for _, todo := range session.Todos() {
		builder.WriteByte(':')
		builder.WriteString(todo.Status)
	}
	return builder.String()
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
//
// idle / busy 描述的是**整个会话**此刻的状态，用于判断"某个还没结果的工具
// 调用是不是真的还在跑"（见 processSteps 的 pending 判定）。
func (c *Client) itemView(item timeline.Item, conv *conversation.Conversation, idle bool, busy driver.Busy) ItemView {
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
		// 只有这一组自己还在进行时才带完整详情并显示转圈。
		//
		// 以前这里用 `streaming := !idle`（整个会话是否空闲），结果会话一跑
		// 起来，几百轮之前的历史工具组全都跟着转圈。是否"活着"是每一组
		// 自己的属性，由 timeline.Build 算好放在 item.Live 里。
		live := item.Live
		view.Process = c.processView(item, live, live, idle, busy)
	case timeline.KindRoundFooter:
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
//
// idle 目前没有用到（正文是否需要特殊处理只取决于消息自身状态）。
// 保留参数是为了不改调用签名，等下次真的需要时再用；不要拿它去判断
// "是不是最后一条"——那是每个 item 自己的属性（见 timeline.Item.Live）。
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
// processView 生成过程组视图。
//
// withDetail 为假时**不填 Steps**，只给 StepCount —— 这是懒加载的关键：
// 过程组的 Steps 里装着思考全文 + 工具参数/输出全文，一屏十几个组就是上 MB
// （实测长会话 1690KB JSON 里绝大部分在这），而它们默认是折叠的。
// 用户点开时再由 ProcessDetailJSON 单独取这一个组。
//
// streaming 是**这一组自己的**进行状态（item.Live）；idle/busy 是会话的，
// 用来判断某个还没结果的工具调用是不是真的还在跑。
func (c *Client) processView(item timeline.Item, streaming, withDetail, idle bool, busy driver.Busy) *ProcessView {
	steps := c.processSteps(item, streaming, idle, busy)

	// 组标题上的转圈：只要这一组里有"还在等结果"的调用就该转，
	// 不只是"消息还在流式"。
	//
	// 工具执行期间消息已 complete（item.Live=false），但工具确实在跑；
	// 这时标题行如果不转，用户会以为整个回合卡死了。
	active := streaming
	if !active {
		for _, step := range steps {
			if step.Pending {
				active = true
				break
			}
		}
	}

	view := &ProcessView{
		Key:       item.Key,
		Title:     timeline.GroupTitle(item.Refs, streaming, c.lang),
		Activity:  activityName(timeline.GroupActivity(item.Refs, streaming)),
		Streaming: active,
	}
	view.StepCount = len(steps)
	// 带详情（Steps）的条件跟 Streaming 一致，不用传进来的 streaming：
	// 正在等工具结果的组也必须带 Steps，否则 Kotlin 侧看到
	// `process.streaming == true` 就跳过拉详情，展开后一片空白。
	if withDetail || active {
		view.Steps = steps
		view.Detail = true
	}
	return view
}

// processSteps 把过程组的 refs 展开成步骤列表（含思考与工具卡片）。
//
// streaming 表示这一组还在流式（item.Live）；idle/busy 是会话状态。
// 二者要分开：工具执行期间模型已经把 message 标成 complete，
// 所以 item.Live 会是 false，但工具**确实还在跑**——这时仍要显示"等待结果"。
func (c *Client) processSteps(item timeline.Item, streaming, idle bool, busy driver.Busy) []ProcessStep {
	var steps []ProcessStep
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
			steps = append(steps, ProcessStep{Kind: "reasoning", Text: text, Done: true})
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
			} else if streaming || busy == driver.BusyRunningTools {
				// 这个调用还没有结果，而且**确实可能还在跑**。
				//
				// 两种"还在跑"的情形：
				//   streaming —— 这一组还在流式，结果随时可能来；
				//   BusyRunningTools —— 模型已经输出完（message 是 complete，
				//     所以 item.Live 为 false），但工具正在执行。
				//
				// 第二种最容易漏：工具执行期间消息状态已经是 complete，
				// 只看 streaming 会让"等待结果"不显示，用户以为卡住了。
				//
				// 会话完全空闲（idle）时还没结果的，属于数据异常
				// （例如崩溃恢复没补上 tool.result），不该谎称还在等。
				step.Pending = true
			}
			steps = append(steps, step)
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
			steps = append(steps, step)
		}
	}
	return steps
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
	// 结构化摘要（与桌面端同一套）：界面拿它们显示人话，
	// 而不是把 Output 里给模型的文本（XML 信封/英文指令/续读提示）摊出来。
	step.Exec = decodeStepExtra[ExecInfoView](data, "exec")
	step.Diff = decodeStepDiff(data)
	step.Image = decodeStepExtra[ImageInfoView](data, "image_info")
	step.Search = decodeStepExtra[SearchView](data, "search")
	step.Web = decodeStepExtra[WebView](data, "web")
	step.Read = decodeStepExtra[ReadView](data, "read")
	step.Todo = decodeStepExtra[TodoProgressView](data, "todo")
	if args, ok := data["args"].(map[string]any); ok {
		if desc, ok := args["description"].(string); ok {
			step.Description = desc
		}
	}
	if pending, ok := data["pending"].(bool); ok && pending {
		step.Pending = true
		return
	}
	step.Done = true
}

// decodeStepExtra 把 part 里的结构化字段转成桥接层的视图类型。
//
// 内存里是 Go 的指针（*tools.WebInfo 等），重放后是 map[string]any。
// 两种都要认 —— 只认一种的话，重启后（或刚做完时）有一边会缺。
// 用 JSON 中转而不是直接类型断言：桥接层不 import internal/tools 的细节，
// 只依赖字段名（两边都带 json tag）。
func decodeStepExtra[T any](data map[string]any, key string) *T {
	value, ok := data[key]
	if !ok || value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return &out
}

// decodeStepDiff 特殊一点：EditInfo 在桌面端叫 FileDiff，桥接层叫 FileDiffView。
func decodeStepDiff(data map[string]any) *FileDiffView {
	return decodeStepExtra[FileDiffView](data, "diff")
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
