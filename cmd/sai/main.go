// Command sai is the command-line chat interface for the agent. Committed
// records render to stdout as human-readable blocks (a common header line
// plus the record's content) as soon as they are committed. While the
// model streams, a dot spinner cycles on stderr; control messages
// (prompts, notices) also go to stderr.
//
// Assembly happens here and only here: the config is read once, the
// provider, tools, and storage are built, and the agent is constructed
// with everything injected. The agent never reads configuration.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/storage"
	"github.com/rexzhao/simple-agent/internal/storage/jsonl"
	"github.com/rexzhao/simple-agent/internal/tools"
)

var version = "dev"

// spinnerFrames cycles ".", "..", "...", and an empty frame: the empty
// frame makes the animation blink rather than crawl.
var spinnerFrames = []string{".", "..", "...", ""}

const spinnerInterval = 250 * time.Millisecond

// resumeTailRecords bounds how much history a resumed session prints; the
// full log stays in the file.
const resumeTailRecords = 20

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		serverRoot  = flag.String("server-root", "", "configuration namespace root (default: $SAI_SERVER_ROOT, else user config dir/<app>)")
		sessionID   = flag.String("session", "", "resume a stored session by id")
		provider    = flag.String("provider", "", "override default_provider")
		modelName   = flag.String("model", "", "override default_model (model profile name)")
		httpAddr    = flag.String("http", "", "serve the web UI on this address (e.g. 127.0.0.1:7420) instead of the interactive REPL")
		httpStatic  = flag.String("http-static", "", "serve a chat client directory (e.g. clients/chat/dist) at /chat")
		httpDevCORS = flag.Bool("http-dev-cors", false, "answer CORS for loopback origins on /api (direct dev-server connections; the Vite proxy flow needs no CORS)")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if strings.TrimSpace(*serverRoot) == "" {
		root, err := resolveServerRoot()
		if err != nil {
			log.Fatalf("sai: %v", err)
		}
		*serverRoot = root
	}
	// The registry subcommands run before any agent assembly; they see
	// the same resolved root the session-bearing modes use.
	resolvedServerRoot = *serverRoot
	if flag.NArg() > 0 && flag.Arg(0) == "project" {
		os.Exit(projectCommand(flag.Args()[1:]))
	}

	// Serve mode hosts many sessions and assembles per project through
	// its own factory — no CLI agent is built for it.
	if *httpAddr != "" {
		resumeID := strings.TrimSpace(*sessionID)
		if resumeID == "" && flag.NArg() > 0 {
			resumeID = strings.TrimSpace(flag.Arg(0))
		}
		serve(*serverRoot, *httpAddr, *provider, *modelName, resumeID, *httpStatic, *httpDevCORS)
		return
	}

	ag, project, err := assembleCurrentDir(*serverRoot, *provider, *modelName)
	if err != nil {
		log.Fatalf("sai: %v", err)
	}
	// The root can be discovered from the environment or the working
	// directory; say which one won so silent discovery never becomes silent
	// configuration drift.
	fmt.Fprintf(os.Stderr, "sai: server root %s, project %s\n", *serverRoot, project)

	// A -session flag (or an optional positional argument) resumes a
	// stored session directly; a session which cannot be found is fatal.
	resumeID := strings.TrimSpace(*sessionID)
	if resumeID == "" && flag.NArg() > 0 {
		resumeID = strings.TrimSpace(flag.Arg(0))
	}

	cli := &cliApp{agent: ag, printer: &spinnerPrinter{}}
	if resumeID != "" {
		if err := cli.resume(resumeID); err != nil {
			log.Fatalf("sai: %v", err)
		}
	} else if err := cli.freshSession(); err != nil {
		log.Fatalf("sai: %v", err)
	}

	// Ctrl+C stops the active run; a second Ctrl+C when idle exits.
	// Color is for terminals only; pipes and files must stay plain.
	colorEnabled = stdoutIsTerminal() && enableVirtualTerminal()
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	go func() {
		for range interrupts {
			if cli.session.Running() {
				cli.session.Stop()
				fmt.Fprintln(os.Stderr, "\rsai: stopping run")
			} else {
				os.Exit(130)
			}
		}
	}()

	fmt.Fprintln(os.Stderr, "sai: /clear /compact /resume <id> /stop /help /quit")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for {
		fmt.Fprint(os.Stderr, "sai> ")
		if !scanner.Scan() {
			break
		}
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		if cli.dispatch(text) {
			break
		}
	}
	// On the way out, name the session to resume — but only if it was
	// persisted: an empty session left nothing on disk to reopen.
	if cli.session != nil && cli.session.Persisted() {
		fmt.Fprintf(os.Stderr, "sai: session %s\n", cli.session.ID())
	}
}

// agentFactory assembles one agent per project: everything project-
// independent (config, provider, store) is resolved once; everything
// else (tool executor, system prompt, project id) is bound to the
// project's cwd at assembly time. serve mode instantiates lazily per
// registered project; the CLI instantiates once for the process cwd.
type agentFactory struct {
	store         *jsonl.Store
	cfg           *config.Config
	provider      model.Provider
	providerName  string
	modelID       string
	parameters    map[string]any
	developerRole model.MessageRole
	outputLimit   int
	contextWindow int
}

// assembleFactory resolves the project-independent half of the assembly.
func assembleFactory(serverRoot, providerName, modelName string) (*agentFactory, error) {
	configPath := filepath.Join(serverRoot, "sai.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", configPath, err)
	}
	resolved, err := cfg.ResolveModel(providerName, modelName)
	if err != nil {
		return nil, err
	}
	provider, err := newProvider(resolved)
	if err != nil {
		return nil, err
	}
	parameters, err := config.ApplyReasoningLevel(resolved.Parameters, resolved.ReasoningConfig, "")
	if err != nil {
		return nil, fmt.Errorf("apply reasoning level: %w", err)
	}
	developerRole := model.MessageRoleDeveloper
	if strings.TrimSpace(resolved.DeveloperRole) != "" {
		developerRole = model.MessageRole(resolved.DeveloperRole)
	}
	store, err := jsonl.OpenStore(filepath.Join(serverRoot, "data"))
	if err != nil {
		return nil, err
	}
	return &agentFactory{
		store:         store,
		cfg:           cfg,
		provider:      provider,
		providerName:  resolved.ProviderName,
		modelID:       resolved.ModelID,
		parameters:    parameters,
		developerRole: developerRole,
		outputLimit:   resolved.OutputLimit,
		contextWindow: resolved.ContextWindow,
	}, nil
}

// Agent assembles the agent for one project directory.
func (f *agentFactory) Agent(projectCwd string) (*agent.Agent, string, error) {
	shell, executor, err := tools.Shell(projectCwd)
	if err != nil {
		return nil, "", fmt.Errorf("register tools: %w", err)
	}
	systemPrompt, err := buildSystemPrompt(f.cfg, projectCwd)
	if err != nil {
		return nil, "", err
	}
	project := jsonl.ProjectID(projectCwd)
	ag, err := agent.New(agent.Options{
		Provider:      f.provider,
		ProviderName:  f.providerName,
		ModelID:       f.modelID,
		Storage:       f.store,
		Tools:         []model.Tool{shell},
		ToolExecutor:  namedExecutor{exec: executor},
		SystemPrompt:  systemPrompt,
		DeveloperRole: f.developerRole,
		Parameters:    f.parameters,
		MaxTokens:     f.outputLimit,
		MaxIterations: f.cfg.Agent.MaxIterations,
		ProjectID:     project,
		CWD:           projectCwd,
		ContextWindow: f.contextWindow,
		AutoCompact:   f.cfg.Agent.AutoCompact,
	})
	if err != nil {
		return nil, "", err
	}
	return ag, project, nil
}

// assembleCurrentDir is the CLI path: one agent for the process cwd.
func assembleCurrentDir(serverRoot, providerName, modelName string) (*agent.Agent, string, error) {
	cwd, err := filepath.Abs(".")
	if err != nil {
		return nil, "", fmt.Errorf("resolve working directory: %w", err)
	}
	factory, err := assembleFactory(serverRoot, providerName, modelName)
	if err != nil {
		return nil, "", err
	}
	return factory.Agent(cwd)
}

// namedExecutor adapts the single-tool executor to the agent loop's
// name-dispatching ToolExecutor interface.
type namedExecutor struct {
	exec tools.Executor
}

func (n namedExecutor) Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error) {
	if name != tools.BuiltinShell {
		return model.ToolResult{}, fmt.Errorf("unknown tool %q", name)
	}
	return n.exec.Execute(ctx, arguments)
}

// spinnerPrinter interleaves the stderr spinner with stdout record lines:
// a record line always erases the spinner first, so the two never mix on a
// combined terminal.
type spinnerPrinter struct {
	mu    sync.Mutex
	shown bool
}

// frame draws one spinner frame on stderr.
func (p *spinnerPrinter) frame(frame string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(os.Stderr, "\r%s", frame)
	p.shown = frame != ""
}

// clear erases the spinner, if drawn.
func (p *spinnerPrinter) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shown {
		fmt.Fprint(os.Stderr, "\r  \r")
		p.shown = false
	}
}

// line prints one record line to stdout, clearing the spinner first.
func (p *spinnerPrinter) line(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shown {
		fmt.Fprint(os.Stderr, "\r  \r")
		p.shown = false
	}
	fmt.Println(line)
}

// write prints streaming text without a trailing newline, clearing the
// spinner first.
func (p *spinnerPrinter) write(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shown {
		fmt.Fprint(os.Stderr, "\r  \r")
		p.shown = false
	}
	fmt.Print(text)
}

// cliApp holds the active session.
type cliApp struct {
	agent       *agent.Agent
	session     *agent.Session
	printer     *spinnerPrinter
	unwatch     func()
	watcherDone chan struct{} // closed when the record watcher goroutine exited

	// liveMu guards liveShown and parked: assistant messages rendered
	// live by the event drain (their committed records must not print
	// twice) and records parked pending that decision.
	liveMu    sync.Mutex
	liveShown map[string]struct{}
	parked    []storage.Record

	// printedMu guards printed: how many records the watcher (and the
	// synchronous history print) has processed. waitForRecords blocks
	// until it catches up with the session's committed count.
	printedMu sync.Mutex
	printed   int
}

// countPrinted records one processed record.
func (c *cliApp) countPrinted() {
	c.printedMu.Lock()
	c.printed++
	c.printedMu.Unlock()
}

// waitForRecords blocks until the record printer has processed every
// committed record, so the next prompt never appears before pending
// output. A bounded timeout keeps a stuck printer from wedging the REPL.
func (c *cliApp) waitForRecords() {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.printedMu.Lock()
		printed := c.printed
		c.printedMu.Unlock()
		if printed >= c.session.RecordCount() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// dispatch handles one input line. It returns true when the REPL should
// exit.
func (c *cliApp) dispatch(text string) bool {
	switch {
	case text == "/quit" || text == "/exit":
		c.closeSession()
		return true
	case text == "/clear":
		if err := c.freshSession(); err != nil {
			fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		}
	case text == "/stop":
		fmt.Fprintln(os.Stderr, "sai: stopping run")
		c.session.Stop()
		// Wait for the run to settle and its records to print, so the
		// prompt never interleaves the settling output.
		deadline := time.Now().Add(5 * time.Second)
		for c.session.Running() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		c.waitForRecords()
	case text == "/compact":
		if err := c.compact(); err != nil {
			fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		}
	case text == "/help":
		fmt.Fprintln(os.Stderr, "sai: /clear new session · /compact summarize the context · /resume <id> reopen a session · /stop stop the run · /quit exit")
	case strings.HasPrefix(text, "/resume"):
		c.resumeCommand(strings.TrimSpace(strings.TrimPrefix(text, "/resume")))
	default:
		if strings.HasPrefix(text, "/") {
			fmt.Fprintf(os.Stderr, "sai: unknown command %s (try /help)\n", text)
			return false
		}
		if err := c.send(text); err != nil {
			fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		}
	}
	return false
}

// send runs one agent turn. The reply streams character by character via
// the live writer; the dot spinner marks only the quiet phases (waiting
// for the first token, running tools). Records print as they are
// committed, except assistant messages already rendered live.
func (c *cliApp) send(text string) error {
	if err := c.session.Send(text); err != nil {
		return err
	}

	// The drain goroutine renders the run's transient events; each
	// message's start event marks its ItemID before the record can commit,
	// so the watcher's record print is filtered by identity.
	live := &liveWriter{printer: c.printer}
	events := c.session.Events()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for event := range events {
			if started, ok := event.(model.AssistantMessageStartedEvent); ok {
				c.markLiveShown(started.ItemID)
			}
			// Budget notices are control messages, like every other
			// 'sai:' notice: stderr, not the rendered record stream.
			if pressure, ok := event.(model.ContextPressureEvent); ok {
				fmt.Fprintf(os.Stderr, "sai: context exceeds the estimated budget (%d ≥ %d tokens); no compaction possible, sending anyway\n",
					pressure.EstimatedTokens, pressure.HardInputLimit)
				continue
			}
			// Retry notices land on stderr for the same reason — a silent
			// retry between streaming chunks looks like a hang.
			if retry, ok := event.(model.ProviderRetryEvent); ok {
				fmt.Fprintf(os.Stderr, "sai: retrying model request (attempt %d/%d, %s)\n",
					retry.Attempt, retry.MaxAttempts, retry.Reason)
				continue
			}
			live.handle(event)
		}
		live.settle()
	}()

	frame := 0
	for c.session.Running() {
		time.Sleep(spinnerInterval)
		if live.inMessage.Load() && live.sawOutput.Load() {
			// Text is flowing: the output itself is the progress.
			c.printer.clear()
		} else {
			c.printer.frame(spinnerFrames[frame%len(spinnerFrames)])
			frame++
		}
	}
	c.printer.clear()
	<-drained
	// The skip set is final now: any assistant record parked mid-run
	// (its start event not yet processed when the record arrived) prints
	// here unless it was live-rendered after all.
	c.reconcileParked()
	// The next prompt must not appear before the watcher printed the
	// run's records.
	c.waitForRecords()
	return nil
}

// markLiveShown records an assistant message the live writer will
// render; its committed record would duplicate it.
func (c *cliApp) markLiveShown(itemID string) {
	c.liveMu.Lock()
	if c.liveShown == nil {
		c.liveShown = make(map[string]struct{})
	}
	c.liveShown[itemID] = struct{}{}
	c.liveMu.Unlock()
}

// liveShownHas reports whether the item was marked, consuming the mark.
func (c *cliApp) liveShownHas(itemID string) bool {
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	_, shown := c.liveShown[itemID]
	delete(c.liveShown, itemID)
	return shown
}

// reconcileParked prints assistant records that were parked mid-run and
// turn out not to have been live-rendered once the event drain finished
// (the skip set is final by then). Normally nothing is parked: the drain
// processes each message's start event long before its record commits.
func (c *cliApp) reconcileParked() {
	c.liveMu.Lock()
	parked := c.parked
	c.parked = nil
	c.liveMu.Unlock()
	for _, record := range parked {
		// Parked records were already counted when they were parked.
		if !c.liveShownHas(record.ItemID) {
			c.printer.line(formatRecord(record))
		}
	}
}

// compact summarizes the session context. The spinner runs while the
// summary generates; the committed record prints through the watcher.
func (c *cliApp) compact() error {
	done := make(chan error, 1)
	go func() { done <- c.session.Compact() }()
	frame := 0
	for {
		select {
		case err := <-done:
			c.printer.clear()
			// The compaction record prints through the watcher; do not let
			// the next prompt overtake it.
			c.waitForRecords()
			return err
		case <-time.After(spinnerInterval):
			c.printer.frame(spinnerFrames[frame%len(spinnerFrames)])
			frame++
		}
	}
}

// printRecord renders one committed record. A user input opens a new run:
// the blank line keeps runs visually separated. An assistant message
// rendered live by the event drain is skipped; one arriving mid-run
// before its start event was processed is parked and decided at
// reconcileParked.
func (c *cliApp) printRecord(record storage.Record) {
	if record.Type == storage.TypeAssistantMessage {
		if c.liveShownHas(record.ItemID) {
			return
		}
		if c.session.Running() {
			c.liveMu.Lock()
			c.parked = append(c.parked, record)
			c.liveMu.Unlock()
			return
		}
	}
	if record.Type == storage.TypeUserInput {
		c.printer.line("")
	}
	c.printer.line(formatRecord(record))
}

// stopWatcher stops the record-printing watcher without closing the
// session — serve mode hands terminal output over to the web UI (/tail
// re-enables it on demand).
func (c *cliApp) stopWatcher() {
	if c.unwatch != nil {
		c.unwatch()
		c.unwatch = nil
	}
	if c.watcherDone != nil {
		<-c.watcherDone
		c.watcherDone = nil
	}
}

// closeSession stops the watcher and closes the session.
func (c *cliApp) closeSession() {
	if c.unwatch != nil {
		c.unwatch()
		c.unwatch = nil
	}
	// Join the watcher goroutine before returning: after Stop it may
	// still process buffered records, and those must not leak past the
	// counter reset in bind (a late countPrinted would inflate the new
	// session's printed; a late printRecord would render old-session
	// records into the new one).
	if c.watcherDone != nil {
		<-c.watcherDone
		c.watcherDone = nil
	}
	c.session.Close()
}

// freshSession switches to a new empty session.
func (c *cliApp) freshSession() error {
	session, err := c.agent.NewSession()
	if err != nil {
		return err
	}
	return c.bind(session)
}

// resume reopens a stored session.
func (c *cliApp) resume(id string) error {
	session, err := c.agent.OpenSession(id)
	if err != nil {
		return err
	}
	return c.bind(session)
}

// resumeCommand implements /resume: it requires a session id.
func (c *cliApp) resumeCommand(arg string) {
	if arg == "" {
		fmt.Fprintln(os.Stderr, "sai: usage: /resume <session-id>")
		return
	}
	if err := c.resume(arg); err != nil {
		fmt.Fprintf(os.Stderr, "sai: %v\n", err)
	}
}

// bind switches to a session: existing records print first, then every
// later commit prints as it happens. The session id prints immediately —
// it exists from creation, before anything is written to disk.
func (c *cliApp) bind(session *agent.Session) error {
	if c.session != nil {
		if c.session.Running() {
			return fmt.Errorf("the current session is still running; /stop it first")
		}
		c.closeSession()
	}
	// Reset the cross-session bookkeeping: printed counts toward the new
	// session's records (a stale counter would make waitForRecords a
	// permanent no-op), and the live-render markers of the old session
	// are meaningless here.
	c.liveMu.Lock()
	c.liveShown = nil
	c.parked = nil
	c.liveMu.Unlock()
	c.printedMu.Lock()
	c.printed = 0
	c.printedMu.Unlock()
	c.session = session
	sub := session.Subscribe()
	c.unwatch = sub.Stop
	watcherDone := make(chan struct{})
	c.watcherDone = watcherDone
	go func() {
		defer close(watcherDone)
		for {
			select {
			case record := <-sub.Records():
				c.printRecord(record)
				c.countPrinted()
			case <-sub.Done():
				return
			}
		}
	}()
	fmt.Fprintf(os.Stderr, "sai: session %s\n", session.ID())
	// History shows only the tail: the full log stays in the file; a
	// resumed session needs the recent context, not a wall of scroll-back.
	// The omitted records count as processed — waitForRecords compares
	// against the session's full committed count, and every omitted record
	// would otherwise be waited for until the timeout.
	initial := sub.Initial()
	if len(initial) > resumeTailRecords {
		omitted := len(initial) - resumeTailRecords
		c.printer.line(fmt.Sprintf("[… showing last %d of %d records]", resumeTailRecords, len(initial)))
		initial = initial[len(initial)-resumeTailRecords:]
		c.printedMu.Lock()
		c.printed += omitted
		c.printedMu.Unlock()
	}
	for _, record := range initial {
		c.printRecord(record)
		c.countPrinted()
	}
	return nil
}

// resolveServerRoot picks the configuration namespace root: an explicit
// --server-root, then the SAI_SERVER_ROOT environment variable, and finally
// the user configuration directory under the app name.
func resolveServerRoot() (string, error) {
	if fromEnv := strings.TrimSpace(os.Getenv("SAI_SERVER_ROOT")); fromEnv != "" {
		return fromEnv, nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(configDir, appName()), nil
}

// appName derives the application name from the executable's base name:
// everything before the first "-", extension removed. dist/sai.exe and
// dist/sai-windows-amd64.exe both yield "sai", so a renamed executable
// (myagent-v2.exe → "myagent") switches to its own configuration
// namespace without any flags.
func appName() string {
	name := filepath.Base(os.Args[0])
	name = strings.TrimSuffix(name, filepath.Ext(name))
	if index := strings.Index(name, "-"); index > 0 {
		name = name[:index]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "sai"
	}
	return name
}

// resolvedServerRoot is the server root after flag/env/default resolution,
// shared with the project registry subcommands.
var resolvedServerRoot string

// Store exposes the shared storage (the serve-mode manager needs it for
// listing and scoped opens).
func (f *agentFactory) Store() *jsonl.Store { return f.store }
