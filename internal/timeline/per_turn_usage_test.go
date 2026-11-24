package timeline_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/timeline"
)

// 每个回合显示**自己那一轮**的缓存率，不是会话累计。
//
// 用户报的："每轮会话的 cache 现在只有一个数字，所有轮显示的都是一个数字"。
// 根因：界面传进来的是 session.Usage()（会话累计值），Build 把它用到了
// 每一个回合上。现在用量随消息留存，按轮汇总。
func TestEachTurnHasOwnCacheRate(t *testing.T) {
	conv := conversation.New("ctx")

	// 第一轮：低命中（新会话刚建立缓存）
	addUser(t, conv, "u1", "第一问")
	addAgent(t, conv, "a1", "第一答", conversation.Usage{
		InputTokens: 50000, CacheReadTokens: 10000, CacheWriteTokens: 40000,
	})

	// 第二轮：高命中（缓存已建立）。
	//
	// 注意 input 必须 >= cache_read：DeepSeek/OpenAI 的 prompt_tokens
	// **已含**命中部分，缓存命中的 90000 是这 90100 里的绝大部分，
	// 只有 100 是真重新算的。
	addUser(t, conv, "u2", "第二问")
	addAgent(t, conv, "a2", "第二答", conversation.Usage{
		InputTokens: 90100, CacheReadTokens: 90000,
	})

	items := timeline.Build(conv.Entries(), true, false)

	var footers []timeline.Item
	for _, item := range items {
		if item.Kind == timeline.KindRoundFooter {
			footers = append(footers, item)
		}
	}
	if len(footers) != 2 {
		t.Fatalf("应有 2 个回合行，实际 %d", len(footers))
	}

	first, second := footers[0].Usage, footers[1].Usage
	if first == nil || second == nil {
		t.Fatal("两个回合都该有用量")
	}

	// 第一轮：Anthropic 口径（有写入 → input 不含缓存）
	// 10000/(50000+10000+40000) = 10%
	if got := first.CacheHitBasisPoints(); got != 1000 {
		t.Fatalf("第一轮命中率应是 10%%，实际 %d.%02d%%", got/100, got%100)
	}
	// 第二轮：无写入 → input 已含命中，90000/90100 = 99.88%
	// （90000*10000/90100 = 9988.9…，向下取整 9988）
	if got := second.CacheHitBasisPoints(); got != 9988 {
		t.Fatalf("第二轮命中率应是 99.88%%，实际 %d.%02d%%（原始 %d）",
			got/100, got%100, got)
	}
	if first.CacheHitBasisPoints() == second.CacheHitBasisPoints() {
		t.Fatal("两轮命中率不该相同（这正是用户报的 bug）")
	}
}

// 没有用量数据的回合不显示缓存率（而不是显示一个假的 0%）。
func TestTurnWithoutUsageHasNoCacheRate(t *testing.T) {
	conv := conversation.New("ctx")
	addUser(t, conv, "u1", "问")
	addAgent(t, conv, "a1", "答", conversation.Usage{})

	items := timeline.Build(conv.Entries(), true, false)
	for _, item := range items {
		if item.Kind != timeline.KindRoundFooter {
			continue
		}
		if item.Usage != nil {
			t.Fatalf("没有数据时 Usage 应为 nil，实际 %+v", item.Usage)
		}
	}
}

// 一轮里有多条 agent 消息（工具调用续轮）时，用量相加。
func TestTurnUsageSumsMultipleEntries(t *testing.T) {
	conv := conversation.New("ctx")
	addUser(t, conv, "u1", "跑两个工具")
	// 两条 agent 消息属于同一轮（中间没有 user 消息）
	addAgent(t, conv, "a1", "第一次", conversation.Usage{
		InputTokens: 1000, CacheReadTokens: 9000,
	})
	addAgent(t, conv, "a2", "第二次", conversation.Usage{
		InputTokens: 2000, CacheReadTokens: 8000,
	})

	items := timeline.Build(conv.Entries(), true, false)
	var footer *timeline.Item
	for index := range items {
		if items[index].Kind == timeline.KindRoundFooter {
			footer = &items[index]
		}
	}
	if footer == nil || footer.Usage == nil {
		t.Fatal("应有回合行且带用量")
	}
	if footer.Usage.InputTokens != 3000 || footer.Usage.CacheReadTokens != 17000 {
		t.Fatalf("应收两次之和：%+v", footer.Usage)
	}
}

func addUser(t *testing.T, conv *conversation.Conversation, id, text string) {
	t.Helper()
	if _, err := conv.AddMessage(message.Message{
		MessageID: id, Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart(text)},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
}

func addAgent(t *testing.T, conv *conversation.Conversation, id, text string, used conversation.Usage) {
	t.Helper()
	entry, err := conv.AddMessage(message.Message{
		MessageID: id, Role: message.RoleAgent,
		Parts: []message.Part{message.NewTextPart(text)},
	}, conversation.StatusComplete)
	if err != nil {
		t.Fatal(err)
	}
	entry.SetUsage(used)
}