package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/config"
)

func TestStartStdioSessionInitializesListsToolsAndCloseExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, result, err := StartStdioSession(ctx, helperMCPServerConfig("fake-mcp", nil))
	if err != nil {
		t.Fatalf("StartStdioSession() error = %v", err)
	}

	if result.ProtocolVersion != protocolVersion {
		t.Fatalf("protocol version = %q, want %q", result.ProtocolVersion, protocolVersion)
	}
	if result.ServerInfo.Name != "fake-mcp" {
		t.Fatalf("server name = %q, want fake-mcp", result.ServerInfo.Name)
	}

	tools, err := session.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("ListTools() returned %d tools, want 2", len(tools))
	}
	if tools[0].Name != "first_tool" || tools[1].Name != "second_tool" {
		t.Fatalf("ListTools() names = %q, %q; want first_tool, second_tool", tools[0].Name, tools[1].Name)
	}
	if got := tools[0].InputSchema["type"]; got != "object" {
		t.Fatalf("first tool input schema type = %v, want object", got)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if session.cmd.ProcessState == nil || !session.cmd.ProcessState.Exited() {
		t.Fatalf("server process did not exit after Close")
	}
}

func TestSessionContextCancelKillsServerWithoutCloseError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	session, _, err := StartStdioSession(ctx, helperMCPServerConfig("stubborn-mcp", nil))
	if err != nil {
		t.Fatalf("StartStdioSession() error = %v", err)
	}

	cancel()
	waitForSessionExit(t, session, 2*time.Second)

	if err := session.Close(); err != nil {
		t.Fatalf("Close() after context cancel error = %v", err)
	}
	if session.cmd.ProcessState == nil {
		t.Fatal("server process was not reaped after context cancel")
	}
}

func TestCloseKillsServerThatIgnoresStdinClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, _, err := StartStdioSession(ctx, helperMCPServerConfig("stubborn-mcp", nil))
	if err != nil {
		t.Fatalf("StartStdioSession() error = %v", err)
	}

	start := time.Now()
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil after successful kill cleanup", err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("Close() took %v, want bounded wait under 1.5s", elapsed)
	}
	if session.cmd.ProcessState == nil {
		t.Fatal("server process was not reaped after Close kill fallback")
	}
}

func TestStartStdioSessionInitializationFailureClosesServer(t *testing.T) {
	exitFile := filepath.Join(t.TempDir(), "mcp-exited")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, err := StartStdioSession(ctx, helperMCPServerConfig("invalid-initialize", map[string]string{
		"SAI_MCP_EXIT_FILE": exitFile,
	}))
	if err == nil {
		t.Fatal("StartStdioSession() error = nil, want initialize failure")
	}
	waitForFile(t, exitFile, 2*time.Second)
}

func TestStartStdioSessionReadWaitCancelClosesServer(t *testing.T) {
	dir := t.TempDir()
	readyFile := filepath.Join(dir, "mcp-ready")
	exitFile := filepath.Join(dir, "mcp-exited")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := make(chan error, 1)
	go func() {
		session, _, err := StartStdioSession(ctx, helperMCPServerConfig("wait-initialize", map[string]string{
			"SAI_MCP_READY_FILE": readyFile,
			"SAI_MCP_EXIT_FILE":  exitFile,
		}))
		if session != nil {
			_ = session.Close()
		}
		errc <- err
	}()

	waitForFile(t, readyFile, 2*time.Second)
	cancel()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("StartStdioSession() error = nil, want cancel/read failure")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StartStdioSession() did not return after context cancel")
	}
	waitForFile(t, exitFile, 2*time.Second)
}

func helperMCPServerConfig(mode string, env map[string]string) config.MCPServerConfig {
	values := map[string]string{
		"SAI_MCP_HELPER_PROCESS": "1",
	}
	for name, value := range env {
		values[name] = value
	}
	return config.MCPServerConfig{
		ID:      "fake",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess", "--", mode},
		Env:     values,
	}
}

func waitForSessionExit(t *testing.T, session *Session, timeout time.Duration) {
	t.Helper()

	select {
	case <-session.waitDone:
	case <-time.After(timeout):
		t.Fatalf("server process did not exit within %v", timeout)
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %q was not created within %v", path, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("SAI_MCP_HELPER_PROCESS") != "1" {
		return
	}
	code := runFakeMCPServer(helperMode())
	writeHelperFile("SAI_MCP_EXIT_FILE")
	os.Exit(code)
}

func helperMode() string {
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return "fake-mcp"
}

func runFakeMCPServer(mode string) int {
	switch mode {
	case "fake-mcp":
		return runNormalFakeMCPServer()
	case "stubborn-mcp":
		return runStubbornFakeMCPServer()
	case "invalid-initialize":
		return runInvalidInitializeMCPServer()
	case "wait-initialize":
		return runWaitInitializeMCPServer()
	default:
		return 64
	}
}

func runNormalFakeMCPServer() int {
	reader := bufio.NewReader(os.Stdin)
	request, code := readTestInitializeRequest(reader)
	if code != 0 {
		return code
	}
	if code := writeTestInitializeResponse(request); code != 0 {
		return code
	}
	if code := readTestInitializedNotification(reader); code != 0 {
		return code
	}

	for {
		payload, err := readMessageLine(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0
			}
			return 9
		}

		var request struct {
			JSONRPC string `json:"jsonrpc"`
			ID      int    `json:"id"`
			Method  string `json:"method"`
			Params  struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		if err := json.Unmarshal(payload, &request); err != nil {
			return 10
		}
		if request.JSONRPC != "2.0" || request.Method != "tools/list" {
			return 11
		}

		result := listToolsResult{
			Tools: []ToolDefinition{
				{
					Name:        "first_tool",
					Description: "first test tool",
					InputSchema: map[string]any{
						"type": "object",
					},
				},
			},
			NextCursor: "second-page",
		}
		if request.Params.Cursor == "second-page" {
			result = listToolsResult{
				Tools: []ToolDefinition{
					{
						Name:        "second_tool",
						Description: "second test tool",
						InputSchema: map[string]any{
							"type": "object",
						},
					},
				},
			}
		} else if request.Params.Cursor != "" {
			return 12
		}

		response := rpcResponse{
			JSONRPC: "2.0",
			ID:      request.ID,
			Result:  mustMarshalJSON(result),
		}
		if err := writeTestMessage(os.Stdout, response); err != nil {
			return 13
		}
	}
}

func runStubbornFakeMCPServer() int {
	reader := bufio.NewReader(os.Stdin)
	request, code := readTestInitializeRequest(reader)
	if code != 0 {
		return code
	}
	if code := writeTestInitializeResponse(request); code != 0 {
		return code
	}
	if code := readTestInitializedNotification(reader); code != 0 {
		return code
	}

	for {
		time.Sleep(time.Hour)
	}
}

func runInvalidInitializeMCPServer() int {
	reader := bufio.NewReader(os.Stdin)
	if _, code := readTestInitializeRequest(reader); code != 0 {
		return code
	}
	writeHelperFile("SAI_MCP_READY_FILE")
	if _, err := io.WriteString(os.Stdout, "{not-json}\n"); err != nil {
		return 5
	}
	_, _ = io.Copy(io.Discard, reader)
	return 0
}

func runWaitInitializeMCPServer() int {
	reader := bufio.NewReader(os.Stdin)
	if _, code := readTestInitializeRequest(reader); code != 0 {
		return code
	}
	writeHelperFile("SAI_MCP_READY_FILE")
	_, _ = io.Copy(io.Discard, reader)
	return 0
}

type testInitializeRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  struct {
		ProtocolVersion string    `json:"protocolVersion"`
		ClientInfo      PartyInfo `json:"clientInfo"`
	} `json:"params"`
}

func readTestInitializeRequest(reader *bufio.Reader) (testInitializeRequest, int) {
	payload, err := readMessageLine(reader)
	if err != nil {
		return testInitializeRequest{}, 2
	}

	var request testInitializeRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return testInitializeRequest{}, 3
	}
	if request.JSONRPC != "2.0" || request.Method != "initialize" || request.Params.ClientInfo.Name != "sai" {
		return testInitializeRequest{}, 4
	}
	return request, 0
}

func writeTestInitializeResponse(request testInitializeRequest) int {
	response := rpcResponse{
		JSONRPC: "2.0",
		ID:      request.ID,
		Result: mustMarshalJSON(InitializeResult{
			ProtocolVersion: request.Params.ProtocolVersion,
			Capabilities:    map[string]any{},
			ServerInfo: PartyInfo{
				Name:    "fake-mcp",
				Version: "test",
			},
		}),
	}
	if err := writeTestMessage(os.Stdout, response); err != nil {
		return 5
	}
	return 0
}

func readTestInitializedNotification(reader *bufio.Reader) int {
	payload, err := readMessageLine(reader)
	if err != nil {
		return 6
	}
	var notification rpcNotification
	if err := json.Unmarshal(payload, &notification); err != nil {
		return 7
	}
	if notification.JSONRPC != "2.0" || notification.Method != "notifications/initialized" {
		return 8
	}
	return 0
}

func writeHelperFile(envName string) {
	path := os.Getenv(envName)
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte("1"), 0o644)
}

func mustMarshalJSON(value any) json.RawMessage {
	payload, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return payload
}

func writeTestMessage(w io.Writer, message any) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	_, err = w.Write(payload)
	return err
}
