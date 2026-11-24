package host

import (
	"context"
	"errors"
	"log"
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

// Session 是执行端的一条会话：**唯一**持有 driver、日志、工具上下文的对象。
//
// uiapp 通过嵌入或委托使用它的**读方法**；所有写操作都必须经 `Host.Submit`
// 走命令队列（见 local.go），这样展示端接进来以后，命令与事件天然串行。
type Session struct {
	ID        string
	Title     string
	Workspace string
	// ProjectID 是所属项目（邮件按项目隔离，靠它拼邮箱目录）。
	//
	// 在 host 里赋值而不是构造参数：Session 的构造点有好几处（新建、恢复、
	// 展示端镜像），项目 ID 只有 host 知道，零值表示"不属于任何项目"
	// —— 那时会话通讯直接不可用。
	ProjectID string

	drv     *driver.Driver
	journal store.SessionStore
	toolCtx *tools.ToolContext

	// ---- 展示端（镜像）用的字段 ----
	//
	// 展示端不跑 driver，只持有一个只读镜像与一份瞬时快照（view）。读方法在
	// view != nil 时返回快照里的值，命令仍然经连接提交给执行端。
	mirror *driver.Mirror
	conv   *conversation.Conversation
	view   *ViewState

	// turnCtx/turnCancel 是回合级生命周期（工具执行、摘要请求都用它）。
	turnCtx    context.Context
	turnCancel context.CancelFunc

	// askGroups/askKey 是展示端用本地交互副本（见 AskGroups）。
	askGroups []*driver.AskGroup
	askKey    string
	// autoRetry 是"连接中断后自动继续"的次数（每轮最多 1 次，用户发新消息时清零）。
	autoRetry int
	// retryPending 表示"已失败、正在等自动重试"（界面据此显示"重试中"）。
	//
	// 为什么需要这个标志：自动重试有退避（最多 8 秒）。如果界面照常显示
	// 红色失败，用户会看到失败闪一下又变回来，以为程序出毛病了。
	retryPending bool
	// keptFromSeq 是快照的归档截断点（更早的归档在界面上只显示、不参与请求）。
	keptFromSeq uint64
	// history 是 backfill 补拉进来的**更早**条目（展示端专用）。
	//
	// 镜像只收到最近 attachTailEvents 条事件，翻页翻到最早时向执行端补拉，
	// 结果存这里，构建时间线时拼在镜像 entries 前面。
	// **不能塞进 mirror**：那段 API 是追加到末尾的，会让时间线顺序错乱。
	historyMu sync.Mutex
	history   []conversation.Entry

	// summaryCancel 是当前摘要请求的取消函数（停止/解压时用）。
	summaryCancel context.CancelFunc

	providerID string
	model      string
	effort     string
	// generation 是持久状态的"代"（压缩后 +1）：续传游标要带它。
	generation uint64

	cfg  *Config
	host *Local

	// archived 表示会话**已归档**：还能看历史，但不能再运行。
	//
	// 语义与"压缩"不同：压缩是日志层面的折叠（继续可用），归档是会话层面的
	// 封存（必须显式恢复才能发消息）。存的项目档案里的 archived_at。
	archived bool
}

// Archived 报告会话是否已归档。
func (s *Session) Archived() bool { return s.archived }

// SetArchived 设置归档状态并落盘。
//
// 归档只是状态位：**不动任何日志文件**。恢复就是清掉它。
func (s *Session) SetArchived(archived bool) error {
	if s.cfg.Catalog == nil {
		s.archived = archived
		return nil
	}
	var stamp string
	if archived {
		stamp = time.Now().UTC().Format(time.RFC3339)
	}
	err := s.cfg.Catalog.UpdateSession(s.ID, func(record *store.SessionRecord) {
		record.ArchivedAt = stamp
	})
	if err != nil {
		return err
	}
	s.archived = archived
	return nil
}

// canRun 报告这个会话现在能不能执行（发消息、压缩、改名等）。
//
// 返回的字符串为空表示可以，否则是给用户看的原因。
func (s *Session) canRun() string {
	if s.archived {
		return s.cfg.Tr("session.archivedHint")
	}
	return ""
}

// blockedByArchive 报告某个命令是否因为会话已归档而被拒绝。
//
// **白名单式**：只有明确列出的"只读"命令放行，其余一律拦。
// 反过来（列黑名单）只要新增一个会改状态的命令就会漏 ——
// "归档了还能跑"比"归档后某个只读操作被误拦"严重得多。
func (s *Session) blockedByArchive(command string) string {
	// session 为 nil：命令不针对具体会话（新建会话走的就是这条）。
	// 没有会话可归档，自然不该拦。
	if s == nil || !s.archived {
		return ""
	}
	switch command {
	case "loadOlder", "backfill":
		// 翻历史是纯读：归档的会话也可以往回看
		return ""
	case "archiveSession", "restoreSession":
		// 归档自身：恢复必须能做（否则永远出不来）
		return ""
	}
	// 其余全部拒绝：send/enqueue/compact/rename/deleteSession/
	// selectModel/setEffort/answer/dismissAsk/stop…
	return s.cfg.Tr("session.archivedHint")
}

// ---- 读接口（全部读本地状态，无网络等待）----

func (s *Session) Conversation() *conversation.Conversation {
	if s.mirror != nil {
		// 重放/增量应用可能**替换**会话对象（session.created / compact.snapshot），
		// 所以每次都要从镜像里取当前的，不能缓存指针。
		return s.mirror.Conversation()
	}
	return s.drv.Conversation()
}

// ApplyHistory 把一段**更早的**持久事件补进会话（backfill 用）。
//
// 展示端的镜像只收到最近 attachTailEvents 条事件，翻页翻到最早时其实
// 磁盘上还有更多。补拉回来的那批通过这里记住，之后构建时间线时
// 拼在镜像 entries **前面**。
//
// **为什么不做成 Mirror.ApplyPersisted**：那是追加到会话末尾的，
// 而这些事件在时间上更早 —— 追加会让时间线顺序变成"新的、老的、新的"，
// 分页的 seq 区间也就乱了（真机表现为 seq_from 比 seq_to 还大）。
//
// 只保留内容事件（调用方保证），并按 seq 去重/升序排好。
func (s *Session) ApplyHistory(events []store.Event) int {
	replayed, err := driver.Replay(nil)
	if err != nil {
		log.Printf("host：补入历史失败（重放器初始化）：%v", err)
		return 0
	}
	applied := 0
	for _, event := range events {
		if err := driver.ApplyPersisted(&replayed, event); err != nil {
			log.Printf("host：补入历史事件失败（seq=%d type=%s）：%v", event.Seq, event.Type, err)
			continue
		}
		applied++
	}
	if applied == 0 {
		return 0
	}

	fresh := replayed.Conversation.Entries()

	s.historyMu.Lock()
	defer s.historyMu.Unlock()

	// **按消息 id 去重**：翻页的 before 是逐步前移的，相邻两次补拉的区间
	// 会在边界处重叠；而且用户来回滑动时会重复请求同一段。
	// 不去重的话同一条消息会在时间线里出现两次 —— 更糟的是
	// timeline.Build 会给同一个 ChatEntry 生成两套 Ref，
	// 一旦其中一份的 Parts 被改写，Ref.Part() 就会越界 panic
	// （真机上表现为 App 直接消失，日志只有一行 panic）。
	seen := make(map[string]bool, len(s.history)+len(fresh))
	for _, entry := range s.history {
		if entry.Message != nil && entry.Message.Message != nil {
			seen[entry.Message.Message.MessageID] = true
		}
	}
	merged := make([]conversation.Entry, 0, len(fresh)+len(s.history))
	// 新来的这批更早，整体排在已有历史之前
	for _, entry := range fresh {
		if entry.Message != nil && entry.Message.Message != nil {
			id := entry.Message.Message.MessageID
			if seen[id] {
				continue
			}
			seen[id] = true
		}
		merged = append(merged, entry)
	}
	s.history = append(merged, s.history...)
	return applied
}

// historyEntries 返回补拉进来的更早条目（可能为空）。
func (s *Session) historyEntries() []conversation.Entry {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	return s.history
}

// OldestContentSeq 返回当前**能看到**的最早内容事件 seq（0 表示还没有内容）。
//
// 包括 backfill 补拉进来的那段：那个更早，所以优先用它。
// 翻页用它判断"是不是已经翻到边界"：到了才值得向执行端补拉。
func (s *Session) OldestContentSeq() uint64 {
	var oldest uint64
	for _, entry := range s.historyEntries() {
		if entry.Message == nil {
			continue
		}
		first, _ := entry.Message.SeqRange()
		if first > 0 && (oldest == 0 || first < oldest) {
			oldest = first
		}
	}
	if oldest != 0 {
		return oldest
	}
	if s.mirror == nil {
		return 0
	}
	for _, entry := range s.mirror.Conversation().Entries() {
		if entry.Message == nil {
			continue
		}
		first, _ := entry.Message.SeqRange()
		if first > 0 && (oldest == 0 || first < oldest) {
			oldest = first
		}
	}
	return oldest
}

// EntriesWithHistory 返回"补拉的历史 + 镜像当前内容"，按时间顺序拼好。
//
// 构建时间线必须用这个而不是 Conversation().Entries()：镜像里只有最近
// attachTailEvents 条，补拉回来的更早内容在 history 里。
func (s *Session) EntriesWithHistory() []conversation.Entry {
	history := s.historyEntries()
	current := s.Conversation().Entries()
	if len(history) == 0 {
		return current
	}
	merged := make([]conversation.Entry, 0, len(history)+len(current))
	merged = append(merged, history...)
	return append(merged, current...)
}

func (s *Session) Busy() driver.Busy {
	if s.view != nil {
		return driver.Busy(s.view.Busy)
	}
	return s.drv.Busy()
}

// RetryPending 报告"这一轮已经失败，正在等自动重试"。
//
// 界面用它把失败行显示成「正在重试…」而不是红色失败 —— 否则用户会看到
// 失败闪一下又变回来，以为程序出了问题。只有重试用完（或不可重试）时，
// 才真正停在失败态。
func (s *Session) RetryPending() bool {
	if s.view != nil {
		return s.view.RetryPending
	}
	return s.retryPending
}

func (s *Session) Selection() providers.Selection {
	if s.view != nil {
		return s.view.Selection
	}
	return s.drv.Selection()
}

func (s *Session) Effort() string {
	if s.view != nil {
		return s.view.Effort
	}
	return s.effort
}

func (s *Session) ProviderID() string {
	if s.view != nil {
		return s.view.Selection.ProviderID
	}
	return s.providerID
}

func (s *Session) Model() string {
	if s.view != nil {
		return s.view.Selection.Model
	}
	return s.model
}

func (s *Session) Queued() []string {
	if s.view != nil {
		return append([]string(nil), s.view.Queued...)
	}
	return s.drv.Queued()
}

func (s *Session) HasQueued() bool { return len(s.Queued()) > 0 }

func (s *Session) CanSteer() bool {
	if s.view != nil {
		return s.view.Busy == string(driver.BusyStreaming) || s.view.Busy == string(driver.BusyRunningTools)
	}
	return s.drv.CanSteer()
}

func (s *Session) CanSend() bool {
	if s.view != nil {
		return s.view.Busy == string(driver.BusyIdle)
	}
	return s.drv.CanSend()
}

func (s *Session) Compacting() bool {
	if s.view != nil {
		return s.view.Compacting
	}
	return s.drv.Compacting()
}

func (s *Session) AskGroups() []*driver.AskGroup {
	if s.view == nil {
		return s.drv.AskGroups()
	}
	// 展示端：按快照里的提问在本地建一份可交互的组（提交时把选择回传执行端）。
	// 组只在"提问集合"变化时重建，避免每帧重置用户的勾选。
	key := ""
	for _, ask := range s.view.Asks {
		key += ask.MessageID + "|" + ask.CallID + "|"
		for _, question := range ask.Questions {
			key += question.ID + ","
		}
	}
	if key == s.askKey && s.askGroups != nil {
		return s.askGroups
	}
	s.askKey = key
	s.askGroups = nil
	for _, ask := range s.view.Asks {
		specs := make([]tools.QuestionSpec, 0, len(ask.Questions))
		for _, question := range ask.Questions {
			spec := tools.QuestionSpec{ID: question.ID, Question: question.Question, Header: question.Header, MultiSelect: question.MultiSelect}
			for _, option := range question.Options {
				spec.Options = append(spec.Options, tools.QuestionOption{Label: option})
			}
			specs = append(specs, spec)
		}
		s.askGroups = append(s.askGroups, driver.NewAskGroup(ask.MessageID, ask.CallID, specs))
	}
	return s.askGroups
}

func (s *Session) Usage() driver.Usage {
	if s.view != nil {
		return driver.Usage{
			InputTokens:      s.view.Usage.Input,
			OutputTokens:     s.view.Usage.Output,
			CacheReadTokens:  s.view.Usage.CacheRead,
			CacheWriteTokens: s.view.Usage.CacheWrite,
		}
	}
	return s.drv.Usage()
}

// TotalUsage 返回会话累计用量（所有已完成请求之和）。
//
// 缓存命中率用这个口径：单次请求的命中率抖得厉害（新建缓存那轮 0%、
// 紧接着全命中 99.9%），累计值才反映"这个会话一共省了多少"。
//
// 远程展示端：执行端会下发 total_usage；旧执行端没有该字段 → 零值 →
// 按 0/0 处理（界面不显示命中率），与旧日志的降级策略一致。
func (s *Session) TotalUsage() driver.Usage {
	if s.view != nil {
		return driver.Usage{
			InputTokens:      s.view.TotalUsage.Input,
			OutputTokens:     s.view.TotalUsage.Output,
			CacheReadTokens:  s.view.TotalUsage.CacheRead,
			CacheWriteTokens: s.view.TotalUsage.CacheWrite,
		}
	}
	return s.drv.TotalUsage()
}

func (s *Session) Counts() driver.Counts {
	if s.view != nil {
		return driver.Counts{Turns: s.view.Counts.Turns, Steps: s.view.Counts.Steps}
	}
	return s.drv.Counts()
}

// Todos 返回当前任务清单。
//
// 展示端读快照（`view.Todos`），执行端读 driver——两边都是「重放到当前」的结果，
// 所以远程与本地看到的清单一致。
func (s *Session) Todos() []conversation.TodoItem {
	if s.view != nil {
		if len(s.view.Todos) == 0 {
			return nil
		}
		out := make([]conversation.TodoItem, 0, len(s.view.Todos))
		for _, todo := range s.view.Todos {
			out = append(out, conversation.TodoItem{Content: todo.Content, Status: todo.Status})
		}
		return out
	}
	return s.drv.Todos()
}

func (s *Session) TokensInUse() int {
	if s.view != nil {
		return s.view.Tokens
	}
	return s.drv.TokensInUse()
}

func (s *Session) EffectiveContextWindow() int {
	if s.view != nil {
		if s.view.Window > 0 {
			return s.view.Window
		}
		return driver.DefaultContextWindow
	}
	return s.drv.EffectiveContextWindow()
}

func (s *Session) Vision() bool {
	if s.view != nil {
		return s.view.Vision
	}
	return s.drv.Vision()
}
func (s *Session) KeptFromSeq() uint64             { return s.keptFromSeq }
func (s *Session) Persisted() bool                 { return s.journal != nil }
func (s *Session) ToolContext() *tools.ToolContext { return s.toolCtx }

// TitleValue 返回当前标题（重命名会改它，uiapp 的 UI 副本要跟着同步）。
func (s *Session) TitleValue() string { return s.Title }

// ---- 内部：命令实现（只允许 host 的命令队列调用）----

func (s *Session) send(payload SendPayload) CmdResult {
	s.autoRetry = 0
	s.retryPending = false
	s.ensurePersisted(payload.Text)
	var attachments []message.Part
	for _, ref := range payload.Attachments {
		part := message.NewURLPart("blob:sha256:" + ref.Hash).WithMediaType(ref.MediaType)
		part = part.WithFilename(ref.Name)
		attachments = append(attachments, part)
	}
	effects, err := s.drv.SendWithAttachments(payload.Text, attachments)
	if err != nil {
		return CmdResult{Error: err.Error()}
	}
	s.host.applyEffects(s, effects)
	return CmdResult{OK: true}
}

// retry 重发这一轮请求（界面上"失败 → 重试"按钮）。
//
// 与 send 的关键区别：**不追加用户消息**。模型看到的前缀和失败前完全一样，
// 缓存照常命中；对话历史里也不会多出"继续"这种噪声消息。
//
// autoRetry 清零：这是用户主动重试，不是自动续跑，次数应重新计。
func (s *Session) retry() CmdResult {
	if s.drv.Busy() != driver.BusyIdle {
		return CmdResult{Error: s.cfg.Tr("session.retryBusy")}
	}
	s.autoRetry = 0
	s.retryPending = false
	effects, err := s.drv.Retry()
	if err != nil {
		return CmdResult{Error: err.Error()}
	}
	s.host.applyEffects(s, effects)
	return CmdResult{OK: true}
}

// ensurePersisted 在会话产生第一条内容时才建立日志目录并写入 session.created。
//
// 标题取第一条用户消息的首行（截断），与 harness 的习惯一致：侧栏里一眼能看出
// 这个会话是聊什么的，而不是一排「新会话」。
func (s *Session) ensurePersisted(firstText string) {
	if s.journal != nil || s.cfg.Catalog == nil {
		return
	}
	opened, err := s.cfg.Catalog.Create(s.ID)
	if err != nil {
		s.host.reportError(s.ID, s.cfg.Tr("session.openFail", err.Error()))
		return
	}
	s.journal = opened

	title := s.Title
	if title == "" || title == s.cfg.Tr("session.defaultTitle") {
		if auto := titleFromText(firstText); auto != "" {
			title = auto
			s.Title = auto
			s.syncModelOptions(s.drv.Selection())
			s.host.applyEffects(s, s.drv.Rename(auto))
		}
	}
	for _, effect := range s.drv.Created(title) {
		if record, ok := effect.(driver.Record); ok {
			if err := opened.Append(record.Event); err != nil {
				s.host.reportError(s.ID, s.cfg.Tr("session.openFail", err.Error()))
				return
			}
		}
	}
	// 登记到项目档案：**启动时的会话列表只读档案**，不扫会话目录。
	// 漏了这一步，新会话重启后就会从侧栏消失（文件还在，但没人认识它）。
	//
	// 传 opened 的目录：档案记下实际位置，而不是假设它在项目目录下
	// —— 这样老会话（在 sessions/ 下）也能被正确登记，**不用搬文件**。
	if err := s.cfg.Catalog.AddSession(s.ID, title, s.Workspace, opened.Dir()); err != nil {
		s.host.reportError(s.ID, s.cfg.Tr("session.openFail", err.Error()))
	}
}

// titleFromText 从第一条用户消息里取标题（首行、截断）。
func titleFromText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	text = strings.TrimSpace(text)
	if runes := []rune(text); len(runes) > 24 {
		text = string(runes[:24]) + "…"
	}
	return text
}

func (s *Session) enqueue(text string) CmdResult {
	effects, err := s.drv.Enqueue(text)
	if err != nil {
		return CmdResult{Error: err.Error()}
	}
	s.host.applyEffects(s, effects)
	return CmdResult{OK: true}
}

func (s *Session) dropQueued(index int) CmdResult {
	effects := s.drv.DropQueued(index)
	s.host.applyEffects(s, effects)
	return CmdResult{OK: true}
}

func (s *Session) stop() CmdResult {
	effects := s.drv.Stop()
	s.host.applyEffects(s, effects)
	return CmdResult{OK: true}
}

// submitAsks 提交提问答案。
func (s *Session) submitAsks(payload AnswerPayload) CmdResult {
	// 其他端已经回答/跳过：这里已经没有待回答的提问，别把它当成"提交成功"
	if s.drv.Busy() != driver.BusyAwaitingAnswer {
		return CmdResult{Notice: s.cfg.Tr("ask.answeredElsewhere")}
	}
	// 来自展示端的答案：先把选择应用回执行端自己的提问组，再统一提交。
	//
	// **只在展示端做**（s.view != nil）。本地界面与执行端共享同一个 driver，
	// 它勾选的选项已经在 driver 里了；再 Toggle 一遍等于把每项**切换回未选**
	// —— 模型收到的就是 selected:[]（这正是「多选没发给 LLM」的根因）。
	if s.view != nil {
		for _, answer := range payload.Answers {
			for _, group := range s.drv.AskGroups() {
				if group.CallID != answer.CallID {
					continue
				}
				for index, question := range group.Questions {
					// **顺序不能反：先写文字，再勾选项。**
					//
					// SetCustom（文字非空时）会清掉该题已选的选项 —— 这是有意的：
					// 用户开始写文字就说明这题以文字作答，界面上的选中标记也该消失。
					// 如果先 Toggle 再 SetCustom，刚勾好的选项会被全部清空。
					//
					// 反过来先 SetCustom 再 Toggle 就对了：勾选会把 customActive
					// 置回 false，语义是"以勾选的选项为准"。
					if text := answer.Custom[question.ID]; text != "" {
						group.SetCustom(index, text)
					}
					for _, option := range answer.Selected[question.ID] {
						group.Toggle(index, option)
					}
				}
			}
		}
	}
	effects, err := s.drv.SubmitAsks(payload.Custom)
	if err != nil {
		return CmdResult{Error: err.Error()}
	}
	s.host.applyEffects(s, effects)
	return CmdResult{OK: true}
}

func (s *Session) dismissAsks() CmdResult {
	effects, err := s.drv.DismissAsks()
	if err != nil {
		return CmdResult{Error: err.Error()}
	}
	s.host.applyEffects(s, effects)
	return CmdResult{OK: true}
}

func (s *Session) compact() CmdResult {
	if s.drv.Busy() != driver.BusyIdle {
		return CmdResult{}
	}
	effects, err := s.drv.BeginCompact()
	if err != nil {
		return CmdResult{Error: err.Error()}
	}
	s.host.applyEffects(s, effects)
	return CmdResult{OK: true}
}

func (s *Session) cancelCompact() CmdResult {
	effects := s.drv.CancelCompact(s.cfg.Tr("compact.cancelled"))
	s.host.applyEffects(s, effects)
	return CmdResult{OK: true}
}

func (s *Session) selectModel(sel providers.Selection, effort string) CmdResult {
	if err := s.drv.SetSelection(sel); err != nil {
		return CmdResult{Error: s.cfg.Tr("session.switchBusy")}
	}
	s.syncModelOptions(sel)
	s.providerID, s.model = sel.ProviderID, sel.Model
	// 档位跟模型走：新模型支持就沿用，不支持就回到它的默认档位
	if !containsString(s.host.effortsFor(sel), effort) {
		effort = s.host.effortFor(sel)
	}
	s.effort = effort
	s.host.applyEffects(s, s.drv.SetReasoningEffort(effort))
	s.host.applyEffects(s, s.drv.TakeRecords())
	s.host.emit(Change{SessionID: s.ID, Kind: "runtime"})
	return CmdResult{OK: true}
}

func (s *Session) setEffort(effort string) CmdResult {
	if s.drv.Busy() != driver.BusyIdle {
		return CmdResult{Error: s.cfg.Tr("effort.busy")}
	}
	s.effort = effort
	s.host.applyEffects(s, s.drv.SetReasoningEffort(effort))
	s.host.applyEffects(s, s.drv.TakeRecords())
	return CmdResult{OK: true}
}

func (s *Session) rename(title string) CmdResult {
	title = strings.TrimSpace(title)
	if title == "" {
		return CmdResult{}
	}
	s.Title = title
	s.host.applyEffects(s, s.drv.Rename(title))
	// 回写项目档案：启动列表读的是档案里的标题，不同步的话
	// 改名只在这一进程内可见，重启就变回旧名字。
	if s.cfg.Catalog != nil {
		// 只回写标题：更新时间由 session.jsonl 的文件时间决定，
		// 不需要在这里写（写它等于每次改名都要落盘一次项目档案）。
		_ = s.cfg.Catalog.UpdateSession(s.ID, func(record *store.SessionRecord) {
			record.Title = title
		})
	}
	return CmdResult{OK: true}
}

// ---- 模型相关选项同步 ----

func (s *Session) syncModelOptions(sel providers.Selection) {
	if s.cfg.WindowFor != nil {
		s.drv.SetContextWindow(s.cfg.WindowFor(sel))
	}
	if s.cfg.MaxOutputFor != nil {
		s.drv.SetMaxOutputTokens(s.cfg.MaxOutputFor(sel))
	}
	vision := s.cfg.VisionFor != nil && s.cfg.VisionFor(sel)
	s.drv.SetVision(vision)
	if s.toolCtx != nil {
		s.toolCtx.Vision = vision
	}
	efforts := s.host.effortsFor(sel)
	if !containsString(efforts, s.effort) {
		s.effort = s.host.effortFor(sel)
		s.host.applyEffects(s, s.drv.SetReasoningEffort(s.effort))
	}
}

// ---- 回合 ctx ----

func (s *Session) turnContext() context.Context {
	if s.turnCtx == nil {
		s.turnCtx, s.turnCancel = context.WithCancel(context.Background())
	}
	return s.turnCtx
}

func (s *Session) cancelTurn() {
	if s.turnCancel != nil {
		s.turnCancel()
	}
	s.turnCtx = nil
	s.turnCancel = nil
}

// ---- 由 handl effect 的部分（执行端内部）----

func (s *Session) recordEffect(record driver.Record) {
	// 事件是逻辑事实，不依赖是否落盘：即时同步（没有 Catalog 的会话）同样要
	// 让展示端看到内容，所以先通知观察者，再决定要不要写日志。
	stamped := s.host.recordWireEvent(s.ID, record.Event)
	for _, obs := range s.host.observersSnapshot() {
		obs.Record(s.ID, stamped)
	}
	if s.journal == nil {
		return
	}
	// 压缩快照要**写进归档并重写日志**（journal.Compact）：只当普通事件追加的话
	// 不会有 archive-*.jsonl，「加载更早历史」永远读不到东西。
	if record.Event.Type == store.TypeCompactSnapshot {
		if err := s.journal.Compact(record.Event); err != nil {
			s.host.reportError(s.ID, s.cfg.Tr("session.writeFail", err.Error()))
			return
		}
	} else if err := s.journal.Append(record.Event); err != nil {
		s.host.reportError(s.ID, s.cfg.Tr("session.writeFail", err.Error()))
		return
	}
	if record.Event.Type == store.TypeCompactSnapshot {
		s.generation++
		// 新代的事件不能与旧代混在 ring 里（续传游标带 generation）
		s.host.resetRing(s.ID)
	}
}

// applyEffects 执行 driver 交回的后续动作。
//
// 回合结束（回到 Idle）时收回合 ctx 并把这一轮的事件刷盘：每个 ctx 都必须被取消，
// Sync 的成本是一个回合一次（保证「一轮完整对话」在断电后也还在）。
func (s *Session) applyEffects(effects []driver.Effect) {
	defer func() {
		for _, obs := range s.host.observersSnapshot() {
			obs.StateChanged(s.ID)
		}
	}()
	for _, effect := range effects {
		switch e := effect.(type) {
		case driver.StartStream:
			s.startStream(e)
		case driver.RunTools:
			s.runTools(e)
		case driver.StartSummary:
			s.startSummary(e)
		case driver.DisplayNotice:
			_, _ = s.Conversation().AddNotice(conversation.NoticeInfo, e.Text)
		case driver.Record:
			s.recordEffect(e)
		case driver.Changed:
		case driver.StreamFailed:
			// 失败可重试：交给统一的重试入口（带退避与次数上限）。
			//
			// 注意这里**不能直接重试**：applyEffects 还在展开 effects 的过程中，
			// 重试会立刻产生新的一批 effects（重入）。maybeAutoRetry 用
			// time.AfterFunc 延后执行，天然避开这一点。
			s.maybeAutoRetry(s.cfg.Tr("session.autoRetryNetwork"))
		}
	}
	if s.drv.Busy() == driver.BusyIdle {
		s.cancelTurn()
		if s.journal != nil {
			_ = s.journal.Sync()
		}
	}
	s.host.emit(Change{SessionID: s.ID, Kind: "conversation"})
}

// startStream 起一个 goroutine 读事件流，逐条投递回主线程。
func (s *Session) startStream(effect driver.StartStream) {
	// 每次流用自己的子 ctx：只随这条流结束而取消，不能连带取消回合 ctx（工具要用）。
	streamCtx, streamCancel := context.WithCancel(s.turnContext())
	mid := effect.MessageID
	selection := s.drv.Selection()
	request := effect.Request
	runtime := s.cfg.Runtime

	go func() {
		defer streamCancel()
		events, err := runtime.OpenStream(streamCtx, selection, request)
		if err != nil {
			reason := err.Error()
			if errors.Is(err, context.Canceled) {
				s.post(func() { s.applyEffects(s.drv.StreamClosed(mid)) })
				return
			}
			s.post(func() {
				s.applyEffects(s.drv.FailStreamWith(mid, reason))
				// 不加显式的 maybeAutoRetry：driver 的 failStream 会在
				// 原因可重试时发出 StreamFailed effect，applyEffects 统一处理。
				// 两处都调会重试两次。
			})
			return
		}
		received := 0
		lastKind := "（无）"
		for event := range events {
			received++
			lastKind = string(event.Kind)
			ev := event
			// 增量的序号在**产生处**打上（与快照 Seq 同一个数列）
			if isDeltaEvent(ev.Kind) {
				seq := s.host.nextStreamSeq(s.ID)
				for _, obs := range s.host.observersSnapshot() {
					obs.StreamDelta(s.ID, mid, ev, seq)
				}
			}
			s.post(func() {
				effects, applyErr := s.drv.ApplyEvent(mid, ev)
				if applyErr != nil {
					return
				}
				s.applyEffects(effects)
			})
		}
		s.post(func() { s.finishInterrupted(mid, received, lastKind) })
	}()
}

// finishInterrupted 处理"流没有结束标记就断了"：先按失败收尾，再考虑自动重试。
func (s *Session) finishInterrupted(messageID string, received int, lastKind string) {
	// 正常结束（收到 Done）之后通道也会关闭：那时 driver 已经不是本轮流式状态，
	// 这里什么都不该做——只有"还在流式却关闭了"才算中断。
	if s.drv.Busy() != driver.BusyStreaming || !s.drv.OwnsStream(messageID) {
		return
	}
	effects := s.drv.StreamClosed(messageID)
	s.applyEffects(effects)
	log.Printf("会话 %s：流意外结束（收到 %d 个事件，最后是 %s）", s.ID, received, lastKind)
	// 不在这里调 maybeAutoRetry：StreamClosed → failStream → 发出 StreamFailed
	// effect，applyEffects 里已经统一处理了。重复调会连点两次自动重试。
}

// maybeAutoRetry 在失败后自动重发这一轮请求（带指数退避）。
//
// 为什么值得做（实测数据）：这个网关波动极大 —— 同一个会话，
// 响应头 6s～95s、思考期 78～84s，偶发断流。pi 有退避重试，我们原来只有
// 手动点「重试」。用户每遇到一次抖动就得自己点一下，很烦。
//
// 关键：用 **Retry()** 而不是 **Send("继续")**。
//   - Retry 沿用历史里最后那条用户消息，模型看到的前缀完全一样，
//     缓存照常命中
//   - Send("继续") 会在对话里多一条用户消息：弄脏历史，且让缓存前缀
//     失效（整段缓存作废）
//   以前这里用的就是 Send("继续")，改掉。
//
// 只对**重发有意义**的失败重试（reason 见 isRetryableReason）：
// 401、上下文超限这些重发一万次也一样，重试只会白等两轮。
func (s *Session) maybeAutoRetry(notice string) {
	if s.drv.Busy() != driver.BusyIdle {
		return
	}
	if s.autoRetry >= maxAutoRetry {
		log.Printf("会话 %s：自动重试已达上限（%d 次），保持失败状态", s.ID, s.autoRetry)
		s.setRetryPending(false)
		return
	}
	s.autoRetry++
	// 指数退避：0.8s、1.6s、3.2s… 最多 8s。
	// 断流往往是网关瞬时抖动，立刻重发容易再撞一次；稍等一下成功率高得多。
	delay := retryBackoff(s.autoRetry)
	log.Printf("会话 %s：第 %d 次自动重试（%s 后）", s.ID, s.autoRetry, delay)
	_, _ = s.Conversation().AddNotice(conversation.NoticeWarn, notice)
	// 标记"正在重试"：界面把失败行显示成「正在重试…」，不要闪红。
	s.setRetryPending(true)
	time.AfterFunc(delay, func() {
		s.post(func() {
			if s.drv.Busy() != driver.BusyIdle {
				s.setRetryPending(false)
				return
			}
			effects, err := s.drv.Retry()
			if err != nil {
				log.Printf("会话 %s：自动重试失败：%v", s.ID, err)
				s.setRetryPending(false)
				return
			}
			// 重发成功了：清掉标记，界面回到正常的"运行中"
			s.setRetryPending(false)
			s.applyEffects(effects)
		})
	})
}

// setRetryPending 更新"正在重试"标记并通知观察者。
//
// 状态变了要主动发通知：界面靠 StateChanged 重绘，不发的话标记变了也不刷新。
// SetRetryPending 供测试与展示端设置"正在自动重试"标记。
func (s *Session) SetRetryPending(pending bool) { s.setRetryPending(pending) }

func (s *Session) setRetryPending(pending bool) {
	if s.retryPending == pending {
		return
	}
	s.retryPending = pending
	for _, obs := range s.host.observersSnapshot() {
		obs.StateChanged(s.ID)
	}
}

// maxAutoRetry 是一轮里自动重试的最多次数。
//
// 2 次的经验依据：网关偶发抖动通常一次就好；连续两次都失败，
// 多半不是抖动（配置错、额度问题），再自动重试只是让用户多等。
const maxAutoRetry = 2

// retryBackoff 返回第 n 次重试前的等待时间（0.8s 起，指数增长，上限 8s）。
func retryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := 800 * time.Millisecond
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= 8*time.Second {
			return 8 * time.Second
		}
	}
	return delay
}

// startSummary 发起摘要请求；完成后由 driver 重建会话并写快照。
func (s *Session) startSummary(effect driver.StartSummary) {
	ctx, cancel := context.WithCancel(s.turnContext())
	s.summaryCancel = cancel
	selection := s.drv.Selection()
	request := effect.Request
	runtime := s.cfg.Runtime

	go func() {
		defer cancel()
		events, err := runtime.OpenStream(ctx, selection, request)
		if err != nil {
			s.post(func() {
				s.applyEffects(s.drv.FailCompact(s.cfg.Tr("compact.summaryFailedWhy", err.Error())))
			})
			return
		}
		var builder strings.Builder
		for event := range events {
			switch event.Kind {
			case providers.EventText:
				builder.WriteString(event.Text)
			case providers.EventError:
				message := s.cfg.Tr("compact.summaryFailed")
				if event.Err != nil {
					message = s.cfg.Tr("compact.summaryFailedWhy", event.Err.Error())
				}
				s.post(func() { s.applyEffects(s.drv.FailCompact(message)) })
				return
			}
		}
		summary := builder.String()
		s.post(func() {
			effects, applyErr := s.drv.ApplyCompact(summary)
			if applyErr != nil {
				s.applyEffects(s.drv.FailCompact(applyErr.Error()))
				return
			}
			s.applyEffects(effects)
		})
	}()
}

// runTools 在同一个回合的 ctx 下**串行**执行工具，结果投递回主线程。
func (s *Session) runTools(effect driver.RunTools) {
	ctx := s.turnContext()
	mid := effect.MessageID
	calls := effect.Calls
	toolCtx := s.toolCtx
	runtime := s.cfg.Runtime

	go func() {
		for _, call := range calls {
			outcomes := runtime.RunTools(ctx, toolCtx, []tools.Call{call})
			outcome := tools.ToolOutcome{ID: call.ID, Name: call.Name, Output: s.cfg.Tr("tools.emptyOutput")}
			if len(outcomes) > 0 {
				outcome = outcomes[0]
			}
			s.persistToolImages(&outcome)
			// 工具是在本 goroutine 里跑的，但清单属于会话状态：必须回主线程读
			// （ToolContext 由它自己加锁保护，读一份快照即可）。
			todos := toolCtx.Todos()
			done := make(chan struct{})
			s.post(func() {
				defer close(done)
				// 只有 todo_write 成功时才同步：失败时清单没变，也就没必要写事件。
				if call.Name == "todo_write" && !outcome.IsError && !outcome.IsPending() {
					s.applyEffects(s.drv.SyncTodos(toConversationTodos(todos)))
				}
				if effects, err := s.drv.ToolResult(mid, outcome); err == nil {
					s.applyEffects(effects)
				}
				// display_image 这类"只给用户看"的内容：在**主线程**插一条独立消息
				// （driver 不线程安全，不能在上面的后台 goroutine 里做）。
				if len(outcome.DisplayParts) > 0 {
					s.addDisplayMessage(outcome.DisplayParts)
					// AddDisplayMessage 把事件攒在 driver 里，要主动取出写日志，
					// 否则重放后这张图就没了。
					s.applyEffects(s.drv.TakeRecords())
				}
			})
			<-done
			if ctx.Err() != nil {
				return
			}
		}
		s.post(func() { s.applyEffects(s.drv.ToolBatchDone(mid)) })
	}()
}

// toConversationTodos 把工具层的清单转成会话层的形态。
//
// 两份类型刻意分开（见 conversation.TodoItem 的注释）：这里是唯一的转换点，
// 别把工具结构直接写进日志格式，否则以后改工具参数会连带改日志格式。
func toConversationTodos(todos []tools.TodoItem) []conversation.TodoItem {
	if len(todos) == 0 {
		return nil
	}
	out := make([]conversation.TodoItem, 0, len(todos))
	for _, todo := range todos {
		out = append(out, conversation.TodoItem{
			Content: todo.Content,
			Status:  string(todo.Status),
		})
	}
	return out
}

// persistToolImages 把工具返回的图片写进 blob 存储，并换成 `blob:sha256:` 引用。
//
// 对 DisplayOnly 的图片（display_image 用）额外生成一条**独立消息**：
// Android 只渲染"消息里的 image part"，工具结果里的图片它不显示；
// 而 mobile 不能随便改版，所以靠多插一条消息让它能看见。
// 那条消息带 display_only 标记，不会进模型上下文。
func (s *Session) persistToolImages(outcome *tools.ToolOutcome) {
	if len(outcome.RawImages) == 0 {
		return
	}
	blobs := s.cfg.Blobs
	refs := make([]tools.ToolImageRef, 0, len(outcome.RawImages))
	var displayParts []message.Part
	for _, image := range outcome.RawImages {
		if blobs == nil {
			outcome.Output += "\n（图片无法保存：没有 blob 存储）"
			continue
		}
		hash, err := blobs.Put(image.Data)
		if err != nil {
			outcome.Output += "\n（图片无法保存：" + err.Error() + "）"
			continue
		}
		refs = append(refs, tools.ToolImageRef{
			Ref: "blob:sha256:" + hash, MediaType: image.MediaType,
			Width: image.Width, Height: image.Height,
			Caption: image.Caption, DisplayOnly: image.DisplayOnly,
		})
		if image.DisplayOnly {
			displayParts = append(displayParts, displayImagePart(hash, image))
		}
	}
	outcome.Images = refs
	outcome.RawImages = nil
	// **不在这里插消息**：本函数跑在工具的后台 goroutine 里，而 driver 不是
	// 线程安全的。把待插入的 part 挂回 outcome，由调用方在主线程处理。
	outcome.DisplayParts = displayParts
}

// addDisplayMessage 插入一条"只展示、不进上下文"的助手消息。
//
// 失败不报错：图片没显示出来不该影响这一轮工具结果（工具本身是成功的）。
func (s *Session) addDisplayMessage(parts []message.Part) {
	id, err := conversation.NewMessageID()
	if err != nil {
		return
	}
	msg := message.Message{
		MessageID: id,
		Role:      message.RoleAgent,
		Parts:     parts,
		Metadata:  message.Metadata{message.MetadataDisplayOnly: true},
	}
	_, _ = s.drv.AddDisplayMessage(msg)
}

// displayImagePart 把一张图片变成消息里的 image part（URLPart + blob 引用）。
//
// 用 URLPart 而不是 RawPart：RawPart 会把字节内联进消息，日志里多拷一份
// （几十 MB）；blob 引用只有几十字节，界面本来也是按 hash 去取图。
func displayImagePart(hash string, image tools.ImageOutput) message.Part {
	return message.Part{
		MediaType: image.MediaType,
		Content:   message.URLPart{URL: "blob:sha256:" + hash},
		// caption 走 Filename：界面按它显示图片上方的说明
		Filename: image.Caption,
	}
}

// post 把函数投递到执行端主线程。
func (s *Session) post(fn func()) {
	if s.cfg.Post != nil {
		s.cfg.Post(fn)
		return
	}
	fn()
}

// summaryCancel 是当前摘要请求的取消函数（停止/压缩取消时用）。
func (s *Session) cancelSummary() {
	if s.summaryCancel != nil {
		s.summaryCancel()
		s.summaryCancel = nil
	}
}

// loadOlder 把归档页转成界面用的条目（只用于显示，不参与请求）。
// LoadOlderForTest 用指定游标翻一页（测试用：与展示端走同一条逻辑）。
func (s *Session) LoadOlderForTest(cursor string) (OlderPage, error) {
	return s.loadOlder(cursor)
}

func (s *Session) loadOlder(cursor string) (OlderPage, error) {
	if s.journal == nil {
		return OlderPage{}, nil
	}
	// 空游标 = 从最近一份归档开始。
	//
	// **游标完全由调用方持有**：以前空游标会沿用会话自己记的游标，
	// 而那个游标是被所有展示端共享的 —— 桌面翻过一页后，手机第一次请求
	// （空游标）就会从中间某份归档开始，最近那份被跳过。
	var from *store.ArchiveCursor
	if cursor != "" {
		from = &store.ArchiveCursor{Name: cursor}
	}
	page, err := s.journal.LoadOlder(from)
	if err != nil {
		return OlderPage{}, err
	}
	// D2：归档是"压缩前的整份日志"，其中被保留下来的那部分已经在当前会话里。
	// 光靠 kept_from_seq 不够（它只能算最后一个事件的序号），按消息 id 再挡一次，
	// 否则界面上会出现两条一模一样的消息。
	keptIDs := map[string]bool{}
	if s.drv != nil {
		for _, item := range s.drv.Conversation().Entries() {
			if item.Message == nil || item.Message.Message == nil {
				continue
			}
			keptIDs[item.Message.Message.MessageID] = true
		}
	}
	var entries []OlderEntry
	for _, event := range page.Events {
		// 截断：只保留 kept_from_seq 之前的消息，避免与当前会话内容重复
		if s.keptFromSeq > 0 && event.Seq >= s.keptFromSeq {
			continue
		}
		switch event.Type {
		case store.TypeMessageInput, store.TypeMessageOutput:
			// 新模型下一条消息只写一条事件（不再有 appended + finalized 两次），
			// 所以这里不需要去重："input 是用户消息，output 是 LLM 输出"。
			var data driver.MessageEventData
			if err := event.Decode(&data); err != nil {
				continue
			}
			status := data.Status
			if status.IsStreaming() || status == conversation.StatusPending {
				status = conversation.StatusCancelled
			}
			if keptIDs[data.Message.MessageID] {
				continue
			}
			entries = append(entries, OlderEntry{
				Message: data.Message, Status: status, Reason: data.Reason, CreatedAt: event.At.Unix(),
			})
		}
	}
	next := ""
	if page.Next != nil {
		next = page.Next.Name
	}
	more := page.Next != nil
	for _, warning := range page.Warnings {
		s.host.warn(s.ID, s.cfg.Tr("archive.corrupt", warning))
	}
	return OlderPage{Entries: entries, More: more, Next: next}, nil
}

// backfill 从磁盘日志里取 **seq < before** 的那一段历史事件。
//
// 为什么需要：首次 Attach 只下发最近 attachTailEvents 条（长会话首屏才不会卡）。
// 展示端在本地镜像里翻页翻到最早时，磁盘上其实还有更多 —— 没有这个命令，
// 界面就只能谎报"已经是最早的内容了"。
//
// 返回原始 store.Event：展示端用 driver.Mirror.ApplyPersisted 注入镜像，
// 与 attach 下发的历史走同一条路径，不用再解析一遍。
func (s *Session) backfill(before uint64, limit int) (BackfillData, error) {
	if s.journal == nil {
		return BackfillData{}, nil // 内存后端没有历史可补
	}
	if limit <= 0 || limit > 2000 {
		limit = 300
	}
	loaded, err := s.journal.Load()
	if err != nil {
		return BackfillData{}, err
	}

	// 只保留能重建时间线的**内容事件**（状态事件对历史渲染没用，
	// 传过去只会让展示端多做无用功）。
	var picked []store.Event
	var oldest uint64
	for _, event := range loaded.Events {
		// 压缩快照也要带上：它里面是压缩时**保留下来的那段对话**（kept），
		// 这些消息只存在快照里，不会再以内容事件出现；而归档那边又按 id
		// 把它们排除掉了（避免重复）。不带快照，展示端翻到这里就永远看不到
		// 这一段 —— 真机表现是"还没到会话开头就说已经是最早的内容了"。
		if !isContentEvent(event.Type) && event.Type != store.TypeCompactSnapshot {
			continue
		}
		if oldest == 0 {
			oldest = event.Seq
		}
		if before > 0 && event.Seq >= before {
			// 进入展示端已有区间：后面的都不要了
			break
		}
		picked = append(picked, event)
	}

	// hasMore 表示"**再往前还有内容**"，不是"碰到了游标"。
	// 截断了（limit 放不下）就说明还有；否则看最早的候选是不是就从
	// 日志开头开始 —— 不是就说明前面还有更早的。
	hasMore := false
	if len(picked) > limit {
		picked = picked[len(picked)-limit:]
		hasMore = true
	}
	if !hasMore && len(picked) > 0 && oldest != 0 && picked[0].Seq > oldest {
		hasMore = true
	}

	return BackfillData{Events: picked, HasMore: hasMore, OldestSeq: oldest}, nil
}

// isContentEvent 报告该事件是否属于"重建时间线需要"的内容事件。
func isContentEvent(kind string) bool {
	switch kind {
	case store.TypeMessageInput, store.TypeMessageOutput, store.TypeToolResult:
		return true
	}
	return false
}

// ---- 测试辅助（生产代码不该调用）----

// BackfillForTest 直接调 backfill（与展示端走同一条逻辑）。
func BackfillForTest(s *Session, before uint64, limit int) (BackfillData, error) {
	return s.backfill(before, limit)
}

// EventsForTest 返回会话日志里的全部事件（测试用来算游标）。
func EventsForTest(s *Session) []store.Event {
	if s.journal == nil {
		return nil
	}
	loaded, err := s.journal.Load()
	if err != nil {
		return nil
	}
	return loaded.Events
}

// AppendUserMessageForTest 往会话里追加一条用户消息并落盘（测试用）。
//
// 会先确保会话已持久化（ensurePersisted），否则 journal 是 nil、事件写不进磁盘，
// backfill 自然读不到东西。
func AppendUserMessageForTest(s *Session, text string) error {
	s.ensurePersisted(text)
	msg := message.Message{
		MessageID: s.cfg.NewID(),
		Role:      message.RoleUser,
		Parts:     []message.Part{message.NewTextPart(text)},
	}
	if _, err := s.Conversation().AddMessage(msg, conversation.StatusComplete); err != nil {
		return err
	}
	event, err := store.NewEvent(store.TypeMessageInput, driver.MessageEventData{
		Round: 1, Turn: 1, Role: string(message.RoleUser),
		Message: msg, Status: conversation.StatusComplete,
	})
	if err != nil {
		return err
	}
	if s.journal != nil {
		return s.journal.Append(event)
	}
	return nil
}

// isDeltaEvent 判断 provider 事件是否属于"纯增量"（要转发给展示端的那几种）。
func isDeltaEvent(kind providers.EventKind) bool {
	switch kind {
	case providers.EventText, providers.EventReasoning,
		providers.EventToolStart, providers.EventToolArgs, providers.EventToolEnd:
		return true
	}
	return false
}
