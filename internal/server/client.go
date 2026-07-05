package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rexzhao/simple-agent/internal/contextwindow"
)

// ServerStatus is the client-facing shape returned by GET /server.
type ServerStatus struct {
	Addr          string    `json:"base_url"`
	PID           int       `json:"pid"`
	Version       string    `json:"version"`
	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	ProjectCount  int       `json:"project_count"`
	SessionCount  int       `json:"session_count"`
	RunningTurns  int       `json:"running_turns"`
}

// ProjectInfo is the client-facing metadata shape returned by project APIs.
type ProjectInfo struct {
	ID          string    `json:"id"`
	Root        string    `json:"root"`
	DisplayName string    `json:"display_name,omitempty"`
	Archived    bool      `json:"archived"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ProjectCreateResult reports whether POST /projects created new metadata or
// returned an existing duplicate canonical root.
type ProjectCreateResult struct {
	Project    ProjectInfo
	Created    bool
	StatusCode int
}

// ProjectRemoveResult reports the project affected by DELETE /projects/{id}.
type ProjectRemoveResult struct {
	Status          string `json:"status"`
	ID              string `json:"id"`
	RemovedSessions int    `json:"removed_sessions"`
	StatusCode      int    `json:"-"`
}

type ProjectMetadataUpdate struct {
	DisplayName *string `json:"display_name,omitempty"`
	Archived    *bool   `json:"archived,omitempty"`
}

// SessionMetadata is the client-facing session summary returned by GET /sessions.
type SessionMetadata struct {
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

// SessionDetail is the client-facing metadata shape returned by GET /sessions/{id}.
type SessionDetail struct {
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

// SessionCreateMetadata is the optional metadata accepted by
// POST /projects/{project_id}/sessions.
type SessionCreateMetadata struct {
	CreatedCWD      string                  `json:"created_cwd,omitempty"`
	ConfigPath      string                  `json:"config_path,omitempty"`
	Provider        string                  `json:"provider,omitempty"`
	ModelProfile    string                  `json:"model_profile,omitempty"`
	ModelID         string                  `json:"model_id,omitempty"`
	ModelParameters map[string]any          `json:"model_parameters,omitempty"`
	EnabledTools    []string                `json:"enabled_tools,omitempty"`
	EnabledMCP      []string                `json:"enabled_mcp,omitempty"`
	EnabledSkills   []string                `json:"enabled_skills,omitempty"`
	ShowReasoning   *bool                   `json:"show_reasoning,omitempty"`
	Context         *contextwindow.Metadata `json:"context,omitempty"`
	SaveToolResults *bool                   `json:"save_tool_results,omitempty"`
}

type SessionListOptions struct {
	Archived    bool
	AllProjects bool
}

type SessionMetadataUpdate struct {
	DisplayName *string `json:"display_name,omitempty"`
	Archived    *bool   `json:"archived,omitempty"`
}

type SessionItemsPage struct {
	Items         []SessionItem `json:"items"`
	OldestSeq     int64         `json:"oldest_seq"`
	NewestSeq     int64         `json:"newest_seq"`
	HasMoreBefore bool          `json:"has_more_before"`
	HasMoreAfter  bool          `json:"has_more_after"`
}

type SessionItem struct {
	Seq        int64               `json:"seq"`
	ID         string              `json:"id"`
	TurnID     string              `json:"turn_id,omitempty"`
	CreatedAt  time.Time           `json:"created_at"`
	Kind       string              `json:"kind"`
	Visibility string              `json:"visibility"`
	Audience   string              `json:"audience"`
	Message    *SessionItemMessage `json:"message,omitempty"`
}

type SessionItemMessage struct {
	Role    string                     `json:"role"`
	Content *SessionItemMessageContent `json:"content,omitempty"`
}

type SessionItemMessageContent struct {
	Inline    string              `json:"inline,omitempty"`
	Preview   string              `json:"preview,omitempty"`
	SizeBytes int64               `json:"size_bytes,omitempty"`
	Truncated bool                `json:"truncated,omitempty"`
	Blob      *SessionItemBlobRef `json:"blob,omitempty"`
}

type SessionItemBlobRef struct {
	Hash      string `json:"hash"`
	SizeBytes int64  `json:"size_bytes"`
	Encoding  string `json:"encoding"`
	MediaType string `json:"media_type,omitempty"`
}

type SessionStreamOptions struct {
	AfterSeq *int64
}

// SessionMessageResult is the committed metadata returned by POST /sessions/{id}/messages.
type SessionMessageResult struct {
	TurnID  string `json:"turn_id"`
	LastSeq int64  `json:"last_seq"`
	Status  string `json:"status"`
}

// SessionCompactResult is the committed metadata returned by POST /sessions/{id}/commands/compact.
type SessionCompactResult struct {
	Status        string `json:"status"`
	CompactionID  string `json:"compaction_id"`
	SummaryItemID string `json:"summary_item_id"`
	LastSeq       int64  `json:"last_seq"`
}

type ShutdownOptions struct {
	Wait    bool
	Timeout time.Duration
}

// DiscoveryResult reports the active healthy server, plus stale records removed
// while checking the selected server root.
type DiscoveryResult struct {
	Record       RegistryRecord
	Found        bool
	StaleRemoved int
}

// DiscoverHealthy loads the registry, checks the singleton record, and removes
// it when stale. startCWD is ignored.
func DiscoverHealthy(ctx context.Context, store RegistryStore, startCWD string, timeout time.Duration) (DiscoveryResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	records, err := store.Load()
	if err != nil {
		return DiscoveryResult{}, err
	}
	_ = startCWD

	var result DiscoveryResult
	if len(records) == 0 {
		return result, nil
	}
	record, err := CanonicalizeRegistryRecord(records[len(records)-1])
	if err != nil {
		return result, err
	}
	if err := CheckHealth(ctx, record.BaseURL, timeout); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, ctxErr
		}
		removed, removeErr := store.Clear()
		if removeErr != nil {
			return result, removeErr
		}
		if removed {
			result.StaleRemoved++
		}
		return result, nil
	}
	result.Record = record
	result.Found = true
	return result, nil
}

// GetServerStatus fetches GET /server from a discovered server with the registry bearer token.
func GetServerStatus(ctx context.Context, addr, token string, timeout time.Duration) (ServerStatus, error) {
	req, err := newServerClientRequest(ctx, http.MethodGet, addr, "/server")
	if err != nil {
		return ServerStatus{}, err
	}
	setBearerToken(req, token)
	client := http.Client{Timeout: clientTimeout(timeout)}
	resp, err := client.Do(req)
	if err != nil {
		return ServerStatus{}, fmt.Errorf("get server status at %s: %w", strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ServerStatus{}, fmt.Errorf("get server status at %s: %s", strings.TrimSpace(addr), serverResponseError(resp))
	}
	var status ServerStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return ServerStatus{}, fmt.Errorf("decode server status at %s: %w", strings.TrimSpace(addr), err)
	}
	return status, nil
}

// ListProjectsWithToken fetches non-archived project metadata from GET /projects.
func ListProjectsWithToken(ctx context.Context, addr, token string, timeout time.Duration) ([]ProjectInfo, error) {
	return ListProjectsWithOptions(ctx, addr, token, false, timeout)
}

func ListProjectsWithOptions(ctx context.Context, addr, token string, archived bool, timeout time.Duration) ([]ProjectInfo, error) {
	path := "/projects"
	if archived {
		values := url.Values{}
		values.Set("archived", "true")
		path += "?" + values.Encode()
	}
	req, err := newServerClientRequest(ctx, http.MethodGet, addr, path)
	if err != nil {
		return nil, err
	}
	setBearerToken(req, token)
	client := http.Client{Timeout: clientTimeout(timeout)}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list projects at %s: %w", strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list projects at %s: %s", strings.TrimSpace(addr), serverResponseError(resp))
	}
	var body struct {
		Projects []ProjectInfo `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode projects at %s: %w", strings.TrimSpace(addr), err)
	}
	return body.Projects, nil
}

// CreateProjectWithToken sends POST /projects with a root path and optional display name.
func CreateProjectWithToken(ctx context.Context, addr, token, root, displayName string, timeout time.Duration) (ProjectCreateResult, error) {
	payload, err := json.Marshal(struct {
		Root        string `json:"root"`
		DisplayName string `json:"display_name,omitempty"`
	}{
		Root:        root,
		DisplayName: displayName,
	})
	if err != nil {
		return ProjectCreateResult{}, fmt.Errorf("encode project create request")
	}
	req, err := newServerClientRequestWithBody(ctx, http.MethodPost, addr, "/projects", payload)
	if err != nil {
		return ProjectCreateResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	setBearerToken(req, token)
	client := http.Client{Timeout: clientTimeout(timeout)}
	resp, err := client.Do(req)
	if err != nil {
		return ProjectCreateResult{}, fmt.Errorf("create project at %s: %w", strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return ProjectCreateResult{}, fmt.Errorf("create project at %s: %s", strings.TrimSpace(addr), serverWriteResponseError(resp))
	}
	var project ProjectInfo
	if err := json.NewDecoder(resp.Body).Decode(&project); err != nil {
		return ProjectCreateResult{}, fmt.Errorf("decode project create result at %s: %w", strings.TrimSpace(addr), err)
	}
	return ProjectCreateResult{
		Project:    project,
		Created:    resp.StatusCode == http.StatusCreated,
		StatusCode: resp.StatusCode,
	}, nil
}

// GetProjectWithToken fetches one project from GET /projects/{id}.
func GetProjectWithToken(ctx context.Context, addr, token, id string, timeout time.Duration) (ProjectInfo, error) {
	req, err := newServerClientRequest(ctx, http.MethodGet, addr, "/projects/"+url.PathEscape(strings.TrimSpace(id)))
	if err != nil {
		return ProjectInfo{}, err
	}
	setBearerToken(req, token)
	client := http.Client{Timeout: clientTimeout(timeout)}
	resp, err := client.Do(req)
	if err != nil {
		return ProjectInfo{}, fmt.Errorf("get project %s at %s: %w", strings.TrimSpace(id), strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ProjectInfo{}, fmt.Errorf("get project %s at %s: %s", strings.TrimSpace(id), strings.TrimSpace(addr), serverResponseError(resp))
	}
	var project ProjectInfo
	if err := json.NewDecoder(resp.Body).Decode(&project); err != nil {
		return ProjectInfo{}, fmt.Errorf("decode project %s at %s: %w", strings.TrimSpace(id), strings.TrimSpace(addr), err)
	}
	return project, nil
}

func RenameProjectWithToken(ctx context.Context, addr, token, id, displayName string, timeout time.Duration) (ProjectInfo, error) {
	displayName = strings.TrimSpace(displayName)
	return UpdateProjectMetadataWithToken(ctx, addr, token, id, ProjectMetadataUpdate{DisplayName: &displayName}, timeout)
}

func ArchiveProjectWithToken(ctx context.Context, addr, token, id string, timeout time.Duration) (ProjectInfo, error) {
	archived := true
	return UpdateProjectMetadataWithToken(ctx, addr, token, id, ProjectMetadataUpdate{Archived: &archived}, timeout)
}

func UpdateProjectMetadataWithToken(ctx context.Context, addr, token, id string, update ProjectMetadataUpdate, timeout time.Duration) (ProjectInfo, error) {
	payload, err := marshalMetadataUpdate(update.DisplayName, update.Archived, "project")
	if err != nil {
		return ProjectInfo{}, err
	}
	id = strings.TrimSpace(id)
	req, err := newServerClientRequestWithBody(ctx, http.MethodPatch, addr, "/projects/"+url.PathEscape(id), payload)
	if err != nil {
		return ProjectInfo{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	setBearerToken(req, token)
	client := http.Client{Timeout: clientTimeout(timeout)}
	resp, err := client.Do(req)
	if err != nil {
		return ProjectInfo{}, fmt.Errorf("update project %s at %s: %w", id, strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ProjectInfo{}, fmt.Errorf("update project %s at %s: %s", id, strings.TrimSpace(addr), serverWriteResponseError(resp))
	}
	var project ProjectInfo
	if err := json.NewDecoder(resp.Body).Decode(&project); err != nil {
		return ProjectInfo{}, fmt.Errorf("decode updated project %s at %s: %w", id, strings.TrimSpace(addr), err)
	}
	return project, nil
}

// RemoveProjectWithToken sends DELETE /projects/{id}.
func RemoveProjectWithToken(ctx context.Context, addr, token, id string, timeout time.Duration) (ProjectRemoveResult, error) {
	id = strings.TrimSpace(id)
	req, err := newServerClientRequest(ctx, http.MethodDelete, addr, "/projects/"+url.PathEscape(id))
	if err != nil {
		return ProjectRemoveResult{}, err
	}
	setBearerToken(req, token)
	client := http.Client{Timeout: clientTimeout(timeout)}
	resp, err := client.Do(req)
	if err != nil {
		return ProjectRemoveResult{}, fmt.Errorf("remove project %s at %s: %w", id, strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ProjectRemoveResult{}, fmt.Errorf("remove project %s at %s: %s", id, strings.TrimSpace(addr), serverWriteResponseError(resp))
	}
	var result ProjectRemoveResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ProjectRemoveResult{}, fmt.Errorf("decode project remove result %s at %s: %w", id, strings.TrimSpace(addr), err)
	}
	result.StatusCode = resp.StatusCode
	return result, nil
}

// ListProjectSessionsWithToken fetches session metadata from GET /projects/{id}/sessions.
func ListProjectSessionsWithToken(ctx context.Context, addr, token, projectID string, timeout time.Duration) ([]SessionMetadata, error) {
	return ListProjectSessionsWithOptions(ctx, addr, token, projectID, SessionListOptions{}, timeout)
}

// ListProjectSessionsWithOptions fetches session metadata from GET /projects/{id}/sessions.
func ListProjectSessionsWithOptions(ctx context.Context, addr, token, projectID string, options SessionListOptions, timeout time.Duration) ([]SessionMetadata, error) {
	projectID = strings.TrimSpace(projectID)
	req, err := newServerClientRequest(ctx, http.MethodGet, addr, sessionListPath("/projects/"+url.PathEscape(projectID)+"/sessions", options))
	if err != nil {
		return nil, err
	}
	setBearerToken(req, token)
	client := http.Client{Timeout: clientTimeout(timeout)}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list project sessions %s at %s: %w", projectID, strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list project sessions %s at %s: %s", projectID, strings.TrimSpace(addr), serverResponseError(resp))
	}
	var body struct {
		Sessions []SessionMetadata `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode project sessions %s at %s: %w", projectID, strings.TrimSpace(addr), err)
	}
	return body.Sessions, nil
}

// CreateProjectSessionWithToken sends POST /projects/{id}/sessions with the registry bearer token.
func CreateProjectSessionWithToken(ctx context.Context, addr, token, projectID string, timeout time.Duration) (SessionDetail, error) {
	return createProjectSessionWithPayload(ctx, addr, token, projectID, nil, timeout)
}

// CreateProjectSessionWithMetadataWithToken sends POST /projects/{id}/sessions
// with optional session creation metadata.
func CreateProjectSessionWithMetadataWithToken(ctx context.Context, addr, token, projectID string, metadata SessionCreateMetadata, timeout time.Duration) (SessionDetail, error) {
	payload, err := marshalSessionCreateMetadata(metadata)
	if err != nil {
		return SessionDetail{}, err
	}
	return createProjectSessionWithPayload(ctx, addr, token, projectID, payload, timeout)
}

func createProjectSessionWithPayload(ctx context.Context, addr, token, projectID string, payload []byte, timeout time.Duration) (SessionDetail, error) {
	projectID = strings.TrimSpace(projectID)
	req, err := newServerClientRequestWithBody(ctx, http.MethodPost, addr, "/projects/"+url.PathEscape(projectID)+"/sessions", payload)
	if err != nil {
		return SessionDetail{}, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	setBearerToken(req, token)
	client := http.Client{}
	if timeout > 0 {
		client.Timeout = timeout
	}
	resp, err := client.Do(req)
	if err != nil {
		return SessionDetail{}, fmt.Errorf("create project session %s at %s: %w", projectID, strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return SessionDetail{}, fmt.Errorf("create project session %s at %s: %s", projectID, strings.TrimSpace(addr), serverWriteResponseError(resp))
	}
	var detail SessionDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return SessionDetail{}, fmt.Errorf("decode created project session %s at %s: %w", projectID, strings.TrimSpace(addr), err)
	}
	return detail, nil
}

func marshalSessionCreateMetadata(metadata SessionCreateMetadata) ([]byte, error) {
	payload := make(map[string]any)
	if value := strings.TrimSpace(metadata.CreatedCWD); value != "" {
		payload["created_cwd"] = value
	}
	if value := strings.TrimSpace(metadata.ConfigPath); value != "" {
		payload["config_path"] = value
	}
	if value := strings.TrimSpace(metadata.Provider); value != "" {
		payload["provider"] = value
	}
	if value := strings.TrimSpace(metadata.ModelProfile); value != "" {
		payload["model_profile"] = value
	}
	if value := strings.TrimSpace(metadata.ModelID); value != "" {
		payload["model_id"] = value
	}
	if metadata.ModelParameters != nil {
		payload["model_parameters"] = metadata.ModelParameters
	}
	if metadata.EnabledTools != nil {
		payload["enabled_tools"] = metadata.EnabledTools
	}
	if metadata.EnabledMCP != nil {
		payload["enabled_mcp"] = metadata.EnabledMCP
	}
	if metadata.EnabledSkills != nil {
		payload["enabled_skills"] = metadata.EnabledSkills
	}
	if metadata.ShowReasoning != nil {
		payload["show_reasoning"] = *metadata.ShowReasoning
	}
	if metadata.Context != nil {
		payload["context"] = *metadata.Context
	}
	if metadata.SaveToolResults != nil {
		payload["save_tool_results"] = *metadata.SaveToolResults
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode project session create request")
	}
	return data, nil
}

// ListSessions fetches session metadata from GET /sessions with the registry bearer token.
func ListSessions(ctx context.Context, addr, token string, timeout time.Duration) ([]SessionMetadata, error) {
	return ListSessionsWithOptions(ctx, addr, token, SessionListOptions{AllProjects: true}, timeout)
}

// ListSessionsWithOptions fetches session metadata from GET /sessions.
func ListSessionsWithOptions(ctx context.Context, addr, token string, options SessionListOptions, timeout time.Duration) ([]SessionMetadata, error) {
	options.AllProjects = true
	req, err := newServerClientRequest(ctx, http.MethodGet, addr, sessionListPath("/sessions", options))
	if err != nil {
		return nil, err
	}
	setBearerToken(req, token)
	client := http.Client{Timeout: clientTimeout(timeout)}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list sessions at %s: %w", strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list sessions at %s: %s", strings.TrimSpace(addr), serverResponseError(resp))
	}
	var body struct {
		Sessions []SessionMetadata `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode sessions at %s: %w", strings.TrimSpace(addr), err)
	}
	return body.Sessions, nil
}

func sessionListPath(path string, options SessionListOptions) string {
	values := url.Values{}
	if options.AllProjects {
		values.Set("all_projects", "true")
	}
	if options.Archived {
		values.Set("archived", "true")
	}
	if len(values) == 0 {
		return path
	}
	return path + "?" + values.Encode()
}

// GetSessionDetail fetches session metadata from GET /sessions/{id} with the registry bearer token.
func GetSessionDetail(ctx context.Context, addr, token, id string, timeout time.Duration) (SessionDetail, error) {
	req, err := newServerClientRequest(ctx, http.MethodGet, addr, "/sessions/"+url.PathEscape(strings.TrimSpace(id)))
	if err != nil {
		return SessionDetail{}, err
	}
	setBearerToken(req, token)
	client := http.Client{Timeout: clientTimeout(timeout)}
	resp, err := client.Do(req)
	if err != nil {
		return SessionDetail{}, fmt.Errorf("get session %s at %s: %w", strings.TrimSpace(id), strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SessionDetail{}, fmt.Errorf("get session %s at %s: %s", strings.TrimSpace(id), strings.TrimSpace(addr), serverResponseError(resp))
	}
	var detail SessionDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return SessionDetail{}, fmt.Errorf("decode session %s at %s: %w", strings.TrimSpace(id), strings.TrimSpace(addr), err)
	}
	return detail, nil
}

// GetSessionChatItemsWithToken fetches the recent display transcript page for a session.
func GetSessionChatItemsWithToken(ctx context.Context, addr, token, id string, timeout time.Duration) (SessionItemsPage, error) {
	id = strings.TrimSpace(id)
	values := url.Values{}
	values.Set("view", "chat")
	values.Set("limit", "50")
	req, err := newServerClientRequest(ctx, http.MethodGet, addr, "/sessions/"+url.PathEscape(id)+"/items?"+values.Encode())
	if err != nil {
		return SessionItemsPage{}, err
	}
	setBearerToken(req, token)
	client := http.Client{Timeout: clientTimeout(timeout)}
	resp, err := client.Do(req)
	if err != nil {
		return SessionItemsPage{}, fmt.Errorf("get session items %s at %s: %w", id, strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SessionItemsPage{}, fmt.Errorf("get session items %s at %s: %s", id, strings.TrimSpace(addr), serverResponseError(resp))
	}
	var page SessionItemsPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return SessionItemsPage{}, fmt.Errorf("decode session items %s at %s: %w", id, strings.TrimSpace(addr), err)
	}
	return page, nil
}

func RenameSessionWithToken(ctx context.Context, addr, token, id, displayName string, timeout time.Duration) (SessionDetail, error) {
	displayName = strings.TrimSpace(displayName)
	return UpdateSessionMetadataWithToken(ctx, addr, token, id, SessionMetadataUpdate{DisplayName: &displayName}, timeout)
}

func ArchiveSessionWithToken(ctx context.Context, addr, token, id string, timeout time.Duration) (SessionDetail, error) {
	archived := true
	return UpdateSessionMetadataWithToken(ctx, addr, token, id, SessionMetadataUpdate{Archived: &archived}, timeout)
}

func UpdateSessionMetadataWithToken(ctx context.Context, addr, token, id string, update SessionMetadataUpdate, timeout time.Duration) (SessionDetail, error) {
	payload, err := marshalMetadataUpdate(update.DisplayName, update.Archived, "session")
	if err != nil {
		return SessionDetail{}, err
	}
	req, err := newServerClientRequestWithBody(ctx, http.MethodPatch, addr, "/sessions/"+url.PathEscape(strings.TrimSpace(id)), payload)
	if err != nil {
		return SessionDetail{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	setBearerToken(req, token)
	client := http.Client{}
	if timeout > 0 {
		client.Timeout = timeout
	}
	resp, err := client.Do(req)
	if err != nil {
		return SessionDetail{}, fmt.Errorf("update session %s at %s: %w", strings.TrimSpace(id), strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SessionDetail{}, fmt.Errorf("update session %s at %s: %s", strings.TrimSpace(id), strings.TrimSpace(addr), serverWriteResponseError(resp))
	}
	var detail SessionDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return SessionDetail{}, fmt.Errorf("decode updated session %s at %s: %w", strings.TrimSpace(id), strings.TrimSpace(addr), err)
	}
	return detail, nil
}

func marshalMetadataUpdate(displayName *string, archived *bool, resource string) ([]byte, error) {
	payload := make(map[string]any)
	if displayName != nil {
		payload["display_name"] = *displayName
	}
	if archived != nil {
		payload["archived"] = *archived
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("%s metadata update requires display_name or archived", resource)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode %s metadata update request", resource)
	}
	return data, nil
}

func RemoveSessionWithToken(ctx context.Context, addr, token, id string, timeout time.Duration) (ProjectRemoveResult, error) {
	id = strings.TrimSpace(id)
	req, err := newServerClientRequest(ctx, http.MethodDelete, addr, "/sessions/"+url.PathEscape(id))
	if err != nil {
		return ProjectRemoveResult{}, err
	}
	setBearerToken(req, token)
	client := http.Client{Timeout: clientTimeout(timeout)}
	resp, err := client.Do(req)
	if err != nil {
		return ProjectRemoveResult{}, fmt.Errorf("remove session %s at %s: %w", id, strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ProjectRemoveResult{}, fmt.Errorf("remove session %s at %s: %s", id, strings.TrimSpace(addr), serverWriteResponseError(resp))
	}
	var result ProjectRemoveResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ProjectRemoveResult{}, fmt.Errorf("decode session remove result %s at %s: %w", id, strings.TrimSpace(addr), err)
	}
	result.StatusCode = resp.StatusCode
	return result, nil
}

// SendSessionMessageWithToken sends POST /sessions/{id}/messages with the registry bearer token.
// A zero timeout leaves the HTTP client without a fixed timeout so long-running
// turns are governed by ctx cancellation.
func SendSessionMessageWithToken(ctx context.Context, addr, token, id, content string, timeout time.Duration) (SessionMessageResult, error) {
	payload, err := json.Marshal(struct {
		Content string `json:"content"`
	}{Content: content})
	if err != nil {
		return SessionMessageResult{}, fmt.Errorf("encode session message request")
	}
	req, err := newServerClientRequestWithBody(ctx, http.MethodPost, addr, "/sessions/"+url.PathEscape(strings.TrimSpace(id))+"/messages", payload)
	if err != nil {
		return SessionMessageResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	setBearerToken(req, token)
	client := http.Client{}
	if timeout > 0 {
		client.Timeout = timeout
	}
	resp, err := client.Do(req)
	if err != nil {
		return SessionMessageResult{}, fmt.Errorf("send message to session %s at %s: %w", strings.TrimSpace(id), strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SessionMessageResult{}, fmt.Errorf("send message to session %s at %s: %s", strings.TrimSpace(id), strings.TrimSpace(addr), serverWriteResponseError(resp))
	}
	var result SessionMessageResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return SessionMessageResult{}, fmt.Errorf("decode send result for session %s at %s: %w", strings.TrimSpace(id), strings.TrimSpace(addr), err)
	}
	return result, nil
}

// CompactSessionWithToken sends POST /sessions/{id}/commands/compact with the registry bearer token.
func CompactSessionWithToken(ctx context.Context, addr, token, id string, timeout time.Duration) (SessionCompactResult, error) {
	req, err := newServerClientRequest(ctx, http.MethodPost, addr, "/sessions/"+url.PathEscape(strings.TrimSpace(id))+"/commands/compact")
	if err != nil {
		return SessionCompactResult{}, err
	}
	setBearerToken(req, token)
	client := http.Client{}
	if timeout > 0 {
		client.Timeout = timeout
	}
	resp, err := client.Do(req)
	if err != nil {
		return SessionCompactResult{}, fmt.Errorf("compact session %s at %s: %w", strings.TrimSpace(id), strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SessionCompactResult{}, fmt.Errorf("compact session %s at %s: %s", strings.TrimSpace(id), strings.TrimSpace(addr), serverWriteResponseError(resp))
	}
	var result SessionCompactResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return SessionCompactResult{}, fmt.Errorf("decode compact result for session %s at %s: %w", strings.TrimSpace(id), strings.TrimSpace(addr), err)
	}
	return result, nil
}

// StreamSessionEvents connects to WS /sessions/{id}/stream with the registry bearer token and decodes JSON events.
func StreamSessionEvents(ctx context.Context, addr, token, id string, timeout time.Duration) (<-chan SessionStreamEvent, <-chan error, func(), error) {
	return StreamSessionEventsWithOptions(ctx, addr, token, id, SessionStreamOptions{}, timeout)
}

// StreamSessionEventsWithOptions connects to WS /sessions/{id}/stream and applies optional catch-up cursors.
func StreamSessionEventsWithOptions(ctx context.Context, addr, token, id string, options SessionStreamOptions, timeout time.Duration) (<-chan SessionStreamEvent, <-chan error, func(), error) {
	target, err := sessionStreamURL(addr, id, options)
	if err != nil {
		return nil, nil, nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	streamCtx, cancel := context.WithCancel(ctx)
	dialer := websocket.Dialer{
		HandshakeTimeout: clientTimeout(timeout),
	}
	headers := http.Header{}
	if strings.TrimSpace(token) != "" {
		headers.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	conn, resp, err := dialer.DialContext(streamCtx, target, headers)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			return nil, nil, nil, fmt.Errorf("connect session stream %s at %s: %s", strings.TrimSpace(id), strings.TrimSpace(addr), serverWriteResponseError(resp))
		}
		cancel()
		return nil, nil, nil, fmt.Errorf("connect session stream %s at %s: %w", strings.TrimSpace(id), strings.TrimSpace(addr), err)
	}

	events := make(chan SessionStreamEvent)
	errs := make(chan error, 1)
	closeStream := func() {
		cancel()
		_ = conn.Close()
	}
	go func() {
		defer close(errs)
		defer close(events)
		defer conn.Close()
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				if streamCtx.Err() != nil || websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					return
				}
				errs <- fmt.Errorf("read session stream %s at %s: %w", strings.TrimSpace(id), strings.TrimSpace(addr), err)
				return
			}
			var event SessionStreamEvent
			if err := json.Unmarshal(payload, &event); err != nil {
				errs <- fmt.Errorf("decode session stream event for session %s at %s: %w", strings.TrimSpace(id), strings.TrimSpace(addr), err)
				return
			}
			select {
			case events <- event:
			case <-streamCtx.Done():
				return
			}
		}
	}()
	return events, errs, closeStream, nil
}

// ShutdownServer sends POST /server/shutdown to a discovered server.
func ShutdownServer(ctx context.Context, addr string, timeout time.Duration) error {
	return ShutdownServerWithToken(ctx, addr, "", timeout)
}

// ShutdownServerWithToken sends POST /server/shutdown with the registry bearer token.
func ShutdownServerWithToken(ctx context.Context, addr, token string, timeout time.Duration) error {
	return ShutdownServerWithOptions(ctx, addr, token, ShutdownOptions{}, timeout)
}

// ShutdownServerWithOptions sends POST /server/shutdown with optional drain mode.
func ShutdownServerWithOptions(ctx context.Context, addr, token string, options ShutdownOptions, timeout time.Duration) error {
	path := shutdownPath(options)
	req, err := newServerClientRequest(ctx, http.MethodPost, addr, path)
	if err != nil {
		return err
	}
	setBearerToken(req, token)
	client := http.Client{Timeout: clientTimeout(timeout)}
	if options.Wait && timeout <= 0 {
		client.Timeout = 0
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("shutdown server at %s: %w", strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("shutdown server at %s: %s", strings.TrimSpace(addr), serverResponseError(resp))
	}
	return nil
}

func shutdownPath(options ShutdownOptions) string {
	if !options.Wait {
		return "/server/shutdown"
	}
	values := url.Values{}
	values.Set("wait", "true")
	if options.Timeout > 0 {
		values.Set("timeout_ms", strconv.FormatInt(int64(options.Timeout/time.Millisecond), 10))
	}
	return "/server/shutdown?" + values.Encode()
}

func newServerClientRequest(ctx context.Context, method, addr, path string) (*http.Request, error) {
	return newServerClientRequestWithBody(ctx, method, addr, path, nil)
}

func newServerClientRequestWithBody(ctx context.Context, method, addr, path string, body []byte) (*http.Request, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, fmt.Errorf("server addr is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return http.NewRequestWithContext(ctx, method, "http://"+addr+path, bytes.NewReader(body))
}

func sessionStreamURL(addr, id string, options SessionStreamOptions) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", fmt.Errorf("server addr is required")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("session id is required")
	}
	path := "/sessions/" + url.PathEscape(id) + "/stream"
	if options.AfterSeq != nil {
		values := url.Values{}
		values.Set("after_seq", strconv.FormatInt(*options.AfterSeq, 10))
		path += "?" + values.Encode()
	}
	return "ws://" + addr + path, nil
}

func clientTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 500 * time.Millisecond
	}
	return timeout
}

func setBearerToken(req *http.Request, token string) {
	token = strings.TrimSpace(token)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func serverResponseError(resp *http.Response) string {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
		code := strings.TrimSpace(body.Error.Code)
		message := strings.TrimSpace(body.Error.Message)
		if code != "" && message != "" {
			return fmt.Sprintf("status %d (%s: %s)", resp.StatusCode, code, message)
		}
		if code != "" {
			return fmt.Sprintf("status %d (%s)", resp.StatusCode, code)
		}
	}
	return fmt.Sprintf("status %d", resp.StatusCode)
}

func serverWriteResponseError(resp *http.Response) string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
		code := strings.TrimSpace(body.Error.Code)
		if code != "" {
			return fmt.Sprintf("status %d (%s)", resp.StatusCode, code)
		}
	}
	return fmt.Sprintf("status %d", resp.StatusCode)
}
