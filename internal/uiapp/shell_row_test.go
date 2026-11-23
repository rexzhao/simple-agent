package uiapp

import (
	"strings"
	"testing"
)

// execArgsSummary 里的时间标注。
//
// ⚠️ timeoutMs 是**超时上限**，不是实际耗时。以前直接 formatElapsed 成
// "2m00s"，用户读成"这条命令跑了 2 分钟"（实际可能只跑了 80ms）。
// 现在带"限时"字样，而且默认值不显示（满屏 2m00s 纯噪音）。
func TestExecArgsSummaryTimeoutLabel(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "默认超时不显示",
			args: map[string]any{"timeoutMs": defaultShellTimeoutMs},
			want: "",
		},
		{
			name: "自定义超时带限时标注",
			args: map[string]any{"timeoutMs": 300_000},
			want: "限时 5m00s",
		},
		{
			name: "workdir 与限时并列",
			args: map[string]any{"workdir": "internal", "timeoutMs": 60_000},
			want: "internal · 限时 1m00s",
		},
		{
			name: "只有 workdir",
			args: map[string]any{"workdir": "internal"},
			want: "internal",
		},
		{
			name: "JSON 解出来的 float64 也要认",
			args: map[string]any{"timeoutMs": float64(30_000)},
			// formatElapsed 对秒级保留两位小数（那是给"实际耗时"用的格式），
			// 超时上限沿用它：30.00s。数值对、标注也对就够了。
			want: "限时 30.00s",
		},
	}
	for _, c := range cases {
		got := execArgsSummary(map[string]any{"args": c.args})
		if got != c.want {
			t.Fatalf("%s：期望 %q，实际 %q", c.name, c.want, got)
		}
	}
}

// 折叠行仍然只给一行摘要（不换行）。
//
// 用户澄清："bash 的换行不是概要里面换行，是展开之后的黑底白字的那个地方换行"。
// 折叠行的作用是扫一眼知道调了什么，多行命令在这里换行会把时间线撑长。
func TestCollapsedShellRowStaysOneLine(t *testing.T) {
	command := "cd /a && \\\ncargo build && \\\nls -la"
	summary := toolArgSummary(map[string]any{
		"args": map[string]any{"command": command},
	})
	if strings.Contains(summary, "\n") {
		t.Fatalf("折叠行摘要不该含换行：%q", summary)
	}
}
