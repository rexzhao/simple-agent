package main

import (
	"os"
	"strings"

	"simple-agent/internal/config"
	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/web"
)

// 本文件是**执行端配置的唯一组装点**：UI 模式与 headless 模式都调它。
//
// 为什么单独抽出来：两处各写一遍字段映射时，漏一个字段不会报错——它只是让某个
// 工具静默消失（真实事故：UI 模式漏传 WebFetch，模型压根看不到 web_fetch，而
// headless 正常）。现在要漏只能一起漏，而且 TestExecutorConfigCoversConfig
// 会把「配置里有的开关没有落到 host.Config」抓出来。

// executorConfig 把应用配置翻成执行端配置。
//
// 参数里只留"入口才知道"的东西（runtime、数据目录、投递函数、口令、名字）；
// 其余全部从 cfg 派生，**不要在调用处再拼字段**。
func executorConfig(cfg *config.Config, runtime *appRuntime, selection providers.Selection,
	catalog *store.Catalog, blobs *store.Blobs, post func(func()), token, name string) host.Config {
	return host.Config{
		Catalog:      catalog,
		Blobs:        blobs,
		Runtime:      runtime,
		Selection:    selection,
		SystemPrompt: systemPrompt(*cfg),

		// ---- 工具与权限（新增开关请加在这里）----
		// 权限档位：完全权限 = 可执行命令 + 路径不限工作区。
		AllowShell: cfg.AllowShell,
		// web_fetch 与权限分开：抓网页不执行东西、不要凭据，默认开启、配置里可关。
		WebFetch: !cfg.DisableWebFetch,
		// web_search 需要 DeepSeek 的 Key（`DEEPSEEK_API_KEY`）：没配就干脆不注册
		// 这个工具——注册了每次报"没配 Key"只会让模型反复试错。
		// 与 web_fetch 无关：抓取走普通 HTTP，不需要任何凭据。
		Searcher:      searchBackend(),
		MaxToolRounds: cfg.MaxToolRounds,

		// ---- 会话与存储 ----
		Workspace: cfg.Workspace,
		Post:      post,
		Token:     token,
		Name:      name,

		// ---- 模型相关（切换模型时要能重新算）----
		ContextWindow: windowFor(runtime.registry, selection),
		EffortFor:     func(sel providers.Selection) string { return effortFor(runtime.registry, sel) },
		VisionFor:     func(sel providers.Selection) bool { return visionFor(runtime.registry, sel) },
		MaxOutputFor:  func(sel providers.Selection) int { return maxOutputFor(runtime.registry, sel) },
		// 模型列表随 catalog 发给展示端：headless 也要给，否则远程端切不了模型。
		Models: providerChoices(runtime.registry),
	}
}

// searchBackend 构造搜索后端（读 DEEPSEEK_API_KEY）。
//
// **每次启动读一次环境变量**（不是每次调用读）：用户在会话里 `$env:DEEPSEEK_API_KEY=...`
// 设置后必须重启才生效，这是有意的——凭据变化时工具清单可能变（没有 Key 就不注册
// web_search），中途改变会让 driver 手里的工具表与执行端不一致。
//
// 没有 Key 时返回的 Searcher 也保留（Available() 为假）：BuiltinWith 据此决定
// 不注册工具，而调用点不必再判空。
func searchBackend() *web.Searcher {
	return &web.Searcher{APIKey: strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))}
}
