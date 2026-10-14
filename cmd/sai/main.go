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
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/projects"
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

	ag, project, defaultMeta, validateMeta, err := assembleCurrentDir(*serverRoot, *provider, *modelName)
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

	cli, err := setupCLI(ag, defaultMeta, validateMeta, resumeID)
	if err != nil {
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

	fmt.Fprintln(os.Stderr, "sai: /new /compact /resume <id> /stop /help /quit")
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
	store    *jsonl.Store
	cfg      *config.Config
	resolver agent.ModelResolver
	// defaultMeta is what new sessions record when the host passes no
	// override: the startup -provider/-model (Q2's inheritance base).
	defaultMeta storage.SessionMeta
}

// assembleFactory resolves the project-independent half of the assembly.
func assembleFactory(serverRoot, providerName, modelName string) (*agentFactory, error) {
	configPath := filepath.Join(serverRoot, "sai.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", configPath, err)
	}
	// Validate the startup model early — a bad -provider/-model fails
	// before anything else assembles.
	resolved, err := cfg.ResolveModel(providerName, modelName)
	if err != nil {
		return nil, err
	}
	// The model resolver is the per-session source of truth (§15.2);
	// provider construction moves inside it (cached per name).
	resolver, err := config.NewModelResolver(cfg, newProvider)
	if err != nil {
		return nil, err
	}
	// The store's corruption notices (mid-file discard warnings) ride
	// the process logger — the one place both the CLI and serve see
	// (§15.1: the store never writes stderr directly).
	store, err := jsonl.OpenStoreWithLogger(filepath.Join(serverRoot, "data"), slog.Default())
	if err != nil {
		return nil, err
	}
	return &agentFactory{
		store:    store,
		cfg:      cfg,
		resolver: resolver,
		defaultMeta: storage.SessionMeta{
			Provider: resolved.ProviderName,
			Model:    providerNameOrModel(providerName, modelName, cfg),
		},
	}, nil
}

// providerNameOrModel normalizes the startup model reference for the
// default meta: empty startup names mean the configured defaults.
func providerNameOrModel(providerName, modelName string, cfg *config.Config) string {
	if modelName = strings.TrimSpace(modelName); modelName != "" {
		return modelName
	}
	return cfg.DefaultModel
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
	project := projects.ProjectID(projectCwd)
	ag, err := agent.New(agent.Options{
		Storage:       f.store,
		Tools:         []model.Tool{shell},
		ToolExecutor:  namedExecutor{exec: executor},
		SystemPrompt:  systemPrompt,
		ModelResolver: f.resolver,
		MaxIterations: f.cfg.Agent.MaxIterations,
		CWD:           projectCwd,
		AutoCompact:   f.cfg.Agent.AutoCompact,
	})
	if err != nil {
		return nil, "", err
	}
	return ag, project, nil
}

// assembleCurrentDir is the CLI path: one agent for the process cwd.
// The returned validator is the /new override check (§15.4 step 4):
// provider/model must resolve in the live configuration and the
// reasoning level must be one the chosen model supports.
func assembleCurrentDir(serverRoot, providerName, modelName string) (*agent.Agent, string, storage.SessionMeta, func(storage.SessionMeta) error, error) {
	cwd, err := filepath.Abs(".")
	if err != nil {
		return nil, "", storage.SessionMeta{}, nil, fmt.Errorf("resolve working directory: %w", err)
	}
	factory, err := assembleFactory(serverRoot, providerName, modelName)
	if err != nil {
		return nil, "", storage.SessionMeta{}, nil, err
	}
	ag, project, err := factory.Agent(cwd)
	return ag, project, factory.defaultMeta, newMetaValidator(factory.cfg), err
}

// newMetaValidator builds the /new override check from the live
// configuration. The user is CHOOSING a model here, so the messages
// say which parameter is wrong and what the alternatives are (the
// resolver's texts list them) — not the internal "not available"
// phrasing of the open path.
func newMetaValidator(cfg *config.Config) func(storage.SessionMeta) error {
	return func(meta storage.SessionMeta) error {
		resolved, err := cfg.ResolveModel(meta.Provider, meta.Model)
		if err != nil {
			return err
		}
		if strings.TrimSpace(meta.ReasoningLevel) == "" {
			return nil
		}
		if len(resolved.ReasoningConfig.Levels) == 0 {
			return fmt.Errorf("model %q takes no reasoning levels (drop -reasoning)", meta.Model)
		}
		if _, err := config.ApplyReasoningLevel(resolved.Parameters, resolved.ReasoningConfig, meta.ReasoningLevel); err != nil {
			return fmt.Errorf("-reasoning: %v", err)
		}
		return nil
	}
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
	// defaultMeta is what /new records: the startup -provider/-model
	// (per-command overrides merge over THIS, never over a previous
	// /new's values — bare /new always returns to the startup default).
	defaultMeta storage.SessionMeta
	// validateMeta checks a /new override against the live
	// configuration (unknown provider/model, unsupported reasoning
	// level) BEFORE any session is created. Wired from the factory's
	// config; nil in bare test harnesses (no /new args to validate).
	validateMeta func(storage.SessionMeta) error
	// ns is the storage namespace of the working directory's project —
	// opaque to the agent, derived here by the host (§15.1).
	ns string
	// lastRenderedItem is the ItemID of the last record this app
	// accounted for (printed or live-rendered): the eviction catch-up
	// resumes the snapshot's records after it. Identity, not an index —
	// a compaction truncates the record stream (§14.4), and a count
	// would be out of range or misaligned against the truncated view.
	// Single-threaded by construction — the REPL renders one run at a
	// time.
	lastRenderedItem string
}

// dispatch handles one input line. It returns true when the REPL should
// exit.
func (c *cliApp) dispatch(text string) bool {
	switch {
	case text == "/quit" || text == "/exit":
		c.closeSession()
		return true
	case text == "/new":
		if err := c.freshSession(c.defaultMeta); err != nil {
			fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		}
	case strings.HasPrefix(text, "/new "):
		if err := c.newCommand(strings.TrimSpace(strings.TrimPrefix(text, "/new"))); err != nil {
			fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		}
	case text == "/stop":
		fmt.Fprintln(os.Stderr, "sai: stopping run")
		if r := c.session.CurrentRun(); r != nil {
			r.Stop()
			// Join: the run's teardown (terminal status on the stream,
			// record commits) completes before the prompt returns.
			<-r.Done()
		}
	case text == "/compact":
		if err := c.compact(); err != nil {
			fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		}
	case text == "/help":
		fmt.Fprintln(os.Stderr, "sai: /new [-provider p] [-model m] [-reasoning lvl] new session (omitted items inherit the startup defaults) · /compact summarize the context · /resume <id> reopen a session · /stop stop the run · /quit exit")
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
// for the first token, running tools). Everything renders from the
// session's single ordered stream in one loop — records print as they
// are committed (assistant messages the live writer already rendered
// are skipped), transient events stream or land on stderr as notices.
// The loop ends at the run's terminal Status{Running:false} — its LAST
// stream event — so when send returns, every output byte of the run is
// already on the terminal and the next prompt cannot precede it.
func (c *cliApp) send(text string) error {
	sub := c.session.Stream()
	defer sub.Stop()
	if _, err := c.session.Start(context.Background(), text); err != nil {
		return err
	}
	c.runLoop(sub)
	return nil
}

// runLoop renders one run's stream until its terminal status: records
// print as committed, transient events render live or land on stderr,
// and the spinner marks the quiet phases between events. The terminal
// Status{Running:false} is the run's guaranteed last stream event
// (enqueued in settle, before the run's done closes) — returning after
// it, not merely after Run.Done (which can fire while trailing events
// are still buffered), is what keeps the next prompt after the run's
// last output byte.
func (c *cliApp) runLoop(sub *agent.StreamSubscription) {
	live := &liveWriter{printer: c.printer}
	frame := 0
	// One reused timer (not time.After per iteration): dense deltas
	// would otherwise allocate and discard a timer per event.
	timer := time.NewTimer(spinnerInterval)
	defer timer.Stop()
	for {
		select {
		case ev := <-sub.Events():
			if !timer.Stop() {
				<-timer.C // drain the already-fired tick before Reset
			}
			timer.Reset(spinnerInterval)
			if c.renderEvent(ev, live) {
				live.settle()
				c.printer.clear()
				return
			}
		case <-sub.Lost():
			// The render fell too far behind (a blocked terminal): the
			// dispatcher evicted this subscription. §14.2: re-Stream.
			// The fresh snapshot's records carry the catch-up (the ones
			// committed during the gap); live delta rendering stays off
			// for the rest of the run — the in-flight item's started
			// event is below the fresh startSeq, so later deltas cannot
			// be attributed; the record (non-droppable, re-delivered
			// fresh) prints the full content instead. If the run already
			// settled, the snapshot itself is the terminal state: return
			// without waiting for a terminal status that was lost with
			// the eviction — the loop must never hang.
			fmt.Fprintln(os.Stderr, "\rsai: stream subscription evicted (rendering fell behind); re-syncing from records")
			sub.Stop()
			sub = c.session.Stream()
			snap := sub.Initial()
			live = &liveWriter{printer: c.printer} // no item context: records print in full
			for _, record := range recordsAfter(snap.Records, c.lastRenderedItem) {
				c.printRecord(record, live)
				c.lastRenderedItem = record.ItemID
			}
			if !snap.StatusRunning {
				c.printer.clear()
				return
			}
		case <-timer.C:
			timer.Reset(spinnerInterval)
			if live.inMessage.Load() && live.sawOutput.Load() {
				// Text is flowing: the output itself is the progress.
				c.printer.clear()
			} else {
				c.printer.frame(spinnerFrames[frame%len(spinnerFrames)])
				frame++
			}
		}
	}
}

// renderEvent maps one stream event onto the terminal. It returns true
// for the run's terminal status — the caller's signal that the run and
// its rendering are complete.
func (c *cliApp) renderEvent(ev agent.SessionEvent, live *liveWriter) bool {
	switch e := ev.(type) {
	case agent.RecordCommitted:
		c.printRecord(e.Record, live)
		c.lastRenderedItem = e.Record.ItemID // printed or live-rendered: accounted
	case agent.Transient:
		// Control notices are 'sai:' messages on stderr, not rendered
		// record stream content.
		switch event := e.Event.(type) {
		case model.ContextPressureEvent:
			fmt.Fprintf(os.Stderr, "sai: context exceeds the estimated budget (%d ≥ %d tokens); no compaction possible, sending anyway\n",
				event.EstimatedTokens, event.HardInputLimit)
		case model.ProviderRetryEvent:
			fmt.Fprintf(os.Stderr, "sai: retrying model request (attempt %d/%d, %s)\n",
				event.Attempt, event.MaxAttempts, event.Reason)
		case model.CompactionFailedEvent:
			fmt.Fprintf(os.Stderr, "sai: compaction failed (soft pressure; run continues): %s\n", event.Reason)
		default:
			live.handle(e.Event)
		}
	case agent.Status:
		return !e.Running
	}
	return false
}

// printRecord renders one committed record. A user input opens a new
// run: the blank line keeps runs visually separated. An assistant
// message the live writer is streaming is skipped: its content is
// already on the terminal, and the stream's order (started → deltas →
// record → completed) makes the check race-free — the writer is still
// inside that item when its record arrives.
func (c *cliApp) printRecord(record storage.Record, live *liveWriter) {
	if record.Type == storage.TypeAssistantMessage && record.ItemID == live.currentItem() {
		return
	}
	if record.Type == storage.TypeUserInput {
		c.printer.line("")
	}
	c.printer.line(formatRecord(record))
}

// compact summarizes the session context. The spinner runs while the
// summary generates (runLoop's quiet phase); the context.compacted
// record and the compaction lifecycle events render from the stream,
// and the loop's terminal-status exit means the next prompt follows the
// committed summary record.
func (c *cliApp) compact() error {
	sub := c.session.Stream()
	defer sub.Stop()
	r, err := c.session.StartCompact(context.Background())
	if err != nil {
		return err
	}
	c.runLoop(sub)
	// r.err is written before settle enqueues the terminal status — but
	// reading it before close(done) is still a data race (the loop exits
	// at the terminal status, which precedes the close). Join Done.
	<-r.Done()
	return r.Err()
}

// closeSession closes the session. With the stream-based rendering
// there is no watcher goroutine to join: every run's loop ends at its
// terminal status, so no render is in flight while the REPL is idle.
func (c *cliApp) closeSession() {
	c.session.Close()
}

// freshSession switches to a new empty session recording the given
// meta (the startup default, or a validated /new override).
func (c *cliApp) freshSession(meta storage.SessionMeta) error {
	session, err := c.agent.NewSession(c.ns, meta)
	if err != nil {
		return err
	}
	return c.bind(session)
}

// newCommand handles "/new <args>": parse the flags, merge the given
// values over the STARTUP defaults (omitted items are the startup
// values — never the previous /new's), validate against the live
// configuration, and only then switch sessions. Any error leaves the
// current session untouched.
func (c *cliApp) newCommand(args string) error {
	overrides, err := parseNewArgs(args)
	if err != nil {
		return err
	}
	meta := c.defaultMeta
	if overrides.Provider != "" {
		meta.Provider = overrides.Provider
	}
	if overrides.Model != "" {
		meta.Model = overrides.Model
	}
	if overrides.ReasoningLevel != "" {
		meta.ReasoningLevel = overrides.ReasoningLevel
	}
	if c.validateMeta != nil {
		if err := c.validateMeta(meta); err != nil {
			return fmt.Errorf("/new: %v", err)
		}
	}
	return c.freshSession(meta)
}

// parseNewArgs parses the /new flag arguments. Errors are ours to
// print (the FlagSet stays silent): unknown flags, missing values,
// and stray positional arguments are all usage errors.
func parseNewArgs(args string) (storage.SessionMeta, error) {
	var meta storage.SessionMeta
	fs := flag.NewFlagSet("/new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&meta.Provider, "provider", "", "provider name (default: the startup provider)")
	fs.StringVar(&meta.Model, "model", "", "model name (default: the startup model)")
	fs.StringVar(&meta.ReasoningLevel, "reasoning", "", "reasoning level (default: the model's default)")
	if err := fs.Parse(strings.Fields(args)); err != nil {
		return storage.SessionMeta{}, fmt.Errorf("/new: %v (usage: /new [-provider p] [-model m] [-reasoning lvl])", err)
	}
	if fs.NArg() > 0 {
		return storage.SessionMeta{}, fmt.Errorf("/new: unexpected argument %q (usage: /new [-provider p] [-model m] [-reasoning lvl])", fs.Arg(0))
	}
	return meta, nil
}

// resume reopens a stored session. Q1=B (§15.1): the search is scoped
// to the current directory's project namespace — a session belonging to
// another project is not found here.
func (c *cliApp) resume(id string) error {
	session, err := c.agent.OpenSession(c.ns, id)
	if err != nil {
		if errors.Is(err, storage.ErrSessionNotFound) {
			return fmt.Errorf("session %s not found in this project — sessions can only be resumed from the project directory they belong to", id)
		}
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

// bind switches to a session: existing records print as history; every
// later record prints from its run's stream loop (send/compact). The
// session id prints immediately — it exists from creation, before
// anything is written to disk.
func (c *cliApp) bind(session *agent.Session) error {
	if c.session != nil {
		if c.session.Running() {
			return fmt.Errorf("the current session is still running; /stop it first")
		}
		c.closeSession()
	}
	c.session = session
	fmt.Fprintf(os.Stderr, "sai: session %s\n", session.ID())
	// History shows only the tail: the full log stays in the file; a
	// resumed session needs the recent context, not a wall of scroll-back.
	// The omitted head counts as rendered — the eviction catch-up must
	// never replay it.
	initial := session.SnapshotStream().Records
	if len(initial) > 0 {
		c.lastRenderedItem = initial[len(initial)-1].ItemID
	}
	if len(initial) > resumeTailRecords {
		c.printer.line(fmt.Sprintf("[… showing last %d of %d records]", resumeTailRecords, len(initial)))
		initial = initial[len(initial)-resumeTailRecords:]
	}
	live := &liveWriter{printer: c.printer} // currentItem "" — history records all print
	for _, record := range initial {
		c.printRecord(record, live)
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

// recordsAfter returns the records committed after the given item. A
// missing marker (a compaction truncated the stream since it was
// rendered — §14.4) falls back to the whole view: the catch-up would
// rather re-print the small post-compaction segment than lose a record
// it never saw.
func recordsAfter(records []storage.Record, itemID string) []storage.Record {
	if itemID == "" {
		return records
	}
	for i, r := range records {
		if r.ItemID == itemID {
			return records[i+1:]
		}
	}
	return records
}

// setupCLI assembles the interactive app and resolves the initial
// session (resume or fresh). The namespace is derived HERE, from the
// agent's project cwd — the single place host semantics enter the CLI
// wiring (§15.1: Q1=B scopes -session and /resume to the current
// project's ns), so a wiring regression is testable without invoking
// main itself.
func setupCLI(ag *agent.Agent, defaultMeta storage.SessionMeta, validateMeta func(storage.SessionMeta) error, resumeID string) (*cliApp, error) {
	ns := projects.ProjectID(ag.CWD())
	cli := &cliApp{agent: ag, printer: &spinnerPrinter{}, ns: ns, defaultMeta: defaultMeta, validateMeta: validateMeta}
	if resumeID != "" {
		if err := cli.resume(resumeID); err != nil {
			return nil, err
		}
	} else if err := cli.freshSession(defaultMeta); err != nil {
		return nil, err
	}
	return cli, nil
}
