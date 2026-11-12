package host

import (
	"testing"

	"simple-agent/internal/store"
)

// attachTailEvents 必须是个合理的小值：太大首屏慢，太小看不到内容。
func TestAttachTailEventsIsSane(t *testing.T) {
	if attachTailEvents <= 0 {
		t.Fatalf("attachTailEvents 必须为正，实际 %d", attachTailEvents)
	}
	if attachTailEvents > 2000 {
		t.Fatalf("attachTailEvents 过大（首屏会变慢）：%d", attachTailEvents)
	}
}

// tailEvents 的截断语义：只保留最后 n 条。
func TestTailEventsKeepsLastN(t *testing.T) {
	// 用 Seq 当标识，便于断言"取到的是哪一段"
	makeEvents := func(n int) []store.Event {
		out := make([]store.Event, n)
		for i := range out {
			out[i] = store.Event{Seq: uint64(i)}
		}
		return out
	}

	// 比上限多：取最后 n 条
	events := makeEvents(attachTailEvents + 50)
	got := tailEvents(events, attachTailEvents)
	if len(got) != attachTailEvents {
		t.Fatalf("期望 %d 条，实际 %d", attachTailEvents, len(got))
	}
	if got[0].Seq != 50 {
		t.Fatalf("应保留最后 %d 条（首条 Seq 应为 50），实际 %d", attachTailEvents, got[0].Seq)
	}
	if last := uint64(attachTailEvents + 49); got[len(got)-1].Seq != last {
		t.Fatalf("最后一条应是最新事件 Seq=%d，实际 %d", last, got[len(got)-1].Seq)
	}

	// 少于上限：原样返回
	short := makeEvents(10)
	if got := tailEvents(short, attachTailEvents); len(got) != 10 {
		t.Fatalf("不足上限时应原样返回，实际 %d 条", len(got))
	}

	// 空
	if got := tailEvents(nil, attachTailEvents); len(got) != 0 {
		t.Fatalf("空输入应返回空，实际 %d 条", len(got))
	}

	// limit 非法（<=0）时原样返回，不能 panic
	if got := tailEvents(short, 0); len(got) != 10 {
		t.Fatalf("limit<=0 时应原样返回，实际 %d 条", len(got))
	}
}
