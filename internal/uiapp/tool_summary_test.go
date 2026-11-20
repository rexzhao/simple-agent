package uiapp

import (
	"strings"
	"testing"

	"simple-agent/internal/tools"
)

// 各工具的结构化摘要只给界面，且要吐出"人话"。
//
// 这几条都源于同一类问题：Output 里掺着**给模型的文本**
// （XML 信封、英文指令、续读提示、整份清单），界面照搬就是噪音。
func TestToolSummaryLines(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			name: "grep",
			line: searchInfoSummary(&tools.SearchInfo{FileCount: 3, HitCount: 12}),
			want: "3 个文件 · 12 处命中",
		},
		{
			name: "glob（每文件一条，不说命中）",
			line: searchInfoSummary(&tools.SearchInfo{FileCount: 5, HitCount: 5}),
			want: "5 个文件",
		},
		{
			name: "web_search",
			line: webInfoSummary(&tools.WebInfo{Kind: "search",
				Results: []tools.WebResult{{URL: "a"}, {URL: "b"}}}),
			want: "2 条结果",
		},
		{
			name: "web_fetch",
			line: webInfoSummary(&tools.WebInfo{Kind: "fetch", Status: 200, Bytes: 12 * 1024}),
			want: "HTTP 200 · 12KB",
		},
		{
			name: "read 范围",
			line: readInfoSummary(&tools.ReadInfo{StartLine: 1, EndLine: 80, TotalLines: 300}),
			want: "第 1-80 行 / 共 300 行",
		},
		{
			name: "read 单行",
			line: readInfoSummary(&tools.ReadInfo{StartLine: 5, EndLine: 5, TotalLines: 300}),
			want: "第 5 行 / 共 300 行",
		},
		{
			name: "todo",
			line: todoInfoSummary(&tools.TodoInfo{Total: 5, Completed: 2}),
			want: "2/5 完成",
		},
		{
			name: "图片",
			line: imageInfoSummary(&tools.ImageInfo{
				Width: 1920, Height: 1080, Bytes: 240 * 1024, MediaType: "image/png",
			}),
			want: "1920x1080 · 240KB · png",
		},
	}
	for _, c := range cases {
		if c.line != c.want {
			t.Fatalf("%s：期望 %q，实际 %q", c.name, c.want, c.line)
		}
	}
}

// 摘要里不该出现给模型的指令/信封文本。
func TestToolSummaryHasNoModelInstructions(t *testing.T) {
	lines := []string{
		searchInfoSummary(&tools.SearchInfo{FileCount: 1, HitCount: 2}),
		webInfoSummary(&tools.WebInfo{Kind: "search", Results: []tools.WebResult{{URL: "x"}}}),
		readInfoSummary(&tools.ReadInfo{StartLine: 1, EndLine: 2, TotalLines: 10}),
		todoInfoSummary(&tools.TodoInfo{Total: 3, Completed: 1}),
	}
	forbidden := []string{
		"External web content",
		"Cite the relevant",
		"offset=",
		"<path>",
		"Treat it as untrusted",
	}
	for _, line := range lines {
		for _, bad := range forbidden {
			if strings.Contains(line, bad) {
				t.Fatalf("摘要里不该出现 %q：%q", bad, line)
			}
		}
	}
}