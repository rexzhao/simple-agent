package com.simpleagent.viewer.ui.chat

import androidx.compose.foundation.lazy.LazyListState
import androidx.compose.runtime.Stable
import androidx.compose.runtime.snapshotFlow
import androidx.compose.runtime.withFrameNanos
import kotlinx.coroutines.flow.collect

/**
 * 聊天列表滚动位置的**唯一权威**。
 *
 * ## 为什么需要它
 *
 * 以前滚动逻辑散在 6 个 `LaunchedEffect` 里，每个都可能调 `scrollToItem`：
 * 加载更早内容、贴底跟随、首屏跳末尾、滚动补偿……谁后执行看运气。
 * 用户看到的就是"内容跳了，也不知道跳到哪"。
 *
 * 这里把决策收进一个地方，外部不再调 `scrollToItem`。
 *
 * ## 核心不变式
 *
 * **用户眼睛盯着的那条内容，必须留在屏幕上的原处。**
 *
 * 内容变化时按这个顺序判断：
 *   1. 条目数变多、且原来那条仍在屏幕上 → 按 key 精确还原（连条目内偏移）
 *   2. 条目数变多、原来那条被挤出屏幕 → 一次性插了很多条，按"插入数量"平移
 *   3. 用户本来就在底部 → 说明追加的是新消息，跟到底部
 */
@Stable
class ChatScrollController(
    val listState: LazyListState,
    /** 顶部状态行（"正在加载更早内容"）的 key。它不是内容，锚点要跳过。 */
    private val headerKey: Any = "chat-header",
) {
    private var lastCount = 0

    /** 用户正在看的那一条（key + 它被视口顶部裁掉的高度）。 */
    private var anchorKey: Any? = null
    private var anchorOffset = 0

    /** 用户是否停在底部附近。 */
    private var atBottom = true

    /**
     * 观察列表并维护滚动位置。**整个界面只调用一次**：
     *
     * ```
     * LaunchedEffect(listState) { controller.observe() }
     * ```
     */
    suspend fun observe() {
        snapshotFlow { readSnapshot() }.collect { snap ->
            if (snap.count == 0) {
                lastCount = 0
                anchorKey = null
                return@collect
            }

            val previousCount = lastCount
            val countGrew = snap.count > previousCount
            val countChanged = snap.count != previousCount
            lastCount = snap.count

            // 情况 A：条目数没变 —— 纯滚动或布局微调，只更新锚点
            if (!countChanged) {
                if (snap.firstKey != null) {
                    anchorKey = snap.firstKey
                    anchorOffset = snap.firstOffset
                }
                atBottom = snap.lastIndex >= snap.count - 2
                return@collect
            }

            // 情况 B：条目数变少 —— 切会话/清空，不补偿（交给跳末尾逻辑）
            if (!countGrew) return@collect

            // 情况 C：条目数变多 —— 内容被插入，需要决定停在哪
            settle(snap, previousCount)
        }
    }

    /**
     * 内容插入后的落位。
     *
     * 先试着按 key 找回"用户原来那条"；找不到就按插入数量平移。
     * 唯独**用户本来就在底部**时例外 —— 那说明新增的是新消息，应该跟随。
     */
    private suspend fun settle(snap: Snapshot, previousCount: Int) {
        val key = anchorKey

        // 按 key 精确定位（最多等 5 帧让新布局生效）
        if (key != null) {
            var tries = 0
            while (tries < 5) {
                withFrameNanos { }
                val live = readSnapshot()
                val target = live.visibleFirstIndexOf(key)
                if (target >= 0) {
                    if (atBottom) {
                        // 用户原本在底部：新增的是新消息，跟随到底
                        listState.scrollToItem(live.count - 1)
                    } else {
                        listState.scrollToItem(target, anchorOffset)
                    }
                    return
                }
                tries++
            }
        }

        // 锚点被挤出屏幕（一次插了很多条）：按插入数量平移
        val inserted = snap.count - previousCount
        if (inserted <= 0) return
        if (atBottom) {
            listState.scrollToItem(snap.count - 1)
            return
        }
        // 插入前用户看的是"当前 firstIndex - inserted"那条，
        // 也就是说现在应该把它推回到 firstIndex 位置
        listState.scrollToItem(snap.firstIndex.coerceIn(0, snap.count - 1), snap.firstOffset)
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
    suspend fun scrollToBottom() {
        jumpToBottom()
    }

    // ---- 内部 ----

    private fun readSnapshot(): Snapshot {
        val info = listState.layoutInfo
        val firstContent = info.visibleItemsInfo.firstOrNull { it.key != headerKey }
        return Snapshot(
            count = info.totalItemsCount,
            firstKey = firstContent?.key,
            firstIndex = firstContent?.index ?: 0,
            firstOffset = firstContent?.offset ?: 0,
            lastIndex = info.visibleItemsInfo.lastOrNull()?.index ?: -1,
        )
    }

    private fun Snapshot.visibleFirstIndexOf(key: Any): Int =
        listState.layoutInfo.visibleItemsInfo.firstOrNull { it.key == key }?.index ?: -1

    internal data class Snapshot(
        val count: Int,
        val firstKey: Any?,
        val firstIndex: Int,
        val firstOffset: Int,
        val lastIndex: Int,
    )
}
