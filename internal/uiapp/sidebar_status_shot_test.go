package uiapp_test

import (
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// 把侧栏画出来供人工核对状态点（不是断言，是取证）。
//
// 用 SIMPLE_AGENT_SHOT 指定输出路径；不设就跳过。
//
// 为什么要有它：状态点是纯视觉的东西（符号/颜色/与标题的对齐），
// 文字断言只能证明"渲染了 ●"，证明不了"看起来是否协调"。
func TestSidebarStatusShot(t *testing.T) {
	path := os.Getenv("SIMPLE_AGENT_SHOT")
	if path == "" {
		t.Skip("设置 SIMPLE_AGENT_SHOT=<png 路径> 才会输出截图")
	}

	app, rt, p, tester := newLayoutApp(t, -1, 1200, 700)

	idle := app.Current()
	idle.SetWorkspaceForTest("/work/demo")
	idle.Title = "空闲的会话"

	builder := app.NewSession()
	builder.SetWorkspaceForTest("/work/demo")
	builder.Title = "正在生成的会话"

	waiter := app.NewSession()
	waiter.SetWorkspaceForTest("/work/demo")
	waiter.Title = "等回答的会话"
	pump(tester, p)

	// 卡住 OpenStream：界面会停在 BusyStreaming，状态点该出现。
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	rt.mu.Lock()
	rt.openGate = gate
	rt.openEntered = entered
	rt.mu.Unlock()
	defer func() {
		rt.mu.Lock()
		rt.openGate = nil
		rt.openEntered = nil
		rt.mu.Unlock()
		close(gate)
	}()

	// 切到「正在生成的会话」并发一条消息
	if err := tester.Click(builder.Title); err != nil {
		t.Fatalf("点不到会话：%v（界面：%v）", err, tester.Texts())
	}
	pump(tester, p)
	// 必须先点输入框拿到焦点，Type 才会进到输入框里
	clickComposer(t, tester)
	tester.Type("开始生成")
	tester.Frame()
	if err := tester.Click(locSend); err != nil {
		t.Fatalf("点不到发送：%v（界面：%v）", err, tester.Texts())
	}

	// 轮询等 OpenStream 真的被调用（发送是异步的）
	enteredOK := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		pump(tester, p)
		select {
		case <-entered:
			enteredOK = true
		default:
		}
		if enteredOK {
			break
		}
	}
	if !enteredOK {
		t.Fatalf("OpenStream 没被调用，发送可能失败了：%v", tester.Texts())
	}
	// 再渲染几帧让状态传播到侧栏
	for i := 0; i < 3; i++ {
		pump(tester, p)
	}
	if !tester.HasText("●") {
		t.Fatalf("流式期间侧栏应有 ●：%v", tester.Texts())
	}

	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, tester.Image()); err != nil {
		t.Fatal(err)
	}
	t.Logf("截图已输出：%s", path)
	_ = ui.KeyEscape
}