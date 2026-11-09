import io

# ---- config.go：去掉 TailscaleConfig 与 help 文本 ----
p = 'internal/config/config.go'
s = io.open(p, encoding='utf-8').read()

old = '''	// Tailscale 是可选的 tsnet 配置：启用后程序自己就是 tailnet 上的一个节点，
	// 目标设备**不需要**安装 Tailscale 客户端。
	Tailscale TailscaleConfig `toml:"tailscale" yaml:"tailscale"`
}

// TailscaleConfig 是内置 Tailscale（tsnet）节点的配置。
//
// 全部可选：Enable 为 false 时一切照旧走普通 TCP。
type TailscaleConfig struct {
	// Enable 打开内置 tailscale。
	Enable bool `toml:"enable" yaml:"enable"`
	// Hostname 是节点在 tailnet 里显示的名字（默认 主机名-角色）。
	Hostname string `toml:"hostname" yaml:"hostname"`
	// AuthKey 是 tailnet 认证密钥（tskey-...）。也可以只用环境变量 TS_AUTHKEY，
	// 避免把密钥写进配置文件。
	//
	// 状态目录里已有身份时可以不填（重启复用同一个节点）。
	AuthKey string `toml:"auth_key" yaml:"auth_key"`
	// StateDir 是节点身份/状态目录（默认 <data_dir>/tailscale/<角色>）。
	//
	// **必须持久化，且每个角色分开**：换目录等于换一台新节点。
	StateDir string `toml:"state_dir" yaml:"state_dir"`
	// ControlURL 指向自建的 headscale（为空时用官方控制面）。
	ControlURL string `toml:"control_url" yaml:"control_url"`
	// Ephemeral 让节点离线后自动从 tailnet 移除（一次性调试用）。
	Ephemeral bool `toml:"ephemeral" yaml:"ephemeral"`
	// Tags 是节点的标签（advertise-tags，需在 ACL 里预授权）。
	Tags []string `toml:"tags" yaml:"tags"`
	// Verbose 打开 tsnet 的详细日志（排障用）。
	Verbose bool `toml:"verbose" yaml:"verbose"`
}

// Enabled 报告是否启用了 tsnet。
func (t TailscaleConfig) Enabled() bool { return t.Enable }

// AuthKeyOrEnv 返回实际使用的认证密钥（环境变量优先，便于密钥不落盘）。
func (t TailscaleConfig) AuthKeyOrEnv() string {
	if key := strings.TrimSpace(os.Getenv("TS_AUTHKEY")); key != "" {
		return key
	}
	return strings.TrimSpace(t.AuthKey)
}'''
new = '''}'''
assert old in s
s = s.replace(old, new)

# help 文本里的 tailscale 段
old = '''  --tailscale        启用内置 tailscale（tsnet）：本进程自己成为 tailnet 节点，
                     对面设备**不需要**安装 Tailscale 客户端。执行端/展示端/relay 都可用
  --ts-authkey KEY   tailnet 认证密钥（也可用环境变量 TS_AUTHKEY，优先）。
                     不填时复用状态目录里已有的身份；两者都没有会等人工授权
  --ts-hostname NAME 节点在 tailnet 里的名字（默认「主机名-角色」，如 pc-executor）
  --ts-state DIR     节点身份目录（默认 <data_dir>/tailscale/<角色>）。
                     必须持久化：换目录等于换一台新节点
'''
assert old in s
s = s.replace(old, '')

io.open(p, 'w', encoding='utf-8').write(s)
print('config ok')

# ---- bridge.go：去掉 tsnet ----
p = 'mobile/bridge/bridge.go'
s = io.open(p, encoding='utf-8').read()

old = '''	// ts 是可选的内置 tailscale 节点（手机不装 Tailscale 客户端时用）。
	ts *tsnetx.Server
}'''
new = '''}'''
assert old in s
s = s.replace(old, new)

old = '''func (c *Client) Connect(addr, route, token string) string {
	return c.ConnectTailscale(addr, route, token, false, "", "", "")
}

// ConnectTailscale 与 Connect 相同，但可以额外启用**内置 tailscale**。
//
// enableTailscale 为真时：先拉起一个 tsnet 节点（手机不需要装 Tailscale 客户端），
// 再用它的线路连 relay。stateDir 必须持久化（每次用同一个目录，节点身份才稳定）。
//
// 参数都是字符串/布尔（gomobile 的限制）。
func (c *Client) ConnectTailscale(addr, route, token string, enableTailscale bool,
	tsHostname, tsAuthKey, tsStateDir string) string {
	if addr == "" {
		return "请填写执行端或 relay 的地址（host:port）"
	}'''
new = '''func (c *Client) Connect(addr, route, token string) string {
	if addr == "" {
		return "请填写执行端或 relay 的地址（host:port）"
	}'''
assert old in s
s = s.replace(old, new)

old = '''	var pinnedMu sync.Mutex
	pinnedRoute := route

	// 可选的 tailscale：先在后台把节点拉起来。tsnet 的 Dial 自己会等线路就绪，
	// 所以不必在这里阻塞到"登录完成"（首次登录可能还要人工授权）。
	var dialer relay.Dialer
	if enableTailscale {
		server, err := tsnetx.New(tsnetx.Config{
			Role:     tsnetx.RoleViewer,
			Hostname: tsHostname,
			AuthKey:  tsAuthKey,
			StateDir: tsStateDir,
		})
		if err != nil {
			return "启动内置 tailscale 失败：" + err.Error()
		}
		c.mu.Lock()
		c.ts = server
		c.mu.Unlock()
		dialer = server.Dial
	}

	dial := func() (wire.Conn, error) {'''
new = '''	var pinnedMu sync.Mutex
	pinnedRoute := route
	dial := func() (wire.Conn, error) {'''
assert old in s
s = s.replace(old, new)

s = s.replace('''		conn, chosen, err := relay.OpenViewerDialer(addr, target, token, dialer)''',
              '''		conn, chosen, err := relay.OpenViewerToken(addr, target, token)''')

# Disconnect / Close 去掉 ts 关闭
s = s.replace('''	remote := c.remote
	c.remote = nil
	ts := c.ts
	c.ts = nil
	c.mu.Unlock()
	if ts != nil {
		_ = ts.Close()
	}''', '''	remote := c.remote
	c.remote = nil
	c.mu.Unlock()''')
s = s.replace('''	remote := c.remote
	c.remote = nil
	ts := c.ts
	c.ts = nil
	c.mu.Unlock()
	if ts != nil {
		_ = ts.Close()
	}
	if remote != nil {''', '''	remote := c.remote
	c.remote = nil
	c.mu.Unlock()
	if remote != nil {''')

s = s.replace('''	"simple-agent/internal/tsnetx"
''', '')

io.open(p, 'w', encoding='utf-8').write(s)
print('bridge ok')

# ---- headless_test.go：恢复调用 ----
p = 'cmd/simple-agent/headless_test.go'
s = io.open(p, encoding='utf-8').read()
s = s.replace('''listenHeadless("127.0.0.1:0", local, post, tailscaleTransport{})''',
              '''listenHeadless("127.0.0.1:0", local, post)''')
s = s.replace('''			func(f func()) { f() }, ""))''', '''			func(f func()) { f() }))''')
io.open(p, 'w', encoding='utf-8').write(s)
print('headless_test ok')
