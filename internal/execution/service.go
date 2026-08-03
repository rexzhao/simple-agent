package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/config"
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

	runCoordinatorMu sync.RWMutex
	runCoordinator   *SessionRunCoordinator
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
	ID                string                 `json:"id"`
	CreatedAt         time.Time              `json:"created_at"`
	UpdatedAt         time.Time              `json:"updated_at"`
	DisplayName       string                 `json:"display_name"`
	CreatedBy         string                 `json:"created_by"`
	ParentSessionID   string                 `json:"parent_session_id,omitempty"`
	RootSessionID     string                 `json:"root_session_id"`
	SpawnDepth        int                    `json:"spawn_depth"`
	Archived          bool                   `json:"archived"`
	LastUsedAt        time.Time              `json:"last_used_at"`
	InterruptedAt     time.Time              `json:"interrupted_at,omitempty"`
	InterruptedTurnID string                 `json:"interrupted_turn_id,omitempty"`
	Provider          string                 `json:"provider"`
	ModelProfile      string                 `json:"model_profile"`
	ModelID           string                 `json:"model_id"`
	Pricing           *config.ModelPricing   `json:"pricing,omitempty"`
	Status            string                 `json:"status"`
	ProjectID         string                 `json:"project_id"`
	CreatedCWD        string                 `json:"created_cwd"`
	LastSeq           int64                  `json:"last_seq"`
	FullAccess        bool                   `json:"full_access"`
	Debug             sessions.DebugSettings `json:"debug"`
}

type SessionDetail struct {
	ID                string                 `json:"id"`
	CreatedAt         time.Time              `json:"created_at"`
	UpdatedAt         time.Time              `json:"updated_at"`
	DisplayName       string                 `json:"display_name"`
	CreatedBy         string                 `json:"created_by"`
	ParentSessionID   string                 `json:"parent_session_id,omitempty"`
	RootSessionID     string                 `json:"root_session_id"`
	SpawnDepth        int                    `json:"spawn_depth"`
	Archived          bool                   `json:"archived"`
	LastUsedAt        time.Time              `json:"last_used_at"`
	InterruptedAt     time.Time              `json:"interrupted_at,omitempty"`
	InterruptedTurnID string                 `json:"interrupted_turn_id,omitempty"`
	Provider          string                 `json:"provider"`
	ModelProfile      string                 `json:"model_profile"`
	ModelID           string                 `json:"model_id"`
	Pricing           *config.ModelPricing   `json:"pricing,omitempty"`
	ReasoningLevel    string                 `json:"reasoning_level,omitempty"`
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
	FullAccess        bool                   `json:"full_access"`
	Context           contextwindow.Metadata `json:"context"`
	SaveToolResults   bool                   `json:"save_tool_results"`
	Debug             sessions.DebugSettings `json:"debug"`
}

// SessionSnapshot is the single-load aggregate returned by the snapshot
// endpoint. Revision is the session LastSeq formatted as a decimal string so
// the browser can compare it without losing int64 precision.
type SessionSnapshot struct {
	SessionID string           `json:"session_id"`
	Revision  string           `json:"revision"`
	Session   SessionDetail    `json:"session"`
	History   SessionItemsPage `json:"history"`
}

type SessionCreateMetadata struct {
	DisplayName     string
	ParentSessionID string
	CreatedCWD      string
	ConfigPath      string
	Provider        string
	ModelProfile    string
	ModelID         string
	Pricing         *config.ModelPricing
	ReasoningLevel  string
	ModelParameters map[string]any
	EnabledTools    []string
	EnabledMCP      []string
	EnabledSkills   []string
	ShowReasoning   *bool
	FullAccess      bool
	Context         *contextwindow.Metadata
	SaveToolResults *bool
	Debug           *sessions.DebugSettings
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

type SessionMessageInput struct {
	Content       string
	ContentBlocks []model.InputContentBlock
	// ReplayItemID starts a turn from the already-persisted active history.
	// It must identify the trailing user message; no new user item is appended.
	ReplayItemID string
	// Replay retries an interrupted turn by resending the entire persisted
	// active history (including tool results) to the model without appending
	// new input. It does not require a trailing user message.
	Replay bool
}

// Message builds the model-facing user message. When attachments exist the
// text becomes an input_text block because OpenAI Responses rejects a message
// that sets both Content and ContentBlocks.
func (input SessionMessageInput) Message() model.Message {
	if len(input.ContentBlocks) == 0 {
		return model.Message{Role: model.MessageRoleUser, Content: input.Content}
	}
	blocks := make([]model.InputContentBlock, 0, len(input.ContentBlocks)+1)
	if input.Content != "" {
		blocks = append(blocks, model.InputContentBlock{Type: "input_text", Text: input.Content})
	}
	for _, block := range input.ContentBlocks {
		copied := block
		if block.ImageBlob != nil {
			ref := *block.ImageBlob
			copied.ImageBlob = &ref
		}
		blocks = append(blocks, copied)
	}
	return model.Message{Role: model.MessageRoleUser, ContentBlocks: blocks}
}

func sessionMessageHasImage(input SessionMessageInput) bool {
	for _, block := range input.ContentBlocks {
		if block.Type == "input_image" || block.ImageURL != "" || block.ImageBlob != nil {
			return true
		}
	}
	return false
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
	Session             sessions.SessionV2
	SessionStore        *sessions.V2Store
	SessionService      *Service
	RunCoordinator      *SessionRunCoordinator
	TurnID              string
	Content             string
	ContentBlocks       []model.InputContentBlock
	ReplayHistory       bool
	Emit                func(model.Event)
	Publisher           eventbus.Publisher
	OnCompactionStarted func(trigger string)
	// ActivePromptDrain is an optional callback polled at safe checkpoints
	// during the active turn. When set, AgentTurnRunner adapts it into the
	// agent-loop active prompt drain so queued user messages are appended to the
	// active turn history within the same TurnID. A nil drain is a no-op.
	ActivePromptDrain SessionActivePromptDrain
	// ToolCancel is an optional registry that allows individual in-flight tool
	// calls to be cancelled without aborting the entire turn. When set,
	// AgentTurnRunner passes it to the agent loop so each tool call runs under
	// a cancellable child context registered by tool call ID.
	ToolCancel *agent.ToolCancellationRegistry
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
	Session        sessions.SessionV2
	SessionStore   *sessions.V2Store
	SessionService *Service
	RunCoordinator *SessionRunCoordinator
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
	Usage       *model.Usage
	Context     *contextwindow.Metadata
}

var (
	ErrSessionBusy           = errors.New("session is currently running a turn")
	ErrTurnRunnerUnavailable = errors.New("turn runner is not configured")
	ErrTurnFailed            = errors.New("turn failed")
	ErrSessionRunSettled     = errors.New("session run is no longer accepting prompts")
	// ErrSessionNotSteerable means there is no active turn whose strict steer
	// gate is still open. Unlike Web AppendActive, strict agent steer never
	// falls back to a follow-up turn.
	ErrSessionNotSteerable   = errors.New("session has no active turn accepting steer messages")
	ErrUnsupportedModelInput = errors.New("model does not support the requested input")
)

// turnFailure is the payload for a turn.failed session stream event. It
// carries a stable code and a short message selected by the failing stage.
// Provider-reported failures — HTTP statuses with response bodies and
// in-stream error events — surface the provider's own message verbatim
// (bounded): operators need it to act on rate limits, quota, and model
// errors. Everything else stays canned: internal error text, auth data,
// the prompt and tool results remain in logs and returned errors and are
// never placed in a SessionStreamEvent.
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

// SetSessionRunCoordinator installs the application-wide active-run owner used
// by both presentation adapters and session tools. Passing nil detaches it.
func (s *Service) SetSessionRunCoordinator(coordinator *SessionRunCoordinator) {
	if s == nil {
		return
	}
	s.runCoordinatorMu.Lock()
	s.runCoordinator = coordinator
	s.runCoordinatorMu.Unlock()
}

// ClearSessionRunCoordinator detaches coordinator only if it is still the
// installed instance. This prevents an older adapter from clearing a newer
// coordinator during overlapping shutdown/startup.
func (s *Service) ClearSessionRunCoordinator(coordinator *SessionRunCoordinator) {
	if s == nil {
		return
	}
	s.runCoordinatorMu.Lock()
	if s.runCoordinator == coordinator {
		s.runCoordinator = nil
	}
	s.runCoordinatorMu.Unlock()
}

func (s *Service) sessionRunCoordinator() *SessionRunCoordinator {
	if s == nil {
		return nil
	}
	s.runCoordinatorMu.RLock()
	defer s.runCoordinatorMu.RUnlock()
	return s.runCoordinator
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
	if err := s.ensureProjectSessionsIdle(project.ID); err != nil {
		return Project{}, err
	}
	project, err = s.projectStore.Archive(project.ID)
	if err != nil {
		return Project{}, err
	}
	return projectFromStore(project), nil
}

func (s *Service) RestoreProject(id string) (Project, error) {
	if s == nil || s.projectStore == nil {
		return Project{}, fmt.Errorf("execution project store is not configured")
	}
	project, err := s.projectStore.Load(id)
	if err != nil {
		return Project{}, err
	}
	if !project.Archived {
		return projectFromStore(project), nil
	}
	project, err = s.projectStore.Restore(project.ID)
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
	if err := s.ensureProjectSessionsIdle(project.ID); err != nil {
		return ProjectRemoveResult{}, err
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
	if session.ParentSessionID != "" {
		parent, err := s.sessionStore.Load(session.ParentSessionID)
		if err != nil {
			return SessionDetail{}, fmt.Errorf("load parent session %q: %w", session.ParentSessionID, err)
		}
		if parent.ProjectID != project.ID {
			return SessionDetail{}, fmt.Errorf("parent session belongs to a different project")
		}
		session.CreatedBy = sessions.SessionCreatedByAgent
		session.RootSessionID = strings.TrimSpace(parent.RootSessionID)
		if session.RootSessionID == "" {
			session.RootSessionID = parent.ID
		}
		session.SpawnDepth = parent.SpawnDepth + 1
		session.EnabledTools = enabledToolsForAgentChild(session.EnabledTools)
	} else {
		session.CreatedBy = sessions.SessionCreatedByUser
	}
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
		session = s.hydrateSessionDebug(session)
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
	session = s.hydrateSessionPricing(session)
	session = s.hydrateSessionDebug(session)
	return sessionDetailFromStore(session), nil
}

// GetSessionSnapshot loads a session once and returns its detail, history
// page, and a revision (LastSeq as a decimal string) in a single response.
// This avoids the mixed state that results from separate detail and items
// loads racing with compaction or run settlement.
func (s *Service) GetSessionSnapshot(id string) (SessionSnapshot, error) {
	if s == nil || s.sessionStore == nil {
		return SessionSnapshot{}, fmt.Errorf("execution session store is not configured")
	}
	session, err := s.sessionStore.Load(id)
	if err != nil {
		return SessionSnapshot{}, err
	}
	session = s.hydrateSessionPricing(session)
	session = s.hydrateSessionDebug(session)
	detail := sessionDetailFromStore(session)
	history, err := s.buildItemsPage(session, 0, 0, defaultSessionChatItemsLimit, true)
	if err != nil {
		return SessionSnapshot{}, err
	}
	return SessionSnapshot{
		SessionID: id,
		Revision:  strconv.FormatInt(session.LastSeq, 10),
		Session:   detail,
		History:   history,
	}, nil
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

// SetSessionFullAccess toggles the session's full access mode: with full
// access the file tools accept absolute paths outside the session workspace.
// The flag is read when a run prepares its tool registry, so a toggle takes
// effect from the next turn; an in-flight turn keeps its original mode.
func (s *Service) SetSessionFullAccess(id string, fullAccess bool) (SessionDetail, error) {
	if s == nil || s.sessionStore == nil {
		return SessionDetail{}, fmt.Errorf("execution session store is not configured")
	}
	session, err := s.sessionStore.Load(id)
	if err != nil {
		return SessionDetail{}, err
	}
	if session.Archived {
		return SessionDetail{}, fmt.Errorf("archived session cannot change full access mode")
	}
	session.FullAccess = fullAccess
	saved, err := s.sessionStore.SaveMetadata(session)
	if err != nil {
		return SessionDetail{}, err
	}
	return sessionDetailFromStore(saved), nil
}

// SetSessionDebug updates diagnostics for one conversation. The setting is
// read when a run prepares its provider, so changes take effect from the next
// turn; an in-flight turn keeps the setting it started with.
func (s *Service) SetSessionDebug(id string, debug sessions.DebugSettings) (SessionDetail, error) {
	if s == nil || s.sessionStore == nil {
		return SessionDetail{}, fmt.Errorf("execution session store is not configured")
	}
	session, err := s.sessionStore.Load(id)
	if err != nil {
		return SessionDetail{}, err
	}
	if session.Archived {
		return SessionDetail{}, fmt.Errorf("archived session cannot change debug settings")
	}
	session.Debug = debug
	session.DebugConfigured = true
	saved, err := s.sessionStore.SaveMetadata(session)
	if err != nil {
		return SessionDetail{}, err
	}
	return sessionDetailFromStore(saved), nil
}

// ArchiveSession archives the session together with every descendant session
// reachable through parent links (children, grandchildren, …). The cascade is
// all-or-nothing: the whole subtree is locked up front, so a running turn on
// any session in the subtree rejects the operation before anything changes.
func (s *Service) ArchiveSession(id string) (SessionDetail, error) {
	if s == nil || s.sessionStore == nil {
		return SessionDetail{}, fmt.Errorf("execution session store is not configured")
	}
	subtree, err := s.sessionSubtreeIDs(id)
	if err != nil {
		return SessionDetail{}, err
	}
	locks, err := s.acquireSessionMutationLocks(subtree)
	if err != nil {
		return SessionDetail{}, err
	}
	defer releaseSessionMutationLocks(locks)

	subtreeSessions, err := s.loadSubtreeSessions(subtree, id)
	if err != nil {
		return SessionDetail{}, err
	}
	for _, sessionID := range subtree {
		session, ok := subtreeSessions[sessionID]
		if !ok || session.Archived {
			continue
		}
		session.Archived = true
		saved, saveErr := s.sessionStore.SaveMetadata(session)
		if saveErr != nil {
			return SessionDetail{}, saveErr
		}
		subtreeSessions[sessionID] = saved
	}
	return sessionDetailFromStore(subtreeSessions[id]), nil
}

func (s *Service) RestoreSession(id string) (SessionDetail, error) {
	if s == nil || s.sessionStore == nil {
		return SessionDetail{}, fmt.Errorf("execution session store is not configured")
	}
	writeLock, err := s.acquireSessionMutationLock(id)
	if err != nil {
		return SessionDetail{}, err
	}
	defer func() { _ = writeLock.Release() }()

	session, err := s.sessionStore.Load(id)
	if err != nil {
		return SessionDetail{}, err
	}
	if !session.Archived {
		return sessionDetailFromStore(session), nil
	}
	if _, err := s.loadActiveProject(session.ProjectID); err != nil {
		return SessionDetail{}, err
	}
	session.Archived = false
	session.ArchivedAt = time.Time{}
	saved, err := s.sessionStore.SaveMetadata(session)
	if err != nil {
		return SessionDetail{}, err
	}
	return sessionDetailFromStore(saved), nil
}

// RemoveSession permanently deletes the session together with every
// descendant session reachable through parent links. The target must be
// archived first; still-active descendants are archived as part of the
// cascade so no run can start on them once the locks drop. The whole subtree
// is locked while validating, so a running turn anywhere in it rejects the
// removal before anything changes.
func (s *Service) RemoveSession(id string) (SessionRemoveResult, error) {
	if s == nil || s.sessionStore == nil {
		return SessionRemoveResult{}, fmt.Errorf("execution session store is not configured")
	}
	subtree, err := s.sessionSubtreeIDs(id)
	if err != nil {
		return SessionRemoveResult{}, err
	}
	locks, err := s.acquireSessionMutationLocks(subtree)
	if err != nil {
		return SessionRemoveResult{}, err
	}
	defer releaseSessionMutationLocks(locks)

	subtreeSessions, err := s.loadSubtreeSessions(subtree, id)
	if err != nil {
		return SessionRemoveResult{}, err
	}
	if !subtreeSessions[id].Archived {
		return SessionRemoveResult{}, fmt.Errorf("archive session before removing it")
	}
	deleteOrder := make([]sessions.SessionV2, 0, len(subtreeSessions))
	for _, sessionID := range subtree {
		session, ok := subtreeSessions[sessionID]
		if !ok {
			continue
		}
		if !session.Archived {
			session.Archived = true
			saved, saveErr := s.sessionStore.SaveMetadata(session)
			if saveErr != nil {
				return SessionRemoveResult{}, saveErr
			}
			session = saved
		}
		deleteOrder = append(deleteOrder, session)
	}
	// Windows cannot remove a directory while its write.lock handle is open.
	// The archived state prevents a new run from starting after this release.
	releaseSessionMutationLocks(locks)

	// Deepest sessions first: a partial failure leaves the parent in place so
	// a retry can finish the cascade instead of leaving orphaned children.
	sort.SliceStable(deleteOrder, func(i, j int) bool {
		if deleteOrder[i].SpawnDepth != deleteOrder[j].SpawnDepth {
			return deleteOrder[i].SpawnDepth > deleteOrder[j].SpawnDepth
		}
		return deleteOrder[i].ID < deleteOrder[j].ID
	})
	for _, session := range deleteOrder {
		if session.ID == id {
			continue
		}
		if err := s.sessionStore.Delete(session.ID); err != nil && !errors.Is(err, sessions.ErrNotFound) {
			return SessionRemoveResult{}, err
		}
	}
	if err := s.sessionStore.Delete(id); err != nil {
		return SessionRemoveResult{}, err
	}
	return SessionRemoveResult{Status: "removed", ID: id}, nil
}

// sessionSubtreeIDs returns the sorted ids of the subtree rooted at rootID:
// rootID itself plus every descendant reachable through parent links. The
// traversal is cycle-safe, so corrupted lineage can never send it into a
// loop, and the sorted result gives every caller the same multi-lock
// acquisition order.
func (s *Service) sessionSubtreeIDs(rootID string) ([]string, error) {
	if s == nil || s.sessionStore == nil {
		return nil, fmt.Errorf("execution session store is not configured")
	}
	infos, err := s.sessionStore.ListWithOptions(sessions.V2ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	childrenByParent := make(map[string][]string)
	for _, info := range infos {
		if parent := strings.TrimSpace(info.ParentSessionID); parent != "" {
			childrenByParent[parent] = append(childrenByParent[parent], info.ID)
		}
	}
	ids := []string{rootID}
	seen := map[string]bool{rootID: true}
	queue := []string{rootID}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range childrenByParent[parent] {
			if seen[child] {
				continue
			}
			seen[child] = true
			ids = append(ids, child)
			queue = append(queue, child)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// loadSubtreeSessions loads every session in the subtree while the caller
// holds the subtree locks and rejects the operation with ErrSessionBusy when
// any session has a running turn. requiredID identifies the operation target:
// it must exist, while a concurrently removed descendant is simply skipped.
func (s *Service) loadSubtreeSessions(subtree []string, requiredID string) (map[string]sessions.SessionV2, error) {
	loaded := make(map[string]sessions.SessionV2, len(subtree))
	for _, sessionID := range subtree {
		session, err := s.sessionStore.Load(sessionID)
		if errors.Is(err, sessions.ErrNotFound) && sessionID != requiredID {
			continue
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(session.RunningTurnID) != "" {
			return nil, ErrSessionBusy
		}
		loaded[sessionID] = session
	}
	return loaded, nil
}

func (s *Service) acquireSessionMutationLock(id string) (*sessions.SessionWriteLock, error) {
	locks, err := s.acquireSessionMutationLocks([]string{id})
	if err != nil {
		return nil, err
	}
	return locks[0], nil
}

// acquireSessionMutationLocks acquires the per-session writer locks for ids
// in deterministic sorted order so concurrent subtree operations cannot
// deadlock. A single shared timeout bounds the whole batch; on any failure
// every lock taken so far is released. context.DeadlineExceeded maps to
// ErrSessionBusy, matching the single-session mutation contract.
func (s *Service) acquireSessionMutationLocks(ids []string) ([]*sessions.SessionWriteLock, error) {
	if s == nil || s.sessionStore == nil {
		return nil, fmt.Errorf("execution session store is not configured")
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	ctx := context.Background()
	cancel := func() {}
	if s.sessionWriteLockTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, s.sessionWriteLockTimeout)
	}
	defer cancel()
	locks := make([]*sessions.SessionWriteLock, 0, len(sorted))
	for _, id := range sorted {
		lock, err := s.sessionStore.AcquireSessionWriteLock(ctx, id)
		if errors.Is(err, context.DeadlineExceeded) {
			releaseSessionMutationLocks(locks)
			return nil, ErrSessionBusy
		}
		if err != nil {
			releaseSessionMutationLocks(locks)
			return nil, err
		}
		locks = append(locks, lock)
	}
	return locks, nil
}

// releaseSessionMutationLocks releases every lock in the batch. Release is
// idempotent, so a batch may be released both explicitly and via defer.
func releaseSessionMutationLocks(locks []*sessions.SessionWriteLock) {
	for _, lock := range locks {
		_ = lock.Release()
	}
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

	// activeQueue holds user prompt contents submitted via AppendActive that
	// have not yet been sent to the model. Messages leave the queue in exactly
	// two ways, both of which send them to the server: (1) the active prompt
	// drain injects them into the in-flight turn at a safe checkpoint, or
	// (2) the run goroutine drains any remainder into a fresh follow-up turn
	// after the active turn settles. Queued messages are never dropped. Every
	// mutation publishes a full run.prompt_queue snapshot via queueNotify.
	activeQueue  []activePrompt
	activeTurnID string
	activeEmit   func(SessionStreamEvent)
	// steerAccepting is scoped to activeTurnID. The terminal checkpoint seals
	// it atomically when no already-accepted prompt remains. Web AppendActive
	// intentionally ignores this gate and keeps its no-loss follow-up policy.
	steerAccepting bool
	nextPromptID   int
	// toolCancel tracks in-flight tool calls so they can be individually
	// cancelled without aborting the entire run.
	toolCancel *agent.ToolCancellationRegistry
}

// activePrompt is one queued append-active prompt with a stable id so clients
// can remove a specific not-yet-sent message even when contents repeat.
type activePrompt struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	// Steer marks a priority prompt: steer prompts always sort ahead of plain
	// queued prompts and drain first, both into the active turn and into a
	// follow-up turn. The queue invariant (steers on top, stable order within
	// each group) is re-established after every mutation.
	Steer bool `json:"steer"`
	// strict is set for agent TrySteer prompts only: a strict prompt left
	// behind when the active turn settles is dropped, never converted into a
	// follow-up turn. Web steer prompts are not strict and stay no-loss.
	strict bool
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
	return s.StartSessionRunWithInput(ctx, id, SessionMessageInput{Content: content}, emit)
}

// StartSessionRunWithInput starts a run whose user message can include image
// input blocks as well as text. Text-only callers should use StartSessionRun.
func (s *Service) StartSessionRunWithInput(ctx context.Context, id string, input SessionMessageInput, emit func(SessionStreamEvent)) *SessionRun {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	run := &SessionRun{
		cancel:     cancel,
		done:       make(chan struct{}),
		status:     SessionRunRunning,
		accepting:  true,
		toolCancel: agent.NewToolCancellationRegistry(),
	}
	go func() {
		defer cancel()
		defer close(run.done)
		result, err := s.runSessionMessageWithActive(runCtx, run, id, input, emit)
		if err == nil {
			// After the active turn settles, send any Web no-loss appends still
			// queued via AppendActive as a follow-up turn. Strict agent steers
			// are never converted into follow-up work. Drain while the emit path
			// is still registered so the emptied snapshot is published.
			remaining := run.drainFollowUpQueue()
			run.clearActiveTurn()
			if len(remaining) > 0 {
				followInput := SessionMessageInput{Content: strings.Join(remaining, "\n\n")}
				var followResult SessionMessageResult
				followResult, err = s.runSessionMessageWithActive(runCtx, run, id, followInput, emit)
				run.clearActiveTurn()
				if err == nil {
					result = followResult
				}
			}
		} else {
			// The active turn failed or was cancelled; drop any unsent queued
			// prompts and publish an empty snapshot before clearing the emit
			// path so the queue visibly drains.
			run.drainActiveQueue()
			run.clearActiveTurn()
		}
		if err == nil {
			for {
				receipt, ok := run.nextReceiptOrStop()
				if !ok {
					break
				}
				turnResult, turnErr := s.runSessionMessage(runCtx, id, SessionMessageInput{Content: receipt.event.Content}, emit, nil)
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

// Done is closed when the run settles. Callers must use Wait to retrieve the
// stable terminal result after the channel is closed.
func (r *SessionRun) Done() <-chan struct{} {
	if r == nil || r.done == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return r.done
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

// CancelToolCall cancels a single in-flight tool call identified by its tool
// call ID without aborting the run. It returns false if no matching tool call
// is currently executing.
func (r *SessionRun) CancelToolCall(toolCallID string) bool {
	if r == nil || r.toolCancel == nil {
		return false
	}
	return r.toolCancel.Cancel(toolCallID)
}

// AppendActive queues a user prompt to be appended to the in-flight turn at
// the next safe checkpoint, or, if the turn has already settled, sent as a
// follow-up turn. Queued prompts are never dropped: they are always delivered
// to the model, either by injection into the active turn or by a follow-up
// turn. It returns ErrSessionRunSettled once the run is no longer accepting
// prompts. Every accepted append publishes a full run.prompt_queue snapshot.
func (r *SessionRun) AppendActive(content string) error {
	if r == nil {
		return ErrSessionRunSettled
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("active prompt content must be a non-empty string")
	}
	r.mu.Lock()
	if !r.accepting {
		r.mu.Unlock()
		return ErrSessionRunSettled
	}
	r.nextPromptID++
	r.activeQueue = append(r.activeQueue, activePrompt{ID: fmt.Sprintf("ap-%d", r.nextPromptID), Content: content})
	r.normalizeActiveQueueLocked()
	r.publishQueueSnapshotLocked()
	r.mu.Unlock()
	return nil
}

// TrySteer strictly appends content to the currently active turn. It is the
// agent-facing counterpart to AppendActive: once the active turn's terminal
// checkpoint seals steer acceptance, TrySteer returns
// ErrSessionNotSteerable and never converts the message into a follow-up turn.
func (r *SessionRun) TrySteer(content string) error {
	if r == nil {
		return ErrSessionNotSteerable
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("steer content must be a non-empty string")
	}
	r.mu.Lock()
	if !r.accepting || strings.TrimSpace(r.activeTurnID) == "" || !r.steerAccepting {
		r.mu.Unlock()
		return ErrSessionNotSteerable
	}
	r.nextPromptID++
	r.activeQueue = append(r.activeQueue, activePrompt{
		ID:      fmt.Sprintf("ap-%d", r.nextPromptID),
		Content: content,
		Steer:   true,
		strict:  true,
	})
	r.normalizeActiveQueueLocked()
	r.publishQueueSnapshotLocked()
	r.mu.Unlock()
	return nil
}

// SetActivePromptSteer marks or unmarks a not-yet-sent queued prompt as a
// steer prompt. Steer prompts sort ahead of plain queued prompts and drain
// first; demoting returns the prompt below any remaining steers. It reports
// whether a prompt with that id is still queued. The updated queue is
// published as a run.prompt_queue snapshot.
func (r *SessionRun) SetActivePromptSteer(promptID string, steer bool) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	index := activePromptIndex(r.activeQueue, promptID)
	if index < 0 {
		r.mu.Unlock()
		return false
	}
	if r.activeQueue[index].Steer == steer {
		r.mu.Unlock()
		return true
	}
	r.activeQueue[index].Steer = steer
	r.normalizeActiveQueueLocked()
	r.publishQueueSnapshotLocked()
	r.mu.Unlock()
	return true
}

// MoveActivePrompt shifts a not-yet-sent queued prompt by delta positions
// (negative moves up, positive moves down). The move is clamped to the
// prompt's own priority group — steer prompts stay ahead of plain queued
// prompts — so reordering can never violate the steers-on-top invariant. It
// reports whether a prompt with that id is still queued. The updated queue is
// published as a run.prompt_queue snapshot.
func (r *SessionRun) MoveActivePrompt(promptID string, delta int) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	index := activePromptIndex(r.activeQueue, promptID)
	if index < 0 {
		r.mu.Unlock()
		return false
	}
	// Group bounds: the queue is partitioned into steers (lo..hi of the steer
	// group) followed by plain queued prompts; a move clamps to whichever
	// group the prompt belongs to.
	lo, hi := 0, len(r.activeQueue)
	for i, prompt := range r.activeQueue {
		if prompt.Steer == r.activeQueue[index].Steer {
			continue
		}
		if i < index && i+1 > lo {
			lo = i + 1
		}
		if i > index && i < hi {
			hi = i
		}
	}
	target := index + delta
	if target < lo {
		target = lo
	}
	if target > hi-1 {
		target = hi - 1
	}
	if target != index {
		prompt := r.activeQueue[index]
		if target < index {
			copy(r.activeQueue[target+1:index+1], r.activeQueue[target:index])
		} else {
			copy(r.activeQueue[index:target], r.activeQueue[index+1:target+1])
		}
		r.activeQueue[target] = prompt
		r.publishQueueSnapshotLocked()
	}
	r.mu.Unlock()
	return true
}

// activePromptIndex returns the index of the queued prompt with id, or -1.
func activePromptIndex(queue []activePrompt, promptID string) int {
	for i, prompt := range queue {
		if prompt.ID == promptID {
			return i
		}
	}
	return -1
}

// normalizeActiveQueueLocked stable-partitions the queue so steer prompts sit
// ahead of plain queued prompts, preserving the relative order inside each
// group. The caller must hold r.mu.
func (r *SessionRun) normalizeActiveQueueLocked() {
	if len(r.activeQueue) < 2 {
		return
	}
	steers := make([]activePrompt, 0, len(r.activeQueue))
	queued := make([]activePrompt, 0, len(r.activeQueue))
	for _, prompt := range r.activeQueue {
		if prompt.Steer {
			steers = append(steers, prompt)
		} else {
			queued = append(queued, prompt)
		}
	}
	r.activeQueue = append(steers, queued...)
}

// RemoveActive deletes a not-yet-sent queued prompt by id and publishes the
// updated snapshot. It reports whether a prompt with that id was present; a
// missing id (already sent or never queued) is a no-op returning false. Only
// prompts still in the queue can be removed; once drained into a turn they are
// durable session input and out of scope.
func (r *SessionRun) RemoveActive(promptID string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	index := activePromptIndex(r.activeQueue, promptID)
	if index < 0 {
		r.mu.Unlock()
		return false
	}
	r.activeQueue = append(r.activeQueue[:index], r.activeQueue[index+1:]...)
	r.publishQueueSnapshotLocked()
	r.mu.Unlock()
	return true
}

// publishQueueSnapshotLocked emits a full snapshot of the current queue. The
// caller must hold r.mu; the emit happens after the lock would normally be
// released, but emitting under the lock keeps the snapshot consistent with the
// mutation and the sink serializes delivery without blocking the bus.
func (r *SessionRun) publishQueueSnapshotLocked() {
	publishPromptQueueSnapshot(r.activeEmit, r.activeTurnID, r.activeQueue)
}

// setActiveTurn registers the in-flight turn's id and emit path so AppendActive
// can publish queue snapshots tagged with the turn being appended to. It is
// called by the run goroutine before RunSessionTurn.
func (r *SessionRun) setActiveTurn(turnID string, emit func(SessionStreamEvent)) {
	r.mu.Lock()
	r.activeTurnID = turnID
	r.activeEmit = emit
	r.steerAccepting = strings.TrimSpace(turnID) != ""
	r.mu.Unlock()
}

// clearActiveTurn deregisters the in-flight turn after it settles. It does not
// touch the queue; any remainder is handled by the run goroutine.
func (r *SessionRun) clearActiveTurn() {
	r.mu.Lock()
	r.activeTurnID = ""
	r.activeEmit = nil
	r.steerAccepting = false
	r.mu.Unlock()
}

// drainActiveQueue removes and returns every queued active prompt, publishing
// an empty snapshot. The failure/cancellation path uses it to clear all
// pending Web appends and strict steers without scheduling follow-up work.
func (r *SessionRun) drainActiveQueue() []string {
	r.mu.Lock()
	if len(r.activeQueue) == 0 {
		r.mu.Unlock()
		return nil
	}
	drained := make([]string, 0, len(r.activeQueue))
	for _, prompt := range r.activeQueue {
		drained = append(drained, prompt.Content)
	}
	r.activeQueue = nil
	r.publishQueueSnapshotLocked()
	r.mu.Unlock()
	return drained
}

// drainFollowUpQueue drains only Web no-loss appends for follow-up delivery.
// Any strict steer left behind by a failed/non-conforming turn runner is
// removed but is never silently converted into a new turn.
func (r *SessionRun) drainFollowUpQueue() []string {
	r.mu.Lock()
	if len(r.activeQueue) == 0 {
		r.mu.Unlock()
		return nil
	}
	drained := make([]string, 0, len(r.activeQueue))
	for _, prompt := range r.activeQueue {
		if !prompt.strict {
			drained = append(drained, prompt.Content)
		}
	}
	r.activeQueue = nil
	r.publishQueueSnapshotLocked()
	r.mu.Unlock()
	return drained
}

// drainActiveQueueAtCheckpoint is the strict-steer linearization point. At a
// terminal checkpoint an empty queue seals the active turn before returning;
// a racing TrySteer therefore either lands in the queue and is drained into
// this turn, or observes the sealed gate and fails explicitly.
func (r *SessionRun) drainActiveQueueAtCheckpoint(checkpoint SessionActivePromptCheckpoint) []string {
	r.mu.Lock()
	if len(r.activeQueue) == 0 {
		if checkpoint == SessionActivePromptCheckpointBeforeTerminal {
			r.steerAccepting = false
		}
		r.mu.Unlock()
		return nil
	}
	drained := make([]string, 0, len(r.activeQueue))
	for _, prompt := range r.activeQueue {
		drained = append(drained, prompt.Content)
	}
	r.activeQueue = nil
	r.publishQueueSnapshotLocked()
	r.mu.Unlock()
	return drained
}

// activePromptDrain adapts the run's append-active queue into the session turn
// drain callback polled at safe checkpoints. Drained prompts become
// model.Message values with role user; the agent loop persists each with the
// shared turn id before appending it to the in-flight history.
func (r *SessionRun) activePromptDrain() SessionActivePromptDrain {
	return func(checkpoint SessionActivePromptCheckpoint) []model.Message {
		drained := r.drainActiveQueueAtCheckpoint(checkpoint)
		if len(drained) == 0 {
			return nil
		}
		messages := make([]model.Message, 0, len(drained))
		for _, content := range drained {
			messages = append(messages, model.Message{Role: model.MessageRoleUser, Content: content})
		}
		// Notify live clients that queued prompts were just injected into the
		// active turn so they can render them in place before the durable
		// history is refreshed at turn end.
		r.mu.Lock()
		turnID := r.activeTurnID
		emit := r.activeEmit
		r.mu.Unlock()
		if emit != nil {
			fields := map[string]any{"prompts": append([]string(nil), drained...)}
			if turnID != "" {
				fields["turn_id"] = turnID
			}
			emit(NewSessionStreamEvent("run.prompt_appended", fields))
		}
		return messages
	}
}

// publishPromptQueueSnapshot emits a full run.prompt_queue snapshot so clients
// can render the not-yet-sent queue. A nil emit is a no-op; prompts is the
// complete current queue (nil or empty clears it). Each prompt carries a stable
// id so clients can remove a specific not-yet-sent message.
func publishPromptQueueSnapshot(emit func(SessionStreamEvent), turnID string, prompts []activePrompt) {
	if emit == nil {
		return
	}
	list := make([]activePrompt, 0, len(prompts))
	list = append(list, prompts...)
	fields := map[string]any{"prompts": list}
	if turnID != "" {
		fields["turn_id"] = turnID
	}
	emit(NewSessionStreamEvent("run.prompt_queue", fields))
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

func (s *Service) ValidateSessionMessageInput(id string, input SessionMessageInput) error {
	if s == nil || s.sessionStore == nil {
		return fmt.Errorf("execution session store is not configured")
	}
	session, err := s.sessionStore.Load(id)
	if err != nil {
		return err
	}
	return s.validateSessionMessageInput(session, input)
}

func (s *Service) validateSessionMessageInput(session sessions.SessionV2, input SessionMessageInput) error {
	if input.Replay {
		if strings.TrimSpace(input.Content) != "" || len(input.ContentBlocks) != 0 {
			return fmt.Errorf("replay cannot include new message content")
		}
		if strings.TrimSpace(input.ReplayItemID) != "" {
			return fmt.Errorf("replay and replay_item_id are mutually exclusive")
		}
		if len(session.ActiveHistory) == 0 {
			return fmt.Errorf("replay requires existing active history")
		}
		return nil
	}
	if strings.TrimSpace(input.ReplayItemID) != "" {
		if strings.TrimSpace(input.Content) != "" || len(input.ContentBlocks) != 0 {
			return fmt.Errorf("replay cannot include new message content")
		}
		if len(session.ActiveHistory) == 0 || session.ActiveHistory[len(session.ActiveHistory)-1] != input.ReplayItemID {
			return fmt.Errorf("replay item must be the trailing active history item")
		}
		for _, item := range session.Items {
			if item.ID != input.ReplayItemID {
				continue
			}
			if item.Message == nil || item.Message.Role != model.MessageRoleUser {
				return fmt.Errorf("replay item must be a user message")
			}
			return nil
		}
		return fmt.Errorf("replay item was not found")
	}
	if err := model.ValidateImageInputBlocks(input.ContentBlocks, false); err != nil {
		return err
	}
	if !sessionMessageHasImage(input) {
		return nil
	}
	cfg, err := config.Load(s.ConfigPath())
	if err != nil {
		return err
	}
	resolved, err := cfg.ResolveModel(session.Provider, session.ModelProfile)
	if err != nil {
		return err
	}
	if !config.ModelSupportsInput(resolved.Input, "image") {
		return fmt.Errorf(
			"%w: model %q is not configured for image input",
			ErrUnsupportedModelInput,
			session.Provider+"/"+session.ModelProfile,
		)
	}
	return nil
}

// runToolCancel returns the tool cancellation registry for a run, or nil if
// the run is nil (e.g. replay/compact paths that do not need tool cancellation).
func runToolCancel(run *SessionRun) *agent.ToolCancellationRegistry {
	if run == nil {
		return nil
	}
	return run.toolCancel
}

// runSessionMessageWithActive runs one session message turn with run's
// append-active queue wired in: it registers the turn so AppendActive publishes
// turn-tagged queue snapshots, and supplies the drain that injects queued
// prompts at safe checkpoints. All other runSessionMessage callers pass a nil
// run and no drain.
func (s *Service) runSessionMessageWithActive(ctx context.Context, run *SessionRun, id string, input SessionMessageInput, emit func(SessionStreamEvent)) (SessionMessageResult, error) {
	return s.runSessionMessage(ctx, id, input, emit, run)
}

func (s *Service) runSessionMessage(ctx context.Context, id string, input SessionMessageInput, emit func(SessionStreamEvent), run *SessionRun) (SessionMessageResult, error) {
	content := input.Content
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil || s.sessionStore == nil {
		return SessionMessageResult{}, fmt.Errorf("execution session store is not configured")
	}
	if strings.TrimSpace(content) == "" && len(input.ContentBlocks) == 0 && strings.TrimSpace(input.ReplayItemID) == "" && !input.Replay {
		return SessionMessageResult{}, fmt.Errorf("message content or image attachment is required")
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
	if session.Archived {
		return SessionMessageResult{}, fmt.Errorf("archived session cannot run a turn")
	}
	session.ConfigPath = s.ConfigPath()
	if err := s.validateSessionMessageInput(session, input); err != nil {
		return SessionMessageResult{}, err
	}
	if strings.TrimSpace(session.CWD) == "" {
		session.CWD = session.CreatedCWD
	}
	if err := s.requireIncrementalSessionTurn(ctx, session, input); err != nil {
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
	var activePromptDrain SessionActivePromptDrain
	if run != nil {
		run.setActiveTurn(turnID, emit)
		activePromptDrain = run.activePromptDrain()
	}

	session, err = s.planAutoCompaction(ctx, bus, session, turnID, input, submit)
	if err != nil {
		markFailed(turnFailureCompaction)
		return SessionMessageResult{}, err
	}
	if !input.Replay && input.ReplayItemID == "" {
		if err := bus.Publish(eventbus.TurnInputReady{TurnID: turnID, Message: input.Message()}); err != nil {
			markFailed(turnFailureTurnInput)
			return SessionMessageResult{}, fmt.Errorf("could not save turn input")
		}
	}

	result, err := s.turnRunner.RunSessionTurn(ctx, SessionTurnRequest{
		Session:           session,
		SessionStore:      s.sessionStore,
		SessionService:    s,
		RunCoordinator:    s.sessionRunCoordinator(),
		TurnID:            turnID,
		Content:           content,
		ContentBlocks:     copyInputContentBlocks(input.ContentBlocks),
		ReplayHistory:     input.ReplayItemID != "" || input.Replay,
		ActivePromptDrain: activePromptDrain,
		ToolCancel:        runToolCancel(run),
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

	var statusErr *httpstream.StatusError
	if errors.As(err, &statusErr) {
		status := strings.TrimSpace(statusErr.Status)
		if status == "" {
			status = fmt.Sprintf("HTTP %d", statusErr.StatusCode)
		}
		message := fmt.Sprintf("model provider returned %s", status)
		if statusErr.Attempts > 1 {
			message = fmt.Sprintf("%s after %d attempts", message, statusErr.Attempts)
		}
		if body := strings.TrimSpace(statusErr.Body); body != "" {
			message = fmt.Sprintf("%s: %s", message, truncateTurnFailureDetail(body, turnFailureDetailLimit))
		}
		return turnFailure{code: "model_http_error", message: message}
	}

	var providerErr *model.ProviderError
	if errors.As(err, &providerErr) {
		if message := strings.TrimSpace(providerErr.Message); message != "" {
			return turnFailure{code: "model_provider_error", message: truncateTurnFailureDetail(message, turnFailureDetailLimit)}
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

	if errors.Is(err, context.DeadlineExceeded) {
		return turnFailure{code: "model_request_deadline", message: "model request exceeded its deadline"}
	}

	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return turnFailure{code: "model_connection_failed", message: "could not reach the model provider (connection failed)"}
	}

	if errors.Is(err, io.ErrUnexpectedEOF) {
		return turnFailure{code: "model_stream_interrupted", message: "model response stream ended unexpectedly"}
	}
	return turnFailureRunner
}

// turnFailureDetailLimit bounds provider error bodies so a multi-kilobyte
// HTML error page cannot flood the session stream and the conversation UI.
const turnFailureDetailLimit = 600

func truncateTurnFailureDetail(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

func (s *Service) requireIncrementalSessionTurn(ctx context.Context, session sessions.SessionV2, input SessionMessageInput) error {
	supporter, ok := s.turnRunner.(SessionIncrementalSupporter)
	if !ok {
		return fmt.Errorf("turn runner does not support incremental persistence")
	}
	supported, err := supporter.SupportsIncrementalSessionTurn(ctx, SessionTurnRequest{
		Session:        session,
		SessionStore:   s.sessionStore,
		SessionService: s,
		RunCoordinator: s.sessionRunCoordinator(),
		Content:        input.Content,
		ContentBlocks:  copyInputContentBlocks(input.ContentBlocks),
		ReplayHistory:  input.ReplayItemID != "",
	})
	if err != nil {
		return ErrTurnFailed
	}
	if !supported {
		return fmt.Errorf("turn runner does not support incremental persistence")
	}
	return nil
}

func (s *Service) planAutoCompaction(ctx context.Context, bus eventbus.Publisher, session sessions.SessionV2, turnID string, input SessionMessageInput, submit func(SessionStreamEvent)) (sessions.SessionV2, error) {
	planner, ok := s.turnRunner.(SessionTurnCompactionPlanner)
	if !ok {
		return session, nil
	}
	result, err := planner.PlanSessionTurnCompaction(ctx, SessionTurnRequest{
		Session:        session,
		SessionStore:   s.sessionStore,
		SessionService: s,
		RunCoordinator: s.sessionRunCoordinator(),
		TurnID:         turnID,
		Content:        input.Content,
		ContentBlocks:  copyInputContentBlocks(input.ContentBlocks),
		ReplayHistory:  input.ReplayItemID != "" || input.Replay,
		OnCompactionStarted: func(trigger string) {
			submitSessionStreamEvent(submit, NewSessionStreamEvent("compaction.started", map[string]any{
				"turn_id": turnID,
				"trigger": trigger,
			}))
		},
	})
	if err != nil {
		return sessions.SessionV2{}, ErrTurnFailed
	}
	if !sessionCompactionPlanPresent(result.Compaction) {
		return session, nil
	}
	if err := publishCompactionUsage(bus, result.Compaction.Usage); err != nil {
		return sessions.SessionV2{}, fmt.Errorf("could not publish compaction usage")
	}
	if err := bus.Publish(eventbus.CompactionRequested{
		TurnID:     turnID,
		Summary:    result.Compaction.SummaryItem,
		Checkpoint: result.Compaction.Checkpoint,
		Context:    result.Compaction.Context,
	}); err != nil {
		return sessions.SessionV2{}, fmt.Errorf("could not compact session")
	}
	fields := map[string]any{
		"turn_id":       turnID,
		"trigger":       "auto",
		"compaction_id": result.Compaction.Checkpoint.ID,
	}
	if result.Compaction.Context != nil {
		fields["active_context_tokens"] = result.Compaction.Context.LastInputTokens
		fields["context_window"] = result.Compaction.Context.ContextWindow
	}
	submitSessionStreamEvent(submit, NewSessionStreamEvent("compaction.completed", fields))
	compacted, err := s.sessionStore.Load(session.ID)
	if err != nil {
		return sessions.SessionV2{}, err
	}
	if strings.TrimSpace(compacted.CWD) == "" {
		compacted.CWD = compacted.CreatedCWD
	}
	return compacted, nil
}

func publishCompactionUsage(publisher eventbus.Publisher, usage *model.Usage) error {
	if publisher == nil || usage == nil {
		return nil
	}
	return publisher.Publish(eventbus.ModelEvent{Event: model.UsageEvent{Usage: *usage}})
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

func (s *Service) ensureProjectSessionsIdle(projectID string) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" || s == nil || s.sessionStore == nil {
		return nil
	}
	infos, err := s.sessionStore.ListWithOptions(sessions.V2ListOptions{All: true})
	if err != nil {
		return err
	}
	for _, info := range infos {
		if info.ProjectID != projectID {
			continue
		}
		session, err := s.sessionStore.Load(info.ID)
		if err != nil {
			return err
		}
		if strings.TrimSpace(session.RunningTurnID) != "" {
			return ErrSessionBusy
		}
	}
	return nil
}

func applySessionCreateMetadata(session sessions.SessionV2, metadata SessionCreateMetadata) sessions.SessionV2 {
	if value := strings.TrimSpace(metadata.DisplayName); value != "" {
		session.DisplayName = value
	}
	if value := strings.TrimSpace(metadata.ParentSessionID); value != "" {
		session.ParentSessionID = value
	}
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
	if metadata.Pricing != nil {
		session.Pricing = copyModelPricing(metadata.Pricing)
	}
	if value := strings.TrimSpace(metadata.ReasoningLevel); value != "" {
		session.ReasoningLevel = value
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
	session.FullAccess = metadata.FullAccess
	if metadata.Context != nil {
		session.Context = *metadata.Context
	}
	if metadata.SaveToolResults != nil {
		session.SaveToolResults = *metadata.SaveToolResults
	}
	if metadata.Debug != nil {
		session.Debug = *metadata.Debug
		session.DebugConfigured = true
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
		CreatedBy:         session.CreatedBy,
		ParentSessionID:   session.ParentSessionID,
		RootSessionID:     session.RootSessionID,
		SpawnDepth:        session.SpawnDepth,
		Archived:          session.Archived,
		LastUsedAt:        session.LastUsedAt,
		InterruptedAt:     session.InterruptedAt,
		InterruptedTurnID: session.InterruptedTurnID,
		Provider:          session.Provider,
		ModelProfile:      session.ModelProfile,
		ModelID:           session.ModelID,
		Pricing:           copyModelPricing(session.Pricing),
		Status:            sessionStatus(session),
		ProjectID:         session.ProjectID,
		CreatedCWD:        session.CreatedCWD,
		LastSeq:           session.LastSeq,
		FullAccess:        session.FullAccess,
		Debug:             session.Debug,
	}
}

// hydrateSessionPricing provides configured pricing to the UI for sessions
// created before pricing snapshots were introduced. The runtime persists the
// snapshot on the next turn; this read-side fallback makes existing sessions
// show their current configured cost immediately without mutating storage.
func (s *Service) hydrateSessionPricing(session sessions.SessionV2) sessions.SessionV2 {
	if s == nil || session.Pricing != nil {
		return session
	}
	configPath := session.RootConfigPath()
	if strings.TrimSpace(configPath) == "" {
		configPath = s.ConfigPath()
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return session
	}
	resolved, err := cfg.ResolveModel(session.Provider, session.ModelProfile)
	if err != nil {
		return session
	}
	session.Pricing = copyModelPricing(resolved.Pricing)
	return session
}

// hydrateSessionDebug exposes the legacy global request-body setting for
// sessions written before debug settings became session-scoped. The fallback
// is read-only: once the user saves a choice, DebugConfigured makes that
// choice authoritative for the session.
func (s *Service) hydrateSessionDebug(session sessions.SessionV2) sessions.SessionV2 {
	if s == nil || session.DebugConfigured {
		return session
	}
	configPath := session.RootConfigPath()
	if strings.TrimSpace(configPath) == "" {
		configPath = s.ConfigPath()
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return session
	}
	if cfg.Logging.RequestBodies {
		session.Debug.RequestBodies = true
	}
	return session
}

func sessionDetailFromStore(session sessions.SessionV2) SessionDetail {
	return SessionDetail{
		ID:                session.ID,
		CreatedAt:         session.CreatedAt,
		UpdatedAt:         session.UpdatedAt,
		DisplayName:       session.DisplayName,
		CreatedBy:         session.CreatedBy,
		ParentSessionID:   session.ParentSessionID,
		RootSessionID:     session.RootSessionID,
		SpawnDepth:        session.SpawnDepth,
		Archived:          session.Archived,
		LastUsedAt:        session.LastUsedAt,
		InterruptedAt:     session.InterruptedAt,
		InterruptedTurnID: session.InterruptedTurnID,
		Provider:          session.Provider,
		ModelProfile:      session.ModelProfile,
		ModelID:           session.ModelID,
		Pricing:           copyModelPricing(session.Pricing),
		ReasoningLevel:    session.ReasoningLevel,
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
		FullAccess:        session.FullAccess,
		Debug:             session.Debug,
		Context:           session.Context,
		SaveToolResults:   session.SaveToolResults,
	}
}

func sessionStatus(session sessions.SessionV2) string {
	if strings.TrimSpace(session.RunningTurnID) != "" {
		return "running"
	}
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

func copyInputContentBlocks(blocks []model.InputContentBlock) []model.InputContentBlock {
	if blocks == nil {
		return nil
	}
	copied := append([]model.InputContentBlock(nil), blocks...)
	for index := range copied {
		if copied[index].ImageBlob != nil {
			ref := *copied[index].ImageBlob
			copied[index].ImageBlob = &ref
		}
	}
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
