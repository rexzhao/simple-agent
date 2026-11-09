package host

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// Local 是进程内的执行端：单机模式与 `--headless` 都用它。
//
// 它持有全部会话（driver + 日志 + 工具上下文 + blob），并负责执行 driver 交回的
// effect。uiapp 只通过 `Host` 接口读状态、提交命令。
type Local struct {
	cfg      Config
	sessions []*Session
	active   int
	subs     *subscribers
	blobs    BlobService

	mu sync.Mutex
	// notices 是每个会话的显示型提示（不写日志，重连时随状态一起给）。
	notices map[string][]Notice
	noticeN int
	// warned 记录已经打过的启动警告（同一条只打一次）。
	warned map[string]bool
	// rings 是每会话最近的事件（断线续传用；覆盖不到就发全量）。
	rings map[string][]store.Event
	// epoch 是本次运行的实例 id（重启会变）。
	epoch string
	// observers 是变化观察者集合（多条连接各自注册；不是覆盖式）。
	observers []Observer
	// eventSeq/streamSeq 是**执行端**给每个会话打的两个序号：
	//   - eventSeq：持久事件（进 ring，用于断线续传）；
	//   - streamSeq：瞬时增量与 state.snapshot（同一个数列，用于 Inflight 衔接）。
	// 序号在**产生的那一刻**打上并进 ring，与"有没有展示端在线"无关——断线期间
	// 产生的事件同样要有序号，否则重连时会被误判成"没有缺口"。
	eventSeq  map[string]uint64
	streamSeq map[string]uint64

	startupErr string
	closed     bool
}

// NewLocal 构造一个进程内执行端（并按数据目录恢复已有会话）。
func NewLocal(cfg Config) *Local {
	if cfg.Post == nil {
		cfg.Post = func(fn func()) { fn() }
	}
	if cfg.Tr == nil {
		cfg.Tr = func(key string, args ...any) string { return key }
	}
	if cfg.NewID == nil {
		cfg.NewID = defaultNewID
	}
	l := &Local{
		cfg:       cfg,
		subs:      newSubscribers(),
		notices:   map[string][]Notice{},
		rings:     map[string][]store.Event{},
		eventSeq:  map[string]uint64{},
		streamSeq: map[string]uint64{},
		epoch:     defaultNewID()[:12],
	}
	if cfg.Blobs != nil {
		l.blobs = LocalBlobs{Store: cfg.Blobs}
	}
	l.restoreSessions()
	return l
}

func defaultNewID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("session-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// ---- Host 接口 ----

func (l *Local) Info() HostInfo {
	return HostInfo{ID: "local", Name: "local", Local: true, AllowShell: l.cfg.AllowShell}
}

// AllowShell 返回执行端的命令执行权限（握手时告诉展示端，避免界面按自己的
// 本地配置猜）。
func (l *Local) AllowShell() bool { return l.cfg.AllowShell }

// RequireToken 报告执行端是否要求展示端出示 token（--listen 模式下配置了才为真）。
func (l *Local) RequireToken() bool { return l.cfg.Token != "" }

// authorize 校验展示端握手带来的 token（未配置时恒为通过）。
//
// 用定长比较，避免通过响应时间逐字节试探。
func (l *Local) authorize(token string) bool {
	if l.cfg.Token == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(l.cfg.Token)) == 1
}

func (l *Local) Sessions() []SessionRef {
	out := make([]SessionRef, 0, len(l.sessions))
	for _, session := range l.sessions {
		updated := int64(0)
		if entries := session.Conversation().Entries(); len(entries) > 0 {
			last := entries[len(entries)-1]
			if last.Message != nil && !last.Message.CreatedAt.IsZero() {
				updated = last.Message.CreatedAt.Unix()
			} else if last.Notice != nil {
				updated = last.Notice.CreatedAt.Unix()
			}
		}
		out = append(out, SessionRef{
			ID: session.ID, Title: session.Title,
			Workspace: session.Workspace, UpdatedAt: updated,
		})
	}
	return out
}

func (l *Local) Session(id string) *Session {
	for _, session := range l.sessions {
		if session.ID == id {
			return session
		}
	}
	return nil
}

func (l *Local) Subscribe(fn func(Change)) func() { return l.subs.add(fn) }

// Providers 返回执行端的模型/档位列表（展示端照它显示选择器）。
func (l *Local) Providers() []ProviderChoice { return l.cfg.Models }

// Models 是 Providers 的同义入口（execServer 发 catalog.models 时用）。
func (l *Local) Models() []ProviderChoice { return l.Providers() }

// SetModels 在构造后注入模型列表（入口把配置里的 provider 列表填进来）。
func (l *Local) SetModels(models []ProviderChoice) { l.cfg.Models = models }

func (l *Local) Blobs() BlobService { return l.blobs }

// ObserverCountForTest 返回当前观察者数量（测试用：检查展示端断开后有没有泄漏）。
func (l *Local) ObserverCountForTest() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.observers)
}

// CatalogForTest 返回会话目录（测试用）。
func (l *Local) CatalogForTest() *store.Catalog { return l.cfg.Catalog }

// SetBlobs 替换图片存储（演示/测试用，比如注入一个会失败的实现对账失败路径）。
func (l *Local) SetBlobs(blobs BlobService) { l.blobs = blobs }

func (l *Local) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	for _, session := range l.sessions {
		if session.journal != nil {
			_ = session.journal.Sync()
			_ = session.journal.Close()
			session.journal = nil
		}
	}
	return nil
}

// ---- 命令 ----

// Submit 提交一条命令。
//
// 语义是**异步**的：立即返回 CmdID，结果通过 Subscribe 的 `cmdResult` 回来。
// 进程内实现可以直接在当前线程执行（没有网络往返，也不会阻塞界面），
// 但调用方**不能**依赖"Submit 返回时已经生效"——远程实现会真的延后。
func (l *Local) Submit(cmd Command) (string, error) {
	if cmd.CmdID == "" {
		cmd.CmdID = l.newCmdID()
	}
	if l.cfg.Deferred {
		// 只排队：下一次主线程 drain 时才真正执行（模拟远程的往返延迟）
		deferredCmd := cmd
		l.cfg.Post(func() { l.run(deferredCmd) })
		return cmd.CmdID, nil
	}
	l.run(cmd)
	return cmd.CmdID, nil
}

// run 实际执行一条命令，并把结果以 cmdResult 广播出去。
func (l *Local) run(cmd Command) {
	session := l.Session(cmd.SessionID)
	var result CmdResult
	if cmd.SessionID != "" && session == nil {
		result = CmdResult{Error: "会话不存在"}
	} else {
		result = l.dispatch(session, cmd)
	}
	result.CmdID = cmd.CmdID
	result.Kind = cmd.Kind
	for _, obs := range l.observersSnapshot() {
		obs.CmdResult(result)
	}
	l.emit(Change{SessionID: cmd.SessionID, Kind: "cmdResult", CmdID: cmd.CmdID, Result: result})
	if session != nil {
		l.emit(Change{SessionID: session.ID, Kind: "conversation"})
	}
}

func (l *Local) newCmdID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf[:])
}

func (l *Local) dispatch(session *Session, cmd Command) CmdResult {
	switch cmd.Kind {
	case "send":
		var payload SendPayload
		if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
			return CmdResult{Error: err.Error()}
		}
		return session.send(payload)
	case "enqueue":
		var payload TextPayload
		if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
			return CmdResult{Error: err.Error()}
		}
		return session.enqueue(payload.Text)
	case "dropQueued":
		var payload IndexPayload
		if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
			return CmdResult{Error: err.Error()}
		}
		return session.dropQueued(payload.Index)
	case "stop":
		return session.stop()
	case "answer":
		var payload AnswerPayload
		if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
			return CmdResult{Error: err.Error()}
		}
		return session.submitAsks(payload)
	case "dismissAsk":
		return session.dismissAsks()
	case "compact":
		return session.compact()
	case "cancelCompact":
		return session.cancelCompact()
	case "selectModel":
		var payload SelectPayload
		if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
			return CmdResult{Error: err.Error()}
		}
		return session.selectModel(payload.Selection, payload.Effort)
	case "setEffort":
		var payload TextPayload
		if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
			return CmdResult{Error: err.Error()}
		}
		return session.setEffort(payload.Text)
	case "rename":
		var payload TextPayload
		if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
			return CmdResult{Error: err.Error()}
		}
		return session.rename(payload.Text)
	case "newSession":
		var payload NewSessionPayload
		_ = json.Unmarshal(cmd.Payload, &payload)
		newSession := l.NewSession(payload.Workspace)
		if payload.Title != "" {
			newSession.rename(payload.Title)
		}
		return CmdResult{OK: true, Data: NewSessionData{SessionID: newSession.ID}}
	case "deleteSession":
		return l.deleteSession(cmd.SessionID)
	case "setVision":
		var vision bool
		if err := json.Unmarshal(cmd.Payload, &vision); err != nil {
			return CmdResult{Error: err.Error()}
		}
		session.drv.SetVision(vision)
		if session.toolCtx != nil {
			session.toolCtx.Vision = vision
		}
		return CmdResult{OK: true}
	case "loadOlder":
		var payload LoadOlderPayload
		if len(cmd.Payload) > 0 {
			_ = json.Unmarshal(cmd.Payload, &payload)
		}
		page, err := session.loadOlder(payload.Cursor)
		if err != nil {
			return CmdResult{Error: err.Error()}
		}
		return CmdResult{OK: true, Data: page}
	default:
		return CmdResult{Error: "未知命令：" + cmd.Kind}
	}
}

// ---- 会话生命周期 ----

// NewSession 新建一个空会话（**先不落盘**：第一条消息才建目录）。
func (l *Local) NewSession(workspace string) *Session {
	id := l.cfg.NewID()
	for l.Session(id) != nil {
		id = l.cfg.NewID()
	}
	title := l.cfg.Title
	if title == "" {
		title = l.cfg.Tr("session.defaultTitle")
	}
	if workspace == "" {
		workspace = l.cfg.Workspace
	}

	conv := conversation.New(id)
	toolCtx := tools.NewToolContext(workspace, l.cfg.AllowShell)
	toolCtx.Vision = l.visionFor(l.cfg.Selection)
	// workspace 按会话传：它会被写进 session.created，决定重放后归到哪个项目
	drv := driver.New(conv, l.driverOptionsFor(l.cfg.Selection, workspace))
	session := &Session{
		ID:         id,
		Title:      title,
		Workspace:  workspace,
		drv:        drv,
		toolCtx:    toolCtx,
		effort:     l.effortFor(l.cfg.Selection),
		providerID: l.cfg.Selection.ProviderID,
		model:      l.cfg.Selection.Model,
		cfg:        &l.cfg,
		host:       l,
	}
	l.sessions = append(l.sessions, session)
	l.active = len(l.sessions) - 1
	for _, obs := range l.observersSnapshot() {
		obs.SessionsChanged()
	}
	l.emit(Change{SessionID: session.ID, Kind: "sessions"})
	return session
}

// Active 返回当前会话（没有则 nil）。
func (l *Local) Active() *Session {
	if l.active < 0 || l.active >= len(l.sessions) {
		return nil
	}
	return l.sessions[l.active]
}

// SetActive 切换当前会话。
func (l *Local) SetActive(id string) {
	for index, session := range l.sessions {
		if session.ID == id {
			l.active = index
			return
		}
	}
}

// StartupError 返回启动时遇到的问题（空表示正常）。
func (l *Local) StartupError() string { return l.startupErr }

// Notices 返回某个会话的显示型提示（展示端连上时随快照带过去）。
func (l *Local) Notices(sessionID string) []Notice { return l.notices[sessionID] }

func (l *Local) deleteSession(id string) CmdResult {
	for index, session := range l.sessions {
		if session.ID != id {
			continue
		}
		session.cancelTurn()
		session.cancelSummary()
		if session.journal != nil {
			_ = session.journal.Close()
			session.journal = nil
		}
		if l.cfg.Catalog != nil {
			if err := l.cfg.Catalog.Delete(id); err != nil {
				l.reportError(id, l.cfg.Tr("session.deleteFail", err.Error()))
			}
		}
		l.sessions = append(l.sessions[:index], l.sessions[index+1:]...)
		if l.active >= len(l.sessions) {
			l.active = len(l.sessions) - 1
		}
		if l.active < 0 {
			l.active = 0
		}
		for _, obs := range l.observersSnapshot() {
			obs.SessionsChanged()
		}
		l.emit(Change{SessionID: id, Kind: "sessions"})
		return CmdResult{OK: true}
	}
	return CmdResult{Error: "会话不存在"}
}

// ---- 启动恢复 ----

// restoreSessions 从数据目录恢复已有会话。
//
// **一个都没有时不自动新建**（用户选 B：删光会话或全新安装 → 界面显示空态）。
func (l *Local) restoreSessions() {
	if l.cfg.Catalog == nil {
		// 纯内存后端（测试/嵌入）：没有"上次删光、重启恢复"这回事，给一个空会话
		l.NewSession("")
		return
	}
	if removed, pruneErr := l.cfg.Catalog.PruneEmpty(); pruneErr == nil && len(removed) > 0 {
		log.Print(l.cfg.Tr("session.pruned", len(removed)))
	}
	summaries, err := l.cfg.Catalog.List()
	if err != nil {
		l.startupErr = l.cfg.Tr("session.listFail", err)
	}
	failed := 0
	firstReason := ""
	for _, summary := range summaries {
		session, openErr := l.openSession(summary.ContextID, summary.Title)
		if openErr != nil {
			failed++
			if firstReason == "" {
				firstReason = l.cfg.Tr("session.restoreFail", summary.ContextID, openErr)
			}
			continue
		}
		l.sessions = append(l.sessions, session)
	}
	if failed > 0 {
		l.startupErr = l.cfg.Tr("session.restoreFailMany", failed, firstReason)
	}
	if len(l.sessions) > 0 {
		l.active = len(l.sessions) - 1
	} else {
		l.active = 0
	}
}

func (l *Local) openSession(contextID, title string) (*Session, error) {
	// 旧版本 headless 没设置 locale，把 locale key 直接写成了标题：
	// 加载时还原成正常的默认标题，免得侧栏一直显示 "session.defaultTitle"。
	if title == "" || title == "session.defaultTitle" {
		title = l.cfg.Tr("session.defaultTitle")
	}
	logStore, err := l.cfg.Catalog.Open(contextID)
	if err != nil {
		return nil, err
	}
	loaded, err := logStore.Load()
	if err != nil {
		_ = logStore.Close()
		return nil, err
	}
	if len(loaded.Events) == 0 {
		_ = logStore.Close()
		return nil, errors.New(l.cfg.Tr("session.logEmpty"))
	}
	drv, replayed, err := driver.Load(loaded.Events, l.driverOptionsFor(l.cfg.Selection, l.cfg.Workspace))
	if err != nil {
		_ = logStore.Close()
		return nil, err
	}
	if replayed.Title != "" {
		title = replayed.Title
	}
	// 重放里的 session.created/renamed 也可能带着旧的 locale key（旧 headless 写的）
	if title == "" || title == "session.defaultTitle" {
		title = l.cfg.Tr("session.defaultTitle")
	}
	workspace := replayed.Workspace
	if workspace == "" {
		workspace = l.cfg.Workspace
	}
	toolCtx := tools.NewToolContext(workspace, l.cfg.AllowShell)
	session := &Session{
		ID:          contextID,
		Title:       title,
		Workspace:   workspace,
		drv:         drv,
		toolCtx:     toolCtx,
		journal:     logStore,
		keptFromSeq: keptFromSeq(loaded.Events),
		effort:      drv.ReasoningEffort(),
		providerID:  drv.Selection().ProviderID,
		model:       drv.Selection().Model,
		cfg:         &l.cfg,
		host:        l,
	}
	// driver 的 Selection 来自重放（可能不是默认模型）：窗口/输出/vision/档位按它同步
	session.syncModelOptions(drv.Selection())

	// 修复事件必须在接受任何新事件之前写进日志，否则下次加载又会重新计算
	for _, repair := range replayed.Repairs {
		if err := logStore.Append(repair); err != nil {
			break
		}
	}
	if text, needed := drv.NeedsDefaultWindowNotice(); needed {
		_, _ = drv.Conversation().AddNotice(conversation.NoticeInfo, text)
	}
	if len(replayed.Warnings) > 0 {
		_, _ = drv.Conversation().AddNotice(conversation.NoticeWarn,
			l.cfg.Tr("session.logCorrupt")+strings.Join(replayed.Warnings, "；"))
	}
	return session, nil
}

func keptFromSeq(events []store.Event) uint64 {
	for _, event := range events {
		if event.Type == store.TypeCompactSnapshot {
			return store.KeptFromSeq(event.Data)
		}
	}
	return 0
}

// ---- 选项 ----

// driverOptionsFor 用指定工作区构造 driver 选项。
//
// 工作区必须按**会话**传（不能一律用 l.cfg.Workspace）：每个会话可以是不同的
// 项目目录，而 `Workspace` 会被写进 session.created 事件、决定重放后的分组。
// 之前 NewSession(workspace) 只把目录给了 Session 与 toolCtx，driver 仍拿全局
// 默认值，于是「添加项目」建出来的会话在重放后会掉回默认项目。
func (l *Local) driverOptionsFor(sel providers.Selection, workspace string) driver.Options {
	return driver.Options{
		Selection:          sel,
		SystemPrompt:       l.cfg.SystemPrompt,
		Tools:              tools.Builtin(l.cfg.AllowShell),
		MaxToolRounds:      l.cfg.MaxToolRounds,
		ContextWindow:      l.cfg.ContextWindow,
		MaxOutputTokens:    l.maxOutputFor(sel),
		ReasoningEffort:    l.effortFor(sel),
		DisableAutoCompact: l.cfg.DisableAutoCompact,
		Vision:             l.visionFor(sel),
		Blobs:              l.blobReader(),
		Workspace:          workspace,
		Tr:                 l.cfg.Tr,
	}
}

func (l *Local) blobReader() providers.BlobReader {
	if l.cfg.Blobs == nil {
		return nil
	}
	return l.cfg.Blobs.Open
}

func (l *Local) visionFor(sel providers.Selection) bool {
	if l.cfg.VisionFor == nil {
		return false
	}
	return l.cfg.VisionFor(sel)
}

func (l *Local) maxOutputFor(sel providers.Selection) int {
	if l.cfg.MaxOutputFor != nil {
		return l.cfg.MaxOutputFor(sel)
	}
	return 0
}

func (l *Local) effortFor(sel providers.Selection) string {
	if l.cfg.EffortFor != nil {
		return l.cfg.EffortFor(sel)
	}
	return ""
}

func (l *Local) effortsFor(sel providers.Selection) []string {
	if l.cfg.Efforts == nil {
		return nil
	}
	return l.cfg.Efforts(sel)
}

// ---- 内部工具 ----

func (l *Local) emit(change Change) { l.subs.emit(change) }

func (l *Local) applyEffects(session *Session, effects []driver.Effect) {
	session.applyEffects(effects)
}

// reportError 把执行端的错误告诉界面（errMsg 是界面状态，只有 uiapp 持有）。
//
// host 不自己存错误文本：命令失败走 cmdResult，后台失败走 notice，
// 两条通道都能到界面，没必要再维护第二份状态。
func (l *Local) reportError(sessionID, text string) {
	if sessionID == "" || text == "" {
		return
	}
	l.emit(Change{SessionID: sessionID, Kind: "cmdResult", Result: CmdResult{OK: false, Error: text}})
}

// notifyNoPersistence 给一个会话加"没有持久化"的显示型提示（每会话一次）。
func (l *Local) notifyNoPersistence(sessionID string) {
	key := "no-persist:" + sessionID
	l.mu.Lock()
	if l.warned[key] {
		l.mu.Unlock()
		return
	}
	l.warned[key] = true
	l.mu.Unlock()
	l.NotifyDisplay(sessionID, "执行端未启用持久化，更早的历史无法同步")
}

// NotifyDisplay 追加一条只用于显示的提示（不写日志；展示端会随快照收到）。
func (l *Local) NotifyDisplay(sessionID, text string) {
	l.warn(sessionID, text)
}

// warn 追加一条显示型提示（不写日志）。
func (l *Local) warn(sessionID, text string) {
	// 位置锚点：这条提示发生在"当前最后一条"之后
	anchor := ""
	if session := l.Session(sessionID); session != nil {
		if entries := session.Conversation().Entries(); len(entries) > 0 {
			anchor = entries[len(entries)-1].ID()
		}
	}
	l.mu.Lock()
	l.noticeN++
	notice := Notice{ID: strconv.Itoa(l.noticeN), Level: "warn", Text: text, After: anchor}
	l.notices[sessionID] = append(l.notices[sessionID], notice)
	l.mu.Unlock()
	if session := l.Session(sessionID); session != nil {
		_, _ = session.Conversation().AddNotice(conversation.NoticeWarn, text)
	}
	for _, obs := range l.observersSnapshot() {
		obs.Notice(sessionID, notice)
	}
	l.emit(Change{SessionID: sessionID, Kind: "notice", Notice: &notice})
}

// AddObserver 注册一个观察者（每条连接各注册一次；不是覆盖式）。
func (l *Local) AddObserver(obs Observer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.observers = append(l.observers, obs)
}

// RemoveObserver 注销观察者（只影响它自己）。
func (l *Local) RemoveObserver(obs Observer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, existing := range l.observers {
		if existing == obs {
			l.observers = append(l.observers[:i], l.observers[i+1:]...)
			return
		}
	}
}

func (l *Local) observersSnapshot() []Observer {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Observer(nil), l.observers...)
}
