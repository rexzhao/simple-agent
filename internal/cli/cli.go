package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/config"
	projectcontext "github.com/rexzhao/simple-agent/internal/context"
	"github.com/rexzhao/simple-agent/internal/contextwindow"
	eventlog "github.com/rexzhao/simple-agent/internal/logging"
	"github.com/rexzhao/simple-agent/internal/mcp"
	"github.com/rexzhao/simple-agent/internal/model"
	anthropicmessages "github.com/rexzhao/simple-agent/internal/model/anthropic_messages"
	openaichat "github.com/rexzhao/simple-agent/internal/model/openai_chat"
	openairesponses "github.com/rexzhao/simple-agent/internal/model/openai_responses"
	"github.com/rexzhao/simple-agent/internal/sessions"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return RunWithContext(ctx, args, os.Stdin, stdout, stderr, os.Getwd)
}

func RunWithGetwd(args []string, stdout, stderr io.Writer, getwd func() (string, error)) int {
	return RunWithIO(args, os.Stdin, stdout, stderr, getwd)
}

func RunWithIO(args []string, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error)) int {
	return RunWithContext(context.Background(), args, stdin, stdout, stderr, getwd)
}

func RunWithContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error)) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := execute(ctx, args, stdin, stdout, stderr, getwd); err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	return 0
}

func execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error)) error {
	rootArgs, err := splitRootArgs(args)
	if err != nil {
		return err
	}
	if rootArgs.command == "" {
		if rootArgs.hasHelp {
			printRootUsage(stdout)
			return nil
		}
		return chatCommand(ctx, rootArgs.commandArgs, rootArgs.configDir, stdin, stdout, stderr, getwd)
	}

	switch rootArgs.command {
	case "help":
		return helpCommand(rootArgs.commandArgs, stdout)
	case "version":
		return versionCommand(rootArgs.commandArgs, stdout)
	case "config":
		subcommand, subArgs, groupHelp, err := splitSubcommandArgs(rootArgs.commandArgs, nil, "sai help config")
		if err != nil {
			return err
		}
		if subcommand == "" && groupHelp {
			printConfigUsage(stdout)
			return nil
		}
		if subcommand != "show" {
			return usageError("usage: sai config show", "", "sai help config show")
		}
		return configShowCommand(subArgs, rootArgs.configDir, stdout, getwd)
	case "models":
		subcommand, subArgs, groupHelp, err := splitSubcommandArgs(rootArgs.commandArgs, nil, "sai help models")
		if err != nil {
			return err
		}
		if subcommand == "" && groupHelp {
			printModelsUsage(stdout)
			return nil
		}
		if subcommand != "list" {
			return usageError("usage: sai models list", "", "sai help models list")
		}
		return modelsListCommand(subArgs, rootArgs.configDir, stdout, getwd)
	case "tools":
		subcommand, subArgs, groupHelp, err := splitSubcommandArgs(rootArgs.commandArgs, nil, "sai help tools")
		if err != nil {
			return err
		}
		if subcommand == "" && groupHelp {
			printToolsUsage(stdout)
			return nil
		}
		if subcommand != "list" {
			return usageError("usage: sai tools list", "", "sai help tools list")
		}
		return toolsListCommand(subArgs, stdout)
	case "mcp":
		subcommand, subArgs, groupHelp, err := splitSubcommandArgs(rootArgs.commandArgs, map[string]flagKind{"enable-mcp": flagKindValue}, "sai help mcp")
		if err != nil {
			return err
		}
		if subcommand == "" && groupHelp {
			printMCPUsage(stdout)
			return nil
		}
		if subcommand != "list" {
			return usageError("usage: sai mcp list [--enable-mcp ids]", "", "sai help mcp list")
		}
		return mcpListCommand(subArgs, rootArgs.configDir, stdout, getwd)
	case "sessions":
		subcommand, subArgs, groupHelp, err := splitSubcommandArgs(rootArgs.commandArgs, map[string]flagKind{"keep": flagKindValue}, "sai help sessions")
		if err != nil {
			return err
		}
		if subcommand == "" && groupHelp {
			printSessionsUsage(stdout)
			return nil
		}
		switch subcommand {
		case "list":
			return sessionsListCommand(subArgs, rootArgs.configDir, stdout, getwd)
		case "show":
			return sessionsShowCommand(subArgs, rootArgs.configDir, stdout, getwd)
		case "delete":
			return sessionsDeleteCommand(subArgs, rootArgs.configDir, stdout, getwd)
		case "prune":
			return sessionsPruneCommand(subArgs, rootArgs.configDir, stdout, getwd)
		default:
			return usageError("usage: sai sessions <list|show|delete|prune>", "", "sai help sessions")
		}
	case "chat":
		return chatCommand(ctx, rootArgs.commandArgs, rootArgs.configDir, stdin, stdout, stderr, getwd)
	default:
		return usageError(fmt.Sprintf("unknown command %q", rootArgs.command), "", "sai help")
	}
}

const rootUsageText = `usage: sai [--config-dir dir] [command] [args]

Commands:
  chat              Start a chat session
  config show        Print resolved config with secrets redacted
  models list        List configured provider model profiles
  tools list         List built-in tools
  mcp list           List configured MCP servers
  sessions           Manage resumable sessions
  version            Print version
  help [command]     Show usage

With no command, sai defaults to chat.

Run "sai help <command>" for command usage.
`

const chatUsageText = `usage: sai chat [--provider name] [--model profile] [--prompt text | --stdin | --file path] [--show-reasoning] [--verbose] [--enable-tools names] [--enable-skills ids] [--disable-skills] [--enable-mcp ids] [--save-session] [--resume id | --continue] [--quit]

Starts a line-oriented chat session using the configured provider and model. When
--prompt is provided, sai runs that turn first; --quit exits after that turn
instead of entering the REPL. --stdin and --file read one complete prompt and
must be used with --quit. Resumable sessions save full sensitive content,
including prompts, assistant output, assistant tool calls, and tool results.
`

const resumableSessionSaveNoticeText = "sai: resumable sessions enabled; full prompts, assistant output, and tool results will be saved to the session file."

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

const toolsUsageText = `usage: sai tools <command>

Commands:
  tools list         List built-in tools

Run "sai help tools list" for command usage.
`

const toolsListUsageText = `usage: sai tools list

Lists built-in tools.
`

const mcpUsageText = `usage: sai mcp <command>

Commands:
  mcp list           List configured MCP servers

Run "sai help mcp list" for command usage.
`

const mcpListUsageText = `usage: sai mcp list [--enable-mcp ids]

Lists configured MCP servers and whether each is enabled for this run.
`

const sessionsUsageText = `usage: sai sessions <command>

Commands:
  sessions list              List resumable sessions
  sessions show <id>         Show resumable session metadata
  sessions delete <id>       Delete a resumable session
  sessions prune --keep N    Delete older resumable sessions

Run "sai help sessions <command>" for command usage.
`

const sessionsListUsageText = `usage: sai sessions list

Lists resumable sessions from the configured sessions.dir without printing
messages, prompts, assistant output, or tool result content.
`

const sessionsShowUsageText = `usage: sai sessions show <id>

Shows resumable session metadata only. Session files contain full sensitive
content, including prompts, assistant output, and tool results.
`

const sessionsDeleteUsageText = `usage: sai sessions delete <id>

Deletes one resumable session.
`

const sessionsPruneUsageText = `usage: sai sessions prune --keep N

Keeps the N most recently updated resumable sessions and deletes older ones.
--keep must be provided explicitly and N must be 0 or greater.
`

func helpCommand(args []string, stdout io.Writer) error {
	if len(args) == 0 || len(args) == 1 && isHelpArg(args[0]) {
		printRootUsage(stdout)
		return nil
	}

	switch strings.Join(args, " ") {
	case "chat":
		printChatUsage(stdout)
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
	case "tools":
		printToolsUsage(stdout)
	case "tools list":
		printToolsListUsage(stdout)
	case "mcp":
		printMCPUsage(stdout)
	case "mcp list":
		printMCPListUsage(stdout)
	case "sessions":
		printSessionsUsage(stdout)
	case "sessions list":
		printSessionsListUsage(stdout)
	case "sessions show":
		printSessionsShowUsage(stdout)
	case "sessions delete":
		printSessionsDeleteUsage(stdout)
	case "sessions prune":
		printSessionsPruneUsage(stdout)
	default:
		return usageError(fmt.Sprintf("unknown help topic %q", strings.Join(args, " ")), "", "sai help")
	}
	return nil
}

func printRootUsage(stdout io.Writer) {
	fmt.Fprint(stdout, rootUsageText)
}

func printChatUsage(stdout io.Writer) {
	fmt.Fprint(stdout, chatUsageText)
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

func printToolsUsage(stdout io.Writer) {
	fmt.Fprint(stdout, toolsUsageText)
}

func printToolsListUsage(stdout io.Writer) {
	fmt.Fprint(stdout, toolsListUsageText)
}

func printMCPUsage(stdout io.Writer) {
	fmt.Fprint(stdout, mcpUsageText)
}

func printMCPListUsage(stdout io.Writer) {
	fmt.Fprint(stdout, mcpListUsageText)
}

func printSessionsUsage(stdout io.Writer) {
	fmt.Fprint(stdout, sessionsUsageText)
}

func printSessionsListUsage(stdout io.Writer) {
	fmt.Fprint(stdout, sessionsListUsageText)
}

func printSessionsShowUsage(stdout io.Writer) {
	fmt.Fprint(stdout, sessionsShowUsageText)
}

func printSessionsDeleteUsage(stdout io.Writer) {
	fmt.Fprint(stdout, sessionsDeleteUsageText)
}

func printSessionsPruneUsage(stdout io.Writer) {
	fmt.Fprint(stdout, sessionsPruneUsageText)
}

func isNestedHelp(args []string, command string) bool {
	return len(args) >= 2 && args[0] == command && containsHelpArg(args[1:])
}

func containsHelpArg(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
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

type flagKind int

const (
	flagKindBool flagKind = iota
	flagKindValue
)

type rootArgs struct {
	configDir   string
	command     string
	commandArgs []string
	hasHelp     bool
}

func splitRootArgs(args []string) (rootArgs, error) {
	known := map[string]flagKind{
		"config-dir":     flagKindValue,
		"provider":       flagKindValue,
		"model":          flagKindValue,
		"prompt":         flagKindValue,
		"file":           flagKindValue,
		"enable-tools":   flagKindValue,
		"enable-skills":  flagKindValue,
		"enable-mcp":     flagKindValue,
		"resume":         flagKindValue,
		"keep":           flagKindValue,
		"h":              flagKindBool,
		"help":           flagKindBool,
		"show-reasoning": flagKindBool,
		"verbose":        flagKindBool,
		"stdin":          flagKindBool,
		"disable-skills": flagKindBool,
		"save-session":   flagKindBool,
		"continue":       flagKindBool,
		"quit":           flagKindBool,
	}
	var out rootArgs
	prefixArgs := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 >= len(args) {
				break
			}
			out.command = args[i+1]
			out.commandArgs = append(prefixArgs, "--")
			out.commandArgs = append(out.commandArgs, args[i+2:]...)
			return stripGlobalArgs(out)
		}
		if !isFlagArg(arg) {
			out.command = arg
			out.commandArgs = append(prefixArgs, args[i+1:]...)
			return stripGlobalArgs(out)
		}

		name, hasInlineValue := flagName(arg)
		kind, ok := known[name]
		if !ok {
			return rootArgs{}, usageError(fmt.Sprintf("flag provided but not defined: -%s", name), "", "sai help")
		}
		if isHelpArg(arg) {
			out.hasHelp = true
		}
		if name == "config-dir" {
			value, next, err := flagValue(args, i, name, hasInlineValue)
			if err != nil {
				return rootArgs{}, usageError(err.Error(), "", "sai help")
			}
			out.configDir = value
			i = next
			continue
		}

		prefixArgs = append(prefixArgs, arg)
		if kind == flagKindValue && !hasInlineValue {
			value, next, err := flagValue(args, i, name, false)
			if err != nil {
				return rootArgs{}, usageError(err.Error(), "", "sai help")
			}
			prefixArgs = append(prefixArgs, value)
			i = next
		}
	}

	out.commandArgs = prefixArgs
	return out, nil
}

func stripGlobalArgs(args rootArgs) (rootArgs, error) {
	stripped := make([]string, 0, len(args.commandArgs))
	for i := 0; i < len(args.commandArgs); i++ {
		arg := args.commandArgs[i]
		if arg == "--" {
			stripped = append(stripped, args.commandArgs[i:]...)
			break
		}
		name, hasInlineValue := flagName(arg)
		if isFlagArg(arg) && name == "config-dir" {
			value, next, err := flagValue(args.commandArgs, i, name, hasInlineValue)
			if err != nil {
				return rootArgs{}, usageError(err.Error(), "", "sai help")
			}
			args.configDir = value
			i = next
			continue
		}
		stripped = append(stripped, arg)
	}
	args.commandArgs = stripped
	return args, nil
}

func splitSubcommandArgs(args []string, known map[string]flagKind, helpCommand string) (string, []string, bool, error) {
	prefixArgs := make([]string, 0, len(args))
	hasHelp := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 >= len(args) {
				break
			}
			subArgs := append(prefixArgs, "--")
			subArgs = append(subArgs, args[i+2:]...)
			return args[i+1], subArgs, hasHelp, nil
		}
		if !isFlagArg(arg) {
			return arg, append(prefixArgs, args[i+1:]...), hasHelp, nil
		}
		if isHelpArg(arg) {
			hasHelp = true
			prefixArgs = append(prefixArgs, arg)
			continue
		}

		name, hasInlineValue := flagName(arg)
		kind, ok := known[name]
		if !ok {
			return "", nil, false, usageError(fmt.Sprintf("flag provided but not defined: -%s", name), "", helpCommand)
		}
		prefixArgs = append(prefixArgs, arg)
		if kind == flagKindValue && !hasInlineValue {
			value, next, err := flagValue(args, i, name, false)
			if err != nil {
				return "", nil, false, usageError(err.Error(), "", helpCommand)
			}
			prefixArgs = append(prefixArgs, value)
			i = next
		}
	}
	return "", prefixArgs, hasHelp, nil
}

func flagValue(args []string, index int, name string, inline bool) (string, int, error) {
	if inline {
		_, value, _ := strings.Cut(args[index], "=")
		return value, index, nil
	}
	if index+1 >= len(args) {
		return "", index, fmt.Errorf("flag needs an argument: -%s", name)
	}
	return args[index+1], index + 1, nil
}

func versionCommand(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("sai version", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printVersionUsage, "sai help")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai version", "", "sai help")
	}
	fmt.Fprintf(stdout, "sai %s\n", Version)
	return nil
}

func configShowCommand(args []string, configDir string, stdout io.Writer, getwd func() (string, error)) error {
	flags := flag.NewFlagSet("sai config show", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printConfigShowUsage, "sai help config show")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai config show", "", "sai help config show")
	}

	cfg, err := loadConfig(configDir, getwd)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(cfg)
}

func modelsListCommand(args []string, configDir string, stdout io.Writer, getwd func() (string, error)) error {
	flags := flag.NewFlagSet("sai models list", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printModelsListUsage, "sai help models list")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai models list", "", "sai help models list")
	}

	cfg, err := loadConfig(configDir, getwd)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "PROVIDER\tPROFILE\tMODEL ID")
	for _, model := range cfg.ModelList() {
		fmt.Fprintf(stdout, "%s\t%s\t%s\n", model.Provider, model.Profile, model.ID)
	}
	return nil
}

func mcpListCommand(args []string, configDir string, stdout io.Writer, getwd func() (string, error)) error {
	flags := flag.NewFlagSet("sai mcp list", flag.ContinueOnError)
	var enabledMCP mcpServerIDsFlag
	flags.Var(&enabledMCP, "enable-mcp", "comma-separated MCP server ids to enable")
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printMCPListUsage, "sai help mcp list")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
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

func toolsListCommand(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("sai tools list", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printToolsListUsage, "sai help tools list")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai tools list", "", "sai help tools list")
	}

	for _, name := range builtInToolNames() {
		fmt.Fprintln(stdout, name)
	}
	return nil
}

func sessionsListCommand(args []string, configDir string, stdout io.Writer, getwd func() (string, error)) error {
	flags := flag.NewFlagSet("sai sessions list", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printSessionsListUsage, "sai help sessions list")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai sessions list", "", "sai help sessions list")
	}

	store, err := sessionStoreFromConfig(configDir, getwd)
	if err != nil {
		return err
	}
	infos, err := store.List()
	if err != nil {
		return err
	}

	fmt.Fprintln(stdout, "ID\tUPDATED\tPROVIDER\tMODEL/PROFILE")
	for _, info := range infos {
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s/%s\n", info.ID, formatSessionTimestamp(info.UpdatedAt), info.Provider, info.ModelID, info.ModelProfile)
	}
	return nil
}

func sessionsShowCommand(args []string, configDir string, stdout io.Writer, getwd func() (string, error)) error {
	flags := flag.NewFlagSet("sai sessions show", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printSessionsShowUsage, "sai help sessions show")
	if done || err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usageError("usage: sai sessions show <id>", "", "sai help sessions show")
	}

	store, err := sessionStoreFromConfig(configDir, getwd)
	if err != nil {
		return err
	}
	session, err := store.Load(positionals[0])
	if err != nil {
		return readableSessionNotFound(err, positionals[0])
	}

	fmt.Fprintln(stdout, "WARNING: session files contain full sensitive content, including prompts, assistant output, and tool results.")
	fmt.Fprintln(stdout, "This command prints metadata only; messages and tool result content are not shown.")
	fmt.Fprintf(stdout, "ID\t%s\n", session.ID)
	fmt.Fprintf(stdout, "CREATED\t%s\n", formatSessionTimestamp(session.CreatedAt))
	fmt.Fprintf(stdout, "UPDATED\t%s\n", formatSessionTimestamp(session.UpdatedAt))
	fmt.Fprintf(stdout, "VERSION\t%d\n", session.Version)
	fmt.Fprintf(stdout, "PROVIDER\t%s\n", session.Provider)
	fmt.Fprintf(stdout, "MODEL_PROFILE\t%s\n", session.ModelProfile)
	fmt.Fprintf(stdout, "MODEL_ID\t%s\n", session.ModelID)
	fmt.Fprintf(stdout, "CWD\t%s\n", session.CWD)
	fmt.Fprintf(stdout, "CONFIG_DIR\t%s\n", session.ConfigDir)
	fmt.Fprintf(stdout, "ENABLED_TOOLS\t%s\n", formatSessionStringList(session.EnabledTools))
	fmt.Fprintf(stdout, "ENABLED_MCP\t%s\n", formatSessionStringList(session.EnabledMCP))
	fmt.Fprintf(stdout, "ENABLED_SKILLS\t%s\n", formatSessionStringList(session.EnabledSkills))
	fmt.Fprintf(stdout, "SHOW_REASONING\t%t\n", session.ShowReasoning)
	if session.Context.ContextWindow > 0 {
		fmt.Fprintf(stdout, "CONTEXT_WINDOW\t%d\n", session.Context.ContextWindow)
		fmt.Fprintf(stdout, "CONTEXT_WINDOW_SOURCE\t%s\n", session.Context.ContextWindowSource)
		fmt.Fprintf(stdout, "CONTEXT_WARNING_THRESHOLD_PERCENT\t%d\n", session.Context.WarningThresholdPercent)
		fmt.Fprintf(stdout, "CONTEXT_LAST_REQUEST_TOKENS\t%d\n", session.Context.LastRequestTokens)
		fmt.Fprintf(stdout, "CONTEXT_LAST_INPUT_TOKENS\t%d\n", session.Context.LastInputTokens)
		fmt.Fprintf(stdout, "CONTEXT_LAST_OUTPUT_TOKENS\t%d\n", session.Context.LastOutputTokens)
		fmt.Fprintf(stdout, "CONTEXT_LAST_TOTAL_TOKENS\t%d\n", session.Context.LastTotalTokens)
		fmt.Fprintf(stdout, "CONTEXT_LAST_USAGE_SOURCE\t%s\n", session.Context.LastUsageSource)
	}
	fmt.Fprintf(stdout, "SAVE_TOOL_RESULTS\t%t\n", session.SaveToolResults)
	fmt.Fprintf(stdout, "INSTRUCTION_COUNT\t%d\n", len(session.InstructionsSnapshot))
	fmt.Fprintf(stdout, "MESSAGE_COUNT\t%d\n", len(session.Messages))
	return nil
}

func sessionsDeleteCommand(args []string, configDir string, stdout io.Writer, getwd func() (string, error)) error {
	flags := flag.NewFlagSet("sai sessions delete", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printSessionsDeleteUsage, "sai help sessions delete")
	if done || err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usageError("usage: sai sessions delete <id>", "", "sai help sessions delete")
	}

	store, err := sessionStoreFromConfig(configDir, getwd)
	if err != nil {
		return err
	}
	if err := store.Delete(positionals[0]); err != nil {
		return readableSessionNotFound(err, positionals[0])
	}
	fmt.Fprintf(stdout, "deleted session %s\n", positionals[0])
	return nil
}

func sessionsPruneCommand(args []string, configDir string, stdout io.Writer, getwd func() (string, error)) error {
	flags := flag.NewFlagSet("sai sessions prune", flag.ContinueOnError)
	keep := flags.Int("keep", -1, "number of most recently updated sessions to keep")
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printSessionsPruneUsage, "sai help sessions prune")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai sessions prune --keep N", "", "sai help sessions prune")
	}
	keepSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "keep" {
			keepSet = true
		}
	})
	if !keepSet {
		return usageError("--keep must be provided", sessionsPruneUsageText, "sai help sessions prune")
	}
	if *keep < 0 {
		return usageError("--keep must be 0 or greater", sessionsPruneUsageText, "sai help sessions prune")
	}

	store, err := sessionStoreFromConfig(configDir, getwd)
	if err != nil {
		return err
	}
	infos, err := store.List()
	if err != nil {
		return err
	}
	if *keep > len(infos) {
		*keep = len(infos)
	}

	deletedIDs := make([]string, 0, len(infos)-*keep)
	for _, info := range infos[*keep:] {
		if err := store.Delete(info.ID); err != nil {
			return readableSessionNotFound(err, info.ID)
		}
		deletedIDs = append(deletedIDs, info.ID)
	}

	fmt.Fprintf(stdout, "deleted %d sessions\n", len(deletedIDs))
	for _, id := range deletedIDs {
		fmt.Fprintln(stdout, id)
	}
	return nil
}

func sessionStoreFromConfig(configDir string, getwd func() (string, error)) (*sessions.Store, error) {
	cfg, err := loadConfig(configDir, getwd)
	if err != nil {
		return nil, err
	}
	return sessions.NewStore(cfg.Sessions.Dir), nil
}

func readableSessionNotFound(err error, id string) error {
	if errors.Is(err, sessions.ErrNotFound) {
		return fmt.Errorf("resumable session %q was not found", id)
	}
	return err
}

func formatSessionTimestamp(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func formatSessionStringList(values []string) string {
	if len(values) == 0 {
		return "(none)"
	}
	return strings.Join(values, ",")
}

func parseCommandFlagArgs(flags *flag.FlagSet, args []string, stdout io.Writer, printUsage func(io.Writer), helpCommand string) ([]string, bool, error) {
	flags.SetOutput(io.Discard)
	if containsHelpArg(args) {
		printUsage(stdout)
		return nil, true, nil
	}
	parsedArgs, err := intersperseFlagArgs(flags, args)
	if err != nil {
		return nil, false, usageError(err.Error(), "", helpCommand)
	}
	if err := flags.Parse(parsedArgs); err != nil {
		return nil, false, usageError(err.Error(), "", helpCommand)
	}
	return flags.Args(), false, nil
}

func builtInToolNames() []string {
	return []string{
		tools.BuiltinListFiles,
		tools.BuiltinReadFile,
		tools.BuiltinWriteFile,
		tools.BuiltinEditFile,
		tools.BuiltinShell,
	}
}

type agentCommandFlags struct {
	providerName     string
	modelProfile     string
	prompt           promptTextFlag
	stdin            bool
	file             filePathFlag
	showReasoning    bool
	showReasoningSet bool
	verbose          bool
	enabledTools     toolNamesFlag
	enabledSkills    skillIDsFlag
	disableSkills    bool
	enabledMCP       mcpServerIDsFlag
	saveSession      bool
	saveSessionSet   bool
	resumeID         string
	continueSession  bool
}

func registerAgentCommandFlags(flags *flag.FlagSet, options *agentCommandFlags) {
	flags.StringVar(&options.providerName, "provider", "", "provider name")
	flags.StringVar(&options.modelProfile, "model", "", "model profile")
	flags.Var(&options.prompt, "prompt", "initial prompt text")
	flags.BoolVar(&options.stdin, "stdin", false, "read initial prompt from stdin")
	flags.Var(&options.file, "file", "read initial prompt from file")
	flags.BoolVar(&options.showReasoning, "show-reasoning", false, "show reasoning output")
	flags.BoolVar(&options.verbose, "verbose", false, "write non-sensitive diagnostics to stderr")
	flags.Var(&options.enabledTools, "enable-tools", "comma-separated tool names to expose")
	flags.Var(&options.enabledSkills, "enable-skills", "comma-separated skill ids to enable")
	flags.BoolVar(&options.disableSkills, "disable-skills", false, "disable all skills for this run")
	flags.Var(&options.enabledMCP, "enable-mcp", "comma-separated MCP server ids to enable")
	flags.BoolVar(&options.saveSession, "save-session", false, "save a resumable session with full sensitive content")
	flags.StringVar(&options.resumeID, "resume", "", "resume a saved session id")
	flags.BoolVar(&options.continueSession, "continue", false, "resume the latest saved session")
}

func (options agentCommandFlags) validate(helpCommand string) error {
	if options.enabledSkills.set && options.disableSkills {
		return usageError("cannot use --enable-skills with --disable-skills", "", helpCommand)
	}
	if options.resumeID != "" && options.continueSession {
		return usageError("cannot use --resume with --continue", "", helpCommand)
	}
	return nil
}

func chatCommand(ctx context.Context, args []string, configDir string, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error)) (chatErr error) {
	flags := flag.NewFlagSet("sai chat", flag.ContinueOnError)
	var options agentCommandFlags
	registerAgentCommandFlags(flags, &options)
	quit := flags.Bool("quit", false, "exit after the initial prompt turn")
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printChatUsage, "sai help chat")
	if done || err != nil {
		return err
	}
	flags.Visit(func(flag *flag.Flag) {
		switch flag.Name {
		case "show-reasoning":
			options.showReasoningSet = true
		case "save-session":
			options.saveSessionSet = true
		}
	})
	if err := options.validate("sai help chat"); err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("unexpected positional argument; use --prompt for the initial prompt", chatUsageText, "sai help chat")
	}
	initialSources := options.initialInputSourceCount()
	if initialSources > 1 {
		return usageError("--prompt, --stdin, and --file are mutually exclusive", chatUsageText, "sai help chat")
	}
	if *quit && initialSources == 0 {
		return usageError("--quit requires --prompt, --stdin, or --file", chatUsageText, "sai help chat")
	}
	if !*quit && options.stdin {
		return usageError("--stdin requires --quit", chatUsageText, "sai help chat")
	}
	if !*quit && options.file.set {
		return usageError("--file requires --quit", chatUsageText, "sai help chat")
	}

	runtime, err := prepareAgentRuntime(ctx, configDir, options, stderr, getwd)
	if err != nil {
		return err
	}
	defer func() {
		chatErr = errors.Join(chatErr, runtime.Close())
	}()
	if err := runtime.writeSessionSaveNotice(stderr); err != nil {
		return err
	}

	messages := runtime.initialMessages()
	initialPrompt, hasInitialPrompt, err := readInitialPrompt(options, stdin)
	if err != nil {
		return err
	}
	if hasInitialPrompt {
		updated, err := runChatTurn(ctx, runtime, messages, initialPrompt, stdout, stderr, !*quit, false)
		if err != nil {
			return err
		}
		messages = updated
		if *quit {
			return nil
		}
	}

	scanner := bufio.NewScanner(stdin)
	for {
		line, multiline, ok, err := readChatInput(ctx, scanner, stderr)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}

		command := strings.TrimSpace(line)
		if command == "" {
			continue
		}
		if !multiline && (command == "/exit" || command == "/quit") {
			return nil
		}
		if !multiline && command == "/usage" {
			if err := runtime.writeUsageSummary(stderr); err != nil {
				return err
			}
			continue
		}

		updated, err := runChatTurn(ctx, runtime, messages, line, stdout, stderr, true, true)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if !isRecoverableTurnError(err) {
				return err
			}
			if _, printErr := fmt.Fprintf(stderr, "sai: %v\n", err); printErr != nil {
				return printErr
			}
			continue
		}
		messages = updated
	}
}

func (options agentCommandFlags) initialInputSourceCount() int {
	count := 0
	if options.prompt.set {
		count++
	}
	if options.stdin {
		count++
	}
	if options.file.set {
		count++
	}
	return count
}

func readInitialPrompt(options agentCommandFlags, stdin io.Reader) (string, bool, error) {
	switch {
	case options.prompt.set:
		return options.prompt.text, true, nil
	case options.stdin:
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", false, fmt.Errorf("read stdin prompt: %w", err)
		}
		return string(data), true, nil
	case options.file.set:
		data, err := os.ReadFile(options.file.path)
		if err != nil {
			return "", false, fmt.Errorf("read prompt file %q: %w", options.file.path, err)
		}
		return string(data), true, nil
	default:
		return "", false, nil
	}
}

const multilineInputDelimiter = `"""`

func readChatInput(ctx context.Context, scanner *bufio.Scanner, stderr io.Writer) (string, bool, bool, error) {
	line, ok, err := scanChatLine(ctx, scanner, stderr)
	if !ok || err != nil {
		return "", false, ok, err
	}
	if strings.TrimSpace(line) != multilineInputDelimiter {
		return line, false, true, nil
	}

	lines := []string{}
	for {
		line, ok, err := scanChatLine(ctx, scanner, stderr)
		if !ok || err != nil {
			return "", true, ok, err
		}
		if strings.TrimSpace(line) == multilineInputDelimiter {
			return strings.Join(lines, "\n"), true, true, nil
		}
		lines = append(lines, line)
	}
}

func scanChatLine(ctx context.Context, scanner *bufio.Scanner, stderr io.Writer) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if _, err := fmt.Fprint(stderr, "> "); err != nil {
		return "", false, err
	}
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", false, fmt.Errorf("read chat input: %w", err)
		}
		return "", false, nil
	}
	return scanner.Text(), true, nil
}

func runChatTurn(ctx context.Context, runtime *agentRuntime, messages []model.Message, prompt string, stdout, stderr io.Writer, addTrailingNewline bool, stderrNeedsLeadingBreak bool) ([]model.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	requestMessages := append(copyMessageSlice(messages), model.Message{
		Role:    model.MessageRoleUser,
		Content: prompt,
	})
	request := model.Request{
		Model:      runtime.modelID,
		Messages:   requestMessages,
		Tools:      runtime.toolSchemas,
		Parameters: runtime.parameters,
	}
	events, results, err := agent.StreamWithResult(turnCtx, request, agent.Options{
		Provider:     runtime.provider,
		ToolExecutor: runtime.toolExecutor,
		MaxTurns:     runtime.maxTurns,
	})
	if err != nil {
		return nil, newRecoverableTurnError(err)
	}

	tracker := &chatOutputWriter{w: stdout}
	if err := writeStreamWithOptions(tracker, stderr, events, runtime.showReasoning, runtime.logger, streamOutputOptions{
		colorReasoning:          shouldColorizeWriter(tracker),
		colorToolStatus:         shouldColorizeWriter(stderr),
		stderrNeedsLeadingBreak: stderrNeedsLeadingBreak,
	}); err != nil {
		return nil, err
	}
	result, ok := <-results
	if !ok {
		if err := turnCtx.Err(); err != nil {
			return nil, err
		}
		return nil, newRecoverableTurnError(fmt.Errorf("agent did not return updated messages"))
	}
	if err := runtime.saveUpdatedMessages(result.Messages); err != nil {
		return nil, err
	}
	if addTrailingNewline && tracker.wrote && tracker.lastByte != '\n' {
		if _, err := fmt.Fprintln(stdout); err != nil {
			return nil, err
		}
	}
	return result.Messages, nil
}

func intersperseFlagArgs(flags *flag.FlagSet, args []string) ([]string, error) {
	flagArgs := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	hasDelimiter := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			hasDelimiter = true
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if !isFlagArg(arg) {
			positionals = append(positionals, arg)
			continue
		}

		flagArgs = append(flagArgs, arg)
		name, hasInlineValue := flagName(arg)
		found := flags.Lookup(name)
		if found == nil || hasInlineValue || isBoolFlagValue(found.Value) {
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("flag needs an argument: -%s", name)
		}
		i++
		flagArgs = append(flagArgs, args[i])
	}

	if hasDelimiter {
		return append(append(flagArgs, "--"), positionals...), nil
	}
	return append(flagArgs, positionals...), nil
}

func isFlagArg(arg string) bool {
	return len(arg) > 1 && strings.HasPrefix(arg, "-")
}

func flagName(arg string) (string, bool) {
	name := strings.TrimLeft(arg, "-")
	if before, _, ok := strings.Cut(name, "="); ok {
		return before, true
	}
	return name, false
}

func isBoolFlagValue(value flag.Value) bool {
	boolFlag, ok := value.(interface {
		IsBoolFlag() bool
	})
	return ok && boolFlag.IsBoolFlag()
}

type agentRuntime struct {
	cwd                   string
	configDir             string
	providerName          string
	modelProfile          string
	modelID               string
	parameters            map[string]any
	provider              model.Provider
	toolExecutor          runToolExecutor
	toolSchemas           []model.Tool
	maxTurns              int
	showReasoning         bool
	enabledTools          []string
	enabledMCP            []string
	enabledSkills         []string
	baseMessages          []model.Message
	resumed               bool
	resumableSession      sessions.Session
	resumableSessionStore *sessions.Store
	saveSessions          bool
	sessionSaveNoticeDone bool
	contextTracker        *contextwindow.Tracker
	logger                *eventlog.Logger
	mcpSessions           []*mcp.Session
}

func (r *agentRuntime) Close() error {
	return errors.Join(r.logger.Close(), closeMCPSessions(r.mcpSessions))
}

func (r *agentRuntime) initialMessages() []model.Message {
	if r.resumed {
		return copyMessageSlice(r.resumableSession.Messages)
	}
	return copyMessageSlice(r.baseMessages)
}

func (r *agentRuntime) writeSessionSaveNotice(stderr io.Writer) error {
	if !r.saveSessions || r.sessionSaveNoticeDone || stderr == nil {
		return nil
	}
	if _, err := fmt.Fprintln(stderr, resumableSessionSaveNoticeText); err != nil {
		return err
	}
	r.sessionSaveNoticeDone = true
	return nil
}

func (r *agentRuntime) writeUsageSummary(stderr io.Writer) error {
	metadata := contextwindow.Metadata{}
	if r.contextTracker != nil {
		metadata = r.contextTracker.Metadata()
	}
	_, err := fmt.Fprintf(stderr, "CONTEXT_WINDOW\t%d\nCONTEXT_WINDOW_SOURCE\t%s\nCONTEXT_WARNING_THRESHOLD_PERCENT\t%d\nCONTEXT_LAST_REQUEST_TOKENS\t%d\nCONTEXT_LAST_INPUT_TOKENS\t%d\nCONTEXT_LAST_OUTPUT_TOKENS\t%d\nCONTEXT_LAST_TOTAL_TOKENS\t%d\nCONTEXT_LAST_USAGE_SOURCE\t%s\n",
		metadata.ContextWindow,
		formatContextMetadataValue(metadata.ContextWindowSource),
		metadata.WarningThresholdPercent,
		metadata.LastRequestTokens,
		metadata.LastInputTokens,
		metadata.LastOutputTokens,
		metadata.LastTotalTokens,
		formatContextMetadataValue(metadata.LastUsageSource),
	)
	return err
}

func formatContextMetadataValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(none)"
	}
	return value
}

func (r *agentRuntime) saveUpdatedMessages(messages []model.Message) error {
	if !r.saveSessions {
		return nil
	}
	if r.resumableSessionStore == nil {
		return fmt.Errorf("session store is not configured")
	}

	session := r.resumableSession
	session.Provider = r.providerName
	session.ModelProfile = r.modelProfile
	session.ModelID = r.modelID
	session.ModelParameters = copyParameterMap(r.parameters)
	session.CWD = r.cwd
	session.ConfigDir = r.configDir
	session.EnabledTools = copyStringSlice(r.enabledTools)
	session.EnabledMCP = copyStringSlice(r.enabledMCP)
	session.EnabledSkills = copyStringSlice(r.enabledSkills)
	session.ShowReasoning = r.showReasoning
	if len(session.InstructionsSnapshot) == 0 {
		session.InstructionsSnapshot = copyMessageSlice(r.baseMessages)
	}
	session.Messages = copyMessageSlice(messages)
	if r.contextTracker != nil {
		session.Context = r.contextTracker.Metadata()
	}
	session.SaveToolResults = true

	saved, err := r.resumableSessionStore.Save(session)
	if err != nil {
		return fmt.Errorf("save resumable session: %w", err)
	}
	r.resumableSession = saved
	return nil
}

func prepareAgentRuntime(ctx context.Context, configDir string, options agentCommandFlags, stderr io.Writer, getwd func() (string, error)) (runtime *agentRuntime, err error) {
	cwd, err := getwd()
	if err != nil {
		return nil, fmt.Errorf("get current directory: %w", err)
	}
	cfg, err := loadConfig(configDir, func() (string, error) {
		return cwd, nil
	})
	if err != nil {
		return nil, err
	}

	var resumedSession sessions.Session
	resumed := false
	saveSessions := cfg.Sessions.Enabled
	if options.saveSessionSet {
		saveSessions = options.saveSession
	}
	if options.resumeID != "" || options.continueSession {
		saveSessions = true
	}
	if saveSessions && !cfg.Sessions.SaveToolResults {
		return nil, fmt.Errorf("resumable sessions require sessions.save_tool_results: true")
	}
	sessionStore := sessions.NewStore(cfg.Sessions.Dir)
	if options.resumeID != "" || options.continueSession {
		resumedSession, err = loadResumableSession(sessionStore, options.resumeID, options.continueSession)
		if err != nil {
			return nil, err
		}
		if resumedSession.Version > sessions.CurrentVersion {
			return nil, fmt.Errorf("session %q uses unsupported version %d; current version is %d", resumedSession.ID, resumedSession.Version, sessions.CurrentVersion)
		}
		if !resumedSession.SaveToolResults {
			return nil, fmt.Errorf("session %q cannot be reliably resumed because save_tool_results is false", resumedSession.ID)
		}
		if err := validateResumeCLIConflicts(resumedSession, options); err != nil {
			return nil, err
		}
		applyResumeOptions(&options, resumedSession)
		resumed = true
	}

	resolved, err := cfg.ResolveModel(options.providerName, options.modelProfile)
	if err != nil {
		return nil, err
	}
	if resumed {
		if resumedSession.ModelID != "" {
			resolved.ModelID = resumedSession.ModelID
		}
		if resumedSession.ModelParameters != nil {
			resolved.Parameters = copyParameterMap(resumedSession.ModelParameters)
		}
	}
	provider, err := newProviderForRun(resolved.ProviderName, resolved.Provider)
	if err != nil {
		return nil, err
	}
	contextTracker := contextwindow.NewTracker(contextwindow.Window{
		Tokens: resolved.ContextWindow,
		Source: contextwindow.ParseWindowSource(resolved.ContextWindowSource),
	}, resumedSession.Context)
	provider = contextwindow.TrackingProvider{
		Inner:         provider,
		Tracker:       contextTracker,
		WarningWriter: stderr,
	}

	enabledToolNames := cfg.Tools.Enabled
	if options.enabledTools.set {
		enabledToolNames = options.enabledTools.names
	}
	toolRegistry, toolSchemas, err := enabledToolsForRun(cwd, enabledToolNames)
	if err != nil {
		return nil, err
	}
	selectedMCPServers, err := cfg.SelectedMCPServers(options.enabledMCP.ids, options.enabledMCP.set)
	if err != nil {
		return nil, err
	}
	var mcpSessions []*mcp.Session
	defer func() {
		if err != nil {
			err = errors.Join(err, closeMCPSessions(mcpSessions))
		}
	}()
	mcpSessions, mcpSessionsByID, mcpToolSchemas, err := mcpToolsForRun(ctx, selectedMCPServers, enabledToolNames)
	if err != nil {
		return nil, err
	}
	toolSchemas = append(toolSchemas, mcpToolSchemas...)

	resolvedShowReasoning := cfg.Agent.ShowReasoning
	if options.showReasoningSet {
		resolvedShowReasoning = options.showReasoning
	}
	if resumed {
		resolvedShowReasoning = resumedSession.ShowReasoning
	}

	var baseMessages []model.Message
	var enabledSkillIDs []string
	if resumed {
		baseMessages = copyMessageSlice(resumedSession.InstructionsSnapshot)
		enabledSkillIDs = copyStringSlice(resumedSession.EnabledSkills)
	} else {
		selectedSkills, err := enabledSkillsForRun(cfg, options.enabledSkills.ids, options.enabledSkills.set, options.disableSkills)
		if err != nil {
			return nil, err
		}
		project, err := projectcontext.Load(cwd)
		if err != nil {
			return nil, err
		}
		baseMessages = chatBaseMessages(project, selectedSkills)
		enabledSkillIDs = skillIDs(selectedSkills)
	}

	logger, err := eventlog.Open(cfg.Logging.Path, eventlog.Attributes{
		Provider: resolved.ProviderName,
		Model:    resolved.ModelID,
		Level:    cfg.Logging.Level,
	})
	if err != nil {
		return nil, err
	}
	if options.verbose {
		if err := writeVerboseDiagnostics(stderr, cfg, resolved, enabledToolNames, resolvedShowReasoning, logger.Path()); err != nil {
			return nil, errors.Join(err, logger.Close())
		}
	}

	return &agentRuntime{
		cwd:                   cwd,
		configDir:             cfg.ConfigDir,
		providerName:          resolved.ProviderName,
		modelProfile:          resolved.Profile,
		modelID:               resolved.ModelID,
		parameters:            resolved.Parameters,
		provider:              provider,
		toolExecutor:          runToolExecutor{builtins: toolRegistry, mcpSessions: mcpSessionsByID},
		toolSchemas:           toolSchemas,
		maxTurns:              cfg.Agent.MaxTurns,
		showReasoning:         resolvedShowReasoning,
		enabledTools:          copyStringSlice(enabledToolNames),
		enabledMCP:            mcpServerIDs(selectedMCPServers),
		enabledSkills:         enabledSkillIDs,
		baseMessages:          baseMessages,
		resumed:               resumed,
		resumableSession:      resumedSession,
		resumableSessionStore: sessionStore,
		saveSessions:          saveSessions,
		contextTracker:        contextTracker,
		logger:                logger,
		mcpSessions:           mcpSessions,
	}, nil
}

func loadResumableSession(store *sessions.Store, id string, latest bool) (sessions.Session, error) {
	if latest {
		session, err := store.Latest()
		if err != nil {
			if errors.Is(err, sessions.ErrNotFound) {
				return sessions.Session{}, fmt.Errorf("no resumable sessions found")
			}
			return sessions.Session{}, err
		}
		return session, nil
	}

	session, err := store.Load(id)
	if err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			return sessions.Session{}, fmt.Errorf("resumable session %q was not found", id)
		}
		return sessions.Session{}, err
	}
	return session, nil
}

func validateResumeCLIConflicts(session sessions.Session, options agentCommandFlags) error {
	if options.providerName != "" && options.providerName != session.Provider {
		return fmt.Errorf("cannot resume session %q with --provider %q; session uses provider %q", session.ID, options.providerName, session.Provider)
	}
	if options.modelProfile != "" && options.modelProfile != session.ModelProfile {
		return fmt.Errorf("cannot resume session %q with --model %q; session uses model profile %q", session.ID, options.modelProfile, session.ModelProfile)
	}
	if options.enabledTools.set && !sameStringSlice(options.enabledTools.names, session.EnabledTools) {
		return fmt.Errorf("cannot resume session %q with --enable-tools %q; session uses %q", session.ID, strings.Join(options.enabledTools.names, ","), strings.Join(session.EnabledTools, ","))
	}
	if options.enabledMCP.set && !sameStringSlice(options.enabledMCP.ids, session.EnabledMCP) {
		return fmt.Errorf("cannot resume session %q with --enable-mcp %q; session uses %q", session.ID, strings.Join(options.enabledMCP.ids, ","), strings.Join(session.EnabledMCP, ","))
	}
	if options.enabledSkills.set && !sameStringSlice(options.enabledSkills.ids, session.EnabledSkills) {
		return fmt.Errorf("cannot resume session %q with --enable-skills %q; session uses %q", session.ID, strings.Join(options.enabledSkills.ids, ","), strings.Join(session.EnabledSkills, ","))
	}
	if options.disableSkills && len(session.EnabledSkills) != 0 {
		return fmt.Errorf("cannot resume session %q with --disable-skills; session uses enabled skills %q", session.ID, strings.Join(session.EnabledSkills, ","))
	}
	if options.showReasoningSet && options.showReasoning != session.ShowReasoning {
		return fmt.Errorf("cannot resume session %q with --show-reasoning=%t; session was saved with show_reasoning %t", session.ID, options.showReasoning, session.ShowReasoning)
	}
	if options.saveSessionSet && !options.saveSession {
		return fmt.Errorf("cannot resume session %q with --save-session=false; session continuation must keep resumable session saving enabled", session.ID)
	}
	return nil
}

func applyResumeOptions(options *agentCommandFlags, session sessions.Session) {
	options.providerName = session.Provider
	options.modelProfile = session.ModelProfile
	options.enabledTools = toolNamesFlag{set: true, names: copyStringSlice(session.EnabledTools)}
	options.enabledMCP = mcpServerIDsFlag{set: true, ids: copyStringSlice(session.EnabledMCP)}
	options.enabledSkills = skillIDsFlag{set: true, ids: copyStringSlice(session.EnabledSkills)}
	options.disableSkills = false
	options.showReasoning = session.ShowReasoning
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mcpServerIDs(servers []config.MCPServerConfig) []string {
	if len(servers) == 0 {
		return nil
	}
	ids := make([]string, 0, len(servers))
	for _, server := range servers {
		ids = append(ids, server.ID)
	}
	return ids
}

func skillIDs(skills []localskills.Skill) []string {
	if len(skills) == 0 {
		return nil
	}
	ids := make([]string, 0, len(skills))
	for _, skill := range skills {
		ids = append(ids, skill.ID)
	}
	return ids
}

func copyStringSlice(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func copyParameterMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	copied := make(map[string]any, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

type chatOutputWriter struct {
	w        io.Writer
	wrote    bool
	lastByte byte
}

func (w *chatOutputWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 {
		w.wrote = true
		w.lastByte = p[n-1]
	}
	return n, err
}

func (w *chatOutputWriter) UnwrapWriter() io.Writer {
	return w.w
}

type promptTextFlag struct {
	set  bool
	text string
}

func (f *promptTextFlag) Set(value string) error {
	f.set = true
	f.text = value
	return nil
}

func (f *promptTextFlag) String() string {
	return f.text
}

type filePathFlag struct {
	set  bool
	path string
}

func (f *filePathFlag) Set(value string) error {
	f.set = true
	f.path = value
	return nil
}

func (f *filePathFlag) String() string {
	return f.path
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

func writeVerboseDiagnostics(stderr io.Writer, cfg *config.Config, resolved config.ResolvedModel, enabledTools []string, showReasoning bool, logPath string) error {
	_, err := fmt.Fprintf(stderr, "config_dir: %s\nprovider: %s\nmodel_profile: %s\nmodel_id: %s\nlog_path: %s\nmax_turns: %d\nenabled_tools: %s\nshow_reasoning: %t\n",
		cfg.ConfigDir,
		resolved.ProviderName,
		resolved.Profile,
		resolved.ModelID,
		verbosePath(logPath),
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

func chatBaseMessages(project projectcontext.Project, enabledSkills []localskills.Skill) []model.Message {
	instructions := projectcontext.ComposeInstructions(builtInBaseInstructions, project, "")
	messages := make([]model.Message, 0, len(instructions)+len(enabledSkills))
	for _, instruction := range instructions {
		if instruction.Source == projectcontext.InstructionSourceUser {
			for _, skill := range enabledSkills {
				messages = append(messages, model.Message{
					Role:    model.MessageRoleDeveloper,
					Content: formatSkillInstructions(skill),
				})
			}
			continue
		}
		messages = append(messages, model.Message{
			Role:    roleForInstruction(instruction.Source),
			Content: instruction.Content,
		})
	}
	return messages
}

func copyMessageSlice(messages []model.Message) []model.Message {
	copied := append([]model.Message(nil), messages...)
	for i := range copied {
		copied[i].ToolCalls = append([]model.ToolCall(nil), messages[i].ToolCalls...)
	}
	return copied
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
	colorReasoning          bool
	colorToolStatus         bool
	stderrNeedsLeadingBreak bool
}

func writeStream(stdout, stderr io.Writer, events <-chan model.Event, showReasoning bool, logger *eventlog.Logger) error {
	return writeStreamWithOptions(stdout, stderr, events, showReasoning, logger, streamOutputOptions{
		colorReasoning:  shouldColorizeWriter(stdout),
		colorToolStatus: shouldColorizeWriter(stderr),
	})
}

func writeStreamWithOptions(stdout, stderr io.Writer, events <-chan model.Event, showReasoning bool, logger *eventlog.Logger, options streamOutputOptions) (err error) {
	needsReasoningBreak := false
	inReasoningBlock := false
	reasoningEndedWithNewline := false
	reasoningColorActive := false
	stdoutAtLineStart := true
	stderrStatusSeparated := false
	stderrNeedsLeadingBreak := options.stderrNeedsLeadingBreak

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
	endReasoningForNonReasoningEvent := func() error {
		if err := resetReasoningColor(); err != nil {
			return err
		}
		inReasoningBlock = false
		return nil
	}
	defer func() {
		if resetErr := resetReasoningColor(); resetErr != nil {
			err = errors.Join(err, resetErr)
		}
	}()
	writeStdout := func(text string) error {
		if text == "" {
			return nil
		}
		if _, err := fmt.Fprint(stdout, text); err != nil {
			return err
		}
		stdoutAtLineStart = strings.HasSuffix(text, "\n")
		stderrStatusSeparated = false
		return nil
	}
	startReasoningBlock := func() error {
		if inReasoningBlock {
			return nil
		}
		if !stdoutAtLineStart {
			if err := writeStdout("\n"); err != nil {
				return err
			}
		}
		if err := startReasoningColor(); err != nil {
			return err
		}
		inReasoningBlock = true
		return nil
	}
	writeToolStatus := func(toolCall model.ToolCall) error {
		if stderr == nil || toolCall.Name == "" {
			return nil
		}
		if (!stdoutAtLineStart && !stderrStatusSeparated) || stderrNeedsLeadingBreak {
			if _, err := fmt.Fprint(stderr, "\n"); err != nil {
				return err
			}
		}
		stderrNeedsLeadingBreak = false
		stderrStatusSeparated = true
		if options.colorToolStatus {
			_, err := fmt.Fprintln(stderr, reasoningColorDarkGray+formatToolStatus(toolCall)+ansiReset)
			return err
		}
		_, err := fmt.Fprintln(stderr, formatToolStatus(toolCall))
		return err
	}

	for event := range events {
		if err := logger.LogEvent(event); err != nil {
			return err
		}
		switch event := event.(type) {
		case model.TextDeltaEvent:
			if err := endReasoningForNonReasoningEvent(); err != nil {
				return err
			}
			if event.Text != "" && needsReasoningBreak {
				if !reasoningEndedWithNewline {
					if err := writeStdout("\n"); err != nil {
						return err
					}
				}
				needsReasoningBreak = false
			}
			if err := writeStdout(event.Text); err != nil {
				return err
			}
		case model.ReasoningDeltaEvent:
			if showReasoning && event.Text != "" {
				if err := startReasoningBlock(); err != nil {
					return err
				}
				if err := writeStdout(event.Text); err != nil {
					return err
				}
				needsReasoningBreak = true
				reasoningEndedWithNewline = strings.HasSuffix(event.Text, "\n")
			}
		case model.ToolCallDoneEvent:
			if err := endReasoningForNonReasoningEvent(); err != nil {
				return err
			}
			if err := writeToolStatus(event.ToolCall); err != nil {
				return err
			}
		case model.ErrorEvent:
			if err := endReasoningForNonReasoningEvent(); err != nil {
				return err
			}
			return streamError(event)
		default:
			if err := endReasoningForNonReasoningEvent(); err != nil {
				return err
			}
		}
	}
	return nil
}

func formatToolStatus(toolCall model.ToolCall) string {
	status := "tool: " + toolCall.Name
	path, ok := toolStatusPath(toolCall)
	if ok {
		status += " " + path
	}
	return status
}

func toolStatusPath(toolCall model.ToolCall) (string, bool) {
	if toolCall.Name != tools.BuiltinReadFile && toolCall.Name != tools.BuiltinListFiles && toolCall.Name != tools.BuiltinWriteFile && toolCall.Name != tools.BuiltinEditFile {
		return "", false
	}
	if toolCall.Name == tools.BuiltinListFiles && strings.TrimSpace(toolCall.Arguments) == "" {
		return ".", true
	}

	var arguments map[string]any
	if err := json.Unmarshal([]byte(toolCall.Arguments), &arguments); err != nil {
		return "", false
	}
	if arguments == nil {
		return "", false
	}
	path, ok := arguments["path"]
	if !ok {
		if toolCall.Name == tools.BuiltinListFiles {
			return ".", true
		}
		return "", false
	}
	text, ok := path.(string)
	if !ok {
		return "", false
	}
	return text, true
}

func shouldColorizeWriter(stdout io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	for {
		unwrapper, ok := stdout.(interface{ UnwrapWriter() io.Writer })
		if !ok {
			break
		}
		unwrapped := unwrapper.UnwrapWriter()
		if unwrapped == nil || unwrapped == stdout {
			break
		}
		stdout = unwrapped
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
			return newRecoverableTurnError(fmt.Errorf("model stream error"))
		}
		return newRecoverableTurnError(fmt.Errorf("%s", event.Message))
	}
	if event.Message == "" {
		return newRecoverableTurnError(event.Err)
	}
	return newRecoverableTurnError(fmt.Errorf("%s: %w", event.Message, event.Err))
}

type recoverableTurnError struct {
	err error
}

func newRecoverableTurnError(err error) error {
	if err == nil {
		return nil
	}
	return recoverableTurnError{err: err}
}

func (e recoverableTurnError) Error() string {
	return e.err.Error()
}

func (e recoverableTurnError) Unwrap() error {
	return e.err
}

func isRecoverableTurnError(err error) bool {
	_, ok := err.(recoverableTurnError)
	return ok
}
