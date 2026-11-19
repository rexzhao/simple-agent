package uiapp

import (
	"testing"

	"simple-agent/internal/driver"
)

// 缓存命中率：分母是本次请求的**全部输入**，返回值单位是千分比。
//
// 两家 provider 的 usage 语义不同，靠 CacheWrite 区分：
//   - OpenAI：prompt_tokens 已含命中部分，CacheWrite 恒 0
//   - Anthropic：input_tokens **不含**缓存读写，三者相加才是完整输入
func TestCacheHitBasisPoints(t *testing.T) {
	cases := []struct {
		name  string
		usage driver.Usage
		want  int
	}{
		{
			name:  "OpenAI：80%",
			usage: driver.Usage{InputTokens: 1000, CacheReadTokens: 800},
			want:  8000,
		},
		{
			name:  "Anthropic：input 不含缓存，三者相加",
			usage: driver.Usage{InputTokens: 120, CacheReadTokens: 900, CacheWriteTokens: 80},
			// 900/1100 = 81.8181…% → 向下取整到两位小数 = 81.81%
			want: 8181,
		},
		{
			name: "真机数据：685312/685801 = 99.9287%",
			usage: driver.Usage{InputTokens: 685801, CacheReadTokens: 685312},
			// 685312*10000/685801 = 9992.87 → 9992（99.92%）
			want: 9992,
		},
		{
			name:  "全命中也不显示 100%",
			usage: driver.Usage{CacheReadTokens: 500},
			want:  9999, // 上限 99.99%
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
			name:  "半数",
			usage: driver.Usage{InputTokens: 1000, CacheReadTokens: 500},
			want:  5000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cacheHitBasisPoints(tc.usage); got != tc.want {
				t.Fatalf("应为 %d（%.2f%%），实际 %d（%.2f%%）",
					tc.want, float64(tc.want)/100, got, float64(got)/100)
			}
		})
	}
}

// 千分比 → 两位小数的字符串。
func TestFormatBasisPoints(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{9992, "99.92"},
		{9999, "99.99"},
		{9900, "99.00"},
		{9905, "99.05"},
		{8181, "81.81"},
		{5000, "50.00"},
		{123, "1.23"},
		{7, "0.07"},
		{0, "0.00"},
	}
	for _, tc := range cases {
		if got := formatBasisPoints(tc.in); got != tc.want {
			t.Fatalf("%d 应格式化成 %q，实际 %q", tc.in, tc.want, got)
		}
	}
}