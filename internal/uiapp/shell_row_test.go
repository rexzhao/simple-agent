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

// shell 命令在折叠行里**保留完整内容与换行**。
//
// 用户明确要求："命令的内容应该可以换行，现在不换行根本看不全内容"。
// 多行命令（heredoc、&& 串联、管道）截到一行等于只看到开头。
func TestShellCommandTextKeepsNewlines(t *testing.T) {
	command := "cd /a && \\\ncargo build && \\\nls -la"
	data := map[string]any{
		"args": map[string]any{"command": command},
	}

	got := shellCommandText(data)
	if got != command {
		t.Fatalf("应原样返回（含换行），实际 %q", got)
	}
	if !strings.Contains(got, "\n") {
		t.Fatal("换行不能被压掉")
	}

	// 对比：toolArgSummary 会截到 60 字符且只有一行
	if summary := toolArgSummary(data); strings.Contains(summary, "\n") {
		t.Fatalf("toolArgSummary 本来就该是一行：%q", summary)
	}
}

// 非 shell 工具仍然取截断摘要（它们本来就是短路径/模式）。
func TestShellCommandTextIgnoresOtherTools(t *testing.T) {
	if got := shellCommandText(map[string]any{
		"args": map[string]any{"file_path": "a.go"},
	}); got != "" {
		t.Fatalf("非 shell 工具不该走这条路，实际 %q", got)
	}
}
