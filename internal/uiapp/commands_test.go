package uiapp

import (
	"strings"
	"testing"
)

// 斜杠命令表本身：名字唯一、帮助文本非空、用法格式正确。
func TestSlashCommandTable(t *testing.T) {
	seen := map[string]bool{}
	for _, command := range slashCommands {
		if command.Name == "" {
			t.Fatal("命令名不能为空")
		}
		if strings.ContainsAny(command.Name, " /") {
			t.Fatalf("命令名不该含空格或斜杠：%q", command.Name)
		}
		if seen[command.Name] {
			t.Fatalf("命令名重复：%s", command.Name)
		}
		seen[command.Name] = true
		if command.Help == "" {
			t.Fatalf("/%s 缺少帮助文本（候选列表要显示它）", command.Name)
		}
		if command.Run == nil {
			t.Fatalf("/%s 没有实现", command.Name)
		}
	}
	// 四个基础命令必须存在
	for _, name := range []string{"connect", "disconnect", "relay", "help"} {
		if !seen[name] {
			t.Fatalf("缺少基础命令 /%s", name)
		}
	}
}

// 前缀匹配：输入 / 列出全部，/con 只留 connect，大小写不敏感。
func TestSlashPrefixMatching(t *testing.T) {
	if got := len(matchCommands("")); got != len(slashCommands) {
		t.Fatalf("空前缀应返回全部 %d 条，实际 %d", len(slashCommands), got)
	}
	got := matchCommands("con")
	if len(got) != 1 || got[0].Name != "connect" {
		t.Fatalf("前缀 con 应只匹配 connect，实际 %+v", got)
	}
	if got := matchCommands("CON"); len(got) != 1 || got[0].Name != "connect" {
		t.Fatalf("前缀匹配应忽略大小写，实际 %+v", got)
	}
	if got := matchCommands("zzz"); len(got) != 0 {
		t.Fatalf("无匹配时应返回空，实际 %+v", got)
	}
}

// isSlashCommand：只认以 / 开头的输入。
func TestIsSlashCommand(t *testing.T) {
	cases := map[string]bool{
		"/help":     true,
		"  /con":    true,
		"你好":        false,
		"":          false,
		"/":         true,
		"a /b":      false,
	}
	for input, want := range cases {
		if got := isSlashCommand(input); got != want {
			t.Fatalf("%q：期望 %v，实际 %v", input, want, got)
		}
	}
}

// 帮助文本要列出每一条命令及其用法。
func TestBuildHelpText(t *testing.T) {
	text := buildHelpText()
	if !strings.Contains(text, "可用命令") {
		t.Fatalf("帮助文本缺少标题：%s", text)
	}
	for _, command := range slashCommands {
		if !strings.Contains(text, "/"+command.Name) {
			t.Fatalf("帮助文本缺少 /%s：%s", command.Name, text)
		}
	}
	if !strings.Contains(text, "/connect") || !strings.Contains(text, "RELAY") {
		t.Fatalf("帮助文本应展示 connect 的用法：%s", text)
	}
}

// slashPrefix：只有"命令名还没打完"时才弹候选（出现空格就不弹了）。
func TestSlashPrefixDetection(t *testing.T) {
	cases := []struct {
		draft  string
		want   string
		wantOK bool
	}{
		{"", "", false},
		{"你好", "", false},
		{"/", "", true},
		{"/con", "con", true},
		{"/connect ", "", false},        // 已经在打参数
		{"/connect 1.2.3.4", "", false}, // 同上
		{" /con", "con", true},          // 前后空格容错
	}
	for _, item := range cases {
		app := &App{draft: item.draft}
		prefix, ok := app.slashPrefix()
		if ok != item.wantOK {
			t.Fatalf("draft=%q：期望 ok=%v，实际 %v", item.draft, item.wantOK, ok)
		}
		if ok && prefix != item.want {
			t.Fatalf("draft=%q：期望前缀 %q，实际 %q", item.draft, item.want, prefix)
		}
	}
}

// 未连接时 /disconnect 与 /relay 都要给出"没有连接"的提示，而不是报错。
func TestRelayCommandsWithoutConnection(t *testing.T) {
	app := &App{}
	if app.Relaying() {
		t.Fatal("零值 App 不该处于 relay 连接状态")
	}
	if message, ok := app.ExecSlashCommand("/disconnect"); !ok ||
		!strings.Contains(message, "没有连接") {
		t.Fatalf("/disconnect 应提示未连接，实际：%q", message)
	}
	if message, ok := app.ExecSlashCommand("/relay"); !ok ||
		!strings.Contains(message, "没有连接") {
		t.Fatalf("/relay 应提示未连接，实际：%q", message)
	}
}

// /connect 的参数校验：缺参数、指纹格式错误都要在本地拦住。
func TestConnectArgumentValidation(t *testing.T) {
	app := &App{}

	// 没有任何保存参数时，/connect 不带参数要提示用法
	if message, ok := app.ExecSlashCommand("/connect"); !ok ||
		!strings.Contains(message, "用法") {
		t.Fatalf("缺参数时应提示用法，实际：%q ok=%v", message, ok)
	}

	// 指纹长度不对：必须本地报错，而不是一直重连失败
	message, ok := app.ExecSlashCommand("/connect 127.0.0.1:7900 token shortfp")
	if !ok || !strings.Contains(message, "指纹") {
		t.Fatalf("指纹格式错误应提示，实际：%q", message)
	}
	if app.Relaying() {
		t.Fatal("校验失败不该进入连接状态")
	}
}

// 非命令文本不该被当作命令处理（否则用户发以 / 开头的正文会丢失）。
func TestNonCommandTextIsNotConsumed(t *testing.T) {
	app := &App{}
	if _, ok := app.ExecSlashCommand("你好"); ok {
		t.Fatal("普通文本不该被当作命令")
	}
	if message, ok := app.ExecSlashCommand("/nosuchcmd"); !ok ||
		!strings.Contains(message, "未知命令") {
		t.Fatalf("未知命令应提示，实际：%q", message)
	}
}
