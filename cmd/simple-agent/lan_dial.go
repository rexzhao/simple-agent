package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"simple-agent/internal/lan"
	"simple-agent/internal/relay"
	"simple-agent/internal/tlsx"
	"simple-agent/internal/wire"
)

// lanDialTimeout 是单次局域网直连尝试的超时。
//
// 800ms：局域网内 RST/不可达通常几十毫秒就返回；这个值覆盖慢速 WiFi，
// 又不至于让"不在同一网络"卡太久（那种情况会走到超时才回退 relay，
// 用户等的就是这个时间）。
const lanDialTimeout = 800 * time.Millisecond

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

// dialRaw 只负责建连（明文或 TLS），不做握手。
func dialRaw(addr, fingerprint string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		if fingerprint == "" {
			// 明文：执行端只配了 --tls-fingerprint（客户端方向）、
			// 没有服务端证书时，它的监听就是明文的
			raw, err := net.DialTimeout("tcp", addr, lanDialTimeout)
			done <- result{conn: raw, err: err}
			return
		}
		raw, err := tlsx.Config{Fingerprint: fingerprint}.Dial(addr)
		done <- result{conn: raw, err: err}
	}()
	select {
	case r := <-done:
		return r.conn, r.err
	case <-time.After(lanDialTimeout):
		// 超时后台回收：迟到的连接要关掉，否则泄漏
		go func() {
			if r := <-done; r.conn != nil {
				_ = r.conn.Close()
			}
		}()
		return nil, fmt.Errorf("直连 %s 超时", addr)
	}
}