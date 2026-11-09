package uiapp_test

import (
	"image/png"
	"os"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
)

// 把用户截图里的那种列表渲染出来存成 png，便于人眼确认编号位置。
// 只在设置 SIMPLE_AGENT_SHOT 时写文件，平时就是一条普通断言。
func TestListScreenshot(t *testing.T) {
	app, _, p := newApp(t)
	long := "文档状态行过时 —— docs/ui-redesign-plan.md 第 3 行还写着「P1 起未开始」，实际 P4 都完成并合并了。改一行的事。"
	text := "不多了，只有三类：\n\n" +
		"1. " + long + "\n\n" +
		"2. **P4 的功能本身是空的** —— 代码里只有一处 TODO，右侧指标按决策只预留了位置，采集方案留档，属于以后开发任务。\n\n" +
		"3. 等真实试用反馈 —— .pi/mailbox 显示上一轮已经把完成情况转给用户，也就是说没有待办中的开发任务。\n"
	msg := message.Message{MessageID: "shot-1", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart(text),
	}}
	if _, err := app.Current().Conversation().AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(app.View, 720, 560)
	pump(tester, p)
	pump(tester, p)

	if path := os.Getenv("SIMPLE_AGENT_SHOT"); path != "" {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := png.Encode(file, tester.Image()); err != nil {
			t.Fatal(err)
		}
		t.Logf("已写出 %s", path)
	}

	for _, want := range []string{"1.", "2.", "3."} {
		if !tester.HasText(want) {
			t.Fatalf("缺少编号 %q：%v", want, tester.Texts())
		}
	}
}
