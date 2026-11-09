package host

import (
	"context"
	"errors"
	"log"
	"strings"
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
	// keptFromSeq 是快照的归档截断点（更早的归档在界面上只显示、不参与请求）。
	keptFromSeq uint64
	// archiveCursor 是"加载更早历史"的游标。
	archiveCursor *store.ArchiveCursor

	// summaryCancel 是当前摘要请求的取消函数（停止/解压时用）。
	summaryCancel context.CancelFunc

	providerID string
	model      string
	effort     string
	// generation 是持久状态的"代"（压缩后 +1）：续传游标要带它。
	generation uint64

	cfg  *Config
	host *Local
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

func (s *Session) Busy() driver.Busy {
	if s.view != nil {
		return driver.Busy(s.view.Busy)
	}
	return s.drv.Busy()
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
		return driver.Usage{InputTokens: s.view.Usage.Input, OutputTokens: s.view.Usage.Output}
	}
	return s.drv.Usage()
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

func (s *Session) submitAsks(payload AnswerPayload) CmdResult {
	// 其他端已经回答/跳过：这里已经没有待回答的提问，别把它当成"提交成功"
	if s.drv.Busy() != driver.BusyAwaitingAnswer {
		return CmdResult{Notice: s.cfg.Tr("ask.answeredElsewhere")}
	}
	// 来自展示端的答案：先把选择应用回执行端自己的提问组，再统一提交
	for _, answer := range payload.Answers {
		for _, group := range s.drv.AskGroups() {
			if group.CallID != answer.CallID {
				continue
			}
			for index, question := range group.Questions {
				for _, option := range answer.Selected[question.ID] {
					group.Toggle(index, option)
				}
				if text := answer.Custom[question.ID]; text != "" {
					group.SetCustom(index, text)
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
			s.post(func() { s.applyEffects(s.drv.FailStreamWith(mid, reason)) })
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

// finishInterrupted 处理"流没有结束标记就断了"：先按失败收尾；
// 如果是**本轮的第一次**中断，自动发起一次"继续"，让会话自己往下走，
// 而不是把用户晾在失败状态里（用户反馈：理论上应该继续）。
func (s *Session) finishInterrupted(messageID string, received int, lastKind string) {
	// 正常结束（收到 Done）之后通道也会关闭：那时 driver 已经不是本轮流式状态，
	// 这里什么都不该做——只有"还在流式却关闭了"才算中断。
	if s.drv.Busy() != driver.BusyStreaming || !s.drv.OwnsStream(messageID) {
		return
	}
	effects := s.drv.StreamClosed(messageID)
	s.applyEffects(effects)
	// 最多自动继续 2 次（网关偶发断流通常重试一次就好；连续两次都断说明不是抖动）
	if s.drv.Busy() != driver.BusyIdle || s.autoRetry >= 2 {
		log.Printf("会话 %s：流中断且自动继续已达上限（%d 次），保持失败状态", s.ID, s.autoRetry)
		return
	}
	s.autoRetry++
	log.Printf("会话 %s：流意外结束（收到 %d 个事件，最后是 %s），第 %d 次自动继续",
		s.ID, received, lastKind, s.autoRetry)
	_, _ = s.Conversation().AddNotice(conversation.NoticeWarn, s.cfg.Tr("session.autoRetry"))
	delay := time.Duration(s.autoRetry) * 800 * time.Millisecond
	time.AfterFunc(delay, func() {
		s.post(func() {
			if s.drv.Busy() != driver.BusyIdle {
				return
			}
			retry, err := s.drv.Send(s.cfg.Tr("session.autoRetryText"))
			if err != nil {
				return
			}
			s.applyEffects(retry)
		})
	})
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
func (s *Session) persistToolImages(outcome *tools.ToolOutcome) {
	if len(outcome.RawImages) == 0 {
		return
	}
	blobs := s.cfg.Blobs
	refs := make([]tools.ToolImageRef, 0, len(outcome.RawImages))
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
		})
	}
	outcome.Images = refs
	outcome.RawImages = nil
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
	s.archiveCursor = nil
	return s.loadOlder(cursor)
}

func (s *Session) loadOlder(cursor string) (OlderPage, error) {
	if s.journal == nil {
		return OlderPage{}, nil
	}
	// 空游标 = 从最近一份归档开始（单机沿用会话自己的游标；展示端每次带上自己的）
	from := s.archiveCursor
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
		case store.TypeMessageAppended:
			var data driver.MessageAppendedData
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
		case store.TypeMessageFinalize:
			var data driver.MessageFinalizedData
			if err := event.Decode(&data); err != nil {
				continue
			}
			if keptIDs[data.Message.MessageID] {
				continue
			}
			entries = append(entries, OlderEntry{
				Message: data.Message, Status: data.Status, Reason: data.Reason, CreatedAt: event.At.Unix(),
			})
		}
	}
	s.archiveCursor = page.Next
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

// isDeltaEvent 判断 provider 事件是否属于"纯增量"（要转发给展示端的那几种）。
func isDeltaEvent(kind providers.EventKind) bool {
	switch kind {
	case providers.EventText, providers.EventReasoning,
		providers.EventToolStart, providers.EventToolArgs, providers.EventToolEnd:
		return true
	}
	return false
}
