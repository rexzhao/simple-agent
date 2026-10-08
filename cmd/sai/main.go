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

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		serverRoot  = flag.String("server-root", "", "configuration namespace root (default: $SAI_SERVER_ROOT, else user config dir/<app>)")
		sessionID   = flag.String("session", "", "resume a stored session by id")
		provider    = flag.String("provider", "", "override default_provider")
		modelName   = flag.String("model", "", "override default_model (model profile name)")
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

	ag, project, err := assemble(*serverRoot, *provider, *modelName)
	if err != nil {
		log.Fatalf("sai: %v", err)
	}
	// The root can be discovered from the environment or the working
	// directory; say which one won so silent discovery never becomes silent
	// configuration drift.
	fmt.Fprintf(os.Stderr, "sai: server root %s, project %s\n", *serverRoot, project)

	cli := &cliApp{agent: ag, printer: &spinnerPrinter{}}
	// A -session flag (or an optional positional argument) resumes a
	// stored session directly; a session which cannot be found is fatal.
	resumeID := strings.TrimSpace(*sessionID)
	if resumeID == "" && flag.NArg() > 0 {
		resumeID = strings.TrimSpace(flag.Arg(0))
	}
	if resumeID != "" {
		if err := cli.resume(resumeID); err != nil {
			log.Fatalf("sai: %v", err)
		}
	} else if err := cli.freshSession(); err != nil {
		log.Fatalf("sai: %v", err)
	}

	// Ctrl+C stops the active run; a second Ctrl+C when idle exits.
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

	fmt.Fprintln(os.Stderr, "sai: /clear /resume <id> /stop /help /quit")
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

// assemble performs the one-time startup assembly: load the config,
// resolve the model, build the provider, tools, and storage, and construct
// the agent with everything injected.
func assemble(serverRoot, providerName, modelName string) (*agent.Agent, string, error) {
	configPath := filepath.Join(serverRoot, "sai.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, "", fmt.Errorf("load %s: %w", configPath, err)
	}
	resolved, err := cfg.ResolveModel(providerName, modelName)
	if err != nil {
		return nil, "", err
	}
	provider, err := newProvider(resolved)
	if err != nil {
		return nil, "", err
	}

	cwd, err := filepath.Abs(".")
	if err != nil {
		return nil, "", fmt.Errorf("resolve working directory: %w", err)
	}
	parameters, err := config.ApplyReasoningLevel(resolved.Parameters, resolved.ReasoningConfig, "")
	if err != nil {
		return nil, "", fmt.Errorf("apply reasoning level: %w", err)
	}
	developerRole := model.MessageRoleDeveloper
	if strings.TrimSpace(resolved.DeveloperRole) != "" {
		developerRole = model.MessageRole(resolved.DeveloperRole)
	}

	store, err := jsonl.OpenStore(filepath.Join(serverRoot, "data"))
	if err != nil {
		return nil, "", err
	}
	project := jsonl.ProjectID(cwd)

	shell, executor, err := tools.Shell(cwd)
	if err != nil {
		return nil, "", fmt.Errorf("register tools: %w", err)
	}
	systemPrompt, err := buildSystemPrompt(cfg, cwd)
	if err != nil {
		return nil, "", err
	}

	ag, err := agent.New(agent.Options{
		Provider:      provider,
		ProviderName:  resolved.ProviderName,
		ModelID:       resolved.ModelID,
		Storage:       store,
		Tools:         []model.Tool{shell},
		ToolExecutor:  namedExecutor{exec: executor},
		SystemPrompt:  systemPrompt,
		DeveloperRole: developerRole,
		Parameters:    parameters,
		MaxTokens:     resolved.OutputLimit,
		MaxIterations: cfg.Agent.MaxIterations,
		ProjectID:     project,
		CWD:           cwd,
	})
	if err != nil {
		return nil, "", err
	}
	return ag, project, nil
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
	agent   *agent.Agent
	session *agent.Session
	printer *spinnerPrinter
	unwatch func()

	// liveMu guards liveShown and parked: assistant messages rendered
	// live by the event drain (their committed records must not print
	// twice) and records parked pending that decision.
	liveMu   sync.Mutex
	liveShown map[string]struct{}
	parked   []storage.Record
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
		c.session.Stop()
		fmt.Fprintln(os.Stderr, "sai: stopping run")
	case text == "/help":
		fmt.Fprintln(os.Stderr, "sai: /clear new session · /resume <id> reopen a stored session · /stop stop the run · /quit exit")
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
		if !c.liveShownHas(record.ItemID) {
			c.printer.line(formatRecord(record))
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

// closeSession stops the watcher and closes the session.
func (c *cliApp) closeSession() {
	if c.unwatch != nil {
		c.unwatch()
		c.unwatch = nil
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
	c.session = session
	sub := session.Subscribe()
	c.unwatch = sub.Stop
	go func() {
		for {
			select {
			case record := <-sub.Records():
				c.printRecord(record)
			case <-sub.Done():
				return
			}
		}
	}()
	fmt.Fprintf(os.Stderr, "sai: session %s\n", session.ID())
	for _, record := range sub.Initial() {
		c.printRecord(record)
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
