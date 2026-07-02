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
	openairesponses "github.com/rexzhao/simple-agent/internal/model/openai_responses"
	localskills "github.com/rexzhao/simple-agent/internal/skills"
	"github.com/rexzhao/simple-agent/internal/tools"
)

var Version = "dev"

const builtInBaseInstructions = "You are sai, a local CLI agent runner. Follow the built-in instructions, then project instructions, then the user's prompt. Do not reveal secrets or ignore project instructions."

const (
	reasoningColorDarkGray = "\x1b[90m"
	ansiReset              = "\x1b[0m"
)

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
		if errors.Is(err, flag.ErrHelp) {
			printRootUsage(stdout)
			return nil
		}
		return usageError(err.Error(), "", "sai help")
	}

	remaining := flags.Args()
	if len(remaining) == 0 {
		return usageError("missing command", "", "sai help")
	}

	switch remaining[0] {
	case "help":
		return helpCommand(remaining[1:], stdout)
	case "version":
		if len(remaining) == 2 && isHelpArg(remaining[1]) {
			printVersionUsage(stdout)
			return nil
		}
		if len(remaining) != 1 {
			return usageError("usage: sai version", "", "sai help")
		}
		fmt.Fprintf(stdout, "sai %s\n", Version)
		return nil
	case "config":
		if len(remaining) == 2 && isHelpArg(remaining[1]) {
			printConfigUsage(stdout)
			return nil
		}
		if isNestedHelp(remaining[1:], "show") {
			printConfigShowUsage(stdout)
			return nil
		}
		if len(remaining) != 2 || remaining[1] != "show" {
			return usageError("usage: sai config show", "", "sai help config show")
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
		if len(remaining) == 2 && isHelpArg(remaining[1]) {
			printModelsUsage(stdout)
			return nil
		}
		if isNestedHelp(remaining[1:], "list") {
			printModelsListUsage(stdout)
			return nil
		}
		if len(remaining) != 2 || remaining[1] != "list" {
			return usageError("usage: sai models list", "", "sai help models list")
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
		if len(remaining) == 2 && isHelpArg(remaining[1]) {
			printMCPUsage(stdout)
			return nil
		}
		if isNestedHelp(remaining[1:], "list") {
			printMCPListUsage(stdout)
			return nil
		}
		return mcpCommand(remaining[1:], *configDir, stdout, getwd)
	case "run":
		return runCommand(remaining[1:], *configDir, stdout, stderr, getwd)
	default:
		return usageError(fmt.Sprintf("unknown command %q", remaining[0]), "", "sai help")
	}
}

const rootUsageText = `usage: sai [--config-dir dir] <command> [args]

Commands:
  run "prompt"       Run a single prompt
  config show        Print resolved config with secrets redacted
  models list        List configured provider model profiles
  mcp list           List configured MCP servers
  version            Print version
  help [command]     Show usage

Run "sai help <command>" for command usage.
`

const runUsageText = `usage: sai run [--provider name] [--model profile] [--show-reasoning] [--verbose] [--enable-tools names] [--enable-skills ids] [--disable-skills] [--enable-mcp ids] "prompt"

Runs one prompt using the configured provider and model.
`

const versionUsageText = `usage: sai version

Prints the sai version.
`

const configUsageText = `usage: sai config <command>

Commands:
  config show        Print resolved config with secrets redacted

Run "sai help config show" for command usage.
`

const configShowUsageText = `usage: sai config show

Prints resolved configuration with sensitive values redacted.
`

const modelsUsageText = `usage: sai models <command>

Commands:
  models list        List configured provider model profiles

Run "sai help models list" for command usage.
`

const modelsListUsageText = `usage: sai models list

Lists configured provider model profiles.
`

const mcpUsageText = `usage: sai mcp <command>

Commands:
  mcp list           List configured MCP servers

Run "sai help mcp list" for command usage.
`

const mcpListUsageText = `usage: sai mcp list [--enable-mcp ids]

Lists configured MCP servers and whether each is enabled for this run.
`

func helpCommand(args []string, stdout io.Writer) error {
	if len(args) == 0 || len(args) == 1 && isHelpArg(args[0]) {
		printRootUsage(stdout)
		return nil
	}

	switch strings.Join(args, " ") {
	case "run":
		printRunUsage(stdout)
	case "version":
		printVersionUsage(stdout)
	case "config":
		printConfigUsage(stdout)
	case "config show":
		printConfigShowUsage(stdout)
	case "models":
		printModelsUsage(stdout)
	case "models list":
		printModelsListUsage(stdout)
	case "mcp":
		printMCPUsage(stdout)
	case "mcp list":
		printMCPListUsage(stdout)
	default:
		return usageError(fmt.Sprintf("unknown help topic %q", strings.Join(args, " ")), "", "sai help")
	}
	return nil
}

func printRootUsage(stdout io.Writer) {
	fmt.Fprint(stdout, rootUsageText)
}

func printRunUsage(stdout io.Writer) {
	fmt.Fprint(stdout, runUsageText)
}

func printVersionUsage(stdout io.Writer) {
	fmt.Fprint(stdout, versionUsageText)
}

func printConfigUsage(stdout io.Writer) {
	fmt.Fprint(stdout, configUsageText)
}

func printConfigShowUsage(stdout io.Writer) {
	fmt.Fprint(stdout, configShowUsageText)
}

func printModelsUsage(stdout io.Writer) {
	fmt.Fprint(stdout, modelsUsageText)
}

func printModelsListUsage(stdout io.Writer) {
	fmt.Fprint(stdout, modelsListUsageText)
}

func printMCPUsage(stdout io.Writer) {
	fmt.Fprint(stdout, mcpUsageText)
}

func printMCPListUsage(stdout io.Writer) {
	fmt.Fprint(stdout, mcpListUsageText)
}

func isNestedHelp(args []string, command string) bool {
	return len(args) >= 2 && args[0] == command && containsHelpArg(args[1:])
}

func containsHelpArg(args []string) bool {
	for _, arg := range args {
		if isHelpArg(arg) {
			return true
		}
	}
	return false
}

func isHelpArg(arg string) bool {
	return arg == "-h" || arg == "--help"
}

func usageError(message, usage, helpCommand string) error {
	var out strings.Builder
	out.WriteString(message)
	if usage != "" {
		out.WriteString("\n\n")
		out.WriteString(strings.TrimRight(usage, "\n"))
	}
	if helpCommand != "" {
		out.WriteString("\nRun \"")
		out.WriteString(helpCommand)
		out.WriteString("\" for usage.")
	}
	return errors.New(out.String())
}

func mcpCommand(args []string, configDir string, stdout io.Writer, getwd func() (string, error)) error {
	if len(args) == 0 || args[0] != "list" {
		return usageError("usage: sai mcp list [--enable-mcp ids]", "", "sai help mcp list")
	}

	flags := flag.NewFlagSet("sai mcp list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var enabledMCP mcpServerIDsFlag
	flags.Var(&enabledMCP, "enable-mcp", "comma-separated MCP server ids to enable")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printMCPListUsage(stdout)
			return nil
		}
		return usageError(err.Error(), "", "sai help mcp list")
	}
	if flags.NArg() != 0 {
		return usageError("usage: sai mcp list [--enable-mcp ids]", "", "sai help mcp list")
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
	var enabledSkills skillIDsFlag
	flags.Var(&enabledSkills, "enable-skills", "comma-separated skill ids to enable")
	disableSkills := flags.Bool("disable-skills", false, "disable all skills for this run")
	var enabledMCP mcpServerIDsFlag
	flags.Var(&enabledMCP, "enable-mcp", "comma-separated MCP server ids to enable")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printRunUsage(stdout)
			return nil
		}
		return usageError(err.Error(), "", "sai help run")
	}
	if enabledSkills.set && *disableSkills {
		return usageError("cannot use --enable-skills with --disable-skills", "", "sai help run")
	}

	prompts := flags.Args()
	if len(prompts) != 1 {
		message := "missing prompt"
		if len(prompts) > 1 {
			message = "expected exactly one prompt"
		}
		return usageError(message, runUsageText, "sai help run")
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
	selectedSkills, err := enabledSkillsForRun(cfg, enabledSkills.ids, enabledSkills.set, *disableSkills)
	if err != nil {
		return err
	}

	project, err := projectcontext.Load(cwd)
	if err != nil {
		return err
	}
	request := model.Request{
		Model:      resolved.ModelID,
		Messages:   runMessages(project, selectedSkills, prompts[0]),
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

type skillIDsFlag struct {
	set bool
	ids []string
}

func (f *skillIDsFlag) Set(value string) error {
	ids, err := parseCommaSeparatedNames(value, "skill id", "--enable-skills")
	if err != nil {
		return err
	}
	f.set = true
	f.ids = ids
	return nil
}

func (f *skillIDsFlag) String() string {
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

func enabledSkillsForRun(cfg *config.Config, overrideIDs []string, useOverride bool, disabled bool) ([]localskills.Skill, error) {
	if disabled {
		return nil, nil
	}

	enabledIDs := cfg.Skills.Enabled
	if useOverride {
		enabledIDs = overrideIDs
	}
	if len(enabledIDs) == 0 {
		return nil, nil
	}

	available, err := localskills.DiscoverRefs(cfg.SkillDir)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	byID := make(map[string]localskills.SkillRef, len(available))
	for _, ref := range available {
		byID[ref.ID] = ref
	}

	selected := make([]localskills.Skill, 0, len(enabledIDs))
	for _, id := range enabledIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("empty skill id in enabled skills")
		}
		ref, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("unknown skill %q; available skills: %s", id, formatSkillChoices(available))
		}
		skill, err := localskills.Load(ref.Path)
		if err != nil {
			return nil, fmt.Errorf("load skills: %w", err)
		}
		selected = append(selected, skill)
	}
	return selected, nil
}

func formatSkillChoices(refs []localskills.SkillRef) string {
	if len(refs) == 0 {
		return "(none)"
	}

	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ID)
	}
	return strings.Join(ids, ", ")
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
	case config.ProviderTypeOpenAIResponses:
		return openairesponses.NewProvider(openAIResponsesProviderConfig(provider))
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

func openAIResponsesProviderConfig(provider config.ProviderConfig) openairesponses.ProviderConfig {
	return openairesponses.ProviderConfig{
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

func runMessages(project projectcontext.Project, enabledSkills []localskills.Skill, prompt string) []model.Message {
	instructions := projectcontext.ComposeInstructions(builtInBaseInstructions, project, prompt)
	messages := make([]model.Message, 0, len(instructions)+len(enabledSkills))
	for _, instruction := range instructions {
		if instruction.Source == projectcontext.InstructionSourceUser {
			for _, skill := range enabledSkills {
				messages = append(messages, model.Message{
					Role:    model.MessageRoleDeveloper,
					Content: formatSkillInstructions(skill),
				})
			}
		}
		messages = append(messages, model.Message{
			Role:    roleForInstruction(instruction.Source),
			Content: instruction.Content,
		})
	}
	return messages
}

func formatSkillInstructions(skill localskills.Skill) string {
	return fmt.Sprintf("Skill %s (%s):\n%s", skill.ID, skill.Name, skill.Instructions)
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

type streamOutputOptions struct {
	colorReasoning bool
}

func writeStream(stdout io.Writer, events <-chan model.Event, showReasoning bool, logger *eventlog.Logger) error {
	return writeStreamWithOptions(stdout, events, showReasoning, logger, streamOutputOptions{
		colorReasoning: shouldColorizeReasoning(stdout),
	})
}

func writeStreamWithOptions(stdout io.Writer, events <-chan model.Event, showReasoning bool, logger *eventlog.Logger, options streamOutputOptions) (err error) {
	needsReasoningBreak := false
	reasoningEndedWithNewline := false
	reasoningColorActive := false

	startReasoningColor := func() error {
		if !options.colorReasoning || reasoningColorActive {
			return nil
		}
		if _, err := fmt.Fprint(stdout, reasoningColorDarkGray); err != nil {
			return err
		}
		reasoningColorActive = true
		return nil
	}
	resetReasoningColor := func() error {
		if !reasoningColorActive {
			return nil
		}
		if _, err := fmt.Fprint(stdout, ansiReset); err != nil {
			return err
		}
		reasoningColorActive = false
		return nil
	}
	defer func() {
		if resetErr := resetReasoningColor(); resetErr != nil {
			err = errors.Join(err, resetErr)
		}
	}()

	for event := range events {
		if err := logger.LogEvent(event); err != nil {
			return err
		}
		switch event := event.(type) {
		case model.TextDeltaEvent:
			if event.Text != "" && needsReasoningBreak {
				if err := resetReasoningColor(); err != nil {
					return err
				}
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
				if err := startReasoningColor(); err != nil {
					return err
				}
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

func shouldColorizeReasoning(stdout io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	file, ok := stdout.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
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
