package uiapp

import (
	"fmt"
	"log"
	"strings"
	"time"

	"simple-agent/internal/host"
	"simple-agent/internal/relay"
	"simple-agent/internal/tlsx"
	"simple-agent/internal/wire"
)

// 斜杠命令框架。
//
// 设计目标：**通用**。以后加命令只往 [slashCommands] 里追加一条，
// 输入框的候选列表、前缀匹配、帮助文本、执行入口都自动生效——
// 不需要再动界面代码。
//
// 为什么放在界面层而不是 host 层：这些命令改变的是"本机怎么对外提供执行端服务"，
// 属于进程级副作用（起 goroutine、连网络），不是会话命令，也不该转发到远端执行端。

// slashCommand 是一条斜杠命令。
type slashCommand struct {
	// Name 是命令名（不含斜杠），小写。
	Name string
	// Args 是参数的占位说明，用于提示（可为空）。
	Args string
	// Help 是一句话说明。
	Help string
	// Run 执行命令。args 是按空白切分后的参数（不含命令名本身）。
	//
	// 返回给用户看的提示文本；空字符串表示成功且无需额外提示。
	Run func(a *App, args []string) string
}

// Usage 返回形如 "connect RELAY [TOKEN] [FINGERPRINT]"。
func (c slashCommand) Usage() string {
	if c.Args == "" {
		return c.Name
	}
	return c.Name + " " + c.Args
}

// slashCommands 是所有可用命令（顺序即提示顺序）。
//
// 用 init() 填充而不是包级字面量：help 命令要遍历这张表，二者互相引用，
// 直接写字面量会形成 Go 的包级初始化环。换成 init() 就只是运行期调用，
// 顺序问题消失。
var slashCommands []slashCommand

func init() {
	slashCommands = []slashCommand{
		{
			Name: "connect",
			Args: "[RELAY] [TOKEN] [FINGERPRINT]",
			Help: "把本机注册到 relay；不带参数则复用上次连接的那台",
			Run:  runConnect,
		},
		{
			Name: "disconnect",
			Args: "[--forget]",
			Help: "断开 relay 注册；加 --forget 同时忘掉保存的连接参数",
			Run:  runDisconnect,
		},
		{
			Name: "relay",
			Help: "查看 relay 连接状态（含上次保存的地址）",
			Run:  runRelayStatus,
		},
		{
			Name: "help",
			Help: "列出所有命令",
			Run: func(a *App, args []string) string {
				return buildHelpText()
			},
		},
	}
}

// lookupCommand 按名字找命令（大小写不敏感）。
func lookupCommand(name string) (slashCommand, bool) {
	for _, command := range slashCommands {
		if strings.EqualFold(command.Name, name) {
			return command, true
		}
	}
	return slashCommand{}, false
}

// matchCommands 返回以 prefix 开头的命令（用于输入框的候选列表）。
//
// prefix 不含斜杠；空字符串返回全部。
func matchCommands(prefix string) []slashCommand {
	lower := strings.ToLower(prefix)
	out := make([]slashCommand, 0, len(slashCommands))
	for _, command := range slashCommands {
		if strings.HasPrefix(command.Name, lower) {
			out = append(out, command)
		}
	}
	return out
}

// isSlashCommand 报告一段输入是否是斜杠命令（以 / 开头）。
func isSlashCommand(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "/")
}

// ExecSlashCommand 执行一条斜杠命令文本（形如 "/connect host token"）。
//
// 返回提示文本与是否成功。命令不存在时返回 false，调用方按普通消息处理。
func (a *App) ExecSlashCommand(text string) (message string, ok bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return "", false
	}
	fields := strings.Fields(strings.TrimPrefix(trimmed, "/"))
	if len(fields) == 0 {
		return "请输入命令，例如 /help", true
	}
	command, found := lookupCommand(fields[0])
	if !found {
		return fmt.Sprintf("未知命令：%s（输入 /help 查看全部）", fields[0]), true
	}
	return command.Run(a, fields[1:]), true
}

// ---- 命令实现 ----

// buildHelpText 生成全部命令的帮助文本。
func buildHelpText() string {
	var builder strings.Builder
	builder.WriteString("可用命令：")
	for _, command := range slashCommands {
		builder.WriteString("\n/")
		builder.WriteString(command.Usage())
		builder.WriteString(" — ")
		builder.WriteString(command.Help)
	}
	return builder.String()
}

// runConnect 把本机作为执行端注册到 relay。
//
// 参数是位置参数：/connect [RELAY] [TOKEN] [FINGERPRINT]
//
// **不带参数时复用上次成功连接的 relay**（参数存在 <data_dir>/relay.json）。
// 这样常用的那台 relay 不用每次重敲地址、token 和 64 位指纹。
func runConnect(a *App, args []string) string {
	if len(args) == 0 {
		saved := a.loadSavedRelay()
		if saved.Addr == "" {
			return "用法：/connect RELAY [TOKEN] [FINGERPRINT]\n" +
				"（还没有记录上次的 relay；连过一次之后可以直接 /connect）"
		}
		return a.StartRelayRegistration(saved.Addr, saved.Token, saved.Fingerprint)
	}
	addr := strings.TrimSpace(args[0])
	if addr == "" {
		return "relay 地址不能为空"
	}
	token := ""
	fingerprint := ""
	if len(args) > 1 {
		token = args[1]
	}
	if len(args) > 2 {
		fingerprint = args[2]
	}
	return a.StartRelayRegistration(addr, token, fingerprint)
}

func runDisconnect(a *App, args []string) string {
	// /disconnect --forget：断开的同时忘掉保存的 relay 参数
	forget := len(args) > 0 && (args[0] == "--forget" || args[0] == "-f")
	wasConnected := a.Relaying()
	if wasConnected {
		a.stopRelayRegistration()
	}
	if forget {
		a.clearSavedRelay()
		if wasConnected {
			return "已断开 relay 注册，并忘掉了保存的连接参数"
		}
		return "已忘掉保存的 relay 连接参数"
	}
	if !wasConnected {
		return "当前没有连接 relay"
	}
	return "已断开 relay 注册"
}

func runRelayStatus(a *App, _ []string) string {
	a.relayMu.Lock()
	connected := a.relay.stop != nil
	addr := a.relay.addr
	hostID := a.relay.hostID
	since := a.relay.since
	a.relayMu.Unlock()

	if !connected {
		// 没连时也告诉用户"上次连的是哪"：这就是 /connect 下次会用的参数
		if saved := a.loadSavedRelay(); saved.Addr != "" {
			encryption := "明文"
			if saved.Fingerprint != "" {
				encryption = "TLS"
			}
			return fmt.Sprintf("当前没有连接 relay\n上次保存：%s（%s）\n用 /connect 可复用",
				saved.Addr, encryption)
		}
		return "当前没有连接 relay（用 /connect 连接）"
	}
	return fmt.Sprintf("relay 已连接\n地址：%s\nhost_id：%s\n已运行：%s",
		addr, hostID, time.Since(since).Truncate(time.Second))
}

// ---- relay 注册的运行时管理 ----

// relaySession 是本机作为执行端注册到 relay 的运行状态。
type relaySession struct {
	stop   chan struct{}
	addr   string
	hostID string
	since  time.Time
}

// Relaying 报告当前是否已注册到 relay。
func (a *App) Relaying() bool {
	a.relayMu.Lock()
	defer a.relayMu.Unlock()
	return a.relay.stop != nil
}

// StartRelayRegistration 在当前进程里启动"注册到 relay"。
//
// 已经连着时先断开再连（与界面上"点连接即重连"的语义一致）。
// 返回给用户看的提示文本。
func (a *App) StartRelayRegistration(addr, token, fingerprint string) string {
	// 先校验参数，再检查 host：参数错是用户输入问题，先告诉他怎么改，
	// 比"当前不是执行端"这种内部状态提示更有用。
	if addr == "" {
		return "relay 地址不能为空"
	}
	tlsConfig := tlsx.Config{Fingerprint: fingerprint}
	if fingerprint != "" {
		if _, err := tlsConfig.ClientConfig(); err != nil {
			return "指纹格式不对：" + err.Error()
		}
	}

	local, ok := a.host.(*host.Local)
	if !ok {
		return "只有执行端才能注册 relay（当前是展示端）"
	}

	a.stopRelayRegistration()

	name := a.hostIDForRelay()
	stop := make(chan struct{})
	a.relayMu.Lock()
	a.relay = relaySession{stop: stop, addr: addr, hostID: name, since: time.Now()}
	a.relayMu.Unlock()

	// 拨号按指纹走 TLS：与进程级 --tls 不冲突（这里只影响这条注册连接）
	wire.SetDialFunc(tlsConfig.Dial)

	post := a.postFunc()
	go relay.RunExecutor(addr, relay.Executor{HostID: name, Token: token}, stop,
		func(conn wire.Conn) {
			_ = host.Serve(conn, local, post)
		})

	log.Printf("界面：已开始向 relay %s 注册（host_id=%s）", addr, name)
	// 记住这次参数：下次 `/connect` 不带参数就能直接连（地址 + token + 指纹都省了）
	a.saveRelay(savedRelay{Addr: addr, Token: token, Fingerprint: fingerprint})

	encryption := "明文"
	if fingerprint != "" {
		encryption = "TLS"
	}
	return fmt.Sprintf("已连接 relay %s（%s）\nhost_id：%s\n以后可直接 /connect 复用这组参数",
		addr, encryption, name)
}

// stopRelayRegistration 停止 relay 注册（幂等）。
func (a *App) stopRelayRegistration() {
	a.relayMu.Lock()
	stop := a.relay.stop
	a.relay = relaySession{}
	a.relayMu.Unlock()
	if stop != nil {
		close(stop)
		log.Printf("界面：已停止 relay 注册")
	}
}

// hostIDForRelay 返回注册到 relay 时使用的主机名。
//
// relay 用它做路由键（展示端按它选执行端），所以要稳定、可读。
func (a *App) hostIDForRelay() string {
	if a.host == nil {
		return "simple-agent"
	}
	if info := a.host.Info(); info.Name != "" {
		return info.Name
	}
	return "simple-agent"
}

// postFunc 返回投递到界面线程的函数（没有时直接执行）。
func (a *App) postFunc() func(func()) {
	if a.post == nil {
		return func(fn func()) { fn() }
	}
	return a.post
}