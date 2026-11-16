package driver_test

import (
	"testing"

	"simple-agent/internal/driver"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
)

// 边界事件必须真的写进日志：Round/Turn 的 started 与 finished 成对出现。
//
// 崩溃恢复依赖这些事件判定"哪些 Turn 没走完"，没有它们就只能靠推导，
// 而推导在自动续轮（没有 user 消息）时会误判。
func TestRoundTurnBoundaryEvents(t *testing.T) {
	d, _ := newDriver(t, false)

	effects, err := d.Send("你好")
	if err != nil {
		t.Fatal(err)
	}
	events := recordEvents(t, effects)
	kinds := countKinds(events)

	for _, want := range []string{
		store.TypeRoundStarted,
		store.TypeTurnStarted,
	} {
		if kinds[want] == 0 {
			t.Fatalf("缺少事件 %s（实际有：%v）", want, kinds)
		}
	}

	// 顺序：round.started 必须在 turn.started 之前
	roundStart, turnStart := -1, -1
	for index, event := range events {
		switch event.Type {
		case store.TypeRoundStarted:
			if roundStart < 0 {
				roundStart = index
			}
		case store.TypeTurnStarted:
			if turnStart < 0 {
				turnStart = index
			}
		}
	}
	if roundStart < 0 || turnStart < 0 {
		t.Fatalf("两种事件都应有：round=%d turn=%d", roundStart, turnStart)
	}
	if roundStart > turnStart {
		t.Fatalf("round.started 应在 turn.started 之前（%d > %d）", roundStart, turnStart)
	}
}

// 正常结束（无工具调用）时，Turn 与 Round 都要收尾。
func TestTurnAndRoundFinishedOnCompletion(t *testing.T) {
	d, _ := newDriver(t, false)
	start := sendAndStart(t, d, "你好")

	effects, err := d.ApplyEvent(start.MessageID, providers.Event{
		Kind: providers.EventDone, StopReason: "stop",
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := countKinds(recordEvents(t, effects))

	if kinds[store.TypeTurnFinished] != 1 {
		t.Fatalf("应有 1 条 turn.finished，实际 %d（事件：%v）", kinds[store.TypeTurnFinished], kinds)
	}
	if kinds[store.TypeRoundFinished] != 1 {
		t.Fatalf("无工具调用时应结束 Round，实际 %d（事件：%v）", kinds[store.TypeRoundFinished], kinds)
	}
}

// 工具续轮：有工具调用时 Round 不能结束。
func TestRoundNotFinishedWithToolCall(t *testing.T) {
	d, _ := newDriver(t, false)
	start := sendAndStart(t, d, "读文件")

	d.ApplyEvent(start.MessageID, providers.Event{
		Kind: providers.EventToolStart, Index: 0, ID: "call-1", Name: "read",
	})
	d.ApplyEvent(start.MessageID, providers.Event{
		Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`,
	})
	effects, err := d.ApplyEvent(start.MessageID, providers.Event{
		Kind: providers.EventDone, StopReason: "tool_calls",
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := countKinds(recordEvents(t, effects))

	if kinds[store.TypeTurnFinished] != 1 {
		t.Fatalf("应有 1 条 turn.finished，实际 %d", kinds[store.TypeTurnFinished])
	}
	if kinds[store.TypeRoundFinished] != 0 {
		t.Fatalf("有工具调用时不该结束 Round，实际 %d", kinds[store.TypeRoundFinished])
	}
}

// sendAndStart 发一条消息并取出 StartStream（后续用它的 MessageID 推进流）。
func sendAndStart(t *testing.T, d *driver.Driver, text string) driver.StartStream {
	t.Helper()
	effects, err := d.Send(text)
	if err != nil {
		t.Fatalf("Send 失败：%v", err)
	}
	return startStream(t, effects)
}

// recordEvents 从 effects 里取出所有 Record 事件。
func recordEvents(t *testing.T, effects []driver.Effect) []store.Event {
	t.Helper()
	var events []store.Event
	for _, effect := range effects {
		if rec, ok := effect.(driver.Record); ok {
			events = append(events, rec.Event)
		}
	}
	return events
}

// countKinds 统计事件类型出现次数。
func countKinds(events []store.Event) map[string]int {
	counts := map[string]int{}
	for _, event := range events {
		counts[event.Type]++
	}
	return counts
}