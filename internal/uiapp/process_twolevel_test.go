package uiapp_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/render"
)

// 过程组两级展开：默认折叠 → 一级只显示最后 3 步 → 二级显示全部。
//
// 为什么要分级：一个长回合的过程组动辄几十步，全展开会把时间线撑得很长，
// 往上翻要划很多屏。默认只给最近发生的几步，要看全貌再点一次。
func TestProcessGroupTwoLevelExpand(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()

	// 造 3 组（思考 + 工具调用 + 工具结果），共 **6 步**：
	// 思考算一步、工具调用算一步，tool_result 不算（折进调用行显示）。
	//
	// 口径必须与标题一致，否则同一组里出现两套数字 ——
	// 这正是用户报的问题（标题 6 步、展开全部 13 步）。
	// 工具名带序号，方便断言"哪些步可见、哪些被折叠"。
	var parts []message.Part
	for i := 1; i <= 3; i++ {
		parts = append(parts,
			message.Part{
				Content:   message.TextPart{Text: fmt.Sprintf("想第 %d 步", i)},
				MediaType: render.MediaReasoning,
			},
			message.Part{Content: message.DataPart{Data: map[string]any{
				"id": fmt.Sprintf("c%d", i), "name": fmt.Sprintf("tool%d", i),
				"args": map[string]any{},
			}}, MediaType: render.MediaToolUse},
			message.Part{Content: message.DataPart{Data: map[string]any{
				"id": fmt.Sprintf("c%d", i), "name": fmt.Sprintf("tool%d", i),
				"output": "ok", "is_error": false,
			}}, MediaType: render.MediaToolResult},
		)
	}
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent, Parts: parts,
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 1000, 900)
	pump(tester, p)

	// 1) 默认折叠：看不到任何步骤内容
	if !hasProcessGroup(tester) {
		t.Fatalf("应有折叠的过程组：%v", tester.Texts())
	}
	if tester.HasText("想第 3 步") {
		t.Fatalf("折叠时不该显示步骤内容：%v", tester.Texts())
	}

	// 2) 一级展开：只显示最后 3 步（即第 3 步那组），并给出"上面还有"与"展开全部"
	clickProcessGroup(t, tester)
	pump(tester, p)
	if !tester.HasText("想第 3 步") {
		t.Fatalf("一级展开应显示最后一步：%v", tester.Texts())
	}
	if tester.HasText("想第 1 步") {
		t.Fatalf("一级展开不该显示最早的步骤：%v", tester.Texts())
	}
	if !tester.HasText("⋯ 上面还有 3 步") {
		t.Fatalf("应提示上方还有多少步被折叠：%v", tester.Texts())
	}
	if !tester.HasText("展开全部 6 步") {
		t.Fatalf("应给出展开全部按钮：%v", tester.Texts())
	}

	// 3) 二级展开：全部步骤可见，"展开全部"变成"收起"
	clickExpandAll(t, tester)
	pump(tester, p)
	for _, want := range []string{"想第 1 步", "想第 2 步", "想第 3 步"} {
		if !tester.HasText(want) {
			t.Fatalf("二级展开应显示全部步骤，缺 %q：%v", want, tester.Texts())
		}
	}
	if tester.HasText("⋯ 上面还有") {
		t.Fatalf("全部展开后不该再提示被折叠：%v", tester.Texts())
	}
	if !tester.HasText("收起，只看最后 3 步") {
		t.Fatalf("二级展开后应给出收起按钮：%v", tester.Texts())
	}

	// 4) 收起回一级
	if err := tester.Click("收起，只看最后 3 步"); err != nil {
		t.Fatalf("点不到收起按钮：%v", err)
	}
	pump(tester, p)
	if tester.HasText("想第 1 步") {
		t.Fatalf("收起后不该再显示最早的步骤：%v", tester.Texts())
	}
	if !tester.HasText("展开全部 6 步") {
		t.Fatalf("收起后应回到一级展开：%v", tester.Texts())
	}
}

// 步骤不多于 3 时不显示"展开全部"按钮：那个按钮点下去不会变任何东西。
func TestProcessGroupNoExpandAllWhenShort(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()

	// 2 步（思考 + 工具调用），少于预览步数
	parts := []message.Part{
		{Content: message.TextPart{Text: "想一下"}, MediaType: render.MediaReasoning},
		{Content: message.DataPart{Data: map[string]any{
			"id": "c1", "name": "read", "args": map[string]any{},
		}}, MediaType: render.MediaToolUse},
	}
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent, Parts: parts,
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 1000, 900)
	pump(tester, p)
	clickProcessGroup(t, tester)
	pump(tester, p)

	if !tester.HasText("想一下") && !tester.HasText("思考") {
		t.Fatalf("一级展开应显示步骤：%v", tester.Texts())
	}
	for _, text := range tester.Texts() {
		if strings.HasPrefix(text, "展开全部") {
			t.Fatalf("步骤不多时不该出现展开全部按钮：%v", tester.Texts())
		}
	}
}