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
	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/execution"
	eventlog "github.com/rexzhao/simple-agent/internal/logging"
	"github.com/rexzhao/simple-agent/internal/mcp"
	"github.com/rexzhao/simple-agent/internal/model"
	anthropicmessages "github.com/rexzhao/simple-agent/internal/model/anthropic_messages"
	openaichat "github.com/rexzhao/simple-agent/internal/model/openai_chat"
	openairesponses "github.com/rexzhao/simple-agent/internal/model/openai_responses"
	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/sessionprojector"
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
	displayCommand := displayProgramName(program)
	if err := execute(ctx, program, args, stdin, stdout, stderr, getwd, interrupts); err != nil {
		if errors.Is(err, errSilentExit) {
			return 1
		}
		fmt.Fprintf(stderr, "%s: %s\n", displayCommand, renderCLIError(err, displayCommand))
		return 1
	}
	return 0
}

var errSilentExit = errors.New("silent exit")

func displayProgramName(program string) string {
	base := strings.TrimSpace(filepath.Base(program))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "sai"
	}
	return base
}

func resolveStorageRoot(argv0, explicitRoot string) (string, error) {
	if strings.TrimSpace(explicitRoot) != "" {
		return canonicalPath(explicitRoot)
	}
	if envName := storageRootEnvVarName(argv0); envName != "" {
		if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
			return canonicalPath(value)
		}
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config dir: %w", err)
	}
	return filepath.Join(dir, storageRootDirName(argv0)), nil
}

func storageRootEnvVarName(argv0 string) string {
	base := strings.ToUpper(storageRootDirName(argv0))
	var out strings.Builder
	previousUnderscore := false
	for _, r := range base {
		isASCIIAlnum := r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !isASCIIAlnum {
			if !previousUnderscore {
				out.WriteByte('_')
				previousUnderscore = true
			}
			continue
		}
		out.WriteRune(r)
		previousUnderscore = false
	}
	normalized := strings.Trim(out.String(), "_")
	if normalized == "" {
		return ""
	}
	return normalized + "_SERVER_ROOT"
}

func storageRootDirName(argv0 string) string {
	base := filepath.Base(strings.TrimSpace(argv0))
	if ext := filepath.Ext(base); strings.EqualFold(ext, ".exe") {
		base = base[:len(base)-len(ext)]
	}
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "sai"
	}
	return base
}

func canonicalPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	return filepath.Clean(abs), nil
}

func renderCommandText(text, command string) string {
	displayCommand := displayProgramName(command)
	if displayCommand == "sai" {
		return text
	}

	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); {
		if strings.HasPrefix(text[i:], "sai") && isCommandBoundary(text, i-1) && isCommandBoundary(text, i+3) {
			out.WriteString(displayCommand)
			i += 3
			continue
		}
		out.WriteByte(text[i])
		i++
	}
	return out.String()
}

func isCommandBoundary(text string, index int) bool {
	if index < 0 || index >= len(text) {
		return true
	}
	ch := text[index]
	return !('a' <= ch && ch <= 'z') &&
		!('A' <= ch && ch <= 'Z') &&
		!('0' <= ch && ch <= '9') &&
		ch != '_' &&
		ch != '-' &&
		ch != '.'
}

func renderCLIError(err error, command string) string {
	var usageErr cliUsageError
	if errors.As(err, &usageErr) {
		return usageErr.Render(command)
	}
	return err.Error()
}

type commandWarningWriter struct {
	inner   io.Writer
	command string
}

func newCommandWarningWriter(inner io.Writer, command string) io.Writer {
	if inner == nil {
		return nil
	}
	return commandWarningWriter{inner: inner, command: displayProgramName(command)}
}

func (w commandWarningWriter) Write(p []byte) (int, error) {
	text := renderWarningPrefix(string(p), w.command)
	if _, err := io.WriteString(w.inner, text); err != nil {
		return 0, err
	}
	return len(p), nil
}

func renderWarningPrefix(text, command string) string {
	command = displayProgramName(command)
	if command == "sai" {
		return text
	}
	const prefix = "sai: warning:"
	replacement := command + ": warning:"
	parts := strings.SplitAfter(text, "\n")
	for i, part := range parts {
		if strings.HasPrefix(part, prefix) {
			parts[i] = replacement + strings.TrimPrefix(part, prefix)
		}
	}
	return strings.Join(parts, "")
}

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
	displayCommand := displayProgramName(program)
	rootArgs, err := splitRootArgs(args)
	if err != nil {
		return err
	}
	serverRoot, err := resolveStorageRoot(program, rootArgs.serverRoot)
	if err != nil {
		return err
	}
	if rootArgs.command == "" {
		if rootArgs.hasHelp {
			printRootUsage(stdout, displayCommand)
			return nil
		}
		var stop func()
		ctx, stop = contextWithInterruptCancel(ctx, interrupts)
		defer stop()
		return defaultSessionCommand(ctx, rootArgs.commandArgs, rootArgs.configPath, serverRoot, rootArgs.mailbox, rootArgs.tui, stdin, stdout, stderr, getwd, program)
	}
	var stop func()
	ctx, stop = contextWithInterruptCancel(ctx, interrupts)
	defer stop()
	if rootArgs.mailbox.Enabled && rootArgs.command != "help" && rootArgs.command != "session" {
		return usageError("--mailbox can only be used with the default session or session resume", "", "sai help")
	}
	if rootArgs.tui && rootArgs.command != "help" && rootArgs.command != "session" {
		return usageError("--tui can only be used with the default session or session resume", "", "sai help")
	}

	switch rootArgs.command {
	case "help":
		return helpCommand(rootArgs.commandArgs, stdout, displayCommand)
	case "version":
		return versionCommand(rootArgs.commandArgs, stdout, displayCommand)
	case "config":
		subcommand, subArgs, groupHelp, err := splitSubcommandArgs(rootArgs.commandArgs, nil, "sai help config")
		if err != nil {
			return err
		}
		if subcommand == "" && groupHelp {
			printConfigUsage(stdout, displayCommand)
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
			printModelsUsage(stdout, displayCommand)
			return nil
		}
		if subcommand != "list" {
			return usageError("usage: sai models list", "", "sai help models list")
		}
		return modelsListCommand(subArgs, rootArgs.configPath, stdout, getwd, program)
	case "doctor":
		return doctorCommand(rootArgs.commandArgs, rootArgs.configPath, stdout, getwd, program)
	case "project":
		subcommand, subArgs, groupHelp, err := splitSubcommandArgs(rootArgs.commandArgs, map[string]flagKind{"cwd": flagKindValue, "name": flagKindValue, "archived": flagKindBool}, "sai help project")
		if err != nil {
			return err
		}
		if subcommand == "" && groupHelp {
			printProjectUsage(stdout, displayCommand)
			return nil
		}
		switch subcommand {
		case "create":
			return projectCreateCommand(ctx, subArgs, rootArgs.configPath, serverRoot, stdout, getwd, program)
		case "list":
			return projectListCommand(ctx, subArgs, rootArgs.configPath, serverRoot, stdout, getwd, program)
		case "show":
			return projectShowCommand(ctx, subArgs, rootArgs.configPath, serverRoot, stdout, getwd, program)
		case "rename":
			return projectRenameCommand(ctx, subArgs, rootArgs.configPath, serverRoot, stdout, getwd, program)
		case "archive":
			return projectArchiveCommand(ctx, subArgs, rootArgs.configPath, serverRoot, stdout, getwd, program)
		case "remove":
			return projectRemoveCommand(ctx, subArgs, rootArgs.configPath, serverRoot, stdout, getwd, program)
		default:
			return usageError("usage: sai project <create|list|show|rename|archive|remove>", "", "sai help project")
		}
	case "session":
		subcommand, subArgs, groupHelp, err := splitSubcommandArgs(rootArgs.commandArgs, map[string]flagKind{"cwd": flagKindValue, "project": flagKindValue, "all-projects": flagKindBool, "archived": flagKindBool}, "sai help session")
		if err != nil {
			return err
		}
		if subcommand == "" && groupHelp {
			printSessionUsage(stdout, displayCommand)
			return nil
		}
		if rootArgs.mailbox.Enabled && subcommand != "resume" {
			return usageError("--mailbox can only be used with the default session or session resume", "", "sai help session resume")
		}
		if rootArgs.tui && subcommand != "resume" {
			return usageError("--tui can only be used with the default session or session resume", "", "sai help session resume")
		}
		switch subcommand {
		case "create":
			return sessionCreateCommand(ctx, subArgs, rootArgs.configPath, serverRoot, stdout, getwd, program)
		case "resume":
			return sessionResumeCommand(ctx, subArgs, rootArgs.configProvided, serverRoot, rootArgs.mailbox, rootArgs.tui, stdin, stdout, stderr, getwd, program)
		case "list":
			return sessionListCommand(ctx, subArgs, rootArgs.configPath, rootArgs.configProvided, serverRoot, stdout, getwd, program)
		case "show":
			return sessionShowCommand(ctx, subArgs, rootArgs.configPath, rootArgs.configProvided, serverRoot, stdout, getwd, program)
		case "rename":
			return sessionRenameCommand(ctx, subArgs, rootArgs.configPath, rootArgs.configProvided, serverRoot, stdout, getwd, program)
		case "archive":
			return sessionArchiveCommand(ctx, subArgs, rootArgs.configPath, rootArgs.configProvided, serverRoot, stdout, getwd, program)
		case "remove":
			return sessionRemoveCommand(ctx, subArgs, rootArgs.configPath, rootArgs.configProvided, serverRoot, stdout, getwd, program)
		default:
			return usageError("usage: sai session <create|resume|list|show|rename|archive|remove>", "", "sai help session")
		}
	case "auth":
		return authCommand(ctx, rootArgs.commandArgs, rootArgs.configPath, stdout, getwd, program)
	case "tools":
		subcommand, subArgs, groupHelp, err := splitSubcommandArgs(rootArgs.commandArgs, nil, "sai help tools")
		if err != nil {
			return err
		}
		if subcommand == "" && groupHelp {
			printToolsUsage(stdout, displayCommand)
			return nil
		}
		if subcommand != "list" {
			return usageError("usage: sai tools list", "", "sai help tools list")
		}
		return toolsListCommand(subArgs, stdout, displayCommand)
	case "mcp":
		subcommand, subArgs, groupHelp, err := splitSubcommandArgs(rootArgs.commandArgs, map[string]flagKind{"enable-mcp": flagKindValue}, "sai help mcp")
		if err != nil {
			return err
		}
		if subcommand == "" && groupHelp {
			printMCPUsage(stdout, displayCommand)
			return nil
		}
		if subcommand != "list" {
			return usageError("usage: sai mcp list [--enable-mcp ids]", "", "sai help mcp list")
		}
		return mcpListCommand(subArgs, rootArgs.configPath, stdout, getwd, program)
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

const rootUsageText = `usage: sai [--server-root dir] [--config file] [--mailbox [host:port]] [--tui] [command] [args]

Commands:
  project           Manage registered projects
  session           Manage explicit sessions
  config show        Print resolved config with secrets redacted
  models list        List configured provider model profiles
  auth               Manage provider authentication
  doctor            Check local configuration health
  tools list         List built-in tools
  mcp list           List configured MCP servers
  version            Print version
  help [command]     Show usage

With no command, sai auto-creates a project for the current directory when
needed, then starts a pending session.

Use --mailbox [host:port] with the default session or session resume to accept
MCP mailbox tasks while the foreground CLI is idle. Without host:port, sai
listens on 127.0.0.1 with an OS-assigned port.

Use --tui with the default session or session resume to start the explicit
Bubble Tea block renderer. Non-interactive stdin/stdout falls back to plain
output.

Run "sai help <command>" for command usage.
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

const projectUsageText = `usage: sai project <command>

Commands:
  project create    Register a project root
  project list      List registered projects
  project show      Show project metadata
  project rename    Rename project metadata
  project archive   Archive project metadata
  project remove    Remove archived project metadata

Run "sai help project <command>" for command usage.
`

const projectCreateUsageText = `usage: sai project create [--cwd path] [--name name]

Registers the effective current directory as a project root. Use --cwd to
register a specific existing directory. --name sets display-only metadata.
`

const projectListUsageText = `usage: sai project list [--archived]

Lists active registered projects. With --archived, lists archived projects only.
`

const projectShowUsageText = `usage: sai project show [project-id]

Shows project metadata. Without project-id, the current directory is matched to
the nearest registered ancestor project.
`

const projectRenameUsageText = `usage: sai project rename [project-id] <name>

Renames an active project. Without project-id, the current directory is matched
to the nearest registered ancestor project.
`

const projectArchiveUsageText = `usage: sai project archive [project-id]

Archives an active project. Without project-id, the current directory is matched
to the nearest registered ancestor project.
`

const projectRemoveUsageText = `usage: sai project remove [project-id]

Removes an archived project and its sessions. Without project-id, the current
directory is matched to the nearest registered ancestor project.
`

const sessionUsageText = `usage: sai session <command>

Commands:
  session create    Create a session in the nearest registered project
  session resume    Resume a session interactively
  session list      List explicit sessions
  session show      Show session metadata
  session rename    Rename a session
  session archive   Archive a session
  session remove    Remove an archived session

Run "sai help session <command>" for command usage.
`

const sessionCreateUsageText = `usage: sai session create [--cwd path]

Creates a session in the nearest registered project. Use --cwd to
select the creation directory; otherwise the effective current directory is used.
`

const sessionResumeUsageText = `usage: sai session resume <session-id>

Renders the visible session snapshot, then resumes the session interactively.
Existing sessions use their stored cwd and config; --cwd and global --config are
rejected.
`

const sessionListUsageText = `usage: sai session list [--project project-id] [--all-projects] [--archived]

Lists session metadata without printing messages, prompts, assistant output, or
tool result content. Without flags, the current directory is matched to the
nearest registered ancestor project. By default, archived sessions are hidden.
`

const sessionShowUsageText = `usage: sai session show <session-id>

Shows metadata for an explicit global session id. Messages, prompts, assistant
output, and tool result content are not printed.
`

const sessionRenameUsageText = `usage: sai session rename <session-id> <name>

Updates the session display name. The name must be passed as a single shell
argument.
`

const sessionArchiveUsageText = `usage: sai session archive <session-id>

Archives a session so default session lists and automatic selection hide it.
`

const sessionRemoveUsageText = `usage: sai session remove <session-id>

Removes an archived session. Active sessions must be archived first.
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

func helpCommand(args []string, stdout io.Writer, command string) error {
	if len(args) == 0 || len(args) == 1 && isHelpArg(args[0]) {
		printRootUsage(stdout, command)
		return nil
	}

	switch strings.Join(args, " ") {
	case "version":
		printVersionUsage(stdout, command)
	case "config":
		printConfigUsage(stdout, command)
	case "config show":
		printConfigShowUsage(stdout, command)
	case "models":
		printModelsUsage(stdout, command)
	case "models list":
		printModelsListUsage(stdout, command)
	case "doctor":
		printDoctorUsage(stdout, command)
	case "project":
		printProjectUsage(stdout, command)
	case "project create":
		printProjectCreateUsage(stdout, command)
	case "project list":
		printProjectListUsage(stdout, command)
	case "project show":
		printProjectShowUsage(stdout, command)
	case "project rename":
		printProjectRenameUsage(stdout, command)
	case "project archive":
		printProjectArchiveUsage(stdout, command)
	case "project remove":
		printProjectRemoveUsage(stdout, command)
	case "session":
		printSessionUsage(stdout, command)
	case "session create":
		printSessionCreateUsage(stdout, command)
	case "session resume":
		printSessionResumeUsage(stdout, command)
	case "session list":
		printSessionListUsage(stdout, command)
	case "session show":
		printSessionShowUsage(stdout, command)
	case "session rename":
		printSessionRenameUsage(stdout, command)
	case "session archive":
		printSessionArchiveUsage(stdout, command)
	case "session remove":
		printSessionRemoveUsage(stdout, command)
	case "auth":
		printAuthUsage(stdout, command)
	case "auth codex":
		printAuthCodexUsage(stdout, command)
	case "auth codex login":
		printAuthCodexLoginUsage(stdout, command)
	case "tools":
		printToolsUsage(stdout, command)
	case "tools list":
		printToolsListUsage(stdout, command)
	case "mcp":
		printMCPUsage(stdout, command)
	case "mcp list":
		printMCPListUsage(stdout, command)
	default:
		return usageError(fmt.Sprintf("unknown help topic %q", strings.Join(args, " ")), "", "sai help")
	}
	return nil
}

func printRootUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(rootUsageText, command))
}

func printVersionUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(versionUsageText, command))
}

func printConfigUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(configUsageText, command))
}

func printConfigShowUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(configShowUsageText, command))
}

func printModelsUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(modelsUsageText, command))
}

func printModelsListUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(modelsListUsageText, command))
}

func printDoctorUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(doctorUsageText, command))
}

func printProjectUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(projectUsageText, command))
}

func printProjectCreateUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(projectCreateUsageText, command))
}

func printProjectListUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(projectListUsageText, command))
}

func printProjectShowUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(projectShowUsageText, command))
}

func printProjectRenameUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(projectRenameUsageText, command))
}

func printProjectArchiveUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(projectArchiveUsageText, command))
}

func printProjectRemoveUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(projectRemoveUsageText, command))
}

func printSessionUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(sessionUsageText, command))
}

func printSessionCreateUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(sessionCreateUsageText, command))
}

func printSessionResumeUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(sessionResumeUsageText, command))
}

func printSessionListUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(sessionListUsageText, command))
}

func printSessionShowUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(sessionShowUsageText, command))
}

func printSessionRenameUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(sessionRenameUsageText, command))
}

func printSessionArchiveUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(sessionArchiveUsageText, command))
}

func printSessionRemoveUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(sessionRemoveUsageText, command))
}

func printAuthUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(authUsageText, command))
}

func printAuthCodexUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(authCodexUsageText, command))
}

func printAuthCodexLoginUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(authCodexLoginUsageText, command))
}

func printToolsUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(toolsUsageText, command))
}

func printToolsListUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(toolsListUsageText, command))
}

func printMCPUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(mcpUsageText, command))
}

func printMCPListUsage(stdout io.Writer, command string) {
	fmt.Fprint(stdout, renderCommandText(mcpListUsageText, command))
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

type cliUsageError struct {
	message     string
	usage       string
	helpCommand string
}

func (err cliUsageError) Error() string {
	return err.Render("sai")
}

func (err cliUsageError) Render(command string) string {
	var out strings.Builder
	message := err.message
	if strings.HasPrefix(message, "usage: sai ") {
		message = renderCommandText(message, command)
	}
	out.WriteString(message)
	if err.usage != "" {
		out.WriteString("\n\n")
		out.WriteString(strings.TrimRight(renderCommandText(err.usage, command), "\n"))
	}
	if err.helpCommand != "" {
		out.WriteString("\nRun \"")
		out.WriteString(renderCommandText(err.helpCommand, command))
		out.WriteString("\" for usage.")
	}
	return out.String()
}

func usageError(message, usage, helpCommand string) error {
	return cliUsageError{message: message, usage: usage, helpCommand: helpCommand}
}

type flagKind int

const (
	flagKindBool flagKind = iota
	flagKindValue
)

type rootArgs struct {
	configPath     string
	configProvided bool
	serverRoot     string
	mailbox        mailboxRootFlag
	tui            bool
	command        string
	commandArgs    []string
	hasHelp        bool
}

type mailboxRootFlag struct {
	Enabled bool
	Addr    string
}

func splitRootArgs(args []string) (rootArgs, error) {
	known := map[string]flagKind{
		"config":         flagKindValue,
		"mailbox":        flagKindBool,
		"tui":            flagKindBool,
		"all-projects":   flagKindBool,
		"archived":       flagKindBool,
		"server-root":    flagKindValue,
		"name":           flagKindValue,
		"project":        flagKindValue,
		"provider":       flagKindValue,
		"model":          flagKindValue,
		"prompt":         flagKindValue,
		"file":           flagKindValue,
		"enable-tools":   flagKindValue,
		"enable-mcp":     flagKindValue,
		"resume":         flagKindValue,
		"keep":           flagKindValue,
		"cwd":            flagKindValue,
		"port":           flagKindValue,
		"listen":         flagKindValue,
		"timeout-ms":     flagKindValue,
		"new":            flagKindBool,
		"background":     flagKindBool,
		"wait":           flagKindBool,
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
		if name == "mailbox" {
			mailbox, next, err := mailboxFlagValue(args, i, hasInlineValue)
			if err != nil {
				return rootArgs{}, usageError(err.Error(), "", "sai help")
			}
			out.mailbox = mailbox
			i = next
			continue
		}
		if name == "tui" {
			out.tui = true
			continue
		}
		if name == "config" || name == "server-root" {
			value, next, err := flagValue(args, i, name, hasInlineValue)
			if err != nil {
				return rootArgs{}, usageError(err.Error(), "", "sai help")
			}
			if name == "config" {
				out.configPath = value
				out.configProvided = true
			} else {
				out.serverRoot = value
			}
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
		if isFlagArg(arg) && name == "mailbox" {
			mailbox, next, err := mailboxFlagValue(args.commandArgs, i, hasInlineValue)
			if err != nil {
				return rootArgs{}, usageError(err.Error(), "", "sai help")
			}
			args.mailbox = mailbox
			i = next
			continue
		}
		if isFlagArg(arg) && name == "tui" {
			args.tui = true
			continue
		}
		if isFlagArg(arg) && (name == "config" || name == "server-root") {
			value, next, err := flagValue(args.commandArgs, i, name, hasInlineValue)
			if err != nil {
				return rootArgs{}, usageError(err.Error(), "", "sai help")
			}
			if name == "config" {
				args.configPath = value
				args.configProvided = true
			} else {
				args.serverRoot = value
			}
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

func mailboxFlagValue(args []string, index int, inline bool) (mailboxRootFlag, int, error) {
	if inline {
		_, value, _ := strings.Cut(args[index], "=")
		value = strings.TrimSpace(value)
		if value == "" {
			return mailboxRootFlag{Enabled: true}, index, nil
		}
		addr, err := normalizeMailboxListenAddr(value)
		if err != nil {
			return mailboxRootFlag{}, index, err
		}
		return mailboxRootFlag{Enabled: true, Addr: addr}, index, nil
	}

	if index+1 >= len(args) {
		return mailboxRootFlag{Enabled: true}, index, nil
	}
	next := strings.TrimSpace(args[index+1])
	if next == "" || isFlagArg(next) || !strings.Contains(next, ":") {
		return mailboxRootFlag{Enabled: true}, index, nil
	}
	addr, err := normalizeMailboxListenAddr(next)
	if err != nil {
		return mailboxRootFlag{}, index, err
	}
	return mailboxRootFlag{Enabled: true, Addr: addr}, index + 1, nil
}

func versionCommand(args []string, stdout io.Writer, command string) error {
	flags := flag.NewFlagSet("sai version", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printVersionUsage, command, "sai help")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai version", "", "sai help")
	}
	fmt.Fprintf(stdout, "%s %s\n", command, Version)
	return nil
}

func configShowCommand(args []string, configPath string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai config show", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printConfigShowUsage, displayCommand, "sai help config show")
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
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai models list", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printModelsListUsage, displayCommand, "sai help models list")
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
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai doctor", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printDoctorUsage, displayCommand, "sai help doctor")
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

func resolveServerCWD(cwdFlag string, getwd func() (string, error)) (string, error) {
	cwdFlag = strings.TrimSpace(cwdFlag)
	if cwdFlag == "" {
		cwd, err := getwd()
		if err != nil {
			return "", fmt.Errorf("get current directory: %w", err)
		}
		cwd = filepath.Clean(cwd)
		if err := checkExistingDirectory(cwd); err != nil {
			return "", fmt.Errorf("cwd %q: %w", cwd, err)
		}
		return cwd, nil
	}

	cwd := cwdFlag
	if !filepath.IsAbs(cwd) {
		base, err := getwd()
		if err != nil {
			return "", fmt.Errorf("get current directory: %w", err)
		}
		cwd = filepath.Join(base, cwd)
	}
	cwd = filepath.Clean(cwd)
	if err := checkExistingDirectory(cwd); err != nil {
		return "", fmt.Errorf("cwd %q: %w", cwd, err)
	}
	return cwd, nil
}

func serverConfigPath(configPath, cwd string) string {
	if strings.TrimSpace(configPath) == "" || filepath.IsAbs(configPath) {
		return configPath
	}
	return filepath.Join(cwd, configPath)
}

func projectCreateCommand(ctx context.Context, args []string, configPath, serverRoot string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai project create", flag.ContinueOnError)
	cwdFlag := flags.String("cwd", "", "project root")
	nameFlag := flags.String("name", "", "project display name")
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printProjectCreateUsage, displayCommand, "sai help project create")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai project create [--cwd path] [--name name]", "", "sai help project create")
	}

	_, root, err := resolveProjectCreatePaths(*cwdFlag, getwd)
	if err != nil {
		return err
	}
	service, err := execution.NewService(serverRoot)
	if err != nil {
		return err
	}
	result, err := service.CreateProject(root, *nameFlag)
	if err != nil {
		return err
	}
	status := "created"
	if !result.Created {
		status = "existing"
	}
	return printProjectCommandStatus(stdout, status, result.Project)
}

func projectListCommand(ctx context.Context, args []string, configPath, serverRoot string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai project list", flag.ContinueOnError)
	archived := flags.Bool("archived", false, "list archived projects")
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printProjectListUsage, displayCommand, "sai help project list")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai project list [--archived]", "", "sai help project list")
	}

	service, err := execution.NewService(serverRoot)
	if err != nil {
		return err
	}
	projects, err := service.ListProjects(execution.ProjectListOptions{Archived: *archived})
	if err != nil {
		return err
	}
	return printProjectList(stdout, projects)
}

func projectShowCommand(ctx context.Context, args []string, configPath, serverRoot string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai project show", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printProjectShowUsage, displayCommand, "sai help project show")
	if done || err != nil {
		return err
	}
	if len(positionals) > 1 {
		return usageError("usage: sai project show [project-id]", "", "sai help project show")
	}

	if len(positionals) == 1 {
		service, err := execution.NewService(serverRoot)
		if err != nil {
			return err
		}
		project, err := service.GetProject(positionals[0])
		if err != nil {
			return err
		}
		return printProjectInfo(stdout, project)
	}

	effectiveCWD, err := resolveClientCWD("", getwd)
	if err != nil {
		return err
	}
	service, err := execution.NewService(serverRoot)
	if err != nil {
		return err
	}
	project, ok, err := service.NearestProject(effectiveCWD, execution.NearestProjectOptions{})
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no registered project found from %s; run %q", effectiveCWD, displayCommand+" project create")
	}
	return printProjectInfo(stdout, project)
}

func projectRenameCommand(ctx context.Context, args []string, configPath, serverRoot string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai project rename", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printProjectRenameUsage, displayCommand, "sai help project rename")
	if done || err != nil {
		return err
	}
	if len(positionals) != 1 && len(positionals) != 2 {
		return usageError("usage: sai project rename [project-id] <name>", "", "sai help project rename")
	}
	name := strings.TrimSpace(positionals[len(positionals)-1])
	if name == "" {
		return usageError("project display name must be a non-empty string", "", "sai help project rename")
	}

	id := ""
	if len(positionals) == 2 {
		id = strings.TrimSpace(positionals[0])
	}
	service, err := execution.NewService(serverRoot)
	if err != nil {
		return err
	}
	if id == "" {
		effectiveCWD, err := resolveClientCWD("", getwd)
		if err != nil {
			return err
		}
		id, err = selectExecutionProjectID(service, id, effectiveCWD, displayCommand, false)
		if err != nil {
			return err
		}
	}
	project, err := service.RenameProject(id, name)
	if err != nil {
		return err
	}
	return printProjectCommandStatus(stdout, "renamed", project)
}

func projectArchiveCommand(ctx context.Context, args []string, configPath, serverRoot string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai project archive", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printProjectArchiveUsage, displayCommand, "sai help project archive")
	if done || err != nil {
		return err
	}
	if len(positionals) > 1 {
		return usageError("usage: sai project archive [project-id]", "", "sai help project archive")
	}
	id := ""
	if len(positionals) == 1 {
		id = strings.TrimSpace(positionals[0])
	}
	service, err := execution.NewService(serverRoot)
	if err != nil {
		return err
	}
	if id == "" {
		effectiveCWD, err := resolveClientCWD("", getwd)
		if err != nil {
			return err
		}
		id, err = selectExecutionProjectID(service, id, effectiveCWD, displayCommand, false)
		if err != nil {
			return err
		}
	}
	project, err := service.ArchiveProject(id)
	if err != nil {
		return err
	}
	return printProjectCommandStatus(stdout, "archived", project)
}

func projectRemoveCommand(ctx context.Context, args []string, configPath, serverRoot string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai project remove", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printProjectRemoveUsage, displayCommand, "sai help project remove")
	if done || err != nil {
		return err
	}
	if len(positionals) > 1 {
		return usageError("usage: sai project remove [project-id]", "", "sai help project remove")
	}

	id := ""
	if len(positionals) == 1 {
		id = strings.TrimSpace(positionals[0])
	}
	service, err := execution.NewService(serverRoot)
	if err != nil {
		return err
	}
	if id == "" {
		effectiveCWD, err := resolveClientCWD("", getwd)
		if err != nil {
			return err
		}
		id, err = selectExecutionProjectID(service, id, effectiveCWD, displayCommand, true)
		if err != nil {
			return err
		}
	}

	result, err := service.RemoveProject(id)
	if err != nil {
		return err
	}
	return printProjectRemoveStatus(stdout, result)
}

func resolveProjectCreatePaths(cwdFlag string, getwd func() (string, error)) (string, string, error) {
	effectiveCWD, err := resolveClientCWD("", getwd)
	if err != nil {
		return "", "", err
	}
	root := effectiveCWD
	if strings.TrimSpace(cwdFlag) != "" {
		root = cwdFlag
		if !filepath.IsAbs(root) {
			root = filepath.Join(effectiveCWD, root)
		}
	}
	canonicalRoot, err := projectstore.CanonicalRoot(root)
	if err != nil {
		return "", "", err
	}
	return effectiveCWD, canonicalRoot, nil
}

func printProjectList(stdout io.Writer, projects []execution.Project) error {
	if _, err := fmt.Fprintln(stdout, "ID\tNAME\tROOT\tARCHIVED\tCREATED_AT\tUPDATED_AT"); err != nil {
		return err
	}
	for _, project := range projects {
		if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\t%t\t%s\t%s\n", project.ID, project.DisplayName, project.Root, project.Archived, formatSessionTimestamp(project.CreatedAt), formatSessionTimestamp(project.UpdatedAt)); err != nil {
			return err
		}
	}
	return nil
}

func printProjectCommandStatus(stdout io.Writer, status string, project execution.Project) error {
	if _, err := fmt.Fprintf(stdout, "STATUS\t%s\n", status); err != nil {
		return err
	}
	return printProjectInfo(stdout, project)
}

func printProjectInfo(stdout io.Writer, project execution.Project) error {
	if _, err := fmt.Fprintf(stdout, "ID\t%s\n", project.ID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "NAME\t%s\n", project.DisplayName); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "ROOT\t%s\n", project.Root); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "ARCHIVED\t%t\n", project.Archived); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "CREATED_AT\t%s\n", formatSessionTimestamp(project.CreatedAt)); err != nil {
		return err
	}
	_, err := fmt.Fprintf(stdout, "UPDATED_AT\t%s\n", formatSessionTimestamp(project.UpdatedAt))
	return err
}

func printProjectRemoveStatus(stdout io.Writer, result execution.ProjectRemoveResult) error {
	return printRemoveStatus(stdout, result.Status, result.ID, result.RemovedSessions, true)
}

func printRemoveStatus(stdout io.Writer, status, id string, removedSessions int, includeRemovedSessions bool) error {
	status = strings.TrimSpace(status)
	if status == "" {
		status = "removed"
	}
	if _, err := fmt.Fprintf(stdout, "STATUS\t%s\n", status); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "ID\t%s\n", id); err != nil {
		return err
	}
	if includeRemovedSessions {
		_, err := fmt.Fprintf(stdout, "REMOVED_SESSIONS\t%d\n", removedSessions)
		return err
	}
	return nil
}

func executionSessionDefaultsFromConfig(cfg *config.Config, cwd string) (sessions.SessionV2, error) {
	providerName := strings.TrimSpace(cfg.DefaultProvider)
	modelProfile := strings.TrimSpace(cfg.DefaultModel)
	if providerName == "" || modelProfile == "" {
		return sessions.SessionV2{}, fmt.Errorf("execution session defaults require default_provider and default_model")
	}
	provider, ok := cfg.Providers[providerName]
	if !ok {
		return sessions.SessionV2{}, fmt.Errorf("execution session defaults reference unknown provider %q", providerName)
	}
	profile, ok := provider.Models[modelProfile]
	if !ok {
		return sessions.SessionV2{}, fmt.Errorf("execution session defaults reference unknown model %q for provider %q", modelProfile, providerName)
	}
	window := contextwindow.ResolveWindow(profile.ContextWindow)
	selectedMCPServers, err := cfg.SelectedMCPServers(nil, false)
	if err != nil {
		return sessions.SessionV2{}, err
	}
	selectedSkills, err := enabledSkillsForRun(cfg)
	if err != nil {
		return sessions.SessionV2{}, err
	}
	return sessions.SessionV2{
		Version:         sessions.VersionV2,
		Provider:        providerName,
		ModelProfile:    modelProfile,
		ModelID:         profile.ID,
		ModelParameters: copyParameterMap(profile.Parameters),
		CWD:             cwd,
		ConfigPath:      cfg.ConfigPath,
		EnabledTools:    copyStringSlice(cfg.Tools.Enabled),
		EnabledMCP:      mcpServerIDs(selectedMCPServers),
		EnabledSkills:   skillIDs(selectedSkills),
		ShowReasoning:   cfg.Agent.ShowReasoning,
		Context: contextwindow.Metadata{
			ContextWindow:           window.Tokens,
			ContextWindowSource:     string(window.Source),
			WarningThresholdPercent: contextwindow.WarningThresholdPercent,
		},
		SaveToolResults: true,
	}, nil
}

func sessionCreateCommand(ctx context.Context, args []string, configPath, homePath string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai session create", flag.ContinueOnError)
	cwdFlag := flags.String("cwd", "", "session creation working directory")
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printSessionCreateUsage, displayCommand, "sai help session create")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai session create [--cwd path]", "", "sai help session create")
	}

	creationCWD, err := resolveClientCWD(*cwdFlag, getwd)
	if err != nil {
		return err
	}
	session, err := createExecutionSessionForCWD(configPath, homePath, creationCWD, program)
	if err != nil {
		return err
	}
	return printSessionCommandStatus(stdout, "created", session)
}

func sessionListCommand(ctx context.Context, args []string, configPath string, configProvided bool, homePath string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai session list", flag.ContinueOnError)
	projectID := flags.String("project", "", "project id")
	allProjects := flags.Bool("all-projects", false, "list sessions across all projects")
	archived := flags.Bool("archived", false, "list archived sessions")
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printSessionListUsage, displayCommand, "sai help session list")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai session list [--project project-id] [--all-projects] [--archived]", "", "sai help session list")
	}
	if err := rejectSessionConfigForExistingCommand(configProvided, "sai help session list"); err != nil {
		return err
	}
	if *allProjects && strings.TrimSpace(*projectID) != "" {
		return usageError("--project cannot be combined with --all-projects", "", "sai help session list")
	}

	service, err := execution.NewService(homePath)
	if err != nil {
		return err
	}
	listOptions := execution.SessionListOptions{Archived: *archived}
	if *allProjects {
		listOptions.AllProjects = true
		infos, err := service.ListSessions(listOptions)
		if err != nil {
			return err
		}
		return printSessionList(stdout, infos)
	}

	project := strings.TrimSpace(*projectID)
	if project == "" {
		effectiveCWD, err := resolveClientCWD("", getwd)
		if err != nil {
			return err
		}
		nearest, ok, err := service.NearestProject(effectiveCWD, execution.NearestProjectOptions{})
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no registered project found from %s; run %q", effectiveCWD, displayCommand+" project create")
		}
		project = nearest.ID
	}

	listOptions.ProjectID = project
	infos, err := service.ListSessions(listOptions)
	if err != nil {
		return err
	}
	return printSessionList(stdout, infos)
}

func sessionShowCommand(ctx context.Context, args []string, configPath string, configProvided bool, homePath string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai session show", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printSessionShowUsage, displayCommand, "sai help session show")
	if done || err != nil {
		return err
	}
	if err := rejectSessionConfigForExistingCommand(configProvided, "sai help session show"); err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usageError("usage: sai session show <session-id>", "", "sai help session show")
	}

	service, err := execution.NewService(homePath)
	if err != nil {
		return err
	}
	session, err := service.GetSession(positionals[0])
	if err != nil {
		return err
	}
	return printSessionDetailWithProject(stdout, session)
}

func sessionRenameCommand(ctx context.Context, args []string, configPath string, configProvided bool, homePath string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai session rename", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printSessionRenameUsage, displayCommand, "sai help session rename")
	if done || err != nil {
		return err
	}
	if err := rejectSessionConfigForExistingCommand(configProvided, "sai help session rename"); err != nil {
		return err
	}
	if len(positionals) != 2 {
		return usageError("usage: sai session rename <session-id> <name>", "", "sai help session rename")
	}
	sessionID := strings.TrimSpace(positionals[0])
	displayName := strings.TrimSpace(positionals[1])
	if sessionID == "" {
		return usageError("session id must be a non-empty string", "", "sai help session rename")
	}
	if displayName == "" {
		return usageError("session display name must be a non-empty string", "", "sai help session rename")
	}

	service, err := execution.NewService(homePath)
	if err != nil {
		return err
	}
	session, err := service.RenameSession(sessionID, displayName)
	if err != nil {
		return err
	}
	return printSessionCommandStatus(stdout, "renamed", session)
}

func sessionArchiveCommand(ctx context.Context, args []string, configPath string, configProvided bool, homePath string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai session archive", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printSessionArchiveUsage, displayCommand, "sai help session archive")
	if done || err != nil {
		return err
	}
	if err := rejectSessionConfigForExistingCommand(configProvided, "sai help session archive"); err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usageError("usage: sai session archive <session-id>", "", "sai help session archive")
	}
	sessionID := strings.TrimSpace(positionals[0])
	if sessionID == "" {
		return usageError("session id must be a non-empty string", "", "sai help session archive")
	}

	service, err := execution.NewService(homePath)
	if err != nil {
		return err
	}
	session, err := service.ArchiveSession(sessionID)
	if err != nil {
		return err
	}
	return printSessionCommandStatus(stdout, "archived", session)
}

func sessionRemoveCommand(ctx context.Context, args []string, configPath string, configProvided bool, homePath string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai session remove", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printSessionRemoveUsage, displayCommand, "sai help session remove")
	if done || err != nil {
		return err
	}
	if err := rejectSessionConfigForExistingCommand(configProvided, "sai help session remove"); err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usageError("usage: sai session remove <session-id>", "", "sai help session remove")
	}
	sessionID := strings.TrimSpace(positionals[0])
	if sessionID == "" {
		return usageError("session id must be a non-empty string", "", "sai help session remove")
	}

	service, err := execution.NewService(homePath)
	if err != nil {
		return err
	}
	result, err := service.RemoveSession(sessionID)
	if err != nil {
		return err
	}
	return printRemoveStatus(stdout, result.Status, result.ID, 0, false)
}

func selectExecutionProjectID(service *execution.Service, id, effectiveCWD, displayCommand string, includeArchived bool) (string, error) {
	id = strings.TrimSpace(id)
	if id != "" {
		return id, nil
	}
	project, ok, err := service.NearestProject(effectiveCWD, execution.NearestProjectOptions{IncludeArchived: includeArchived})
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no registered project found from %s; run %q", effectiveCWD, displayCommand+" project create")
	}
	return project.ID, nil
}

func createExecutionSessionForCWD(configPath, homePath, creationCWD, program string) (execution.SessionDetail, error) {
	displayCommand := displayProgramName(program)
	service, err := execution.NewService(homePath)
	if err != nil {
		return execution.SessionDetail{}, err
	}
	project, ok, err := service.NearestProject(creationCWD, execution.NearestProjectOptions{})
	if err != nil {
		return execution.SessionDetail{}, err
	}
	if !ok {
		return execution.SessionDetail{}, fmt.Errorf("no registered project found from %s; run %q", creationCWD, displayCommand+" project create")
	}

	cfg, err := loadConfig(serverConfigPath(configPath, creationCWD), func() (string, error) {
		return creationCWD, nil
	}, program)
	if err != nil {
		return execution.SessionDetail{}, err
	}
	defaults, err := executionSessionDefaultsFromConfig(cfg, creationCWD)
	if err != nil {
		return execution.SessionDetail{}, err
	}
	defaults.CreatedCWD = creationCWD
	metadata := executionSessionCreateMetadataFromDefaults(defaults)
	return service.CreateSession(project.ID, metadata)
}

func executionSessionCreateMetadataFromDefaults(session sessions.SessionV2) execution.SessionCreateMetadata {
	showReasoning := session.ShowReasoning
	saveToolResults := session.SaveToolResults
	context := session.Context
	createdCWD := session.CreatedCWD
	if strings.TrimSpace(createdCWD) == "" {
		createdCWD = session.CWD
	}
	return execution.SessionCreateMetadata{
		CreatedCWD:      createdCWD,
		ConfigPath:      session.ConfigPath,
		Provider:        session.Provider,
		ModelProfile:    session.ModelProfile,
		ModelID:         session.ModelID,
		ModelParameters: copyParameterMap(session.ModelParameters),
		EnabledTools:    copyStringSlice(session.EnabledTools),
		EnabledMCP:      copyStringSlice(session.EnabledMCP),
		EnabledSkills:   copyStringSlice(session.EnabledSkills),
		ShowReasoning:   &showReasoning,
		Context:         &context,
		SaveToolResults: &saveToolResults,
	}
}

func sessionResumeCommand(ctx context.Context, args []string, configProvided bool, homePath string, mailbox mailboxRootFlag, tui bool, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai session resume", flag.ContinueOnError)
	flags.String("cwd", "", "discovery working directory")
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printSessionResumeUsage, displayCommand, "sai help session resume")
	if done || err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usageError("usage: sai session resume <session-id>", "", "sai help session resume")
	}
	if err := rejectSessionResumeOverrides(flagWasSet(flags, "cwd"), configProvided, "sai help session resume"); err != nil {
		return err
	}
	service, err := newCLIAttachExecutionService(homePath, program)
	if err != nil {
		return err
	}
	startupCWD := ""
	if mailbox.Enabled {
		startupCWD, err = resolveClientCWD("", getwd)
		if err != nil {
			return err
		}
	}
	replCtx := ctx
	var cancelMailbox context.CancelFunc
	if mailbox.Enabled {
		replCtx, cancelMailbox = context.WithCancel(ctx)
	}
	mailboxQueue, stopMailbox, err := startMailboxForREPL(replCtx, mailbox, startupCWD, stderr, displayCommand, program)
	if err != nil {
		if cancelMailbox != nil {
			cancelMailbox()
		}
		return err
	}
	defer func() {
		if cancelMailbox != nil {
			cancelMailbox()
		}
		stopMailbox()
	}()
	return resumeExecutionSessionREPL(replCtx, service, positionals[0], stdin, stdout, stderr, displayCommand, "sai help session resume", mailboxQueue, tui)
}

func defaultSessionCommand(ctx context.Context, args []string, configPath string, homePath string, mailbox mailboxRootFlag, tui bool, stdin io.Reader, stdout, stderr io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai", flag.ContinueOnError)
	cwdFlag := flags.String("cwd", "", "discovery working directory")
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printRootUsage, displayCommand, "sai help")
	if done || err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usageError("usage: sai [--cwd path]", "", "sai help")
	}

	service, err := newCLIAttachExecutionService(homePath, program)
	if err != nil {
		return err
	}
	startupCWD := ""
	creationGetwd := getwd
	if mailbox.Enabled {
		startupCWD, err = resolveClientCWD("", getwd)
		if err != nil {
			return err
		}
		creationGetwd = func() (string, error) {
			return startupCWD, nil
		}
	}
	creationCWD, err := resolveClientCWD(*cwdFlag, creationGetwd)
	if err != nil {
		return err
	}
	replCtx := ctx
	var cancelMailbox context.CancelFunc
	if mailbox.Enabled {
		replCtx, cancelMailbox = context.WithCancel(ctx)
	}
	mailboxQueue, stopMailbox, err := startMailboxForREPL(replCtx, mailbox, startupCWD, stderr, displayCommand, program)
	if err != nil {
		if cancelMailbox != nil {
			cancelMailbox()
		}
		return err
	}
	defer func() {
		if cancelMailbox != nil {
			cancelMailbox()
		}
		stopMailbox()
	}()
	if tui {
		return runPendingAttachTUI(replCtx, service, configPath, homePath, creationCWD, stdin, stdout, stderr, program, mailboxQueue)
	}
	return runPendingAttachREPL(replCtx, service, configPath, homePath, creationCWD, stdin, stdout, stderr, program, mailboxQueue)
}

func resumeExecutionSessionREPL(ctx context.Context, service *execution.Service, sessionID string, stdin io.Reader, stdout, stderr io.Writer, displayCommand, helpCommand string, mailbox *mailboxQueue, tui bool) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return usageError("session id must be a non-empty string", "", helpCommand)
	}
	detail, err := service.GetSession(sessionID)
	if err != nil {
		return err
	}
	snapshot, err := service.GetSessionChatItems(sessionID)
	if err != nil {
		return err
	}
	if tui {
		return runResumeAttachTUI(ctx, service, detail, snapshot, stdin, stdout, stderr, displayCommand, mailbox)
	}
	if err := writeAttachSnapshot(stdout, snapshot); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stderr, "%s: attached to session %s\n", displayCommand, sessionID); err != nil {
		return err
	}
	return runAttachREPL(ctx, service, sessionID, stdin, stdout, stderr, displayCommand, mailbox)
}

func runPendingAttachREPL(ctx context.Context, service *execution.Service, configPath, serverRoot, creationCWD string, stdin io.Reader, stdout, stderr io.Writer, program string, mailbox *mailboxQueue) error {
	displayCommand := displayProgramName(program)
	if err := ensurePendingAttachProject(service, creationCWD, program); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdin)
	var inputCh <-chan chatInputEvent
	var mailboxCh <-chan mailboxTaskRead
	for {
		if scanner != nil && inputCh == nil {
			inputCh = startChatInputRead(ctx, scanner, stderr)
		}
		if mailbox != nil && mailboxCh == nil {
			mailboxCh = startMailboxTaskRead(ctx, mailbox)
		}
		if inputCh == nil && mailboxCh == nil {
			return nil
		}

		var input chatInputEvent
		select {
		case input = <-inputCh:
			inputCh = nil
			if input.err != nil {
				return input.err
			}
			if !input.ok {
				scanner = nil
				if mailbox == nil {
					return nil
				}
				continue
			}
		case mailboxRead := <-mailboxCh:
			mailboxCh = nil
			if mailboxRead.err != nil {
				return mailboxRead.err
			}
			if mailboxRead.task == nil {
				continue
			}
			input = chatInputEvent{line: mailboxRead.task.Prompt, ok: true, mailboxTask: mailboxRead.task}
		case <-ctx.Done():
			return ctx.Err()
		}

		command := strings.TrimSpace(input.line)
		if command == "" {
			continue
		}
		if input.mailboxTask == nil && !input.multiline && (command == "/exit" || command == "/quit") {
			return nil
		}
		if input.mailboxTask == nil && !input.multiline && command == "/compact" {
			if _, err := fmt.Fprintf(stderr, "%s: compact requires a session; send a message first to create one\n", displayCommand); err != nil {
				return err
			}
			continue
		}

		detail, err := createExecutionSessionForCWD(configPath, serverRoot, creationCWD, program)
		if err != nil {
			return err
		}
		sessionID := strings.TrimSpace(detail.ID)
		if sessionID == "" {
			return fmt.Errorf("create session: response missing session id")
		}
		if _, err := fmt.Fprintf(stderr, "%s: attached to session %s\n", displayCommand, sessionID); err != nil {
			return err
		}
		initialInput := input
		return runAttachREPLWithScanner(ctx, service, sessionID, scanner, stdout, stderr, displayCommand, attachREPLSources{
			initialInput: &initialInput,
			inputCh:      inputCh,
			mailbox:      mailbox,
			mailboxCh:    mailboxCh,
		})
	}
}

func ensurePendingAttachProject(service *execution.Service, creationCWD, program string) error {
	project, ok, err := service.NearestProject(creationCWD, execution.NearestProjectOptions{})
	if err != nil {
		return err
	}
	if ok && strings.TrimSpace(project.ID) != "" {
		return nil
	}
	result, err := service.CreateProject(creationCWD, "")
	if err != nil {
		return err
	}
	if strings.TrimSpace(result.Project.ID) == "" {
		return fmt.Errorf("create project: response missing project id")
	}
	return nil
}

func writeAttachSnapshot(stdout io.Writer, snapshot execution.SessionItemsPage) error {
	for _, item := range snapshot.Items {
		if item.Kind != sessions.ItemKindMessage || item.Visibility != sessions.ItemVisibilityVisible || item.Message == nil || item.Message.Content == nil {
			continue
		}
		role := strings.TrimSpace(item.Message.Role)
		switch role {
		case string(model.MessageRoleUser):
			if item.Audience != sessions.ItemAudienceUser {
				continue
			}
		case string(model.MessageRoleAssistant):
			if item.Audience != sessions.ItemAudienceModel {
				continue
			}
		default:
			continue
		}
		text := item.Message.Content.Inline
		if text == "" {
			text = item.Message.Content.Preview
		}
		if text == "" {
			continue
		}
		if _, err := fmt.Fprintf(stdout, "%s: %s\n", role, text); err != nil {
			return err
		}
	}
	return nil
}

type attachOutputState struct {
	stdoutAtLineStart         bool
	wroteText                 bool
	inReasoningBlock          bool
	needsReasoningBreak       bool
	reasoningEndedWithNewline bool
}

type attachSendResult struct {
	result      execution.SessionMessageResult
	err         error
	mailboxTask *mailboxTask
}

type attachREPLSources struct {
	initialInput *chatInputEvent
	inputCh      <-chan chatInputEvent
	mailbox      *mailboxQueue
	mailboxCh    <-chan mailboxTaskRead
}

func runAttachREPL(ctx context.Context, service *execution.Service, sessionID string, stdin io.Reader, stdout, stderr io.Writer, displayCommand string, mailbox *mailboxQueue) error {
	scanner := bufio.NewScanner(stdin)
	return runAttachREPLWithScanner(ctx, service, sessionID, scanner, stdout, stderr, displayCommand, attachREPLSources{mailbox: mailbox})
}

func runAttachREPLWithScanner(ctx context.Context, service *execution.Service, sessionID string, scanner *bufio.Scanner, stdout, stderr io.Writer, displayCommand string, sources attachREPLSources) error {
	inheritedInputCh := sources.inputCh
	inputCh := inheritedInputCh
	mailbox := sources.mailbox
	mailboxCh := sources.mailboxCh
	var sendDone <-chan attachSendResult
	var events <-chan execution.SessionStreamEvent
	output := attachOutputState{stdoutAtLineStart: true}
	turnInFlight := false
	turnStarted := false
	expectedTurnID := ""
	terminalSeen := false
	var pendingSendErr error
	var terminalTurnIDs map[string]bool
	var pendingMailboxEndTask *mailboxTask
	pendingMailboxEndStatus := ""
	setExpectedTurnID := func(turnID string) {
		turnID = strings.TrimSpace(turnID)
		if turnID == "" || expectedTurnID != "" {
			return
		}
		expectedTurnID = turnID
		if terminalTurnIDs[turnID] {
			terminalSeen = true
		}
	}
	mailboxTerminalStatus := func(task *mailboxTask, fallback string) string {
		status := strings.TrimSpace(fallback)
		if task != nil && mailbox != nil {
			if snapshot, ok := mailbox.get(task.ID); ok && strings.TrimSpace(snapshot.Status) != "" {
				status = snapshot.Status
			}
		}
		if status == "" {
			status = "finished"
		}
		return status
	}
	writePendingMailboxEnd := func() error {
		if pendingMailboxEndTask == nil {
			return nil
		}
		task := pendingMailboxEndTask
		status := pendingMailboxEndStatus
		pendingMailboxEndTask = nil
		pendingMailboxEndStatus = ""
		return writeMailboxTaskEnd(stderr, displayCommand, task, status)
	}
	resetTurnState := func() {
		turnInFlight = false
		turnStarted = false
		expectedTurnID = ""
		terminalSeen = false
		pendingSendErr = nil
		terminalTurnIDs = nil
	}
	finishTurnWithSendError := func(err error) error {
		if output.wroteText && !output.stdoutAtLineStart {
			if _, newlineErr := fmt.Fprintln(stdout); newlineErr != nil {
				return newlineErr
			}
			output.stdoutAtLineStart = true
		}
		resetTurnState()
		if _, printErr := fmt.Fprintf(stderr, "%s: send failed: %v\n", displayCommand, err); printErr != nil {
			return printErr
		}
		return writePendingMailboxEnd()
	}
	finishTurnIfReady := func() error {
		if !turnInFlight || !terminalSeen || sendDone != nil {
			return nil
		}
		if output.wroteText && !output.stdoutAtLineStart {
			if _, err := fmt.Fprintln(stdout); err != nil {
				return err
			}
			output.stdoutAtLineStart = true
		}
		if err := writePendingMailboxEnd(); err != nil {
			return err
		}
		resetTurnState()
		return nil
	}

	var initialInputCh <-chan chatInputEvent
	if sources.initialInput != nil {
		ch := make(chan chatInputEvent, 1)
		ch <- *sources.initialInput
		close(ch)
		initialInputCh = ch
		inputCh = ch
	}

	handleInput := func(input chatInputEvent) (bool, error) {
		command := strings.TrimSpace(input.line)
		if command == "" {
			return false, nil
		}
		if input.mailboxTask == nil && !input.multiline && (command == "/exit" || command == "/quit") {
			return true, nil
		}
		if input.mailboxTask == nil && !input.multiline && command == "/compact" {
			if _, err := service.CompactSession(ctx, sessionID); err != nil {
				if _, printErr := fmt.Fprintf(stderr, "%s: compact failed: %v\n", displayCommand, err); printErr != nil {
					return false, printErr
				}
				return false, nil
			}
			if _, err := fmt.Fprintf(stderr, "%s: compacted session context\n", displayCommand); err != nil {
				return false, err
			}
			return false, nil
		}

		sendCtx := ctx
		var taskCancel context.CancelFunc
		if input.mailboxTask != nil {
			sendCtx, taskCancel = context.WithCancel(ctx)
			if !mailbox.startTask(input.mailboxTask, taskCancel) {
				taskCancel()
				return false, nil
			}
			if err := writeMailboxTaskStart(stderr, displayCommand, input.mailboxTask, input.line); err != nil {
				taskCancel()
				mailbox.failTask(input.mailboxTask, err)
				return false, err
			}
		}

		done := make(chan attachSendResult, 1)
		eventCh := make(chan execution.SessionStreamEvent, 64)
		prompt := input.line
		task := input.mailboxTask
		sendDone = done
		events = eventCh
		turnInFlight = true
		turnStarted = false
		expectedTurnID = ""
		terminalSeen = false
		pendingSendErr = nil
		terminalTurnIDs = make(map[string]bool)
		go func() {
			defer close(eventCh)
			if taskCancel != nil {
				defer taskCancel()
			}
			result, err := service.SendSessionMessageWithEvents(sendCtx, sessionID, prompt, func(event execution.SessionStreamEvent) {
				select {
				case eventCh <- event:
				case <-sendCtx.Done():
				}
			})
			done <- attachSendResult{result: result, err: err, mailboxTask: task}
		}()
		return false, nil
	}

	for {
		if scanner != nil && inputCh == nil && !turnInFlight {
			inputCh = startChatInputRead(ctx, scanner, stderr)
		}
		if mailbox != nil && mailboxCh == nil && !turnInFlight {
			mailboxCh = startMailboxTaskRead(ctx, mailbox)
		}

		activeInputCh := inputCh
		activeMailboxCh := mailboxCh
		if turnInFlight {
			activeInputCh = nil
			activeMailboxCh = nil
		}
		if activeInputCh == nil && activeMailboxCh == nil && sendDone == nil && events == nil && !turnInFlight {
			return nil
		}

		select {
		case input := <-activeInputCh:
			wasInitialInput := initialInputCh != nil && activeInputCh == initialInputCh
			inputCh = nil
			if wasInitialInput {
				initialInputCh = nil
				if inheritedInputCh != nil {
					inputCh = inheritedInputCh
					inheritedInputCh = nil
				}
			}
			if input.err != nil {
				return input.err
			}
			if !input.ok {
				scanner = nil
				if mailbox == nil {
					return nil
				}
				continue
			}
			done, err := handleInput(input)
			if done || err != nil {
				return err
			}
		case mailboxRead := <-activeMailboxCh:
			mailboxCh = nil
			if mailboxRead.err != nil {
				return mailboxRead.err
			}
			if mailboxRead.task == nil {
				continue
			}
			done, err := handleInput(chatInputEvent{line: mailboxRead.task.Prompt, ok: true, mailboxTask: mailboxRead.task})
			if done || err != nil {
				return err
			}
		case sendResult := <-sendDone:
			sendDone = nil
			if sendResult.mailboxTask != nil {
				status := mailboxTaskCompleted
				if sendResult.err != nil {
					mailbox.failTask(sendResult.mailboxTask, sendResult.err)
					status = mailboxTaskFailed
				} else {
					result, err := mailboxFinalAssistantOutput(service, sessionID, sendResult.result.TurnID)
					if err != nil {
						mailbox.failTask(sendResult.mailboxTask, err)
						status = mailboxTaskFailed
					} else {
						mailbox.completeTask(sendResult.mailboxTask, result)
					}
				}
				pendingMailboxEndTask = sendResult.mailboxTask
				pendingMailboxEndStatus = mailboxTerminalStatus(sendResult.mailboxTask, status)
			}
			if sendResult.err != nil {
				pendingSendErr = sendResult.err
				if turnStarted || events != nil {
					if err := finishTurnIfReady(); err != nil {
						return err
					}
					continue
				}
				if err := finishTurnWithSendError(sendResult.err); err != nil {
					return err
				}
				continue
			}
			setExpectedTurnID(sendResult.result.TurnID)
			if err := finishTurnIfReady(); err != nil {
				return err
			}
		case event, ok := <-events:
			if !ok {
				events = nil
				if turnInFlight && sendDone == nil && pendingSendErr != nil && !terminalSeen {
					err := pendingSendErr
					if err := finishTurnWithSendError(err); err != nil {
						return err
					}
					continue
				}
				if err := finishTurnIfReady(); err != nil {
					return err
				}
				continue
			}
			eventType := attachEventType(event)
			if turnInFlight && eventType == "turn.started" {
				turnStarted = true
				setExpectedTurnID(attachEventTurnID(event))
			}
			if err := writeAttachStreamEvent(stdout, stderr, event, &output, displayCommand); err != nil {
				return err
			}
			if turnInFlight && isAttachTerminalEvent(event) {
				turnID := attachEventTurnID(event)
				if expectedTurnID == "" && turnID != "" {
					terminalTurnIDs[turnID] = true
				}
				if turnID != "" && turnID == expectedTurnID {
					terminalSeen = true
				}
				if err := finishTurnIfReady(); err != nil {
					return err
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func mailboxFinalAssistantOutput(service *execution.Service, sessionID, turnID string) (string, error) {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return "", fmt.Errorf("mailbox result missing turn id")
	}
	output, err := service.GetSessionTurnFinalAssistantOutput(sessionID, turnID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(output) == "" {
		return "", fmt.Errorf("mailbox result missing final assistant output")
	}
	return output, nil
}

func mailboxTaskDisplayID(task *mailboxTask) string {
	taskID := ""
	if task != nil {
		taskID = strings.TrimSpace(task.ID)
	}
	if taskID == "" {
		taskID = "(unknown)"
	}
	return taskID
}

func writeMailboxTaskStart(stderr io.Writer, command string, task *mailboxTask, prompt string) error {
	if _, err := fmt.Fprintf(stderr, "\n%s: ----- mailbox task %s started -----\n", command, mailboxTaskDisplayID(task)); err != nil {
		return err
	}
	prompt = strings.TrimRight(prompt, "\r\n")
	if strings.ContainsAny(prompt, "\r\n") {
		_, err := fmt.Fprintf(stderr, "user:\n%s\n", prompt)
		return err
	}
	_, err := fmt.Fprintf(stderr, "user: %s\n", prompt)
	return err
}

func writeMailboxTaskEnd(stderr io.Writer, command string, task *mailboxTask, status string) error {
	status = strings.TrimSpace(status)
	if status == "" {
		status = "finished"
	}
	_, err := fmt.Fprintf(stderr, "%s: ----- mailbox task %s %s -----\n", command, mailboxTaskDisplayID(task), status)
	return err
}

func attachEventType(event execution.SessionStreamEvent) string {
	eventType, _ := event["type"].(string)
	return eventType
}

func attachEventTurnID(event execution.SessionStreamEvent) string {
	turnID, _ := event["turn_id"].(string)
	return strings.TrimSpace(turnID)
}

func isAttachTerminalEvent(event execution.SessionStreamEvent) bool {
	eventType := attachEventType(event)
	return eventType == "turn.committed" || eventType == "turn.failed"
}

func writeAttachStreamEvent(stdout, stderr io.Writer, event execution.SessionStreamEvent, output *attachOutputState, command string) error {
	eventType, _ := event["type"].(string)
	writeStdout := func(text string) error {
		if text == "" {
			return nil
		}
		if _, err := fmt.Fprint(stdout, text); err != nil {
			return err
		}
		output.wroteText = true
		output.stdoutAtLineStart = strings.HasSuffix(text, "\n")
		return nil
	}
	endReasoningForText := func(text string) error {
		if output.needsReasoningBreak && text != "" {
			if !output.reasoningEndedWithNewline {
				if err := writeStdout("\n"); err != nil {
					return err
				}
			}
			output.needsReasoningBreak = false
			output.reasoningEndedWithNewline = false
		}
		output.inReasoningBlock = false
		return nil
	}
	endReasoningBeforeStatus := func() error {
		if output.inReasoningBlock && !output.stdoutAtLineStart {
			if err := writeStdout("\n"); err != nil {
				return err
			}
		}
		output.inReasoningBlock = false
		output.needsReasoningBreak = false
		output.reasoningEndedWithNewline = false
		return nil
	}
	switch eventType {
	case "text.delta":
		text, _ := event["text"].(string)
		if text == "" {
			return nil
		}
		if err := endReasoningForText(text); err != nil {
			return err
		}
		if err := writeStdout(text); err != nil {
			return err
		}
	case "reasoning.delta":
		text, _ := event["text"].(string)
		if text == "" {
			return nil
		}
		if !output.inReasoningBlock && !output.stdoutAtLineStart {
			if err := writeStdout("\n"); err != nil {
				return err
			}
		}
		if err := writeStdout(text); err != nil {
			return err
		}
		output.inReasoningBlock = true
		output.needsReasoningBreak = true
		output.reasoningEndedWithNewline = strings.HasSuffix(text, "\n")
	case "tool.started":
		name, _ := event["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			return nil
		}
		if err := endReasoningBeforeStatus(); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stderr, "tool: %s\n", name); err != nil {
			return err
		}
	case "turn.failed":
		if err := endReasoningBeforeStatus(); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stderr, "%s: turn failed\n", command); err != nil {
			return err
		}
	case "turn.committed":
		if err := endReasoningBeforeStatus(); err != nil {
			return err
		}
	case "compact.failed":
		if err := endReasoningBeforeStatus(); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stderr, "%s: compact failed\n", command); err != nil {
			return err
		}
	}
	return nil
}

func newCLIAttachExecutionService(homePath, _ string) (*execution.Service, error) {
	return execution.NewServiceWithAgentRunner(homePath)
}

func rejectSessionResumeOverrides(cwdProvided, configProvided bool, helpCommand string) error {
	if cwdProvided {
		return usageError("--cwd cannot be used when resuming an existing session", "", helpCommand)
	}
	if configProvided {
		return usageError("--config cannot be used when resuming an existing session", "", helpCommand)
	}
	return nil
}

func rejectSessionConfigForExistingCommand(configProvided bool, helpCommand string) error {
	if configProvided {
		return usageError("--config can only be used when creating a new session", "", helpCommand)
	}
	return nil
}

func resolveClientCWD(cwdFlag string, getwd func() (string, error)) (string, error) {
	cwd, err := resolveServerCWD(cwdFlag, getwd)
	if err != nil {
		return "", err
	}
	return canonicalPath(cwd)
}

func authCommand(ctx context.Context, args []string, configPath string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	if len(args) == 0 || containsHelpArg(args) && len(args) == 1 {
		printAuthUsage(stdout, displayCommand)
		return nil
	}
	if args[0] != "codex" {
		return usageError("usage: sai auth codex login", "", "sai help auth")
	}
	return authCodexCommand(ctx, args[1:], configPath, stdout, getwd, program)
}

func authCodexCommand(ctx context.Context, args []string, configPath string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
	if len(args) == 0 || containsHelpArg(args) && len(args) == 1 {
		printAuthCodexUsage(stdout, displayCommand)
		return nil
	}
	if args[0] != "login" {
		return usageError("usage: sai auth codex login", "", "sai help auth codex")
	}
	return authCodexLoginCommand(ctx, args[1:], configPath, stdout, getwd, program)
}

func authCodexLoginCommand(ctx context.Context, args []string, configPath string, stdout io.Writer, getwd func() (string, error), program string) error {
	displayCommand := displayProgramName(program)
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
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printAuthCodexLoginUsage, displayCommand, "sai help auth codex login")
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
	displayCommand := displayProgramName(program)
	flags := flag.NewFlagSet("sai mcp list", flag.ContinueOnError)
	var enabledMCP mcpServerIDsFlag
	flags.Var(&enabledMCP, "enable-mcp", "comma-separated MCP server ids to enable")
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printMCPListUsage, displayCommand, "sai help mcp list")
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

func toolsListCommand(args []string, stdout io.Writer, command string) error {
	flags := flag.NewFlagSet("sai tools list", flag.ContinueOnError)
	positionals, done, err := parseCommandFlagArgs(flags, args, stdout, printToolsListUsage, command, "sai help tools list")
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

func printSessionDetail(stdout io.Writer, session execution.SessionDetail) error {
	return printSessionDetailFields(stdout, session, false)
}

func printSessionDetailWithProject(stdout io.Writer, session execution.SessionDetail) error {
	return printSessionDetailFields(stdout, session, true)
}

func printSessionCommandStatus(stdout io.Writer, status string, session execution.SessionDetail) error {
	if _, err := fmt.Fprintf(stdout, "STATUS\t%s\n", status); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "ID\t%s\n", session.ID); err != nil {
		return err
	}
	if strings.TrimSpace(session.ProjectID) != "" {
		if _, err := fmt.Fprintf(stdout, "PROJECT_ID\t%s\n", session.ProjectID); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(stdout, "NAME\t%s\n", session.DisplayName); err != nil {
		return err
	}
	if strings.TrimSpace(session.CreatedCWD) != "" {
		if _, err := fmt.Fprintf(stdout, "CREATED_CWD\t%s\n", session.CreatedCWD); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(stdout, "ARCHIVED\t%t\n", session.Archived); err != nil {
		return err
	}
	lastUsedAt := session.LastUsedAt
	if lastUsedAt.IsZero() {
		lastUsedAt = session.UpdatedAt
	}
	_, err := fmt.Fprintf(stdout, "LAST_USED_AT\t%s\n", formatSessionTimestamp(lastUsedAt))
	return err
}

func printSessionDetailFields(stdout io.Writer, session execution.SessionDetail, includeProject bool) error {
	fmt.Fprintf(stdout, "ID\t%s\n", session.ID)
	fmt.Fprintf(stdout, "CREATED_AT\t%s\n", formatSessionTimestamp(session.CreatedAt))
	fmt.Fprintf(stdout, "UPDATED_AT\t%s\n", formatSessionTimestamp(session.UpdatedAt))
	lastUsedAt := session.LastUsedAt
	if lastUsedAt.IsZero() {
		lastUsedAt = session.UpdatedAt
	}
	fmt.Fprintf(stdout, "LAST_USED_AT\t%s\n", formatSessionTimestamp(lastUsedAt))
	fmt.Fprintf(stdout, "NAME\t%s\n", session.DisplayName)
	fmt.Fprintf(stdout, "ARCHIVED\t%t\n", session.Archived)
	fmt.Fprintf(stdout, "PROVIDER\t%s\n", session.Provider)
	fmt.Fprintf(stdout, "MODEL_PROFILE\t%s\n", session.ModelProfile)
	fmt.Fprintf(stdout, "MODEL_ID\t%s\n", session.ModelID)
	fmt.Fprintf(stdout, "STATUS\t%s\n", session.Status)
	fmt.Fprintf(stdout, "LAST_SEQ\t%d\n", session.LastSeq)
	if includeProject && strings.TrimSpace(session.ProjectID) != "" {
		fmt.Fprintf(stdout, "PROJECT_ID\t%s\n", session.ProjectID)
	}
	if includeProject && strings.TrimSpace(session.CreatedCWD) != "" {
		fmt.Fprintf(stdout, "CREATED_CWD\t%s\n", session.CreatedCWD)
	}
	fmt.Fprintf(stdout, "CONFIG_PATH\t%s\n", session.ConfigPath)
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
	return nil
}

func printSessionList(stdout io.Writer, infos []execution.SessionMetadata) error {
	if _, err := fmt.Fprintln(stdout, "ID\tPROJECT_ID\tNAME\tCREATED_CWD\tARCHIVED\tLAST_USED_AT"); err != nil {
		return err
	}
	for _, info := range infos {
		lastUsedAt := info.LastUsedAt
		if lastUsedAt.IsZero() {
			lastUsedAt = info.UpdatedAt
		}
		if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%t\t%s\n", info.ID, info.ProjectID, info.DisplayName, info.CreatedCWD, info.Archived, formatSessionTimestamp(lastUsedAt)); err != nil {
			return err
		}
	}
	return nil
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

func countSessionV2MessageItems(items []sessions.SessionItem) int {
	count := 0
	for _, item := range items {
		if item.Message != nil {
			count++
		}
	}
	return count
}

func parseCommandFlagArgs(flags *flag.FlagSet, args []string, stdout io.Writer, printUsage func(io.Writer, string), command, helpCommand string) ([]string, bool, error) {
	flags.SetOutput(io.Discard)
	if containsHelpArg(args) {
		printUsage(stdout, command)
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

func flagWasSet(flags *flag.FlagSet, name string) bool {
	wasSet := false
	flags.Visit(func(flag *flag.Flag) {
		if flag.Name == name {
			wasSet = true
		}
	})
	return wasSet
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

const multilineInputDelimiter = `"""`

type chatInputEvent struct {
	line        string
	multiline   bool
	ok          bool
	err         error
	mailboxTask *mailboxTask
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	doneInterrupts := forwardTurnInterrupts(turnCtx, cancel, interrupts)
	defer doneInterrupts()

	if shouldRunChatTurnWithSessionProjector(runtime) {
		return runChatTurnWithSessionProjector(ctx, turnCtx, runtime, messages, prompt, stdout, stderr, addTrailingNewline, stderrNeedsLeadingBreak)
	}

	messages, err := runtime.autoCompactBeforeTurn(turnCtx, messages, prompt, stderr)
	if err != nil {
		return nil, newRecoverableTurnError(err)
	}
	requestMessages := append(copyMessageSlice(messages), model.Message{
		Role:    model.MessageRoleUser,
		Content: prompt,
	})
	return runChatMessagesInTurn(ctx, turnCtx, runtime, requestMessages, stdout, stderr, addTrailingNewline, stderrNeedsLeadingBreak)
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

	return runChatMessagesInTurn(ctx, turnCtx, runtime, requestMessages, stdout, stderr, addTrailingNewline, stderrNeedsLeadingBreak)
}

func runChatMessagesInTurn(ctx, turnCtx context.Context, runtime *agentRuntime, requestMessages []model.Message, stdout, stderr io.Writer, addTrailingNewline bool, stderrNeedsLeadingBreak bool) ([]model.Message, error) {
	return runChatMessagesInTurnWithEventHook(ctx, turnCtx, runtime, requestMessages, stdout, stderr, addTrailingNewline, stderrNeedsLeadingBreak, nil)
}

func runChatMessagesInTurnWithEventHook(ctx, turnCtx context.Context, runtime *agentRuntime, requestMessages []model.Message, stdout, stderr io.Writer, addTrailingNewline bool, stderrNeedsLeadingBreak bool, onEvent func(model.Event)) ([]model.Message, error) {
	return runChatMessagesInTurnWithOptions(ctx, turnCtx, runtime, requestMessages, stdout, stderr, addTrailingNewline, stderrNeedsLeadingBreak, chatMessagesInTurnOptions{
		onEvent: onEvent,
	})
}

type chatMessagesInTurnOptions struct {
	onEvent   func(model.Event)
	bus       *eventbus.Bus
	publisher eventbus.Publisher
	turnID    string
}

func runChatMessagesInTurnWithOptions(ctx, turnCtx context.Context, runtime *agentRuntime, requestMessages []model.Message, stdout, stderr io.Writer, addTrailingNewline bool, stderrNeedsLeadingBreak bool, options chatMessagesInTurnOptions) ([]model.Message, error) {
	request := model.Request{
		Model:      runtime.modelID,
		Messages:   copyMessageSlice(requestMessages),
		Tools:      runtime.toolSchemas,
		Parameters: runtime.parameters,
	}
	renderBus := options.bus
	closeRenderBus := false
	if renderBus == nil {
		renderBus = eventbus.NewBus(nil)
		closeRenderBus = true
	}
	if closeRenderBus {
		defer renderBus.Close()
	}
	busEvents := renderBus.SubscribeLossless(0)
	agentEvents, results, err := agent.StreamWithResult(turnCtx, request, agent.Options{
		Provider:     runtime.provider,
		ToolExecutor: runtime.toolExecutor,
		MaxTurns:     runtime.maxTurns,
		TurnID:       options.turnID,
		Publisher:    options.publisher,
	})
	if err != nil {
		return nil, newRecoverableTurnError(err)
	}
	bridgeCtx := context.Background()
	pumpDone := publishModelEventsToBus(bridgeCtx, renderBus, agentEvents)
	events := modelEventsFromBusUntil(bridgeCtx, busEvents, pumpDone)

	tracker := &chatOutputWriter{w: stdout}
	if err := writeStreamWithOptions(tracker, stderr, events, runtime.showReasoning, runtime.logger, streamOutputOptions{
		colorReasoning:          shouldColorizeWriter(tracker),
		colorToolStatus:         shouldColorizeWriter(stderr),
		stderrNeedsLeadingBreak: stderrNeedsLeadingBreak,
		onEvent:                 options.onEvent,
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
	if addTrailingNewline && tracker.wrote && tracker.lastByte != '\n' {
		if _, err := fmt.Fprintln(stdout); err != nil {
			return nil, err
		}
	}
	return result.Messages, nil
}

func shouldRunChatTurnWithSessionProjector(runtime *agentRuntime) bool {
	if runtime == nil || !runtime.saveSessions || runtime.resumableSessionStore == nil {
		return false
	}
	return true
}

func publishCLIInterruptedTurn(publisher eventbus.Publisher, store *sessions.V2Store, sessionID, turnID string) {
	if publisher == nil {
		return
	}
	if err := publisher.Publish(eventbus.TurnInterrupted{TurnID: turnID}); err != nil && store != nil && sessionID != "" {
		_, _ = store.MarkTurnInterrupted(sessionID, turnID)
	}
}

func runChatTurnWithSessionProjector(ctx, turnCtx context.Context, runtime *agentRuntime, messages []model.Message, prompt string, stdout, stderr io.Writer, addTrailingNewline bool, stderrNeedsLeadingBreak bool) ([]model.Message, error) {
	session, writeLock, err := runtime.prepareSessionProjectorMetadataLocked(turnCtx)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = writeLock.Release()
	}()
	messages, err = runtime.materializeMessagesForSessionTurn(session)
	if err != nil {
		return nil, err
	}
	turnID := nextCLISessionTurnID(session)
	projector, err := sessionprojector.New(runtime.resumableSessionStore, session)
	if err != nil {
		return nil, err
	}
	defer projector.Close()
	bus := eventbus.NewBus(projector.Handler())
	defer bus.Close()

	turnStarted := false
	turnFinished := false
	defer func() {
		if !turnStarted || turnFinished {
			return
		}
		publishCLIInterruptedTurn(bus, runtime.resumableSessionStore, session.ID, turnID)
		_ = runtime.reloadResumableSession(session.ID)
	}()

	if err := bus.Publish(eventbus.TurnStarted{TurnID: turnID}); err != nil {
		return nil, err
	}
	turnStarted = true

	plannedMessages, compaction, err := runtime.planAutoCompactBeforeTurn(turnCtx, messages, prompt)
	if err != nil {
		return nil, newRecoverableTurnError(err)
	}
	if compaction != nil {
		if err := bus.Publish(eventbus.CompactionRequested{
			TurnID:     turnID,
			Summary:    compaction.summaryItem,
			Checkpoint: compaction.checkpoint,
		}); err != nil {
			return nil, newRecoverableTurnError(err)
		}
		messages = plannedMessages
	}

	userMessage := model.Message{
		Role:    model.MessageRoleUser,
		Content: prompt,
	}
	if err := bus.Publish(eventbus.TurnInputReady{TurnID: turnID, Message: userMessage}); err != nil {
		return nil, err
	}
	requestSession, err := runtime.resumableSessionStore.Load(session.ID)
	if err != nil {
		return nil, fmt.Errorf("load persisted turn input: %w", err)
	}
	requestMessages, err := runtime.materializeActiveHistory(requestSession)
	if err != nil {
		return nil, err
	}
	if err := validateActiveHistoryToolExchanges(requestSession.ID, requestMessages); err != nil {
		return nil, err
	}
	runtime.resumableSession = requestSession
	runtime.activeItemIDs = copyStringSlice(requestSession.ActiveHistory)
	updated, err := runChatMessagesInTurnWithOptions(ctx, turnCtx, runtime, requestMessages, stdout, stderr, addTrailingNewline, stderrNeedsLeadingBreak, chatMessagesInTurnOptions{
		bus:       bus,
		publisher: bus,
		turnID:    turnID,
	})
	if err != nil {
		return nil, err
	}
	if err := bus.Publish(eventbus.TurnCompleted{TurnID: turnID}); err != nil {
		return nil, err
	}
	turnFinished = true
	if err := runtime.saveRuntimeMetadataForSession(session.ID); err != nil {
		return nil, err
	}
	return updated, nil
}

func nextCLISessionTurnID(session sessions.SessionV2) string {
	return fmt.Sprintf("turn-%06d", session.LastSeq+1)
}

func (r *agentRuntime) reloadResumableSession(sessionID string) error {
	if r == nil || r.resumableSessionStore == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	loaded, err := r.resumableSessionStore.Load(sessionID)
	if err != nil {
		return err
	}
	r.resumableSession = loaded
	r.activeItemIDs = copyStringSlice(loaded.ActiveHistory)
	return nil
}

func publishModelEventsToBus(ctx context.Context, publisher eventbus.Publisher, events <-chan model.Event) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if publisher == nil {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				if err := publisher.Publish(eventbus.ModelEvent{Event: event}); err != nil {
					return
				}
			}
		}
	}()
	return done
}

func modelEventsFromBusUntil(ctx context.Context, events <-chan eventbus.Event, done <-chan struct{}) <-chan model.Event {
	out := make(chan model.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				if !sendModelEventFromBus(ctx, out, event) {
					return
				}
			case <-done:
				for {
					select {
					case event, ok := <-events:
						if !ok {
							return
						}
						if !sendModelEventFromBus(ctx, out, event) {
							return
						}
					default:
						return
					}
				}
			}
		}
	}()
	return out
}

func sendModelEventFromBus(ctx context.Context, out chan<- model.Event, event eventbus.Event) bool {
	modelEvent, ok := event.(eventbus.ModelEvent)
	if !ok || modelEvent.Event == nil {
		return true
	}
	select {
	case out <- modelEvent.Event:
		return true
	case <-ctx.Done():
		return false
	}
}

func modelEventsFromBus(ctx context.Context, events <-chan eventbus.Event) <-chan model.Event {
	out := make(chan model.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				if !sendModelEventFromBus(ctx, out, event) {
					return
				}
			}
		}
	}()
	return out
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
		updated, err := runChatTurn(ctx, runtime, messages, subagentCompletionPrompt(completions), stdout, stderr, addTrailingNewline, stderrNeedsLeadingBreak, interrupts)
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

func subagentCompletionPrompt(completions []subagents.JobSnapshot) string {
	parts := make([]string, 0, len(completions))
	for _, completion := range completions {
		parts = append(parts, formatSubagentCompletionEvent(completion))
	}
	return strings.Join(parts, "\n\n")
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
	displayCommand        string
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
	resumableSession      sessions.SessionV2
	resumableSessionStore *sessions.V2Store
	activeItemIDs         []string
	saveSessions          bool
	sessionSaveNoticeDone bool
	contextTracker        *contextwindow.Tracker
	logger                *eventlog.Logger
	mcpSessions           []*mcp.Session
	subagentManager       *subagents.Manager
	subagentCancel        context.CancelFunc
	config                *config.Config
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
		messages, err := r.materializeActiveHistory(r.resumableSession)
		if err == nil {
			if len(messages) == 0 && len(r.activeItemIDs) == 0 && len(r.baseMessages) > 0 {
				return copyMessageSlice(r.baseMessages)
			}
			return copyMessageSlice(messages)
		}
		return nil
	}
	return copyMessageSlice(r.baseMessages)
}

func (r *agentRuntime) materializeActiveHistory(session sessions.SessionV2) ([]model.Message, error) {
	if r != nil && r.resumableSessionStore != nil {
		return r.resumableSessionStore.MaterializeActiveHistory(session)
	}
	return session.MaterializeActiveHistory()
}

func (r *agentRuntime) writeSessionSaveNotice(stderr io.Writer) error {
	if !r.saveSessions || r.sessionSaveNoticeDone || stderr == nil {
		return nil
	}
	if _, err := fmt.Fprintln(stderr, renderCommandText(resumableSessionSaveNoticeText, r.displayCommand)); err != nil {
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

func (r *agentRuntime) compactSession(ctx context.Context, stderr io.Writer) ([]model.Message, error) {
	return r.compactSessionWithCheckpoint(ctx, stderr, compactionCheckpointOptions{
		reason:  "user_requested",
		phase:   "manual",
		trigger: "manual",
	})
}

type compactionCheckpointOptions struct {
	reason  string
	phase   string
	trigger string
}

type compactionPlan struct {
	summaryItem sessions.SessionItem
	checkpoint  sessions.CompactionCheckpoint
	messages    []model.Message
}

func (r *agentRuntime) autoCompactBeforeTurn(ctx context.Context, messages []model.Message, prompt string, stderr io.Writer) ([]model.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || r.config == nil || !r.config.Compaction.Enabled {
		return messages, nil
	}
	if !r.saveSessions || r.resumableSessionStore == nil || strings.TrimSpace(r.resumableSession.ID) == "" {
		return messages, nil
	}
	compactable, err := hasCompleteVisibleTurn(r.resumableSession)
	if err != nil {
		return nil, err
	}
	if !compactable {
		return messages, nil
	}
	contextWindow := 0
	if r.contextTracker != nil {
		contextWindow = r.contextTracker.Metadata().ContextWindow
	}
	if contextWindow <= 0 {
		return messages, nil
	}

	requestMessages := append(copyMessageSlice(messages), model.Message{
		Role:    model.MessageRoleUser,
		Content: prompt,
	})
	estimated := contextwindow.EstimateRequestTokens(model.Request{
		Model:      r.modelID,
		Messages:   requestMessages,
		Tools:      r.toolSchemas,
		Parameters: r.parameters,
	})
	if !autoCompactionThresholdExceeded(estimated, contextWindow, r.config.Compaction.ThresholdPercent) {
		return messages, nil
	}

	updated, err := r.compactSessionWithCheckpoint(ctx, stderr, compactionCheckpointOptions{
		reason:  "context_limit",
		phase:   "pre_turn",
		trigger: "auto",
	})
	if err != nil {
		return nil, fmt.Errorf("auto compact failed: %w", err)
	}
	return updated, nil
}

func (r *agentRuntime) planAutoCompactBeforeTurn(ctx context.Context, messages []model.Message, prompt string) ([]model.Message, *compactionPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if r == nil || r.config == nil || !r.config.Compaction.Enabled {
		return messages, nil, nil
	}
	if !r.saveSessions || strings.TrimSpace(r.resumableSession.ID) == "" {
		return messages, nil, nil
	}
	compactable, err := hasCompleteVisibleTurn(r.resumableSession)
	if err != nil {
		return nil, nil, err
	}
	if !compactable {
		return messages, nil, nil
	}
	contextWindow := 0
	if r.contextTracker != nil {
		contextWindow = r.contextTracker.Metadata().ContextWindow
	}
	if contextWindow <= 0 {
		return messages, nil, nil
	}

	requestMessages := append(copyMessageSlice(messages), model.Message{
		Role:    model.MessageRoleUser,
		Content: prompt,
	})
	estimated := contextwindow.EstimateRequestTokens(model.Request{
		Model:      r.modelID,
		Messages:   requestMessages,
		Tools:      r.toolSchemas,
		Parameters: r.parameters,
	})
	if !autoCompactionThresholdExceeded(estimated, contextWindow, r.config.Compaction.ThresholdPercent) {
		return messages, nil, nil
	}

	plan, err := r.planCompactionCheckpoint(ctx, compactionCheckpointOptions{
		reason:  "context_limit",
		phase:   "pre_turn",
		trigger: "auto",
	})
	if err != nil {
		return nil, nil, fmt.Errorf("auto compact failed: %w", err)
	}
	return plan.messages, &plan, nil
}

func autoCompactionThresholdExceeded(inputTokens, contextWindow, thresholdPercent int) bool {
	if inputTokens <= 0 || contextWindow <= 0 || thresholdPercent <= 0 {
		return false
	}
	return int64(inputTokens)*100 > int64(contextWindow)*int64(thresholdPercent)
}

func (r *agentRuntime) compactSessionWithCheckpoint(ctx context.Context, stderr io.Writer, checkpointOptions compactionCheckpointOptions) ([]model.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || r.config == nil {
		return nil, fmt.Errorf("runtime is not configured")
	}
	if !r.config.Compaction.Enabled {
		return nil, fmt.Errorf("compaction is disabled")
	}
	if !r.saveSessions || r.resumableSessionStore == nil || strings.TrimSpace(r.resumableSession.ID) == "" {
		return nil, fmt.Errorf("compaction requires a saved or resumed session")
	}

	plan, err := r.planCompactionCheckpoint(ctx, checkpointOptions)
	if err != nil {
		return nil, err
	}
	saved, err := r.resumableSessionStore.AppendCompactionCheckpoint(r.resumableSession.ID, plan.summaryItem, plan.checkpoint)
	if err != nil {
		return nil, fmt.Errorf("write compaction checkpoint: %w", err)
	}
	messages, err := r.materializeActiveHistory(saved)
	if err != nil {
		return nil, err
	}
	if err := validateActiveHistoryToolExchanges(saved.ID, messages); err != nil {
		return nil, err
	}
	r.resumableSession = saved
	r.activeItemIDs = copyStringSlice(saved.ActiveHistory)
	if _, err := fmt.Fprintf(stderr, "%s: compacted session context\n", r.displayCommand); err != nil {
		return nil, err
	}
	return messages, nil
}

func (r *agentRuntime) planCompactionCheckpoint(ctx context.Context, checkpointOptions compactionCheckpointOptions) (compactionPlan, error) {
	if err := ctx.Err(); err != nil {
		return compactionPlan{}, err
	}
	if r == nil || r.config == nil {
		return compactionPlan{}, fmt.Errorf("runtime is not configured")
	}
	if !r.config.Compaction.Enabled {
		return compactionPlan{}, fmt.Errorf("compaction is disabled")
	}
	if !r.saveSessions || strings.TrimSpace(r.resumableSession.ID) == "" {
		return compactionPlan{}, fmt.Errorf("compaction requires a saved or resumed session")
	}

	summaryModel, err := r.resolveSummaryModel()
	if err != nil {
		return compactionPlan{}, err
	}
	summaryInput, err := buildCompactionSummaryInput(r.resumableSession, summaryModel)
	if err != nil {
		return compactionPlan{}, err
	}
	provider, err := newProviderForRun(summaryModel.ProviderName, summaryModel.Type, summaryModel.Provider)
	if err != nil {
		return compactionPlan{}, err
	}
	summaryText, err := collectCompactionSummary(ctx, provider, summaryModel, summaryInput)
	if err != nil {
		return compactionPlan{}, err
	}

	summaryItemID := nextCompactionSummaryItemID(sessions.SessionItemIDs(r.resumableSession.Items))
	summaryMessage := model.Message{
		Role:    model.MessageRoleDeveloper,
		Content: formatCompactionSummary(summaryText),
	}
	summaryItem := sessions.SessionItem{
		ID:         summaryItemID,
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityHidden,
		Audience:   sessions.ItemAudienceModel,
		Message:    &summaryMessage,
	}
	replacementHistory, err := replacementHistoryAfterCompaction(r.resumableSession, summaryItemID)
	if err != nil {
		return compactionPlan{}, err
	}
	messages, err := validateCompactionReplacementHistory(r.resumableSession, summaryItem, replacementHistory)
	if err != nil {
		return compactionPlan{}, err
	}
	checkpoint := sessions.CompactionCheckpoint{
		ID:                    nextCompactionCheckpointID(r.resumableSession.Compactions),
		Reason:                checkpointOptions.reason,
		Phase:                 checkpointOptions.phase,
		Trigger:               checkpointOptions.trigger,
		SummaryItemID:         summaryItemID,
		FromItemID:            firstString(r.resumableSession.ActiveHistory),
		ToItemID:              lastString(r.resumableSession.ActiveHistory),
		PreviousActiveHistory: copyStringSlice(r.resumableSession.ActiveHistory),
		ReplacementHistory:    replacementHistory,
		SummaryProvider:       summaryModel.ProviderName,
		SummaryModel:          summaryModel.Profile,
	}
	return compactionPlan{
		summaryItem: summaryItem,
		checkpoint:  checkpoint,
		messages:    messages,
	}, nil
}

func (r *agentRuntime) resolveSummaryModel() (config.ResolvedModel, error) {
	compaction := r.config.Compaction
	providerName := strings.TrimSpace(compaction.SummaryProvider)
	modelProfile := strings.TrimSpace(compaction.SummaryModel)
	if providerName == "" {
		providerName = r.providerName
	}
	if modelProfile == "" {
		modelProfile = r.modelProfile
	}
	if strings.TrimSpace(compaction.SummaryProvider) != "" && strings.TrimSpace(compaction.SummaryModel) == "" {
		provider, ok := r.config.Providers[providerName]
		if !ok {
			return config.ResolvedModel{}, fmt.Errorf("unknown compaction summary provider %q", providerName)
		}
		if _, ok := provider.Models[modelProfile]; !ok {
			return config.ResolvedModel{}, fmt.Errorf("compaction summary_provider %q requires summary_model because current model profile %q is not available for that provider", providerName, modelProfile)
		}
	}
	resolved, err := r.config.ResolveModel(providerName, modelProfile)
	if err != nil {
		return config.ResolvedModel{}, fmt.Errorf("resolve compaction summary model: %w", err)
	}
	return resolved, nil
}

type compactionSummaryInput struct {
	Messages []model.Message
}

func buildCompactionSummaryInput(session sessions.SessionV2, resolved config.ResolvedModel) (compactionSummaryInput, error) {
	activeItems, err := activeHistoryItems(session)
	if err != nil {
		return compactionSummaryInput{}, err
	}
	contextItems, visibleGroups := splitCompactionInputItems(activeItems)
	for drop := 0; drop <= len(visibleGroups); drop++ {
		messages := compactionPromptMessages(contextItems, visibleGroups[drop:])
		request := model.Request{
			Model:      resolved.ModelID,
			Messages:   messages,
			Parameters: resolved.Parameters,
		}
		estimated := contextwindow.EstimateRequestTokens(request)
		if resolved.ContextWindow <= 0 || estimated < resolved.ContextWindow {
			return compactionSummaryInput{Messages: messages}, nil
		}
	}
	return compactionSummaryInput{}, fmt.Errorf("compaction summary input exceeds context window after trimming older visible history")
}

func activeHistoryItems(session sessions.SessionV2) ([]sessions.SessionItem, error) {
	itemsByID := make(map[string]sessions.SessionItem, len(session.Items))
	for _, item := range session.Items {
		itemsByID[item.ID] = item
	}
	items := make([]sessions.SessionItem, 0, len(session.ActiveHistory))
	for _, id := range session.ActiveHistory {
		item, ok := itemsByID[id]
		if !ok {
			return nil, corruptedSessionHistoryError(session.ID, "active history references missing item %q", id)
		}
		if item.Message == nil {
			return nil, corruptedSessionHistoryError(session.ID, "active history references item %q without a message", id)
		}
		items = append(items, item)
	}
	return items, nil
}

func splitCompactionInputItems(activeItems []sessions.SessionItem) ([]sessions.SessionItem, [][]sessions.SessionItem) {
	contextItems := []sessions.SessionItem{}
	groups := [][]sessions.SessionItem{}
	var current []sessions.SessionItem
	for _, item := range activeItems {
		if item.Visibility != sessions.ItemVisibilityVisible {
			contextItems = append(contextItems, item)
			continue
		}
		if item.Message != nil && item.Message.Role == model.MessageRoleUser {
			if len(current) > 0 {
				groups = append(groups, current)
			}
			current = []sessions.SessionItem{item}
			continue
		}
		if len(current) == 0 {
			contextItems = append(contextItems, item)
			continue
		}
		current = append(current, item)
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return contextItems, groups
}

func compactionPromptMessages(contextItems []sessions.SessionItem, visibleGroups [][]sessions.SessionItem) []model.Message {
	transcript := &strings.Builder{}
	if len(contextItems) > 0 {
		transcript.WriteString("## Current Model-Facing Runtime Context\n\n")
		for _, item := range contextItems {
			writeSummaryTranscriptMessage(transcript, *item.Message)
		}
		transcript.WriteByte('\n')
	}
	if len(visibleGroups) > 0 {
		transcript.WriteString("## Visible Conversation History\n\n")
		for _, group := range visibleGroups {
			for _, item := range group {
				writeSummaryTranscriptMessage(transcript, *item.Message)
			}
			transcript.WriteByte('\n')
		}
	}
	if transcript.Len() == 0 {
		transcript.WriteString("(No active model-facing messages are available.)\n")
	}

	return []model.Message{
		{
			Role:    model.MessageRoleSystem,
			Content: "Create a concise handoff checkpoint for continuing this session. Do not include hidden reasoning or chain-of-thought. Preserve facts, decisions, constraints, relevant files/APIs/commands, tool/environment state, open questions, and next steps. Use exactly these markdown headings: # Context Checkpoint, ## Goal, ## Current Progress, ## Decisions Made, ## Constraints / User Preferences, ## Relevant Files / APIs / Commands, ## Tool State / Environment State, ## Open Questions, ## Next Steps. Mention that old complete session items remain stored but may be omitted from active model context.",
		},
		{
			Role:    model.MessageRoleUser,
			Content: transcript.String(),
		},
	}
}

func writeSummaryTranscriptMessage(out *strings.Builder, message model.Message) {
	fmt.Fprintf(out, "<message role=%q", message.Role)
	if message.ToolCallID != "" {
		fmt.Fprintf(out, " tool_call_id=%q", message.ToolCallID)
	}
	out.WriteString(">\n")
	if message.Content != "" {
		out.WriteString(message.Content)
		if !strings.HasSuffix(message.Content, "\n") {
			out.WriteByte('\n')
		}
	}
	for _, toolCall := range message.ToolCalls {
		fmt.Fprintf(out, "<tool_call id=%q name=%q arguments=%q />\n", toolCall.ID, toolCall.Name, toolCall.Arguments)
	}
	out.WriteString("</message>\n")
}

func collectCompactionSummary(ctx context.Context, provider model.Provider, resolved config.ResolvedModel, input compactionSummaryInput) (string, error) {
	stream, err := provider.Stream(ctx, model.Request{
		Model:      resolved.ModelID,
		Messages:   input.Messages,
		Parameters: resolved.Parameters,
	})
	if err != nil {
		return "", fmt.Errorf("request compaction summary: %w", err)
	}
	var text strings.Builder
	var doneText string
	for event := range stream {
		switch event := event.(type) {
		case model.TextDeltaEvent:
			text.WriteString(event.Text)
		case model.MessageDoneEvent:
			doneText = event.Message.Content
		case model.ErrorEvent:
			if event.Message != "" {
				return "", fmt.Errorf("%s: %w", event.Message, event.Err)
			}
			return "", event.Err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	summary := strings.TrimSpace(text.String())
	if summary == "" {
		summary = strings.TrimSpace(doneText)
	}
	if summary == "" {
		return "", fmt.Errorf("compaction summary was empty")
	}
	return summary, nil
}

func formatCompactionSummary(summary string) string {
	return "<compaction_summary>\nAnother agent continued this session from a checkpoint. Use the state below as handoff context. Do not treat it as a new user request.\n\n" + strings.TrimSpace(summary) + "\n</compaction_summary>"
}

const compactionRecentVisibleTurnLimit = 2

func replacementHistoryAfterCompaction(session sessions.SessionV2, summaryItemID string) ([]string, error) {
	activeItems, err := activeHistoryItems(session)
	if err != nil {
		return nil, err
	}
	replacement := make([]string, 0, len(activeItems)+1)
	for _, item := range activeItems {
		if item.Kind == sessions.ItemKindRuntimeContext && item.Message != nil {
			replacement = append(replacement, item.ID)
		}
	}
	for _, group := range recentCompleteVisibleTurns(activeItems, compactionRecentVisibleTurnLimit) {
		for _, item := range group {
			replacement = append(replacement, item.ID)
		}
	}
	replacement = append(replacement, summaryItemID)
	return replacement, nil
}

func validateCompactionReplacementHistory(session sessions.SessionV2, summaryItem sessions.SessionItem, replacementHistory []string) ([]model.Message, error) {
	messages, err := materializeCompactionReplacementHistory(session, summaryItem, replacementHistory)
	if err != nil {
		return nil, err
	}
	if err := validateActiveHistoryToolExchanges(session.ID, messages); err != nil {
		return nil, err
	}
	return messages, nil
}

func materializeCompactionReplacementHistory(session sessions.SessionV2, summaryItem sessions.SessionItem, replacementHistory []string) ([]model.Message, error) {
	itemsByID := make(map[string]sessions.SessionItem, len(session.Items)+1)
	for _, item := range session.Items {
		itemsByID[item.ID] = item
	}
	itemsByID[summaryItem.ID] = summaryItem

	messages := make([]model.Message, 0, len(replacementHistory))
	for _, id := range replacementHistory {
		item, ok := itemsByID[id]
		if !ok {
			return nil, corruptedSessionHistoryError(session.ID, "compaction replacement history references missing item %q", id)
		}
		if item.Message == nil {
			return nil, corruptedSessionHistoryError(session.ID, "compaction replacement history references item %q without a message", id)
		}
		messages = append(messages, copyMessageSlice([]model.Message{*item.Message})[0])
	}
	return messages, nil
}

func recentCompleteVisibleTurns(activeItems []sessions.SessionItem, limit int) [][]sessions.SessionItem {
	if limit <= 0 {
		return nil
	}
	_, groups := splitCompactionInputItems(activeItems)
	selected := make([][]sessions.SessionItem, 0, limit)
	for i := len(groups) - 1; i >= 0; i-- {
		if visibleTurnIsComplete(groups[i]) {
			selected = append([][]sessions.SessionItem{groups[i]}, selected...)
			if len(selected) == limit {
				return selected
			}
		}
	}
	return selected
}

func hasCompleteVisibleTurn(session sessions.SessionV2) (bool, error) {
	activeItems, err := activeHistoryItems(session)
	if err != nil {
		return false, err
	}
	_, groups := splitCompactionInputItems(activeItems)
	for _, group := range groups {
		if visibleTurnIsComplete(group) {
			return true, nil
		}
	}
	return false, nil
}

func visibleTurnIsComplete(items []sessions.SessionItem) bool {
	if len(items) == 0 || items[0].Message == nil || items[0].Message.Role != model.MessageRoleUser {
		return false
	}
	last := items[len(items)-1].Message
	if last == nil || last.Role != model.MessageRoleAssistant || len(last.ToolCalls) != 0 {
		return false
	}
	messages := make([]model.Message, 0, len(items))
	for _, item := range items {
		if item.Message == nil {
			return false
		}
		messages = append(messages, *item.Message)
	}
	return validateActiveHistoryToolExchanges("", messages) == nil
}

func nextCompactionSummaryItemID(existing map[string]struct{}) string {
	for i := len(existing) + 1; ; i++ {
		id := fmt.Sprintf("summary-%06d", i)
		if _, ok := existing[id]; !ok {
			return id
		}
	}
}

func nextCompactionCheckpointID(existing []sessions.CompactionCheckpoint) string {
	used := make(map[string]struct{}, len(existing))
	for _, checkpoint := range existing {
		used[checkpoint.ID] = struct{}{}
	}
	for i := len(existing) + 1; ; i++ {
		id := fmt.Sprintf("compact-%06d", i)
		if _, ok := used[id]; !ok {
			return id
		}
	}
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func lastString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

func formatContextMetadataValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(none)"
	}
	return value
}

func (r *agentRuntime) refreshedSessionMetadata() sessions.SessionV2 {
	return r.refreshSessionRuntimeMetadata(r.resumableSession)
}

func (r *agentRuntime) refreshSessionRuntimeMetadata(session sessions.SessionV2) sessions.SessionV2 {
	var contextMetadata *contextwindow.Metadata
	if r.contextTracker != nil {
		metadata := r.contextTracker.Metadata()
		contextMetadata = &metadata
	}
	return sessions.RefreshRuntimeMetadata(session, sessions.RuntimeMetadataUpdate{
		Provider:             r.providerName,
		ModelProfile:         r.modelProfile,
		ModelID:              r.modelID,
		ModelParameters:      r.parameters,
		CWD:                  r.cwd,
		ConfigPath:           r.configPath,
		EnabledTools:         r.enabledTools,
		EnabledMCP:           r.enabledMCP,
		EnabledSkills:        r.enabledSkills,
		ShowReasoning:        r.showReasoning,
		InstructionsSnapshot: r.baseMessages,
		InstructionSources:   r.instructionSources,
		Context:              contextMetadata,
		SaveToolResults:      true,
	})
}

func (r *agentRuntime) saveRuntimeMetadataForSession(sessionID string) error {
	if r == nil || r.resumableSessionStore == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	loaded, err := r.resumableSessionStore.Load(sessionID)
	if err != nil {
		return err
	}
	saved, err := r.resumableSessionStore.SaveMetadata(r.refreshSessionRuntimeMetadata(loaded))
	if err != nil {
		return fmt.Errorf("save resumable session metadata: %w", err)
	}
	r.resumableSession = saved
	r.activeItemIDs = copyStringSlice(saved.ActiveHistory)
	return nil
}

func (r *agentRuntime) prepareSessionProjectorMetadata() (sessions.SessionV2, error) {
	session, lock, err := r.prepareSessionProjectorMetadataLocked(context.Background())
	if lock != nil {
		_ = lock.Release()
	}
	return session, err
}

func (r *agentRuntime) prepareSessionProjectorMetadataLocked(ctx context.Context) (sessions.SessionV2, *sessions.SessionWriteLock, error) {
	if !r.saveSessions {
		return sessions.SessionV2{}, nil, fmt.Errorf("resumable session saving is not enabled")
	}
	if r.resumableSessionStore == nil {
		return sessions.SessionV2{}, nil, fmt.Errorf("session store is not configured")
	}

	session := r.refreshedSessionMetadata()
	sessionID := strings.TrimSpace(session.ID)
	if sessionID == "" {
		saved, err := r.resumableSessionStore.SaveMetadata(session)
		if err != nil {
			return sessions.SessionV2{}, nil, fmt.Errorf("save resumable session metadata: %w", err)
		}
		sessionID = saved.ID
	}
	writeLock, err := r.resumableSessionStore.AcquireSessionWriteLock(ctx, sessionID)
	if err != nil {
		return sessions.SessionV2{}, nil, err
	}
	releaseOnError := true
	defer func() {
		if releaseOnError {
			_ = writeLock.Release()
		}
	}()

	loaded, err := r.resumableSessionStore.Load(sessionID)
	if err != nil {
		return sessions.SessionV2{}, nil, fmt.Errorf("load resumable session metadata: %w", err)
	}
	saved, err := r.resumableSessionStore.SaveMetadata(r.refreshSessionRuntimeMetadata(loaded))
	if err != nil {
		return sessions.SessionV2{}, nil, fmt.Errorf("save resumable session metadata: %w", err)
	}
	loaded, err = r.resumableSessionStore.Load(saved.ID)
	if err != nil {
		return sessions.SessionV2{}, nil, fmt.Errorf("load resumable session metadata: %w", err)
	}
	r.resumableSession = loaded
	r.activeItemIDs = copyStringSlice(loaded.ActiveHistory)
	releaseOnError = false
	return loaded, writeLock, nil
}

func (r *agentRuntime) materializeMessagesForSessionTurn(session sessions.SessionV2) ([]model.Message, error) {
	messages, err := r.materializeActiveHistory(session)
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 && len(session.ActiveHistory) == 0 && len(r.baseMessages) > 0 {
		return copyMessageSlice(r.baseMessages), nil
	}
	return copyMessageSlice(messages), nil
}

type runtimePreparationOptions struct {
	enableSubagents             bool
	resumedSessionOverride      *sessions.SessionV2
	resumedSessionStoreOverride *sessions.V2Store
}

func prepareAgentRuntimeWithOptions(ctx context.Context, configPath string, options agentCommandFlags, stderr io.Writer, getwd func() (string, error), program string, prep runtimePreparationOptions) (runtime *agentRuntime, err error) {
	displayCommand := displayProgramName(program)
	warningWriter := newCommandWarningWriter(stderr, displayCommand)
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

	var resumedSession sessions.SessionV2
	var resumedMessages []model.Message
	resumed := false
	saveSessions := cfg.Sessions.Enabled
	if options.saveSessionSet {
		saveSessions = options.saveSession
	}
	if options.resumeID != "" || options.continueSession || prep.resumedSessionOverride != nil {
		saveSessions = true
	}
	if saveSessions && !cfg.Sessions.SaveToolResults {
		return nil, fmt.Errorf("resumable sessions require sessions.save_tool_results: true")
	}
	sessionStore := sessions.NewV2Store(cfg.Sessions.Dir)
	resumeRequested := options.resumeID != "" || options.continueSession || prep.resumedSessionOverride != nil
	if resumeRequested {
		if prep.resumedSessionOverride != nil {
			resumedSession = *prep.resumedSessionOverride
			if strings.TrimSpace(resumedSession.ID) == "" {
				return nil, fmt.Errorf("resumable session id is required")
			}
			if options.resumeID != "" && options.resumeID != resumedSession.ID {
				return nil, fmt.Errorf("resumable session override id %q does not match requested session %q", resumedSession.ID, options.resumeID)
			}
		} else {
			resumedSession, err = loadResumableSession(sessionStore, options.resumeID, options.continueSession)
		}
		if err != nil {
			return nil, err
		}
		if resumedSession.Version > sessions.VersionV2 {
			return nil, fmt.Errorf("session %q uses unsupported version %d; current version is %d", resumedSession.ID, resumedSession.Version, sessions.VersionV2)
		}
		if !resumedSession.SaveToolResults {
			return nil, fmt.Errorf("session %q cannot be reliably resumed because save_tool_results is false", resumedSession.ID)
		}
		resumeMaterializer := sessionStore
		if prep.resumedSessionOverride != nil && prep.resumedSessionStoreOverride != nil {
			resumeMaterializer = prep.resumedSessionStoreOverride
		}
		if resumeMaterializer != nil {
			resumedMessages, err = resumeMaterializer.MaterializeActiveHistory(resumedSession)
		} else {
			resumedMessages, err = resumedSession.MaterializeActiveHistory()
		}
		if err != nil {
			return nil, err
		}
		if err := validateActiveHistoryToolExchanges(resumedSession.ID, resumedMessages); err != nil {
			return nil, err
		}
		if err := validateResumeCLIConflicts(resumedSession, options); err != nil {
			return nil, err
		}
		applyResumeOptions(&options, resumedSession)
		resumed = true
	}
	resumableSessionStore := sessionStore
	if prep.resumedSessionOverride != nil {
		resumableSessionStore = prep.resumedSessionStoreOverride
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
		WarningWriter: warningWriter,
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
	if resumed && (len(resumedSession.InstructionsSnapshot) > 0 || len(resumedSession.ActiveHistory) > 0) {
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
			WarningWriter:    warningWriter,
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
		displayCommand:        displayCommand,
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
		resumableSessionStore: resumableSessionStore,
		activeItemIDs:         copyStringSlice(resumedSession.ActiveHistory),
		saveSessions:          saveSessions,
		contextTracker:        contextTracker,
		logger:                logger,
		mcpSessions:           mcpSessions,
		subagentManager:       subagentManager,
		subagentCancel:        subagentCancel,
		config:                cfg,
	}, nil
}

func loadResumableSession(store *sessions.V2Store, id string, latest bool) (sessions.SessionV2, error) {
	if latest {
		session, err := store.Latest()
		if err != nil {
			if errors.Is(err, sessions.ErrNotFound) {
				return sessions.SessionV2{}, fmt.Errorf("no resumable sessions found")
			}
			return sessions.SessionV2{}, err
		}
		return session, nil
	}

	session, err := store.Load(id)
	if err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			return sessions.SessionV2{}, fmt.Errorf("resumable session %q was not found", id)
		}
		return sessions.SessionV2{}, err
	}
	return session, nil
}

func validateResumeCLIConflicts(session sessions.SessionV2, options agentCommandFlags) error {
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

func applyResumeOptions(options *agentCommandFlags, session sessions.SessionV2) {
	options.providerName = session.Provider
	options.modelProfile = session.ModelProfile
	options.enabledTools = toolNamesFlag{set: true, names: copyStringSlice(session.EnabledTools)}
	options.enabledMCP = mcpServerIDsFlag{set: true, ids: copyStringSlice(session.EnabledMCP)}
	options.showReasoning = session.ShowReasoning
}

func validateActiveHistoryToolExchanges(sessionID string, messages []model.Message) error {
	pendingToolCalls := map[string]struct{}{}
	for index, message := range messages {
		if message.Role == model.MessageRoleTool {
			if message.ToolCallID == "" {
				return corruptedSessionHistoryError(sessionID, "active history tool message at index %d is missing tool_call_id", index)
			}
			if _, ok := pendingToolCalls[message.ToolCallID]; !ok {
				return corruptedSessionHistoryError(sessionID, "active history tool message at index %d references unresolved tool call %q", index, message.ToolCallID)
			}
			delete(pendingToolCalls, message.ToolCallID)
			continue
		}
		if len(pendingToolCalls) > 0 {
			return corruptedSessionHistoryError(sessionID, "active history message at index %d appears before tool results for %s", index, formatPendingToolCallIDs(pendingToolCalls))
		}
		if len(message.ToolCalls) == 0 {
			continue
		}
		if message.Role != model.MessageRoleAssistant {
			return corruptedSessionHistoryError(sessionID, "active history %s message at index %d contains tool calls", message.Role, index)
		}
		for _, toolCall := range message.ToolCalls {
			if toolCall.ID == "" {
				return corruptedSessionHistoryError(sessionID, "active history assistant message at index %d contains a tool call without id", index)
			}
			if _, exists := pendingToolCalls[toolCall.ID]; exists {
				return corruptedSessionHistoryError(sessionID, "active history assistant message at index %d repeats tool call id %q", index, toolCall.ID)
			}
			pendingToolCalls[toolCall.ID] = struct{}{}
		}
	}
	if len(pendingToolCalls) > 0 {
		return corruptedSessionHistoryError(sessionID, "active history ends before tool results for %s", formatPendingToolCallIDs(pendingToolCalls))
	}
	return nil
}

func corruptedSessionHistoryError(sessionID, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	if sessionID == "" {
		return fmt.Errorf("%w: %s", sessions.ErrCorruptedSession, message)
	}
	return fmt.Errorf("%w %q: %s", sessions.ErrCorruptedSession, sessionID, message)
}

func formatPendingToolCallIDs(pending map[string]struct{}) string {
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
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
	onEvent                 func(model.Event)
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
		if options.onEvent != nil {
			options.onEvent(event)
		}
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
		case model.ToolStartedEvent:
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
