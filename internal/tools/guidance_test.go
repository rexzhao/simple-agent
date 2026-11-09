package tools_test

import (
	"strings"
	"testing"

	"simple-agent/internal/tools"
)

// 指导只从**实际注册的工具**里取：关掉 shell 就不该出现 bash/pwsh 的指导。
//
// 这是对齐 dsh 的「段落随工具可见性消失」性质。之前 main.go 里手写两份
// 提示词（开/关各一段），与 Builtin() 的条件分支是两处独立维护。
func TestGuidanceFollowsRegisteredTools(t *testing.T) {
	withShell := tools.GuidanceSection(tools.Builtin(true))
	withoutShell := tools.GuidanceSection(tools.Builtin(false))

	// 关掉 shell：不能有 bash / pwsh 的指导
	if strings.Contains(withoutShell, "exit code") {
		t.Fatalf("关掉 shell 后不该出现退出码指导：\n%s", withoutShell)
	}
	if strings.Contains(strings.ToLower(withoutShell), "pwsh") {
		t.Fatalf("关掉 shell 后不该提到 pwsh：\n%s", withoutShell)
	}

	// 打开 shell：bash 的指导必须出现
	if !strings.Contains(withShell, "exit code") {
		t.Fatalf("打开 shell 后应包含退出码指导：\n%s", withShell)
	}

	// 文件类工具两种情况下都在
	for _, want := range []string{"read tool", "glob tool", "grep tool"} {
		if !strings.Contains(withoutShell, want) {
			t.Fatalf("关掉 shell 也不该影响文件工具指导（缺 %q）：\n%s", want, withoutShell)
		}
	}
}

// 每个可见工具的指导都要被带上（新增工具时别忘了填 Guidance）。
func TestEveryGuidedToolAppearsInSection(t *testing.T) {
	list := tools.Builtin(true)
	section := tools.GuidanceSection(list)
	for _, tool := range list {
		if tool.Guidance == "" {
			continue
		}
		// Guidance 原文应逐字出现（前面只加了 "- "）
		if !strings.Contains(section, tool.Guidance) {
			t.Fatalf("工具 %s 的指导没进系统提示：\n%q\n---\n%s", tool.Name, tool.Guidance, section)
		}
	}
}

// 没有任何指导时返回空串（调用方据此不加空行）。
func TestGuidanceSectionEmpty(t *testing.T) {
	if got := tools.GuidanceSection(nil); got != "" {
		t.Fatalf("没有工具时应返回空串，实际 %q", got)
	}
	// 只有不带指导的工具
	bare := []*tools.ToolDef{{Name: "x", Description: "d"}}
	if got := tools.GuidanceSection(bare); got != "" {
		t.Fatalf("工具都没有指导时应返回空串，实际 %q", got)
	}
}

// 关键文案逐字取自 dsh 的 tool 段落（改字符串时这条会提醒对齐上游）。
func TestGuidanceWordingMatchesDSH(t *testing.T) {
	want := map[string]string{
		"read":       "Use the read tool — not shell commands like cat — to inspect text files.",
		"glob":       "Use the glob tool — not shell find — to discover files by path pattern.",
		"grep":       "Use the grep tool — not shell grep or rg — to search file contents.",
		"write":      "Read an existing file before overwriting it with write",
		"edit":       "Read a file before editing it",
		"bash":       "Check the [exit code: N] marker on every bash result",
		"read_image": "Use the read_image tool to look at an image file directly",
	}
	byName := map[string]*tools.ToolDef{}
	for _, tool := range tools.Builtin(true) {
		byName[tool.Name] = tool
	}
	for name, text := range want {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("工具 %s 不存在", name)
		}
		if !strings.Contains(tool.Guidance, text) {
			t.Fatalf("%s 的指导应包含 %q，实际 %q", name, text, tool.Guidance)
		}
	}
}
