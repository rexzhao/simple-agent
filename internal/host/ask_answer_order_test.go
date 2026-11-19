package host_test

import (
	"strings"
	"testing"

	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/tools"
)

// 多选答案回传：**既勾了选项又写了文字时，勾选必须留下来**。
//
// 回归（用户反馈「ask 工具的多选似乎没有把用户的选择发给模型」）：
// submitAsks 以前先 Toggle 再 SetCustom，而 SetCustom（文字非空时）会
// clear 掉该题已选的选项 —— 于是刚勾好的选项被清空，模型收到的是
// "用户只写了文字"。改成先 SetCustom 再 Toggle 就对了。
func TestSubmitAsksKeepsMultiSelectAlongsideCustomText(t *testing.T) {
	// 多选问题：三个选项
	spec := tools.QuestionSpec{
		ID: "q1", Question: "选哪些？", MultiSelect: true,
		Options: []tools.QuestionOption{{Label: "甲"}, {Label: "乙"}, {Label: "丙"}},
	}

	// 模拟执行端收到的提问组（此时还没应用任何选择）
	group := driver.NewAskGroup("call-1", "ctx-1", []tools.QuestionSpec{spec})

	// 展示端上报：既勾了"甲""丙"，又写了自定义文字。
	//
	// 这正是"先写文字、后勾选项"之后的状态：CustomActive 已被 Toggle 置回
	// false，但展示端仍按自己的界面状态上报两份数据（实际两端都可能出现）。
	answer := host.AskAnswer{
		CallID:   "call-1",
		Selected: map[string][]int{"q1": {0, 2}},
		Custom:   map[string]string{"q1": "我自己写的"},
	}

	// 复刻 submitAsks 里应用答案的那段（先文字、后选项）
	if text := answer.Custom["q1"]; text != "" {
		group.SetCustom(0, text)
	}
	for _, option := range answer.Selected["q1"] {
		group.Toggle(0, option)
	}

	// 勾选必须还在
	if !group.Selected(0, 0) || !group.Selected(0, 2) {
		t.Fatalf("勾选的选项被清掉了（Selected(0,0)=%v Selected(0,2)=%v）",
			group.Selected(0, 0), group.Selected(0, 2))
	}

	out := group.Output(driver.AskSubmit, "")
	if !strings.Contains(out, "甲") || !strings.Contains(out, "丙") {
		t.Fatalf("模型收到的答案里应包含勾选的选项，实际：%s", out)
	}
}

// 反向验证：如果先 Toggle 再 SetCustom，选项会被清掉 —— 说明这个顺序确实
// 是 bug 的成因，不是无害的写法差异。
func TestSubmitAsksWrongOrderLosesSelection(t *testing.T) {
	spec := tools.QuestionSpec{
		ID: "q1", Question: "选哪些？", MultiSelect: true,
		Options: []tools.QuestionOption{{Label: "甲"}, {Label: "乙"}},
	}
	group := driver.NewAskGroup("call-1", "ctx-1", []tools.QuestionSpec{spec})

	// 旧顺序：先勾选，再写文字
	group.Toggle(0, 0)
	group.SetCustom(0, "我自己写的")

	if group.Selected(0, 0) {
		t.Fatal("先 Toggle 再 SetCustom 时选项应当被清掉（这正是旧 bug 的行为）")
	}
	out := group.Output(driver.AskSubmit, "")
	if strings.Contains(out, "甲") {
		t.Fatalf("旧顺序下不该还带着选项，实际：%s", out)
	}
}
