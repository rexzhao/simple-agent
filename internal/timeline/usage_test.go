package timeline_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/timeline"
)

// 缓存命中率的口径：命中 / (命中 + 未命中)，**不含写入**。
//
// 为什么不含 cache_write：它是"写缓存的量"，费用和普通输入不同；
// 混进分母会让第一次请求（全写入）显示成 0%，看着像缓存没用。
func TestUsageCacheHitBasisPoints(t *testing.T) {
	cases := []struct {
		name string
		used timeline.Usage
		want int
	}{
		{
			name: "全命中",
			used: timeline.Usage{InputTokens: 2, CacheReadTokens: 120000},
			want: 999, // 向下取整，不显示 100%
		},
		{
			name: "一半",
			used: timeline.Usage{InputTokens: 500, CacheReadTokens: 500},
			want: 500,
		},
		{
			name: "只写不读 → 不显示",
			used: timeline.Usage{InputTokens: 4, CacheWriteTokens: 9902},
			want: -1,
		},
		{
			name: "没有任何数据 → 不显示",
			used: timeline.Usage{},
			want: -1,
		},
		{
			name: "命中为 0 但未命中很大",
			used: timeline.Usage{InputTokens: 50000},
			want: -1,
		},
	}
	for _, c := range cases {
		if got := c.used.CacheHitBasisPoints(); got != c.want {
			t.Fatalf("%s：期望 %d，实际 %d", c.name, c.want, got)
		}
	}
}

// 总 token 要把四类都算上（用户看的是"这条消息花了多少"）。
func TestUsageTotalTokens(t *testing.T) {
	used := timeline.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 100, CacheWriteTokens: 5}
	if got := used.TotalTokens(); got != 135 {
		t.Fatalf("应为 135，实际 %d", got)
	}
}

// 流式期间也要有回合行（Live），且带上本轮用量 —— 用户要在跑的时候
// 就看到运行时间/token/缓存率，而不是等跑完才有这一行。
func TestRoundFooterShownWhileLive(t *testing.T) {
	conv := conversation.New("ctx")
	if _, err := conv.AddMessage(message.Message{
		MessageID: "u1", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("提问")},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	// 助手消息**还在流式**
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{message.NewTextPart("回答中")},
	}, conversation.StatusStreaming); err != nil {
		t.Fatal(err)
	}

	usage := &timeline.Usage{InputTokens: 5, CacheReadTokens: 500}
	// idle=false：会话还在跑
	items := timeline.Build(conv.Entries(), false, usage)

	footer := findFooter(items)
	if footer == nil {
		t.Fatal("流式期间也应有回合行（让用户看到运行时间/用量）")
	}
	if !footer.Live {
		t.Fatal("流式期间的回合行应标记 Live")
	}
	if footer.Usage == nil || footer.Usage.CacheReadTokens != 500 {
		t.Fatalf("回合行应带上本轮用量：%+v", footer.Usage)
	}
}

// 结束后同一个位置只有一条，且不再标 Live（变成"总计"）。
func TestRoundFooterSingleAfterDone(t *testing.T) {
	conv := conversation.New("ctx")
	if _, err := conv.AddMessage(message.Message{
		MessageID: "u1", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("提问")},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{message.NewTextPart("答复")},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	items := timeline.Build(conv.Entries(), true,
		&timeline.Usage{InputTokens: 5, CacheReadTokens: 500})

	count := 0
	for _, item := range items {
		if item.Kind == timeline.KindRoundFooter {
			count++
			if item.Live {
				t.Fatal("结束后不该再标 Live")
			}
		}
	}
	if count != 1 {
		t.Fatalf("结束行应只有一条，实际 %d", count)
	}
}

func findFooter(items []timeline.Item) *timeline.Item {
	for index := range items {
		if items[index].Kind == timeline.KindRoundFooter {
			return &items[index]
		}
	}
	return nil
}