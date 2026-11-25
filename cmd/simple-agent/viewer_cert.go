package main

import (
	"fmt"
	"log"

	"simple-agent/internal/tlsx"
)

// viewerCertificate 确保本机证书存在（没有就生成），返回指纹。
//
// # 为什么 viewer 需要自己的证书
//
// mTLS 是双向的：**执行端也要验证 viewer**。执行端凭什么认你？凭你出示的
// 证书指纹。所以每个 viewer 必须有稳定身份 —— 不能每次启动现生成新的
// （那样执行端每次都要问一遍"你是谁，让进吗"，用户会被问疯）。
//
// # 与执行端共用一张证书
//
// 证书落在 `<DataDir>/tls/`，**和执行端是同一份**。这不是偷懒：
//   - 同一台机器上，viewer 和执行端本来就是同一个用户、同一个信任域
//   - 共用证书后"自己连自己"的指纹天然等于本机指纹 → 执行端判定为已知
//     身份、**不弹窗**（比按回环 IP 豁免更准确：IP 可伪造，证书不能）
//   - 不同机器上 DataDir 不同，各有一份证书、互不相同 —— 那才需要确认
//
// 换句话说：指纹相同 = 同一身份 = 默认信任；指纹不同 = 别人 = 要确认。
func viewerCertificate(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	fingerprint, err := (tlsx.Config{DataDir: dataDir}).EnsureCert()
	if err != nil {
		// 生成失败不该拦住启动：没有自己的证书时退化回单向校验
		// （只验对端，不向对端证明自己）。执行端会拒绝这种连接，
		// 用户看到的是明确的"没出示证书"而不是莫名其妙的失败。
		log.Printf("viewer 证书生成失败（将不向执行端出示身份）：%v", err)
		return ""
	}
	return fingerprint
}

// describeViewerIdentity 返回给日志用的一句话说明。
func describeViewerIdentity(fingerprint string) string {
	if fingerprint == "" {
		return "无证书（执行端只能靠 token 认证）"
	}
	return fmt.Sprintf("%s…", fingerprint[:16])
}