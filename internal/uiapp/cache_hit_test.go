package uiapp

import (
	"testing"

	"simple-agent/internal/driver"
)

// 缓存命中率的算法：分母是本次请求的**全部输入**。
//
// 两家 provider 的 usage 语义不同，靠同一个公式覆盖：
//   - OpenAI：prompt_tokens 已含命中部分，CacheWrite 恒 0
//   - Anthropic：input_tokens **不含**缓存读写，三者相加才是完整输入
func TestCacheHitPercent(t *testing.T) {
	cases := []struct {
		name  string
		usage driver.Usage
		want  int
	}{
		{
			name:  "OpenAI：80% 命中",
			usage: driver.Usage{InputTokens: 1000, CacheReadTokens: 800},
			want:  80,
		},
		{
			name:  "Anthropic：input 不含缓存，三者相加",
			usage: driver.Usage{InputTokens: 120, CacheReadTokens: 900, CacheWriteTokens: 80},
			// 900 / (900+80+120) = 900/1100 = 81.8% → 82
			want: 82,
		},
		{
			name:  "全部命中",
			usage: driver.Usage{CacheReadTokens: 500},
			want:  100,
		},
		{
			name:  "没命中：只有写入与新增",
			usage: driver.Usage{InputTokens: 400, CacheWriteTokens: 100},
			want:  0,
		},
		{
			name:  "全零：不该除零崩掉",
			usage: driver.Usage{},
			want:  0,
		},
		{
			name:  "四舍五入：98.6% 显示 99%",
			usage: driver.Usage{InputTokens: 1000, CacheReadTokens: 986},
			want:  99,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cacheHitPercent(tc.usage); got != tc.want {
				t.Fatalf("命中率应为 %d%%，实际 %d%%", tc.want, got)
			}
		})
	}
}