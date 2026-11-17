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
 * ## 核心思路（一句话）
 *
 * **数据到了之后，把"用户正在看的那一条"重新放回原来的屏幕位置。**
 *
 * 就这一件事。所有复杂度都来自"怎么把这一件事做准"，而不是来自
 * "要不要补偿""补偿多少"的各种规则。
 *
 * ## 具体做法
 *
 * 1. 记住锚点：用户在滚动时，记下**第一条可见的内容项**的 key，
 *    以及它相对视口顶部的偏移（负值 = 顶部被裁掉多少）。
 * 2. 数据变了之后，拿这个 key 在**完整列表**里查它的新下标 ——
 *    列表是数据源给的，不依赖 LazyColumn 有没有把它渲染出来。
 *    这就是为什么不需要"估算插入条数"：查一下 index 就有了。
 * 3. `scrollToItem(newIndex, 原偏移)`，一条命令搞定。
 *
 * ## 为什么以前会跳
 *
 * 之前的实现只在 `LazyColumn` 的 `visibleItemsInfo` 里找锚点。
 * 一次补拉插 60 条，锚点必然被挤出可见区 → 找不到 → 退化成
 * "按插入条数估算"，而估算用的是插入**之后**读到的 firstIndex（通常是 0），
 * 于是滚到了顶部。真机上"加载完跳到接近顶部"就是这么来的。
 *
 * 数据源本来就有完整列表，查 index 是 O(n) 的一次线性扫描，
 * 比任何估算都准，也不需要维护"插了多少条"这类会失效的中间状态。
 */
@Stable
class ChatScrollController(
    val listState: LazyListState,
    /** 顶部状态行（"正在加载更早内容"）的 key。它不是内容，锚点要跳过。 */
    private val headerKey: Any = "chat-header",
) {
    /** 上一次看到的列表快照，用来判断"数据变了"。 */
    private var lastKeys: List<Any> = emptyList()

    /**
     * 锚点：用户正在看的那一条。
     *
     * [offset] 是该条**顶部相对视口顶部**的偏移（≤0 表示顶部被裁掉）。
     * 恢复时原样带回去，这样即使那一条是长消息、只露出下半截，也停在原处。
     */
    private var anchorKey: Any? = null
    private var anchorOffset = 0

    /** 用户是否停在底部附近。 */
    private var atBottom = true

    /**
     * 数据加载期间冻结锚点更新。
     *
     * 为什么：fetch 时顶部提示"按钮 → 转圈 → 已加载"会反复改变 header 高度，
     * 内容区跟着上下位移。如果这期间继续用"第一条可见项"更新锚点，
     * 记下来的偏移参照的是一个正在漂移的坐标系，等数据到了再拿它去恢复必然不准。
     *
     * 冻结后锚点保持"用户最后一次真正滚动时"看到的位置；
     * 数据到位那一帧再用当时的可见项做基准（见 restoreAnchor 的注释）。
     */
    private var frozen = false

    /** 外部（数据层）告知"开始加载" / "加载结束"。 */
    fun setLoading(loading: Boolean) {
        frozen = loading
    }

    /**
     * 观察列表并维护滚动位置。**整个界面只调用一次**：
     *
     * ```
     * LaunchedEffect(listState) { controller.observe() }
     * ```
     *
     * @param keysOf 返回"当前完整列表的 key 序列"。数据源变化后组件能立刻
     *   从这里查到锚点的新下标，不依赖 LazyColumn 是否渲染了它。
     */
    suspend fun observe(keysOf: () -> List<Any>) {
        snapshotFlow { readSnapshot() }.collect { snap ->
            val keys = keysOf()
            val previousKeys = lastKeys
            lastKeys = keys

            if (keys.isEmpty()) {
                anchorKey = null
                return@collect
            }

            // 数据没变：只更新锚点与贴底状态（纯滚动）
            if (keys == previousKeys) {
                // **加载期间冻结锚点**：此时 header 高度在变（按钮 ↔ 转圈），
                // 内容区整体在漂，这期间记下的偏移不可靠。
                if (!frozen && snap.firstKey != null) {
                    anchorKey = snap.firstKey
                    anchorOffset = snap.firstOffset
                }
                atBottom = snap.lastIndex >= snap.count - 2
                return@collect
            }

            // **数据变了：此刻屏幕上第一个有效项就是基准**。
            //
            // 为什么不直接用之前记下的 anchorKey：
            // fetch 期间顶部提示会从"按钮"变成"转圈"、再变成"已加载"，
            // header 高度反复变化，内容区跟着上下位移。那段时间记下的
            // anchorOffset 参照的是一个已经不存在的位置。
            //
            // 而"数据刚到位这一帧"的可见项，正好就是用户此刻真正在看的东西 ——
            // 用它的 key 与偏移做基准，最准确，也不需要知道期间发生了什么。
            val baselineKey = snap.firstKey ?: anchorKey
            val baselineOffset = if (snap.firstKey != null) snap.firstOffset else anchorOffset

            restoreAnchor(keys, previousKeys, baselineKey, baselineOffset)
        }
    }

    /**
     * 数据变化后的落位。**唯一要做的事**：把基准项放回原来的屏幕位置。
     *
     * 三种情形，按优先级：
     *   1. 用户原本在底部 → 跟随到新的末尾（新消息，用户想看）
     *   2. 有基准项 → 在完整列表里查它的新下标，滚过去 + 恢复原偏移
     *   3. 没有基准（首屏）→ 什么都不做
     */
    private suspend fun restoreAnchor(
        keys: List<Any>,
        previousKeys: List<Any>,
        baselineKey: Any?,
        baselineOffset: Int,
    ) {
        if (atBottom) {
            withFrameNanos { }
            listState.scrollToItem((keys.size - 1).coerceAtLeast(0))
            return
        }

        val key = baselineKey ?: return
        // **关键**：在完整列表里查下标，而不是在可见项里找。
        // 一次插 60 条时锚点会被挤出屏幕，可见项里根本没有它；
        // 但完整列表里它一定在。
        val newIndex = keys.indexOf(key)
        if (newIndex < 0) {
            // 基准项在新列表里不存在（消息被删/切会话）：不动，交给用户
            return
        }

        withFrameNanos { }
        // 先按 index 对齐到视口顶部（offset=0 是 scrollToItem 唯一可靠的行为），
        // 再由 nudgeTo 用像素级 scrollBy 补出精确偏移。
        listState.scrollToItem(newIndex)
        nudgeTo(key, baselineOffset)
    }

    /**
     * 按实际布局把偏移校正到期望值。
     *
     * `scrollToItem(index, offset)` 在 offset 为负值时行为不稳定（会把目标
     * 项夹到视口顶部，实测差 40~160px 收敛不了）。所以这里改用两步：
     *   1. `scrollToItem(index)` 先把目标项对齐到视口顶部（offset=0）
     *   2. `scrollBy(-expectedOffset)` 把整个列表再推一下，得到精确偏移
     *
     * 为什么第 2 步可靠：`scrollBy` 是像素级滚动，不受 item 边界约束。
     */
    private suspend fun nudgeTo(key: Any, expectedOffset: Int) {
        repeat(3) {
            withFrameNanos { }
            val info = listState.layoutInfo.visibleItemsInfo.firstOrNull { it.key == key }
                ?: return  // 目标不在可见区：无法测量，放弃（位置已由 index 决定）
            val drift = info.offset - expectedOffset
            if (drift == 0) return
            android.util.Log.d(
                "SimpleAgent",
                "滚动落位微调：key=$key offset ${info.offset} → $expectedOffset（漂 $drift）",
            )
            // `scrollBy(positive)` = 内容向"更新"方向滚（用户往上滑，看到后面的内容），
            // 也就是 item 在屏幕上往上移动。
            //
            // drift = 当前偏移 - 目标偏移。drift > 0 表示目标项偏下，
            // 需要让它往上走 drift 像素 → scrollBy(+drift)。
            // 之前写成 -drift，结果每轮偏离翻倍（52 → 104 → 208），
            // 是典型的符号写反。
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
        // header 已经不在列表里（固定在列表上方），可见项全是内容
        val firstContent = info.visibleItemsInfo.firstOrNull()
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
