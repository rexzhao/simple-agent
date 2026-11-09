package main

import (
	"fmt"
	"log"
	"net"
	"sync"

	"simple-agent/internal/host"
	"simple-agent/internal/uiapp"
	"simple-agent/internal/wire"
)

// parseRemoteFlags 抢在 config 之前解析 --listen / --connect，并把它们从 args
// 里摘掉（config.Load 不认这两个参数）。
//
// 两者互斥（用户的决定）：展示端不跑本地会话，执行端只负责监听。
// remoteOptions 是远程模式相关的命令行开关（P4 增加 relay / headless）。
type remoteOptions struct {
	listen  string
	connect string
	// relay 是"执行端注册到 relay"的地址；relayServer 表示本进程充当 relay。
	relay       string
	relayServer bool
	// route 是展示端经 relay 时要连的 hostID。
	route string
	// name 是执行端在 relay 上的名字（默认取主机名）。
	name     string
	headless bool
	// netLog 打开网络收发日志（--net-log），排查连接/转发用。
	netLog bool
	// relayToken 是 relay 的固定口令（relay 启动时 --token 指定；
	// 执行端注册、展示端连接都要带同一个）。
	relayToken string
}

// parseRemoteOptions 解析 --listen/--connect/--relay/--route/--headless，
// 并把它们从 args 里摘掉（config.Load 不认这些参数）。
func parseRemoteOptions(args *[]string) remoteOptions {
	var opts remoteOptions
	rest := make([]string, 0, len(*args))
	for i := 0; i < len(*args); i++ {
		switch (*args)[i] {
		case "relay":
			// 子命令形式：simple-agent relay --listen ADDR
			if i == 0 {
				opts.relayServer = true
			} else {
				rest = append(rest, (*args)[i])
			}
		case "--listen":
			if i+1 < len(*args) {
				opts.listen = (*args)[i+1]
				i++
			} else {
				opts.listen = "127.0.0.1:7801"
			}
		case "--connect":
			if i+1 < len(*args) {
				opts.connect = (*args)[i+1]
				i++
			}
		case "--relay":
			if i+1 < len(*args) {
				opts.relay = (*args)[i+1]
				i++
			}
		case "--route":
			if i+1 < len(*args) {
				opts.route = (*args)[i+1]
				i++
			}
		case "--headless":
			opts.headless = true
		case "--net-log":
			opts.netLog = true
		case "--name":
			if i+1 < len(*args) {
				opts.name = (*args)[i+1]
				i++
			}
		case "--token":
			if i+1 < len(*args) {
				opts.relayToken = (*args)[i+1]
				i++
			}
		default:
			rest = append(rest, (*args)[i])
		}
	}
	*args = rest
	return opts
}

// startListener 让当前进程同时作为执行端对外服务（默认只监听回环）。
//
// 每条连接一个 goroutine；协议层不关心底层是直连还是 relay。
func startListener(addr string, app *uiapp.App, post func(func())) (func(), error) {
	local, ok := app.Host().(*host.Local)
	if !ok {
		return nil, fmt.Errorf("当前进程不是执行端，无法监听")
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	log.Printf("执行端已监听 %s（展示端可用 --connect %s 连接）", addr, listener.Addr())
	warnIfUnprotected(listener.Addr().String())
	var mu sync.Mutex
	open := map[net.Conn]bool{}
	stop := func() {
		_ = listener.Close()
		mu.Lock()
		for conn := range open {
			_ = conn.Close()
		}
		mu.Unlock()
	}
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			mu.Lock()
			open[conn] = true
			mu.Unlock()
			go func(conn net.Conn) {
				defer func() {
					mu.Lock()
					delete(open, conn)
					mu.Unlock()
					_ = conn.Close()
				}()
				// Serve 会一直读到连接关闭；关闭监听器时上面的 stop 也会关掉它
				_ = host.Serve(wire.NewTCPConn(conn), local, post)
			}(conn)
		}
	}()
	return stop, nil
}
