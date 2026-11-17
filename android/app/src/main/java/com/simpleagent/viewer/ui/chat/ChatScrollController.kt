package com.simpleagent.viewer.ui.chat

import androidx.compose.foundation.gestures.scrollBy
import androidx.compose.foundation.lazy.LazyListState
import androidx.compose.runtime.Stable
import androidx.compose.runtime.snapshotFlow
import androidx.compose.runtime.withFrameNanos
import kotlinx.coroutines.flow.collect

/**
 * 聊天列表滚动位置的**唯一权威**。
 *
 * ## 一句话
 *
 * **数据到达前，记下屏幕上第一个有效元素和它的偏移；数据到达后，把它放回原处。**
 *
 * 没有别的规则。不需要知道加载过程发生了什么，不需要知道插了多少条，
 * 也不需要"冻结"任何状态 —— 因为基准是在数据到达的那一刻取的，
 * 那一刻的屏幕就是用户真正看到的东西。
 *
 * ## 具体三步
 *
 * 1. **数据没变**（纯滚动）：持续记下第一个可见项的 key 与 offset
 * 2. **数据变了**（到达那一刻）：
 *    - 用**当前这一帧**的第一个可见项作基准（不是用历史记的值 ——
 *      加载期间顶部提示一直在变，历史值参照的坐标系已经不一样了）
 *    - 拿 key 在**完整列表**里查新下标
 *    - `scrollToItem(newIndex)` 对齐，再 `scrollBy` 补出精确偏移
 * 3. 用户原本在底部 → 跟随到底（新消息）
 */
@Stable
class ChatScrollController(
    val listState: LazyListState,
    /** 顶部状态行（"正在加载更早内容"）的 key。它不是内容，基准要跳过。 */
    private val headerKey: Any = "chat-header",
) {
    /** 上一次看到的列表 key 序列，用来判断"数据变了没有"。 */
    private var lastKeys: List<Any> = emptyList()

    /** 用户是否停在底部附近（决定新增内容要不要跟随）。 */
    private var atBottom = true

    /**
     * 观察列表并维护滚动位置。**整个界面只调用一次**：
     *
     * ```
     * LaunchedEffect(listState) { controller.observe { keys } }
     * ```
     *
     * @param keysOf 返回当前完整列表的 key 序列。数据变化后从这里查基准项的新下标，
     *   不依赖 LazyColumn 是否把基准项渲染出来了（一次补拉几十条时它会被挤出屏幕）。
     */
    suspend fun observe(keysOf: () -> List<Any>) {
        snapshotFlow { readSnapshot() }.collect { snap ->
            val keys = keysOf()
            val previousKeys = lastKeys
            lastKeys = keys

            if (keys.isEmpty()) {
                atBottom = true
                return@collect
            }

            if (keys == previousKeys) {
                // 数据没变：唯一要做的是记住"用户在不在底部"。
                // 这是滚动的副产品，不需要额外记录锚点 —— 锚点在数据
                // 变化那一刻现取（见下面），记历史值没有用处。
                atBottom = snap.lastIndex >= snap.count - 2
                return@collect
            }

            // 数据变了：**基准就是此刻屏幕上第一个有效项**。
            // 不依赖任何历史记录 —— 加载期间顶部提示在变、坐标系在漂，
            // 历史值不可信；而"刚到达这一帧"的可见项就是用户真正在看的东西。
            val baselineKey = snap.firstKey ?: return@collect
            restore(baselineKey, snap.firstOffset)
        }
    }

    /**
     * 把基准项放回原来的屏幕位置。
     *
     * `scrollToItem(index, offset)` 在 offset 为负时行为不可靠（实测差 160px
     * 收敛不了），所以拆成两步：先对齐到顶部，再用像素级 scrollBy 补偏移。
     */
    private suspend fun restore(key: Any, offset: Int) {
        if (atBottom) {
            withFrameNanos { }
            listState.scrollToItem((listState.layoutInfo.totalItemsCount - 1).coerceAtLeast(0))
            return
        }

        val newIndex = lastKeys.indexOf(key)
        if (newIndex < 0) return  // 基准项不存在了（切换会话/删除）：不动

        withFrameNanos { }
        listState.scrollToItem(newIndex)

        // 补精确偏移：量一次实际值，差多少用 scrollBy 补多少。
        // scrollBy(正数) = 内容往"更新"方向滚（元素在屏幕上往上移）。
        repeat(3) {
            withFrameNanos { }
            val info = listState.layoutInfo.visibleItemsInfo.firstOrNull { it.key == key }
                ?: return  // 已经不在可见区：位置由 index 决定，不必再补
            val drift = info.offset - offset
            if (drift == 0) return
            listState.scrollBy(drift.toFloat())
        }
    }

    /** 首次打开会话：跳到末尾。 */
    suspend fun jumpToBottom() {
        withFrameNanos { }
        val count = listState.layoutInfo.totalItemsCount
        if (count == 0) return
        listState.scrollToItem(count - 1)
        atBottom = true
    }

    /** 贴底跟随（发消息后调用）。 */
    suspend fun scrollToBottom() = jumpToBottom()

    // ---- 内部 ----

    private fun readSnapshot(): Snapshot {
        val info = listState.layoutInfo
        val firstContent = info.visibleItemsInfo.firstOrNull { it.key != headerKey }
        return Snapshot(
            count = info.totalItemsCount,
            firstKey = firstContent?.key,
            firstOffset = firstContent?.offset ?: 0,
            lastIndex = info.visibleItemsInfo.lastOrNull()?.index ?: -1,
        )
    }

    internal data class Snapshot(
        val count: Int,
        val firstKey: Any?,
        val firstOffset: Int,
        val lastIndex: Int,
    )
}