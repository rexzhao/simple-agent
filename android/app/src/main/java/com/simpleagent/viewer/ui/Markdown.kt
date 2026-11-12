package com.simpleagent.viewer.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextDecoration
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.simpleagent.viewer.data.BlockView
import com.simpleagent.viewer.data.SpanView

/**
 * markdown 渲染。
 *
 * **块结构完全由 Go 解析**（internal/markdown，与桌面共用一份），这里只把结构画出来。
 * 这样手机和电脑的换行、列表、表格、行内代码样式不会各自长一套。
 */
@Composable
fun MarkdownBlocks(
    blocks: List<BlockView>,
    modifier: Modifier = Modifier,
    onImageClick: ((String) -> Unit)? = null,
) {
    Column(modifier = modifier, verticalArrangement = Arrangement.spacedBy(8.dp)) {
        blocks.forEach { block -> MarkdownBlock(block, onImageClick) }
    }
}

@Composable
private fun MarkdownBlock(block: BlockView, onImageClick: ((String) -> Unit)?) {
    when (block.kind) {
        "heading" -> Text(
            text = spansToAnnotated(block.spans),
            fontSize = when (block.level) {
                1 -> 20.sp
                2 -> 18.sp
                3 -> 16.sp
                else -> 15.sp
            },
            fontWeight = FontWeight.SemiBold,
            color = ViewerColors.muted.let { androidx.compose.material3.MaterialTheme.colorScheme.onBackground },
        )

        "paragraph" -> SelectionContainer {
            Text(
                text = spansToAnnotated(block.spans),
                fontSize = 15.sp,
                lineHeight = 22.sp,
                color = androidx.compose.material3.MaterialTheme.colorScheme.onBackground,
            )
        }

        "code" -> CodeBlock(block.code, block.lang)

        "list" -> Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
            block.items.forEachIndexed { index, itemBlocks ->
                Row(modifier = Modifier.fillMaxWidth()) {
                    Text(
                        text = if (block.ordered) "${block.start + index}." else "•",
                        fontSize = 15.sp,
                        color = ViewerColors.muted,
                        modifier = Modifier.padding(end = 8.dp),
                    )
                    Column(modifier = Modifier.weight(1f)) {
                        itemBlocks.forEach { MarkdownBlock(it, onImageClick) }
                    }
                }
            }
        }

        "quote" -> Row(modifier = Modifier.fillMaxWidth()) {
            // 左侧竖线用一条窄背景模拟（Compose 的 Border 在暗色下不够明显）
            Column(
                modifier = Modifier
                    .padding(end = 10.dp)
                    .background(ViewerColors.muted.copy(alpha = 0.4f), RoundedCornerShape(1.dp))
                    .padding(horizontal = 1.dp, vertical = 0.dp),
            ) { Text(text = "", fontSize = 1.sp) }
            Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
                block.blocks.forEach { MarkdownBlock(it, onImageClick) }
            }
        }

        "divider" -> Column(
            modifier = Modifier
                .fillMaxWidth()
                .padding(vertical = 4.dp)
                .background(ViewerColors.muted.copy(alpha = 0.3f), RoundedCornerShape(1.dp))
                .padding(vertical = 0.5.dp),
        ) { Text(text = "", fontSize = 1.sp) }

        "table" -> MarkdownTable(block)
    }
}

@Composable
private fun MarkdownTable(block: BlockView) {
    // 表格用横向滚动兜底：手机上列多时不能硬挤，也不能截断内容。
    Column(
        modifier = Modifier
            .fillMaxWidth()
            .horizontalScroll(rememberScrollState())
            .background(ViewerColors.surfaceHigh, RoundedCornerShape(8.dp))
            .padding(8.dp),
        verticalArrangement = Arrangement.spacedBy(6.dp),
    ) {
        Row {
            block.header.forEach { cell ->
                Text(
                    text = spansToAnnotated(cell),
                    fontSize = 13.sp,
                    fontWeight = FontWeight.SemiBold,
                    modifier = Modifier.padding(end = 16.dp),
                )
            }
        }
        block.rows.forEach { row ->
            Row {
                row.forEach { cell ->
                    Text(
                        text = spansToAnnotated(cell),
                        fontSize = 13.sp,
                        modifier = Modifier.padding(end = 16.dp),
                    )
                }
            }
        }
    }
}

@Composable
fun CodeBlock(code: String, lang: String) {
    Column(
        modifier = Modifier
            .fillMaxWidth()
            .background(ViewerColors.codeBg, RoundedCornerShape(8.dp))
            .padding(PaddingValues(horizontal = 10.dp, vertical = 8.dp)),
    ) {
        if (lang.isNotEmpty()) {
            Text(text = lang, fontSize = 11.sp, color = ViewerColors.muted)
        }
        SelectionContainer {
            Text(
                text = code,
                fontFamily = FontFamily.Monospace,
                fontSize = 13.sp,
                lineHeight = 19.sp,
                color = Color(0xFFD6E2EF),
                modifier = Modifier
                    .fillMaxWidth()
                    .horizontalScroll(rememberScrollState()),
                softWrap = false,
            )
        }
    }
}

/**
 * 把行内 span 转成 AnnotatedString（粗体/斜体/删除线/行内代码/链接）。
 *
 * **结果按 spans 缓存**：滚动时每个可见条目都会重组，而 Text 只有在
 * AnnotatedString 是同一个实例时才能跳过重新测量与排版。以前每帧都重建，
 * 向上滚较远距离时会明显卡（一屏十几个段落 × 每段几十个 span）。
 *
 * spans 是 data class 列表，equals 可靠，可以直接作为 remember 的 key。
 */
@Composable
fun spansToAnnotated(spans: List<SpanView>): AnnotatedString {
    // 颜色是 @Composable 的，先在组合里取出来当 key 的一部分
    val muted = ViewerColors.muted
    val accent = ViewerColors.accent
    val codeBg = ViewerColors.codeBg
    return remember(spans, muted, accent, codeBg) {
        buildAnnotatedString {
            spans.forEach { span ->
                val start = length
                append(span.text)
                val end = length
                if (end <= start) return@forEach
                if (span.bold) addStyle(SpanStyle(fontWeight = FontWeight.SemiBold), start, end)
                if (span.italic) addStyle(SpanStyle(fontStyle = FontStyle.Italic), start, end)
                if (span.strike) addStyle(SpanStyle(textDecoration = TextDecoration.LineThrough), start, end)
                if (span.code) {
                    addStyle(
                        SpanStyle(fontFamily = FontFamily.Monospace, background = codeBg),
                        start,
                        end,
                    )
                }
                if (span.link.isNotEmpty()) {
                    addStyle(SpanStyle(color = accent, textDecoration = TextDecoration.Underline), start, end)
                }
                if (span.imageUrl.isNotEmpty()) {
                    addStyle(SpanStyle(color = muted, fontStyle = FontStyle.Italic), start, end)
                }
            }
        }
    }
}
