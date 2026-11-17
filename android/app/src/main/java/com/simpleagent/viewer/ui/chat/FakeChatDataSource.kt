package com.simpleagent.viewer.ui.chat

import kotlinx.coroutines.delay
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock

/**
 * 内存里的假数据源：实现 [ChatDataSource]，不碰网络。
 *
 * 用来在虚拟设备上验证滚动行为，也作为"可挂载 provider"的参考实现 ——
 * 真实数据源照着这个语义接进来即可。
 *
 * 默认参数按**真实补拉的规模**设置：
 * `pageSize = 60`，与 Go 侧 backfillPageLimit(300 事件) 转成的 item 数量级一致。
 * 之前验证页只插 20 条，不足以把锚点挤出屏幕，走的是理想的
 * "按 key 精确还原"路径，掩盖了真实的降级分支 —— 这里改成 60。
 */
class FakeChatDataSource(
    initialCount: Int = 120,
    private val pageSize: Int = 60,
    /** 模拟每次加载的耗时，用来观察"正在加载"的显示。 */
    private val loadDelayMs: Long = 300,
    /** 每次插入的条目高度模式；null 表示交替。 */
    private val heightPattern: ((Int) -> HeightHint)? = null,
) : ChatDataSource {

    private val mutex = Mutex()
    private val list = ArrayList<ChatItem>(initialCount)
    private var topSeq = 1000
    private var bottomSeq = 1000 + initialCount

    init {
        // 初始内容：序号连续
        for (i in 0 until initialCount) {
            list.add(makeItem(bottomSeq - initialCount + i))
        }
    }

    override val items: List<ChatItem> get() = synchronized(list) { list.toList() }

    @Volatile
    private var _hasMore = true
    override val hasMore: Boolean get() = _hasMore

    @Volatile
    private var _loading = false
    override val loading: Boolean get() = _loading

    /**
     * 加载更早的一批，插到**头部**。
     *
     * 互斥：组件可能因滚动事件重复触发，这里保证同一时刻只有一个在跑
     * （与真实实现里 ViewModel 用 AtomicBoolean 做的是同一件事）。
     */
    override suspend fun loadEarlier() {
        mutex.withLock {
            if (!_hasMore) return
            _loading = true
            try {
                delay(loadDelayMs)
                val batch = (topSeq - pageSize until topSeq).map { makeItem(it) }
                topSeq -= pageSize
                synchronized(list) { list.addAll(0, batch) }
                // 造一个"到底"的边界：总条数超过 500 就不再给
                if (synchronized(list) { list.size } > 500) _hasMore = false
            } finally {
                _loading = false
            }
        }
    }

    /** 追加新内容到尾部（模拟流式输出）。 */
    override suspend fun refresh() {
        mutex.withLock {
            val batch = (bottomSeq until bottomSeq + 1).map { makeItem(it) }
            bottomSeq += 1
            synchronized(list) { list.addAll(batch) }
        }
    }

    /** 仅供测试：模拟"用户发一条消息"。 */
    suspend fun appendOne() = refresh()

    /** 仅供测试：重置回初始状态。 */
    fun reset(initialCount: Int = 120) {
        synchronized(list) {
            list.clear()
            bottomSeq = 1000 + initialCount
            topSeq = 1000
            for (i in 0 until initialCount) list.add(makeItem(bottomSeq - initialCount + i))
        }
        _hasMore = true
    }

    private fun makeItem(seq: Int): ChatItem {
        val hint = heightPattern?.invoke(seq) ?: run {
            // 每 7 条一个高块：用来验证"条目内偏移"也被还原了，
            // 而不是碰巧对齐到了条目顶部。
            if (seq % 7 == 0) HeightHint.Tall else HeightHint.Normal
        }
        return ChatItem(key = "msg-$seq", heightHint = hint, text = "#$seq")
    }
}
