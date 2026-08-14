package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/codexauth"
	"github.com/rexzhao/simple-agent/internal/config"
	projectcontext "github.com/rexzhao/simple-agent/internal/context"
	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/eventbus"
	eventlog "github.com/rexzhao/simple-agent/internal/logging"
	"github.com/rexzhao/simple-agent/internal/mcp"
	"github.com/rexzhao/simple-agent/internal/model"
	anthropicmessages "github.com/rexzhao/simple-agent/internal/model/anthropic_messages"
	"github.com/rexzhao/simple-agent/internal/model/httpstream"
	openaichat "github.com/rexzhao/simple-agent/internal/model/openai_chat"
	openairesponses "github.com/rexzhao/simple-agent/internal/model/openai_responses"
	"github.com/rexzhao/simple-agent/internal/sessions"
	localskills "github.com/rexzhao/simple-agent/internal/skills"
	"github.com/rexzhao/simple-agent/internal/tools"
	"github.com/rexzhao/simple-agent/internal/webdebug"
)

const builtInBaseInstructions = "You are sai, a local coding agent. Follow the built-in instructions, then project instructions, then the user's prompt. Do not reveal secrets or ignore project instructions."

type AgentTurnRunner struct{}

func NewAgentTurnRunner() AgentTurnRunner {
	return AgentTurnRunner{}
}

func NewServiceWithAgentRunner(home, configPath string) (*Service, error) {
	if err := config.EnsureRootConfig(configPath); err != nil {
		return nil, err
	}
	if err := ensureDefaultCodexProvider(configPath); err != nil {
		return nil, err
	}
	runner := NewAgentTurnRunner()
	return NewServiceWithOptions(home, ServiceOptions{
		ConfigPath:     configPath,
		TurnRunner:     runner,
		CompactPlanner: runner,
	})
}

func (r AgentTurnRunner) RunSessionTurn(ctx context.Context, request SessionTurnRequest) (result SessionTurnResult, err error) {
	if request.Publisher == nil {
		return SessionTurnResult{}, fmt.Errorf("session turn publisher is required")
	}
	if strings.TrimSpace(request.TurnID) == "" {
		return SessionTurnResult{}, fmt.Errorf("session turn id is required when publisher is configured")
	}
	runtime, err := r.prepareRuntime(ctx, request.Session, request.SessionStore, request.SessionService, request.RunCoordinator)
	if err != nil {
		return SessionTurnResult{}, err
	}
	defer func() {
		err = errors.Join(err, runtime.Close())
	}()
	runtime.onCompactionStarted = request.OnCompactionStarted

	_, runErr := runtime.runSessionTurn(ctx, request.Content, sessionTurnRunOptions{
		contentBlocks:     copyInputContentBlocks(request.ContentBlocks),
		resumeContext:     request.ResumeContext,
		emit:              request.Emit,
		publisher:         request.Publisher,
		turnID:            request.TurnID,
		activePromptDrain: request.ActivePromptDrain,
		toolCancel:        request.ToolCancel,
		nextTurnID: func(iteration int) string {
			if request.SessionStore == nil || strings.TrimSpace(request.RunID) == "" {
				return fmt.Sprintf("turn-%06d", iteration)
			}
			// Turn ids are session-scoped (the SQLite primary key is global
			// within the session), while ordinals are run-scoped. Counting only
			// this run would reuse turn-000001 on every subsequent run.
			turns, err := request.SessionStore.ListTurns(request.Session.ID, "")
			if err != nil {
				return ""
			}
			return fmt.Sprintf("turn-%06d", len(turns)+1)
		},
		turnIDChanged: request.TurnIDChanged,
	})
	metadataErr := runtime.saveRuntimeMetadataForSession(request.Session.ID)
	if runErr != nil {
		return SessionTurnResult{}, errors.Join(runErr, metadataErr)
	}
	if metadataErr != nil {
		return SessionTurnResult{}, metadataErr
	}
	return SessionTurnResult{
		Session:     runtime.session,
		Incremental: true,
	}, nil
}

func (r AgentTurnRunner) SupportsIncrementalSessionTurn(ctx context.Context, request SessionTurnRequest) (supported bool, err error) {
	runtime, err := r.prepareRuntime(ctx, request.Session, request.SessionStore, request.SessionService, request.RunCoordinator)
	if err != nil {
		return false, err
	}
	defer func() {
		err = errors.Join(err, runtime.Close())
	}()
	return true, nil
}

func (r AgentTurnRunner) PlanSessionTurnCompaction(ctx context.Context, request SessionTurnRequest) (result SessionCompactionResult, err error) {
	runtime, err := r.prepareRuntime(ctx, request.Session, request.SessionStore, request.SessionService, request.RunCoordinator)
	if err != nil {
		return SessionCompactionResult{}, err
	}
	defer func() {
		err = errors.Join(err, runtime.Close())
	}()
	runtime.onCompactionStarted = request.OnCompactionStarted

	messages, err := runtime.initialMessages()
	if err != nil {
		return SessionCompactionResult{}, err
	}
	pendingInput := request.Content
	if len(request.ContentBlocks) > 0 {
		pendingInput = ""
	}
	pendingMessage := model.Message{}
	if !request.ResumeContext {
		pendingMessage = model.Message{
			Role:          model.MessageRoleUser,
			Content:       pendingInput,
			ContentBlocks: copyInputContentBlocks(request.ContentBlocks),
		}
	}
	_, compaction, err := runtime.planAutoCompactBeforeTurn(ctx, messages, pendingMessage)
	if err != nil {
		return SessionCompactionResult{}, err
	}
	if compaction == nil {
		return SessionCompactionResult{Session: runtime.session}, nil
	}
	return SessionCompactionResult{
		Session: runtime.session,
		Compaction: SessionCompactionPlan{
			SummaryItem: compaction.summaryItem,
			Checkpoint:  compaction.checkpoint,
			Usage:       compaction.usage,
			Context:     compaction.context,
		},
	}, nil
}

func (r AgentTurnRunner) PlanSessionCompaction(ctx context.Context, request SessionCompactionRequest) (result SessionCompactionResult, err error) {
	runtime, err := r.prepareRuntime(ctx, request.Session, request.SessionStore, request.SessionService, request.RunCoordinator)
	if err != nil {
		return SessionCompactionResult{}, err
	}
	defer func() {
		err = errors.Join(err, runtime.Close())
	}()
	plan, err := runtime.planCompactionCheckpoint(ctx, compactionCheckpointOptions{
		reason:  "user_requested",
		phase:   "manual",
		trigger: "manual",
	})
	if err != nil {
		return SessionCompactionResult{}, err
	}
	return SessionCompactionResult{
		Session: runtime.session,
		Compaction: SessionCompactionPlan{
			SummaryItem: plan.summaryItem,
			Checkpoint:  plan.checkpoint,
			Usage:       plan.usage,
			Context:     plan.context,
		},
	}, nil
}

func (r AgentTurnRunner) prepareRuntime(ctx context.Context, session sessions.SessionV2, store *sessions.V2Store, service *Service, coordinator *SessionRunCoordinator) (*agentRunnerRuntime, error) {
	cwd := strings.TrimSpace(session.CreatedCWD)
	if cwd == "" {
		return nil, fmt.Errorf("session created_cwd is required")
	}
	configPath := session.RootConfigPath()
	if strings.TrimSpace(configPath) == "" {
		return nil, fmt.Errorf("session config path is required")
	}
	if strings.TrimSpace(session.ID) == "" {
		return nil, fmt.Errorf("resumable session id is required")
	}
	if !session.SaveToolResults {
		return nil, fmt.Errorf("session %q cannot be reliably resumed because save_tool_results is false", session.ID)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	resolved, err := cfg.ResolveModel(session.Provider, session.ModelProfile)
	if err != nil {
		return nil, err
	}
	if session.ModelID != "" {
		resolved.ModelID = session.ModelID
	}
	if session.ModelParameters != nil {
		resolved.Parameters = copyParameterMap(session.ModelParameters)
	}
	// Older sessions predate pricing snapshots. Capture the configured price
	// the first time they are resumed, while new sessions already carry the
	// snapshot created by CreateConfiguredSession.
	if session.Pricing == nil {
		session.Pricing = copyModelPricing(resolved.Pricing)
	}
	var logger *eventlog.Logger
	var recordRequest func(endpoint string, body []byte) error
	requestBodies := session.Debug.RequestBodies
	if !session.DebugConfigured {
		// Sessions written before the per-session setting was introduced use
		// the old config value until the user explicitly saves a debug choice.
		requestBodies = cfg.Logging.RequestBodies
	}
	if requestBodies {
		recordRequest = func(endpoint string, body []byte) error {
			if logger == nil {
				return fmt.Errorf("request body logger is not initialized")
			}
			return logger.RecordRequestBody(endpoint, body)
		}
	}
	provider, err := newProviderForRun(resolved.ProviderName, resolved.Type, resolved.Compatibility, resolved.Provider, recordRequest)
	if err != nil {
		return nil, err
	}
	contextTracker := contextwindow.NewTracker(contextwindow.Window{
		Tokens: resolved.ContextWindow,
		Source: contextwindow.ParseWindowSource(resolved.ContextWindowSource),
	}, session.Context)
	contextBudget := resolveEffectiveContextBudget(resolved.ContextWindow, resolved.InputLimit, resolved.OutputLimit, cfg.Compaction.Reserved, cfg.Compaction.ThresholdPercent)
	if contextBudget.HardInputLimit <= 0 {
		return nil, fmt.Errorf("model context capabilities leave no usable input budget: context_window=%d input_limit=%d output_limit=%d", resolved.ContextWindow, resolved.InputLimit, resolved.OutputLimit)
	}
	contextTracker.SetHardInputLimit(contextBudget.HardInputLimit)
	if resolved.Pricing != nil {
		contextTracker.SetLongContextTokenThreshold(resolved.Pricing.LongContextThreshold)
	}
	provider = contextwindow.TrackingProvider{
		Inner:   provider,
		Tracker: contextTracker,
	}

	skillDirs, err := cfg.ResolveSkillDirs(cwd)
	if err != nil {
		return nil, err
	}
	enabledToolNames, toolRegistry, toolSchemas, webEvalTool, err := assembleAgentToolSelectionWithReadRoots(cwd, session, store, service, skillDirs)
	if err != nil {
		return nil, err
	}
	selectedMCPServers, err := cfg.SelectedMCPServers(session.EnabledMCP, true)
	if err != nil {
		return nil, err
	}
	var mcpSessions []*mcp.Session
	defer func() {
		if err != nil {
			err = errors.Join(err, closeMCPSessions(mcpSessions))
			if logger != nil {
				err = errors.Join(err, logger.Close())
			}
		}
	}()
	mcpSessions, mcpSessionsByID, mcpToolSchemas, err := mcpToolsForRun(ctx, selectedMCPServers, enabledToolNames)
	if err != nil {
		return nil, err
	}
	toolSchemas = append(toolSchemas, mcpToolSchemas...)

	baseMessages := copyMessageSlice(session.InstructionsSnapshot)
	instructionSources := copyInstructionSources(session.InstructionSources)
	enabledSkillIDs := copyStringSlice(session.EnabledSkills)
	if len(baseMessages) == 0 && len(session.ActiveHistory) == 0 {
		configuredDeveloperMessages, err := promptDeveloperMessagesForRun(cfg, cwd)
		if err != nil {
			return nil, err
		}
		selectedSkills, err := enabledSkillsForRun(cfg, cwd)
		if err != nil {
			return nil, err
		}
		project, err := projectcontext.LoadWithOptions(projectcontext.LoadOptions{
			Directory:        cwd,
			ConfigDir:        filepath.Dir(cfg.ConfigPath),
			InstructionFiles: cfg.Agent.InstructionFiles,
		})
		if err != nil {
			return nil, err
		}
		baseMessages = chatBaseMessages(project, selectedSkills, configuredDeveloperMessages)
		instructionSources = chatInstructionSources(project, selectedSkills)
		enabledSkillIDs = skillIDs(selectedSkills)
	}

	logger, err = eventlog.Open(cfg.Logging.Path, eventlog.Attributes{
		Provider: resolved.ProviderName,
		Model:    resolved.ModelID,
		Level:    cfg.Logging.Level,
	})
	if err != nil {
		return nil, err
	}

	runtime := &agentRunnerRuntime{
		cwd:           cwd,
		configPath:    cfg.ConfigPath,
		providerName:  resolved.ProviderName,
		modelProfile:  resolved.Profile,
		modelID:       resolved.ModelID,
		modelType:     resolved.Type,
		developerRole: model.MessageRole(resolved.DeveloperRole),
		inputLimit:    resolved.InputLimit,
		outputLimit:   resolved.OutputLimit,
		parameters:    resolved.Parameters,
		provider:      provider,
		toolExecutor: runToolExecutor{
			builtins:            toolRegistry,
			mcpSessions:         mcpSessionsByID,
			sessionTools:        newSessionToolExecutor(service, coordinator, session),
			enabledSessionTools: enabledSessionToolSet(enabledToolNames),
			webEval:             webEvalTool,
		},
		toolSchemas:        toolSchemas,
		maxTurns:           cfg.Agent.MaxTurns,
		enabledTools:       enabledToolNames,
		enabledMCP:         mcpServerIDs(selectedMCPServers),
		enabledSkills:      enabledSkillIDs,
		baseMessages:       baseMessages,
		instructionSources: instructionSources,
		session:            session,
		sessionStore:       store,
		activeItemIDs:      copyStringSlice(session.ActiveHistory),
		contextTracker:     contextTracker,
		contextBudget:      contextBudget,
		logger:             logger,
		recordRequest:      recordRequest,
		mcpSessions:        mcpSessions,
		config:             cfg,
	}
	if messages, err := runtime.materializeActiveHistory(session); err != nil {
		return nil, err
	} else if err := validateActiveHistoryToolExchanges(session.ID, messages); err != nil {
		return nil, err
	}
	return runtime, nil
}

type agentRunnerRuntime struct {
	cwd                 string
	configPath          string
	providerName        string
	modelProfile        string
	modelID             string
	modelType           string
	developerRole       model.MessageRole
	inputLimit          int
	outputLimit         int
	parameters          map[string]any
	provider            model.Provider
	toolExecutor        runToolExecutor
	toolSchemas         []model.Tool
	maxTurns            int
	enabledTools        []string
	enabledMCP          []string
	enabledSkills       []string
	baseMessages        []model.Message
	instructionSources  []sessions.InstructionSource
	session             sessions.SessionV2
	sessionStore        *sessions.V2Store
	activeItemIDs       []string
	contextTracker      *contextwindow.Tracker
	contextBudget       effectiveContextBudget
	logger              *eventlog.Logger
	recordRequest       func(endpoint string, body []byte) error
	mcpSessions         []*mcp.Session
	config              *config.Config
	onCompactionStarted func(trigger string)
}

func (r *agentRunnerRuntime) Close() error {
	return errors.Join(r.logger.Close(), closeMCPSessions(r.mcpSessions))
}

func (r *agentRunnerRuntime) initialMessages() ([]model.Message, error) {
	messages, err := r.materializeActiveHistory(r.session)
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 && len(r.activeItemIDs) == 0 && len(r.baseMessages) > 0 {
		return copyMessageSlice(r.baseMessages), nil
	}
	return copyMessageSlice(messages), nil
}

func (r *agentRunnerRuntime) materializeActiveHistory(session sessions.SessionV2) ([]model.Message, error) {
	if r != nil && r.sessionStore != nil {
		return r.sessionStore.MaterializeActiveHistory(session)
	}
	return session.MaterializeActiveHistory()
}

func (r *agentRunnerRuntime) saveRuntimeMetadataForSession(sessionID string) error {
	if r == nil || r.sessionStore == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	loaded, err := r.sessionStore.LoadState(sessionID)
	if err != nil {
		return err
	}
	saved, err := r.sessionStore.SaveMetadata(r.refreshSessionRuntimeMetadata(loaded))
	if err != nil {
		return fmt.Errorf("save resumable session metadata: %w", err)
	}
	r.session = saved
	r.activeItemIDs = copyStringSlice(saved.ActiveHistory)
	return nil
}

func (r *agentRunnerRuntime) refreshSessionRuntimeMetadata(session sessions.SessionV2) sessions.SessionV2 {
	var contextMetadata *contextwindow.Metadata
	if r.contextTracker != nil {
		metadata := r.contextTracker.Metadata()
		contextMetadata = &metadata
	}
	return sessions.RefreshRuntimeMetadata(session, sessions.RuntimeMetadataUpdate{
		Provider:             r.providerName,
		ModelProfile:         r.modelProfile,
		ModelID:              r.modelID,
		Pricing:              copyModelPricing(r.session.Pricing),
		ModelParameters:      r.parameters,
		CWD:                  r.cwd,
		ConfigPath:           r.configPath,
		EnabledTools:         r.enabledTools,
		EnabledMCP:           r.enabledMCP,
		EnabledSkills:        r.enabledSkills,
		ShowReasoning:        r.session.ShowReasoning,
		InstructionsSnapshot: r.baseMessages,
		InstructionSources:   r.instructionSources,
		Context:              contextMetadata,
		SaveToolResults:      true,
	})
}

type sessionTurnRunOptions struct {
	contentBlocks     []model.InputContentBlock
	resumeContext     bool
	emit              func(model.Event)
	publisher         eventbus.Publisher
	turnID            string
	activePromptDrain SessionActivePromptDrain
	toolCancel        *agent.ToolCancellationRegistry
	nextTurnID        func(iteration int) string
	turnIDChanged     func(string)
}

// adaptActivePromptDrain adapts an execution-domain active prompt drain into
// the agent-loop callback. A nil drain adapts to nil so the agent loop treats it
// as a no-op. Each agent checkpoint is mapped explicitly to the matching
// execution checkpoint; an unrecognized agent checkpoint is a programmer error
// and fails loudly rather than silently mis-mapping.
func adaptActivePromptDrain(drain SessionActivePromptDrain) agent.ActivePromptDrain {
	if drain == nil {
		return nil
	}
	return func(checkpoint agent.ActivePromptCheckpoint) []model.Message {
		return drain(toSessionActivePromptCheckpoint(checkpoint))
	}
}

func toSessionActivePromptCheckpoint(checkpoint agent.ActivePromptCheckpoint) SessionActivePromptCheckpoint {
	switch checkpoint {
	case agent.ActivePromptCheckpointBeforeProvider:
		return SessionActivePromptCheckpointBeforeProvider
	case agent.ActivePromptCheckpointAfterToolBatch:
		return SessionActivePromptCheckpointAfterToolBatch
	case agent.ActivePromptCheckpointBeforeTerminal:
		return SessionActivePromptCheckpointBeforeTerminal
	default:
		panic(fmt.Sprintf("execution: unknown agent active prompt checkpoint %d", checkpoint))
	}
}

func (r *agentRunnerRuntime) runSessionTurn(ctx context.Context, prompt string, options sessionTurnRunOptions) ([]model.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	messages, err := r.initialMessages()
	if err != nil {
		return nil, err
	}
	requestMessages := copyMessageSlice(messages)
	if !options.resumeContext {
		requestMessages = append(requestMessages, SessionMessageInput{
			Content:       prompt,
			ContentBlocks: copyInputContentBlocks(options.contentBlocks),
		}.Message())
	}
	request := model.Request{
		Model:         r.modelID,
		Messages:      requestMessages,
		Tools:         r.toolSchemas,
		Parameters:    r.parameters,
		SessionID:     r.session.ID,
		DeveloperRole: r.developerRole,
		MaxTokens:     r.outputLimit,
	}
	activeTurnID := options.turnID
	events, results, err := agent.StreamWithResult(turnCtx, request, agent.Options{
		Provider:     r.provider,
		ToolExecutor: r.toolExecutor,
		MaxTurns:     r.maxTurns,
		TurnID:       activeTurnID,
		Publisher:    options.publisher,
		NextTurnID:   options.nextTurnID,
		TurnIDChanged: func(turnID string) {
			activeTurnID = turnID
			if options.turnIDChanged != nil {
				options.turnIDChanged(turnID)
			}
		},
		ActivePromptDrain: adaptActivePromptDrain(options.activePromptDrain),
		AutoCompact: func(ctx context.Context, messages []model.Message) ([]model.Message, error) {
			return r.autoCompactAfterToolBatch(ctx, messages, options.publisher, activeTurnID)
		},
		ToolCancel: options.toolCancel,
	})
	if err != nil {
		return nil, err
	}
	for event := range events {
		if err := r.logger.LogEvent(event); err != nil {
			return nil, err
		}
		if options.emit != nil {
			options.emit(event)
		}
		if errEvent, ok := event.(model.ErrorEvent); ok {
			return nil, modelStreamError(errEvent)
		}
	}
	result, ok := <-results
	if !ok {
		if err := turnCtx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("agent did not return updated messages")
	}
	return result.Messages, nil
}

func modelStreamError(event model.ErrorEvent) error {
	if event.Err == nil {
		if event.Message == "" {
			return fmt.Errorf("model stream error")
		}
		return fmt.Errorf("%s", event.Message)
	}
	if event.Message == "" {
		return event.Err
	}
	return fmt.Errorf("%s: %w", event.Message, event.Err)
}

type compactionCheckpointOptions struct {
	reason  string
	phase   string
	trigger string
}

type compactionPlan struct {
	summaryItem sessions.SessionItem
	checkpoint  sessions.CompactionCheckpoint
	messages    []model.Message
	usage       *model.Usage
	context     *contextwindow.Metadata
}

type compactionPressureLevel uint8

const (
	compactionPressureNone compactionPressureLevel = iota
	compactionPressureSoft
	compactionPressureHard
)

type compactionPressure struct {
	level                compactionPressureLevel
	estimatedTokens      int
	requestBytes         int
	requestLimitExceeded bool
}

func (p compactionPressure) reason() string {
	if p.requestLimitExceeded {
		return "request_size_limit"
	}
	return "context_limit"
}

func (r *agentRunnerRuntime) planAutoCompactBeforeTurn(ctx context.Context, messages []model.Message, pendingInput model.Message) ([]model.Message, *compactionPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if r == nil || r.config == nil || !r.config.Compaction.Enabled || r.session.AutoCompactOff {
		return messages, nil, nil
	}
	if r.sessionStore == nil || strings.TrimSpace(r.session.ID) == "" {
		return messages, nil, nil
	}
	compactable, err := hasCompleteVisibleTurn(r.session)
	if err != nil {
		return nil, nil, err
	}
	if !compactable {
		return messages, nil, nil
	}
	pressureMessages := copyMessageSlice(messages)
	if pendingInput.Role != "" {
		pressureMessages = append(pressureMessages, pendingInput)
	}
	pressure := r.currentCompactionPressure(pressureMessages)
	if pressure.level == compactionPressureNone {
		return messages, nil, nil
	}
	if pressure.level == compactionPressureSoft && softCompactionCooldownActive(r.session) {
		return messages, nil, nil
	}
	plan, err := r.planCompactionCheckpoint(ctx, compactionCheckpointOptions{
		reason:  pressure.reason(),
		phase:   "pre_turn",
		trigger: "auto",
	})
	if err != nil {
		return nil, nil, fmt.Errorf("auto compact failed: %w", err)
	}
	if err := r.validateAutomaticCompactionResult(plan, pendingInput); err != nil {
		return nil, nil, err
	}
	if pressure.level == compactionPressureSoft && !compactionReclaimsEnough(pressure.estimatedTokens, r.compactionReplacementEstimate(plan, pendingInput)) {
		return messages, nil, nil
	}
	if r.onCompactionStarted != nil {
		r.onCompactionStarted("auto")
	}
	return plan.messages, &plan, nil
}

func (r *agentRunnerRuntime) autoCompactAfterToolBatch(ctx context.Context, messages []model.Message, publisher eventbus.Publisher, turnID string) ([]model.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || r.config == nil || !r.config.Compaction.Enabled || r.session.AutoCompactOff || r.sessionStore == nil || publisher == nil {
		return messages, nil
	}
	pressure := r.currentCompactionPressure(messages)
	if pressure.level == compactionPressureNone {
		return messages, nil
	}

	latest, err := r.sessionStore.LoadExecutionState(r.session.ID)
	if err != nil {
		return nil, fmt.Errorf("load session before mid-turn auto compact: %w", err)
	}
	r.session = latest
	r.activeItemIDs = copyStringSlice(latest.ActiveHistory)
	if pressure.level == compactionPressureSoft && softCompactionCooldownActive(r.session) {
		return messages, nil
	}
	plan, err := r.planCompactionCheckpoint(ctx, compactionCheckpointOptions{
		reason:  pressure.reason(),
		phase:   "mid_turn",
		trigger: "auto",
	})
	if err != nil {
		return nil, fmt.Errorf("mid-turn auto compact failed: %w", err)
	}
	if err := r.validateAutomaticCompactionResult(plan, model.Message{}); err != nil {
		return nil, err
	}
	if pressure.level == compactionPressureSoft && !compactionReclaimsEnough(pressure.estimatedTokens, r.compactionReplacementEstimate(plan, model.Message{})) {
		return messages, nil
	}
	if r.onCompactionStarted != nil {
		r.onCompactionStarted("auto")
	}
	if err := publishCompactionUsage(publisher, plan.usage); err != nil {
		return nil, fmt.Errorf("publish mid-turn compaction usage: %w", err)
	}
	if err := publisher.Publish(eventbus.CompactionRequested{
		TurnID:     strings.TrimSpace(turnID),
		Summary:    plan.summaryItem,
		Checkpoint: plan.checkpoint,
		Context:    plan.context,
	}); err != nil {
		return nil, fmt.Errorf("publish mid-turn compaction: %w", err)
	}
	compacted, err := r.sessionStore.LoadExecutionState(r.session.ID)
	if err != nil {
		return nil, fmt.Errorf("load session after mid-turn auto compact: %w", err)
	}
	r.session = compacted
	r.activeItemIDs = copyStringSlice(compacted.ActiveHistory)
	return plan.messages, nil
}

func (r *agentRunnerRuntime) autoCompactionPressure(messages []model.Message) bool {
	return r.currentCompactionPressure(messages).level != compactionPressureNone
}

func (r *agentRunnerRuntime) currentCompactionPressure(messages []model.Message) compactionPressure {
	if r == nil || r.config == nil {
		return compactionPressure{}
	}
	request := model.Request{
		Model: r.modelID, Messages: messages, Tools: r.toolSchemas,
		Parameters: r.parameters, SessionID: r.session.ID,
		DeveloperRole: r.developerRole, MaxTokens: r.outputLimit,
	}
	estimated := contextwindow.EstimateRequestTokens(request)
	if r.contextTracker != nil {
		estimated = r.contextTracker.EstimateRequest(request).InputTokens
	}
	requestBytes := r.autoCompactionRequestBytes(messages)
	requestLimitExceeded := r.config.Compaction.MaxRequestBytes > 0 && requestBytes >= r.config.Compaction.MaxRequestBytes
	if (r.contextBudget.HardInputLimit > 0 && estimated >= r.contextBudget.HardInputLimit) || requestLimitExceeded {
		return compactionPressure{level: compactionPressureHard, estimatedTokens: estimated, requestBytes: requestBytes, requestLimitExceeded: requestLimitExceeded}
	}
	if r.contextBudget.SoftInputLimit > 0 && estimated >= r.contextBudget.SoftInputLimit {
		return compactionPressure{level: compactionPressureSoft, estimatedTokens: estimated, requestBytes: requestBytes}
	}
	return compactionPressure{estimatedTokens: estimated, requestBytes: requestBytes}
}

func (r *agentRunnerRuntime) validateAutomaticCompactionResult(plan compactionPlan, pending model.Message) error {
	messages := copyMessageSlice(plan.messages)
	if pending.Role != "" {
		messages = append(messages, pending)
	}
	pressure := r.currentCompactionPressure(messages)
	if pressure.level == compactionPressureHard {
		return fmt.Errorf("automatic compaction could not reclaim enough context: replacement estimate %d tokens and %d request bytes still exceed the safe request budget", pressure.estimatedTokens, pressure.requestBytes)
	}
	return nil
}

func (r *agentRunnerRuntime) compactionReplacementEstimate(plan compactionPlan, pending model.Message) int {
	messages := copyMessageSlice(plan.messages)
	if pending.Role != "" {
		messages = append(messages, pending)
	}
	request := model.Request{Model: r.modelID, Messages: messages, Tools: r.toolSchemas, Parameters: r.parameters, SessionID: r.session.ID, DeveloperRole: r.developerRole, MaxTokens: r.outputLimit}
	return contextwindow.EstimateRequestTokens(request)
}

func compactionReclaimsEnough(before, after int) bool {
	if before <= 0 || after <= 0 {
		return true
	}
	reclaimed := before - after
	return reclaimed >= maxInt(2*1024, before/20)
}

const minimumCompleteTurnsBetweenSoftCompactions = 2

func softCompactionCooldownActive(session sessions.SessionV2) bool {
	if len(session.Compactions) == 0 {
		return false
	}
	checkpoint := session.Compactions[len(session.Compactions)-1]
	toIndex := -1
	for index, item := range session.Items {
		if item.ID == checkpoint.ToItemID {
			toIndex = index
			break
		}
	}
	if toIndex < 0 {
		return false
	}
	completeTurns := 0
	for _, item := range session.Items[toIndex+1:] {
		if item.Visibility == sessions.ItemVisibilityVisible && item.Message != nil && item.Message.Role == model.MessageRoleAssistant && len(item.Message.ToolCalls) == 0 {
			completeTurns++
		}
	}
	return completeTurns < minimumCompleteTurnsBetweenSoftCompactions
}

func (r *agentRunnerRuntime) planCompactionCheckpoint(ctx context.Context, checkpointOptions compactionCheckpointOptions) (compactionPlan, error) {
	if err := ctx.Err(); err != nil {
		return compactionPlan{}, err
	}
	if r == nil || r.config == nil {
		return compactionPlan{}, fmt.Errorf("runtime is not configured")
	}
	if !r.config.Compaction.Enabled {
		return compactionPlan{}, fmt.Errorf("compaction is disabled")
	}
	if strings.TrimSpace(r.session.ID) == "" {
		return compactionPlan{}, fmt.Errorf("compaction requires a saved or resumed session")
	}

	remote, err := openairesponses.UsesStandaloneCompaction(r.parameters)
	if err != nil {
		return compactionPlan{}, err
	}
	if remote {
		plan, remoteErr := r.planRemoteCompactionCheckpoint(ctx, checkpointOptions)
		if remoteErr == nil {
			return plan, nil
		}
		if r.logger != nil {
			message := fmt.Sprintf(
				"POST /responses/compact failed (reason=%s phase=%s trigger=%s); falling back to local summary",
				checkpointOptions.reason,
				checkpointOptions.phase,
				checkpointOptions.trigger,
			)
			if logErr := r.logger.LogEvent(model.ErrorEvent{Err: remoteErr, Message: message}); logErr != nil {
				return compactionPlan{}, errors.Join(
					fmt.Errorf("remote Responses compaction failed: %w", remoteErr),
					fmt.Errorf("log remote Responses compaction failure: %w", logErr),
				)
			}
		}
		plan, fallbackErr := r.planSummaryCompactionCheckpoint(ctx, checkpointOptions)
		if fallbackErr == nil {
			return plan, nil
		}
		return compactionPlan{}, errors.Join(
			fmt.Errorf("remote Responses compaction failed: %w", remoteErr),
			fmt.Errorf("local summary fallback failed: %w", fallbackErr),
		)
	}
	return r.planSummaryCompactionCheckpoint(ctx, checkpointOptions)
}

func (r *agentRunnerRuntime) planSummaryCompactionCheckpoint(ctx context.Context, checkpointOptions compactionCheckpointOptions) (compactionPlan, error) {
	summaryModel, err := r.resolveSummaryModel()
	if err != nil {
		return compactionPlan{}, err
	}
	summarySession, err := expandRemoteCompactionHistory(r.session)
	if err != nil {
		return compactionPlan{}, err
	}
	summaryBudget := resolveEffectiveContextBudget(summaryModel.ContextWindow, summaryModel.InputLimit, summaryModel.OutputLimit, r.config.Compaction.Reserved, r.config.Compaction.ThresholdPercent)
	if summaryBudget.HardInputLimit <= 0 {
		return compactionPlan{}, fmt.Errorf("compaction summary model has no usable input budget")
	}
	summaryInput, err := buildCompactionSummaryInput(summarySession, summaryModel, summaryBudget.HardInputLimit)
	if err != nil {
		return compactionPlan{}, err
	}
	provider, err := newProviderForRun(summaryModel.ProviderName, summaryModel.Type, summaryModel.Compatibility, summaryModel.Provider, r.recordRequest)
	if err != nil {
		return compactionPlan{}, err
	}
	summaryText, err := collectCompactionSummary(ctx, provider, summaryModel, summaryInput)
	if err != nil {
		return compactionPlan{}, err
	}

	summaryItemID := nextCompactionSummaryItemID(sessions.SessionItemIDs(r.session.Items))
	summaryMessage := model.Message{
		Role:    model.MessageRoleDeveloper,
		Content: formatCompactionSummary(summaryText),
	}
	summaryItem := sessions.SessionItem{
		ID:         summaryItemID,
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityHidden,
		Audience:   sessions.ItemAudienceModel,
		Message:    &summaryMessage,
	}
	replacementHistory, err := r.replacementHistoryAfterSummaryCompaction(summarySession, summaryItem)
	if err != nil {
		return compactionPlan{}, err
	}
	messages, err := validateCompactionReplacementHistory(r.session, summaryItem, replacementHistory)
	if err != nil {
		return compactionPlan{}, err
	}
	checkpoint := sessions.CompactionCheckpoint{
		ID:                    nextCompactionCheckpointID(r.session.Compactions),
		Reason:                checkpointOptions.reason,
		Phase:                 checkpointOptions.phase,
		Trigger:               checkpointOptions.trigger,
		SummaryItemID:         summaryItemID,
		FromItemID:            firstString(r.session.ActiveHistory),
		ToItemID:              lastString(r.session.ActiveHistory),
		PreviousActiveHistory: copyStringSlice(r.session.ActiveHistory),
		ReplacementHistory:    replacementHistory,
		SummaryProvider:       summaryModel.ProviderName,
		SummaryModel:          summaryModel.Profile,
	}
	return compactionPlan{
		summaryItem: summaryItem,
		checkpoint:  checkpoint,
		messages:    messages,
		context:     r.estimatedReplacementContext(messages),
	}, nil
}

func (r *agentRunnerRuntime) planRemoteCompactionCheckpoint(ctx context.Context, checkpointOptions compactionCheckpointOptions) (compactionPlan, error) {
	resolved, err := r.config.ResolveModel(r.providerName, r.modelProfile)
	if err != nil {
		return compactionPlan{}, err
	}
	if resolved.Type != config.ProviderTypeOpenAIResponses && resolved.Type != config.ProviderTypeOpenAICodex {
		return compactionPlan{}, fmt.Errorf("responses.compaction.mode requires an OpenAI Responses or Codex model")
	}
	resolved.ModelID = r.modelID
	resolved.Parameters = copyParameterMap(r.parameters)

	messages, err := r.materializeActiveHistory(r.session)
	if err != nil {
		return compactionPlan{}, err
	}
	provider, err := newProviderForRun(resolved.ProviderName, resolved.Type, resolved.Compatibility, resolved.Provider, r.recordRequest)
	if err != nil {
		return compactionPlan{}, err
	}
	compactor, ok := provider.(model.CompactionProvider)
	if !ok {
		return compactionPlan{}, fmt.Errorf("provider %q does not support standalone compaction", resolved.ProviderName)
	}
	compacted, err := compactor.Compact(ctx, model.Request{
		Model:      resolved.ModelID,
		Messages:   messages,
		Tools:      r.toolSchemas,
		Parameters: resolved.Parameters,
		SessionID:  r.session.ID,
	})
	if err != nil {
		return compactionPlan{}, err
	}
	compactionMessage := model.Message{
		Role:          model.MessageRoleProvider,
		ProviderItems: copyProviderItemSlice(compacted.Items),
	}
	itemID := sessions.NextSessionItemID(sessions.SessionItemIDs(r.session.Items), compactionMessage)
	compactionItem := sessions.SessionItem{
		ID:         itemID,
		Kind:       sessions.ItemKindCompaction,
		Visibility: sessions.ItemVisibilityHidden,
		Audience:   sessions.ItemAudienceModel,
		Message:    &compactionMessage,
	}
	replacementHistory := []string{itemID}
	messages, err = validateCompactionReplacementHistory(r.session, compactionItem, replacementHistory)
	if err != nil {
		return compactionPlan{}, err
	}
	usage, contextMetadata, err := r.recordRemoteCompactionUsage(compacted.Usage, messages)
	if err != nil {
		return compactionPlan{}, err
	}
	checkpoint := sessions.CompactionCheckpoint{
		ID:                    nextCompactionCheckpointID(r.session.Compactions),
		Reason:                checkpointOptions.reason,
		Phase:                 checkpointOptions.phase,
		Trigger:               checkpointOptions.trigger,
		SummaryItemID:         itemID,
		FromItemID:            firstString(r.session.ActiveHistory),
		ToItemID:              lastString(r.session.ActiveHistory),
		PreviousActiveHistory: copyStringSlice(r.session.ActiveHistory),
		ReplacementHistory:    replacementHistory,
		SummaryProvider:       resolved.ProviderName,
		SummaryModel:          resolved.Profile,
	}
	return compactionPlan{
		summaryItem: compactionItem,
		checkpoint:  checkpoint,
		messages:    messages,
		usage:       usage,
		context:     contextMetadata,
	}, nil
}

func (r *agentRunnerRuntime) recordRemoteCompactionUsage(usage model.Usage, replacementMessages []model.Message) (*model.Usage, *contextwindow.Metadata, error) {
	if usage == (model.Usage{}) {
		return nil, nil, nil
	}
	event := model.UsageEvent{Usage: usage}
	if r.logger != nil {
		if err := r.logger.LogEvent(event); err != nil {
			return nil, nil, err
		}
	}

	usageCopy := usage
	if r.contextTracker == nil {
		return &usageCopy, nil, nil
	}
	// Standalone compaction bypasses TrackingProvider, so record its provider
	// usage explicitly. The replacement-history estimate below is only for the
	// context-window meter and is intentionally not billable usage.
	r.contextTracker.RecordProviderUsage(usage)
	return &usageCopy, r.estimatedReplacementContext(replacementMessages), nil
}

func (r *agentRunnerRuntime) estimatedReplacementContext(replacementMessages []model.Message) *contextwindow.Metadata {
	if r == nil || r.contextTracker == nil {
		return nil
	}
	estimate := contextwindow.EstimateRequestTokens(model.Request{
		Model:      r.modelID,
		Messages:   replacementMessages,
		Tools:      r.toolSchemas,
		Parameters: r.parameters,
		SessionID:  r.session.ID,
	})
	r.contextTracker.RecordEstimatedContextUsage(estimate, 0)
	metadata := r.contextTracker.Metadata()
	return &metadata
}

func (r *agentRunnerRuntime) autoCompactionRequestBytes(messages []model.Message) int {
	if r == nil || (r.modelType != config.ProviderTypeOpenAIResponses && r.modelType != config.ProviderTypeOpenAICodex) {
		return 0
	}
	body, err := openairesponses.BuildRequestBody(model.Request{
		Model:      r.modelID,
		Messages:   messages,
		Tools:      r.toolSchemas,
		Parameters: r.parameters,
		SessionID:  r.session.ID,
	}, true)
	if err != nil {
		return 0
	}
	return len(body)
}

func expandRemoteCompactionHistory(session sessions.SessionV2) (sessions.SessionV2, error) {
	itemsByID := make(map[string]sessions.SessionItem, len(session.Items))
	for _, item := range session.Items {
		itemsByID[item.ID] = item
	}
	checkpointsByItemID := make(map[string]sessions.CompactionCheckpoint, len(session.Compactions))
	for _, checkpoint := range session.Compactions {
		checkpointsByItemID[checkpoint.SummaryItemID] = checkpoint
	}

	var expand func([]string, map[string]struct{}) ([]string, error)
	expand = func(ids []string, visiting map[string]struct{}) ([]string, error) {
		result := make([]string, 0, len(ids))
		for _, id := range ids {
			item, ok := itemsByID[id]
			if !ok {
				return nil, corruptedSessionHistoryError(session.ID, "active history references missing item %q", id)
			}
			if item.Kind != sessions.ItemKindCompaction || item.Message == nil || item.Message.Role != model.MessageRoleProvider {
				result = append(result, id)
				continue
			}
			if _, cycle := visiting[id]; cycle {
				return nil, corruptedSessionHistoryError(session.ID, "remote compaction history contains a cycle at item %q", id)
			}
			checkpoint, ok := checkpointsByItemID[id]
			if !ok || len(checkpoint.PreviousActiveHistory) == 0 {
				return nil, corruptedSessionHistoryError(session.ID, "remote compaction item %q has no previous history checkpoint", id)
			}
			nextVisiting := make(map[string]struct{}, len(visiting)+1)
			for key := range visiting {
				nextVisiting[key] = struct{}{}
			}
			nextVisiting[id] = struct{}{}
			expanded, err := expand(checkpoint.PreviousActiveHistory, nextVisiting)
			if err != nil {
				return nil, err
			}
			result = append(result, expanded...)
		}
		return result, nil
	}

	activeHistory, err := expand(session.ActiveHistory, map[string]struct{}{})
	if err != nil {
		return sessions.SessionV2{}, err
	}
	session.ActiveHistory = activeHistory
	return session, nil
}

func (r *agentRunnerRuntime) resolveSummaryModel() (config.ResolvedModel, error) {
	compaction := r.config.Compaction
	providerName := strings.TrimSpace(compaction.SummaryProvider)
	modelProfile := strings.TrimSpace(compaction.SummaryModel)
	if providerName == "" {
		providerName = r.providerName
	}
	if modelProfile == "" {
		modelProfile = r.modelProfile
	}
	if strings.TrimSpace(compaction.SummaryProvider) != "" && strings.TrimSpace(compaction.SummaryModel) == "" {
		provider, ok := r.config.Providers[providerName]
		if !ok {
			return config.ResolvedModel{}, fmt.Errorf("unknown compaction summary provider %q", providerName)
		}
		if _, ok := provider.Models[modelProfile]; !ok {
			return config.ResolvedModel{}, fmt.Errorf("compaction summary_provider %q requires summary_model because current model profile %q is not available for that provider", providerName, modelProfile)
		}
	}
	resolved, err := r.config.ResolveModel(providerName, modelProfile)
	if err != nil {
		return config.ResolvedModel{}, fmt.Errorf("resolve compaction summary model: %w", err)
	}
	if providerName == r.providerName && modelProfile == r.modelProfile {
		// Summaries produced by the session's own model must use the session's
		// pinned parameters, which include the reasoning level selected at
		// creation. Otherwise the summary request silently falls back to the
		// current config defaults, diverging from the main turn loop and from
		// the remote compaction path.
		if r.modelID != "" {
			resolved.ModelID = r.modelID
		}
		if r.parameters != nil {
			resolved.Parameters = copyParameterMap(r.parameters)
		}
	}
	return resolved, nil
}

type compactionSummaryInput struct {
	Messages []model.Message
}

func buildCompactionSummaryInput(session sessions.SessionV2, resolved config.ResolvedModel, hardInputLimit int) (compactionSummaryInput, error) {
	activeItems, err := activeHistoryItems(session)
	if err != nil {
		return compactionSummaryInput{}, err
	}
	contextItems, visibleGroups := splitCompactionInputItems(activeItems)
	for drop := 0; drop <= len(visibleGroups); drop++ {
		messages := compactionPromptMessages(contextItems, visibleGroups[drop:])
		request := model.Request{
			Model:         resolved.ModelID,
			Messages:      messages,
			Parameters:    resolved.Parameters,
			DeveloperRole: model.MessageRole(resolved.DeveloperRole),
		}
		estimated := contextwindow.EstimateRequestTokens(request)
		if hardInputLimit <= 0 || estimated < hardInputLimit {
			return compactionSummaryInput{Messages: messages}, nil
		}
	}
	return compactionSummaryInput{}, fmt.Errorf("compaction summary input exceeds the summary model's safe input budget after trimming older visible history")
}

func activeHistoryItems(session sessions.SessionV2) ([]sessions.SessionItem, error) {
	itemsByID := make(map[string]sessions.SessionItem, len(session.Items))
	for _, item := range session.Items {
		itemsByID[item.ID] = item
	}
	items := make([]sessions.SessionItem, 0, len(session.ActiveHistory))
	for _, id := range session.ActiveHistory {
		item, ok := itemsByID[id]
		if !ok {
			return nil, corruptedSessionHistoryError(session.ID, "active history references missing item %q", id)
		}
		if item.Message == nil {
			return nil, corruptedSessionHistoryError(session.ID, "active history references item %q without a message", id)
		}
		items = append(items, item)
	}
	return items, nil
}

func splitCompactionInputItems(activeItems []sessions.SessionItem) ([]sessions.SessionItem, [][]sessions.SessionItem) {
	contextItems := []sessions.SessionItem{}
	groups := [][]sessions.SessionItem{}
	var current []sessions.SessionItem
	for _, item := range activeItems {
		if item.Visibility != sessions.ItemVisibilityVisible {
			contextItems = append(contextItems, item)
			continue
		}
		if item.Message != nil && item.Message.Role == model.MessageRoleUser {
			if len(current) > 0 {
				groups = append(groups, current)
			}
			current = []sessions.SessionItem{item}
			continue
		}
		if len(current) == 0 {
			contextItems = append(contextItems, item)
			continue
		}
		current = append(current, item)
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return contextItems, groups
}

func compactionPromptMessages(contextItems []sessions.SessionItem, visibleGroups [][]sessions.SessionItem) []model.Message {
	transcript := &strings.Builder{}
	if len(contextItems) > 0 {
		transcript.WriteString("## Current Model-Facing Runtime Context\n\n")
		for _, item := range contextItems {
			writeSummaryTranscriptMessage(transcript, *item.Message)
		}
		transcript.WriteByte('\n')
	}
	if len(visibleGroups) > 0 {
		transcript.WriteString("## Visible Conversation History\n\n")
		for _, group := range visibleGroups {
			for _, item := range group {
				writeSummaryTranscriptMessage(transcript, *item.Message)
			}
			transcript.WriteByte('\n')
		}
	}
	if transcript.Len() == 0 {
		transcript.WriteString("(No active model-facing messages are available.)\n")
	}

	return []model.Message{
		{
			Role:    model.MessageRoleSystem,
			Content: "Create a concise handoff checkpoint for continuing this session. Do not include hidden reasoning or chain-of-thought. Preserve facts, decisions, constraints, relevant files/APIs/commands, tool/environment state, open questions, and next steps. Use exactly these markdown headings: # Context Checkpoint, ## Goal, ## Current Progress, ## Decisions Made, ## Constraints / User Preferences, ## Relevant Files / APIs / Commands, ## Tool State / Environment State, ## Open Questions, ## Next Steps. Mention that old complete session items remain stored but may be omitted from active model context.",
		},
		{
			Role:    model.MessageRoleUser,
			Content: transcript.String(),
		},
	}
}

func writeSummaryTranscriptMessage(out *strings.Builder, message model.Message) {
	fmt.Fprintf(out, "<message role=%q", message.Role)
	if message.ToolCallID != "" {
		fmt.Fprintf(out, " tool_call_id=%q", message.ToolCallID)
	}
	out.WriteString(">\n")
	if message.Content != "" {
		out.WriteString(message.Content)
		if !strings.HasSuffix(message.Content, "\n") {
			out.WriteByte('\n')
		}
	}
	for _, block := range message.ContentBlocks {
		fmt.Fprintf(out, "<content_block type=%q", block.Type)
		if block.FileID != "" {
			fmt.Fprintf(out, " file_id=%q", block.FileID)
		}
		out.WriteString(">\n")
		if block.Text != "" {
			out.WriteString(block.Text)
			out.WriteByte('\n')
		}
		if block.ImageURL != "" {
			if strings.HasPrefix(block.ImageURL, "data:") {
				out.WriteString("[inline image content omitted from summary transcript]\n")
			} else {
				fmt.Fprintf(out, "image_url=%q\n", block.ImageURL)
			}
		}
		out.WriteString("</content_block>\n")
	}
	for _, toolCall := range message.ToolCalls {
		fmt.Fprintf(out, "<tool_call id=%q name=%q arguments=%q />\n", toolCall.ID, toolCall.Name, toolCall.Arguments)
	}
	for _, item := range message.ProviderItems {
		var value any
		if json.Unmarshal(item.Data, &value) != nil {
			continue
		}
		redactCompactionProviderContent(value)
		if data, err := json.Marshal(value); err == nil {
			fmt.Fprintf(out, "<provider_item origin=%q model=%q>%s</provider_item>\n", item.Origin, item.Model, data)
		}
	}
	out.WriteString("</message>\n")
}

func redactCompactionProviderContent(value any) {
	switch value := value.(type) {
	case map[string]any:
		if _, ok := value["encrypted_content"]; ok {
			value["encrypted_content"] = "[redacted]"
		}
		for _, child := range value {
			redactCompactionProviderContent(child)
		}
	case []any:
		for _, child := range value {
			redactCompactionProviderContent(child)
		}
	}
}

func collectCompactionSummary(ctx context.Context, provider model.Provider, resolved config.ResolvedModel, input compactionSummaryInput) (string, error) {
	stream, err := provider.Stream(ctx, model.Request{
		Model:         resolved.ModelID,
		Messages:      input.Messages,
		Parameters:    resolved.Parameters,
		DeveloperRole: model.MessageRole(resolved.DeveloperRole),
		MaxTokens:     resolved.OutputLimit,
	})
	if err != nil {
		return "", fmt.Errorf("request compaction summary: %w", err)
	}
	var text strings.Builder
	var doneText string
	for event := range stream {
		switch event := event.(type) {
		case model.TextDeltaEvent:
			text.WriteString(event.Text)
		case model.MessageDoneEvent:
			doneText = event.Message.Content
		case model.ErrorEvent:
			return "", modelStreamError(event)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	summary := strings.TrimSpace(text.String())
	if summary == "" {
		summary = strings.TrimSpace(doneText)
	}
	if summary == "" {
		return "", fmt.Errorf("compaction summary was empty")
	}
	return summary, nil
}

func formatCompactionSummary(summary string) string {
	return "<compaction_summary>\nAnother agent continued this session from a checkpoint. Use the state below as handoff context. Do not treat it as a new user request.\n\n" + strings.TrimSpace(summary) + "\n</compaction_summary>"
}

const compactionRecentVisibleTurnFallback = 2

func (r *agentRunnerRuntime) replacementHistoryAfterSummaryCompaction(session sessions.SessionV2, summaryItem sessions.SessionItem) ([]string, error) {
	activeItems, err := activeHistoryItems(session)
	if err != nil {
		return nil, err
	}
	prefix := make([]string, 0, len(activeItems)+1)
	for _, item := range activeItems {
		if item.Kind == sessions.ItemKindRuntimeContext && item.Message != nil {
			prefix = append(prefix, item.ID)
		}
	}
	prefix = append(prefix, summaryItem.ID)
	groups := recentCompleteVisibleTurns(activeItems, len(activeItems))
	if r == nil || r.contextBudget.TargetTokens <= 0 {
		groups = recentCompleteVisibleTurns(activeItems, compactionRecentVisibleTurnFallback)
	}
	selected := make([][]sessions.SessionItem, 0, len(groups))
	for index := len(groups) - 1; index >= 0; index-- {
		candidateGroups := append([][]sessions.SessionItem{groups[index]}, selected...)
		candidate := append([]string{}, prefix...)
		for _, group := range candidateGroups {
			for _, item := range group {
				candidate = append(candidate, item.ID)
			}
		}
		if r == nil || r.contextBudget.TargetTokens <= 0 {
			selected = candidateGroups
			continue
		}
		messages, materializeErr := materializeCompactionReplacementHistory(session, summaryItem, candidate)
		if materializeErr != nil {
			return nil, materializeErr
		}
		estimate := contextwindow.EstimateRequestTokens(model.Request{
			Model: r.modelID, Messages: messages, Tools: r.toolSchemas,
			Parameters: r.parameters, SessionID: r.session.ID,
			DeveloperRole: r.developerRole, MaxTokens: r.outputLimit,
		})
		if estimate <= r.contextBudget.TargetTokens {
			selected = candidateGroups
		} else {
			break
		}
	}
	replacement := append([]string{}, prefix...)
	for _, group := range selected {
		for _, item := range group {
			replacement = append(replacement, item.ID)
		}
	}
	return replacement, nil
}

func validateCompactionReplacementHistory(session sessions.SessionV2, summaryItem sessions.SessionItem, replacementHistory []string) ([]model.Message, error) {
	messages, err := materializeCompactionReplacementHistory(session, summaryItem, replacementHistory)
	if err != nil {
		return nil, err
	}
	if err := validateActiveHistoryToolExchanges(session.ID, messages); err != nil {
		return nil, err
	}
	return messages, nil
}

func materializeCompactionReplacementHistory(session sessions.SessionV2, summaryItem sessions.SessionItem, replacementHistory []string) ([]model.Message, error) {
	itemsByID := make(map[string]sessions.SessionItem, len(session.Items)+1)
	for _, item := range session.Items {
		itemsByID[item.ID] = item
	}
	itemsByID[summaryItem.ID] = summaryItem

	messages := make([]model.Message, 0, len(replacementHistory))
	for _, id := range replacementHistory {
		item, ok := itemsByID[id]
		if !ok {
			return nil, corruptedSessionHistoryError(session.ID, "compaction replacement history references missing item %q", id)
		}
		if item.Message == nil {
			return nil, corruptedSessionHistoryError(session.ID, "compaction replacement history references item %q without a message", id)
		}
		messages = append(messages, copyMessageSlice([]model.Message{*item.Message})[0])
	}
	return messages, nil
}

func recentCompleteVisibleTurns(activeItems []sessions.SessionItem, limit int) [][]sessions.SessionItem {
	if limit <= 0 {
		return nil
	}
	_, groups := splitCompactionInputItems(activeItems)
	selected := make([][]sessions.SessionItem, 0, limit)
	for i := len(groups) - 1; i >= 0; i-- {
		if visibleTurnIsComplete(groups[i]) {
			selected = append([][]sessions.SessionItem{groups[i]}, selected...)
			if len(selected) == limit {
				return selected
			}
		}
	}
	return selected
}

func hasCompleteVisibleTurn(session sessions.SessionV2) (bool, error) {
	activeItems, err := activeHistoryItems(session)
	if err != nil {
		return false, err
	}
	_, groups := splitCompactionInputItems(activeItems)
	for _, group := range groups {
		if visibleTurnIsComplete(group) {
			return true, nil
		}
	}
	return false, nil
}

func visibleTurnIsComplete(items []sessions.SessionItem) bool {
	if len(items) == 0 || items[0].Message == nil || items[0].Message.Role != model.MessageRoleUser {
		return false
	}
	last := items[len(items)-1].Message
	if last == nil || last.Role != model.MessageRoleAssistant || len(last.ToolCalls) != 0 {
		return false
	}
	messages := make([]model.Message, 0, len(items))
	for _, item := range items {
		if item.Message == nil {
			return false
		}
		messages = append(messages, *item.Message)
	}
	return validateActiveHistoryToolExchanges("", messages) == nil
}

func nextCompactionSummaryItemID(existing map[string]struct{}) string {
	for i := len(existing) + 1; ; i++ {
		id := fmt.Sprintf("summary-%06d", i)
		if _, ok := existing[id]; !ok {
			return id
		}
	}
}

func nextCompactionCheckpointID(existing []sessions.CompactionCheckpoint) string {
	used := make(map[string]struct{}, len(existing))
	for _, checkpoint := range existing {
		used[checkpoint.ID] = struct{}{}
	}
	for i := len(existing) + 1; ; i++ {
		id := fmt.Sprintf("compact-%06d", i)
		if _, ok := used[id]; !ok {
			return id
		}
	}
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func lastString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

func enabledToolsForRun(rootDir string, enabled []string, fullAccess bool) (*tools.Registry, []model.Tool, error) {
	return enabledToolsForRunWithReadRoots(rootDir, enabled, fullAccess, nil)
}

func enabledToolsForRunWithReadRoots(rootDir string, enabled []string, fullAccess bool, extraReadRoots []string) (*tools.Registry, []model.Tool, error) {
	builtinEnabled := make([]string, 0, len(enabled))
	for _, name := range enabled {
		// web.eval is an internal runtime attachment, never a configured or
		// durable tool. Ignore a stale/manual config entry rather than letting
		// the built-in registry turn it into an unknown-tool startup error.
		if name == WebEvalToolName {
			continue
		}
		if IsSessionTool(name) {
			continue
		}
		if !strings.HasPrefix(name, "mcp.") {
			builtinEnabled = append(builtinEnabled, name)
		}
	}
	if len(builtinEnabled) == 0 {
		return nil, nil, nil
	}

	registry := tools.NewRegistry()
	if err := tools.RegisterBuiltinsWithReadRoots(registry, rootDir, fullAccess, extraReadRoots); err != nil {
		return nil, nil, fmt.Errorf("register built-in tools: %w", err)
	}
	schemas, err := registry.EnabledSchemas(builtinEnabled)
	if err != nil {
		return nil, nil, err
	}
	return registry, schemas, nil
}

// assembleAgentToolSelection is the runtime-only tool assembly boundary. In
// particular, web.eval is removed before child filtering and before any
// durable runtime metadata can see the enabled names. Its schema/executor are
// added independently from the configured tool list.
func assembleAgentToolSelection(cwd string, session sessions.SessionV2, store *sessions.V2Store, service *Service) (enabled []string, registry *tools.Registry, schemas []model.Tool, webEval *webEvalToolExecutor, err error) {
	return assembleAgentToolSelectionWithReadRoots(cwd, session, store, service, nil)
}

func assembleAgentToolSelectionWithReadRoots(cwd string, session sessions.SessionV2, store *sessions.V2Store, service *Service, extraReadRoots []string) (enabled []string, registry *tools.Registry, schemas []model.Tool, webEval *webEvalToolExecutor, err error) {
	configured := copyStringSlice(session.EnabledTools)
	enabled = make([]string, 0, len(configured))
	for _, name := range configured {
		if name == WebEvalToolName {
			continue
		}
		enabled = append(enabled, name)
	}
	if strings.TrimSpace(session.ParentSessionID) != "" {
		enabled = enabledToolsForAgentChild(enabled)
	}
	registry, schemas, err = enabledToolsForRunWithReadRoots(cwd, enabled, session.FullAccess, extraReadRoots)
	if err != nil {
		return enabled, nil, nil, nil, err
	}
	schemas = append(schemas, enabledSessionToolSchemas(enabled)...)
	webEval = prepareWebEvalTool(session, store, service)
	if webEval != nil {
		schemas = append(schemas, WebEvalToolSchema())
	}
	return enabled, registry, schemas, webEval, nil
}

type runToolExecutor struct {
	builtins            *tools.Registry
	mcpSessions         map[string]*mcp.Session
	sessionTools        *sessionToolExecutor
	enabledSessionTools map[string]struct{}
	webEval             *webEvalToolExecutor
}

func (e runToolExecutor) Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error) {
	if name == WebEvalToolName {
		if e.webEval == nil {
			return webEvalToolFailure(0, webdebug.ErrorCodeNotConnected, "web debug executor is unavailable"), nil
		}
		return e.webEval.Execute(ctx, arguments)
	}
	if IsSessionTool(name) {
		if _, enabled := e.enabledSessionTools[name]; !enabled {
			return model.ToolResult{}, fmt.Errorf("session tool %q is not enabled for this run", name)
		}
		if e.sessionTools == nil {
			return model.ToolResult{}, fmt.Errorf("session tool %q is not configured", name)
		}
		return e.sessionTools.Execute(ctx, name, arguments)
	}

	if strings.HasPrefix(name, "mcp.") {
		serverID, toolName, err := mcp.ParseToolName(name)
		if err != nil {
			return model.ToolResult{}, err
		}
		session, ok := e.mcpSessions[serverID]
		if !ok {
			return model.ToolResult{}, fmt.Errorf("MCP server %q is not running for tool %q", serverID, name)
		}
		result, err := session.CallTool(ctx, toolName, arguments)
		if err != nil {
			return model.ToolResult{}, fmt.Errorf("call MCP tool %q: %w", name, err)
		}
		return mcp.ToModelToolResult(name, result), nil
	}

	if e.builtins == nil {
		return model.ToolResult{}, fmt.Errorf("tool %q is not registered", name)
	}
	return e.builtins.Execute(ctx, name, arguments)
}

func enabledSessionToolSet(enabled []string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, name := range enabled {
		if IsSessionTool(name) {
			result[name] = struct{}{}
		}
	}
	return result
}

func mcpToolsForRun(ctx context.Context, servers []config.MCPServerConfig, enabled []string) ([]*mcp.Session, map[string]*mcp.Session, []model.Tool, error) {
	activeSessions := make([]*mcp.Session, 0, len(servers))
	sessionsByID := make(map[string]*mcp.Session, len(servers))
	convertedTools := []model.Tool{}

	for _, server := range servers {
		session, _, err := mcp.StartStdioSession(ctx, server)
		if err != nil {
			return nil, nil, nil, errors.Join(err, closeMCPSessions(activeSessions))
		}
		activeSessions = append(activeSessions, session)
		sessionsByID[server.ID] = session

		definitions, err := session.ListTools(ctx)
		if err != nil {
			return nil, nil, nil, errors.Join(fmt.Errorf("list MCP tools for server %q: %w", server.ID, err), closeMCPSessions(activeSessions))
		}
		tools, err := mcp.ConvertTools(server.ID, definitions)
		if err != nil {
			return nil, nil, nil, errors.Join(fmt.Errorf("convert MCP tools for server %q: %w", server.ID, err), closeMCPSessions(activeSessions))
		}
		convertedTools = append(convertedTools, tools...)
	}

	schemas, err := mcp.EnabledSchemas(convertedTools, enabled)
	if err != nil {
		return nil, nil, nil, errors.Join(err, closeMCPSessions(activeSessions))
	}
	return activeSessions, sessionsByID, schemas, nil
}

func enabledSkillsForRun(cfg *config.Config, cwd string) ([]localskills.Skill, error) {
	skillDirs, err := cfg.ResolveSkillDirs(cwd)
	if err != nil {
		return nil, err
	}
	discovered, err := localskills.DiscoverDirs(skillDirs)
	if err != nil {
		return nil, fmt.Errorf("load skills: %w", err)
	}
	return modelInvokedSkills(discovered), nil
}

func modelInvokedSkills(discovered []localskills.Skill) []localskills.Skill {
	if len(discovered) == 0 {
		return nil
	}
	loaded := make([]localskills.Skill, 0, len(discovered))
	for _, skill := range discovered {
		if !skill.DisableModelInvocation {
			loaded = append(loaded, skill)
		}
	}
	return loaded
}

func closeMCPSessions(sessions []*mcp.Session) error {
	var closeErr error
	for _, session := range sessions {
		if err := session.Close(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	if closeErr != nil {
		return fmt.Errorf("close MCP sessions: %w", closeErr)
	}
	return nil
}

func newProviderForRun(providerName, modelType, compatibility string, provider config.ProviderConfig, recordRequest func(endpoint string, body []byte) error) (model.Provider, error) {
	httpOptions, err := providerHTTPOptions(provider)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", providerName, err)
	}
	httpClient, err := providerHTTPClient(provider)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", providerName, err)
	}
	switch modelType {
	case config.ProviderTypeOpenAIChat:
		providerConfig := openAIChatProviderConfig(provider, compatibility, httpClient, httpOptions)
		providerConfig.RecordRequest = recordRequest
		return openaichat.NewProvider(providerConfig)
	case config.ProviderTypeOpenAIResponses:
		providerConfig := openAIResponsesProviderConfig(provider, httpClient, httpOptions)
		providerConfig.RecordRequest = recordRequest
		return openairesponses.NewProvider(providerConfig)
	case config.ProviderTypeOpenAICodex:
		providerConfig := openAICodexProviderConfig(provider, httpClient, httpOptions)
		providerConfig.RecordRequest = recordRequest
		return openairesponses.NewProvider(providerConfig)
	case config.ProviderTypeAnthropicMessages:
		return anthropicmessages.NewProvider(anthropicMessagesProviderConfig(provider, httpClient, httpOptions))
	default:
		return nil, fmt.Errorf("unsupported model type %q for provider %q", modelType, providerName)
	}
}

func providerHTTPOptions(provider config.ProviderConfig) (httpstream.Options, error) {
	// Status-code retries are decided at the session layer (agent.streamModelTurn);
	// the transport layer must not retry statuses on its own. An explicit 1 is
	// required on every return path: httpstream.Options.WithDefaults backfills
	// DefaultMaxRetryAttempts when the value is <= 0.
	options := httpstream.Options{MaxRetryAttempts: 1}
	if provider.RequestTimeout == "" {
		return options, nil
	}
	requestTimeout, err := time.ParseDuration(provider.RequestTimeout)
	if err != nil || requestTimeout <= 0 {
		return httpstream.Options{}, fmt.Errorf("request_timeout must be a positive duration")
	}
	options.RequestTimeout = requestTimeout
	return options, nil
}

func openAIChatProviderConfig(provider config.ProviderConfig, compatibility string, httpClient *http.Client, httpOptions httpstream.Options) openaichat.ProviderConfig {
	return openaichat.ProviderConfig{
		BaseURL:       provider.BaseURL,
		APIKey:        provider.ResolvedAPIKey,
		Compatibility: compatibility,
		HTTPClient:    httpClient,
		HTTPOptions:   httpOptions,
	}
}

func openAIResponsesProviderConfig(provider config.ProviderConfig, httpClient *http.Client, httpOptions httpstream.Options) openairesponses.ProviderConfig {
	return openairesponses.ProviderConfig{
		BaseURL:     provider.BaseURL,
		APIKey:      provider.ResolvedAPIKey,
		HTTPClient:  httpClient,
		HTTPOptions: httpOptions,
	}
}

func openAICodexProviderConfig(provider config.ProviderConfig, httpClient *http.Client, httpOptions httpstream.Options) openairesponses.ProviderConfig {
	return openairesponses.ProviderConfig{
		BaseURL:         provider.BaseURL,
		ForceStoreFalse: true,
		// The Codex backend enforces a strict parameter allowlist and rejects
		// max_output_tokens with HTTP 400, so the runner-level output limit
		// must stay client-side only.
		OmitMaxOutputTokens: true,
		HTTPClient:          httpClient,
		HTTPOptions:         httpOptions,
		TokenSource: codexResponsesTokenSource{
			source: &codexauth.TokenSource{
				Store:      codexauth.Store{Path: provider.AuthFile},
				HTTPClient: httpClient,
			},
		},
	}
}

func anthropicMessagesProviderConfig(provider config.ProviderConfig, httpClient *http.Client, httpOptions httpstream.Options) anthropicmessages.ProviderConfig {
	return anthropicmessages.ProviderConfig{
		BaseURL:     provider.BaseURL,
		APIKey:      provider.ResolvedAPIKey,
		HTTPClient:  httpClient,
		HTTPOptions: httpOptions,
	}
}

type codexResponsesTokenSource struct {
	source *codexauth.TokenSource
}

func (s codexResponsesTokenSource) AccessToken(ctx context.Context) (openairesponses.AccessToken, error) {
	token, err := s.source.AccessToken(ctx)
	if err != nil {
		return openairesponses.AccessToken{}, err
	}
	return openairesponses.AccessToken{Token: token.Token, AccountID: token.AccountID}, nil
}

func promptDeveloperMessagesForRun(cfg *config.Config, cwd string) ([]string, error) {
	messages := []string{}
	if strings.TrimSpace(cfg.Prompt.SystemPrompt) == "" {
		return messages, nil
	}
	rendered, err := projectcontext.RenderPromptTemplate(cfg.Prompt.SystemPrompt, projectcontext.PromptRenderValues{
		CWD:       cwd,
		ConfigDir: filepath.Dir(cfg.ConfigPath),
		Now:       time.Now().UTC(),
	})
	if err != nil {
		return nil, fmt.Errorf("render prompt.system_prompt: %w", err)
	}
	if strings.TrimSpace(rendered) != "" {
		messages = append(messages, rendered)
	}
	return messages, nil
}

func chatBaseMessages(project projectcontext.Project, enabledSkills []localskills.Skill, developerMessages []string) []model.Message {
	instructions := projectcontext.ComposeInstructions(builtInInstructions(enabledSkills), project, "")
	messages := make([]model.Message, 0, len(instructions)+len(developerMessages)+len(enabledSkills))
	for _, instruction := range instructions {
		if instruction.Source == projectcontext.InstructionSourceUser {
			for _, skill := range enabledSkills {
				messages = append(messages, model.Message{
					Role:    model.MessageRoleDeveloper,
					Content: formatSkillInstructions(skill),
				})
			}
			continue
		}
		messages = append(messages, model.Message{
			Role:    roleForInstruction(instruction.Source),
			Content: instruction.Content,
		})
		if instruction.Source == projectcontext.InstructionSourceBuiltIn {
			for _, content := range developerMessages {
				messages = append(messages, model.Message{
					Role:    model.MessageRoleDeveloper,
					Content: content,
				})
			}
		}
	}
	return messages
}

func builtInInstructions(enabledSkills []localskills.Skill) string {
	if len(enabledSkills) == 0 {
		return builtInBaseInstructions
	}

	var builder strings.Builder
	builder.WriteString(builtInBaseInstructions)
	builder.WriteString("\n\nUse only the registered skill files below for skill discovery. Do not scan the current working directory for skills:")
	for _, skill := range enabledSkills {
		builder.WriteString("\n- ")
		builder.WriteString(skill.Path)
	}
	return builder.String()
}

func chatInstructionSources(project projectcontext.Project, enabledSkills []localskills.Skill) []sessions.InstructionSource {
	sources := []sessions.InstructionSource{
		{
			Role:   model.MessageRoleSystem,
			Source: string(projectcontext.InstructionSourceBuiltIn),
		},
	}
	for _, file := range project.InstructionFiles {
		sources = append(sources, sessions.InstructionSource{
			Role:   model.MessageRoleDeveloper,
			Source: string(projectcontext.InstructionSourceProject),
			Path:   file.Path,
		})
	}
	if len(project.InstructionFiles) == 0 && project.HasInstructions {
		sources = append(sources, sessions.InstructionSource{
			Role:   model.MessageRoleDeveloper,
			Source: string(projectcontext.InstructionSourceProject),
			Path:   project.InstructionsPath,
		})
	}
	for _, skill := range enabledSkills {
		sources = append(sources, sessions.InstructionSource{
			Role:   model.MessageRoleDeveloper,
			Source: "skill",
			Path:   skill.Path,
		})
	}
	return sources
}

func formatSkillInstructions(skill localskills.Skill) string {
	return fmt.Sprintf("Skill %s (%s):\n%s", skill.ID, skill.Name, skill.Instructions)
}

func roleForInstruction(source projectcontext.InstructionSource) model.MessageRole {
	switch source {
	case projectcontext.InstructionSourceBuiltIn:
		return model.MessageRoleSystem
	case projectcontext.InstructionSourceProject:
		return model.MessageRoleDeveloper
	default:
		return model.MessageRoleUser
	}
}

func validateActiveHistoryToolExchanges(sessionID string, messages []model.Message) error {
	pendingToolCalls := map[string]struct{}{}
	for index, message := range messages {
		if message.Role == model.MessageRoleTool {
			if message.ToolCallID == "" {
				return corruptedSessionHistoryError(sessionID, "active history tool message at index %d is missing tool_call_id", index)
			}
			if _, ok := pendingToolCalls[message.ToolCallID]; !ok {
				return corruptedSessionHistoryError(sessionID, "active history tool message at index %d references unresolved tool call %q", index, message.ToolCallID)
			}
			delete(pendingToolCalls, message.ToolCallID)
			continue
		}
		if len(pendingToolCalls) > 0 {
			return corruptedSessionHistoryError(sessionID, "active history message at index %d appears before tool results for %s", index, formatPendingToolCallIDs(pendingToolCalls))
		}
		if len(message.ToolCalls) == 0 {
			continue
		}
		if message.Role != model.MessageRoleAssistant {
			return corruptedSessionHistoryError(sessionID, "active history %s message at index %d contains tool calls", message.Role, index)
		}
		for _, toolCall := range message.ToolCalls {
			if toolCall.ID == "" {
				return corruptedSessionHistoryError(sessionID, "active history assistant message at index %d contains a tool call without id", index)
			}
			if _, exists := pendingToolCalls[toolCall.ID]; exists {
				return corruptedSessionHistoryError(sessionID, "active history assistant message at index %d repeats tool call id %q", index, toolCall.ID)
			}
			pendingToolCalls[toolCall.ID] = struct{}{}
		}
	}
	if len(pendingToolCalls) > 0 {
		return corruptedSessionHistoryError(sessionID, "active history ends before tool results for %s", formatPendingToolCallIDs(pendingToolCalls))
	}
	return nil
}

func corruptedSessionHistoryError(sessionID, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	if sessionID == "" {
		return fmt.Errorf("%w: %s", sessions.ErrCorruptedSession, message)
	}
	return fmt.Errorf("%w %q: %s", sessions.ErrCorruptedSession, sessionID, message)
}

func formatPendingToolCallIDs(pending map[string]struct{}) string {
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func mcpServerIDs(servers []config.MCPServerConfig) []string {
	if len(servers) == 0 {
		return nil
	}
	ids := make([]string, 0, len(servers))
	for _, server := range servers {
		ids = append(ids, server.ID)
	}
	return ids
}

func skillIDs(skills []localskills.Skill) []string {
	if len(skills) == 0 {
		return nil
	}
	ids := make([]string, 0, len(skills))
	for _, skill := range skills {
		ids = append(ids, skill.ID)
	}
	return ids
}

func copyStringSlice(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func copyInstructionSources(values []sessions.InstructionSource) []sessions.InstructionSource {
	if values == nil {
		return nil
	}
	return append([]sessions.InstructionSource(nil), values...)
}

func copyParameterMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	copied := make(map[string]any, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func copyMessageSlice(messages []model.Message) []model.Message {
	copied := append([]model.Message(nil), messages...)
	for i := range copied {
		copied[i].ContentBlocks = append([]model.InputContentBlock(nil), messages[i].ContentBlocks...)
		for blockIndex := range copied[i].ContentBlocks {
			if copied[i].ContentBlocks[blockIndex].ImageBlob != nil {
				ref := *copied[i].ContentBlocks[blockIndex].ImageBlob
				copied[i].ContentBlocks[blockIndex].ImageBlob = &ref
			}
		}
		copied[i].ToolCalls = append([]model.ToolCall(nil), messages[i].ToolCalls...)
		copied[i].ProviderItems = copyProviderItemSlice(messages[i].ProviderItems)
		if messages[i].ResponseState != nil {
			state := *messages[i].ResponseState
			state.ReasoningItems = make([]json.RawMessage, len(messages[i].ResponseState.ReasoningItems))
			for index, item := range messages[i].ResponseState.ReasoningItems {
				state.ReasoningItems[index] = append(json.RawMessage(nil), item...)
			}
			if messages[i].ResponseState.OutputItems != nil {
				state.OutputItems = make([]json.RawMessage, len(messages[i].ResponseState.OutputItems))
				for index, item := range messages[i].ResponseState.OutputItems {
					state.OutputItems[index] = append(json.RawMessage(nil), item...)
				}
			}
			copied[i].ResponseState = &state
		}
	}
	return copied
}

func copyProviderItemSlice(items []model.ProviderItem) []model.ProviderItem {
	copied := append([]model.ProviderItem(nil), items...)
	for index := range copied {
		copied[index].Data = append(json.RawMessage(nil), items[index].Data...)
	}
	return copied
}
