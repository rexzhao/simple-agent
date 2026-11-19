package providers

import "testing"

// marshalCompact 必须稳定：它的输出会进历史消息的 arguments 字段，
// 键顺序随机会让同一段历史在每次请求里字节表示都不同，
// prompt caching 从那里起全部失效。
func TestMarshalCompactIsStable(t *testing.T) {
	args := map[string]any{
		"description": "列出关键子目录与最近提交",
		"command":     "dir /b crates tools",
		"n":           float64(3),
	}
	first := marshalCompact(args)
	for i := 0; i < 50; i++ {
		if got := marshalCompact(args); got != first {
			t.Fatalf("第 %d 次序列化不同：\n第一次：%s\n这一次：%s", i+1, first, got)
		}
	}
	// 按字母序（command < description < n）
	if first != `{"command":"dir /b crates tools","description":"列出关键子目录与最近提交","n":3}` {
		t.Fatalf("键顺序应按字母序，实际：%s", first)
	}
}