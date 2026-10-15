package web

// The real-browser UI fixture (C6a-fix): headless Chromium (playwright,
// the client's devDependency) against the BUILT chat dist served by a
// live in-process manager. Asserts the sidebar's new-session entry —
// the C6a acceptance item that a source-level miss dropped — and drives
// the whole create flow. Takes the three smoke screenshots.

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage/jsonl"
	"github.com/rexzhao/simple-agent/tools"
)

// buildChatDistIfStale runs the client's vite build when dist is
// missing or older than any source file. The UI fixture must test
// what a user would be served — a stale dist would green-light old
// code (exactly the C6a lesson).
func buildChatDistIfStale(t *testing.T, chatDir string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; the UI check needs node")
	}
	if _, err := os.Stat(filepath.Join(chatDir, "node_modules")); err != nil {
		t.Skip("clients/chat node_modules absent; run npm install there")
	}
	dist := filepath.Join(chatDir, "dist")
	distIdx := filepath.Join(dist, "index.html")
	distInfo, distErr := os.Stat(distIdx)

	var newest time.Time
	_ = filepath.Walk(filepath.Join(chatDir, "src"), func(path string, info os.FileInfo, err error) error {
		if err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	if distErr == nil && newest.Before(distInfo.ModTime()) {
		return dist // fresh enough
	}
	viteBin := filepath.Join(chatDir, "node_modules", "vite", "bin", "vite.js")
	build := exec.Command(node, viteBin, "build")
	build.Dir = chatDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("vite build: %v\n%s", err, out)
	}
	return dist
}

// newChatUIFixture builds a manager wired like the production serve
// (jsonl store, registry with one project, ModelList for the create
// validation) and serves the chat dist at /chat.
func newChatUIFixture(t *testing.T, dist string) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	projectDir := t.TempDir()
	if err := (&projects.Registry{Projects: []projects.Entry{
		{Path: projectDir, Added: time.Now().UTC()},
	}}).Save(projects.FilePath(root)); err != nil {
		t.Fatal(err)
	}
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DefaultProvider: "fake",
		DefaultModel:    "fake-model",
		Providers: map[string]config.ProviderConfig{
			"fake": {
				Name:    "fake",
				BaseURL: "http://127.0.0.1:1",
				APIKey:  "test",
				Models: map[string]config.ModelProfile{
					"fake-model": {ID: "fake-model", Type: config.ProviderTypeOpenAIChat},
				},
			},
		},
	}
	// A scripted provider (no network): turn 1 is a plain answer (the
	// message materializes the session — the accepted trade-off, §17.8
	// e), turn 2 asks for the echo tool (the approval card), turn 3
	// closes the run after the approval.
	uiProvider := &uiScriptedProvider{}
	resolver, err := config.NewModelResolver(cfg, func(config.ResolvedModel) (model.Provider, error) {
		return uiProvider, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerOptions{
		ServerRoot: root,
		Store:      store,
		AgentFor: func(projectCwd string) (*agent.Agent, error) {
			return agent.New(agent.Options{
				ModelResolver: resolver,
				Storage:       store,
				CWD:           projectCwd,
				// The echo tool + an Ask-always policy: the browser
				// step drives the REAL approval card (the C6c UI
				// surface — logic fixtures cannot prove it renders).
				Tools: []agent.Tool{tools.Named(model.ToolDefinition{Name: "echo"}, tools.ExecutorFunc(func(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
					// Slow on purpose: the resolved card's outcome line is
					// transient (cleared at tool_result) — an instant tool
					// makes it unobservable in the browser.
					select {
					case <-time.After(1500 * time.Millisecond):
					case <-ctx.Done():
					}
					return model.ToolResult{Content: "tool ran"}, nil
				}))},
				Hooks: agent.Hooks{
					BeforeToolCall: func(ctx context.Context, call agent.ToolCallInfo) agent.ToolDecision {
						return agent.ToolDecision{Action: agent.ToolActionAsk}
					},
				},
			})
		},
		ModelList: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	server := New(manager, WithStaticDir(dist))
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(func() { ts.Close(); server.Close() })
	return ts, projects.ProjectID(projectDir)
}

// uiScriptedProvider: turn 1 a plain reply (materializes the
// session), turn 2 a tool call (triggers the approval card), turn 3+
// the post-approval close.
type uiScriptedProvider struct {
	turn int
}

func (p *uiScriptedProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	p.turn++
	events := make(chan model.Event, 8)
	go func() {
		defer close(events)
		switch p.turn {
		case 2:
			events <- model.ToolCallDoneEvent{ToolCall: model.ToolCall{
				ID: "c-smoke", Name: "echo", Arguments: `{"say":"hi"}`,
			}}
		default:
			events <- model.TextDeltaEvent{Text: "hello from the smoke test"}
			events <- model.MessageDoneEvent{}
		}
	}()
	return events, nil
}

func TestSidebarUIFixture(t *testing.T) {
	chatDir, err := filepath.Abs(filepath.Join("..", "..", "clients", "chat"))
	if err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; the UI check needs node")
	}
	if _, err := os.Stat(filepath.Join(chatDir, "node_modules", "playwright")); err != nil {
		t.Skip("playwright not installed in clients/chat; run npm install there")
	}
	dist := buildChatDistIfStale(t, chatDir)

	base, _ := newChatUIFixture(t, dist)

	shots, err := os.MkdirTemp("", "sai-ui-shots-")
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately NOT cleaned: the screenshots are the review
	// artifact (paths go into the submission mail).
	t.Logf("sidebar_check: shots dir %s", shots)

	script := filepath.Join(chatDir, "sidebar-check.mjs")
	cmd := exec.Command(node, script, base.URL, shots)
	cmd.Dir = chatDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Exit 3 = no browser binary (the script's SKIP marker): an
		// environment without the playwright browsers skips rather
		// than fails — CI installs them explicitly.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
			t.Skipf("no browser binary; %s", strings.TrimSpace(string(out)))
		}
		t.Fatalf("sidebar_check.mjs: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "sidebar_check: OK") {
		t.Fatalf("sidebar_check.mjs: unexpected output\n%s", out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}
