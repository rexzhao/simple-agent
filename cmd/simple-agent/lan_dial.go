package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"simple-agent/internal/lan"
	"simple-agent/internal/peerverify"
	"simple-agent/internal/relay"
	"simple-agent/internal/tlsx"
	"simple-agent/internal/trust"
	"simple-agent/internal/wire"
)

// lanDialTimeout 是单次局域网直连尝试的**网络**超时。
//
// 800ms：局域网内 RST/不可达通常几十毫秒就返回；这个值覆盖慢速 WiFi，
// 又不至于让"不在同一网络"卡太久（那种情况会走到超时才回退 relay，
// 用户等的就是这个时间）。
//
// **它只管网络，不管等用户**（见 dialRaw 的实现）：指纹确认在 TLS
// 握手内部阻塞，那是人的速度 —— 把这个计时套在整条 `cfg.Dial` 上，
// 用户看弹窗超过 800ms 就会被判成"直连失败、回退 relay"。
const lanDialTimeout = 800 * time.Millisecond

// viewerDataDir 是本进程的数据目录（证书与信任列表都在这下面）。
//
// 用全局变量而不是层层传参：它被 dialRaw → tlsx.Config 这条深处的
// 调用链用到，而那条链上每一层都只是转发。进程级配置这样更清楚。
var viewerDataDir string

// verifierStore 返回信任列表（与 relay 校验共用同一份）。
//
// 懒加载：setupTLS 里已经打开过一份，但那条路径在 main 的早期，
// 且不保证只执行一次；这里按需再打开（Open 是只读的，重复打开无害）。
func verifierStore() *trust.Store {
	if viewerTrustStore != nil {
		return viewerTrustStore
	}
	store, err := trust.Open(trust.DefaultPath(viewerDataDir))
	if err != nil {
		log.Printf("信任列表读取失败（将每次重新确认）：%v", err)
		return nil
	}
	return store
}

// openViewer 连到目标执行端：**优先局域网直连**，失败回退 relay。
//
// 返回 (连接, 实际选中的 hostID, 错误)。连接已完成握手（welcome 已垫回），
// 可以直接交给 host.NewRemoteToken。
//
// # 为什么直连要在这里而不是 bridge 里做
//
// mobile/bridge 是给 Android（gomobile）用的那层；桌面端 viewer 走的是
// cmd/simple-agent 里这条独立路径。两边都要各自实现，否则"手机上直连生效、
// 桌面上不生效"（或者反过来）。
//
// # 直连为什么不复用 relay 的拨号
//
// 三个理由，每个都踩过：
//   - **不同的证书**：relay 和执行端是两张自签名证书，拿 relay 的指纹
//     去校验执行端必然失败
//   - **不同的协议**：relay 用 MsgRoute 路由，执行端用 hello/welcome 认证。
//     只建连不握手，连接交给 host.NewRemoteToken 必然握手失败
//   - **不同的 token**：relay 的 token 挡"经过 relay 这一跳"，
//     执行端的 token 挡"直连这一跳"，是两套
func openViewer(relayAddr, target, token, relayFingerprint string, list []relay.Executor) (wire.Conn, string, error) {
	// 1. 有局域网地址就试直连
	if conn, chosen := tryLAN(target, token, list); conn != nil {
		return conn, chosen, nil
	}

	// 2. 回退 relay（原来的路径，一字未改）
	conn, chosen, err := relay.OpenViewerToken(relayAddr, target, token)
	return conn, chosen, err
}

// tryLAN 逐个尝试目标执行端的局域网地址，成功返回已握手的连接。
func tryLAN(target, token string, list []relay.Executor) (wire.Conn, string) {
	for _, executor := range list {
		if target != "" && executor.HostID != target {
			continue
		}
		for _, addr := range executor.LANAddrs {
			if !addr.Dialable() {
				continue
			}
			conn, err := dialLAN(addr, token)
			if err != nil {
				// 打出来：以前这里静默 continue，"为什么没走直连"完全看不出
				log.Printf("展示端：直连 %s 失败（%v），回退 relay", addr.String(), err)
				continue
			}
			log.Printf("展示端：已直连执行端 %s 的局域网地址 %s", executor.HostID, addr.String())
			return conn, executor.HostID
		}
	}
	return nil, ""
}

// dialLAN 拨一条到局域网地址的连接**并完成执行端握手**。
func dialLAN(addr address, token string) (wire.Conn, error) {
	// **没有对端指纹就不直连**（TLS 与明文严格隔离）。
	//
	// 以前这里会"没有指纹就走明文"——那是个降级漏洞：攻击者只要让 TLS
	// 握手失败（丢包、伪造证书链），客户端就会自动改用无校验的明文连接，
	// 指纹体系形同虚设。
	//
	// 现在的规则：**加密的和明文的不能互相通讯**。对端没在 lan_addrs 里
	// 报指纹 = 它是明文执行端 = 本客户端不直连它（走 relay 或干脆连不上，
	// 由用户自己决定要不要配置）。
	if addr.TLSFingerprint == "" {
		return nil, fmt.Errorf("%s 没有上报证书指纹，本端只连加密对端", addr.String())
	}
	raw, err := dialRaw(addr.String(), addr.TLSFingerprint)
	if err != nil {
		return nil, err
	}
	conn := wire.NewTCPConn(raw)
	hello, _ := wire.Encode(wire.Hello{
		Protocol: wire.Protocol, Role: "viewer", Purpose: "control",
		Credentials: wire.Credentials{Kind: "none", Token: token},
	})
	if err := conn.Send(wire.Packet{Kind: "hello", Body: hello}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	first, err := conn.Recv()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if first.Kind != "welcome" {
		_ = conn.Close()
		return nil, fmt.Errorf("对端不是执行端（收到 %s）", first.Kind)
	}
	var welcome wire.Welcome
	_ = wire.Decode(first.Body, &welcome)
	if welcome.Error != "" {
		_ = conn.Close()
		return nil, errors.New(welcome.Error)
	}
	// welcome 垫回去：连接已握手，上层不再重复 hello
	return wire.Prefetch(conn, first), nil
}

// address 是 lan.Address 的别名（让 dialLAN 的签名短一点）。
type address = lan.Address

// dialRaw 建连（只走 TLS），并走与 relay 相同的指纹确认流程。
//
// # 三处必须和 relay 拨号一致
//
//  1. **挂 OnPeerFingerprint**：直连时对端是执行端，它的指纹同样要经过
//     TOFU（没确认过就弹窗）。之前这里用裸 tlsx.Config 直接校验，跳过了
//     确认流程 —— 用户看到的是"直连悄悄失败、回退 relay"，而弹窗里显示的
//     还是 relay 的指纹（同一条待确认项被重复渲染）。
//
//  2. **带上自己的证书**（DataDir）：mTLS 下执行端要验证我们是谁。
//     不带证书时执行端直接拒绝（"没有出示客户端证书"），直连必失败。
//
//  3. **超时只管网络**：等用户在弹窗上做决定的时间不能算进 dialTimeout。
//     所以这里的超时只包住"TCP+TLS 握手"，握手内部的 TOFU 阻塞不受
//     这个计时器影响（计时器只决定"要不要放弃等待结果"，而结果由
//     Gate 的 channel 给出 —— 见下面的实现说明）。
func dialRaw(addr, fingerprint string) (net.Conn, error) {
	if fingerprint == "" {
		return nil, fmt.Errorf("%s 没有证书指纹，拒绝明文连接", addr)
	}
	cfg := tlsx.Config{
		// **必须显式开启客户端 TLS**。
		//
		// ClientTLS() = ClientTLSEnabled || Fingerprint != ""。这里
		// 两者都不设时它为 false，Dial 会退化成**明文** net.Dial ——
		// 既不带证书也不握手，执行端只能看到一条明文连接然后拒绝
		// （日志："没有出示客户端证书"）。真实踩到的坑。
		ClientTLSEnabled: true,
		// 自己的证书：执行端要验证我们是谁（mTLS）
		DataDir: viewerDataDir,
		// 只有 TCP 建连有超时；握手可能等用户看弹窗，不计时
		DialTimeout: lanDialTimeout,
	}
	if peerGate != nil {
		// 直连执行端时按 executor 记账（与 relay 分开）
		verifier := &peerverify.Verifier{
			Store: verifierStore(),
			Role:  trust.RoleExecutor,
			Ask:   peerGate.Ask,
		}
		cfg.OnPeerFingerprint = verifier.Verify
	}
	return cfg.Dial(addr)
}