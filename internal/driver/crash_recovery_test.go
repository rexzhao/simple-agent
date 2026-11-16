package driver_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// 崩溃在工具执行中：message.output 已落盘（含 tool_use），但没有 tool.result。
//
// 恢复时必须补 tool.result（is_error）—— 否则下次请求会因
// 「tool_use 无配对结果」被 LLM 协议直接拒收。这与 turn.finished 无关。
func TestCrashDuringToolExecutionFillsResults(t *testing.T) {
	conv := conversation.New("ctx")
	d := driver.New(conv, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:     tools.Builtin(false),
	})
	rec := &recorder{}
	rec.apply(d.Created(""))

	effects, _ := d.Send("读文件")
	effects = rec.apply(effects)
	start := startStream(t, effects)

	// 制造一个工具调用，然后让流结束（message.output 落盘）
	e1, _ := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"})
	rec.apply(e1)
	e2, _ := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`})
	rec.apply(e2)
	e3, _ := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	rec.apply(e3)
	// 崩溃：不执行工具，不写 tool.result

	replayed, err := driver.Replay(rec.events)
	if err != nil {
		t.Fatal(err)
	}

	// 必须补出 tool.result
	var toolResults int
	for _, repair := range replayed.Repairs {
		if repair.Type == store.TypeToolResult {
			toolResults++
			var data driver.ToolResultData
			if err := repair.Decode(&data); err != nil {
				t.Fatal(err)
			}
			if !data.IsError {
				t.Fatal("补齐的结果应标为 is_error")
			}
		}
	}
	if toolResults == 0 {
		t.Fatal("崩溃在工具执行中时必须补 tool.result，否则下次请求会被协议拒收")
	}
}

// 崩溃在流式输出中：只有 message.input，没有 output。
//
// 按新模型这是"请求未发出"：不补内容（流式增量本来就没落盘），
// 但要留下 OpenTurn 标记，让界面提示"上次异常结束"并可重发。
func TestCrashDuringStreamingMarksOpenTurn(t *testing.T) {
	conv := conversation.New("ctx")
	d := driver.New(conv, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:     tools.Builtin(false),
	})
	rec := &recorder{}
	rec.apply(d.Created(""))

	effects, _ := d.Send("你好")
	rec.apply(effects) // 只记录到这里：round.started / turn.started / message.input
	// 崩溃：没有任何 output

	replayed, err := driver.Replay(rec.events)
	if err != nil {
		t.Fatal(err)
	}

	// OpenTurn 应被保留（表示有一次请求没走完）
	if replayed.OpenTurn.Round == 0 {
		t.Fatal("应记录 OpenTurn（round.started 已落盘，turn.finished 缺失）")
	}
	// 不该凭空空补出 message.output
	var outputs int
	for _, event := range rec.events {
		if event.Type == store.TypeMessageOutput {
			outputs++
		}
	}
	if outputs != 0 {
		t.Fatalf("流式崩溃不该产生 message.output，实际 %d", outputs)
	}
}

// 内容完整但缺 turn.finished：只补边界标记，不重复写内容。
func TestMissingTurnFinishedOnlyAddsBoundary(t *testing.T) {
	conv := conversation.New("ctx")
	d := driver.New(conv, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:     tools.Builtin(false),
	})
	rec := &recorder{}
	rec.apply(d.Created(""))

	effects, _ := d.Send("你好")
	rec.apply(effects) // round.started / turn.started / message.input
	start := startStream(t, effects)

	doneEffects, _ := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	// 只保留非 turn.finished 的事件，模拟"内容写了但边界没写"
	var kept []driver.Effect
	for _, effect := range doneEffects {
		if record, ok := effect.(driver.Record); ok && record.Event.Type == store.TypeTurnFinished {
			continue
		}
		kept = append(kept, effect)
	}
	rec.apply(kept)

	replayed, err := driver.Replay(rec.events)
	if err != nil {
		t.Fatal(err)
	}

	var turnFinished int
	for _, repair := range replayed.Repairs {
		if repair.Type == store.TypeTurnFinished {
			turnFinished++
		}
		if repair.Type == store.TypeMessageOutput {
			t.Fatal("内容已经完整，不该再补 message.output")
		}
	}
	if turnFinished != 1 {
		t.Fatalf("应补 1 条 turn.finished，实际 %d（repairs=%d）", turnFinished, len(replayed.Repairs))
	}
}