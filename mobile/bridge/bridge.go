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
	"sort"
	"sync"

	"simple-agent/internal/host"
	"simple-agent/internal/locale"
	"simple-agent/internal/markdown"
	"simple-agent/internal/providers"
	"simple-agent/internal/relay"
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
	// 上一个已知的连接状态（用来说明"连接/断开"的迁移）。
	lastConnected bool
	// lastError 是最近一次失败原因（BlobPath 这类阻塞操作把错误留在这里给界面看）。
	lastError string
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
	return &Client{
		post:     newPoster(),
		listener: listener,
		lang:     parsed,
		blobDir:  cacheDir,
		parser:   markdown.NewParser(),
	}
}

// Connect 连上执行端或 relay。
//
// addr 是 host:port；route 非空表示经 relay 转发到某个 hostID（空则按 relay
// 上唯一的执行端自动选择）；token 是 relay 的固定口令（没配就传空）。
//
// 返回空字符串表示成功，否则是给用户看的错误原因。
func (c *Client) Connect(addr, route, token string) string {
	if addr == "" {
		return "请填写执行端或 relay 的地址（host:port）"
	}
	c.mu.Lock()
	if c.remote != nil {
		c.mu.Unlock()
		return "已经连接过了，请先断开"
	}
	c.mu.Unlock()

	// 连接与**断线重连**都走同一个拨号函数：relay 的 token、route 必须一起带过去，
	// 否则重连会悄悄变成"未认证/未路由"。
	//
	// 注意 OpenViewerToken 返回 (conn, 选中的 hostID, err)：首连时那个 hostID 要
	// 记下来（relay 上有多个执行端时，重连必须连回同一台，不能随机换一台）。
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
		// relay 上有多个执行端、又没指定 route：把候选列表报给界面去选
		var needsChoice *relay.NeedsChoice
		if errors.As(err, &needsChoice) {
			return formatNeedsChoice(needsChoice)
		}
		return err.Error()
	}

	c.mu.Lock()
	c.remote = remote
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
func (c *Client) Disconnect() {
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
	c.mu.Unlock()
	if remote != nil {
		_ = remote.Close()
	}
	c.post.close()
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
		// 连接状态变化：把 allow_shell 也刷一遍（握手拿到了）
		if remote := c.host(); remote != nil {
			c.setAllowShell(remote.Info().AllowShell)
		}
		c.setConnected(change.Connected)
	case "models":
		c.emit("providers", "")
	}
	c.emit(change.Kind, change.SessionID)
}

// emit 触发一次回调（在 Go 的串行投递器上执行）。
func (c *Client) emit(kind, sessionID string) {
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

func (c *Client) setConnected(value bool) {
	c.mu.Lock()
	c.lastConnected = value
	c.mu.Unlock()
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

// formatNeedsChoice 把"relay 上有多个执行端"变成可读提示（含候选 id）。
func formatNeedsChoice(choice *relay.NeedsChoice) string {
	if len(choice.Executors) == 0 {
		return "relay 上还没有执行端在线"
	}
	names := make([]string, 0, len(choice.Executors))
	for _, executor := range choice.Executors {
		if executor.Name != "" {
			names = append(names, executor.Name+"("+executor.HostID+")")
			continue
		}
		names = append(names, executor.HostID)
	}
	return fmt.Sprintf("relay 上有多个执行端，请指定 route：%v", names)
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
