package relay_test

import (
	"sync/atomic"
	"testing"
	"time"

	"simple-agent/internal/lan"
	"simple-agent/internal/relay"
	"simple-agent/internal/wire"
)

// 执行端上报的局域网地址要能被展示端从 relay 看到。
//
// 这是局域网直连的前提：展示端不看这份地址就不知道去连哪里。
func TestExecutorReportsLANAddrs(t *testing.T) {
	addr, stopRelay := startRelay(t)
	defer stopRelay()

	stop := make(chan struct{})
	defer close(stop)
	go relay.RunExecutor(addr, relay.Executor{
		HostID:   "exec-lan",
		LANAddrs: []lan.Address{{IP: "10.0.0.1", Port: 9001, Name: "eth0"}},
	}, stop, func(conn wire.Conn) { _ = conn.Close() })

	waitForExecutor(t, addr, "exec-lan")
	got := lanAddrsOf(t, addr, "exec-lan")
	if len(got) != 1 {
		t.Fatalf("应上报 1 个地址，实际 %v", got)
	}
	if got[0].IP != "10.0.0.1" || got[0].Port != 9001 {
		t.Fatalf("地址不对：%+v", got[0])
	}
}

// **在线期间**重新上报：换 WiFi / DHCP 续约后，relay 上的地址要跟着变。
//
// 覆盖 MsgUpdate 这条路径 —— 不能重新注册（会被"同名执行端已在线"拒绝），
// 只能在既有连接上发更新。
func TestExecutorUpdatesLANAddrsWhileOnline(t *testing.T) {
	defer relay.SetExecutorInfoIntervalForTest(20 * time.Millisecond)()

	addr, stopRelay := startRelay(t)
	defer stopRelay()

	stop := make(chan struct{})
	defer close(stop)

	// 第一次返回旧地址（注册用），之后都返回新地址（模拟换了网）
	var calls int32
	go relay.RunExecutorRefresh(addr, relay.Executor{
		HostID:   "exec-refresh",
		LANAddrs: []lan.Address{{IP: "10.0.0.1", Port: 9001, Name: "eth0"}},
	}, stop, func(conn wire.Conn) { _ = conn.Close() }, nil,
		func(base relay.Executor) relay.Executor {
			if atomic.AddInt32(&calls, 1) == 1 {
				return base // 注册时是旧地址
			}
			base.LANAddrs = []lan.Address{{IP: "10.0.0.2", Port: 9002, Name: "wlan0"}}
			return base
		})

	waitForExecutor(t, addr, "exec-refresh")

	// 等 relay 上的地址变成新的（复查间隔已缩到 20ms）
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := lanAddrsOf(t, addr, "exec-refresh")
		if len(got) == 1 && got[0].IP == "10.0.0.2" && got[0].Port == 9002 {
			return // 成功：在线重新上报生效了
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("地址没有更新成 10.0.0.2:9002，实际 %v", lanAddrsOf(t, addr, "exec-refresh"))
}

// lanAddrsOf 从 relay 列表里取某个执行端的局域网地址。
func lanAddrsOf(t *testing.T, relayAddr, hostID string) []lan.Address {
	t.Helper()
	list, err := relay.ListExecutors(relayAddr)
	if err != nil {
		t.Fatal(err)
	}
	for _, executor := range list {
		if executor.HostID == hostID {
			return executor.LANAddrs
		}
	}
	return nil
}

// waitForExecutor 等执行端在 relay 上线。
func waitForExecutor(t *testing.T, relayAddr, hostID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		list, err := relay.ListExecutors(relayAddr)
		if err == nil {
			for _, executor := range list {
				if executor.HostID == hostID {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("执行端 %s 没在 relay 上线", hostID)
}