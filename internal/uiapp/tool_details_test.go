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

// 展开区不该出现"实现细节"参数：
//   - bash 不要 "command:" 前缀（用户明确要求）
//   - edit 不要 file_path/old_string/new_string（内容和 diff 已经画了）
//   - write 不要 content（整篇内容已经显示）
func TestExpandedArgsHideNoise(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()

	diff := tools.BuildLineDiff("app.go", "a\nb\n", "a\nc\n", tools.LineDiffOptions{})
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			// bash：workdir 要显示；timeoutMs 用**非默认值**才显示
			// （默认 120s 满屏都是，是噪音）
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "bash",
				"args": map[string]any{
					"command":   "go build ./...",
					"workdir":   "internal",
					"timeoutMs": 300000,
				},
			}}, MediaType: render.MediaToolUse},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "bash", "is_error": false,
				"output": "exit_code=0 elapsed=100ms cwd=.\ndone",
				"exec":   &tools.ExecInfo{ExitCode: 0, ElapsedMS: 100},
			}}, MediaType: render.MediaToolResult},
			// edit：old_string/new_string 不该出现在展开区
			{Content: message.DataPart{Data: map[string]any{
				"id": "c2", "name": "edit",
				"args": map[string]any{
					"file_path": "app.go", "old_string": "旧", "new_string": "新",
				},
			}}, MediaType: render.MediaToolUse},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c2", "name": "edit", "is_error": false,
				"output": "已编辑 app.go（+1 -1）", "diff": diff,
			}}, MediaType: render.MediaToolResult},
		},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 1000, 700)
	pump(tester, p)
	clickProcessGroup(t, tester)
	pump(tester, p)
	// 展开两个
	for _, want := range []string{"go build", "app.go"} {
		for _, text := range tester.Texts() {
			if strings.Contains(text, want) {
				_ = tester.Click(text)
				break
			}
		}
		pump(tester, p)
	}

	all := strings.Join(tester.Texts(), "\n")
	if strings.Contains(all, "command:") {
		t.Fatalf("bash 不该有 command: 前缀：%v", tester.Texts())
	}
	if strings.Contains(all, "old_string") || strings.Contains(all, "file_path") {
		t.Fatalf("edit 不该罗列 old_string/file_path：%v", tester.Texts())
	}
	// workdir / timeout 显示在行内（折叠行里），不展开也在
	if !strings.Contains(all, "internal") {
		t.Fatalf("应显示 workdir：%v", tester.Texts())
	}
	// **带"限时"字样**：timeoutMs 是超时上限，不是实际耗时。
	// 以前直接 formatElapsed 成 "5m00s"，用户读成"跑了 5 分钟"。
	if !strings.Contains(all, "限时 5m00s") {
		t.Fatalf("应显示带标注的限时：%v", tester.Texts())
	}
}

// edit 的 +N 要紧跟在文件名后面，而不是被推到行尾。
func TestEditPlusMinusFollowsFileName(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()

	diff := tools.BuildLineDiff("app.go", "a\nb\n", "a\nc\n", tools.LineDiffOptions{})
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "edit",
				"args": map[string]any{"file_path": "app.go"},
			}}, MediaType: render.MediaToolUse},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "edit", "is_error": false,
				"output": "已编辑 app.go（+1 -1）", "diff": diff,
			}}, MediaType: render.MediaToolResult},
		},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 1000, 700)
	pump(tester, p)
	clickProcessGroup(t, tester)
	pump(tester, p)

	// 界面把它们渲染成相邻的独立元素（"app.go" / "+1" / "-1"），
	// 所以按"整行拼接后紧邻"来断言。
	joined := strings.Join(tester.Texts(), " ")
	if !strings.Contains(joined, "app.go +1 -1") {
		t.Fatalf("+N -N 应紧跟文件名（期望 \"app.go +1 -1\"）：%v", tester.Texts())
	}
}
// bash 的 description 要显示在行内，和命令摘要**并存**。
//
// 用户的要求："如果有 description 的话，description 紧跟着 bash 显示，
// 方便了解干什么的，bash 行的命令内容如果太长可以截断"。
func TestBashRowShowsDescriptionAndClippedCommand(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()

	longCommand := "go test ./internal/uiapp/ -run 'TestProcessGroup|TestBashRow' -count=1 -v 2>&1 | grep -E 'RUN|PASS|FAIL'"
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "bash",
				"args": map[string]any{
					"command":     longCommand,
					"description": "跑过程组相关测试",
				},
			}}, MediaType: render.MediaToolUse},
		},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)

	all := strings.Join(tester.Texts(), "\n")
	// 描述要出现（它在 args 里，容易读成 data 顶层而静默丢失）
	if !strings.Contains(all, "跑过程组相关测试") {
		t.Fatalf("应显示 description：%v", tester.Texts())
	}
	// **命令完整显示、不截断**：单行命令以前被截到 36/56 字符，
	// 现在 shell 命令保留完整内容（多行的还能换行到 4 行）。
	// 用户明确要求"命令的内容应该可以换行，现在不换行根本看不全内容"。
	if !strings.Contains(all, longCommand) {
		t.Fatalf("shell 命令应完整显示（不截断）：%v", tester.Texts())
	}
	// 展开后同样能看到完整命令（展开区一直是完整的，这里顺带确认没回归）
	tester.Frame()
	for _, text := range tester.Texts() {
		if strings.HasPrefix(text, "go test ./internal/uiapp/") {
			if err := tester.Click(text); err != nil {
				break // 已经完整显示时可能点不到单独的元素，不算失败
			}
			break
		}
	}
	pump(tester, p)
	if !strings.Contains(strings.Join(tester.Texts(), "\n"), longCommand) {
		t.Fatalf("展开后应看到完整命令：%v", tester.Texts())
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