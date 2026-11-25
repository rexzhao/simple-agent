package main

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"

	"simple-agent/internal/trust"
)

// viewerTOFU 是 viewer 连出去时的指纹校验策略。
//
// # 为什么需要
//
// 用户要求："viewer 只需要加 --tls 就行，指纹并不是必须参数，连上 relay
// 之后提示即可"。
//
// 但"不要求指纹"不能等于"不校验"—— 那样任何人都能冒充 relay/执行端。
// 所以走 TOFU：
//
//	已知指纹（命令行给了 / 信任列表里记着）→ 严格校验，不符就断开
//	首次见到（两者都没有）→ 接受，但**记进信任列表并提示用户**
//
// 与执行端验证 viewer 用的是同一份信任列表（`known_hosts`），角色分开记
// （relay / executor / viewer），因为同一个地址可能同时扮演多个角色。
type viewerTOFU struct {
	store *trust.Store
	role  trust.Role
	// addr 是对端地址（信任列表的 key）
	addr string
	// pinned 是**已确认**的指纹。非空时必须严格匹配。
	pinned string
	// learned 记录本次连接实际见到的指纹（首次连接时填）。
	learned string
	// callback 在学到新指纹时调用（界面据此提示用户）。
	callback func(addr, fingerprint string)
}

// verify 返回 tls.Config 用的校验函数。
//
// 关键点：**pinned 为空时不拒绝**，而是把见到的指纹记下来交给 callback。
// 这里不能静默接受 —— callback 必须提示用户"已信任这台"，
// 否则用户根本不知道自己在和谁通信。
func (v *viewerTOFU) verify(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return fmt.Errorf("对端 %s 没有提供证书", v.addr)
	}
	sum := sha256.Sum256(rawCerts[0])
	got := fmt.Sprintf("%x", sum[:])

	// 1. 命令行给了指纹：严格校验（用户明确指定，不容含糊）
	if v.pinned != "" {
		if normalizeFingerprint(v.pinned) != got {
			return fmt.Errorf("证书指纹不匹配：%s 实际是 %s，期望 %s",
				v.addr, trust.Format(got), trust.Format(v.pinned))
		}
		return nil
	}

	// 2. 信任列表里记着：同样严格校验；**变了必须报错**
	//    （那可能是换了证书，也可能是冒充 —— 都不能默默接受）
	if v.store != nil {
		status, err := v.store.Check(v.role, v.addr, got)
		if status == trust.StatusChanged {
			return err
		}
		if status == trust.StatusKnown {
			return nil
		}
	}

	// 3. 首次见到：接受，但记下来让界面提示用户
	v.learned = got
	if v.callback != nil {
		v.callback(v.addr, got)
	}
	return nil
}

// clientTLSConfig 构造带 TOFU 校验的 TLS 配置。
//
// ownCert 是本机证书（要出示给对端）。没有它时对端会拒绝我们 ——
// 那是 mTLS 的正常行为，不是错误。
func (v *viewerTOFU) clientTLSConfig(ownCert *tls.Certificate) *tls.Config {
	cfg := &tls.Config{
		InsecureSkipVerify:    true,
		MinVersion:            tls.VersionTLS12,
		VerifyPeerCertificate: v.verify,
	}
	if ownCert != nil && len(ownCert.Certificate) > 0 {
		cfg.Certificates = []tls.Certificate{*ownCert}
	}
	return cfg
}

// trustLearned 把本次学到的指纹写进信任列表。
//
// **只在连接成功后调用**：握手通过但业务层失败（口令错、被拒）时不该
// 把它记成"已信任"—— 那等于给伪造者留了后门。
func (v *viewerTOFU) trustLearned() {
	if v.learned == "" || v.store == nil {
		return
	}
	if err := v.store.Trust(v.role, v.addr, v.learned); err != nil {
		log.Printf("信任列表写入失败（%s）：%v", v.addr, err)
		return
	}
	log.Printf("已信任 %s %s（指纹 %s）", v.role, v.addr, trust.Format(v.learned))
}

// normalizeFingerprint 去冒号/空格并转小写。
func normalizeFingerprint(value string) string {
	out := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch c {
		case ':', ' ', '-':
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}