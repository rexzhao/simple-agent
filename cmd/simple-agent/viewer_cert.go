package main

import (
	"fmt"
	"log"

	"simple-agent/internal/tlsx"
)

// viewerCertificate 确保 viewer 有自己的证书，返回 (证书配置, 指纹)。
//
// # 为什么 viewer 需要自己的证书
//
// mTLS 是双向的：**执行端也要验证 viewer**。执行端凭什么认你？凭你出示的
// 证书指纹。所以每个 viewer 必须有稳定身份 —— 不能每次启动现生成新的
// （那样执行端每次都要问一遍"你是谁，让进吗"，用户会被问疯）。
//
// # 与执行端共用一张证书（同一台机器时）
//
// 证书落在 `<DataDir>/tls/`，**和执行端是同一份**。这不是偷懒：
//   - 同一台机器上，viewer 和执行端本来就是同一个用户、同一个信任域
//   - 共用证书后，"自己连自己"天然就是同一个指纹 → 执行端判定为已知身份，
//     **不弹窗**（比按回环地址豁免更准确：IP 可以伪造，证书不能）
//   - 不同机器上 DataDir 不同，各有一份证书，互不相同 —— 那才是需要确认的
//
// 换句话说：指纹相同 = 同一身份 = 默认信任；指纹不同 = 别人 = 要确认。
//
// # 自动生成而不是要求用户配置
//
// viewer 通常是随手启动的（尤其手机/另一台机器），要求先跑个命令生成证书
// 再填指纹，等于把 mTLS 的复杂度摊给用户。首次启动静默生成、之后固定，
// 用户只需要在**执行端**点一次"允许"。
func viewerCertificate(dataDir string) (tlsx.Config, string) {
	if dataDir == "" {
		return tlsx.Config{}, ""
	}
	// 与执行端同一个目录（见上面注释：同一台机器 = 同一身份）
	cfg := tlsx.Config{DataDir: dataDir}
	fingerprint, err := cfg.EnsureCert()
	if err != nil {
		// 生成失败不该拦住启动：没有自己的证书时退化回单向校验
		// （只验对端，不向对端证明自己）。
		log.Printf("viewer 证书生成失败（将不向执行端出示身份）：%v", err)
		return tlsx.Config{}, ""
	}
	return cfg, fingerprint
}

// viewerTLSConfig 构造 viewer 的拨号 TLS 配置：
//   - 校验对端（relay 或执行端的指纹）
//   - 出示自己的证书（供执行端验证）
//
// peerFingerprint 为空表示不校验对端（明文）；own 为空表示没有自己的身份。
func viewerTLSConfig(peerFingerprint string, own tlsx.Config) tlsx.Config {
	cfg := own
	cfg.Fingerprint = peerFingerprint
	return cfg
}

// describeViewerIdentity 返回给日志用的一句话说明。
func describeViewerIdentity(fingerprint string) string {
	if fingerprint == "" {
		return "无证书（执行端只能靠 token 认证）"
	}
	return fmt.Sprintf("%s…", fingerprint[:16])
}