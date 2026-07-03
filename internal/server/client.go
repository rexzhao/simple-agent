package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
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

// ShutdownServer sends POST /server/shutdown to a discovered server.
func ShutdownServer(ctx context.Context, addr string, timeout time.Duration) error {
	req, err := newServerClientRequest(ctx, http.MethodPost, addr, "/server/shutdown")
	if err != nil {
		return err
	}
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
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, fmt.Errorf("server addr is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return http.NewRequestWithContext(ctx, method, "http://"+addr+path, nil)
}

func clientTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 500 * time.Millisecond
	}
	return timeout
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
