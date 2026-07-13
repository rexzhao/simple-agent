package execution

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/sessionprojector"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

type Service struct {
	projectStore            *projectstore.Store
	sessionStore            *sessions.V2Store
	turnRunner              SessionTurnRunner
	compactPlanner          SessionCompactPlanner
	sessionWriteLockTimeout time.Duration
}

type Project struct {
	ID          string
	Root        string
	DisplayName string
	Archived    bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type ProjectCreateResult struct {
	Project Project
	Created bool
}

type ProjectRemoveResult struct {
	Status          string
	ID              string
	RemovedSessions int
}

type ProjectListOptions struct {
	Archived bool
}

type NearestProjectOptions struct {
	IncludeArchived bool
}

type SessionMetadata struct {
	ID                string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DisplayName       string
	Archived          bool
	LastUsedAt        time.Time
	InterruptedAt     time.Time
	InterruptedTurnID string
	Provider          string
	ModelProfile      string
	ModelID           string
	ProjectID         string
	CreatedCWD        string
	LastSeq           int64
}

type SessionDetail struct {
	ID                string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DisplayName       string
	Archived          bool
	LastUsedAt        time.Time
	InterruptedAt     time.Time
	InterruptedTurnID string
	Provider          string
	ModelProfile      string
	ModelID           string
	Status            string
	LastSeq           int64
	CWD               string
	ProjectID         string
	CreatedCWD        string
	ConfigPath        string
	ModelParameters   map[string]any
	EnabledTools      []string
	EnabledMCP        []string
	EnabledSkills     []string
	ShowReasoning     bool
	Context           contextwindow.Metadata
	SaveToolResults   bool
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
	Status string
	ID     string
}

type SessionMessageResult struct {
	Status  string
	TurnID  string
	LastSeq int64
}

type SessionCompactResult struct {
	Status        string
	CompactionID  string
	SummaryItemID string
	LastSeq       int64
}

type ServiceOptions struct {
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
}

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
)

const defaultSessionWriteLockTimeout = 2 * time.Second

func NewService(home string) (*Service, error) {
	return NewServiceWithOptions(home, ServiceOptions{})
}

func NewServiceWithOptions(home string, options ServiceOptions) (*Service, error) {
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
		projectStore:            projectstore.NewStore(projectRoot),
		sessionStore:            sessions.NewV2Store(sessionRoot),
		turnRunner:              options.TurnRunner,
		compactPlanner:          compactPlanner,
		sessionWriteLockTimeout: lockTimeout,
	}, nil
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
	turnFailed := false
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
	markFailed := func() {
		interruptTurn()
		turnFailed = true
	}
	// finalize is the single drain point: the durable bridge is closed and joined
	// first so all mapped model/persisted events are submitted, then the terminal
	// event (turn.failed or turn.committed) is submitted last, and the sink is
	// flushed and joined before returning so no callback fires after return.
	finalize := func() {
		closeBridge()
		if turnFailed {
			submitSessionStreamEvent(submit, NewSessionStreamEvent("turn.failed", map[string]any{
				"turn_id": turnID,
				"message": "turn failed",
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
		return SessionMessageResult{}, fmt.Errorf("could not mark turn running")
	}
	turnStarted = true
	submitSessionStreamEvent(submit, NewSessionStreamEvent("turn.started", map[string]any{
		"turn_id": turnID,
	}))

	session, err = s.planAutoCompaction(ctx, bus, session, turnID, content)
	if err != nil {
		markFailed()
		return SessionMessageResult{}, err
	}
	if err := bus.Publish(eventbus.TurnInputReady{TurnID: turnID, Message: model.Message{Role: model.MessageRoleUser, Content: content}}); err != nil {
		markFailed()
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
		markFailed()
		return SessionMessageResult{}, ErrTurnFailed
	}
	if !result.Incremental {
		markFailed()
		return SessionMessageResult{}, fmt.Errorf("turn runner did not use incremental persistence")
	}
	if err := bus.Publish(eventbus.TurnCompleted{TurnID: turnID}); err != nil {
		markFailed()
		return SessionMessageResult{}, fmt.Errorf("could not clear running turn")
	}
	turnClosed = true

	saved, err := s.sessionStore.Load(id)
	if err != nil {
		return SessionMessageResult{}, err
	}
	committed = true
	committedLastSeq = saved.LastSeq
	return SessionMessageResult{Status: "committed", TurnID: turnID, LastSeq: saved.LastSeq}, nil
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
