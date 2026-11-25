package com.simpleagent.viewer.data

import com.simpleagent.viewer.data.SessionSummary
import com.simpleagent.viewer.data.TimelineView
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.json.Json
import java.io.File

/**
 * 本地缓存：让手机端「打开就能看」，不必每次连上都重新拉一遍。
 *
 * 存什么、怎么存：
 *
 *   cache/
 *     sessions.json          会话列表 + 项目分组（**只有一份**，整表覆盖）
 *     timeline/<会话id>.json 每个会话的历史（**一个会话一个文件**）
 *
 * 为什么一个会话一个文件而不是一个 SQLite：
 *  - 会话之间互不相关，删除/失效都是按会话整体处理，天然对应一个文件
 *  - 不引入 native 依赖（SQLite 的 .so 在 16KB 页设备上是踩过坑的地方）
 *  - 写入量小（一个会话几 MB 顶天），没有必要为了随机查询上数据库
 *
 * 有效性靠 [TimelineView.sync] 里的游标判断（见 [isCompatible]）：
 *  - epoch 变了 → 执行端重启过，seq 重新计数，缓存整体作废
 *  - generation 变了 → 会话被压缩过，行数对不上，作废
 * 这两个都不能用"内容看起来差不多"糊过去，否则会出现错位或漏消息。
 */
class LocalCache(private val root: File) {

    private val json = Json {
        // 忽略未知字段：手机端版本比执行端旧时，多出来的字段不该让整份缓存读失败
        ignoreUnknownKeys = true
        encodeDefaults = true
    }

    private val timelinesDir = File(root, "timeline").apply { mkdirs() }
    private val sessionsFile = File(root, "sessions.json")

    // ---- 会话列表 / 项目状态（只缓存一份）----

    /** readSessions 读缓存的会话列表（没有缓存或读失败返回空）。 */
    fun readSessions(): List<SessionSummary> {
        if (!sessionsFile.exists()) return emptyList()
        return runCatching {
            json.decodeFromString<List<SessionSummary>>(sessionsFile.readText())
        }.getOrElse {
            // 缓存坏了就当没有：它只是加速，不该让 App 起不来
            sessionsFile.delete()
            emptyList()
        }
    }

    /** writeSessions 整表覆盖（列表很小，没必要做增量）。 */
    fun writeSessions(sessions: List<SessionSummary>) {
        runCatching {
            // 显式类型参数：encodeToString 有多个重载，从 List<T> 推不出
            // 该用哪个（编译期会报"无法推断类型"）。
            sessionsFile.writeText(json.encodeToString(ListSerializer(SessionSummary.serializer()), sessions))
        }
    }

    // ---- 会话历史（一个会话一个文件）----

    private fun timelineFile(sessionId: String): File =
        File(timelinesDir, "${sanitize(sessionId)}.json")

    /** readTimeline 读某个会话的缓存历史（没有则返回 null）。 */
    fun readTimeline(sessionId: String): TimelineView? {
        val file = timelineFile(sessionId)
        if (!file.exists()) {
            android.util.Log.d("SimpleAgent", "cache: ${file.name} 不存在")
            return null
        }
        return try {
            val text = file.readText()
            val view = json.decodeFromString(TimelineView.serializer(), text)
            android.util.Log.d(
                "SimpleAgent",
                "cache: read ${file.name} items=${view.items.size} sync=${view.sync}",
            )
            view
        } catch (e: Throwable) {
            android.util.Log.e("SimpleAgent", "cache: read ${file.name} failed", e)
            file.delete()
            null
        }
    }

    /** writeTimeline 写入某个会话的历史。 */
    fun writeTimeline(view: TimelineView) {
        val file = timelineFile(view.sessionId)
        try {
            file.writeText(json.encodeToString(TimelineView.serializer(), view))
            android.util.Log.d("SimpleAgent", "cache: wrote ${file.name} (${file.length()} bytes)")
        } catch (e: Throwable) {
            // **不能静默吞掉**：写失败等于缓存永远没有内容，而界面表现是
            // "重新打开还是空白"，完全看不出是序列化/写盘出错（踩过）。
            android.util.Log.e("SimpleAgent", "cache: write ${file.name} failed", e)
        }
    }

    /** dropTimeline 删掉某个会话的缓存（会话被删、或缓存失效时）。 */
    fun dropTimeline(sessionId: String) {
        timelineFile(sessionId).delete()
    }

    /**
     * isCompatible 判断缓存还能不能用。
     *
     * 只能用**执行端给的当前游标**来比：缓存的 epoch/generation 与当前不一致
     * 就必须整体丢弃。这两项的语义见 [TimelineView.sync] 的注释。
     *
     * 空游标（旧版本执行端）一律视为不可用 —— 没有游标就没法判断，
     * 宁可重拉一次，也不要用一份可能错位的历史。
     */
    fun isCompatible(cached: TimelineView, epoch: String, generation: Long): Boolean {
        val sync = cached.sync ?: return false
        if (sync.epoch.isEmpty() || sync.epoch != epoch) return false
        return sync.generation == generation
    }

    /** clear 清空全部缓存（退出登录/换执行端时用）。 */
    fun clear() {
        timelinesDir.listFiles()?.forEach { it.delete() }
        sessionsFile.delete()
    }

    /** sanitize 把会话 id 变成安全文件名（id 是十六进制，这里只是防御）。 */
    private fun sanitize(id: String): String =
        id.map { if (it.isLetterOrDigit() || it == '-' || it == '_') it else '_' }
            .joinToString("")
}