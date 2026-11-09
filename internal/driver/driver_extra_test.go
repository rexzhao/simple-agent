package driver_test

import (
	"fmt"
	"strings"
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/render"
	"simple-agent/internal/tools"
)

// effectOf 泛型取出指定类型的 effect。
func effectOf[T any](effects []driver.Effect) (T, bool) {
	var zero T
	for _, e := range effects {
		if v, ok := e.(T); ok {
			return v, true
		}
	}
	return zero, false
}

// P1：推理内容在流式过程中就要可见（不能等 Done 才落盘，否则思考阶段界面全白）。
func TestReasoningVisibleWhileStreaming(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("想想")
	start := startStream(t, effects)

	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventReasoning, Text: "第一步"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventReasoning, Text: "第二步"})

	// 还没收到 Done：界面上就应该能看到推理
	entry, _ := conv.Message(start.MessageID)
	var reasoningText string
	reasoningParts := 0
	for _, part := range entry.Message.Parts {
		if part.MediaType != render.MediaReasoning {
			continue
		}
		reasoningParts++
		if text, ok := part.AsText(); ok {
			reasoningText = text
		}
	}
	if reasoningParts != 1 {
		t.Fatalf("应有 1 个推理 part，实际 %d", reasoningParts)
	}
	if reasoningText != "第一步第二步" {
		t.Fatalf("推理应累加：%q", reasoningText)
	}

	// 正文随后到达时不能串进推理 part
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: "答案"})
	entry, _ = conv.Message(start.MessageID)
	for _, part := range entry.Message.Parts {
		text, _ := part.AsText()
		if part.MediaType == render.MediaReasoning && strings.Contains(text, "答案") {
			t.Fatalf("正文串进了推理 part：%q", text)
		}
	}
}

// P1：流式中停止，已显示的推理内容仍要保留。
func TestReasoningKeptAfterStop(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("想想")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventReasoning, Text: "思考中"})

	d.Stop()

	entry, _ := conv.Message(start.MessageID)
	var reasoningText string
	for _, part := range entry.Message.Parts {
		if part.MediaType == render.MediaReasoning {
			reasoningText, _ = part.AsText()
		}
	}
	if reasoningText != "思考中" {
		t.Fatalf("停止后推理内容应保留：%q", reasoningText)
	}
}

// P2-1：执行器返回的结果少于调用数时补配对结果，否则下一轮请求会被拒收。
func TestMissingToolResultsAreSettled(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("两个调用")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 1, ID: "c2", Name: "read"})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	run := runTools(t, effects)
	if len(run.Calls) != 2 {
		t.Fatalf("应有 2 个调用，实际 %d", len(run.Calls))
	}

	if _, err := d.ToolResults(start.MessageID, []tools.ToolOutcome{
		{ID: "c1", Name: "read", Output: "ok"},
	}); err != nil {
		t.Fatal(err)
	}

	entry, _ := conv.Message(start.MessageID)
	results := map[string]bool{}
	isError := map[string]bool{}
	for _, part := range entry.Message.Parts {
		if part.MediaType != render.MediaToolResult {
			continue
		}
		data, _ := part.Content.(message.DataPart).Data.(map[string]any)
		id, _ := data["id"].(string)
		results[id] = true
		isError[id] = data["is_error"] == true
	}
	if !results["c1"] || !results["c2"] {
		t.Fatalf("两个调用都应有结果：%#v", results)
	}
	if isError["c1"] {
		t.Fatal("已返回的结果不应被标成错误")
	}
	if !isError["c2"] {
		t.Fatal("缺失的结果应标成错误")
	}

	// 配对完整 → 下一轮请求能正常映射
	if _, err := providers.ToOpenAIMessages(conv.History(), nil); err != nil {
		t.Fatalf("历史应可映射：%v", err)
	}
}

// P2-4：工具轮数达到上限时给通知并回到空闲。
func TestMaxToolRoundsStopsRunaway(t *testing.T) {
	conv := conversation.New("ctx-limit")
	d := driver.New(conv, driver.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:         tools.Builtin(false),
		MaxToolRounds: 2,
	})

	effects, _ := d.Send("开始")
	start := startStream(t, effects)

	for round := 1; round <= 5; round++ {
		d.ApplyEvent(start.MessageID, providers.Event{
			Kind: providers.EventToolStart, Index: 0, ID: fmt.Sprintf("c%d", round), Name: "read",
		})
		effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
		run, isRun := effectOf[driver.RunTools](effects)
		if !isRun {
			break // 达到上限：不再有 RunTools
		}
		effects, err := d.ToolResults(start.MessageID, []tools.ToolOutcome{
			{ID: run.Calls[0].ID, Name: "read", Output: "ok"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if next, ok := effectOf[driver.StartStream](effects); ok {
			start = next
		}
		if round == 5 {
			t.Fatal("轮数上限没有生效")
		}
	}

	if d.Busy() != driver.BusyIdle {
		t.Fatalf("达到上限后应回到 idle，实际 %v", d.Busy())
	}
	sawNotice := false
	for _, entry := range conv.Entries() {
		if entry.Notice != nil && strings.Contains(entry.Notice.Text, "轮数上限") {
			sawNotice = true
		}
	}
	if !sawNotice {
		t.Fatal("应给出「达到轮数上限」的通知")
	}
	// 之后仍可继续发送
	if _, err := d.Send("继续"); err != nil {
		t.Fatalf("达到上限后应能继续发送：%v", err)
	}
}

// P2-2：stop_reason=length 时提示用户回答被截断。
func TestTruncatedStopAddsNotice(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("写很长的东西")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: "写了一半"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "length"})

	sawNotice := false
	for _, entry := range conv.Entries() {
		if entry.Notice != nil && strings.Contains(entry.Notice.Text, "截断") {
			sawNotice = true
		}
	}
	if !sawNotice {
		t.Fatal("截断应给出通知")
	}
}

// P2-3：等待回答时停止 → pending 占位被覆盖成错误结果，之后仍能发送。
func TestStopWhileAwaitingAnswer(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("问")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "ask_1", Name: "ask_user_question"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	if _, err := d.ToolResults(start.MessageID, []tools.ToolOutcome{{
		ID: "ask_1", Name: "ask_user_question",
		Pending: []tools.QuestionSpec{{ID: "q1", Question: "?"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if d.Busy() != driver.BusyAwaitingAnswer {
		t.Fatalf("应等待回答，实际 %v", d.Busy())
	}

	d.Stop()
	if d.Busy() != driver.BusyIdle {
		t.Fatalf("停止后应空闲，实际 %v", d.Busy())
	}

	entry, _ := conv.Message(start.MessageID)
	settled := false
	for _, part := range entry.Message.Parts {
		if part.MediaType != render.MediaToolResult {
			continue
		}
		data, _ := part.Content.(message.DataPart).Data.(map[string]any)
		if data["id"] == "ask_1" && data["is_error"] == true {
			settled = true
		}
	}
	if !settled {
		t.Fatal("pending 占位应被覆盖成错误结果")
	}
	if _, err := d.Send("继续"); err != nil {
		t.Fatalf("停止后应能继续发送：%v", err)
	}
}

// 同一轮多个提问：PendingAsks 要能一并列出并带回 CallID。
func TestMultiplePendingAsks(t *testing.T) {
	d, _ := newDriver(t, false)
	effects, _ := d.Send("两个问题")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "ask_a", Name: "ask_user_question"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 1, ID: "ask_b", Name: "ask_user_question"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	if _, err := d.ToolResults(start.MessageID, []tools.ToolOutcome{
		{ID: "ask_a", Name: "ask_user_question", Pending: []tools.QuestionSpec{{ID: "q1", Question: "A?"}}},
		{ID: "ask_b", Name: "ask_user_question", Pending: []tools.QuestionSpec{{ID: "q2", Question: "B?"}}},
	}); err != nil {
		t.Fatal(err)
	}

	groups := d.AskGroups()
	if len(groups) != 2 || groups[0].CallID != "ask_a" || groups[1].CallID != "ask_b" {
		t.Fatalf("应列出两个待回答提问组：%#v", groups)
	}

	// 一次提交会把两组答案一起回填（Rust 版 AskGroup 的语义）
	groups[0].Toggle(0, 0)
	effects, err := d.SubmitAsks("自由文本")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := effectOf[driver.StartStream](effects); !ok {
		t.Fatal("提交后应开新流")
	}
	if d.AskGroups() != nil {
		t.Fatal("提交后不应还有待回答")
	}
}

// 流式参数只落前缀：write 写大文件时不能让每个增量都复制整段参数。
func TestLiveToolArgsAreClipped(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("写文件")
	start := startStream(t, effects)

	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "write"})
	// 一个 8KB 的参数分两次到达
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0,
		Delta: `{"file_path":"big.txt","content":"`})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0,
		Delta: strings.Repeat("x", 8192) + `"}`})

	entry, ok := conv.Message(start.MessageID)
	if !ok {
		t.Fatal("找不到消息")
	}
	part := entry.Message.Parts[0]
	data, ok := part.Content.(message.DataPart)
	if !ok {
		t.Fatalf("应为 DataPart：%#v", part)
	}
	fields, ok := data.Data.(map[string]any)
	if !ok {
		t.Fatal("data 应是 map")
	}
	// 界面摘要要用的 file_path 必须还在（在 raw_args 前缀里），
	// 整段内容不能全塞进 part
	raw, _ := fields["raw_args"].(string)
	if !strings.HasPrefix(raw, `{"file_path":"big.txt"`) {
		t.Fatalf("流式参数前缀里应含 file_path：%.80q", raw)
	}
	if len(raw) > 2048 {
		t.Fatalf("流式参数应被截断到 2KB 内，实际 %d 字节", len(raw))
	}

	// 完整参数仍然来自缓冲区：执行时拿到的是全量内容
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	run := runTools(t, effects)
	if len(run.Calls) != 1 {
		t.Fatalf("应有一个调用：%#v", run.Calls)
	}
	content := run.Calls[0].Args.OptStr("content")
	if len(content) != 8192 {
		t.Fatalf("执行时应用完整参数，实际内容长度 %d", len(content))
	}
}

// 开流是异步的（请求在另一个 goroutine 里 marshal），而主线程随后还在改会话：
// buildRequest 交给对方的必须是**深拷贝**，否则 DataPart 里的 map 会被同时读写。
// 这个测试在 -race 下才有意义。
func TestRequestIsDeepCopiedForAsyncMarshal(t *testing.T) {
	d, conv := newDriver(t, false)

	// 造一条带 DataPart（map）的历史
	effects, _ := d.Send("读文件")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	run := runTools(t, effects)

	effects, err := d.ToolResult(start.MessageID, tools.ToolOutcome{ID: run.Calls[0].ID, Name: "read", Output: "内容"})
	if err != nil {
		t.Fatal(err)
	}
	next := startStream(t, effects)

	// 请求里的消息不能与会话共享可变状态：修改会话里的 part 不应影响请求
	entry, ok := conv.Message(start.MessageID)
	if !ok {
		t.Fatal("找不到消息")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 模拟 marshal：并发读请求里的 DataPart
		for i := 0; i < 200; i++ {
			for _, msg := range next.Request.Messages {
				for _, part := range msg.Parts {
					if data, ok := part.Content.(message.DataPart); ok {
						if fields, ok := data.Data.(map[string]any); ok {
							_ = fields["output"]
						}
					}
				}
			}
		}
	}()
	// 主线程同时改会话里的同一条消息
	for i := 0; i < 200; i++ {
		_ = conv.UpdatePart(start.MessageID, 0, message.Part{
			Content:   message.DataPart{Data: map[string]any{"id": "c1", "name": "read", "output": "x"}},
			MediaType: render.MediaToolUse,
		})
		_ = entry
	}
	<-done

	// 深拷贝的语义检查：请求里的 tool 结果仍是原始内容
	found := false
	for _, msg := range next.Request.Messages {
		for _, part := range msg.Parts {
			if data, ok := part.Content.(message.DataPart); ok {
				if fields, ok := data.Data.(map[string]any); ok {
					if fields["output"] == "内容" {
						found = true
					}
					if fields["output"] == "x" {
						t.Fatal("请求与会话共享了 DataPart 的 map")
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("请求里应包含工具结果")
	}
}

// 模型不支持图片时：历史里的图片换成「[图片]」，并且同一个模型只提示一次。
func TestVisionUnsupportedReplacesHistoryImages(t *testing.T) {
	d, conv := newDriver(t, false)
	// 造一条带图片的历史（blob 引用，driver 不需要能读到它）
	hash := strings.Repeat("ab", 32)
	image := message.NewURLPart("blob:sha256:" + hash).WithMediaType("image/png")
	if _, err := conv.AddMessage(message.Message{
		MessageID: "u-img", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("看图"), image},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	effects, err := d.Send("描述一下")
	if err != nil {
		t.Fatal(err)
	}
	start := startStream(t, effects)

	// 提示只发一次，并且是显示型（不落盘）
	notice, ok := effectOf[driver.DisplayNotice](effects)
	if !ok {
		t.Fatalf("应有一条显示型提示：%#v", effects)
	}
	if !strings.Contains(notice.Text, "[图片]") {
		t.Fatalf("提示内容应说明图片被替换：%q", notice.Text)
	}
	for _, effect := range effects {
		if rec, ok := effect.(driver.Record); ok && strings.Contains(string(rec.Event.Data), "不支持图片") {
			t.Fatal("提示不应落盘")
		}
	}

	// 请求里图片已变成文字
	openaiMsgs, err := providers.ToOpenAIMessages(start.Request.Messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msgMap := range openaiMsgs {
		content, ok := msgMap["content"].(string)
		if !ok {
			if _, isArray := msgMap["content"].([]any); isArray {
				t.Fatalf("不支持图片时不应出现多模态 content：%#v", msgMap)
			}
			continue
		}
		if strings.Contains(content, "[图片]") {
			found = true
		}
	}
	if !found {
		t.Fatalf("请求里应看到被替换的文字：%#v", openaiMsgs)
	}

	// 第二次发送不再提示
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	effects, err = d.Send("再问一次")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := effectOf[driver.DisplayNotice](effects); ok {
		t.Fatal("同一个模型只应提示一次")
	}
}

// 模型支持图片时：历史里的图片原样保留（请求里是多模态）。
func TestVisionSupportedKeepsImages(t *testing.T) {
	conv := conversation.New("ctx-vision")
	d := driver.New(conv, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:     tools.Builtin(false),
		Vision:    true,
		Blobs: func(hash string) ([]byte, error) {
			return []byte{0x89, 'P', 'N', 'G'}, nil
		},
	})
	hash := strings.Repeat("cd", 32)
	if _, err := conv.AddMessage(message.Message{
		MessageID: "u-img", Role: message.RoleUser,
		Parts: []message.Part{
			message.NewTextPart("看图"),
			message.NewURLPart("blob:sha256:" + hash).WithMediaType("image/png"),
		},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	effects, err := d.Send("描述")
	if err != nil {
		t.Fatal(err)
	}
	start := startStream(t, effects)
	if _, ok := effectOf[driver.DisplayNotice](effects); ok {
		t.Fatal("支持图片时不该有提示")
	}
	openaiMsgs, err := providers.ToOpenAIMessages(start.Request.Messages, func(hash string) ([]byte, error) {
		return []byte{0x89, 'P', 'N', 'G'}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sawArray := false
	for _, msgMap := range openaiMsgs {
		if _, ok := msgMap["content"].([]any); ok {
			sawArray = true
		}
	}
	if !sawArray {
		t.Fatalf("支持图片时请求应是多模态：%#v", openaiMsgs)
	}
}

// 图片按固定值计入估算（每张 1000 token）。
func TestEstimateTokensCountsImages(t *testing.T) {
	hash := strings.Repeat("ef", 32)
	textOnly := []message.Message{{MessageID: "m1", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("hi")}}}
	withImage := append(textOnly, message.Message{MessageID: "m2", Role: message.RoleUser,
		Parts: []message.Part{message.NewURLPart("blob:sha256:" + hash).WithMediaType("image/png")}})
	if got, want := driver.EstimateTokensForTest(withImage)-driver.EstimateTokensForTest(textOnly), 1000; got != want {
		t.Fatalf("每张图片应计 1000 token，实际差值 %d", got)
	}
}

// 用户实测崩溃的回归：工具输出带非法 UTF-8（GBK 文件内容）时，
// `ToolResult` 写日志不能 panic（以前 `MustEvent` 直接 panic 掉界面线程）。
func TestToolResultWithInvalidUTF8DoesNotPanic(t *testing.T) {
	d, _ := newDriver(t, false)
	effects, _ := d.Send("读文件")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	run := runTools(t, effects)

	gbk := string([]byte{0xd6, 0xd0, 0xce, 0xc4})
	effects, err := d.ToolResult(start.MessageID, tools.ToolOutcome{
		ID: run.Calls[0].ID, Name: "read", Output: "内容：" + gbk,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = effects
}

// read_image 的图片引用要跟着工具结果一起落盘，并在重放后仍在（否则重开会话
// 模型就"忘记"看过这张图，界面也显示不出来）。
func TestToolResultImagesPersistAndReplay(t *testing.T) {
	d, _ := newDriver(t, false)
	rec := &recorder{}
	rec.apply(d.Created(""))

	effects, _ := d.Send("看图")
	effects = rec.apply(effects)
	start := startStream(t, effects)
	startEffects, _ := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read_image"})
	rec.apply(startEffects)
	argEffects, _ := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.png"}`})
	rec.apply(argEffects)
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	run := runTools(t, effects)

	ref := tools.ToolImageRef{
		Ref:       "blob:sha256:" + strings.Repeat("ef", 32),
		MediaType: "image/png", Width: 2, Height: 3,
	}
	outcome := tools.ToolOutcome{
		ID: run.Calls[0].ID, Name: "read_image", Output: "<type>image</type>",
		Images: []tools.ToolImageRef{ref},
	}
	effects, err := d.ToolResult(start.MessageID, outcome)
	if err != nil {
		t.Fatal(err)
	}
	rec.apply(effects)

	// 事件 → 重放
	replayed, err := driver.Replay(rec.events)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := replayed.Conversation.Message(start.MessageID)
	if !ok {
		t.Fatal("重放后找不到消息")
	}
	var found bool
	for _, part := range entry.Message.Parts {
		if part.MediaType != render.MediaToolResult {
			continue
		}
		data, ok := partDataOf(part)
		if !ok || data["name"] != "read_image" {
			continue
		}
		images, _ := data["images"].([]any)
		if len(images) != 1 {
			t.Fatalf("重放后应保留图片引用：%#v", data)
		}
		image := images[0].(map[string]any)
		if image["ref"] != ref.Ref || image["media_type"] != "image/png" {
			t.Fatalf("重放后的引用不对：%#v", image)
		}
		found = true
	}
	if !found {
		t.Fatal("重放后没找到 read_image 的结果")
	}
}

// 模型不支持图片时，工具结果里的图片（read_image）也要被剥掉，
// 否则请求会把图片发给不收图片的模型 → 400。
func TestToolResultImagesStrippedForTextModel(t *testing.T) {
	d, _ := newDriver(t, false) // newDriver 的模型不带 vision
	effects, _ := d.Send("看图")
	start := startStream(t, effects)
	startEffects, _ := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read_image"})
	_ = startEffects
	argEffects, _ := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.png"}`})
	_ = argEffects
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	run := runTools(t, effects)

	effects, err := d.ToolResult(start.MessageID, tools.ToolOutcome{
		ID: run.Calls[0].ID, Name: "read_image", Output: "<type>image</type>",
		Images: []tools.ToolImageRef{
			{Ref: "blob:sha256:" + strings.Repeat("11", 32), MediaType: "image/png", Width: 1, Height: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	next := startStream(t, effects)
	for _, msg := range next.Request.Messages {
		for _, part := range msg.Parts {
			if part.MediaType != render.MediaToolResult {
				continue
			}
			data, ok := partDataOf(part)
			if !ok {
				continue
			}
			if images, _ := data["images"].([]any); len(images) > 0 {
				t.Fatalf("文本模型不该收到工具图片：%#v", data)
			}
			if output, _ := data["output"].(string); !strings.Contains(output, "图片已省略") {
				t.Fatalf("应说明图片被省略：%q", output)
			}
		}
	}
}
