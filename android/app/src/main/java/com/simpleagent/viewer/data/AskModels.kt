package com.simpleagent.viewer.data

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

// ---- 提问面板 ----

@Serializable
data class AskView(
    @SerialName("message_id") val messageId: String = "",
    @SerialName("call_id") val callId: String = "",
    val questions: List<QuestionView> = emptyList(),
    val answered: Boolean = false,
)

@Serializable
data class QuestionView(
    val id: String = "",
    val question: String = "",
    val header: String = "",
    @SerialName("multi_select") val multiSelect: Boolean = false,
    val options: List<String> = emptyList(),
    val selected: List<Int> = emptyList(),
    val custom: String = "",
    @SerialName("custom_active") val customActive: Boolean = false,
    val answered: Boolean = false,
)

// ---- 更早的历史（归档）----

@Serializable
data class ArchiveView(
    val entries: List<ArchivedMessage> = emptyList(),
    val next: String = "",
    val more: Boolean = false,
    val error: String = "",
)

@Serializable
data class ArchivedMessage(
    val id: String = "",
    val role: String = "agent",
    val status: String = "",
    val reason: String = "",
    val at: Long = 0,
    val parts: List<PartView> = emptyList(),
)
