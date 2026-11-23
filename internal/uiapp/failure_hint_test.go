package uiapp_test

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
)

// 失败原因落盘后，界面上要优先显示**人话**而不是内部错误码。
//
// 真实的 reason 形如：
//
//	network: Post "https://...": read tcp ...: wsarecv: An existing connection was forcibly closed by the remote host.
//	timeout: 超过 1m30s 没有开始输出，已中止
//
// 折叠行直接摆这些，用户看不懂；failureHint 认得这些关键词，能给出"怎么办"，
// 所以折叠行优先用它。这里通过渲染整行来验证（而不是直接调私有函数）。
func TestFailedRowShowsHumanHint(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()

	// 造一条失败的助手消息，reason 用真实的网络错误
	if _, err := conv.AddMessage(message.Message{
		MessageID: "u1", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("跑一下")},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	entry, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts:     []message.Part{message.NewTextPart("好的")},
	}, conversation.StatusFailed)
	if err != nil {
		t.Fatal(err)
	}
	_ = conv.SetFailureReason("a1", `network: Post "https://tc-paperhub.diezhi.net/anthropic/v1/messages": `+
		`read tcp 172.19.0.1:1313->198.18.0.35:443: wsarecv: An existing connection was forcibly closed by the remote host.`)

	tester := ui.NewTester(app.View, 1000, 700)
	pump(tester, p)
	clickProcessGroup(t, tester)
	pump(tester, p)
	_ = entry

	all := strings.Join(tester.Texts(), "\n")
	// 折叠行应给人话（"连不上服务端"），不是原始错误码
	if !strings.Contains(all, "连不上服务端") {
		t.Fatalf("折叠行应显示人话提示，实际：%v", tester.Texts())
	}
	// 展开后的详情**仍要**保留原始错误（排查时那个才有用）。
	if !strings.Contains(all, "wsarecv") {
		t.Fatalf("展开详情应保留原始错误供排查：%v", tester.Texts())
	}
}