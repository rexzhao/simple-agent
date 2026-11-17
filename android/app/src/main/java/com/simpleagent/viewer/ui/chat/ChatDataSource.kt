package com.simpleagent.viewer.ui.chat

import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.State
import androidx.compose.runtime.derivedStateOf
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import kotlinx.coroutines.delay

/**
 * 聊天列表的数据源。
 *
 * 组件只依赖这个接口，不关心数据来自网络、内存还是测试桩。
 * 这样滚动行为可以脱离真实后端单独验证 —— 真机上的问题混着网络、
 * 分页、状态机，出问题时分不清是哪一层。
 *
 * ## 实现方必须保证的语义
 *
 * 1. **[items] 只增不减**（同一会话内）。减少会触发"切会话"的处理路径。
 * 2. **新内容只从两端进入**：往前加载 → 插到头部；流式输出 → 追加到尾部。
 * 3. **[loadEarlier] 返回后，新插入的条目必须已经出现在 [items] 里** ——
 *    组件靠对比前后快照判断"插了多少条"。
 */
interface ChatDataSource {
    /** 当前列表（最旧 → 最新）。 */
    val items: List<ChatItem>

    /**
     * 还有没有更早的内容可加载。
     *
     * 组件用它在顶部显示"+ 更多"，并在滚到顶部时请求 [loadEarlier]。
     */
    val hasMore: Boolean

    /** 正在加载更早内容（组件据此显示转圈）。 */
    val loading: Boolean

    /**
     * 加载更早的一批（插到 [items] 头部）。
     *
     * 必须是**幂等**的：组件可能因为滚动事件重复触发，
     * 实现方自己保证同一时刻只有一个请求在飞。
     */
    suspend fun loadEarlier()

    /**
     * 刷新最新内容（可能追加到尾部，也可能什么都不变）。
     *
     * 流式输出时会被高频调用，实现方应自己节流。
     */
    suspend fun refresh()
}

/**
 * 列表里的一个条目。
 *
 * **key 必须稳定且唯一**：组件用它做锚点、做 LazyColumn 的 key。
 * 用序号/时间戳这类会变的值会导致滚动位置错乱。
 */
data class ChatItem(
    val key: String,
    /** 视觉高度类型：用来在测试里制造"不等高"，验证条目内偏移也保住了。 */
    val heightHint: HeightHint = HeightHint.Normal,
    val text: String = key,
)

/** 条目高度提示（测试用；真实数据不需要关心）。 */
enum class HeightHint { Normal, Tall, Short }

/**
 * 把一个 [ChatDataSource] 接到 Compose：返回会被自动重组的列表与状态。
 *
 * 真实实现（网络数据）与测试桩都通过它接入，组件侧代码完全一样。
 */
@Composable
fun rememberChatData(source: ChatDataSource): ChatDataState {
    // 用 mutableStateOf 包一层：让 source 的普通字段变化也能触发重组。
    // 实现方不必依赖 Compose 的 State 类型。
    var snapshot by remember(source) {
        mutableStateOf(
            ChatDataSnapshot(
                items = source.items,
                hasMore = source.hasMore,
                loading = source.loading,
            )
        )
    }

    // 轮询：简单可靠，避免接口里塞 Flow 让实现方复杂化。
    // 16ms 一帧的量级足够，且只在内容真的变了时才更新 state。
    LaunchedEffect(source) {
        while (true) {
            val next = ChatDataSnapshot(
                items = source.items,
                hasMore = source.hasMore,
                loading = source.loading,
            )
            if (next != snapshot) snapshot = next
            delay(16)
        }
    }

    return ChatDataState(
        items = snapshot.items,
        hasMore = snapshot.hasMore,
        loading = snapshot.loading,
        loadEarlier = { source.loadEarlier() },
        refresh = { source.refresh() },
    )
}

/** 供 UI 使用的数据快照 + 操作。 */
class ChatDataState(
    val items: List<ChatItem>,
    val hasMore: Boolean,
    val loading: Boolean,
    val loadEarlier: suspend () -> Unit,
    val refresh: suspend () -> Unit,
)

private data class ChatDataSnapshot(
    val items: List<ChatItem>,
    val hasMore: Boolean,
    val loading: Boolean,
)