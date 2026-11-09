package driver_test

import (
	"strings"
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
)

// 这组用例对齐 Rust 版 AskGroup::output 的 6 个测试。

func questionSingle(id, text string, labels ...string) tools.QuestionSpec {
	spec := tools.QuestionSpec{ID: id, Question: text}
	for _, label := range labels {
		spec.Options = append(spec.Options, tools.QuestionOption{Label: label})
	}
	return spec
}

// 1. 提交：选中项写进 selected。
func TestAskGroupSubmit(t *testing.T) {
	group := driver.NewAskGroup("m1", "c1", []tools.QuestionSpec{
		questionSingle("q1", "选哪个？", "甲", "乙"),
	})
	group.Toggle(0, 1)
	out := group.Output(driver.AskSubmit, "")
	if !strings.Contains(out, `"id":"q1"`) || !strings.Contains(out, `"selected":["乙"]`) {
		t.Fatalf("提交结果不一致：%s", out)
	}
	if strings.Contains(out, "custom") {
		t.Fatalf("提交不应带 custom：%s", out)
	}
}

// 2. 跳过：answers 为空且带 dismissed。
func TestAskGroupDismiss(t *testing.T) {
	group := driver.NewAskGroup("m1", "c1", []tools.QuestionSpec{questionSingle("q1", "?", "甲")})
	group.Toggle(0, 0)
	out := group.Output(driver.AskDismiss, "")
	if out != `{"answers":[],"dismissed":true}` {
		t.Fatalf("跳过结果不一致：%s", out)
	}
}

// 3. 单问题 custom：输入文字作为答案。
func TestAskGroupCustomSingleQuestion(t *testing.T) {
	group := driver.NewAskGroup("m1", "c1", []tools.QuestionSpec{questionSingle("q1", "你的名字？")})
	out := group.Output(driver.AskCustom, "张三")
	if !strings.Contains(out, `"custom":"张三"`) {
		t.Fatalf("custom 应写进答案：%s", out)
	}
	if strings.Contains(out, "note") {
		t.Fatalf("custom 已消费时不应带 note：%s", out)
	}
}

// 4. 多问题：custom 只给第一个没选的问题。
func TestAskGroupCustomOnlyFirstUnanswered(t *testing.T) {
	group := driver.NewAskGroup("m1", "c1", []tools.QuestionSpec{
		questionSingle("q1", "第一个", "甲"),
		questionSingle("q2", "第二个"),
	})
	group.Toggle(0, 0) // 第一题已选
	out := group.Output(driver.AskCustom, "补充")
	if !strings.Contains(out, `"custom":"补充"`) {
		t.Fatalf("custom 应出现在答案里：%s", out)
	}
	// 只能出现一次
	if strings.Count(out, `"custom"`) != 1 {
		t.Fatalf("custom 只用一次：%s", out)
	}
	// 且属于 q2
	if !strings.Contains(out, `"id":"q2","selected":[],"custom":"补充"`) {
		t.Fatalf("custom 应给第一个未选项的问题：%s", out)
	}
}

// 5. 全部已选 + 输入文字：文字放进 note，不静默丢弃。
func TestAskGroupNoteWhenAllSelected(t *testing.T) {
	group := driver.NewAskGroup("m1", "c1", []tools.QuestionSpec{
		questionSingle("q1", "选哪个？", "甲", "乙"),
	})
	group.Toggle(0, 0)
	out := group.Output(driver.AskCustom, "另外补充一句")
	if !strings.Contains(out, `"note":"另外补充一句"`) {
		t.Fatalf("全部已选时文字应进 note：%s", out)
	}
	if strings.Contains(out, `"custom"`) {
		t.Fatalf("不应把文字塞成 custom：%s", out)
	}
}

// 6. 空白文本：等同于普通提交。
func TestAskGroupBlankCustomIgnored(t *testing.T) {
	group := driver.NewAskGroup("m1", "c1", []tools.QuestionSpec{questionSingle("q1", "?", "甲")})
	group.Toggle(0, 0)
	out := group.Output(driver.AskCustom, "   ")
	if strings.Contains(out, "custom") || strings.Contains(out, "note") {
		t.Fatalf("空白文本应被忽略：%s", out)
	}
	if !strings.Contains(out, `"selected":["甲"]`) {
		t.Fatalf("选中项应保留：%s", out)
	}
}

// 多选叠加、单选互斥、再次点击取消多选。
func TestAskGroupToggleSemantics(t *testing.T) {
	multi := tools.QuestionSpec{ID: "q1", Question: "多选", MultiSelect: true,
		Options: []tools.QuestionOption{{Label: "a"}, {Label: "b"}, {Label: "c"}}}
	group := driver.NewAskGroup("m", "c", []tools.QuestionSpec{multi})

	group.Toggle(0, 0)
	group.Toggle(0, 2)
	if !group.Selected(0, 0) || !group.Selected(0, 2) || group.Selected(0, 1) {
		t.Fatal("多选应叠加")
	}
	group.Toggle(0, 0)
	if group.Selected(0, 0) {
		t.Fatal("再次点击应取消")
	}

	single := tools.QuestionSpec{ID: "q2", Question: "单选",
		Options: []tools.QuestionOption{{Label: "x"}, {Label: "y"}}}
	group2 := driver.NewAskGroup("m", "c", []tools.QuestionSpec{single})
	group2.Toggle(0, 0)
	group2.Toggle(0, 1)
	if group2.Selected(0, 0) || !group2.Selected(0, 1) {
		t.Fatal("单选应互斥")
	}

	// 越界不 panic
	group.Toggle(9, 9)
	group.Toggle(-1, 0)
}

// 多选答案的顺序稳定（map 遍历顺序随机，必须按选项顺序输出）。
func TestAskGroupMultiSelectOrderStable(t *testing.T) {
	spec := tools.QuestionSpec{ID: "q1", Question: "多选", MultiSelect: true,
		Options: []tools.QuestionOption{{Label: "a"}, {Label: "b"}, {Label: "c"}, {Label: "d"}}}
	for i := 0; i < 20; i++ {
		group := driver.NewAskGroup("m", "c", []tools.QuestionSpec{spec})
		group.Toggle(0, 3)
		group.Toggle(0, 1)
		out := group.Output(driver.AskSubmit, "")
		if !strings.Contains(out, `"selected":["b","d"]`) {
			t.Fatalf("多选顺序应稳定：%s", out)
		}
	}
}

// 提交/跳过的驱动层流程：多组提问一次提交。
func TestSubmitAsksSettlesAllGroups(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("两个问题")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providersEventToolStart(0, "ask_a"))
	d.ApplyEvent(start.MessageID, providersEventToolStart(1, "ask_b"))
	effects, _ = d.ApplyEvent(start.MessageID, doneEventToolCalls())
	run := runTools(t, effects)
	if len(run.Calls) != 2 {
		t.Fatalf("应有 2 个调用，实际 %d", len(run.Calls))
	}
	if _, err := d.ToolResults(start.MessageID, []tools.ToolOutcome{
		{ID: "ask_a", Name: "ask_user_question", Pending: []tools.QuestionSpec{questionSingle("q1", "A?", "甲")}},
		{ID: "ask_b", Name: "ask_user_question", Pending: []tools.QuestionSpec{questionSingle("q2", "B?")}},
	}); err != nil {
		t.Fatal(err)
	}

	groups := d.AskGroups()
	if len(groups) != 2 {
		t.Fatalf("应有 2 组提问，实际 %d", len(groups))
	}
	groups[0].Toggle(0, 0)

	effects, err := d.SubmitAsks("自定义")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := effectOf[driver.StartStream](effects); !ok {
		t.Fatal("提交后应继续开流")
	}

	// 两条工具结果都被回填（不再是 pending），且都带答案
	entry, _ := conv.Message(start.MessageID)
	var outputs []string
	for _, part := range entry.Message.Parts {
		data, ok := partDataOf(part)
		if !ok {
			continue
		}
		if pending, _ := data["pending"].(bool); pending {
			t.Fatal("提交后不应还有 pending 占位")
		}
		if out, _ := data["output"].(string); strings.Contains(out, "answers") {
			outputs = append(outputs, out)
		}
	}
	if len(outputs) != 2 {
		t.Fatalf("应回填 2 条答案：%#v", outputs)
	}
	// 第一组带选中项，第二组带 custom
	joined := strings.Join(outputs, "\n")
	if !strings.Contains(joined, `"selected":["甲"]`) || !strings.Contains(joined, `"custom":"自定义"`) {
		t.Fatalf("答案内容不一致：%s", joined)
	}
}

// 三个小辅助：把测试里反复写的事件与取值收起来。
func providersEventToolStart(index int, id string) providers.Event {
	return providers.Event{Kind: providers.EventToolStart, Index: index, ID: id, Name: "ask_user_question"}
}

func doneEventToolCalls() providers.Event {
	return providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"}
}

func partDataOf(part message.Part) (map[string]any, bool) {
	dataPart, ok := part.Content.(message.DataPart)
	if !ok {
		return nil, false
	}
	data, ok := dataPart.Data.(map[string]any)
	return data, ok
}

// 思考档位必须跟着选择一起持久化，并在重放后恢复。
func TestReasoningEffortPersisted(t *testing.T) {
	conv := conversation.New("ctx-effort")
	d := driver.New(conv, driver.Options{
		Selection:       providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:           tools.Builtin(false),
		ReasoningEffort: "medium",
	})
	rec := &recorder{}
	rec.apply(d.Created(""))

	effects := d.SetReasoningEffort("high")
	if len(effects) == 0 {
		t.Fatal("切换档位应产生事件")
	}
	rec.apply(effects)
	if d.ReasoningEffort() != "high" {
		t.Fatalf("档位未更新：%q", d.ReasoningEffort())
	}

	replayed, err := driver.Replay(rec.events)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ReasoningEffort != "high" {
		t.Fatalf("重放后档位不一致：%q", replayed.ReasoningEffort)
	}
	restored := driver.Restore(replayed, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:     tools.Builtin(false),
	})
	if restored.ReasoningEffort() != "high" {
		t.Fatalf("恢复后档位不一致：%q", restored.ReasoningEffort())
	}

	// 请求里要带上当前档位
	effects, err = restored.Send("hi")
	if err != nil {
		t.Fatal(err)
	}
	start, _ := effectOfType[driver.StartStream](effects)
	if start.Request.ReasoningEffort != "high" {
		t.Fatalf("请求未带档位：%q", start.Request.ReasoningEffort)
	}
}

// 最大输出 token 数来自模型配置（而不是写死）。
func TestMaxOutputTokensFromModel(t *testing.T) {
	conv := conversation.New("ctx-max")
	d := driver.New(conv, driver.Options{
		Selection:       providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:           tools.Builtin(false),
		MaxOutputTokens: 8_000,
	})
	effects, err := d.Send("hi")
	if err != nil {
		t.Fatal(err)
	}
	start, _ := effectOfType[driver.StartStream](effects)
	if start.Request.MaxTokens != 8_000 {
		t.Fatalf("请求的 max_tokens 应为 8000，实际 %d", start.Request.MaxTokens)
	}
	// 切换模型后同步新上限
	d.SetMaxOutputTokens(16_000)
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	rec := &recorder{}
	rec.apply(effects)
	effects, err = d.Send("next")
	if err != nil {
		t.Fatal(err)
	}
	start, _ = effectOfType[driver.StartStream](effects)
	if start.Request.MaxTokens != 16_000 {
		t.Fatalf("切换后 max_tokens 应为 16000，实际 %d", start.Request.MaxTokens)
	}
}
