package uiapp_test

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/render"
	"simple-agent/internal/tools"
)

// bash 行展开后：**命令 → 元信息 → 输出**三段分开，且命令在运行前就能看到。
//
// 用户的要求：
//  - "bash 或者 pwsh 应该在运行之前就可以展开，看到要运行的命令"
//  - "有结果之后，exit_code elapsed 之类信息和结果分开显示，方便查看"
func TestBashRowShowsCommandAndSeparateMeta(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()

	command := "go test ./internal/uiapp/ -run TestProcessGroup -count=1"
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "bash", "args": map[string]any{"command": command},
			}}, MediaType: render.MediaToolUse},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "bash", "is_error": false,
				"output": "exit_code=0 elapsed=1876ms cwd=.\nok  simple-agent/internal/uiapp  0.315s",
				"exec":   &tools.ExecInfo{ExitCode: 0, ElapsedMS: 1876, Cwd: "internal/uiapp"},
			}}, MediaType: render.MediaToolResult},
		},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 1000, 700)
	pump(tester, p)
	clickProcessGroup(t, tester)
	pump(tester, p)
	// 点开 bash 行
	for _, text := range tester.Texts() {
		if strings.Contains(text, "go test") {
			if err := tester.Click(text); err != nil {
				t.Fatalf("点不到 bash 行 %q：%v", text, err)
			}
			break
		}
	}
	pump(tester, p)

	all := strings.Join(tester.Texts(), "\n")
	// ① 命令全文可见
	if !strings.Contains(all, command) {
		t.Fatalf("展开后应看到完整命令：%v", tester.Texts())
	}
	// ② 元信息：exit 0 与耗时
	if !strings.Contains(all, "exit 0") {
		t.Fatalf("应显示退出码：%v", tester.Texts())
	}
	if !strings.Contains(all, "1.88s") {
		t.Fatalf("应显示耗时：%v", tester.Texts())
	}
	// ③ 输出正文**不含**那行 exit_code=... （和元信息重复）
	if strings.Contains(all, "exit_code=0 elapsed=") {
		t.Fatalf("输出里不该重复 exit_code 行：%v", tester.Texts())
	}
	// ④ 真正的输出仍在
	if !strings.Contains(all, "simple-agent/internal/uiapp") {
		t.Fatalf("应显示命令输出：%v", tester.Texts())
	}
}

// 还没有结果（命令正在跑）时，也能展开看到要执行的命令。
func TestBashRowExpandableBeforeResult(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()

	command := "go build ./... && go vet ./internal/uiapp/"
	// 只有 tool_use，**没有 tool_result**：模拟"正在运行"
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "bash", "args": map[string]any{"command": command},
			}}, MediaType: render.MediaToolUse},
		},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 1000, 700)
	pump(tester, p)
	// 只有一个 part 时**内联显示**（不组成过程组），不需要先展开组。

	// 折叠态只有截断摘要；点开后要能看全文
	found := false
	for _, text := range tester.Texts() {
		if strings.Contains(text, "go build") {
			found = true
			if err := tester.Click(text); err != nil {
				t.Fatalf("无结果时也该能展开：%v", err)
			}
			break
		}
	}
	if !found {
		t.Fatalf("应显示命令摘要：%v", tester.Texts())
	}
	pump(tester, p)
	if !strings.Contains(strings.Join(tester.Texts(), "\n"), command) {
		t.Fatalf("无结果时展开应看到完整命令：%v", tester.Texts())
	}
}

// edit 行**折叠态**就显示 +N -N，不用展开也能看出改了多少。
//
// 用户的要求："edit 有结果之后，在 edit 本身那一行显示 +/- 数量"。
func TestEditRowShowsPlusMinusInline(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()

	diff := tools.BuildLineDiff("app.go",
		"a\nb\nc\nd\n", "a\nX\nY\nZ\nd\n", tools.LineDiffOptions{})
	if diff.Added != 3 || diff.Removed != 2 {
		t.Fatalf("前提不成立：应 +3 -2，实际 +%d -%d", diff.Added, diff.Removed)
	}
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "edit",
				"args": map[string]any{"file_path": "app.go"},
			}}, MediaType: render.MediaToolUse},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "edit", "is_error": false,
				"output": "已编辑 app.go（+3 -2）", "diff": diff,
			}}, MediaType: render.MediaToolResult},
		},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 1000, 700)
	pump(tester, p)
	clickProcessGroup(t, tester)
	pump(tester, p)

	// **不展开**就要看到 +3 和 -2
	all := strings.Join(tester.Texts(), " ")
	if !strings.Contains(all, "+3") {
		t.Fatalf("折叠行应显示 +3：%v", tester.Texts())
	}
	if !strings.Contains(all, "-2") {
		t.Fatalf("折叠行应显示 -2：%v", tester.Texts())
	}
}

// write 展开后能看到写入的内容。
func TestWriteRowShowsContent(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()

	content := "line one\nline two\nline three"
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "write",
				"args": map[string]any{"file_path": "notes.txt"},
			}}, MediaType: render.MediaToolUse},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "write", "is_error": false,
				"output": "已创建 notes.txt（30 字节，3 行）",
				"diff": &tools.FileDiff{
					Path: "notes.txt", Content: content, Verb: "已创建",
				},
			}}, MediaType: render.MediaToolResult},
		},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 1000, 700)
	pump(tester, p)
	clickProcessGroup(t, tester)
	pump(tester, p)
	for _, text := range tester.Texts() {
		if strings.Contains(text, "notes.txt") {
			if err := tester.Click(text); err != nil {
				t.Fatalf("点不到 write 行：%v", err)
			}
			break
		}
	}
	pump(tester, p)

	all := strings.Join(tester.Texts(), "\n")
	if !strings.Contains(all, "line two") {
		t.Fatalf("展开后应看到写入的内容：%v", tester.Texts())
	}
}