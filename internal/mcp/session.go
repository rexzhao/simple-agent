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

	requestMu sync.Mutex
	nextID    int

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

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
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
		nextID: 1,
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
	var result InitializeResult
	if err := s.call(ctx, "initialize", initializeParams{
		ProtocolVersion: protocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo: PartyInfo{
			Name:    "sai",
			Version: "dev",
		},
	}, &result); err != nil {
		return InitializeResult{}, err
	}

	notification := rpcNotification{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
		Params:  map[string]any{},
	}
	if err := s.writeMessage(notification); err != nil {
		return InitializeResult{}, err
	}
	return result, nil
}

func (s *Session) ListTools(ctx context.Context) ([]ToolDefinition, error) {
	var tools []ToolDefinition
	cursor := ""
	for {
		page, nextCursor, err := s.listToolsPage(ctx, cursor)
		if err != nil {
			return nil, err
		}
		tools = append(tools, page...)
		if nextCursor == "" {
			return tools, nil
		}
		cursor = nextCursor
	}
}

func (s *Session) listToolsPage(ctx context.Context, cursor string) ([]ToolDefinition, string, error) {
	var params any
	if cursor != "" {
		params = listToolsParams{Cursor: cursor}
	}

	var result listToolsResult
	if err := s.call(ctx, "tools/list", params, &result); err != nil {
		return nil, "", err
	}
	return result.Tools, result.NextCursor, nil
}

func (s *Session) call(ctx context.Context, method string, params any, result any) error {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()

	id := s.nextID
	s.nextID++
	request := rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	if err := s.writeMessage(request); err != nil {
		return err
	}

	payload, err := s.readMessage(ctx)
	if err != nil {
		return err
	}

	var response rpcResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}
	if response.JSONRPC != "2.0" {
		return fmt.Errorf("%s response has jsonrpc %q", method, response.JSONRPC)
	}
	if response.ID != id {
		return fmt.Errorf("%s response id = %d, want %d", method, response.ID, id)
	}
	if response.Error != nil {
		return fmt.Errorf("%s response error %d: %s", method, response.Error.Code, response.Error.Message)
	}
	if result == nil {
		return nil
	}
	if len(response.Result) == 0 {
		return fmt.Errorf("%s response missing result", method)
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
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

type listToolsParams struct {
	Cursor string `json:"cursor,omitempty"`
}

type listToolsResult struct {
	Tools      []ToolDefinition `json:"tools"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
