package main

import (
	"log"
	"net"
	"os"
	"time"

	"simple-agent/internal/config"
	"simple-agent/internal/relay"
	"simple-agent/internal/tsnetx"
)

// tailscaleTransport 是启用内置 tailscale 后的网络底座。
//
// 全部字段为 nil 表示"没启用"——调用方保持普通 TCP 行为。
type tailscaleTransport struct {
	// dialer 用来连别人（执行端连 relay、展示端连 relay）。
	dialer relay.Dialer
	// listener 用来被别人连（执行端监听、relay 监听）。
	listener relay.Listener
	// stop 退出时关闭节点。
	stop func()
	// server 供"同时监听主机 + tailnet"的场景使用。
	server *tsnetx.Server
}

// startTailscale 按需启动内置 tailscale 节点。
//
// 三种情况：
//   - 没启用：返回零值，一切走普通 TCP（默认行为，零影响）；
//   - 启用成功：返回可注入的 dialer/listener；
//   - 启用失败：打日志并退回普通网络——网络叠加层失败不该让程序整体不可用。
func startTailscale(cfg *config.Config, opts remoteOptions, role tsnetx.Role) tailscaleTransport {
	enabled := opts.tailscale || cfg.Tailscale.Enabled()
	if !enabled {
		return tailscaleTransport{}
	}

	ts := cfg.Tailscale
	tsnetCfg := tsnetx.Config{
		Role:          role,
		Hostname:      firstNonEmpty(opts.tsHostname, ts.Hostname),
		AuthKey:       firstNonEmpty(opts.tsAuthKey, ts.AuthKeyOrEnv()),
		StateDir:      firstNonEmpty(opts.tsStateDir, ts.StateDir),
		DataDir:       cfg.DataDir,
		ControlURL:    ts.ControlURL,
		Ephemeral:     ts.Ephemeral,
		AdvertiseTags: ts.Tags,
		Verbose:       ts.Verbose,
	}
	server, err := tsnetx.New(tsnetCfg)
	if err != nil {
		log.Printf("tailscale：启动失败，退回普通网络：%v", err)
		return tailscaleTransport{}
	}
	log.Printf("tailscale：已启用（角色 %s，节点 %s，状态目录 %s）",
		role, tsnetx.HostnameFor(tsnetCfg), server.Dir())

	// 把 tailnet IP 打进日志：用户要知道"该连哪个地址"。
	// 不阻塞主流程——首次登录可能要等人工授权，放后台。
	go func() {
		ip, waitErr := server.WaitIP(30 * time.Second)
		if waitErr != nil {
			log.Printf("tailscale：还没拿到 tailnet IP（%v）——首次登录可能需要人工授权；"+
				"授权完成后本进程会自动上线", waitErr)
			return
		}
		log.Printf("tailscale：节点已上线，IP %s（其它设备可用 <节点名>:端口 访问）", ip)
	}()

	return tailscaleTransport{
		dialer:   server.Dial,
		listener: server.Listen,
		stop:     func() { _ = server.Close() },
		server:   server,
	}
}

// listen 在给定地址上监听。
//
// 启用 tailscale 时**主机回环与 tailnet 都要监听**：只监听 tailnet 的话，
// 本机的展示端连 127.0.0.1 会突然连不上，调试体验倒退。
func (t tailscaleTransport) listen(addr string) (net.Listener, error) {
	if t.server == nil {
		return net.Listen("tcp", addr)
	}
	hostAddr := addr
	if hostAddr == "" || hostAddr == ":0" {
		hostAddr = "127.0.0.1:0"
	}
	return t.server.ListenBoth(hostAddr, addr)
}

// relayTailscaleConfig 给 relay 子命令构造一份最小配置。
//
// relay 在加载完整配置之前就退出了（那样会创建并锁定 data_dir），但 tailscale
// 的身份目录仍需持久化。所以这里只取最小信息：环境变量 + 命令行。
func relayTailscaleConfig(opts remoteOptions) config.Config {
	cfg := config.Config{}
	// 数据目录只用来推导默认状态目录，不创建会话目录
	cfg.DataDir = os.Getenv("SIMPLE_AGENT_DATA")
	cfg.Tailscale = config.TailscaleConfig{
		Enable:   opts.tailscale,
		Hostname: opts.tsHostname,
		AuthKey:  opts.tsAuthKey,
		StateDir: opts.tsStateDir,
	}
	return cfg
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
