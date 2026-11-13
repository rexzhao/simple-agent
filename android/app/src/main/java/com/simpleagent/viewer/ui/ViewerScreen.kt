package com.simpleagent.viewer.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.gestures.awaitEachGesture
import androidx.compose.foundation.gestures.awaitFirstDown
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Send
import androidx.compose.material.icons.filled.BugReport
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.Stop
import com.simpleagent.viewer.BuildConfig
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.ModalDrawerSheet
import androidx.compose.material3.ModalNavigationDrawer
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.input.pointer.PointerEventPass
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.platform.LocalFocusManager
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.simpleagent.viewer.data.ProviderChoice
import com.simpleagent.viewer.data.ExecutorChoice
import com.simpleagent.viewer.data.SessionSummary
import kotlinx.coroutines.launch
import android.net.Uri
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.material.icons.filled.Add
import androidx.compose.runtime.snapshotFlow
import androidx.compose.runtime.withFrameNanos
import androidx.compose.ui.platform.LocalContext
import com.simpleagent.viewer.data.ArchivedMessage
import com.simpleagent.viewer.data.PartView

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ViewerScreen(viewModel: ViewerViewModel) {
    val state by viewModel.uiState.collectAsStateWithLifecycle()
    val snackbar = remember { SnackbarHostState() }
    val drawerState = androidx.compose.material3.rememberDrawerState(
        androidx.compose.material3.DrawerValue.Closed,
    )
    val scope = rememberCoroutineScope()

    LaunchedEffect(state.toast) {
        if (state.toast.isNotEmpty()) {
            snackbar.showSnackbar(state.toast)
            viewModel.consumeToast()
        }
    }

    // 断线重连期间留在聊天页（显示"重连中"横幅），只有真正没连过/连接失败
    // 才回到连接页。以前一断线就直接踢回连接页，用户不得不手动重连。
    if (!state.connected && !state.reconnecting) {
        ConnectScreen(
            state = state,
            onAddr = viewModel::updateAddr,
            onRoute = viewModel::updateRoute,
            onToken = viewModel::updateToken,
            onFingerprint = viewModel::updateFingerprint,
            onConnect = viewModel::connect,
            onDisconnect = viewModel::disconnect,
        )
        return
    }
    // 断线重连中：顶部显示一条不可关闭的横幅，内容区保持原样（可继续阅读）。
    // 必须包一层 Column：Compose 的 setContent 里只能有一个根节点。
    Column(modifier = Modifier.fillMaxSize()) {
        if (state.reconnecting) {
            ReconnectingBanner()
        }
        ModalNavigationDrawer(
        drawerState = drawerState,
        drawerContent = {
            ModalDrawerSheet(drawerContainerColor = androidx.compose.material3.MaterialTheme.colorScheme.surface) {
                SessionDrawer(
                    sessions = state.sessions,
                    activeId = state.activeSessionId,
                    executors = state.executors,
                    currentHostId = state.currentHostId,
                    onSelect = {
                        viewModel.selectSession(it)
                        scope.launch { drawerState.close() }
                    },
                    onNew = {
                        viewModel.newSession()
                        scope.launch { drawerState.close() }
                    },
                    onSwitchHost = {
                        viewModel.switchHost(it)
                        scope.launch { drawerState.close() }
                    },
                    onDisconnect = viewModel::disconnect,
                )
            }
        },
    ) {
        Scaffold(
            snackbarHost = { SnackbarHost(snackbar) },
            topBar = {
                TopAppBar(
                    colors = TopAppBarDefaults.topAppBarColors(
                        containerColor = androidx.compose.material3.MaterialTheme.colorScheme.surface,
                    ),
                    navigationIcon = {
                        IconButton(onClick = { scope.launch { drawerState.open() } }) {
                            Icon(Icons.Filled.Menu, contentDescription = "会话列表")
                        }
                    },
                    title = {
                        Column {
                            Text(
                                text = state.timeline.title.ifEmpty { "simple-agent" },
                                fontSize = 15.sp,
                                fontWeight = FontWeight.Medium,
                                maxLines = 1,
                            )
                            StatusLine(state.timeline.runtime.modelLabel, state.timeline.runtime.allowShell)
                        }
                    },
                    actions = {
                        // 调试入口（只在 Debug 构建出现）：一键新建会话并发一条消息。
                        // 手机上输入法经常把自动化的 input text 吞掉，"点一下自动发"最省事；
                        // 同时也是整条链路（App → relay → 执行端 → 模型）的最快自检。
                        if (BuildConfig.DEBUG) {
                            IconButton(onClick = viewModel::debugSend) {
                                Icon(Icons.Filled.BugReport, contentDescription = "调试：自动发一条消息")
                            }
                        }
                    },
                )
            },
            bottomBar = {
                // imePadding 加在输入卡上（而不是内容区）：键盘弹出时整条输入栏
                // 要**整体抬起**，否则输入框会被键盘盖住，用户看不到自己打什么。
                Column(modifier = Modifier.imePadding()) {
                    // 提问面板在输入卡上方：模型在等回答时，输入框仍可用（可以插话）
                    if (state.asks.isNotEmpty()) {
                        AskPanel(
                            asks = state.asks,
                            tab = state.askTab,
                            onSelectTab = viewModel::selectAskTab,
                            onToggleOption = viewModel::toggleAskOption,
                            onCustom = viewModel::setAskCustom,
                            onSubmit = viewModel::submitAsks,
                            onDismiss = viewModel::dismissAsks,
                        )
                    }
                    Composer(
                        runtime = state.timeline.runtime,
                        onSend = viewModel::send,
                        onStop = viewModel::stop,
                        onDropQueued = viewModel::dropQueued,
                        providers = state.providers,
                        onSelectModel = viewModel::selectModel,
                        uploading = state.uploading,
                        attachments = state.pendingAttachments,
                        onAttach = viewModel::attachImage,
                        onRemoveAttachment = viewModel::removeAttachment,
                    )
                }
            },
        ) { padding ->
            TimelinePane(
                state = state,
                viewModel = viewModel,
                modifier = Modifier
                    .fillMaxSize()
                    .padding(padding),
            )
        }
        }
    }
}

/** 断线重连中的顶部横幅（不可关闭：它表达的是当前状态，不是可忽略的提示）。 */
@Composable
private fun ReconnectingBanner() {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .background(ViewerColors.warn.copy(alpha = 0.15f))
            .padding(horizontal = 14.dp, vertical = 7.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        CircularProgressIndicator(modifier = Modifier.size(12.dp), strokeWidth = 2.dp)
        Text("连接已断开，正在重连…", fontSize = 12.sp, color = ViewerColors.warn)
    }
}

@Composable
private fun StatusLine(model: String, allowShell: Boolean) {
    val text = buildString {
        if (model.isNotEmpty()) append(model)
        if (allowShell) {
            if (isNotEmpty()) append(" · ")
            append("可执行命令")
        }
    }
    if (text.isNotEmpty()) {
        Text(text = text, fontSize = 11.sp, color = ViewerColors.muted, maxLines = 1)
    }
}

@Composable
private fun TimelinePane(state: ViewerState, viewModel: ViewerViewModel, modifier: Modifier) {
    // 列表状态按**会话**区分：换会话时必须从头开始，否则会沿用上一个会话的滚动位置
    // （表现是打开新会话停在中段，而不是末尾）。
    val listState = rememberLazyListState()
    val itemCount = state.timeline.items.size

    // 用户一拖动列表就让输入框失焦（收起键盘）。
    // 手机上读长回答时键盘一直占着半屏很碍事；这是几乎所有聊天 App 的行为。
    //
    // 为什么不用 detectDragGestures：那套在 **Main pass** 上等事件，
    // 而 LazyColumn 是子层级、会先拿到并消费掉拖拽 → 外层永远等不到，
    // onDragStart 不触发（之前"滚动正常但键盘不收起"就是这个原因）。
    // 这里在 **Initial pass**（父节点先看到）只做"检测"，不消费事件，
    // 让 LazyColumn 照常滚动。
    val focusManager = LocalFocusManager.current
    val dragDetector = Modifier.pointerInput(Unit) {
        awaitEachGesture {
            val down = awaitFirstDown(requireUnconsumed = false, pass = PointerEventPass.Initial)
            var cleared = false
            while (true) {
                val event = awaitPointerEvent(PointerEventPass.Initial)
                val change = event.changes.firstOrNull { it.id == down.id } ?: break
                if (!change.pressed) break
                // 超过触摸阈值才算"在拖动"，避免把点击链接/选中文本也当成滚动
                if (!cleared &&
                    (change.position - down.position).getDistance() > viewConfiguration.touchSlop
                ) {
                    cleared = true
                    focusManager.clearFocus()
                }
            }
        }
    }

    // 打开/切换会话时直接跳到末尾：用户期望看到"最新说了什么"。
    // 只在这个会话第一次有内容时做一次，之后交给下面的"贴底跟随"。
    var jumpedSession by remember { mutableStateOf("") }
    LaunchedEffect(state.activeSessionId, itemCount) {
        if (itemCount == 0) return@LaunchedEffect
        if (jumpedSession == state.activeSessionId) return@LaunchedEffect
        jumpedSession = state.activeSessionId
        // 用 scrollToItem 而不是 animate：首屏动画反而像卡了一下
        listState.scrollToItem(itemCount - 1)
    }

    // 贴着底部时自动跟随（流式输出时不要一直把用户拽回底部）
    LaunchedEffect(itemCount, state.timeline.runtime.busy) {
        if (itemCount == 0) return@LaunchedEffect
        // 首次加载完成前 visibleItemsInfo 还是空的：那种情况下"last=0"会让
        // nearBottom 误判，于是永远不跟随。没有可见项时按"需要跟随"处理。
        val visible = listState.layoutInfo.visibleItemsInfo
        val nearBottom = if (visible.isEmpty()) {
            true
        } else {
            (visible.lastOrNull()?.index ?: 0) >= itemCount - 3
        }
        if (nearBottom) listState.scrollToItem(itemCount - 1)
    }
    // 滚到顶部时拉更早的历史（与桌面一致：按需分页，不一次全加载）
    LaunchedEffect(listState, state.activeSessionId) {
        snapshotFlow { listState.firstVisibleItemIndex }
            .collect { first ->
                if (first == 0 && state.hasMoreHistory && !state.loadingOlder) {
                    viewModel.loadOlder()
                }
            }
    }
    // 滚到顶部时加载**同一段会话里更早的 items**（分页尚未加载的部分）。
    // 与上面的归档加载是两件事：归档是压缩后的旧消息，这里是同一段会话的历史。
    //
    // 这里监听的是"滚动位置 + 列表是否撑满"，而不是单纯的 firstVisibleItemIndex：
    // 内容不足一屏时下标恒为 0，单纯的 snapshotFlow 只在第一次发射，之后
    // 再也不会有新值 —— 表现就是"只加载一页就停了，再也翻不动"。
    LaunchedEffect(listState, state.activeSessionId) {
        snapshotFlow {
            // 把"到顶"与"列表不能继续下滑"一起作为信号
            val first = listState.firstVisibleItemIndex
            val cannotScrollMore = !listState.canScrollBackward
            first == 0 && cannotScrollMore
        }.collect { atTopAndStuck ->
            if (atTopAndStuck && state.hasMoreItems && !state.loadingEarlier) {
                viewModel.loadEarlierItems()
            }
        }
    }
    // 加载完成后如果仍然到顶（内容还没撑满一屏），继续拉下一页。
    // 这条专门处理"列表总高度不足一屏"的情况：那时滚动位置不会变化，
    // 上面的 snapshotFlow 不会再次发射。
    LaunchedEffect(state.loadingEarlier, state.hasMoreItems, state.earlierPagesLoaded) {
        if (state.loadingEarlier || !state.hasMoreItems) return@LaunchedEffect
        withFrameNanos { }
        if (listState.firstVisibleItemIndex == 0 && !listState.canScrollForward) {
            viewModel.loadEarlierItems()
        }
    }
    // **滚动锚定**：往列表头部插入内容后，把视野拉回原来的位置。
    //
    // 不补偿的话：用户滚到顶部触发加载，几十条内容插到上面，当前看到的
    // 那条被推到下面 —— 感觉像"突然跳走了"，完全没法连续阅读。
    LaunchedEffect(state.justPrependCount) {
        val added = state.justPrependCount
        if (added <= 0) return@LaunchedEffect
        // 必须先等 Compose 把新 item 排好版：否则 scrollToItem 用的是旧布局
        withFrameNanos { }
        listState.scrollToItem(listState.firstVisibleItemIndex + added)
        viewModel.consumePrependCount()
    }
    // 把回调提到列表外并记住：否则 items 的 lambda 每帧都是新实例，
    // 会拖累 Compose 对每个条目的重组跳过判断（滚动时尤其明显）。
    val imagePath: (String, (String) -> Unit) -> Unit = remember(viewModel) {
        { hash, ready -> viewModel.imagePath(hash, ready) }
    }
    val toggleExpanded: (String) -> Unit = remember(viewModel) { viewModel::toggleExpanded }
    LazyColumn(state = listState, modifier = modifier.then(dragDetector)) {
        // 顶部：正在加载更早的 items / 归档加载 / 到底提示
        if (state.loadingEarlier || state.loadingOlder || state.archive.isNotEmpty()) {
            item(key = "archive-header") {
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(vertical = 10.dp),
                    horizontalArrangement = Arrangement.Center,
                ) {
                    when {
                        state.loadingEarlier || state.loadingOlder -> {
                            CircularProgressIndicator(modifier = Modifier.size(14.dp), strokeWidth = 2.dp)
                        }
                        !state.hasMoreHistory && !state.hasMoreItems -> {
                            Text("已经是最早的内容了", fontSize = 11.sp, color = ViewerColors.muted)
                        }
                    }
                }
            }
        }
        items(state.archive, key = { "arch:" + it.id }) { archived ->
            ArchivedRow(archived, onImagePath = imagePath)
        }
        items(state.timeline.items, key = { it.key }) { item ->
            // 直接渲染单个条目：不再对每个 item 复制整个 TimelineView。
            // 之前是 `Timeline(timeline.copy(items = listOf(item)))` ——
            // 每帧为每个可见条目新建一个 TimelineView 对象，Compose 的
            // 重组跳过完全失效，滚动时反复重组（真机卡顿的主因）。
            TimelineItem(
                item = item,
                expanded = state.expanded,
                onToggle = toggleExpanded,
                onImagePath = imagePath,
            )
        }
        item { Box(Modifier.size(1.dp)) }
    }
}

/** 归档里的一条消息（只读显示，样式比当前会话淡一些以示区分）。 */
@Composable
private fun ArchivedRow(
    archived: com.simpleagent.viewer.data.ArchivedMessage,
    onImagePath: (String, (String) -> Unit) -> Unit,
) {
    Column(
        modifier = Modifier
            .fillMaxWidth()
            .padding(horizontal = 12.dp, vertical = 4.dp)
            .background(ViewerColors.surfaceHigh.copy(alpha = 0.35f), RoundedCornerShape(8.dp))
            .padding(10.dp),
        verticalArrangement = Arrangement.spacedBy(6.dp),
    ) {
        Text(
            text = if (archived.role == "user") "你（历史）" else "助手（历史）",
            fontSize = 10.sp,
            color = ViewerColors.muted,
        )
        archived.parts.forEach { part ->
            when (part.kind) {
                "markdown" -> MarkdownBlocks(part.blocks)
                "code" -> CodeBlock(part.text, part.lang)
                "image" -> ZoomableImageOrPlaceholder(part, onImagePath)
                "file" -> Text("📎 ${part.name}", fontSize = 12.sp, color = ViewerColors.muted)
                else -> Text(part.text, fontSize = 13.sp, color = ViewerColors.muted)
            }
        }
        if (archived.status == "failed" && archived.reason.isNotEmpty()) {
            Text("当时失败：${archived.reason}", fontSize = 11.sp, color = ViewerColors.danger)
        }
    }
}

/** 归档里的图片（与当前会话同一套加载路径：按 hash 从执行端拉）。 */
@Composable
private fun ZoomableImageOrPlaceholder(
    part: com.simpleagent.viewer.data.PartView,
    onImagePath: (String, (String) -> Unit) -> Unit,
) {
    val hash = part.image?.blob.orEmpty()
    if (hash.isEmpty()) {
        Text("[图片]", fontSize = 12.sp, color = ViewerColors.muted)
        return
    }
    var path by remember(hash) { mutableStateOf<String?>(null) }
    remember(hash) {
        onImagePath(hash) { result -> if (result.isNotEmpty()) path = result }
        true
    }
    val current = path
    if (current != null) {
        ZoomableImage(path = current)
    } else {
        Text("正在加载图片…", fontSize = 11.sp, color = ViewerColors.muted)
    }
}

// ---- 连接页 ----

@Composable
private fun ConnectScreen(
    state: ViewerState,
    onAddr: (String) -> Unit,
    onRoute: (String) -> Unit,
    onToken: (String) -> Unit,
    onFingerprint: (String) -> Unit,
    onConnect: () -> Unit,
    onDisconnect: () -> Unit,
) {
    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(androidx.compose.material3.MaterialTheme.colorScheme.background)
            .padding(24.dp),
        verticalArrangement = Arrangement.spacedBy(14.dp),
    ) {
        Text("simple-agent", fontSize = 24.sp, fontWeight = FontWeight.SemiBold)
        Text(
            "连接执行端或 relay。执行端可用 `simple-agent --headless --relay HOST:PORT --token 口令` 启动。",
            fontSize = 13.sp,
            color = ViewerColors.muted,
        )
        OutlinedTextField(
            value = state.addr,
            onValueChange = onAddr,
            label = { Text("地址（host:port）") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = state.route,
            onValueChange = onRoute,
            label = { Text("执行端 host_id（经 relay 时可选）") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = state.token,
            onValueChange = onToken,
            label = { Text("relay token（没有就留空）") },
            singleLine = true,
            visualTransformation = PasswordVisualTransformation(),
            modifier = Modifier.fillMaxWidth(),
        )
        // 传输加密：留空走明文；填了则只接受指纹完全一致的 relay 证书。
        // 指纹由服务端 `--tls --tls-print-fingerprint` 打印（64 位十六进制，可带冒号）。
        OutlinedTextField(
            value = state.fingerprint,
            onValueChange = onFingerprint,
            label = { Text("证书指纹（启用 TLS 时填，留空则明文）") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        if (state.connectError.isNotEmpty()) {
            Text(state.connectError, fontSize = 13.sp, color = ViewerColors.danger)
        }
        Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
            androidx.compose.material3.Button(onClick = onConnect, enabled = !state.connecting) {
                if (state.connecting) {
                    CircularProgressIndicator(modifier = Modifier.size(14.dp), strokeWidth = 2.dp)
                } else {
                    Text("连接")
                }
            }
            // 兜底：连接页也要能断开。
            // 正常情况下"点连接"就会先断开再重连（bridge 层已处理），
            // 但用户可能想停在连接页改参数，这时需要一个显式的断开入口
            // （以前只有侧边抽屉里有，返回连接页后就找不到了）。
            if (state.connected) {
                androidx.compose.material3.OutlinedButton(onClick = onDisconnect) {
                    Text("断开")
                }
            }
        }
    }
}

// ---- 会话抽屉 ----

@Composable
private fun SessionDrawer(
    sessions: List<SessionSummary>,
    activeId: String,
    executors: List<ExecutorChoice>,
    currentHostId: String,
    onSelect: (String) -> Unit,
    onNew: () -> Unit,
    onSwitchHost: (String) -> Unit,
    onDisconnect: () -> Unit,
) {
    Column(modifier = Modifier.fillMaxSize()) {
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .padding(horizontal = 20.dp, vertical = 16.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text("会话", fontSize = 16.sp, fontWeight = FontWeight.SemiBold, modifier = Modifier.weight(1f))
            // 执行端在"一个会话都没有"时是空态（用户决定：不自动新建），
            // 所以这里必须有新建入口，否则手机端没法开始第一轮对话。
            TextButton(onClick = onNew) { Text("新建", fontSize = 13.sp) }
        }
        HorizontalDivider(color = ViewerColors.muted.copy(alpha = 0.2f))
        // 执行端切换：经 relay 连且有多台时才出现。
        // 一台时也显示当前是哪台（让用户知道连的是哪个 host），但不给切换按钮。
        if (executors.size > 1) {
            Text(
                text = "执行端",
                fontSize = 12.sp,
                color = ViewerColors.muted,
                modifier = Modifier.padding(start = 20.dp, top = 12.dp, bottom = 4.dp),
            )
            executors.forEach { executor ->
                val active = executor.hostId == currentHostId
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .clickable { onSwitchHost(executor.hostId) }
                        .padding(horizontal = 20.dp, vertical = 10.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Text(
                        text = if (executor.name.isNotBlank()) executor.name else executor.hostId,
                        fontSize = 14.sp,
                        color = if (active) ViewerColors.accent
                        else androidx.compose.material3.MaterialTheme.colorScheme.onSurface,
                        modifier = Modifier.weight(1f),
                    )
                    if (active) {
                        Text("当前", fontSize = 11.sp, color = ViewerColors.accent)
                    }
                }
            }
            HorizontalDivider(color = ViewerColors.muted.copy(alpha = 0.2f))
        }
        if (sessions.isEmpty()) {
            Text(
                text = "还没有会话，点右上角「新建」开始。",
                fontSize = 13.sp,
                color = ViewerColors.muted,
                modifier = Modifier.padding(20.dp),
            )
        }
        LazyColumn(modifier = Modifier.weight(1f)) {
            items(sessions, key = { it.id }) { session ->
                val active = session.id == activeId
                Column(
                    modifier = Modifier
                        .fillMaxWidth()
                        .clickable { onSelect(session.id) }
                        .background(
                            if (active) ViewerColors.surfaceHigh else androidx.compose.ui.graphics.Color.Transparent,
                        )
                        .padding(horizontal = 20.dp, vertical = 12.dp),
                    verticalArrangement = Arrangement.spacedBy(3.dp),
                ) {
                    Text(
                        text = session.title.ifEmpty { session.id.take(8) },
                        fontSize = 14.sp,
                        fontWeight = if (active) FontWeight.Medium else FontWeight.Normal,
                        maxLines = 2,
                    )
                    if (session.workspace.isNotEmpty()) {
                        Text(session.workspace, fontSize = 11.sp, color = ViewerColors.muted, maxLines = 1)
                    }
                }
            }
        }
        HorizontalDivider(color = ViewerColors.muted.copy(alpha = 0.2f))
        TextButton(
            onClick = onDisconnect,
            modifier = Modifier.padding(12.dp),
        ) { Text("断开连接") }
    }
}

// ---- 输入卡 ----

@Composable
private fun Composer(
    runtime: com.simpleagent.viewer.data.RuntimeView,
    onSend: (String) -> Unit,
    onStop: () -> Unit,
    onDropQueued: (Int) -> Unit,
    providers: List<ProviderChoice>,
    onSelectModel: (String, String, String) -> Unit,
    uploading: Int,
    attachments: List<PendingAttachment>,
    onAttach: (ByteArray, String, String) -> Unit,
    onRemoveAttachment: (String) -> Unit,
) {
    var text by remember { mutableStateOf("") }
    val context = LocalContext.current

    // 相册选图：用系统选择器（ACTION_GET_CONTENT），不申请存储权限
    val pickImage = rememberLauncherForActivityResult(
        contract = ActivityResultContracts.GetContent(),
    ) { uri: Uri? ->
        if (uri == null) return@rememberLauncherForActivityResult
        // 读图 + 压缩要 IO，放后台；结果回主线程
        Thread {
            val loaded = Attachments.load(context, uri)
            if (loaded != null) {
                onAttach(loaded.bytes, loaded.name, loaded.mediaType)
            }
        }.start()
    }

    // 拍照：系统相机（ActivityResultContracts.TakePicturePreview 免文件权限）
    val takePhoto = rememberLauncherForActivityResult(
        contract = ActivityResultContracts.TakePicturePreview(),
    ) { bitmap: android.graphics.Bitmap? ->
        if (bitmap == null) return@rememberLauncherForActivityResult
        Thread {
            val out = java.io.ByteArrayOutputStream()
            bitmap.compress(android.graphics.Bitmap.CompressFormat.PNG, 100, out)
            onAttach(out.toByteArray(), "photo.png", "image/png")
        }.start()
    }

    Column(
        modifier = Modifier
            .fillMaxWidth()
            .background(androidx.compose.material3.MaterialTheme.colorScheme.surface)
            .padding(horizontal = 12.dp, vertical = 8.dp),
        verticalArrangement = Arrangement.spacedBy(6.dp),
    ) {
        if (runtime.queued.isNotEmpty()) {
            Text("插话队列（${runtime.queued.size}）", fontSize = 11.sp, color = ViewerColors.muted)
            runtime.queued.forEachIndexed { index, queued ->
                Row(
                    modifier = Modifier.fillMaxWidth(),
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    Text(
                        text = queued,
                        fontSize = 12.sp,
                        color = ViewerColors.muted,
                        maxLines = 1,
                        modifier = Modifier.weight(1f),
                    )
                    Text(
                        text = "移除",
                        fontSize = 11.sp,
                        color = ViewerColors.accent,
                        modifier = Modifier.clickable { onDropQueued(index) },
                    )
                }
            }
        }

        // 待发附件：显示文件名 + 可移除
        if (attachments.isNotEmpty() || uploading > 0) {
            Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                attachments.forEach { item ->
                    Row(
                        modifier = Modifier
                            .background(ViewerColors.surfaceHigh, RoundedCornerShape(6.dp))
                            .padding(start = 8.dp, end = 4.dp, top = 3.dp, bottom = 3.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        Text("🖼 ${item.name.take(18)}", fontSize = 11.sp, color = ViewerColors.muted)
                        Text(
                            " ✕",
                            fontSize = 11.sp,
                            color = ViewerColors.danger,
                            modifier = Modifier.clickable { onRemoveAttachment(item.hash) },
                        )
                    }
                }
                if (uploading > 0) {
                    Row(
                        modifier = Modifier
                            .background(ViewerColors.surfaceHigh, RoundedCornerShape(6.dp))
                            .padding(horizontal = 8.dp, vertical = 4.dp),
                        verticalAlignment = Alignment.CenterVertically,
                        horizontalArrangement = Arrangement.spacedBy(6.dp),
                    ) {
                        CircularProgressIndicator(modifier = Modifier.size(11.dp), strokeWidth = 2.dp)
                        Text("正在上传 $uploading 张…", fontSize = 11.sp, color = ViewerColors.muted)
                    }
                }
            }
        }

        Row(
            verticalAlignment = Alignment.Bottom,
            horizontalArrangement = Arrangement.spacedBy(4.dp),
        ) {
            // 附件菜单：相册 / 拍照
            var attachMenu by remember { mutableStateOf(false) }
            Box {
                IconButton(onClick = { attachMenu = true }, enabled = uploading == 0) {
                    Icon(
                        imageVector = Icons.Filled.Add,
                        contentDescription = "添加图片",
                        tint = if (uploading == 0) ViewerColors.muted else ViewerColors.muted.copy(alpha = 0.4f),
                    )
                }
                DropdownMenu(expanded = attachMenu, onDismissRequest = { attachMenu = false }) {
                    DropdownMenuItem(
                        text = { Text("从相册选择", fontSize = 13.sp) },
                        onClick = {
                            attachMenu = false
                            pickImage.launch("image/*")
                        },
                    )
                    DropdownMenuItem(
                        text = { Text("拍照", fontSize = 13.sp) },
                        onClick = {
                            attachMenu = false
                            takePhoto.launch(null)
                        },
                    )
                }
            }

            OutlinedTextField(
                value = text,
                onValueChange = { text = it },
                modifier = Modifier.weight(1f),
                placeholder = {
                    Text(
                        if (runtime.isIdle) "发送消息…" else "插话（会在安全点投递）…",
                        fontSize = 14.sp,
                    )
                },
                maxLines = 5,
                // 回车=发送（手机上比"去够右上角的按钮"自然得多）。
                // 插话态也走同一个键：忙时提交的本来就是 enqueue。
                keyboardOptions = KeyboardOptions(imeAction = ImeAction.Send),
                keyboardActions = KeyboardActions(
                    onSend = {
                        if (text.isNotBlank() || attachments.isNotEmpty()) {
                            onSend(text)
                            text = ""
                        }
                    },
                ),
            )
            if (runtime.isIdle) {
                IconButton(
                    onClick = {
                        if (text.isNotBlank() || attachments.isNotEmpty()) {
                            onSend(text)
                            text = ""
                        }
                    },
                ) { Icon(Icons.AutoMirrored.Filled.Send, contentDescription = "发送") }
            } else {
                IconButton(onClick = onStop) { Icon(Icons.Filled.Stop, contentDescription = "停止") }
            }
        }
        ModelChip(runtime = runtime, providers = providers, onSelect = onSelectModel)
    }
}

@Composable
private fun ModelChip(
    runtime: com.simpleagent.viewer.data.RuntimeView,
    providers: List<ProviderChoice>,
    onSelect: (String, String, String) -> Unit,
) {
    var expanded by remember { mutableStateOf(false) }
    val label = buildString {
        append(runtime.modelLabel.ifEmpty { runtime.model })
        if (runtime.effortLabel.isNotEmpty()) append(" · ").append(runtime.effortLabel)
    }
    Box {
        Row(
            modifier = Modifier
                .clickable(enabled = runtime.isIdle) { expanded = true }
                .background(ViewerColors.surfaceHigh, RoundedCornerShape(6.dp))
                .padding(horizontal = 8.dp, vertical = 4.dp),
            horizontalArrangement = Arrangement.spacedBy(4.dp),
        ) {
            Text(label.ifEmpty { "模型" }, fontSize = 11.sp, color = ViewerColors.muted)
            Text("▾", fontSize = 11.sp, color = ViewerColors.muted)
        }
        DropdownMenu(expanded = expanded, onDismissRequest = { expanded = false }) {
            providers.forEach { provider ->
                provider.models.forEach { model ->
                    val name = provider.labels[model] ?: model
                    DropdownMenuItem(
                        text = { Text("${provider.label.ifEmpty { provider.providerId }} / $name", fontSize = 13.sp) },
                        onClick = {
                            expanded = false
                            onSelect(provider.providerId, model, runtime.effort)
                        },
                    )
                }
            }
            if (providers.isEmpty()) {
                DropdownMenuItem(text = { Text("执行端没有公布模型", fontSize = 13.sp) }, onClick = { expanded = false })
            }
        }
    }
}
