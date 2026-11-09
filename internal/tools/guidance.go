package tools

import "strings"

// 系统提示词里与**工具使用指导**相关的那一段。
//
// 对齐 deepseek-harness 的 tool 段落机制（`ctx.systemPrompt.section`）：
// 每个工具包贡献自己的使用指导，**工具不可见时该段自动消失**。
// 我们不做 dsh 那套注册表 + waterfall（本仓只有一份内置工具列表，
// 引入动态贡献只会增加理解成本），但保留最关键的那条性质：
//
//	指导只从**实际注册的工具**里取 → 关掉 shell 就不会再有 bash 指导。
//
// 之前 main.go 里手写了两份（allow_shell 开/关各一段），和 Builtin() 的
// 条件分支是两处独立维护，很容易改一处漏一处。

// Guidance 把一组工具的 Guidance 拼成系统提示里的一段。
//
// 顺序沿用列表顺序（= Builtin 的顺序，也是 schema 的顺序），
// 空指导的工具自然跳过。
func Guidance(list []*ToolDef) string {
	var lines []string
	for _, tool := range list {
		if tool == nil {
			continue
		}
		text := strings.TrimSpace(tool.Guidance)
		if text == "" {
			continue
		}
		lines = append(lines, "- "+text)
	}
	return strings.Join(lines, "\n")
}

// Section 返回**这一段**的完整文本（含标题），空指导时返回空串。
func GuidanceSection(list []*ToolDef) string {
	body := Guidance(list)
	if body == "" {
		return ""
	}
	return "Tool usage:\n" + body
}
