package relay_test

import (
	"testing"

	"simple-agent/internal/lan"
	"simple-agent/internal/relay"
	"simple-agent/internal/wire"
)

// **首次连接就要能拿到局域网地址**。
//
// 这是直连生效的前提。以前 OpenViewerToken 解析了 relay 发的执行端列表，
// 但只取出 host_id 就把列表扔了 —— 展示端明明拿到了 lan_addrs 却不知道，
// 直连永远不生效（真机上表现为"在同一局域网还是走中转、加载很慢"）。
//
// 这个测试锁住"列表要能带出来"这件事。
func TestOpenViewerTokenListReturnsLANAddrs(t *testing.T) {
	addr, stopRelay := startRelay(t)
	defer stopRelay()

	stop := make(chan struct{})
	defer close(stop)
	go relay.RunExecutor(addr, relay.Executor{
		HostID:   "exec-viewer",
		LANAddrs: []lan.Address{{IP: "10.10.110.196", Port: 5685, Name: "以太网 3"}},
	}, stop, func(conn wire.Conn) { _ = conn.Close() })

	waitForExecutor(t, addr, "exec-viewer")

	// 展示端首次连接：不看连接本身，只看"列表带出来了没有"
	conn, chosen, list, err := relay.OpenViewerTokenList(addr, "exec-viewer", "")
	if conn != nil {
		_ = conn.Close()
	}
	if err != nil {
		t.Fatalf("连接失败：%v", err)
	}
	if chosen != "exec-viewer" {
		t.Fatalf("选中的执行端不对：%q", chosen)
	}
	if len(list) != 1 {
		t.Fatalf("应带回 1 个执行端，实际 %d 个", len(list))
	}
	if len(list[0].LANAddrs) != 1 {
		t.Fatalf("执行端列表里应带局域网地址，实际 %+v", list[0])
	}
	got := list[0].LANAddrs[0]
	if got.IP != "10.10.110.196" || got.Port != 5685 {
		t.Fatalf("局域网地址不对：%+v", got)
	}
}

// 不指定 route 且只有一台执行端时，也要能把地址带回来
// （界面上"直接连唯一的执行端"是最常见的路径）。
func TestOpenViewerTokenListAutoRouteKeepsLANAddrs(t *testing.T) {
	addr, stopRelay := startRelay(t)
	defer stopRelay()

	stop := make(chan struct{})
	defer close(stop)
	go relay.RunExecutor(addr, relay.Executor{
		HostID:   "only-one",
		LANAddrs: []lan.Address{{IP: "192.168.1.10", Port: 7001}},
	}, stop, func(conn wire.Conn) { _ = conn.Close() })

	waitForExecutor(t, addr, "only-one")

	conn, chosen, list, err := relay.OpenViewerTokenList(addr, "", "")
	if conn != nil {
		_ = conn.Close()
	}
	if err != nil {
		t.Fatalf("连接失败：%v", err)
	}
	if chosen != "only-one" {
		t.Fatalf("应自动选中唯一执行端，实际 %q", chosen)
	}
	if len(list) != 1 || len(list[0].LANAddrs) != 1 {
		t.Fatalf("列表或地址丢失：%+v", list)
	}
}