package server

import (
	"context"
	"crypto/subtle"
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

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/model"
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
)

type Options struct {
	CWD             string
	ConfigPath      string
	Listen          string
	Version         string
	AuthToken       string
	Now             func() time.Time
	SessionStore    *sessions.V2Store
	SessionRoot     string
	SessionDefaults sessions.SessionV2
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
	authToken       string
}

type Info struct {
	CWD          string    `json:"cwd"`
	ConfigPath   string    `json:"config_path"`
	Addr         string    `json:"addr"`
	PID          int       `json:"pid"`
	Version      string    `json:"version"`
	StartedAt    time.Time `json:"started_at"`
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

	process := &Process{
		listener: listener,
		info: Info{
			CWD:        options.CWD,
			ConfigPath: options.ConfigPath,
			Addr:       listener.Addr().String(),
			PID:        os.Getpid(),
			Version:    version,
			StartedAt:  now().UTC(),
		},
		shutdownDone:    make(chan struct{}),
		sessionStore:    options.SessionStore,
		sessionDefaults: copySessionMetadata(options.SessionDefaults),
		authToken:       strings.TrimSpace(options.AuthToken),
	}
	if process.sessionStore == nil && strings.TrimSpace(options.SessionRoot) != "" {
		process.sessionStore = sessions.NewV2Store(options.SessionRoot)
	}
	if strings.TrimSpace(process.sessionDefaults.CWD) == "" {
		process.sessionDefaults.CWD = options.CWD
	}
	if strings.TrimSpace(process.sessionDefaults.ConfigPath) == "" {
		process.sessionDefaults.ConfigPath = options.ConfigPath
	}
	process.httpServer = &http.Server{
		Handler: process,
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
	if ctx == nil {
		ctx = context.Background()
	}
	p.shutdownOnce.Do(func() {
		p.shutdownErr = p.httpServer.Shutdown(ctx)
		close(p.shutdownDone)
	})
	<-p.shutdownDone
	return p.shutdownErr
}

func (p *Process) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health":
		p.handleHealth(w, r)
	case "/server":
		p.handleServer(w, r)
	case "/server/shutdown":
		p.handleShutdown(w, r)
	case "/sessions":
		p.handleSessions(w, r)
	default:
		if strings.HasPrefix(r.URL.Path, "/sessions/") {
			p.handleSessionPath(w, r)
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "path not found")
	}
}

func (p *Process) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	info := p.snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": info.Version,
		"pid":     info.PID,
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
	writeJSON(w, http.StatusOK, map[string]any{
		"cwd":            info.CWD,
		"config_path":    info.ConfigPath,
		"addr":           info.Addr,
		"pid":            info.PID,
		"version":        info.Version,
		"started_at":     info.StartedAt,
		"uptime_seconds": int64(time.Since(info.StartedAt).Seconds()),
		"session_count":  sessionCount,
		"running_turns":  info.RunningTurns,
	})
}

func (p *Process) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if p.snapshot().RunningTurns > 0 {
		writeError(w, http.StatusConflict, "server_busy", "server has running turns")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "shutting_down",
	})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	}()
}

func (p *Process) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p.handleSessionsList(w, r)
	case http.MethodPost:
		p.handleSessionsCreate(w, r)
	default:
		writeMethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
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
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		p.handleSessionDetail(w, r, id)
	case len(parts) == 2 && parts[1] == "items":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		p.handleSessionItems(w, r, id)
	case len(parts) == 4 && parts[1] == "items" && parts[3] == "content":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		p.handleSessionItemContent(w, r, id, parts[2])
	default:
		writeError(w, http.StatusNotFound, "not_found", "path not found")
	}
}

func (p *Process) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	store := p.sessionStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session store is not configured")
		return
	}
	infos, err := store.List()
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
		items = append(items, sessionMetadataDTO{
			ID:           info.ID,
			CreatedAt:    info.CreatedAt,
			UpdatedAt:    info.UpdatedAt,
			Provider:     info.Provider,
			ModelProfile: info.ModelProfile,
			ModelID:      info.ModelID,
			LastSeq:      session.LastSeq,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": items,
	})
}

func (p *Process) handleSessionsCreate(w http.ResponseWriter, r *http.Request) {
	store := p.sessionStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session store is not configured")
		return
	}
	if err := readEmptySessionCreateRequest(w, r); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	session := copySessionMetadata(p.sessionDefaults)
	session.ID = ""
	session.Version = sessions.VersionV2
	session.CreatedAt = time.Time{}
	session.UpdatedAt = time.Time{}
	session.Items = nil
	session.ActiveHistory = nil
	session.Compactions = nil
	session.LastSeq = 0
	session.SaveToolResults = true

	saved, err := store.SaveMetadata(session)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not create session")
		return
	}
	writeJSON(w, http.StatusCreated, sessionDetailDTOFromSession(saved, "idle"))
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
	writeJSON(w, http.StatusOK, sessionDetailDTOFromSession(session, "idle"))
}

func (p *Process) handleSessionItems(w http.ResponseWriter, r *http.Request, id string) {
	store := p.sessionStore
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "session_store_unavailable", "session store is not configured")
		return
	}
	if !validSessionAPIID(id) {
		writeError(w, http.StatusBadRequest, "invalid_session_id", "invalid session id")
		return
	}
	query, err := parseSessionItemsQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
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
	writeJSON(w, http.StatusOK, sessionItemsResponseDTO{
		Items:         items,
		HasMoreBefore: hasMoreBefore,
		HasMoreAfter:  hasMoreAfter,
	})
}

func (p *Process) handleSessionItemContent(w http.ResponseWriter, r *http.Request, id, itemID string) {
	query, err := parseSessionItemContentQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	if query.View == sessionItemsViewDebug && !p.hasRegistryToken(r) {
		writeError(w, http.StatusForbidden, "permission_denied", "permission denied")
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
	if strings.TrimSpace(itemID) == "" || itemID != strings.TrimSpace(itemID) {
		writeError(w, http.StatusNotFound, "item_not_found", "item not found")
		return
	}

	session, err := store.Load(id)
	if err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			writeError(w, http.StatusNotFound, "session_not_found", "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "session_store_error", "could not load session item content")
		return
	}
	item, ok := findSessionItemByID(session.Items, itemID)
	if !ok {
		writeError(w, http.StatusNotFound, "item_not_found", "item not found")
		return
	}
	if !sessionItemContentReadableInView(item, query.View) {
		writeError(w, http.StatusNotFound, "content_unavailable", "item content is not available")
		return
	}

	content, offset, sizeBytes, bytesReturned, hasMore := sessionItemContentRange(item.Message.Content, query.Offset, query.MaxBytes)
	writeJSON(w, http.StatusOK, sessionItemContentResponseDTO{
		ItemID:        item.ID,
		Content:       content,
		Offset:        offset,
		SizeBytes:     sizeBytes,
		BytesReturned: bytesReturned,
		HasMore:       hasMore,
	})
}

func (p *Process) snapshot() Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.info
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

type sessionMetadataDTO struct {
	ID           string    `json:"id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Provider     string    `json:"provider"`
	ModelProfile string    `json:"model_profile"`
	ModelID      string    `json:"model_id"`
	LastSeq      int64     `json:"last_seq"`
}

type sessionDetailDTO struct {
	ID              string                 `json:"id"`
	CreatedAt       time.Time              `json:"created_at"`
	UpdatedAt       time.Time              `json:"updated_at"`
	Provider        string                 `json:"provider"`
	ModelProfile    string                 `json:"model_profile"`
	ModelID         string                 `json:"model_id"`
	Status          string                 `json:"status"`
	LastSeq         int64                  `json:"last_seq"`
	CWD             string                 `json:"cwd,omitempty"`
	ConfigPath      string                 `json:"config_path,omitempty"`
	ModelParameters map[string]any         `json:"model_parameters,omitempty"`
	EnabledTools    []string               `json:"enabled_tools,omitempty"`
	EnabledMCP      []string               `json:"enabled_mcp,omitempty"`
	EnabledSkills   []string               `json:"enabled_skills,omitempty"`
	ShowReasoning   bool                   `json:"show_reasoning"`
	Context         contextwindow.Metadata `json:"context"`
	SaveToolResults bool                   `json:"save_tool_results"`
}

type sessionItemsResponseDTO struct {
	Items         []sessionItemDTO `json:"items"`
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
	Message    *sessionItemMessageDTO `json:"message,omitempty"`
}

type sessionItemMessageDTO struct {
	Role    model.MessageRole             `json:"role"`
	Content *sessionItemMessageContentDTO `json:"content,omitempty"`
}

type sessionItemMessageContentDTO struct {
	Inline    string `json:"inline,omitempty"`
	Preview   string `json:"preview,omitempty"`
	SizeBytes int    `json:"size_bytes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type sessionItemContentResponseDTO struct {
	ItemID        string `json:"item_id"`
	Content       string `json:"content"`
	Offset        int64  `json:"offset"`
	SizeBytes     int    `json:"size_bytes"`
	BytesReturned int    `json:"bytes_returned"`
	HasMore       bool   `json:"has_more"`
}

type sessionItemsQuery struct {
	BeforeSeq *int64
	AfterSeq  *int64
	Limit     int
	View      string
}

type sessionItemContentQuery struct {
	Offset   int64
	MaxBytes int
	View     string
}

func sessionDetailDTOFromSession(session sessions.SessionV2, status string) sessionDetailDTO {
	return sessionDetailDTO{
		ID:              session.ID,
		CreatedAt:       session.CreatedAt,
		UpdatedAt:       session.UpdatedAt,
		Provider:        session.Provider,
		ModelProfile:    session.ModelProfile,
		ModelID:         session.ModelID,
		Status:          status,
		LastSeq:         session.LastSeq,
		CWD:             session.CWD,
		ConfigPath:      session.RootConfigPath(),
		ModelParameters: copyMap(session.ModelParameters),
		EnabledTools:    copyStrings(session.EnabledTools),
		EnabledMCP:      copyStrings(session.EnabledMCP),
		EnabledSkills:   copyStrings(session.EnabledSkills),
		ShowReasoning:   session.ShowReasoning,
		Context:         session.Context,
		SaveToolResults: session.SaveToolResults,
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
	if item.Message == nil || item.Message.Content == "" {
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

func findSessionItemByID(items []sessions.SessionItem, id string) (sessions.SessionItem, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return sessions.SessionItem{}, false
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
	}
	if item.Message != nil {
		dto.Message = &sessionItemMessageDTO{
			Role: item.Message.Role,
		}
		if item.Message.Content != "" {
			dto.Message.Content = sessionItemMessageContentDTOFromString(item.Message.Content)
		}
	}
	return dto
}

func sessionItemMessageContentDTOFromString(content string) *sessionItemMessageContentDTO {
	if len(content) <= sessionItemInlineMessageBytes {
		return &sessionItemMessageContentDTO{
			Inline: content,
		}
	}
	return &sessionItemMessageContentDTO{
		Preview:   truncateStringByBytes(content, sessionItemPreviewMessageBytes),
		SizeBytes: len(content),
		Truncated: true,
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

func sessionItemContentRange(value string, offset int64, maxBytes int) (string, int64, int, int, bool) {
	sizeBytes := len(value)
	if offset >= int64(sizeBytes) {
		return "", int64(sizeBytes), sizeBytes, 0, false
	}
	start := int(offset)
	for start < sizeBytes && !utf8.RuneStart(value[start]) {
		start++
	}
	if start >= sizeBytes {
		return "", int64(start), sizeBytes, 0, false
	}
	end := start + maxBytes
	if end > sizeBytes {
		end = sizeBytes
	}
	for end > start && end < sizeBytes && !utf8.RuneStart(value[end]) {
		end--
	}
	content := value[start:end]
	return content, int64(start), sizeBytes, len(content), end < sizeBytes
}

func readEmptySessionCreateRequest(w http.ResponseWriter, r *http.Request) error {
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
	if len(value) != 0 {
		return fmt.Errorf("request body must be empty or {}")
	}
	return nil
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
