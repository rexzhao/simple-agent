package execution

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/model/httpstream"
	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/sessionprojector"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

type Service struct {
	serverRoot              string
	configPath              string
	projectStore            *projectstore.Store
	sessionStore            *sessions.V2Store
	turnRunner              SessionTurnRunner
	compactPlanner          SessionCompactPlanner
	sessionWriteLockTimeout time.Duration
}

type Project struct {
	ID          string    `json:"id"`
	Root        string    `json:"root"`
	DisplayName string    `json:"display_name"`
	Archived    bool      `json:"archived"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ProjectCreateResult struct {
	Project Project `json:"project"`
	Created bool    `json:"created"`
}

type ProjectRemoveResult struct {
	Status          string `json:"status"`
	ID              string `json:"id"`
	RemovedSessions int    `json:"removed_sessions"`
}

type ProjectListOptions struct {
	Archived bool
}

type NearestProjectOptions struct {
	IncludeArchived bool
}

type SessionMetadata struct {
	ID                string    `json:"id"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	DisplayName       string    `json:"display_name"`
	Archived          bool      `json:"archived"`
	LastUsedAt        time.Time `json:"last_used_at"`
	InterruptedAt     time.Time `json:"interrupted_at,omitempty"`
	InterruptedTurnID string    `json:"interrupted_turn_id,omitempty"`
	Provider          string    `json:"provider"`
	ModelProfile      string    `json:"model_profile"`
	ModelID           string    `json:"model_id"`
	ProjectID         string    `json:"project_id"`
	CreatedCWD        string    `json:"created_cwd"`
	LastSeq           int64     `json:"last_seq"`
}

type SessionDetail struct {
	ID                string                 `json:"id"`
	CreatedAt         time.Time              `json:"created_at"`
	UpdatedAt         time.Time              `json:"updated_at"`
	DisplayName       string                 `json:"display_name"`
	Archived          bool                   `json:"archived"`
	LastUsedAt        time.Time              `json:"last_used_at"`
	InterruptedAt     time.Time              `json:"interrupted_at,omitempty"`
	InterruptedTurnID string                 `json:"interrupted_turn_id,omitempty"`
	Provider          string                 `json:"provider"`
	ModelProfile      string                 `json:"model_profile"`
	ModelID           string                 `json:"model_id"`
	Status            string                 `json:"status"`
	LastSeq           int64                  `json:"last_seq"`
	CWD               string                 `json:"cwd"`
	ProjectID         string                 `json:"project_id"`
	CreatedCWD        string                 `json:"created_cwd"`
	ConfigPath        string                 `json:"config_path"`
	ModelParameters   map[string]any         `json:"model_parameters,omitempty"`
	EnabledTools      []string               `json:"enabled_tools,omitempty"`
	EnabledMCP        []string               `json:"enabled_mcp,omitempty"`
	EnabledSkills     []string               `json:"enabled_skills,omitempty"`
	ShowReasoning     bool                   `json:"show_reasoning"`
	Context           contextwindow.Metadata `json:"context"`
	SaveToolResults   bool                   `json:"save_tool_results"`
}

type SessionCreateMetadata struct {
	CreatedCWD      string
	ConfigPath      string
	Provider        string
	ModelProfile    string
	ModelID         string
	ModelParameters map[string]any
	EnabledTools    []string
	EnabledMCP      []string
	EnabledSkills   []string
	ShowReasoning   *bool
	Context         *contextwindow.Metadata
	SaveToolResults *bool
}

type SessionListOptions struct {
	ProjectID   string
	AllProjects bool
	Archived    bool
}

type SessionRemoveResult struct {
	Status string `json:"status"`
	ID     string `json:"id"`
}

type SessionMessageResult struct {
	Status  string `json:"status"`
	TurnID  string `json:"turn_id"`
	LastSeq int64  `json:"last_seq"`
}

type SessionCompactResult struct {
	Status        string `json:"status"`
	CompactionID  string `json:"compaction_id"`
	SummaryItemID string `json:"summary_item_id"`
	LastSeq       int64  `json:"last_seq"`
}

type ServiceOptions struct {
	ConfigPath              string
	TurnRunner              SessionTurnRunner
	CompactPlanner          SessionCompactPlanner
	SessionWriteLockTimeout time.Duration
}

type SessionTurnRunner interface {
	RunSessionTurn(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error)
}

type SessionIncrementalSupporter interface {
	SupportsIncrementalSessionTurn(ctx context.Context, request SessionTurnRequest) (bool, error)
}

type SessionCompactPlanner interface {
	PlanSessionCompaction(ctx context.Context, request SessionCompactionRequest) (SessionCompactionResult, error)
}

type SessionTurnCompactionPlanner interface {
	PlanSessionTurnCompaction(ctx context.Context, request SessionTurnRequest) (SessionCompactionResult, error)
}

type SessionTurnRequest struct {
	Session      sessions.SessionV2
	SessionStore *sessions.V2Store
	TurnID       string
	Content      string
	Emit         func(model.Event)
	Publisher    eventbus.Publisher
	// ActivePromptDrain is an optional callback polled at safe checkpoints
	// during the active turn. When set, AgentTurnRunner adapts it into the
	// agent-loop active prompt drain so queued user messages are appended to the
	// active turn history within the same TurnID. A nil drain is a no-op.
	ActivePromptDrain SessionActivePromptDrain
}

// SessionActivePromptCheckpoint identifies a safe point in an active session
// turn where queued active prompts may be drained. It mirrors
// agent.ActivePromptCheckpoint in the execution domain; AgentTurnRunner adapts
// between the two.
type SessionActivePromptCheckpoint int

const (
	// SessionActivePromptCheckpointBeforeProvider is the checkpoint before the
	// first provider request of the turn.
	SessionActivePromptCheckpointBeforeProvider SessionActivePromptCheckpoint = iota
	// SessionActivePromptCheckpointAfterToolBatch is the checkpoint after a
	// complete assistant tool-call batch with every tool result durably
	// published.
	SessionActivePromptCheckpointAfterToolBatch
	// SessionActivePromptCheckpointBeforeTerminal is the checkpoint after a
	// no-tool assistant response, before terminal return.
	SessionActivePromptCheckpointBeforeTerminal
)

// SessionActivePromptDrain returns queued user messages to append to the active
// turn history at the given checkpoint. It is the execution-domain counterpart
// of agent.ActivePromptDrain.
type SessionActivePromptDrain func(SessionActivePromptCheckpoint) []model.Message

type SessionCompactionRequest struct {
	Session      sessions.SessionV2
	SessionStore *sessions.V2Store
}

type SessionTurnResult struct {
	Session       sessions.SessionV2
	Compaction    *SessionCompactionPlan
	Items         []sessions.SessionItem
	ActiveHistory []string
	Incremental   bool
}

type SessionCompactionResult struct {
	Session    sessions.SessionV2
	Compaction SessionCompactionPlan
}

type SessionCompactionPlan struct {
	SummaryItem sessions.SessionItem
	Checkpoint  sessions.CompactionCheckpoint
}

var (
	ErrSessionBusy           = errors.New("session is currently running a turn")
	ErrTurnRunnerUnavailable = errors.New("turn runner is not configured")
	ErrTurnFailed            = errors.New("turn failed")
	ErrSessionRunSettled     = errors.New("session run is no longer accepting prompts")
)

// turnFailure is the safe, stable payload for a turn.failed session stream
// event. It carries only a stable code and a short canned message selected by
// the failing stage; the underlying error text, provider body, auth data, the
// prompt and tool results stay in logs and returned errors and are never placed
// in a SessionStreamEvent.
type turnFailure struct {
	code    string
	message string
}

var (
	turnFailureCompaction     = turnFailure{code: "compaction_failed", message: "compaction planning failed"}
	turnFailureTurnInput      = turnFailure{code: "turn_input_failed", message: "could not save turn input"}
	turnFailureRunner         = turnFailure{code: "runner_failed", message: "turn runner failed"}
	turnFailureNotIncremental = turnFailure{code: "runner_not_incremental", message: "turn runner did not persist incrementally"}
	turnFailureCompletion     = turnFailure{code: "turn_completion_failed", message: "could not complete turn"}
	turnFailureSessionReload  = turnFailure{code: "session_reload_failed", message: "could not reload session"}
)

const defaultSessionWriteLockTimeout = 2 * time.Second

func NewService(home string) (*Service, error) {
	return NewServiceWithOptions(home, ServiceOptions{})
}

func NewServiceWithOptions(home string, options ServiceOptions) (*Service, error) {
	serverRoot, err := filepath.Abs(strings.TrimSpace(home))
	if err != nil {
		return nil, fmt.Errorf("resolve server root %q: %w", home, err)
	}
	serverRoot = filepath.Clean(serverRoot)
	configPath := strings.TrimSpace(options.ConfigPath)
	if configPath == "" {
		configPath = filepath.Join(serverRoot, "sai.yaml")
	} else if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(serverRoot, configPath)
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("resolve server-root config path %q: %w", options.ConfigPath, err)
	}
	configPath = filepath.Clean(configPath)
	if projectPathKey(filepath.Dir(configPath)) != projectPathKey(serverRoot) {
		return nil, fmt.Errorf("server-root config file %q must be directly inside %q", configPath, serverRoot)
	}

	projectRoot, err := projectstore.RootForHome(home)
	if err != nil {
		return nil, err
	}
	sessionRoot, err := sessions.RootForHome(home)
	if err != nil {
		return nil, err
	}
	compactPlanner := options.CompactPlanner
	if compactPlanner == nil {
		if planner, ok := options.TurnRunner.(SessionCompactPlanner); ok {
			compactPlanner = planner
		}
	}
	lockTimeout := options.SessionWriteLockTimeout
	if lockTimeout <= 0 {
		lockTimeout = defaultSessionWriteLockTimeout
	}
	return &Service{
		serverRoot:              serverRoot,
		configPath:              configPath,
		projectStore:            projectstore.NewStore(projectRoot),
		sessionStore:            sessions.NewV2Store(sessionRoot),
		turnRunner:              options.TurnRunner,
		compactPlanner:          compactPlanner,
		sessionWriteLockTimeout: lockTimeout,
	}, nil
}

func (s *Service) ServerRoot() string {
	if s == nil {
		return ""
	}
	return s.serverRoot
}

func (s *Service) ConfigPath() string {
	if s == nil {
		return ""
	}
	return s.configPath
}

func (s *Service) CreateProject(root, displayName string) (ProjectCreateResult, error) {
	if s == nil || s.projectStore == nil {
		return ProjectCreateResult{}, fmt.Errorf("execution project store is not configured")
	}
	project, created, err := s.projectStore.Create(root, displayName)
	if err != nil {
		return ProjectCreateResult{}, err
	}
	return ProjectCreateResult{Project: projectFromStore(project), Created: created}, nil
}

func (s *Service) ListProjects(options ProjectListOptions) ([]Project, error) {
	if s == nil || s.projectStore == nil {
		return nil, fmt.Errorf("execution project store is not configured")
	}
	projects, err := s.projectStore.ListWithOptions(projectstore.ListOptions{Archived: options.Archived})
	if err != nil {
		return nil, err
	}
	return projectsFromStore(projects), nil
}

func (s *Service) GetProject(id string) (Project, error) {
	if s == nil || s.projectStore == nil {
		return Project{}, fmt.Errorf("execution project store is not configured")
	}
	project, err := s.projectStore.Load(id)
	if err != nil {
		return Project{}, err
	}
	return projectFromStore(project), nil
}

func (s *Service) NearestProject(cwd string, options NearestProjectOptions) (Project, bool, error) {
	if s == nil || s.projectStore == nil {
		return Project{}, false, fmt.Errorf("execution project store is not configured")
	}
	canonicalCWD, err := projectstore.CanonicalRoot(cwd)
	if err != nil {
		return Project{}, false, err
	}
	projects, err := s.projectStore.ListWithOptions(projectstore.ListOptions{})
	if err != nil {
		return Project{}, false, err
	}
	if options.IncludeArchived {
		archived, err := s.projectStore.ListWithOptions(projectstore.ListOptions{Archived: true})
		if err != nil {
			return Project{}, false, err
		}
		projects = append(projects, archived...)
	}
	var best projectstore.Project
	bestLen := -1
	for _, project := range projects {
		if strings.TrimSpace(project.Root) == "" || (!options.IncludeArchived && project.Archived) {
			continue
		}
		if !isSameOrAncestorProjectPath(project.Root, canonicalCWD) {
			continue
		}
		rootLen := len(projectPathKey(project.Root))
		if rootLen > bestLen {
			best = project
			bestLen = rootLen
		}
	}
	if bestLen < 0 {
		return Project{}, false, nil
	}
	return projectFromStore(best), true, nil
}

func (s *Service) RenameProject(id, displayName string) (Project, error) {
	if s == nil || s.projectStore == nil {
		return Project{}, fmt.Errorf("execution project store is not configured")
	}
	project, err := s.projectStore.Load(id)
	if err != nil {
		return Project{}, err
	}
	if project.Archived {
		return Project{}, fmt.Errorf("archived project cannot be renamed")
	}
	project, err = s.projectStore.Rename(project.ID, displayName)
	if err != nil {
		return Project{}, err
	}
	return projectFromStore(project), nil
}

func (s *Service) ArchiveProject(id string) (Project, error) {
	if s == nil || s.projectStore == nil {
		return Project{}, fmt.Errorf("execution project store is not configured")
	}
	project, err := s.projectStore.Load(id)
	if err != nil {
		return Project{}, err
	}
	if project.Archived {
		return projectFromStore(project), nil
	}
	project, err = s.projectStore.Archive(project.ID)
	if err != nil {
		return Project{}, err
	}
	return projectFromStore(project), nil
}

func (s *Service) RemoveProject(id string) (ProjectRemoveResult, error) {
	if s == nil || s.projectStore == nil {
		return ProjectRemoveResult{}, fmt.Errorf("execution project store is not configured")
	}
	project, err := s.projectStore.Load(id)
	if err != nil {
		return ProjectRemoveResult{}, err
	}
	if !project.Archived {
		return ProjectRemoveResult{}, fmt.Errorf("archive project before removing it")
	}
	removedSessions, err := s.removeProjectSessions(project.ID)
	if err != nil {
		return ProjectRemoveResult{}, err
	}
	if err := s.projectStore.Delete(project.ID); err != nil {
		if errors.Is(err, projectstore.ErrNotFound) {
			return ProjectRemoveResult{}, err
		}
		return ProjectRemoveResult{}, fmt.Errorf("remove project %s: %w", project.ID, err)
	}
	return ProjectRemoveResult{Status: "removed", ID: project.ID, RemovedSessions: removedSessions}, nil
}

func (s *Service) CreateSession(projectID string, metadata SessionCreateMetadata) (SessionDetail, error) {
	project, err := s.loadActiveProject(projectID)
	if err != nil {
		return SessionDetail{}, err
	}
	if s == nil || s.sessionStore == nil {
		return SessionDetail{}, fmt.Errorf("execution session store is not configured")
	}
	session := applySessionCreateMetadata(sessions.SessionV2{}, metadata)
	session.ProjectID = project.ID
	if strings.TrimSpace(session.CreatedCWD) == "" {
		session.CreatedCWD = session.CWD
	}
	if strings.TrimSpace(session.CWD) == "" {
		session.CWD = session.CreatedCWD
	}
	saved, err := s.sessionStore.SaveMetadata(session)
	if err != nil {
		return SessionDetail{}, err
	}
	return sessionDetailFromStore(saved), nil
}

func (s *Service) ListSessions(options SessionListOptions) ([]SessionMetadata, error) {
	if s == nil || s.sessionStore == nil {
		return nil, fmt.Errorf("execution session store is not configured")
	}
	projectID := strings.TrimSpace(options.ProjectID)
	if projectID != "" {
		project, err := s.loadActiveProject(projectID)
		if err != nil {
			return nil, err
		}
		projectID = project.ID
	} else if !options.AllProjects {
		return nil, fmt.Errorf("project id is required")
	}
	infos, err := s.sessionStore.ListWithOptions(sessions.V2ListOptions{Archived: options.Archived})
	if err != nil {
		return nil, err
	}
	items := make([]SessionMetadata, 0, len(infos))
	for _, info := range infos {
		if projectID != "" && info.ProjectID != projectID {
			continue
		}
		session, err := s.sessionStore.Load(info.ID)
		if err != nil {
			return nil, err
		}
		items = append(items, sessionMetadataFromStore(session))
	}
	return items, nil
}

func (s *Service) GetSession(id string) (SessionDetail, error) {
	if s == nil || s.sessionStore == nil {
		return SessionDetail{}, fmt.Errorf("execution session store is not configured")
	}
	session, err := s.sessionStore.Load(id)
	if err != nil {
		return SessionDetail{}, err
	}
	return sessionDetailFromStore(session), nil
}

func (s *Service) RenameSession(id, displayName string) (SessionDetail, error) {
	if s == nil || s.sessionStore == nil {
		return SessionDetail{}, fmt.Errorf("execution session store is not configured")
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return SessionDetail{}, fmt.Errorf("session display name must be a non-empty string")
	}
	session, err := s.sessionStore.Load(id)
	if err != nil {
		return SessionDetail{}, err
	}
	if session.Archived {
		return SessionDetail{}, fmt.Errorf("archived session cannot be renamed")
	}
	session.DisplayName = displayName
	saved, err := s.sessionStore.SaveMetadata(session)
	if err != nil {
		return SessionDetail{}, err
	}
	return sessionDetailFromStore(saved), nil
}

func (s *Service) ArchiveSession(id string) (SessionDetail, error) {
	if s == nil || s.sessionStore == nil {
		return SessionDetail{}, fmt.Errorf("execution session store is not configured")
	}
	session, err := s.sessionStore.Load(id)
	if err != nil {
		return SessionDetail{}, err
	}
	if !session.Archived {
		session.Archived = true
		var saved sessions.SessionV2
		saved, err = s.sessionStore.SaveMetadata(session)
		if err != nil {
			return SessionDetail{}, err
		}
		session = saved
	}
	return sessionDetailFromStore(session), nil
}

func (s *Service) RemoveSession(id string) (SessionRemoveResult, error) {
	if s == nil || s.sessionStore == nil {
		return SessionRemoveResult{}, fmt.Errorf("execution session store is not configured")
	}
	session, err := s.sessionStore.Load(id)
	if err != nil {
		return SessionRemoveResult{}, err
	}
	if !session.Archived {
		return SessionRemoveResult{}, fmt.Errorf("archive session before removing it")
	}
	if err := s.sessionStore.Delete(session.ID); err != nil {
		return SessionRemoveResult{}, err
	}
	return SessionRemoveResult{Status: "removed", ID: session.ID}, nil
}

func (s *Service) SendSessionMessage(ctx context.Context, id, content string) (SessionMessageResult, error) {
	return s.SendSessionMessageWithEvents(ctx, id, content, nil)
}

func (s *Service) SendSessionMessageWithEvents(ctx context.Context, id, content string, emit func(SessionStreamEvent)) (SessionMessageResult, error) {
	run := s.StartSessionRun(ctx, id, content, emit)
	return run.Wait()
}

// SessionRunStatus is the lifecycle status of a SessionRun.
type SessionRunStatus string

const (
	SessionRunRunning   SessionRunStatus = "running"
	SessionRunCommitted SessionRunStatus = "committed"
	SessionRunFailed    SessionRunStatus = "failed"
	SessionRunCancelled SessionRunStatus = "cancelled"
)

// SessionRun is the lifecycle handle for an asynchronous session message turn
// started by StartSessionRun. It is safe for concurrent use.
type SessionRun struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu        sync.Mutex
	status    SessionRunStatus
	result    SessionMessageResult
	err       error
	accepting bool
	queue     []*PromptReceipt
}

// StartSessionRun begins running the session message orchestration for id in a
// background goroutine under a child context derived from ctx and returns
// immediately. The returned SessionRun lets the caller Wait for completion,
// query Status, or Cancel the run.
//
// Cancel is run-local and idempotent: it cancels only the child context created
// for this run, never the caller's ctx. Wait is repeatable: every call returns
// the same final result and error. Status is thread-safe: a nil error settles
// as committed, context.Canceled as cancelled, and any other error as failed.
// Cancelling after the run has settled does not change its status.
func (s *Service) StartSessionRun(ctx context.Context, id, content string, emit func(SessionStreamEvent)) *SessionRun {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	run := &SessionRun{
		cancel:    cancel,
		done:      make(chan struct{}),
		status:    SessionRunRunning,
		accepting: true,
	}
	go func() {
		defer cancel()
		defer close(run.done)
		result, err := s.runSessionMessage(runCtx, id, content, emit)
		if err == nil {
			for {
				receipt, ok := run.nextReceiptOrStop()
				if !ok {
					break
				}
				turnResult, turnErr := s.runSessionMessage(runCtx, id, receipt.event.Content, emit)
				if turnErr != nil {
					effErr := run.effectiveError(turnErr, runCtx)
					receipt.settle(turnResult, effErr)
					run.failRemaining(effErr)
					run.settle(turnResult, turnErr, runCtx)
					return
				}
				receipt.settle(turnResult, nil)
				result = turnResult
			}
		} else {
			run.failRemaining(run.effectiveError(err, runCtx))
		}
		run.settle(result, err, runCtx)
	}()
	return run
}

// Wait blocks until the run completes and returns its result and error. It is
// safe to call concurrently and repeatedly; every call returns the same values.
func (r *SessionRun) Wait() (SessionMessageResult, error) {
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.result, r.err
}

// Status returns the current lifecycle status of the run.
func (r *SessionRun) Status() SessionRunStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Cancel requests cancellation of the run. It is idempotent and run-local: it
// cancels only the child context created for this run, never the caller's ctx.
// Cancelling after the run has settled has no effect on its status.
func (r *SessionRun) Cancel() {
	if r == nil || r.cancel == nil {
		return
	}
	r.cancel()
}

// settle records the terminal result and derives the run status from the
// returned error and the child context. It is called exactly once, from the
// run goroutine, after the orchestration returns.
func (r *SessionRun) settle(result SessionMessageResult, err error, runCtx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.result = result
	if err == nil {
		r.status = SessionRunCommitted
		r.err = nil
		return
	}
	if errors.Is(err, context.Canceled) || runCtx.Err() == context.Canceled {
		r.status = SessionRunCancelled
		r.err = context.Canceled
		return
	}
	r.status = SessionRunFailed
	r.err = err
}

// Enqueue submits a validated enqueue_turn prompt to be processed as an
// independent durable session turn after the current turn completes, using the
// same emit path. It returns a per-event receipt whose Wait reports that turn's
// result or error.
//
// Events are accepted only while the run is still running and accepting. A
// structurally invalid event is rejected with ErrPromptEventInvalid. An
// append_active (or any non-enqueue_turn) mode is rejected with
// ErrPromptModeNotSupported. Once the run has drained its queue and begun
// settling, further enqueues are rejected with ErrSessionRunSettled. Accepted
// events are processed FIFO; if a turn fails or is cancelled, every
// already-accepted but unstarted receipt is settled with the same effective
// error and never silently dropped. The empty-queue terminal transition and
// Enqueue are coordinated under the run lock, so a racing enqueue is either
// accepted and processed or explicitly rejected.
func (r *SessionRun) Enqueue(event PromptEvent) (*PromptReceipt, error) {
	if r == nil {
		return nil, ErrSessionRunSettled
	}
	if err := event.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrPromptEventInvalid, err)
	}
	if event.Mode != PromptModeEnqueueTurn {
		return nil, ErrPromptModeNotSupported
	}
	r.mu.Lock()
	if !r.accepting {
		r.mu.Unlock()
		return nil, ErrSessionRunSettled
	}
	receipt := &PromptReceipt{event: event, done: make(chan struct{})}
	r.queue = append(r.queue, receipt)
	r.mu.Unlock()
	return receipt, nil
}

// nextReceiptOrStop returns the next accepted receipt to process. If the queue
// is empty it marks the run no-longer-accepting and returns ok=false so the run
// goroutine can settle. The accept/terminal decision is made under the run
// lock, so a racing Enqueue is either accepted before this call (and returned
// here) or rejected after this call marks the run terminal.
func (r *SessionRun) nextReceiptOrStop() (*PromptReceipt, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.queue) > 0 {
		receipt := r.queue[0]
		r.queue = r.queue[1:]
		return receipt, true
	}
	r.accepting = false
	return nil, false
}

// failRemaining settles every still-queued (unstarted) receipt with err and
// marks the run no-longer-accepting. It is called from the run goroutine when a
// turn fails or is cancelled, so already-accepted prompts are never silently
// dropped.
func (r *SessionRun) failRemaining(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accepting = false
	for _, receipt := range r.queue {
		receipt.settle(SessionMessageResult{}, err)
	}
	r.queue = nil
}

// effectiveError collapses a turn error to context.Canceled when the run's child
// context was cancelled, mirroring the status derivation in settle.
func (r *SessionRun) effectiveError(err error, runCtx context.Context) error {
	if errors.Is(err, context.Canceled) || runCtx.Err() == context.Canceled {
		return context.Canceled
	}
	return err
}

// PromptReceipt is the per-event handle returned by SessionRun.Enqueue. Wait
// blocks until the enqueued prompt's turn completes and returns that turn's
// result and error. It is safe to call concurrently and repeatedly.
type PromptReceipt struct {
	event PromptEvent
	done  chan struct{}

	mu     sync.Mutex
	result SessionMessageResult
	err    error
}

// Wait blocks until the enqueued prompt's turn completes and returns its result
// and error. Every call returns the same values.
func (r *PromptReceipt) Wait() (SessionMessageResult, error) {
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.result, r.err
}

// settle records the terminal result/error for the receipt and unblocks Wait.
// It is called exactly once per receipt, either after its turn completes or via
// failRemaining when an earlier turn failed.
func (r *PromptReceipt) settle(result SessionMessageResult, err error) {
	r.mu.Lock()
	r.result = result
	r.err = err
	r.mu.Unlock()
	close(r.done)
}

func (s *Service) runSessionMessage(ctx context.Context, id, content string, emit func(SessionStreamEvent)) (SessionMessageResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil || s.sessionStore == nil {
		return SessionMessageResult{}, fmt.Errorf("execution session store is not configured")
	}
	if strings.TrimSpace(content) == "" {
		return SessionMessageResult{}, fmt.Errorf("content must be a non-empty string")
	}
	if s.turnRunner == nil {
		return SessionMessageResult{}, ErrTurnRunnerUnavailable
	}
	if _, err := s.sessionStore.Load(id); err != nil {
		return SessionMessageResult{}, err
	}

	lockCtx := ctx
	cancelLock := func() {}
	if s.sessionWriteLockTimeout > 0 {
		lockCtx, cancelLock = context.WithTimeout(ctx, s.sessionWriteLockTimeout)
	}
	writeLock, err := s.sessionStore.AcquireSessionWriteLock(lockCtx, id)
	cancelLock()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return SessionMessageResult{}, ErrSessionBusy
		}
		return SessionMessageResult{}, err
	}
	defer func() {
		_ = writeLock.Release()
	}()

	session, err := s.sessionStore.Load(id)
	if err != nil {
		return SessionMessageResult{}, err
	}
	session.ConfigPath = s.ConfigPath()
	if strings.TrimSpace(session.CWD) == "" {
		session.CWD = session.CreatedCWD
	}
	if err := s.requireIncrementalSessionTurn(ctx, session, content); err != nil {
		return SessionMessageResult{}, err
	}

	turnID := nextSessionTurnID(session)
	projector, err := sessionprojector.New(s.sessionStore, session)
	if err != nil {
		return SessionMessageResult{}, fmt.Errorf("could not start session projector")
	}
	defer projector.Close()

	bus := eventbus.NewBusWithCheckpoint(projector.CheckpointHandler())
	var sink *sessionEventSink
	var submit func(SessionStreamEvent)
	if emit != nil {
		sink = newSessionEventSink(emit)
		submit = sink.submit
	}
	waitBridge := s.startSessionEventBridge(id, turnID, bus, session.LastSeq, session.ShowReasoning, submit)
	bridgeClosed := false
	closeBridge := func() {
		if bridgeClosed {
			return
		}
		bus.Close()
		waitBridge()
		bridgeClosed = true
	}

	turnStarted := false
	turnClosed := false
	var failure *turnFailure
	committed := false
	committedLastSeq := int64(0)
	interruptTurn := func() {
		if !turnStarted || turnClosed {
			return
		}
		if err := bus.Publish(eventbus.TurnInterrupted{TurnID: turnID}); err != nil {
			_, _ = s.sessionStore.MarkTurnInterrupted(id, turnID)
		}
		turnClosed = true
	}
	markFailed := func(f turnFailure) {
		interruptTurn()
		if failure == nil {
			f0 := f
			failure = &f0
		}
	}
	// finalize is the single drain point: the durable bridge is closed and joined
	// first so all mapped model/persisted events are submitted, then the terminal
	// event (turn.failed or turn.committed) is submitted last, and the sink is
	// flushed and joined before returning so no callback fires after return. The
	// turn.failed payload carries only a stable code and a short canned message
	// selected by the failing stage; it never includes the underlying error text,
	// provider body, auth data, the prompt, or tool results.
	finalize := func() {
		closeBridge()
		if failure != nil {
			submitSessionStreamEvent(submit, NewSessionStreamEvent("turn.failed", map[string]any{
				"turn_id": turnID,
				"code":    failure.code,
				"message": failure.message,
			}))
		} else if committed {
			submitSessionStreamEvent(submit, NewSessionStreamEvent("turn.committed", map[string]any{
				"turn_id":  turnID,
				"last_seq": committedLastSeq,
			}))
		}
		if sink != nil {
			sink.close()
			sink.wait()
		}
	}
	defer finalize()

	if err := bus.Publish(eventbus.TurnStarted{TurnID: turnID}); err != nil {
		return SessionMessageResult{}, fmt.Errorf("could not mark turn running: %w", err)
	}
	turnStarted = true
	submitSessionStreamEvent(submit, NewSessionStreamEvent("turn.started", map[string]any{
		"turn_id": turnID,
	}))

	session, err = s.planAutoCompaction(ctx, bus, session, turnID, content)
	if err != nil {
		markFailed(turnFailureCompaction)
		return SessionMessageResult{}, err
	}
	if err := bus.Publish(eventbus.TurnInputReady{TurnID: turnID, Message: model.Message{Role: model.MessageRoleUser, Content: content}}); err != nil {
		markFailed(turnFailureTurnInput)
		return SessionMessageResult{}, fmt.Errorf("could not save turn input")
	}

	result, err := s.turnRunner.RunSessionTurn(ctx, SessionTurnRequest{
		Session:      session,
		SessionStore: s.sessionStore,
		TurnID:       turnID,
		Content:      content,
		Emit: func(event model.Event) {
			if event != nil {
				_ = bus.Publish(eventbus.ModelEvent{Event: event})
			}
		},
		Publisher: bus,
	})
	if err != nil {
		markFailed(turnFailureForRunnerError(err))
		return SessionMessageResult{}, ErrTurnFailed
	}
	if !result.Incremental {
		markFailed(turnFailureNotIncremental)
		return SessionMessageResult{}, fmt.Errorf("turn runner did not use incremental persistence")
	}
	if err := bus.Publish(eventbus.TurnCompleted{TurnID: turnID}); err != nil {
		markFailed(turnFailureCompletion)
		return SessionMessageResult{}, fmt.Errorf("could not clear running turn: %w", err)
	}
	turnClosed = true

	saved, err := s.sessionStore.Load(id)
	if err != nil {
		// TurnCompleted already cleared the running turn durably, so only the
		// terminal stream event bookkeeping is needed here: emit exactly one
		// turn.failed last and preserve the returned public error.
		markFailed(turnFailureSessionReload)
		return SessionMessageResult{}, err
	}
	committed = true
	committedLastSeq = saved.LastSeq
	return SessionMessageResult{Status: "committed", TurnID: turnID, LastSeq: saved.LastSeq}, nil
}

func turnFailureForRunnerError(err error) turnFailure {
	var contextBudget *contextwindow.BudgetExceededError
	if errors.As(err, &contextBudget) {
		return turnFailure{
			code: "context_window_exceeded",
			message: fmt.Sprintf(
				"estimated context usage reached the model context window (%d/%d tokens)",
				contextBudget.EstimatedInputTokens,
				contextBudget.ContextWindow,
			),
		}
	}

	var requestTimeout *httpstream.RequestTimeoutError
	if errors.As(err, &requestTimeout) {
		if requestTimeout.Attempts > 1 {
			return turnFailure{
				code:    "model_request_timeout",
				message: fmt.Sprintf("model service did not return response headers after %d attempts (%s each)", requestTimeout.Attempts, requestTimeout.Timeout),
			}
		}
		return turnFailure{
			code:    "model_request_timeout",
			message: fmt.Sprintf("model service did not return response headers within %s", requestTimeout.Timeout),
		}
	}

	var streamIdleTimeout *httpstream.StreamIdleTimeoutError
	if errors.As(err, &streamIdleTimeout) {
		return turnFailure{
			code:    "model_stream_idle_timeout",
			message: fmt.Sprintf("model response stream produced no data for %s", streamIdleTimeout.Timeout),
		}
	}
	return turnFailureRunner
}

func (s *Service) requireIncrementalSessionTurn(ctx context.Context, session sessions.SessionV2, content string) error {
	supporter, ok := s.turnRunner.(SessionIncrementalSupporter)
	if !ok {
		return fmt.Errorf("turn runner does not support incremental persistence")
	}
	supported, err := supporter.SupportsIncrementalSessionTurn(ctx, SessionTurnRequest{
		Session:      session,
		SessionStore: s.sessionStore,
		Content:      content,
	})
	if err != nil {
		return ErrTurnFailed
	}
	if !supported {
		return fmt.Errorf("turn runner does not support incremental persistence")
	}
	return nil
}

func (s *Service) planAutoCompaction(ctx context.Context, bus eventbus.Publisher, session sessions.SessionV2, turnID, content string) (sessions.SessionV2, error) {
	planner, ok := s.turnRunner.(SessionTurnCompactionPlanner)
	if !ok {
		return session, nil
	}
	result, err := planner.PlanSessionTurnCompaction(ctx, SessionTurnRequest{
		Session:      session,
		SessionStore: s.sessionStore,
		TurnID:       turnID,
		Content:      content,
	})
	if err != nil {
		return sessions.SessionV2{}, ErrTurnFailed
	}
	if !sessionCompactionPlanPresent(result.Compaction) {
		return session, nil
	}
	if err := bus.Publish(eventbus.CompactionRequested{
		TurnID:     turnID,
		Summary:    result.Compaction.SummaryItem,
		Checkpoint: result.Compaction.Checkpoint,
	}); err != nil {
		return sessions.SessionV2{}, fmt.Errorf("could not compact session")
	}
	compacted, err := s.sessionStore.Load(session.ID)
	if err != nil {
		return sessions.SessionV2{}, err
	}
	if strings.TrimSpace(compacted.CWD) == "" {
		compacted.CWD = compacted.CreatedCWD
	}
	return compacted, nil
}

func (s *Service) loadActiveProject(id string) (projectstore.Project, error) {
	if s == nil || s.projectStore == nil {
		return projectstore.Project{}, fmt.Errorf("execution project store is not configured")
	}
	project, err := s.projectStore.Load(id)
	if err != nil {
		return projectstore.Project{}, err
	}
	if project.Archived {
		return projectstore.Project{}, fmt.Errorf("project is archived")
	}
	return project, nil
}

func (s *Service) removeProjectSessions(projectID string) (int, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" || s == nil || s.sessionStore == nil {
		return 0, nil
	}
	infos, err := s.sessionStore.ListWithOptions(sessions.V2ListOptions{All: true})
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, info := range infos {
		if info.ProjectID != projectID {
			continue
		}
		if err := s.sessionStore.Delete(info.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func applySessionCreateMetadata(session sessions.SessionV2, metadata SessionCreateMetadata) sessions.SessionV2 {
	if value := strings.TrimSpace(metadata.CreatedCWD); value != "" {
		session.CreatedCWD = value
		session.CWD = value
	}
	if value := strings.TrimSpace(metadata.ConfigPath); value != "" {
		session.ConfigPath = value
		session.ConfigDir = ""
	}
	if value := strings.TrimSpace(metadata.Provider); value != "" {
		session.Provider = value
	}
	if value := strings.TrimSpace(metadata.ModelProfile); value != "" {
		session.ModelProfile = value
	}
	if value := strings.TrimSpace(metadata.ModelID); value != "" {
		session.ModelID = value
	}
	if metadata.ModelParameters != nil {
		session.ModelParameters = copyMap(metadata.ModelParameters)
	}
	if metadata.EnabledTools != nil {
		session.EnabledTools = copyStrings(metadata.EnabledTools)
	}
	if metadata.EnabledMCP != nil {
		session.EnabledMCP = copyStrings(metadata.EnabledMCP)
	}
	if metadata.EnabledSkills != nil {
		session.EnabledSkills = copyStrings(metadata.EnabledSkills)
	}
	if metadata.ShowReasoning != nil {
		session.ShowReasoning = *metadata.ShowReasoning
	}
	if metadata.Context != nil {
		session.Context = *metadata.Context
	}
	if metadata.SaveToolResults != nil {
		session.SaveToolResults = *metadata.SaveToolResults
	}
	return session
}

func projectFromStore(project projectstore.Project) Project {
	return Project{
		ID:          project.ID,
		Root:        project.Root,
		DisplayName: project.DisplayName,
		Archived:    project.Archived,
		CreatedAt:   project.CreatedAt,
		UpdatedAt:   project.UpdatedAt,
	}
}

func projectsFromStore(projects []projectstore.Project) []Project {
	items := make([]Project, 0, len(projects))
	for _, project := range projects {
		items = append(items, projectFromStore(project))
	}
	return items
}

func sessionMetadataFromStore(session sessions.SessionV2) SessionMetadata {
	return SessionMetadata{
		ID:                session.ID,
		CreatedAt:         session.CreatedAt,
		UpdatedAt:         session.UpdatedAt,
		DisplayName:       session.DisplayName,
		Archived:          session.Archived,
		LastUsedAt:        session.LastUsedAt,
		InterruptedAt:     session.InterruptedAt,
		InterruptedTurnID: session.InterruptedTurnID,
		Provider:          session.Provider,
		ModelProfile:      session.ModelProfile,
		ModelID:           session.ModelID,
		ProjectID:         session.ProjectID,
		CreatedCWD:        session.CreatedCWD,
		LastSeq:           session.LastSeq,
	}
}

func sessionDetailFromStore(session sessions.SessionV2) SessionDetail {
	return SessionDetail{
		ID:                session.ID,
		CreatedAt:         session.CreatedAt,
		UpdatedAt:         session.UpdatedAt,
		DisplayName:       session.DisplayName,
		Archived:          session.Archived,
		LastUsedAt:        session.LastUsedAt,
		InterruptedAt:     session.InterruptedAt,
		InterruptedTurnID: session.InterruptedTurnID,
		Provider:          session.Provider,
		ModelProfile:      session.ModelProfile,
		ModelID:           session.ModelID,
		Status:            sessionStatus(session),
		LastSeq:           session.LastSeq,
		CWD:               session.CWD,
		ProjectID:         session.ProjectID,
		CreatedCWD:        session.CreatedCWD,
		ConfigPath:        session.RootConfigPath(),
		ModelParameters:   copyMap(session.ModelParameters),
		EnabledTools:      copyStrings(session.EnabledTools),
		EnabledMCP:        copyStrings(session.EnabledMCP),
		EnabledSkills:     copyStrings(session.EnabledSkills),
		ShowReasoning:     session.ShowReasoning,
		Context:           session.Context,
		SaveToolResults:   session.SaveToolResults,
	}
}

func sessionStatus(session sessions.SessionV2) string {
	if !session.InterruptedAt.IsZero() && (session.LastUsedAt.IsZero() || !session.LastUsedAt.After(session.InterruptedAt)) {
		return "interrupted"
	}
	return "idle"
}

func nextSessionTurnID(session sessions.SessionV2) string {
	return fmt.Sprintf("turn-%06d", session.LastSeq+1)
}

func nextSessionCompactID(session sessions.SessionV2) string {
	return fmt.Sprintf("compact-%06d", session.LastSeq+1)
}

func sessionCompactionPlanPresent(plan SessionCompactionPlan) bool {
	return strings.TrimSpace(plan.SummaryItem.ID) != "" || strings.TrimSpace(plan.Checkpoint.ID) != ""
}

func copyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	copied := make(map[string]any, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func copyStrings(values []string) []string {
	if values == nil {
		return nil
	}
	copied := make([]string, len(values))
	copy(copied, values)
	return copied
}

func isSameOrAncestorProjectPath(root, cwd string) bool {
	rootKey := projectPathKey(root)
	cwdKey := projectPathKey(cwd)
	if rootKey == cwdKey {
		return true
	}
	rel, err := filepath.Rel(rootKey, cwdKey)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func projectPathKey(path string) string {
	key := filepath.Clean(path)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key
}
