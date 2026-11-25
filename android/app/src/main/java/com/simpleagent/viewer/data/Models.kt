package com.simpleagent.viewer.data

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

/*
 * 这些类型是 Go 侧 mobile/bridge/viewmodel.go 的镜像。
 *
 * 分工：Go 负责语义（哪些 part 是正文 / 什么算过程组 / 标题怎么拼 / markdown 怎么解析），
 * Kotlin 只负责画。所以这里**不要**加任何推断逻辑，字段原样来自 Go。
 */

/** relay 上一个在线的执行端（用于界面切换）。 */
@Serializable
data class ExecutorChoice(
    @SerialName("host_id") val hostId: String,
    val name: String = "",
)

@Serializable
data class SessionSummary(
    val id: String,
    val title: String = "",
    val workspace: String = "",
    @SerialName("updated_at") val updatedAt: Long = 0,
    // 项目分组用。为空表示执行端没有项目档案（旧版本/内存后端），
    // 这时退回按 workspace 分组，与桌面端的降级策略一致。
    @SerialName("project_id") val projectId: String = "",
    @SerialName("project_name") val projectName: String = "",
    // 已归档：仍列出但折叠在「已归档」小节里，不能运行。
    val archived: Boolean = false,
    // 未读邮件数（会话通讯）：侧栏在会话标题旁画未读标记。
    //
    // 邮件是**别的会话主动发来的**，不看这个数就永远不知道哪个会话在等它
    // 处理事情。0 表示没有未读（不画标记）。
    @SerialName("mail_unread") val mailUnread: Int = 0,
) {
    /** 侧栏分组用的 key：有项目档案就按项目，否则按 workspace。 */
    val groupKey: String get() = projectId.ifEmpty { workspace }
}

@Serializable
data class RuntimeView(
    val busy: String = "idle",
    val queued: List<String> = emptyList(),
    @SerialName("input_tokens") val inputTokens: Int = 0,
    @SerialName("output_tokens") val outputTokens: Int = 0,
    @SerialName("tokens_in_use") val tokensInUse: Int = 0,
    @SerialName("context_window") val contextWindow: Int = 0,
    val turns: Int = 0,
    val steps: Int = 0,
    @SerialName("provider_id") val providerId: String = "",
    val model: String = "",
    @SerialName("model_label") val modelLabel: String = "",
    val effort: String = "",
    @SerialName("effort_label") val effortLabel: String = "",
    val vision: Boolean = false,
    val compacting: Boolean = false,
    @SerialName("can_send") val canSend: Boolean = false,
    @SerialName("can_steer") val canSteer: Boolean = false,
    @SerialName("allow_shell") val allowShell: Boolean = false,
    val connected: Boolean = false,
) {
    val isIdle: Boolean get() = busy == "idle"
}

@Serializable
data class TimelineView(
    @SerialName("session_id") val sessionId: String = "",
    val title: String = "",
    val items: List<ItemView> = emptyList(),
    val todos: List<TodoView> = emptyList(),
    val asks: List<AskView> = emptyList(),
    val runtime: RuntimeView = RuntimeView(),
    /**
     * 同步游标：本地缓存的有效性判断靠它（见 [LocalCache.isCompatible]）。
     *
     * 三个字段各有用途：epoch 变了说明执行端重启（seq 重新计数）、
     * generation 变了说明会话被压缩（行数对不上）、lastSeq 是这份内容
     * 覆盖到的事件序号（下次只要它之后的部分）。
     */
    val sync: SyncCursor? = null,
)

/**
 * SyncCursor 是增量同步的游标。
 *
 * 为空表示执行端没给（旧版本），此时缓存一律视为不可用 —— 没有游标就
 * 没法判断，宁可重拉一次也不要用一份可能错位的历史。
 */
@Serializable
data class SyncCursor(
    val epoch: String = "",
    val generation: Long = 0,
    @SerialName("last_seq") val lastSeq: Long = 0,
)

@Serializable
data class ItemView(
    val kind: String,
    val key: String = "",
    val message: MessageView? = null,
    val process: ProcessView? = null,
    val notice: NoticeView? = null,
    val footer: FooterView? = null,
)

@Serializable
data class MessageView(
    val id: String = "",
    val role: String = "agent",
    val status: String = "",
    val reason: String = "",
    val at: Long = 0,
    val parts: List<PartView> = emptyList(),
) {
    val isUser: Boolean get() = role == "user"
}

@Serializable
data class PartView(
    val kind: String,
    val text: String = "",
    val lang: String = "",
    val image: ImageView? = null,
    val name: String = "",
    val blocks: List<BlockView> = emptyList(),
)

@Serializable
data class ImageView(
    val blob: String = "",
    val url: String = "",
    @SerialName("media_type") val mediaType: String = "",
    val width: Int = 0,
    val height: Int = 0,
)

@Serializable
data class ProcessView(
    val key: String = "",
    val title: String = "",
    val activity: String = "tools",
    val streaming: Boolean = false,
    /**
     * 这一组有多少步。
     *
     * 折叠时 [steps] 是空的（Go 侧懒加载），但步数照样能显示。
     */
    @SerialName("step_count") val stepCount: Int = 0,
    /** [steps] 是否已带上（false 表示还没拉详情，展开时要请求）。 */
    val detail: Boolean = false,
    val steps: List<ProcessStep> = emptyList(),
)

/**
 * 一个过程组的完整步骤（Go 侧 ProcessDetailJSON 的镜像）。
 *
 * 只在用户展开折叠的过程组时请求：折叠状态下 Go 侧不传 Steps。
 */
@Serializable
data class ProcessDetailPage(
    val key: String = "",
    val steps: List<ProcessStep> = emptyList(),
    val error: String = "",
)

/**
 * 一页时间线（Go 侧 TimelinePageJSON 的镜像）。
 *
 * 只有最后一页带 runtime/todos/asks —— 它们是"当前状态"，
 * 翻历史页时不该用旧值覆盖界面。
 */
@Serializable
data class TimelinePage(
    @SerialName("session_id") val sessionId: String = "",
    val items: List<ItemView> = emptyList(),
    @SerialName("has_more") val hasMore: Boolean = false,
    /** 本页覆盖的事件序号区间；翻更早的页时用它当游标。 */
    @SerialName("seq_from") val seqFrom: Long = 0,
    @SerialName("seq_to") val seqTo: Long = 0,
    /**
     * 本次没取到内容、但**不代表到底了**（断线 / 补拉失败），可以重试。
     *
     * 与"真的到最早"（`hasMore == false` 且 `retry == false`）区分开：
     * 以前两者都表现为空页 + has_more=false，界面于是永久显示
     * "已经是最早的内容了" —— 用户明明还没翻到第一条。
     */
    val retry: Boolean = false,
    val title: String? = null,
    val runtime: RuntimeView? = null,
    val todos: List<TodoView>? = null,
    val asks: List<AskView>? = null,
    val error: String = "",
)

@Serializable
data class ProcessStep(
    val kind: String,
    val text: String = "",
    @SerialName("tool_id") val toolId: String = "",
    @SerialName("tool_name") val toolName: String = "",
    @SerialName("tool_title") val toolTitle: String = "",
    val args: String = "",
    val output: String = "",
    @SerialName("is_error") val isError: Boolean = false,
    val pending: Boolean = false,
    val done: Boolean = false,
    @SerialName("duration_ms") val durationMs: Long = 0,
    // 以下是与桌面端对齐的结构化摘要（只给界面看，不发给模型）。
    // 用来显示人话：「3 个文件 · 12 处命中」「第 1-80 行 / 共 300 行」……
    // 没有它们就只能照搬 output 里给模型的文本（XML 信封、英文指令）。
    val description: String = "",
    val exec: ExecInfoView? = null,
    val diff: FileDiffView? = null,
    @SerialName("image_info") val imageInfo: ImageInfoView? = null,
    val search: SearchView? = null,
    val web: WebView? = null,
    val read: ReadView? = null,
    val todo: TodoProgressView? = null,
)

/** 命令执行的元信息（bash/pwsh）。 */
@Serializable
data class ExecInfoView(
    @SerialName("exit_code") val exitCode: Int = 0,
    @SerialName("elapsed_ms") val elapsedMs: Long = 0,
    val cwd: String = "",
    @SerialName("timed_out") val timedOut: Boolean = false,
)

/** diff 的一行。 */
@Serializable
data class DiffLineView(
    val kind: String = "ctx",
    val text: String = "",
    @SerialName("old_line") val oldLine: Int = 0,
    @SerialName("new_line") val newLine: Int = 0,
)

/** 一段差异（带上下文行）。 */
@Serializable
data class DiffHunkView(
    @SerialName("old_start") val oldStart: Int = 0,
    @SerialName("new_start") val newStart: Int = 0,
    val lines: List<DiffLineView> = emptyList(),
)

/** 文件修改的差异（edit）或写入内容（write）。 */
@Serializable
data class FileDiffView(
    val path: String = "",
    val hunks: List<DiffHunkView> = emptyList(),
    val added: Int = 0,
    val removed: Int = 0,
    val truncated: Boolean = false,
    val content: String = "",
    val verb: String = "",
)

/** 图片摘要。 */
@Serializable
data class ImageInfoView(
    val path: String = "",
    val width: Int = 0,
    val height: Int = 0,
    @SerialName("media_type") val mediaType: String = "",
    val bytes: Int = 0,
)

/** 一条检索命中。 */
@Serializable
data class SearchHitView(
    val line: Int = 0,
    val text: String = "",
)

/** 一个文件里的检索命中。 */
@Serializable
data class SearchFileView(
    val path: String = "",
    val hits: List<SearchHitView> = emptyList(),
)

/** 检索结果（grep/glob）。 */
@Serializable
data class SearchView(
    val pattern: String = "",
    val files: List<SearchFileView> = emptyList(),
    @SerialName("file_count") val fileCount: Int = 0,
    @SerialName("hit_count") val hitCount: Int = 0,
    val truncated: Boolean = false,
)

/** 一条搜索结果。 */
@Serializable
data class WebResultView(
    val title: String = "",
    val url: String = "",
    val excerpt: String = "",
)

/** web 工具的摘要。 */
@Serializable
data class WebView(
    val kind: String = "",
    val url: String = "",
    val status: Int = 0,
    val bytes: Int = 0,
    val truncated: Boolean = false,
    val results: List<WebResultView> = emptyList(),
)

/** read 的行范围。 */
@Serializable
data class ReadView(
    val path: String = "",
    @SerialName("start_line") val startLine: Int = 0,
    @SerialName("end_line") val endLine: Int = 0,
    @SerialName("total_lines") val totalLines: Int = 0,
    val truncated: Boolean = false,
)

/** todo_write 的进度摘要。 */
@Serializable
data class TodoProgressView(
    val total: Int = 0,
    val completed: Int = 0,
)

@Serializable
data class NoticeView(
    val id: String = "",
    val level: String = "info",
    val text: String = "",
)

@Serializable
data class FooterView(
    val status: String = "complete",
    val reason: String = "",
    @SerialName("duration_ms") val durationMs: Long = 0,
    val tokens: Int = 0,
)

@Serializable
data class TodoView(
    val content: String = "",
    val status: String = "",
)

// ---- markdown 块（结构由 Go 解析好）----

@Serializable
data class BlockView(
    val kind: String,
    val level: Int = 0,
    val lang: String = "",
    val code: String = "",
    val spans: List<SpanView> = emptyList(),
    val blocks: List<BlockView> = emptyList(),
    val items: List<List<BlockView>> = emptyList(),
    val ordered: Boolean = false,
    val start: Int = 0,
    val header: List<List<SpanView>> = emptyList(),
    val rows: List<List<List<SpanView>>> = emptyList(),
    val aligns: List<String> = emptyList(),
)

@Serializable
data class SpanView(
    val text: String = "",
    val bold: Boolean = false,
    val italic: Boolean = false,
    val strike: Boolean = false,
    val code: Boolean = false,
    val link: String = "",
    @SerialName("image_url") val imageUrl: String = "",
)

// ---- 模型列表（来自执行端）----

@Serializable
data class ProviderChoice(
    @SerialName("provider_id") val providerId: String = "",
    val label: String = "",
    val models: List<String> = emptyList(),
    val labels: Map<String, String> = emptyMap(),
    val efforts: Map<String, List<String>> = emptyMap(),
)
