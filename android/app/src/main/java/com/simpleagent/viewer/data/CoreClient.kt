package com.simpleagent.viewer.data

import android.os.Handler
import android.os.Looper
import kotlinx.serialization.json.Json
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
        io.execute {
            val error = client.connect(addr, route, token)
            main.post {
                if (!closed.get()) onEvent(CoreEvent.Connected(error))
            }
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

    fun timeline(sessionId: String): TimelineView = decode(client.timelineJSON(sessionId), TimelineView())

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
