package uiapp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
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
	Selection     providers.Selection
	SystemPrompt  string
	AllowShell    bool
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
	// Title 是新会话的默认标题。
	Title string
	// Host 覆盖执行端（展示端接入 remoteHost 时用；nil = 进程内 Local）。
	Host host.Host
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
			a.active = index
			a.host.SetActive(id)
			a.restoreDraftIfIdle(shell)
			return
		}
	}
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
	// 附件条是插在输入框**前面**的兄弟元素：它出现/消失会让元素树变化、焦点丢失
	// （实测贴完图后输入框就不再聚焦，用户得再点一下才能继续打字）。
	refocusComposer bool
	// PasteImagesForTest 覆盖剪贴板读取（测试用：headless 里没有真实剪贴板图片）。
	PasteImagesForTest func() ([][]byte, error)
	// pendingCaret/pendingCaretArmed 是"下一帧把输入框光标放到这里"的请求。
	// 在本帧设置会被编辑器缓冲的**旧长度**截断，所以只能延后一帧。
	pendingCaret      int
	pendingCaretArmed bool
	// expandedParts 记录哪些折叠行被展开（key = 消息 id + part 序号）。
	expandedParts map[string]bool
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
		rt:            rt,
		opts:          opts,
		post:          post,
		lang:          lang,
		mdParser:      markdown.NewParser(),
		mdCache:       markdown.NewCache(256),
		images:        newImageStore(),
		hlCache:       markdown.NewLRU[[]markdown.HighlightLine](128),
		expandedParts: map[string]bool{},
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
		app.host.Subscribe(app.onHostChange)
		if errText := app.host.StartupError(); errText != "" && app.Current() != nil {
			app.startupErr = errText
			app.NotifyDisplay(errText)
		}
		return app
	}
	app.host = host.NewLocal(host.Config{
		Runtime:            rt,
		Selection:          opts.Selection,
		SystemPrompt:       opts.SystemPrompt,
		AllowShell:         opts.AllowShell,
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
		Tr:                 app.tr,
		Title:              opts.Title,
		Efforts:            app.effortsFor,
		Deferred:           opts.DeferredCommands,
		// 执行端把模型列表公布给展示端（providerChoices 已按配置生成）。
		Models: opts.Providers,
	})
	if local, ok := app.host.(*host.Local); ok {
		local.SetEfforts(app.effortsFor)
	}
	// 会话壳（界面状态）与执行端会话一一对应
	app.syncSessions()
	app.host.Subscribe(app.onHostChange)
	app.startupErr = app.host.StartupError()
	if app.startupErr != "" && app.Current() != nil {
		// 有会话时：用显示型提示告诉用户"有些会话没恢复出来"（不写日志）
		app.NotifyDisplay(app.startupErr)
	}
	return app
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
		a.host.SetActive(a.sessions[a.active].ID)
		a.restoreDraftIfIdle(a.sessions[a.active])
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
					a.active = i
					a.host.SetActive(shell.ID)
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
				a.active = i
				a.host.SetActive(shell.ID)
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
	case itemTurnFooter:
		a.renderTurnFooter(c, item)
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

// renderProcessItem 渲染过程组：1 个 part 就内联，≥2 个合并成一行。
func (a *App) renderProcessItem(c *ui.Context, item chatItem) {
	theme := c.Theme()
	if len(item.Refs) == 1 {
		ref := item.Refs[0]
		a.partView(c, ref.Chat, ref.Index, ref.Chat.Message.Parts[ref.Index], theme.Text)
		return
	}

	expanded := a.expandedParts[item.Key]
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
			}
		})
	}
	if !expanded {
		header()
		return
	}
	// 展开后：**标题行**保持原样，竖线只从标题下方的正文开始。
	//
	// 之前竖线把标题行也包进去（整组都缩进），看起来像"标题也是被引用的内容"，
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
			for _, ref := range item.Refs {
				a.partView(c, ref.Chat, ref.Index, ref.Chat.Message.Parts[ref.Index], theme.Text)
			}
		})
	})
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
		// 对齐 harness：工具是一行式（图标 + 名称 + 参数摘要 + 当前状态）
		id := stringOf(data, "id")
		status := a.toolCallStatus(chat, id, streaming)
		ui.Row(c).Gap(6).Children(func() {
			ui.Text(c, "⚙").FontSize(12).TextColor(theme.TextMuted)
			ui.Text(c, stringOf(data, "name")).FontSize(13).TextColor(theme.Text)
			if summary := toolArgSummary(data); summary != "" {
				ui.Text(c, summary).FontSize(12).TextColor(theme.TextMuted)
			}
			if status != "" {
				color := theme.TextMuted
				if status == a.tr("chat.toolRunning") {
					color = theme.Accent
				}
				ui.Text(c, "· "+status).FontSize(11).TextColor(color)
			}
		})
	case render.MediaToolResult:
		data, ok := partData(part)
		if !ok {
			return
		}
		status := "toolResult"
		isError, _ := data["is_error"].(bool)
		if isError {
			status = "toolFailed"
		}
		if inProgress, _ := data["pending"].(bool); inProgress {
			status = "toolWaiting"
		}
		key := expansionKey(messageID, partIndex)
		expanded := a.expandedParts[key]
		// 用静态 key（动态拼 key 会让"表里有没有这条文案"无法静态检查）
		statusText := a.tr("chat.toolResult")
		switch status {
		case "toolFailed":
			statusText = a.tr("chat.toolFailed")
		case "toolWaiting":
			statusText = a.tr("chat.toolWaiting")
		}
		toggle := outputIndicator(status) + " " + statusText + " · " + stringOf(data, "name")
		if expanded {
			toggle += a.tr("chat.collapse")
		} else {
			toggle += a.tr("chat.expand")
		}
		if textButton(c, toggle, 12, theme.TextMuted) {
			a.expandedParts[key] = !expanded
		}
		a.toolOutput(c, data, isError, expanded, color)
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
		if output != "" {
			shown, _ := truncateOutput(output, expanded)
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

// truncateOutput 按行截断工具输出。
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
				if ui.PrimaryButton(c, a.tr("ask.submit")).Clicked() {
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
			if session.errMsg != "" {
				ui.Text(c, session.errMsg).FontSize(12).TextColor(theme.Danger).Padding(2, 8)
			}
			// 排队中的插话：让用户看得见"已经记下了、还没发出去"
			if queued := session.Queued(); len(queued) > 0 {
				a.queuedPanel(c, queued)
			}
			// 任务清单：dsh 把它挂在 `conversation.input.dock`，即输入卡**正上方**
			a.todoPanel(c, session, theme)
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
// 左侧：上下文用量 + 一条 `ui.Meter` 条形（80% 起变警告色，与自动压缩阈值同源）；
// 右侧：轮数 / 步数（口径见 driver.Counts）。
//
// 还没做的指标（tok/s、cache hit、环形）留档在方案 §6：tok/s 与 cache hit 要动
// providers 的协议层（解析 usage 的缓存字段）并用真实网关冒烟，不适合与本期一起改。
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
		ui.Spacer(c)
		ui.Text(c, a.tr("composer.turns", counts.Turns)).FontSize(11).TextColor(theme.TextMuted)
		ui.Text(c, a.tr("composer.steps", counts.Steps)).FontSize(11).TextColor(theme.TextMuted)
	})
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
