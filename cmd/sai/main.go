// Command sai is the command-line chat interface for the agent. It reads
// input line by line; while a run streams it shows a dot spinner, and when
// the run settles it prints the JSONL records the run appended, one line
// each, to stdout. Control messages (prompts, notices, session lists) go to
// stderr, so stdout is a pure session-record stream.
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
	"time"

	"github.com/rexzhao/simple-agent/internal/chatapp"
)

var version = "dev"

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		serverRoot  = flag.String("server-root", "", "configuration namespace root (default: $SAI_SERVER_ROOT, else ./.sai/sai.yaml if present, else user config dir/sai)")
		cwd         = flag.String("cwd", "", "working directory for tools (default: process cwd)")
		provider    = flag.String("provider", "", "override default_provider")
		model       = flag.String("model", "", "override default_model (model profile name)")
		fullAccess  = flag.Bool("full-access", false, "let file tools read and write outside the cwd")
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
		FullAccess: *fullAccess,
	})
	if err != nil {
		log.Fatalf("sai: %v", err)
	}
	// The root can be discovered from the environment or the working
	// directory; say which one won so silent discovery never becomes silent
	// configuration drift.
	fmt.Fprintf(os.Stderr, "sai: server root %s\n", *serverRoot)
	if notice := app.ToolNotice(); notice != "" {
		fmt.Fprintf(os.Stderr, "sai: %s\n", notice)
	}

	cli := &cliApp{app: app}
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

// cliApp holds the active session and the sequence of its last printed
// record. Everything runs on the main thread; the SIGINT handler only calls
// session.Stop.
type cliApp struct {
	app     *chatapp.App
	session *chatapp.Session
	lastSeq int64
}

// dispatch handles one input line. It returns true when the REPL should
// exit.
func (c *cliApp) dispatch(text string) bool {
	switch {
	case text == "/quit" || text == "/exit":
		c.session.Close()
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

// send runs one agent turn: spinner while streaming, then the run's JSONL
// records printed to stdout.
func (c *cliApp) send(text string) error {
	if err := c.session.Send(text); err != nil {
		return err
	}
	c.spinWhileRunning()
	c.printNewRecords()
	return nil
}

// spinWhileRunning shows the cycling dot spinner on stderr until the active
// run settles, then erases it.
func (c *cliApp) spinWhileRunning() {
	frames := []string{".", "..", "..."}
	index := 0
	for c.session.Running() {
		fmt.Fprintf(os.Stderr, "\r%s", frames[index%len(frames)])
		index++
		time.Sleep(300 * time.Millisecond)
	}
	fmt.Fprint(os.Stderr, "\r  \r")
}

// printNewRecords writes every record appended since the last print, one
// JSONL line each, in sequence order.
func (c *cliApp) printNewRecords() {
	records, err := c.session.RecordsAfter(c.lastSeq)
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
		fmt.Println(string(line))
		c.lastSeq = record.Seq
	}
}

// freshSession switches to a new empty session.
func (c *cliApp) freshSession() error {
	session, err := c.app.NewSession()
	if err != nil {
		return err
	}
	c.bind(session, false)
	return nil
}

// resume reopens a stored session and prints its existing records first.
func (c *cliApp) resume(id string) error {
	session, err := c.app.ResumeSession(id)
	if err != nil {
		return err
	}
	c.bind(session, true)
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
func (c *cliApp) bind(session *chatapp.Session, printHistory bool) {
	if c.session != nil {
		if c.session.Running() {
			session.Close()
			fmt.Fprintln(os.Stderr, "sai: the current session is still running; /stop it first")
			return
		}
		c.session.Close()
	}
	c.session = session
	c.lastSeq = 0
	if printHistory {
		c.printNewRecords()
	}
}

func displayTitle(title string) string {
	if strings.TrimSpace(title) == "" {
		return "(empty session)"
	}
	return title
}

// resolveServerRoot picks the configuration namespace root: an explicit
// --server-root, then the SAI_SERVER_ROOT environment variable, then a
// local .<app> directory in the current working directory (when it contains
// sai.yaml), and finally the user configuration directory under the app
// name.
func resolveServerRoot() (string, error) {
	if fromEnv := strings.TrimSpace(os.Getenv("SAI_SERVER_ROOT")); fromEnv != "" {
		return fromEnv, nil
	}
	name := appName()
	if _, err := os.Stat(filepath.Join("."+name, "sai.yaml")); err == nil {
		return "." + name, nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(configDir, name), nil
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
