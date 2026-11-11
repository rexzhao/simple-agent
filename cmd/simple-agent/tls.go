package main

import (
	"fmt"
	"log"
	"os"

	"simple-agent/internal/config"
	"simple-agent/internal/tlsx"
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
// 返回 false 表示应该直接退出（打印指纹模式）。
func setupTLS(cfg *config.Config, opts remoteOptions) bool {
	tlsCfg := tlsx.Config{
		CertFile:    firstNonEmptyStr(opts.tlsCert, cfg.TLS.CertFile),
		KeyFile:     firstNonEmptyStr(opts.tlsKey, cfg.TLS.KeyFile),
		Fingerprint: firstNonEmptyStr(opts.tlsFingerprint, cfg.TLS.Fingerprint),
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
		if _, err := tlsCfg.ClientConfig(); err != nil {
			log.Printf("TLS：客户端配置有误：%v", err)
			os.Exit(1)
		}
		wire.SetDialFunc(tlsCfg.Dial)
		log.Printf("TLS：拨号将校验服务端指纹 %s", tlsx.FormatFingerprint(tlsCfg.Fingerprint))
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
