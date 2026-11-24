package host

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
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
	// ID 与 Name 都用执行端自己的名字（`--name` 或机器名）。
	//
	// 以前这里写死 "local"：握手时的 HostName 改了还不够——展示端后来
	// 调用 Info() 拿到的仍是 "local"，界面上就一直是 local。
	// ID 用显示名（而不是 epoch）：展示端要靠它区分"我连的是哪台"。
	name := l.displayName()
	return HostInfo{ID: name, Name: name, Local: true, AllowShell: l.cfg.AllowShell}
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
	// 项目档案：按会话 id 查它的项目和归档状态。
	//
	// 一次读全表（项目数很少），比每个会话查一次便宜。
	projectOf := map[string]string{}   // 会话 id → 项目名
	projectIDOf := map[string]string{} // 会话 id → 项目 id
	archivedOf := map[string]bool{}
	// 按工作区查项目：留给"还没登记进档案"的新会话用（见下面回退）。
	byWorkspace := map[string]ProjectRef{}
	if l.cfg.Catalog != nil {
		if projects, err := l.cfg.Catalog.ListProjects(); err == nil {
			for _, project := range projects {
				key := normalizeWorkspaceKey(project.Workspace)
				if key != "" {
					byWorkspace[key] = ProjectRef{ID: project.ID, Name: project.Name}
				}
				for _, record := range project.Sessions {
					projectOf[record.ID] = project.Name
					projectIDOf[record.ID] = project.ID
					archivedOf[record.ID] = record.ArchivedAt != ""
				}
			}
		}
	}
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
		projectID := projectIDOf[session.ID]
		projectName := projectOf[session.ID]
		if projectID == "" {
			// **新会话还没登记进档案**（还没产生第一条内容，ensurePersisted
			// 没跑）。这时按工作区找它该属于哪个项目 —— 否则界面会拿
			// workspace 路径当分组 key，而其它会话用的是项目 ID，
			// 于是同一个项目显示成**两个同名分组**（真实踩过：手机上新
			// 建会话，侧栏先出现两个 simple-agent，聊两句又合成一个）。
			if ref, ok := byWorkspace[normalizeWorkspaceKey(session.Workspace)]; ok {
				projectID = ref.ID
				projectName = ref.Name
			}
		}
		out = append(out, SessionRef{
			ID: session.ID, Title: session.Title,
			Workspace:   session.Workspace,
			UpdatedAt:   updated,
			ProjectID:   projectID,
			ProjectName: projectName,
			Archived:    archivedOf[session.ID],
		})
	}
	return out
}

// ProjectRef 是项目的最小标识（分组用，不需要完整档案）。
type ProjectRef struct {
	ID   string
	Name string
}

// normalizeWorkspaceKey 把工作区路径规范化成查表用的 key。
//
// 与 store 里的比较口径一致：斜杠统一、去掉尾部斜杠、小写
// （Windows 路径大小写不敏感）。
func normalizeWorkspaceKey(workspace string) string {
	cleaned := strings.TrimRight(strings.ReplaceAll(workspace, "\\", "/"), "/")
	return strings.ToLower(cleaned)
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

// AppendMessagesForPerfTest 往会话里追加一轮"用户问 + 助手答"（性能测试用）。
//
// 直接改 conversation，不走 driver：目的是快速造出长会话内容，
// 用来量"重建 timeline"的代价，不关心事件流。
func (l *Local) AppendMessagesForPerfTest(sessionID, userText, assistantText string) {
	session := l.Session(sessionID)
	if session == nil {
		return
	}
	conv := session.Conversation()
	next := len(conv.Entries())
	_, _ = conv.AddMessage(message.Message{
		MessageID: fmt.Sprintf("perf-u-%d", next),
		Role:      message.RoleUser,
		Parts:     []message.Part{message.NewTextPart(userText)},
	}, conversation.StatusComplete)
	_, _ = conv.AddMessage(message.Message{
		MessageID: fmt.Sprintf("perf-a-%d", next+1),
		Role:      message.RoleAgent,
		Parts:     []message.Part{message.NewTextPart(assistantText)},
	}, conversation.StatusComplete)
}

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
// DispatchForTest 同步执行一条命令并返回结果（测试用）。
//
// 正常入口 Submit 是异步的（结果经 Change 回调），测试里要断言
// "命令被拒绝"就必须拿到返回值。
func (l *Local) DispatchForTest(sessionID string, cmd Command) CmdResult {
	session := l.Session(sessionID)
	return l.dispatch(session, cmd)
}

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
	// 归档的会话**不能运行**：发消息、压缩、改名等一律拒绝，必须显式恢复。
	//
	// 在这里统一拦而不是散在各个方法里：入口有十几个，逐个加判断
	// 迟早会漏一个（漏了就是"归档了还能跑"，比不做更糟）。
	// 只读操作（翻历史、加载更早归档）不受影响。
	if reason := session.blockedByArchive(cmd.Kind); reason != "" {
		return CmdResult{Error: reason}
	}
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
	case "retry":
		// 重发这一轮请求（网络失败后的一键重试）：
		// **不追加用户消息**，所以不会弄脏历史、也不会让缓存前缀失效。
		return session.retry()
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
	case "archiveSession":
		return l.setArchived(cmd.SessionID, true)
	case "restoreSession":
		return l.setArchived(cmd.SessionID, false)
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
	case "backfill":
		// 展示端翻到本地镜像最早时，回头要"seq < before"的更早事件。
		var payload BackfillPayload
		if len(cmd.Payload) > 0 {
			_ = json.Unmarshal(cmd.Payload, &payload)
		}
		data, err := session.backfill(payload.Before, payload.Limit)
		if err != nil {
			return CmdResult{Error: err.Error()}
		}
		return CmdResult{OK: true, Data: data}
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
	// **立刻登记进项目档案**，不等第一条消息。
	//
	// 为什么：界面的侧栏分组用项目 ID。不登记的话新会话没有 ProjectID，
	// 界面只好退回按 workspace 分组 —— 而它和其它已登记会话用的是项目 ID，
	// 同一个项目就显示成**两个同名分组**（真实踩过：手机新建几个会话，
	// 桌面侧栏出现两组 simple-agent / wow-viewer）。
	//
	// 登记只是往 project.json 里加一条 id，很快；空会话重启后本来就会被
	// PruneEmpty 清掉，档案里多一条记录无害（Catalog.Delete 会一并移除）。
	//
	// **登记前先打开日志**：让会话落在老布局 `sessions/<id>/`，
	// 与已有会话一致。反过来的话 Catalog.Open 会按"项目目录"建路径，
	// 把新会话塞进 projects/<uuid>/sessions/ 下 —— 用户的数据位置
	// 不该因为这次改动而变。
	if l.cfg.Catalog != nil {
		if dir, err := l.cfg.Catalog.SessionDirFor(session.ID); err == nil {
			_ = l.cfg.Catalog.AddSession(session.ID, session.Title, workspace, dir)
		}
	}
	l.attachMailbox(session)
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

// setArchived 归档/恢复一个会话。
//
// 归档 = 封存：历史仍可查看，但不能发消息、压缩、改名，必须恢复才能继续。
// **不移动、不删除任何文件**，只是项目档案里的一个状态位。
func (l *Local) setArchived(id string, archived bool) CmdResult {
	session := l.Session(id)
	if session == nil {
		return CmdResult{Error: "会话不存在"}
	}
	// 正在跑的时候不让归档：归档后运行中的回合会失去归属，
	// 而它还会继续写日志（状态自相矛盾）。让用户先停。
	if archived && session.Busy() != driver.BusyIdle {
		return CmdResult{Error: l.cfg.Tr("session.archiveBusy")}
	}
	if err := session.SetArchived(archived); err != nil {
		return CmdResult{Error: err.Error()}
	}
	// 让侧栏重画："sessions" 这类变更会让界面重新同步列表与项目信息
	l.emit(Change{SessionID: session.ID, Kind: "sessions"})
	return CmdResult{OK: true}
}

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

// errSessionGone 表示档案里有记录但目录已经不在了。
//
// 单独一个哨兵值（不用普通 error）：启动恢复时它要**静默跳过**，
// 而不是计进"恢复失败"——后者会让界面弹一串没意义的提示。
var errSessionGone = errors.New("会话目录已不存在")

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
	// 再清档案里"指向不存在目录"的记录。
	//
	// 新建会话时会立刻登记进档案（为了侧栏有稳定的项目 ID），但空会话
	// 不落盘 —— 重启后目录不存在而记录还在，启动时按它去打开就会得到
	// "Could not restore session: The log is empty" 这类噪音提示。
	// 顺序在 PruneEmpty 之后：先清空目录，再清指向它们的记录。
	if removed, pruneErr := l.cfg.Catalog.PruneMissing(); pruneErr == nil && len(removed) > 0 {
		log.Printf("已从项目档案移除 %d 个目录不存在的会话记录", len(removed))
	}
	// 老布局（sessions/<id>/）的会话要先登记进项目档案，
	// 否则启动只读档案时它们会从侧栏消失（文件还在，只是没人认识）。
	if _, migrateErr := l.cfg.Catalog.MigrateLegacySessions(); migrateErr != nil {
		log.Printf("迁移老会话失败：%v", migrateErr)
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
			// 目录已经不在了：这不是"恢复失败"，是这条记录本就该消失
			// （PruneMissing 已尽力清理，这里兜住竞态与手工删除）。
			// 不计入失败数 —— 否则界面会弹一串"无法恢复"的噪音。
			if errors.Is(openErr, errSessionGone) {
				continue
			}
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

// openSession 打开并重放一个已有会话。
func (l *Local) openSession(contextID, title string) (*Session, error) {
	// 目录不存在 = 这条档案记录已经没数据了（空会话没落盘就被登记，
	// 或者用户手工删了目录）。**直接跳过，不要 Open**：
	// Open 里有 MkdirAll，会白建一个空目录，然后下一个报"日志为空"，
	// 下次启动又被 PruneEmpty 删掉 —— 白白制造噪音。
	if l.cfg.Catalog != nil {
		if dir, err := l.cfg.Catalog.SessionDirFor(contextID); err == nil {
			if _, statErr := os.Stat(dir); statErr != nil {
				return nil, errSessionGone
			}
		}
	}
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
		// 日志为空 = 这个会话从没产生过内容（新建后没发消息）。
		//
		// 不是"恢复失败"，是**该清掉的残留**：Open 里的 MkdirAll 会建出
		// 空文件，删掉目录即可。删除失败也不报错 —— 下次启动再试，
		// 否则用户会看到 "N sessions failed to restore" 这种没意义的噪音。
		//
		// 注意只删自己刚建的那个目录：有归档/备份文件的会话走不到这里
		// （那些日志非空）。
		if dir, err := l.cfg.Catalog.SessionDirFor(contextID); err == nil {
			_ = os.RemoveAll(dir)
		}
		// 档案里也要移除：留着的话下次启动还会试着打开它，
		// 又走一遍"建目录 → 发现为空 → 删目录"。
		_ = l.cfg.Catalog.DropSession(contextID)
		return nil, errSessionGone
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
	// **把解析出的工作区回写进 driver**。
	//
	// Load 时的 opts 用的是 l.cfg.Workspace（启动目录）——那时还不知道
	// 这个会话真正属于哪个项目。不回写的话，下一次压缩会把启动目录
	// 当成本会话的工作区写进快照，从此固化下来（真实踩过：
	// wow-viewer 的会话压一次就永远变成 simple-agent 的了）。
	drv.SetWorkspace(workspace)
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

	// 归档状态来自项目档案：**不读它的话，重启后归档的会话又能跑了**
	// （档案是唯一权威，日志里没有这个状态）。
	if l.cfg.Catalog != nil {
		if archived, ok := l.cfg.Catalog.SessionArchived(contextID); ok {
			session.archived = archived
		}
	}

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
	// 会话通讯：填项目 ID + 挂工具 + 挂请求注入钩子。
	// 恢复路径也要挂 —— 否则重启后老会话收不到也发不出邮件（真实会踩）。
	l.attachMailbox(session)
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
		Selection:    sel,
		SystemPrompt: l.cfg.SystemPrompt,
		Tools: tools.BuiltinWith(tools.Options{
			AllowShell: l.cfg.AllowShell,
			WebFetch:   l.cfg.WebFetch,
			Searcher:   l.cfg.Searcher,
		}),
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
