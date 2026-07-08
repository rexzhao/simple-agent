package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/rexzhao/simple-agent/internal/cli/turnview"
	"github.com/rexzhao/simple-agent/internal/execution"
)

type tuiCreateSessionFunc func() (execution.SessionDetail, error)
type tuiSendFunc func(context.Context, string, string, func(execution.SessionStreamEvent)) (execution.SessionMessageResult, error)
type tuiCompactFunc func(context.Context, string) (execution.SessionCompactResult, error)
type tuiFinalOutputFunc func(string, string) (string, error)

type tuiModel struct {
	ctx            context.Context
	displayCommand string
	view           *turnview.State

	sessionID     string
	createSession tuiCreateSessionFunc
	send          tuiSendFunc
	compact       tuiCompactFunc
	finalOutput   tuiFinalOutputFunc

	mailbox        *mailboxQueue
	waitingMailbox bool
	deferredTask   *mailboxTask

	input          string
	width          int
	height         int
	active         bool
	creating       bool
	eventsOpen     bool
	sendDoneSeen   bool
	activeCancel   context.CancelFunc
	activeEventCh  <-chan execution.SessionStreamEvent
	activeDoneCh   <-chan attachSendResult
	activeTask     *mailboxTask
	pendingPrompt  string
	pendingTask    *mailboxTask
	pendingSendErr error
}

type tuiEventMsg struct {
	event execution.SessionStreamEvent
	ok    bool
}

type tuiSendDoneMsg struct {
	result execution.SessionMessageResult
	err    error
	task   *mailboxTask
}

type tuiMailboxMsg struct {
	task *mailboxTask
	err  error
}

type tuiSessionCreatedMsg struct {
	detail execution.SessionDetail
	err    error
	prompt string
	task   *mailboxTask
}

type tuiCompactDoneMsg struct {
	err error
}

func runPendingAttachTUI(ctx context.Context, service *execution.Service, configPath, homePath, creationCWD string, stdin io.Reader, stdout, stderr io.Writer, program string, mailbox *mailboxQueue) error {
	displayCommand := displayProgramName(program)
	if !shouldRunTUI(stdin, stdout) {
		if _, err := fmt.Fprintf(stderr, "%s: --tui requires an interactive terminal; falling back to plain output\n", displayCommand); err != nil {
			return err
		}
		return runPendingAttachREPL(ctx, service, configPath, homePath, creationCWD, stdin, stdout, stderr, program, mailbox)
	}

	model := newTUIModel(ctx, service, "", displayCommand, mailbox)
	model.createSession = func() (execution.SessionDetail, error) {
		return createExecutionSessionForCWD(configPath, homePath, creationCWD, program)
	}
	return runTUIProgram(stdin, stdout, model)
}

func runResumeAttachTUI(ctx context.Context, service *execution.Service, detail execution.SessionDetail, snapshot execution.SessionItemsPage, stdin io.Reader, stdout, stderr io.Writer, displayCommand string, mailbox *mailboxQueue) error {
	if !shouldRunTUI(stdin, stdout) {
		if _, err := fmt.Fprintf(stderr, "%s: --tui requires an interactive terminal; falling back to plain output\n", displayCommand); err != nil {
			return err
		}
		if err := writeAttachSnapshot(stdout, snapshot); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stderr, "%s: attached to session %s\n", displayCommand, detail.ID); err != nil {
			return err
		}
		return runAttachREPL(ctx, service, detail.ID, stdin, stdout, stderr, displayCommand, mailbox)
	}

	model := newTUIModel(ctx, service, detail.ID, displayCommand, mailbox)
	model.view.SetSession(detail.ID, detail.Provider, detail.ModelProfile, detail.ModelID)
	seedTurnViewSnapshot(model.view, snapshot)
	return runTUIProgram(stdin, stdout, model)
}

func newTUIModel(ctx context.Context, service *execution.Service, sessionID, displayCommand string, mailbox *mailboxQueue) *tuiModel {
	model := &tuiModel{
		ctx:            ctx,
		displayCommand: displayCommand,
		view:           turnview.New(),
		sessionID:      strings.TrimSpace(sessionID),
		mailbox:        mailbox,
	}
	if model.ctx == nil {
		model.ctx = context.Background()
	}
	if service != nil {
		model.send = service.SendSessionMessageWithEvents
		model.compact = service.CompactSession
		model.finalOutput = service.GetSessionTurnFinalAssistantOutput
	}
	return model
}

func runTUIProgram(stdin io.Reader, stdout io.Writer, model *tuiModel) error {
	program := tea.NewProgram(model, tea.WithInput(stdin), tea.WithOutput(stdout), tea.WithAltScreen())
	_, err := program.Run()
	return err
}

func (m *tuiModel) Init() tea.Cmd {
	return m.waitMailboxCmd()
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		return m, m.handleKey(msg)
	case tuiSessionCreatedMsg:
		m.creating = false
		if msg.err != nil {
			if msg.task != nil && m.mailbox != nil {
				m.mailbox.failTask(msg.task, msg.err)
				m.view.AddMailboxEnd(msg.task.ID, tuiMailboxTerminalStatus(m.mailbox, msg.task, mailboxTaskFailed))
			}
			m.view.AddSystem("session create failed", msg.err.Error(), "failed")
			m.active = false
			return m, m.waitMailboxCmd()
		}
		m.sessionID = strings.TrimSpace(msg.detail.ID)
		m.view.SetSession(msg.detail.ID, msg.detail.Provider, msg.detail.ModelProfile, msg.detail.ModelID)
		if m.sessionID == "" {
			m.view.AddSystem("session create failed", "create session: response missing session id", "failed")
			return m, m.waitMailboxCmd()
		}
		return m, m.startTurn(msg.prompt, msg.task)
	case tuiCompactDoneMsg:
		m.active = false
		if msg.err != nil {
			m.view.AddSystem("compact failed", msg.err.Error(), "failed")
		} else {
			m.view.AddSystem("compacted session context", "", "completed")
		}
		return m, m.waitMailboxCmd()
	case tuiMailboxMsg:
		m.waitingMailbox = false
		if msg.err != nil {
			if m.ctx.Err() != nil {
				return m, tea.Quit
			}
			m.view.AddSystem("mailbox failed", msg.err.Error(), "failed")
			return m, m.waitMailboxCmd()
		}
		if msg.task == nil {
			return m, m.waitMailboxCmd()
		}
		if m.active || m.creating {
			m.deferredTask = msg.task
			return m, nil
		}
		return m, m.submitPrompt(msg.task.Prompt, msg.task)
	case tuiEventMsg:
		if !msg.ok {
			m.eventsOpen = false
			return m, m.finishTurnIfReady()
		}
		m.view.Apply(msg.event)
		return m, waitTUIEvent(m.activeEventCh)
	case tuiSendDoneMsg:
		m.sendDoneSeen = true
		m.pendingSendErr = msg.err
		if msg.task != nil {
			m.finishMailboxTask(msg)
		}
		return m, m.finishTurnIfReady()
	}
	return m, nil
}

func (m *tuiModel) View() string {
	snapshot := m.view.Snapshot()
	var out strings.Builder
	out.WriteString(m.renderStatus(snapshot.Status))
	out.WriteString("\n\n")

	blocks := snapshot.Blocks
	available := m.height - 4
	if available < 4 {
		available = 4
	}
	if len(blocks) > available {
		blocks = blocks[len(blocks)-available:]
	}
	for i, block := range blocks {
		if i > 0 {
			out.WriteString("\n")
		}
		out.WriteString(m.renderBlock(block))
	}
	out.WriteString("\n\n")
	prompt := "> "
	if m.active || m.creating {
		prompt = "... "
	}
	out.WriteString(prompt)
	out.WriteString(m.input)
	return out.String()
}

func (m *tuiModel) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyCtrlC:
		if m.active || m.creating {
			if m.activeCancel != nil {
				m.activeCancel()
				m.view.AddSystem("turn cancel requested", "", "cancelled")
			}
			return nil
		}
		return tea.Quit
	case tea.KeyEnter:
		if m.active || m.creating {
			return nil
		}
		prompt := strings.TrimSpace(m.input)
		m.input = ""
		if prompt == "" {
			return m.waitMailboxCmd()
		}
		return m.submitPrompt(prompt, nil)
	case tea.KeyBackspace, tea.KeyCtrlH:
		if m.active || m.creating || m.input == "" {
			return nil
		}
		runes := []rune(m.input)
		m.input = string(runes[:len(runes)-1])
		return nil
	default:
		if m.active || m.creating {
			return nil
		}
		if len(msg.Runes) > 0 {
			m.input += string(msg.Runes)
		}
	}
	return nil
}

func (m *tuiModel) submitPrompt(prompt string, task *mailboxTask) tea.Cmd {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return m.waitMailboxCmd()
	}
	if task == nil {
		switch prompt {
		case "/exit", "/quit":
			return tea.Quit
		case "/compact":
			return m.startCompact()
		}
	}
	if m.sessionID == "" {
		if m.createSession == nil {
			m.view.AddSystem("session create failed", "session creator is not configured", "failed")
			return m.waitMailboxCmd()
		}
		m.creating = true
		m.active = true
		m.pendingPrompt = prompt
		m.pendingTask = task
		return createTUISessionCmd(m.createSession, prompt, task)
	}
	return m.startTurn(prompt, task)
}

func (m *tuiModel) startCompact() tea.Cmd {
	if m.sessionID == "" {
		m.view.AddSystem("compact failed", "compact requires a session; send a message first to create one", "failed")
		return m.waitMailboxCmd()
	}
	if m.compact == nil {
		m.view.AddSystem("compact failed", "compact planner is not configured", "failed")
		return m.waitMailboxCmd()
	}
	m.active = true
	return func() tea.Msg {
		_, err := m.compact(m.ctx, m.sessionID)
		return tuiCompactDoneMsg{err: err}
	}
}

func (m *tuiModel) startTurn(prompt string, task *mailboxTask) tea.Cmd {
	if m.send == nil {
		m.view.AddSystem("send failed", "session sender is not configured", "failed")
		return m.waitMailboxCmd()
	}
	sendCtx := m.ctx
	var cancel context.CancelFunc
	if task != nil {
		if m.mailbox == nil {
			m.view.AddSystem("mailbox failed", "mailbox queue is not configured", "failed")
			return m.waitMailboxCmd()
		}
		sendCtx, cancel = context.WithCancel(m.ctx)
		if !m.mailbox.startTask(task, cancel) {
			cancel()
			return m.waitMailboxCmd()
		}
		m.view.AddMailboxStart(task.ID)
		m.view.AddInput("mailbox", prompt)
	} else {
		sendCtx, cancel = context.WithCancel(m.ctx)
		m.view.AddInput("user", prompt)
	}

	eventCh := make(chan execution.SessionStreamEvent, 64)
	doneCh := make(chan attachSendResult, 1)
	m.active = true
	m.creating = false
	m.eventsOpen = true
	m.sendDoneSeen = false
	m.pendingSendErr = nil
	m.activeCancel = cancel
	m.activeEventCh = eventCh
	m.activeDoneCh = doneCh
	m.activeTask = task
	m.pendingPrompt = ""
	m.pendingTask = nil

	go func() {
		result, err := m.send(sendCtx, m.sessionID, prompt, func(event execution.SessionStreamEvent) {
			select {
			case eventCh <- event:
			case <-sendCtx.Done():
			}
		})
		close(eventCh)
		doneCh <- attachSendResult{result: result, err: err, mailboxTask: task}
		cancel()
	}()

	return tea.Batch(waitTUIEvent(eventCh), waitTUISendDone(doneCh))
}

func (m *tuiModel) finishMailboxTask(msg tuiSendDoneMsg) {
	status := mailboxTaskCompleted
	if msg.err != nil {
		m.mailbox.failTask(msg.task, msg.err)
		status = mailboxTaskFailed
	} else if m.finalOutput == nil {
		err := fmt.Errorf("mailbox result reader is not configured")
		m.mailbox.failTask(msg.task, err)
		status = mailboxTaskFailed
	} else {
		result, err := m.finalOutput(m.sessionID, msg.result.TurnID)
		if err != nil {
			m.mailbox.failTask(msg.task, err)
			status = mailboxTaskFailed
		} else {
			m.mailbox.completeTask(msg.task, result)
		}
	}
	m.view.AddMailboxEnd(msg.task.ID, tuiMailboxTerminalStatus(m.mailbox, msg.task, status))
}

func (m *tuiModel) finishTurnIfReady() tea.Cmd {
	if !m.active || m.eventsOpen || !m.sendDoneSeen {
		return nil
	}
	if m.pendingSendErr != nil && m.view.Status.TurnStatus != "failed" {
		m.view.AddSystem("send failed", m.pendingSendErr.Error(), "failed")
		m.view.Status.TurnStatus = "failed"
	}
	m.active = false
	m.activeCancel = nil
	m.activeEventCh = nil
	m.activeDoneCh = nil
	m.activeTask = nil
	m.pendingSendErr = nil
	if m.deferredTask != nil {
		task := m.deferredTask
		m.deferredTask = nil
		return m.submitPrompt(task.Prompt, task)
	}
	return m.waitMailboxCmd()
}

func (m *tuiModel) waitMailboxCmd() tea.Cmd {
	if m.mailbox == nil || m.active || m.creating || m.waitingMailbox || m.deferredTask != nil {
		return nil
	}
	m.waitingMailbox = true
	return func() tea.Msg {
		task, err := m.mailbox.dequeue(m.ctx)
		return tuiMailboxMsg{task: task, err: err}
	}
}

func (m *tuiModel) renderStatus(status turnview.StatusBarState) string {
	parts := []string{}
	if status.Provider != "" || status.ModelProfile != "" || status.ModelID != "" {
		model := strings.TrimSpace(status.Provider + "/" + status.ModelProfile)
		if status.ModelID != "" {
			model += " (" + status.ModelID + ")"
		}
		parts = append(parts, model)
	}
	if status.SessionID != "" {
		parts = append(parts, "session "+status.SessionID)
	}
	if status.TurnID != "" {
		parts = append(parts, "turn "+status.TurnID)
	}
	if status.TurnStatus != "" {
		parts = append(parts, status.TurnStatus)
	}
	if status.TotalTokens > 0 {
		parts = append(parts, fmt.Sprintf("tokens %d/%d/%d", status.InputTokens, status.OutputTokens, status.TotalTokens))
	}
	if status.ToolCount > 0 {
		parts = append(parts, fmt.Sprintf("tools %d", status.ToolCount))
	}
	if status.Mailbox != "" {
		parts = append(parts, "mailbox "+status.Mailbox)
	}
	if len(parts) == 0 {
		return "sai"
	}
	return strings.Join(parts, " | ")
}

func (m *tuiModel) renderBlock(block turnview.Block) string {
	title := string(block.Kind)
	if block.Title != "" {
		title = block.Title
	}
	if block.Status != "" {
		title += " [" + block.Status + "]"
	}
	if block.Text == "" {
		return title
	}
	return title + "\n" + block.Text
}

func createTUISessionCmd(create tuiCreateSessionFunc, prompt string, task *mailboxTask) tea.Cmd {
	return func() tea.Msg {
		detail, err := create()
		return tuiSessionCreatedMsg{detail: detail, err: err, prompt: prompt, task: task}
	}
}

func waitTUIEvent(ch <-chan execution.SessionStreamEvent) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-ch
		return tuiEventMsg{event: event, ok: ok}
	}
}

func waitTUISendDone(ch <-chan attachSendResult) tea.Cmd {
	return func() tea.Msg {
		result := <-ch
		return tuiSendDoneMsg{result: result.result, err: result.err, task: result.mailboxTask}
	}
}

func shouldRunTUI(stdin io.Reader, stdout io.Writer) bool {
	return isCharDevice(stdin) && isCharDevice(stdout)
}

func isCharDevice(value any) bool {
	for {
		unwrapper, ok := value.(interface{ UnwrapWriter() io.Writer })
		if !ok {
			break
		}
		unwrapped := unwrapper.UnwrapWriter()
		if unwrapped == nil || unwrapped == value {
			break
		}
		value = unwrapped
	}
	file, ok := value.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func seedTurnViewSnapshot(state *turnview.State, snapshot execution.SessionItemsPage) {
	for _, item := range snapshot.Items {
		if item.Message == nil || item.Message.Content == nil {
			continue
		}
		text := item.Message.Content.Inline
		if text == "" {
			text = item.Message.Content.Preview
		}
		if text == "" {
			continue
		}
		state.AddMessage(item.Message.Role, text)
	}
}

func tuiMailboxTerminalStatus(mailbox *mailboxQueue, task *mailboxTask, fallback string) string {
	status := strings.TrimSpace(fallback)
	if task != nil && mailbox != nil {
		if snapshot, ok := mailbox.get(task.ID); ok && strings.TrimSpace(snapshot.Status) != "" {
			status = snapshot.Status
		}
	}
	if status == "" {
		status = "finished"
	}
	return status
}
