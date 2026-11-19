package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"simple-agent/internal/config"
	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
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
		runtime := &appRuntime{}
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

// 回归：**driver 的清单**与**执行端真正执行用的表**必须是同一份工具集。
//
// 真实事故（web_search 第一次上线）：driver 拿到的是带 Searcher 的选项，而
// appRuntime.RunTools 自己拼了一份没有 Searcher 的 Options——于是模型看到
// web_search（清单里有），一调用就回「未知工具：web_search」。这个 bug 只在
// 真机上表现为"清单有、调用无"，界面上完全看不出来，所以必须由测试守住。
//
// 关键是**真的走 RunTools 去执行**：只比 runtime.toolOptions 字段抓不到
// "RunTools 自己另拼一份"的那种写法（我第一版测试就漏掉了，故意打回 bug 也没红）。
func TestRuntimeToolsMatchDriverList(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-test") // 让 searcher 可用，才走得到"两边都该有"的情形
	cfg := config.Config{Workspace: "/ws", AllowShell: true}
	runtime := &appRuntime{}

	driverConfig := executorConfig(&cfg, runtime, providers.Selection{ProviderID: "p", Model: "m"},
		nil, nil, func(func()) {}, "", "")

	// driver 侧：用 host.Config 的字段还原出工具表（host 内部就是这么做的）
	driverTools := tools.BuiltinWith(tools.Options{
		AllowShell: driverConfig.AllowShell,
		WebFetch:   driverConfig.WebFetch,
		Searcher:   driverConfig.Searcher,
	})
	if !strings.Contains(names(driverTools), "web_search") {
		t.Fatalf("配了 Key 时 driver 清单里该有 web_search：%v", names(driverTools))
	}

	// 执行侧：**让 RunTools 真的去执行**清单里的每个工具名，看它认不认得。
	// 认不得的那一个会回「未知工具」——这正是线上看到的现象。
	tc := tools.NewToolContext(t.TempDir(), false)
	calls := make([]tools.Call, 0, len(driverTools))
	for index, tool := range driverTools {
		calls = append(calls, tools.Call{
			ID:   fmt.Sprintf("c%d", index),
			Name: tool.Name,
			// 参数故意留空：这些工具都会因为缺参数而报错，但**不会**报「未知工具」。
			Args: tools.Args{},
		})
	}
	outcomes := runtime.RunTools(context.Background(), tc, calls)
	for index, outcome := range outcomes {
		if strings.Contains(outcome.Output, "未知工具") {
			t.Fatalf("清单里有 %s，但执行端不认识它（这就是「清单有、调用说未知工具」）：%s",
				driverTools[index].Name, outcome.Output)
		}
	}
}

// names 把工具表拍成一行名字，便于比对/报错。
func names(list []*tools.ToolDef) string {
	out := make([]string, 0, len(list))
	for _, tool := range list {
		out = append(out, tool.Name)
	}
	return strings.Join(out, ",")
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
