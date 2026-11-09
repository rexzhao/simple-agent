// Package tsnetx 是可选的 Tailscale（tsnet）接入层。
//
// # 为什么需要它
//
// 有些设备装不了 / 不方便装 Tailscale 客户端（公司机器没权限、安卓上不想装 VPN
// 应用、固件里跑不了客户端）。把 tsnet 编进程序里，程序自己就是 tailnet 上的
// 一个节点：不装客户端也能互相访问，流量还是端到端加密的。
//
// # 它是可选的
//
// 不配置 tailscale 时本包完全不起作用——所有拨号/监听仍走 `net.Dial` /
// `net.Listen`，与以前一模一样。只有显式启用才拉起 tsnet 服务器。
//
// # 节点身份必须稳定
//
// tsnet 的节点身份（tailnet 里那个 100.x.y.z 与机器名）保存在 `Dir` 指向的状态
// 目录里。**这个目录必须持久化，而且每个角色用不同目录**：换目录 = 换一台新节点，
// tailnet 里就会多出一台陌生设备。所以：
//
//   - 默认落在 `<data_dir>/tailscale/<role>`，跟数据目录一起持久化；
//   - `--ts-state DIR` 可显式指定（比如放在用户目录，跨 data_dir 变更仍保留）；
//   - 目录里已有身份时，不再需要 authkey 也能复用同一个节点；
//   - 同一台机器上的多个角色（执行端 / 展示端 / relay）必须用不同子目录。
package tsnetx

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/tsnet"
)

// Role 是 tsnet 节点扮演的角色（决定状态目录与默认主机名）。
type Role string

const (
	// RoleExecutor 是执行端（agent host）。
	RoleExecutor Role = "executor"
	// RoleViewer 是展示端。
	RoleViewer Role = "viewer"
	// RoleRelay 是 relay 服务端。
	RoleRelay Role = "relay"
)

// Config 是 tsnet 的启动配置。
type Config struct {
	// Role 决定状态目录与默认主机名。
	Role Role
	// Hostname 是节点名（DNS 安全）。为空时用 主机名-角色。
	Hostname string
	// AuthKey 是 tailnet 认证密钥（tskey-...）。
	//
	// 为空时复用状态目录里已有的身份；两者都没有会一直等人工授权（日志会提示）。
	AuthKey string
	// StateDir 是身份/状态目录。为空时用 <DataDir>/tailscale/<role>。
	StateDir string
	// DataDir 是程序的数据目录（用来推导默认 StateDir）。
	DataDir string
	// ControlURL 指向自建 headscale（为空用官方控制面）。
	ControlURL string
	// Ephemeral 让节点离线后自动从 tailnet 移除（一次性调试用）。
	Ephemeral bool
	// AdvertiseTags 是节点标签（需在 ACL 里预授权）。
	AdvertiseTags []string
	// Verbose 打开 tsnet 自己的详细日志。
	Verbose bool
	// OpenBrowser 在没有 authkey、需要交互式登录时，自动用系统浏览器打开登录页。
	//
	// headless 场景（服务器、无桌面环境）应设为 false——那里没有浏览器可开，
	// 强行调用只会失败并留下噪音日志；用户会从日志里拿到 URL 自己访问。
	OpenBrowser bool
}

// stateDir 解析出最终的状态目录。
func (c Config) stateDir() string {
	if c.StateDir != "" {
		return c.StateDir
	}
	base := c.DataDir
	if base == "" {
		// 没有 data_dir 时退回用户配置目录，不要写当前工作目录
		home, err := os.UserConfigDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, "simple-agent")
	}
	return filepath.Join(base, "tailscale", string(c.Role))
}

// StateDirFor 返回这份配置最终使用的状态目录（导出给测试与日志用）。
//
// 规则：显式 StateDir 优先；否则 <DataDir>/tailscale/<角色>。
// **每个角色必须是不同目录**，否则会互相覆盖节点身份。
func StateDirFor(c Config) string { return c.stateDir() }

// hostname 解析出节点名。
func (c Config) hostname() string {
	if c.Hostname != "" {
		return sanitizeHostname(c.Hostname)
	}
	name, err := os.Hostname()
	if err != nil || name == "" {
		name = "simple-agent"
	}
	// 同一台机器可能同时跑执行端和 relay：加角色后缀，否则 tailnet 里
	// 会出现两台同名节点（tsnet 会自作主张加 -1，那样"身份稳定"就靠运气了）。
	if c.Role != "" {
		return sanitizeHostname(name + "-" + string(c.Role))
	}
	return sanitizeHostname(name)
}

// HostnameFor 返回这份配置最终使用的节点名（导出给测试与日志用）。
func HostnameFor(c Config) string { return c.hostname() }

// sanitizeHostname 把名字改成 DNS 安全的形式（tsnet 会拒绝非法主机名）。
func sanitizeHostname(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r-'A'+'a')
		case r == '.' || r == '_' || r == ' ':
			out = append(out, '-')
		}
	}
	trimmed := string(out)
	for len(trimmed) > 0 && trimmed[0] == '-' {
		trimmed = trimmed[1:]
	}
	for len(trimmed) > 0 && trimmed[len(trimmed)-1] == '-' {
		trimmed = trimmed[:len(trimmed)-1]
	}
	if trimmed == "" {
		return "simple-agent"
	}
	if len(trimmed) > 63 {
		trimmed = trimmed[:63]
	}
	return trimmed
}

// Server 是一个 tsnet 节点。
//
// 它同时提供「拨号」与「监听」，因此可以直接替换 net.Dial / net.Listen：
//
//	dialer := server.Dial
//	listener, _ := server.Listen("tcp", ":7900")
type Server struct {
	srv *tsnet.Server
	dir string
	// once 保证 Close 幂等。
	once sync.Once
	// authURL 是最近一次拿到的登录地址（交互式登录时才有）。
	authURL atomic.Pointer[string]
	// openedOnce 保证浏览器只自动打开一次（登录循环每 5 秒会重复给同一个 URL）。
	openedOnce sync.Once
	// OpenBrowser 为真时，拿到登录 URL 后自动打开系统浏览器。
	OpenBrowser bool
}

// New 创建 tsnet 节点并**在后台开始登录**。
//
// 它不阻塞到"登录完成"：首次登录可能要做网络协商、甚至等人工授权，阻塞在这里
// 会让执行端迟迟不监听。要 IP 用 [Server.WaitIP]；要连通性直接拨号即可
// （tsnet 自己会等线路就绪）。
func New(cfg Config) (*Server, error) {
	dir := cfg.stateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("无法创建 tailscale 状态目录 %s：%w", dir, err)
	}
	self := &Server{dir: dir, OpenBrowser: cfg.OpenBrowser}
	// 诊断：确认 authkey 真的传到这里（只打前缀，避免泄漏）
	if cfg.AuthKey != "" {
		log.Printf("tailscale：使用 authkey（%s…，长度 %d）", safePrefix(cfg.AuthKey), len(cfg.AuthKey))
	} else {
		log.Printf("tailscale：没有 authkey")
	}
	srv := &tsnet.Server{
		Hostname:  cfg.hostname(),
		AuthKey:   cfg.AuthKey,
		Dir:       dir,
		Ephemeral: cfg.Ephemeral,
		UserLogf:  func(format string, args ...any) { log.Printf("tailscale: "+format, args...) },
	}
	self.srv = srv
	// 接管 tsnet 自己的日志：**登录 URL 就在这里面**（tsnet 的 printAuthURLLoop
	// 每 5 秒打一行 "go to: https://login.tailscale.com/a/xxx"）。
	// 不接管的话这条 URL 会被丢掉，用户只能自己去 tailnet 后台或手动看 tsnet 日志。
	srv.Logf = func(format string, args ...any) {
		message := fmt.Sprintf(format, args...)
		if url := extractAuthURL(message); url != "" {
			self.noteAuthURL(url)
		}
		if cfg.Verbose {
			log.Printf("tailscale: %s", message)
		}
	}
	if cfg.ControlURL != "" {
		srv.ControlURL = cfg.ControlURL
	}
	if len(cfg.AdvertiseTags) > 0 {
		srv.AdvertiseTags = cfg.AdvertiseTags
	}
	if cfg.AuthKey == "" && !hasState(dir) {
		// 没 key 也没历史身份：需要交互式登录。说清楚接下来会发生什么，
		// 否则用户只会看到"连不上"而不知道为什么。
		log.Printf("tailscale：%s 没有授权密钥、也没有已保存的身份（%s）", cfg.Role, dir)
		if cfg.OpenBrowser {
			log.Printf("tailscale：正在向 Tailscale 申请登录链接，拿到后会自动打开浏览器…")
		} else {
			log.Printf("tailscale：正在向 Tailscale 申请登录链接，随后请用日志里的 URL 在浏览器完成授权")
		}
	}
	return self, nil
}

// noteAuthURL 记下登录地址，并按配置自动打开浏览器（只开一次）。
func (s *Server) noteAuthURL(url string) {
	if previous := s.authURL.Load(); previous != nil && *previous == url {
		return
	}
	value := url
	s.authURL.Store(&value)
	log.Printf("tailscale：需要在浏览器完成登录：%s", url)
	if !s.OpenBrowser {
		return
	}
	s.openedOnce.Do(func() {
		if err := openBrowser(url); err != nil {
			log.Printf("tailscale：自动打开浏览器失败（%v），请手动访问上面的 URL", err)
			return
		}
		log.Printf("tailscale：已尝试在浏览器中打开登录页（授权完成后本进程会自动上线）")
	})
}

// AuthURL 返回当前待完成的登录地址（已有身份或已登录时为空）。
func (s *Server) AuthURL() string {
	if value := s.authURL.Load(); value != nil {
		return *value
	}
	return ""
}

// safePrefix 只暴露密钥的前缀，用于日志确认（绝不打印完整密钥）。
func safePrefix(key string) string {
	if len(key) <= 16 {
		return "***"
	}
	return key[:16]
}

// extractAuthURL 从一条日志里找出 tailscale 的登录地址。
//
// tsnet 的原文形如：
//
//	To start this tsnet server, restart with TS_AUTHKEY set, or go to: https://login.tailscale.com/a/xxx
func extractAuthURL(message string) string {
	const marker = "https://login.tailscale.com/"
	index := strings.Index(message, marker)
	if index < 0 {
		return ""
	}
	rest := message[index:]
	// URL 以空白结尾
	if end := strings.IndexAny(rest, " \t\r\n"); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}

// hasState 报告状态目录里是否已有节点身份（tsnet 会写 tailscaled.state）。
func hasState(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Name() == "tailscaled.state" {
			return true
		}
	}
	return false
}

// Dir 返回状态目录（日志/排障用）。
func (s *Server) Dir() string { return s.dir }

// dialTimeout 是一次拨号的上限（含等待线路就绪）。
const dialTimeout = 45 * time.Second

// Dial 是 `func(network, addr string) (net.Conn, error)` 形状的拨号函数，
// 可以直接替换 net.Dial。
//
// addr 可以是 MagicDNS 名字（my-pc:7900）或 100.x.y.z。
func (s *Server) Dial(network, addr string) (net.Conn, error) {
	// 拨号带超时：线路没就绪时 tsnet 会等，但不能永远等
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	return s.srv.Dial(ctx, network, addr)
}

// Listen 在 tailnet 上监听（只对 tailnet 内可见，完全不占主机端口）。
func (s *Server) Listen(network, addr string) (net.Listener, error) {
	return s.srv.Listen(network, addr)
}

// ListenBoth 同时监听主机回环与 tailnet，返回合并后的监听器。
//
// hostAddr 是主机上要监听的地址（如 127.0.0.1:7900）；
// tsAddr 是 tailnet 上的端口（如 :7900）。
//
// 为什么要两边都听：开了 tailscale 后如果只监听 tailnet，本机的展示端就连不上
// 127.0.0.1 了，调试会莫名变麻烦。两条路都通更符合预期。
func (s *Server) ListenBoth(hostAddr, tsAddr string) (net.Listener, error) {
	tsListener, err := s.srv.Listen("tcp", tsAddr)
	if err != nil {
		return nil, fmt.Errorf("tailscale 监听失败：%w", err)
	}
	hostListener, err := net.Listen("tcp", hostAddr)
	if err != nil {
		// 主机端口被占用不该拖垮 tailnet：那条已经起来了
		log.Printf("tailscale：主机地址 %s 监听失败（%v），只保留 tailnet 监听", hostAddr, err)
		return tsListener, nil
	}
	return newMergedListener(hostListener, tsListener), nil
}

// WaitIP 等到节点拿到 tailnet IP（首次登录后才有），超时返回错误。
//
// 只用于"把 IP 打进日志"这种展示用途；功能上不需要等它。
func (s *Server) WaitIP(timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	status, err := s.srv.Up(ctx)
	if err != nil {
		return "", err
	}
	if len(status.TailscaleIPs) > 0 {
		return status.TailscaleIPs[0].String(), nil
	}
	if status.Self != nil && len(status.Self.TailscaleIPs) > 0 {
		return status.Self.TailscaleIPs[0].String(), nil
	}
	return "", errors.New("没有拿到 tailscale IP")
}

// Close 关闭节点（幂等）。
func (s *Server) Close() error {
	var err error
	s.once.Do(func() { err = s.srv.Close() })
	return err
}

// ---- 合并监听器 ----

// mergedListener 把多个 listener 合并成一个（Accept 任一即可）。
//
// 用于"既监听主机回环、又监听 tailnet"：本机调试走 127.0.0.1，
// 远程设备走 tailnet，两条路都通。
type mergedListener struct {
	listeners []net.Listener
	conns     chan acceptResult
	closeOnce sync.Once
	done      chan struct{}
}

type acceptResult struct {
	conn net.Conn
	err  error
}

func newMergedListener(listeners ...net.Listener) net.Listener {
	m := &mergedListener{
		listeners: listeners,
		conns:     make(chan acceptResult, len(listeners)*2),
		done:      make(chan struct{}),
	}
	for _, listener := range listeners {
		listener := listener
		go func() {
			for {
				conn, err := listener.Accept()
				select {
				case m.conns <- acceptResult{conn: conn, err: err}:
				case <-m.done:
					if conn != nil {
						_ = conn.Close()
					}
					return
				}
				if err != nil {
					return
				}
			}
		}()
	}
	return m
}

func (m *mergedListener) Accept() (net.Conn, error) {
	select {
	case result := <-m.conns:
		return result.conn, result.err
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *mergedListener) Close() error {
	m.closeOnce.Do(func() {
		close(m.done)
		for _, listener := range m.listeners {
			_ = listener.Close()
		}
	})
	return nil
}

func (m *mergedListener) Addr() net.Addr {
	if len(m.listeners) > 0 {
		return m.listeners[0].Addr()
	}
	return nil
}
