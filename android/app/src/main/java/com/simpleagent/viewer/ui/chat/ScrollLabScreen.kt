package com.simpleagent.viewer.ui.chat

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import kotlinx.coroutines.launch

/**
 * 滚动组件的验证页：**只依赖 [ChatDataSource] 接口**，不碰网络。
 *
 * 打开方式：
 * ```
 * adb shell am start -e screen scrolllab
 * ```
 *
 * 设计目的：滚动问题在真实数据里排查非常困难（网络、分页、状态机混在
 * 一起）。这里换成可控的假数据，组件代码完全一样，问题就只剩两种可能：
 * 组件本身，或数据源违反了接口语义。
 *
 * 真机验证的三个场景（也是三个按钮）：
 *   1. 头部插入 60 条 → 用户正在看的那条必须分毫不动
 *   2. 末尾追加 → 用户在中间时不动；在底部时跟随
 *   3. 跳到底部 → 准确停在最后一条
 */
@Composable
fun ScrollLabScreen(
    /** 数据源；默认用假数据，真实数据源可直接替换进来。 */
    makeSource: () -> ChatDataSource = { FakeChatDataSource() },
) {
    val source = remember { makeSource() }
    val data = rememberChatData(source)
    val listState = rememberLazyListState()
    val controller = remember(listState) { ChatScrollController(listState, HEADER_KEY) }
    val scope = rememberCoroutineScope()

    var action by remember { mutableStateOf("（尚未操作）") }

    // 把"完整列表的 key"喂给控制器：它据此在数据变化后查到锚点的新下标，
    // 不依赖 LazyColumn 是否把锚点渲染出来了（一次插 60 条时它会被挤出屏幕）。
    //
    // 用 rememberUpdatedState 而不是把 keys 放进 LaunchedEffect 的 key：
    // 后者会在每次数据变化时重启 observe()，控制器内部状态就丢了。
    val latestKeys = rememberUpdatedState(data.items.map { it.key as Any })
    LaunchedEffect(listState) {
        controller.observe { latestKeys.value }
    }
    // 加载状态同步给控制器：加载期间冻结锚点，避免 header 高度变化把
    // 坐标系带偏（"按钮 ↔ 转圈"会让内容区整体上下移动）。
    LaunchedEffect(data.loading) {
        controller.setLoading(data.loading)
    }

    Column(Modifier.fillMaxSize()) {
        Column(Modifier.padding(12.dp)) {
            Text("滚动组件验证", fontSize = 16.sp, fontWeight = FontWeight.Bold)
            Text(
                "滑到中间某条（记住编号）→ 按「头部插入 60」→ 看那条是否还在原处。\n" +
                    "60 条 = 真实补拉的量，会把锚点挤出屏幕，专门验证降级路径。",
                fontSize = 11.sp, color = Color.Gray,
                modifier = Modifier.padding(top = 4.dp),
            )
            Text(
                "最近操作：$action",
                fontSize = 12.sp, color = Color(0xFF1976D2),
                modifier = Modifier.padding(top = 6.dp),
            )
        }

        Row(
            Modifier.padding(horizontal = 8.dp),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            Button(onClick = {
                scope.launch {
                    source.loadEarlier()
                    action = "头部插入完成（现 ${data.items.size} 条）"
                }
            }) { Text("头部插入 60", fontSize = 12.sp) }

            Button(onClick = {
                scope.launch {
                    source.refresh()
                    action = "末尾追加 1 条（现 ${data.items.size} 条）"
                }
            }) { Text("末尾追加", fontSize = 12.sp) }
        }

        Row(Modifier.padding(8.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            TextButton(onClick = {
                scope.launch {
                    (source as? FakeChatDataSource)?.reset()
                    controller.jumpToBottom()
                    action = "已重置（${data.items.size} 条）"
                }
            }) { Text("重置", fontSize = 12.sp) }

            TextButton(onClick = {
                scope.launch {
                    controller.jumpToBottom()
                    action = "跳到底部"
                }
            }) { Text("跳到底部", fontSize = 12.sp) }
        }

        // 固定的"加载更早"提示：**不进 LazyColumn**，所以不参与滚动，
        // 也不会污染 scrollToItem 的 offset 参照系。
        HeaderRow(
            loading = data.loading,
            hasMore = data.hasMore,
            onLoad = {
                scope.launch {
                    source.loadEarlier()
                    action = "头部插入完成（现 ${data.items.size} 条）"
                }
            },
        )

        LazyColumn(state = listState, modifier = Modifier.fillMaxSize()) {
            // **header 不进 LazyColumn**：它固定在列表上方。
            //
            // 以前把"加载更早"提示当成列表第一项，导致 scrollToItem 的
            // offset 参照系里混着 header 高度：想让某条内容停在 -2px 时，
            // LazyColumn 只能把它压在 header 下方（实测卡在 162px 左右），
            // 怎么补都收敛不到目标位置 —— 真机上表现为"位置总是差一点"。
            items(data.items, key = { it.key }) { item ->
                ItemRow(item)
            }
        }
    }
}

private const val HEADER_KEY = "chat-header"

@Composable
private fun HeaderRow(loading: Boolean, hasMore: Boolean, onLoad: suspend () -> Unit) {
    val scope = rememberCoroutineScope()
    Box(
        Modifier.fillMaxWidth().height(44.dp),
        contentAlignment = Alignment.Center,
    ) {
        when {
            loading -> Row(
                horizontalArrangement = Arrangement.spacedBy(6.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                CircularProgressIndicator(Modifier.size(12.dp), strokeWidth = 2.dp)
                Text("正在加载更早内容…", fontSize = 11.sp, color = Color.Gray)
            }
            hasMore -> TextButton(onClick = { scope.launch { onLoad() } }) {
                Text("↑ 加载更早内容", fontSize = 11.sp)
            }
            else -> Text("已经是最早的内容了", fontSize = 11.sp, color = Color.Gray)
        }
    }
}

@Composable
private fun ItemRow(item: ChatItem) {
    val height = when (item.heightHint) {
        HeightHint.Tall -> 160.dp
        HeightHint.Short -> 36.dp
        HeightHint.Normal -> 60.dp
    }
    // 用 key 的哈希交替配色，方便肉眼比对"是不是同一条"
    val even = item.key.hashCode() % 2 == 0
    Box(
        Modifier
            .fillMaxWidth()
            .height(height)
            .padding(2.dp)
            .background(if (even) Color(0xFFE3F2FD) else Color(0xFFF5F5F5)),
        contentAlignment = Alignment.Center,
    ) {
        Text(item.text, fontSize = 14.sp)
    }
}