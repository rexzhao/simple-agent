package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
)

// ServerStatus is the client-facing shape returned by GET /server.
type ServerStatus struct {
	CWD           string    `json:"cwd"`
	ConfigPath    string    `json:"config_path"`
	Addr          string    `json:"addr"`
	PID           int       `json:"pid"`
	Version       string    `json:"version"`
	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	SessionCount  int       `json:"session_count"`
	RunningTurns  int       `json:"running_turns"`
}

// SessionMetadata is the client-facing session summary returned by GET /sessions.
type SessionMetadata struct {
	ID           string    `json:"id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Provider     string    `json:"provider"`
	ModelProfile string    `json:"model_profile"`
	ModelID      string    `json:"model_id"`
	LastSeq      int64     `json:"last_seq"`
}

// SessionDetail is the client-facing metadata shape returned by GET /sessions/{id}.
type SessionDetail struct {
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

// SessionMessageResult is the committed metadata returned by POST /sessions/{id}/messages.
type SessionMessageResult struct {
	TurnID  string `json:"turn_id"`
	LastSeq int64  `json:"last_seq"`
	Status  string `json:"status"`
}

// DiscoveryResult reports the nearest healthy server, plus stale records removed
// while searching from the requested cwd upward.
type DiscoveryResult struct {
	Record       RegistryRecord
	Found        bool
	StaleRemoved int
}

// DiscoverHealthy loads the registry, checks ancestor records nearest-first, and
// removes stale ancestor records before continuing the search.
func DiscoverHealthy(ctx context.Context, store RegistryStore, startCWD string, timeout time.Duration) (DiscoveryResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	records, err := store.Load()
	if err != nil {
		return DiscoveryResult{}, err
	}
	matches, err := AncestorRecords(startCWD, records)
	if err != nil {
		return DiscoveryResult{}, err
	}

	var result DiscoveryResult
	for _, record := range matches {
		if err := CheckHealth(ctx, record.Addr, timeout); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			removed, removeErr := store.RemoveIdentity(record.Identity())
			if removeErr != nil {
				return result, removeErr
			}
			if removed {
				result.StaleRemoved++
			}
			continue
		}
		result.Record = record
		result.Found = true
		return result, nil
	}
	return result, nil
}

// GetServerStatus fetches GET /server from a discovered server.
func GetServerStatus(ctx context.Context, addr string, timeout time.Duration) (ServerStatus, error) {
	req, err := newServerClientRequest(ctx, http.MethodGet, addr, "/server")
	if err != nil {
		return ServerStatus{}, err
	}
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

// ListSessions fetches public session metadata from GET /sessions.
func ListSessions(ctx context.Context, addr string, timeout time.Duration) ([]SessionMetadata, error) {
	req, err := newServerClientRequest(ctx, http.MethodGet, addr, "/sessions")
	if err != nil {
		return nil, err
	}
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

// GetSessionDetail fetches public session metadata from GET /sessions/{id}.
func GetSessionDetail(ctx context.Context, addr, id string, timeout time.Duration) (SessionDetail, error) {
	req, err := newServerClientRequest(ctx, http.MethodGet, addr, "/sessions/"+url.PathEscape(strings.TrimSpace(id)))
	if err != nil {
		return SessionDetail{}, err
	}
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

// CreateSessionWithToken sends POST /sessions with the registry bearer token.
func CreateSessionWithToken(ctx context.Context, addr, token string, timeout time.Duration) (SessionDetail, error) {
	req, err := newServerClientRequest(ctx, http.MethodPost, addr, "/sessions")
	if err != nil {
		return SessionDetail{}, err
	}
	setBearerToken(req, token)
	client := http.Client{Timeout: clientTimeout(timeout)}
	resp, err := client.Do(req)
	if err != nil {
		return SessionDetail{}, fmt.Errorf("create session at %s: %w", strings.TrimSpace(addr), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return SessionDetail{}, fmt.Errorf("create session at %s: %s", strings.TrimSpace(addr), serverWriteResponseError(resp))
	}
	var detail SessionDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return SessionDetail{}, fmt.Errorf("decode created session at %s: %w", strings.TrimSpace(addr), err)
	}
	return detail, nil
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

// ShutdownServer sends POST /server/shutdown to a discovered server.
func ShutdownServer(ctx context.Context, addr string, timeout time.Duration) error {
	return ShutdownServerWithToken(ctx, addr, "", timeout)
}

// ShutdownServerWithToken sends POST /server/shutdown with the registry bearer token.
func ShutdownServerWithToken(ctx context.Context, addr, token string, timeout time.Duration) error {
	req, err := newServerClientRequest(ctx, http.MethodPost, addr, "/server/shutdown")
	if err != nil {
		return err
	}
	setBearerToken(req, token)
	client := http.Client{Timeout: clientTimeout(timeout)}
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
