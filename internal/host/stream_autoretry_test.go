package host_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
)

// flakyRuntime 第一次流"没有结束标记就断"，第二次正常结束。
type flakyRuntime struct {
	calls atomic.Int64
}

func (r *flakyRuntime) OpenStream(_ context.Context, _ providers.Selection, _ providers.ChatRequest) (<-chan providers.Event, error) {
	n := r.calls.Add(1)
	events := make(chan providers.Event, 8)
	go func() {
		defer close(events)
		if n == 1 {
			events <- providers.Event{Kind: providers.EventText, Text: "半句"}
			return // 没有 Done 就关闭 → 连接意外中断
		}
		events <- providers.Event{Kind: providers.EventText, Text: "完整回答"}
		events <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	}()
	return events, nil
}

func (r *flakyRuntime) RunTools(context.Context, *tools.ToolContext, []tools.Call) []tools.ToolOutcome {
	return nil
}

func TestStreamInterruptionAutoContinues(t *testing.T) {
	rt := &flakyRuntime{}
	p := &poster{}
	local := host.NewLocal(host.Config{
		Runtime: rt, Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: p.Post,
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	sess := local.Active()
	payload, _ := host.EncodePayload(host.SendPayload{Text: "说说看"})
	if _, err := local.Submit(host.Command{SessionID: sess.ID, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		if local.Session(sess.ID).Busy() == driver.BusyIdle && rt.calls.Load() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rt.calls.Load() < 2 {
		t.Fatalf("没有自动继续（流只开了 %d 次）", rt.calls.Load())
	}
	if busy := local.Session(sess.ID).Busy(); busy != driver.BusyIdle {
		t.Fatalf("自动继续后没有回到空闲：%v", busy)
	}
}

// alwaysBrokenRuntime 每次都在没有结束标记的情况下关闭流。
type alwaysBrokenRuntime struct{ calls atomic.Int64 }

func (r *alwaysBrokenRuntime) OpenStream(context.Context, providers.Selection, providers.ChatRequest) (<-chan providers.Event, error) {
	r.calls.Add(1)
	events := make(chan providers.Event, 4)
	go func() {
		defer close(events)
		events <- providers.Event{Kind: providers.EventText, Text: "半句"}
	}()
	return events, nil
}

func (r *alwaysBrokenRuntime) RunTools(context.Context, *tools.ToolContext, []tools.Call) []tools.ToolOutcome {
	return nil
}

// 连续中断：自动继续最多 2 次，之后保持失败（不能无限重试）。
func TestStreamInterruptionStopsAfterLimit(t *testing.T) {
	rt := &alwaysBrokenRuntime{}
	p := &poster{}
	local := host.NewLocal(host.Config{
		Runtime: rt, Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: p.Post,
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	sess := local.Active()
	payload, _ := host.EncodePayload(host.SendPayload{Text: "再试一次"})
	if _, err := local.Submit(host.Command{SessionID: sess.ID, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		if rt.calls.Load() >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	p.pump()
	if rt.calls.Load() > 3 {
		t.Fatalf("自动继续超过上限：开流 %d 次", rt.calls.Load())
	}
}
