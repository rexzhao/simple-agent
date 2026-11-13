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
import com.simpleagent.viewer.data.ProviderChoice
import com.simpleagent.viewer.data.SessionSummary
import com.simpleagent.viewer.data.TimelineView
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
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
    val connectError: String = "",
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
    /** 展开的过程组 key。 */
    val expanded: Set<String> = emptySet(),
    /**
     * 当前加载的 items 之前还有更多（用于滚到顶部时翻页）。
     *
     * 注意与 [hasMoreHistory] 区分：后者是**归档**（压缩后的旧消息），
     * 这个是**同一段会话里更早的 items**（分页尚未加载的部分）。
     */
    val hasMoreItems: Boolean = false,
    /** 当前加载的第一条 item 的 key（往前翻页的游标）。 */
    val firstItemKey: String = "",
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
 * 展示端的 ViewModel。
 *
 * 它只做三件事：持有 Go 内核、把内核推来的事件变成"重新拉一次快照"、
 * 把用户操作转成命令。**不推断任何会话语义**（那是 Go 的事）。
 */
class ViewerViewModel(app: Application) : AndroidViewModel(app) {

    private val cacheDir: File = File(app.cacheDir, "simpleagent").apply { mkdirs() }

    // 连接参数记在本机（手机输入地址很麻烦，尤其是中文输入法打不出 ASCII 路径）
    private val prefs = app.getSharedPreferences("viewer", android.content.Context.MODE_PRIVATE)

    // 流式刷新的节流状态（见 scheduleTimelineRefresh）
    private var timelineRefreshJob: kotlinx.coroutines.Job? = null
    private var timelineRefreshPending = false
    private val timelineRefreshIntervalMs = 80L
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
        core.connectTLS(
            current.addr.trim(),
            current.route.trim(),
            current.token.trim(),
            current.fingerprint.trim(),
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
                    state.update {
                        it.copy(
                            connecting = false,
                            connected = false,
                            connectError = event.error,
                            sessions = emptyList(),
                            activeSessionId = "",
                            timeline = TimelineView(),
                        )
                    }
                    return
                }
                state.update { it.copy(connecting = false, connected = true, connectError = "") }
                // 记住"上次连接成功过"：下次启动 App 自动连回去。
                // 只在成功时记，连接失败不算（否则启动时会自动连一个连不上的地址）。
                prefs.edit().putBoolean("autoConnect", true).apply()
                refreshAll()
            }
            is CoreEvent.Changed -> when (event.kind) {
                "sessions" -> refreshSessions()
                "providers" -> refreshProviders()
                "connection" -> refreshConnection()
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

    private fun refreshSessions() {
        val sessions = core.sessions()
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

    private fun refreshConnection() {
        // 断线/重连只靠 timeline 是看不出来的：timeline 的 runtime.connected
        // 反映的是"会话状态"，而连接本身是否在线要问 Go 侧。
        // 以前这里只 refreshTimeline()，导致断线后 connected 永远停在 true，
        // 界面显示"已连接"但实际断了，用户只能手动重连。
        val online = core.connected()
        state.update {
            if (it.connected == online) it else it.copy(connected = online, reconnecting = !online)
        }
        refreshTimeline()
    }

    /**
     * 节流版刷新：流式输出时 `conversation` 事件每个 token 来一次，
     * 每次都重建整条时间线（长会话要序列化上百 KB）会直接卡住界面。
     *
     * 策略：最多每 [timelineRefreshIntervalMs] 刷一次，且**保证最后一次一定执行**
     * （用 pending 标记 + 尾部补刷），否则流式结束后界面会停在中间状态。
     */
    private fun scheduleTimelineRefresh() {
        if (timelineRefreshJob?.isActive == true) {
            // 已经有排队的刷新：只标记"期间又来过新数据"，等它跑完再补一次
            timelineRefreshPending = true
            return
        }
        timelineRefreshJob = viewModelScope.launch {
            refreshTimeline()
            while (timelineRefreshPending) {
                timelineRefreshPending = false
                kotlinx.coroutines.delay(timelineRefreshIntervalMs)
                refreshTimeline()
            }
        }
    }

    private fun refreshTimeline() {
        val id = state.value.activeSessionId
        if (id.isEmpty()) return
        // 分页取最后一页：完整 timeline 在长会话里是几百 KB（实测 774KB），
        // 全量传过来再反序列化就是打开会话时那几秒白屏。
        //
        // 流式输出期间取最后一页是安全的：新内容总在末尾，不会因为分页而丢。
        val page = core.timelinePage(id, "", timelinePageSize)
        val previous = state.value.timeline
        // 已经展开过的过程组，其详情是单独拉的：合并时要把已加载的 steps 留住，
        // 否则每次刷新都会把用户展开的内容退回折叠（体验上像是"自己收起来了"）。
        val merged = mergeProcessDetails(page.items, previous.items)
        state.update {
            it.copy(
                timeline = TimelineView(
                    sessionId = page.sessionId,
                    title = page.title ?: it.timeline.title,
                    items = merged,
                    todos = page.todos ?: it.timeline.todos,
                    asks = page.asks ?: it.timeline.asks,
                    runtime = page.runtime ?: it.timeline.runtime,
                ),
                connected = (page.runtime ?: it.timeline.runtime).connected,
                asks = page.asks ?: it.timeline.asks,
                hasMoreItems = page.hasMore,
                firstItemKey = page.firstKey,
            )
        }
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
        state.update { it.copy(connecting = true, connectError = "") }
        val current = state.value
        // 记住这次填的全部连接参数：手机输入很麻烦，指纹还是 64 位十六进制。
        // token/指纹属于凭据，但存在应用私有 SharedPreferences 里（未 root 时
        // 其它应用读不到），比每次手敲安全得多。
        prefs.edit()
            .putString("addr", current.addr.trim())
            .putString("route", current.route.trim())
            .putString("token", current.token.trim())
            .putString("fingerprint", current.fingerprint.trim())
            .apply()
        core.connectTLS(
            current.addr.trim(),
            current.route.trim(),
            current.token.trim(),
            current.fingerprint.trim(),
        )
    }

    fun disconnect() {
        core.disconnect()
        // 用户主动断开 = 明确表示"不要连着"：清掉自动重连标记，
        // 否则下次启动 App 又会自己连回去，违背用户意图。
        prefs.edit().putBoolean("autoConnect", false).apply()
        state.update { it.copy(connected = false, reconnecting = false, sessions = emptyList(), activeSessionId = "", timeline = TimelineView()) }
    }

    fun selectSession(id: String) {
        if (id == state.value.activeSessionId) return
        state.update { it.copy(activeSessionId = id, timeline = TimelineView()) }
        // 记住正在看的会话：重启 App 后直接回到这里
        prefs.edit().putString("activeSessionId", id).apply()
        refreshTimeline()
    }

    /**
     * 新建会话。
     *
     * 命令是异步的：新会话的 id 经 cmdResult 回到 refreshSessions，
     * 这里用"会话数量变多"来选中最后一个（与桌面一致：新会话排在末尾）。
     */
    fun newSession() {
        val before = state.value.sessions.map { it.id }.toSet()
        val error = core.newSession("", "")
        if (error.isNotEmpty()) {
            state.update { it.copy(toast = error) }
            return
        }
        // 稍等一轮事件循环再拉列表：命令要先在执行端跑
        viewModelScope.launch {
            kotlinx.coroutines.delay(300)
            val sessions = core.sessions()
            val created = sessions.lastOrNull { it.id !in before } ?: sessions.lastOrNull()
            state.update { current ->
                current.copy(
                    sessions = sessions,
                    activeSessionId = created?.id ?: current.activeSessionId,
                )
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
            state.update { it.copy(sessions = sessions, activeSessionId = created.id) }
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

        val error = if (current.timeline.runtime.canSend) {
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

    /** 加载下一页归档（滚到顶部时触发）。 */
    fun loadOlder() {
        val current = state.value
        val id = current.activeSessionId
        if (id.isEmpty() || current.loadingOlder || !current.hasMoreHistory) return
        state.update { it.copy(loadingOlder = true) }
        core.loadOlderAsync(id, current.archiveCursor) { page ->
            state.update { st ->
                if (page.error.isNotEmpty()) {
                    st.copy(loadingOlder = false, hasMoreHistory = false, toast = page.error)
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
            if (!expanded.add(key)) expanded.remove(key)
            current.copy(expanded = expanded)
        }
        if (willExpand) {
            loadProcessDetailIfNeeded(key)
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
