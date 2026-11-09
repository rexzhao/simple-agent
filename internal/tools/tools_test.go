package tools_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"simple-agent/internal/tools"
)

// newCtx 建一个临时工作区（测试只走公开入口：Builtin + Run）。
func newCtx(t *testing.T, allowShell bool) *tools.ToolContext {
	t.Helper()
	dir := t.TempDir()
	return tools.NewToolContext(dir, allowShell)
}

func runTool(t *testing.T, tc *tools.ToolContext, name string, args tools.Args) (tools.ToolReturn, error) {
	t.Helper()
	tool, ok := tools.Find(tools.Builtin(tc.AllowShell), name)
	if !ok {
		t.Fatalf("工具未注册：%s", name)
	}
	return tool.Run(context.Background(), args, tc)
}

func mustOutput(t *testing.T, tc *tools.ToolContext, name string, args tools.Args) string {
	t.Helper()
	result, err := runTool(t, tc, name, args)
	if err != nil {
		t.Fatalf("%s 应成功：%v", name, err)
	}
	if result.IsAsk() {
		t.Fatalf("%s 不应提问", name)
	}
	return result.Output
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuiltinToolSet(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range tools.Builtin(false) {
		names[tool.Name] = true
		if tool.Description == "" || tool.Parameters == nil || tool.Run == nil {
			t.Fatalf("工具定义不完整：%s", tool.Name)
		}
	}
	for _, want := range []string{"read", "write", "edit", "glob", "grep", "todo_write", "working_directory", "ask_user_question"} {
		if !names[want] {
			t.Fatalf("缺少工具 %s", want)
		}
	}
	if names["bash"] {
		t.Fatal("未开启 shell 时不应注册 bash")
	}
	if withShell := tools.Builtin(true); len(withShell) <= len(tools.Builtin(false)) {
		t.Fatal("开启 shell 后应多出 bash（以及 Windows 的 pwsh）")
	}
}

func TestReadReturnsLineNumbersAndRespectsOffset(t *testing.T) {
	tc := newCtx(t, false)
	writeFile(t, tc.Workspace(), "a.txt", "one\ntwo\nthree\nfour\n")

	out := mustOutput(t, tc, "read", tools.Args{"file_path": "a.txt"})
	if !strings.Contains(out, "     1→one") {
		t.Fatalf("应带行号：%q", out)
	}
	if !strings.Contains(out, "     4→four") {
		t.Fatalf("应读到第 4 行：%q", out)
	}

	out = mustOutput(t, tc, "read", tools.Args{"file_path": "a.txt", "offset": 2, "limit": 2})
	if strings.Contains(out, "one") {
		t.Fatalf("offset=2 应跳过第一行：%q", out)
	}
	if !strings.Contains(out, "     2→two") || !strings.Contains(out, "     3→three") {
		t.Fatalf("范围不正确：%q", out)
	}
	if !strings.Contains(out, "offset=4") {
		t.Fatalf("应提示继续读取位置：%q", out)
	}
}

func TestReadOutOfRange(t *testing.T) {
	tc := newCtx(t, false)
	writeFile(t, tc.Workspace(), "a.txt", "one\n")
	out := mustOutput(t, tc, "read", tools.Args{"file_path": "a.txt", "offset": 99})
	if !strings.Contains(out, "超出范围") {
		t.Fatalf("应提示超出范围：%q", out)
	}
}

func TestWriteThenEditRequiresUniqueMatch(t *testing.T) {
	tc := newCtx(t, false)

	mustOutput(t, tc, "write", tools.Args{"file_path": "sub/n.txt", "content": "same\nsame\nother"})
	data, err := os.ReadFile(filepath.Join(tc.Workspace(), "sub", "n.txt"))
	if err != nil {
		t.Fatalf("应自动创建父目录：%v", err)
	}
	if string(data) != "same\nsame\nother" {
		t.Fatalf("内容不一致：%q", data)
	}

	// 多处命中且未开 replace_all → 报错
	_, err = runTool(t, tc, "edit", tools.Args{"file_path": "sub/n.txt", "old_string": "same", "new_string": "x"})
	if err == nil || !strings.Contains(err.Error(), "出现 2 次") {
		t.Fatalf("应拒绝多处命中：%v", err)
	}

	// replace_all
	mustOutput(t, tc, "edit", tools.Args{
		"file_path": "sub/n.txt", "old_string": "same", "new_string": "x", "replace_all": true,
	})
	data, _ = os.ReadFile(filepath.Join(tc.Workspace(), "sub", "n.txt"))
	if string(data) != "x\nx\nother" {
		t.Fatalf("replace_all 未生效：%q", data)
	}

	// 唯一命中 + 删除（new_string 为空）
	mustOutput(t, tc, "edit", tools.Args{"file_path": "sub/n.txt", "old_string": "other", "new_string": ""})
	data, _ = os.ReadFile(filepath.Join(tc.Workspace(), "sub", "n.txt"))
	if string(data) != "x\nx\n" {
		t.Fatalf("删除未生效：%q", data)
	}
}

func TestEditRejectsEmptyAndIdentical(t *testing.T) {
	tc := newCtx(t, false)
	writeFile(t, tc.Workspace(), "a.txt", "hello")

	cases := []struct {
		name string
		args tools.Args
		want string
	}{
		{"空 old_string", tools.Args{"file_path": "a.txt", "old_string": "", "new_string": "x"}, "不能为空"},
		{"完全相同", tools.Args{"file_path": "a.txt", "old_string": "hello", "new_string": "hello"}, "无需编辑"},
		{"找不到", tools.Args{"file_path": "a.txt", "old_string": "nope", "new_string": "x"}, "未在"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := runTool(t, tc, "edit", c.args)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("期望错误含 %q，实际 %v", c.want, err)
			}
		})
	}
}

func TestWorkingDirectorySwitchesAndStaysBounded(t *testing.T) {
	tc := newCtx(t, false)
	writeFile(t, tc.Workspace(), "sub/inner.txt", "hi")

	mustOutput(t, tc, "working_directory", tools.Args{"cd": "sub"})
	if got := tc.Cwd(); filepath.Base(got) != "sub" {
		t.Fatalf("cwd 未切换：%s", got)
	}
	// 切目录后相对路径基于新 cwd
	out := mustOutput(t, tc, "read", tools.Args{"file_path": "inner.txt"})
	if !strings.Contains(out, "hi") {
		t.Fatalf("相对路径应基于新 cwd：%q", out)
	}

	// 仍不允许逃出工作区
	_, err := runTool(t, tc, "working_directory", tools.Args{"cd": "../.."})
	if err == nil || !strings.Contains(err.Error(), "越界") {
		t.Fatalf("应拒绝越界：%v", err)
	}

	// 不带参数读取当前目录
	out = mustOutput(t, tc, "working_directory", tools.Args{})
	if !strings.Contains(out, "sub") {
		t.Fatalf("应返回当前目录：%q", out)
	}
}

func TestResolveRejectsEscapes(t *testing.T) {
	tc := newCtx(t, false)
	cases := []struct {
		raw  string
		want string
	}{
		{"../x", "越界"},
		{"a/../../x", "越界"},
		{filepath.Join(tc.Workspace(), "x"), "绝对路径"},
	}
	for _, c := range cases {
		_, err := runTool(t, tc, "read", tools.Args{"file_path": c.raw})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%q 期望含 %q，实际 %v", c.raw, c.want, err)
		}
	}

	// 未创建的新路径不受影响（写新文件场景）
	mustOutput(t, tc, "write", tools.Args{"file_path": "new/dir/file.txt", "content": "ok"})
}

// 回归（opus）：工作区里的 symlink/junction 会把读写带到工作区外。
// Windows 上 junction（mklink /J）普通用户即可创建，不需要开发者模式。
func TestSymlinkEscapeRejected(t *testing.T) {
	tc := newCtx(t, false)
	outside := t.TempDir()
	link := filepath.Join(tc.Workspace(), "escape")
	if !createDirLink(link, outside) {
		t.Skip("当前环境无法创建目录链接（符号链接/junction）")
	}
	defer removeDirLink(link)

	_, err := runTool(t, tc, "read", tools.Args{"file_path": "escape/secret.txt"})
	if err == nil || !strings.Contains(err.Error(), "越界") {
		t.Fatalf("经链接逃出工作区应被拒绝：%v", err)
	}
	// 回归（opus P2-1）：规范化路径不能带 `\\?\` 扩展长度前缀（之前 TrimPrefix 写成单反斜杠，
	// 前缀一直没被去掉；只是因为两边都带才凑巧比较正确，错误信息里能看到）。
	if strings.Contains(err.Error(), `\\?\`) || strings.Contains(err.Error(), `\?\`) {
		t.Fatalf("错误信息里不应出现扩展长度前缀：%v", err)
	}
	// 链接目录本身也访问不了
	if _, err := runTool(t, tc, "glob", tools.Args{"pattern": "*", "path": "escape"}); err == nil {
		t.Fatal("经链接访问目录也应被拒绝")
	}
}

// 回归（opus）：悬空链接会被 os.Stat 判为「不存在」，
// 从而被当成尚未创建的段按词法拼回，write 会顺着链接在工作区外建文件。
//
// Windows 上文件符号链接需要开发模式，所以用「junction 建好后删掉目标」制造悬空，
// 这条路径同样必须被拒绝。
func TestDanglingLinkRejected(t *testing.T) {
	tc := newCtx(t, false)
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tc.Workspace(), "dangling")
	if !createDirLink(link, outside) {
		t.Skip("当前环境无法创建目录链接")
	}
	defer removeDirLink(link)

	// 删掉目标 → 链接悬空
	if err := os.RemoveAll(outside); err != nil {
		t.Fatal(err)
	}

	_, err := runTool(t, tc, "write", tools.Args{"file_path": "dangling/leak.txt", "content": "leak"})
	if err == nil {
		t.Fatal("悬空链接必须被拒绝，否则 write 会在工作区外建文件")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "leak.txt")); statErr == nil {
		t.Fatalf("工作区外不应被创建文件：%s", outside)
	}
}

func TestGlobFindsFilesOnlyAndRecurses(t *testing.T) {
	tc := newCtx(t, false)
	writeFile(t, tc.Workspace(), "src/main.rs", "fn main() {}")
	writeFile(t, tc.Workspace(), "src/nested/mod.rs", "pub fn helper() {}")
	writeFile(t, tc.Workspace(), "README.md", "docs")
	writeFile(t, tc.Workspace(), "node_modules/skip.js", "nope")

	out := mustOutput(t, tc, "glob", tools.Args{"pattern": "**/*.rs"})
	if !strings.Contains(out, "src/main.rs") || !strings.Contains(out, "src/nested/mod.rs") {
		t.Fatalf("应递归匹配：%q", out)
	}
	if strings.Contains(out, "README.md") {
		t.Fatalf("不应匹配 md：%q", out)
	}
	if strings.Contains(out, "node_modules") {
		t.Fatalf("应跳过 node_modules：%q", out)
	}

	out = mustOutput(t, tc, "glob", tools.Args{"pattern": "*.md"})
	if !strings.Contains(out, "README.md") {
		t.Fatalf("裸文件名模式应匹配根目录：%q", out)
	}

	out = mustOutput(t, tc, "glob", tools.Args{"pattern": "*.nope"})
	if !strings.Contains(out, "没有文件匹配") {
		t.Fatalf("无命中应给出提示：%q", out)
	}
}

func TestGrepGroupsByFileWithLineNumbers(t *testing.T) {
	tc := newCtx(t, false)
	writeFile(t, tc.Workspace(), "src/main.rs", "fn main() {\n    // needle\n}\n")
	writeFile(t, tc.Workspace(), "README.md", "needle in docs\n")

	out := mustOutput(t, tc, "grep", tools.Args{"pattern": "needle"})
	if !strings.Contains(out, "src/main.rs") || !strings.Contains(out, "README.md") {
		t.Fatalf("应按文件分组：%q", out)
	}
	if !strings.Contains(out, "     2: ") {
		t.Fatalf("应带行号：%q", out)
	}

	out = mustOutput(t, tc, "grep", tools.Args{"pattern": "needle", "include": "*.md"})
	if strings.Contains(out, "main.rs") {
		t.Fatalf("include 应排除 .rs：%q", out)
	}

	// 正则而非字面量
	out = mustOutput(t, tc, "grep", tools.Args{"pattern": `fn \w+\(`})
	if !strings.Contains(out, "main.rs") {
		t.Fatalf("应支持正则：%q", out)
	}

	// 非法正则
	_, err := runTool(t, tc, "grep", tools.Args{"pattern": "([unclosed"})
	if err == nil || !strings.Contains(err.Error(), "正则非法") {
		t.Fatalf("应报正则错误：%v", err)
	}

	// 二进制文件跳过
	if err := os.WriteFile(filepath.Join(tc.Workspace(), "bin.dat"), []byte{0, 1, 2, 'n', 'e', 'e', 'd', 'l', 'e'}, 0o644); err != nil {
		t.Fatal(err)
	}
	out = mustOutput(t, tc, "grep", tools.Args{"pattern": "needle"})
	if strings.Contains(out, "bin.dat") {
		t.Fatalf("二进制文件应跳过：%q", out)
	}
}

func TestTodoWriteReplacesList(t *testing.T) {
	tc := newCtx(t, false)
	out := mustOutput(t, tc, "todo_write", tools.Args{"todos": []any{
		map[string]any{"content": "读代码", "status": "completed"},
		map[string]any{"content": "改实现", "status": "in_progress"},
	}})
	if !strings.Contains(out, "1/2 完成") || !strings.Contains(out, "[x] 读代码") || !strings.Contains(out, "[~] 改实现") {
		t.Fatalf("输出不一致：%q", out)
	}
	if len(tc.Todos()) != 2 {
		t.Fatalf("状态未更新：%#v", tc.Todos())
	}

	// 覆盖式：旧列表被替换
	mustOutput(t, tc, "todo_write", tools.Args{"todos": []any{
		map[string]any{"content": "新任务", "status": "pending"},
	}})
	if todos := tc.Todos(); len(todos) != 1 || todos[0].Content != "新任务" {
		t.Fatalf("应覆盖旧列表：%#v", todos)
	}

	// 清空
	out = mustOutput(t, tc, "todo_write", tools.Args{"todos": []any{}})
	if !strings.Contains(out, "已清空") {
		t.Fatalf("应提示清空：%q", out)
	}
}

func TestTodoWriteValidates(t *testing.T) {
	tc := newCtx(t, false)
	cases := []struct {
		name string
		args tools.Args
		want string
	}{
		{"状态非法", tools.Args{"todos": []any{map[string]any{"content": "x", "status": "done"}}}, "status 非法"},
		{"内容为空", tools.Args{"todos": []any{map[string]any{"content": "  ", "status": "pending"}}}, "不能为空"},
		{"不是数组", tools.Args{"todos": "x"}, "必须是数组"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := runTool(t, tc, "todo_write", c.args)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("期望含 %q，实际 %v", c.want, err)
			}
		})
	}

	// 多个 in_progress 直接拒绝（对齐 dsh 的 toTodoList）
	_, err := runTool(t, tc, "todo_write", tools.Args{"todos": []any{
		map[string]any{"content": "a", "status": "in_progress"},
		map[string]any{"content": "b", "status": "in_progress"},
	}})
	if err == nil || !strings.Contains(err.Error(), "最多只能有一个") {
		t.Fatalf("多个 in_progress 应被拒绝，实际 %v", err)
	}
	// 被拒绝时清单不变（不能留下半套状态）
	if todos := tc.Todos(); len(todos) != 0 {
		t.Fatalf("校验失败不应改动清单：%#v", todos)
	}

	// 重复 content 也拒绝（dsh 同样拒绝：日志里的快照必须与模型以为写的一致）
	_, err = runTool(t, tc, "todo_write", tools.Args{"todos": []any{
		map[string]any{"content": "同一件事", "status": "pending"},
		map[string]any{"content": "同一件事", "status": "completed"},
	}})
	if err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复 content 应被拒绝，实际 %v", err)
	}
}

func TestAskUserQuestionParsesSpec(t *testing.T) {
	tc := newCtx(t, false)
	result, err := runTool(t, tc, "ask_user_question", tools.Args{"questions": []any{
		map[string]any{
			"id":       "q1",
			"question": "用哪种方案？",
			"header":   "选择",
			"options": []any{
				map[string]any{"label": "A (Recommended)", "description": "更快"},
				map[string]any{"label": "B"},
			},
			"multiSelect": true,
		},
	}})
	if err != nil {
		t.Fatalf("应成功：%v", err)
	}
	if !result.IsAsk() || len(result.Ask) != 1 {
		t.Fatalf("应返回 1 个问题：%#v", result)
	}
	q := result.Ask[0]
	if q.ID != "q1" || q.Header != "选择" || len(q.Options) != 2 || q.Options[0].Description != "更快" || !q.MultiSelect {
		t.Fatalf("问题解析不一致：%#v", q)
	}
	if result.Output != "" {
		t.Fatalf("提问结果不应有输出：%q", result.Output)
	}
}

func TestAskUserQuestionValidates(t *testing.T) {
	tc := newCtx(t, false)
	cases := []struct {
		name string
		args tools.Args
		want string
	}{
		{"重复 id", tools.Args{"questions": []any{
			map[string]any{"id": "a", "question": "x"},
			map[string]any{"id": "a", "question": "y"},
		}}, "重复"},
		{"空列表", tools.Args{"questions": []any{}}, "不能为空"},
		{"缺 question", tools.Args{"questions": []any{map[string]any{"id": "a"}}}, "question 不能为空"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := runTool(t, tc, "ask_user_question", c.args)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("期望含 %q，实际 %v", c.want, err)
			}
		})
	}
}

func TestAnswersToOutputShape(t *testing.T) {
	out := tools.AnswersToOutput([]tools.QuestionAnswer{
		{ID: "q1", Selected: []string{"A"}},
		{ID: "q2", Custom: "自定义"},
	})
	if !strings.Contains(out, `"answers"`) || !strings.Contains(out, `"q1"`) || !strings.Contains(out, "自定义") {
		t.Fatalf("答案序列化不一致：%q", out)
	}
	if skipped := tools.SkippedAnswers([]tools.QuestionSpec{{ID: "q1"}, {ID: "q2"}}); len(skipped) != 2 || skipped[0].Selected != nil {
		t.Fatalf("跳过答案应为空选择：%#v", skipped)
	}
}

func TestExecuteAllHandlesUnknownAndErrors(t *testing.T) {
	tc := newCtx(t, false)
	calls := []tools.Call{
		{ID: "c1", Name: "nope", Args: tools.Args{}},
		{ID: "c2", Name: "read", Args: tools.Args{"file_path": "missing.txt"}},
	}
	outcomes := tools.ExecuteAll(context.Background(), tools.Builtin(false), tc, calls)
	if len(outcomes) != 2 {
		t.Fatalf("应有 2 条结果：%#v", outcomes)
	}
	if !outcomes[0].IsError || !strings.Contains(outcomes[0].Output, "未知工具") {
		t.Fatalf("未知工具应返回错误结果：%#v", outcomes[0])
	}
	if !outcomes[1].IsError {
		t.Fatalf("读取失败应标错误：%#v", outcomes[1])
	}
}

func TestExecuteAllEmptyOutputPlaceholder(t *testing.T) {
	tc := newCtx(t, false)
	// write 一定有输出；用一个输出恒为空的场景不好构造，这里验证「无输出」占位逻辑存在
	outcomes := tools.ExecuteAll(context.Background(), tools.Builtin(false), tc, []tools.Call{
		{ID: "c1", Name: "todo_write", Args: tools.Args{"todos": []any{}}},
	})
	if outcomes[0].Output == "" {
		t.Fatal("工具结果不应为空字符串（空输出要替换成占位）")
	}
}

func TestShellDisabledReportsClearError(t *testing.T) {
	tc := newCtx(t, false)
	// shell 未注册，ExecuteAll 会当成未知工具
	outcomes := tools.ExecuteAll(context.Background(), tools.Builtin(false), tc, []tools.Call{
		{ID: "c1", Name: "bash", Args: tools.Args{"command": "echo hi"}},
	})
	if !outcomes[0].IsError || !strings.Contains(outcomes[0].Output, "未知工具") {
		t.Fatalf("未开启时 bash 不应可用：%#v", outcomes[0])
	}

	// 注册但 ctx 层禁用时也要给出明确原因
	tcShell := tools.NewToolContext(t.TempDir(), false)
	tool, ok := tools.Find(tools.Builtin(true), "bash")
	if !ok {
		t.Fatal("开启 shell 的工具集应含 bash")
	}
	_, err := tool.Run(context.Background(), tools.Args{"command": "echo hi"}, tcShell)
	if err == nil || !strings.Contains(err.Error(), "已禁用") {
		t.Fatalf("应提示已禁用：%v", err)
	}
}

func TestShellRunsAndReportsExitCode(t *testing.T) {
	tc := newCtx(t, true)
	out := mustOutput(t, tc, "bash", tools.Args{"command": echoCommand("hello")})
	if !strings.Contains(out, "hello") {
		t.Fatalf("应捕获输出：%q", out)
	}
	if !strings.Contains(out, "exit_code=0") {
		t.Fatalf("应带退出码：%q", out)
	}

	// 非零退出码 → 错误，但输出保留
	_, err := runTool(t, tc, "bash", tools.Args{"command": "echo failing && exit 3"})
	if err == nil || !strings.Contains(err.Error(), "exit_code=3") || !strings.Contains(err.Error(), "failing") {
		t.Fatalf("非零退出应报错并保留输出：%v", err)
	}
}

func TestShellWorkdir(t *testing.T) {
	tc := newCtx(t, true)
	if err := os.MkdirAll(filepath.Join(tc.Workspace(), "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := mustOutput(t, tc, "bash", tools.Args{"command": cwdCommand(), "workdir": "sub"})
	if !strings.Contains(out, "sub") {
		t.Fatalf("workdir 未生效：%q", out)
	}
	// 越界的 workdir 被拒绝
	if _, err := runTool(t, tc, "bash", tools.Args{"command": echoCommand("x"), "workdir": "../.."}); err == nil {
		t.Fatal("越界 workdir 应被拒绝")
	}
}

// 超时必须杀掉整棵进程树，且把已产生的输出带回来。
func TestShellTimeoutKillsTree(t *testing.T) {
	if testing.Short() {
		t.Skip("耗时用例")
	}
	tc := newCtx(t, true)
	_, err := runTool(t, tc, "bash", tools.Args{"command": slowCommand(), "timeoutMs": 1000})
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("应报超时：%v", err)
	}
}

// ctx 取消（用户点停止）必须真正中止命令。
func TestShellCancelledByContext(t *testing.T) {
	tc := newCtx(t, true)
	tool, ok := tools.Find(tools.Builtin(true), "bash")
	if !ok {
		t.Fatal("缺少 bash")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := tool.Run(ctx, tools.Args{"command": slowCommand(), "timeoutMs": 60_000}, tc)
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("取消后应返回错误")
		}
		if !errorsIsCanceled(err) {
			t.Fatalf("应返回 context.Canceled（或中止说明），实际 %v", err)
		}
	case <-timeoutAfter(20):
		t.Fatal("取消后命令未终止（进程树可能没被杀掉）")
	}
}

func errorsIsCanceled(err error) bool {
	return err != nil && (err == context.Canceled || strings.Contains(err.Error(), "context canceled") || strings.Contains(err.Error(), "已中止"))
}

func echoCommand(text string) string {
	if runtime.GOOS == "windows" {
		return "echo " + text
	}
	return "echo " + text
}

func cwdCommand() string {
	if runtime.GOOS == "windows" {
		return "cd"
	}
	return "pwd"
}

func slowCommand() string {
	if runtime.GOOS == "windows" {
		return "ping -n 20 127.0.0.1 > nul"
	}
	return "sleep 20"
}

// createDirLink 创建指向 target 的目录链接。
//
// Windows 上符号链接需要开发者模式/管理员，但 junction（mklink /J）普通用户即可创建，
// 所以先试 junction——这也正是 Rust 阶段 opus 指出的真实攻击面。
func createDirLink(link, target string) bool {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("cmd", "/C", "mklink", "/J", link, target)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := cmd.Run(); err == nil {
			return true
		}
	}
	return os.Symlink(target, link) == nil
}

// removeDirLink 删除目录链接（junction 用 RemoveAll 只删链接本身）。
func removeDirLink(link string) {
	if runtime.GOOS == "windows" {
		_ = os.Remove(link) // 空 junction 目录用 Remove 即可
	}
	_ = os.RemoveAll(link)
}

// timeoutAfter 返回一个「n 秒后触发」的 channel（测试辅助，避免全局 import time）。
func timeoutAfter(seconds int) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		timer := time.NewTimer(time.Duration(seconds) * time.Second)
		defer timer.Stop()
		<-timer.C
		close(ch)
	}()
	return ch
}

// 回归（opus P1-1）：Windows 上 exec.Command 的拼参数会把 " 转义成 \"，
// 而 cmd.exe 不认 CommandLineToArgvW 规则，导致带引号的命令全出错。
func TestShellQuotingOnWindows(t *testing.T) {
	tc := newCtx(t, true)
	cases := []struct {
		name    string
		command string
		want    []string
	}{
		{
			name:    "引号与空格",
			command: `echo "a b" & echo done`,
			want:    []string{`"a b"`, "done"},
		},
		{
			name:    "引号内中文",
			command: `echo "中文 混合 ascii"`,
			want:    []string{`"中文 混合 ascii"`},
		},
		{
			name:    "引号内含与号",
			command: `echo "x & y"`,
			want:    []string{`"x & y"`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := mustOutput(t, tc, "bash", tools.Args{"command": c.command})
			if strings.Contains(out, `\"`) {
				t.Fatalf("引号被转义破坏（Go 拼参数规则进了 cmd）：%q", out)
			}
			for _, want := range c.want {
				if !strings.Contains(out, want) {
					t.Fatalf("输出应包含 %q，实际 %q", want, out)
				}
			}
		})
	}
}

// 回归（opus P1-2）：输出必须限流读取，不能先整读再截断（会让 OOM）。
func TestShellOutputIsCapped(t *testing.T) {
	tc := newCtx(t, true)
	// 输出远超上限（约 1MB 的中文），断言被截断且带原始大小提示
	command := `for /L %i in (1,1,3000) do @echo 这是一行很长的中文输出用于测试截断上限`
	if runtime.GOOS != "windows" {
		command = `for i in $(seq 1 3000); do echo "这是一行很长的中文输出用于测试截断上限"; done`
	}
	out := mustOutput(t, tc, "bash", tools.Args{"command": command})
	if len(out) > 200_000 {
		t.Fatalf("输出未被截断：%d 字节", len(out))
	}
	if !strings.Contains(out, "已截断") {
		t.Fatalf("应提示已截断：%q", out[:min(len(out), 200)])
	}
	// 截断处不能出现半个 UTF-8 字符
	if !utf8.ValidString(out) {
		t.Fatal("截断后出现非法 UTF-8（应退回完整字符边界）")
	}
}

// 回归（opus P2-3）：CRLF 文件用 LF 的 old_string 也能编辑。
func TestEditHandlesCRLFFile(t *testing.T) {
	tc := newCtx(t, false)
	path := filepath.Join(tc.Workspace(), "crlf.txt")
	if err := os.WriteFile(path, []byte("line one\r\nline two\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := mustOutput(t, tc, "edit", tools.Args{
		"file_path":  "crlf.txt",
		"old_string": "line one\nline two",
		"new_string": "changed\nalso",
	})
	if !strings.Contains(out, "替换 1 处") {
		t.Fatalf("CRLF 文件应能匹配 LF 串：%q", out)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "changed\r\nalso\r\n" {
		t.Fatalf("应保持 CRLF 风格：%q", data)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// 回归（opus）：同一份输出里混着 UTF-8 行与 GBK 行时，必须逐行判断，
// 否则整体按 GBK 解码会把本来正确的 UTF-8 部分变成乱码。
func TestShellMixedEncodingOutput(t *testing.T) {
	tc := newCtx(t, true)
	// 第一行 UTF-8（Go 写出的文件），第二行用 cmd 的 OEM 代码页输出
	utf8Line := "utf8-中文-ok"
	out := mustOutput(t, tc, "bash", tools.Args{"command": mixedEncodingCommand(utf8Line)})
	if !strings.Contains(out, utf8Line) {
		t.Fatalf("UTF-8 行应保持原样：%q", out)
	}
	if strings.Contains(out, "\uFFFD") {
		t.Fatalf("不应出现替换字符：%q", out)
	}
}

func mixedEncodingCommand(utf8Line string) string {
	if runtime.GOOS == "windows" {
		// echo 走 OEM 代码页输出 GBK；两个 echo 之间用 & 串联
		return "echo " + utf8Line + " & echo 中文行"
	}
	return "printf '%s\n中文行\n' '" + utf8Line + "'"
}

// 读一个 GBK 编码的文件不能产出非法 UTF-8（否则写日志时会 panic）。
func TestReadGBKFileIsValidUTF8(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gbk.txt")
	// 「中文测试」的 GBK 字节
	gbk := []byte{0xd6, 0xd0, 0xce, 0xc4, 0xb2, 0xe2, 0xca, 0xd4}
	if err := os.WriteFile(path, gbk, 0o644); err != nil {
		t.Fatal(err)
	}
	tc := tools.NewToolContext(dir, false)
	output := mustOutput(t, tc, "read", tools.Args{"file_path": "gbk.txt"})
	if !utf8.ValidString(output) {
		t.Fatalf("输出不是合法 UTF-8：%q", output)
	}
	// 编码提示要摆在最前面，模型据此知道 write 写回会变成 UTF-8
	if !strings.HasPrefix(output, "（注意：文件不是 UTF-8") {
		t.Fatalf("缺少非 UTF-8 提示：%q", output)
	}
	// UTF-8 文件不该有这条提示
	utf8Path := filepath.Join(dir, "utf8.txt")
	if err := os.WriteFile(utf8Path, []byte("中文\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustOutput(t, tc, "read", tools.Args{"file_path": "utf8.txt"}); strings.Contains(got, "不是 UTF-8") {
		t.Fatalf("UTF-8 文件不该有编码提示：%q", got)
	}
}

// 编辑非 UTF-8 文件要明确报错，而不是按字节改坏它。
func TestEditNonUTF8FileFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gbk.txt")
	if err := os.WriteFile(path, []byte{0xd6, 0xd0}, 0o644); err != nil {
		t.Fatal(err)
	}
	tc := tools.NewToolContext(dir, false)
	_, err := runTool(t, tc, "edit", tools.Args{
		"file_path": "gbk.txt", "old_string": "a", "new_string": "b",
	})
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("应报「不是 UTF-8」错误：%v", err)
	}
}

// 用户崩溃的直接原因：`read` 读了 PNG。二进制文件必须被拒绝，而不是当成文本行返回。
func TestReadBinaryFileRefused(t *testing.T) {
	dir := t.TempDir()
	// 最小 PNG 头：\x89PNG\r\n\x1a\n + IHDR 长度字段（含 NUL）
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R'}
	if err := os.WriteFile(filepath.Join(dir, "pic.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	tc := tools.NewToolContext(dir, false)
	_, err := runTool(t, tc, "read", tools.Args{"file_path": "pic.png"})
	if err == nil {
		t.Fatal("读二进制文件应当报错")
	}
	if !strings.Contains(err.Error(), "二进制") {
		t.Fatalf("错误信息应说明是二进制文件：%v", err)
	}
	// 中文 GBK 文本不含 NUL，不能被误判成二进制（这条保护 GBK 支持）
	gbkPath := filepath.Join(dir, "gbk.txt")
	if err := os.WriteFile(gbkPath, []byte{0xd6, 0xd0, 0xce, 0xc4}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, tc, "read", tools.Args{"file_path": "gbk.txt"}); err != nil {
		t.Fatalf("GBK 文本不该被当成二进制：%v", err)
	}
}

// UTF-16 文本（Windows 的 .reg 等）不能因为带 NUL 就被当成二进制拒绝：
// read 按 BOM 转成 UTF-8 并给出提示；edit 明确拒绝（不做隐式编码转换）。
func TestReadUTF16File(t *testing.T) {
	dir := t.TempDir()
	text := "中文测试\nsecond line\n"
	// UTF-16LE + BOM
	encoded := []byte{0xFF, 0xFE}
	for _, r := range text {
		if r == '\n' {
			encoded = append(encoded, '\n', 0x00)
			continue
		}
		encoded = append(encoded, byte(r), byte(r>>8))
	}
	if err := os.WriteFile(filepath.Join(dir, "u16.txt"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	tc := tools.NewToolContext(dir, false)
	output := mustOutput(t, tc, "read", tools.Args{"file_path": "u16.txt"})
	if !strings.Contains(output, "中文测试") {
		t.Fatalf("应按 UTF-16 解码出内容：%q", output)
	}
	if !strings.HasPrefix(output, "（注意：文件是 UTF-16 编码") {
		t.Fatalf("应给出 UTF-16 提示：%q", output)
	}

	// edit 拒绝 UTF-16（避免静默改编码）
	_, err := runTool(t, tc, "edit", tools.Args{
		"file_path": "u16.txt", "old_string": "第二个", "new_string": "x",
	})
	if err == nil || !strings.Contains(err.Error(), "UTF-16") {
		t.Fatalf("edit 应拒绝 UTF-16 文件：%v", err)
	}
}

// read_image：能返回图片字节与尺寸；非视觉模型、非图片文件、缺文件都要明确报错。
func TestReadImageTool(t *testing.T) {
	dir := t.TempDir()
	// 100x50 的纯色 PNG
	img := image.NewRGBA(image.Rect(0, 0, 100, 50))
	for y := 0; y < 50; y++ {
		for x := 0; x < 100; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 30, B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "red.png"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	tc := tools.NewToolContext(dir, false)
	tc.Vision = true
	result, err := runTool(t, tc, "read_image", tools.Args{"file_path": "red.png"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != 1 {
		t.Fatalf("应返回 1 张图片：%#v", result.Images)
	}
	got := result.Images[0]
	if got.MediaType != "image/png" || got.Width != 100 || got.Height != 50 {
		t.Fatalf("图片信息不对：%+v", got)
	}
	if len(got.Data) == 0 {
		t.Fatal("图片字节为空")
	}
	if !strings.Contains(result.Output, "<type>image</type>") || !strings.Contains(result.Output, "100x50 px") {
		t.Fatalf("输出信封不对：%q", result.Output)
	}

	// 非视觉模型：明确报错，不读文件
	blind := tools.NewToolContext(dir, false)
	if _, err := runTool(t, blind, "read_image", tools.Args{"file_path": "red.png"}); err == nil ||
		!strings.Contains(err.Error(), "不支持图片输入") {
		t.Fatalf("非视觉模型应报错：%v", err)
	}
	// 非图片文件
	if _, err := runTool(t, tc, "read_image", tools.Args{"file_path": "note.txt"}); err == nil ||
		!strings.Contains(err.Error(), "PNG/JPEG/WebP/GIF") {
		t.Fatalf("非图片应报错：%v", err)
	}
	// 大图会被缩放
	big := image.NewRGBA(image.Rect(0, 0, 2400, 1200))
	for y := 0; y < 1200; y++ {
		for x := 0; x < 2400; x++ {
			big.Set(x, y, color.RGBA{R: 10, G: 120, B: 200, A: 255})
		}
	}
	var bigBuf bytes.Buffer
	if err := png.Encode(&bigBuf, big); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.png"), bigBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	scaled, err := runTool(t, tc, "read_image", tools.Args{"file_path": "big.png"})
	if err != nil {
		t.Fatal(err)
	}
	if scaled.Images[0].Width != 1568 || scaled.Images[0].Height != 784 {
		t.Fatalf("大图应缩到长边 1568：%+v", scaled.Images[0])
	}
	if !strings.Contains(scaled.Output, "downscaled from 2400x1200") {
		t.Fatalf("应说明原尺寸：%q", scaled.Output)
	}
}
