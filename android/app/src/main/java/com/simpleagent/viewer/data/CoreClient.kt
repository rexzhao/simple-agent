package com.simpleagent.viewer.data

import android.os.Handler
import android.os.Looper
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicBoolean

/**
 * Go 内核（bridge.Client）的 Kotlin 封装。
 *
 * 三条纪律：
 *  1. Go 的回调发生在它自己的 goroutine 上，**结果统一 post 回主线程**再交给界面；
 *  2. 阻塞操作（blobPath 要等执行端回图片）走后台线程池，绝不占主线程；
 *  3. Go 侧已经保证镜像只在它自己的串行线程上改，这里不再重复加锁。
 */
class CoreClient private constructor(
    cacheDir: String,
    private val onEvent: (CoreEvent) -> Unit,
) : bridge.ChangeListener {

    private val main = Handler(Looper.getMainLooper())
    private val io = Executors.newCachedThreadPool { runnable ->
        Thread(runnable, "bridge-io").apply { isDaemon = true }
    }
    private val json = Json {
        ignoreUnknownKeys = true
        explicitNulls = false
    }

    // bridge.Client 是 Go 对象，跨线程使用安全（Go 侧自己串行化）
    private val client = bridge.Client(this, "zh", cacheDir)
    private val closed = AtomicBoolean(false)

    override fun onChange(kind: String, sessionID: String) {
        // Go 在自己的 goroutine 上回调：切回主线程再通知界面
        main.post {
            if (!closed.get()) onEvent(CoreEvent.Changed(kind, sessionID))
        }
    }

    // ---- 连接 ----

    /** 连上执行端/relay（后台拨号，结果经 [CoreEvent.Connected] 回来）。 */
    fun connect(addr: String, route: String, token: String) {
        connectTLS(addr, route, token, "")
    }

    /**
     * 与 [connect] 相同，但可以额外校验 relay 的证书指纹（传输加密）。
     *
     * fingerprint 为空表示不启用 TLS；非空时只接受指纹完全一致的证书。
     */
    fun connectTLS(addr: String, route: String, token: String, fingerprint: String) {
        // **用异步版**（Go 侧后台拨号，JNI 立刻返回）。
        //
        // 同步版会在 TOFU 等用户确认时阻塞整条 JNI 线程 —— gomobile 的
        // 调用是串行化的，于是 Kotlin 再想调 pendingPeer() 询问"要确认什么"
        // 就永远进不来。表现是界面一直转圈、弹窗从不出现。
        val error = client.connectTLSAsync(addr, route, token, fingerprint)
        if (error.isNotEmpty()) {
            main.post { if (!closed.get()) onEvent(CoreEvent.Connected(error)) }
        }
    }

    fun disconnect() {
        io.execute { client.disconnect() }
    }

    fun close() {
        if (!closed.compareAndSet(false, true)) return
        io.execute { client.close() }
        io.shutdown()
    }

    // ---- 读（读的是本地镜像，很快，可以直接在主线程调）----

    fun sessions(): List<SessionSummary> = decode(client.sessionsJSON(), emptyList())

    /** 当前是否连着（断线重连期间为 false）。 */
    fun connected(): Boolean = client.connected()

    /** relay 上在线的执行端（直连时为空）。 */
    fun executors(): List<ExecutorChoice> = decode(client.executorsJSON(), emptyList())

    /** 当前连着的执行端 id。 */
    fun currentHostId(): String = client.currentHostID()

    /** 切到另一个执行端（沿用上次连接的地址与凭据）。返回空字符串表示成功。 */
    fun switchHost(hostId: String): String = client.switchHost(hostId)

    fun timeline(sessionId: String): TimelineView = decode(client.timelineJSON(sessionId), TimelineView())

    /**
     * 取一页时间线（分页）。
     *
     * beforeKey 为空表示"最后一页"；否则返回该 key 之前的一页。
     * 首屏用它只拿最后几十条——完整 timeline 在长会话里是几百 KB，
     * 全量传过来再反序列化就是打开会话时那几秒白屏。
     */
    /**
     * 取一页时间线（**按事件 seq 分页**）。
     *
     * beforeSeq = 0 表示"最后一页"；否则返回 seq 小于它的那一页。
     *
     * 为什么用 seq 而不是 item key：item 是渲染产物，key 里含 part 索引，
     * 流式输出期间会变（proc:xxx#1 ↔ xxx#0），游标因此失效——真机上表现为
     * "翻到某处就再也翻不动"。日志的 seq 一旦写入永不变。
     */
    fun timelinePage(sessionId: String, beforeSeq: Long, limit: Int): TimelinePage =
        decode(client.timelinePageJSON(sessionId, beforeSeq, limit.toLong()), TimelinePage())

    /**
     * 取某个会话当前的同步游标（判断本地缓存是否还有效）。
     *
     * 单独一次调用：分页结果里不带游标（每页都背一份没必要），
     * 而校验缓存只需要这一次。
     */
    fun syncCursor(sessionId: String): SyncCursor =
        decode(client.syncCursorJSON(sessionId), SyncCursor())

    /**
     * 探测对端 TLS 证书指纹（TOFU 用，**不做校验**）。
     *
     * 返回原始 JSON：`{"fingerprint":"…","plain":bool,"error":"…"}`。
     * 不在这里解析成 data class —— gomobile 的返回值只能是一个字符串，
     * 而解析放在调用方（ViewModel）能让它把解析失败也当成一种结果处理。
     *
     * 对端是明文服务时 plain=true（应直接明文连接，不弹确认框）；
     * 连不上时 error 非空。
     */
    fun probeFingerprint(addr: String): String = client.probeFingerprint(addr)

    /**
     * 取当前等待用户确认的对端（TOFU）。
     *
     * 返回 `地址\n指纹\n是否变化(0/1)\n旧指纹`，空串表示没有待确认项。
     * 连接过程中由 bridge 在 TLS 握手时登记 —— 那时那条连接被挂起，
     * 等这里返回信息、界面弹窗、用户回答后才会继续。
     */
    fun pendingPeer(): String = client.pendingPeers()

    /** 回答"要不要信任 addr 这个对端"。返回空串表示已提交。 */
    fun resolvePeer(addr: String, allow: Boolean): String = client.resolvePeer(addr, allow)

    /** 取一个过程组的完整步骤（用户展开时调；折叠时 Go 侧不传 Steps）。 */
    fun processDetail(sessionId: String, key: String): List<ProcessStep> {
        val page = decode(client.processDetailJSON(sessionId, key), ProcessDetailPage())
        return page.steps
    }

    fun providers(): List<ProviderChoice> = decode(client.providersJSON(), emptyList())

    // ---- 写（命令：立即返回错误文本，空字符串表示已提交）----

    fun sendText(sessionId: String, text: String): String = client.sendText(sessionId, text)

    fun enqueue(sessionId: String, text: String): String = client.enqueue(sessionId, text)

    fun stop(sessionId: String): String = client.stop(sessionId)

    fun dropQueued(sessionId: String, index: Int): String = client.dropQueued(sessionId, index.toLong())

    fun selectModel(sessionId: String, providerId: String, model: String, effort: String): String =
        client.selectModel(sessionId, providerId, model, effort)

    fun newSession(workspace: String, title: String): String = client.newSession(workspace, title)

    fun rename(sessionId: String, title: String): String = client.rename(sessionId, title)

    fun deleteSession(sessionId: String): String = client.deleteSession(sessionId)

    // ---- 提问（选择状态存在 Go 的 AskGroup 里，这里只转发操作）----

    /** 切换某个选项，返回新的提问列表 JSON（含选中态）。 */
    fun toggleAskOption(sessionId: String, callId: String, questionIndex: Int, optionIndex: Int): String =
        client.toggleAskOption(sessionId, callId, questionIndex.toLong(), optionIndex.toLong())

    /** 写入某题的手写答案，返回新的提问列表 JSON。 */
    fun setAskCustom(sessionId: String, callId: String, questionIndex: Int, text: String): String =
        client.setAskCustom(sessionId, callId, questionIndex.toLong(), text)

    /** 提交当前选择（异步命令）。 */
    fun submitAsks(sessionId: String): String = client.submitAsks(sessionId)

    /** 跳过这次提问。 */
    fun dismissAsks(sessionId: String): String = client.dismissAsks(sessionId)

    fun decodeAsks(json: String): List<AskView> = decode(json, emptyList())

    // ---- 更早的历史（阻塞：等执行端回包，必须在后台线程）----

    fun loadOlder(sessionId: String, cursor: String): ArchiveView =
        decode(client.loadOlder(sessionId, cursor), ArchiveView())

    fun loadOlderAsync(sessionId: String, cursor: String, onResult: (ArchiveView) -> Unit) {
        io.execute {
            val page = decode(client.loadOlder(sessionId, cursor), ArchiveView())
            main.post { if (!closed.get()) onResult(page) }
        }
    }

    // ---- 上传（阻塞：等执行端确认落盘）----

    /**
     * 上传一张图片到执行端，返回内容 hash。
     *
     * 失败返回空字符串，原因在 [lastError]。**必须在后台线程**：它会等执行端回包。
     */
    fun putBlobAsync(data: ByteArray, onResult: (String) -> Unit) {
        io.execute {
            val hash = client.putBlob(data)
            main.post { if (!closed.get()) onResult(hash) }
        }
    }

    /** 带附件发送（附件是已上传的 blob 引用 JSON）。 */
    fun sendWithAttachments(sessionId: String, text: String, attachmentsJson: String): String =
        client.sendWithAttachments(sessionId, text, attachmentsJson)

    // ---- 图片（阻塞：等执行端回包 + 落盘，**必须在后台线程**）----

    /** 取图片的本地路径；失败返回空字符串，原因在 [lastError]。 */
    fun blobPath(hash: String): String = client.blobPath(hash)

    fun lastError(): String = client.lastError()

    /** 在后台线程取图片，结果回主线程。 */
    fun blobPathAsync(hash: String, onResult: (String) -> Unit) {
        io.execute {
            val path = client.blobPath(hash)
            main.post {
                if (!closed.get()) onResult(path)
            }
        }
    }

    private inline fun <reified T> decode(text: String, fallback: T): T = try {
        json.decodeFromString<T>(text)
    } catch (error: Exception) {
        // 解码失败不要把界面搞崩：Go 侧异常时会回 {"error": "..."}
        onEvent(CoreEvent.ProtocolError("解析失败：${error.message}"))
        fallback
    }

    companion object {
        fun create(cacheDir: String, onEvent: (CoreEvent) -> Unit): CoreClient =
            CoreClient(cacheDir, onEvent)
    }
}

/** Go 内核推给界面的事件。 */
sealed interface CoreEvent {
    /** 某类东西变了（kind 对应 Go 侧 host.Change.Kind）。 */
    data class Changed(val kind: String, val sessionId: String) : CoreEvent

    /** 连接流程结束；error 为空表示成功。 */
    data class Connected(val error: String) : CoreEvent

    /** 协议/解码错误（界面可以弹一条提示）。 */
    data class ProtocolError(val message: String) : CoreEvent
}
