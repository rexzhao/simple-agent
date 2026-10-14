package main

// /new parameter override tests (B1 step 4, §15.4 / Q2): bare /new
// records the startup default, overrides merge over the STARTUP
// values (never a previous /new's), invalid input keeps the current
// session, and the real entrypoint writes the override to disk.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage"
	"github.com/rexzhao/simple-agent/storage/jsonl"
)

// newTestConfig builds a two-model configuration for the /new tests:
// provider "fake" with fake-model (no reasoning config) and
// fake-reasoner (with low/high tiers); provider "other" with
// other-model only — enough surface for every illegal-input class.
func newTestConfig() *config.Config {
	return &config.Config{
		DefaultProvider: "fake",
		DefaultModel:    "fake-model",
		Providers: map[string]config.ProviderConfig{
			"fake": {
				Name:    "fake",
				BaseURL: "http://127.0.0.1:1",
				APIKey:  "test",
				Models: map[string]config.ModelProfile{
					"fake-model": {ID: "fake-model", Type: config.ProviderTypeOpenAIChat},
					"fake-reasoner": {ID: "fake-reasoner", Type: config.ProviderTypeOpenAIChat,
						ReasoningConfig: config.ReasoningConfig{
							Parameter: "reasoning_effort",
							Default:   "low",
							Levels:    map[string]any{"low": "low", "high": "high"},
						}},
				},
			},
			"other": {
				Name:    "other",
				BaseURL: "http://127.0.0.1:1",
				APIKey:  "test",
				Models: map[string]config.ModelProfile{
					"other-model": {ID: "other-model", Type: config.ProviderTypeOpenAIChat},
				},
			},
		},
	}
}

// recordingResolverProvider records the model each provider
// construction resolved to — the reopen assertions read it.
type newCmdProvider struct {
	mu     sync.Mutex
	models []string
}

func (p *newCmdProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	events := make(chan model.Event, 2)
	go func() {
		defer close(events)
		events <- model.TextDeltaEvent{Text: "ok"}
	}()
	return events, nil
}

// newCmdWorld assembles a cliApp over a jsonl store with the
// production config resolver wired as the /new validator.
func newCmdWorld(t *testing.T, defaultMeta storage.SessionMeta) (*cliApp, *newCmdProvider, *config.Config, *jsonl.Store, string) {
	t.Helper()
	root := t.TempDir()
	projectDir := t.TempDir()
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := newTestConfig()
	provider := &newCmdProvider{}
	resolver, err := config.NewModelResolver(cfg, func(config.ResolvedModel) (model.Provider, error) {
		return provider, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agent.Options{
		ModelResolver: resolver,
		Storage:       store,
		CWD:           projectDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	cli, err := setupCLI(ag, defaultMeta, newMetaValidator(cfg), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cli.closeSession)
	return cli, provider, cfg, store, projectDir
}

// createdOf reads the session.created record of the CLI's current
// session.
func createdOf(t *testing.T, cli *cliApp) storage.Record {
	t.Helper()
	// The created record is pending until the first append; committing
	// it is what "writes session.created" means on disk.
	if err := cli.session.MaterializeHeader(); err != nil {
		t.Fatal(err)
	}
	records := cli.session.SnapshotStream().Records
	for _, r := range records {
		if r.Type == storage.TypeSessionCreated {
			return r
		}
	}
	t.Fatal("no session.created record")
	return storage.Record{}
}

// (a) Bare /new records the startup default meta.
func TestNewBareRecordsStartupDefault(t *testing.T) {
	def := storage.SessionMeta{Provider: "fake", Model: "fake-model"}
	cli, _, _, _, _ := newCmdWorld(t, def)
	if cli.dispatch("/new") {
		t.Fatal("bare /new asked the REPL to exit")
	}
	created := createdOf(t, cli)
	if created.Provider != "fake" || created.Model != "fake-model" || created.ReasoningLevel != "" {
		t.Fatalf("bare /new created = %+v, want the startup default %+v", created, def)
	}
}

// (b) Each override lands in the created record (the other fields
// inherit the startup values), and a reopen resolves the same model.
func TestNewOverridesRecordedAndReopenResolves(t *testing.T) {
	def := storage.SessionMeta{Provider: "fake", Model: "fake-model"}
	for _, tc := range []struct {
		name string
		line string
		want storage.Record
	}{
		{"provider", "/new -provider other -model other-model", storage.Record{Provider: "other", Model: "other-model"}},
		{"model", "/new -model fake-reasoner", storage.Record{Provider: "fake", Model: "fake-reasoner"}},
		{"reasoning", "/new -model fake-reasoner -reasoning high", storage.Record{Provider: "fake", Model: "fake-reasoner", ReasoningLevel: "high"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, _, _, store, projectDir := newCmdWorld(t, def)
			if cli.dispatch(tc.line) {
				t.Fatalf("%s asked the REPL to exit", tc.line)
			}
			created := createdOf(t, cli)
			if created.Provider != tc.want.Provider || created.Model != tc.want.Model ||
				created.ReasoningLevel != tc.want.ReasoningLevel {
				t.Fatalf("created = %+v, want %+v", created, tc.want)
			}
			// The override survives a reopen: close, reopen through a
			// fresh agent with the same config — the created record is
			// what resolves (step 2's mechanism), and it must resolve
			// (no ErrModelUnavailable).
			id := cli.session.ID()
			cli.closeSession()
			resolver, err := config.NewModelResolver(newTestConfig(), func(config.ResolvedModel) (model.Provider, error) {
				return &newCmdProvider{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			ag, err := agent.New(agent.Options{
				ModelResolver: resolver,
				Storage:       store,
				CWD:           projectDir,
			})
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := ag.OpenSession(projects.ProjectID(projectDir), id)
			if err != nil {
				t.Fatalf("reopen after %s: %v (the recorded meta must resolve)", tc.line, err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// (c) After an override, bare /new returns to the STARTUP default —
// overrides are never sticky.
func TestNewBareAfterOverrideReturnsToDefault(t *testing.T) {
	def := storage.SessionMeta{Provider: "fake", Model: "fake-model"}
	cli, _, _, _, _ := newCmdWorld(t, def)
	if cli.dispatch("/new -model fake-reasoner -reasoning high") {
		t.Fatal("override /new asked the REPL to exit")
	}
	if cli.dispatch("/new") {
		t.Fatal("bare /new asked the REPL to exit")
	}
	created := createdOf(t, cli)
	if created.Provider != "fake" || created.Model != "fake-model" || created.ReasoningLevel != "" {
		t.Fatalf("bare /new after override created = %+v, want the startup default %+v (sticky override?)", created, def)
	}

	// The non-bare form inherits from the STARTUP defaults too, never
	// from the previous /new: after reasoning=high, a /new overriding
	// only the model gets the startup reasoning (empty). Direct call:
	// a sticky implementation fails VALIDATION here (fake-model takes
	// no levels) and the error must surface, not vanish.
	if err := cli.newCommand("-model fake-model"); err != nil {
		t.Fatalf("omitted -reasoning inherited the previous /new's value: %v", err)
	}
	created = createdOf(t, cli)
	if created.ReasoningLevel != "" {
		t.Fatalf("omitted -reasoning kept the previous /new's value %q — overrides are sticky", created.ReasoningLevel)
	}
}

// (d) Every illegal-input class: an error, no new session (the id is
// unchanged, nothing new on disk), and the current session still
// works.
func TestNewInvalidInputKeepsSession(t *testing.T) {
	def := storage.SessionMeta{Provider: "fake", Model: "fake-model"}
	for _, tc := range []struct {
		line string
		want string
	}{
		{"/new -bogus", "flag provided but not defined"},
		{"/new -model", "flag needs an argument"}, // missing value
		{"/new extra", "unexpected argument"},
		{"/new -provider nope", "unknown provider"},    // lists the choices
		{"/new -provider other", "available models"},   // inherited fake-model is not under other — listed, not auto-picked
		{"/new -model no-such-model", "unknown model"}, // lists the choices
		{"/new -model fake-reasoner -reasoning ultra", "reasoning"},
		{"/new -reasoning high", "takes no reasoning levels"}, // fake-model has no tiers
	} {
		t.Run(tc.line, func(t *testing.T) {
			cli, _, _, store, projectDir := newCmdWorld(t, def)
			before := cli.session.ID()
			nsDir := filepath.Join(store.Root(), projects.ProjectID(projectDir))
			if err := os.MkdirAll(nsDir, 0o755); err != nil {
				t.Fatal(err)
			}
			dirsBefore, err := os.ReadDir(nsDir)
			if err != nil {
				t.Fatal(err)
			}
			if cli.dispatch(tc.line) {
				t.Fatalf("%s asked the REPL to exit", tc.line)
			}
			if cli.session.ID() == before {
				// good — but the error must have printed; the dispatch
				// prints to stderr, so assert the validation path
				// directly for the message.
				err := cli.newCommand(strings.TrimSpace(strings.TrimPrefix(tc.line, "/new")))
				if err == nil {
					t.Fatalf("%s: no error", tc.line)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%s: error %q does not mention %q", tc.line, err.Error(), tc.want)
				}
			} else {
				t.Fatalf("%s: switched to a new session despite invalid input", tc.line)
			}
			// Nothing new on disk.
			dirsAfter, err := os.ReadDir(nsDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(dirsAfter) != len(dirsBefore) {
				t.Fatalf("%s: disk grew from %d to %d session dirs", tc.line, len(dirsBefore), len(dirsAfter))
			}
			// The current session still works (a turn runs to done).
			if err := cli.session.Send("still here"); err != nil {
				t.Fatalf("%s: current session unusable: %v", tc.line, err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for cli.session.Running() && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if cli.session.Running() {
				t.Fatalf("%s: current session run never settled", tc.line)
			}
		})
	}
}

// (e) /newfoo is NOT /new: it lands in unknown-command territory and
// never switches the session.
func TestNewfooIsNotNew(t *testing.T) {
	def := storage.SessionMeta{Provider: "fake", Model: "fake-model"}
	cli, _, _, _, _ := newCmdWorld(t, def)
	before := cli.session.ID()
	// Capture stderr: the assertion must distinguish "routed to
	// unknown-command" from "routed to /new and rejected by the
	// argument parser" — a widened prefix match takes the second
	// path and still never switches the session, which is NOT the
	// contract (exact command matching, contract line 6).
	stderr := captureStderr(t, func() {
		if cli.dispatch("/newfoo") {
			t.Fatal("/newfoo asked the REPL to exit")
		}
	})
	if !strings.Contains(stderr, "unknown command /newfoo") {
		t.Fatalf("/newfoo did not land in unknown-command: stderr = %q", stderr)
	}
	if strings.Contains(stderr, "/new:") {
		t.Fatalf("/newfoo was routed into the /new argument parser (prefix match too wide): stderr = %q", stderr)
	}
	if cli.session.ID() != before {
		t.Fatal("/newfoo switched the session — prefix matching is wrong")
	}
}

// captureStderr redirects os.Stderr around fn and returns what was
// written (the CLI prints its command errors there).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// (f) The real entrypoint: /new -model <other model> through the
// built binary, then read the created record from disk. The loopback
// stub answers one turn (an httptest server on 127.0.0.1 — no
// external network).
func TestCLIEntrypointNewOverride(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess build is slow")
	}
	root := t.TempDir()
	projectDir := t.TempDir()
	cfg := `default_provider: fake
default_model: fake-model
`
	if err := os.WriteFile(filepath.Join(root, "sai.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	// A local SSE server answering one valid chat-completion turn:
	// the input commits the created record to disk and the run settles
	// instantly (a failing endpoint would retry 5× with long backoff,
	// and the dead-port form takes ~25s per connect on this machine —
	// everything stays on loopback either way).
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(modelServer.Close)
	provider := `name: fake
base_url: ` + modelServer.URL + `
api_key: test
models:
  fake-model:
    id: fake-model
    type: openai-chat
  fake-reasoner:
    id: fake-reasoner
    type: openai-chat
`
	if err := os.MkdirAll(filepath.Join(root, "providers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "providers", "fake.yaml"), []byte(provider), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(t.TempDir(), "sai-test.exe")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build sai: %v\n%s", err, out)
	}

	cmd := exec.Command(bin, "--server-root", root)
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(), "SAI_SERVER_ROOT="+root)
	// One input line commits session.created + user.input to disk; the
	// local loopback SSE server answers the turn (no external
	// network — the stub is an httptest server on 127.0.0.1).
	cmd.Stdin = strings.NewReader("/new -model fake-reasoner\nhi\n/quit\n")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}

	// The exit line names the persisted session; read its created
	// record from disk and pin the model override.
	out := stderr.String()
	idx := strings.LastIndex(out, "sai: session ")
	if idx < 0 {
		t.Fatalf("exit output does not name the session: %s", out)
	}
	rest := strings.TrimSpace(out[idx+len("sai: session "):])
	id := strings.SplitN(rest, "\n", 2)[0]
	logPath := filepath.Join(root, "data", projects.ProjectID(projectDir), id, "log.jsonl")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read created record: %v", err)
	}
	first := strings.SplitN(string(data), "\n", 2)[0]
	var record struct {
		Type     string `json:"t"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.Unmarshal([]byte(first), &record); err != nil {
		t.Fatalf("decode created record %q: %v", first, err)
	}
	if record.Type != "session.created" {
		t.Fatalf("first record is %q, want session.created", record.Type)
	}
	if record.Provider != "fake" || record.Model != "fake-reasoner" {
		t.Fatalf("created = %s/%s, want fake/fake-reasoner", record.Provider, record.Model)
	}

	// The ILLEGAL override through the same real entrypoint: a
	// reasoning level for a model that takes none. Only the CLI's
	// validator blocks this class (the resolver would accept it —
	// ApplyReasoningLevel no-ops without reasoning config), so this
	// scenario is the one that catches the validator WIRING being
	// dropped: unwired, the session is created and the invalid
	// reasoning_level lands in the created record.
	nsDir := filepath.Join(root, "data", projects.ProjectID(projectDir))
	dirsBefore, err := os.ReadDir(nsDir)
	if err != nil {
		t.Fatal(err)
	}
	bad := exec.Command(bin, "--server-root", root)
	bad.Dir = projectDir
	bad.Env = append(os.Environ(), "SAI_SERVER_ROOT="+root)
	bad.Stdin = strings.NewReader("/new -reasoning high\n/quit\n")
	var badErr strings.Builder
	bad.Stderr = &badErr
	if err := bad.Run(); err != nil {
		t.Fatalf("illegal /new run: %v\nstderr: %s", err, badErr.String())
	}
	badOut := badErr.String()
	if !strings.Contains(badOut, "takes no reasoning levels") {
		t.Fatalf("illegal /new lacks the user-facing message: %s", badOut)
	}
	if strings.Contains(badOut, "not available in the configuration") {
		t.Fatalf("illegal /new surfaced the internal resolver text: %s", badOut)
	}
	dirsAfter, err := os.ReadDir(nsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirsAfter) != len(dirsBefore) {
		t.Fatalf("illegal /new created a session on disk: %d -> %d dirs", len(dirsBefore), len(dirsAfter))
	}
}

// NewSession's failure path leaves nothing on disk: the jsonl store's
// Create is lazy (an in-memory handle; directories appear only on the
// first append), so a meta that fails resolution never creates a
// directory or lock.
func TestNewSessionFailureLeavesNoDirectory(t *testing.T) {
	root := t.TempDir()
	projectDir := t.TempDir()
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := config.NewModelResolver(newTestConfig(), func(config.ResolvedModel) (model.Provider, error) {
		return &newCmdProvider{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agent.Options{ModelResolver: resolver, Storage: store, CWD: projectDir})
	if err != nil {
		t.Fatal(err)
	}
	nsDir := filepath.Join(store.Root(), projects.ProjectID(projectDir))
	if err := os.MkdirAll(nsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(nsDir)
	if err != nil {
		t.Fatal(err)
	}
	// An unresolvable meta: NewSession must fail with the typed error.
	_, err = ag.NewSession(projects.ProjectID(projectDir), storage.SessionMeta{Provider: "fake", Model: "no-such-model"})
	if !errors.Is(err, agent.ErrModelUnavailable) {
		t.Fatalf("NewSession with unresolvable meta: %v, want ErrModelUnavailable", err)
	}
	after, err := os.ReadDir(nsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("failed NewSession left a directory: %d → %d entries", len(before), len(after))
	}
}
