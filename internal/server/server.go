package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const DefaultListenAddress = "127.0.0.1:0"

type Options struct {
	CWD        string
	ConfigPath string
	Listen     string
	Version    string
	Now        func() time.Time
}

type Process struct {
	httpServer *http.Server
	listener   net.Listener

	mu           sync.Mutex
	info         Info
	shutdownOnce sync.Once
	shutdownDone chan struct{}
	shutdownErr  error
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
		shutdownDone: make(chan struct{}),
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
	default:
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
	writeJSON(w, http.StatusOK, map[string]any{
		"cwd":            info.CWD,
		"config_path":    info.ConfigPath,
		"addr":           info.Addr,
		"pid":            info.PID,
		"version":        info.Version,
		"started_at":     info.StartedAt,
		"uptime_seconds": int64(time.Since(info.StartedAt).Seconds()),
		"session_count":  info.SessionCount,
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

func (p *Process) snapshot() Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.info
}

func writeMethodNotAllowed(w http.ResponseWriter, method string) {
	w.Header().Set("Allow", method)
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
