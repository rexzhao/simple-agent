package relay

import (
	"errors"
	"fmt"
	"log"
	"time"

	"simple-agent/internal/wire"
)

// NeedsChoice 表示"对面是 relay，但没有指定/无法唯一确定执行端"。
//
// 界面应当打开选择列表（而不是直接退出）；用户选中后设置目标并重连。
type NeedsChoice struct {
	Executors []Executor
}

func (e *NeedsChoice) Error() string {
	return fmt.Sprintf("relay 上有 %d 个执行端，需要选择", len(e.Executors))
}

// OpenViewer 用**一条连接、一次握手**分辨对面是执行端还是 relay：
//
//   - 对面回 welcome：就是执行端，直接把连接交给上层（垫回这条 welcome）；
//   - 对面回 relay.executors：是 relay；route 为空时按列表决定（唯一者自动选中）。
//
// 不再单独开探测连接：直连执行端时不会白等一个超时，执行端日志也不会多一次连接。
//
// token 是 relay 的固定口令（relay 没配 token 时传空）。
func OpenViewer(addr, route string) (wire.Conn, string, error) {
	return OpenViewerToken(addr, route, "")
}

// OpenViewerToken 与 OpenViewer 相同，但带上 relay 的固定 token。
func OpenViewerToken(addr, route, token string) (wire.Conn, string, error) {
	return OpenViewerDialer(addr, route, token, nil)
}

// OpenViewerDialer 与 OpenViewerToken 相同，但可以指定底层拨号方式。
//
// dialer 为 nil 时走普通 TCP；启用 Tailscale 时传入 tsnet 的拨号函数即可，
// 上层不需要知道"这一跳是直连还是走 tailnet"。
func OpenViewerDialer(addr, route, token string, dialer Dialer) (wire.Conn, string, error) {
	conn, err := dialer.Dial(addr)
	if err != nil {
		return nil, "", err
	}
	hello, _ := wire.Encode(wire.Hello{
		Protocol: wire.Protocol, Role: "viewer", Purpose: "control",
		// relay 的固定 token 走握手凭据（协议里本来就预留了这个字段），
		// 这样"探测对端"那一步也带着认证，relay 能在回执行端列表之前就校验。
		Credentials: wire.Credentials{Kind: "none", Token: token},
	})
	if err := conn.Send(wire.Packet{Kind: "hello", Body: hello}); err != nil {
		_ = conn.Close()
		return nil, "", err
	}
	first, err := conn.Recv()
	if err != nil {
		_ = conn.Close()
		return nil, "", err
	}
	switch first.Kind {
	case "welcome":
		// 直连执行端：welcome 垫回去，连接带 Handshaken 标记，上层不再重复 hello
		return wire.Prefetch(conn, first), "", nil
	case MsgExecutors:
		var list []Executor
		_ = wire.Decode(first.Body, &list)
		if err := relayError(first.Body); err != nil {
			_ = conn.Close()
			return nil, "", err
		}
		target := route
		if target == "" {
			if len(list) != 1 {
				_ = conn.Close()
				return nil, "", &NeedsChoice{Executors: list}
			}
			target = list[0].HostID
		}
		_ = conn.Close()
		viewerConn, dialErr := DialViewerDialer(addr, target, token, dialer)
		return viewerConn, target, dialErr
	default:
		_ = conn.Close()
		return nil, "", fmt.Errorf("无法识别的对端（%s）", first.Kind)
	}
}

// DialViewer 连到 relay 并请求转发到 hostID，返回一条"看起来就像直连执行端"的连接。
//
// 上层 host.Remote 完全不知道 relay 的存在：relay 把后续帧原样转发。
func DialViewer(addr, hostID string) (wire.Conn, error) {
	return DialViewerToken(addr, hostID, "")
}

// DialViewerToken 与 DialViewer 相同，但带上 relay 的固定 token。
func DialViewerToken(addr, hostID, token string) (wire.Conn, error) {
	return DialViewerDialer(addr, hostID, token, nil)
}

// DialViewerDialer 与 DialViewerToken 相同，但可以指定底层拨号方式。
func DialViewerDialer(addr, hostID, token string, dialer Dialer) (wire.Conn, error) {
	conn, err := dialer.Dial(addr)
	if err != nil {
		return nil, err
	}
	body, _ := wire.Encode(map[string]any{"host_id": hostID, "token": token})
	if err := conn.Send(wire.Packet{Kind: MsgRoute, Body: body}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reply, err := conn.Recv()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if reply.Kind != MsgRouted {
		_ = conn.Close()
		return nil, fmt.Errorf("relay 拒绝了路由请求")
	}
	var result struct {
		HostID string `json:"host_id"`
		Error  string `json:"error"`
	}
	_ = wire.Decode(reply.Body, &result)
	if result.Error != "" {
		_ = conn.Close()
		return nil, fmt.Errorf("%s", result.Error)
	}
	return conn, nil
}

// ListExecutors 查询 relay 上有哪些执行端。
func ListExecutors(addr string) ([]Executor, error) {
	return ListExecutorsToken(addr, "")
}

// ListExecutorsToken 与 ListExecutors 相同，但带上 relay 的固定 token。
func ListExecutorsToken(addr, token string) ([]Executor, error) {
	return ListExecutorsDialer(addr, token, nil)
}

// ListExecutorsDialer 与 ListExecutorsToken 相同，但可以指定底层拨号方式。
func ListExecutorsDialer(addr, token string, dialer Dialer) ([]Executor, error) {
	conn, err := dialer.Dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	body, _ := wire.Encode(tokenPayload{Token: token})
	if err := conn.Send(wire.Packet{Kind: MsgList, Body: body}); err != nil {
		return nil, err
	}
	reply, err := conn.Recv()
	if err != nil {
		return nil, err
	}
	if err := relayError(reply.Body); err != nil {
		return nil, err
	}
	var out []Executor
	if err := wire.Decode(reply.Body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// relayError 从回包里取出 error 字段（relay 拒绝时用）。
func relayError(body []byte) error {
	var payload struct {
		Error string `json:"error"`
	}
	if err := wire.Decode(body, &payload); err == nil && payload.Error != "" {
		return errors.New(payload.Error)
	}
	// 列表回包是数组，解析成结构体会失败——那不是错误
	return nil
}

// ProbeExecutors 在 timeout 内问一次"这是不是 relay、上面有哪些执行端"。
//
// 用于 `--connect ADDR` 没写 `--route` 的情况：如果它是 relay 且只注册了一个执行端，
// 界面就能自动接上（GUI 里没有选择器）；对直连执行端会超时返回错误，调用方按直连处理。
func ProbeExecutors(addr string, timeout time.Duration) ([]Executor, error) {
	return ProbeExecutorsToken(addr, timeout, "")
}

// ProbeExecutorsToken 与 ProbeExecutors 相同，但带上 relay 的固定 token。
func ProbeExecutorsToken(addr string, timeout time.Duration, token string) ([]Executor, error) {
	return ProbeExecutorsDialer(addr, timeout, token, nil)
}

// ProbeExecutorsDialer 与 ProbeExecutorsToken 相同，但可以指定底层拨号方式。
func ProbeExecutorsDialer(addr string, timeout time.Duration, token string, dialer Dialer) ([]Executor, error) {
	conn, err := dialer.Dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	body, _ := wire.Encode(tokenPayload{Token: token})
	if err := conn.Send(wire.Packet{Kind: MsgList, Body: body}); err != nil {
		return nil, err
	}
	type reply struct {
		executors []Executor
		err       error
	}
	ch := make(chan reply, 1)
	go func() {
		packet, recvErr := conn.Recv()
		if recvErr != nil {
			ch <- reply{err: recvErr}
			return
		}
		if authErr := relayError(packet.Body); authErr != nil {
			ch <- reply{err: authErr}
			return
		}
		var out []Executor
		if decodeErr := wire.Decode(packet.Body, &out); decodeErr != nil {
			ch <- reply{err: decodeErr}
			return
		}
		ch <- reply{executors: out}
	}()
	select {
	case result := <-ch:
		return result.executors, result.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("不是 relay（查询执行端列表超时）")
	}
}

// RunExecutor 把执行端注册到 relay，并为每个展示端建立一条虚拟连接交给 serve。
//
// 连接断了（relay 重启/网络抖动）会退避重连并重新注册；重连期间展示端自己按
// 直连同样的语义处理断线（既有 remoteHost 逻辑）。
func RunExecutor(addr string, info Executor, stop <-chan struct{}, serve func(wire.Conn)) {
	RunExecutorDialer(addr, info, nil, stop, serve)
}

// RunExecutorDialer 与 RunExecutor 相同，但可以指定底层拨号方式。
//
// 启用 Tailscale 的执行端用它注册 relay：断线重连也走同一个 dialer，所以
// "重启后还是同一个节点"这件事由 tsnet 的状态目录保证，而不是这里。
func RunExecutorDialer(addr string, info Executor, dialer Dialer, stop <-chan struct{}, serve func(wire.Conn)) {
	delay := time.Second
	for {
		select {
		case <-stop:
			return
		default:
		}
		conn, err := dialer.Dial(addr)
		if err != nil {
			delay = nextBackoff(delay)
			sleep(delay, stop)
			continue
		}
		body, _ := wire.Encode(info)
		if err := conn.Send(wire.Packet{Kind: MsgRegister, Body: body}); err != nil {
			_ = conn.Close()
			delay = nextBackoff(delay)
			sleep(delay, stop)
			continue
		}
		// 先等注册结果：同名/超限时 relay 会拒绝，把原因打出来并按退避重试
		reply, readErr := conn.Recv()
		if readErr != nil || reply.Kind != MsgRegistered {
			_ = conn.Close()
			delay = nextBackoff(delay)
			sleep(delay, stop)
			continue
		}
		var registered struct {
			Error  string `json:"error"`
			HostID string `json:"host_id"`
		}
		_ = wire.Decode(reply.Body, &registered)
		if registered.Error != "" {
			log.Printf("relay：注册被拒绝（%s）：%s", info.HostID, registered.Error)
			_ = conn.Close()
			delay = nextBackoff(delay)
			sleep(delay, stop)
			continue
		}
		delay = time.Second // 注册成功后重置退避
		mux := wire.NewMux(conn)
		// stop 关闭时立刻断开这条 relay 连接（否则 Accept 会一直阻塞到对端关闭）
		stopped := make(chan struct{})
		go func() {
			select {
			case <-stop:
				mux.Close()
			case <-stopped:
			}
		}()
		for {
			accepted, acceptErr := mux.Accept()
			if acceptErr != nil {
				break
			}
			go serve(accepted)
		}
		close(stopped)
		mux.Close()
		sleep(time.Second, stop)
	}
}

func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > 30*time.Second {
		next = 30 * time.Second
	}
	return next
}

func sleep(d time.Duration, stop <-chan struct{}) {
	select {
	case <-time.After(d):
	case <-stop:
	}
}
