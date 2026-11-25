package relay

import (
	"errors"
	"fmt"
	"log"
	"time"

	"simple-agent/internal/lan"
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
	conn, chosen, _, err := OpenViewerTokenList(addr, route, token)
	return conn, chosen, err
}

// OpenViewerTokenList 与 [OpenViewerToken] 相同，另外返回 relay 上的
// **执行端列表**（含各自的局域网直连地址）。
//
// 为什么单独返回它：首次连接时 relay 本来就会先发一份列表（用来选目标），
// 以前解析完只取了 host_id 就把列表扔了 —— 于是展示端明明拿到了
// lan_addrs 却不知道，直连永远不生效（每次都走中转）。
//
// 只在"经 relay 连接"时有值；对端是执行端（直接 welcome）时返回 nil。
func OpenViewerTokenList(addr, route, token string) (wire.Conn, string, []Executor, error) {
	conn, err := wire.Dial(addr)
	if err != nil {
		return nil, "", nil, err
	}
	hello, _ := wire.Encode(wire.Hello{
		Protocol: wire.Protocol, Role: "viewer", Purpose: "control",
		// relay 的固定 token 走握手凭据（协议里本来就预留了这个字段），
		// 这样"探测对端"那一步也带着认证，relay 能在回执行端列表之前就校验。
		Credentials: wire.Credentials{Kind: "none", Token: token},
	})
	if err := conn.Send(wire.Packet{Kind: "hello", Body: hello}); err != nil {
		_ = conn.Close()
		return nil, "", nil, err
	}
	first, err := conn.Recv()
	if err != nil {
		_ = conn.Close()
		return nil, "", nil, err
	}
	switch first.Kind {
	case "welcome":
		// 直连执行端：welcome 垫回去，连接带 Handshaken 标记，上层不再重复 hello
		return wire.Prefetch(conn, first), "", nil, nil
	case MsgExecutors:
		var list []Executor
		_ = wire.Decode(first.Body, &list)
		if err := relayError(first.Body); err != nil {
			_ = conn.Close()
			return nil, "", nil, err
		}
		target := route
		if target == "" {
			if len(list) != 1 {
				_ = conn.Close()
				return nil, "", list, &NeedsChoice{Executors: list}
			}
			target = list[0].HostID
		}
		_ = conn.Close()
		viewerConn, dialErr := DialViewerToken(addr, target, token)
		return viewerConn, target, list, dialErr
	default:
		_ = conn.Close()
		return nil, "", nil, fmt.Errorf("无法识别的对端（%s）", first.Kind)
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
	conn, err := wire.Dial(addr)
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
	conn, err := wire.Dial(addr)
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
	conn, err := wire.Dial(addr)
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
	RunExecutorNotify(addr, info, stop, serve, nil)
}

// RunExecutorNotify 与 [RunExecutor] 相同，另外在**每次注册成功**时回调
// onRegistered（首次成功、以及断线后重连成功都会调用），允许为 nil。
//
// 调用方用它区分"真的连上了"和"只是启动了重试循环" —— 后者在 relay 拒绝
// 注册时（同名执行端已在线）可能永远连不上，界面不能显示"已连接"。
func RunExecutorNotify(
	addr string, info Executor, stop <-chan struct{},
	serve func(wire.Conn), onRegistered func(),
) {
	RunExecutorRefresh(addr, info, stop, serve, onRegistered, nil)
}

// RunExecutorRefresh 与 [RunExecutorNotify] 相同，另外在**每次注册前**
// 回调 refresh 拿最新的 info。
//
// 用途：执行端上报的局域网地址会变（WiFi 切换、DHCP 续约、笔记本换网络）。
// 只在启动时算一次的话，换了网络之后上报的就成了过期地址 —— 展示端会去
// 连一个已经不存在的 IP，每次都要等超时才回退 relay，比没有直连还慢。
//
// refresh 为 nil 时行为与 RunExecutorNotify 完全一致。
func RunExecutorRefresh(
	addr string, info Executor, stop <-chan struct{},
	serve func(wire.Conn), onRegistered func(), refresh func(Executor) Executor,
) {
	delay := time.Second
	for {
		select {
		case <-stop:
			return
		default:
		}
		if refresh != nil {
			info = refresh(info)
		}
		conn, err := wire.Dial(addr)
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
		if onRegistered != nil {
			onRegistered()
		}
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
		// **在线期间定期复查地址变化并重新上报**。
		//
		// 为什么不能只在重连时刷新：连接一直好好的，地址却可能已经变了
		// （换 WiFi、DHCP 续约）。那时展示端会去连一个不存在的 IP，
		// 每次等超时才回退 relay —— 比没有直连还慢。
		//
		// 轮询而不是监听网卡事件：Go 标准库没有跨平台的网络变化通知，
		// 而枚举一次网卡是微秒级的（每 30 秒一次毫无负担）。
		if refresh != nil {
			go watchExecutorInfo(conn, info, refresh, stopped)
		}
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

// watchExecutorInfo 定期复查执行端信息，变化了就经 relay 连接重新上报。
//
// 只发**非 mux 帧**（MsgUpdate 没有 Conn 字段），relay 的 serveExecutor
// 循环能直接识别它 —— 不需要占用一条逻辑通道，也不会和展示端的通道 id 冲突。
//
// 只在内容真的变了时才发：无脑重发会让 relay 频繁刷新列表，进而让展示端
// 收到多余的 executors 更新（界面会跟着抖动）。
func watchExecutorInfo(conn wire.Conn, current Executor, refresh func(Executor) Executor, stopped <-chan struct{}) {
	ticker := time.NewTicker(executorInfoInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stopped:
			return
		case <-ticker.C:
		}
		next := refresh(current)
		if sameLANAddrs(current.LANAddrs, next.LANAddrs) {
			continue
		}
		current = next
		body, err := wire.Encode(next)
		if err != nil {
			continue
		}
		// Conn 留空（0）：这是控制帧，不是某条逻辑通道的数据。
		if err := conn.Send(wire.Packet{Kind: MsgUpdate, Body: body}); err != nil {
			return
		}
		log.Printf("relay：已重新上报执行端信息（%d 个局域网地址）", len(next.LANAddrs))
	}
}

// executorInfoInterval 是复查局域网地址的间隔。
//
// 30 秒：换网络后最多半分钟展示端就能拿到新地址。太短没必要
// （枚举网卡虽然便宜，但每次变化都会让 relay 广播一次列表）；
// 太长则换网后有较长时间会去试过期地址。
//
// 是变量而不是常量：测试需要把它调小验证"在线重新上报"这条路径，
// 否则要等 30 秒（生产代码里没人改它）。
var executorInfoInterval = 30 * time.Second

// SetExecutorInfoIntervalForTest 调整复查间隔（仅测试用）。
//
// 在线重新上报的验证必须把 30 秒缩短，否则一条测试要跑半分钟。
// 返回一个恢复原值的函数，用 defer 调用。
func SetExecutorInfoIntervalForTest(interval time.Duration) func() {
	previous := executorInfoInterval
	executorInfoInterval = interval
	return func() { executorInfoInterval = previous }
}

// sameLANAddrs 判断两组地址是否完全一致（顺序无关）。
func sameLANAddrs(a, b []lan.Address) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(addr lan.Address) string { return addr.String() }
	set := make(map[string]bool, len(a))
	for _, addr := range a {
		set[key(addr)] = true
	}
	for _, addr := range b {
		if !set[key(addr)] {
			return false
		}
	}
	return true
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
