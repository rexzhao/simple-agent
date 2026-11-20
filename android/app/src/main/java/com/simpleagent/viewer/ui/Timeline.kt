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
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.simpleagent.viewer.data.FileDiffView
import com.simpleagent.viewer.data.FooterView
import com.simpleagent.viewer.data.ItemView
import com.simpleagent.viewer.data.MessageView
import com.simpleagent.viewer.data.NoticeView
import com.simpleagent.viewer.data.ProcessStep
import com.simpleagent.viewer.data.ProcessView
import com.simpleagent.viewer.data.SearchView
import com.simpleagent.viewer.data.TimelineView
import com.simpleagent.viewer.data.WebView

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
    expandedFull: Set<String>,
    onToggle: (String) -> Unit,
    onToggleFull: (String) -> Unit,
    onImagePath: (String, (String) -> Unit) -> Unit,
    modifier: Modifier = Modifier,
) {
    Column(
        modifier = modifier.padding(horizontal = 12.dp, vertical = 8.dp),
        verticalArrangement = Arrangement.spacedBy(10.dp),
    ) {
        view.items.forEach { item ->
            TimelineItem(item, expanded, expandedFull, onToggle, onToggleFull, onImagePath)
        }
        if (view.runtime.compacting) {
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                CircularProgressIndicator(modifier = Modifier.size(12.dp), strokeWidth = 2.dp)
                Text("正在压缩上下文…", fontSize = 12.sp, color = ViewerColors.muted)
            }
        }
    }
}

/**
 * 渲染**单个**时间线条目（列表里逐条调用）。
 *
 * 为什么不复用 [Timeline]：以前列表里对每个 item 都调用
 * `Timeline(view = timeline.copy(items = listOf(item)))` ——
 * 那会为每一帧、每个可见条目都复制整个 TimelineView 对象，
 * Compose 的重组跳过直接失效，滚动时反复重组（真机上的卡顿来源）。
 *
 * 这里只依赖 item 本身与"这个条目是否展开"，参数是稳定的值类型，
 * 未变化的条目可以真正跳过重组。
 */
@Composable
fun TimelineItem(
    item: ItemView,
    expanded: Set<String>,
    expandedFull: Set<String>,
    onToggle: (String) -> Unit,
    onToggleFull: (String) -> Unit,
    onImagePath: (String, (String) -> Unit) -> Unit,
    modifier: Modifier = Modifier,
) {
    Column(
        modifier = modifier.padding(horizontal = 12.dp, vertical = 5.dp),
    ) {
        when (item.kind) {
            "content" -> item.message?.let { MessageRow(it, onImagePath) }
            "process" -> item.process?.let {
                ProcessGroup(
                    process = it,
                    expanded = expanded.contains(item.key),
                    expandedFull = expandedFull.contains(item.key),
                    onToggle = { onToggle(item.key) },
                    onToggleFull = { onToggleFull(item.key) },
                )
            }
            "notice" -> item.notice?.let { NoticeRow(it) }
            "footer" -> item.footer?.let { FooterRow(it) }
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

/**
 * 一级展开时默认显示的步骤数。
 *
 * 一个长回合的过程组动辄几十步，全展开会把时间线撑得很长（往上翻要划很多屏）。
 * 默认只给最后几步 —— 最近发生的通常最关心 —— 需要看全貌时再点"展开全部"。
 */
private const val COLLAPSED_STEP_PREVIEW = 3

/** 过程组：折叠标题 + 展开后的思考/工具卡片。
 *
 * 展开分两级：
 *   - 一级（[expanded]）：只显示最后 [COLLAPSED_STEP_PREVIEW] 步
 *   - 二级（[expandedFull]）：显示全部步骤
 */
@Composable
private fun ProcessGroup(
    process: ProcessView,
    expanded: Boolean,
    expandedFull: Boolean,
    onToggle: () -> Unit,
    onToggleFull: () -> Unit,
) {
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
            // 折叠时也显示步数：详情没拉下来，但用户能知道这一组有多少内容。
            if (!expanded && process.stepCount > 1) {
                Text("${process.stepCount} 步", fontSize = 11.sp, color = ViewerColors.muted)
            }
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
                // 详情是展开时才拉的（Go 侧折叠时不传 steps）：
                // 还没到的时候显示"正在加载"，避免用户以为点开是空的。
                if (process.steps.isEmpty() && !process.detail && process.stepCount > 0) {
                    Row(
                        horizontalArrangement = Arrangement.spacedBy(8.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        CircularProgressIndicator(modifier = Modifier.size(12.dp), strokeWidth = 2.dp)
                        Text("正在加载…", fontSize = 12.sp, color = ViewerColors.muted)
                    }
                }
                // 详情到手后才知道真实步数：以实际 steps 为准（stepCount 是
                // 折叠时 Go 侧给的估算，可能比实际多/少一条）。
                val total = if (process.steps.isNotEmpty()) process.steps.size else process.stepCount
                val hidden = if (expandedFull) 0 else (total - COLLAPSED_STEP_PREVIEW).coerceAtLeast(0)
                val shown = if (hidden > 0) process.steps.takeLast(COLLAPSED_STEP_PREVIEW) else process.steps

                // "上面还有 N 步"：告诉用户这里是被截断的，而不是内容就这么多。
                if (hidden > 0) {
                    Text(
                        text = "⋯ 上面还有 $hidden 步",
                        fontSize = 11.sp,
                        color = ViewerColors.muted,
                        modifier = Modifier.padding(start = 2.dp),
                    )
                }
                shown.forEach { step ->
                    when (step.kind) {
                        "reasoning" -> ReasoningStep(step)
                        else -> ToolStep(step)
                    }
                }
                // 切换按钮：只有真的被截断（或已全展开）时才出现，
                // 步数本来就少于预览数时不该多一个没用的按钮。
                if (total > COLLAPSED_STEP_PREVIEW) {
                    Text(
                        text = if (expandedFull) "收起，只看最后 $COLLAPSED_STEP_PREVIEW 步"
                        else "展开全部 $total 步",
                        fontSize = 11.sp,
                        color = ViewerColors.accent,
                        modifier = Modifier
                            .fillMaxWidth()
                            .clip(RoundedCornerShape(6.dp))
                            .clickable(onClick = onToggleFull)
                            .padding(vertical = 6.dp),
                        textAlign = androidx.compose.ui.text.style.TextAlign.Center,
                    )
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
                imageVector = activityIcon(step.toolName.ifEmpty { "tools" }),
                contentDescription = null,
                tint = statusColor,
                modifier = Modifier.size(13.dp),
            )
            Text(step.toolTitle, fontSize = 12.sp, fontWeight = FontWeight.Medium, color = statusColor)
            // 摘要（与桌面端同一套规则）：
            //   bash  → description + 命令截断
            //   edit  → 文件名 + "+N -N"
            //   read  → 文件 + "第 1-80 行 / 共 300 行"
            //   其它  → 各自的摘要
            val summary = toolSummaryLine(step)
            if (summary.isNotEmpty()) {
                Text(
                    text = summary,
                    fontSize = 11.sp,
                    color = ViewerColors.muted,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f, fill = false),
                )
            }
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
        }
        // 有结构化结果时不再显示纯文本 output（里面掺着给模型的指令），
        // 改成按结构渲染。
        if (step.hasStructuredResult()) {
            ToolStructuredBody(step)
        } else if (step.output.isNotEmpty()) {
            Text(
                text = step.output,
                fontFamily = FontFamily.Monospace,
                fontSize = 11.sp,
                lineHeight = 16.sp,
                color = if (step.isError) ViewerColors.danger else MaterialTheme.colorScheme.onSurface,
            )
        }
        if (step.args.isNotEmpty() && !step.hasStructuredResult()) {
            Text(
                text = if (showArgs) "收起参数" else "参数",
                fontSize = 11.sp,
                color = ViewerColors.accent,
                modifier = Modifier.clickable { showArgs = !showArgs },
            )
            if (showArgs) {
                Text(
                    text = step.args,
                    fontFamily = FontFamily.Monospace,
                    fontSize = 11.sp,
                    color = ViewerColors.muted,
                    lineHeight = 16.sp,
                )
            }
        }
    }
}

/** 有结构化摘要就不必再摊 output 文本。 */
private fun ProcessStep.hasStructuredResult(): Boolean =
    diff != null || search != null || web != null || read != null ||
        todo != null || imageInfo != null

/**
 * 折叠行的一行摘要（与桌面端 uiapp.toolSummaryLine 同一套规则）。
 */
private fun toolSummaryLine(step: ProcessStep): String {
    step.imageInfo?.let {
        val size = if (it.width > 0) "${it.width}x${it.height}" else ""
        val bytes = formatBytes(it.bytes)
        val type = it.mediaType.removePrefix("image/")
        return listOf(size, bytes, type).filter { part -> part.isNotEmpty() }.joinToString(" · ")
    }
    step.search?.let {
        if (it.fileCount == 0 && it.hitCount == 0) return ""
        // glob 每个文件一条命中：说"处命中"没意义
        return if (it.hitCount == it.fileCount) "${it.fileCount} 个文件"
        else "${it.fileCount} 个文件 · ${it.hitCount} 处命中"
    }
    step.web?.let {
        if (it.kind == "search") return "${it.results.size} 条结果"
        val parts = mutableListOf<String>()
        if (it.status > 0) parts += "HTTP ${it.status}"
        if (it.bytes > 0) parts += formatBytes(it.bytes)
        return parts.joinToString(" · ")
    }
    step.read?.let {
        if (it.totalLines == 0) return ""
        if (it.endLine == 0) return "共 ${it.totalLines} 行"
        if (it.startLine == it.endLine) return "第 ${it.startLine} 行 / 共 ${it.totalLines} 行"
        return "第 ${it.startLine}-${it.endLine} 行 / 共 ${it.totalLines} 行"
    }
    step.todo?.let {
        return if (it.total == 0) "已清空" else "${it.completed}/${it.total} 完成"
    }
    step.diff?.let { diff ->
        // write：整篇写入，给行数
        if (diff.content.isNotEmpty() || diff.verb.isNotEmpty()) {
            val lines = diff.content.trimEnd('\n').split('\n').size
            return "$lines 行"
        }
        if (diff.added > 0 || diff.removed > 0) {
            return listOfNotNull(
                if (diff.added > 0) "+${diff.added}" else null,
                if (diff.removed > 0) "-${diff.removed}" else null,
            ).joinToString(" ")
        }
    }
    return ""
}

/** 把字节数格式化成 "240KB" / "1.2MB"。 */
private fun formatBytes(n: Int): String = when {
    n <= 0 -> ""
    n < 1024 -> "${n}B"
    n < 1024 * 1024 -> "${n / 1024}KB"
    else -> String.format("%.1fMB", n / (1024.0 * 1024.0))
}

/** 按结构渲染工具结果（对应桌面端的 renderStructuredResult）。 */
@Composable
private fun ToolStructuredBody(step: ProcessStep) {
    step.search?.let { SearchBody(it); return }
    step.web?.let {
        // fetch 的正文有价值，仍显示（去掉给模型的指令头）
        if (it.kind != "search") {
            val body = step.output.substringAfter("\n\n", step.output)
                .substringBeforeLast("\n\n(Content truncated")
            if (body.isNotBlank()) CodeText(body)
        } else {
            WebSearchBody(it)
        }
        return
    }
    step.read?.let { read ->
        // 去掉末尾那句 "…（共 300 行，已返回第 1-80 行；用 offset=81 继续）"
        val body = step.output.substringBeforeLast("\n…（共 ", step.output)
        if (body.isNotBlank()) CodeText(body)
        return
    }
    step.todo?.let {
        CodeText(if (it.total == 0) "任务列表已清空" else "任务列表：${it.completed}/${it.total} 完成")
        return
    }
    step.diff?.let { diff ->
        if (diff.content.isNotEmpty()) WriteBody(diff) else DiffBody(diff)
        return
    }
}

/** 检索结果：按文件分组。 */
@Composable
private fun SearchBody(search: SearchView) {
    if (search.files.isEmpty()) {
        Text("没有匹配", fontSize = 11.sp, color = ViewerColors.muted)
        return
    }
    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        search.files.forEach { file ->
            Column {
                Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                    Text(file.path, fontFamily = FontFamily.Monospace, fontSize = 11.sp,
                        maxLines = 1, overflow = TextOverflow.Ellipsis)
                    if (file.hits.size > 1) {
                        Text("${file.hits.size} 处", fontSize = 10.sp, color = ViewerColors.muted)
                    }
                }
                file.hits.forEach { hit ->
                    Text(
                        text = "%4d  %s".format(hit.line, hit.text.trim().trimText(100)),
                        fontFamily = FontFamily.Monospace,
                        fontSize = 11.sp,
                        color = ViewerColors.muted,
                        maxLines = 1,
                    )
                }
            }
        }
        if (search.truncated) {
            Text("… 已达上限，结果被截断", fontSize = 10.sp, color = ViewerColors.muted)
        }
    }
}

/** 搜索结果列表。 */
@Composable
private fun WebSearchBody(web: WebView) {
    Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
        web.results.forEachIndexed { index, result ->
            Column {
                Text("${index + 1}. ${result.title}", fontSize = 12.sp, maxLines = 1,
                    overflow = TextOverflow.Ellipsis)
                if (result.url.isNotEmpty()) {
                    Text(result.url, fontFamily = FontFamily.Monospace, fontSize = 10.sp,
                        color = ViewerColors.muted, maxLines = 1,
                        overflow = TextOverflow.Ellipsis)
                }
                if (result.excerpt.isNotEmpty()) {
                    Text(result.excerpt.trimText(120), fontSize = 11.sp,
                        color = ViewerColors.muted, maxLines = 2)
                }
            }
        }
        if (web.truncated) {
            Text("… 只显示了前面的结果", fontSize = 10.sp, color = ViewerColors.muted)
        }
    }
}

/** edit 的 diff：行号两列 + 红绿行。 */
@Composable
private fun DiffBody(diff: FileDiffView) {
    Column {
        diff.hunks.forEach { hunk ->
            hunk.lines.forEach { line ->
                val bg = when (line.kind) {
                    "add" -> ViewerColors.diffAddBg
                    "del" -> ViewerColors.diffDelBg
                    else -> Color.Transparent
                }
                val fg = when (line.kind) {
                    "add" -> ViewerColors.diffAddText
                    "del" -> ViewerColors.diffDelText
                    else -> ViewerColors.muted
                }
                val mark = when (line.kind) {
                    "add" -> "+"
                    "del" -> "-"
                    else -> " "
                }
                Text(
                    text = "%4s %4s %s %s".format(
                        line.oldLine.takeIf { it > 0 }?.toString() ?: "",
                        line.newLine.takeIf { it > 0 }?.toString() ?: "",
                        mark,
                        line.text,
                    ),
                    fontFamily = FontFamily.Monospace,
                    fontSize = 11.sp,
                    color = fg,
                    maxLines = 1,
                    modifier = Modifier
                        .fillMaxWidth()
                        .background(bg),
                )
            }
        }
        if (diff.truncated) {
            Text("… 差异过大，只显示前面部分", fontSize = 10.sp, color = ViewerColors.muted)
        }
    }
}

/** write 写入的内容（带行号）。 */
@Composable
private fun WriteBody(diff: FileDiffView) {
    Column {
        diff.content.trimEnd('\n').split('\n').forEachIndexed { index, line ->
            Text(
                text = "%4d  %s".format(index + 1, line),
                fontFamily = FontFamily.Monospace,
                fontSize = 11.sp,
                maxLines = 1,
            )
        }
    }
}

@Composable
private fun CodeText(text: String) {
    Text(
        text = text,
        fontFamily = FontFamily.Monospace,
        fontSize = 11.sp,
        lineHeight = 16.sp,
        color = ViewerColors.muted,
    )
}

/** 截断到 n 个字符（按 code point，中文不会被切一半）。 */
private fun String.trimText(n: Int): String =
    if (codePointCount(0, length) <= n) this
    else substring(0, offsetByCodePoints(0, n)) + "…"

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
