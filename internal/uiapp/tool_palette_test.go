package uiapp

import (
	"testing"
)

// 只有 bash/pwsh 用终端配色（黑底白字）。
//
// 用户的要求："除了 bash / pwsh 都不应该显示黑底白字"。
// 黑底是终端的语义 —— 命令和它的输出确实在终端里发生；
// 别的工具（edit/write/read）套同款配色只是"看起来像终端"，反而突兀。
func TestOnlyShellToolsUseTerminalPalette(t *testing.T) {
	for _, name := range []string{"bash", "pwsh"} {
		if !isShellTool(name) {
			t.Fatalf("%q 应该用终端配色", name)
		}
	}
	for _, name := range []string{
		"edit", "write", "read", "read_image", "grep", "glob",
		"web_fetch", "web_search", "ask_user_question", "todo_write",
	} {
		if isShellTool(name) {
			t.Fatalf("%q 不该用终端配色", name)
		}
	}
}