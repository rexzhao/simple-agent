// Package host 是界面与"执行端"之间的唯一接口层。
//
// 为什么要有这一层：远程模式下，会话状态、日志、blob、工具执行、模型凭据都在
// **执行端**，展示端只是复现它的状态并发送命令。如果 uiapp 继续直接调用
// `driver`/`store`，那远端就必须再写一套界面逻辑，"完整复现"就不可能保证。
//
// 现在 uiapp 只依赖 `Host`：
//   - 本包提供 `Local`（进程内直接调用，单机模式与执行端都用它）；
//   - 远程实现（P2）会在同一个接口后面，把读请求转发给本地镜像、把命令塞进连接。
//
// 接口按"读 / 写"分开：
//   - 读：全部读本地状态，不产生任何网络等待；
//   - 写：一律 `Submit(Command)`，立即返回 CmdID，结果通过 `Subscribe` 的
//     `Change{Kind:"cmdResult"}` 回到界面线程（单机实现也遵循同样语义）。
package host

import (
	"context"
	"encoding/json"
	"sync"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// Runtime 是执行端需要的外部能力：发流、跑工具。
//
// 与 uiapp 原来的 Runtime 同签名，因此测试里的假实现可以直接复用。
type Runtime interface {
	OpenStream(ctx context.Context, sel providers.Selection, req providers.ChatRequest) (<-chan providers.Event, error)
	RunTools(ctx context.Context, tc *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome
}

// Config 是执行端的配置（模型、工具权限、工作区、存储、文案）。
type Config struct {
	Runtime Runtime

	Selection     providers.Selection
	SystemPrompt  string
	AllowShell    bool
	// Token 是执行端自己的访问口令（`--listen` 模式下由 `--token` 指定）。
	//
	// 与 relay 的 token 是两套：relay 保护"经过 relay 这一跳"，这里保护
	// "直连执行端这一跳"。为空表示不校验（保持原来的开放行为）。
	Token         string
	Workspace     string
	MaxToolRounds int
	ContextWindow int

	WindowFor    func(providers.Selection) int
	MaxOutputFor func(providers.Selection) int
	EffortFor    func(providers.Selection) string
	VisionFor    func(providers.Selection) bool

	DisableAutoCompact bool

	Blobs   *store.Blobs
	Catalog *store.Catalog

	// NewID 生成会话 id（测试可注入；不能用"列表长度 + 1"）。
	NewID func() string
	// Post 把函数投递到执行端主线程（生产是 win.Update；测试直接执行）。
	Post func(func())
	// Tr 查文案（通知、错误原因要跟界面同一种语言）。
	Tr func(key string, args ...any) string
	// Title 是新会话的默认标题（空则用 Tr("session.defaultTitle")）。
	Title string

	// RingLimit 是断线续传 ring 的条数上限（0 = 默认 2000；测试可注入很小的值）。
	RingLimit int

	// Deferred 为真时，Submit 只把命令排进 Post 队列，到下一次主线程 drain 才执行。
	//
	// 单机默认同步执行（没有网络往返，也便于现有测试"发送后立刻断言"）；
	// 测试用 Deferred 跑一组用例来守住**异步语义**——命令返回不代表已经生效，
	// 失败原因只能经 cmdResult 回来。远程实现天然是 deferred 的。
	Deferred bool

	// Efforts 按模型返回可选思考档位（切换模型时判断当前档位是否还合法）。
	// nil 表示"不校验"。**必须在 NewLocal 之前给**：恢复会话时就要用它判断
	// 重放出来的档位在新模型下是否仍合法。
	Efforts func(providers.Selection) []string

	// Models 是本执行端对外公布的模型/档位列表（展示端的选择器照它显示）。
	// 由入口按配置生成（生产在 main.go 的 providerChoices）。
	Models []ProviderChoice
}

// HostInfo 描述一个执行端。
type HostInfo struct {
	// ID 是执行端实例 id（重启会变）。
	ID string
	// Name 是显示名（机器名/自定义名）。
	Name string
	// Local 表示这是进程内的执行端。
	Local bool
	// AllowShell 是**执行端**的 allow_shell 设置。
	//
	// 展示端不能用自己进程的配置来显示权限：执行端开了命令执行、展示端没开
	// （或反之，--connect 模式根本不读本地配置），标签就会骗人。远程模式下这个
	// 值来自握手（wire.Welcome.Capability），连上之前为 false（保守显示"不能"）。
	AllowShell bool
}

// Command 是一次操作请求。Payload 的具体类型由 Kind 决定（见 payloads.go）。
type Command struct {
	CmdID     string          `json:"cmd_id"`
	SessionID string          `json:"session_id,omitempty"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// CmdResult 是命令的执行结果。
type CmdResult struct {
	CmdID string
	// Kind 是原命令的种类：Data 是 any，跨进程走 JSON 后要靠它还原成具体类型。
	Kind   string `json:"kind,omitempty"`
	OK     bool
	Error  string
	Notice string
	Data   any
	// Unknown 表示"命令已经发出去，但连接断了、结果未知"。
	//
	// 界面**不能**据此还原草稿：执行端可能已经执行了（再发一次会重复）。
	Unknown bool
}

// Change 是"有东西变了，请重绘"的通知；Kind 为 "cmdResult" 时带命令结果。
type Change struct {
	SessionID string
	Kind      string
	CmdID     string
	Result    CmdResult
	// Connected 用于 "connection" 类变化：true=已（重新）连上。
	Connected bool
	// Notice 是非命令类的一次性提示（例如"已被其他端停止"），显示用。
	Notice *Notice
}

// Notice 是一条只用于显示的提示（不写日志）。
type Notice struct {
	ID    string
	Level string // info / warn
	Text  string
	// After 是这条提示发生时 Conversation 里最后一个条目的 id（空表示在最前面）。
	// 展示端合并快照时按它插回原来的位置，保证"后连上来的端看到的顺序也一样"。
	After string `json:",omitempty"`
}

// Host 是 uiapp 眼里的"一个执行端"。
type Host interface {
	Info() HostInfo

	// 会话（读）
	Sessions() []SessionRef
	Session(id string) *Session
	// Active/SetActive 是"用户正在看哪个会话"（执行端也要知道：命令要落到它上面）。
	Active() *Session
	SetActive(id string)
	// StartupError 是启动时遇到的问题（空表示正常）。
	StartupError() string
	Subscribe(fn func(Change)) (cancel func())

	// Providers 是执行端的模型/档位列表（空表示还没同步到）。
	//
	// 展示端**必须**用这份列表：API Key 与模型配置只在执行端，按本机配置显示
	// 会让切模型的菜单指向执行端根本不存在的模型。
	Providers() []ProviderChoice

	// 会话（写）：异步提交，结果经 Subscribe 回来。
	Submit(cmd Command) (string, error)

	// blob 存取（远端按需拉取 + 本地缓存）。
	Blobs() BlobService

	Close() error
}

// ProviderChoice 是模型选择器里的一项提供商。
//
// 定义在 host 而不是 uiapp，是因为它现在要跨进程同步：执行端按自己的配置生成，
// 展示端只是照着显示（不能自己再算一遍）。
type ProviderChoice struct {
	ProviderID string   `json:"provider_id"`
	Label      string   `json:"label,omitempty"`
	Models     []string `json:"models,omitempty"`
	// Labels 是模型的显示名（key 是模型 id；没配就用 id）。
	Labels map[string]string `json:"labels,omitempty"`
	// Efforts 是每个模型支持的思考档位（key 是模型 id）。
	Efforts map[string][]string `json:"efforts,omitempty"`
}

// SetEfforts 注入"按模型返回思考档位"的查询函数（构造后替换也支持）。
func (l *Local) SetEfforts(fn func(providers.Selection) []string) {
	if fn == nil {
		fn = func(providers.Selection) []string { return nil }
	}
	l.cfg.Efforts = fn
}

// SessionRef 是会话列表里的一项。
type SessionRef struct {
	ID        string
	Title     string
	Workspace string
	UpdatedAt int64
}

// OlderEntry 是归档里的一条消息（只用于显示）。
type OlderEntry struct {
	Message   message.Message
	Status    conversation.EntryStatus
	Reason    string
	CreatedAt int64
}

// OlderPage 是一页归档。
type OlderPage struct {
	Entries []OlderEntry
	// More 表示还有更早的页。
	More bool
	// Next 是下一页的游标（归档文件名）；More=false 时为空。
	//
	// 游标必须由**展示端**带回：执行端的会话是共享的，两个展示端各自翻页时
	// 不能共用一个"读到哪了"。
	Next string
}

// BlobService 是内容寻址存储的读写口（远端实现会带缓存）。
type BlobService interface {
	Put(data []byte) (string, error)
	Open(hash string) ([]byte, error)
	Has(hash string) bool
}

// LocalBlobs 把 *store.Blobs 适配成 BlobService。
type LocalBlobs struct{ Store *store.Blobs }

func (b LocalBlobs) Put(data []byte) (string, error)  { return b.Store.Put(data) }
func (b LocalBlobs) Open(hash string) ([]byte, error) { return b.Store.Open(hash) }
func (b LocalBlobs) Has(hash string) bool             { return b.Store.Has(hash) }

// subscribers 是"变化通知"的注册表（合并成一次重绘即可）。
type subscribers struct {
	mu    sync.Mutex
	next  int
	funcs map[int]func(Change)
}

func newSubscribers() *subscribers {
	return &subscribers{funcs: map[int]func(Change){}}
}

func (s *subscribers) add(fn func(Change)) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.next
	s.next++
	s.funcs[id] = fn
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.funcs, id)
	}
}

func (s *subscribers) emit(change Change) {
	s.mu.Lock()
	list := make([]func(Change), 0, len(s.funcs))
	for _, fn := range s.funcs {
		list = append(list, fn)
	}
	s.mu.Unlock()
	for _, fn := range list {
		fn(change)
	}
}

// containsString 报告列表中是否包含某个值。
func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
