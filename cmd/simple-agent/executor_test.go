package main

import (
	"testing"

	"simple-agent/internal/config"
	"simple-agent/internal/host"
	"simple-agent/internal/providers"
)

// 执行端配置只有一个组装点（executorConfig）：这里守两件事。
//
// 背景：UI 模式（uiapp.NewApp）与 headless 模式以前各写一遍字段映射，漏一个字段
// 不会报错——只是某个工具静默消失。真实两次事故是 allow_shell（headless 漏传，
// 没有 bash）与 web_fetch（UI 漏传，模型压根看不到这个工具）。
func TestExecutorConfigCoversConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
		want host.Config
	}{
		{
			name: "默认：命令关闭、联网开启",
			cfg:  config.Config{Workspace: "/ws", MaxToolRounds: 12},
			want: host.Config{AllowShell: false, WebFetch: true, MaxToolRounds: 12},
		},
		{
			name: "完全权限",
			cfg:  config.Config{AllowShell: true, Workspace: "/ws", MaxToolRounds: 0},
			want: host.Config{AllowShell: true, WebFetch: true, MaxToolRounds: 0},
		},
		{
			name: "关掉联网",
			cfg:  config.Config{AllowShell: true, DisableWebFetch: true, Workspace: "/ws"},
			want: host.Config{AllowShell: true, WebFetch: false},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.cfg
			cfg.Workspace = c.want.Workspace
			got := executorConfig(&cfg, &appRuntime{}, providers.Selection{ProviderID: "p", Model: "m"},
				nil, nil, func(func()) {}, "tok", "name")
			if got.AllowShell != c.want.AllowShell {
				t.Fatalf("AllowShell 没透传：%v", got.AllowShell)
			}
			if got.WebFetch != c.want.WebFetch {
				t.Fatalf("WebFetch 没透传：%v（DisableWebFetch=%v）", got.WebFetch, cfg.DisableWebFetch)
			}
			if got.MaxToolRounds != c.want.MaxToolRounds {
				t.Fatalf("MaxToolRounds 没透传：%v", got.MaxToolRounds)
			}
			if got.Workspace != c.want.Workspace {
				t.Fatalf("Workspace 没透传：%v", got.Workspace)
			}
			if got.Token != "tok" || got.Name != "name" {
				t.Fatalf("口令/名字没透传：%q %q", got.Token, got.Name)
			}
			if got.SystemPrompt == "" {
				t.Fatal("SystemPrompt 不该为空")
			}
		})
	}
}

// UI 模式与 headless 模式必须拿到**同一份**执行端配置。
//
// 这条是那个 bug 的直接守门人：web_fetch 在 UI 模式下静默消失，就是因为两条路径
// 各拼了一遍字段。现在两边都走 executorConfig —— 这里用同一个 cfg 分别按两条
// 路径构造，逐字段比对。
func TestUIAndHeadlessShareExecutorConfig(t *testing.T) {
	for _, cfg := range []config.Config{
		{Workspace: "/ws", MaxToolRounds: 3},
		{Workspace: "/ws", AllowShell: true, DisableWebFetch: true},
	} {
		cfg := cfg
		runtime := &appRuntime{registry: nil, allowShell: cfg.AllowShell, webFetch: !cfg.DisableWebFetch}
		selection := providers.Selection{ProviderID: "p", Model: "m"}
		post := func(func()) {}

		// headless 路径（薄封装，直接委托）
		headless := headlessHostConfig(&cfg, runtime, selection, nil, nil, post, "tok", "name")
		// UI 路径（main.go 里构造 uiapp.Options 用的同一份）
		ui := executorConfig(&cfg, runtime, selection, nil, nil, post, "tok", "name")

		if !sameExecutorShape(ui, headless) {
			t.Fatalf("UI 与 headless 的执行端配置不一致：\nUI       : %+v\nheadless : %+v", ui, headless)
		}
		if ui.WebFetch != !cfg.DisableWebFetch {
			t.Fatalf("WebFetch 应为 %v：%+v", !cfg.DisableWebFetch, ui)
		}
		if ui.AllowShell != cfg.AllowShell {
			t.Fatalf("AllowShell 应为 %v：%+v", cfg.AllowShell, ui)
		}
	}
}

// 搜索凭据只从环境变量读：设了就有后端，没设后端也存在但 Available() 为假
// （由 BuiltinWith 决定不注册工具，而不是让每次调用报错）。
func TestSearchBackendFollowsEnv(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	if searcher := searchBackend(); searcher.Available() {
		t.Fatalf("没设 Key 时不该可用：%+v", searcher)
	}

	t.Setenv("DEEPSEEK_API_KEY", "  sk-test  ")
	searcher := searchBackend()
	if !searcher.Available() {
		t.Fatal("设了 Key 应可用")
	}
	if searcher.APIKey != "sk-test" {
		t.Fatalf("Key 应去掉首尾空白，实际 %q", searcher.APIKey)
	}

	// 落到执行端配置里（UI 与 headless 共用同一个组装点）
	cfg := config.Config{Workspace: "/ws"}
	got := executorConfig(&cfg, &appRuntime{}, providers.Selection{ProviderID: "p", Model: "m"},
		nil, nil, func(func()) {}, "", "")
	if got.Searcher == nil || !got.Searcher.Available() {
		t.Fatalf("executorConfig 应带上搜索后端：%+v", got.Searcher)
	}
}

// sameExecutorShape 比较配置里与"入口选择"无关的字段（函数字段比指针太脆，
// nil 与 nil 视为相同）。
func sameExecutorShape(a, b host.Config) bool {
	if a.AllowShell != b.AllowShell || a.WebFetch != b.WebFetch ||
		a.MaxToolRounds != b.MaxToolRounds || a.Workspace != b.Workspace ||
		a.Token != b.Token || a.Name != b.Name || a.SystemPrompt != b.SystemPrompt ||
		a.Selection != b.Selection || a.ContextWindow != b.ContextWindow {
		return false
	}
	if (a.Catalog == nil) != (b.Catalog == nil) || (a.Blobs == nil) != (b.Blobs == nil) {
		return false
	}
	// 搜索后端也必须同源（两边都不该各自去读环境变量）
	if (a.Searcher == nil) != (b.Searcher == nil) {
		return false
	}
	if a.Searcher != nil && a.Searcher.APIKey != b.Searcher.APIKey {
		return false
	}
	return true
}
