package timeline_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/timeline"
)

// 缓存命中率的口径：命中 / (命中 + 未命中)，**不含写入**。
//
// 单位是**万分比**（9993 → 99.93%）。曾经写成千分比，界面显示成 9.74%
// 而实际是 97.4% —— 差 10 倍，用户一眼就看出来了。
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
			// 45000/(45000+1200) = 97.4% → 9740 万分比
			name: "接近全命中",
			used: timeline.Usage{InputTokens: 1200, CacheReadTokens: 45000},
			want: 9740,
		},
		{
			name: "全命中封顶 99.99%",
			used: timeline.Usage{InputTokens: 2, CacheReadTokens: 120000},
			want: 9999, // 向下取整，且不显示 100%
		},
		{
			name: "一半",
			used: timeline.Usage{InputTokens: 500, CacheReadTokens: 500},
			want: 5000,
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
	// idle=false, running=true：会话还在跑
	items := timeline.Build(conv.Entries(), false, true, usage)

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

// **工具执行期间**也要有「● 运行中」——这是用户报的"有时候 running 那条会消失"。
//
// 根因：工具执行时助手消息已经是 complete（模型已经输出完哪个工具要调），
// 但回合还没结束（下一轮流还没开始）。只看消息的 streaming 状态就会漏掉这一段，
// 界面上「运行中」突然消失，用户以为任务结束了，实际工具还在跑。
func TestRoundFooterShownWhileRunningTools(t *testing.T) {
	conv := conversation.New("ctx")
	if _, err := conv.AddMessage(message.Message{
		MessageID: "u1", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("跑个命令")},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	// 助手消息**已经 complete**（工具调用已发出），但工具正在执行
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{{Content: message.DataPart{Data: map[string]any{
			"id": "c1", "name": "bash", "args": map[string]any{"command": "ls"},
		}}, MediaType: "application/vnd.simple-agent.tool-use+json"}},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	// idle=false（会话不空闲，工具在跑）、running=true
	items := timeline.Build(conv.Entries(), false, true,
		&timeline.Usage{InputTokens: 5, CacheReadTokens: 500})

	footer := findFooter(items)
	if footer == nil {
		t.Fatal("工具执行期间也该有「运行中」行（否则用户以为任务结束了）")
	}
	if !footer.Live {
		t.Fatal("工具执行期间应标 Live")
	}
	if footer.Usage == nil {
		t.Fatal("运行中行应带上实时用量")
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

	items := timeline.Build(conv.Entries(), true, false,
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