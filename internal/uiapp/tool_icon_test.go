package uiapp

import (
	"testing"
)

// 每种工具用不同图标，而不是统一一个 ⚙。
//
// 用户的要求："每种工具用不同图标"。
// 注意 activityIcon 是按**活动分类**（读/搜/写…）取的，而单个工具行需要
// 更细的区分：grep 和 web_search 同属 ActivitySearch，但图标该不一样。
func TestToolIconDistinguishesTools(t *testing.T) {
	// 同一分类下的不同工具要给不同图标
	if toolIcon("grep") == toolIcon("web_search") {
		t.Fatal("grep 与 web_search 同属搜索分类，但图标应不同")
	}
	if toolIcon("read") == toolIcon("web_fetch") {
		t.Fatal("read 与 web_fetch 同属读取分类，但图标应不同")
	}
	// 关键工具之间也不该撞
	pairs := [][2]string{
		{"bash", "read"},
		{"read", "grep"},
		{"grep", "edit"},
		{"edit", "bash"},
	}
	for _, pair := range pairs {
		if toolIcon(pair[0]) == toolIcon(pair[1]) {
			t.Fatalf("%q 与 %q 的图标不该相同", pair[0], pair[1])
		}
	}
	// 未知工具也要有兜底图标（不能返回 nil 导致 panic）
	if toolIcon("some_new_tool") == nil {
		t.Fatal("未知工具应有兜底图标")
	}
}

// bash 和 pwsh 是同一类（都执行命令），共用图标是合理的。
func TestToolIconSharedForSameKind(t *testing.T) {
	if toolIcon("bash") != toolIcon("pwsh") {
		t.Fatal("bash 与 pwsh 都是执行命令，应共用图标")
	}
	if toolIcon("grep") != toolIcon("glob") {
		t.Fatal("grep 与 glob 都是本地搜索，应共用图标")
	}
}