package com.simpleagent.viewer.ui

import android.app.Application
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.initializer
import androidx.lifecycle.viewmodel.viewModelFactory
import com.simpleagent.viewer.data.ArchivedMessage
import com.simpleagent.viewer.data.AskView
import com.simpleagent.viewer.data.CoreClient
import com.simpleagent.viewer.data.CoreEvent
import com.simpleagent.viewer.data.ExecutorChoice
import com.simpleagent.viewer.data.ItemView
import com.simpleagent.viewer.data.LocalCache
import com.simpleagent.viewer.data.ProcessView
import com.simpleagent.viewer.data.ProviderChoice
import com.simpleagent.viewer.data.SessionSummary
import com.simpleagent.viewer.data.TimelinePage
import com.simpleagent.viewer.data.TimelineView
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.io.File

/** 界面状态（单一数据类，渲染只读它）。 */
data class ViewerState(
    val connecting: Boolean = false,
    val connected: Boolean = false,
    /**
     * 正在断线重连（已连过，但当前这一跳断了）。
     *
     * 与 [connected] 的区别：断线重连期间两边都是"未连接"，但界面表现不同——
     * [reconnecting] 为真时**留在聊天页**并显示"重连中"，而不是把用户踢回连接页。
     * 用户没做错什么，不该被迫重新点一遍连接。
     */
    val reconnecting: Boolean = false,
    /**
     * 本次运行是否**成功连接过**。
     *
     * 用来区分两种"当前没连上"：
     * - 连过又掉了 → 是重连（显示横幅，留在聊天页）
     * - 从没连上过 → 不是重连（回连接页报错，不该出现转圈横幅）
     *
     * 用户主动断开时清回 false：那是明确意图，不是异常。
     */
    val hasConnectedOnce: Boolean = false,
    val connectError: String = "",
    /**
     * 等待用户确认的对端证书（TOFU，见 confirmPeer）。
     *
     * 由 bridge 在 TLS 握手期间登记（`pendingPeer()`），界面弹窗展示。
     * 确认过的指纹由 Go 侧写进信任列表（`<cacheDir>/known_hosts`），
     * 与桌面端同一份格式 —— 下次连接直接严格校验，不再询问。
     */
    val pendingPeer: PendingPeer? = null,
    /**
     * 正在同步（连接刚建立、正在拉会话列表与时间线）。
     *
     * 同步期间界面显示的是**缓存**或上一轮的内容，此时禁止发消息：
     * 用户以为在对着当前内容说话，而执行端状态可能已经变了。
     * 同步完成后置回 false（见 refreshConnection / syncNow）。
     */
    val syncing: Boolean = false,
    // 连接表单（恢复上次输入用）
    val addr: String = "127.0.0.1:7900",
    val route: String = "",
    val token: String = "",
    /**
     * relay 证书指纹（传输加密）。留空表示不启用 TLS。
     *
     * 十六进制，可带冒号；服务端用 `--tls --tls-print-fingerprint` 打印出来。
     */
    val fingerprint: String = "",
    /**
     * relay 上在线的执行端（用于切换）。
     *
     * 只有经 relay 且 relay 上多于一台时才有意义；直连执行端时为空。
     */
    val executors: List<ExecutorChoice> = emptyList(),
    /** 当前连着的执行端 id（直连时可能为空）。 */
    val currentHostId: String = "",
    val sessions: List<SessionSummary> = emptyList(),
    val activeSessionId: String = "",
    val timeline: TimelineView = TimelineView(),
    val providers: List<ProviderChoice> = emptyList(),
    /** 展开的过程组 key（**一级展开**：只显示最后几步）。 */
    val expanded: Set<String> = emptySet(),
    /**
     * 进一步展开到全部步骤的过程组 key（**二级展开**）。
     *
     * 为什么分两级：一个长回合的过程组动辄几十步，全展开会把时间线撑得很长，
     * 用户往上翻的时候要划很多屏。默认只给最后几步（最近发生的、通常最关心），
     * 需要看全貌时再点一次。
     *
     * 这里的 key 一定是 [expanded] 的子集：收起一级展开时要把二级一起清掉，
     * 否则下次展开会直接跳到"全部"，与"默认只显示最后几步"的预期不符。
     */
    val expandedFull: Set<String> = emptySet(),
    /**
     * 当前加载的 items 之前还有更多（用于滚到顶部时翻页）。
     *
     * 注意与 [hasMoreHistory] 区分：后者是**归档**（压缩后的旧消息），
     * 这个是**同一段会话里更早的 items**（分页尚未加载的部分）。
     */
    val hasMoreItems: Boolean = false,
    /**
     * 往前翻页的游标：当前已加载内容里**最早的事件序号**。
     *
     * 用 seq 而不是 item key：item 是渲染产物，key 里含 part 索引，
     * 流式输出期间会变，游标因此失效（真机上"翻不动"的根因）。
     * 日志的 seq 一旦写入永不变。
     */
    val firstSeq: Long = 0,
    /** 正在加载更早的 items（与 [loadingOlder] 区分：那个是归档）。 */
    val loadingEarlier: Boolean = false,
    /**
     * 用户已经往上翻了几页（0 表示还停在最后一页）。
     *
     * 这个字段决定"流式刷新时要不要重置分页游标"。不能用"保留了多少早期
     * items"去反推——那个推导在边界上会失效，表现为翻一点就弹回最后一页。
     */
    val earlierPagesLoaded: Int = 0,
    /** 一次性提示（Snackbar）。 */
    val toast: String = "",
    /** 待回答的提问（非空时弹面板）。 */
    val asks: List<AskView> = emptyList(),
    /** 提问面板当前显示第几题（多题时用页签，不竖着堆）。 */
    val askTab: Int = 0,
    /** 正在上传的附件数（界面据此显示进度并暂时禁用发送）。 */
    val uploading: Int = 0,
    /** 已上传、待发送的附件（发送后清空）。 */
    val pendingAttachments: List<PendingAttachment> = emptyList(),
    /** 加载更早历史的进度。 */
    val loadingOlder: Boolean = false,
    val archiveCursor: String = "",
    val hasMoreHistory: Boolean = true,
    /** 更早的历史（倒序渲染在列表顶部）。 */
    val archive: List<ArchivedMessage> = emptyList(),
)

/** 一张已上传到执行端、等待随消息发出的图片。 */
data class PendingAttachment(
    val hash: String,
    val name: String,
    val mediaType: String,
)

/**
 * PendingPeer 是等待用户确认的对端证书（TOFU）。
 *
 * 与桌面端 internal/peerverify 同一套语义：首次连接展示指纹让用户确认，
 * 确认过的记进信任列表，下次不再询问。
 * [changed] 为真表示"以前确认过别的值"——那可能是换证书，也可能是被冒充，
 * 提示要明显不同。
 *
 * 注意这里**没有 route/token**：确认只是回答"这个指纹可信吗"，
 * 与连接参数无关 —— 连接由 bridge 继续推进，界面不需要重新发起。
 */
data class PendingPeer(
    val addr: String,
    val fingerprint: String,
    val changed: Boolean = false,
    /** 以前确认过的指纹（changed 为真时有意义） */
    val known: String = "",
)

/**
 * 展示端的 ViewModel。
 *
 * 它只做三件事：持有 Go 内核、把内核推来的事件变成"重新拉一次快照"、
 * 把用户操作转成命令。**不推断任何会话语义**（那是 Go 的事）。
 */
class ViewerViewModel(app: Application) : AndroidViewModel(app) {

    private val cacheDir: File = File(app.cacheDir, "simpleagent").apply { mkdirs() }

    // 连接参数记在本机（手机输入地址很麻烦，尤其是中文输入法打不出 ASCII 路径）
    private val prefs = app.getSharedPreferences("viewer", android.content.Context.MODE_PRIVATE)
    /**
     * "加载更早"的互斥标志（原子）。
     *
     * 同一时刻只允许一个请求在飞：滑到顶时滚动事件很密集，
     * 光靠 state.loadingEarlier 判断会有窗口，可能并发进两次，
     * 各自用同一份游标插一批内容 —— 结果重复且位置全乱。
     */
    private val earlierLoading = java.util.concurrent.atomic.AtomicBoolean(false)
    /**
     * 上一次已经请求过的分页游标。
     *
     * 用来挡住"同一游标被反复请求"：Go 侧对同一个 before 会返回同一页，
     * 去重后 items 不增长但 has_more 仍为 true，于是界面无限重复加载
     * （真机日志里 pages 涨到 1048 就是这样）。游标推进后才允许再拉。
     */
    private var lastLoadedCursor: Long = -1

    // 流式刷新的节流状态（见 scheduleTimelineRefresh）
    private var timelineRefreshJob: kotlinx.coroutines.Job? = null
    // pollJob 是"等待用户确认对端指纹"的轮询任务（见 pollPendingPeer）。
    private var pollJob: kotlinx.coroutines.Job? = null
    private var timelineRefreshPending = false
    private val timelineRefreshIntervalMs = 80L
    /** 上次真正刷新的时间戳（配合 scheduleTimelineRefresh 做节流）。 */
    private var lastTimelineRefreshAt = 0L
    /** 首屏与翻页时每次取多少条 item（Go 侧上限 500）。 */
    private val timelinePageSize = 60

    private val state = MutableStateFlow(
        ViewerState(
            addr = prefs.getString("addr", "")?.ifEmpty { null } ?: ViewerState().addr,
            route = prefs.getString("route", "") ?: "",
            // token 与指纹也记住：手机上每次重敲这两串是灾难，而且指纹有 64 位十六进制
            token = prefs.getString("token", "") ?: "",
            fingerprint = prefs.getString("fingerprint", "") ?: "",
        ),
    )
    val uiState: StateFlow<ViewerState> = state.asStateFlow()

    private val core = CoreClient.create(cacheDir.absolutePath) { event -> onCoreEvent(event) }

    /**
     * 本地缓存：让「打开 App 就能看到上次的内容」成立，不必每次连上都重拉。
     *
     * 放在 filesDir 而不是 cacheDir：cacheDir 可能被系统在空间紧张时清掉，
     * 而这份缓存是我们主动要长期保留的（用户期望"接着上次看"）。
     */
    private val cache = LocalCache(File(app.filesDir, "simpleagent-cache").apply { mkdirs() })

    // 启动时恢复的最后会话 id（连上后 refreshSessions 会尝试选中它）。
    // **必须声明在 init 之前**：Kotlin 的类体按声明顺序初始化，
    // init 里会用到它，放后面会先看到未初始化值。
    private var restoredSessionId: String = ""

    init {
        // 用户期望"打开 App 就接着用"：上次连着谁、正在看哪个会话，
        // 直接恢复，而不是每次都手点一遍连接再找会话。
        restoreLastSession()
    }

    /**
     * 启动时自动恢复上次的使用状态。
     *
     * 只在**上次连接成功过**（autoConnect=true）时才自动连：如果上次是用户主动
     * 断开的，就不该自作主张连回去。
     */
    private fun restoreLastSession() {
        // **先展示缓存**：不论接下来连不连得上，用户都该立刻看到上次的内容
        // （这正是"缓存"的意义）。连接成功后 refreshSessions / refreshTimeline
        // 会用真实数据覆盖它。
        val cachedSessions = cache.readSessions()
        if (cachedSessions.isNotEmpty()) {
            // 记住的会话仍在缓存里就用它，否则回到最后一个（最新的）
            val remembered = prefs.getString("activeSessionId", "").orEmpty()
            val active = when {
                remembered.isNotEmpty() && cachedSessions.any { it.id == remembered } -> remembered
                else -> cachedSessions.lastOrNull()?.id.orEmpty()
            }
            android.util.Log.d(
                "SimpleAgent",
                "restore：remembered=$remembered 缓存会话数=${cachedSessions.size} " +
                    "命中=${cachedSessions.any { it.id == remembered }} 选定=$active",
            )
            state.update { it.copy(sessions = cachedSessions, activeSessionId = active) }
            if (active.isNotEmpty()) {
                applyCachedTimeline(active)
            }
        }

        if (!prefs.getBoolean("autoConnect", false)) {
            return
        }
        val current = state.value
        if (current.addr.isEmpty()) {
            return
        }
        // 记住的会话 id：连上后由 refreshSessions 决定是否还能用它
        restoredSessionId = prefs.getString("activeSessionId", "") ?: ""
        state.update { it.copy(connecting = true, connectError = "") }
        // 指纹优先用 prefs 里确认过的（state 初始值来自同一处，这里兜一层
        // 防止将来 state 初始化被改动时悄悄退化成"每次启动都重新确认"）。
        var fingerprint = current.fingerprint.trim()
        if (fingerprint.isEmpty()) {
            val savedAddr = prefs.getString("savedAddr", "") ?: ""
            if (savedAddr == current.addr.trim()) {
                fingerprint = prefs.getString("fingerprint", "") ?: ""
            }
        }
        core.connectTLS(
            current.addr.trim(),
            current.route.trim(),
            current.token.trim(),
            fingerprint,
        )
    }

    // ---- 事件 ----

    private fun onCoreEvent(event: CoreEvent) {
        when (event) {
            is CoreEvent.Connected -> {
                if (event.error.isNotEmpty()) {
                    // 连接失败必须把 connected 也置回 false：
                    // 否则"先连上、再重连失败"时会残留 connected=true，
                    // 界面停在聊天页但实际没有连接（也回不到连接页）。
                    //
                    // reconnecting 也要一并清掉：连接已经彻底失败，不该继续显示
                    // "正在重连…"的转圈横幅（它会因为没人再发 connection 事件而
                    // 永久卡住）。回连接页 + 显示失败原因才是正确的收场。
                    state.update {
                        it.copy(
                            connecting = false,
                            connected = false,
                            reconnecting = false,
                            connectError = event.error,
                            sessions = emptyList(),
                            activeSessionId = "",
                            timeline = TimelineView(),
                        )
                    }
                    return
                }
                state.update {
                    it.copy(
                        connecting = false,
                        connected = true,
                        reconnecting = false,
                        hasConnectedOnce = true,
                        connectError = "",
                    )
                }
                // 记住"上次连接成功过"：下次启动 App 自动连回去。
                // 只在成功时记，连接失败不算（否则启动时会自动连一个连不上的地址）。
                prefs.edit().putBoolean("autoConnect", true).apply()
                refreshAll()
            }
            is CoreEvent.Changed -> when (event.kind) {
                "sessions" -> refreshSessions()
                "providers" -> refreshProviders()
                "connection" -> refreshConnection()
                // **有对端证书待确认**：Go 侧在 TLS 握手中登记了一条。
                //
                // 这是主路径 —— 比轮询可靠得多：Go 一登记就通知，
                // 不依赖 Kotlin 定时任务有没有活着（踩过：轮询没跑，
                // 界面永远转圈而弹窗从不出现）。
                "peerConfirm" -> pullPendingPeer()
                "cmdResult" -> {
                    // 命令结果：统一刷新（草稿还原之类由界面按提示处理）
                    refreshTimeline()
                    refreshSessions()
                }
                // 流式输出时这几个事件来得极密（每个 token 一次）。
                // 每次都重建整条时间线 + 传整份 JSON 会直接卡死，所以节流。
                "conversation", "runtime", "notice" -> scheduleTimelineRefresh()
            }
            is CoreEvent.ProtocolError -> state.update { it.copy(toast = event.message) }
        }
    }

    // ---- 刷新（读快照，不推断）----

    private fun refreshAll() {
        refreshSessions()
        refreshProviders()
        refreshExecutors()
        refreshTimeline()
    }

    /**
     * 拉取 relay 上在线的执行端列表。
     *
     * 连上 relay 后界面用它渲染"切换执行端"；只有一台时也照常显示（让用户知道连的是谁）。
     */
    private fun refreshExecutors() {
        val list = core.executors()
        val hostId = core.currentHostId()
        state.update { it.copy(executors = list, currentHostId = hostId) }
    }

    /** 切换到指定执行端（沿用上次连接的地址与凭据）。 */
    fun switchHost(hostId: String) {
        if (hostId == state.value.currentHostId) return
        state.update { it.copy(connecting = true, connectError = "") }
        core.switchHost(hostId)
    }

    /**
     * applyCachedTimeline 用本地缓存填充界面（只读，不连后端）。
     *
     * 校验规则见 [LocalCache.isCompatible]：拿执行端刚给的游标比。这里只用
     * 缓存做**首屏占位** —— 随后的 refreshTimeline 会用增量覆盖它，
     * 所以即使缓存稍旧也不会让人看到错误内容（回合一结束就被替换）。
     */
    private fun applyCachedTimeline(sessionId: String) {
        // **已经有内容就直接返回**：缓存只是首屏占位，不该反复覆盖。
        //
        // 这条防的是一个真实 bug：启动时 restoreLastSession 会用缓存填一次
        // （未连接，无条件信任），紧接着 streamConnected 又调到这里 ——
        // 那时还没连上、core.syncCursor() 返回空游标，isCompatible 判 false，
        // 于是**刚填好的缓存被删掉**，界面变回空白（201ms 后的事）。
        val existing = state.value.timeline
        if (existing.items.isNotEmpty() && existing.sessionId == sessionId) {
            return
        }
        val cached = cache.readTimeline(sessionId) ?: return
        // **判断依据是"拿不拿得到游标"，不是 connected 标志位**。
        //
        // connected 可能已经是 true，但此刻镜像还没同步完（syncCursor 返回
        // 空 epoch）。那种情况下判"不兼容"并把缓存删掉，就又变空白了。
        // 空游标只说明"现在没法判断"，不等于"缓存坏了" —— 先信它。
        val cursor = core.syncCursor(sessionId)
        if (cursor.epoch.isEmpty()) {
            android.util.Log.d("SimpleAgent", "applyCached：游标还没就绪，先用缓存")
            applyCachedIntoState(cached)
            return
        }
        if (!cache.isCompatible(cached, cursor.epoch, cursor.generation)) {
            // 执行端重启过、或会话被压缩：缓存必须整体作废，
            // 否则会按错误基准续，出现错位或漏消息。
            android.util.Log.d(
                "SimpleAgent",
                "applyCached：不兼容，丢弃（cached=${cached.sync} cursor=$cursor）",
            )
            cache.dropTimeline(sessionId)
            return
        }
        applyCachedIntoState(cached)
    }

    /** applyCachedIntoState 把缓存内容填进界面状态。 */
    private fun applyCachedIntoState(cached: TimelineView) {
        state.update {
            it.copy(
                timeline = it.timeline.copy(
                    sessionId = cached.sessionId,
                    title = cached.title,
                    items = cached.items,
                    todos = cached.todos,
                    asks = cached.asks,
                ),
                asks = cached.asks,
            )
        }
        android.util.Log.d(
            "SimpleAgent",
            "applyCachedTimeline：填入 ${cached.items.size} 条 session=${cached.sessionId} " +
                "active=${state.value.activeSessionId}",
        )
    }

    /**
     * persistTimeline 把当前时间线写进缓存（供下次启动秒开）。
     *
     * 只在**空闲**时写：流式输出期间每 60ms 就要写一次文件，既浪费又没
     * 必要 —— 一轮结束时的内容才是用户下次真正想看到的。
     */
    private fun persistTimeline() {
        val view = state.value.timeline
        if (view.sessionId.isEmpty() || view.items.isEmpty()) return
        if (view.runtime.busy != "idle") return
        // **游标必须单独取**：界面用的是分页接口（timelinePage），它不带 sync
        // （每页都背一份没必要）。不补这一步，写进缓存的 sync 就是 null，
        // 下次读回来 isCompatible 直接判 false → 缓存被删 → 永远是空的
        // （这就是"重新打开还是空白"的根因）。
        val cursor = core.syncCursor(view.sessionId)
        cache.writeTimeline(view.copy(sync = cursor))
    }

    private fun refreshSessions() {
        val sessions = core.sessions()
        // 会话列表与项目分组**只缓存一份**（整表覆盖）：它是当前状态，
        // 不是历史，增量没有意义。
        if (sessions.isNotEmpty()) {
            cache.writeSessions(sessions)
        }
        state.update { current ->
            // 选哪个会话的顺序：
            // 1. 当前已选的（用户正在看，别打断）
            // 2. 上次退出时正在看的（重启 App 后接着用）
            // 3. 最后一个（最新的会话）
            val active = when {
                current.activeSessionId.isNotEmpty() -> current.activeSessionId
                restoredSessionId.isNotEmpty() && sessions.any { it.id == restoredSessionId } ->
                    restoredSessionId
                else -> sessions.lastOrNull()?.id.orEmpty()
            }
            // 恢复只做一次：之后用户自己切走就不该再被拉回去
            if (restoredSessionId.isNotEmpty() && sessions.any { it.id == restoredSessionId }) {
                restoredSessionId = ""
            }
            current.copy(sessions = sessions, activeSessionId = active)
        }
    }

    private fun refreshProviders() {
        state.update { it.copy(providers = core.providers()) }
    }

    /**
     * 主动向 Go 侧核对一次连接状态。
     *
     * 界面在**回到前台**时调用：App 在后台期间 Go 侧可能已经重连成功了，
     * 而 "connection" 事件在后台被合并/丢弃，界面就会一直挂着
     * "连接已断开，正在重连…" 的横幅。回到前台主动对一次账最省事。
     */
    fun syncConnection() = refreshConnection()

    private fun refreshConnection() {
        // 断线/重连只靠 timeline 是看不出来的：timeline 的 runtime.connected
        // 反映的是"会话状态"，而连接本身是否在线要问 Go 侧。
        // 以前这里只 refreshTimeline()，导致断线后 connected 永远停在 true，
        // 界面显示"已连接"但实际断了，用户只能手动重连。
        //
        // **不要用 `if (it.connected == online) it` 早退**：reconnecting 是该写的
        // 另一个字段，早退会让"connected 已被别处改对、reconnecting 还停在 true"
        // 的组合永远修不回来 —— 表现就是顶部"连接已断开，正在重连…"横幅不消失。
        //
        // reconnecting 只表示"曾经连上过、这一跳掉了"：
        // 从没连上过（初次连接失败/用户主动断开）时它是 false，界面该回连接页
        // 报错，而不是显示一个永远转圈的重连横幅。
        val online = core.connected()
        var wasOnline = false
        state.update { current ->
            wasOnline = current.connected
            // hasConnectedOnce 在一次成功连接后被置为 true，disconnect() 时清掉
            val reconnecting = !online && current.hasConnectedOnce
            if (current.connected == online && current.reconnecting == reconnecting) {
                current
            } else {
                current.copy(connected = online, reconnecting = reconnecting)
            }
        }
        // **刚连上就做一次完整同步**：会话列表 + 当前会话的时间线。
        //
        // 这一步的意义是把"缓存/上一轮的内容"换成执行端的真实状态。
        // 期间置 syncing=true，界面据此禁止发送（见 send 的检查）——
        // 否则用户可能在同步完成前对着旧内容说话。
        //
        // 只在这一跳从"未连接"变成"已连接"时触发：每帧都同步会反复拉数据，
        // 把流式刷新也堵住。
        if (online && !wasOnline) {
            runFullSync()
        } else if (online) {
            // **未连接时不要刷新时间线**：core.timelinePage 拿不到数据、返回空页，
            // 而 refreshTimeline 会拿这个空结果去覆盖 state —— 刚才从缓存
            // 填进去的 25 条瞬间被清成 0，界面变空白。
            //
            // 这正是"重新打开还是空白"的最后一环：缓存读对了、也填进去了，
            // 但紧接着被一个空刷新覆盖掉。未连接时保持现状（显示缓存）。
            refreshTimeline()
        }
    }

    /**
     * runFullSync 做一次完整同步：会话列表 → 当前会话时间线 → 解除 syncing。
     *
     * 顺序不能反：会话列表决定了"当前是哪个会话"，先拉时间线可能拉的是
     * 已经被删掉的会话。
     */
    private fun runFullSync() {
        state.update { it.copy(syncing = true) }
        try {
            refreshSessions()
            refreshTimeline()
        } finally {
            // 无论成功失败都要解除：卡在 syncing=true 会让用户永远发不出消息。
            state.update { it.copy(syncing = false) }
        }
    }

    /**
     * 节流版刷新：流式输出时 `conversation` 事件每个 token 来一次，
     * 每次都重建整条时间线（长会话要序列化上百 KB）会直接卡住界面。
     *
     * 策略：最多每 [timelineRefreshIntervalMs] 刷一次，且**保证最后一次一定执行**
     * （用 pending 标记 + 尾部补刷），否则流式结束后界面会停在中间状态。
     */
    private fun scheduleTimelineRefresh() {
        // **用时间戳节流，不依赖 job 的 isActive**。
        //
        // 之前用 `timelineRefreshJob?.isActive == true` 判断：如果 refreshTimeline
        // 执行很快（几十毫秒），job 在下一次事件到来时已经结束，isActive 为 false，
        // 于是每次都新建 job —— 节流完全失效。实测 772ms 里刷了 27 次，
        // 每次都在做同样的工作，白白占用 UI 线程。
        val now = System.currentTimeMillis()
        if (now - lastTimelineRefreshAt >= timelineRefreshIntervalMs) {
            lastTimelineRefreshAt = now
            refreshTimeline()
            timelineRefreshPending = false
            return
        }
        // 还在窗口内：记下"有新数据"，窗口结束时补刷一次，保证最终状态不丢
        timelineRefreshPending = true
        if (timelineRefreshJob?.isActive == true) return
        timelineRefreshJob = viewModelScope.launch {
            kotlinx.coroutines.delay(timelineRefreshIntervalMs)
            if (timelineRefreshPending) {
                timelineRefreshPending = false
                lastTimelineRefreshAt = System.currentTimeMillis()
                refreshTimeline()
            }
        }
    }

    private fun refreshTimeline() {
        val id = state.value.activeSessionId
        if (id.isEmpty()) return
        // **先用缓存顶上**：连接刚建立、或还没连上时，界面不该是空白。
        //
        // 只在"当前没有内容"时套用：已经有实时内容时再用缓存覆盖会把
        // 用户正在看的东西换掉。
        //
        // **未连接时不要套**：那会把刚恢复出来的缓存又覆盖成空
        // （refreshTimeline 在未连接时拿到的就是空页）。
        if (state.value.timeline.items.isEmpty() && state.value.connected) {
            applyCachedTimeline(id)
        }
        // **加载更早内容的期间跳过刷新**。
        //
        // refreshTimeline 由流式输出高频触发（几十毫秒一次），它会用
        // "最后一页 + 旧列表里更早的部分"重建整体列表。如果正好和
        // loadEarlierItems 的插入并发，后者的结果会被前者基于旧快照覆盖 ——
        // 表现为：加载成功但 items 条数不变，hasMore 仍是 true，
        // 于是界面在顶部反复触发加载，陷入死循环（真机日志里 pages 一直涨、
        // items 一直是 60 就是这样）。
        //
        // 跳过是安全的：加载结束后会有新的刷新把最新内容带回来。
        if (state.value.loadingEarlier) return
        // 分页取最后一页：完整 timeline 在长会话里是几百 KB（实测 774KB），
        // 全量传过来再反序列化就是打开会话时那几秒白屏。
        //
        // 流式输出期间取最后一页是安全的：新内容总在末尾，不会因为分页而丢。
        val page = core.timelinePage(id, 0L, timelinePageSize)
        val previous = state.value.timeline
        // **保留已经翻页加载过的早期 items**：这次只拿回最后 60 条，
        // 如果直接用新结果替换整个列表，用户向上翻出来的历史会全部消失。
        //
        // 用 seq 判定比找 key 更可靠：本页覆盖 [seqFrom, seqTo]，
        // 旧列表里"不在这个区间内、且 seq 更小"的就是早前翻出来的部分。
        val keptEarlier = earlierPartBySeq(previous.items, page)
        // 合并时按 key 去重：keptEarlier 与 page.items 在补拉/边界处可能
        // 有重叠条目，不去重的话列表里会出现两条一样的 key ——
        // LazyColumn 对重复 key 的行为未定义，会导致条目错位、数量抖动。
        val merged = mergeProcessDetails(dedupeByKey(keptEarlier + page.items), previous.items)

        // **空页不能覆盖同一会话已有的内容**（最后一道保险）。
        //
        // 空页有两种来源：这个会话本来就空，或"现在拿不到数据"
        // （未连接、镜像还没同步完）。后者用空结果覆盖，就会把刚从缓存
        // 恢复出来的内容清掉 —— 界面表现为"打开就空白"，而缓存其实好着。
        //
        // **但必须区分是不是同一个会话**：切换会话后 previous 里是
        // 上一个会话的残留内容，此时"保留"就大错特错 —— 界面会一直显示
        // 别的会话的消息，直到这个会话真的加载出来（真机上切到长会话
        // "很久都没内容，显示的还是刚才那个"就是这么来的）。
        // 换会话时宁可先显示空白 + 加载中，也不能张冠李戴。
        val sameSession = previous.sessionId == page.sessionId && page.sessionId.isNotEmpty()
        if (merged.isEmpty() && previous.items.isNotEmpty() && sameSession) {
            android.util.Log.d(
                "SimpleAgent",
                "refreshTimeline：空页，保留已有的 ${previous.items.size} 条",
            )
            return
        }

        state.update {
            // 分页游标：只要用户往上翻过，就保持用户翻到的位置不动。
            // 否则每次流式刷新都会把游标重置回最后一页——表现就是"翻一点就弹回去"。
            val paginated = it.earlierPagesLoaded > 0
            // **不要在这里改 connected**：连接状态的唯一权威来源是
            // refreshConnection()（它读 core.connected()，并能同步维护
            // reconnecting）。以前这里也从 page.runtime.connected 反写，
            // 形成第二个写入者且不同步 reconnecting，
            // 结果是 connected=true 与 reconnecting=true 并存，
            // 顶部"正在重连"横幅再也不消失。
            it.copy(
                timeline = TimelineView(
                    sessionId = page.sessionId,
                    title = page.title ?: it.timeline.title,
                    items = merged,
                    todos = page.todos ?: it.timeline.todos,
                    asks = page.asks ?: it.timeline.asks,
                    runtime = page.runtime ?: it.timeline.runtime,
                ),
                asks = page.asks ?: it.timeline.asks,
                hasMoreItems = if (paginated) it.hasMoreItems else page.hasMore,
                firstSeq = if (paginated) it.firstSeq else page.seqFrom,
            )
        }
        android.util.Log.d(
            "SimpleAgent",
            "refreshTimeline：items=${merged.size} hasMore=${page.hasMore} " +
                "seq=${page.seqFrom}..${page.seqTo} earlierPart=${keptEarlier.size} " +
                "pages=${state.value.earlierPagesLoaded} prevItems=${previous.items.size}",
        )
        // 写缓存：只在空闲时真正落盘（见 persistTimeline 的注释）
        persistTimeline()
    }

    /**
     * 从旧列表里取出"本页之前"的那部分（即之前翻页加载的历史）。
     *
     * 判定方式：用本页的 seq 区间做锚。旧列表里第一条 key 与 page.items 首条
     * **相同**的位置之前的内容，就是早前翻出来的部分。
     *
     * 为什么不用 seq 直接比：ItemView 里不带 seq（seq 在 Go 侧的 ChatEntry 上）。
     * 但 key 在这里是安全的锚——同一条消息的 key 在**未流式**时是稳定的，
     * 而历史页里的消息早已 finalize，不会被流式改写。
     */
    /**
     * 按 key 去重，保留**先出现的**那条。
     *
     * 翻页/补拉合并时，旧列表与 Go 返回的新页在边界处会有重叠条目。
     * 不去重会让 LazyColumn 里出现重复 key —— 它的行为未定义，
     * 表现为条目错位、列表数量抖动，滚动位置跟着乱跳。
     */
    private fun dedupeByKey(items: List<ItemView>): List<ItemView> {
        if (items.size < 2) return items
        val seen = HashSet<String>(items.size)
        val out = ArrayList<ItemView>(items.size)
        for (item in items) {
            if (!seen.add(item.key)) continue
            out.add(item)
        }
        return out
    }

    private fun earlierPartBySeq(old: List<ItemView>, page: TimelinePage): List<ItemView> {
        if (old.isEmpty() || page.items.isEmpty()) return emptyList()

        // **判据：新页覆盖了哪些 key，旧列表里在第一个被覆盖的 key 之前的部分
        // 就是"只有旧列表才有"的更早内容。**
        //
        // 不再用 indexOfFirst 找锚点后 subList —— 那个写法在"新页第一条
        // 恰好也在旧列表中间"时会算错锚点位置，把本该保留的历史丢掉
        // （真机日志：earlierPart 从 25 掉到 4，items 从 87 掉回 64）。
        //
        // 这里遍历旧列表（它是从旧到新排的），一旦遇到"新页也有"的 key
        // 就停 —— 那之前的都是更早的内容。
        val freshKeys = HashSet<String>(page.items.size)
        for (item in page.items) freshKeys.add(item.key)

        val kept = ArrayList<ItemView>()
        for (item in old) {
            if (item.key in freshKeys) break
            kept.add(item)
        }
        return kept
    }

    /**
     * 把旧 items 里**已经加载过详情**的过程组内容补进新 items。
     *
     * Go 侧折叠的过程组不带 steps；用户点开时我们单独拉过一次。刷新回来后
     * 如果直接用新 items，那份详情就没了 —— 表现为"展开的内容自己收起来了"。
     */
    private fun mergeProcessDetails(fresh: List<ItemView>, old: List<ItemView>): List<ItemView> {
        if (fresh.isEmpty() || old.isEmpty()) return fresh
        val loaded = HashMap<String, ProcessView>()
        for (item in old) {
            val process = item.process ?: continue
            if (process.detail) loaded[process.key] = process
        }
        if (loaded.isEmpty()) return fresh
        return fresh.map { item ->
            val process = item.process ?: return@map item
            val cached = loaded[process.key] ?: return@map item
            // 正在流式的组用新数据（内容还在增长），已结束的用缓存的详情
            if (process.streaming) return@map item
            item.copy(process = process.copy(steps = cached.steps, detail = true))
        }
    }

    // ---- 用户操作 ----

    fun updateAddr(value: String) = state.update { it.copy(addr = value) }
    fun updateRoute(value: String) = state.update { it.copy(route = value) }
    fun updateToken(value: String) = state.update { it.copy(token = value) }
    fun updateFingerprint(value: String) = state.update { it.copy(fingerprint = value) }

    fun connect() {
        // 用户手动发起连接：把上一次遗留的重连横幅清掉。
        // 这是"我动手了"的明确信号，界面应该显示 connecting，而不是同时
        // 显示"正在重连…"——两个转圈会让人以为卡住了。
        state.update {
            it.copy(connecting = true, reconnecting = false, connectError = "")
        }
        val current = state.value
        // 记住连接参数：手机输入很麻烦。
        //
        // **指纹不再存这里** —— 改由 Go 侧的信任列表管理
        // （`<cacheDir>/known_hosts`，与桌面端同一份格式）。
        // 以前在 SharedPreferences 里存一份，两边各记各的，
        // 容易出现"桌面端认得了、手机还问一遍"这种不一致。
        prefs.edit()
            .putString("addr", current.addr.trim())
            .putString("route", current.route.trim())
            .putString("token", current.token.trim())
            .apply()

        // **指纹确认交给 bridge**：连上过程中如果需要用户确认对端证书，
        // bridge 会在握手回调里登记一条待确认项并阻塞那条连接，
        // 界面通过 refreshPendingPeer() 拿到并弹窗。
        //
        // 不再"先探测、再弹窗、再连接"：那套流程要把同一件事做两遍
        // （探测一次握手、连接又一次），而且探测结果和真正握手之间
        // 有机会窗口 —— 中间被换掉也看不出来。
        core.connectTLS(
            current.addr.trim(),
            current.route.trim(),
            current.token.trim(),
            // 用户手填过指纹就带上（严格校验）；否则留空走 TOFU
            current.fingerprint.trim(),
        )
        // 连接是异步的：轮询看看有没有待确认的对端要弹窗
        pollPendingPeer()
    }

    /**
     * pollPendingPeer 轮询 bridge 的待确认对端，有就弹窗。
     *
     * 为什么用轮询而不是事件回调：指纹确认发生在 Go 的 TLS 握手 goroutine
     * 里，而 Kotlin 侧的弹窗必须在主线程。bridge 已经有 emit 通知机制，
     * 但那条路径是给高频事件合帧用的；这里改动最小、最不容易出错的做法
     * 是短轮询 —— 只有在"连接中"才轮询，连上就停。
     */
    private fun pollPendingPeer() {
        pollJob?.cancel()
        pollJob = viewModelScope.launch {
            // 用 for 而不是 repeat(...)：`continue` 只在真循环里可用，
            // repeat 的 lambda 里写 continue 会编译失败（踩过）。
            for (attempt in 0 until 120) { // 最多等 60 秒（用户可能要想一会儿）
                delay(500)
                pullPendingPeer()
                // **连上且没有待确认项就停**：主路径是 peerConfirm 事件，
                // 轮询只是兜底。连上之后还一直问就是白耗电（每 500ms 一次 JNI）。
                if (state.value.connected && state.value.pendingPeer == null) {
                    return@launch
                }
            }
        }
    }

    /**
     * pullPendingPeer 立刻问一次 Go 侧"有没有对端要确认"，有就弹窗。
     *
     * 两条路径都会调它：
     *  1. `peerConfirm` 事件（Go 主动推，**主路径**）
     *  2. 轮询（兜底：事件丢了/漏了时仍能弹出来）
     *
     * 它本身很快（只读内存里的队列），不需要放后台线程。
     */
    private fun pullPendingPeer() {
        val peer = core.pendingPeer()
        if (peer.isEmpty()) return
        val parts = peer.split("\n")
        if (parts.size < 4) return
        // 已经在问同一个了就别重复弹窗
        if (state.value.pendingPeer?.addr == parts[0]) return
        state.update {
            it.copy(
                pendingPeer = PendingPeer(
                    addr = parts[0],
                    fingerprint = parts[1],
                    changed = parts[2] == "1",
                    known = parts[3],
                ),
            )
        }
    }


    /**
     * importRelayConfig 从 JSON 文件导入 relay 配置。
     *
     * 认的格式就是执行端存的那份 relay.json：
     *
     *	{"addr":"129.211.171.158:7900","token":"…","fingerprint":"…"}
     *
     * 好处：手机上不用手输地址、token 和 64 位指纹 —— 把执行端那份文件
     * 传到手机上一选即可。字段都宽松处理（缺 token/指纹也能导入，
     * 指纹缺了就按 TOFU 走）。
     */
    fun importRelayConfig(text: String) {
        val config = try {
            val obj = kotlinx.serialization.json.Json.parseToJsonElement(text)
                as kotlinx.serialization.json.JsonObject
            fun str(key: String): String {
                val value = obj[key] as? kotlinx.serialization.json.JsonPrimitive ?: return ""
                return if (value is kotlinx.serialization.json.JsonNull) "" else value.content
            }
            Triple(str("addr"), str("token"), str("fingerprint"))
        } catch (e: Throwable) {
            state.update { it.copy(connectError = "配置不是合法 JSON：${e.message}") }
            return
        }
        val (addr, token, fingerprint) = config
        if (addr.isBlank()) {
            state.update { it.copy(connectError = "配置里没有 addr 字段") }
            return
        }
        state.update {
            it.copy(
                addr = addr,
                token = token,
                fingerprint = fingerprint,
                connectError = "",
                toast = "已导入配置：$addr",
            )
        }
    }

    /** reportImportError 报告导入失败（读不了文件等）。 */
    fun reportImportError(message: String) {
        state.update { it.copy(connectError = message) }
    }

    /**
     * 用户确认了指纹：告诉 bridge 放行，那条被挂起的连接会继续握手。
     *
     * **不需要重新连接** —— 这正是与旧实现的关键区别。以前是"探测→弹窗→
     * 用指纹重连"，现在是"连接过程中问一句，用户答了连接自己就建好了"。
     */
    fun confirmPeer() {
        val pending = state.value.pendingPeer ?: return
        state.update { it.copy(pendingPeer = null) }
        core.resolvePeer(pending.addr, true)
        // 继续轮询：可能还有别的对端要确认（比如再直连执行端时）
        pollPendingPeer()
    }

    /** 用户拒绝了指纹：那条连接握手失败，不会建立。 */
    fun dismissPeer() {
        val pending = state.value.pendingPeer ?: return
        state.update { it.copy(pendingPeer = null) }
        core.resolvePeer(pending.addr, false)
    }

    fun disconnect() {
        core.disconnect()
        // 用户主动断开 = 明确表示"不要连着"：清掉自动重连标记，
        // 否则下次启动 App 又会自己连回去，违背用户意图。
        prefs.edit().putBoolean("autoConnect", false).apply()
        // hasConnectedOnce 一并清掉：这是主动断开，不是"掉线重连"，
        // 不该再被当成异常状态去显示重连横幅。
        state.update {
            it.copy(
                connected = false,
                reconnecting = false,
                hasConnectedOnce = false,
                sessions = emptyList(),
                activeSessionId = "",
                timeline = TimelineView(),
            )
        }
    }

    /**
     * 放开"加载更早"的互斥标志、清掉"游标已拉过"的记录（切换会话时调用）。
     *
     * 上一个会话的请求可能还在飞（它的结果会被 activeSessionId 检查丢掉），
     * 标志不释放的话新会话就再也加载不了更早内容；"已拉过"的游标也会误挡
     * 新会话的第一页。
     */
    private fun resetPagingGuards() {
        earlierLoading.set(false)
        lastLoadedCursor = -1
    }

    fun selectSession(id: String) {
        if (id == state.value.activeSessionId) return
        // 切换会话：把"加载更早"的互斥标志也放开 —— 上一个会话的请求
        // 可能还在飞（它的结果会被 activeSessionId 检查丢掉），
        // 但标志不释放的话新会话就再也加载不了更早内容了。
        resetPagingGuards()
        // 分页状态必须一起清：否则新会话会带着上一个会话的 firstSeq 去翻页
        state.update { it.switchedTo(id) }
        // 记住正在看的会话：重启 App 后直接回到这里
        prefs.edit().putString("activeSessionId", id).apply()
        refreshTimeline()
    }

    /**
     * 新建会话。
     *
     * workspace 决定新会话归到哪个项目：
     *  - 传具体路径 → 执行端把它登记到（或新建）那个项目下；
     *  - 传空串   → 落到执行端的默认工作区。
     *
     * 命令是异步的：新会话的 id 经 cmdResult 回到 refreshSessions，
     * 这里用"会话数量变多"来选中最后一个（与桌面一致：新会话排在末尾）。
     */
    fun newSession(workspace: String = "") {
        val before = state.value.sessions.map { it.id }.toSet()
        val error = core.newSession(workspace, "")
        if (error.isNotEmpty()) {
            state.update { it.copy(toast = error) }
            return
        }
        // 稍等一轮事件循环再拉列表：命令要先在执行端跑
        viewModelScope.launch {
            kotlinx.coroutines.delay(300)
            val sessions = core.sessions()
            val created = sessions.lastOrNull { it.id !in before } ?: sessions.lastOrNull()
            if (created != null && created.id != state.value.activeSessionId) resetPagingGuards()
            state.update { current ->
                val target = created?.id ?: current.activeSessionId
                if (target == current.activeSessionId) current.copy(sessions = sessions)
                else current.switchedTo(target).copy(sessions = sessions)
            }
            refreshTimeline()
        }
    }

    /**
     * 调试用：一键"自检发送"。
     *
     * 做的事：没有活动会话就自动新建一个，然后发一条带时间戳的消息。
     * 目的有两个：
     *  1. 绕开手机输入法（中文/候选词经常把自动化的 input text 吞掉）；
     *  2. 一键验证整条链路（App → relay → 执行端 → 模型 → 回来）。
     *
     * 界面上只在 Debug 构建显示（不污染正式版）。
     */
    fun debugSend() {
        val current = state.value
        if (!current.connected) {
            state.update { it.copy(toast = "还没连接：先点「连接」") }
            return
        }
        val text = "debug-" + android.text.format.DateFormat.format(
            "HH:mm:ss", System.currentTimeMillis(),
        ).toString()

        if (current.activeSessionId.isNotEmpty()) {
            send(text)
            state.update { it.copy(toast = "调试发送：$text") }
            return
        }
        // 没有会话：先建一个，等它出现在列表里再发
        val before = current.sessions.map { it.id }.toSet()
        val error = core.newSession("", "")
        if (error.isNotEmpty()) {
            state.update { it.copy(toast = error) }
            return
        }
        viewModelScope.launch {
            kotlinx.coroutines.delay(400)
            val sessions = core.sessions()
            val created = sessions.lastOrNull { it.id !in before } ?: sessions.lastOrNull()
            if (created == null) {
                state.update { it.copy(toast = "调试发送失败：没有建出会话") }
                return@launch
            }
            resetPagingGuards()
            state.update { it.switchedTo(created.id).copy(sessions = sessions) }
            refreshTimeline()
            send(text)
            state.update { it.copy(toast = "调试发送：$text") }
        }
    }

    /** 发送消息：忙时自动转插话（与桌面一致）。 */
    fun send(text: String) {
        val current = state.value
        val id = current.activeSessionId
        if (id.isEmpty()) {
            // 以前这里直接 return，界面毫无反应 —— 用户只会以为"点了没反应"。
            // 现在明确提示：没有活动会话时应该先「新建」。
            state.update {
                it.copy(toast = "没有活动会话：请先在左侧菜单点「新建」")
            }
            return
        }
        if (text.isBlank() && current.pendingAttachments.isEmpty()) return

        // **同步中不允许发消息**（用户要求）。
        //
        // 同步期间界面上的内容可能还是缓存里的旧版本，此时发消息容易让人
        // 以为"基于当前看到的内容在说话"，而实际上执行端的状态已经变了。
        // 明确拦住并说明原因，比默默排队更清楚。
        if (current.syncing) {
            state.update {
                it.copy(toast = "正在同步，请稍候再发送")
            }
            return
        }

        // 有待发附件时走"带附件发送"：执行端在忙时会拒收并提示
        // 「插话暂不支持附件」，客户端不自己拆成两步。
        if (current.pendingAttachments.isNotEmpty()) {
            val attachments = buildString {
                append('[')
                current.pendingAttachments.forEachIndexed { index, item ->
                    if (index > 0) append(',')
                    append("{\"hash\":\"").append(item.hash).append("\"")
                    append(",\"media_type\":\"").append(item.mediaType).append("\"")
                    append(",\"name\":\"").append(item.name.replace("\"", "")).append("\"}")
                }
                append(']')
            }
            val error = core.sendWithAttachments(id, text, attachments)
            if (error.isNotEmpty()) {
                state.update { it.copy(toast = error) }
            } else {
                // 附件已经交出去了：清空待发列表
                state.update { it.copy(pendingAttachments = emptyList()) }
            }
            refreshTimeline()
            return
        }

        // 忙时才排队（插话）；空闲时一律走 send。
        //
        // 不能只看 canSend：它是**上一次拉取的状态**，新会话刚建好时还没更新，
        // 会是 false —— 于是走了 enqueue，而空闲会话不能插话，
        // 后端报"当前状态不支持该操作"，用户看到的就是"点发送没反应"。
        // 用 busy 兜一层：只要不是生成中/跑工具，就当普通消息发。
        val busy = current.timeline.runtime.busy
        val streaming = busy == "streaming" || busy == "running_tools" || busy == "compacting"
        val error = if (current.timeline.runtime.canSend || !streaming) {
            core.sendText(id, text)
        } else {
            core.enqueue(id, text)
        }
        if (error.isNotEmpty()) {
            state.update { it.copy(toast = error) }
        }
        refreshTimeline()
    }

    // ---- 提问面板 ----

    /** 切换某题的一个选项（选择状态在 Go 里维护）。 */
    fun toggleAskOption(callId: String, questionIndex: Int, optionIndex: Int) {
        val id = state.value.activeSessionId
        if (id.isEmpty()) return
        val json = core.toggleAskOption(id, callId, questionIndex, optionIndex)
        applyAsks(json)
    }

    /** 记录某题的手写答案。 */
    fun setAskCustom(callId: String, questionIndex: Int, text: String) {
        val id = state.value.activeSessionId
        if (id.isEmpty()) return
        val json = core.setAskCustom(id, callId, questionIndex, text)
        applyAsks(json)
    }

    private fun applyAsks(json: String) {
        if (json.isEmpty()) return
        state.update { it.copy(asks = core.decodeAsks(json)) }
    }

    fun selectAskTab(index: Int) = state.update { it.copy(askTab = index) }

    fun submitAsks() {
        val id = state.value.activeSessionId
        if (id.isEmpty()) return
        val error = core.submitAsks(id)
        if (error.isNotEmpty()) {
            state.update { it.copy(toast = error) }
            return
        }
        state.update { it.copy(asks = emptyList(), askTab = 0) }
        refreshTimeline()
    }

    fun dismissAsks() {
        val id = state.value.activeSessionId
        if (id.isEmpty()) return
        val error = core.dismissAsks(id)
        if (error.isNotEmpty()) state.update { it.copy(toast = error) }
        state.update { it.copy(asks = emptyList(), askTab = 0) }
        refreshTimeline()
    }

    // ---- 附件上传 ----

    /**
     * 上传一张图片，成功后加入待发附件列表。
     *
     * 图片必须落在**执行端**（模型要读它、其它展示端也要按 hash 拉同一张图），
     * 所以这里先上传、拿到 hash，再随消息一起发出去。
     */
    fun attachImage(bytes: ByteArray, name: String, mediaType: String) {
        if (bytes.isEmpty()) return
        state.update { it.copy(uploading = it.uploading + 1) }
        core.putBlobAsync(bytes) { hash ->
            state.update { current ->
                if (hash.isEmpty()) {
                    current.copy(
                        uploading = (current.uploading - 1).coerceAtLeast(0),
                        toast = "图片上传失败：" + core.lastError(),
                    )
                } else {
                    current.copy(
                        uploading = (current.uploading - 1).coerceAtLeast(0),
                        pendingAttachments = current.pendingAttachments +
                            PendingAttachment(hash = hash, name = name, mediaType = mediaType),
                    )
                }
            }
        }
    }

    fun removeAttachment(hash: String) {
        state.update { current ->
            current.copy(pendingAttachments = current.pendingAttachments.filterNot { it.hash == hash })
        }
    }

    // ---- 更早的历史 ----

    /**
     * 加载更早的 items（滚到顶部时触发）。
     *
     * 与 [loadOlder] 的区别：那个拉的是**归档**（压缩后的旧消息，只读）；
     * 这个拉的是**同一段会话里更早的 items**（分页尚未加载的部分）。
     *
     * 返回本次新插入的条数（界面用它做滚动锚定：如果不补偿，
     * 往头部插入内容会让用户正在看的位置突然跳走）。
     */
    fun loadEarlierItems() {
        // **互斥**：同一时刻只允许一个"加载更早"请求在飞。
        //
        // 光靠 state.loadingEarlier 判断不够 —— 它和随后的 state.update
        // 之间有窗口，滚动事件密集时（滑到顶会连续触发）会并发进两次，
        // 各自拿到同一份游标、各自插一批，结果内容重复且位置全乱。
        // 这里用 compareAndSet 做真正的原子占位。
        if (!earlierLoading.compareAndSet(false, true)) {
            android.util.Log.d("SimpleAgent", "loadEarlierItems 跳过：已有请求在飞")
            return
        }
        val current = state.value
        val id = current.activeSessionId
        if (id.isEmpty() || !current.hasMoreItems) {
            earlierLoading.set(false)
            android.util.Log.d(
                "SimpleAgent",
                "loadEarlierItems 跳过：id=${id.ifEmpty { "空" }} " +
                    "hasMore=${current.hasMoreItems} cursor=${current.firstSeq} " +
                    "items=${current.timeline.items.size}",
            )
            return
        }
        val cursor = current.firstSeq
        if (cursor <= 0) {
            earlierLoading.set(false)
            android.util.Log.d("SimpleAgent", "loadEarlierItems 跳过：游标为 0")
            return
        }
        // **同一个游标不重复请求**。
        //
        // 互斥只挡住"同时"的请求，挡不住"上一个刚完成、界面又立刻触发同样的
        // 游标"。那种情况下 Go 侧会返回和上次相同的一页，Kotlin 侧去重后
        // items 不增长，但 has_more 仍是 true —— 于是无限循环。
        //
        // 记录"这个游标已经拉过"，直到 firstSeq 真的推进了才允许再拉。
        if (cursor == lastLoadedCursor) {
            earlierLoading.set(false)
            android.util.Log.d(
                "SimpleAgent",
                "loadEarlierItems 跳过：游标 $cursor 已拉过，等它推进",
            )
            return
        }
        android.util.Log.d("SimpleAgent", "loadEarlierItems 开始：cursor=$cursor items=${current.timeline.items.size}")
        // **先置 loading 再发起请求**，且请求放在 IO 线程。
        //
        // core.timelinePage 是**同步阻塞**调用（Go 侧翻到镜像边界时会向执行端
        // 补拉更早事件，最长等 20 秒）。以前直接写在 viewModelScope.launch 里，
        // 而它默认跑在主线程 —— 于是主线程被阻塞，连"正在加载"的转圈都画不出来，
        // 界面看起来像卡死。真机上的表现就是"完全看不到在加载"。
        state.update { it.copy(loadingEarlier = true) }
        viewModelScope.launch {
            try {
                val page = withContext(Dispatchers.IO) {
                    core.timelinePage(id, cursor, timelinePageSize)
                }
                // 记下这个游标已经拉过：下次同一个游标直接跳过，
                // 避免"返回同一页 → items 不增长 → 无限重复"。
                // 只有成功拿到内容时才记：失败的情况下次还要能重试同一游标。
                if (!page.retry && page.items.isNotEmpty()) {
                    lastLoadedCursor = cursor
                }
                state.update { st ->
                    if (st.activeSessionId != id) return@update st.copy(loadingEarlier = false)
                    if (page.items.isEmpty()) {
                        if (page.retry) {
                            // **没取到内容，但不是到底了**（断线 / 补拉失败）。
                            //
                            // 以前这里统一当成"已到最早"，把 hasMoreItems 置 false，
                            // 界面于是永久显示"已经是最早的内容了" —— 用户明明
                            // 还没翻到第一条。现在保持 hasMore=true、游标不动，
                            // 用户再滑一次就能重试。
                            return@update st.copy(loadingEarlier = false)
                        }
                        // 执行端明确说没有了：真的到最早
                        return@update st.copy(loadingEarlier = false, hasMoreItems = false)
                    }
                    // 插到最前面；已展开的组保留已加载的详情
                    val merged = mergeProcessDetails(
                        dedupeByKey(page.items + st.timeline.items), st.timeline.items,
                    )
                    st.copy(
                        loadingEarlier = false,
                        timeline = st.timeline.copy(items = merged),
                        hasMoreItems = page.hasMore,
                        // 游标推进到本页最早处；为 0 时保持原值（防御）
                        firstSeq = if (page.seqFrom > 0) page.seqFrom else st.firstSeq,
                        // 记录翻过页：之后刷新不再重置游标
                        earlierPagesLoaded = st.earlierPagesLoaded + 1,
                    )
                }
            } finally {
                // **必须释放**：任何异常/提前返回都不能让标志卡住，
                // 否则"加载更早"永久失效（界面一直显示正在加载）。
                earlierLoading.set(false)
            }
        }
    }

    /** 加载下一页归档（滚到顶部时触发）。 */
    fun loadOlder() {
        val current = state.value
        val id = current.activeSessionId
        if (id.isEmpty() || current.loadingOlder || !current.hasMoreHistory) return
        state.update { it.copy(loadingOlder = true) }
        core.loadOlderAsync(id, current.archiveCursor) { page ->
            state.update { st ->
                // 结果回来时已经切到别的会话：丢掉，否则会把 A 的归档插进 B 里
                if (st.activeSessionId != id) return@update st
                if (page.error.isNotEmpty()) {
                    // **失败不等于没有更多**：保持 hasMoreHistory，用户再滑一次就能重试。
                    // 以前这里置 false，网络抖一下归档就永久看不到了。
                    st.copy(loadingOlder = false, toast = page.error)
                } else {
                    st.copy(
                        loadingOlder = false,
                        archive = page.entries + st.archive,
                        archiveCursor = page.next,
                        hasMoreHistory = page.more && page.next.isNotEmpty(),
                    )
                }
            }
        }
    }

    fun stop() {
        val id = state.value.activeSessionId
        if (id.isEmpty()) return
        val error = core.stop(id)
        if (error.isNotEmpty()) state.update { it.copy(toast = error) }
        refreshTimeline()
    }

    fun dropQueued(index: Int) {
        val id = state.value.activeSessionId
        if (id.isEmpty()) return
        val error = core.dropQueued(id, index)
        if (error.isNotEmpty()) state.update { it.copy(toast = error) }
        refreshTimeline()
    }

    fun selectModel(providerId: String, model: String, effort: String) {
        val id = state.value.activeSessionId
        if (id.isEmpty()) return
        val error = core.selectModel(id, providerId, model, effort)
        if (error.isNotEmpty()) state.update { it.copy(toast = error) }
        refreshTimeline()
    }

    fun toggleExpanded(key: String) {
        val willExpand = key !in state.value.expanded
        state.update { current ->
            val expanded = current.expanded.toMutableSet()
            // 一级展开收起时，二级也要一起清掉：否则下次展开会直接跳到
            // "全部步骤"，与"默认只显示最后几步"的预期不符。
            val expandedFull = current.expandedFull.toMutableSet()
            if (!expanded.add(key)) {
                expanded.remove(key)
                expandedFull.remove(key)
            }
            current.copy(expanded = expanded, expandedFull = expandedFull)
        }
        if (willExpand) {
            loadProcessDetailIfNeeded(key)
        }
    }

    /**
     * 在一级展开的基础上，切换"显示全部步骤 / 只显示最后几步"。
     *
     * 只对已经一级展开的组有效：没展开时点它没有意义（用户看不到内容）。
     */
    fun toggleExpandedFull(key: String) {
        val current = state.value
        if (key !in current.expanded) return
        state.update {
            val expandedFull = it.expandedFull.toMutableSet()
            if (!expandedFull.add(key)) expandedFull.remove(key)
            it.copy(expandedFull = expandedFull)
        }
    }

    /**
     * 展开过程组时按需拉详情。
     *
     * Go 侧折叠的过程组不带 steps（懒加载：那些思考全文与工具输出往往几十 KB，
     * 一屏十几个组就是上 MB）。这里在展开时才取，并写回 timeline.items。
     *
     * 已经加载过（detail=true）就跳过；正在流式的组本来就带详情，也跳过。
     */
    private fun loadProcessDetailIfNeeded(key: String) {
        val current = state.value
        val id = current.activeSessionId
        if (id.isEmpty()) return
        val item = current.timeline.items.firstOrNull { it.key == key } ?: return
        val process = item.process ?: return
        if (process.detail || process.streaming) return

        viewModelScope.launch {
            val steps = core.processDetail(id, key)
            if (steps.isEmpty()) return@launch
            state.update { st ->
                // 会话可能在等待期间切走了：确认还是同一个会话再写回
                if (st.activeSessionId != id) return@update st
                st.copy(
                    timeline = st.timeline.copy(
                        items = st.timeline.items.map { candidate ->
                            if (candidate.key != key) return@map candidate
                            val existing = candidate.process ?: return@map candidate
                            candidate.copy(
                                process = existing.copy(steps = steps, detail = true),
                            )
                        },
                    ),
                )
            }
        }
    }

    /** 取图片路径（后台拉取 + 落盘），结果回来后再触发一次重绘。 */
    fun imagePath(hash: String, onReady: (String) -> Unit) {
        core.blobPathAsync(hash) { path ->
            if (path.isNotEmpty()) onReady(path)
        }
    }

    fun consumeToast() = state.update { it.copy(toast = "") }

    override fun onCleared() {
        core.close()
        super.onCleared()
    }

    companion object {
        val Factory: ViewModelProvider.Factory = viewModelFactory {
            initializer { ViewerViewModel(this[ViewModelProvider.AndroidViewModelFactory.APPLICATION_KEY] as Application) }
        }
    }
}

/**
 * 切到另一个会话：清掉所有**按会话**的分页状态。
 *
 * 分页游标、"还有更早"、归档及其游标都只对当前会话有意义。漏清任何一个，
 * 新会话就会带着上一个会话的游标去翻页、或者把上一个会话的归档显示在顶上。
 * 所有改 activeSessionId 的地方都走这里，免得各处各清一部分。
 */
internal fun ViewerState.switchedTo(id: String): ViewerState = copy(
    activeSessionId = id,
    timeline = TimelineView(),
    firstSeq = 0,
    hasMoreItems = false,
    loadingEarlier = false,
    earlierPagesLoaded = 0,
    archive = emptyList(),
    archiveCursor = "",
    hasMoreHistory = true,
    loadingOlder = false,
)
