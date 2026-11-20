package uiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/locale"
	"simple-agent/internal/markdown"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/render"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// Runtime 是 App 需要的外部能力（发流、跑工具）。
//
// 抽成接口是为了让 UI 测试能在没有网络、没有真实命令的情况下走完整流程：
// 测试用假 Runtime 推事件，界面逻辑仍是同一套。
type Runtime interface {
	// OpenStream 发起一次流式请求。ctx 取消即中止。
	// 契约：发出 Done/Error 之后必须**立即关闭** channel（与真实 provider 一致）。
	OpenStream(ctx context.Context, sel providers.Selection, req providers.ChatRequest) (<-chan providers.Event, error)
	// RunTools 执行工具调用。抽出来是为了测试能控制时序与返回值。
	RunTools(ctx context.Context, tc *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome
}

// Options 是 App 的初始配置。
type Options struct {
	Selection    providers.Selection
	SystemPrompt string
	AllowShell   bool
	// WebFetch 决定 web_fetch 是否注册（字段式路径用；生产走 LocalConfig）。
	//
	// 这个字段本身是踩过坑的：host.Config 的零值是 false，漏传就等于静默关掉
	// 这个工具，而执行端（appRuntime.RunTools）却仍然认得它——表现为模型压根
	// 看不到工具（不是调用失败，更难发现）。生产入口现在统一走 LocalConfig。
	WebFetch bool
	// Token 是执行端自己的访问口令（`--listen` 直连时校验展示端的握手口令）。
	// 与 relay 的 token 是两套：这里保护"直连执行端"这一跳。
	Token         string
	Workspace     string
	MaxToolRounds int
	// ContextWindow 是初始模型的上下文窗口（用于自动压缩；0 = 未知，按 32k）
	ContextWindow int
	// WindowFor 按模型选择返回上下文窗口（切换模型时用；nil 表示不更新）。
	WindowFor func(providers.Selection) int
	// MaxOutputFor 按模型选择返回最大输出 token 数。
	MaxOutputFor func(providers.Selection) int
	// EffortFor 按模型选择返回默认思考档位（切换模型时用）。
	EffortFor func(providers.Selection) string
	// SidebarAutoCollapseWidth 是侧栏自动收起的窗口宽度阈值：
	// 0 表示用 dsh 默认值（1024），负数表示禁用自动收起（测试用）。
	SidebarAutoCollapseWidth int
	// Language 是界面语言；空值时跟随系统（见 locale.Detect）。
	Language locale.Lang
	// VisionFor 按模型选择返回是否支持图片（切换模型时用；nil 视为不支持）。
	VisionFor func(providers.Selection) bool
	// PasteImages 读取剪贴板里的图片（位图或图片文件）。
	//
	// 生产实现在 cmd 里用 mygo.Clipboard；测试注入假的。返回空切片表示"剪贴板里
	// 没有图片"，此时 Ctrl+V 会放行给编辑器按文本粘贴。
	PasteImages func() ([][]byte, error)
	// PickImageFiles 打开系统文件对话框（阻塞，调用方负责放后台）。
	PickImageFiles func() ([]string, error)
	// PickDirectory 打开系统**目录**选择对话框（阻塞，调用方负责放后台）。
	//
	// 用于「添加项目」：对齐 dsh 的 WorkspacePickFlow——添加项目只有一条路，
	// 就是选一个宿主目录（目录选择器自带「新建文件夹」，不需要另做表单）。
	// 返回空字符串表示用户取消了。
	PickDirectory func() (string, error)
	// Executors 非空表示经 relay 的展示端（可列出/切换执行端）。
	Executors ExecutorSource
	// Blobs 是内容寻址的二进制存储（图片、以后的大文件都走它）。
	Blobs *store.Blobs
	// DisableAutoCompact 关闭自动压缩（只用按钮）
	DisableAutoCompact bool
	// DeferredCommands 让执行端的命令走"延后执行"（测试用：守住异步语义）。
	//
	// 单机默认同步执行；远程实现天然是延后的。这组测试就是 remoteHost 的前哨。
	DeferredCommands bool
	// ImageFetcher 覆盖图片下载（测试用；nil 表示走真实 HTTP）。
	ImageFetcher func(url string) ([]byte, error)
	// Post 把函数投递到主线程（生产环境是 win.Update，测试里直接执行）。
	Post func(func())
	// Name 是本机作为执行端对外显示的名字（`--name`）。
	//
	// 为空时用操作系统机器名——展示端界面上要能看到"连的是哪台机器"。
	Name string
	// Title 是新会话的默认标题。
	Title string
	// Host 覆盖执行端（展示端接入 remoteHost 时用；nil = 进程内 Local）。
	Host host.Host
	// LocalConfig 是**进程内执行端**的完整配置（nil 时按下面的字段拼）。
	//
	// 生产入口（cmd/simple-agent）一律给这个：UI 模式与 headless 模式共用同一份
	// `executorConfig(...)`，字段漏传导致「某个工具静默消失」的问题就只剩一处可犯。
	// 字段式 Options（Selection/AllowShell/WebFetch/…）保留给测试——测试要的是
	// 几个开关，不是一整份执行端配置。
	//
	// 优先级：Host（注入的 host）> LocalConfig > 字段式选项。
	LocalConfig *host.Config
	// Providers 是界面上可选的提供商与模型（空表示不显示选择器）。
	Providers []ProviderChoice
	// Catalog 是数据目录索引（nil 表示不持久化：测试与临时会话用）。
	// 有它时启动会枚举已有会话并逐个恢复。
	Catalog *store.Catalog
	// OnSelectionChanged 在切换模型后回调（仅界面用途；持久化走 driver 的 Record）。
	OnSelectionChanged func(sessionID string, sel providers.Selection)
	// UIStatePath 是本机界面偏好的落盘路径（空表示不持久化）。
	//
	// 由入口决定（默认 `uistate.Path()`，即系统应用配置目录）：这是**每台客户端
	// 自己**的偏好，远程模式下不该写进执行端的 data_dir。
	UIStatePath string
	// NewID 生成会话 id（默认用 crypto/rand 的 16 字节 hex）。
	// 注入是为了测试能拿到可预测的 id；**不能用「列表长度 + 1」**：
	// 删掉中间会话后新建会与现存会话重名，迟到事件就会投递到错误的会话。
	NewID func() string
}

// archivedEntry 是「更早的历史」在界面上的形态（不进 Conversation）。
//
// 带状态是必要的：归档里可能有 Failed/Cancelled 的半截回复，显示成正常回答
// 会误导用户。
type archivedEntry struct {
	Message   message.Message
	Status    conversation.EntryStatus
	Reason    string
	CreatedAt time.Time
}

// ProviderChoice 是选择器里的一项提供商。
//
// 类型本身定义在 host（要跨进程同步）；这里保留别名，入口与既有测试不用改。
type ProviderChoice = host.ProviderChoice

// Session 是一个会话的**界面**状态。
//
// 执行端的状态（driver / Conversation / Busy / 队列 / 提问 / 用量 / 标题…）全部
// 来自嵌入的 *host.Session；这里只保留"只属于这个界面"的东西：滚动、展开、草稿、
// 附件、提问面板的页签与手写答案等（用户决定 8：界面状态不同步到其它端）。
type Session struct {
	*host.Session

	list ui.ListState

	// 重命名：正在编辑时的草稿
	renaming         bool
	renameDraft      string
	renameWasFocused bool

	// projectID/projectName 来自项目档案（后端每帧同步过来的）。
	//
	// 侧栏用它分组和排顺序：项目顺序由 projects.json 决定，
	// 拿 workspace 反推只能得到名字、排不出先后。
	// 空表示没有档案（内存后端），退回按 workspace 分组。
	projectID   string
	projectName string
	// archivedSession 表示会话**已归档**：不能再运行，恢复后才能继续。
	//
	// 注意与上面的 `archived`（更早历史的显示缓存）区分：那个是日志概念，
	// 这个是会话级状态。
	archivedSession bool

	// archived 是按需加载的更早历史：**只用于显示**，永远不进 Conversation、
	// 也永远不参与请求（用户已确认）。
	archived []archivedEntry
	// summaryExpanded 控制「以上内容已压缩」分隔线是否展开摘要原文
	summaryExpanded bool
	// loadingOlder 防止滚动到顶时重复请求
	loadingOlder bool
	// archiveNext 是归档翻页游标（每页由执行端返回，各展示端互不影响）
	archiveNext string
	// noMoreArchives 归档读完了
	noMoreArchives bool
	// errMsg 是本会话自己的错误提示（跨会话不串）
	errMsg string
	// restoreDraft 是"发送失败但当时不在这个会话"时留下的草稿。
	//
	// 切回这个会话（且输入框为空）时放回去——否则提示里说"请切回该会话查看"，
	// 用户切回去却什么都看不到。
	restoreDraft string
	// draft 是这个会话**未发送**的输入（切走时存、切回来时取）。
	//
	// 为什么放在会话上而不是 App 上：草稿天然属于某个会话。以前是 App 上一个
	// 全局字符串，切会话时没人动它，于是 A 会话打了一半的字会跟着到 B
	// （附件早就是按会话存的，只有草稿漏了）。
	//
	// 与 restoreDraft 的区别：那个是「发送失败、且用户已经切走」时留下的，
	// 只在输入框为空时才放回；draft 是用户正常切换会话时要原样带回的内容。
	// 两者语义不同，不能合并。
	draft string
	// attachments 是待发送的图片（粘贴/拖入）。
	attachments []*attachment
	// askTab 是提问面板当前显示的问题下标（多个问题用 tab 切换，不竖着堆）
	askTab int
	// askDrafts 是每个问题的"手写答案"输入框草稿（键：callID#题号）。
	askDrafts map[string]*string
	// askSynced 记录已经同步给 driver 的草稿值：只有用户**改了**输入框才回写，
	// 否则改选预设选项后会被下一帧的同步把文字又推回去。
	askSynced map[string]string
	// todoCollapsed 是任务清单面板是否收起（纯界面态，**不落盘**）：
	// 对齐 dsh 的 `useState(true)`——每次启动都是折叠的。
	todoCollapsed bool
}

// askKey 是某个问题在 askDrafts / askSynced 里的键。
func askKey(callID string, index int) string { return callID + "#" + itoa(index) }

// syncAsk 记录"已经同步给 driver 的草稿值"。
func (s *Session) syncAsk(key, value string) {
	if s.askSynced == nil {
		s.askSynced = make(map[string]string, 4)
	}
	s.askSynced[key] = value
}

// askDraft 取某个问题的输入框草稿（首次访问时创建）。
func (s *Session) askDraft(callID string, index int) *string {
	if s.askDrafts == nil {
		s.askDrafts = make(map[string]*string, 4)
	}
	key := askKey(callID, index)
	draft, ok := s.askDrafts[key]
	if !ok {
		text := ""
		draft = &text
		s.askDrafts[key] = draft
	}
	return draft
}

// tr 查界面文案（所有界面文字都必须走这里，见 locale 包）。
func (a *App) tr(key string, args ...any) string { return a.lang.Tr(key, args...) }

// SetLanguageForTest 切换界面语言（测试用：验证英文界面）。
func (a *App) SetLanguageForTest(lang locale.Lang) { a.lang = lang }

// DataDirForTest 返回展示给用户的数据目录（测试用）。
func (a *App) DataDirForTest() string { return a.dataDir() }

// ToolImagesForTest 返回当前会话里工具结果携带的图片引用（测试用）。
func (a *App) ToolImagesForTest() []tools.ToolImageRef {
	session := a.Current()
	if session == nil {
		return nil
	}
	var out []tools.ToolImageRef
	for _, entry := range session.Conversation().Entries() {
		if entry.Message == nil || entry.Message.Message == nil {
			continue
		}
		for _, part := range entry.Message.Message.Parts {
			if part.MediaType != render.MediaToolResult {
				continue
			}
			data, ok := partData(part)
			if !ok {
				continue
			}
			out = append(out, toolImagesOf(data)...)
		}
	}
	return out
}

// SubmitSendForSessionForTest 在**指定会话**上提交一次发送（测试用：覆盖
// "失败结果回来时用户已经在别的会话"这种时序）。
func (a *App) SubmitSendForSessionForTest(id string) {
	shell := a.shellByID(id)
	if shell == nil {
		return
	}
	attachments, _ := a.attachmentParts(shell)
	a.submitSend(shell, strings.TrimSpace(a.draft), attachments)
}

// SubmitSendForTest 直接走"提交发送"这条路径（测试用：绕过界面上的预检查，
// 覆盖"命令失败后草稿与附件还原"的异步语义）。
func (a *App) SubmitSendForTest() {
	session := a.Current()
	if session == nil {
		return
	}
	attachments, _ := a.attachmentParts(session)
	a.submitSend(session, strings.TrimSpace(a.draft), attachments)
}

// SelectSessionForTest 切到指定会话（测试用：走与侧栏相同的路径）。
func (a *App) SelectSessionForTest(id string) {
	for index, shell := range a.sessions {
		if shell.ID == id {
			a.switchTo(index)
			return
		}
	}
}

// switchTo 切到第 index 个会话——**所有切换入口都必须走这里**。
//
// 为什么收敛成一个函数：以前"切会话"散落在 5 处（侧栏点击、新建会话、
// 删除后定位、执行端切换、测试入口），每处各做各的事，于是漏了草稿。
// 以后要加"切换时的副作用"（保存滚动位置、落盘草稿…）也只需改这一处。
func (a *App) switchTo(index int) {
	if index < 0 || index >= len(a.sessions) {
		return
	}
	next := a.sessions[index]
	if next == nil {
		return
	}
	// 离开当前会话：把输入框里的内容存到**它自己**身上
	if current := a.currentShell(); current != nil && current != next {
		current.draft = a.draft
	}
	a.active = index
	a.host.SetActive(next.ID)
	// 进入新会话：先放回"失败留下的草稿"（仅当它是唯一来源），
	// 再恢复用户自己没发出去的内容。两者不会同时有内容 —— 
	// restoreDraft 只在输入框为空时生效。
	a.draft = ""
	a.restoreDraftIfIdle(next)
	if a.draft == "" {
		a.draft = next.draft
	}
	a.pendingCaret = -1
}

// currentShell 返回当前会话对应的界面壳（a.sessions 里的元素）。
func (a *App) currentShell() *Session {
	if a.active < 0 || a.active >= len(a.sessions) {
		return nil
	}
	return a.sessions[a.active]
}

// SetFlashErrForTest 设置下一帧要 Toast 的错误（测试用）。
func (a *App) SetFlashErrForTest(text string) { a.flashErr = text }

// FlashErrForTest 返回当前的（待显示）错误文案（测试用）。
func (a *App) FlashErrForTest() string { return a.flashErr }

// SessionsForTest 返回会话列表（测试用）。
func (a *App) SessionsForTest() []*Session { return a.sessions }

// SetPickDirectoryForTest 替换目录选择器（测试用：模拟用户选了另一个目录）。
func (a *App) SetPickDirectoryForTest(pick func() (string, error)) {
	a.opts.PickDirectory = pick
}

// PickingProjectForTest 报告目录选择器是否还开着（测试用：等它复位）。
func (a *App) PickingProjectForTest() bool { return a.pickingProject }

// AdoptProjectForTest 直接走"接纳一个目录作为项目"这一步（测试用：
// 绕开系统对话框与它的帧时机，单独验证去重与新建）。
func (a *App) AdoptProjectForTest(dir string) { a.adoptProject(dir) }

// DeleteConfirmForTest 返回删除确认框状态（测试用）。
func (a *App) DeleteConfirmForTest() bool { return a.deleteConfirm }

// DeleteTargetForTest 返回待删除会话 id（测试用）。
func (a *App) DeleteTargetForTest() string { return a.deleteTarget }

// ThemeForTest 返回上一帧实际使用的主题（测试用）。
func (a *App) ThemeForTest() *ui.Theme { return a.lastTheme }

// SidebarWidthForTest 返回侧栏当前宽度（测试用）。
func (a *App) SidebarWidthForTest() int { return int(a.sidebarWidth) }

// StatusDotForTest 返回某个运行状态对应的状态点符号（测试用）。
//
// 只暴露符号不暴露颜色：颜色由主题决定，换主题不该让测试变红。
func StatusDotForTest(busy driver.Busy) string {
	dot, _ := statusDotFor(busy)
	return dot
}

// SidebarGroupOpenForTest 返回项目的展开状态表（测试用）。
func (a *App) SidebarGroupOpenForTest() map[string]bool { return a.sidebarGroupOpen }

// SetSidebarGroupOpenForTest 直接设置某个项目的展开状态（测试用：
// 绕开点击，单独验证"状态 → 渲染"这一段）。
func (a *App) SetSidebarGroupOpenForTest(key string, open bool) {
	a.setSidebarGroupOpen(key, open)
}

// ContentWidthForTest 返回正文列当前宽度（测试用）。
func (a *App) ContentWidthForTest() int { return int(a.contentWidth()) }

// ContentWidthHandleWidthForTest 返回宽度把手当前的可用宽度（0 = 不可见，
// 对应窄窗口下"把手自然消失"）。
func (a *App) ContentWidthHandleWidthForTest() int { return int(a.widthHandleStrip()) }

// ContentWidthPreferenceForTest 返回用户设置的正文宽度偏好（0 = 未设置）。
func (a *App) ContentWidthPreferenceForTest() int { return int(a.contentWidthPreference) }

// SidebarCollapsedForTest 报告侧栏当前是否收起成图标栏（测试用）。
func (a *App) SidebarCollapsedForTest() bool { return a.sidebarCollapsedForWidth() }

// ViewportForTest 返回上一帧的窗口尺寸（测试用）。
func (a *App) ViewportForTest() (float32, float32) { return a.viewportW, a.viewportH }

// SidebarMinWidthForTest 返回侧栏拖动下限（测试用）。
func SidebarMinWidthForTest() int { return sidebarWidthMin }

// SidebarDefaultWidthForTest 返回侧栏默认宽度（测试用）。
func SidebarDefaultWidthForTest() int { return sidebarWidthDefault }

// SetDraftForTest 直接设置草稿（测试用）。
func (a *App) SetDraftForTest(text string) { a.draft = text }

// DraftForTest 返回当前草稿（测试用）。
func (a *App) DraftForTest() string { return a.draft }

// SetPostForTest 替换 Post 回调（测试用）。
func (a *App) SetPostForTest(post func(func())) {
	if post != nil {
		a.post = post
		a.opts.Post = post
	}
}

// ReasoningEffortForTest 返回会话当前的思考档位（测试用）。
func (s *Session) ReasoningEffortForTest() string { return s.Effort() }

// VisionForTest 返回会话当前模型是否支持图片（测试用）。
func (s *Session) VisionForTest() bool { return s.Session.Vision() }

// ContextWindowForTest 返回会话当前的上下文窗口（测试用）。
func (s *Session) ContextWindowForTest() int { return s.Session.EffectiveContextWindow() }

// QueuedForTest 返回排队中的插话（测试用）。
func (s *Session) QueuedForTest() []string { return s.Session.Queued() }

// CanSteerForTest 报告当前能否插话（测试用）。
func (s *Session) CanSteerForTest() bool { return s.Session.CanSteer() }

// Selection 返回会话当前使用的提供商与模型。
func (s *Session) Selection() providers.Selection { return s.Session.Selection() }

// App 是根视图状态：会话列表 + 当前会话。
type App struct {
	rt   Runtime
	opts Options
	post func(func())
	// host 是执行端（单机 = 进程内 Local；展示端 = 走连接的 Remote）。
	//
	// uiapp 只通过它读状态、提交命令：driver / 日志 / blob / 工具都在 host 后面，
	// 这样"展示端复现执行端状态"不需要第二套界面逻辑（见 docs/remote-design.md）。
	host host.Host
	// hostCancel 取消 host 变更订阅（换 host 或退出时必须调用，否则旧订阅会泄漏）。
	hostCancel func()
	// relay 是本机作为执行端注册到 relay 的运行状态（斜杠命令 /connect 管理）。
	relayMu sync.Mutex
	relay   relaySession
	// lang 是界面语言（所有界面文字走 a.tr）。
	lang locale.Lang

	sessions   []*Session
	active     int
	draft      string
	startupErr string
	// history 是输入框 ↑/↓ 翻历史的状态（只在这里，不进 driver、不落盘）
	history historyState
	// theme 是应用统一使用的主题（dsh 浅色；实例缓存，避免每帧改根样式）。
	theme *ui.Theme
	// sidebarQuery 是侧栏搜索框的内容；sidebarList 是项目/会话树的滚动与行状态。
	sidebarQuery string
	sidebarList  ui.ListState
	// sidebarGroupSeen 记录"这个项目已经出现过"（用来给新项目默认展开）；
	// sidebarGroupOpen 记住用户手动收起/展开的结果（纯界面态，不落盘）。
	sidebarGroupSeen map[string]bool
	sidebarGroupOpen map[string]bool
	// showArchived 记录哪些项目打开了"显示已归档"（key 是项目 ID）。
	//
	// **纯内存态，重启不保留**：用户明确要求"只需要运行时保持，重启不用记状态"。
	// 默认不显示归档会话 —— 归档的意图就是收起来，一进来就看见等于没归档。
	// 开关放在项目行的菜单里（对齐 dsh：那是项目自己的视角设置）。
	showArchived map[string]bool
	// deleteConfirm/deleteTarget 是"删除会话"的二次确认。
	deleteConfirm bool
	deleteTarget  string
	// flashErr 是"下一帧用 Toast 显示一次"的错误（删除会话失败等）。
	flashErr string
	// pickingProject 表示正在弹目录选择器（防重复点击，对齐 dsh 的 directoryBusy）。
	pickingProject bool
	// pendingNewSession 是还没收到结果的新建会话命令数（结果里带新会话 id）。
	pendingNewSession int
	// activeAfterDelete 是删除会话后应该停在哪（删除前算好）。
	activeAfterDelete string
	// pending 记录已经"乐观清空"但还没收到 cmdResult 的发送。
	//
	// 命令是异步的：Submit 返回不代表成功。失败时要把草稿与附件还给用户，
	// 否则"发送失败 + 内容被清空"会让人白打一段字（e8c389b 修过同类问题）。
	pending map[string]pendingSend
	// viewportW/H 是上一帧的窗口尺寸（测试用：收起判定要用它）。
	viewportW, viewportH float32
	// lastTheme 是上一帧实际使用的主题（测试用）。
	lastTheme *ui.Theme
	// sidebarWidth 是侧栏宽度（0 = 使用默认值），由 ui.Split 拖动改写。
	sidebarWidth float32
	// sidebarUserCollapsed 是用户手动收起（宽窗口下也保持图标栏）。
	sidebarUserCollapsed bool
	// sidebarExpandNarrow 是用户在窄窗口下主动展开（dsh 的 narrowExpanded）。
	sidebarExpandNarrow bool
	// contentWidthPreference 是用户拖动出来的正文列宽度（0 = 没设置过，
	// 用默认 chatColumnWidth），由宽度把手改写。
	//
	// 这是**本机偏好**（跟着这台客户端走，不跟执行端），松手时写 ui-state.json。
	contentWidthPreference float32
	// uiStatePath 是本机偏好的落盘路径（空表示不持久化，例如取不到配置目录）。
	uiStatePath string
	// uiStateSaveFailed 记录"这次运行已经提示过保存失败"，避免拖动时刷屏。
	uiStateSaveFailed bool
	// widthDragging 记录这一帧是否正在拖正文宽度（Draw 里要用：光标离开热区
	// 但手指还按着时，光条要一直显示）。
	widthDragging bool
	// chatAreaW 是正文区容器的实测宽度（上一帧）。把手宽度与偏移都以它为
	// 分母/基准——用窗口宽度会让侧栏右边那条区域算出不对称的位置。
	chatAreaW float32
	// executors 非空表示这是经 relay 的展示端：可以列出/切换执行端。
	executors   ExecutorSource
	pickerOpen  bool
	pickerIndex int
	// executorList 是后台拉来的执行端列表缓存（渲染只读它，绝不发网络请求）。
	executorList     []ExecutorRef
	executorErr      string
	executorFetched  time.Time
	executorFetching bool
	// draftsByHost 是按"执行端 + 会话"分开保存的草稿：切换执行端不能把 A 的草稿带到 B。
	draftsByHost map[string]string
	// copiedAt 记录每个复制按钮的"已复制"到期时间（key 区分消息/代码块）。
	copiedAt map[string]time.Time
	// refocusComposer 表示下一帧把焦点还给输入框。
	//
	// 插在输入框**前面**的兄弟元素（附件条、任务清单、斜杠候选、排队条…）
	// 出现/消失会让元素树变化、焦点丢失（实测贴完图后输入框就不再聚焦，
	// 用户得再点一下才能继续打字）；斜杠候选也是同一个毛病：
	// 打 "/" 时它冒出来，输入框就失焦了。
	refocusComposer bool
	// composerSiblings 是上一帧"插在输入框前面的兄弟元素"数量。
	//
	// 拿它跟本帧比：**只要数量变了就重新聚焦**。比逐个条件判断稳 ——
	// 以后再加一个插在输入框前的面板（比如审批条）不需要改这里。
	composerSiblings int
	// PasteImagesForTest 覆盖剪贴板读取（测试用：headless 里没有真实剪贴板图片）。
	PasteImagesForTest func() ([][]byte, error)
	// pendingCaret/pendingCaretArmed 是"下一帧把输入框光标放到这里"的请求。
	// 在本帧设置会被编辑器缓冲的**旧长度**截断，所以只能延后一帧。
	pendingCaret      int
	pendingCaretArmed bool
	// expandedParts 记录哪些折叠行被展开（key = 消息 id + part 序号）。
	expandedParts map[string]bool
	// expandedFullParts 记录哪些过程组被**二级展开**（显示全部步骤）。
	//
	// 一级展开只显示最后 processPreviewSteps 步；这一层再点"展开全部"才全给。
	// 这里的 key 一定是 expandedParts 的子集：收起一级时要把二级一起清掉，
	// 否则下次展开会直接跳到"全部"，与"默认只显示最后几步"的预期不符。
	expandedFullParts map[string]bool
	// mdParser/mdCache 是 markdown 解析与渲染缓存（键含消息 id 与内容哈希）
	mdParser *markdown.Parser
	mdCache  *markdown.Cache
	// images 是按 URL 缓存的图片状态（异步下载 + 解码）
	images *imageStore
	// imageFetches 记录实际发起的下载次数（测试用）
	imageFetches int64
	// hlCache/highlighter 是代码高亮（键含语言与内容）
	hlCache     *markdown.LRU[[]markdown.HighlightLine]
	highlighter *markdown.Highlighter
}

// historyState 是"翻输入历史"的游标。
//
// index == len(历史) 表示没在翻（正在编辑自己的草稿）；active 为 false 时
// stash 里的内容没有意义。
type historyState struct {
	// sessionID 记录这份状态属于哪个会话：切换会话就整体重置，
	// 免得把 A 会话的历史/草稿串到 B 会话（草稿本身目前是全局的）。
	sessionID string
	active    bool
	index     int
	// stash 是开始翻历史前，输入框里那半句还没写完的话。
	stash string
}

// reset 清空翻历史状态（切换会话、发送之后）。
func (h *historyState) reset() {
	h.active = false
	h.index = 0
	h.stash = ""
}

// historyTexts 返回当前会话里用户输入过的文本（时间顺序，连续重复只留一条）。
//
// 只看 Conversation：压缩后被折叠进归档的部分不在里面——归档条目本来
// "只用于显示、不参与请求"，翻历史也不该翻出已经离开上下文的旧话。
func (s *Session) historyTexts() []string {
	var out []string
	for _, entry := range s.Conversation().Entries() {
		if entry.Message == nil || entry.Message.Message.Role != message.RoleUser {
			continue
		}
		var builder strings.Builder
		for _, part := range entry.Message.Message.Parts {
			if text, ok := part.AsText(); ok {
				builder.WriteString(text)
			}
		}
		text := builder.String()
		if strings.TrimSpace(text) == "" {
			continue
		}
		if len(out) > 0 && out[len(out)-1] == text {
			continue // 连续重复（比如重试发了同一句）不重复占一位
		}
		out = append(out, text)
	}
	return out
}

// historyNavigate 处理 ↑/↓ 翻历史，返回是否认领了按键。
func (a *App) historyNavigate(session *Session, delta, caret int) bool {
	if session == nil {
		return false
	}
	if a.history.sessionID != session.ID {
		a.history = historyState{sessionID: session.ID}
	}
	texts := session.historyTexts()
	if len(texts) == 0 {
		return false
	}
	// 翻出的历史条目被改过：改后的内容就是**新的草稿**，退出翻历史状态后
	// 再按本次按键重新处理（↑ 还能继续往上翻，↓ 不再有"更晚的一条"）。
	if a.history.active && a.history.index < len(texts) && a.draft != texts[a.history.index] {
		edited := a.draft
		a.history = historyState{sessionID: session.ID}
		a.draft = edited
	}
	if !a.history.active {
		// 没在翻历史、↓ 没有"更晚的一条"可去：交还编辑器
		if delta > 0 {
			return false
		}
		// 长单行自动折行时，光标不在开头按 ↑ 多半是想把光标往上移，
		// 不该突然翻历史（草稿为空时照常翻）
		if a.draft != "" && caret != 0 {
			return false
		}
		a.history.active = true
		a.history.stash = a.draft
		a.history.index = len(texts)
	}
	next := a.history.index + delta
	if next >= len(texts) {
		// 翻过最新一条 → 回到自己没写完的草稿
		stash := a.history.stash
		a.history.reset()
		a.draft = stash
		return true
	}
	if next < 0 {
		next = 0 // 到最早一条就停住
	}
	a.history.index = next
	a.draft = texts[next]
	return true
}

// NewApp 构造 App（初始一个空会话）。
func NewApp(rt Runtime, opts Options) *App {
	post := opts.Post
	if post == nil {
		post = func(f func()) { f() }
	}
	lang := opts.Language
	if lang == "" {
		lang = locale.Detect()
	}
	locale.SetCurrent(lang) // 进程级默认（图片校验等拿不到 App 的地方用）
	app := &App{
		rt:                rt,
		opts:              opts,
		post:              post,
		lang:              lang,
		mdParser:          markdown.NewParser(),
		mdCache:           markdown.NewCache(256),
		images:            newImageStore(),
		hlCache:           markdown.NewLRU[[]markdown.HighlightLine](128),
		expandedParts:     map[string]bool{},
		expandedFullParts: map[string]bool{},
	}
	app.loadUIState()
	app.executors = opts.Executors
	if app.executors != nil && app.executors.Current() == "" {
		// 经 relay 但还没选定执行端：启动即显示选择列表
		app.pickerOpen = true
	}
	if opts.Host != nil {
		app.host = opts.Host
		app.syncSessions()
		app.hostCancel = app.host.Subscribe(app.onHostChange)
		if errText := app.host.StartupError(); errText != "" && app.Current() != nil {
			app.startupErr = errText
			app.NotifyDisplay(errText)
		}
		return app
	}
	if opts.LocalConfig != nil {
		// 生产路径：执行端配置由入口一次性组装（UI 与 headless 共用同一份），
		// 这里只补齐界面才知道的部分。
		local := *opts.LocalConfig
		local.Runtime = rt
		local.Post = post
		local.Tr = app.tr
		if local.Efforts == nil {
			local.Efforts = app.effortsFor
		}
		app.host = host.NewLocal(local)
	} else {
		app.host = host.NewLocal(localConfigFromOptions(rt, opts, post, app.tr, app.effortsFor))
	}
	if local, ok := app.host.(*host.Local); ok {
		local.SetEfforts(app.effortsFor)
	}
	// 会话壳（界面状态）与执行端会话一一对应
	app.syncSessions()
	// 保存取消函数：以前丢弃返回值，一旦将来要换 host 或重复订阅就会泄漏
	// （旧订阅继续往已失效的界面状态里写）。
	app.hostCancel = app.host.Subscribe(app.onHostChange)
	app.startupErr = app.host.StartupError()
	if app.startupErr != "" && app.Current() != nil {
		// 有会话时：用显示型提示告诉用户"有些会话没恢复出来"（不写日志）
		app.NotifyDisplay(app.startupErr)
	}
	return app
}

// localConfigFromOptions 把字段式 Options 翻成执行端配置。
//
// **只在测试路径上用**：生产入口一律通过 Options.LocalConfig 传入由
// `cmd/simple-agent` 组装的配置，UI 与 headless 共用同一份，避免两处手写
// 字段映射（漏一个字段 = 某个工具静默消失，界面上完全看不出来）。
//
// 测试需要的是"几个开关"，给它一整份 host.Config 太重；这条路径让测试继续
// 只写 Options{AllowShell: true} 这种最小配置。
func localConfigFromOptions(rt Runtime, opts Options, post func(func()),
	tr func(string, ...any) string, efforts func(providers.Selection) []string) host.Config {
	return host.Config{
		Runtime:            rt,
		Selection:          opts.Selection,
		SystemPrompt:       opts.SystemPrompt,
		AllowShell:         opts.AllowShell,
		WebFetch:           opts.WebFetch,
		Name:               opts.Name,
		Token:              opts.Token,
		Workspace:          opts.Workspace,
		MaxToolRounds:      opts.MaxToolRounds,
		ContextWindow:      opts.ContextWindow,
		WindowFor:          opts.WindowFor,
		MaxOutputFor:       opts.MaxOutputFor,
		EffortFor:          opts.EffortFor,
		VisionFor:          opts.VisionFor,
		DisableAutoCompact: opts.DisableAutoCompact,
		Blobs:              opts.Blobs,
		Catalog:            opts.Catalog,
		NewID:              opts.NewID,
		Post:               post,
		Tr:                 tr,
		Title:              opts.Title,
		Efforts:            efforts,
		Deferred:           opts.DeferredCommands,
		// 执行端把模型列表公布给展示端（providerChoices 已按配置生成）。
		Models: opts.Providers,
	}
}

// ProjectIDForTest 返回会话所属项目 ID（供侧栏分组用）。
func (s *Session) ProjectIDForTest() string { return s.projectID }

// ProjectNameForTest 返回会话所属项目名（显示用）。
func (s *Session) ProjectNameForTest() string { return s.projectName }

// SetProjectForTest 设置会话的项目信息（测试用：模拟后端同步过来的档案数据）。
func (s *Session) SetProjectForTest(id, name string) {
	s.projectID = id
	s.projectName = name
}

// ArchivedForTest 报告会话是否已归档。
func (s *Session) ArchivedForTest() bool { return s.archivedSession }

// ShowArchivedForTest 报告某个项目是否打开了"显示已归档"。
func (a *App) ShowArchivedForTest(projectKey string) bool {
	return a.showArchived != nil && a.showArchived[projectKey]
}

// SetShowArchivedForTest 设置某个项目的"显示已归档"（测试用）。
func (a *App) SetShowArchivedForTest(projectKey string, show bool) {
	if a.showArchived == nil {
		a.showArchived = map[string]bool{}
	}
	a.showArchived[projectKey] = show
}

// SetArchivedForTest 设置会话的归档状态（测试用：模拟后端同步过来的状态）。
func (s *Session) SetArchivedForTest(archived bool) { s.archivedSession = archived }

// projectOrder 返回"项目 ID → 名次"的映射（用于侧栏排序）。
//
// 名次就是会话出现的先后：后端 Sessions() 按 projects.json 的顺序返回
// 会话，所以第一次遇到某个项目的位置就是它的名次。这样界面不需要额外
// 请求项目列表。
func (a *App) projectOrder() map[string]int {
	order := make(map[string]int, 8)
	next := 0
	for _, session := range a.sessions {
		id := session.projectID
		if id == "" {
			continue
		}
		if _, ok := order[id]; !ok {
			order[id] = next
			next++
		}
	}
	if len(order) == 0 {
		return nil
	}
	return order
}

// syncSessions 让界面壳与执行端会话一一对应（新建/删除/恢复后调用）。
//
// 界面壳只保留**本地界面状态**（滚动、展开、草稿、附件…）；执行端状态在
// host.Session 里，这里按 id 对齐：新增的建壳、消失的删壳。
func (a *App) syncSessions() {
	// 记住当前会话 id：重建列表后尽量保持"当前还是同一个"
	var previous string
	if a.active >= 0 && a.active < len(a.sessions) {
		previous = a.sessions[a.active].ID
	}
	refs := a.host.Sessions()
	next := make([]*Session, 0, len(refs))
	for _, ref := range refs {
		shell := a.shellByID(ref.ID)
		if shell == nil {
			// todoCollapsed 默认 true：对齐 dsh 的 `useState(true)`（面板初始折叠）
			shell = &Session{Session: a.host.Session(ref.ID), list: ui.ListState{FollowEnd: true}, todoCollapsed: true}
			shell.list.Key = func(row int) any {
				entries := shell.Conversation().Entries()
				if row < 0 || row >= len(entries) {
					return row
				}
				return entries[row].ID()
			}
		}
		// 项目信息与归档状态每帧同步：它可能被别的窗口/命令改掉，
		// 界面上"归到哪个项目、是否已归档"必须跟着变。
		shell.projectID = ref.ProjectID
		shell.projectName = ref.ProjectName
		shell.archivedSession = ref.Archived
		next = append(next, shell)
	}
	a.sessions = next

	// 优先保持原来的当前会话；它被删了才跟随执行端的当前会话
	active := -1
	for index, shell := range a.sessions {
		if previous != "" && shell.ID == previous {
			active = index
			break
		}
	}
	if active < 0 {
		if current := a.host.Active(); current != nil {
			for index, shell := range a.sessions {
				if shell.ID == current.ID {
					active = index
					break
				}
			}
		}
	}
	if active < 0 {
		active = len(a.sessions) - 1
	}
	if active < 0 {
		active = 0
	}
	a.active = active
	// 执行端的"当前会话"跟着界面走（命令要落到用户正在看的这个会话上）
	if a.active < len(a.sessions) {
		shell := a.sessions[a.active]
		a.host.SetActive(shell.ID)
		// 这里不是"用户切换会话"，而是列表刷新后的重新定位：
		// **不保存**当前草稿（否则会把刚输入的内容误存到别的会话上），
		// 只在输入框为空时把该会话自己的内容放回去。
		if strings.TrimSpace(a.draft) == "" {
			a.restoreDraftIfIdle(shell)
			if a.draft == "" {
				a.draft = shell.draft
			}
		}
	}
}

// shellByID 按 id 找界面壳（不存在返回 nil）。
func (a *App) shellByID(id string) *Session {
	for _, shell := range a.sessions {
		if shell.ID == id {
			return shell
		}
	}
	return nil
}

// onHostChange 收到执行端的变化：粗粒度通知，下一帧整体重绘。
func (a *App) onHostChange(change host.Change) {
	switch change.Kind {
	case "sessions":
		a.syncSessions()
	case "cmdResult":
		a.handleCmdResult(change)
	case "notice":
		// Conversation 的内容只由执行端维护（单机就是同一个对象）。
		// uiapp 收到 notice 只负责重绘，否则同一条提示会出现两次。
	case "connection":
		// 重连成功后清掉图片的失败状态：断线时的"加载失败"不是结论，要允许重拉
		if change.Connected {
			a.images.clearFailed()
		}
		// 展示端断线/重连：在会话里留一条提示（输入卡在断线时由 host 侧拒绝命令）
		if change.Notice != nil {
			if shell := a.Current(); shell != nil {
				_, _ = shell.Conversation().AddNotice(
					conversation.NoticeLevel(change.Notice.Level), change.Notice.Text)
			}
		}
	}
	a.RequestRedraw()
}

// restoreDraftIfIdle 在切到某个会话时，把"失败留下的草稿"放回空的输入框。
func (a *App) restoreDraftIfIdle(session *Session) {
	if session == nil || session.restoreDraft == "" {
		return
	}
	if strings.TrimSpace(a.draft) != "" {
		return // 用户已经在写别的了，不覆盖
	}
	a.draft = session.restoreDraft
	session.restoreDraft = ""
	a.pendingCaret = -1
}

// handleCmdResult 处理命令结果：失败原因原样显示（跨会话不串）。
//
// 失败时要**还原**乐观清空的草稿与附件：命令是异步的，Submit 返回成功不代表
// 执行成功（P2 起执行端在远端，预检查用的还是镜像状态）。
func (a *App) handleCmdResult(change host.Change) {
	result := change.Result
	shell := a.shellByID(change.SessionID)
	pending, hasPending := a.pending[change.CmdID]
	if hasPending {
		delete(a.pending, change.CmdID)
	}

	// 命令可能改变会话列表（新建/删除）：同步一次（同步执行时结果已在 Submit 内到达）
	a.syncSessions()

	if result.OK && shell != nil && result.Notice == "" {
		shell.errMsg = ""
	}
	if shell != nil && result.Kind == "loadOlder" && result.Error != "" {
		shell.loadingOlder = false
	}
	// 归档分页：把这一页插到"更早的历史"最前面（更早的在上）
	if shell != nil && result.Kind == "loadOlder" {
		// 只接受界面上真正请求过的页（读完后迟到的结果直接忽略）
		requested := shell.loadingOlder
		shell.loadingOlder = false
		page, ok := result.Data.(host.OlderPage)
		if requested && ok {
			// 这一页比已有的更早：插到最前面，时间顺序才对
			older := make([]archivedEntry, 0, len(page.Entries))
			for _, entry := range page.Entries {
				older = append(older, archivedEntry{
					Message: entry.Message, Status: entry.Status,
					Reason: entry.Reason, CreatedAt: time.Unix(entry.CreatedAt, 0),
				})
			}
			shell.archived = append(older, shell.archived...)
			shell.noMoreArchives = !page.More
			shell.archiveNext = page.Next
		}
	}
	// 新建会话：用结果里的 id 切换（不能靠"提交后查列表"）
	if result.OK && a.pendingNewSession > 0 {
		if data, ok := result.Data.(host.NewSessionData); ok && data.SessionID != "" {
			a.pendingNewSession--
			a.syncSessions()
			for i, shell := range a.sessions {
				if shell.ID == data.SessionID {
					a.switchTo(i)
					break
				}
			}
		}
	}
	// 删除会话：按删除前定下的"删除后停在哪"
	if a.activeAfterDelete != "" {
		want := a.activeAfterDelete
		a.activeAfterDelete = ""
		for i, shell := range a.sessions {
			if shell.ID == want {
				a.switchTo(i)
				break
			}
		}
	}
	if result.Error != "" && shell != nil {
		shell.errMsg = result.Error
	}
	if result.Error != "" && hasPending && result.Unknown {
		// 命令已经发出去、结果未知：**不还原草稿**（执行端可能已经执行，
		// 再发一次会重复），只提示用户重连后查看
		a.flashErr = a.tr("composer.cmdUnknown")
	}
	if result.Error != "" && hasPending && !result.Unknown && shell != nil {
		current := a.Current()
		// 用户在这期间已经切到别的会话：草稿只属于原会话，不能还原到当前输入框
		if current == nil || current.ID != pending.sessionID {
			// 存进**原会话**的槽位：用户切回去（输入框为空）时再放回输入框
			shell.restoreDraft = pending.draft
			if len(pending.attachments) > 0 {
				shell.attachments = append(pending.attachments, shell.attachments...)
			}
			a.flashErr = a.tr("composer.sendFailedOther", result.Error)
		} else if strings.TrimSpace(a.draft) != "" {
			// 用户已经开始输入新内容：不覆盖，只提示
			a.flashErr = a.tr("composer.sendFailedKept", result.Error)
		} else {
			a.draft = pending.draft
			if len(pending.attachments) > 0 {
				shell.attachments = append(pending.attachments, shell.attachments...)
			}
			a.flashErr = a.tr("composer.sendFailedRestored", result.Error)
		}
	}
	if result.Notice != "" {
		a.flashErr = result.Notice
	}
}

// NewSession 新建会话并切过去（命令走执行端）。
//
// **不能**"提交后立刻查列表"来猜新会话：命令是异步的（Deferred/远程），这时它
// 还不存在。执行端在 cmdResult 的 Data 里带回新会话 id，收到之后才切换。
func (a *App) NewSession() *Session {
	a.pendingNewSession++
	_, _ = a.host.Submit(host.Command{Kind: "newSession"})
	return a.Current()
}

// newSessionIn 在指定工作区新建会话（「添加项目」用它：加项目 = 在该目录开一个会话）。
//
// 项目在侧栏是**从会话的 workspace 反推**出来的（没有独立的项目存储），
// 所以"添加一个空项目"无法表达——必须真的开一个会话，项目才会出现在树里。
// 这也是 dsh 的语义：选中工作区后 openWorkspace 会建/开一个空会话。
func (a *App) newSessionIn(workspace string) {
	payload, err := json.Marshal(host.NewSessionPayload{Workspace: workspace})
	if err != nil {
		return
	}
	a.pendingNewSession++
	_, _ = a.host.Submit(host.Command{Kind: "newSession", Payload: payload})
}

// addProject 让用户选一个目录作为新项目（对齐 dsh 的「添加工作区…」）。
//
// 流程：系统目录选择器 → 去重检查 → 在该目录新建会话。
//
// `mygo.Dialog.Open` 会**阻塞到对话框关闭**，所以必须放到 goroutine 里，
// 结果再 post 回主线程（与 pickImageFiles 同一套做法）。
func (a *App) addProject() {
	if a.opts.PickDirectory == nil {
		return
	}
	pick := a.opts.PickDirectory
	// 已经在选：忽略重复点击（dsh 的 directoryBusy 语义）
	if a.pickingProject {
		return
	}
	a.pickingProject = true
	go func() {
		dir, err := pick()
		a.post(func() {
			a.pickingProject = false
			if err != nil {
				a.flashErr = a.tr("project.pickFailed", err.Error())
				return
			}
			dir = strings.TrimSpace(dir)
			if dir == "" {
				return // 用户取消
			}
			a.adoptProject(dir)
		})
	}()
}

// adoptProject 把一个目录接纳为新项目；已存在同名项目时提示并放弃。
func (a *App) adoptProject(dir string) {
	if name := a.projectNameTaken(dir); name != "" {
		// 对齐 dsh 的 conflict.named：重名不建第二个组，直接告诉用户
		a.flashErr = a.tr("project.conflict", name)
		return
	}
	a.newSessionIn(dir)
}

// projectNameTaken 返回与 dir 同名的已有项目**显示名**（没有则空）。
//
// 比较的是"目录显示名"而不是完整路径：侧栏显示的就是目录名，
// 两个不同路径但同名目录在侧栏看起来是完全一样的两个组，用户无法分辨。
func (a *App) projectNameTaken(dir string) string {
	name := a.workspaceGroupName(dir)
	for _, session := range a.sessions {
		existing := workspaceOr(session.Workspace, "")
		if existing == "" {
			continue
		}
		if a.workspaceGroupName(existing) == name {
			return name
		}
	}
	return ""
}

// Close 关闭执行端（刷盘 + 关文件）。
func (a *App) Close() {
	if a.hostCancel != nil {
		a.hostCancel()
		a.hostCancel = nil
	}
	// relay 注册也要停掉，否则进程退出前还在重连
	a.stopRelayRegistration()
	if a.host != nil {
		_ = a.host.Close()
	}
}

// deleteSession 删除会话（中止、关日志、删目录都由执行端完成）。
func (a *App) deleteSession(index int) {
	if index < 0 || index >= len(a.sessions) {
		return
	}
	// 与旧行为一致：删前面的会话时当前会话不变；删当前会话时落到它后面的那个
	var nextID string
	switch {
	case index < a.active:
		nextID = a.sessions[a.active].ID
	case index == a.active && index+1 < len(a.sessions):
		nextID = a.sessions[index+1].ID
	case index == a.active && index-1 >= 0:
		nextID = a.sessions[index-1].ID
	}
	id := a.sessions[index].ID
	a.activeAfterDelete = nextID
	// 删的是当前会话：输入框里那份草稿也要一起丢掉。
	// （别的会话的草稿挂在各自的 Session 上，随对象一起被回收。）
	if index == a.active {
		a.draft = ""
		if index < len(a.sessions) {
			a.sessions[index].draft = ""
		}
	}
	_, _ = a.host.Submit(host.Command{SessionID: id, Kind: "deleteSession"})
}

// Compact 手动压缩上下文（界面上「压缩上下文」按钮）。
func (a *App) Compact() {
	session := a.Current()
	if session == nil || session.Busy() != driver.BusyIdle {
		return
	}
	_, _ = a.host.Submit(host.Command{SessionID: session.ID, Kind: "compact"})
}

// loadOlder 按需加载更早的归档（结果经 cmdResult 回来）。
func (a *App) loadOlder(session *Session) {
	if session.loadingOlder || session.noMoreArchives {
		return
	}
	session.loadingOlder = true
	payload, _ := json.Marshal(host.LoadOlderPayload{Cursor: session.archiveNext})
	_, _ = a.host.Submit(host.Command{SessionID: session.ID, Kind: "loadOlder", Payload: payload})
}

// find 按 id 查找界面壳（迟到的回调靠它判断会话是否还在）。
func (a *App) find(id string) *Session { return a.shellByID(id) }

// Current 返回当前会话（没有则 nil）。
func (a *App) Current() *Session {
	if a.active < 0 || a.active >= len(a.sessions) {
		return nil
	}
	return a.sessions[a.active]
}

// sessionIndex 返回会话下标（找不到 -1）。
func (a *App) sessionIndex(id string) int {
	for i, session := range a.sessions {
		if session.ID == id {
			return i
		}
	}
	return -1
}

// Sessions 返回全部会话。
func (a *App) Sessions() []*Session { return a.sessions }

// View 是根视图（纯函数：所有状态在 App 上）。
func (a *App) View(c *ui.Context) {
	// 始终使用 dsh 的浅色令牌（不跟随系统深色模式；见 themeFor）
	if a.theme == nil {
		a.theme = themeFor(c.Theme())
	}
	if c.Theme() != a.theme {
		c.SetTheme(a.theme)
	}
	theme := c.Theme()
	a.lastTheme = theme
	if a.flashErr != "" {
		c.Toast(a.flashErr)
		a.flashErr = ""
	}
	a.viewportW, a.viewportH = c.Size()
	ui.Row(c).Fill().Children(func() {
		// 左栏：会话列表（dsh 的宽度与收起形态，见 layout.go / sidebarMetrics）
		a.sidebarPane(c)

	})
	a.confirmDialogs(c)
}

// mainPane 渲染右栏：标题 + 消息列表 + 提问面板 + 输入卡。
//
// 拖放挂在**整个右栏**上（不只是输入框）：用户把图片拖进聊天区就该能收到，
// 否则很难对准。mygo 的 drop 是元素级，这里用最外层的会话容器。
func (a *App) mainPane(c *ui.Context) {
	if a.pickerOpen && a.executors != nil {
		a.executorPickerPane(c)
		return
	}
	if a.Current() == nil {
		// 没有会话：显示空态，**不渲染**头部与输入卡（决策 #54）
		a.emptyStatePane(c)
		return
	}
	chatArea := ui.Column(c).Grow(1).FillHeight().MinWidth(centerMinWidth)
	if chatArea.FileDragOver() {
		chatArea.Background(c.Theme().Surface)
	}
	if dropped := chatArea.DroppedFiles(); len(dropped) > 0 {
		a.attachFiles(a.Current(), dropped)
	}
	chatArea.Children(func() {
		a.header(c)
		a.chat(c)
		a.askPanel(c) // 固定在输入框上方：不会被列表裁切，按钮一定可点
		a.composer(c)
		// 正文区两侧的宽度把手（dsh 的 ConversationWidthControls）。
		// 放在最后：Absolute 定位，只覆盖两侧留白，不吃正文与输入卡的点击。
		// 记下容器实际宽度：把手宽度的算式以**它**为分母（不是窗口宽度）。
		a.setChatAreaWidth(chatArea.Bounds().W)
		a.contentWidthHandle(c, -1)
		a.contentWidthHandle(c, 1)
	})
}

// header 渲染标题栏（会话名 + 模型 + 状态）。
func (a *App) header(c *ui.Context) {
	session := a.Current()
	if session == nil {
		return
	}
	theme := c.Theme()
	ui.Column(c).Height(headerHeight).Padding(6, 12).Gap(2).FillWidth().Children(func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			// dsh 的头部：会话名 + 忙状态；模型只在输入卡里选（P3 的决定）
			ui.Text(c, session.Title).FontSize(14).Bold().TextColor(theme.Text)
			if label := a.busyLabel(session.Busy()); label != a.tr("busy.ready") {
				ui.Text(c, "· "+label).FontSize(12).TextColor(theme.TextMuted)
			}
			ui.Spacer(c)
			// 经 relay 时：头部显示当前执行端，点开可切换（直连模式没有这一项）
			if a.executors != nil {
				host := a.executors.Current()
				label := host
				if label == "" {
					label = a.tr("relay.pick.title")
				}
				switchButton := ui.Row(c).Padding(4, 8).Gap(6).Radius(6).
					AlignItems(ui.Center).Cursor(ui.CursorPointer).
					Label(a.tr("relay.switch"))
				if switchButton.Hovered() {
					switchButton.Background(dsh.SurfaceHover)
				}
				switchButton.Children(func() {
					ui.Text(c, label).FontSize(12).TextColor(theme.TextMuted)
				})
				if switchButton.Clicked() {
					a.OpenExecutorPicker()
				}
			}
			// 侧栏开关（dsh 的 HeaderLeadingControls 里的同一个图标）
			if iconButton(c, iconPanelLeftSVG, a.tr("toggle.collapse"), 16, 4) {
				a.sidebarUserCollapsed = true
			}
			// 更多：压缩上下文 / 删除会话（原生菜单）
			more := ui.Row(c).Padding(4).Radius(6).Cursor(ui.CursorPointer).
				Label(a.tr("header.more"))
			if more.Hovered() {
				more.Background(dsh.SurfaceHover)
			}
			more.Children(func() {
				ui.Icon(c, iconEllipsisSVG).Size(16, 16).TextColor(theme.TextMuted)
			})
			idle := session.Busy() == driver.BusyIdle
			more.Menu(func(m *ui.Menu) {
				if !idle {
					// 「更多」菜单里没有模型选项，所以这里不能讲"不能切换模型"
					m.Item(a.tr("header.compactBusy")).Disabled(true)
					m.Separator()
				}
				if m.Item(a.tr("header.compact")).Disabled(!idle).Chosen() {
					a.Compact()
				}
				m.Separator()
				if m.Item(a.tr("header.delete")).Chosen() {
					a.deleteTarget = session.ID
					a.deleteConfirm = true
				}
			})
		})
		if session.errMsg != "" {
			ui.Text(c, session.errMsg).FontSize(12).TextColor(theme.Danger)
		}
		ui.Box(c).Height(1).FillWidth().Background(theme.Border)
	})
}

// effortLabel 把档位值变成界面文案。
func (a *App) effortLabel(effort string) string {
	switch effort {
	case "":
		return a.tr("effort.default")
	case "off":
		return a.tr("effort.off")
	case "low":
		return a.tr("effort.low")
	case "medium":
		return a.tr("effort.medium")
	case "high":
		return a.tr("effort.high")
	case "xhigh":
		return a.tr("effort.xhigh")
	case "max":
		return a.tr("effort.max")
	default:
		return effort
	}
}

// providers 返回界面上可选的提供商与模型。
//
// 展示端（远程 Host）**必须**用执行端公布的那份：API Key 与模型配置都在执行端，
// 用本机配置会让菜单指向执行端不存在的模型（切过去直接失败）。执行端还没把列表
// 发过来时返回空——宁可暂时不显示选择器，也不要显示一份错的。
func (a *App) providers() []host.ProviderChoice {
	if a.host != nil && !a.host.Info().Local {
		return a.host.Providers()
	}
	return a.opts.Providers
}

// modelLabel 返回模型的显示名（没配置则用 id）。
func (a *App) modelLabel(providerID, model string) string {
	for _, choice := range a.providers() {
		if choice.ProviderID != providerID {
			continue
		}
		if label, ok := choice.Labels[model]; ok && label != "" {
			return label
		}
	}
	return model
}

// effortsFor 返回某个模型支持的思考档位。
func (a *App) effortsFor(sel providers.Selection) []string {
	for _, choice := range a.providers() {
		if choice.ProviderID != sel.ProviderID {
			continue
		}
		if efforts, ok := choice.Efforts[sel.Model]; ok {
			return efforts
		}
	}
	return nil
}

// ImageStateForTest 返回某 URL 的加载状态（测试用）。
func (a *App) ImageStateForTest(url string) (loaded bool, failed string) {
	state, ok := a.images.lookup(url)
	if !ok {
		return false, ""
	}
	return state.bitmap != nil, state.failed
}

// CheckImageURLForTest 暴露图片地址检查（测试用）。
func CheckImageURLForTest(url string) error { return checkImageURL(url) }

// ItoaForTest 暴露整数格式化（测试用）。
func ItoaForTest(value int) string { return itoa(value) }

// SetImageFetcherForTest 注入图片下载函数（测试用）。
func (a *App) SetImageFetcherForTest(fetcher func(url string) ([]byte, error)) {
	a.opts.ImageFetcher = fetcher
}

// ImageFetchCountForTest 返回实际发起的图片下载次数（测试用）。
func (a *App) ImageFetchCountForTest() int { return int(atomic.LoadInt64(&a.imageFetches)) }

// ImageCountForTest 返回已登记加载的图片数量（测试用）。
func (a *App) ImageCountForTest() int { return a.images.count() }

// CodeCacheStatsForTest 返回代码高亮缓存统计（测试用）。
func (a *App) CodeCacheStatsForTest() (hits, misses int) { return a.hlCache.Stats() }

// MarkdownCacheStatsForTest 返回缓存统计（测试用）。
func (a *App) MarkdownCacheStatsForTest() (hits, misses int) { return a.mdCache.Stats() }

// dataDir 是会话数据目录（展示给用户，方便自己排查）。
// blobs 返回当前生效的图片存储：远程展示端走 host（按需拉取/上传到执行端），
// 本地模式就是 store.Blobs。
func (a *App) blobs() host.BlobService {
	if a.host != nil {
		return a.host.Blobs()
	}
	if a.opts.Blobs == nil {
		return nil
	}
	return a.opts.Blobs
}

func (a *App) dataDir() string {
	if a.opts.Catalog == nil {
		return ""
	}
	// 展示给用户的是**数据目录**（sessions 的父目录），不是内部的 sessions 子目录
	return filepath.Dir(a.opts.Catalog.SessionsDir())
}

// primaryButton 渲染 dsh 风格的**近黑主按钮**（`dsh.ButtonSolid`）。
//
// 不用 `ui.PrimaryButton`：mygo 的 PrimaryButton 用主题的 Accent（品牌蓝），
// 而 dsh 的主按钮是近黑（见 §0.5.1 的说明）；圆形发送按钮也是这个颜色。
func (a *App) primaryButton(c *ui.Context, label string, onClick func()) {
	button := ui.Row(c).Gap(6).Padding(6, 14).Radius(8).Center().
		Background(dsh.ButtonSolid).Cursor(ui.CursorPointer).Label(label)
	if button.Hovered() {
		button.Background(dsh.BrandBlueLo)
	}
	button.Children(func() {
		ui.Text(c, label).FontSize(12).TextColor(dsh.BgBase)
	})
	if button.Clicked() {
		onClick()
	}
}

// Host 返回执行端（展示端接入、--listen 启动监听时用）。
func (a *App) Host() host.Host { return a.host }

// StartupError 返回启动时遇到的问题（打开/重放日志失败等），空表示正常。
func (a *App) StartupError() string { return a.startupErr }

// NotifyDisplay 在会话里插入一条**只用于显示**的提示（不写日志）。
//
// 用于「加载时日志损坏」「已有实例在运行」这类一次性提示：写进日志的话，
// 每次启动都会多一条，用户会以为会话里真的发生过这些事。
// 需要持久化的提示走 driver（它会产生 notice.appended 事件）。
func (a *App) NotifyDisplay(text string) {
	session := a.Current()
	if session == nil {
		return
	}
	_, _ = session.Conversation().AddNotice(conversation.NoticeInfo, text)
}

// RequestRedraw 请求重绘（无法拿到 ui.Context 的场合用）。
func (a *App) RequestRedraw() {
	if a.opts.Post != nil {
		a.opts.Post(func() {})
	}
}

// chat 渲染消息列表。
func (a *App) chat(c *ui.Context) {
	session := a.Current()
	if session == nil {
		return
	}
	// 渲染条目：一条消息可能被拆成"正文 + 过程组"，而**连续的过程**
	// （思考/工具调用，跨消息也算）会合并进同一个折叠组。
	items := a.buildChatItems(session)
	// 归档条目只显示，排在会话内容前面（永远不进 Conversation）
	rows := len(session.archived) + len(items)
	// 对齐 deepseek-harness：正文列居中、最宽 chatColumnWidth（748），两侧留白；
	// 宽窗口下不让每行拉得太长。列表要 Grow(1) 才吃满高度。
	ui.Column(c).FillHeight().Padding(chatSidePadding).Children(func() {
		// FillWidth 是必须的：不给宽度时这个 Column 会被内容撑开——
		// 消息短的时候列很窄并居中，只有出现长消息时才变宽（用户报的"抖动"）。
		ui.Column(c).Grow(1).FillWidth().MaxWidth(a.contentWidth()).
			AlignSelf(ui.Center).Children(func() {
			ui.List(c, &session.list, rows, func(row int) {
				if archived := len(session.archived); row < archived {
					a.archivedRow(c, session.archived[row])
					return
				}
				a.renderChatItem(c, items[row-len(session.archived)])
			}).Grow(1)
		})
	})

	// 滚到顶 → 按需加载更早的归档
	if first, _ := session.list.Visible(); first <= 0 && len(session.archived) == 0 {
		a.loadOlder(session)
	}
}

// textButton 画一个无边框的文本按钮（悬停才出现底色）。
//
// mygo 的 ui.Button 自带边框与面板色，用在侧栏/工具行/标题栏会显得很重；
// harness 里这些都是「安静」的文本行，只有悬停时才浮出背景。
func textButton(c *ui.Context, label string, size float32, color ui.Color) bool {
	return textButtonLines(c, label, size, color, 0)
}

// textButtonLines 同上，但限制最多显示几行（0 = 不限）。
//
// 折叠行必须单行：思考与工具摘要很长时换行会把行高撑开，看起来像正文。
func textButtonLines(c *ui.Context, label string, size float32, color ui.Color, maxLines int) bool {
	theme := c.Theme()
	element := ui.Text(c, label).FontSize(size).TextColor(color).
		Padding(2, 6).Radius(6).Cursor(ui.CursorPointer)
	if maxLines > 0 {
		element.MaxLines(maxLines)
	}
	if element.Hovered() {
		element.Background(theme.SurfaceHover)
	}
	return element.Clicked()
}

// outputIndicator 给工具结果行加一个前缀符号（harness 用图标，我们用字符）。
func outputIndicator(text string) string {
	switch text {
	case "toolFailed":
		return "✕"
	case "toolWaiting":
		return "？"
	default:
		return "⤷"
	}
}

// expansionKey 是「可展开行」的状态键（消息 + part 序号）。
func expansionKey(messageID string, partIndex int) string {
	return messageID + "#" + itoa(partIndex)
}

// hasTextAfter 报告某个 part 之后是否已经有正文（用来判断推理是否还在进行）。
func hasTextAfter(parts []message.Part, index int) bool {
	for _, part := range parts[index+1:] {
		if part.MediaType == render.MediaReasoning || part.MediaType == render.MediaSummary {
			continue
		}
		if text, ok := part.AsText(); ok && strings.TrimSpace(text) != "" {
			return true
		}
	}
	return false
}

// toolCallStatus 返回工具调用的当前状态文案（"" 表示无从判断）。
func (a *App) toolCallStatus(chat *conversation.ChatEntry, callID string, streaming bool) string {
	if status := a.toolCallStatusFor(chat, callID); status != "" {
		return status
	}
	if streaming {
		return a.tr("chat.toolRunning")
	}
	return ""
}

// lastLine 取最后一行非空文本并截断（流式预览用）。
func lastLine(text string, limit int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) > limit {
			return "…" + string(runes[len(runes)-limit:])
		}
		return line
	}
	return ""
}

// clipLine 把单行文本截断到指定长度。
func clipLine(text string, limit int) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\n", " "), "\r", " ")
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

// archivedRow 渲染一条「更早的历史」（来自归档，只读显示）。
//
// 状态必须显示：归档里可能有失败或已中止的半截回复。
func (a *App) archivedRow(c *ui.Context, entry archivedEntry) {
	theme := c.Theme()
	role := a.tr("chat.you")
	if entry.Message.Role == message.RoleAgent {
		role = a.tr("chat.assistant")
	}
	ui.Column(c).Padding(8).Gap(2).Children(func() {
		ui.Text(c, role+a.tr("chat.earlier")).FontSize(11).TextColor(theme.TextMuted)
		for index, part := range entry.Message.Parts {
			if text, ok := part.AsText(); ok && text != "" {
				if part.MediaType == render.MediaReasoning || part.MediaType == render.MediaSummary {
					ui.Text(c, text).FontSize(12).TextColor(theme.TextMuted)
					continue
				}
				a.renderMarkdown(c, entry.Message.MessageID+"-archived", index, text)
			}
		}
		switch entry.Status {
		case conversation.StatusFailed:
			ui.Text(c, a.tr("chat.failed", entry.Reason)).FontSize(11).TextColor(theme.Danger)
		case conversation.StatusCancelled:
			ui.Text(c, a.tr("chat.aborted")).FontSize(11).TextColor(theme.TextMuted)
		}
	})
}

// renderChatItem 渲染一个渲染条目。
func (a *App) renderChatItem(c *ui.Context, item chatItem) {
	theme := c.Theme()
	switch item.Kind {
	case itemNotice:
		color := theme.TextMuted
		switch item.Notice.Level {
		case conversation.NoticeError:
			color = theme.Danger
		case conversation.NoticeWarn:
			color = theme.Warning
		}
		ui.Row(c).Gap(6).Padding(6).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, noticeIcon(item.Notice.Level)).Size(13, 13).TextColor(color)
			ui.Text(c, item.Notice.Text).FontSize(12).TextColor(color)
		})
	case itemRoundFooter:
		a.renderRoundFooter(c, item)
	case itemContent:
		a.renderContent(c, item.Refs)
	case itemProcess:
		a.renderProcessItem(c, item)
	}
}

// emptyStatePane 渲染"一个会话都没有"的主区域：提示 + 新建按钮。
//
// 决策 #54（用户选 B）：删掉最后一个会话后允许列表为空；此时不渲染头部与
// 输入卡（不要显示一个禁用的输入框）。
func (a *App) emptyStatePane(c *ui.Context) {
	theme := c.Theme()
	ui.Column(c).Grow(1).FillHeight().MinWidth(centerMinWidth).Center().Gap(10).
		Children(func() {
			ui.Text(c, a.tr("session.empty.title")).FontSize(15).TextColor(theme.Text)
			ui.Text(c, a.tr("session.empty.hint")).FontSize(12).TextColor(theme.TextMuted)
			// 恢复失败要**说出来**：否则用户看到"还没有会话"会以为数据丢了
			if a.startupErr != "" {
				ui.Text(c, a.startupErr).FontSize(12).TextColor(theme.Danger).
					MaxLines(4).TextAlign(ui.Center)
				if dir := a.dataDir(); dir != "" {
					ui.Text(c, a.tr("session.dataDir", dir)).FontSize(11).
						TextColor(theme.TextMuted).MaxLines(2).TextAlign(ui.Center)
				}
			}
			a.primaryButton(c, a.tr("session.new.label"), func() { a.NewSession() })
		})
}

// renderContent 渲染正文条目（用户气泡或助手正文）。
func (a *App) renderContent(c *ui.Context, refs []partRef) {
	theme := c.Theme()
	chat := refs[0].Chat
	if chat.Message.Role == message.RoleUser {
		ui.Column(c).AlignSelf(ui.End).MaxWidth(chatBubbleMaxWidth).Children(func() {
			ui.Column(c).Background(theme.Selection).Radius(bubbleRadius).Padding(10, 14).Gap(6).Children(func() {
				for _, ref := range refs {
					a.partView(c, ref.Chat, ref.Index, ref.Chat.Message.Parts[ref.Index], theme.Text)
				}
			})
			// 气泡下方右对齐：时间 + 操作按钮组（dsh 的 message meta 行）。
			// 用户消息保留自己的复制入口（它不是回合最终输出）。
			ui.Row(c).AlignSelf(ui.End).Gap(8).Padding(2, 4).AlignItems(ui.Center).
				Children(func() {
					ui.Text(c, a.formatMessageTime(chat.CreatedAt)).
						FontSize(10).TextColor(theme.TextMuted)
					a.messageActionsRow(c, []messageAction{
						a.copyAction(chat, a.tr("common.copyMessage")),
					}, false)
				})
		})
		return
	}
	ui.Column(c).Gap(10).Padding(4, 0).Children(func() {
		for _, ref := range refs {
			a.partView(c, ref.Chat, ref.Index, ref.Chat.Message.Parts[ref.Index], theme.Text)
		}
		// 助手正文下方**不再放**复制按钮：复制只出现在回合结束行（最终输出），
		// 否则一个回合里的旁白、过程组各带一个按钮，界面上到处都是「复制」。
	})
}

// stepRefs 过滤出"算一步"的 part：思考与工具调用。
//
// **tool_result 不算一步**：它已经折进对应的调用行显示（见 partView 的
// MediaToolUse 分支），再计一次就会出现两套数字 —— 标题按调用数说
// 「6 步」，而"上面还有 N 步"/"展开全部 N 步"按 part 数算出 13，
// 同一组里自相矛盾。
//
// 数量同样用 timeline.StepCount 的算法（思考 + tool_use），
// 保证标题与展开按钮永远是同一个数字。
func stepRefs(refs []partRef) []partRef {
	out := make([]partRef, 0, len(refs))
	for _, ref := range refs {
		switch ref.Chat.Message.Parts[ref.Index].MediaType {
		case render.MediaToolResult:
			continue
		}
		out = append(out, ref)
	}
	return out
}

// processPreviewSteps 是一级展开时默认显示的过程步骤数。
//
// 一个长回合的过程组动辄几十步，全展开会把时间线撑得很长（往上翻要划很多屏）。
// 默认只给最后几步 —— 最近发生的通常最关心 —— 需要看全貌时再点"展开全部"。
// 与 Android 端 Timeline.kt 的 COLLAPSED_STEP_PREVIEW 保持一致。
const processPreviewSteps = 3

// renderProcessItem 渲染过程组：1 个 part 就内联，≥2 个合并成一行。
//
// 展开分两级（与 Android 端一致）：
//   - 一级：只显示**最后 processPreviewSteps 步**（最近发生的通常最关心）
//   - 二级：显示全部，另有按钮收起回一级
//
// 为什么要分级：一个长回合的过程组动辄几十步，全展开会把时间线撑得很长，
// 往上翻要划很多屏。
func (a *App) renderProcessItem(c *ui.Context, item chatItem) {
	theme := c.Theme()
	if len(item.Refs) == 1 {
		ref := item.Refs[0]
		a.partView(c, ref.Chat, ref.Index, ref.Chat.Message.Parts[ref.Index], theme.Text)
		return
	}

	expanded := a.expandedParts[item.Key]
	expandedFull := a.expandedFullParts[item.Key]
	last := item.Refs[len(item.Refs)-1]
	streaming := last.Chat.Status().IsStreaming()

	// 标题照抄 deepseek-harness 的 step-process：
	//   进行中 →「正在读取文件 · docs/plan.md」（活动 + 当前任务摘要）
	//   已完成 →「已读取文件并搜索代码」（按活动分类，最多三项，多则「等」）
	label := a.processGroupTitle(item.Refs, streaming)
	// 三角要反映当前状态：折叠 ▸ / 展开 ▾；标题前是活动图标（dsh 的 PROCESS_ICONS）
	icon := "▸"
	if expanded {
		icon = "▾"
	}
	header := func() {
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, activityIcon(groupActivity(item.Refs, streaming))).
				Size(13, 13).TextColor(theme.TextMuted)
			if textButtonLines(c, icon+" "+label, 12, theme.TextMuted, 1) {
				a.expandedParts[item.Key] = !expanded
				// 收起一级时把二级也清掉：否则下次展开会直接跳到"全部"，
				// 与"默认只显示最后几步"的预期不符。
				if expanded {
					delete(a.expandedFullParts, item.Key)
				}
			}
		})
	}
	if !expanded {
		header()
		return
	}
	// 展开后：**标题行**保持原样，竖线只从标题下方的正文开始。
	//
	// 之前竖线把标题行也包进去（整组都缩进），看起来像"标题本身也是被引用的内容"，
	// 而且标题的图标被挤到线的右边。现在标题不缩进、线与标题图标左对齐，
	// 视觉上是"这条线从标题下面挂下来"。
	header()
	// 线的左缘要落在**标题图标的中线**上：图标 13px 宽，中线在 6.5，
	// 线宽 2 所以左缘 = 6.5 - 1 = 5.5，取 5。
	// 外层不加 padding/gap 的水平偏移，否则线会被推右（之前就是这样偏了 3px）。
	ui.Row(c).Gap(8).Children(func() {
		ui.Box(c).Width(2).MarginX(5).AlignSelf(ui.Stretch).
			Background(theme.Border.Mix(theme.Text, 0.25)).Radius(1)
		ui.Column(c).Grow(1).Gap(6).Children(func() {
			// 只渲染**步骤**：tool_use（一次调用 = 一步）与思考。
			//
			// tool_result 不算一步 —— 它已经折进对应的调用行里显示，
			// 再计一次会让三个数字互相打架：标题说「6 步」（只数调用）、
			// 「上面还有 10 步」和「展开全部 13 步」用的是 part 数，
			// 同一组里出现 6 / 13 两个口径（用户一眼就看出来了）。
			steps := stepRefs(item.Refs)
			total := len(steps)
			// 只显示最后 N 步：用切片下标，避免把前面的 refs 也遍历一遍。
			start := 0
			if !expandedFull && total > processPreviewSteps {
				start = total - processPreviewSteps
			}
			// "上面还有 N 步"：告诉用户这里是被截断的，不是内容就这么点。
			if start > 0 {
				ui.Text(c, fmt.Sprintf("⋯ 上面还有 %d 步", start)).
					FontSize(11).TextColor(theme.TextMuted)
			}
			for _, ref := range steps[start:] {
				a.partView(c, ref.Chat, ref.Index, ref.Chat.Message.Parts[ref.Index], theme.Text)
			}
			// 切换按钮：只有真的被截断（或已全展开）时才出现。
			if total > processPreviewSteps {
				label := fmt.Sprintf("展开全部 %d 步", total)
				if expandedFull {
					label = fmt.Sprintf("收起，只看最后 %d 步", processPreviewSteps)
				}
				if textButtonLines(c, label, 11, theme.Accent, 1) {
					a.expandedFullParts[item.Key] = !expandedFull
				}
			}
		})
	})
}

// execOfPart 从工具结果 data 里取出命令元信息。
//
// 内存里是 *tools.ExecInfo，重放后是 map[string]any（JSON 解出来的）。
func execOfPart(data map[string]any) *tools.ExecInfo {
	switch value := data["exec"].(type) {
	case *tools.ExecInfo:
		return value
	case map[string]any:
		raw, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		var info tools.ExecInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			return nil
		}
		return &info
	}
	return nil
}

// toolArgsDetail 把工具参数摊成一段可读的全文（展开时显示）。
//
// 折叠态那行是截断到 72 字符的摘要，展开后要看完整的 ——
// 长命令被截断时根本认不出跑的是什么。
func toolArgsDetail(name string, data map[string]any) string {
	args, ok := data["args"].(map[string]any)
	if !ok || len(args) == 0 {
		return ""
	}
	// 有些参数是**给工具用的实现细节**，不该出现在"要执行什么"这一段里：
	//   - bash 的 timeoutMs/workdir：元信息里已经有 cwd 了
	//   - edit/write 的 old_string/new_string/content：那就是改动本身，
	//     下面已经用 diff / 全文画出来了，再罗列一遍就是重复
	//     （用户看到的就是"diff 之前多了一堆奇怪的东西"）
	primary := primaryArgKeys(name)
	// edit/write 没有"要执行的命令"可言：文件名已在折叠行显示，
	// 展开区该直接是 diff / 写入内容。所以主参数为空 → 不渲染黑底块。
	// （曾经把 file_path 当主参数，展开后先冒出一行黑底的文件名，
	//  紧接着才是 diff —— 用户看到的就是"后面跟着一行多余的文件名"。）
	if name == "edit" || name == "write" {
		primary = ""
	}
	if primary != "" {
		if value, ok := args[primary]; ok {
			if text, isString := value.(string); isString {
				return text
			}
		}
	}
	// 其余工具：按键名排序，一行一个（跳过上面那类噪音参数）
	skipped := noiseArgKeys(name)
	keys := make([]string, 0, len(args))
	for key := range args {
		if skipped[key] {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("%s: %s", key, formatArgValue(args[key])))
	}
	return strings.Join(lines, "\n")
}

// terminalBg / terminalFg 是"终端块"的底色与文字色。
//
// 深灰底 + 浅灰字，但**字不要纯白**：纯白（#FFF）在深底上对比过强，
// 长时间盯着很刺眼。用 #C9C9C9 —— 足够清楚，又不会"发光"。
// 底色也不用纯黑：#1C1C1C 比 #000 柔和，和界面的浅色底衔接更自然。
const (
	terminalBg = "#1C1C1C"
	terminalFg = "#C9C9C9"
)

// isShellTool 报告这个工具是不是"在终端里跑命令"。
//
// 只有它（bash/pwsh）的详情用黑底白字：命令和输出确实是终端里发生的事。
// 别的工具套同款配色只是"看起来像终端"，反而突兀。
func isShellTool(name string) bool {
	return name == "bash" || name == "pwsh"
}

// renderArgsBlock 渲染参数全文 / 非终端工具的输出。
//
// terminal 为真时用终端配色（深底浅字），否则用普通代码块（浅底深字）。
func (a *App) renderArgsBlock(c *ui.Context, text string, terminal bool, theme *ui.Theme) {
	if !terminal {
		a.renderMonoBlock(c, text, theme.Surface, theme.Text)
		return
	}
	a.renderTerminalBlock(c, text, theme)
}

// renderTerminalBlock 用终端风格渲染一段等宽文本（命令或输出）。
//
// **只给 bash/pwsh 用**：黑底白字是终端的语义，命令和它的输出确实是终端里
// 发生的事。别的工具（edit/write/read）套这个配色只是"看起来像终端"，
// 实际会显得突兀（用户的原话："除了 bash / pwsh 都不应该显示黑底白字"）。
func (a *App) renderTerminalBlock(c *ui.Context, text string, theme *ui.Theme) {
	a.renderMonoBlock(c, text, ui.Hex(terminalBg), ui.Hex(terminalFg))
}

// renderMonoBlock 渲染一段带行、等宽的代码块（背景/前景由调用方给）。
func (a *App) renderMonoBlock(c *ui.Context, text string, background, foreground ui.Color) {
	if strings.TrimSpace(text) == "" {
		return
	}
	ui.Column(c).FillWidth().Padding(8, 10).Radius(6).Gap(3).
		Background(background).Children(func() {
			for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
				ui.Text(c, line).Font(dshCodeFontStack).FontSize(12).
					TextColor(foreground).MaxLines(1)
			}
		})
}

// renderStructuredResult 用结构化数据渲染工具结果（检索/web/read/todo）。
//
// 返回 false 表示"没有结构化数据"，调用方退回渲染原始 Output。
// 只给**有结构化摘要**的工具用：它们 Output 里掺着给模型的指令文本
// （"External web content follows..."、"用 offset=81 继续"），
// 直接摊出来用户看不懂。
func (a *App) renderStructuredResult(c *ui.Context, data map[string]any, theme *ui.Theme) bool {
	if info := searchInfoOfPart(data); info != nil {
		a.renderSearchResult(c, info, theme)
		return true
	}
	if info := webInfoOfPart(data); info != nil {
		a.renderWebResult(c, info, theme)
		return true
	}
	if info := readInfoOfPart(data); info != nil {
		// read 的正文有价值（就是文件内容），所以仍然显示 Output，
		// 但要**去掉末尾那句续读指令** —— 那是给模型的。
		a.renderReadResult(c, data, info, theme)
		return true
	}
	if info := todoInfoOfPart(data); info != nil {
		a.renderTodoResult(c, info, theme)
		return true
	}
	return false
}

// renderSearchResult 按文件分组显示检索结果（grep）或文件列表（glob）。
func (a *App) renderSearchResult(c *ui.Context, info *tools.SearchInfo, theme *ui.Theme) {
	if len(info.Files) == 0 {
		ui.Text(c, "没有匹配").FontSize(11).TextColor(theme.TextMuted)
		return
	}
	ui.Column(c).FillWidth().Padding(6, 8).Radius(6).
		Background(theme.Surface).Gap(4).Children(func() {
			for _, file := range info.Files {
				ui.Column(c).FillWidth().Gap(1).Children(func() {
					// 文件路径 + 该文件命中数
					ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
						ui.Text(c, file.Path).Font(dshCodeFontStack).FontSize(11).
							TextColor(theme.Text).MaxLines(1)
						if len(file.Hits) > 1 {
							ui.Text(c, fmt.Sprintf("%d 处", len(file.Hits))).
								FontSize(10).TextColor(theme.TextMuted)
						}
					})
					// 命中行：行号 + 内容
					for _, hit := range file.Hits {
						text := fmt.Sprintf("%4d  %s", hit.Line,
							truncateRunes(strings.TrimSpace(hit.Text), 100))
						ui.Text(c, text).Font(dshCodeFontStack).FontSize(11).
							TextColor(theme.TextMuted).MaxLines(1)
					}
				})
			}
			if info.Truncated {
				ui.Text(c, "… 已达上限，结果被截断").
					FontSize(10).TextColor(theme.TextMuted)
			}
		})
}

// renderWebResult 显示抓取摘要或搜索结果列表。
func (a *App) renderWebResult(c *ui.Context, info *tools.WebInfo, theme *ui.Theme) {
	if info.Kind == "fetch" {
		// fetch 的正文就是抓到的网页内容，有价值 —— 由调用方继续显示 Output。
		// 这里只补一行"抓的哪个地址、什么状态"。
		return
	}
	ui.Column(c).FillWidth().Padding(6, 8).Radius(6).
		Background(theme.Surface).Gap(4).Children(func() {
			for index, result := range info.Results {
				ui.Column(c).FillWidth().Gap(1).Children(func() {
					ui.Text(c, fmt.Sprintf("%d. %s", index+1, result.Title)).
						FontSize(12).TextColor(theme.Text).MaxLines(1)
					if result.URL != "" {
						ui.Text(c, result.URL).Font(dshCodeFontStack).FontSize(10).
							TextColor(theme.TextMuted).MaxLines(1)
					}
					if result.Excerpt != "" {
						ui.Text(c, truncateRunes(result.Excerpt, 120)).FontSize(11).
							TextColor(theme.TextMuted).MaxLines(2)
					}
				})
			}
			if info.Truncated {
				ui.Text(c, "… 只显示了前面的结果").
					FontSize(10).TextColor(theme.TextMuted)
			}
		})
}

// renderReadResult 显示文件内容（保留行号），去掉末尾的续读指令。
func (a *App) renderReadResult(c *ui.Context, data map[string]any, info *tools.ReadInfo, theme *ui.Theme) {
	output := stringOf(data, "output")
	// 去掉末尾那句 "…（共 300 行，已返回第 1-80 行；用 offset=81 继续）"：
	// 那是让模型续读的指令，界面用折叠行的"第 1-80 行 / 共 300 行"表达。
	if index := strings.LastIndex(output, "\n…（共 "); index >= 0 {
		output = output[:index]
	}
	if strings.TrimSpace(output) != "" {
		a.renderMonoBlock(c, strings.TrimRight(output, "\n"), theme.Surface, theme.Text)
	}
}

// renderTodoResult 显示任务进度（清单本身在 todo 面板里）。
func (a *App) renderTodoResult(c *ui.Context, info *tools.TodoInfo, theme *ui.Theme) {
	if info.Total == 0 {
		ui.Text(c, "任务列表已清空").FontSize(11).TextColor(theme.TextMuted)
		return
	}
	ui.Text(c, fmt.Sprintf("任务列表：%d/%d 完成", info.Completed, info.Total)).
		FontSize(11).TextColor(theme.TextMuted)
}

// toolSummaryLine 生成工具结果的一行结构化摘要（折叠行显示）。
//
// 优先级：图片 > 检索 > web > read > todo。
// 一般只有一种（一个工具只产一类结果），排序只是为了行为确定。
func toolSummaryLine(data map[string]any) string {
	if info := imageInfoOfPart(data); info != nil {
		return imageInfoSummary(info)
	}
	if info := searchInfoOfPart(data); info != nil {
		return searchInfoSummary(info)
	}
	if info := webInfoOfPart(data); info != nil {
		return webInfoSummary(info)
	}
	if info := readInfoOfPart(data); info != nil {
		return readInfoSummary(info)
	}
	if info := todoInfoOfPart(data); info != nil {
		return todoInfoSummary(info)
	}
	return ""
}

// searchInfoSummary："3 个文件 · 12 处命中"（glob 只有文件数）。
func searchInfoSummary(info *tools.SearchInfo) string {
	if info.FileCount == 0 && info.HitCount == 0 {
		return ""
	}
	// glob：每个文件一条命中，说"处命中"没意义
	if info.HitCount == info.FileCount {
		text := fmt.Sprintf("%d 个文件", info.FileCount)
		if info.Truncated {
			text += "（已达上限）"
		}
		return text
	}
	text := fmt.Sprintf("%d 个文件 · %d 处命中", info.FileCount, info.HitCount)
	if info.Truncated {
		text += "（已达上限）"
	}
	return text
}

// webInfoSummary："HTTP 200 · 12KB" 或 "5 条结果"。
func webInfoSummary(info *tools.WebInfo) string {
	if info.Kind == "search" {
		text := fmt.Sprintf("%d 条结果", len(info.Results))
		if info.Truncated {
			text += "（已截断）"
		}
		return text
	}
	var parts []string
	if info.Status > 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", info.Status))
	}
	if info.Bytes > 0 {
		parts = append(parts, formatBytes(info.Bytes))
	}
	if info.Truncated {
		parts = append(parts, "已截断")
	}
	return strings.Join(parts, " · ")
}

// readInfoSummary："第 1-80 行 / 共 300 行"。
func readInfoSummary(info *tools.ReadInfo) string {
	if info.TotalLines == 0 {
		return ""
	}
	if info.EndLine == 0 {
		return fmt.Sprintf("共 %d 行", info.TotalLines)
	}
	if info.StartLine == info.EndLine {
		return fmt.Sprintf("第 %d 行 / 共 %d 行", info.StartLine, info.TotalLines)
	}
	return fmt.Sprintf("第 %d-%d 行 / 共 %d 行",
		info.StartLine, info.EndLine, info.TotalLines)
}

// todoInfoSummary："3/5 完成"。
func todoInfoSummary(info *tools.TodoInfo) string {
	if info.Total == 0 {
		return "已清空"
	}
	return fmt.Sprintf("%d/%d 完成", info.Completed, info.Total)
}

// decodePartExtra 把 part 里的结构化字段还原成具体类型。
//
// 内存里是 *T 指针，重放后是 map[string]any（JSON 解出来的）。
// 与 driver 侧的 decodeExtra 同一思路，避免每个字段复制一遍。
func decodePartExtra[T any](data map[string]any, key string) *T {
	switch value := data[key].(type) {
	case *T:
		return value
	case map[string]any:
		raw, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		var out T
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil
		}
		return &out
	}
	return nil
}

func searchInfoOfPart(data map[string]any) *tools.SearchInfo {
	return decodePartExtra[tools.SearchInfo](data, "search")
}

func webInfoOfPart(data map[string]any) *tools.WebInfo {
	return decodePartExtra[tools.WebInfo](data, "web")
}

func readInfoOfPart(data map[string]any) *tools.ReadInfo {
	return decodePartExtra[tools.ReadInfo](data, "read")
}

func todoInfoOfPart(data map[string]any) *tools.TodoInfo {
	return decodePartExtra[tools.TodoInfo](data, "todo")
}

// imageInfoOfPart 从工具结果 data 里取出图片摘要。
func imageInfoOfPart(data map[string]any) *tools.ImageInfo {
	switch value := data["image_info"].(type) {
	case *tools.ImageInfo:
		return value
	case map[string]any:
		raw, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		var info tools.ImageInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			return nil
		}
		return &info
	}
	return nil
}

// imageInfoSummary 生成图片的一行摘要："1920x1080 · 240KB · image/png"。
//
// 路径不重复（折叠行前面已经有文件名摘要了），description 也不重复。
func imageInfoSummary(info *tools.ImageInfo) string {
	var parts []string
	if info.Width > 0 && info.Height > 0 {
		parts = append(parts, fmt.Sprintf("%dx%d", info.Width, info.Height))
	}
	if info.Bytes > 0 {
		parts = append(parts, formatBytes(info.Bytes))
	}
	if info.MediaType != "" {
		parts = append(parts, strings.TrimPrefix(info.MediaType, "image/"))
	}
	if info.Downscaled && info.OrigWidth > 0 {
		parts = append(parts, fmt.Sprintf("原图 %dx%d", info.OrigWidth, info.OrigHeight))
	}
	return strings.Join(parts, " · ")
}

// formatBytes 把字节数格式化成 "240KB" / "1.2MB"。
func formatBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.0fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	}
}

// argString 从工具调用的 data 里读一个**参数**的字符串值。
//
// 参数都在 data["args"] 里（不是顶层）：容易写成 stringOf(data, key)
// 而静默拿到空串 —— description 就这么丢过一次。
func argString(data map[string]any, key string) string {
	args, ok := data["args"].(map[string]any)
	if !ok {
		return ""
	}
	value, _ := args[key].(string)
	return value
}

// execArgsSummary 生成 bash/pwsh 调用行尾的补充信息（workdir / 超时）。
//
// 只显示**用户显式设过**的项：默认 workdir 就是会话目录、默认超时不用提，
// 全都显示反而让这一行变长。
func execArgsSummary(data map[string]any) string {
	args, ok := data["args"].(map[string]any)
	if !ok {
		return ""
	}
	var parts []string
	if workdir, ok := args["workdir"].(string); ok && workdir != "" {
		parts = append(parts, workdir)
	}
	// timeoutMs 可能是 float64（JSON 解出来）也可能是 int
	if value, ok := args["timeoutMs"]; ok {
		if ms := intOf(value); ms > 0 {
			parts = append(parts, formatElapsed(int64(ms)))
		}
	}
	return strings.Join(parts, " · ")
}

// intOf 尽力把 JSON 数值转成 int（float64/int/json.Number/字符串都认）。
func intOf(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	case json.Number:
		if n, err := typed.Int64(); err == nil {
			return int(n)
		}
	}
	return 0
}

// primaryArgKeys 是每种工具"要执行什么"的那个主参数。
//
// 有它就直接显示它的值，不加 "key: " 前缀 —— 一整行都是命令，
// 加前缀反而挤（用户明确要求 bash/pwsh 不要 command: 前缀）。
func primaryArgKeys(name string) string {
	switch name {
	case "bash", "pwsh":
		return "command"
	case "read", "read_image":
		return "file_path"
	case "edit", "write":
		return "file_path"
	case "grep":
		return "pattern"
	case "glob":
		return "pattern"
	case "web_fetch":
		return "url"
	case "web_search":
		return "query"
	default:
		return ""
	}
}

// noiseArgKeys 是**不展示**的参数（它们要么已在别处显示，要么就是改动本身）。
func noiseArgKeys(name string) map[string]bool {
	switch name {
	case "bash", "pwsh":
		// 只跳过 command：它作为主参数显示在正文里（不带前缀）。
		// workdir / timeoutMs **要保留显示** —— 用户需要知道这条命令
		// 在哪个目录、限时多久跑的。
		return map[string]bool{"command": true}
	case "edit":
		// old/new 就是改动，diff 里已经画了
		return map[string]bool{"file_path": true, "old_string": true, "new_string": true}
	case "write":
		// content 就是写入内容，下面整篇显示
		return map[string]bool{"file_path": true, "content": true}
	default:
		return nil
	}
}

// formatArgValue 把参数值转成紧凑的可读文本。
func formatArgValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case nil:
		return "null"
	default:
		if raw, err := json.Marshal(typed); err == nil {
			return string(raw)
		}
		return fmt.Sprintf("%v", typed)
	}
}

// renderExecMeta 显示命令执行的元信息（exit_code / 耗时 / 目录）。
//
// **单独一行、和输出分开**：原来这些是拼在输出第一行的
// （"exit_code=0 elapsed=123ms cwd=..."），用户得先跳过它才能看正文。
// 现在元信息在输出上方自成一行，正文从第一行就是真正的输出。
func (a *App) renderExecMeta(c *ui.Context, exec *tools.ExecInfo, theme *ui.Theme) {
	// 元信息：一行小字，**下方一条浅分隔线**把它和输出隔开。
	//
	// 试过给它单独加底色：和下面输出的底色一样，反而看不出是两块
	// （截图里两段糊成一片）。用分隔线更省空间也更清楚。
	ui.Column(c).FillWidth().Children(func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Padding(0, 2, 3, 2).Children(func() {
			// 退出码：0 用中性色，非 0 用红色（一眼看出失败）
			codeColor := theme.TextMuted
			codeText := fmt.Sprintf("exit %d", exec.ExitCode)
			if exec.TimedOut {
				codeText = "超时"
				codeColor = theme.Danger
			} else if exec.ExitCode != 0 {
				codeColor = theme.Danger
			}
			ui.Text(c, codeText).Font(dshCodeFontStack).FontSize(11).
				TextColor(codeColor)
			if exec.ElapsedMS > 0 {
				ui.Text(c, formatElapsed(exec.ElapsedMS)).
					Font(dshCodeFontStack).FontSize(11).TextColor(theme.TextMuted)
			}
			if exec.Cwd != "" {
				ui.Text(c, exec.Cwd).Font(dshCodeFontStack).FontSize(11).
					TextColor(theme.TextMuted).MaxLines(1)
			}
		})
		ui.Box(c).Height(1).FillWidth().Background(theme.Border)
	})
}

// formatElapsed 把毫秒格式化成人读的时长。
func formatElapsed(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.2fs", float64(ms)/1000)
	default:
		return fmt.Sprintf("%dm%02ds", ms/60000, (ms%60000)/1000)
	}
}

// renderWrittenContent 显示 write 写入的内容（带行号）。
//
// 用行号而不是 diff 的红绿：write 是整篇写入，没有"改动"可言，
// 用户想看的是"文件里现在是什么"。
func (a *App) renderWrittenContent(c *ui.Context, diff *tools.FileDiff, theme *ui.Theme) {
	// **不再画"已创建 xxx.txt"那种标题行**：文件名和行数已经在折叠行里
	// （`write notes.txt 2 行`），这里再来一行是重复。
	//
	// 用**普通代码块**（浅色底）而不是终端黑底：黑底是 bash/pwsh 的语义，
	// write 只是写文件，套黑底会显得像在终端里敲了什么。
	lines := strings.Split(diff.Content, "\n")
	// 末尾换行会多出一个空元素，不算一行
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	numbered := make([]string, 0, len(lines))
	for index, line := range lines {
		numbered = append(numbered, formatDiffLineNo(index+1)+" "+line)
	}
	a.renderMonoBlock(c, strings.Join(numbered, "\n"), theme.Surface, theme.Text)
}

// stripExecHeader 去掉命令输出开头那行 "exit_code=... elapsed=... cwd=..."。
//
// 那行是 bash 工具写给**模型**的（模型拿不到结构化字段，需要知道成败），
// 界面已经有了 renderExecMeta 画的同一份信息，再显示一遍就是重复。
//
// 只在确认存在该行时才去掉：手写的输出、别的工具的输出不该被动。
func stripExecHeader(output string) string {
	line, rest, found := strings.Cut(output, "\n")
	if !found {
		// 只有一行：整行都是 header 时返回空（没有正文可显示）
		if strings.HasPrefix(line, "exit_code=") {
			return ""
		}
		return output
	}
	if !strings.HasPrefix(line, "exit_code=") {
		return output
	}
	return rest
}

// diffSummaryForWrite 生成 write 在折叠行的摘要（"12 行"）。
func diffSummaryForWrite(diff *tools.FileDiff) string {
	lines := strings.Split(diff.Content, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return fmt.Sprintf("%d 行", len(lines))
}

// hasMatchingToolUse 报告某个 call id 是否有配对的 tool_use part。
//
// 用来区分"正常结果（已折进调用行）"与"孤儿结果（没找到调用，需要单独显示）"。
func (a *App) hasMatchingToolUse(chat *conversation.ChatEntry, callID string) bool {
	for _, part := range chat.Message.Parts {
		if part.MediaType != render.MediaToolUse {
			continue
		}
		if data, ok := partData(part); ok && stringOf(data, "id") == callID {
			return true
		}
	}
	return false
}

// renderOrphanToolResult 显示找不到配对调用的工具结果。
//
// 什么时候会出现：旧日志只记了结果、异常中断导致调用缺失。
// 直接吞掉等于丢数据，所以仍然画出来，只是没有调用信息可合并。
func (a *App) renderOrphanToolResult(c *ui.Context, data map[string]any, theme *ui.Theme) {
	isError, _ := data["is_error"].(bool)
	statusText := a.tr("chat.toolResult")
	color := theme.TextMuted
	if isError {
		statusText = a.tr("chat.toolFailed")
		color = theme.Danger
	}
	ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
		ui.Icon(c, toolIcon(stringOf(data, "name"))).Size(13, 13).TextColor(theme.TextMuted)
		ui.Text(c, stringOf(data, "name")).FontSize(11).
			Padding(1, 5).Radius(4).
			Background(theme.SurfaceHover).TextColor(theme.Text)
		ui.Spacer(c)
		ui.Text(c, statusText).FontSize(11).TextColor(color)
	})
	if output := stringOf(data, "output"); output != "" {
		ui.Column(c).FillWidth().Padding(2, 20).Children(func() {
			a.toolOutput(c, data, isError, false, color)
		})
	}
}

// diffOfPart 从工具结果的 data 里取出结构化差异。
//
// 内存里是 *tools.FileDiff；重放后是 map[string]any（JSON 解出来的）。
func diffOfPart(data map[string]any) *tools.FileDiff {
	switch value := data["diff"].(type) {
	case *tools.FileDiff:
		return value
	case map[string]any:
		raw, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		var diff tools.FileDiff
		if err := json.Unmarshal(raw, &diff); err != nil {
			return nil
		}
		return &diff
	}
	return nil
}

// toolResultOf 找到与某次工具调用配对的 result part 的 data。
//
// 返回的 bool 表示"已经有结果了"（可能成功也可能失败）—— 用它替代
// 原来"只看 status 字符串"的判断，因为要把结果**折叠进 tool call 行**，
// 需要拿到 result 的内容（output / diff）。
func (a *App) toolResultOf(chat *conversation.ChatEntry, callID string) (map[string]any, bool) {
	for _, part := range chat.Message.Parts {
		if part.MediaType != render.MediaToolResult {
			continue
		}
		data, ok := partData(part)
		if !ok || stringOf(data, "id") != callID {
			continue
		}
		return data, true
	}
	return nil, false
}

// toolCallStatusFor 判断某次工具调用是否已经有结果。
func (a *App) toolCallStatusFor(chat *conversation.ChatEntry, callID string) string {
	for _, part := range chat.Message.Parts {
		if part.MediaType != render.MediaToolResult {
			continue
		}
		data, ok := partData(part)
		if !ok || stringOf(data, "id") != callID {
			continue
		}
		if failed, _ := data["is_error"].(bool); failed {
			return a.tr("common.failed")
		}
		return a.tr("chat.toolDone")
	}
	return ""
}

// partView 按 media type 渲染一个 part。
func (a *App) partView(c *ui.Context, chat *conversation.ChatEntry, partIndex int, part message.Part, color ui.Color) {
	theme := c.Theme()
	messageID := chat.Message.MessageID
	streaming := chat.Status().IsStreaming()
	switch part.MediaType {
	case render.MediaToolUse:
		data, ok := partData(part)
		if !ok {
			return
		}
		// 一行式：图标 + 工具名（胶囊）+ 参数摘要 + 结果状态
		//
		// **结果折进这一行**：原来 tool_use 和 tool_result 是两行 ——
		// 一行"调用了什么"、一行"工具结果 · read（展开）"，
		// 读起来割裂，而且结果那行的按钮占了半行。现在一行看完：
		// 调用 + 成败，点这行展开详情（edit 显示 diff，其它显示输出全文）。
		id := stringOf(data, "id")
		resultData, hasResult := a.toolResultOf(chat, id)
		status := a.toolCallStatus(chat, id, streaming)
		isError := false
		if hasResult {
			isError, _ = resultData["is_error"].(bool)
		}
		name := stringOf(data, "name")
		key := expansionKey(messageID, partIndex)
		expanded := a.expandedParts[key]
		// 参数全文：展开时看完整命令/路径（上面那行是截断摘要）
		argsText := toolArgsDetail(name, data)

		arrow := "▸"
		if expanded {
			arrow = "▾"
		}
		row := ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Radius(4).
			Padding(1, 2)
		if row.Hovered() {
			row.Background(theme.SurfaceHover)
		}
		var toggle bool
		row.Children(func() {
			// 展开三角**任何时候都在**：不只是有结果才能展开 ——
			// 命令还在跑（甚至刚发起还没结果）时，用户就想先看看要执行什么。
			if ui.Text(c, arrow).FontSize(10).TextColor(theme.TextMuted).
				Cursor(ui.CursorPointer).Clicked() {
				toggle = true
			}
			ui.Icon(c, toolIcon(name)).Size(13, 13).TextColor(theme.TextMuted)
			ui.Text(c, name).FontSize(11).
				Padding(1, 5).Radius(4).
				Background(theme.SurfaceHover).TextColor(theme.Text)
			// description 优先显示：模型写的描述通常比命令好懂。
			//
			// **description 在 args 里面**（bash 的一个参数），不在 data 顶层。
			// 描述和命令摘要**都显示**：描述说明意图，命令说明具体动作，
			// 少哪个都不完整。但有描述时命令摘要让位 —— 截短到 36 字符，
			// 把宽度留给描述（完整命令展开可见）。
			desc := argString(data, "description")
			if desc != "" {
				// 描述用正文色（重），命令摘要用弱化色（轻）：两者语义不同
				// ——"要做什么"是意图，"跑什么命令"是细节，同色会读成一整句。
				ui.Text(c, truncateRunes(desc, 48)).FontSize(12).
					TextColor(theme.Text).MaxLines(1)
			}
			if summary := toolArgSummary(data); summary != "" {
				// 摘要（文件名/命令）不 Grow：让后面的 +/- 紧跟在它后面，
				// 而不是被推到行尾（用户要求"edit 的 +/- 紧跟文件名后面"）。
				limit := 56
				if desc != "" {
					limit = 36
				}
				ui.Text(c, truncateRunes(summary, limit)).FontSize(12).
					TextColor(theme.TextMuted).MaxLines(1)
			}
			// 各工具的结构化摘要（折叠行一行）。
			//
			// 这些都是"Output 里掺着给模型的文本，界面不该照搬"的同一类问题：
			//   read_image → XML 信封
			//   grep/glob  → 纯文本命中列表（看不出总数、文件分组）
			//   web_*      → 夹着 "External web content follows..." 之类的指令
			//   read       → 末尾挂着"用 offset=81 继续"
			//   todo_write → 整个清单（界面上已有 todo 面板）
			if hasResult {
				if line := toolSummaryLine(resultData); line != "" {
					ui.Text(c, line).FontSize(11).
						TextColor(theme.TextMuted).MaxLines(1)
				}
			}
			// edit/write 的增删数量：**紧跟在文件名后面**，折叠态就能看出改了多少。
			//
			// 为什么不放行尾：行尾已经被状态（完成/失败）占了，
			// 而 "+3 -2" 是在说"这个文件"，贴着文件名才有归属感。
			if hasResult && !isError {
				if diff := diffOfPart(resultData); diff != nil {
					if diff.Content != "" || diff.Verb != "" {
						// write：显示行数更有意义
						ui.Text(c, diffSummaryForWrite(diff)).FontSize(11).
							TextColor(theme.TextMuted)
					} else if diff.Added > 0 || diff.Removed > 0 {
						ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
							if diff.Added > 0 {
								ui.Text(c, fmt.Sprintf("+%d", diff.Added)).
									FontSize(11).TextColor(diffAddText(theme))
							}
							if diff.Removed > 0 {
								ui.Text(c, fmt.Sprintf("-%d", diff.Removed)).
									FontSize(11).TextColor(diffDelText(theme))
							}
						})
					}
				}
			}
			ui.Spacer(c)
			// bash/pwsh 的 workdir 与 timeout：**显示在这一行**。
			//
			// 它们是"这条命令怎么跑的"的一部分，和命令本身一起看才顺；
			// 放进展开区会离得太远（用户明确要求放这一行）。
			// 只在设了的时候显示 —— 默认值时是噪音。
			if meta := execArgsSummary(data); meta != "" {
				ui.Text(c, meta).FontSize(11).TextColor(theme.TextMuted).
					MaxLines(1)
			}
			// 状态：失败用红色，运行中用强调色，完成用弱化色
			if status != "" {
				color := theme.TextMuted
				switch {
				case isError:
					color = theme.Danger
				case status == a.tr("chat.toolRunning"):
					color = theme.Accent
				}
				ui.Text(c, status).FontSize(11).TextColor(color)
			}
			// 点行内空白也能展开（但别和上面的三角重复触发）
			if row.Clicked() {
				toggle = true
			}
		})
		if toggle {
			a.expandedParts[key] = !expanded
			expanded = !expanded
		}
		// 展开：先看**要执行什么/改了什么**，再看结果。
		//
		// 顺序有意如此：命令 > 元信息 > 输出。用户点开一个工具调用，
		// 最常想知道的是"它到底跑了什么"；结果反而次之。
		if expanded {
			// Gap 8：命令块、元信息、输出块之间要留出**明显**的间隔。
			// 两块黑底贴太近会连成一片，看不出"命令"和"输出"是两段
			// （截图里它们几乎粘在一起）。
			ui.Column(c).FillWidth().Padding(2, 20).Gap(8).Children(func() {
				// **黑底白字只给 bash/pwsh**：那是终端的语义。
				// 别的工具（read/read_image/ask…）的"参数全文"和"输出"
				// 套终端配色只会显得突兀 —— 它们本来就不是在终端里发生的。
				terminal := isShellTool(name)

				// 1) 参数全文（bash 的命令、read 的路径…）
				//
				// **只给 shell 用**：别的工具的参数已经浓缩进折叠行摘要了
				// （grep 的 pattern、read 的路径、edit 的文件名），
				// 再来一块就是重复（截图里 grep 展开后先冒出一行 pattern）。
				if argsText != "" && terminal {
					a.renderArgsBlock(c, argsText, terminal, theme)
				}
				if !hasResult {
					return
				}
				// 2) 执行元信息（exit_code / 耗时 / 目录），与输出分开
				if exec := execOfPart(resultData); exec != nil {
					a.renderExecMeta(c, exec, theme)
				}
				// 3) 结果正文：edit 画 diff，write 显示写入内容，其它显示输出
				if diff := diffOfPart(resultData); diff != nil {
					if diff.Content != "" || diff.Verb != "" {
						a.renderWrittenContent(c, diff, theme)
					} else {
						a.renderDiff(c, diff, theme)
					}
				} else if images := toolImagesOf(resultData); len(images) > 0 {
					// 带图片的结果（read_image / display_image）：走 toolOutput，
					// 它会画缩略图。**不能**用终端块 —— 那只会把 Output 里的
					// XML 信封画成黑底，图片本体就丢了。
					a.toolOutput(c, resultData, isError, true, color)
				} else if a.renderStructuredResult(c, resultData, theme) {
					// 检索/web/read/todo 有结构化数据：按结构渲染，
					// 而不是把 Output 里掺着指令的文本摊出来。
				} else if output := stringOf(resultData, "output"); output != "" {
					// 有结构化元信息时，把 Output 开头那行 "exit_code=... cwd=..."
					// 去掉 —— 它和上面刚画的元信息是同一件事，留着就是重复。
					// 那行是给**模型**看的（模型拿不到 Exec 字段），界面不需要。
					if execOfPart(resultData) != nil {
						output = stripExecHeader(output)
					}
					if strings.TrimSpace(output) != "" {
						a.renderArgsBlock(c, strings.TrimRight(output, "\n"), terminal, theme)
					}
				}
			})
		}
	case render.MediaToolResult:
		// 结果已经折进对应的 tool_use 行，这里不再单独渲染。
		//
		// 但**不能什么都不做**：配对信息缺失时（旧日志、异常中断）结果会
		// 变成孤儿，直接吞掉等于丢数据。所以只在"找不到配对调用"时才画。
		data, ok := partData(part)
		if !ok {
			return
		}
		if a.hasMatchingToolUse(chat, stringOf(data, "id")) {
			return
		}
		a.renderOrphanToolResult(c, data, theme)
		return
	case render.MediaSummary:
		text, _ := part.AsText()
		label := a.tr("chat.summary")
		if a.Current() != nil && a.Current().summaryExpanded {
			label = a.tr("chat.summary.expanded")
		}
		ui.Row(c).FillWidth().Gap(8).Children(func() {
			ui.Box(c).Height(1).Grow(1).Background(theme.Border)
			if textButton(c, label, 11, theme.TextMuted) {
				if session := a.Current(); session != nil {
					session.summaryExpanded = !session.summaryExpanded
				}
			}
			ui.Box(c).Height(1).Grow(1).Background(theme.Border)
		})
		if a.Current() != nil && a.Current().summaryExpanded && text != "" {
			ui.Text(c, text).FontSize(12).TextColor(theme.TextMuted)
		}

	case render.MediaReasoning:
		text, ok := part.AsText()
		if !ok || strings.TrimSpace(text) == "" {
			return
		}
		key := expansionKey(messageID, partIndex)
		expanded := a.expandedParts[key]
		// 还在思考（流式且后面没有正文）：显示a.tr("chat.thinking.running") + **最后一行**，
		// 让用户看得到正在想什么；结束后退回首行摘要（harness 的做法）。
		running := streaming && !hasTextAfter(chat.Message.Parts, partIndex)
		label := a.tr("chat.thinking")
		if running {
			label = a.tr("chat.thinking.running")
		}
		// 只显示**最后一行**：思考是连续输出的，最后一行才是"现在在想什么"；
		// 过长时截断**前面**（保留最近的内容），整行不换行。
		preview := lastLine(text, 48)
		if preview != "" {
			label += " · " + preview
		}
		if expanded {
			label = strings.SplitN(label, " · ", 2)[0] + a.tr("chat.collapse")
		}
		// 图标在标题前（dsh 的 PROCESS_ICONS：thinking → IconThinkOutlineRegular）。
		// 之前 Thinking 单独渲染时没有图标，而过程组标题有——同一类内容两种外观。
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, iconThinkSVG).Size(13, 13).TextColor(theme.TextMuted)
			if textButtonLines(c, label, 12, theme.TextMuted, 1) {
				a.expandedParts[key] = !expanded
			}
		})
		if expanded {
			ui.Text(c, strings.TrimSpace(text)).FontSize(12).
				TextColor(theme.TextMuted).LineHeight(1.6)
		}
	default:
		// 图片/文件 part 交给 render 包的分类结果（它已经处理了 MIME 规范化与魔数嗅探）
		view := render.Classify(part)
		switch view.Kind {
		case render.KindImage:
			if view.Image != nil {
				if view.Image.Blob != "" {
					// 内容寻址存储里的本地图片（不是远程 URL）
					a.renderBlobImage(c, view.Name, view.Image.Blob, view.Image.MediaType)
					return
				}
				a.renderImagePart(c, view.Name, view.Image.URL, view.Image.Bytes, view.Image.MediaType)
				return
			}
		case render.KindFile:
			if view.File != nil {
				name := view.Name
				if name == "" {
					name = view.File.URL
				}
				ui.Text(c, a.tr("chat.file", name)).FontSize(12).TextColor(theme.TextMuted)
				return
			}
		}
		if text, ok := part.AsText(); ok && text != "" {
			// 正文走 markdown 渲染（带 LRU 缓存：流式时只重算这一条）
			a.renderMarkdown(c, messageID, partIndex, text)
		}
	}
}

// toolOutput 渲染工具输出：默认只显示前几行。
//
// 工具输出上限 100KB，整段塞进列表行会让渲染和滚动都很重；默认截断，
// 需要时点「展开」看全文。
func (a *App) toolOutput(c *ui.Context, data map[string]any, isError, expanded bool, color ui.Color) {
	theme := c.Theme()
	output := stringOf(data, "output")
	images := toolImagesOf(data)
	if output == "" && len(images) == 0 {
		return
	}
	ui.Column(c).FillWidth().Padding(6, 8).Radius(6).Background(theme.Surface).Gap(6).Children(func() {
		// 有图片时**不显示 Output 文本**：read_image 的 output 是
		// <path>/<type>/<content> 这种给模型的 XML 信封，人看着是一堆标签。
		// 图片本身就是全部信息（尺寸/大小已经在折叠行那行摘要里）。
		if output != "" && len(images) == 0 {
			shown, _ := truncateOutput(output, expanded)
			// 失败且折叠时：只留一行要点。
			//
			// 原始错误常常是"绝对路径 + Windows errno 长句"占三行，
			// 折叠态扫一眼只需要知道"失败了、为什么"，细节展开再看。
			if isError && !expanded {
				shown = simplifyToolError(output)
			}
			ui.Text(c, shown).Font(dshCodeFontStack).FontSize(12).
				TextColor(pick(isError, theme.Danger, theme.TextMuted))
		}
		// read_image 的图片：展开时缩略显示，折叠时只留上面那行摘要（不刷屏）
		if expanded {
			for _, image := range images {
				if image.Ref == "" {
					continue
				}
				hash := strings.TrimPrefix(image.Ref, "blob:sha256:")
				a.renderBlobImage(c, "", hash, image.MediaType)
			}
		}
	})
}

// toolImagesOf 读出工具结果里的图片引用。
func toolImagesOf(data map[string]any) []tools.ToolImageRef {
	list, _ := data["images"].([]any)
	out := make([]tools.ToolImageRef, 0, len(list))
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, tools.ToolImageRef{
			Ref:       stringOf(obj, "ref"),
			MediaType: stringOf(obj, "media_type"),
		})
	}
	return out
}

// truncateRunes 把字符串截到 n 个字符（按 rune 算，中文不会被切一半）。
//
// 超长时末尾加省略号：调用方用它把"参数摘要"限制在一行内，
// 免得一条长命令把行内其它信息（状态）挤出去。
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}

// truncateOutput 按行截断工具输出。
// simplifyToolError 把工具失败的长错误压成一行能读的要点。
//
// 真实见到的：读一个不存在的文件，输出是
//
//	读取失败：internal/host/local.go: open C:\Users\rex\AppData\Local\Temp\
//	TestShotToolDisplay3531176701\001\internal\host\local.go: The system
//	cannot find the path specified.
//
// 三行里只有"文件不存在"有用，其余是临时目录的绝对路径和 Windows 的
// errno 长句。折叠态只留第一行 + 关键原因；展开才看完整的（展开态直接
// 返回原文，不做简化）。
func simplifyToolError(output string) string {
	first := output
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(first)
	// 去掉 Windows 的长尾 errno 描述：它总是这两句之一
	for _, tail := range []string{
		": The system cannot find the path specified.",
		": The system cannot find the file specified.",
		": No such file or directory",
	} {
		if strings.HasSuffix(first, tail) {
			first = strings.TrimSuffix(first, tail) + "（不存在）"
			break
		}
	}
	// 绝对路径只留末尾两级：完整路径在展开态里能看到，折叠态没必要占宽度
	first = shortenPaths(first)
	return truncateRunes(first, 96)
}

// shortenPaths 把文本里的 Windows/Unix 绝对路径压成"…/末两级"。
//
// 只处理以盘符或 / 开头的片段，普通单词不动 —— 宁可少压也不要误伤
// （比如把 "grep func (l *Local)" 里的东西当成路径）。
func shortenPaths(s string) string {
	fields := strings.Fields(s)
	for i, field := range fields {
		trimmed := strings.TrimRight(field, "：:，,。.;;")
		if !looksLikeAbsolutePath(trimmed) {
			continue
		}
		parts := strings.FieldsFunc(trimmed, func(r rune) bool { return r == '\\' || r == '/' })
		if len(parts) <= 2 {
			continue
		}
		sep := string(filepathSeparatorFor(trimmed))
		short := "…" + sep + strings.Join(parts[len(parts)-2:], sep)
		fields[i] = strings.Replace(field, trimmed, short, 1)
	}
	return strings.Join(fields, " ")
}

// looksLikeAbsolutePath 判断一段文本是不是绝对路径。
func looksLikeAbsolutePath(s string) bool {
	if len(s) < 4 {
		return false
	}
	// Windows：C:\ 或 C:/
	if len(s) >= 3 && s[1] == ':' && (s[2] == '\\' || s[2] == '/') {
		return true
	}
	// Unix：以 / 开头且有至少两级
	return s[0] == '/' && strings.Count(s, "/") >= 2
}

// filepathSeparatorFor 返回该路径惯用的分隔符。
func filepathSeparatorFor(path string) rune {
	if strings.ContainsRune(path, '\\') {
		return '\\'
	}
	return '/'
}

func truncateOutput(output string, expanded bool) (string, bool) {
	const maxLines = 1
	const maxRunes = 200
	if expanded {
		return output, true
	}
	lines := strings.Split(output, "\n")
	truncated := false
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	text := strings.Join(lines, "\n")
	if runes := []rune(text); len(runes) > maxRunes {
		text = string(runes[:maxRunes])
		truncated = true
	}
	if truncated {
		text += "…"
	}
	return text, truncated
}

// askEntry 是摊平后的一个"待回答的问题"（tab 的一页）。
type askEntry struct {
	group *driver.AskGroup
	index int
	spec  tools.QuestionSpec
}

// askPanel 渲染提问面板：本轮**所有**调用的所有问题统一提交一次。
//
// 多个问题时用 tab 切换（一次只看一题，不竖着堆一屏）；选项文字左对齐，
// 整行都是点击目标。单选互斥、多选叠加；输入框里的文字作为 custom 答案
// （只给第一个未选项的问题），全部已选时文字进 note。
func (a *App) askPanel(c *ui.Context) {
	session := a.Current()
	if session == nil {
		return
	}
	groups := session.AskGroups()
	if len(groups) == 0 {
		return
	}
	theme := c.Theme()

	// 把 (组, 题) 摊平：tab 顺序 = 模型提问的顺序
	entries := make([]askEntry, 0, len(groups))
	for _, group := range groups {
		for index, question := range group.Questions {
			entries = append(entries, askEntry{group: group, index: index, spec: question})
		}
	}
	if len(entries) == 0 {
		return
	}
	if session.askTab < 0 || session.askTab >= len(entries) {
		session.askTab = 0
	}
	answered := 0
	for _, entry := range entries {
		if entry.group.Answered(entry.index) {
			answered++
		}
	}

	// 和正文/输入卡同宽、居中（harness 的提问卡也在这条列里）
	ui.Column(c).FillWidth().Padding(chatSidePadding, 4).Children(func() {
		ui.Column(c).MaxWidth(a.contentWidth()).AlignSelf(ui.Center).FillWidth().
			Padding(10).Gap(8).Background(theme.Surface).Radius(8).Children(func() {
			a.askHeader(c, session, entries, answered, theme)
			a.askQuestion(c, session, entries[session.askTab], theme)
			ui.Text(c, a.tr("ask.hint")).
				FontSize(11).TextColor(theme.TextMuted)
			ui.Row(c).Gap(8).Children(func() {
				if ui.Button(c, a.tr("ask.skip")).Clicked() {
					a.dismissAsks()
				}
				// 「提交」要等**每一题**都答完才可用：
				// 多问题时用户容易漏答一题就提交，模型拿到空答案还得反问一次，
				// 白跑一个回合。所有组都要答完（一个回合可能有多个 ask 调用）。
				ready := true
				for _, group := range session.AskGroups() {
					if !group.AllAnswered() {
						ready = false
						break
					}
				}
				submit := ui.PrimaryButton(c, a.tr("ask.submit")).Disabled(!ready)
				if ready && submit.Clicked() {
					a.submitAsks("")
				}
			})
		})
	})
}

// askHeader 渲染标题行与页签条。
func (a *App) askHeader(c *ui.Context, session *Session, entries []askEntry,
	answered int, theme *ui.Theme) {
	ui.Row(c).Gap(6).Children(func() {
		ui.Text(c, a.tr("ask.title")).Bold().FontSize(13).TextColor(theme.Accent)
		ui.Text(c, a.tr("ask.counts", len(entries), answered)).
			FontSize(11).TextColor(theme.TextMuted)
	})
	if len(entries) < 2 {
		return
	}
	// 一个问题一个页签：已回答的加 ✓，当前页签高亮
	ui.Row(c).Gap(4).Children(func() {
		for index, entry := range entries {
			label := entry.spec.Header
			if label == "" {
				label = a.tr("ask.question", index+1)
			}
			if entry.group.Answered(entry.index) {
				label = "✓ " + label
			}
			tab := ui.Text(c, label).FontSize(12).Padding(3, 8).Radius(6).
				Cursor(ui.CursorPointer)
			if index == session.askTab {
				tab.TextColor(theme.Text).Background(theme.Background).Border(1, theme.Border)
			} else {
				tab.TextColor(theme.TextMuted)
				if tab.Hovered() {
					tab.Background(theme.SurfaceHover)
				}
			}
			if tab.Clicked() {
				session.askTab = index
			}
		}
	})
}

// askQuestion 渲染当前页签的问题与选项。
func (a *App) askQuestion(c *ui.Context, session *Session, entry askEntry, theme *ui.Theme) {
	question := entry.spec
	label := question.Question
	if question.MultiSelect {
		label += a.tr("ask.multiselect")
	}
	ui.Column(c).FillWidth().Gap(4).Children(func() {
		ui.Text(c, label).FontSize(13)
		if len(question.Options) == 0 {
			ui.Text(c, a.tr("ask.noOptions")).
				FontSize(11).TextColor(theme.TextMuted)
			return
		}
		for optionIndex, option := range question.Options {
			a.askOption(c, session, entry, optionIndex, option, theme)
		}
	})
	// 每一题都有自己的输入框：写了文字就等于用这段文字作答（同时取消已选选项）。
	// 文字会一直留在框里——改选预设选项不算"删掉"，随时能改回文字作答。
	draft := session.askDraft(entry.group.CallID, entry.index)
	inactive := entry.group.Custom(entry.index) != "" && !entry.group.CustomActive(entry.index)
	border := theme.Border
	if inactive {
		border = theme.Border.Mix(theme.Text, 0.2)
	}
	ui.Column(c).FillWidth().Background(theme.Background).Border(1, border).
		Radius(8).Padding(4, 8).Children(func() {
		area := ui.TextAreaBase(c, draft).Lines(1, 4)
		area.Placeholder(a.tr("ask.answerPlaceholder"))
		area.Label(a.tr("ask.answer", entry.index+1))
	})
	if inactive {
		ui.Text(c, a.tr("ask.inactive")).
			FontSize(11).TextColor(theme.TextMuted)
	}
	// 只有用户**改了**输入框才回写 driver：改选选项后文字留在框里，不能被同步覆盖
	key := askKey(entry.group.CallID, entry.index)
	if trimmed := strings.TrimSpace(*draft); session.askSynced[key] != trimmed {
		session.syncAsk(key, trimmed)
		entry.group.SetCustom(entry.index, *draft)
	}
}

// askOption 渲染一个选项行（整行可点，文字左对齐）。
func (a *App) askOption(c *ui.Context, session *Session, entry askEntry,
	optionIndex int, option tools.QuestionOption, theme *ui.Theme) {
	picked := entry.group.Selected(entry.index, optionIndex)
	row := ui.Row(c).FillWidth().Gap(8).Padding(5, 8).Radius(6).Cursor(ui.CursorPointer)
	if picked || row.Hovered() {
		row.Background(theme.SurfaceHover)
	}
	row.Children(func() {
		ui.Text(c, pickGlyph(entry.spec.MultiSelect, picked)).FontSize(13).
			TextColor(pick(picked, theme.Accent, theme.TextMuted))
		ui.Column(c).Grow(1).Gap(2).Children(func() {
			ui.Text(c, option.Label).FontSize(13).TextColor(theme.Text)
			if option.Description != "" {
				ui.Text(c, option.Description).FontSize(11).TextColor(theme.TextMuted)
			}
		})
	})
	if row.Clicked() {
		// 选选项即以选项作答；已写的文字**保留**在输入框里，随时可以改回去
		entry.group.Toggle(entry.index, optionIndex)
		session.syncAsk(askKey(entry.group.CallID, entry.index),
			strings.TrimSpace(*session.askDraft(entry.group.CallID, entry.index)))
	}
}

// pickGlyph 是选项前的选择标记。
func pickGlyph(multiSelect, picked bool) string {
	switch {
	case multiSelect && picked:
		return "☑"
	case multiSelect:
		return "☐"
	case picked:
		return "◉"
	default:
		return "○"
	}
}

// composer 渲染输入区（Enter 发送/插话、Alt+Enter 换行、Shift+Enter 换行）。
//
// 回合进行中（Streaming / RunningTools）输入框**仍然可用**：此时回车不是发送，
// 而是把这条消息排队，等当前回合的下一个安全边界投递给模型（steer）。
// 只有「等待回答」（输入框里是自定义答案）与「压缩中」（不改会话内容）时才禁用。
func (a *App) composer(c *ui.Context) {
	session := a.Current()
	if session == nil {
		return
	}
	theme := c.Theme()
	busy := session.Busy()
	// steer 可用时输入框照常输入，只是按钮语义从「发送」变成「插话」
	steerable := session.CanSteer()
	disabled := busy != driver.BusyIdle && busy != driver.BusyAwaitingAnswer && !steerable

	// 对齐 deepseek-harness 的输入卡：居中、圆角面板、更宽的内边距，
	// 输入框用无装饰的 TextAreaBase（卡片本身提供边框与底色），
	// 底部一行是控制区（提示 + 上下文用量 + 发送/停止）。
	ui.Column(c).FillWidth().Padding(chatSidePadding, 8).Children(func() {
		ui.Column(c).MaxWidth(a.contentWidth()).AlignSelf(ui.Center).FillWidth().Children(func() {
			// 先数这一帧有几个元素插在输入卡**前面**（含错误条、排队条、
			// 任务清单、斜杠候选）。数量一变，输入框在树里的位置就变了，
			// 焦点会丢 —— 下一帧还给输入框。
			//
			// 为什么按"数量"而不是逐个判断：这些面板各有各的显示条件，
			// 逐个写容易漏（斜杠候选就是这么漏掉的）；按数量比对新加面板
			// 不需要改这里。
			before := 0
			if session.errMsg != "" {
				before++
			}
			if session.archivedSession {
				before++
			}
			if len(session.Queued()) > 0 {
				before++
			}
			if a.todoPanelShown(session) {
				before++
			}
			if a.slashHintShown() {
				before++
			}
			if before != a.composerSiblings {
				a.composerSiblings = before
				a.refocusComposer = true
			}

			if session.errMsg != "" {
				ui.Text(c, session.errMsg).FontSize(12).TextColor(theme.Danger).Padding(2, 8)
			}
			// 归档会话：说明"为什么发不出去"，并给一个恢复入口。
			//
			// 只禁用输入框不解释，用户只会以为坏了 —— 归档是**会话级封存**，
			// 得先恢复才能继续，这一点必须说清楚。恢复按钮就放在提示旁边：
			// 让用户在这里一步就能解开，不用去翻右键菜单。
			if session.archivedSession {
				a.archivedBanner(c, session, theme)
			}
			// 排队中的插话：让用户看得见"已经记下了、还没发出去"
			if queued := session.Queued(); len(queued) > 0 {
				a.queuedPanel(c, queued)
			}
			// 任务清单：dsh 把它挂在 `conversation.input.dock`，即输入卡**正上方**
			a.todoPanel(c, session, theme)
			// 斜杠命令候选：输入框内容以 / 开头时浮在输入卡上方
			a.slashHintsPanel(c, theme)
			ui.Column(c).Background(theme.Background).Border(1, theme.Border).Radius(composerRadius).
				Padding(10, 12).Gap(8).FillWidth().Children(func() {
				// 待发送的图片（粘贴/拖入）
				if len(session.attachments) > 0 {
					a.attachmentsPanel(c, session)
				}
				handler := &Composer{
					Draft: &a.draft,
					Send:  func() { a.Send() },
					History: func(delta, caret int) bool {
						return a.historyNavigate(session, delta, caret)
					},
					RequestCaret: func(pos int) {
						a.pendingCaret = pos
						a.pendingCaretArmed = true
					},
					Paste: func() bool { return a.pasteImages(session) },
				}
				area := ui.TextAreaBase(c, &a.draft).Lines(1, 8)
				area.Placeholder(a.composerPlaceholder(session)).Label(a.tr("composer.label"))
				area.Disabled(disabled)
				handler.Area = area
				// 上一帧请求的光标位置（翻历史 / Alt+Enter 换行）要在**编辑器
				// 同步完新文本之后**设置，否则会被旧长度截断；因此先应用再挂
				// HandleInput（这样本帧按下的键只会影响下一帧）
				if a.pendingCaretArmed {
					area.SetTextSelection(a.pendingCaret, a.pendingCaret)
					a.pendingCaretArmed = false
				}
				if a.refocusComposer {
					area.Focus()
					a.refocusComposer = false
				}
				area.HandleInput(handler.HandleInput)

				// 底部控制行（dsh 输入卡）：左「+ 附件」「权限」，右侧模型 chip 与发送
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					if iconButton(c, iconPlusSVG, a.tr("composer.attach"), 14, 3) {
						a.pickImageFiles(session)
					}
					a.permissionChip(c, session, theme)
					if hint := a.composerWarning(session); hint != "" {
						ui.Text(c, hint).FontSize(11).TextColor(theme.Danger)
					}
					ui.Spacer(c)
					a.modelChip(c, session, theme)
					if busy != driver.BusyIdle || session.Compacting() {
						if ui.Button(c, a.tr("composer.stop")).Clicked() {
							a.Stop()
						}
					}
					a.sendCircleButton(c, session, steerable, theme)
				})
			})
			// P4 状态条：左「上下文 x / y（n%）」+ 条形，右「轮数 / 步数」。
			// 其余指标（tok/s、cache hit）见方案 §6。
			a.statusBar(c, session, theme)
		})
	})
}

// composerWarning 返回输入卡里需要**一直看得见**的提示（空表示没有）。
//
// 键盘提示（Enter/Alt+Enter）不再常驻（dsh 也没有）；这里只放"会阻止发送"或
// "行为与平时不同"的说明：模型不支持图片、插话暂不支持附件。
func (a *App) composerWarning(session *Session) string {
	if len(session.attachments) > 0 && !session.Vision() {
		return a.tr("composer.vision.unsupported")
	}
	return ""
}

// pickImageFiles 打开系统文件对话框选图片（阻塞调用放到后台，结果回主线程）。
//
// mygo 的 `Dialog.Open` 会阻塞到对话框关闭，所以必须 `go` 出去，
// 再用 `a.post` 回到主线程（与拖入共用 `attachFiles` 这条路径）。
func (a *App) pickImageFiles(session *Session) {
	if a.opts.PickImageFiles == nil {
		return
	}
	pick := a.opts.PickImageFiles
	sid := session.ID
	go func() {
		paths, err := pick()
		if err != nil || len(paths) == 0 {
			return
		}
		a.post(func() {
			if s := a.find(sid); s != nil {
				a.attachFiles(s, paths)
			}
		})
	}()
}

// sendDisabled 报告发送/插话按钮此刻是否应禁用。
//
// 空输入只有一种情况可以点：**空闲**且队列里有失败后留下的插话（点一下等于
// "把队列发出去"）。忙时队列非空但输入为空，按钮必须禁用——此时 Send 会直接
// return，点了没有任何反应。
func (a *App) sendDisabled(session *Session) bool {
	// 归档的会话必须先恢复才能对话（历史仍可查看）。
	// 放在最前面：这不是"暂时不能发"，而是"这个会话被封存了"，
	// 输入卡会用专门的提示文案说明（见 composer 的归档提示）。
	if session.archivedSession {
		return true
	}
	busy := session.Busy()
	steerable := session.CanSteer()
	if busy != driver.BusyIdle && busy != driver.BusyAwaitingAnswer && !steerable {
		return true
	}
	// 附件还有没就绪的（正在写 blob 存储）：先 Put 成功再发，别写出悬空引用
	if !attachmentsReady(session) {
		return true
	}
	// 模型不支持图片：禁止发送（提示在输入框下方）
	if len(session.attachments) > 0 && !session.Vision() {
		return true
	}
	if strings.TrimSpace(a.draft) != "" || len(session.attachments) > 0 {
		return false
	}
	return !(session.CanSend() && session.HasQueued())
}

// SetErrMsgForTest 设置会话错误提示（测试用：清空以观察按键链路）。
func (a *App) SetErrMsgForTest(text string) {
	if session := a.Current(); session != nil {
		session.errMsg = text
	}
}

// ErrMsgForTest 返回会话当前的错误提示（测试用）。
func (a *App) ErrMsgForTest() string {
	if session := a.Current(); session != nil {
		return session.errMsg
	}
	return ""
}

// ApplyChangeForTest 把一条 host 变化喂给界面（测试用：验证 cmdResult 的处理）。
func (a *App) ApplyChangeForTest(change host.Change) { a.onHostChange(change) }

// ArchivedForTest 返回当前会话已加载的归档条数与其消息文本（测试用）。
func (a *App) ArchivedForTest() []string {
	session := a.Current()
	if session == nil {
		return nil
	}
	out := make([]string, 0, len(session.archived))
	for _, entry := range session.archived {
		out = append(out, entry.Message.PreviewText())
	}
	return out
}

// LoadOlderForTest 触发一次归档加载（测试用）。
func (a *App) LoadOlderForTest() {
	if session := a.Current(); session != nil {
		a.loadOlder(session)
	}
}

// SetExecutorsForTest 注入执行端来源（测试用）。
func (a *App) SetExecutorsForTest(source ExecutorSource) { a.executors = source }

// PickerReadyForTest 报告执行端列表是否已经拉到（测试用）。
func (a *App) PickerReadyForTest() bool { return len(a.executorList) > 0 }

// PickerOpenForTest 报告选择执行端的列表是否打开（测试用）。
func (a *App) PickerOpenForTest() bool { return a.pickerOpen }

// PickExecutorForTest 选中列表第 index 项（测试用）。
func (a *App) PickExecutorForTest(index int) { a.pickExecutor(index) }

// SyncSessionsForTest 重新同步会话列表（测试用）。
func (a *App) SyncSessionsForTest() { a.syncSessions() }

// HostForTestNewLocal 新建一个独立的本地执行端（测试用：模拟切换到的另一台机器）。
func (a *App) HostForTestNewLocal() host.Host {
	return host.NewLocal(host.Config{
		Deferred: true, Runtime: a.rt, Post: func(f func()) { a.post(f) },
	})
}

// SetHostForTest 替换 host（测试用：注入"上传总会失败"的 blob 服务等）。
func (a *App) SetHostForTest(h host.Host) { a.host = h }

// AttachImageBytesForTest 把图片字节挂成附件（测试用：走粘贴/拖入同一条路径）。
func (a *App) AttachImageBytesForTest(data []byte) {
	if session := a.Current(); session != nil {
		a.attachImages(session, [][]byte{data})
	}
}

// AddPendingAttachmentForTest 挂一个"读取中"的附件（测试用）。
func (a *App) AddPendingAttachmentForTest(name string) {
	if session := a.Current(); session != nil {
		session.attachments = append(session.attachments,
			&attachment{status: attachPending, name: name})
	}
}

// WorkspaceForTest 返回会话的工作区（测试用）。
func (s *Session) WorkspaceForTest() string { return s.Session.Workspace }

// SetWorkspaceForTest 设置会话的工作区（测试用：模拟不同项目的会话）。
func (s *Session) SetWorkspaceForTest(workspace string) { s.Session.Workspace = workspace }

// SelectModelForTest 切换当前会话的模型（测试用：走与界面相同的路径）。
func (a *App) SelectModelForTest(sel providers.Selection) {
	if session := a.Current(); session != nil {
		a.selectModel(session, sel, "")
	}
}

// selectModel 切换模型（命令走执行端）。
func (a *App) selectModel(session *Session, sel providers.Selection, effort string) {
	payload, _ := json.Marshal(host.SelectPayload{Selection: sel, Effort: effort})
	_, _ = a.host.Submit(host.Command{SessionID: session.ID, Kind: "selectModel", Payload: payload})
	if a.opts.OnSelectionChanged != nil {
		a.opts.OnSelectionChanged(session.ID, sel)
	}
}

// AttachmentsForTest 返回待发送附件数量（测试用）。
func (a *App) AttachmentsForTest() int {
	if session := a.Current(); session != nil {
		return len(session.attachments)
	}
	return 0
}

// AttachmentsReadyForTest 报告附件是否都已就绪（测试用）。
func (a *App) AttachmentsReadyForTest() bool {
	session := a.Current()
	if session == nil {
		return true
	}
	return attachmentsReady(session)
}

// SetVisionForTest 切换当前模型是否支持图片（测试用：模拟换模型）。
func (a *App) SetVisionForTest(vision bool) {
	if session := a.Current(); session != nil {
		payload, _ := json.Marshal(vision)
		_, _ = a.host.Submit(host.Command{SessionID: session.ID, Kind: "setVision", Payload: payload})
	}
}

// DropFilesForTest 模拟"把文件拖进聊天区"（测试用）。
//
// mygo 的 drop 事件只能由平台层触发（Tester 没有公开事件入口），所以在这里
// 提供与平台回调完全相同的入口；mygo 自己的 ui/drop_test.go 覆盖了事件链路。
func (a *App) DropFilesForTest(paths []string) { a.attachFiles(a.Current(), paths) }

// SendDisabledForTest 返回发送按钮是否禁用（测试用：与 composer 用同一套判断）。
func (a *App) SendDisabledForTest() bool {
	session := a.Current()
	if session == nil {
		return true
	}
	return a.sendDisabled(session)
}

// ContextPercentForTest 返回当前会话的上下文占比（0–100，测试用）。
func (a *App) ContextPercentForTest() int {
	if session := a.Current(); session != nil {
		return a.contextPercent(session)
	}
	return 0
}

// CacheHitBasisPointsForTest 返回当前会话的缓存命中率（千分比，测试用）。
//
// 用累计口径（与状态条一致）。千分比而不是百分比：显示保留两位小数
// （99.93% → 9993），整数传递不会引入浮点误差。
func (a *App) CacheHitBasisPointsForTest() int {
	if session := a.Current(); session != nil {
		return cacheHitBasisPoints(session.TotalUsage())
	}
	return 0
}

// DeleteSessionForTest 按 id 删除会话（测试用：走界面同一条删除路径）。
func (a *App) DeleteSessionForTest(id string) {
	for index, session := range a.sessions {
		if session.ID == id {
			a.deleteSession(index)
			return
		}
	}
}

// composerPlaceholder 返回输入框占位文案（忙时提示这是「插话」）。
func (a *App) composerPlaceholder(session *Session) string {
	switch {
	case session.CanSteer():
		return a.tr("composer.placeholder.steer")
	case session.HasQueued():
		return a.tr("composer.placeholder.queued")
	case session.Busy() == driver.BusyAwaitingAnswer:
		return a.tr("composer.placeholder.ask")
	case session.Compacting():
		return a.tr("composer.placeholder.compact")
	default:
		return a.tr("composer.placeholder")
	}
}

// queuedPanel 渲染排队中的插话（每条一行，带删除按钮）。
// archivedBanner 提示"会话已归档"，并给一键恢复。
//
// 归档 = 封存：历史照常能看（翻页、加载更早都不受影响），但不能再对话。
// 用户在输入框里打字时看到的是灰色不可用的发送键 —— 不说清楚原因，
// 只会以为程序坏了。
func (a *App) archivedBanner(c *ui.Context, session *Session, theme *ui.Theme) {
	ui.Row(c).FillWidth().Gap(8).Padding(4, 8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, a.tr("session.archivedHint")).
			FontSize(12).TextColor(theme.TextMuted).Grow(1)
		if ui.Button(c, a.tr("sidebar.restore")).Clicked() {
			a.setSessionArchived(session, false)
		}
	})
}

func (a *App) queuedPanel(c *ui.Context, queued []string) {
	theme := c.Theme()
	ui.Column(c).FillWidth().Padding(4, 8).Gap(2).Children(func() {
		ui.Text(c, a.tr("composer.queued", len(queued))).
			FontSize(11).TextColor(theme.TextMuted)
		for index, text := range queued {
			row := ui.Row(c).FillWidth().Gap(6).Padding(2, 4).Radius(6)
			row.Children(func() {
				ui.Text(c, clipLine(text, 80)).FontSize(12).TextColor(theme.Text).
					MaxLines(1).Grow(1)
				if textButton(c, "×", 12, theme.TextMuted) {
					a.dropQueued(index)
				}
			})
		}
	})
}

// dropQueued 撤回一条排队中的插话。
func (a *App) dropQueued(index int) {
	session := a.Current()
	if session == nil {
		return
	}
	payload, _ := json.Marshal(host.IndexPayload{Index: index})
	_, _ = a.host.Submit(host.Command{SessionID: session.ID, Kind: "dropQueued", Payload: payload})
}

// contextNumbers 返回上下文已用 token 与窗口大小（窗口未知时用默认值）。
func (a *App) contextNumbers(session *Session) (used, window int) {
	used = session.TokensInUse()
	// 窗口必须取**会话自己的**模型（切换/恢复后可能与默认模型不同），否则百分比
	// 和 Meter 的警告色会与自动压缩的真实阈值对不上。
	return used, session.EffectiveContextWindow()
}

// contextPercent 返回上下文占比（0–100，向上取整：只要用了一点就不显示 0%）。
func (a *App) contextPercent(session *Session) int {
	used, window := a.contextNumbers(session)
	if window <= 0 {
		return 0
	}
	percent := (used*100 + window - 1) / window
	return clampInt(percent, 0, 100)
}

// statusBar 是输入卡下方的一行状态条。
//
// 左侧：上下文用量 + 一条 `ui.Meter` 条形（80% 起变警告色，与自动压缩阈值同源）
// + 缓存命中率（provider 报了缓存字段才显示）；
// 右侧：轮数 / 步数（口径见 driver.Counts）。
//
// tok/s 还没做：它要按回合统计输出速度，与本条"瞬时状态"的口径不同。
func (a *App) statusBar(c *ui.Context, session *Session, theme *ui.Theme) {
	used, window := a.contextNumbers(session)
	percent := a.contextPercent(session)
	counts := session.Counts()
	// 条形宽度：上下文数值本身不长，给一条窄条即可；不抢正文的视觉重心。
	const meterWidth = 64
	const meterHeight = 4

	ui.Row(c).FillWidth().Padding(2, 8).Gap(8).AlignItems(ui.Center).Children(func() {
		// 文本**不设 Label**：mygo 里元素一旦有 Label，a11y 树就用 Label 顶替
		// 可见文本（runtime.go 的 label 取值逻辑），文本本身反而不暴露给读屏。
		// ui.Text 的无障碍名就是它自己的文字，设 Label 只会帮倒忙。
		ui.Text(c, a.tr("composer.usagePercent", compactTokens(used), compactTokens(window), percent)).
			FontSize(11).TextColor(theme.TextMuted)
		// Meter 是图形，没有文字，必须靠 Label 才能被读屏念出来。
		ui.Meter(c, float64(percent), 0, 100, &ui.MeterLevels{
			// 与 driver.AutoCompactRatio 同源：到了会自动压缩的位置就是警告色。
			Warning:  driver.AutoCompactRatio * 100,
			Critical: 95,
		}).Width(meterWidth).Height(meterHeight).Label(a.tr("composer.usageLabel"))

		// 缓存命中率：用**累计口径**（这个会话所有已完成请求之和）。
		//
		// 为什么不用单次：单次抖动极大 —— 新建缓存那一轮是 0%，紧接着
		// 全命中的一轮是 99.9%，看单次会以为缓存时好时坏。累计值才反映
		// "这个会话一共省了多少"，而且不会因为切换回合而闪烁。
		//
		// 判据用 CacheRead > 0 而不是"字段存在"：旧网关压根不发这些字段，
		// 读出来恒为 0；画面上突然多个"缓存 0%"会让人以为缓存失效了。
		// 命中过才显示，第一次请求（纯写入、还没得命中）也自然不显示。
		// 旧日志（没有累计基线）同样是 0/0 → 不显示，等跑一轮真实请求再出现。
		usage := session.TotalUsage()
		if usage.CacheReadTokens > 0 {
			ui.Text(c, a.tr("composer.cacheHit", formatBasisPoints(cacheHitBasisPoints(usage)))).
				FontSize(11).TextColor(theme.TextMuted).
				Label(a.tr("composer.cacheHint",
					compactTokens(usage.CacheReadTokens),
					compactTokens(usage.CacheReadTokens+usage.CacheWriteTokens+usage.InputTokens)))
		}

		ui.Spacer(c)
		ui.Text(c, a.tr("composer.turns", counts.Turns)).FontSize(11).TextColor(theme.TextMuted)
		ui.Text(c, a.tr("composer.steps", counts.Steps)).FontSize(11).TextColor(theme.TextMuted)
	})
}

// cacheHitBasisPoints 返回缓存命中率（单位：千分比）。
//
// **两家 provider 的 input 语义不同，不能用一个公式套**：
//
//	OpenAI     prompt_tokens **已包含**命中部分 → 分母就是 InputTokens
//	Anthropic  input_tokens **不含**缓存读写     → 分母要三者相加
//
// 怎么区分？CacheWriteTokens 只有 Anthropic 才有（OpenAI 是自动缓存、
// 没有"写入费"这个概念）。所以：
//   - CacheWrite > 0 → Anthropic 口径
//   - 否则           → OpenAI 口径
//
// 这个判据在"Anthropic 本次没写缓存"时会退化，但那也正是 OpenAI 口径的
// 算法（分母=Input），结果仍然正确。
func cacheHitBasisPoints(usage driver.Usage) int {
	var total int
	if usage.CacheWriteTokens > 0 {
		// Anthropic：input 不含缓存，三者相加才是完整输入
		total = usage.CacheReadTokens + usage.CacheWriteTokens + usage.InputTokens
	} else {
		// OpenAI：prompt_tokens 已含命中部分
		total = usage.InputTokens
		if total < usage.CacheReadTokens {
			// 防御：个别网关的 prompt_tokens 不含缓存，以两者的较大值为准
			total = usage.CacheReadTokens
		}
	}
	if total <= 0 {
		return 0
	}
	// **向下取整到两位小数，并且不显示 100%**。
	//
	// 缓存不可能真的全命中：本轮新增的那段提示词（最新用户消息、最新工具
	// 结果）必然要重新计算，至少占几百 token。99.996% 四舍五入成 100% 会让
	// 人以为"这次一分钱没花"，反而不可信。
	//
	// 所以上限压在 99.99：显示"缓存 99.99%"是诚实的；真到 100 时宁可少报一点。
	//
	// 返回值单位是**千分比**（99.93% → 9993），显示时再除以 100 保留两位小数。
	// 用整数算避免浮点误差（0.1+0.2 那类问题在百分比上会显示成 99.989999%）。
	basisPoints := usage.CacheReadTokens * 10_000 / total
	if basisPoints > 9_999 {
		basisPoints = 9_999
	}
	return clampInt(basisPoints, 0, 9_999)
} // formatBasisPoints 把千分比转成两位小数的百分比字符串（9993 → "99.93"）。
// 不返回浮点再 fmt：百分比只有两位小数，用整数拆整个/小数部分最直观，
// 也避免 9993/100 = 99.93000000000001 这种显示。
func formatBasisPoints(basisPoints int) string {
	whole := basisPoints / 100
	frac := basisPoints % 100
	// 小数补足两位：9900 → "99.00"、9905 → "99.05"
	if frac < 10 {
		return itoa(whole) + ".0" + itoa(frac)
	}
	return itoa(whole) + "." + itoa(frac)
}

// compactTokens 把 token 数压成 12k / 1.2M 这样的短形式。
func compactTokens(tokens int) string {
	switch {
	case tokens >= 1_000_000:
		return itoa(tokens/1_000_000) + "M"
	case tokens >= 1_000:
		return itoa(tokens/1_000) + "k"
	default:
		return itoa(tokens)
	}
}

// Send 发送当前草稿；回合进行中则改为「插话」（排队到下一个安全边界）。
func (a *App) Send() {
	session := a.Current()
	if session == nil {
		return
	}
	text := strings.TrimSpace(a.draft)
	// 斜杠命令：以 / 开头时先按命令解释，**不发给模型**。
	// 未知命令会返回"未知命令"提示（不会把正文当命令吞掉——
	// 命令表里没有的 /xxx 也给出提示，是想发正文的话可以加个空格前缀）。
	if isSlashCommand(text) {
		a.draft = ""
		if message, ok := a.ExecSlashCommand(text); ok {
			if message != "" {
				a.NotifyDisplay(message)
			}
			return
		}
	}
	// 等待回答时，输入框里的文字是 custom 答案（开放式问题靠这条作答）
	if session.Busy() == driver.BusyAwaitingAnswer {
		if text == "" {
			return // 空文本提交等于「按已选项提交」，用「提交」按钮
		}
		a.draft = ""
		a.submitAsks(text)
		return
	}
	// 回合进行中：排队（不追加消息、不开流、不落盘）。
	if session.CanSteer() {
		// 插话暂不带图片（v1）：文字进队列、图片跟着下一条消息发，会被拆开。
		if len(session.attachments) > 0 {
			session.errMsg = a.tr("composer.steer.images")
			return
		}
		if text == "" {
			return
		}
		a.enqueue(session, text)
		return
	}
	// 空闲时有排队的插话（回合失败后保留的）：即使输入框是空的，按发送也把它们发出去
	if text == "" && !session.HasQueued() && len(session.attachments) == 0 {
		return
	}
	if !session.CanSend() {
		return
	}
	// Enter 绕过了按钮的禁用状态，这里必须再判一次：否则"模型不支持图片"时
	// 后面的清理已经把草稿和附件删掉，发送却失败了——用户的字和图都会丢。
	if a.sendDisabled(session) {
		switch {
		case len(session.attachments) > 0 && !session.Vision():
			session.errMsg = a.tr("composer.vision.unsupported")
		case !attachmentsReady(session):
			session.errMsg = a.tr("composer.images.pending")
		}
		return
	}
	attachments, ok := a.attachmentParts(session)
	if !ok {
		session.errMsg = a.tr("composer.images.pending")
		return
	}
	a.submitSend(session, text, attachments)
}

// submitSend 提交一条 send 命令，并**乐观**清空草稿与附件。
//
// 命令是异步的：这里先清空（界面立刻响应），同时把原来的内容记在 pending 里；
// 收到 cmdResult 失败时再还回去（见 handleCmdResult）。
func (a *App) submitSend(session *Session, text string, attachments []message.Part) string {
	refs := make([]host.AttachmentRef, 0, len(attachments))
	for _, part := range attachments {
		url, _ := part.AsURL()
		hash := strings.TrimPrefix(url, "blob:sha256:")
		refs = append(refs, host.AttachmentRef{Hash: hash, MediaType: part.MediaType, Name: part.Filename})
	}
	payload, _ := json.Marshal(host.SendPayload{Text: text, Attachments: refs})
	cmdID, err := a.host.Submit(host.Command{SessionID: session.ID, Kind: "send", Payload: payload})
	if err != nil {
		session.errMsg = a.tr("composer.error.send", err.Error())
		return ""
	}
	if a.pending == nil {
		a.pending = map[string]pendingSend{}
	}
	a.pending[cmdID] = pendingSend{
		sessionID:   session.ID,
		draft:       a.draft,
		attachments: session.attachments,
	}
	a.draft = ""
	// 会话自己存的那份也要清：否则切走再切回来，
	// 已经发出去的内容会重新出现在输入框里。
	session.draft = ""
	a.history.reset()
	session.attachments = nil
	return cmdID
}

// enqueue 提交一条插话（同样乐观清空草稿）。
func (a *App) enqueue(session *Session, text string) {
	payload, _ := json.Marshal(host.TextPayload{Text: text})
	cmdID, err := a.host.Submit(host.Command{SessionID: session.ID, Kind: "enqueue", Payload: payload})
	if err != nil {
		session.errMsg = a.tr("composer.error.steer", err.Error())
		return
	}
	if a.pending == nil {
		a.pending = map[string]pendingSend{}
	}
	a.pending[cmdID] = pendingSend{sessionID: session.ID, draft: a.draft}
	a.draft = ""
	a.history.reset()
}

// pendingSend 是一条"已经乐观清空"的发送，失败时要还原。
type pendingSend struct {
	sessionID   string
	draft       string
	attachments []*attachment
}

// Stop 停止当前生成。
func (a *App) Stop() {
	session := a.Current()
	if session == nil {
		return
	}
	// 压缩中停止：执行端会放弃压缩并清空插话队列（停止就是停止）
	if session.Compacting() {
		_, _ = a.host.Submit(host.Command{SessionID: session.ID, Kind: "cancelCompact"})
		return
	}
	// 顺序：先让 driver 收尾（补配对结果、状态归位），再取消 ctx（host 内部做）
	_, _ = a.host.Submit(host.Command{SessionID: session.ID, Kind: "stop"})
}

// submitAsks 提交全部提问（custom 作为第一个未选项问题的答案）。
func (a *App) submitAsks(custom string) {
	session := a.Current()
	if session == nil {
		return
	}
	// 展示端：把本地勾选/手写答案一起回传（执行端据此更新自己的提问组再提交）。
	answers := make([]host.AskAnswer, 0, 2)
	for _, group := range session.AskGroups() {
		answer := host.AskAnswer{CallID: group.CallID, Selected: map[string][]int{}, Custom: map[string]string{}}
		for index, question := range group.Questions {
			for option := range question.Options {
				if group.Selected(index, option) {
					answer.Selected[question.ID] = append(answer.Selected[question.ID], option)
				}
			}
			if group.CustomActive(index) {
				answer.Custom[question.ID] = group.Custom(index)
			}
		}
		answers = append(answers, answer)
	}
	payload, _ := json.Marshal(host.AnswerPayload{Custom: custom, Answers: answers})
	_, _ = a.host.Submit(host.Command{SessionID: session.ID, Kind: "answer", Payload: payload})
}

// dismissAsks 跳过全部提问。
func (a *App) dismissAsks() {
	session := a.Current()
	if session == nil {
		return
	}
	_, _ = a.host.Submit(host.Command{SessionID: session.ID, Kind: "dismissAsk"})
}

// commitRename 提交重命名（事件由 driver 产生，重放后标题仍然在）。
func (a *App) commitRename(session *Session) {
	title := strings.TrimSpace(session.renameDraft)
	if title != "" && title != session.Title {
		payload, _ := json.Marshal(host.TextPayload{Text: title})
		_, _ = a.host.Submit(host.Command{SessionID: session.ID, Kind: "rename", Payload: payload})
	}
	session.renaming = false
}

// failureHint 把常见失败原因翻译成可操作的提示。
//
// 原始错误（服务端返回的 JSON/状态码）仍然原样显示在前面——排查问题需要它。
func (a *App) failureHint(reason string) string {
	lower := strings.ToLower(reason)
	switch {
	case strings.Contains(lower, "401"), strings.Contains(lower, "invalid api key"),
		strings.Contains(lower, "unauthorized"), strings.Contains(lower, "authentication"):
		return a.tr("error.apiKey")
	case strings.Contains(lower, "403"), strings.Contains(lower, "permission"):
		return a.tr("error.auth")
	case strings.Contains(lower, "402"), strings.Contains(lower, "insufficient"), strings.Contains(lower, "balance"):
		return a.tr("error.balance")
	case strings.Contains(lower, "429"), strings.Contains(lower, "rate limit"), strings.Contains(lower, "quota"):
		return a.tr("error.rate")
	case strings.Contains(lower, "context_length_exceeded"), strings.Contains(lower, "maximum context length"),
		strings.Contains(lower, "prompt is too long"), strings.Contains(lower, "too many tokens"):
		return a.tr("error.overflow")
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "deadline exceeded"):
		return a.tr("error.timeout")
	case strings.Contains(lower, "connection refused"), strings.Contains(lower, "no such host"),
		strings.Contains(lower, "network"):
		return a.tr("error.network")
	case strings.Contains(lower, "max_tokens"):
		return a.tr("error.maxOutput")
	case strings.Contains(lower, "404"), strings.Contains(lower, "not found"):
		return a.tr("error.badRequest")
	}
	return ""
}

func (a *App) busyLabel(busy driver.Busy) string {
	switch busy {
	case driver.BusyStreaming:
		return a.tr("busy.streaming")
	case driver.BusyRunningTools:
		return a.tr("busy.tools")
	case driver.BusyAwaitingAnswer:
		return a.tr("chat.toolWaiting")
	case driver.BusyCompacting:
		return a.tr("busy.compacting")
	default:
		return a.tr("busy.ready")
	}
}

func partData(part message.Part) (map[string]any, bool) {
	dataPart, ok := part.Content.(message.DataPart)
	if !ok {
		return nil, false
	}
	data, ok := dataPart.Data.(map[string]any)
	return data, ok
}

func stringOf(data map[string]any, key string) string {
	value, _ := data[key].(string)
	return value
}

// pick 返回条件成立时的第一个值。
func pick[T any](condition bool, whenTrue, whenFalse T) T {
	if condition {
		return whenTrue
	}
	return whenFalse
}

// setEffort 切换思考档位（仅空闲时），命令走执行端。
func (a *App) setEffort(session *Session, effort string) {
	if session.Busy() != driver.BusyIdle {
		session.errMsg = a.tr("effort.busy")
		return
	}
	payload, _ := json.Marshal(host.TextPayload{Text: effort})
	_, _ = a.host.Submit(host.Command{SessionID: session.ID, Kind: "setEffort", Payload: payload})
}
