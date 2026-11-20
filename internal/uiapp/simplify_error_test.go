package uiapp

import (
	"strings"
	"testing"
)

// 失败信息在折叠态要压成一行要点，不堆绝对路径和 errno 长句。
//
// 真实见到的：读一个不存在的文件，输出占三行 ——
//   "读取失败：internal/host/local.go: open C:\Users\rex\AppData\Local\Temp\
//    TestShot...\001\internal\host\local.go: The system cannot find the path
//    specified."
func TestSimplifyToolError(t *testing.T) {
	raw := "读取失败：internal/host/local.go: open C:\\Users\\rex\\AppData\\Local\\Temp\\TestShot123\\001\\internal\\host\\local.go: The system cannot find the path specified."
	got := simplifyToolError(raw)

	if strings.Contains(got, "\n") {
		t.Fatalf("结果应是单行：%q", got)
	}
	if strings.Contains(got, "AppData") {
		t.Fatalf("不该保留完整绝对路径：%q", got)
	}
	if strings.Contains(got, "The system cannot find") {
		t.Fatalf("不该保留 Windows 的 errno 长句：%q", got)
	}
	if !strings.Contains(got, "不存在") {
		t.Fatalf("应说明文件不存在：%q", got)
	}
	// 末尾两级路径要留着 —— 用户得知道是哪个文件
	if !strings.Contains(got, "local.go") {
		t.Fatalf("应保留文件名：%q", got)
	}
}

// 多行错误只取第一行（折叠态）。
func TestSimplifyToolErrorKeepsFirstLineOnly(t *testing.T) {
	raw := "命令失败：退出码 1\n第二行细节\n第三行细节"
	got := simplifyToolError(raw)
	if got != "命令失败：退出码 1" {
		t.Fatalf("应只保留第一行，实际 %q", got)
	}
}

// 普通的短错误不该被改动（避免过度简化把信息弄丢）。
func TestSimplifyToolErrorLeavesShortErrors(t *testing.T) {
	for _, raw := range []string{
		"未知工具：bash",
		"权限不足",
		"命令超时",
	} {
		if got := simplifyToolError(raw); got != raw {
			t.Fatalf("%q 不该被改写，实际 %q", raw, got)
		}
	}
}

// 非路径文本不能被误伤（宁可少压也不要改坏内容）。
func TestSimplifyToolErrorDoesNotManglePlainText(t *testing.T) {
	raw := "grep 匹配 0 条：func (l *Local) 没找到"
	got := simplifyToolError(raw)
	if got != raw {
		t.Fatalf("普通文本不该被改写，实际 %q", got)
	}
}

// Unix 风格的长路径也要缩短。
func TestSimplifyToolErrorShortensUnixPaths(t *testing.T) {
	raw := "open /home/user/projects/app/internal/host/local.go: No such file or directory"
	got := simplifyToolError(raw)
	if strings.Contains(got, "/home/user") {
		t.Fatalf("不该保留完整路径：%q", got)
	}
	if !strings.Contains(got, "local.go") {
		t.Fatalf("应保留文件名：%q", got)
	}
}