package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/config"
)

const protocolVersion = "2024-11-05"

type Session struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	closeOnce sync.Once
	closeErr  error
}

type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      PartyInfo      `json:"serverInfo"`
}

type PartyInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

func StartStdioSession(ctx context.Context, server config.MCPServerConfig) (*Session, InitializeResult, error) {
	if strings.TrimSpace(server.Command) == "" {
		return nil, InitializeResult{}, fmt.Errorf("MCP server %q command must not be blank", server.ID)
	}

	cmd := exec.Command(server.Command, server.Args...)
	cmd.Env = append(os.Environ(), envList(server.Env)...)
	cmd.Stderr = io.Discard

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, InitializeResult{}, fmt.Errorf("open MCP server %q stdin: %w", server.ID, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, InitializeResult{}, fmt.Errorf("open MCP server %q stdout: %w", server.ID, err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, InitializeResult{}, fmt.Errorf("start MCP server %q: %w", server.ID, err)
	}

	session := &Session{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
	}
	result, err := session.initialize(ctx)
	if err != nil {
		_ = session.Close()
		return nil, InitializeResult{}, fmt.Errorf("initialize MCP server %q: %w", server.ID, err)
	}
	return session, result, nil
}

func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		if s.stdin != nil {
			_ = s.stdin.Close()
		}

		done := make(chan error, 1)
		go func() {
			done <- s.cmd.Wait()
		}()

		select {
		case err := <-done:
			s.closeErr = err
		case <-time.After(2 * time.Second):
			if s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
			s.closeErr = <-done
		}
	})
	return s.closeErr
}

func (s *Session) initialize(ctx context.Context) (InitializeResult, error) {
	request := rpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params: initializeParams{
			ProtocolVersion: protocolVersion,
			Capabilities:    map[string]any{},
			ClientInfo: PartyInfo{
				Name:    "sai",
				Version: "dev",
			},
		},
	}
	if err := s.writeMessage(request); err != nil {
		return InitializeResult{}, err
	}

	payload, err := s.readMessage(ctx)
	if err != nil {
		return InitializeResult{}, err
	}

	var response initializeResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return InitializeResult{}, fmt.Errorf("decode initialize response: %w", err)
	}
	if response.Error != nil {
		return InitializeResult{}, fmt.Errorf("initialize response error %d: %s", response.Error.Code, response.Error.Message)
	}
	if response.JSONRPC != "2.0" {
		return InitializeResult{}, fmt.Errorf("initialize response has jsonrpc %q", response.JSONRPC)
	}

	notification := rpcNotification{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
		Params:  map[string]any{},
	}
	if err := s.writeMessage(notification); err != nil {
		return InitializeResult{}, err
	}
	return response.Result, nil
}

func (s *Session) writeMessage(message any) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode MCP message: %w", err)
	}
	payload = append(payload, '\n')
	if _, err := s.stdin.Write(payload); err != nil {
		return fmt.Errorf("write MCP message: %w", err)
	}
	return nil
}

func (s *Session) readMessage(ctx context.Context) ([]byte, error) {
	type readResult struct {
		payload []byte
		err     error
	}
	result := make(chan readResult, 1)
	go func() {
		payload, err := readMessageLine(s.stdout)
		result <- readResult{payload: payload, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case read := <-result:
		if read.err != nil {
			return nil, read.err
		}
		return read.payload, nil
	}
}

func readMessageLine(reader *bufio.Reader) ([]byte, error) {
	payload, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read MCP message: %w", err)
	}
	return payload, nil
}

func envList(env map[string]string) []string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)

	values := make([]string, 0, len(names))
	for _, name := range names {
		values = append(values, name+"="+env[name])
	}
	return values
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      PartyInfo      `json:"clientInfo"`
}

type initializeResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      int              `json:"id"`
	Result  InitializeResult `json:"result"`
	Error   *rpcError        `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
