package com.simpleagent.viewer.ui

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ExpandLess
import androidx.compose.material.icons.filled.ExpandMore
import androidx.compose.material.icons.outlined.AutoAwesome
import androidx.compose.material.icons.outlined.Build
import androidx.compose.material.icons.outlined.EditNote
import androidx.compose.material.icons.outlined.Image
import androidx.compose.material.icons.outlined.Search
import androidx.compose.material.icons.outlined.Terminal
import androidx.compose.material.icons.outlined.Visibility
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.simpleagent.viewer.data.FooterView
import com.simpleagent.viewer.data.ItemView
import com.simpleagent.viewer.data.MessageView
import com.simpleagent.viewer.data.NoticeView
import com.simpleagent.viewer.data.ProcessStep
import com.simpleagent.viewer.data.ProcessView
import com.simpleagent.viewer.data.TimelineView

/**
 * 对话流：把 Go 算好的时间线条目逐条画出来。
 *
 * 分组规则（哪些内容进过程组、回合何时结束）已经在 Go 的 internal/timeline 里，
 * 这里只做"条目类型 → 布局"的映射，不做任何判断。
 */
@Composable
fun Timeline(
    view: TimelineView,
    expanded: Set<String>,
    onToggle: (String) -> Unit,
    onImagePath: (String, (String) -> Unit) -> Unit,
    modifier: Modifier = Modifier,
) {
    Column(
        modifier = modifier.padding(horizontal = 12.dp, vertical = 8.dp),
        verticalArrangement = Arrangement.spacedBy(10.dp),
    ) {
        view.items.forEach { item ->
            when (item.kind) {
                "content" -> item.message?.let { MessageRow(it, onImagePath) }
                "process" -> item.process?.let {
                    ProcessGroup(it, expanded = expanded.contains(item.key), onToggle = { onToggle(item.key) })
                }
                "notice" -> item.notice?.let { NoticeRow(it) }
                "footer" -> item.footer?.let { FooterRow(it) }
            }
        }
        if (view.runtime.compacting) {
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                CircularProgressIndicator(modifier = Modifier.size(12.dp), strokeWidth = 2.dp)
                Text("正在压缩上下文…", fontSize = 12.sp, color = ViewerColors.muted)
            }
        }
    }
}

@Composable
private fun MessageRow(message: MessageView, onImagePath: (String, (String) -> Unit) -> Unit) {
    if (message.isUser) {
        // 用户消息右对齐气泡
        Row(modifier = Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.End) {
            Column(
                modifier = Modifier
                    .weight(1f, fill = false)
                    .clip(RoundedCornerShape(14.dp))
                    .background(ViewerColors.bubble)
                    .padding(horizontal = 12.dp, vertical = 9.dp),
                verticalArrangement = Arrangement.spacedBy(6.dp),
            ) {
                message.parts.forEach { part ->
                    when (part.kind) {
                        "markdown" -> MarkdownBlocks(part.blocks)
                        "code" -> CodeBlock(part.text, part.lang)
                        "image" -> ImagePart(part.image?.blob.orEmpty(), part.image?.url.orEmpty(), onImagePath)
                        else -> Text(part.text, fontSize = 15.sp)
                    }
                }
            }
        }
        return
    }
    // 助手消息：正文占满宽度（不套气泡，和桌面一致，便于长文阅读）
    Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
        message.parts.forEach { part ->
            when (part.kind) {
                "markdown" -> MarkdownBlocks(part.blocks)
                "code" -> CodeBlock(part.text, part.lang)
                "summary" -> SummaryBlock(part.text)
                "image" -> ImagePart(part.image?.blob.orEmpty(), part.image?.url.orEmpty(), onImagePath)
                "text" -> Text(part.text, fontSize = 15.sp)
                "file" -> Text("📎 ${part.name}", fontSize = 13.sp, color = ViewerColors.muted)
            }
        }
        if (message.status == "failed") {
            Text(
                text = "处理失败：${message.reason}",
                fontSize = 13.sp,
                color = ViewerColors.danger,
            )
        }
        if (message.status == "streaming" || message.status == "pending") {
            Text("正在生成…", fontSize = 12.sp, color = ViewerColors.muted)
        }
    }
}

@Composable
private fun SummaryBlock(text: String) {
    Column(
        modifier = Modifier
            .fillMaxWidth()
            .background(ViewerColors.surfaceHigh, RoundedCornerShape(8.dp))
            .padding(10.dp),
        verticalArrangement = Arrangement.spacedBy(4.dp),
    ) {
        Text("上下文摘要", fontSize = 11.sp, color = ViewerColors.muted)
        Text(text, fontSize = 13.sp, lineHeight = 20.sp)
    }
}

@Composable
private fun ImagePart(blob: String, url: String, onImagePath: (String, (String) -> Unit) -> Unit) {
    var path by remember(blob) { mutableStateOf<String?>(null) }
    var failed by remember(blob) { mutableStateOf(false) }
    if (blob.isNotEmpty()) {
        remember(blob) {
            onImagePath(blob) { result -> if (result.isEmpty()) failed = true else path = result }
            true
        }
        val current = path
        when {
            current != null -> ZoomableImage(path = current)
            failed -> Text("[图片丢失]", fontSize = 13.sp, color = ViewerColors.muted)
            else -> Row(
                horizontalArrangement = Arrangement.spacedBy(6.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                CircularProgressIndicator(modifier = Modifier.size(12.dp), strokeWidth = 2.dp)
                Text("正在加载图片…", fontSize = 12.sp, color = ViewerColors.muted)
            }
        }
    } else if (url.isNotEmpty()) {
        Text("[远程图片] $url", fontSize = 12.sp, color = ViewerColors.muted)
    }
}

@Composable
private fun NoticeRow(notice: NoticeView) {
    val color = when (notice.level) {
        "error" -> ViewerColors.danger
        "warn" -> ViewerColors.warn
        else -> ViewerColors.muted
    }
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .background(color.copy(alpha = 0.10f), RoundedCornerShape(8.dp))
            .padding(horizontal = 10.dp, vertical = 7.dp),
    ) {
        Text(notice.text, fontSize = 12.sp, color = color)
    }
}

@Composable
private fun FooterRow(footer: FooterView) {
    val color = when (footer.status) {
        "failed" -> ViewerColors.danger
        "cancelled" -> ViewerColors.warn
        else -> ViewerColors.muted
    }
    val label = when {
        footer.status == "failed" && footer.reason.isNotEmpty() -> "处理失败 · ${footer.reason}"
        footer.status == "failed" -> "处理失败"
        footer.status == "cancelled" -> "已中止"
        footer.durationMs >= 1000 -> "已完成，用时 ${formatDuration(footer.durationMs)}"
        else -> "已完成"
    }
    Row(
        horizontalArrangement = Arrangement.spacedBy(8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(
            modifier = Modifier
                .background(color.copy(alpha = 0.18f), RoundedCornerShape(6.dp))
                .padding(horizontal = 8.dp, vertical = 3.dp),
        ) {
            Text(label, fontSize = 11.sp, color = color)
        }
        if (footer.tokens > 0) {
            Text("${footer.tokens} tokens", fontSize = 11.sp, color = ViewerColors.muted)
        }
    }
}

/** 过程组：折叠标题 + 展开后的思考/工具卡片。 */
@Composable
private fun ProcessGroup(process: ProcessView, expanded: Boolean, onToggle: () -> Unit) {
    Column(
        modifier = Modifier
            .fillMaxWidth()
            .clip(RoundedCornerShape(10.dp))
            .background(ViewerColors.surfaceHigh.copy(alpha = 0.6f)),
    ) {
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .clickable(onClick = onToggle)
                .padding(horizontal = 10.dp, vertical = 8.dp),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Icon(
                imageVector = activityIcon(process.activity),
                contentDescription = null,
                tint = ViewerColors.muted,
                modifier = Modifier.size(15.dp),
            )
            Text(
                text = process.title,
                fontSize = 12.sp,
                color = ViewerColors.muted,
                modifier = Modifier.weight(1f),
            )
            if (process.streaming) {
                CircularProgressIndicator(modifier = Modifier.size(12.dp), strokeWidth = 2.dp)
            }
            Icon(
                imageVector = if (expanded) Icons.Filled.ExpandLess else Icons.Filled.ExpandMore,
                contentDescription = null,
                tint = ViewerColors.muted,
                modifier = Modifier.size(16.dp),
            )
        }
        AnimatedVisibility(visible = expanded) {
            Column(
                modifier = Modifier.padding(start = 10.dp, end = 10.dp, bottom = 10.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                process.steps.forEach { step ->
                    when (step.kind) {
                        "reasoning" -> ReasoningStep(step)
                        else -> ToolStep(step)
                    }
                }
            }
        }
    }
}

@Composable
private fun ReasoningStep(step: ProcessStep) {
    Column(verticalArrangement = Arrangement.spacedBy(3.dp)) {
        Text("思考", fontSize = 11.sp, color = ViewerColors.muted)
        Text(
            text = step.text,
            fontSize = 13.sp,
            lineHeight = 19.sp,
            color = ViewerColors.muted,
            fontStyle = androidx.compose.ui.text.font.FontStyle.Italic,
        )
    }
}

@Composable
private fun ToolStep(step: ProcessStep) {
    var showArgs by remember(step.toolId) { mutableStateOf(false) }
    val statusColor = when {
        step.isError -> ViewerColors.danger
        step.pending -> ViewerColors.warn
        step.done -> ViewerColors.ok
        else -> ViewerColors.muted
    }
    Column(
        modifier = Modifier
            .fillMaxWidth()
            .background(ViewerColors.codeBg.copy(alpha = 0.7f), RoundedCornerShape(8.dp))
            .padding(8.dp),
        verticalArrangement = Arrangement.spacedBy(5.dp),
    ) {
        Row(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
            Icon(
                imageVector = Icons.Outlined.Build,
                contentDescription = null,
                tint = statusColor,
                modifier = Modifier.size(13.dp),
            )
            Text(step.toolTitle, fontSize = 12.sp, fontWeight = FontWeight.Medium, color = statusColor)
            Text(
                text = when {
                    step.isError -> "失败"
                    step.pending -> "等待结果"
                    step.done -> "完成"
                    else -> "运行中"
                },
                fontSize = 11.sp,
                color = ViewerColors.muted,
            )
            if (step.args.isNotEmpty()) {
                Text(
                    text = if (showArgs) "收起参数" else "参数",
                    fontSize = 11.sp,
                    color = ViewerColors.accent,
                    modifier = Modifier.clickable { showArgs = !showArgs },
                )
            }
        }
        if (showArgs && step.args.isNotEmpty()) {
            Text(
                text = step.args,
                fontFamily = FontFamily.Monospace,
                fontSize = 11.sp,
                color = ViewerColors.muted,
                lineHeight = 16.sp,
            )
        }
        if (step.output.isNotEmpty()) {
            Text(
                text = step.output,
                fontFamily = FontFamily.Monospace,
                fontSize = 11.sp,
                lineHeight = 16.sp,
                color = if (step.isError) ViewerColors.danger else MaterialTheme.colorScheme.onSurface,
            )
        }
    }
}

/** 活动分类 → 图标（分类名来自 Go 的 activityName）。 */
private fun activityIcon(activity: String): ImageVector = when (activity) {
    "thinking" -> Icons.Outlined.AutoAwesome
    "read", "read_image" -> Icons.Outlined.Visibility
    "search" -> Icons.Outlined.Search
    "write", "edit" -> Icons.Outlined.EditNote
    "commands" -> Icons.Outlined.Terminal
    "tools" -> Icons.Outlined.Build
    else -> Icons.Outlined.Build
}

/** 把毫秒格式化成 "22s" / "1m5s"。 */
fun formatDuration(ms: Long): String {
    val seconds = (ms / 1000).toInt()
    return if (seconds < 60) "${seconds}s" else "${seconds / 60}m${seconds % 60}s"
}
