// Command sai is the command-line chat interface for the agent. Records are
// printed to stdout as soon as they are appended to the session log, one
// JSONL line each. While the model streams, a dot spinner cycles on stderr;
// control messages (prompts, notices, session lists) also go to stderr, so
// stdout stays a pure session-record stream.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rexzhao/simple-agent/internal/chatapp"
	"github.com/rexzhao/simple-agent/internal/model"
)

var version = "dev"

// spinnerFrames cycles ".", "..", "...", and an empty frame: the empty frame
// makes the animation blink rather than crawl.
var spinnerFrames = []string{".", "..", "...", ""}

const spinnerInterval = 250 * time.Millisecond

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		serverRoot  = flag.String("server-root", "", "configuration namespace root (default: $SAI_SERVER_ROOT, else user config dir/<app>)")
		cwd         = flag.String("cwd", "", "working directory for tools (default: process cwd)")
		provider    = flag.String("provider", "", "override default_provider")
		model       = flag.String("model", "", "override default_model (model profile name)")
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

	app, err := chatapp.New(chatapp.Options{
		ServerRoot: *serverRoot,
		CWD:        *cwd,
		Provider:   *provider,
		Model:      *model,
	})
	if err != nil {
		log.Fatalf("sai: %v", err)
	}
	// The root can be discovered from the environment or the working
	// directory; say which one won so silent discovery never becomes silent
	// configuration drift.
	fmt.Fprintf(os.Stderr, "sai: server root %s, project %s (%s)\n", *serverRoot, app.Project(), *cwd)

	cli := &cliApp{app: app, printer: &spinnerPrinter{}}
	// An optional positional argument resumes a stored session directly.
	if flag.NArg() > 0 {
		if err := cli.resume(flag.Arg(0)); err != nil {
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

	fmt.Fprintln(os.Stderr, "sai: /clear /resume [id] /stop /help /quit")
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

// cliApp holds the active session. Record printing and the last printed
// sequence are shared between the main thread and the per-session watcher
// goroutine.
type cliApp struct {
	app     *chatapp.App
	session *chatapp.Session
	printer *spinnerPrinter

	mu      sync.Mutex
	lastSeq int64
	unwatch func()
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
		fmt.Fprintln(os.Stderr, "sai: /clear new session · /resume [id] pick or open a session · /stop stop the run · /quit exit")
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

// send runs one agent turn: records print as they are appended, and the
// spinner shows only while the model is streaming.
func (c *cliApp) send(text string) error {
	if err := c.session.Send(text); err != nil {
		return err
	}

	// Drain the run's transient events to know when the model streams. The
	// assistant message window (started → completed/failed) is exactly the
	// streaming window; waiting for the first token and running tools are
	// silent by design.
	streaming := &atomic.Bool{}
	events := c.session.Events()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for event := range events {
			switch event.(type) {
			case model.AssistantMessageStartedEvent:
				streaming.Store(true)
			case model.AssistantMessageCompletedEvent, model.AssistantMessageFailedEvent:
				streaming.Store(false)
			}
		}
	}()

	frame := 0
	for c.session.Running() {
		time.Sleep(spinnerInterval)
		if streaming.Load() {
			c.printer.frame(spinnerFrames[frame%len(spinnerFrames)])
			frame++
		} else {
			c.printer.clear()
		}
	}
	c.printer.clear()
	<-drained
	// The watcher prints on append notifications; make sure the final
	// records are flushed even if their notification raced the run ending.
	c.tail()
	return nil
}

// tail prints every record appended since the last print, one JSONL line
// each, in sequence order.
func (c *cliApp) tail() {
	c.mu.Lock()
	lastSeq := c.lastSeq
	c.mu.Unlock()
	records, err := c.session.RecordsAfter(lastSeq)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		return
	}
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sai: encode record: %v\n", err)
			continue
		}
		c.printer.line(string(line))
		c.mu.Lock()
		c.lastSeq = record.Seq
		c.mu.Unlock()
	}
}

// watch prints records as they are appended, until the session is switched
// away or closed.
func (c *cliApp) watch() {
	session := c.session
	notify, unsubscribe := session.Subscribe()
	c.unwatch = unsubscribe
	go func() {
		for range notify {
			if c.currentSession() != session {
				return
			}
			c.tail()
		}
	}()
}

func (c *cliApp) currentSession() *chatapp.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
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
	session, err := c.app.NewSession()
	if err != nil {
		return err
	}
	if err := c.bind(session, false); err != nil {
		session.Close()
		return err
	}
	return nil
}

// resume reopens a stored session and prints its existing records first.
func (c *cliApp) resume(id string) error {
	session, err := c.app.ResumeSession(id)
	if err != nil {
		return err
	}
	if err := c.bind(session, true); err != nil {
		session.Close()
		return err
	}
	return nil
}

// resumeCommand implements /resume: without an argument it lists stored
// sessions; with one it opens that session.
func (c *cliApp) resumeCommand(arg string) {
	if arg == "" {
		summaries, err := c.app.ListSessions()
		if err != nil {
			fmt.Fprintf(os.Stderr, "sai: %v\n", err)
			return
		}
		if len(summaries) == 0 {
			fmt.Fprintln(os.Stderr, "sai: no stored sessions")
			return
		}
		for _, summary := range summaries {
			fmt.Fprintf(os.Stderr, "%s  %s  (%d messages)\n", summary.ID, displayTitle(summary.Title), summary.MessageCount)
		}
		return
	}
	if err := c.resume(arg); err != nil {
		fmt.Fprintf(os.Stderr, "sai: %v\n", err)
	}
}

// bind switches to session. When printHistory is set, every stored record is
// printed to stdout first so a resumed session continues the JSONL stream.
func (c *cliApp) bind(session *chatapp.Session, printHistory bool) error {
	if c.session != nil {
		if c.session.Running() {
			return fmt.Errorf("the current session is still running; /stop it first")
		}
		c.closeSession()
	}
	c.mu.Lock()
	c.session = session
	c.lastSeq = 0
	c.mu.Unlock()
	c.watch()
	if printHistory {
		c.tail()
	}
	return nil
}

func displayTitle(title string) string {
	if strings.TrimSpace(title) == "" {
		return "(empty session)"
	}
	return title
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
