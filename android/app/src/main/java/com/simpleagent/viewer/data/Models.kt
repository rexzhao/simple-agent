package com.simpleagent.viewer.data

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

/*
 * 这些类型是 Go 侧 mobile/bridge/viewmodel.go 的镜像。
 *
 * 分工：Go 负责语义（哪些 part 是正文 / 什么算过程组 / 标题怎么拼 / markdown 怎么解析），
 * Kotlin 只负责画。所以这里**不要**加任何推断逻辑，字段原样来自 Go。
 */

@Serializable
data class SessionSummary(
    val id: String,
    val title: String = "",
    val workspace: String = "",
    @SerialName("updated_at") val updatedAt: Long = 0,
)

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
    val steps: List<ProcessStep> = emptyList(),
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
