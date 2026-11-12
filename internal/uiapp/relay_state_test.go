package uiapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
)

// newLocalForCommandTest 造一个带数据目录的本地执行端（命令逻辑需要 Local）。
func newLocalForCommandTest(t *testing.T) (*host.Local, string) {
	t.Helper()
	dataDir := t.TempDir()
	local := host.NewLocal(host.Config{
		Runtime:       &stubRuntime{},
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Workspace:     dataDir,
		MaxToolRounds: 3,
		ContextWindow: 200_000,
		Catalog:       store.NewCatalog(dataDir),
		Post:          func(fn func()) { fn() },
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	sessionID := ""
	if active := local.Active(); active != nil {
		sessionID = active.ID
	}
	return local, sessionID
}

// newCommandTestApp 造一个最小的 App（只挂 host 与数据目录，够跑斜杠命令）。
func newCommandTestApp(t *testing.T, local *host.Local, dataDir string) *App {
	t.Helper()
	app := &App{
		host: local,
		post: func(fn func()) { fn() },
	}
	app.opts.Catalog = store.NewCatalog(dataDir)
	return app
}

// 记住上次 relay：连过一次之后，`/connect` 不带参数就能复用。
func TestConnectRemembersLastRelay(t *testing.T) {
	local, sessionID := newLocalForCommandTest(t)
	dataDir := t.TempDir()
	app := newCommandTestApp(t, local, dataDir)
	_ = sessionID

	// 还没连过：不带参数的 /connect 应该提示用法
	if message, _ := app.ExecSlashCommand("/connect"); !strings.Contains(message, "用法") {
		t.Fatalf("首次无参数应提示用法，实际：%q", message)
	}

	// 带参数连一次（用一个不存在的地址也行：保存发生在发起注册时）
	message, _ := app.ExecSlashCommand("/connect 10.0.0.9:7900 mytoken")
	if !strings.Contains(message, "已连接") {
		t.Fatalf("连接应成功发起，实际：%q", message)
	}
	t.Cleanup(func() { app.stopRelayRegistration() })

	// 参数应该已经落盘
	statePath := filepath.Join(dataDir, "relay.json")
	content, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("应该写出 %s：%v", statePath, err)
	}
	if !strings.Contains(string(content), "10.0.0.9:7900") ||
		!strings.Contains(string(content), "mytoken") {
		t.Fatalf("保存内容不完整：%s", content)
	}

	// 现在不带参数的 /connect 应该复用这组参数
	message, _ = app.ExecSlashCommand("/connect")
	if strings.Contains(message, "用法") {
		t.Fatalf("已经有保存参数，不该再提示用法：%q", message)
	}
	if !strings.Contains(message, "10.0.0.9:7900") {
		t.Fatalf("应复用保存的地址，实际：%q", message)
	}
}

// /relay 在未连接时要显示上次保存的地址（这是 /connect 下次会用的）。
func TestRelayStatusShowsSaved(t *testing.T) {
	local, _ := newLocalForCommandTest(t)
	app := newCommandTestApp(t, local, t.TempDir())

	app.saveRelay(savedRelay{Addr: "1.2.3.4:7900", Fingerprint: strings.Repeat("a", 64)})
	message, ok := app.ExecSlashCommand("/relay")
	if !ok {
		t.Fatal("/relay 应是有效命令")
	}
	if !strings.Contains(message, "1.2.3.4:7900") {
		t.Fatalf("应显示上次保存的地址，实际：%q", message)
	}
	if !strings.Contains(message, "TLS") {
		t.Fatalf("有指纹时应标注 TLS，实际：%q", message)
	}
}

// /disconnect --forget 要清掉保存的参数。
func TestDisconnectForgetClearsSaved(t *testing.T) {
	local, _ := newLocalForCommandTest(t)
	dataDir := t.TempDir()
	app := newCommandTestApp(t, local, dataDir)

	app.saveRelay(savedRelay{Addr: "5.6.7.8:7900"})
	statePath := filepath.Join(dataDir, "relay.json")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("应先有保存文件：%v", err)
	}

	message, _ := app.ExecSlashCommand("/disconnect --forget")
	if !strings.Contains(message, "忘掉") {
		t.Fatalf("应提示已忘掉，实际：%q", message)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("保存文件应被删除，实际 err=%v", err)
	}

	// 忘掉之后，不带参数的 /connect 又该提示用法了
	if message, _ := app.ExecSlashCommand("/connect"); !strings.Contains(message, "用法") {
		t.Fatalf("忘掉后应回到提示用法，实际：%q", message)
	}
}