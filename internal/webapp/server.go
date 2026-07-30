package webapp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/rexzhao/simple-agent/internal/execution"
	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

var Version = "dev"

type ServerOptions struct {
	Context   context.Context
	Service   *execution.Service
	Token     string
	CWD       string
	LogWriter io.Writer
}

type Server struct {
	service     *execution.Service
	token       string
	cwd         string
	mux         *http.ServeMux
	runs        *runRegistry
	codexLogins *codexLoginRegistry
}

func NewServer(options ServerOptions) (*Server, error) {
	if options.Service == nil {
		return nil, fmt.Errorf("web execution service is required")
	}
	if strings.TrimSpace(options.Token) == "" {
		return nil, fmt.Errorf("web capability token is required")
	}
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	server := &Server{
		service: options.Service,
		token:   options.Token,
		cwd:     options.CWD,
		mux:     http.NewServeMux(),
	}
	server.runs = newRunRegistry(ctx, options.Service, options.LogWriter)
	server.codexLogins = newCodexLoginRegistry(ctx, options.Service)
	server.routes()
	return server, nil
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		if !validLoopbackHost(r.Host) {
			writeAPIError(w, http.StatusForbidden, "invalid_host", "request host is not allowed")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if !s.authorized(r) {
				writeAPIError(w, http.StatusUnauthorized, "unauthorized", "valid capability token required")
				return
			}
			if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && origin != "http://"+r.Host {
				writeAPIError(w, http.StatusForbidden, "invalid_origin", "request origin is not allowed")
				return
			}
		}
		s.mux.ServeHTTP(w, r)
	})
}

func (s *Server) Close() {
	if s == nil || s.runs == nil {
		return
	}
	s.runs.Close()
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/bootstrap", s.handleBootstrap)
	s.mux.HandleFunc("GET /api/projects", s.handleListProjects)
	s.mux.HandleFunc("POST /api/projects", s.handleCreateProject)
	s.mux.HandleFunc("PATCH /api/projects/{projectID}", s.handleRenameProject)
	s.mux.HandleFunc("POST /api/projects/{projectID}/archive", s.handleArchiveProject)
	s.mux.HandleFunc("POST /api/projects/{projectID}/restore", s.handleRestoreProject)
	s.mux.HandleFunc("DELETE /api/projects/{projectID}", s.handleRemoveProject)
	s.mux.HandleFunc("GET /api/projects/{projectID}/models", s.handleSessionModels)
	s.mux.HandleFunc("GET /api/provider-settings", s.handleProviderSettings)
	s.mux.HandleFunc("POST /api/providers", s.handleCreateProvider)
	s.mux.HandleFunc("PUT /api/providers/{providerName}", s.handleUpdateProvider)
	s.mux.HandleFunc("PATCH /api/provider-default", s.handleUpdateDefaultProviderModel)
	s.mux.HandleFunc("GET /api/providers/{providerName}/models", s.handleDiscoverProviderModels)
	s.mux.HandleFunc("POST /api/providers/{providerName}/codex-login", s.handleStartCodexLogin)
	s.mux.HandleFunc("GET /api/providers/{providerName}/codex-login", s.handleCodexLoginStatus)
	s.mux.HandleFunc("DELETE /api/providers/{providerName}/codex-login", s.handleClearCodexLogin)
	s.mux.HandleFunc("GET /api/projects/{projectID}/sessions", s.handleListSessions)
	s.mux.HandleFunc("POST /api/projects/{projectID}/sessions", s.handleCreateSession)
	s.mux.HandleFunc("GET /api/sessions/{sessionID}", s.handleGetSession)
	s.mux.HandleFunc("GET /api/sessions/{sessionID}/snapshot", s.handleGetSessionSnapshot)
	s.mux.HandleFunc("PATCH /api/sessions/{sessionID}", s.handleRenameSession)
	s.mux.HandleFunc("POST /api/sessions/{sessionID}/full-access", s.handleSetSessionFullAccess)
	s.mux.HandleFunc("POST /api/sessions/{sessionID}/archive", s.handleArchiveSession)
	s.mux.HandleFunc("POST /api/sessions/{sessionID}/restore", s.handleRestoreSession)
	s.mux.HandleFunc("DELETE /api/sessions/{sessionID}", s.handleRemoveSession)
	s.mux.HandleFunc("GET /api/sessions/{sessionID}/items", s.handleSessionItems)
	s.mux.HandleFunc("GET /api/sessions/{sessionID}/images/{hash}", s.handleSessionImage)
	s.mux.HandleFunc("POST /api/sessions/{sessionID}/compact", s.handleCompactSession)
	s.mux.HandleFunc("POST /api/sessions/{sessionID}/runs", s.handleStartRun)
	s.mux.HandleFunc("GET /api/runs/active", s.handleListActiveRuns)
	s.mux.HandleFunc("GET /api/runs/{runID}/events", s.handleRunEvents)
	s.mux.HandleFunc("POST /api/runs/{runID}/prompts", s.handleAppendActive)
	s.mux.HandleFunc("DELETE /api/runs/{runID}/prompts/{promptID}", s.handleRemoveActivePrompt)
	s.mux.HandleFunc("POST /api/runs/{runID}/prompts/{promptID}/steer", s.handleSteerActivePrompt)
	s.mux.HandleFunc("POST /api/runs/{runID}/prompts/{promptID}/move", s.handleMoveActivePrompt)
	s.mux.HandleFunc("DELETE /api/runs/{runID}", s.handleCancelRun)
	s.mux.HandleFunc("DELETE /api/runs/{runID}/tools/{toolCallID}", s.handleCancelToolCall)
	s.mux.Handle("/", s.staticHandler())
}

func (s *Server) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) == 1
}

func (s *Server) staticHandler() http.Handler {
	assets, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		assetPath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if assetPath == "." || assetPath == "" {
			assetPath = "index.html"
		}
		if _, err := fs.Stat(assets, assetPath); err != nil {
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     Version,
		"cwd":         s.cwd,
		"server_root": s.service.ServerRoot(),
		"config_path": s.service.ConfigPath(),
	})
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.service.ListProjects(execution.ProjectListOptions{Archived: queryBool(r, "archived")})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Root        string `json:"root"`
		DisplayName string `json:"display_name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := s.service.CreateProject(body.Root, body.DisplayName)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

func (s *Server) handleRenameProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DisplayName string `json:"display_name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	project, err := s.service.RenameProject(r.PathValue("projectID"), body.DisplayName)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) handleArchiveProject(w http.ResponseWriter, r *http.Request) {
	project, err := s.service.ArchiveProject(r.PathValue("projectID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) handleRestoreProject(w http.ResponseWriter, r *http.Request) {
	project, err := s.service.RestoreProject(r.PathValue("projectID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) handleRemoveProject(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.RemoveProject(r.PathValue("projectID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.ListSessions(execution.SessionListOptions{
		ProjectID: r.PathValue("projectID"),
		Archived:  queryBool(r, "archived"),
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": items})
}

func (s *Server) handleSessionModels(w http.ResponseWriter, r *http.Request) {
	options, err := s.service.ConfiguredSessionModels(r.PathValue("projectID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, options)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD            string `json:"cwd"`
		ConfigPath     string `json:"config_path"`
		Provider       string `json:"provider"`
		ModelProfile   string `json:"model_profile"`
		ReasoningLevel string `json:"reasoning_level"`
		FullAccess     bool   `json:"full_access"`
	}
	if !decodeOptionalJSON(w, r, &body) {
		return
	}
	session, err := s.service.CreateConfiguredSession(r.PathValue("projectID"), execution.ConfiguredSessionOptions{
		CWD:            body.CWD,
		ConfigPath:     body.ConfigPath,
		Provider:       body.Provider,
		ModelProfile:   body.ModelProfile,
		ReasoningLevel: body.ReasoningLevel,
		FullAccess:     body.FullAccess,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, session)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	session, err := s.service.GetSession(r.PathValue("sessionID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) handleGetSessionSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.service.GetSessionSnapshot(r.PathValue("sessionID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DisplayName string `json:"display_name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	session, err := s.service.RenameSession(r.PathValue("sessionID"), body.DisplayName)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) handleSetSessionFullAccess(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FullAccess bool `json:"full_access"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	session, err := s.service.SetSessionFullAccess(r.PathValue("sessionID"), body.FullAccess)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) handleArchiveSession(w http.ResponseWriter, r *http.Request) {
	session, err := s.service.ArchiveSession(r.PathValue("sessionID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) handleRestoreSession(w http.ResponseWriter, r *http.Request) {
	session, err := s.service.RestoreSession(r.PathValue("sessionID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) handleRemoveSession(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.RemoveSession(r.PathValue("sessionID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleSessionItems(w http.ResponseWriter, r *http.Request) {
	options := execution.SessionItemsOptions{
		BeforeSeq: queryInt64(r, "before_seq"),
		AfterSeq:  queryInt64(r, "after_seq"),
		Limit:     queryInt(r, "limit"),
		AlignTurn: queryBool(r, "align_turn"),
	}
	page, err := s.service.GetSessionChatItemsPage(r.PathValue("sessionID"), options)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleCompactSession(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.CompactSession(r.Context(), r.PathValue("sessionID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func validLoopbackHost(hostport string) bool {
	host := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

const defaultJSONRequestBytes = 1 << 20

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	return decodeJSONWithLimit(w, r, target, defaultJSONRequestBytes)
}

func decodeJSONWithLimit(w http.ResponseWriter, r *http.Request, target any, maxBytes int64) bool {
	if maxBytes <= 0 {
		maxBytes = defaultJSONRequestBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "request body must be valid JSON")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON value")
		return false
	}
	return true
}

func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	return decodeJSON(w, r, target)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

func writeServiceError(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	code := "request_failed"
	switch {
	case errors.Is(err, projectstore.ErrNotFound), errors.Is(err, sessions.ErrNotFound):
		status = http.StatusNotFound
		code = "not_found"
	case errors.Is(err, execution.ErrSessionBusy):
		status = http.StatusConflict
		code = "session_busy"
	case errors.Is(err, context.Canceled):
		status = http.StatusRequestTimeout
		code = "cancelled"
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		message = "request failed"
	}
	writeAPIError(w, status, code, message)
}

func queryBool(r *http.Request, name string) bool {
	value, _ := strconv.ParseBool(r.URL.Query().Get(name))
	return value
}

func queryInt(r *http.Request, name string) int {
	value, _ := strconv.Atoi(r.URL.Query().Get(name))
	return value
}

func queryInt64(r *http.Request, name string) int64 {
	value, _ := strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
	return value
}
