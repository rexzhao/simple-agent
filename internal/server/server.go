package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/sessionprojector"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

const DefaultListenAddress = "127.0.0.1:0"

const (
	defaultSessionItemsLimit       = 50
	maxSessionItemsLimit           = 200
	sessionItemInlineMessageBytes  = 4 * 1024
	sessionItemPreviewMessageBytes = 240
	defaultSessionItemContentBytes = 64 * 1024
	maxSessionItemContentBytes     = 1024 * 1024

	sessionItemsViewChat  = "chat"
	sessionItemsViewDebug = "debug"

	sessionStreamClientBuffer = 32
	sessionStreamWriteTimeout = 5 * time.Second
)

var sessionStreamUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
}

var errSessionStoreUnavailable = errors.New("session store is not configured")

type Options struct {
	CWD             string
	Listen          string
	Version         string
	AuthToken       string
	Now             func() time.Time
	SessionStore    *sessions.V2Store
	SessionRoot     string
	SessionDefaults sessions.SessionV2
	ProjectStore    *projectstore.Store
	ProjectRoot     string
	TurnRunner      SessionTurnRunner
	CompactPlanner  SessionCompactPlanner
}

type Process struct {
	httpServer *http.Server
	listener   net.Listener

	mu           sync.Mutex
	info         Info
	shutdownOnce sync.Once
	shutdownDone chan struct{}
	shutdownErr  error

	sessionStore    *sessions.V2Store
	sessionDefaults sessions.SessionV2
	projectStore    *projectstore.Store
	authToken       string
	streams         *sessionStreamHub
	turnRunner      SessionTurnRunner
	compactPlanner  SessionCompactPlanner
	acceptingTurns  bool
	runningTurns    map[string]runningTurn
	turnsChanged    chan struct{}
}

type runningTurn struct {
	turnID string
	cancel context.CancelFunc
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

type Info struct {
	CWD          string    `json:"-"`
	Addr         string    `json:"base_url"`
	PID          int       `json:"pid"`
	Version      string    `json:"version"`
	StartedAt    time.Time `json:"started_at"`
	ProjectCount int       `json:"project_count"`
	SessionCount int       `json:"session_count"`
	RunningTurns int       `json:"running_turns"`
}

func CheckHealth(ctx context.Context, addr string, timeout time.Duration) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return fmt.Errorf("server addr is required")
	}
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/health", nil)
	if err != nil {
		return fmt.Errorf("create health request: %w", err)
	}
	client := http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("check health at %s: %w", addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("check health at %s: status %d", addr, resp.StatusCode)
	}
	return nil
}

func Start(options Options) (*Process, error) {
	listen := strings.TrimSpace(options.Listen)
	if listen == "" {
		listen = DefaultListenAddress
	}
	if err := ValidateListenAddress(listen); err != nil {
		return nil, err
	}

	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", listen, err)
	}

	version := strings.TrimSpace(options.Version)
	if version == "" {
		version = "dev"
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}

	compactPlanner := options.CompactPlanner
	if compactPlanner == nil {
		if planner, ok := options.TurnRunner.(SessionCompactPlanner); ok {
			compactPlanner = planner
		}
	}

	process := &Process{
		listener: listener,
		info: Info{
			CWD:       options.CWD,
			Addr:      listener.Addr().String(),
			PID:       os.Getpid(),
			Version:   version,
			StartedAt: now().UTC(),
		},
		shutdownDone:    make(chan struct{}),
		sessionStore:    options.SessionStore,
		sessionDefaults: copySessionMetadata(options.SessionDefaults),
		projectStore:    options.ProjectStore,
		authToken:       strings.TrimSpace(options.AuthToken),
		streams:         newSessionStreamHub(),
		turnRunner:      options.TurnRunner,
		compactPlanner:  compactPlanner,
		acceptingTurns:  true,
		runningTurns:    make(map[string]runningTurn),
		turnsChanged:    make(chan struct{}),
	}
	if process.sessionStore == nil && strings.TrimSpace(options.SessionRoot) != "" {
		process.sessionStore = sessions.NewV2Store(options.SessionRoot)
	}
	if process.projectStore == nil && strings.TrimSpace(options.ProjectRoot) != "" {
		process.projectStore = projectstore.NewStore(options.ProjectRoot)
	}
	process.httpServer = &http.Server{
		Handler: process,
	}
	if process.sessionStore != nil {
		if _, err := process.sessionStore.MarkRunningTurnsInterrupted(); err != nil {
			_ = listener.Close()
			return nil, fmt.Errorf("recover interrupted sessions: %w", err)
		}
	}
	return process, nil
}

func ValidateListenAddress(addr string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return fmt.Errorf("listen address must be host:port: %w", err)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("listen host must be a loopback address")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return fmt.Errorf("listen port must be a number from 0 to 65535")
	}
	if isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("listen host %q is not a loopback address", host)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (p *Process) Addr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.info.Addr
}

func (p *Process) Info() Info {
	return p.snapshot()
}

func (p *Process) Serve(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	serveErr := make(chan error, 1)
	go func() {
		err := p.httpServer.Serve(p.listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := p.Shutdown(shutdownCtx)
		cancel()
		if err != nil {
			_ = p.httpServer.Close()
		}
		return errors.Join(err, <-serveErr)
	}
}

func (p *Process) Shutdown(ctx context.Context) error {
	return p.shutdown(ctx, true)
}

func (p *Process) shutdown(ctx context.Context, immediate bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.shutdownOnce.Do(func() {
		p.stopAcceptingTurns()
		if immediate {
			p.cancelRunningTurns()
		}
		p.streams.close()
		p.shutdownErr = p.httpServer.Shutdown(ctx)
		if p.shutdownErr != nil && immediate {
			if closeErr := p.httpServer.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
				p.shutdownErr = errors.Join(p.shutdownErr, closeErr)
			}
		}
		close(p.shutdownDone)
	})
	<-p.shutdownDone
	return p.shutdownErr
}

func (p *Process) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !publicEndpoint(r) && !p.requireRegistryToken(w, r) {
		return
	}
	switch r.URL.Path {
	case "/health":
		p.handleHealth(w, r)
	case "/server":
		p.handleServer(w, r)
	case "/server/shutdown":
		p.handleShutdown(w, r)
	case "/projects":
		p.handleProjects(w, r)
	case "/sessions":
		p.handleSessions(w, r)
	default:
		if strings.HasPrefix(r.URL.Path, "/projects/") {
			p.handleProjectPath(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/sessions/") {
			p.handleSessionPath(w, r)
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "path not found")
	}
}

func publicEndpoint(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/health"
}

func (p *Process) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
	})
}

func (p *Process) handleServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	info := p.snapshot()
	sessionCount, err := p.sessionCount()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not list sessions")
		return
	}
	projectCount, err := p.projectCount()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "project_store_error", "could not list projects")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"base_url":       info.Addr,
		"pid":            info.PID,
		"version":        info.Version,
		"started_at":     info.StartedAt,
		"uptime_seconds": int64(time.Since(info.StartedAt).Seconds()),
		"project_count":  projectCount,
		"session_count":  sessionCount,
		"running_turns":  info.RunningTurns,
	})
}

func (p *Process) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if !p.requireRegistryToken(w, r) {
		return
	}
	request, err := parseShutdownQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	p.stopAcceptingTurns()
	timedOut := false
	if request.Wait {
		waitCtx := context.Background()
		cancel := func() {}
		if request.Timeout > 0 {
			waitCtx, cancel = context.WithTimeout(waitCtx, request.Timeout)
		}
		err := p.waitForRunningTurns(waitCtx)
		cancel()
		if err != nil {
			timedOut = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "shutting_down",
		"wait":      request.Wait,
		"timed_out": timedOut,
	})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.shutdown(ctx, !request.Wait || timedOut)
	}()
}

func (p *Process) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p.handleSessionsList(w, r)
	default:
		writeMethodNotAllowed(w, http.MethodGet)
	}
}

func (p *Process) handleProjects(w http.ResponseWriter, r *http.Request) {
	if !p.requireRegistryToken(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		p.handleProjectsList(w, r)
	case http.MethodPost:
		p.handleProjectsCreate(w, r)
	default:
		writeMethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (p *Process) handleProjectPath(w http.ResponseWriter, r *http.Request) {
	if !p.requireRegistryToken(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/projects/")
	if strings.TrimSpace(path) == "" {
		writeError(w, http.StatusNotFound, "not_found", "path not found")
		return
	}
	parts := strings.Split(path, "/")
	projectID := parts[0]
	if strings.TrimSpace(projectID) == "" {
		writeError(w, http.StatusNotFound, "not_found", "path not found")
		return
	}
	switch {
	case len(parts) == 1:
		switch r.Method {
		case http.MethodGet:
			p.handleProjectDetail(w, r, projectID)
		case http.MethodPatch:
			p.handleProjectMetadataUpdate(w, r, projectID)
		case http.MethodDelete:
			p.handleProjectRemove(w, r, projectID)
		default:
			writeMethodNotAllowed(w, http.MethodGet, http.MethodPatch, http.MethodDelete)
			return
		}
	case len(parts) == 2 && parts[1] == "sessions":
		switch r.Method {
		case http.MethodGet:
			p.handleProjectSessionsList(w, r, projectID)
		case http.MethodPost:
			p.handleProjectSessionsCreate(w, r, projectID)
		default:
			writeMethodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
	default:
		writeError(w, http.StatusNotFound, "not_found", "path not found")
	}
}

func (p *Process) handleProjectsList(w http.ResponseWriter, r *http.Request) {
	store := p.projectStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "project_store_unavailable", "project store is not configured")
		return
	}
	query, err := parseSessionListQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	projects, err := store.ListWithOptions(projectstore.ListOptions{Archived: query.Archived})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "project_store_error", "could not list projects")
		return
	}
	items := make([]projectDTO, 0, len(projects))
	for _, project := range projects {
		items = append(items, projectDTOFromProject(project))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"projects": items,
	})
}

func (p *Process) handleProjectsCreate(w http.ResponseWriter, r *http.Request) {
	store := p.projectStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "project_store_unavailable", "project store is not configured")
		return
	}
	request, err := readProjectCreateRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	project, created, err := store.Create(request.Root, request.DisplayName)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_project_root", err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, projectDTOFromProject(project))
}

func (p *Process) handleProjectDetail(w http.ResponseWriter, r *http.Request, id string) {
	store := p.projectStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "project_store_unavailable", "project store is not configured")
		return
	}
	project, err := store.Load(id)
	if err != nil {
		if errors.Is(err, projectstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project_not_found", "project not found")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_project_id", "invalid project id")
		return
	}
	writeJSON(w, http.StatusOK, projectDTOFromProject(project))
}

func (p *Process) handleProjectMetadataUpdate(w http.ResponseWriter, r *http.Request, id string) {
	store := p.projectStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "project_store_unavailable", "project store is not configured")
		return
	}
	request, err := readProjectMetadataUpdateRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	project, err := store.Load(id)
	if err != nil {
		if errors.Is(err, projectstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project_not_found", "project not found")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_project_id", "invalid project id")
		return
	}
	if request.DisplayName != nil && project.Archived {
		writeError(w, http.StatusConflict, "project_archived", "archived project cannot be renamed")
		return
	}
	if request.Archived != nil && !*request.Archived {
		writeError(w, http.StatusBadRequest, "restore_not_supported", "project restore is not supported")
		return
	}
	if request.Archived != nil && *request.Archived {
		busy, err := p.projectHasRunningTurn(project.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session_store_error", "could not check running sessions")
			return
		}
		if busy {
			writeError(w, http.StatusConflict, "project_busy", "project has a running turn")
			return
		}
		project, err = store.Archive(project.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "project_store_error", "could not archive project")
			return
		}
	}
	if request.DisplayName != nil {
		project, err = store.Rename(project.ID, *request.DisplayName)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "project_store_error", "could not rename project")
			return
		}
	}
	writeJSON(w, http.StatusOK, projectDTOFromProject(project))
}

func (p *Process) handleProjectRemove(w http.ResponseWriter, r *http.Request, id string) {
	store := p.projectStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "project_store_unavailable", "project store is not configured")
		return
	}
	project, err := store.Load(id)
	if err != nil {
		if errors.Is(err, projectstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project_not_found", "project not found")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_project_id", "invalid project id")
		return
	}
	busy, err := p.projectHasRunningTurn(project.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not check running sessions")
		return
	}
	if busy {
		writeError(w, http.StatusConflict, "project_busy", "project has a running turn")
		return
	}
	if !project.Archived {
		writeError(w, http.StatusConflict, "project_active", "archive project before removing it")
		return
	}
	removedSessions, err := p.removeProjectSessions(project.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not remove project sessions")
		return
	}
	if err := store.Delete(project.ID); err != nil {
		if errors.Is(err, projectstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project_not_found", "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "project_store_error", "could not remove project")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "removed",
		"id":               project.ID,
		"removed_sessions": removedSessions,
	})
}

func (p *Process) handleProjectSessionsList(w http.ResponseWriter, r *http.Request, projectID string) {
	project, ok := p.loadActiveProjectForSessionPath(w, projectID)
	if !ok {
		return
	}
	query, err := parseSessionListQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	store := p.sessionStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session store is not configured")
		return
	}
	infos, err := store.ListWithOptions(sessions.V2ListOptions{Archived: query.Archived})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not list sessions")
		return
	}
	items := make([]sessionMetadataDTO, 0, len(infos))
	for _, info := range infos {
		if info.ProjectID != project.ID {
			continue
		}
		session, err := store.Load(info.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session_store_error", "could not load session metadata")
			return
		}
		items = append(items, sessionMetadataDTOFromSession(session))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": items,
	})
}

func (p *Process) handleProjectSessionsCreate(w http.ResponseWriter, r *http.Request, projectID string) {
	project, ok := p.loadActiveProjectForSessionPath(w, projectID)
	if !ok {
		return
	}
	store := p.sessionStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session store is not configured")
		return
	}
	request, err := readProjectSessionCreateRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	session := p.newSessionFromDefaults()
	session = applySessionCreateMetadata(session, request)
	session.ProjectID = project.ID
	if strings.TrimSpace(session.CreatedCWD) == "" {
		session.CreatedCWD = session.CWD
	}
	if strings.TrimSpace(session.CWD) == "" {
		session.CWD = session.CreatedCWD
	}

	saved, err := store.SaveMetadata(session)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not create session")
		return
	}
	writeJSON(w, http.StatusCreated, sessionDetailDTOFromSession(saved, "idle"))
}

func (p *Process) loadActiveProjectForSessionPath(w http.ResponseWriter, projectID string) (projectstore.Project, bool) {
	store := p.projectStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "project_store_unavailable", "project store is not configured")
		return projectstore.Project{}, false
	}
	project, err := store.Load(projectID)
	if err != nil {
		if errors.Is(err, projectstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project_not_found", "project not found")
			return projectstore.Project{}, false
		}
		writeError(w, http.StatusBadRequest, "invalid_project_id", "invalid project id")
		return projectstore.Project{}, false
	}
	if project.Archived {
		writeError(w, http.StatusConflict, "project_archived", "project is archived")
		return projectstore.Project{}, false
	}
	return project, true
}

func (p *Process) handleSessionPath(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/sessions/")
	if strings.TrimSpace(path) == "" {
		writeError(w, http.StatusNotFound, "not_found", "path not found")
		return
	}
	parts := strings.Split(path, "/")
	id := parts[0]
	if strings.TrimSpace(id) == "" {
		writeError(w, http.StatusNotFound, "not_found", "path not found")
		return
	}
	switch {
	case len(parts) == 1:
		switch r.Method {
		case http.MethodGet:
			p.handleSessionDetail(w, r, id)
		case http.MethodPatch:
			p.handleSessionMetadataUpdate(w, r, id)
		case http.MethodDelete:
			p.handleSessionRemove(w, r, id)
		default:
			writeMethodNotAllowed(w, http.MethodGet, http.MethodPatch, http.MethodDelete)
			return
		}
	case len(parts) == 2 && parts[1] == "items":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		p.handleSessionItems(w, r, id)
	case len(parts) == 3 && parts[1] == "items":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		p.handleSessionItem(w, r, id, parts[2])
	case len(parts) == 2 && parts[1] == "stream":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		p.handleSessionStream(w, r, id)
	case len(parts) == 2 && parts[1] == "messages":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, http.MethodPost)
			return
		}
		p.handleSessionMessage(w, r, id)
	case len(parts) == 3 && parts[1] == "commands" && parts[2] == "compact":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, http.MethodPost)
			return
		}
		p.handleSessionCompact(w, r, id)
	case len(parts) == 3 && parts[1] == "content":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		p.handleSessionBlobContent(w, r, id, parts[2])
	default:
		writeError(w, http.StatusNotFound, "not_found", "path not found")
	}
}

func (p *Process) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	query, err := parseSessionListQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	if !query.AllProjects {
		writeError(w, http.StatusBadRequest, "invalid_query", "all_projects=true is required")
		return
	}
	store := p.sessionStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session store is not configured")
		return
	}
	infos, err := store.ListWithOptions(sessions.V2ListOptions{Archived: query.Archived})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not list sessions")
		return
	}
	items := make([]sessionMetadataDTO, 0, len(infos))
	for _, info := range infos {
		session, err := store.Load(info.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session_store_error", "could not load session metadata")
			return
		}
		items = append(items, sessionMetadataDTOFromSession(session))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": items,
	})
}

func (p *Process) handleSessionDetail(w http.ResponseWriter, r *http.Request, id string) {
	store := p.sessionStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session store is not configured")
		return
	}
	if !validSessionAPIID(id) {
		writeError(w, http.StatusBadRequest, "invalid_session_id", "invalid session id")
		return
	}
	session, err := store.Load(id)
	if err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			writeError(w, http.StatusNotFound, "session_not_found", "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not load session metadata")
		return
	}
	writeJSON(w, http.StatusOK, sessionDetailDTOFromSession(session, p.sessionStatus(session)))
}

func (p *Process) handleSessionMetadataUpdate(w http.ResponseWriter, r *http.Request, id string) {
	if !validSessionAPIID(id) {
		writeError(w, http.StatusBadRequest, "invalid_session_id", "invalid session id")
		return
	}
	request, err := readSessionMetadataUpdateRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if p.sessionIsRunning(id) {
		writeError(w, http.StatusConflict, "session_busy", "session is currently running a turn")
		return
	}
	store := p.sessionStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session store is not configured")
		return
	}
	session, err := store.Load(id)
	if err != nil {
		p.writeSessionLoadError(w, err, "could not load session")
		return
	}
	if request.DisplayName != nil && session.Archived {
		writeError(w, http.StatusConflict, "session_archived", "archived session cannot be renamed")
		return
	}
	if request.Archived != nil && !*request.Archived {
		writeError(w, http.StatusBadRequest, "restore_not_supported", "session restore is not supported")
		return
	}
	session = applySessionMetadataUpdate(session, request)
	saved, err := store.SaveMetadata(session)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not update session metadata")
		return
	}
	writeJSON(w, http.StatusOK, sessionDetailDTOFromSession(saved, p.sessionStatus(saved)))
}

func (p *Process) handleSessionRemove(w http.ResponseWriter, r *http.Request, id string) {
	if !validSessionAPIID(id) {
		writeError(w, http.StatusBadRequest, "invalid_session_id", "invalid session id")
		return
	}
	if p.sessionIsRunning(id) {
		writeError(w, http.StatusConflict, "session_busy", "session is currently running a turn")
		return
	}
	store := p.sessionStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session store is not configured")
		return
	}
	session, err := store.Load(id)
	if err != nil {
		p.writeSessionLoadError(w, err, "could not load session")
		return
	}
	if !session.Archived {
		writeError(w, http.StatusConflict, "session_active", "archive session before removing it")
		return
	}
	if err := store.Delete(id); err != nil {
		p.writeSessionLoadError(w, err, "could not remove session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "removed",
		"id":     id,
	})
}

func (p *Process) handleSessionItems(w http.ResponseWriter, r *http.Request, id string) {
	if !validSessionAPIID(id) {
		writeError(w, http.StatusBadRequest, "invalid_session_id", "invalid session id")
		return
	}
	query, err := parseSessionItemsQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	if query.View == sessionItemsViewDebug && !p.requireRegistryToken(w, r) {
		return
	}
	store := p.sessionStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session store is not configured")
		return
	}
	session, err := store.Load(id)
	if err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			writeError(w, http.StatusNotFound, "session_not_found", "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not load session items")
		return
	}

	filtered := filterSessionItemsForView(session.Items, query.View)
	page, hasMoreBefore, hasMoreAfter := paginateSessionItems(filtered, query)
	items := make([]sessionItemDTO, 0, len(page))
	for _, item := range page {
		items = append(items, sessionItemDTOFromSessionItem(item))
	}
	oldestSeq, newestSeq := sessionItemPageSeqBounds(page)
	writeJSON(w, http.StatusOK, sessionItemsResponseDTO{
		Items:         items,
		OldestSeq:     oldestSeq,
		NewestSeq:     newestSeq,
		HasMoreBefore: hasMoreBefore,
		HasMoreAfter:  hasMoreAfter,
	})
}

func (p *Process) handleSessionItem(w http.ResponseWriter, r *http.Request, id, itemID string) {
	if !validSessionAPIID(id) {
		writeError(w, http.StatusBadRequest, "invalid_session_id", "invalid session id")
		return
	}
	if !validSessionItemAPIID(itemID) {
		writeError(w, http.StatusBadRequest, "invalid_item_id", "invalid item id")
		return
	}
	query, err := parseSessionItemQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	if query.View == sessionItemsViewDebug && !p.requireRegistryToken(w, r) {
		return
	}
	store := p.sessionStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session store is not configured")
		return
	}
	session, err := store.Load(id)
	if err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			writeError(w, http.StatusNotFound, "session_not_found", "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not load session item")
		return
	}
	item, ok := findSessionItemByID(session.Items, itemID)
	if !ok || !sessionItemVisibleInView(item, query.View) {
		writeError(w, http.StatusNotFound, "item_not_found", "session item not found")
		return
	}
	item, err = resolveSessionItemInlineContent(store, item)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not load session item")
		return
	}
	writeJSON(w, http.StatusOK, sessionItemRefetchDTOFromSessionItem(item, query.View == sessionItemsViewDebug))
}

func (p *Process) handleSessionBlobContent(w http.ResponseWriter, r *http.Request, id, hash string) {
	query, err := parseSessionItemContentQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	if query.View == sessionItemsViewDebug && !p.requireRegistryToken(w, r) {
		return
	}

	store := p.sessionStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session store is not configured")
		return
	}
	if !validSessionAPIID(id) {
		writeError(w, http.StatusBadRequest, "invalid_session_id", "invalid session id")
		return
	}
	hash = strings.TrimSpace(hash)
	if !validBlobHash(hash) {
		writeError(w, http.StatusBadRequest, "invalid_blob_hash", "invalid blob hash")
		return
	}
	hash = strings.ToLower(hash)

	session, err := store.Load(id)
	if err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			writeError(w, http.StatusNotFound, "session_not_found", "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not load session blob content")
		return
	}
	ref, ok := findReachableSessionBlobRef(session.Items, hash, query.View)
	if !ok {
		writeError(w, http.StatusNotFound, "content_unavailable", "blob content is not available")
		return
	}

	rawContent, err := store.ReadBlob(ref)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not load session blob content")
		return
	}
	content, offset, sizeBytes, bytesReturned, hasMore := sessionContentBytesRange(rawContent, query.Offset, query.MaxBytes)
	writeJSON(w, http.StatusOK, sessionBlobContentResponseDTO{
		BlobHash:      ref.Hash,
		Content:       content,
		Offset:        offset,
		SizeBytes:     sizeBytes,
		BytesReturned: bytesReturned,
		HasMore:       hasMore,
		Encoding:      ref.Encoding,
		MediaType:     ref.MediaType,
	})
}

func (p *Process) handleSessionMessage(w http.ResponseWriter, r *http.Request, id string) {
	if !p.requireRegistryToken(w, r) {
		return
	}
	if !validSessionAPIID(id) {
		writeError(w, http.StatusBadRequest, "invalid_session_id", "invalid session id")
		return
	}
	content, err := readSessionMessageRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if p.turnRunner == nil {
		writeError(w, http.StatusServiceUnavailable, "turn_runner_unavailable", "turn runner is not configured")
		return
	}

	store := p.sessionStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session store is not configured")
		return
	}
	session, err := store.Load(id)
	if err != nil {
		p.writeSessionLoadError(w, err, "could not load session")
		return
	}
	if strings.TrimSpace(session.CWD) == "" {
		session.CWD = p.snapshot().CWD
	}

	turnID := nextSessionTurnID(session)
	turnCtx, cancelTurn := context.WithCancel(r.Context())
	beginResult := p.beginSessionTurn(id, turnID, cancelTurn)
	if beginResult == beginTurnBusy {
		cancelTurn()
		writeError(w, http.StatusConflict, "session_busy", "session is currently running a turn")
		return
	}
	if beginResult == beginTurnShuttingDown {
		cancelTurn()
		writeError(w, http.StatusServiceUnavailable, "server_shutting_down", "server is shutting down")
		return
	}
	incremental, err := p.supportsIncrementalSessionTurn(turnCtx, SessionTurnRequest{
		Session:      session,
		SessionStore: store,
		Content:      content,
	})
	if err != nil {
		p.endSessionTurn(id)
		cancelTurn()
		p.writeTurnError(w, err)
		return
	}
	if incremental {
		defer func() {
			p.endSessionTurn(id)
			cancelTurn()
		}()
		p.handleIncrementalSessionMessage(w, id, turnID, turnCtx, session, store, content)
		return
	}
	marked, err := store.MarkTurnRunning(id, turnID)
	if err != nil {
		p.endSessionTurn(id)
		cancelTurn()
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not mark turn running")
		return
	}
	session.RunningTurnID = marked.RunningTurnID
	session.RunningStartedAt = marked.RunningStartedAt
	defer func() {
		p.endSessionTurn(id)
		cancelTurn()
	}()

	p.publishSessionEvent(id, NewSessionStreamEvent("turn.started", map[string]any{
		"turn_id": turnID,
	}))
	result, err := p.turnRunner.RunSessionTurn(turnCtx, SessionTurnRequest{
		Session:      session,
		SessionStore: store,
		Content:      content,
		Emit: func(event model.Event) {
			p.publishModelTurnEvent(id, turnID, event)
		},
	})
	if err != nil {
		p.finishDurableTurn(id, turnID, errors.Is(err, context.Canceled))
		p.publishTurnFailed(id, turnID, err)
		p.writeTurnError(w, err)
		return
	}

	if strings.TrimSpace(result.Session.ID) == "" {
		result.Session = session
	} else {
		result.Session.ID = session.ID
	}
	for i := range result.Items {
		if result.Items[i].TurnID == "" {
			result.Items[i].TurnID = turnID
		}
	}
	result.Session.RunningTurnID = turnID
	result.Session.RunningStartedAt = session.RunningStartedAt
	var saved sessions.SessionV2
	if result.Compaction != nil {
		saved, err = store.SaveCompactedTurn(result.Session, result.Compaction.SummaryItem, result.Compaction.Checkpoint, result.Items, result.ActiveHistory)
	} else {
		saved, err = store.SaveTurn(result.Session, result.Items, result.ActiveHistory)
	}
	if err != nil {
		p.finishDurableTurn(id, turnID, false)
		p.publishTurnFailed(id, turnID, err)
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not save turn")
		return
	}
	if _, err := store.ClearRunningTurn(id, turnID); err != nil {
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not clear running turn")
		return
	}
	appendedItems := result.Items
	if result.Compaction != nil {
		appendedItems = append([]sessions.SessionItem{result.Compaction.SummaryItem}, appendedItems...)
	}
	for _, item := range savedSessionItemsByID(saved.Items, appendedItems) {
		p.publishSessionEvent(id, NewSessionStreamEvent("item.appended", map[string]any{
			"seq":     item.Seq,
			"item_id": item.ID,
		}))
	}
	if result.Compaction != nil {
		p.publishSessionEvent(id, NewSessionStreamEvent("compaction.created", map[string]any{
			"compaction_id": result.Compaction.Checkpoint.ID,
		}))
	}
	p.publishSessionEvent(id, NewSessionStreamEvent("turn.committed", map[string]any{
		"turn_id":  turnID,
		"last_seq": saved.LastSeq,
	}))
	writeJSON(w, http.StatusOK, map[string]any{
		"turn_id":  turnID,
		"last_seq": saved.LastSeq,
		"status":   "committed",
	})
}

func (p *Process) supportsIncrementalSessionTurn(ctx context.Context, request SessionTurnRequest) (bool, error) {
	supporter, ok := p.turnRunner.(SessionIncrementalSupporter)
	if !ok {
		return false, nil
	}
	return supporter.SupportsIncrementalSessionTurn(ctx, request)
}

func (p *Process) handleIncrementalSessionMessage(w http.ResponseWriter, id, turnID string, turnCtx context.Context, session sessions.SessionV2, store *sessions.V2Store, content string) {
	projector, err := sessionprojector.New(store, session)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not start session projector")
		return
	}
	defer projector.Close()

	bus := eventbus.NewBusWithCheckpoint(projector.CheckpointHandler())
	waitBridge := p.startSessionEventBusBridge(id, turnID, bus, session.LastSeq)
	bridgeClosed := false
	closeBridge := func() {
		if bridgeClosed {
			return
		}
		bus.Close()
		waitBridge()
		bridgeClosed = true
	}
	defer closeBridge()

	turnStarted := false
	turnClosed := false
	interruptTurn := func() {
		if !turnStarted || turnClosed {
			return
		}
		if err := bus.Publish(eventbus.TurnInterrupted{TurnID: turnID}); err != nil {
			_, _ = store.MarkTurnInterrupted(id, turnID)
		}
		turnClosed = true
	}
	defer interruptTurn()

	if err := bus.Publish(eventbus.TurnStarted{TurnID: turnID}); err != nil {
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not mark turn running")
		return
	}
	turnStarted = true
	p.publishSessionEvent(id, NewSessionStreamEvent("turn.started", map[string]any{
		"turn_id": turnID,
	}))
	if err := bus.Publish(eventbus.TurnInputReady{TurnID: turnID, Message: model.Message{Role: model.MessageRoleUser, Content: content}}); err != nil {
		interruptTurn()
		closeBridge()
		p.publishTurnFailed(id, turnID, err)
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not save turn input")
		return
	}

	result, err := p.turnRunner.RunSessionTurn(turnCtx, SessionTurnRequest{
		Session:      session,
		SessionStore: store,
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
		interruptTurn()
		closeBridge()
		p.publishTurnFailed(id, turnID, err)
		p.writeTurnError(w, err)
		return
	}
	if !result.Incremental {
		err := fmt.Errorf("turn runner did not use incremental persistence")
		interruptTurn()
		closeBridge()
		p.publishTurnFailed(id, turnID, err)
		writeError(w, http.StatusInternalServerError, "turn_runner_error", "turn runner did not use incremental persistence")
		return
	}
	if err := bus.Publish(eventbus.TurnCompleted{TurnID: turnID}); err != nil {
		interruptTurn()
		closeBridge()
		p.publishTurnFailed(id, turnID, err)
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not clear running turn")
		return
	}
	turnClosed = true
	closeBridge()

	saved, err := store.Load(id)
	if err != nil {
		p.writeSessionLoadError(w, err, "could not load committed session")
		return
	}
	p.publishSessionEvent(id, NewSessionStreamEvent("turn.committed", map[string]any{
		"turn_id":  turnID,
		"last_seq": saved.LastSeq,
	}))
	writeJSON(w, http.StatusOK, map[string]any{
		"turn_id":  turnID,
		"last_seq": saved.LastSeq,
		"status":   "committed",
	})
}

func (p *Process) handleSessionCompact(w http.ResponseWriter, r *http.Request, id string) {
	if !p.requireRegistryToken(w, r) {
		return
	}
	if !validSessionAPIID(id) {
		writeError(w, http.StatusBadRequest, "invalid_session_id", "invalid session id")
		return
	}
	if err := readEmptyObjectRequest(w, r); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	store := p.sessionStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session store is not configured")
		return
	}
	session, err := store.Load(id)
	if err != nil {
		p.writeSessionLoadError(w, err, "could not load session")
		return
	}
	if strings.TrimSpace(session.CWD) == "" {
		session.CWD = p.snapshot().CWD
	}

	operationID := nextSessionCompactOperationID(session)
	operationCtx, cancelOperation := context.WithCancel(r.Context())
	beginResult := p.beginSessionTurn(id, operationID, cancelOperation)
	if beginResult == beginTurnBusy {
		cancelOperation()
		writeError(w, http.StatusConflict, "session_busy", "session is currently running a turn")
		return
	}
	if beginResult == beginTurnShuttingDown {
		cancelOperation()
		writeError(w, http.StatusServiceUnavailable, "server_shutting_down", "server is shutting down")
		return
	}
	marked, err := store.MarkTurnRunning(id, operationID)
	if err != nil {
		p.endSessionTurn(id)
		cancelOperation()
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not mark turn running")
		return
	}
	session.RunningTurnID = marked.RunningTurnID
	session.RunningStartedAt = marked.RunningStartedAt
	defer func() {
		p.endSessionTurn(id)
		cancelOperation()
	}()
	if p.compactPlanner == nil {
		p.finishDurableTurn(id, operationID, false)
		writeError(w, http.StatusServiceUnavailable, "compact_planner_unavailable", "compact planner is not configured")
		return
	}

	p.publishSessionEvent(id, NewSessionStreamEvent("compact.started", map[string]any{
		"reason": "user_requested",
	}))
	result, err := p.compactPlanner.PlanSessionCompaction(operationCtx, SessionCompactionRequest{
		Session:      session,
		SessionStore: store,
	})
	if err != nil {
		p.finishDurableTurn(id, operationID, errors.Is(err, context.Canceled))
		p.publishCompactFailed(id, err)
		p.writeCompactError(w, err)
		return
	}

	saved, err := store.AppendCompactionCheckpoint(session.ID, result.Compaction.SummaryItem, result.Compaction.Checkpoint)
	if err != nil {
		p.finishDurableTurn(id, operationID, false)
		p.publishCompactFailed(id, err)
		p.writeCompactionStoreError(w, err)
		return
	}
	if _, err := store.ClearRunningTurn(id, operationID); err != nil {
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not clear running turn")
		return
	}
	savedSummary, _ := findSessionItemByID(saved.Items, result.Compaction.SummaryItem.ID)
	if savedSummary.ID == "" {
		savedSummary = result.Compaction.SummaryItem
	}
	p.publishSessionEvent(id, NewSessionStreamEvent("item.appended", map[string]any{
		"seq":     savedSummary.Seq,
		"item_id": result.Compaction.SummaryItem.ID,
	}))
	p.publishSessionEvent(id, NewSessionStreamEvent("compaction.created", map[string]any{
		"seq":           compactionCreatedSeq(savedSummary.Seq),
		"compaction_id": result.Compaction.Checkpoint.ID,
	}))
	p.publishSessionEvent(id, NewSessionStreamEvent("active_history.replaced", map[string]any{
		"seq": activeHistoryReplacedSeq(saved.LastSeq),
	}))
	p.publishSessionEvent(id, NewSessionStreamEvent("compact.completed", map[string]any{
		"compaction_id":   result.Compaction.Checkpoint.ID,
		"summary_item_id": result.Compaction.SummaryItem.ID,
		"last_seq":        saved.LastSeq,
	}))
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "committed",
		"compaction_id":   result.Compaction.Checkpoint.ID,
		"summary_item_id": result.Compaction.SummaryItem.ID,
		"last_seq":        saved.LastSeq,
	})
}

func (p *Process) handleSessionStream(w http.ResponseWriter, r *http.Request, id string) {
	if !validSessionAPIID(id) {
		writeError(w, http.StatusBadRequest, "invalid_session_id", "invalid session id")
		return
	}
	afterSeq, err := parseSessionStreamAfterSeq(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	if err := p.ensureSessionExists(id); err != nil {
		switch {
		case errors.Is(err, errSessionStoreUnavailable):
			writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session store is not configured")
		case errors.Is(err, sessions.ErrNotFound):
			writeError(w, http.StatusNotFound, "session_not_found", "session not found")
		default:
			writeError(w, http.StatusInternalServerError, "session_store_error", "could not load session stream")
		}
		return
	}
	conn, err := sessionStreamUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	var client *sessionStreamClient
	var ok bool
	if afterSeq != nil {
		client, ok, err = p.streams.subscribeWithCatchUp(id, conn, func() ([][]byte, error) {
			return p.sessionStreamCatchUpPayloads(id, *afterSeq)
		})
	} else {
		client, ok = p.streams.subscribe(id, conn)
	}
	if err != nil {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "could not load session stream"), time.Now().Add(sessionStreamWriteTimeout))
		_ = conn.Close()
		return
	}
	if !ok {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down"), time.Now().Add(sessionStreamWriteTimeout))
		_ = conn.Close()
		return
	}
	go client.writeLoop()
	client.readLoop()
}

func (p *Process) snapshot() Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.info
}

type beginTurnResult int

const (
	beginTurnStarted beginTurnResult = iota
	beginTurnBusy
	beginTurnShuttingDown
)

func (p *Process) beginSessionTurn(sessionID, turnID string, cancel context.CancelFunc) beginTurnResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.acceptingTurns {
		return beginTurnShuttingDown
	}
	if _, running := p.runningTurns[sessionID]; running {
		return beginTurnBusy
	}
	p.runningTurns[sessionID] = runningTurn{turnID: turnID, cancel: cancel}
	p.info.RunningTurns = len(p.runningTurns)
	return beginTurnStarted
}

func (p *Process) endSessionTurn(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, running := p.runningTurns[sessionID]; !running {
		return
	}
	delete(p.runningTurns, sessionID)
	p.info.RunningTurns = len(p.runningTurns)
	if len(p.runningTurns) == 0 {
		close(p.turnsChanged)
		p.turnsChanged = make(chan struct{})
	}
}

func (p *Process) sessionStatus(session sessions.SessionV2) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, running := p.runningTurns[session.ID]; running {
		return "running"
	}
	if !session.InterruptedAt.IsZero() && (session.LastUsedAt.IsZero() || !session.LastUsedAt.After(session.InterruptedAt)) {
		return "interrupted"
	}
	return "idle"
}

func (p *Process) sessionIsRunning(sessionID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, running := p.runningTurns[sessionID]
	return running
}

func (p *Process) stopAcceptingTurns() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.acceptingTurns = false
}

func (p *Process) cancelRunningTurns() {
	p.mu.Lock()
	running := make([]runningTurn, 0, len(p.runningTurns))
	for _, turn := range p.runningTurns {
		running = append(running, turn)
	}
	p.mu.Unlock()
	for _, turn := range running {
		if turn.cancel != nil {
			turn.cancel()
		}
	}
}

func (p *Process) waitForRunningTurns(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		p.mu.Lock()
		if len(p.runningTurns) == 0 {
			p.mu.Unlock()
			return nil
		}
		changed := p.turnsChanged
		p.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (p *Process) finishDurableTurn(sessionID, turnID string, interrupted bool) {
	if p.sessionStore == nil {
		return
	}
	if interrupted {
		_, _ = p.sessionStore.MarkTurnInterrupted(sessionID, turnID)
		return
	}
	_, _ = p.sessionStore.ClearRunningTurn(sessionID, turnID)
}

func (p *Process) projectHasRunningTurn(projectID string) (bool, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return false, nil
	}
	p.mu.Lock()
	runningSessionIDs := make(map[string]struct{}, len(p.runningTurns))
	for sessionID := range p.runningTurns {
		runningSessionIDs[sessionID] = struct{}{}
	}
	p.mu.Unlock()
	if len(runningSessionIDs) == 0 || p.sessionStore == nil {
		return false, nil
	}
	infos, err := p.sessionStore.ListWithOptions(sessions.V2ListOptions{All: true})
	if err != nil {
		return false, err
	}
	for _, info := range infos {
		if _, running := runningSessionIDs[info.ID]; running && info.ProjectID == projectID {
			return true, nil
		}
	}
	return false, nil
}

func (p *Process) removeProjectSessions(projectID string) (int, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" || p.sessionStore == nil {
		return 0, nil
	}
	infos, err := p.sessionStore.ListWithOptions(sessions.V2ListOptions{All: true})
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, info := range infos {
		if info.ProjectID != projectID {
			continue
		}
		if err := p.sessionStore.Delete(info.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (p *Process) sessionCount() (int, error) {
	if p.sessionStore == nil {
		info := p.snapshot()
		return info.SessionCount, nil
	}
	infos, err := p.sessionStore.List()
	if err != nil {
		return 0, err
	}
	return len(infos), nil
}

func (p *Process) projectCount() (int, error) {
	if p.projectStore == nil {
		info := p.snapshot()
		return info.ProjectCount, nil
	}
	projects, err := p.projectStore.List()
	if err != nil {
		return 0, err
	}
	return len(projects), nil
}

func (p *Process) ensureSessionExists(id string) error {
	store := p.sessionStore
	if store == nil {
		return errSessionStoreUnavailable
	}
	_, err := store.Load(id)
	return err
}

func (p *Process) writeSessionLoadError(w http.ResponseWriter, err error, message string) {
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		writeError(w, http.StatusNotFound, "session_not_found", "session not found")
	case errors.Is(err, sessions.ErrCorruptedSession):
		writeError(w, http.StatusInternalServerError, "session_corrupted", "session is corrupted")
	default:
		writeError(w, http.StatusInternalServerError, "session_store_error", message)
	}
}

func (p *Process) writeTurnError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sessions.ErrCorruptedSession):
		writeError(w, http.StatusInternalServerError, "session_corrupted", "session is corrupted")
	case errors.Is(err, context.Canceled):
		writeError(w, http.StatusInternalServerError, "turn_failed", "turn failed")
	default:
		writeError(w, http.StatusInternalServerError, "turn_failed", "turn failed")
	}
}

func (p *Process) writeCompactError(w http.ResponseWriter, err error) {
	if errors.Is(err, sessions.ErrCorruptedSession) {
		writeError(w, http.StatusInternalServerError, "session_corrupted", "session is corrupted")
		return
	}
	writeError(w, http.StatusInternalServerError, "compact_failed", "compact failed")
}

func (p *Process) writeCompactionStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, sessions.ErrCorruptedSession) {
		writeError(w, http.StatusInternalServerError, "session_corrupted", "session is corrupted")
		return
	}
	writeError(w, http.StatusInternalServerError, "session_store_error", "could not save compaction")
}

type SessionStreamEvent map[string]any

func NewSessionStreamEvent(eventType string, fields map[string]any) SessionStreamEvent {
	event := make(SessionStreamEvent, len(fields)+1)
	for key, value := range fields {
		event[key] = value
	}
	event["type"] = eventType
	return event
}

func (p *Process) PublishSessionEvent(sessionID string, event SessionStreamEvent) error {
	if !validSessionAPIID(sessionID) {
		return fmt.Errorf("invalid session id")
	}
	if err := p.ensureSessionExists(sessionID); err != nil {
		return err
	}
	payload, err := marshalSessionStreamEvent(event)
	if err != nil {
		return err
	}
	p.streams.publish(sessionID, payload)
	return nil
}

func (p *Process) publishSessionEvent(sessionID string, event SessionStreamEvent) {
	payload, err := marshalSessionStreamEvent(event)
	if err != nil {
		return
	}
	p.streams.publish(sessionID, payload)
}

// startSessionEventBusBridge drains a turn-local bus into the process session
// stream. The returned function only waits for the bridge goroutine; callers
// must close bus first so the lossless subscription terminates.
func (p *Process) startSessionEventBusBridge(sessionID, turnID string, bus *eventbus.Bus, afterSeq int64) func() {
	done := make(chan struct{})
	if p == nil || bus == nil {
		close(done)
		return func() {}
	}
	events := bus.SubscribeLossless(sessionStreamClientBuffer)
	go func() {
		defer close(done)
		lastSeq := afterSeq
		for event := range events {
			switch event := event.(type) {
			case eventbus.ModelEvent:
				if event.Event != nil {
					p.publishModelTurnEvent(sessionID, turnID, event.Event)
				}
			case eventbus.DurableCommitted:
				nextSeq := p.publishPersistedSessionEventsThrough(sessionID, lastSeq, event.Seq)
				if nextSeq > lastSeq {
					lastSeq = nextSeq
				}
			}
		}
	}()
	return func() {
		<-done
	}
}

func (p *Process) publishPersistedSessionEventsThrough(sessionID string, afterSeq, throughSeq int64) int64 {
	if p == nil || p.sessionStore == nil {
		return afterSeq
	}
	if throughSeq <= afterSeq {
		return afterSeq
	}
	events, err := p.sessionStore.PersistedEventsAfter(sessionID, afterSeq)
	if err != nil {
		return afterSeq
	}
	for _, event := range events {
		if event.Seq > throughSeq {
			break
		}
		streamEvent, ok := sessionStreamEventFromPersistedEvent(event)
		if !ok {
			continue
		}
		p.publishSessionEvent(sessionID, streamEvent)
	}
	return throughSeq
}

func (p *Process) sessionStreamCatchUpPayloads(sessionID string, afterSeq int64) ([][]byte, error) {
	store := p.sessionStore
	if store == nil {
		return nil, errSessionStoreUnavailable
	}
	events, err := store.PersistedEventsAfter(sessionID, afterSeq)
	if err != nil {
		return nil, err
	}
	payloads := make([][]byte, 0, len(events))
	for _, event := range events {
		streamEvent, ok := sessionStreamEventFromPersistedEvent(event)
		if !ok {
			continue
		}
		payload, err := marshalSessionStreamEvent(streamEvent)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, payload)
	}
	return payloads, nil
}

func sessionStreamEventFromPersistedEvent(event sessions.PersistedEvent) (SessionStreamEvent, bool) {
	switch event.Type {
	case sessions.RecordTypeItemAppended:
		return NewSessionStreamEvent("item.appended", map[string]any{
			"seq":     event.Seq,
			"item_id": event.ItemID,
		}), true
	case sessions.RecordTypeItemUpdated:
		return NewSessionStreamEvent("item.updated", map[string]any{
			"seq":     event.Seq,
			"item_id": event.ItemID,
		}), true
	case sessions.RecordTypeCompactionCreated:
		return NewSessionStreamEvent("compaction.created", map[string]any{
			"seq":           event.Seq,
			"compaction_id": event.CompactionID,
		}), true
	case sessions.RecordTypeActiveHistoryReplaced:
		return NewSessionStreamEvent("active_history.replaced", map[string]any{
			"seq": event.Seq,
		}), true
	default:
		return nil, false
	}
}

func (p *Process) publishModelTurnEvent(sessionID, turnID string, event model.Event) {
	switch event := event.(type) {
	case model.TextDeltaEvent:
		if event.Text == "" {
			return
		}
		p.publishSessionEvent(sessionID, NewSessionStreamEvent("text.delta", map[string]any{
			"turn_id": turnID,
			"text":    event.Text,
		}))
	case model.ToolCallDoneEvent:
		if event.ToolCall.Name == "" {
			return
		}
		p.publishSessionEvent(sessionID, NewSessionStreamEvent("tool.started", map[string]any{
			"turn_id":      turnID,
			"tool_call_id": event.ToolCall.ID,
			"name":         event.ToolCall.Name,
		}))
	case model.ToolResultEvent:
		if event.Result.Name == "" {
			return
		}
		p.publishSessionEvent(sessionID, NewSessionStreamEvent("tool.finished", map[string]any{
			"turn_id":      turnID,
			"tool_call_id": event.Result.ToolCallID,
			"name":         event.Result.Name,
			"is_error":     event.Result.IsError,
		}))
	}
}

func (p *Process) publishTurnFailed(sessionID, turnID string, err error) {
	message := "turn failed"
	if errors.Is(err, sessions.ErrCorruptedSession) {
		message = "session is corrupted"
	}
	p.publishSessionEvent(sessionID, NewSessionStreamEvent("turn.failed", map[string]any{
		"turn_id": turnID,
		"message": message,
	}))
}

func (p *Process) publishCompactFailed(sessionID string, err error) {
	message := "compact failed"
	if errors.Is(err, sessions.ErrCorruptedSession) {
		message = "session is corrupted"
	}
	p.publishSessionEvent(sessionID, NewSessionStreamEvent("compact.failed", map[string]any{
		"message": message,
	}))
}

func marshalSessionStreamEvent(event SessionStreamEvent) ([]byte, error) {
	eventType, ok := event["type"].(string)
	if !ok || strings.TrimSpace(eventType) == "" {
		return nil, fmt.Errorf("stream event type is required")
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("marshal stream event: %w", err)
	}
	return payload, nil
}

type sessionStreamHub struct {
	mu       sync.Mutex
	closed   bool
	sessions map[string]map[*sessionStreamClient]struct{}
}

type sessionStreamClient struct {
	hub       *sessionStreamHub
	sessionID string
	conn      *websocket.Conn
	send      chan []byte
	closeOnce sync.Once
}

func newSessionStreamHub() *sessionStreamHub {
	return &sessionStreamHub{
		sessions: make(map[string]map[*sessionStreamClient]struct{}),
	}
}

func (h *sessionStreamHub) subscribe(sessionID string, conn *websocket.Conn) (*sessionStreamClient, bool) {
	return h.subscribeWithBuffer(sessionID, conn, sessionStreamClientBuffer)
}

func (h *sessionStreamHub) subscribeWithBuffer(sessionID string, conn *websocket.Conn, buffer int) (*sessionStreamClient, bool) {
	if buffer < sessionStreamClientBuffer {
		buffer = sessionStreamClientBuffer
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, false
	}
	client := &sessionStreamClient{
		hub:       h,
		sessionID: sessionID,
		conn:      conn,
		send:      make(chan []byte, buffer),
	}
	if h.sessions[sessionID] == nil {
		h.sessions[sessionID] = make(map[*sessionStreamClient]struct{})
	}
	h.sessions[sessionID][client] = struct{}{}
	return client, true
}

func (h *sessionStreamHub) subscribeWithCatchUp(sessionID string, conn *websocket.Conn, catchUp func() ([][]byte, error)) (*sessionStreamClient, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, false, nil
	}
	payloads, err := catchUp()
	if err != nil {
		return nil, false, err
	}
	client := &sessionStreamClient{
		hub:       h,
		sessionID: sessionID,
		conn:      conn,
		send:      make(chan []byte, len(payloads)+sessionStreamClientBuffer),
	}
	for _, payload := range payloads {
		client.send <- payload
	}
	if h.sessions[sessionID] == nil {
		h.sessions[sessionID] = make(map[*sessionStreamClient]struct{})
	}
	h.sessions[sessionID][client] = struct{}{}
	return client, true, nil
}

func (h *sessionStreamHub) publish(sessionID string, payload []byte) {
	var slowClients []*sessionStreamClient
	h.mu.Lock()
	if !h.closed {
		for client := range h.sessions[sessionID] {
			select {
			case client.send <- payload:
			default:
				slowClients = append(slowClients, client)
			}
		}
	}
	h.mu.Unlock()
	for _, client := range slowClients {
		client.close()
	}
}

func (h *sessionStreamHub) close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	clients := make([]*sessionStreamClient, 0)
	for _, sessionClients := range h.sessions {
		for client := range sessionClients {
			clients = append(clients, client)
		}
	}
	h.sessions = make(map[string]map[*sessionStreamClient]struct{})
	h.mu.Unlock()

	for _, client := range clients {
		c := client
		c.closeOnce.Do(func() {
			close(c.send)
			_ = c.conn.Close()
		})
	}
}

func (h *sessionStreamHub) subscriberCount(sessionID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions[sessionID])
}

func (c *sessionStreamClient) writeLoop() {
	defer c.close()
	for payload := range c.send {
		_ = c.conn.SetWriteDeadline(time.Now().Add(sessionStreamWriteTimeout))
		if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			return
		}
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(sessionStreamWriteTimeout))
	_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(sessionStreamWriteTimeout))
}

func (c *sessionStreamClient) readLoop() {
	defer c.close()
	c.conn.SetReadLimit(1024)
	for {
		if _, _, err := c.conn.NextReader(); err != nil {
			return
		}
	}
}

func (c *sessionStreamClient) close() {
	c.closeOnce.Do(func() {
		c.hub.mu.Lock()
		sessionClients := c.hub.sessions[c.sessionID]
		if sessionClients != nil {
			delete(sessionClients, c)
			if len(sessionClients) == 0 {
				delete(c.hub.sessions, c.sessionID)
			}
		}
		c.hub.mu.Unlock()
		close(c.send)
		_ = c.conn.Close()
	})
}

type sessionMetadataDTO struct {
	ID                string    `json:"id"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	DisplayName       string    `json:"display_name,omitempty"`
	Archived          bool      `json:"archived"`
	LastUsedAt        time.Time `json:"last_used_at"`
	InterruptedAt     time.Time `json:"interrupted_at,omitempty"`
	InterruptedTurnID string    `json:"interrupted_turn_id,omitempty"`
	Provider          string    `json:"provider"`
	ModelProfile      string    `json:"model_profile"`
	ModelID           string    `json:"model_id"`
	ProjectID         string    `json:"project_id,omitempty"`
	CreatedCWD        string    `json:"created_cwd,omitempty"`
	LastSeq           int64     `json:"last_seq"`
}

type projectDTO struct {
	ID          string    `json:"id"`
	Root        string    `json:"root"`
	DisplayName string    `json:"display_name,omitempty"`
	Archived    bool      `json:"archived"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type projectCreateRequest struct {
	Root        string
	DisplayName string
}

type sessionDetailDTO struct {
	ID                string                 `json:"id"`
	CreatedAt         time.Time              `json:"created_at"`
	UpdatedAt         time.Time              `json:"updated_at"`
	DisplayName       string                 `json:"display_name,omitempty"`
	Archived          bool                   `json:"archived"`
	LastUsedAt        time.Time              `json:"last_used_at"`
	InterruptedAt     time.Time              `json:"interrupted_at,omitempty"`
	InterruptedTurnID string                 `json:"interrupted_turn_id,omitempty"`
	Provider          string                 `json:"provider"`
	ModelProfile      string                 `json:"model_profile"`
	ModelID           string                 `json:"model_id"`
	Status            string                 `json:"status"`
	LastSeq           int64                  `json:"last_seq"`
	CWD               string                 `json:"cwd,omitempty"`
	ProjectID         string                 `json:"project_id,omitempty"`
	CreatedCWD        string                 `json:"created_cwd,omitempty"`
	ConfigPath        string                 `json:"config_path,omitempty"`
	ModelParameters   map[string]any         `json:"model_parameters,omitempty"`
	EnabledTools      []string               `json:"enabled_tools,omitempty"`
	EnabledMCP        []string               `json:"enabled_mcp,omitempty"`
	EnabledSkills     []string               `json:"enabled_skills,omitempty"`
	ShowReasoning     bool                   `json:"show_reasoning"`
	Context           contextwindow.Metadata `json:"context"`
	SaveToolResults   bool                   `json:"save_tool_results"`
}

type sessionItemsResponseDTO struct {
	Items         []sessionItemDTO `json:"items"`
	OldestSeq     int64            `json:"oldest_seq"`
	NewestSeq     int64            `json:"newest_seq"`
	HasMoreBefore bool             `json:"has_more_before"`
	HasMoreAfter  bool             `json:"has_more_after"`
}

type sessionItemDTO struct {
	Seq        int64                  `json:"seq"`
	ID         string                 `json:"id"`
	TurnID     string                 `json:"turn_id,omitempty"`
	CreatedAt  time.Time              `json:"created_at"`
	Kind       string                 `json:"kind"`
	Visibility string                 `json:"visibility"`
	Audience   string                 `json:"audience"`
	Status     string                 `json:"status,omitempty"`
	Message    *sessionItemMessageDTO `json:"message,omitempty"`
}

type sessionItemMessageDTO struct {
	Role       model.MessageRole             `json:"role"`
	Content    *sessionItemMessageContentDTO `json:"content,omitempty"`
	ToolCallID string                        `json:"tool_call_id,omitempty"`
	ToolCalls  []sessionItemToolCallDTO      `json:"tool_calls,omitempty"`
	IsError    bool                          `json:"is_error,omitempty"`
}

type sessionItemToolCallDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

type sessionItemMessageContentDTO struct {
	Inline    string                 `json:"inline,omitempty"`
	Preview   string                 `json:"preview,omitempty"`
	SizeBytes int64                  `json:"size_bytes,omitempty"`
	Truncated bool                   `json:"truncated,omitempty"`
	Blob      *sessionItemBlobRefDTO `json:"blob,omitempty"`
}

type sessionItemBlobRefDTO struct {
	Hash      string `json:"hash"`
	SizeBytes int64  `json:"size_bytes"`
	Encoding  string `json:"encoding"`
	MediaType string `json:"media_type,omitempty"`
}

type sessionBlobContentResponseDTO struct {
	BlobHash      string `json:"blob_hash"`
	Content       string `json:"content"`
	Offset        int64  `json:"offset"`
	SizeBytes     int64  `json:"size_bytes"`
	BytesReturned int    `json:"bytes_returned"`
	HasMore       bool   `json:"has_more"`
	Encoding      string `json:"encoding,omitempty"`
	MediaType     string `json:"media_type,omitempty"`
}

type sessionItemsQuery struct {
	BeforeSeq *int64
	AfterSeq  *int64
	Limit     int
	View      string
}

type sessionItemQuery struct {
	View string
}

type sessionListQuery struct {
	Archived    bool
	AllProjects bool
}

type sessionItemContentQuery struct {
	Offset   int64
	MaxBytes int
	View     string
}

type shutdownQuery struct {
	Wait    bool
	Timeout time.Duration
}

func sessionDetailDTOFromSession(session sessions.SessionV2, status string) sessionDetailDTO {
	return sessionDetailDTO{
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
		Status:            status,
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

func projectDTOFromProject(project projectstore.Project) projectDTO {
	return projectDTO{
		ID:          project.ID,
		Root:        project.Root,
		DisplayName: project.DisplayName,
		Archived:    project.Archived,
		CreatedAt:   project.CreatedAt,
		UpdatedAt:   project.UpdatedAt,
	}
}

func parseSessionItemsQuery(r *http.Request) (sessionItemsQuery, error) {
	values := r.URL.Query()
	query := sessionItemsQuery{
		Limit: defaultSessionItemsLimit,
		View:  sessionItemsViewChat,
	}
	var err error
	query.BeforeSeq, err = parseOptionalNonNegativeInt64Query(values, "before_seq")
	if err != nil {
		return sessionItemsQuery{}, err
	}
	query.AfterSeq, err = parseOptionalNonNegativeInt64Query(values, "after_seq")
	if err != nil {
		return sessionItemsQuery{}, err
	}
	if query.BeforeSeq != nil && query.AfterSeq != nil {
		return sessionItemsQuery{}, fmt.Errorf("before_seq and after_seq are mutually exclusive")
	}
	if rawLimit, ok, err := singleQueryValue(values, "limit"); err != nil {
		return sessionItemsQuery{}, err
	} else if ok {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit <= 0 {
			return sessionItemsQuery{}, fmt.Errorf("limit must be a positive integer")
		}
		if limit > maxSessionItemsLimit {
			limit = maxSessionItemsLimit
		}
		query.Limit = limit
	}
	if rawView, ok, err := singleQueryValue(values, "view"); err != nil {
		return sessionItemsQuery{}, err
	} else if ok {
		switch rawView {
		case sessionItemsViewChat, sessionItemsViewDebug:
			query.View = rawView
		default:
			return sessionItemsQuery{}, fmt.Errorf("view must be %q or %q", sessionItemsViewChat, sessionItemsViewDebug)
		}
	}
	return query, nil
}

func parseSessionItemQuery(r *http.Request) (sessionItemQuery, error) {
	values := r.URL.Query()
	query := sessionItemQuery{
		View: sessionItemsViewChat,
	}
	if rawView, ok, err := singleQueryValue(values, "view"); err != nil {
		return sessionItemQuery{}, err
	} else if ok {
		switch rawView {
		case sessionItemsViewChat, sessionItemsViewDebug:
			query.View = rawView
		default:
			return sessionItemQuery{}, fmt.Errorf("view must be %q or %q", sessionItemsViewChat, sessionItemsViewDebug)
		}
	}
	return query, nil
}

func parseSessionListQuery(r *http.Request) (sessionListQuery, error) {
	values := r.URL.Query()
	query := sessionListQuery{}
	if rawArchived, ok, err := singleQueryValue(values, "archived"); err != nil {
		return sessionListQuery{}, err
	} else if ok {
		archived, err := strconv.ParseBool(rawArchived)
		if err != nil {
			return sessionListQuery{}, fmt.Errorf("archived must be a boolean")
		}
		query.Archived = archived
	}
	if rawAllProjects, ok, err := singleQueryValue(values, "all_projects"); err != nil {
		return sessionListQuery{}, err
	} else if ok {
		allProjects, err := strconv.ParseBool(rawAllProjects)
		if err != nil {
			return sessionListQuery{}, fmt.Errorf("all_projects must be a boolean")
		}
		query.AllProjects = allProjects
	}
	return query, nil
}

func parseSessionItemContentQuery(r *http.Request) (sessionItemContentQuery, error) {
	values := r.URL.Query()
	query := sessionItemContentQuery{
		MaxBytes: defaultSessionItemContentBytes,
		View:     sessionItemsViewChat,
	}
	offset, err := parseOptionalNonNegativeInt64Query(values, "offset")
	if err != nil {
		return sessionItemContentQuery{}, err
	}
	if offset != nil {
		query.Offset = *offset
	}
	if rawMaxBytes, ok, err := singleQueryValue(values, "max_bytes"); err != nil {
		return sessionItemContentQuery{}, err
	} else if ok {
		maxBytes, err := strconv.ParseInt(rawMaxBytes, 10, 64)
		if err != nil || maxBytes <= 0 {
			return sessionItemContentQuery{}, fmt.Errorf("max_bytes must be a positive integer")
		}
		if maxBytes > maxSessionItemContentBytes {
			maxBytes = maxSessionItemContentBytes
		}
		query.MaxBytes = int(maxBytes)
	}
	if rawView, ok, err := singleQueryValue(values, "view"); err != nil {
		return sessionItemContentQuery{}, err
	} else if ok {
		switch rawView {
		case sessionItemsViewChat, sessionItemsViewDebug:
			query.View = rawView
		default:
			return sessionItemContentQuery{}, fmt.Errorf("view must be %q or %q", sessionItemsViewChat, sessionItemsViewDebug)
		}
	}
	return query, nil
}

func parseSessionStreamAfterSeq(r *http.Request) (*int64, error) {
	return parseOptionalNonNegativeInt64Query(r.URL.Query(), "after_seq")
}

func parseOptionalNonNegativeInt64Query(values map[string][]string, name string) (*int64, error) {
	raw, ok, err := singleQueryValue(values, name)
	if err != nil || !ok {
		return nil, err
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return nil, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return &value, nil
}

func singleQueryValue(values map[string][]string, name string) (string, bool, error) {
	rawValues, ok := values[name]
	if !ok {
		return "", false, nil
	}
	if len(rawValues) != 1 {
		return "", true, fmt.Errorf("%s must be specified once", name)
	}
	raw := strings.TrimSpace(rawValues[0])
	if raw == "" {
		return "", true, fmt.Errorf("%s must not be empty", name)
	}
	return raw, true, nil
}

func parseShutdownQuery(r *http.Request) (shutdownQuery, error) {
	var query shutdownQuery
	rawWait, ok, err := singleQueryValue(r.URL.Query(), "wait")
	if err != nil {
		return shutdownQuery{}, err
	}
	if ok {
		query.Wait, err = strconv.ParseBool(rawWait)
		if err != nil {
			return shutdownQuery{}, fmt.Errorf("wait must be true or false")
		}
	}
	rawTimeout, ok, err := singleQueryValue(r.URL.Query(), "timeout_ms")
	if err != nil {
		return shutdownQuery{}, err
	}
	if ok {
		timeoutMS, err := strconv.Atoi(rawTimeout)
		if err != nil || timeoutMS < 0 {
			return shutdownQuery{}, fmt.Errorf("timeout_ms must be a non-negative integer")
		}
		query.Timeout = time.Duration(timeoutMS) * time.Millisecond
	}
	return query, nil
}

func filterSessionItemsForView(items []sessions.SessionItem, view string) []sessions.SessionItem {
	filtered := make([]sessions.SessionItem, 0, len(items))
	for _, item := range items {
		if sessionItemVisibleInView(item, view) {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func sessionItemVisibleInView(item sessions.SessionItem, view string) bool {
	if view == sessionItemsViewDebug {
		return true
	}
	if item.Visibility != sessions.ItemVisibilityVisible {
		return false
	}
	if item.Audience == sessions.ItemAudienceInternal {
		return false
	}
	if item.Message != nil && item.Message.Role == model.MessageRoleTool {
		return false
	}
	return item.Kind != sessions.ItemKindRuntimeContext
}

func sessionItemContentReadableInView(item sessions.SessionItem, view string) bool {
	if item.Message == nil || !sessionItemHasContent(item) {
		return false
	}
	if view == sessionItemsViewDebug {
		return true
	}
	if item.Kind != sessions.ItemKindMessage {
		return false
	}
	if item.Visibility != sessions.ItemVisibilityVisible {
		return false
	}
	if item.Audience != sessions.ItemAudienceUser && item.Audience != sessions.ItemAudienceModel {
		return false
	}
	return item.Message.Role == model.MessageRoleUser || item.Message.Role == model.MessageRoleAssistant
}

func sessionItemHasContent(item sessions.SessionItem) bool {
	if item.Message != nil && item.Message.Content != "" {
		return true
	}
	if item.Content == nil {
		return false
	}
	if item.Content.Inline != "" {
		return true
	}
	return item.Content.Blob != nil
}

func findSessionItemByID(items []sessions.SessionItem, id string) (sessions.SessionItem, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return sessions.SessionItem{}, false
}

func sessionItemPageSeqBounds(items []sessions.SessionItem) (int64, int64) {
	if len(items) == 0 {
		return 0, 0
	}
	return items[0].Seq, items[len(items)-1].Seq
}

func paginateSessionItems(items []sessions.SessionItem, query sessionItemsQuery) ([]sessions.SessionItem, bool, bool) {
	start := 0
	end := len(items)
	if query.AfterSeq != nil {
		start = firstSessionItemIndexAfterSeq(items, *query.AfterSeq)
		end = start + query.Limit
		if end > len(items) {
			end = len(items)
		}
	} else if query.BeforeSeq != nil {
		end = firstSessionItemIndexAtOrAfterSeq(items, *query.BeforeSeq)
		start = end - query.Limit
		if start < 0 {
			start = 0
		}
	} else if len(items) > query.Limit {
		start = len(items) - query.Limit
	}
	return items[start:end], start > 0, end < len(items)
}

func firstSessionItemIndexAfterSeq(items []sessions.SessionItem, seq int64) int {
	for i, item := range items {
		if item.Seq > seq {
			return i
		}
	}
	return len(items)
}

func firstSessionItemIndexAtOrAfterSeq(items []sessions.SessionItem, seq int64) int {
	for i, item := range items {
		if item.Seq >= seq {
			return i
		}
	}
	return len(items)
}

func sessionItemDTOFromSessionItem(item sessions.SessionItem) sessionItemDTO {
	dto := sessionItemDTO{
		Seq:        item.Seq,
		ID:         item.ID,
		TurnID:     item.TurnID,
		CreatedAt:  item.CreatedAt,
		Kind:       item.Kind,
		Visibility: item.Visibility,
		Audience:   item.Audience,
		Status:     item.Status,
	}
	if item.Message != nil {
		dto.Message = &sessionItemMessageDTO{
			Role: item.Message.Role,
		}
		dto.Message.Content = sessionItemMessageContentDTOFromSessionItem(item)
	}
	return dto
}

func sessionItemRefetchDTOFromSessionItem(item sessions.SessionItem, includeDebugFields bool) sessionItemDTO {
	dto := sessionItemDTO{
		Seq:        item.Seq,
		ID:         item.ID,
		TurnID:     item.TurnID,
		CreatedAt:  item.CreatedAt,
		Kind:       item.Kind,
		Visibility: item.Visibility,
		Audience:   item.Audience,
		Status:     item.Status,
	}
	if item.Message == nil {
		return dto
	}
	message := &sessionItemMessageDTO{Role: item.Message.Role}
	if item.Message.Content != "" {
		message.Content = &sessionItemMessageContentDTO{Inline: item.Message.Content}
	} else {
		message.Content = sessionItemMessageContentDTOFromSessionItem(item)
	}
	if includeDebugFields {
		message.ToolCallID = item.Message.ToolCallID
		message.IsError = item.Message.IsError
		if len(item.Message.ToolCalls) > 0 {
			message.ToolCalls = make([]sessionItemToolCallDTO, 0, len(item.Message.ToolCalls))
			for _, toolCall := range item.Message.ToolCalls {
				message.ToolCalls = append(message.ToolCalls, sessionItemToolCallDTO{
					ID:        toolCall.ID,
					Name:      toolCall.Name,
					Arguments: toolCall.Arguments,
				})
			}
		}
	}
	dto.Message = message
	return dto
}

func resolveSessionItemInlineContent(store *sessions.V2Store, item sessions.SessionItem) (sessions.SessionItem, error) {
	if store == nil || item.Message == nil || item.Content == nil {
		return item, nil
	}
	var content string
	switch {
	case item.Content.Inline != "":
		content = item.Content.Inline
	case item.Content.Blob != nil:
		raw, err := store.ReadBlob(*item.Content.Blob)
		if err != nil {
			return sessions.SessionItem{}, err
		}
		content = string(raw)
	default:
		return item, nil
	}
	message := *item.Message
	message.Content = content
	item.Message = &message
	item.Content = nil
	return item, nil
}

func sessionItemMessageContentDTOFromSessionItem(item sessions.SessionItem) *sessionItemMessageContentDTO {
	if item.Content != nil {
		if dto := sessionItemMessageContentDTOFromStoredContent(item.Content); dto != nil {
			return dto
		}
	}
	if item.Message != nil && item.Message.Content != "" {
		return sessionItemMessageContentDTOFromString(item.Message.Content)
	}
	return nil
}

func sessionItemMessageContentDTOFromString(content string) *sessionItemMessageContentDTO {
	if len(content) <= sessionItemInlineMessageBytes {
		return &sessionItemMessageContentDTO{
			Inline: content,
		}
	}
	return &sessionItemMessageContentDTO{
		Preview:   truncateStringByBytes(content, sessionItemPreviewMessageBytes),
		SizeBytes: int64(len(content)),
		Truncated: true,
	}
}

func sessionItemMessageContentDTOFromStoredContent(content *sessions.StoredContent) *sessionItemMessageContentDTO {
	if content == nil {
		return nil
	}
	if content.Inline != "" {
		return &sessionItemMessageContentDTO{
			Inline: content.Inline,
		}
	}
	if content.Blob != nil {
		return &sessionItemMessageContentDTO{
			Preview:   truncateStringByBytes(content.Preview, sessionItemPreviewMessageBytes),
			SizeBytes: content.Blob.SizeBytes,
			Truncated: true,
			Blob:      sessionItemBlobRefDTOFromBlobRef(*content.Blob),
		}
	}
	return nil
}

func sessionItemBlobRefDTOFromBlobRef(ref sessions.BlobRef) *sessionItemBlobRefDTO {
	return &sessionItemBlobRefDTO{
		Hash:      ref.Hash,
		SizeBytes: ref.SizeBytes,
		Encoding:  ref.Encoding,
		MediaType: ref.MediaType,
	}
}

func truncateStringByBytes(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	cut := 0
	for i := range value {
		if i > maxBytes {
			break
		}
		cut = i
	}
	if cut == 0 {
		return ""
	}
	return value[:cut]
}

func findReachableSessionBlobRef(items []sessions.SessionItem, hash, view string) (sessions.BlobRef, bool) {
	for _, item := range items {
		if !sessionItemContentReadableInView(item, view) || item.Content == nil || item.Content.Blob == nil {
			continue
		}
		if strings.EqualFold(item.Content.Blob.Hash, hash) {
			return *item.Content.Blob, true
		}
	}
	return sessions.BlobRef{}, false
}

func validBlobHash(hash string) bool {
	if hash == "" || hash != strings.TrimSpace(hash) || len(hash) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

func sessionContentBytesRange(value []byte, offset int64, maxBytes int) (string, int64, int64, int, bool) {
	sizeBytes := int64(len(value))
	if offset >= sizeBytes {
		return "", sizeBytes, sizeBytes, 0, false
	}
	start := int(offset)
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	if start >= len(value) {
		return "", int64(start), sizeBytes, 0, false
	}
	end := start + maxBytes
	if end > len(value) {
		end = len(value)
	}
	for end > start && end < len(value) && !utf8.RuneStart(value[end]) {
		end--
	}
	content := string(value[start:end])
	return content, int64(start), sizeBytes, len(content), int64(end) < sizeBytes
}

func readProjectSessionCreateRequest(w http.ResponseWriter, r *http.Request) (SessionCreateMetadata, error) {
	body := http.MaxBytesReader(w, r.Body, 64*1024)
	data, err := io.ReadAll(body)
	if err != nil {
		return SessionCreateMetadata{}, fmt.Errorf("read request body: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return SessionCreateMetadata{}, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return SessionCreateMetadata{}, fmt.Errorf("request body must be empty or a JSON object")
	}
	if raw == nil {
		return SessionCreateMetadata{}, fmt.Errorf("request body must be empty or a JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return SessionCreateMetadata{}, fmt.Errorf("request body must contain a single JSON object")
	}
	allowed := map[string]bool{
		"created_cwd":       true,
		"config_path":       true,
		"provider":          true,
		"model_profile":     true,
		"model_id":          true,
		"model_parameters":  true,
		"enabled_tools":     true,
		"enabled_mcp":       true,
		"enabled_skills":    true,
		"show_reasoning":    true,
		"context":           true,
		"save_tool_results": true,
	}
	for name := range raw {
		if !allowed[name] {
			return SessionCreateMetadata{}, fmt.Errorf("unsupported session create metadata field %q", name)
		}
	}

	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var request SessionCreateMetadata
	if err := decoder.Decode(&request); err != nil {
		return SessionCreateMetadata{}, fmt.Errorf("invalid session create metadata")
	}
	return request, nil
}

func readEmptyObjectRequest(w http.ResponseWriter, r *http.Request) error {
	body := http.MaxBytesReader(w, r.Body, 1024)
	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("request body must be empty or an object")
	}
	if value == nil || len(value) != 0 {
		return fmt.Errorf("request body must be empty or {}")
	}
	return nil
}

func readSessionMetadataUpdateRequest(w http.ResponseWriter, r *http.Request) (SessionMetadataUpdate, error) {
	return readMetadataUpdateRequest(w, r, "session")
}

func readProjectMetadataUpdateRequest(w http.ResponseWriter, r *http.Request) (SessionMetadataUpdate, error) {
	return readMetadataUpdateRequest(w, r, "project")
}

func readMetadataUpdateRequest(w http.ResponseWriter, r *http.Request, resource string) (SessionMetadataUpdate, error) {
	body := http.MaxBytesReader(w, r.Body, 64*1024)
	decoder := json.NewDecoder(body)
	decoder.UseNumber()

	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return SessionMetadataUpdate{}, fmt.Errorf("request body must be a JSON object")
	}
	if raw == nil || len(raw) == 0 {
		return SessionMetadataUpdate{}, fmt.Errorf("request body must contain display_name and/or archived")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return SessionMetadataUpdate{}, fmt.Errorf("request body must contain a single JSON object")
	}

	var request SessionMetadataUpdate
	for name, rawValue := range raw {
		switch name {
		case "display_name":
			var value string
			if err := json.Unmarshal(rawValue, &value); err != nil {
				return SessionMetadataUpdate{}, fmt.Errorf("display_name must be a non-empty string")
			}
			value = strings.TrimSpace(value)
			if value == "" {
				return SessionMetadataUpdate{}, fmt.Errorf("display_name must be a non-empty string")
			}
			request.DisplayName = &value
		case "archived":
			var value bool
			if err := json.Unmarshal(rawValue, &value); err != nil {
				return SessionMetadataUpdate{}, fmt.Errorf("archived must be a boolean")
			}
			request.Archived = &value
		default:
			return SessionMetadataUpdate{}, fmt.Errorf("unsupported %s metadata field %q", resource, name)
		}
	}
	if request.DisplayName == nil && request.Archived == nil {
		return SessionMetadataUpdate{}, fmt.Errorf("request body must contain display_name and/or archived")
	}
	return request, nil
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

func applySessionMetadataUpdate(session sessions.SessionV2, metadata SessionMetadataUpdate) sessions.SessionV2 {
	if metadata.DisplayName != nil {
		session.DisplayName = *metadata.DisplayName
	}
	if metadata.Archived != nil {
		session.Archived = *metadata.Archived
	}
	return session
}

func readProjectCreateRequest(w http.ResponseWriter, r *http.Request) (projectCreateRequest, error) {
	body := http.MaxBytesReader(w, r.Body, 64*1024)
	decoder := json.NewDecoder(body)
	decoder.UseNumber()

	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return projectCreateRequest{}, fmt.Errorf("request body must be a JSON object")
	}
	if raw == nil {
		return projectCreateRequest{}, fmt.Errorf("request body must be a JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return projectCreateRequest{}, fmt.Errorf("request body must contain a single JSON object")
	}

	root, err := optionalStringField(raw, "root")
	if err != nil {
		return projectCreateRequest{}, err
	}
	cwd, err := optionalStringField(raw, "cwd")
	if err != nil {
		return projectCreateRequest{}, err
	}
	if root != "" && cwd != "" && root != cwd {
		return projectCreateRequest{}, fmt.Errorf("root and cwd must not conflict")
	}
	if root == "" {
		root = cwd
	}
	if strings.TrimSpace(root) == "" {
		return projectCreateRequest{}, fmt.Errorf("root or cwd must be a non-empty string")
	}

	displayName, err := optionalStringField(raw, "display_name")
	if err != nil {
		return projectCreateRequest{}, err
	}
	name, err := optionalStringField(raw, "name")
	if err != nil {
		return projectCreateRequest{}, err
	}
	if displayName == "" {
		displayName = name
	}
	return projectCreateRequest{
		Root:        root,
		DisplayName: displayName,
	}, nil
}

func optionalStringField(raw map[string]json.RawMessage, name string) (string, error) {
	valueRaw, ok := raw[name]
	if !ok {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(valueRaw, &value); err != nil {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return strings.TrimSpace(value), nil
}

func readSessionMessageRequest(w http.ResponseWriter, r *http.Request) (string, error) {
	body := http.MaxBytesReader(w, r.Body, 1024*1024)
	decoder := json.NewDecoder(body)
	decoder.UseNumber()

	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return "", fmt.Errorf("request body must be a JSON object")
	}
	if raw == nil {
		return "", fmt.Errorf("request body must be a JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return "", fmt.Errorf("request body must contain a single JSON object")
	}

	contentRaw, ok := raw["content"]
	if !ok {
		return "", fmt.Errorf("content must be a non-empty string")
	}
	var content string
	if err := json.Unmarshal(contentRaw, &content); err != nil {
		return "", fmt.Errorf("content must be a non-empty string")
	}
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("content must be a non-empty string")
	}
	return content, nil
}

func nextSessionTurnID(session sessions.SessionV2) string {
	return fmt.Sprintf("turn-%06d", session.LastSeq+1)
}

func nextSessionCompactOperationID(session sessions.SessionV2) string {
	return fmt.Sprintf("compact-%06d", session.LastSeq+1)
}

func compactionCreatedSeq(summaryItemSeq int64) int64 {
	if summaryItemSeq <= 0 {
		return 0
	}
	return summaryItemSeq + 1
}

func activeHistoryReplacedSeq(lastSeq int64) int64 {
	if lastSeq <= 1 {
		return 0
	}
	return lastSeq - 1
}

func savedSessionItemsByID(savedItems, requestedItems []sessions.SessionItem) []sessions.SessionItem {
	byID := make(map[string]sessions.SessionItem, len(savedItems))
	for _, item := range savedItems {
		byID[item.ID] = item
	}
	items := make([]sessions.SessionItem, 0, len(requestedItems))
	for _, item := range requestedItems {
		if saved, ok := byID[item.ID]; ok {
			items = append(items, saved)
		}
	}
	return items
}

func validSessionAPIID(id string) bool {
	if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) || id == "." || id == ".." {
		return false
	}
	if strings.EqualFold(strings.TrimRight(id, ". "), "blobs") {
		return false
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func validSessionItemAPIID(id string) bool {
	return validSessionAPIID(id)
}

func (p *Process) newSessionFromDefaults() sessions.SessionV2 {
	session := copySessionMetadata(p.sessionDefaults)
	session.ID = ""
	session.Version = sessions.VersionV2
	session.CreatedAt = time.Time{}
	session.UpdatedAt = time.Time{}
	session.Items = nil
	session.ActiveHistory = nil
	session.Compactions = nil
	session.LastSeq = 0
	session.RunningTurnID = ""
	session.RunningStartedAt = time.Time{}
	session.InterruptedTurnID = ""
	session.InterruptedAt = time.Time{}
	session.SaveToolResults = true
	return session
}

func sessionMetadataDTOFromSession(session sessions.SessionV2) sessionMetadataDTO {
	return sessionMetadataDTO{
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

func copySessionMetadata(session sessions.SessionV2) sessions.SessionV2 {
	session.ModelParameters = copyMap(session.ModelParameters)
	session.EnabledTools = copyStrings(session.EnabledTools)
	session.EnabledMCP = copyStrings(session.EnabledMCP)
	session.EnabledSkills = copyStrings(session.EnabledSkills)
	session.InstructionsSnapshot = copyMessages(session.InstructionsSnapshot)
	session.InstructionSources = copyInstructionSources(session.InstructionSources)
	session.Items = nil
	session.ActiveHistory = nil
	session.Compactions = nil
	session.LastSeq = 0
	return session
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
	return append([]string(nil), values...)
}

func copyMessages(messages []model.Message) []model.Message {
	if messages == nil {
		return nil
	}
	copied := append([]model.Message(nil), messages...)
	for i := range copied {
		copied[i].ToolCalls = append([]model.ToolCall(nil), messages[i].ToolCalls...)
	}
	return copied
}

func copyInstructionSources(sources []sessions.InstructionSource) []sessions.InstructionSource {
	if sources == nil {
		return nil
	}
	return append([]sessions.InstructionSource(nil), sources...)
}

func (p *Process) hasRegistryToken(r *http.Request) bool {
	token := strings.TrimSpace(p.authToken)
	if token == "" {
		return false
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return false
	}
	provided := strings.TrimSpace(auth[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func (p *Process) requireRegistryToken(w http.ResponseWriter, r *http.Request) bool {
	if p.hasRegistryToken(r) {
		return true
	}
	writeError(w, http.StatusForbidden, "permission_denied", "permission denied")
	return false
}

func writeMethodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
