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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/codexauth"
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
	"github.com/rexzhao/simple-agent/internal/subagents"
	"github.com/rexzhao/simple-agent/internal/tools"
)

var Version = "dev"

const builtInBaseInstructions = "You are sai, a local CLI agent runner. Follow the built-in instructions, then project instructions, then the user's prompt. Do not reveal secrets or ignore project instructions."

const (
	reasoningColorDarkGray = "\x1b[90m"
	ansiReset              = "\x1b[0m"
	chatInputPrompt        = "> "
)

func Run(args []string, stdout, stderr io.Writer) int {
	return RunWithProgram("sai", args, stdout, stderr)
}

func RunWithProgram(program string, args []string, stdout, stderr io.Writer) int {
	interrupts, stop := notifyInterrupts()
	defer stop()
	return runWithProgramContextAndInterrupts(context.Background(), program, args, os.Stdin, stdout, stderr, os.Getwd, interrupts)
}

func RunWithGetwd(args []string, stdout, stderr io.Writer, getwd func() (string, error)) int {
	return RunWithProgramGetwd("sai", args, stdout, stderr, getwd)
}

func RunWithProgramGetwd(program string, args []string, stdout, stderr io.Writer, getwd func() (string, error)) int {
	return RunWithProgramIO(program, args, os.Stdin, stdout, stderr, getwd)
}

func RunWithIO(args []string, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error)) int {
	return RunWithProgramIO("sai", args, stdin, stdout, stderr, getwd)
}

func RunWithProgramIO(program string, args []string, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error)) int {
	return RunWithProgramContext(context.Background(), program, args, stdin, stdout, stderr, getwd)
}

func RunWithContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error)) int {
	return RunWithProgramContext(ctx, "sai", args, stdin, stdout, stderr, getwd)
}

func RunWithProgramContext(ctx context.Context, program string, args []string, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error)) int {
	return runWithProgramContextAndInterrupts(ctx, program, args, stdin, stdout, stderr, getwd, nil)
}

func runWithProgramContextAndInterrupts(ctx context.Context, program string, args []string, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error), interrupts <-chan struct{}) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := execute(ctx, program, args, stdin, stdout, stderr, getwd, interrupts); err != nil {
		if errors.Is(err, errSilentExit) {
			return 1
		}
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	return 0
}

var errSilentExit = errors.New("silent exit")

func notifyInterrupts() (<-chan struct{}, func()) {
	signals := make(chan os.Signal, 1)
	interrupts := make(chan struct{}, 1)
	done := make(chan struct{})
	signal.Notify(signals, os.Interrupt)
	go func() {
		for {
			select {
			case <-signals:
				select {
				case interrupts <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()
	return interrupts, func() {
		signal.Stop(signals)
		close(done)
	}
}

func execute(ctx context.Context, program string, args []string, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error), interrupts <-chan struct{}) error {
	rootArgs, err := splitRootArgs(args)
	if err != nil {
		return err
	}
	if rootArgs.command == "" {
		if rootArgs.hasHelp {
			printRootUsage(stdout)
			return nil
		}
		return chatCommand(ctx, rootArgs.commandArgs, rootArgs.configPath, stdin, stdout, stderr, getwd, program, interrupts)
	}
	if rootArgs.command != "chat" {
		var stop func()
		ctx, stop = contextWithInterruptCancel(ctx, interrupts)
		defer stop()
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
		return configShowCommand(subArgs, rootArgs.configPath, stdout, getwd, program)
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
		return modelsListCommand(subArgs, rootArgs.configPath, stdout, getwd, program)
	case "doctor":
		return doctorCommand(rootArgs.commandArgs, rootArgs.configPath, stdout, getwd, program)
	case "auth":
		return authCommand(ctx, rootArgs.commandArgs, rootArgs.configPath, stdout, getwd, program)
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
		return mcpListCommand(subArgs, rootArgs.configPath, stdout, getwd, program)
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
			return sessionsListCommand(subArgs, rootArgs.configPath, stdout, getwd, program)
		case "show":
			return sessionsShowCommand(subArgs, rootArgs.configPath, stdout, getwd, program)
		case "delete":
			return sessionsDeleteCommand(subArgs, rootArgs.configPath, stdout, getwd, program)
		case "prune":
			return sessionsPruneCommand(subArgs, rootArgs.configPath, stdout, getwd, program)
		default:
			return usageError("usage: sai sessions <list|show|delete|prune>", "", "sai help sessions")
		}
	case "chat":
		return chatCommand(ctx, rootArgs.commandArgs, rootArgs.configPath, stdin, stdout, stderr, getwd, program, interrupts)
	default:
		return usageError(fmt.Sprintf("unknown command %q", rootArgs.command), "", "sai help")
	}
}

func contextWithInterruptCancel(ctx context.Context, interrupts <-chan struct{}) (context.Context, func()) {
	if interrupts == nil {
		return ctx, func() {}
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		select {
		case <-interrupts:
			cancel()
		case <-cancelCtx.Done():
		case <-done:
		}
	}()
	return cancelCtx, func() {
		close(done)
		<-closed
	}
}

const rootUsageText = `usage: sai [--config file] [command] [args]

Commands:
  chat              Start a chat session
  config show        Print resolved config with secrets redacted
  models list        List configured provider model profiles
  auth               Manage provider authentication
  doctor            Check local configuration health
  tools list         List built-in tools
  mcp list           List configured MCP servers
  sessions           Manage resumable sessions
  version            Print version
  help [command]     Show usage

With no command, sai defaults to chat.

Run "sai help <command>" for command usage.
`

const chatUsageText = `usage: sai chat [--provider name] [--model profile] [--prompt text | --stdin | --file path] [--show-reasoning] [--verbose] [--enable-tools names] [--enable-mcp ids] [--save-session] [--resume id | --continue] [--quit]

Starts a line-oriented chat session using the configured provider and model. When
--prompt is provided, sai runs that turn first; --quit exits after that turn
instead of entering the REPL. --stdin and --file read one complete prompt and
must be used with --quit. Resumable sessions save full sensitive content,
including prompts, assistant output, assistant tool calls, and tool results.
`

const resumableSessionSaveNoticeText = "sai: resumable sessions enabled; full prompts, assistant output, and tool results will be saved to the session file."

const subagentCompletionExitWait = 250 * time.Millisecond

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

const doctorUsageText = `usage: sai doctor

Checks local configuration files, default model selection, enabled local tools,
skills, MCP server configuration, and JSONL log directory writability without
sending provider HTTP requests, starting MCP servers, running a model, or
printing secrets.
`

const authUsageText = `usage: sai auth <command>

Commands:
  auth codex login   Create or refresh a Codex OAuth provider

Run "sai help auth codex login" for command usage.
`

const authCodexUsageText = `usage: sai auth codex <command>

Commands:
  auth codex login   Create or refresh a Codex OAuth provider

Run "sai help auth codex login" for command usage.
`

const authCodexLoginUsageText = `usage: sai auth codex login [--provider name] [--force] [--issuer-url url | --user-code-url url --device-token-url url --token-url url] [--base-url url] [--model id] [--context-window tokens]

Uses OAuth device flow to create a Codex provider YAML in provider_dir and an
independent token JSON in auth_dir. The default provider name is codex.
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
	case "doctor":
		printDoctorUsage(stdout)
	case "auth":
		printAuthUsage(stdout)
	case "auth codex":
		printAuthCodexUsage(stdout)
	case "auth codex login":
		printAuthCodexLoginUsage(stdout)
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

func printDoctorUsage(stdout io.Writer) {
	fmt.Fprint(stdout, doctorUsageText)
}

func printAuthUsage(stdout io.Writer) {
	fmt.Fprint(stdout, authUsageText)
}

func printAuthCodexUsage(stdout io.Writer) {
	fmt.Fprint(stdout, authCodexUsageText)
}

func printAuthCodexLoginUsage(stdout io.Writer) {
	fmt.Fprint(stdout, authCodexLoginUsageText)
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
	configPath  string
	command     string
	commandArgs []string
	hasHelp     bool
}

func splitRootArgs(args []string) (rootArgs, error) {
	known := map[string]flagKind{
		"config":         flagKindValue,
		"provider":       flagKindValue,
		"model":          flagKindValue,
		"prompt":         flagKindValue,
		"file":           flagKindValue,
		"enable-tools":   flagKindValue,
		"enable-mcp":     flagKindValue,
		"resume":         flagKindValue,
		"keep":           flagKindValue,
		"h":              flagKindBool,
		"help":           flagKindBool,
		"show-reasoning": flagKindBool,
		"verbose":        flagKindBool,
		"stdin":          flagKindBool,
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
		if name == "config" {
			value, next, err := flagValue(args, i, name, hasInlineValue)
			if err != nil {
				return rootArgs{}, usageError(err.Error(), "", "sai help")
			}
			out.configPath = value
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
		if isFlagArg(arg) && name == "config" {
			value, next, err := flagValue(args.commandArgs, i, name, hasInlineValue)
			if err != nil {
				return rootArgs{}, usageError(err.Error(), "", "sai help")
			}
			args.configPath = value
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

func configShowCommand(args []string, configPath string, stdout io.Writer, getwd func() (string, error), program string) error {
	flags := flag.NewFlagSet("sai config show", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printConfigShowUsage, "sai help config show")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai config show", "", "sai help config show")
	}

	cfg, err := loadConfig(configPath, getwd, program)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(cfg)
}

func modelsListCommand(args []string, configPath string, stdout io.Writer, getwd func() (string, error), program string) error {
	flags := flag.NewFlagSet("sai models list", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printModelsListUsage, "sai help models list")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai models list", "", "sai help models list")
	}

	cfg, err := loadConfig(configPath, getwd, program)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "PROVIDER\tPROFILE\tMODEL ID")
	for _, model := range cfg.ModelList() {
		fmt.Fprintf(stdout, "%s\t%s\t%s\n", model.Provider, model.Profile, model.ID)
	}
	return nil
}

func doctorCommand(args []string, configPath string, stdout io.Writer, getwd func() (string, error), program string) error {
	flags := flag.NewFlagSet("sai doctor", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printDoctorUsage, "sai help doctor")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai doctor", "", "sai help doctor")
	}

	results := runDoctor(configPath, getwd, program)
	hasError := false
	for _, result := range results {
		if result.status == doctorStatusError {
			hasError = true
		}
		if _, err := fmt.Fprintf(stdout, "%s %s %s\n", result.status, result.subject, result.detail); err != nil {
			return err
		}
	}
	if hasError {
		return errSilentExit
	}
	return nil
}

func authCommand(ctx context.Context, args []string, configPath string, stdout io.Writer, getwd func() (string, error), program string) error {
	if len(args) == 0 || containsHelpArg(args) && len(args) == 1 {
		printAuthUsage(stdout)
		return nil
	}
	if args[0] != "codex" {
		return usageError("usage: sai auth codex login", "", "sai help auth")
	}
	return authCodexCommand(ctx, args[1:], configPath, stdout, getwd, program)
}

func authCodexCommand(ctx context.Context, args []string, configPath string, stdout io.Writer, getwd func() (string, error), program string) error {
	if len(args) == 0 || containsHelpArg(args) && len(args) == 1 {
		printAuthCodexUsage(stdout)
		return nil
	}
	if args[0] != "login" {
		return usageError("usage: sai auth codex login", "", "sai help auth codex")
	}
	return authCodexLoginCommand(ctx, args[1:], configPath, stdout, getwd, program)
}

func authCodexLoginCommand(ctx context.Context, args []string, configPath string, stdout io.Writer, getwd func() (string, error), program string) error {
	flags := flag.NewFlagSet("sai auth codex login", flag.ContinueOnError)
	providerName := flags.String("provider", "codex", "provider name")
	force := flags.Bool("force", false, "overwrite generated provider and auth files")
	issuerURL := flags.String("issuer-url", codexauth.DefaultIssuerURL, "OAuth issuer URL")
	userCodeURL := flags.String("user-code-url", "", "Codex headless auth user code endpoint URL")
	deviceTokenURL := flags.String("device-token-url", "", "Codex headless auth polling endpoint URL")
	tokenURL := flags.String("token-url", "", "OAuth token exchange endpoint URL")
	redirectURI := flags.String("redirect-uri", "", "OAuth authorization-code redirect URI")
	clientID := flags.String("client-id", codexauth.DefaultClientID, "OAuth client id")
	scope := flags.String("scope", codexauth.DefaultScope, "OAuth scope")
	baseURL := flags.String("base-url", codexauth.DefaultBaseURL, "Codex API base URL")
	modelID := flags.String("model", codexauth.DefaultModelID(), "default Codex model id")
	contextWindow := flags.Int("context-window", 400000, "model context window")
	pollInterval := flags.Duration("poll-interval", 0, "device flow polling interval")
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printAuthCodexLoginUsage, "sai help auth codex login")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai auth codex login", "", "sai help auth codex login")
	}
	name := strings.TrimSpace(*providerName)
	if err := validateGeneratedProviderName(name); err != nil {
		return err
	}
	if *contextWindow <= 0 {
		return fmt.Errorf("--context-window must be a positive integer")
	}

	cfg, err := loadBaseConfig(configPath, getwd, program)
	if err != nil {
		return err
	}
	providerPath := filepath.Join(cfg.ProviderDir, name+".yaml")
	authPath := filepath.Join(cfg.AuthDir, name+".json")
	if !*force {
		if err := failIfAnyExists(providerPath, authPath); err != nil {
			return err
		}
	}

	resolvedUserCodeURL := strings.TrimSpace(*userCodeURL)
	resolvedDeviceTokenURL := strings.TrimSpace(*deviceTokenURL)
	resolvedTokenURL := strings.TrimSpace(*tokenURL)
	resolvedRedirectURI := strings.TrimSpace(*redirectURI)
	if resolvedUserCodeURL == "" {
		resolvedUserCodeURL = codexauth.UserCodeURLForIssuer(*issuerURL)
	}
	if resolvedDeviceTokenURL == "" {
		resolvedDeviceTokenURL = codexauth.DeviceTokenURLForIssuer(*issuerURL)
	}
	if resolvedTokenURL == "" {
		resolvedTokenURL = codexauth.TokenURLForIssuer(*issuerURL)
	}
	if resolvedRedirectURI == "" {
		resolvedRedirectURI = codexauth.RedirectURIForIssuer(*issuerURL)
	}
	result, err := codexauth.DeviceLogin(ctx, codexauth.DeviceLoginOptions{
		UserCodeURL:    resolvedUserCodeURL,
		DeviceTokenURL: resolvedDeviceTokenURL,
		TokenURL:       resolvedTokenURL,
		RedirectURI:    resolvedRedirectURI,
		ClientID:       *clientID,
		Scope:          *scope,
		Output:         stdout,
		PollInterval:   *pollInterval,
	})
	if err != nil {
		return err
	}
	token := result.Token
	token.TokenURL = resolvedTokenURL
	token.ClientID = strings.TrimSpace(*clientID)
	if err := (codexauth.Store{Path: authPath}).Save(token); err != nil {
		return err
	}
	if err := writeCodexProviderFile(providerPath, authPath, name, *baseURL, *modelID, *contextWindow); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Saved provider %q to %s\n", name, providerPath)
	fmt.Fprintf(stdout, "Saved Codex auth token to %s\n", authPath)
	return nil
}

func loadBaseConfig(configPath string, getwd func() (string, error), program string) (*config.Config, error) {
	resolved, err := resolveConfigPath(configPath, getwd, program)
	if err != nil {
		return nil, err
	}
	return config.LoadBase(resolved)
}

func validateGeneratedProviderName(name string) error {
	if name == "" {
		return fmt.Errorf("--provider is required")
	}
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '-' || char == '_':
		default:
			return fmt.Errorf("--provider may contain only letters, digits, hyphen, and underscore")
		}
	}
	return nil
}

func failIfAnyExists(paths ...string) error {
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; pass --force to overwrite", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check %s: %w", path, err)
		}
	}
	return nil
}

func writeCodexProviderFile(providerPath, authPath, providerName, baseURL, modelID string, contextWindow int) error {
	if err := os.MkdirAll(filepath.Dir(providerPath), 0o755); err != nil {
		return fmt.Errorf("create provider dir: %w", err)
	}
	relAuthPath, err := filepath.Rel(filepath.Dir(providerPath), authPath)
	if err != nil {
		relAuthPath = authPath
	}
	relAuthPath = filepath.ToSlash(relAuthPath)
	data := fmt.Sprintf("name: %s\nbase_url: %s\nauth_file: %s\n\nmodels:\n  %s:\n    id: %s\n    type: openai-codex\n    context_window: %s\n    parameters:\n      store: false\n      reasoning:\n        effort: high\n", providerName, strings.TrimSpace(baseURL), relAuthPath, modelID, modelID, strconv.Itoa(contextWindow))
	if err := os.WriteFile(providerPath, []byte(data), 0o600); err != nil {
		return fmt.Errorf("write provider file %q: %w", providerPath, err)
	}
	return nil
}

type doctorStatus string

const (
	doctorStatusOK    doctorStatus = "OK"
	doctorStatusWarn  doctorStatus = "WARN"
	doctorStatusError doctorStatus = "ERROR"
)

type doctorResult struct {
	status  doctorStatus
	subject string
	detail  string
}

func runDoctor(configPath string, getwd func() (string, error), program string) []doctorResult {
	var results []doctorResult
	add := func(status doctorStatus, subject, detail string) {
		results = append(results, doctorResult{status: status, subject: subject, detail: detail})
	}

	resolvedConfigPath, err := resolveConfigPath(configPath, getwd, program)
	if err != nil {
		add(doctorStatusError, "config_path", err.Error())
		return results
	}
	add(doctorStatusOK, "config_path", resolvedConfigPath)
	if _, err := os.ReadFile(resolvedConfigPath); err != nil {
		add(doctorStatusError, "config_file", fmt.Sprintf("%s: %v", resolvedConfigPath, err))
		return results
	}
	add(doctorStatusOK, "config_file", fmt.Sprintf("%s readable", resolvedConfigPath))

	cfg, err := config.LoadBase(resolvedConfigPath)
	if err != nil {
		add(doctorStatusError, "config_file", err.Error())
		return results
	}

	providersLoaded := checkDoctorProviders(cfg, add)
	if providersLoaded {
		checkDoctorDefaultModel(cfg, add)
	}
	selectedMCPServers := checkDoctorMCP(cfg, add)
	checkDoctorSkills(cfg, add)
	checkDoctorTools(cfg, selectedMCPServers, getwd, add)
	checkDoctorLogging(cfg, add)
	return results
}

func checkDoctorProviders(cfg *config.Config, add func(doctorStatus, string, string)) bool {
	providers, err := config.LoadProviderConfigs(cfg.ProviderDir)
	if err != nil {
		add(doctorStatusError, "provider_files", err.Error())
		return false
	}
	cfg.Providers = providers
	add(doctorStatusOK, "provider_files", fmt.Sprintf("%d loaded from %s", len(providers), cfg.ProviderDir))
	return true
}

func checkDoctorDefaultModel(cfg *config.Config, add func(doctorStatus, string, string)) {
	resolved, err := cfg.ResolveModel("", "")
	if err != nil {
		subject := "default_model"
		if strings.Contains(err.Error(), "api_key") {
			subject = "api_key"
		}
		add(doctorStatusError, subject, err.Error())
		return
	}
	add(doctorStatusOK, "default_model", fmt.Sprintf("%s/%s -> %s", resolved.ProviderName, resolved.Profile, resolved.ModelID))
	if resolved.Type == config.ProviderTypeOpenAICodex {
		if strings.TrimSpace(resolved.Provider.AuthFile) == "" {
			add(doctorStatusError, "auth_file", fmt.Sprintf("provider %q has no auth_file configured", resolved.ProviderName))
			return
		}
		if _, err := os.Stat(resolved.Provider.AuthFile); err != nil {
			add(doctorStatusError, "auth_file", fmt.Sprintf("%s: %v", resolved.Provider.AuthFile, err))
			return
		}
		add(doctorStatusOK, "auth_file", fmt.Sprintf("provider %q configured", resolved.ProviderName))
		return
	}
	if strings.TrimSpace(resolved.Provider.ResolvedAPIKey) == "" {
		add(doctorStatusError, "api_key", fmt.Sprintf("provider %q has no api_key configured", resolved.ProviderName))
		return
	}
	add(doctorStatusOK, "api_key", fmt.Sprintf("provider %q configured", resolved.ProviderName))
}

func checkDoctorMCP(cfg *config.Config, add func(doctorStatus, string, string)) []config.MCPServerConfig {
	if strings.TrimSpace(cfg.MCPDir) == "" {
		add(doctorStatusWarn, "mcp_dir", "disabled; no MCP servers configured")
		return nil
	}

	mcpDirExists := true
	if err := checkExistingDirectory(cfg.MCPDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			mcpDirExists = false
			add(doctorStatusWarn, "mcp_dir", fmt.Sprintf("%s not found; no MCP servers configured", cfg.MCPDir))
		} else {
			add(doctorStatusError, "mcp_dir", fmt.Sprintf("%s: %v", cfg.MCPDir, err))
			return nil
		}
	}

	servers, err := config.LoadMCPServerConfigs(cfg.MCPDir)
	if err != nil {
		add(doctorStatusError, "mcp_dir", err.Error())
		return nil
	}
	cfg.MCPServers = servers
	if mcpDirExists {
		add(doctorStatusOK, "mcp_dir", fmt.Sprintf("%d servers loaded from %s", len(servers), cfg.MCPDir))
	}

	selected, err := cfg.SelectedMCPServers(nil, false)
	if err != nil {
		add(doctorStatusError, "enabled_mcp", err.Error())
		return nil
	}
	if len(selected) == 0 {
		add(doctorStatusOK, "enabled_mcp", "(none)")
		return nil
	}
	add(doctorStatusOK, "enabled_mcp", formatDoctorServerIDs(selected))
	return selected
}

func checkDoctorSkills(cfg *config.Config, add func(doctorStatus, string, string)) {
	if len(cfg.SkillDirs) == 0 {
		add(doctorStatusOK, "skill_dirs", "disabled")
		add(doctorStatusOK, "loaded_skills", "(none)")
		return
	}

	existingDirs := make([]string, 0, len(cfg.SkillDirs))
	for _, dir := range cfg.SkillDirs {
		if strings.TrimSpace(dir) == "" {
			add(doctorStatusError, "skill_dirs", "empty path")
			continue
		}
		if err := checkExistingDirectory(dir); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				add(doctorStatusWarn, "skill_dirs", fmt.Sprintf("%s not found; no skills loaded from this directory", dir))
			} else {
				add(doctorStatusError, "skill_dirs", fmt.Sprintf("%s: %v", dir, err))
			}
		} else {
			existingDirs = append(existingDirs, dir)
		}
	}

	discovered, err := localskills.DiscoverDirs(existingDirs)
	if err != nil {
		add(doctorStatusError, "skill_dirs", err.Error())
		return
	}
	loaded := modelInvokedSkills(discovered)
	add(doctorStatusOK, "skill_dirs", fmt.Sprintf("%d configured, %d discovered, %d loaded", len(cfg.SkillDirs), len(discovered), len(loaded)))
	if len(loaded) == 0 {
		add(doctorStatusOK, "loaded_skills", "(none)")
		return
	}
	add(doctorStatusOK, "loaded_skills", strings.Join(skillIDs(loaded), ","))
}

func checkDoctorTools(cfg *config.Config, selectedMCPServers []config.MCPServerConfig, getwd func() (string, error), add func(doctorStatus, string, string)) {
	if len(cfg.Tools.Enabled) == 0 {
		add(doctorStatusOK, "enabled_tools", "(none)")
		return
	}

	errorsBefore := 0
	addToolError := func(detail string) {
		errorsBefore++
		add(doctorStatusError, "enabled_tools", detail)
	}

	for _, name := range cfg.Tools.Enabled {
		if subagents.IsTool(name) {
			addToolError(explicitSubagentToolError(name).Error())
		}
	}

	if hasBuiltinEnabledTool(cfg.Tools.Enabled) {
		cwd, err := getwd()
		if err != nil {
			addToolError(fmt.Sprintf("get current directory: %v", err))
		} else if _, _, err := enabledToolsForRun(cwd, withoutSubagentToolNames(cfg.Tools.Enabled)); err != nil {
			addToolError(err.Error())
		}
	}

	selectedMCP := make(map[string]bool, len(selectedMCPServers))
	for _, server := range selectedMCPServers {
		selectedMCP[server.ID] = true
	}
	for _, name := range cfg.Tools.Enabled {
		if !strings.HasPrefix(name, "mcp.") {
			continue
		}
		serverID, _, err := mcp.ParseToolName(name)
		if err != nil {
			addToolError(err.Error())
			continue
		}
		if _, ok := cfg.MCPServers[serverID]; !ok {
			addToolError(fmt.Sprintf("enabled MCP tool %q references unknown MCP server %q", name, serverID))
			continue
		}
		if !selectedMCP[serverID] {
			addToolError(fmt.Sprintf("enabled MCP tool %q references MCP server %q, but it is not enabled", name, serverID))
		}
	}

	if errorsBefore == 0 {
		add(doctorStatusOK, "enabled_tools", strings.Join(cfg.Tools.Enabled, ","))
	}
}

func checkDoctorLogging(cfg *config.Config, add func(doctorStatus, string, string)) {
	if strings.TrimSpace(cfg.Logging.Path) == "" {
		add(doctorStatusOK, "logging", "disabled")
		return
	}

	root := doctorLogSessionRoot(cfg.Logging.Path)
	if err := probeWritableDirectory(root); err != nil {
		add(doctorStatusError, "logging", fmt.Sprintf("%s: %v", root, err))
		return
	}
	add(doctorStatusOK, "logging", fmt.Sprintf("%s writable", root))
}

func resolveConfigPath(configPath string, getwd func() (string, error), program string) (string, error) {
	if strings.TrimSpace(configPath) == "" {
		cwd, err := getwd()
		if err != nil {
			return "", fmt.Errorf("get current directory: %w", err)
		}
		configPath = filepath.Join(cwd, ".agents", defaultConfigBasename(program)+".yaml")
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return "", fmt.Errorf("resolve config file: %w", err)
	}
	return filepath.Clean(abs), nil
}

func defaultConfigBasename(program string) string {
	base := strings.TrimSpace(filepath.Base(program))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "sai"
	}
	if strings.EqualFold(filepath.Ext(base), ".exe") {
		base = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if base == "" {
		return "sai"
	}
	return base
}

func checkExistingDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}
	return nil
}

func hasBuiltinEnabledTool(enabled []string) bool {
	for _, name := range enabled {
		if !strings.HasPrefix(name, "mcp.") && !subagents.IsTool(name) {
			return true
		}
	}
	return false
}

func formatDoctorServerIDs(servers []config.MCPServerConfig) string {
	if len(servers) == 0 {
		return "(none)"
	}
	ids := make([]string, 0, len(servers))
	for _, server := range servers {
		ids = append(ids, server.ID)
	}
	return strings.Join(ids, ",")
}

func doctorLogSessionRoot(path string) string {
	path = filepath.Clean(path)
	if filepath.Ext(filepath.Base(path)) != "" {
		return filepath.Dir(path)
	}
	return path
}

func probeWritableDirectory(path string) error {
	path = filepath.Clean(path)
	if strings.TrimSpace(path) == "" || path == "." {
		return fmt.Errorf("directory is required")
	}

	created := missingDirectoryAncestors(path)
	if err := os.MkdirAll(path, 0o755); err != nil {
		cleanupCreatedDirectories(created)
		return fmt.Errorf("create directory: %w", err)
	}
	defer cleanupCreatedDirectories(created)

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}

	file, err := os.CreateTemp(path, ".sai-doctor-*")
	if err != nil {
		return fmt.Errorf("create probe file: %w", err)
	}
	name := file.Name()
	closeErr := file.Close()
	removeErr := os.Remove(name)
	if closeErr != nil {
		return fmt.Errorf("close probe file: %w", closeErr)
	}
	if removeErr != nil {
		return fmt.Errorf("remove probe file: %w", removeErr)
	}
	return nil
}

func missingDirectoryAncestors(path string) []string {
	var missing []string
	for {
		if _, err := os.Stat(path); err == nil {
			return missing
		} else if !errors.Is(err, os.ErrNotExist) {
			return missing
		}

		missing = append(missing, path)
		parent := filepath.Dir(path)
		if parent == path {
			return missing
		}
		path = parent
	}
}

func cleanupCreatedDirectories(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

func mcpListCommand(args []string, configPath string, stdout io.Writer, getwd func() (string, error), program string) error {
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

	cfg, err := loadConfig(configPath, getwd, program)
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

func sessionsListCommand(args []string, configPath string, stdout io.Writer, getwd func() (string, error), program string) error {
	flags := flag.NewFlagSet("sai sessions list", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printSessionsListUsage, "sai help sessions list")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai sessions list", "", "sai help sessions list")
	}

	store, err := sessionStoreFromConfig(configPath, getwd, program)
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

func sessionsShowCommand(args []string, configPath string, stdout io.Writer, getwd func() (string, error), program string) error {
	flags := flag.NewFlagSet("sai sessions show", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printSessionsShowUsage, "sai help sessions show")
	if done || err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usageError("usage: sai sessions show <id>", "", "sai help sessions show")
	}

	store, err := sessionStoreFromConfig(configPath, getwd, program)
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
	fmt.Fprintf(stdout, "CONFIG_PATH\t%s\n", session.RootConfigPath())
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

func sessionsDeleteCommand(args []string, configPath string, stdout io.Writer, getwd func() (string, error), program string) error {
	flags := flag.NewFlagSet("sai sessions delete", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printSessionsDeleteUsage, "sai help sessions delete")
	if done || err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usageError("usage: sai sessions delete <id>", "", "sai help sessions delete")
	}

	store, err := sessionStoreFromConfig(configPath, getwd, program)
	if err != nil {
		return err
	}
	if err := store.Delete(positionals[0]); err != nil {
		return readableSessionNotFound(err, positionals[0])
	}
	fmt.Fprintf(stdout, "deleted session %s\n", positionals[0])
	return nil
}

func sessionsPruneCommand(args []string, configPath string, stdout io.Writer, getwd func() (string, error), program string) error {
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

	store, err := sessionStoreFromConfig(configPath, getwd, program)
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

func sessionStoreFromConfig(configPath string, getwd func() (string, error), program string) (*sessions.Store, error) {
	cfg, err := loadConfig(configPath, getwd, program)
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
		tools.BuiltinGlobFiles,
		tools.BuiltinGrepFiles,
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
	flags.Var(&options.enabledMCP, "enable-mcp", "comma-separated MCP server ids to enable")
	flags.BoolVar(&options.saveSession, "save-session", false, "save a resumable session with full sensitive content")
	flags.StringVar(&options.resumeID, "resume", "", "resume a saved session id")
	flags.BoolVar(&options.continueSession, "continue", false, "resume the latest saved session")
}

func (options agentCommandFlags) validate(helpCommand string) error {
	if options.resumeID != "" && options.continueSession {
		return usageError("cannot use --resume with --continue", "", helpCommand)
	}
	return nil
}

func chatCommand(ctx context.Context, args []string, configPath string, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error), program string, interrupts <-chan struct{}) (chatErr error) {
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

	runtime, err := prepareAgentRuntime(ctx, configPath, options, stderr, getwd, program)
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
		updated, err := runChatTurnAndCompletions(ctx, runtime, messages, initialPrompt, stdout, stderr, !*quit, false, interrupts)
		if err != nil {
			if *quit {
				return err
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if !isRecoverableTurnError(err) {
				return err
			}
			if _, printErr := fmt.Fprintf(stderr, "sai: %v\n", err); printErr != nil {
				return printErr
			}
		} else {
			messages = updated
		}
		if *quit {
			messages, err = runCompletionTurnsWithOptionalWait(ctx, runtime, messages, stdout, stderr, false, false, subagentCompletionExitWait, interrupts)
			if err != nil {
				return err
			}
			return nil
		}
	}

	scanner := bufio.NewScanner(stdin)
	var inputCh <-chan chatInputEvent
	for {
		if inputCh == nil {
			inputCh = startChatInputRead(ctx, scanner, stderr)
		}

		select {
		case input := <-inputCh:
			inputCh = nil
			if input.err != nil {
				return input.err
			}
			if !input.ok {
				return nil
			}

			command := strings.TrimSpace(input.line)
			if command == "" {
				continue
			}
			if !input.multiline && (command == "/exit" || command == "/quit") {
				return nil
			}
			if !input.multiline && command == "/usage" {
				if err := runtime.writeUsageSummary(stderr); err != nil {
					return err
				}
				continue
			}

			updated, err := runChatTurnAndCompletions(ctx, runtime, messages, input.line, stdout, stderr, true, true, interrupts)
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
		case <-interrupts:
			return context.Canceled
		case <-runtime.subagentCompletionSignal():
			redrawPrompt := inputCh != nil
			if redrawPrompt {
				if _, err := fmt.Fprint(stderr, "\n"); err != nil {
					return err
				}
			}
			updated, err := runAvailableCompletionTurns(ctx, runtime, messages, stdout, stderr, true, true, interrupts)
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
				if redrawPrompt {
					if _, printErr := fmt.Fprint(stderr, chatInputPrompt); printErr != nil {
						return printErr
					}
				}
				continue
			}
			messages = updated
			if redrawPrompt {
				if _, err := fmt.Fprint(stderr, chatInputPrompt); err != nil {
					return err
				}
			}
		}
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

type chatInputEvent struct {
	line      string
	multiline bool
	ok        bool
	err       error
}

func startChatInputRead(ctx context.Context, scanner *bufio.Scanner, stderr io.Writer) <-chan chatInputEvent {
	ch := make(chan chatInputEvent, 1)
	go func() {
		line, multiline, ok, err := readChatInput(ctx, scanner, stderr)
		ch <- chatInputEvent{line: line, multiline: multiline, ok: ok, err: err}
		close(ch)
	}()
	return ch
}

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
	if _, err := fmt.Fprint(stderr, chatInputPrompt); err != nil {
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

func runChatTurn(ctx context.Context, runtime *agentRuntime, messages []model.Message, prompt string, stdout, stderr io.Writer, addTrailingNewline bool, stderrNeedsLeadingBreak bool, interrupts <-chan struct{}) ([]model.Message, error) {
	requestMessages := append(copyMessageSlice(messages), model.Message{
		Role:    model.MessageRoleUser,
		Content: prompt,
	})
	return runChatMessages(ctx, runtime, requestMessages, stdout, stderr, addTrailingNewline, stderrNeedsLeadingBreak, interrupts)
}

func runChatTurnAndCompletions(ctx context.Context, runtime *agentRuntime, messages []model.Message, prompt string, stdout, stderr io.Writer, addTrailingNewline bool, stderrNeedsLeadingBreak bool, interrupts <-chan struct{}) ([]model.Message, error) {
	updated, err := runChatTurn(ctx, runtime, messages, prompt, stdout, stderr, addTrailingNewline, stderrNeedsLeadingBreak, interrupts)
	if err != nil {
		return nil, err
	}
	return runAvailableCompletionTurns(ctx, runtime, updated, stdout, stderr, addTrailingNewline, true, interrupts)
}

func runChatMessages(ctx context.Context, runtime *agentRuntime, requestMessages []model.Message, stdout, stderr io.Writer, addTrailingNewline bool, stderrNeedsLeadingBreak bool, interrupts <-chan struct{}) ([]model.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	doneInterrupts := forwardTurnInterrupts(turnCtx, cancel, interrupts)
	defer doneInterrupts()

	request := model.Request{
		Model:      runtime.modelID,
		Messages:   copyMessageSlice(requestMessages),
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
		if errors.Is(err, context.Canceled) && ctx.Err() == nil {
			return nil, newRecoverableTurnError(context.Canceled)
		}
		return nil, err
	}
	result, ok := <-results
	if !ok {
		if err := turnCtx.Err(); err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() == nil {
				return nil, newRecoverableTurnError(context.Canceled)
			}
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

func forwardTurnInterrupts(ctx context.Context, cancel context.CancelFunc, interrupts <-chan struct{}) func() {
	if interrupts == nil {
		return func() {}
	}
	done := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			select {
			case <-interrupts:
				cancel()
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		<-closed
		for {
			select {
			case <-interrupts:
			default:
				return
			}
		}
	}
}

func runAvailableCompletionTurns(ctx context.Context, runtime *agentRuntime, messages []model.Message, stdout, stderr io.Writer, addTrailingNewline bool, stderrNeedsLeadingBreak bool, interrupts <-chan struct{}) ([]model.Message, error) {
	if runtime == nil || runtime.subagentManager == nil {
		return messages, nil
	}

	for {
		completions := runtime.subagentManager.PendingCompletions()
		if len(completions) == 0 {
			return messages, nil
		}
		requestMessages := append(copyMessageSlice(messages), subagentCompletionMessages(completions)...)
		updated, err := runChatMessages(ctx, runtime, requestMessages, stdout, stderr, addTrailingNewline, stderrNeedsLeadingBreak, interrupts)
		if err != nil {
			return nil, err
		}
		if err := logSubagentCompletionEvents(runtime.logger, completions); err != nil {
			return nil, err
		}
		runtime.subagentManager.AckCompletions(completions)
		messages = updated
		stderrNeedsLeadingBreak = true
	}
}

func logSubagentCompletionEvents(logger *eventlog.Logger, completions []subagents.JobSnapshot) error {
	for _, completion := range completions {
		if err := logger.LogEvent(model.SubagentCompletionEvent{
			JobID:       completion.JobID,
			AgentID:     completion.AgentID,
			DisplayName: completion.DisplayName,
			JobName:     completion.JobName,
			Status:      string(completion.Status),
		}); err != nil {
			return err
		}
	}
	return nil
}

func runCompletionTurnsWithOptionalWait(ctx context.Context, runtime *agentRuntime, messages []model.Message, stdout, stderr io.Writer, addTrailingNewline bool, stderrNeedsLeadingBreak bool, wait time.Duration, interrupts <-chan struct{}) ([]model.Message, error) {
	updated, err := runAvailableCompletionTurns(ctx, runtime, messages, stdout, stderr, addTrailingNewline, stderrNeedsLeadingBreak, interrupts)
	if err != nil {
		return nil, err
	}
	messages = updated
	if runtime == nil || runtime.subagentManager == nil || wait <= 0 || !runtime.subagentManager.HasJobs() {
		return messages, nil
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-runtime.subagentManager.CompletionSignal():
		return runAvailableCompletionTurns(ctx, runtime, messages, stdout, stderr, addTrailingNewline, true, interrupts)
	case <-timer.C:
		return runAvailableCompletionTurns(ctx, runtime, messages, stdout, stderr, addTrailingNewline, true, interrupts)
	case <-interrupts:
		return nil, context.Canceled
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func subagentCompletionMessages(completions []subagents.JobSnapshot) []model.Message {
	messages := make([]model.Message, 0, len(completions))
	for _, completion := range completions {
		messages = append(messages, model.Message{
			Role:    model.MessageRoleUser,
			Content: formatSubagentCompletionEvent(completion),
		})
	}
	return messages
}

func formatSubagentCompletionEvent(completion subagents.JobSnapshot) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Runtime event: subagent job %s", completion.Status)
	fmt.Fprintf(&out, "\njob_id: %s", completion.JobID)
	fmt.Fprintf(&out, "\nagent_id: %s", completion.AgentID)
	if completion.DisplayName != "" {
		fmt.Fprintf(&out, "\ndisplay_name: %s", completion.DisplayName)
	}
	if completion.JobName != "" {
		fmt.Fprintf(&out, "\njob_name: %s", completion.JobName)
	}
	fmt.Fprintf(&out, "\nstatus: %s", completion.Status)
	if completion.Status == subagents.StatusCompleted || completion.Output != "" {
		writeCompletionValue(&out, "output", completion.Output)
	}
	if completion.Status == subagents.StatusFailed || completion.Status == subagents.StatusCanceled || completion.Error != "" {
		writeCompletionValue(&out, "error", completion.Error)
	}
	return out.String()
}

func writeCompletionValue(out *strings.Builder, label, value string) {
	if value == "" {
		fmt.Fprintf(out, "\n%s: \"\"", label)
		return
	}
	if strings.Contains(value, "\n") {
		fmt.Fprintf(out, "\n%s:\n%s", label, value)
		return
	}
	fmt.Fprintf(out, "\n%s: %s", label, value)
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
	configPath            string
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
	instructionSources    []sessions.InstructionSource
	resumed               bool
	resumableSession      sessions.Session
	resumableSessionStore *sessions.Store
	saveSessions          bool
	sessionSaveNoticeDone bool
	contextTracker        *contextwindow.Tracker
	logger                *eventlog.Logger
	mcpSessions           []*mcp.Session
	subagentManager       *subagents.Manager
	subagentCancel        context.CancelFunc
}

func (r *agentRuntime) Close() error {
	var subagentErr error
	if r.subagentCancel != nil {
		r.subagentCancel()
	}
	if r.subagentManager != nil {
		subagentErr = r.subagentManager.Close()
	}
	return errors.Join(subagentErr, r.logger.Close(), closeMCPSessions(r.mcpSessions))
}

func (r *agentRuntime) subagentCompletionSignal() <-chan struct{} {
	if r == nil || r.subagentManager == nil {
		return nil
	}
	return r.subagentManager.CompletionSignal()
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
	session.ConfigPath = r.configPath
	session.ConfigDir = ""
	session.EnabledTools = copyStringSlice(r.enabledTools)
	session.EnabledMCP = copyStringSlice(r.enabledMCP)
	session.EnabledSkills = copyStringSlice(r.enabledSkills)
	session.ShowReasoning = r.showReasoning
	if len(session.InstructionsSnapshot) == 0 {
		session.InstructionsSnapshot = copyMessageSlice(r.baseMessages)
	}
	if len(session.InstructionSources) == 0 {
		session.InstructionSources = copyInstructionSources(r.instructionSources)
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

type runtimePreparationOptions struct {
	enableSubagents bool
}

func prepareAgentRuntime(ctx context.Context, configPath string, options agentCommandFlags, stderr io.Writer, getwd func() (string, error), program string) (runtime *agentRuntime, err error) {
	return prepareAgentRuntimeWithOptions(ctx, configPath, options, stderr, getwd, program, runtimePreparationOptions{
		enableSubagents: true,
	})
}

func prepareAgentRuntimeWithOptions(ctx context.Context, configPath string, options agentCommandFlags, stderr io.Writer, getwd func() (string, error), program string, prep runtimePreparationOptions) (runtime *agentRuntime, err error) {
	cwd, err := getwd()
	if err != nil {
		return nil, fmt.Errorf("get current directory: %w", err)
	}
	cfg, err := loadConfig(configPath, func() (string, error) {
		return cwd, nil
	}, program)
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
	provider, err := newProviderForRun(resolved.ProviderName, resolved.Type, resolved.Provider)
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

	var subagentManager *subagents.Manager
	var subagentCancel context.CancelFunc
	defer func() {
		if err != nil && subagentCancel != nil {
			subagentCancel()
		}
	}()
	if prep.enableSubagents && len(cfg.Subagents) > 0 {
		subagentCtx, cancel := context.WithCancel(ctx)
		subagentCancel = cancel
		subagentManager, err = subagents.NewManager(cfg.Subagents, cliSubagentRunner{
			cwd:     cwd,
			program: program,
		}, subagents.WithRootContext(subagentCtx))
		if err != nil {
			return nil, err
		}
		toolSchemas = append(toolSchemas, subagents.Definitions()...)
	}

	resolvedShowReasoning := cfg.Agent.ShowReasoning
	if options.showReasoningSet {
		resolvedShowReasoning = options.showReasoning
	}
	if resumed {
		resolvedShowReasoning = resumedSession.ShowReasoning
	}

	var baseMessages []model.Message
	var instructionSources []sessions.InstructionSource
	var enabledSkillIDs []string
	if resumed {
		baseMessages = copyMessageSlice(resumedSession.InstructionsSnapshot)
		instructionSources = copyInstructionSources(resumedSession.InstructionSources)
		enabledSkillIDs = copyStringSlice(resumedSession.EnabledSkills)
	} else {
		configuredDeveloperMessages, err := promptDeveloperMessagesForRun(cfg, cwd, prep.enableSubagents)
		if err != nil {
			return nil, err
		}
		selectedSkills, err := enabledSkillsForRun(cfg)
		if err != nil {
			return nil, err
		}
		project, err := projectcontext.LoadWithOptions(projectcontext.LoadOptions{
			Directory:        cwd,
			ConfigDir:        filepath.Dir(cfg.ConfigPath),
			InstructionFiles: cfg.Agent.InstructionFiles,
			WarningWriter:    stderr,
		})
		if err != nil {
			return nil, err
		}
		baseMessages = chatBaseMessages(project, selectedSkills, configuredDeveloperMessages)
		instructionSources = chatInstructionSources(project, selectedSkills)
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
		configPath:            cfg.ConfigPath,
		providerName:          resolved.ProviderName,
		modelProfile:          resolved.Profile,
		modelID:               resolved.ModelID,
		parameters:            resolved.Parameters,
		provider:              provider,
		toolExecutor:          runToolExecutor{builtins: toolRegistry, mcpSessions: mcpSessionsByID, subagentManager: subagentManager},
		toolSchemas:           toolSchemas,
		maxTurns:              cfg.Agent.MaxTurns,
		showReasoning:         resolvedShowReasoning,
		enabledTools:          copyStringSlice(enabledToolNames),
		enabledMCP:            mcpServerIDs(selectedMCPServers),
		enabledSkills:         enabledSkillIDs,
		baseMessages:          baseMessages,
		instructionSources:    instructionSources,
		resumed:               resumed,
		resumableSession:      resumedSession,
		resumableSessionStore: sessionStore,
		saveSessions:          saveSessions,
		contextTracker:        contextTracker,
		logger:                logger,
		mcpSessions:           mcpSessions,
		subagentManager:       subagentManager,
		subagentCancel:        subagentCancel,
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

func copyInstructionSources(values []sessions.InstructionSource) []sessions.InstructionSource {
	if values == nil {
		return nil
	}
	return append([]sessions.InstructionSource(nil), values...)
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

func writeVerboseDiagnostics(stderr io.Writer, cfg *config.Config, resolved config.ResolvedModel, enabledTools []string, showReasoning bool, logPath string) error {
	_, err := fmt.Fprintf(stderr, "config_path: %s\nprovider: %s\nmodel_profile: %s\nmodel_id: %s\nlog_path: %s\nmax_turns: %d\nenabled_tools: %s\nshow_reasoning: %t\n",
		cfg.ConfigPath,
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
		if subagents.IsTool(name) {
			return nil, nil, explicitSubagentToolError(name)
		}
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

func explicitSubagentToolError(name string) error {
	return fmt.Errorf("enabled tool %q is a subagent tool; subagent tools are auto-enabled by configuring subagents and must not be listed in tools.enabled or --enable-tools", name)
}

func withoutSubagentToolNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if !subagents.IsTool(name) {
			out = append(out, name)
		}
	}
	return out
}

type runToolExecutor struct {
	builtins        *tools.Registry
	mcpSessions     map[string]*mcp.Session
	subagentManager *subagents.Manager
}

func (e runToolExecutor) Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error) {
	if subagents.IsTool(name) {
		if e.subagentManager == nil {
			return model.ToolResult{}, fmt.Errorf("subagent tool %q is not registered", name)
		}
		return e.subagentManager.Execute(ctx, name, arguments)
	}

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

type cliSubagentRunner struct {
	cwd     string
	program string
}

func (r cliSubagentRunner) Run(ctx context.Context, request subagents.RunRequest, _ <-chan subagents.Message) (result subagents.RunResult, err error) {
	runtime, err := prepareAgentRuntimeWithOptions(ctx, request.ConfigPath, agentCommandFlags{}, io.Discard, func() (string, error) {
		return r.cwd, nil
	}, r.program, runtimePreparationOptions{
		enableSubagents: false,
	})
	if err != nil {
		return subagents.RunResult{}, err
	}
	defer func() {
		err = errors.Join(err, runtime.Close())
	}()

	messages, err := runSilentAgentTurn(ctx, runtime, runtime.initialMessages(), request.Prompt)
	if err != nil {
		return subagents.RunResult{}, err
	}
	for {
		nextMessage := request.NextMessage
		if nextMessage == nil {
			return subagents.RunResult{Output: finalAssistantOutput(messages)}, nil
		}
		message, ok := nextMessage()
		if !ok {
			return subagents.RunResult{Output: finalAssistantOutput(messages)}, nil
		}
		messages, err = runSilentAgentTurn(ctx, runtime, messages, message.Content)
		if err != nil {
			return subagents.RunResult{}, err
		}
	}
}

func runSilentAgentTurn(ctx context.Context, runtime *agentRuntime, messages []model.Message, prompt string) ([]model.Message, error) {
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
		return nil, err
	}
	if err := writeStreamWithOptions(io.Discard, io.Discard, events, runtime.showReasoning, runtime.logger, streamOutputOptions{}); err != nil {
		return nil, err
	}
	result, ok := <-results
	if !ok {
		if err := turnCtx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("child agent did not return updated messages")
	}
	if err := runtime.saveUpdatedMessages(result.Messages); err != nil {
		return nil, err
	}
	return result.Messages, nil
}

func finalAssistantOutput(messages []model.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == model.MessageRoleAssistant && len(messages[i].ToolCalls) == 0 {
			return messages[i].Content
		}
	}
	return ""
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

func enabledSkillsForRun(cfg *config.Config) ([]localskills.Skill, error) {
	discovered, err := localskills.DiscoverDirs(cfg.SkillDirs)
	if err != nil {
		return nil, fmt.Errorf("load skills: %w", err)
	}
	return modelInvokedSkills(discovered), nil
}

func modelInvokedSkills(discovered []localskills.Skill) []localskills.Skill {
	if len(discovered) == 0 {
		return nil
	}
	loaded := make([]localskills.Skill, 0, len(discovered))
	for _, skill := range discovered {
		if !skill.DisableModelInvocation {
			loaded = append(loaded, skill)
		}
	}
	return loaded
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

func loadConfig(configPath string, getwd func() (string, error), program string) (*config.Config, error) {
	resolved, err := resolveConfigPath(configPath, getwd, program)
	if err != nil {
		return nil, err
	}
	return config.Load(resolved)
}

func newProviderForRun(providerName, modelType string, provider config.ProviderConfig) (model.Provider, error) {
	switch modelType {
	case config.ProviderTypeOpenAIChat:
		return openaichat.NewProvider(openAIChatProviderConfig(provider))
	case config.ProviderTypeOpenAIResponses:
		return openairesponses.NewProvider(openAIResponsesProviderConfig(provider))
	case config.ProviderTypeOpenAICodex:
		return openairesponses.NewProvider(openAICodexProviderConfig(provider))
	case config.ProviderTypeAnthropicMessages:
		return anthropicmessages.NewProvider(anthropicMessagesProviderConfig(provider))
	default:
		return nil, fmt.Errorf("unsupported model type %q for provider %q", modelType, providerName)
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

func openAICodexProviderConfig(provider config.ProviderConfig) openairesponses.ProviderConfig {
	return openairesponses.ProviderConfig{
		BaseURL:         provider.BaseURL,
		ForceStoreFalse: true,
		TokenSource: codexResponsesTokenSource{
			source: &codexauth.TokenSource{
				Store: codexauth.Store{Path: provider.AuthFile},
			},
		},
	}
}

func anthropicMessagesProviderConfig(provider config.ProviderConfig) anthropicmessages.ProviderConfig {
	return anthropicmessages.ProviderConfig{
		BaseURL: provider.BaseURL,
		APIKey:  provider.ResolvedAPIKey,
	}
}

type codexResponsesTokenSource struct {
	source *codexauth.TokenSource
}

func (s codexResponsesTokenSource) AccessToken(ctx context.Context) (openairesponses.AccessToken, error) {
	token, err := s.source.AccessToken(ctx)
	if err != nil {
		return openairesponses.AccessToken{}, err
	}
	return openairesponses.AccessToken{Token: token.Token, AccountID: token.AccountID}, nil
}

func promptDeveloperMessagesForRun(cfg *config.Config, cwd string, includeSubagents bool) ([]string, error) {
	messages := []string{}
	if strings.TrimSpace(cfg.Prompt.SystemPrompt) != "" {
		rendered, err := projectcontext.RenderPromptTemplate(cfg.Prompt.SystemPrompt, projectcontext.PromptRenderValues{
			CWD:       cwd,
			ConfigDir: filepath.Dir(cfg.ConfigPath),
			Now:       time.Now().UTC(),
		})
		if err != nil {
			return nil, fmt.Errorf("render prompt.system_prompt: %w", err)
		}
		if strings.TrimSpace(rendered) != "" {
			messages = append(messages, rendered)
		}
	}

	if includeSubagents {
		subagentMessage, err := configuredSubagentsPrompt(cfg)
		if err != nil {
			return nil, err
		}
		if subagentMessage != "" {
			messages = append(messages, subagentMessage)
		}
	}
	return messages, nil
}

func configuredSubagentsPrompt(cfg *config.Config) (string, error) {
	if len(cfg.Subagents) == 0 {
		return "", nil
	}

	ids := make([]string, 0, len(cfg.Subagents))
	for id := range cfg.Subagents {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var out strings.Builder
	out.WriteString("Configured subagents:")
	for _, id := range ids {
		path := cfg.Subagents[id]
		child, err := config.LoadBase(path)
		if err != nil {
			return "", fmt.Errorf("load subagent %q config %q: %w", id, path, err)
		}
		description := strings.TrimSpace(child.Agent.Description)
		if description == "" {
			description = "(no description)"
		}
		fmt.Fprintf(&out, "\n- %s: %s", id, description)
	}
	return out.String(), nil
}

func chatBaseMessages(project projectcontext.Project, enabledSkills []localskills.Skill, developerMessages []string) []model.Message {
	instructions := projectcontext.ComposeInstructions(builtInBaseInstructions, project, "")
	messages := make([]model.Message, 0, len(instructions)+len(developerMessages)+len(enabledSkills))
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
		if instruction.Source == projectcontext.InstructionSourceBuiltIn {
			for _, content := range developerMessages {
				messages = append(messages, model.Message{
					Role:    model.MessageRoleDeveloper,
					Content: content,
				})
			}
		}
	}
	return messages
}

func chatInstructionSources(project projectcontext.Project, enabledSkills []localskills.Skill) []sessions.InstructionSource {
	sources := []sessions.InstructionSource{
		{
			Role:   model.MessageRoleSystem,
			Source: string(projectcontext.InstructionSourceBuiltIn),
		},
	}
	for _, file := range project.InstructionFiles {
		sources = append(sources, sessions.InstructionSource{
			Role:   model.MessageRoleDeveloper,
			Source: string(projectcontext.InstructionSourceProject),
			Path:   file.Path,
		})
	}
	if len(project.InstructionFiles) == 0 && project.HasInstructions {
		sources = append(sources, sessions.InstructionSource{
			Role:   model.MessageRoleDeveloper,
			Source: string(projectcontext.InstructionSourceProject),
			Path:   project.InstructionsPath,
		})
	}
	for _, skill := range enabledSkills {
		sources = append(sources, sessions.InstructionSource{
			Role:   model.MessageRoleDeveloper,
			Source: "skill",
			Path:   skill.Path,
		})
	}
	return sources
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
	detail, ok := toolStatusDetail(toolCall)
	if ok {
		status += " " + detail
	}
	return status
}

func toolStatusDetail(toolCall model.ToolCall) (string, bool) {
	if toolCall.Name == subagents.ToolSubagentStart {
		return subagentStartStatusDetail(toolCall)
	}
	if toolCall.Name == tools.BuiltinGlobFiles {
		return toolStatusStringArgument(toolCall, "pattern")
	}
	return toolStatusPath(toolCall)
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

func toolStatusStringArgument(toolCall model.ToolCall, name string) (string, bool) {
	var arguments map[string]any
	if err := json.Unmarshal([]byte(toolCall.Arguments), &arguments); err != nil {
		return "", false
	}
	if arguments == nil {
		return "", false
	}
	return compactToolStatusString(arguments[name])
}

func subagentStartStatusDetail(toolCall model.ToolCall) (string, bool) {
	var arguments map[string]any
	if err := json.Unmarshal([]byte(toolCall.Arguments), &arguments); err != nil {
		return "", false
	}
	if arguments == nil {
		return "", false
	}
	agentID, ok := compactToolStatusString(arguments["agent_id"])
	if !ok {
		return "", false
	}
	status := agentID
	if displayName, ok := compactToolStatusString(arguments["display_name"]); ok {
		status += " " + displayName
	} else if jobName, ok := compactToolStatusString(arguments["job_name"]); ok {
		status += " " + jobName
	}
	return status, true
}

func compactToolStatusString(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", false
	}
	return strings.Join(fields, " "), true
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
