package com.simpleagent.viewer.ui.chat

import androidx.compose.foundation.lazy.LazyListState
import androidx.compose.runtime.Stable
import androidx.compose.runtime.snapshots.Snapshot
import androidx.compose.runtime.withFrameNanos

/**
 * 聊天列表滚动位置的**唯一权威**。
 *
 * ## 为什么以前会跳
 *
 * 旧实现在 `snapshotFlow { layoutInfo }` 里检测"数据变了"，再现取"此刻第一个
 * 可见内容项"作基准去恢复。这有两个致命问题：
 *
 * 1. **基准取晚了**。snapshotFlow 发射时，新数据**已经布局完了**。
 *    LazyColumn 自带"按首个可见项的 key 保持位置"，而"滑到顶才触发加载"
 *    意味着那一刻首个可见项正是顶部提示行（index 0）—— 于是 LazyColumn
 *    把提示行钉在顶部，新插入的内容直接出现在它下面，视野已经跳了。
 *    此时再取"第一个可见内容项"拿到的是**新插入的那一页**，恢复等于原地不动。
 * 2. **下标坐标系不对**。基准 key 在"timeline 列表"里的下标被直接拿去
 *    `scrollToItem`，但 LazyColumn 的下标前面还有顶部提示行和归档条目，
 *    差了 `1 + archive.size`。
 *
 * ## 现在的做法
 *
 * 在**组合提交后、新数据测量前**（[SideEffect][androidx.compose.runtime.SideEffect]）
 * 调用 [onKeysChanged]：此时 `layoutInfo` 还是旧数据的布局，就是用户眼前的画面。
 *
 * - 首个可见项是普通内容 → 什么都不做，LazyColumn 自带的 key 锚定就是对的
 *   （而且不会打断用户正在进行的惯性滑动）。
 * - 首个可见项是顶部提示行（或已被删掉）→ 改用它下面第一个仍存在的内容项
 *   作锚点，用 [LazyListState.requestScrollToItem] 让**下一次测量**直接落到
 *   原位置。没有"先跳过去再拉回来"的中间帧，所以不闪。
 * - 用户原本在底部且末尾有新增 → 跟随到底。
 */
@Stable
class ChatScrollController(
    val listState: LazyListState,
    /** 顶部状态行（"正在加载更早内容"）的 key。它不是内容，不能作锚点。 */
    private val headerKey: Any = "chat-header",
    /** 尾部占位项的 key（没有就传 null）。同样不是内容。 */
    private val footerKey: Any? = null,
) {
    /** 上一次同步的 key 序列（与 LazyColumn 的下标一一对应）。 */
    private var lastKeys: List<Any> = emptyList()

    /**
     * 同步列表 key。**必须放在 SideEffect 里调用**，并且 [keys] 必须与 LazyColumn
     * 的条目**逐个对应**（含顶部提示行、归档项、尾部占位）：
     *
     * ```
     * SideEffect { controller.onKeysChanged(allKeys) }
     * ```
     *
     * SideEffect 在组合提交之后、本帧测量之前执行 —— 这时 layoutInfo 仍是
     * 旧数据的布局，正好用来记基准；请求的位置会在紧接着的测量里生效。
     */
    fun onKeysChanged(keys: List<Any>) {
        val previous = lastKeys
        if (keys == previous) return
        lastKeys = keys
        if (previous.isEmpty() || keys.isEmpty()) return // 首次加载/切换会话：交给 jumpToBottom

        val info = Snapshot.withoutReadObservation { listState.layoutInfo }
        val visible = info.visibleItemsInfo
        if (visible.isEmpty()) return

        // 用户在底部、且末尾有新增 → 跟随
        val atBottom = visible.last().index >= info.totalItemsCount - 2
        if (atBottom && lastContentKey(keys) != lastContentKey(previous)) {
            listState.requestScrollToItem(keys.lastIndex)
            return
        }

        // 首个可见项是普通内容且仍存在：LazyColumn 自己会按 key 保持位置
        val first = visible.first()
        if (isContent(first.key) && keys.contains(first.key)) return

        // 否则找下面第一个仍存在的内容项，把它放回原来的屏幕位置
        for (item in visible) {
            if (!isContent(item.key)) continue
            val newIndex = keys.indexOf(item.key)
            if (newIndex < 0) continue
            if (newIndex == item.index) return // 前面什么都没插：不用动
            // scrollOffset = 该项顶部被滚出视口的像素；它在提示行下方时为负，
            // LazyColumn 测量时会向前补齐上面的条目。
            listState.requestScrollToItem(newIndex, -item.offset)
            return
        }
    }

    /** 首次打开会话：跳到末尾。 */
    suspend fun jumpToBottom() {
        withFrameNanos { }
        val count = listState.layoutInfo.totalItemsCount
        if (count == 0) return
        listState.scrollToItem(count - 1)
    }

    /** 贴底跟随（发消息后调用）。 */
    suspend fun scrollToBottom() = jumpToBottom()

    private fun isContent(key: Any): Boolean = key != headerKey && key != footerKey

    private fun lastContentKey(keys: List<Any>): Any? = keys.lastOrNull(::isContent)
}
