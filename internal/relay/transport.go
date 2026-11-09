package relay

import (
	"net"

	"simple-agent/internal/wire"
)

// Dialer 是"怎么建立一条到 relay / 执行端的连接"。
//
// 抽出来的唯一目的是让传输层可替换：默认是普通 TCP（net.Dial），
// 启用 Tailscale 时换成 tsnet 的拨号函数（同样满足这个签名）。
//
// 用函数类型而不是接口：整个仓库的拨号点都只需要
// `(network, addr) -> (net.Conn, error)`，函数足够、也最好测
// （一个假函数就能换成进程内管道）。
type Dialer func(network, addr string) (net.Conn, error)

// Dial 用这个 Dialer 建一条 wire 连接（带分帧与心跳）。
func (d Dialer) Dial(addr string) (wire.Conn, error) {
	if d == nil {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		return wire.NewTCPConn(conn), nil
	}
	conn, err := d("tcp", addr)
	if err != nil {
		return nil, err
	}
	return wire.NewTCPConn(conn), nil
}

// Listener 是"怎么在地址上接受连接"。
//
// 默认是 net.Listen；启用 Tailscale 时是 tsnet 的 listener（只对 tailnet 可见），
// 或者"主机回环 + tailnet 同时监听"的合并实现。
type Listener func(network, addr string) (net.Listener, error)

// Listen 用这个 Listener 监听（nil 时退回 net.Listen）。
func (l Listener) Listen(addr string) (net.Listener, error) {
	if l == nil {
		return net.Listen("tcp", addr)
	}
	return l("tcp", addr)
}
