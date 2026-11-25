package main

import (
	"fmt"
	"log"
	"os"
	"sync/atomic"

	"simple-agent/internal/config"
	"simple-agent/internal/peerverify"
	"simple-agent/internal/tlsx"
	"simple-agent/internal/trust"
	"simple-agent/internal/wire"
)

// setupTLS 按配置装配进程级 TLS（默认不启用）。
//
// 三件事：
//  1. 服务端侧（--tls 或 --tls-cert/--tls-key）：证书不存在就自签生成，并打印指纹；
//     `--tls` 单独用时证书落在 `<data_dir>/tls/`，重启复用，指纹稳定。
//  2. 客户端侧（--tls-fingerprint）：装到 wire 的拨号钩子上，
//     所有 wire.Dial 调用点（relay 客户端、执行端直连）自动走 TLS；
//  3. --tls-print-fingerprint：只打印指纹后退出，方便把指纹填到客户端。
//
// viewer 侧的指纹确认（net 层实现见 internal/peerverify）。
//
// 全局变量而不是层层传参：`OnPeerFingerprint` 与 `wire.DialFunc` 都是
// 全局钩子，顺着调用链传会污染一大片签名。
var (
	peerGate       *peerverify.Gate
	activeVerifier *peerverify.Verifier
	// uiReady 表示界面已经创建好、可以弹窗了。
	//
	// 为什么需要：setupTLS 必须在**任何连接建立之前**执行（它装拨号钩子），
	// 而界面要更晚才创建。执行端一启动就去注册 relay，那时如果遇到
	// 未确认指纹，弹窗无处可弹 —— 连接会挂在那儿等到进程结束。
	//
	// 所以 Gate.Ready 查这个标志：还没就绪就立刻返回"暂时无人可问"，
	// 注册循环退避重试，等界面起来那一轮自然弹窗。
	uiReady atomic.Bool
)

// setupTLS 按命令行/配置装好 TLS。
//
// 返回 false 表示应该直接退出（打印指纹模式）。
func setupTLS(cfg *config.Config, opts remoteOptions) bool {
	tlsCfg := tlsx.Config{
		CertFile:    firstNonEmptyStr(opts.tlsCert, cfg.TLS.CertFile),
		KeyFile:     firstNonEmptyStr(opts.tlsKey, cfg.TLS.KeyFile),
		Fingerprint: firstNonEmptyStr(opts.tlsFingerprint, cfg.TLS.Fingerprint),
		// **--tls 同时启用拨号加密**，不要求已知指纹。
		//
		// 用户的要求："viewer 只需要加 --tls 就行，指纹并不是必须参数，
		// 连上 relay 之后提示即可"。所以这里把"用不用 TLS"与"信任哪个
		// 指纹"分开：前者是开关，后者由 TOFU 在连接时确认。
		ClientTLSEnabled: opts.tlsEnabled,
	}
	// 只给了 --tls（或配置里 enable: true）而没给路径：用数据目录下的默认位置
	serverWanted := opts.tlsEnabled || cfg.TLS.ServerEnabled()
	if serverWanted && tlsCfg.CertFile == "" && tlsCfg.KeyFile == "" {
		tlsCfg.DataDir = cfg.DataDir
	}

	// 只打印指纹：生成/读取证书后打印，然后退出
	if opts.printFingerprint {
		if !tlsCfg.ServerTLS() {
			fmt.Fprintln(os.Stderr, "打印指纹需要 --tls（或 --tls-cert 与 --tls-key）")
			os.Exit(2)
		}
		fingerprint, err := tlsCfg.EnsureCert()
		if err != nil {
			fmt.Fprintf(os.Stderr, "生成/读取证书失败：%v\n", err)
			os.Exit(1)
		}
		certFile, _ := tlsCfg.CertPaths()
		fmt.Printf("证书：%s\n", certFile)
		fmt.Printf("指纹：%s\n", tlsx.FormatFingerprint(fingerprint))
		fmt.Printf("原始：%s\n", fingerprint)
		fmt.Println()
		fmt.Println("把指纹填到客户端：")
		fmt.Printf("  simple-agent --connect HOST:PORT --tls-fingerprint %s\n", fingerprint)
		return false
	}

	if !serverWanted && !tlsCfg.ClientTLS() {
		// 默认不开启：一切照旧走明文 TCP
		return true
	}

	// 服务端启用了 TLS：确保证书存在（没有就自签），把指纹打出来
	if serverWanted {
		fingerprint, err := tlsCfg.EnsureCert()
		if err != nil {
			// TLS 配置错误不该让程序静默降级成明文——那会让用户以为加密了
			log.Printf("TLS：证书准备失败，无法启用：%v", err)
			os.Exit(1)
		}
		certFile, _ := tlsCfg.CertPaths()
		log.Printf("TLS：已启用服务端加密（证书 %s）", certFile)
		log.Printf("TLS：证书指纹 %s", tlsx.FormatFingerprint(fingerprint))
		log.Printf("TLS：客户端需要加 --tls-fingerprint %s", fingerprint)
	}

	// 客户端启用了 TLS：装到 wire 的拨号钩子上
	if tlsCfg.ClientTLS() {
		// **viewer 要有自己的证书**（mTLS：执行端也要验证我们）。
		//
		// 与服务端共用 `<DataDir>/tls` —— 同一台机器上 viewer 和执行端是
		// 同一个用户、同一个信任域，"自己连自己"的指纹天然等于本机指纹，
		// 执行端判定为已知身份、不弹窗。换一台机器 DataDir 不同，证书也就
		// 不同，那才需要用户确认。
		//
		// **赋值 DataDir 而不是复制各个字段**：own 只是
		// `tlsx.Config{DataDir: …}`，它的 CertDir/CertFile/KeyFile 都是空的；
		// 逐个复制等于什么都没设置（踩过）。
		if fingerprint := viewerCertificate(cfg.DataDir); fingerprint != "" {
			tlsCfg.DataDir = cfg.DataDir
			log.Printf("TLS：viewer 身份 %s", describeViewerIdentity(fingerprint))
		}
		if _, err := tlsCfg.ClientConfig(); err != nil {
			log.Printf("TLS：客户端配置有误：%v", err)
			os.Exit(1)
		}
		// **TOFU：校验对端指纹**（查信任列表；首次/变了 → 返回待确认错误）。
		//
		// 用户要求"viewer 只需要加 --tls 就行，指纹并不是必须参数" ——
		// 但"不要求指纹"不等于"不校验"，否则谁都能冒充 relay/执行端。
		//
		// 这里**不阻塞、不自动信任**：遇到未确认的对端就返回
		// 指纹确认（net 层实现，见 internal/peerverify）：
		//
		// **握手期间阻塞等用户确认** —— 用户同意则握手继续、连接成功；
		// 取消则握手失败、连接建立不起来。所以用户看到的是"我同意了就连上"，
		// 而不是"先失败、又自己重连"。
		//
		// 等待**不计入任何超时**：网络 IO 有自己的超时，但人看弹窗的速度
		// 不是网络速度 —— 给"等用户"设上限的话，去倒杯水回来就被判成取消了。
		store, trustErr := trust.Open(trust.DefaultPath(cfg.DataDir))
		if trustErr != nil {
			log.Printf("TLS：信任列表读取失败（将重新确认所有对端）：%v", trustErr)
		}
		// 有新的待确认对端时界面会自动弹窗（帧循环每帧查一次 Pending），
		// 所以这里不需要额外的通知回调。
		peerGate = &peerverify.Gate{
			// 界面还没起来时不要登记等待（见 uiReady 的注释）
			Ready: func() bool { return uiReady.Load() },
		}
		verifier := &peerverify.Verifier{
			Store:  store,
			Role:   trust.RoleRelay,
			Pinned: tlsCfg.Fingerprint,
			Ask:    peerGate.Ask,
		}
		activeVerifier = verifier
		tlsCfg.OnPeerFingerprint = verifier.Verify

		wire.SetDialFunc(tlsCfg.Dial)
		if tlsCfg.HasFingerprint() {
			log.Printf("TLS：拨号将校验服务端指纹 %s",
				tlsx.FormatFingerprint(tlsCfg.Fingerprint))
		} else {
			// **没有指纹是允许的**（用户要求"加 --tls 就行"）。
			// 校验走 TOFU：连上后把对端指纹记进信任列表，下次用它严格校验。
			// 见 viewer_tofu.go —— 那里才是真正的校验点。
			log.Printf("TLS：未指定对端指纹，将按 TOFU 校验（首次连接后记入信任列表）")
		}
	} else if serverWanted {
		// 只开了服务端：自己不该用 TLS 拨号（拨号由客户端侧决定），
		// 但同一进程既是执行端又是展示端时要小心——这里保持明文拨号并提示。
		log.Printf("TLS：当前进程只启用了服务端加密；如果需要拨号也加密，请加 --tls-fingerprint")
	}

	// 监听侧走 tlsx（服务端 TLS 时生效，否则就是普通 listen）
	tlsx.SetActive(tlsCfg)
	return true
}

// firstNonEmptyStr 返回第一个非空字符串。
func firstNonEmptyStr(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
