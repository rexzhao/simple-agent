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
    val sessions: List<SessionSummary> = emptyList(),
    val activeSessionId: String = "",
    val timeline: TimelineView = TimelineView(),
    val providers: List<ProviderChoice> = emptyList(),
    /** 展开的过程组 key。 */
    val expanded: Set<String> = emptySet(),
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
                "conversation", "runtime", "notice" -> refreshTimeline()
            }
            is CoreEvent.ProtocolError -> state.update { it.copy(toast = event.message) }
        }
    }

    // ---- 刷新（读快照，不推断）----

    private fun refreshAll() {
        refreshSessions()
        refreshProviders()
        refreshTimeline()
    }

    private fun refreshSessions() {
        val sessions = core.sessions()
        state.update { current ->
            val active = current.activeSessionId.ifEmpty {
                sessions.lastOrNull()?.id.orEmpty()
            }
            current.copy(sessions = sessions, activeSessionId = active)
        }
    }

    private fun refreshProviders() {
        state.update { it.copy(providers = core.providers()) }
    }

    private fun refreshConnection() {
        // 连接状态从 timeline 的 runtime 里读（Go 侧已经算好）
        refreshTimeline()
    }

    private fun refreshTimeline() {
        val id = state.value.activeSessionId
        if (id.isEmpty()) return
        val timeline = core.timeline(id)
        state.update {
            it.copy(
                timeline = timeline,
                connected = timeline.runtime.connected,
                asks = timeline.asks,
            )
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
        state.update { it.copy(connected = false, sessions = emptyList(), activeSessionId = "", timeline = TimelineView()) }
    }

    fun selectSession(id: String) {
        if (id == state.value.activeSessionId) return
        state.update { it.copy(activeSessionId = id, timeline = TimelineView()) }
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
        state.update { current ->
            val expanded = current.expanded.toMutableSet()
            if (!expanded.add(key)) expanded.remove(key)
            current.copy(expanded = expanded)
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
