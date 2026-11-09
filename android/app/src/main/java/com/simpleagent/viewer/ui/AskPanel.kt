package com.simpleagent.viewer.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Check
import androidx.compose.material3.Button
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.simpleagent.viewer.data.AskView
import com.simpleagent.viewer.data.QuestionView

/**
 * 提问面板（模型调 ask_user_question 时弹出）。
 *
 * 交互规则全部由 Go 的 AskGroup 决定（单选互斥、写文字就取消选项、手写答案优先）：
 * 这里点了就调 bridge，再把返回的最新提问列表画出来。**不在 Kotlin 里算这些规则**，
 * 否则手机上选的和电脑上选的会不一致。
 */
@Composable
fun AskPanel(
    asks: List<AskView>,
    tab: Int,
    onSelectTab: (Int) -> Unit,
    onToggleOption: (callId: String, questionIndex: Int, optionIndex: Int) -> Unit,
    onCustom: (callId: String, questionIndex: Int, text: String) -> Unit,
    onSubmit: () -> Unit,
    onDismiss: () -> Unit,
    modifier: Modifier = Modifier,
) {
    if (asks.isEmpty()) return

    // 摊平：一个 (组, 题) 一个页签，顺序与模型提问一致
    val entries = remember(asks) {
        asks.flatMap { ask ->
            ask.questions.mapIndexed { index, question ->
                Triple(ask, index, question)
            }
        }
    }
    if (entries.isEmpty()) return
    val current = tab.coerceIn(0, entries.size - 1)
    val (ask, questionIndex, question) = entries[current]
    val answered = entries.count { it.third.answered }

    // 手写答案的本地草稿（切页签/重绘时保留用户正在敲的内容）
    val drafts = remember { mutableStateMapOf<String, String>() }
    val draftKey = ask.callId + "#" + questionIndex
    val draft = drafts[draftKey] ?: question.custom

    Column(
        modifier = modifier
            .fillMaxWidth()
            .background(MaterialTheme.colorScheme.surface, RoundedCornerShape(topStart = 14.dp, topEnd = 14.dp))
            .padding(horizontal = 14.dp, vertical = 12.dp),
        verticalArrangement = Arrangement.spacedBy(10.dp),
    ) {
        // 标题 + 进度
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Text("需要你的回答", fontSize = 14.sp, fontWeight = FontWeight.SemiBold, color = ViewerColors.accent)
            Text("$answered / ${entries.size}", fontSize = 12.sp, color = ViewerColors.muted)
        }

        // 多题时显示页签（已答的带 ✓）
        if (entries.size > 1) {
            Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                entries.forEachIndexed { index, (_, _, item) ->
                    val active = index == current
                    val label = (if (item.answered) "✓ " else "") +
                        item.header.ifEmpty { "问题 ${index + 1}" }
                    Text(
                        text = label,
                        fontSize = 12.sp,
                        color = if (active) MaterialTheme.colorScheme.onSurface else ViewerColors.muted,
                        modifier = Modifier
                            .clip(RoundedCornerShape(6.dp))
                            .background(
                                if (active) ViewerColors.surfaceHigh else androidx.compose.ui.graphics.Color.Transparent,
                            )
                            .clickable { onSelectTab(index) }
                            .padding(horizontal = 8.dp, vertical = 4.dp),
                    )
                }
            }
        }

        // 题干
        Text(
            text = question.question + if (question.multiSelect) "（可多选）" else "",
            fontSize = 14.sp,
            lineHeight = 20.sp,
        )

        // 选项
        question.options.forEachIndexed { optionIndex, option ->
            val picked = question.selected.contains(optionIndex)
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .clip(RoundedCornerShape(8.dp))
                    .background(if (picked) ViewerColors.bubble else androidx.compose.ui.graphics.Color.Transparent)
                    .clickable { onToggleOption(ask.callId, questionIndex, optionIndex) }
                    .padding(horizontal = 10.dp, vertical = 9.dp),
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                // 单选 ○/◉，多选 ☐/☑
                Text(
                    text = when {
                        question.multiSelect && picked -> "☑"
                        question.multiSelect -> "☐"
                        picked -> "◉"
                        else -> "○"
                    },
                    fontSize = 14.sp,
                    color = if (picked) ViewerColors.accent else ViewerColors.muted,
                )
                Text(option, fontSize = 14.sp, lineHeight = 20.sp)
            }
        }

        // 手写答案：写了就以文字作答（Go 侧会清掉该题已选选项）
        OutlinedTextField(
            value = draft,
            onValueChange = { text ->
                drafts[draftKey] = text
                onCustom(ask.callId, questionIndex, text)
            },
            placeholder = { Text("或者自己写一个答案…", fontSize = 13.sp) },
            modifier = Modifier.fillMaxWidth(),
            maxLines = 3,
            keyboardOptions = KeyboardOptions(imeAction = ImeAction.Done),
        )
        if (question.custom.isNotEmpty() && !question.customActive) {
            Text(
                text = "手写答案未生效（已选择预设选项）",
                fontSize = 11.sp,
                color = ViewerColors.muted,
            )
        }

        Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
            TextButton(onClick = onDismiss) { Text("跳过", fontSize = 13.sp) }
            Button(onClick = onSubmit, enabled = answered > 0) {
                Icon(Icons.Filled.Check, contentDescription = null, modifier = Modifier.size(15.dp))
                Text("  提交", fontSize = 13.sp)
            }
            if (ask.answered) {
                Text("可以提交了", fontSize = 11.sp, color = ViewerColors.ok)
            }
        }
    }
}
