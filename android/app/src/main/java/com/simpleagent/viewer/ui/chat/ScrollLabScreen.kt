package com.simpleagent.viewer.ui.chat

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.material3.Button
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateListOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import kotlinx.coroutines.launch

/**
 * 滚动组件的**独立验证页**：不接真实数据，用假数据验证行为。
 *
 * 为什么单独做一页：真实数据里混着网络、分页、状态机，出问题时分不清是
 * 组件本身的问题还是数据接入的问题。这里只有确定性的假数据。
 *
 * 验证三件事：
 *   1. 往头部插内容 → 用户看到的那条**位置不变**
 *   2. 往末尾追加内容 → 用户本来在底部时跟着走，不在底部时不动
 *   3. 跳到底部 → 准确停在最后一条
 */
@Composable
fun ScrollLabScreen() {
    val listState = rememberLazyListState()
    val controller = remember { ChatScrollController(listState) }
    val scope = rememberCoroutineScope()

    // 假数据：每条带序号，肉眼能看出"我原来看到的是几号"。
    //
    // **必须给足够多**（120 条）：内容不足一屏时"用户在底部"恒成立，
    // 末尾追加就会跟随到底——那是正确行为，但验证不出"在中间读历史
    // 不该被拽走"这个关键场景。
    val items = remember { mutableStateListOf<Int>().apply { addAll(100 until 220) } }
    var topSeq by remember { mutableStateOf(100) }
    var bottomSeq by remember { mutableStateOf(220) }
    var action by remember { mutableStateOf("（尚未操作）") }

    LaunchedEffect(listState) { controller.observe() }

    Column(Modifier.fillMaxSize()) {
        Text(
            "滚动组件验证",
            fontSize = 16.sp, fontWeight = FontWeight.Bold,
            modifier = Modifier.padding(12.dp),
        )
        Text(
            "操作：滑到中间某条（记住编号），然后按「头部插入 60 条」，\n" +
                "看那条编号是否还停在原处。\n" +
                "（60 条是真实补拉的量：会把锚点挤出屏幕，专门验证这段路径）",
            fontSize = 12.sp, color = Color.Gray,
            modifier = Modifier.padding(horizontal = 12.dp),
        )
        Text(
            "最近操作：$action",
            fontSize = 12.sp, color = Color(0xFF1976D2),
            modifier = Modifier.padding(horizontal = 12.dp, vertical = 6.dp),
        )

        Row(Modifier.padding(8.dp)) {
            Button(onClick = {
                // 头部插入：编号比当前最小值还小
                val batch = (topSeq - 60 until topSeq).toList()
                topSeq -= 60
                items.addAll(0, batch)
                action = "头部插入 ${batch.size} 条（${batch.first()}..${batch.last()}）"
            }) { Text("头部插入 60", fontSize = 12.sp) }

            Button(onClick = {
                val batch = (bottomSeq until bottomSeq + 10).toList()
                bottomSeq += 10
                items.addAll(batch)
                action = "末尾追加 ${batch.size} 条"
            }, modifier = Modifier.padding(start = 8.dp)) { Text("末尾追加 10", fontSize = 12.sp) }
        }

        Row(Modifier.padding(horizontal = 8.dp)) {
            Button(onClick = {
                topSeq = 100
                bottomSeq = 220
                items.clear()
                items.addAll(100 until 220)
                action = "重置为 100..219"
            }) { Text("重置", fontSize = 12.sp) }

            Button(onClick = {
                scope.launch { controller.jumpToBottom() }
                action = "跳到底部"
            }, modifier = Modifier.padding(start = 8.dp)) { Text("跳到底部", fontSize = 12.sp) }
        }

        LazyColumn(state = listState, modifier = Modifier.fillMaxSize()) {
            item(key = "chat-header") {
                Box(
                    Modifier.fillMaxWidth().height(44.dp),
                    contentAlignment = Alignment.Center,
                ) {
                    Text("↑ 顶部状态行（不是内容）", fontSize = 11.sp, color = Color.Gray)
                }
            }
            items(items, key = { it }) { n ->
                Box(
                    Modifier
                        .fillMaxWidth()
                        // 高度不等，才能验证"条目内偏移"也保住了
                        .height(if (n % 7 == 0) 160.dp else 60.dp)
                        .padding(4.dp)
                        .background(if (n % 2 == 0) Color(0xFFE3F2FD) else Color(0xFFF5F5F5)),
                    contentAlignment = Alignment.Center,
                ) {
                    Text("#$n", fontSize = 14.sp)
                }
            }
        }
    }
}
