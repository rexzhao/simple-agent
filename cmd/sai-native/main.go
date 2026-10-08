// Command sai-native is the minimal Go-native chat interface for the agent.
// It shows the folded session log as a conversation, takes input at the
// bottom, and supports slash commands: /clear starts a new session, /resume
// reopens a stored one, /stop stops the active run.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/rexzhao/simple-agent/internal/chatapp"
	"github.com/rexzhao/simple-agent/internal/sessionlog"
)

var version = "dev"

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		serverRoot   = flag.String("server-root", "", "configuration namespace root (default: $SAI_SERVER_ROOT, else ./.sai/sai.yaml if present, else user config dir/sai)")
		cwd        = flag.String("cwd", "", "working directory for tools (default: process cwd)")
		provider   = flag.String("provider", "", "override default_provider")
		model      = flag.String("model", "", "override default_model (model profile name)")
		fullAccess = flag.Bool("full-access", false, "let file tools read and write outside the cwd")
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

	chat := newChatUI(app)
	if notice := app.ToolNotice(); notice != "" {
		chat.notice = notice
	}
	if err := chat.newSession(); err != nil {
		log.Fatalf("sai: %v", err)
	}

	mygo.App.WhenReady(func() {
		chat.win = mygo.NewWindow(mygo.WindowOptions{
			Title:   "sai",
			Width:   900,
			Height:  680,
			Content: ui.View(chat.view),
		})
		chat.watch()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

// chatUI is the window state. The view reads it on the main thread; the
// background watcher changes it through Window.Update.
type chatUI struct {
	app     *chatapp.App
	session *chatapp.Session
	win     *mygo.Window

	items   []sessionlog.Item
	running bool
	notice  string
	draft   string
	list    ui.ListState

	// /resume picker state.
	showResume bool
	sessions   []sessionlog.Summary
	pickList   ui.ListState
	pickPicked int

	// unwatch ends the current session's background watcher.
	unwatch func()
}

func newChatUI(app *chatapp.App) *chatUI {
	chat := &chatUI{app: app, pickPicked: -1}
	chat.list.FollowEnd = true
	chat.pickList.Selected = &chat.pickPicked
	return chat
}

// view builds the whole window: header, conversation (or the resume picker),
// and the input row.
func (chat *chatUI) view(c *ui.Context) {
	ui.Column(c).Fill().Children(func() {
		chat.header(c)
		if chat.showResume {
			chat.resumePicker(c)
		} else {
			chat.conversation(c)
			chat.inputRow(c)
		}
	})
}

func (chat *chatUI) header(c *ui.Context) {
	ui.Row(c).Padding(10, 14).Gap(10).Children(func() {
		ui.Text(c, "sai").Bold()
		ui.Text(c, chat.session.ID()).TextColor(c.Theme().TextMuted)
		if chat.running {
			ui.Spinner(c)
			ui.Text(c, "running").TextColor(c.Theme().TextMuted)
		}
		if chat.notice != "" {
			ui.Text(c, chat.notice).TextColor(c.Theme().TextMuted)
		}
	})
	ui.Divider(c)
}

func (chat *chatUI) conversation(c *ui.Context) {
	// The row builder and Key closure are also invoked during layout, when
	// mygo re-measures retained rows. A /clear or /resume changes chat.items
	// mid-frame, so a retained index can point past the new slice: guard
	// every index and draw nothing for a row that no longer exists.
	chat.list.Key = func(i int) any {
		if i < 0 || i >= len(chat.items) {
			return nil
		}
		return chat.items[i].ID
	}
	ui.List(c, &chat.list, len(chat.items), func(i int) {
		if i < 0 || i >= len(chat.items) {
			return
		}
		chat.itemRow(c, chat.items[i])
	}).Grow(1).Justify(ui.End).Padding(8, 14).Gap(10).Children(func() {
		if len(chat.items) == 0 {
			ui.Text(c, "Say something, or type /help for commands.").
				TextColor(c.Theme().TextMuted).Padding(12)
		}
	})
	ui.Divider(c)
}

// itemRow renders one folded item. Streaming assistant items get a caret so
// partial output reads as live.
func (chat *chatUI) itemRow(c *ui.Context, item sessionlog.Item) {
	switch item.Kind {
	case sessionlog.ItemKindUser:
		ui.Column(c).Gap(2).Children(func() {
			ui.Text(c, "you").FontSize(11).TextColor(c.Theme().TextMuted)
			ui.Text(c, item.Content)
		})
	case sessionlog.ItemKindAssistant:
		ui.Column(c).Gap(2).Children(func() {
			ui.Text(c, "assistant").FontSize(11).TextColor(c.Theme().TextMuted)
			content := item.Content
			if item.Streaming {
				content += " ▌"
			}
			if content != "" {
				ui.Text(c, content)
			}
			for _, call := range item.ToolCalls {
				ui.Text(c, "⚡ "+call.Name+" "+call.Arguments).FontSize(11).TextColor(c.Theme().TextMuted)
			}
		})
	case sessionlog.ItemKindTool:
		ui.Column(c).Gap(2).Children(func() {
			label := item.ToolName
			if item.Status == sessionlog.ToolStatusError {
				label += " (error)"
			}
			ui.Text(c, label).FontSize(11).TextColor(c.Theme().TextMuted)
			if item.Content != "" {
				ui.Text(c, item.Content).FontSize(11).TextColor(c.Theme().TextMuted)
			}
		})
	case sessionlog.ItemKindError:
		ui.Text(c, "error: "+item.Content).TextColor(c.Theme().Danger)
	default:
		ui.Text(c, item.Content).TextColor(c.Theme().TextMuted)
	}
}

func (chat *chatUI) inputRow(c *ui.Context) {
	ui.Row(c).Padding(10, 14).Gap(8).Children(func() {
		input := ui.TextInput(c, &chat.draft).Placeholder("Message — Enter to send · /clear /resume /stop /help").Grow(1)
		if input.Submitted() {
			chat.submit()
		}
		if chat.running {
			if ui.Button(c, "Stop").Clicked() {
				chat.session.Stop()
			}
		} else {
			if ui.PrimaryButton(c, "Send").Clicked() {
				chat.submit()
			}
		}
	})
}

func (chat *chatUI) resumePicker(c *ui.Context) {
	chat.pickList.Key = func(i int) any {
		if i < 0 || i >= len(chat.sessions) {
			return nil
		}
		return chat.sessions[i].ID
	}
	chat.pickList.Label = func(i int) string {
		if i < 0 || i >= len(chat.sessions) {
			return ""
		}
		return displayTitle(chat.sessions[i].Title)
	}
	picker := ui.List(c, &chat.pickList, len(chat.sessions), func(i int) {
		if i < 0 || i >= len(chat.sessions) {
			return
		}
		summary := chat.sessions[i]
		ui.Column(c).Padding(6, 8).Gap(2).Children(func() {
			ui.Text(c, displayTitle(summary.Title))
			ui.Text(c, fmt.Sprintf("%s  ·  %s  ·  %s  ·  %d messages",
				summary.ID, summary.Provider, summary.Model, summary.MessageCount)).
				FontSize(11).TextColor(c.Theme().TextMuted)
		})
	}).Grow(1).Padding(8, 14).Gap(4).Children(func() {
		if len(chat.sessions) == 0 {
			ui.Text(c, "No stored sessions.").TextColor(c.Theme().TextMuted).Padding(12)
		}
	})
	if picker.Submitted() && chat.pickPicked >= 0 && chat.pickPicked < len(chat.sessions) {
		chat.resumeAt(chat.pickPicked)
	}
	ui.Divider(c)
	ui.Row(c).Padding(10, 14).Gap(8).Children(func() {
		if ui.PrimaryButton(c, "Open").Clicked() && chat.pickPicked >= 0 && chat.pickPicked < len(chat.sessions) {
			chat.resumeAt(chat.pickPicked)
		}
		if ui.Button(c, "Cancel").Clicked() {
			chat.showResume = false
		}
	})
}

func displayTitle(title string) string {
	if strings.TrimSpace(title) == "" {
		return "(empty session)"
	}
	return title
}

// submit handles Enter/Send: slash commands or a message send.
func (chat *chatUI) submit() {
	text := strings.TrimSpace(chat.draft)
	if text == "" {
		return
	}
	chat.draft = ""
	switch {
	case text == "/clear":
		if err := chat.newSession(); err != nil {
			chat.notice = err.Error()
			return
		}
		chat.notice = "new session"
	case text == "/resume":
		sessions, err := chat.app.ListSessions()
		if err != nil {
			chat.notice = err.Error()
			return
		}
		chat.sessions = sessions
		chat.pickPicked = -1
		chat.showResume = true
		chat.notice = ""
	case text == "/stop":
		chat.session.Stop()
		chat.notice = "stopping…"
	case text == "/help":
		chat.notice = "/clear new session · /resume pick a session · /stop stop the run"
	default:
		if strings.HasPrefix(text, "/") {
			chat.notice = "unknown command " + text
			return
		}
		if err := chat.session.Send(text); err != nil {
			chat.notice = err.Error()
			return
		}
		chat.notice = ""
	}
}

// newSession replaces the current session with a fresh one.
func (chat *chatUI) newSession() error {
	session, err := chat.app.NewSession()
	if err != nil {
		return err
	}
	return chat.bindSession(session)
}

// resumeAt reopens the stored session at index.
func (chat *chatUI) resumeAt(index int) {
	if index < 0 || index >= len(chat.sessions) {
		return
	}
	session, err := chat.app.ResumeSession(chat.sessions[index].ID)
	if err != nil {
		chat.notice = err.Error()
		return
	}
	if err := chat.bindSession(session); err != nil {
		session.Close()
		chat.notice = err.Error()
		return
	}
	chat.notice = "resumed " + chat.sessions[index].ID
}

// bindSession switches the UI to session, refreshing the snapshot and
// restarting the background watcher. It refuses to switch away from a session
// with an active run: closing it would block the UI thread for up to the
// pump's settle timeout.
func (chat *chatUI) bindSession(session *chatapp.Session) error {
	if chat.session != nil {
		if chat.session.Running() {
			return fmt.Errorf("the current session is still running; /stop it first")
		}
		if chat.unwatch != nil {
			chat.unwatch()
			chat.unwatch = nil
		}
		chat.session.Close()
	}
	chat.session = session
	chat.refresh()
	chat.showResume = false
	if chat.win != nil {
		chat.watch()
	}
	return nil
}

// refresh pulls the folded state into the view state. It runs on the main
// thread, either inside the view's event handling or from Window.Update.
func (chat *chatUI) refresh() {
	state := chat.session.Snapshot()
	chat.items = state.Items
	chat.running = sessionRunning(state)
}

func sessionRunning(state sessionlog.State) bool {
	for _, status := range state.Runs {
		if status == sessionlog.RunStatusRunning {
			return true
		}
	}
	return false
}

// watch drains the session's append notifications on a background goroutine
// and refreshes the view on the main thread. The goroutine exits when the
// subscription is closed by bindSession's switch. chat.session is only read
// inside the Window.Update callback, which runs on the main thread.
func (chat *chatUI) watch() {
	session := chat.session
	notify, unsubscribe := session.Subscribe()
	chat.unwatch = unsubscribe
	go func() {
		for range notify {
			chat.win.Update(func() {
				if chat.session != session {
					return
				}
				chat.refresh()
			})
		}
	}()
}

// resolveServerRoot picks the configuration namespace root: an explicit
// --server-root, then the SAI_SERVER_ROOT environment variable, then a
// .sai directory in the current working directory (so a checkout can carry
// its own configuration), and finally the user configuration directory.
func resolveServerRoot() (string, error) {
	if fromEnv := strings.TrimSpace(os.Getenv("SAI_SERVER_ROOT")); fromEnv != "" {
		return fromEnv, nil
	}
	if _, err := os.Stat(filepath.Join(".sai", "sai.yaml")); err == nil {
		return ".sai", nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(configDir, "sai"), nil
}
