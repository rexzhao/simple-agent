package timeline_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/timeline"
)

// 缓存命中率的口径：按协议区分分母。
//
// ⚠️ 这是踩过两次的坑：DeepSeek/OpenAI 的 `prompt_tokens` **已含**命中部分，
// 若再算 `read/(read+input)` 就等于把命中重复计入分母 ——
// 真实 99.53% 会显示成 49.92%（用户一眼看出不对）。
//
// 单位是万分比（9993 → 99.93%）。
func TestUsageCacheHitBasisPoints(t *testing.T) {
	cases := []struct {
		name string
		used timeline.Usage
		want int
	}{
		{
			// 真实数据（DeepSeek）：input 已含命中
			// 656006016 / 659079718 = 99.53%
			name: "DeepSeek input 已含命中",
			used: timeline.Usage{
				InputTokens: 659079718, CacheReadTokens: 656006016,
			},
			want: 9953,
		},
		{
			// 同样两个数字，若按 Anthropic 口径（input 不含命中）
			// 完整输入 = read + input，命中率约 49.89% —— 那是**错的**，
			// 但只有当 InputExcludesCache=true 才该这么算。
			name: "Anthropic input 不含命中",
			used: timeline.Usage{
				InputTokens: 1200, CacheReadTokens: 45000, InputExcludesCache: true,
			},
			want: 9740, // 45000/(45000+1200)
		},
		{
			name: "Anthropic 有写入也算进分母",
			used: timeline.Usage{
				InputTokens: 4, CacheReadTokens: 120000, CacheWriteTokens: 9902,
				InputExcludesCache: true,
			},
			// 120000/(120000+9902+4) = 92.37%
			want: 9237,
		},
		{
			name: "全命中封顶 99.99%",
			used: timeline.Usage{InputTokens: 120000, CacheReadTokens: 120000},
			want: 9999,
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
	}
	for _, c := range cases {
		if got := c.used.CacheHitBasisPoints(); got != c.want {
			t.Fatalf("%s：期望 %d（%.2f%%），实际 %d（%.2f%%）",
				c.name, c.want, float64(c.want)/100, got, float64(got)/100)
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