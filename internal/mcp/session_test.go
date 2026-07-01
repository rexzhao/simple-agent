package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/config"
)

func TestStartStdioSessionInitializesAndCloseExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, result, err := StartStdioSession(ctx, config.MCPServerConfig{
		ID:      "fake",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess", "--", "fake-mcp"},
		Env: map[string]string{
			"SAI_MCP_HELPER_PROCESS": "1",
		},
	})
	if err != nil {
		t.Fatalf("StartStdioSession() error = %v", err)
	}

	if result.ProtocolVersion != protocolVersion {
		t.Fatalf("protocol version = %q, want %q", result.ProtocolVersion, protocolVersion)
	}
	if result.ServerInfo.Name != "fake-mcp" {
		t.Fatalf("server name = %q, want fake-mcp", result.ServerInfo.Name)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if session.cmd.ProcessState == nil || !session.cmd.ProcessState.Exited() {
		t.Fatalf("server process did not exit after Close")
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("SAI_MCP_HELPER_PROCESS") != "1" {
		return
	}
	os.Exit(runFakeMCPServer())
}

func runFakeMCPServer() int {
	reader := bufio.NewReader(os.Stdin)
	payload, err := readMessageLine(reader)
	if err != nil {
		return 2
	}

	var request struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			ProtocolVersion string    `json:"protocolVersion"`
			ClientInfo      PartyInfo `json:"clientInfo"`
		} `json:"params"`
	}
	if err := json.Unmarshal(payload, &request); err != nil {
		return 3
	}
	if request.JSONRPC != "2.0" || request.Method != "initialize" || request.Params.ClientInfo.Name != "sai" {
		return 4
	}

	response := initializeResponse{
		JSONRPC: "2.0",
		ID:      request.ID,
		Result: InitializeResult{
			ProtocolVersion: request.Params.ProtocolVersion,
			Capabilities:    map[string]any{},
			ServerInfo: PartyInfo{
				Name:    "fake-mcp",
				Version: "test",
			},
		},
	}
	if err := writeTestMessage(os.Stdout, response); err != nil {
		return 5
	}

	payload, err = readMessageLine(reader)
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

	for {
		if _, err := readMessageLine(reader); err != nil {
			if errors.Is(err, io.EOF) {
				return 0
			}
			return 9
		}
	}
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
