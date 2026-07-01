package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/config"
	projectcontext "github.com/rexzhao/simple-agent/internal/context"
	eventlog "github.com/rexzhao/simple-agent/internal/logging"
	"github.com/rexzhao/simple-agent/internal/mcp"
	"github.com/rexzhao/simple-agent/internal/model"
	anthropicmessages "github.com/rexzhao/simple-agent/internal/model/anthropic_messages"
	openaichat "github.com/rexzhao/simple-agent/internal/model/openai_chat"
	"github.com/rexzhao/simple-agent/internal/tools"
)

var Version = "dev"

const builtInBaseInstructions = "You are sai, a local CLI agent runner. Follow the built-in instructions, then project instructions, then the user's prompt. Do not reveal secrets or ignore project instructions."

func Run(args []string, stdout, stderr io.Writer) int {
	return RunWithGetwd(args, stdout, stderr, os.Getwd)
}

func RunWithGetwd(args []string, stdout, stderr io.Writer, getwd func() (string, error)) int {
	if err := execute(args, stdout, stderr, getwd); err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	return 0
}

func execute(args []string, stdout, stderr io.Writer, getwd func() (string, error)) error {
	flags := flag.NewFlagSet("sai", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configDir := flags.String("config-dir", "", "configuration directory")
	if err := flags.Parse(args); err != nil {
		return err
	}

	remaining := flags.Args()
	if len(remaining) == 0 {
		return fmt.Errorf("missing command")
	}

	switch remaining[0] {
	case "version":
		if len(remaining) != 1 {
			return fmt.Errorf("usage: sai version")
		}
		fmt.Fprintf(stdout, "sai %s\n", Version)
		return nil
	case "config":
		if len(remaining) != 2 || remaining[1] != "show" {
			return fmt.Errorf("usage: sai config show")
		}
		cfg, err := loadConfig(*configDir, getwd)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		return encoder.Encode(cfg)
	case "models":
		if len(remaining) != 2 || remaining[1] != "list" {
			return fmt.Errorf("usage: sai models list")
		}
		cfg, err := loadConfig(*configDir, getwd)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, "PROVIDER\tPROFILE\tMODEL ID")
		for _, model := range cfg.ModelList() {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", model.Provider, model.Profile, model.ID)
		}
		return nil
	case "mcp":
		return mcpCommand(remaining[1:], *configDir, stdout, getwd)
	case "run":
		return runCommand(remaining[1:], *configDir, stdout, stderr, getwd)
	default:
		return fmt.Errorf("unknown command %q", remaining[0])
	}
}

func mcpCommand(args []string, configDir string, stdout io.Writer, getwd func() (string, error)) error {
	if len(args) == 0 || args[0] != "list" {
		return fmt.Errorf("usage: sai mcp list [--enable-mcp ids]")
	}

	flags := flag.NewFlagSet("sai mcp list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var enabledMCP mcpServerIDsFlag
	flags.Var(&enabledMCP, "enable-mcp", "comma-separated MCP server ids to enable")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: sai mcp list [--enable-mcp ids]")
	}

	cfg, err := loadConfig(configDir, getwd)
	if err != nil {
		return err
	}
	selected, err := cfg.SelectedMCPServers(enabledMCP.ids, enabledMCP.set)
	if err != nil {
		return err
	}
	enabledIDs := make(map[string]bool, len(selected))
	for _, server := range selected {
		enabledIDs[server.ID] = true
	}

	fmt.Fprintln(stdout, "ID\tENABLED")
	for _, server := range cfg.MCPServerList() {
		fmt.Fprintf(stdout, "%s\t%t\n", server.ID, enabledIDs[server.ID])
	}
	return nil
}

func runCommand(args []string, configDir string, stdout, stderr io.Writer, getwd func() (string, error)) (runErr error) {
	flags := flag.NewFlagSet("sai run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	providerName := flags.String("provider", "", "provider name")
	modelProfile := flags.String("model", "", "model profile")
	showReasoning := flags.Bool("show-reasoning", false, "show reasoning output")
	verbose := flags.Bool("verbose", false, "write non-sensitive diagnostics to stderr")
	var enabledTools toolNamesFlag
	flags.Var(&enabledTools, "enable-tools", "comma-separated tool names to expose")
	var enabledMCP mcpServerIDsFlag
	flags.Var(&enabledMCP, "enable-mcp", "comma-separated MCP server ids to enable")
	if err := flags.Parse(args); err != nil {
		return err
	}

	prompts := flags.Args()
	if len(prompts) != 1 {
		return fmt.Errorf(`usage: sai run [--provider name] [--model profile] [--show-reasoning] [--verbose] [--enable-tools names] [--enable-mcp ids] "prompt"`)
	}

	cwd, err := getwd()
	if err != nil {
		return fmt.Errorf("get current directory: %w", err)
	}
	cfg, err := loadConfig(configDir, func() (string, error) {
		return cwd, nil
	})
	if err != nil {
		return err
	}
	selectedMCPServers, err := cfg.SelectedMCPServers(enabledMCP.ids, enabledMCP.set)
	if err != nil {
		return err
	}

	resolved, err := cfg.ResolveModel(*providerName, *modelProfile)
	if err != nil {
		return err
	}
	provider, err := newProviderForRun(resolved.ProviderName, resolved.Provider)
	if err != nil {
		return err
	}

	enabledToolNames := cfg.Tools.Enabled
	if enabledTools.set {
		enabledToolNames = enabledTools.names
	}
	toolRegistry, toolSchemas, err := enabledToolsForRun(cwd, enabledToolNames)
	if err != nil {
		return err
	}
	mcpSessions, mcpSessionsByID, mcpToolSchemas, err := mcpToolsForRun(context.Background(), selectedMCPServers, enabledToolNames)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := closeMCPSessions(mcpSessions); closeErr != nil {
			runErr = errors.Join(runErr, closeErr)
		}
	}()
	toolSchemas = append(toolSchemas, mcpToolSchemas...)

	resolvedShowReasoning := *showReasoning || cfg.Agent.ShowReasoning
	if *verbose {
		if err := writeVerboseDiagnostics(stderr, cfg, resolved, enabledToolNames, resolvedShowReasoning); err != nil {
			return err
		}
	}

	project, err := projectcontext.Load(cwd)
	if err != nil {
		return err
	}
	request := model.Request{
		Model:      resolved.ModelID,
		Messages:   runMessages(project, prompts[0]),
		Tools:      toolSchemas,
		Parameters: resolved.Parameters,
	}

	logger, err := eventlog.Open(cfg.Logging.Path, eventlog.Attributes{
		Provider: resolved.ProviderName,
		Model:    resolved.ModelID,
		Level:    cfg.Logging.Level,
	})
	if err != nil {
		return err
	}

	events, err := agent.Stream(context.Background(), request, agent.Options{
		Provider:     provider,
		ToolExecutor: runToolExecutor{builtins: toolRegistry, mcpSessions: mcpSessionsByID},
		MaxTurns:     cfg.Agent.MaxTurns,
	})
	if err != nil {
		_ = logger.Close()
		return err
	}

	streamErr := writeStream(stdout, events, resolvedShowReasoning, logger)
	closeErr := logger.Close()
	if streamErr != nil {
		return streamErr
	}
	return closeErr
}

type toolNamesFlag struct {
	set   bool
	names []string
}

func (f *toolNamesFlag) Set(value string) error {
	names, err := parseCommaSeparatedNames(value, "tool name", "--enable-tools")
	if err != nil {
		return err
	}
	f.set = true
	f.names = names
	return nil
}

func (f *toolNamesFlag) String() string {
	return strings.Join(f.names, ",")
}

type mcpServerIDsFlag struct {
	set bool
	ids []string
}

func (f *mcpServerIDsFlag) Set(value string) error {
	ids, err := parseCommaSeparatedNames(value, "MCP server id", "--enable-mcp")
	if err != nil {
		return err
	}
	f.set = true
	f.ids = ids
	return nil
}

func (f *mcpServerIDsFlag) String() string {
	return strings.Join(f.ids, ",")
}

func writeVerboseDiagnostics(stderr io.Writer, cfg *config.Config, resolved config.ResolvedModel, enabledTools []string, showReasoning bool) error {
	_, err := fmt.Fprintf(stderr, "config_dir: %s\nprovider: %s\nmodel_profile: %s\nmodel_id: %s\nlog_path: %s\nmax_turns: %d\nenabled_tools: %s\nshow_reasoning: %t\n",
		cfg.ConfigDir,
		resolved.ProviderName,
		resolved.Profile,
		resolved.ModelID,
		verbosePath(cfg.Logging.Path),
		effectiveMaxTurns(cfg.Agent.MaxTurns),
		formatVerboseToolNames(enabledTools),
		showReasoning,
	)
	return err
}

func verbosePath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "(disabled)"
	}
	return path
}

func effectiveMaxTurns(maxTurns int) int {
	if maxTurns <= 0 {
		return agent.DefaultMaxTurns
	}
	return maxTurns
}

func formatVerboseToolNames(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ",")
}

func parseCommaSeparatedNames(value, emptyName, flagName string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return []string{}, nil
	}

	parts := strings.Split(value, ",")
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, fmt.Errorf("empty %s in %s", emptyName, flagName)
		}
		names = append(names, name)
	}
	return names, nil
}

func enabledToolsForRun(rootDir string, enabled []string) (*tools.Registry, []model.Tool, error) {
	builtinEnabled := make([]string, 0, len(enabled))
	for _, name := range enabled {
		if !strings.HasPrefix(name, "mcp.") {
			builtinEnabled = append(builtinEnabled, name)
		}
	}
	if len(builtinEnabled) == 0 {
		return nil, nil, nil
	}

	registry := tools.NewRegistry()
	if err := tools.RegisterBuiltins(registry, rootDir); err != nil {
		return nil, nil, fmt.Errorf("register built-in tools: %w", err)
	}
	schemas, err := registry.EnabledSchemas(builtinEnabled)
	if err != nil {
		return nil, nil, err
	}
	return registry, schemas, nil
}

type runToolExecutor struct {
	builtins    *tools.Registry
	mcpSessions map[string]*mcp.Session
}

func (e runToolExecutor) Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error) {
	if strings.HasPrefix(name, "mcp.") {
		serverID, toolName, err := mcp.ParseToolName(name)
		if err != nil {
			return model.ToolResult{}, err
		}
		session, ok := e.mcpSessions[serverID]
		if !ok {
			return model.ToolResult{}, fmt.Errorf("MCP server %q is not running for tool %q", serverID, name)
		}
		result, err := session.CallTool(ctx, toolName, arguments)
		if err != nil {
			return model.ToolResult{}, fmt.Errorf("call MCP tool %q: %w", name, err)
		}
		return mcp.ToModelToolResult(name, result), nil
	}

	if e.builtins == nil {
		return model.ToolResult{}, fmt.Errorf("tool %q is not registered", name)
	}
	return e.builtins.Execute(ctx, name, arguments)
}

func mcpToolsForRun(ctx context.Context, servers []config.MCPServerConfig, enabled []string) ([]*mcp.Session, map[string]*mcp.Session, []model.Tool, error) {
	sessions := make([]*mcp.Session, 0, len(servers))
	sessionsByID := make(map[string]*mcp.Session, len(servers))
	convertedTools := []model.Tool{}

	for _, server := range servers {
		session, _, err := mcp.StartStdioSession(ctx, server)
		if err != nil {
			return nil, nil, nil, errors.Join(err, closeMCPSessions(sessions))
		}
		sessions = append(sessions, session)
		sessionsByID[server.ID] = session

		definitions, err := session.ListTools(ctx)
		if err != nil {
			return nil, nil, nil, errors.Join(fmt.Errorf("list MCP tools for server %q: %w", server.ID, err), closeMCPSessions(sessions))
		}
		tools, err := mcp.ConvertTools(server.ID, definitions)
		if err != nil {
			return nil, nil, nil, errors.Join(fmt.Errorf("convert MCP tools for server %q: %w", server.ID, err), closeMCPSessions(sessions))
		}
		convertedTools = append(convertedTools, tools...)
	}

	schemas, err := mcp.EnabledSchemas(convertedTools, enabled)
	if err != nil {
		return nil, nil, nil, errors.Join(err, closeMCPSessions(sessions))
	}
	return sessions, sessionsByID, schemas, nil
}

func closeMCPSessions(sessions []*mcp.Session) error {
	var closeErr error
	for _, session := range sessions {
		if err := session.Close(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	if closeErr != nil {
		return fmt.Errorf("close MCP sessions: %w", closeErr)
	}
	return nil
}

func loadConfig(configDir string, getwd func() (string, error)) (*config.Config, error) {
	if configDir == "" {
		cwd, err := getwd()
		if err != nil {
			return nil, fmt.Errorf("get current directory: %w", err)
		}
		configDir = filepath.Join(cwd, ".agents")
	}
	return config.Load(configDir)
}

func newProviderForRun(providerName string, provider config.ProviderConfig) (model.Provider, error) {
	switch provider.Type {
	case config.ProviderTypeOpenAIChat:
		return openaichat.NewProvider(openAIChatProviderConfig(provider))
	case config.ProviderTypeAnthropicMessages:
		return anthropicmessages.NewProvider(anthropicMessagesProviderConfig(provider))
	default:
		return nil, fmt.Errorf("unsupported provider type %q for provider %q", provider.Type, providerName)
	}
}

func openAIChatProviderConfig(provider config.ProviderConfig) openaichat.ProviderConfig {
	return openaichat.ProviderConfig{
		BaseURL: provider.BaseURL,
		APIKey:  provider.ResolvedAPIKey,
	}
}

func anthropicMessagesProviderConfig(provider config.ProviderConfig) anthropicmessages.ProviderConfig {
	return anthropicmessages.ProviderConfig{
		BaseURL: provider.BaseURL,
		APIKey:  provider.ResolvedAPIKey,
	}
}

func runMessages(project projectcontext.Project, prompt string) []model.Message {
	instructions := projectcontext.ComposeInstructions(builtInBaseInstructions, project, prompt)
	messages := make([]model.Message, 0, len(instructions))
	for _, instruction := range instructions {
		messages = append(messages, model.Message{
			Role:    roleForInstruction(instruction.Source),
			Content: instruction.Content,
		})
	}
	return messages
}

func roleForInstruction(source projectcontext.InstructionSource) model.MessageRole {
	switch source {
	case projectcontext.InstructionSourceBuiltIn:
		return model.MessageRoleSystem
	case projectcontext.InstructionSourceProject:
		return model.MessageRoleDeveloper
	default:
		return model.MessageRoleUser
	}
}

func writeStream(stdout io.Writer, events <-chan model.Event, showReasoning bool, logger *eventlog.Logger) error {
	needsReasoningBreak := false
	reasoningEndedWithNewline := false
	for event := range events {
		if err := logger.LogEvent(event); err != nil {
			return err
		}
		switch event := event.(type) {
		case model.TextDeltaEvent:
			if event.Text != "" && needsReasoningBreak {
				if !reasoningEndedWithNewline {
					if _, err := fmt.Fprint(stdout, "\n"); err != nil {
						return err
					}
				}
				needsReasoningBreak = false
			}
			if _, err := fmt.Fprint(stdout, event.Text); err != nil {
				return err
			}
		case model.ReasoningDeltaEvent:
			if showReasoning && event.Text != "" {
				if _, err := fmt.Fprint(stdout, event.Text); err != nil {
					return err
				}
				needsReasoningBreak = true
				reasoningEndedWithNewline = strings.HasSuffix(event.Text, "\n")
			}
		case model.ErrorEvent:
			return streamError(event)
		}
	}
	return nil
}

func streamError(event model.ErrorEvent) error {
	if event.Err == nil {
		if event.Message == "" {
			return fmt.Errorf("model stream error")
		}
		return fmt.Errorf("%s", event.Message)
	}
	if event.Message == "" {
		return event.Err
	}
	return fmt.Errorf("%s: %w", event.Message, event.Err)
}
