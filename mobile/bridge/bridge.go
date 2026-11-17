// Package bridge 是 Android 客户端与 Go 内核之间的唯一接口。
//
// # 为什么用 JSON 字符串当边界
//
// gomobile 只能绑定有限的类型（基本类型、string、[]byte，以及本包定义的结构体），
// 不支持 interface / map / 泛型 / 复杂嵌套 / 泛型回调。所以这里**只暴露 string 与
// []byte**，Kotlin 侧用 kotlinx.serialization 解析。好处是协议演进不用重新生成
// 绑定，也不用担心 gomobile 对某个类型的限制。
//
// # 边界原则
//
//   - **语义留在 Go**：哪些 part 是正文、哪些进过程组、工具卡片的标题、
//     失败原因、markdown 解析、思考档位文案，全部由 Go 的
//     timeline / render / markdown 决定（与桌面共用同一份实现）。
//     Kotlin 只负责把结构画出来。
//   - **状态留在 Go**：会话镜像、续传游标、流式增量合并、blob 缓存都在
//     host.Remote 里。Kotlin 拿到的永远是"当前快照"，不自己拼状态。
//   - **线程**：所有回调都可能在 Go 的 goroutine 上触发（网络读循环、重连计时器），
//     Kotlin 侧必须自己切回主线程再碰 UI。
package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/locale"
	"simple-agent/internal/markdown"
	"simple-agent/internal/providers"
	"simple-agent/internal/relay"
	"simple-agent/internal/timeline"
	"simple-agent/internal/tlsx"
	"simple-agent/internal/wire"
)

// ChangeListener 接收"有东西变了"的通知。
//
// kind 与 host.Change 的 Kind 一致（conversation / runtime / sessions / cmdResult /
// connection / notice / models / providers）；sessionID 可能为空（例如会话列表变化）。
//
// **在 Go 的 goroutine 上调用**，Kotlin 侧必须 post 回主线程。
type ChangeListener interface {
	OnChange(kind string, sessionID string)
}

// Client 是一个 Android 展示端：连着某个执行端（直连或经 relay），持有只读镜像。
type Client struct {
	mu     sync.Mutex
	remote *host.Remote
	post   *poster

	listener ChangeListener
	// closed 让回调在 Close 之后不再触发。
	closed bool

	lang locale.Lang
	// blobDir 是图片缓存的落地目录（展示端只读缓存）。
	blobDir string
	// parser 解析 markdown（带内部缓存，长会话每帧都解析会很慢）。
	parser *markdown.Parser

	// allowShell 来自执行端握手（展示端不读本地配置）。
	allowShell bool
	// lastError 是最近一次失败原因（BlobPath 这类阻塞操作把错误留在这里给界面看）。
	lastError string
	// restoreDial 恢复连接前的全局拨号函数（TLS 是进程级开关，断开时还原）。
	restoreDial func()
	// executors 是 relay 上当前在线的执行端（连接后可以切换目标）。
	executors []relay.Executor
	// conn 记住这次连接用的参数，切换执行端时复用。
	conn connectParams
	// pendingChange 是攒着还没发的高频变化（见 emit）。
	//
	// 流式输出时 "conversation" 每个 token 来一次，每一次都跨 JNI 回调 +
	// Kotlin 侧重建 timeline。这里把窗口内的同类事件合并成一次回调。
	pendingChange map[string]string // kind → sessionID
	// flushStop 关闭时通知 flushLoop 退出。
	flushStop chan struct{}
	// timelineCache 按会话缓存上次的时间线构建结果（见 buildTimelineWithRaw）。
	//
	// 流式输出时每 60ms 就会重建一次，但很多次 entries 根本没变——
	// 构建占 TimelineJSON 耗时的 61%，跳过它收益明确。
	timelineCache map[string]*timelineCacheEntry
	// exhausted 记录哪些会话已经翻到执行端日志的真正开头。
	//
	// has_more 不能只靠本地判断（镜像只有 attachTailEvents 条），
	// 所以默认按"还有"处理；补拉返回空或 HasMore=false 时记在这里，
	// 之后就不再谎报"还有更多"。
	exhausted map[string]bool
}

// connectParams 是"连到哪个 relay、带什么凭据"（切换执行端时复用）。
type connectParams struct {
	addr        string
	token       string
	fingerprint string
}

// NewClient 创建一个尚未连接的展示端。
//
// lang 取 "zh" / "en" / "auto"；cacheDir 是图片缓存目录（可为空）。
func NewClient(listener ChangeListener, lang string, cacheDir string) *Client {
	parsed, ok := locale.Parse(lang)
	if !ok {
		parsed = locale.Detect()
	}
	locale.SetCurrent(parsed)
	// 把 Go 侧日志与崩溃信息落到文件里：Android 上 stderr 不进 logcat，
	// 不这么做的话崩溃后拿不到任何 Go 侧原因（真机上已经吃过两次亏）。
	setupLogging(cacheDir)
	retainLogging()
	client := &Client{
		post:        newPoster(),
		listener:    listener,
		lang:        parsed,
		blobDir:     cacheDir,
		parser:      markdown.NewParser(),
		flushStop:   make(chan struct{}),
	}
	// 高频事件合并器：见 emit 的说明
	go client.flushLoop()
	return client
}

// Connect 连上执行端或 relay。
//
// addr 是 host:port；route 非空表示经 relay 转发到某个 hostID（空则按 relay
// 上唯一的执行端自动选择）；token 是 relay 的固定口令（没配就传空）。
//
// 返回空字符串表示成功，否则是给用户看的错误原因。
func (c *Client) Connect(addr, route, token string) string {
	return c.ConnectTLS(addr, route, token, "")
}

// ConnectTLS 与 Connect 相同，但可以额外校验 relay 的证书指纹（传输加密）。
//
// fingerprint 为空表示不启用 TLS（明文 TCP，与以前完全一样）；
// 非空时只接受**指纹完全一致**的服务端证书，不做 CA 校验（等价于"只信任这一台"）。
//
// 十六进制，可带冒号/空格，大小写不敏感。
func (c *Client) ConnectTLS(addr, route, token, fingerprint string) string {
	if addr == "" {
		return "请填写执行端或 relay 的地址（host:port）"
	}
	// 已经连着就直接重连：界面上点「连接」的意图很明确——用当前参数重新建立连接。
	//
	// 以前这里返回"已经连接过了，请先断开"，但连接页**没有断开按钮**
	// （断开在侧边抽屉里），用户会卡在一个点不动也退不出的状态。
	c.disconnectIfConnected()

	// 连接与**断线重连**都走同一个拨号函数：relay 的 token、route 必须一起带过去，
	// 否则重连会悄悄变成"未认证/未路由"。
	//
	// 注意 OpenViewerToken 返回 (conn, 选中的 hostID, err)：首连时那个 hostID 要
	// 记下来（relay 上有多个执行端时，重连必须连回同一台，不能随机换一台）。
	// 传输加密（可选）：有指纹就走 TLS，否则保持明文。
	// 拨号与重连共用同一个 tlsConfig，所以断线重连也仍然是加密的。
	tlsConfig := tlsx.Config{Fingerprint: fingerprint}
	if fingerprint != "" {
		if _, err := tlsConfig.ClientConfig(); err != nil {
			return "指纹格式不对：" + err.Error()
		}
	}
	// 装到 wire 的拨号钩子上：relay.OpenViewerToken 内部用的是 wire.Dial。
	// 断开时恢复，避免影响后续以明文连接。
	previousDial := wire.DialFunc()
	if fingerprint != "" {
		wire.SetDialFunc(tlsConfig.Dial)
	}
	c.mu.Lock()
	c.restoreDial = func() { wire.SetDialFunc(previousDial) }
	c.mu.Unlock()

	var pinnedMu sync.Mutex
	pinnedRoute := route
	dial := func() (wire.Conn, error) {
		pinnedMu.Lock()
		target := pinnedRoute
		pinnedMu.Unlock()
		conn, chosen, err := relay.OpenViewerToken(addr, target, token)
		if err == nil && chosen != "" {
			pinnedMu.Lock()
			pinnedRoute = chosen
			pinnedMu.Unlock()
		}
		return conn, err
	}
	remote, err := host.NewRemoteWithToken(dial, c.post.submit, token)
	if err != nil {
		c.restoreDialNow()
		// relay 上有多个执行端、又没指定 route：把候选列表留给界面选
		var needsChoice *relay.NeedsChoice
		if errors.As(err, &needsChoice) {
			c.saveExecutors(needsChoice.Executors)
			return "" // 不算失败：界面会弹出选择
		}
		return err.Error()
	}

	c.mu.Lock()
	c.remote = remote
	c.conn = connectParams{addr: addr, token: token, fingerprint: fingerprint}
	c.mu.Unlock()

	remote.Subscribe(func(change host.Change) { c.onHostChange(change) })
	// 握手拿到 allow_shell 之后才有值：主动刷一次，界面不用等下一次变化
	if info := remote.Info(); !info.Local {
		c.setAllowShell(info.AllowShell)
	}
	c.post.submit(func() { c.emit("sessions", "") })
	return ""
}

// AttachForTest 直接挂一个已经建好的展示端（测试用：跳过 TCP 拨号，
// 用进程内管道验证视图模型与回调）。
func AttachForTest(client *Client, remote *host.Remote) {
	client.mu.Lock()
	client.remote = remote
	client.mu.Unlock()
	remote.Subscribe(func(change host.Change) { client.onHostChange(change) })
}

// Disconnect 断开展示端（保留界面上的内容，用户可以继续看）。
// restoreDialNow 恢复连接前的拨号函数（幂等）。
func (c *Client) restoreDialNow() {
	c.mu.Lock()
	restore := c.restoreDial
	c.restoreDial = nil
	c.mu.Unlock()
	if restore != nil {
		restore()
	}
}

// disconnectIfConnected 如果当前连着，先干净地断开（供"重连"用）。
//
// 与 Disconnect 的区别：它不关闭 Client（closed 保持 false），
// 调用方紧接着会重新 Connect。
func (c *Client) disconnectIfConnected() {
	c.mu.Lock()
	remote := c.remote
	c.remote = nil
	c.mu.Unlock()
	if remote == nil {
		return
	}
	// 先还原拨号函数再关连接：关连接时 Remote 可能会尝试最后几次重连，
	// 保持原来的拨号函数更安全（避免它用到即将被替换的 TLS 配置）。
	c.restoreDialNow()
	_ = remote.Close()
}

// ---- 执行端选择 / 切换 ----

// executorInfo 是"relay 上有哪些执行端"的一项（给界面渲染用）。
type executorInfo struct {
	HostID string `json:"host_id"`
	Name   string `json:"name,omitempty"`
}

// ExecutorsJSON 返回 relay 上在线的执行端列表：[{"host_id","name"}]。
//
// 连上 relay 后界面用它渲染"切换执行端"；直连执行端时为空。
func (c *Client) ExecutorsJSON() string {
	c.mu.Lock()
	list := make([]executorInfo, 0, len(c.executors))
	for _, executor := range c.executors {
		list = append(list, executorInfo{HostID: executor.HostID, Name: executor.Name})
	}
	c.mu.Unlock()
	return encode(list)
}

// CurrentHostID 返回当前连着的执行端 id（直连或未连接时为空）。
func (c *Client) CurrentHostID() string {
	c.mu.Lock()
	remote := c.remote
	c.mu.Unlock()
	if remote == nil {
		return ""
	}
	return remote.Info().ID
}

// SwitchHost 在当前 relay 上切到另一个执行端。
//
// hostID 是被选中的执行端；内部用**上次连接时记下的**地址与凭据重连，
// 所以界面上不需要让用户重填。返回空字符串表示成功。
func (c *Client) SwitchHost(hostID string) string {
	if hostID == "" {
		return "没有选择执行端"
	}
	c.mu.Lock()
	params := c.conn
	c.mu.Unlock()
	if params.addr == "" {
		return "还没连接过，无法切换执行端"
	}
	// ConnectTLS 内部会先断开旧连接，再按同一个 relay 连到指定执行端
	return c.ConnectTLS(params.addr, hostID, params.token, params.fingerprint)
}

// saveExecutors 记录候选执行端列表。
func (c *Client) saveExecutors(list []relay.Executor) {
	c.mu.Lock()
	c.executors = append([]relay.Executor(nil), list...)
	c.mu.Unlock()
}

func (c *Client) Disconnect() {
	c.restoreDialNow()
	c.mu.Lock()
	remote := c.remote
	c.remote = nil
	c.mu.Unlock()
	if remote != nil {
		_ = remote.Close()
	}
}

// Close 释放资源（回调之后不会再触发）。
func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	remote := c.remote
	c.remote = nil
	flushStop := c.flushStop
	c.mu.Unlock()
	if flushStop != nil {
		close(flushStop)
	}
	c.restoreDialNow()
	if remote != nil {
		_ = remote.Close()
	}
	c.post.close()
	// 关掉日志句柄：不关的话 Windows 上临时目录删不掉（测试里很常见）
	releaseLogging()
}

// ---- 读（全部返回 JSON，不阻塞）----

// SessionsJSON 返回侧栏的会话列表：[{"id","title","workspace","updated_at"}]
func (c *Client) SessionsJSON() string {
	sessions := c.sessions()
	summaries := make([]SessionSummary, 0, len(sessions))
	for _, ref := range sessions {
		summaries = append(summaries, SessionSummary{
			ID: ref.ID, Title: ref.Title, Workspace: ref.Workspace, UpdatedAt: ref.UpdatedAt,
		})
	}
	return encode(summaries)
}

// TimelineJSON 返回一个会话的完整可渲染内容（见 TimelineView）。
func (c *Client) TimelineJSON(sessionID string) string {
	return encode(c.buildTimeline(sessionID))
}

// itemSeqRange 返回一个渲染条目覆盖的事件序号区间。
//
// 条目可能引用多条消息（过程组把同一轮的多个 part 合在一起），
// 取所有引用里最小的 first 与最大的 last——这样整条 item 的区间是完整的，
// 分页切在它前后都不会把内容切成两半。
func itemSeqRange(item timeline.Item) (uint64, uint64, bool) {
	var first, last uint64
	for _, ref := range item.Refs {
		if ref.Chat == nil {
			continue
		}
		refFirst, refLast := ref.Chat.SeqRange()
		if refLast == 0 {
			continue // 这条消息还没有 seq（内存里新建、尚未落盘）
		}
		if refFirst == 0 {
			refFirst = refLast
		}
		if first == 0 || refFirst < first {
			first = refFirst
		}
		if refLast > last {
			last = refLast
		}
	}
	if last == 0 {
		// 回合结束行之类没有 refs 的条目：靠 TurnEntries 判断
		for _, entry := range item.TurnEntries {
			if entry == nil {
				continue
			}
			entryFirst, entryLast := entry.SeqRange()
			if entryLast == 0 {
				continue
			}
			if entryFirst == 0 {
				entryFirst = entryLast
			}
			if first == 0 || entryFirst < first {
				first = entryFirst
			}
			if entryLast > last {
				last = entryLast
			}
		}
	}
	if last == 0 {
		return 0, 0, false
	}
	return first, last, true
}

// backfillPageLimit 是每次向执行端补拉的历史事件条数。
//
// 太小会让长会话需要滚很多次才到顶；太大则单次回包变重（每条事件都带
// 完整消息内容）。300 条大约相当于几十轮对话，滚一次能翻挺久。
const backfillPageLimit = 300

// ensureEarlierAvailable 在"已经翻到本地镜像最早"时，向执行端要更早的一批事件
// 并注入镜像。
func (c *Client) ensureEarlierAvailable(sessionID string, before uint64) {
	session := c.currentSession(sessionID)
	remote := c.host()
	if session == nil || remote == nil {
		return
	}
	// 本地能看到的最早内容事件 seq（0 表示还没有内容）
	oldest := session.OldestContentSeq()
	if oldest != 0 && before > oldest {
		return // 还没翻到边界，本地够用
	}

	data, err := remote.Backfill(sessionID, before, backfillPageLimit)
	if err != nil {
		// 补拉失败不该让翻页整体失败：界面会显示"已经是最早"，
		// 但那比"转圈卡住"好；用户再滑一次会重试。
		log.Printf("bridge：补拉更早内容失败（session=%s before=%d）：%v", sessionID, before, err)
		return
	}
	if len(data.Events) == 0 {
		// 执行端说没有了：记住这个会话已经到底，别再让界面反复请求。
		c.markExhausted(sessionID)
		return
	}

	applied := session.ApplyHistory(data.Events)
	if applied > 0 {
		// 注入改变 entries 结构：让缓存指纹失效，下次 build 会重建
		c.invalidateTimeline(sessionID)
		log.Printf("bridge：补拉了 %d 条更早事件（session=%s before=%d）",
			applied, sessionID, before)
	}
	// 执行端明确说没有更多了（即使这次给了内容）
	if !data.HasMore {
		c.markExhausted(sessionID)
	}
}

// markExhausted 记录某个会话已经翻到执行端日志的真正开头。
func (c *Client) markExhausted(sessionID string) {
	c.mu.Lock()
	if c.exhausted == nil {
		c.exhausted = map[string]bool{}
	}
	c.exhausted[sessionID] = true
	c.mu.Unlock()
}

// isExhausted 报告某个会话是否已确认翻到最早。
func (c *Client) isExhausted(sessionID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exhausted[sessionID]
}

// invalidateTimeline 丢掉某个会话的时间线缓存（注入历史后必须失效）。
func (c *Client) invalidateTimeline(sessionID string) {
	c.mu.Lock()
	delete(c.timelineCache, sessionID)
	c.mu.Unlock()
}

// resetExhausted 清掉"已翻到最早"的标记（会话有大量新内容时用）。
func (c *Client) resetExhausted(sessionID string) {
	c.mu.Lock()
	delete(c.exhausted, sessionID)
	c.mu.Unlock()
}

// TimelinePageJSON 返回时间线的一页（**按事件 seq 分页**）。
//
// 为什么不用 item key：item 是渲染产物，key 里含 part 索引，流式输出期间会变
// （proc:xxx#1 ↔ xxx#0），游标因此不稳定——真机上表现为"翻到某处就再也翻不动"。
// 日志的 seq 一旦写入永不变，是唯一可靠的游标。
//
// **参数用 int64 而不是 uint64**：gomobile 不支持 uint64，那样导出的方法
// 会整个被跳过（Kotlin 侧编译报"找不到 timelinePageJSON"）。
//
// 分页保证**不切断一条消息**：页边界按 item 的 seq 区间对齐，
// 一页里不会出现"某条消息一半在这页、一半在下页"。
//
// 返回 {session_id, items, has_more, seq_from, seq_to, title, runtime, todos, asks}。
// 只有最后一页带 runtime/todos/asks：它们是"当前状态"，翻历史页时不该覆盖。
func (c *Client) TimelinePageJSON(sessionID string, beforeSeq int64, limit int) string {
	if limit <= 0 || limit > 500 {
		limit = 60
	}
	before := uint64(0)
	if beforeSeq > 0 {
		before = uint64(beforeSeq)
	}
	// 翻到本地镜像最早时，向执行端补拉更早的事件。
	//
	// **为什么必须补拉**：首次 attach 只下发最近 attachTailEvents 条事件
	// （长会话首屏才不卡），本地镜像里就只有这一段。只在镜像里翻页时会
	// 翻到"镜像的最早"，根本不是会话的最早 —— 界面于是谎报
	// "已经是最早的内容了"。
	//
	// 只在"已经到镜像边界"时才走网络：正常翻页都在本地完成，不增加延迟。
	if before > 0 {
		c.ensureEarlierAvailable(sessionID, before)
	}
	view, raw := c.buildTimelineWithRaw(sessionID)
	total := len(view.Items)
	if total == 0 || len(raw) != total {
		return encode(map[string]any{
			"session_id": sessionID, "items": []ItemView{}, "has_more": false,
		})
	}

	// 预计算每条 item 的 seq 区间（无 seq 的条目返回 ok=false，向前继承）
	type span struct {
		first, last uint64
		valid       bool
	}
	spans := make([]span, total)
	var carried uint64
	for index := 0; index < total; index++ {
		first, last, ok := itemSeqRange(raw[index])
		if !ok {
			// 没有 seq 的条目（例如还没落盘的回合结束行）沿用前一条的下界，
			// 避免它变成一个"永远切不开"的孤岛
			spans[index] = span{first: carried, last: carried, valid: carried > 0}
			continue
		}
		spans[index] = span{first: first, last: last, valid: true}
		carried = last
	}

	// 定位这一页的结束下标：默认末尾；有游标时取"最后一个 last < before"的下一位置
	end := total
	if before > 0 {
		end = 0
		for index := 0; index < total; index++ {
			if spans[index].valid && spans[index].last < before {
				end = index + 1
			}
		}
		if end == 0 {
			// 游标之前没有任何条目：已经到最早了
			return encode(map[string]any{
				"session_id": view.SessionID,
				"items":      []ItemView{},
				"has_more":   false,
				"seq_from":   before,
				"seq_to":     before,
			})
		}
	}

	// 从 end 往前数 limit 条，但**不切在一条消息中间**
	start := end - limit
	if start < 0 {
		start = 0
	}
	if start > 0 {
		// 若 start 落在某条消息的区间内部，往前退到它之前
		for start > 0 && spans[start].valid && spans[start].first == spans[start-1].last {
			start--
		}
	}

	// 本页最早的 seq（用来判断执行端还有没有更早的）
	var pageFirst uint64
	if start < total && spans[start].valid {
		pageFirst = spans[start].first
	}

	// has_more 表示「**还有更早的内容可以拿**」，不是「本地这一页前面还有」。
	//
	// 两者差别很关键：首次 attach 只下发最近 attachTailEvents 条事件，
	// 本地镜像里就只有几十条。如果只看本地，翻到镜像开头就会报
	// has_more=false，界面于是认为到底了 —— 而执行端磁盘上还有几百条。
	// 真机表现就是「加载一部分就说已经是最早的内容了」。
	//
	// **判据是 start == 0（本地这一页已到镜像开头），与 before 无关**：
	// 以前这里写 `before == 0`，于是用户翻页（before>0）翻到镜像开头时
	// 永远拿不到 has_more=true，界面就停在那里 —— 这正是"加载一部分
	// 就说到最早了"的直接原因。
	hasMore := start > 0
	if !hasMore {
		hasMore = c.hasEarlierOnHost(sessionID, pageFirst)
	}

	page := map[string]any{
		"session_id": view.SessionID,
		"items":      view.Items[start:end],
		"has_more":   hasMore,
	}
	if start < total && spans[start].valid {
		page["seq_from"] = spans[start].first
	}
	if end > 0 && spans[end-1].valid {
		page["seq_to"] = spans[end-1].last
	}
	// 当前状态只跟最后一页走：翻历史页时不能用旧状态覆盖界面
	if before == 0 {
		page["title"] = view.Title
		page["runtime"] = view.Runtime
		page["todos"] = view.Todos
		page["asks"] = view.Asks
	}
	return encode(page)
}

// hasEarlierOnHost 判断执行端那边是否还有比 from 更早的内容。
//
// **不做网络往返**：这个函数在构建页面的路径上，阻塞会直接卡住界面
// （旧版执行端不认识 backfill 命令，甚至要等到超时）。
//
// 判断方式：看本地是否"只装了镜像的尾巴"。首次 attach 只下发最近
// attachTailEvents 条事件，如果本地条目数明显少于典型会话长度、
// 且本页已经翻到镜像开头，就保守地认为执行端还有更多。
// 真正的边界由补拉返回空来确认（那次是异步的，不阻塞界面）。
func (c *Client) hasEarlierOnHost(sessionID string, from uint64) bool {
	if from == 0 {
		return false
	}
	// 已经确认过翻到最早就别再报"还有"
	if c.isExhausted(sessionID) {
		return false
	}
	// 执行端那边必然有 seq < from 的内容 —— 除非 from 就是日志第一条。
	// 日志的 seq 从 1 开始且不重置，所以 from > 1 就意味着前面还有。
	// 这是个纯本地判断，不需要问执行端。
	return from > 1
}

// ProcessDetailJSON 返回一个过程组的完整步骤（用户展开时才调）。
//
// 懒加载的另一半：timeline 里折叠的过程组只带 StepCount，点开时到这里
// 单独取那一个组。返回 {"key","steps":[...]}；找不到返回 {"error":...}。
func (c *Client) ProcessDetailJSON(sessionID, key string) string {
	session := c.currentSession(sessionID)
	if session == nil {
		return encode(map[string]any{"error": "会话不存在"})
	}
	busy := session.Busy()
	idle := busy == driver.BusyIdle
	// 同样要带补拉的历史：否则点开"更早那段"里的过程组会找不到
	for _, item := range timeline.Build(session.EntriesWithHistory(), idle) {
		if item.Key != key || item.Kind != timeline.KindProcess {
			continue
		}
		return encode(map[string]any{
			"key": key,
			// streaming 传 item.Live（这一组自己是否还在流式），
			// idle/busy 给 processSteps 判断"还没结果的工具是不是还在跑"。
			// 以前这里传 !idle（会话级），会让历史组也当成在流式。
			"steps": c.processSteps(item, item.Live, idle, busy),
		})
	}
	return encode(map[string]any{"error": "过程组不存在（可能已被新内容顶掉）"})
}

// ProvidersJSON 返回执行端公布的模型列表：[{"provider_id","label","models":[…]}]
//
// 展示端**必须**用这份：API Key 与模型配置只在执行端。
func (c *Client) ProvidersJSON() string { return encode(c.providers()) }

// InfoJSON 返回执行端信息与连接状态。
func (c *Client) InfoJSON() string {
	info := host.HostInfo{}
	if remote := c.host(); remote != nil {
		info = remote.Info()
	}
	return encode(map[string]any{
		"name":        info.Name,
		"connected":   c.connected(),
		"allow_shell": c.isAllowShell(),
	})
}

// BlobPath 取一张图片的本地缓存路径（没有就现拉）。
//
// **会阻塞**（等执行端回包），必须在后台线程调用。返回空字符串表示失败，
// 原因见 LastError。
func (c *Client) BlobPath(hash string) string {
	remote := c.host()
	if remote == nil || hash == "" {
		c.setError("未连接执行端")
		return ""
	}
	data, err := remote.Blobs().Open(hash)
	if err != nil {
		c.setError(err.Error())
		return ""
	}
	// 校验：执行端回的必须是**这个 hash 的内容**。空数据/错内容一律当失败，
	// 否则界面会显示一张坏图，比"图片丢失"更难排查。
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != hash {
		c.setError(fmt.Sprintf("图片校验失败：收到 %d 字节，与 hash 不符", len(data)))
		return ""
	}
	path, err := c.saveBlob(hash, data)
	if err != nil {
		c.setError(err.Error())
		return ""
	}
	return path
}

// LastError 返回最近一次操作失败的原因（供界面提示）。
func (c *Client) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastError
}

// ---- 写（命令）----

// SendText 发送一条消息。返回空字符串表示已经交给连接（结果经变更通知回来）。
func (c *Client) SendText(sessionID, text string) string {
	payload, err := host.EncodePayload(host.SendPayload{Text: text})
	if err != nil {
		return err.Error()
	}
	return c.submit(sessionID, "send", payload)
}

// PutBlob 把一段字节上传到执行端的 blob 存储，返回内容 hash。
//
// **阻塞**（要等执行端确认落盘），调用方在后台线程用。attachments 参数是
// JSON：[{"hash":"..","media_type":"image/png","name":"a.png"}]，可空。
//
// 为什么要把附件跟文字一起发：执行端的 `send` 在忙时会把**纯文字**自动转成
// 插话，但**带附件时不能转**（插话队列只存文字，图片无处可放），所以这一点
// 必须在发送时就由执行端判断，客户端不能自己拆成两步。
func (c *Client) SendWithAttachments(sessionID, text, attachments string) string {
	var refs []host.AttachmentRef
	if attachments != "" && attachments != "[]" {
		if errText := decodeJSON(attachments, &refs); errText != "" {
			return errText
		}
	}
	payload, err := host.EncodePayload(host.SendPayload{Text: text, Attachments: refs})
	if err != nil {
		return err.Error()
	}
	return c.submit(sessionID, "send", payload)
}

// PutBlob 上传字节到执行端，返回内容 hash（失败返回空，原因见 LastError）。
//
// 展示端不本地存会话数据，但图片必须落在**执行端**：模型要读它、
// 其它展示端也要按 hash 拉到同一张图。
func (c *Client) PutBlob(data []byte) string {
	remote := c.host()
	if remote == nil {
		c.setError("未连接执行端")
		return ""
	}
	hash, err := remote.Blobs().Put(data)
	if err != nil {
		c.setError(err.Error())
		return ""
	}
	return hash
}

// Enqueue 在忙时插话。
func (c *Client) Enqueue(sessionID, text string) string {
	payload, err := host.EncodePayload(host.TextPayload{Text: text})
	if err != nil {
		return err.Error()
	}
	return c.submit(sessionID, "enqueue", payload)
}

// Stop 停止当前回合（对所有端生效）。
func (c *Client) Stop(sessionID string) string {
	return c.submit(sessionID, "stop", nil)
}

// DropQueued 删除队列里的一项。
func (c *Client) DropQueued(sessionID string, index int) string {
	payload, err := host.EncodePayload(host.IndexPayload{Index: index})
	if err != nil {
		return err.Error()
	}
	return c.submit(sessionID, "dropQueued", payload)
}

// Answer 回答提问。selected 是 JSON：{"questionID": [选项下标…]}；
// custom 是 JSON：{"questionID": "手写答案"}。两者都可以传 "{}" 或空串。
//
// 一般用不着直接调它：界面用 ToggleOption / SetAskCustom 操作本地选择，
// 再调 SubmitAsks。保留 Answer 是为了"一次提交，答案由调用方全权决定"的场景。
func (c *Client) Answer(sessionID, callID, selected, custom string) string {
	answer := host.AskAnswer{CallID: callID}
	if selected != "" && selected != "{}" {
		if errText := decodeJSON(selected, &answer.Selected); errText != "" {
			return errText
		}
	}
	if custom != "" && custom != "{}" {
		if errText := decodeJSON(custom, &answer.Custom); errText != "" {
			return errText
		}
	}
	payload, err := host.EncodePayload(host.AnswerPayload{Answers: []host.AskAnswer{answer}})
	if err != nil {
		return err.Error()
	}
	return c.submit(sessionID, "answer", payload)
}

// ToggleAskOption 切换某个选项（**只改本地选择**，不提交）。
//
// 单选互斥、多选叠加、写文字时取消选项……这些规则都在 driver.AskGroup 里，
// 桌面与 Android 共用同一份实现，客户端不重新实现一遍。
//
// 返回新的提问列表 JSON（界面直接用它重绘，不用再等一次快照）。
func (c *Client) ToggleAskOption(sessionID, callID string, questionIndex, optionIndex int) string {
	group := findAskGroup(c, sessionID, callID)
	if group == nil {
		return ""
	}
	group.Toggle(questionIndex, optionIndex)
	return c.AsksJSON(sessionID)
}

// SetAskCustom 记录某题的手写答案（同样只改本地选择）。
func (c *Client) SetAskCustom(sessionID, callID string, questionIndex int, text string) string {
	group := findAskGroup(c, sessionID, callID)
	if group == nil {
		return ""
	}
	group.SetCustom(questionIndex, text)
	return c.AsksJSON(sessionID)
}

// AsksJSON 返回当前会话待回答的提问（含选中态）。
func (c *Client) AsksJSON(sessionID string) string {
	return encode(c.buildAsks(sessionID))
}

// SubmitAsks 提交当前选择（结果为异步命令）。
//
// 选择已经写进本地 AskGroup（展示端的 driver 副本），这里把它展开成
// host.AskAnswer 一起发给执行端——不依赖"两端 AskGroup 恰好同步"这个前提。
func (c *Client) SubmitAsks(sessionID string) string {
	remote := c.host()
	if remote == nil {
		return "未连接执行端"
	}
	session := remote.Session(sessionID)
	if session == nil {
		return "会话不存在"
	}
	groups := session.AskGroups()
	if len(groups) == 0 {
		return "提问已经不在了"
	}
	answers := make([]host.AskAnswer, 0, len(groups))
	for _, group := range groups {
		answer := host.AskAnswer{
			CallID:   group.CallID,
			Selected: map[string][]int{},
			Custom:   map[string]string{},
		}
		for index, question := range group.Questions {
			if picked := selectedOf(group, index); len(picked) > 0 {
				answer.Selected[question.ID] = picked
			}
			if group.CustomActive(index) {
				answer.Custom[question.ID] = group.Custom(index)
			}
		}
		answers = append(answers, answer)
	}
	payload, err := host.EncodePayload(host.AnswerPayload{Answers: answers})
	if err != nil {
		return err.Error()
	}
	return c.submit(sessionID, "answer", payload)
}

// DismissAsks 跳过（关闭）这次提问。
func (c *Client) DismissAsks(sessionID string) string {
	return c.submit(sessionID, "dismissAsk", nil)
}

// LoadOlder 取一页更早的历史（**阻塞**：等执行端回包，调用方用后台线程）。
//
// cursor 为空表示从最近一份归档开始；返回的 JSON 里 `next` 是下一页游标，
// 空表示没有更早的了。归档是"压缩前的整份日志"，只用于显示、不改会话状态。
func (c *Client) LoadOlder(sessionID, cursor string) string {
	remote := c.host()
	if remote == nil {
		return encode(ArchiveView{Error: "未连接执行端"})
	}
	page, err := remote.LoadOlder(sessionID, cursor)
	if err != nil {
		return encode(ArchiveView{Error: err.Error()})
	}
	view := ArchiveView{Next: page.Next, More: page.More}
	for _, entry := range page.Entries {
		archived := ArchivedMessage{
			ID:     entry.Message.MessageID,
			Role:   roleOf(entry.Message.Role),
			Status: string(entry.Status),
			Reason: entry.Reason,
			At:     entry.CreatedAt,
		}
		for _, part := range entry.Message.Parts {
			if partView, ok := c.partView(part); ok {
				archived.Parts = append(archived.Parts, partView)
			}
		}
		view.Entries = append(view.Entries, archived)
	}
	return encode(view)
}

// SelectModel 切换提供商/模型（执行端校验是否可行）。
func (c *Client) SelectModel(sessionID, providerID, model, effort string) string {
	payload, err := host.EncodePayload(host.SelectPayload{
		Selection: providers.Selection{ProviderID: providerID, Model: model},
		Effort:    effort,
	})
	if err != nil {
		return err.Error()
	}
	return c.submit(sessionID, "selectModel", payload)
}

// NewSession 新建会话（结果里的 id 经 cmdResult 回来）。
func (c *Client) NewSession(workspace, title string) string {
	payload, err := host.EncodePayload(host.NewSessionPayload{Workspace: workspace, Title: title})
	if err != nil {
		return err.Error()
	}
	return c.submit("", "newSession", payload)
}

// Rename 重命名会话。
func (c *Client) Rename(sessionID, title string) string {
	payload, err := host.EncodePayload(host.TextPayload{Text: title})
	if err != nil {
		return err.Error()
	}
	return c.submit(sessionID, "rename", payload)
}

// DeleteSession 删除会话。
func (c *Client) DeleteSession(sessionID string) string {
	return c.submit(sessionID, "deleteSession", nil)
}

// ---- 内部 ----

// submit 提交一条命令：立即返回（结果经 cmdResult 通知回来）。
func (c *Client) submit(sessionID, kind string, payload []byte) string {
	remote := c.host()
	if remote == nil {
		return "未连接执行端"
	}
	_, err := remote.Submit(host.Command{SessionID: sessionID, Kind: kind, Payload: payload})
	if err != nil {
		return err.Error()
	}
	return ""
}

// onHostChange 把 host 的变化转成一次粗粒度通知。
//
// 只告诉界面"哪一类东西变了"，界面自己去拉最新快照（与桌面"Changed 只是提示
// 重绘"的做法一致）：这样不用为每种变化定义增量协议。
func (c *Client) onHostChange(change host.Change) {
	switch change.Kind {
	case "connection":
		// 连接状态变化：把 allow_shell 也刷一遍（握手拿到了）。
		//
		// 不再在这里缓存连接状态：界面要判断"连没连着"时会通过
		// Connected() 直接问 remote（见那里的注释）。
		if remote := c.host(); remote != nil {
			c.setAllowShell(remote.Info().AllowShell)
		}
	case "models":
		c.emit("providers", "")
	}
	c.emit(change.Kind, change.SessionID)
}

// emit 触发一次回调（在 Go 的串行投递器上执行）。
//
// **高频事件要合并**：流式输出时 "conversation" 每个 token 来一次。每次回调
// 都会让 Kotlin 侧重建整条 timeline（长会话序列化上百 KB），不合并会把界面压垮。
//
// 策略：按 kind 合并——同一 kind 在 [changeFlushInterval] 窗口内只回调一次；
// 不同 kind 各自独立（保留语义：runtime 变化不会被当成 conversation）。
// 其余低频事件（sessions / models / connection / cmdResult）立即发，
// 它们关系到界面结构，延迟会让用户觉得"点了没反应"。
func (c *Client) emit(kind, sessionID string) {
	if !isHighFrequencyChange(kind) {
		c.emitNow(kind, sessionID)
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if c.pendingChange == nil {
		c.pendingChange = map[string]string{}
	}
	// 同一 kind 只保留最后一次的 sessionID：合并窗口内的中间状态不需要重放
	c.pendingChange[kind] = sessionID
	c.mu.Unlock()
}

// isHighFrequencyChange 判断这个变化是否会被流式输出高频触发。
func isHighFrequencyChange(kind string) bool {
	switch kind {
	case "conversation", "runtime", "notice":
		return true
	}
	return false
}

// changeFlushInterval 是高频事件的最小回调间隔。
//
// 60ms ≈ 16 帧：比一帧略长，避免每帧都重建；又短到肉眼察觉不出延迟。
const changeFlushInterval = 60 * time.Millisecond

// flushLoop 定期把攒下的高频事件发出去（每个 Client 一条）。
//
// 用 ticker 而不是"攒够 N 个再发"：流式输出的节奏不稳定，按时间合并才能
// 保证"最多每 changeFlushInterval 一次"这个上限。
func (c *Client) flushLoop() {
	ticker := time.NewTicker(changeFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.flushStop:
			return
		case <-ticker.C:
			c.flushPendingSafely()
		}
	}
}

// flushPendingSafely 调 flushPending，并兜住 panic。
//
// 这条 goroutine 会穿过 JNI 调用 Kotlin 的变更回调。那里抛出的异常、
// 或者本身逻辑的越界，都会让整个 Go 运行时 abort —— Android 上表现为
// 进程直接消失。以前这里没有 recover，日志里只能看到一行
// "panic: runtime error: index out of range"，拿不到栈，也分不清是哪个回调。
//
// 现在 panic 只影响这一轮刷新：记一条带完整栈的日志，ticker 继续跑。
func (c *Client) flushPendingSafely() {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("bridge：高频事件刷新 panic（已忽略，循环继续）：%v\n%s",
				recovered, debug.Stack())
		}
	}()
	c.flushPending()
}

// flushPending 把当前攒下的高频事件各发一次。
func (c *Client) flushPending() {
	c.mu.Lock()
	if len(c.pendingChange) == 0 {
		c.mu.Unlock()
		return
	}
	batch := c.pendingChange
	c.pendingChange = nil
	c.mu.Unlock()

	for kind, sessionID := range batch {
		c.emitNow(kind, sessionID)
	}
}

// emitNow 立即触发一次回调。
func (c *Client) emitNow(kind, sessionID string) {
	c.mu.Lock()
	listener := c.listener
	closed := c.closed
	c.mu.Unlock()
	if closed || listener == nil {
		return
	}
	c.post.submit(func() {
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return
		}
		listener.OnChange(kind, sessionID)
	})
}

// host 返回当前的展示端（可能为 nil）。
func (c *Client) host() *host.Remote {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.remote
}

func (c *Client) sessions() []host.SessionRef {
	remote := c.host()
	if remote == nil {
		return nil
	}
	return remote.Sessions()
}

func (c *Client) providers() []host.ProviderChoice {
	remote := c.host()
	if remote == nil {
		return nil
	}
	return remote.Providers()
}

// currentSession 取一个会话：优先用界面指定的，其次执行端的当前会话。
func (c *Client) currentSession(sessionID string) *host.Session {
	remote := c.host()
	if remote == nil {
		return nil
	}
	if sessionID != "" {
		if session := remote.Session(sessionID); session != nil {
			return session
		}
	}
	return remote.Active()
}

func (c *Client) connected() bool {
	remote := c.host()
	return remote != nil && remote.Connected()
}

// Connected 报告当前是否连着（断线重连期间为 false）。
//
// 界面每收到一次 "connection" 事件就来问一次；回到前台时也会主动问。
//
// **必须直接问 remote，不能读缓存的 lastConnected**：
// lastConnected 只在 onHostChange 收到 connection 事件时更新，而首次连接
// 成功时那条事件并不总是发（Remote 构造里 connectedState.Store(true) 是
// 静默的，connManager 里的 "connected" 消息类型至今没有人发）。于是
// lastConnected 会一直停在初始值 false —— 界面据此判断"没连上、正在重连"，
// 顶部横幅永远不消失。
//
// 和内部 connected() 保持一致：两者都问 remote.Connected()（原子读，
// 由连接管理器维护），不引入第二份可能失真的状态。
func (c *Client) Connected() bool {
	return c.connected()
}

func (c *Client) isAllowShell() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.allowShell
}

func (c *Client) setAllowShell(value bool) {
	c.mu.Lock()
	c.allowShell = value
	c.mu.Unlock()
}

func (c *Client) setError(text string) {
	c.mu.Lock()
	c.lastError = text
	c.mu.Unlock()
}

// tr 查界面语言对应的文案（Go 侧生成的提示要跟界面同一种语言）。
func (c *Client) tr(key string, args ...any) string {
	return c.lang.Tr(key, args...)
}

// encode 把视图模型编成 JSON（失败时返回一个带 error 字段的对象，界面能显示原因）。
func encode(value any) string {
	body, err := json.Marshal(value)
	if err != nil {
		fallback, _ := json.Marshal(map[string]any{"error": err.Error()})
		return string(fallback)
	}
	return string(body)
}

func decodeJSON(text string, target any) string {
	if err := json.Unmarshal([]byte(text), target); err != nil {
		return err.Error()
	}
	return ""
}

// sortedKeys 返回 map 的键（字母序，输出稳定便于测试与阅读）。
func sortedKeys(data map[string]any) []string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
