package main

import (
	"fmt"

	"log"
	"net"
	"os"
	"os/signal"
	"simple-agent/internal/locale"
	"sync"
	"syscall"
	"time"

	"simple-agent/internal/config"
	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/relay"
	"simple-agent/internal/store"
	"simple-agent/internal/tlsx"
	"simple-agent/internal/wire"
)

// runHeadless 以无界面方式运行执行端（§8）。
//
// 生命周期：启动 → 监听（可选注册到 relay）→ 等待 SIGINT/SIGTERM →
// 停掉在跑的回合 → Sync → 关闭监听 → 退出。启动失败返回非 0。
func runHeadless(cfg *config.Config, runtime *appRuntime, selection providers.Selection,
	listenAddr, relayAddr, name, relayToken string, netLog bool, catalog *store.Catalog, blobs *store.Blobs) int {
	host.EnableNetLog(netLog)
	// headless 在 main 的界面语言解析之前就退出了：这里自己设置一次，
	// 否则新建会话的默认标题会显示成 locale key（session.defaultTitle）。
	if lang, recognized := locale.Parse(cfg.Language); recognized {
		locale.SetCurrent(lang)
	} else {
		locale.SetCurrent(locale.Detect())
	}

	log.Printf("headless：配置 %s，提供商 %s/%s，数据目录 %s",
		cfgPathForLog(cfg), selection.ProviderID, selection.Model, cfg.DataDir)

	post := serialPost()
	local := host.NewLocal(headlessHostConfig(cfg, runtime, selection, catalog, blobs, post, relayToken))
	defer local.Close()

	if startup := local.StartupError(); startup != "" {
		log.Printf("headless：启动告警：%s", startup)
	}

	// 每回合结束打一行摘要（session / 耗时 / 是否成功）
	local.Subscribe(turnLogger())

	var stopListener func()
	if listenAddr != "" {
		stop, _, err := listenHeadless(listenAddr, local, post)
		if err != nil {
			log.Printf("headless：监听失败：%v", err)
			return 1
		}
		stopListener = stop
		defer stopListener()
	}

	stopRelay := make(chan struct{})
	if relayAddr != "" {
		go relay.RunExecutor(relayAddr, relay.Executor{
			HostID: hostID(name), Token: relayToken,
		}, stopRelay, func(conn wire.Conn) {
			_ = host.Serve(conn, local, post)
			log.Printf("headless：展示端已断开（观察者剩余 %d）", local.ObserverCountForTest())
		})
		log.Printf("headless：正在向 relay %s 注册（host_id=%s）", relayAddr, hostID(name))
		if relayToken == "" {
			log.Printf("headless：警告：relay 没有配置 --token，任何能连上 relay 的人都可以操作这个执行端")
		} else {
			log.Printf("headless：已启用 relay token 认证")
		}
	}
	if listenAddr == "" && relayAddr == "" {
		log.Printf("headless：既没有 --listen 也没有 --relay，只做本地运行（没有远程入口）")
	}

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	sig := <-signals
	log.Printf("headless：收到 %v，正在停止…", sig)

	// 先停掉在跑的回合（driver.Stop 会落盘），再关闭会话日志
	for _, ref := range local.Sessions() {
		if session := local.Session(ref.ID); session != nil {
			_, _ = local.Submit(host.Command{SessionID: ref.ID, Kind: "stop"})
		}
	}
	close(stopRelay)
	if stopListener != nil {
		stopListener()
	}
	if err := local.Close(); err != nil {
		log.Printf("headless：关闭会话日志失败：%v", err)
		return 1
	}
	log.Printf("headless：已退出")
	return 0
}

// headlessHostConfig 把配置转成执行端的 host.Config。
//
// 单独抽出来是为了能被单测覆盖：这里的字段**漏一个就是静默的功能缺失**
// （典型是 allow_shell 没传 → 工具列表里根本不注册 bash/pwsh，而系统提示仍然
// 写着"可以执行命令"，模型只能拿回「未知工具」）。
func headlessHostConfig(cfg *config.Config, runtime *appRuntime, selection providers.Selection,
	catalog *store.Catalog, blobs *store.Blobs, post func(func()), token string) host.Config {
	return host.Config{
		Catalog:      catalog,
		Blobs:        blobs,
		Runtime:      runtime,
		Selection:    selection,
		SystemPrompt: systemPrompt(*cfg),
		// 权限是启动期全局开关：必须原样传给执行端，否则 --allow-shell 在
		// --headless 模式下会被悄悄丢掉（界面模式由 uiapp 传，不要照抄成两套口径）。
		AllowShell:    cfg.AllowShell,
		Workspace:     cfg.Workspace,
		MaxToolRounds: cfg.MaxToolRounds,
		ContextWindow: windowFor(runtime.registry, selection),
		Post:          post,
		Tr:            locale.Tr,
		EffortFor:     func(sel providers.Selection) string { return effortFor(runtime.registry, sel) },
		VisionFor:     func(sel providers.Selection) bool { return visionFor(runtime.registry, sel) },
		MaxOutputFor:  func(sel providers.Selection) int { return maxOutputFor(runtime.registry, sel) },
		// 模型列表随 catalog 发给展示端：headless 也要给，否则远程端切不了模型。
		Models: providerChoices(runtime.registry),
		// 执行端自己的 token：`--listen` 直连时校验展示端握手带来的口令。
		// 与 relay 的 token 是两套，互不影响。
		Token: token,
	}
}

// listenHeadless 起一个 TCP 监听，每条连接交给 host.Serve。
func listenHeadless(addr string, local *host.Local, post func(func())) (func(), string, error) {
	listener, err := tlsx.Listen(addr)
	if err != nil {
		return nil, "", err
	}
	log.Printf("headless：执行端已监听 %s", listener.Addr())
	warnIfUnprotected(listener.Addr().String())
	var mu sync.Mutex
	open := map[net.Conn]bool{}
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			mu.Lock()
			open[conn] = true
			mu.Unlock()
			go func() {
				defer func() {
					mu.Lock()
					delete(open, conn)
					mu.Unlock()
					_ = conn.Close()
				}()
				_ = host.Serve(wire.NewTCPConn(conn), local, post)
				log.Printf("headless：展示端已断开（观察者剩余 %d）", local.ObserverCountForTest())
			}()
		}
	}()
	stop := func() {
		_ = listener.Close()
		mu.Lock()
		for conn := range open {
			_ = conn.Close()
		}
		mu.Unlock()
	}
	return stop, listener.Addr().String(), nil
}

// turnLogger 在回合结束时打一行摘要。
func turnLogger() func(host.Change) {
	started := map[string]time.Time{}
	return func(change host.Change) {
		switch change.Kind {
		case "conversation":
			return
		case "cmdResult":
			if change.Result.Kind == "send" && !change.Result.OK && change.Result.Error != "" {
				log.Printf("headless：发送失败（%s）：%s", change.SessionID, change.Result.Error)
			}
		}
		if change.SessionID == "" {
			return
		}
		if _, ok := started[change.SessionID]; !ok {
			started[change.SessionID] = time.Now()
		}
	}
}

// runRelayServer 运行 relay 子命令（只转发，不看会话协议）。
func runRelayServer(listenAddr, token string) int {
	// relay 是控制台工具：有 SIMPLE_AGENT_DATA 时也落一份日志，方便排查转发问题
	closeLog, _ := setupLogging(os.Getenv("SIMPLE_AGENT_DATA"), "relay")
	defer closeLog()
	if listenAddr == "" {
		listenAddr = "127.0.0.1:7900"
	}
	listener, err := tlsx.Listen(listenAddr)
	if err != nil {
		log.Printf("relay：监听失败：%v", err)
		return 1
	}
	defer listener.Close()
	server := relay.New()
	server.Token = token
	log.Printf("relay：已监听 %s", listener.Addr())
	if token == "" {
		log.Printf("relay：警告：没有指定 --token，任何能连上这个端口的人都能连到执行端")
	} else {
		log.Printf("relay：已启用固定 token 认证")
	}

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		log.Printf("relay：正在退出")
		server.Close()
		_ = listener.Close()
	}()
	err = server.ServeAll(func() (wire.Conn, error) {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return nil, wire.ErrClosed
		}
		return wire.NewTCPConn(conn), nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay：%v\n", err)
		return 1
	}
	return 0
}

// serialPost 返回一个**串行**执行器：所有会话状态的修改都排成一队依次执行
// （无界面时没有 mygo 主线程，这是它的替代品；并发执行会让 Conversation 竞态）。
func serialPost() func(func()) {
	queue := make(chan func(), 4096)
	go func() {
		for f := range queue {
			f()
		}
	}()
	return func(f func()) { queue <- f }
}

// hostID 是执行端在 relay 上的名字：显式 --name 优先，否则用主机名。
//
// 同一台机器上跑两个执行端时同名会被 relay 拒绝（不会互相顶掉），需要 --name 区分。
func hostID(name string) string {
	if name != "" {
		return name
	}
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		return hostname
	}
	return "simple-agent"
}

func cfgPathForLog(cfg *config.Config) string {
	return fmt.Sprintf("%s（%d 个提供商）", cfg.DataDir, len(cfg.Providers))
}

// warnIfUnprotected 在监听非回环地址时提醒用户：v1 没有认证。
func warnIfUnprotected(addr string) {
	if shouldWarnUnprotected(addr) {
		log.Printf("执行端警告：正在监听 %s。v1 没有认证，任何能连上这个地址的人都可以操作这个执行端（执行命令、修改文件）", addr)
	}
}

// shouldWarnUnprotected 判断这个监听地址是否"暴露在回环之外"。
//
// 注意：`--listen :7801` 的 host 是空字符串，在 Go 里表示**监听所有网卡**，
// 与 0.0.0.0 等价——恰恰最需要警告，不能当成回环放过。
func shouldWarnUnprotected(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" || host == "::" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback()
	}
	return host != "localhost"
}
