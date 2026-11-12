// Command simple-agent 是桌面 AI 助手的入口。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"simple-agent/internal/config"
	"simple-agent/internal/host"
	"simple-agent/internal/locale"
	"simple-agent/internal/providers"
	"simple-agent/internal/relay"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
	"simple-agent/internal/uiapp"
	"simple-agent/internal/uistate"
	"simple-agent/internal/wire"
)

// version 由构建脚本通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

// handleEarlyArgs 处理不需要启动界面的参数（打印后退出），返回是否已处理。
//
// 抽成函数是为了能单测：这两个参数的行为是"无论有没有别的实例在跑，
// 都应当立刻打印并退出"。
func handleEarlyArgs(args []string, out io.Writer) bool {
	for _, arg := range args {
		switch arg {
		case "--version", "-v":
			fmt.Fprintf(out, "simple-agent %s\n", version)
			return true
		case "--help", "-h":
			fmt.Fprint(out, config.Help())
			return true
		}
	}
	return false
}

func main() {
	// --version / --help 要在**拿单实例锁之前**处理：它们只是打印文本，
	// 不该因为已经有一个实例在跑就被拦住（更不能弹「已在运行」的对话框）。
	if handleEarlyArgs(os.Args[1:], os.Stdout) {
		return
	}

	args := os.Args[1:]

	// --listen / --connect 是本进程的"执行端/展示端"开关（远程模式，见
	// docs/remote-design.md）。它们与本地模式互斥；展示端不拿单实例锁、
	// 不建 session 目录锁，只连执行端。
	opts := parseRemoteOptions(&args)
	listenAddr, connectAddr := opts.listen, opts.connect

	// relay 子命令：只做转发，不需要配置/会话/单实例锁
	if opts.relayServer {
		// relay 也要能上 TLS（它就是那个需要被客户端验指纹的服务端）。
		// 它不走完整配置加载（那样会创建/锁定 data_dir），所以只取 TLS 这几项；
		// DataDir 用来放自动生成的证书（--tls 不给路径时）。
		relayTLSCfg := config.Config{TLS: config.TLSConfig{
			Enable:      opts.tlsEnabled,
			CertFile:    firstNonEmptyStr(opts.tlsCert, ""),
			KeyFile:     firstNonEmptyStr(opts.tlsKey, ""),
			Fingerprint: firstNonEmptyStr(opts.tlsFingerprint, ""),
		}}
		relayTLSCfg.DataDir = firstNonEmptyStr(os.Getenv("SIMPLE_AGENT_DATA"), config.DefaultDataDir())
		if !setupTLS(&relayTLSCfg, opts) {
			return
		}
		os.Exit(runRelayServer(listenAddr, opts.relayToken))
	}

	// 单实例锁必须**最先**获取：配置加载会创建 data_dir，M5 之后还会打开会话
	// 日志；锁要是排在后面，两个实例就会同时写同一个 JSONL。
	// 远程模式（监听/展示端）不参与本地单实例锁：一个进程可以既开执行端又开
	// 若干个展示端。
	if listenAddr == "" && connectAddr == "" && !opts.headless {
		if !mygo.App.RequestSingleInstanceLock() {
			notifyAlreadyRunning()
			return
		}
	}

	configPath := ""
	for i := 0; i < len(args); i++ {
		if (args[i] == "--config" || args[i] == "-c") && i+1 < len(args) {
			configPath = args[i+1]
		}
	}

	cfg, err := config.Load(configPath, args)
	if errors.Is(err, config.ErrHelp) {
		fmt.Print(config.Help())
		return
	}
	if err != nil {
		showFatal(locale.Tr("dialog.config.title"), err.Error())
		return
	}

	// 界面语言：配置里的 language（auto 跟随系统），进程级默认也同步一份。
	// 环境变量 SIMPLE_AGENT_LANG 已在 config.applyEnv 里覆盖到 cfg.Language。
	lang, langRecognized := locale.Parse(cfg.Language)
	if !langRecognized {
		lang = locale.Detect()
	}
	locale.SetCurrent(lang)

	// 传输加密（默认关闭）。必须在**任何连接建立之前**装配：
	// 它要装 wire 的拨号钩子，晚了第一条连接就已经是明文了。
	// 返回 false 表示只打印了指纹、该退出。
	if !setupTLS(&cfg, opts) {
		return
	}

	registry := cfg.Registry()
	selection, ok := registry.Initial()
	if !ok {
		// 没有任何凭据：走模拟提供商，界面里给出一条「请配置 API Key」的提示流
		selection = providers.SimulatedSelection()
	}
	provider, err := registry.Build(selection)
	if err != nil {
		log.Printf("构造提供商失败（回退到模拟）：%v", err)
		selection = providers.SimulatedSelection()
		provider, _ = registry.Build(selection)
	}

	// 数据目录独占锁：mygo 的单实例锁在建不了 socket 时会「当作唯一实例」放行，
	// 那会让两个进程写同一份 JSONL。这把锁负责护数据。展示端不写会话日志，不拿锁。
	var dirLock *store.DirLock
	var catalog *store.Catalog
	var blobs *store.Blobs
	if connectAddr == "" {
		lock, lockErr := store.AcquireDirLock(cfg.DataDir)
		if lockErr != nil {
			notifyLocked(lockErr)
			return
		}
		dirLock = lock
		defer dirLock.Release()
		catalog = store.NewCatalog(cfg.DataDir)
		opened, blobsErr := store.NewBlobs(cfg.DataDir)
		if blobsErr != nil {
			showFatal(locale.Tr("dialog.dataDir.title"), blobsErr.Error())
			return
		}
		blobs = opened
	}

	// 窗口在 WhenReady 里创建；Post 用它把回调投递到主线程并请求重绘。
	// 窗口出来之前到来的回调先排队，等窗口建好再统一投递。
	var window *mygo.Window
	var pending []func()
	post := func(f func()) {
		if window == nil {
			pending = append(pending, f)
			return
		}
		window.Update(f)
	}

	// 日志同时写文件：GUI / headless 都需要事后能查看断流等失败原因
	role := ""
	if connectAddr != "" {
		role = "viewer"
	}
	closeLog, logFile := setupLogging(cfg.DataDir, role)
	defer closeLog()
	// 收发日志只写文件，不打到控制台
	host.SetNetLogOutput(logFile)
	host.EnableNetLog(opts.netLog)

	// headless：不建窗口、不跑 mygo 事件循环，只做执行端
	if opts.headless {
		// 无界面时的提示写进会话即可；Remote 通知仍可经 relay 到达展示端
		os.Exit(runHeadless(&cfg, &appRuntime{
			registry: registry, provider: provider, allowShell: cfg.AllowShell,
		}, selection, listenAddr, opts.relay, opts.name, opts.relayToken, opts.netLog, catalog, blobs))
	}

	// 展示端：先连上执行端，拿到 Remote 之后把它注入 uiapp（界面代码不变）。
	var remoteHost host.Host
	var executors uiapp.ExecutorSource
	if connectAddr != "" {
		// 一条连接、一次握手分辨执行端/relay；直连时没有额外探测超时。
		// 自动选中的执行端**固定下来**：重连只连它，绝不悄悄换成另一台机器。
		pinned := &pinnedHost{hostID: opts.route}
		dial := func() (wire.Conn, error) {
			pinned.Lock()
			target := pinned.hostID
			pinned.Unlock()
			if target == "" {
				return nil, errNoExecutorChosen
			}
			conn, chosen, dialErr := relay.OpenViewerToken(connectAddr, target, opts.relayToken)
			if dialErr == nil && chosen != "" {
				pinned.Lock()
				pinned.hostID = chosen
				pinned.Unlock()
			}
			return conn, dialErr
		}
		// 先亲手连一次：如果对面是 relay 且没指定执行端，就进入"选择执行端"界面，
		// 而不是 fatal 退出（用户的需求正是在这里选）。
		conn, chosen, openErr := relay.OpenViewerToken(connectAddr, pinned.hostID, opts.relayToken)
		var needsChoice *relay.NeedsChoice
		switch {
		case openErr == nil:
			if chosen != "" {
				pinned.Lock()
				pinned.hostID = chosen
				pinned.Unlock()
				executors = &relayExecutors{addr: connectAddr, current: pinned, token: opts.relayToken}
			}
			// 直连执行端时 `--token` 同时是执行端的访问口令：握手要带上，
			// 否则执行端会拒绝（表现是"连上了但立刻断开"）。
			remote, remoteErr := host.NewRemoteToken(conn, post, opts.relayToken)
			if remoteErr != nil {
				showFatal(locale.Tr("dialog.config.title"), remoteErr.Error())
				return
			}
			remoteHost = remote
			if source, ok := executors.(*relayExecutors); ok {
				source.remote = remote
			}
		case errors.As(openErr, &needsChoice):
			// relay（可能 0 个执行端）：界面直接进选择列表，选中后 RetryNow 重连
			remote, remoteErr := host.NewRemoteDeferredToken(dial, post, opts.relayToken)
			if remoteErr != nil {
				showFatal(locale.Tr("dialog.config.title"), remoteErr.Error())
				return
			}
			remoteHost = remote
			source := &relayExecutors{addr: connectAddr, current: pinned, token: opts.relayToken}
			source.remote = remote
			executors = source
		default:
			showFatal(locale.Tr("dialog.config.title"), openErr.Error())
			return
		}
	}

	runtime := &appRuntime{
		registry:   registry,
		provider:   provider,
		allowShell: cfg.AllowShell,
	}

	app := uiapp.NewApp(runtime, uiapp.Options{
		Host:          remoteHost,
		Executors:     executors,
		Selection:     selection,
		SystemPrompt:  systemPrompt(cfg),
		AllowShell:    cfg.AllowShell,
		// --token 同时用于"直连执行端的认证"与"注册 relay 的认证"。
		// 两个场景用同一个口令更方便（用户只记一个），语义上也一致：
		// 都是"能出示口令的人可以操作这个执行端"。
		Token:         opts.relayToken,
		// 执行端对外显示的名字：空则用机器名（展示端界面靠它区分"连的是哪台"）
		Name:          opts.name,
		Workspace:     cfg.Workspace,
		MaxToolRounds: cfg.MaxToolRounds,
		ContextWindow: windowFor(registry, selection),
		WindowFor: func(sel providers.Selection) int {
			return windowFor(registry, sel)
		},
		MaxOutputFor: func(sel providers.Selection) int {
			return maxOutputFor(registry, sel)
		},
		EffortFor: func(sel providers.Selection) string {
			return effortFor(registry, sel)
		},
		VisionFor: func(sel providers.Selection) bool {
			return visionFor(registry, sel)
		},
		Language:       lang,
		PasteImages:    pasteImages,
		PickImageFiles: pickImageFiles,
		PickDirectory:  pickDirectory,
		Blobs:          blobs,
		Title:          locale.Tr("session.new"),
		Providers:      providerChoices(registry),
		Catalog:        catalog,
		// 界面偏好（正文宽度等）是**本机**的：存系统应用配置目录，
		// 不写执行端的 data_dir（远程模式下展示端也要有自己的偏好）。
		UIStatePath: uistate.Path(),
		// Window.Update 在 mygo 里等于「postMain + contentChanged」：
		// 回调在主线程执行，随后请求重建内容（流式 token 走这里）。
		Post: post,
	})

	var stopListener func()
	if listenAddr != "" {
		stop, err := startListener(listenAddr, app, post)
		if err != nil {
			showFatal(locale.Tr("dialog.config.title"), err.Error())
			return
		}
		stopListener = stop
		defer stopListener()
	}

	// 执行端注册到 relay（有界面也可以）：断线自动重连并重新注册
	stopRelay := make(chan struct{})
	if opts.relay != "" {
		if local, ok := app.Host().(*host.Local); ok {
			go relay.RunExecutor(opts.relay, relay.Executor{
				HostID: hostID(opts.name), Token: opts.relayToken,
			}, stopRelay, func(conn wire.Conn) {
				_ = host.Serve(conn, local, post)
			})
			defer close(stopRelay)
		}
	}

	// 配置里的 language 值无法识别：给一条显示型提示（不落盘），说明已跟随系统。
	if !langRecognized {
		app.NotifyDisplay(locale.Tr("notice.unknownLanguage", cfg.Language))
	}

	// 第二个实例被拦下时：把窗口拉到前台，并在当前会话里留一条提示。
	mygo.App.OnSecondInstance(func(_ []string, _ string) {
		app.NotifyDisplay(locale.Tr("app.switched"))
		app.RequestRedraw()
		if window != nil {
			window.Show()
			window.Focus()
		}
	})

	mygo.App.WhenReady(func() {
		window = mygo.NewWindow(mygo.WindowOptions{
			Title:   "simple-agent",
			Width:   1180,
			Height:  760,
			Content: ui.View(app.View),
		})
		// 窗口就绪：投递之前排队的回调
		queued := pending
		pending = nil
		for _, f := range queued {
			window.Update(f)
		}
	})

	sessionErr := app.StartupError()
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
	app.Close()
	if sessionErr != "" {
		log.Print(locale.Tr("startup.problem", sessionErr))
	}
}

// notifyLocked 提示数据目录已被占用后退出。
func notifyLocked(err error) {
	log.Printf("无法打开数据目录：%v", err)
	mygo.App.WhenReady(func() {
		_, _ = mygo.Dialog.Message(mygo.MessageOptions{
			Type:    mygo.MessageWarning,
			Title:   "simple-agent",
			Message: locale.Tr("dialog.locked.message"),
			Detail:  err.Error(),
		})
		mygo.App.Exit(0)
	})
	_ = mygo.App.Run()
}

// notifyAlreadyRunning 提示「已有实例在运行」后退出。
//
// 用对话框而不是日志：发布版没有控制台，日志没人看得见。
// 对话框必须在事件循环起来之后才能用（mygo 的对话框走主线程），
// 所以走 WhenReady + Run，弹出后立即退出。
func notifyAlreadyRunning() {
	mygo.App.WhenReady(func() {
		_, _ = mygo.Dialog.Message(mygo.MessageOptions{
			Type:    mygo.MessageInfo,
			Title:   "simple-agent",
			Message: locale.Tr("dialog.running.title"),
			Detail:  locale.Tr("dialog.running.message"),
		})
		mygo.App.Exit(0)
	})
	if err := mygo.App.Run(); err != nil {
		log.Printf("提示窗口未能显示：%v", err)
	}
}

// showFatal 报告致命错误并退出。
//
// 配置错误发生在事件循环之前，对话框还不能用（mygo 要求 WhenReady 之后），
// 所以这里走 WhenReady + Run，弹完就退。
func showFatal(title, text string) {
	log.Printf("%s：%s", title, text)
	mygo.App.WhenReady(func() {
		_, _ = mygo.Dialog.Message(mygo.MessageOptions{
			Type:    mygo.MessageError,
			Title:   title,
			Message: text,
		})
		mygo.App.Exit(1)
	})
	_ = mygo.App.Run()
}

// errNoExecutorChosen 表示还没选执行端（界面在选择列表里）。
var errNoExecutorChosen = errors.New("尚未选择执行端")

// pinnedHost 是"当前选中的执行端"（dial 闭包与界面共享，加锁访问）。
type pinnedHost struct {
	sync.Mutex
	hostID string
}

// appRuntime 把 providers 与 tools 接到 uiapp.Runtime 上。
type appRuntime struct {
	registry   *providers.Registry
	provider   providers.Provider
	allowShell bool
}

// OpenStream 发起一次流式请求。
func (r *appRuntime) OpenStream(ctx context.Context, sel providers.Selection, req providers.ChatRequest) (<-chan providers.Event, error) {
	provider := r.provider
	if sel.ProviderID != provider.Config().ID {
		built, err := r.registry.Build(sel)
		if err != nil {
			return nil, err
		}
		provider = built
	}
	return provider.Stream(ctx, req)
}

// RunTools 执行工具调用。
func (r *appRuntime) RunTools(ctx context.Context, tc *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
	return tools.ExecuteAll(ctx, tools.Builtin(r.allowShell), tc, calls)
}

// windowFor 返回某模型配置的上下文窗口（0 表示未知，driver 会按 32k 处理）。
func windowFor(registry *providers.Registry, sel providers.Selection) int {
	if registry == nil {
		return 0
	}
	entry, ok := registry.Get(sel.ProviderID)
	if !ok {
		return 0
	}
	return entry.Config.ContextWindow(sel.Model)
}

// maxOutputFor 返回某模型配置的最大输出 token 数（0 表示不限制）。
func maxOutputFor(registry *providers.Registry, sel providers.Selection) int {
	if registry == nil {
		return 0
	}
	entry, ok := registry.Get(sel.ProviderID)
	if !ok {
		return 0
	}
	return entry.Config.MaxOutputOf(sel.Model)
}

// visionFor 返回某模型是否支持图片（未配置 = 不支持）。
func visionFor(registry *providers.Registry, sel providers.Selection) bool {
	if registry == nil {
		return false
	}
	entry, ok := registry.Get(sel.ProviderID)
	if !ok {
		return false
	}
	return entry.Config.SupportsVision(sel.Model)
}

// effortFor 返回某模型配置的默认思考档位（"" 表示沿用服务端默认）。
//
// 配置里写 reasoning_effort: off 时，registry 已经把这个键从请求参数里去掉，
// 这里返回 "off" 让界面显示「关闭」。
func effortFor(registry *providers.Registry, sel providers.Selection) string {
	if registry == nil {
		return ""
	}
	entry, ok := registry.Get(sel.ProviderID)
	if !ok {
		return ""
	}
	efforts := entry.Config.Efforts(sel.Model)
	if len(efforts) == 0 {
		return ""
	}
	if effort, ok := entry.Config.ModelExtra(sel.Model)["reasoning_effort"].(string); ok {
		return effort
	}
	for _, effort := range efforts {
		if effort != "off" { // 优先选第一个非「关闭」档位
			return effort
		}
	}
	return efforts[0]
}

// systemPrompt 组合系统提示。
//
// 结构对齐 deepseek-harness 的提示词分层：
//   - 固定身份与基本约定（相当于 HARNESS_IDENTITY + persona）；
//   - **工具使用指导**：从实际注册的工具里取（`tools.GuidanceSection`），
//     所以关掉 shell 时不会再有 bash/pwsh 的指导——这是 dsh 的
//     「段落随工具可见性消失」性质，也是之前手写两份提示词最容易漏的地方；
//   - 用户自己追加的 `system_prompt`。
//
// 工具指导用英文：这些句子直接取自 dsh 的原文，保持逐字一致便于与上游对照。
func systemPrompt(cfg config.Config) string {
	base := `你是 simple-agent，一个运行在用户本机的桌面 AI 助手。
- 用中文回答，除非用户使用其它语言。
- 路径一律使用相对会话工作目录的相对路径。
- 任务步骤较多时先用 todo_write 列出计划，并在推进过程中更新状态。`
	if !cfg.AllowShell {
		// 工具不可用时要说清楚：否则模型会去调用一个不存在的工具反复试错
		base += `
- 命令执行当前**不可用**：不要调用 bash/pwsh，改用文件与搜索工具。`
	} else {
		base += `
- 执行命令前先说明目的；危险操作（删除、覆盖、安装）要先征得用户同意。`
	}
	if guidance := tools.GuidanceSection(tools.Builtin(cfg.AllowShell)); guidance != "" {
		base += "\n\n" + guidance
	}
	if prompt := cfg.SystemPrompt; prompt != "" {
		base += "\n\n" + prompt
	}
	return base
}

// providerChoices 构造界面上可选的提供商与模型（只含已配置的，外加模拟）。
//
// registry 为 nil 时返回空（headless 的单测会以 nil 构造配置，模型列表是可选信息，
// 不该让它把整条启动路径搞崩）。
func providerChoices(registry *providers.Registry) []uiapp.ProviderChoice {
	if registry == nil {
		return nil
	}
	choices := make([]uiapp.ProviderChoice, 0, len(registry.All()))
	for _, entry := range registry.Configured() {
		efforts := map[string][]string{}
		labels := map[string]string{}
		// Anthropic Messages API 没有 reasoning_effort 字段（多传会 400），
		// 因此这类提供商不暴露档位选择，配置里写了也不生效。
		supportsEffort := entry.Config.Kind != providers.KindAnthropic
		for _, model := range entry.Config.Models {
			if supportsEffort && len(model.ReasoningEfforts) > 0 {
				efforts[model.ID] = model.ReasoningEfforts
			}
			if model.Label != "" {
				labels[model.ID] = model.Label
			}
		}
		choices = append(choices, uiapp.ProviderChoice{
			ProviderID: entry.Config.ID,
			Label:      entry.Config.Label,
			Models:     entry.Config.ModelIDs(),
			Labels:     labels,
			Efforts:    efforts,
		})
	}
	simulated := providers.SimulatedConfig()
	choices = append(choices, uiapp.ProviderChoice{
		ProviderID: simulated.ID,
		Label:      simulated.Label,
		Models:     simulated.ModelIDs(),
	})
	return choices
}
