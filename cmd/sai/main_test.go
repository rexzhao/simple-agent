package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/storage/jsonl"
	"github.com/rexzhao/simple-agent/internal/storage/memory"
)

func TestAppNameFromArgv0(t *testing.T) {
	original := os.Args
	defer func() { os.Args = original }()
	for argv0, want := range map[string]string{
		"sai":                        "sai",
		"sai.exe":                    "sai",
		"dist/sai-windows-amd64.exe": "sai",
		`F:\bin\sai.exe`:             "sai",
		"myagent-v2.exe":             "myagent",
		"plain":                      "plain",
	} {
		os.Args = []string{argv0}
		if got := appName(); got != want {
			t.Errorf("appName(%q) = %q, want %q", argv0, got, want)
		}
	}
	// A leading dash keeps the whole name: the part before it is empty.
	os.Args = []string{"-weird.exe"}
	if got := appName(); got != "-weird" {
		t.Errorf("appName with leading dash = %q", got)
	}
}

// scriptedProvider answers each turn from a fixed script, one event per
// channel slot — enough for byte-level send() tests.
type scriptedProvider struct {
	turn int
}

func (p *scriptedProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	p.turn++
	events := make(chan model.Event, 4)
	go func() {
		defer close(events)
		switch p.turn {
		case 1:
			events <- model.TextDeltaEvent{Text: "hello "}
			events <- model.TextDeltaEvent{Text: "world"}
		default:
			events <- model.TextDeltaEvent{Text: "again"}
		}
		events <- model.MessageDoneEvent{}
	}()
	return events, nil
}

// TestPromptAfterRunOutputByteLevel pins the step-4 CLI contract: the
// prompt never precedes the run's output. Both stdout and stderr are
// redirected onto ONE pipe, so the captured byte order is exactly the
// write order; send() must not return before the run's terminal status
// (its last stream event) has been rendered, and the prompt the REPL
// prints next must land after every output byte of the run. The
// assistant content must also appear exactly once: the live rendering
// shows it, and the committed record is skipped.
func TestPromptAfterRunOutputByteLevel(t *testing.T) {
	ag, err := agent.New(agent.Options{
		Provider:     &scriptedProvider{},
		ProviderName: "test",
		ModelID:      "model-test",
		Storage:      memory.OpenStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession("test-ns")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
	}()

	cli := &cliApp{agent: ag, session: session, printer: &spinnerPrinter{}}
	if err := cli.send("hi"); err != nil {
		t.Fatal(err)
	}
	// What the REPL does the moment dispatch returns:
	fmt.Fprint(os.Stderr, "sai> ")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = oldOut, oldErr

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	captured := string(data)

	// The prompt comes after the run's last output byte.
	promptAt := strings.Index(captured, "sai> ")
	answerAt := strings.Index(captured, "world")
	if answerAt == -1 {
		t.Fatalf("the run's answer never rendered: %q", captured)
	}
	if promptAt == -1 || promptAt < answerAt {
		t.Fatalf("prompt (%d) precedes the run output (%d): %q", promptAt, answerAt, captured)
	}
	// The answer appears exactly once (live-rendered; the record skipped).
	if n := strings.Count(captured, "world"); n != 1 {
		t.Fatalf("answer rendered %d times, want exactly 1 (live render, record skipped): %q", n, captured)
	}
	// The user input record precedes the answer.
	if inputAt := strings.Index(captured, "hi"); inputAt == -1 || inputAt > answerAt {
		t.Fatalf("user input record missing or after the answer: %q", captured)
	}
}

// noopExecutor answers every tool call instantly — the tool loop's
// record volume is what matters here, not the tool itself.
type noopExecutor struct{}

func (noopExecutor) Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error) {
	return model.ToolResult{Content: "ok"}, nil
}

// toolLoopProvider keeps the run in the tool loop for N rounds, then
// answers with text. Each round commits an assistant.message and a
// tool.result record.
type toolLoopProvider struct {
	rounds int
	turn   int
}

func (p *toolLoopProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	p.turn++
	events := make(chan model.Event, 4)
	go func() {
		defer close(events)
		if p.turn <= p.rounds {
			events <- model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "c", Name: "noop", Arguments: "{}"}}
		} else {
			events <- model.TextDeltaEvent{Text: "done"}
		}
		events <- model.MessageDoneEvent{}
	}()
	return events, nil
}

// TestSendReturnsAfterEviction pins the Lost contract (§14.2) for the
// CLI: when its stream subscription is evicted (rendering fell behind
// by more than the buffer — e.g. a blocked terminal under a long
// self-serve run), send must STILL return, re-syncing from the fresh
// snapshot's records. Deterministic: the printer's mutex is held so the
// very first render blocks while a 1400+ record run completes and
// overflows the 1024-slot buffer.
func TestSendReturnsAfterEviction(t *testing.T) {
	ag, err := agent.New(agent.Options{
		Provider:     &toolLoopProvider{rounds: 700},
		ProviderName: "test",
		ModelID:      "model-test",
		Storage:      memory.OpenStore(),
		Tools:        []model.Tool{{Name: "noop"}},
		ToolExecutor: noopExecutor{},
		// The default iteration cap (8) would end the run long before
		// the 1024-slot buffer overflows; the eviction needs the full
		// 700-round volume.
		MaxIterations: 750,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession("test-ns")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
	}()
	// Drain continuously: the catch-up alone writes more than a pipe
	// buffer of records, and a full pipe would re-block the renderer.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(io.Discard, r)
	}()

	printer := &spinnerPrinter{}
	cli := &cliApp{agent: ag, printer: printer}
	if err := cli.bind(session); err != nil {
		t.Fatal(err)
	}

	// Freeze ALL rendering: the runLoop blocks inside its first record
	// print while the run itself proceeds (the session never waits for
	// the renderer). 1402 committed records overflow the subscription
	// buffer (1024) with non-droppable events → eviction.
	printer.mu.Lock()
	sent := make(chan error, 1)
	go func() { sent <- cli.send("loop") }()
	// Wait until the run has committed PAST the eviction point: the
	// loop is parked on the printer lock (its very first record print),
	// so the subscription's 1024-slot buffer overflows on
	// non-droppable records — 1100 > 1024 guarantees the eviction.
	deadline := time.Now().Add(30 * time.Second)
	for len(session.SnapshotStream().Records) < 1100 {
		if time.Now().After(deadline) {
			t.Fatalf("the tool-loop run stalled at %d records", len(session.SnapshotStream().Records))
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The subscription is evicted; release the renderer and require
	// send to find its way back (via the Lost case).
	printer.mu.Unlock()

	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("send: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cli.send never returned while its stream subscription was evicted — runLoop has no working Lost case")
	}
	// Every committed record is accounted for: the last rendered item
	// is the session's last committed record.
	committed := session.SnapshotStream().Records
	if last := committed[len(committed)-1]; cli.lastRenderedItem != last.ItemID {
		t.Fatalf("last rendered item %q, session's last record %q — the eviction catch-up lost records", cli.lastRenderedItem, last.ItemID)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = oldOut, oldErr
	<-drained
}

// compactingToolLoopProvider serves a tool loop, then a final answer,
// then a compaction summary. gateAt parks the run mid-loop (after that
// many rendered tool rounds) so a test can freeze the renderer with
// records already behind it — the natural "old anchor truncated away"
// state.
type compactingToolLoopProvider struct {
	rounds  int
	gateAt  int
	gate    chan struct{}
	entered chan struct{}
	turn    int
}

func (p *compactingToolLoopProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	p.turn++
	events := make(chan model.Event, 4)
	go func() {
		defer close(events)
		if p.turn == p.gateAt {
			if p.entered != nil {
				select {
				case <-p.entered:
				default:
					close(p.entered)
				}
			}
			select {
			case <-p.gate:
			case <-ctx.Done():
				return
			}
		}
		switch {
		case p.turn <= p.rounds:
			events <- model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "c", Name: "noop", Arguments: "{}"}}
		case p.turn == p.rounds+1:
			// The tool loop's final answer (the run ends here).
			events <- model.TextDeltaEvent{Text: "the run is done"}
		default:
			// The compaction summary: its content lands verbatim in the
			// context.compacted record the catch-up must re-render.
			events <- model.TextDeltaEvent{Text: "the summary"}
		}
		events <- model.MessageDoneEvent{}
	}()
	return events, nil
}

// TestSendReturnsAfterCompactionAndEviction pins the §14.4 interaction
// opus flagged: the CLI's catch-up must not depend on record indexes.
// The run parks mid-loop after several RENDERED tool rounds (the
// provider gates), the renderer freezes with a real pre-compaction
// record as its last rendered item, the remaining tool rounds evict
// the subscription, and a compaction then TRUNCATES the record stream —
// the last rendered item no longer exists in the snapshot's view. The
// catch-up's miss branch (re-render the whole post-compaction segment,
// the compaction record included — its frame was lost with the
// eviction, nothing else prints it) is the only path that can advance
// the rendered item to the compaction; a count-based resume would
// slice out of range here.
func TestSendReturnsAfterCompactionAndEviction(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	ag, err := agent.New(agent.Options{
		Provider: &compactingToolLoopProvider{
			rounds: 700, gateAt: 50, gate: gate, entered: entered,
		},
		ProviderName:  "test",
		ModelID:       "model-test",
		Storage:       memory.OpenStore(),
		Tools:         []model.Tool{{Name: "noop"}},
		ToolExecutor:  noopExecutor{},
		MaxIterations: 750,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession("test-ns")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
	}()
	captured := make(chan string, 1)
	var readBytes atomic.Int64
	go func() {
		buf := make([]byte, 64*1024)
		var sb strings.Builder
		for {
			n, err := r.Read(buf)
			if n > 0 {
				readBytes.Add(int64(n))
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		captured <- sb.String()
	}()

	printer := &spinnerPrinter{}
	cli := &cliApp{agent: ag, printer: printer}
	if err := cli.bind(session); err != nil {
		t.Fatal(err)
	}
	// The rendered-output baseline, taken BEFORE the run starts: past
	// the bind output, record lines are the only traffic, so a rise
	// proves records rendered (observed through the atomic read-side
	// count — the rendered-item field belongs to the loop goroutine).
	baseline := readBytes.Load()

	// The run renders rounds 1..49 unhindered, then parks at round 50.
	sent := make(chan error, 1)
	go func() { sent <- cli.send("loop") }()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the run never reached its mid-loop gate")
	}
	// With the run parked, let the renderer drain the committed rounds.
	// The precondition is observed through the pipe's read-side byte
	// count (atomic — the rendered-item field is written by the loop
	// goroutine and must not be read from here): past the bind output,
	// record lines are the only traffic.
	renderDeadline := time.Now().Add(30 * time.Second)
	for readBytes.Load() < baseline+500 {
		if time.Now().After(renderDeadline) {
			t.Fatal("precondition: nothing rendered before the freeze")
		}
		time.Sleep(2 * time.Millisecond)
	}
	printer.mu.Lock()
	// Resume the flood: rounds 50..700 commit far past the 1024-slot
	// buffer while the renderer is frozen — eviction.
	close(gate)
	deadline := time.Now().Add(30 * time.Second)
	for len(session.SnapshotStream().Records) < 1100 {
		if time.Now().After(deadline) {
			printer.mu.Unlock()
			t.Fatalf("the tool-loop run stalled at %d records", len(session.SnapshotStream().Records))
		}
		time.Sleep(5 * time.Millisecond)
	}
	settleDeadline := time.Now().Add(30 * time.Second)
	for session.Running() {
		if time.Now().After(settleDeadline) {
			printer.mu.Unlock()
			t.Fatal("the tool-loop run never settled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Compact ON THE SESSION directly (no second CLI loop: its
	// rendering would satisfy the output assertions below regardless of
	// the catch-up under test). The evicted subscription never sees the
	// compacted record's frame.
	if err := session.Compact(); err != nil {
		printer.mu.Unlock()
		t.Fatalf("compact while frozen: %v", err)
	}
	if n := len(session.SnapshotStream().Records); n != 2 {
		printer.mu.Unlock()
		t.Fatalf("post-compaction records = %d, want [created, compacted]", n)
	}
	printer.mu.Unlock()

	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("send: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cli.send never returned after compaction+eviction — the catch-up is index-based (§14.4 break)")
	}
	// Only the miss branch can reach the compaction from a truncated-
	// away anchor: the whole segment re-rendered, rendered item
	// advanced to the compaction.
	committed := session.SnapshotStream().Records
	if last := committed[len(committed)-1]; cli.lastRenderedItem != last.ItemID {
		t.Fatalf("last rendered item %q, want the compaction %q — the catch-up's miss branch did not re-render the segment", cli.lastRenderedItem, last.ItemID)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = oldOut, oldErr
	out := <-captured
	// The catch-up actually re-rendered the frozen gap: the compaction
	// record (lost with the eviction — no other loop printed it) must
	// be on the terminal.
	if !strings.Contains(out, "the summary") {
		t.Fatal("the compaction record never rendered — the catch-up's miss branch printed nothing")
	}
}

// TestNewCommandSwitchesSession pins the /new rename: /new opens a
// fresh session (records reset, ids differ) and /clear is no longer a
// command (the unknown-command path).
func TestNewCommandSwitchesSession(t *testing.T) {
	ag, err := agent.New(agent.Options{
		Provider:     &scriptedProvider{},
		ProviderName: "test",
		ModelID:      "model-test",
		Storage:      memory.OpenStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession("test-ns")
	if err != nil {
		t.Fatal(err)
	}
	printer := &spinnerPrinter{}
	cli := &cliApp{agent: ag, printer: printer}
	if err := cli.bind(session); err != nil {
		t.Fatal(err)
	}
	if err := session.Send("first"); err != nil {
		t.Fatal(err)
	}
	settleDeadline := time.Now().Add(30 * time.Second)
	for session.Running() {
		if time.Now().After(settleDeadline) {
			t.Fatal("the run never settled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	before := session.ID()
	beforeRecords := len(session.SnapshotStream().Records)

	// /new: a fresh session — different id, no records (the old session's
	// teardown is exercised by the closeSession paths, not asserted here).
	if cli.dispatch("/new") {
		t.Fatal("/new asked the REPL to exit")
	}
	if cli.session.ID() == before {
		t.Fatal("/new kept the old session")
	}
	if got := len(cli.session.SnapshotStream().Records); got != 0 {
		t.Fatalf("fresh session has %d records, want 0", got)
	}
	_ = beforeRecords

	// /clear is gone: the unknown-command path (dispatch returns false,
	// the session object unchanged).
	afterNew := cli.session.ID()
	if cli.dispatch("/clear") {
		t.Fatal("/clear asked the REPL to exit")
	}
	if cli.session.ID() != afterNew {
		t.Fatal("/clear was still routed to freshSession")
	}
}

// setupCLIFixture assembles a real jsonl-backed CLI world in temp
// directories: two project namespaces under one root, a session
// materialized in project A, and agents whose CWD points at one of
// them. No real provider or user config is involved.
func setupCLIFixture(t *testing.T, sessionIn string) (*agent.Agent, *agent.Agent, string) {
	t.Helper()
	root := t.TempDir()
	projectA := t.TempDir()
	projectB := t.TempDir()
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	newAgent := func(cwd string) *agent.Agent {
		ag, err := agent.New(agent.Options{
			Provider:     &scriptedProvider{},
			ProviderName: "test",
			ModelID:      "model-test",
			Storage:      store,
			CWD:          cwd,
		})
		if err != nil {
			t.Fatal(err)
		}
		return ag
	}
	if sessionIn == "" {
		return newAgent(projectA), newAgent(projectB), ""
	}
	// Create the session under project A's ns through A's agent.
	agA := newAgent(projectA)
	session, err := agA.NewSession(projects.ProjectID(projectA))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.MaterializeHeader(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	return agA, newAgent(projectB), session.ID()
}

// TestCLIResumeScopedToCurrentProject pins the Q1=B wiring end to end
// through setupCLI (the same code main runs): a session resumes only
// from its own project's directory, and the cross-project failure
// carries the explaining message. Also pins /resume (the command) to
// the same scope, and a wiring regression (ns derived as anything but
// the current project) fails the same-project case.
func TestCLIResumeScopedToCurrentProject(t *testing.T) {
	agA, agB, id := setupCLIFixture(t, "session")

	// Same project: the -session path resumes.
	cli, err := setupCLI(agA, id)
	if err != nil {
		t.Fatalf("same-project resume: %v", err)
	}
	if cli.session == nil || cli.session.ID() != id {
		t.Fatal("setupCLI returned without the resumed session")
	}
	cli.closeSession()

	// Cross project: a clean not-found with the explaining message.
	_, err = setupCLI(agB, id)
	if err == nil {
		t.Fatal("cross-project resume succeeded — Q1=B scoping is broken")
	}
	if !strings.Contains(err.Error(), "can only be resumed from the project directory they belong to") {
		t.Fatalf("cross-project error lacks the hint: %v", err)
	}

	// The /resume command is scoped the same way: same project works.
	cli, err = setupCLI(agA, "")
	if err != nil {
		t.Fatalf("fresh setup: %v", err)
	}
	defer cli.closeSession()
	cli.resumeCommand(id)
	if cli.session == nil || cli.session.ID() != id {
		t.Fatal("/resume same-project did not switch sessions")
	}
	// Cross project through the command: the error goes to stderr and
	// the app still holds its current session (the resumed one).
	cli.resumeCommand("s0000000000000000000000000")
	if cli.session == nil || cli.session.ID() != id {
		t.Fatal("/resume of an unknown session dropped the current session")
	}

	// A fresh session (no resume id) lands in the CURRENT project's ns.
	// The wiring mutant (ns derived from anything but CWD) makes the
	// same-project resume above fail, so this is a positive control.
	fresh, err := setupCLI(agA, "")
	if err != nil {
		t.Fatalf("fresh setup (positive control): %v", err)
	}
	if fresh.ns != projects.ProjectID(agA.CWD()) {
		t.Fatalf("ns = %q, want the current project's namespace", fresh.ns)
	}
	fresh.closeSession()
	_ = agB
}

// TestCLIEntrypointSessionScoping runs the REAL binary (built to a
// temp path) with a fake provider config and temp root: the -session
// and /resume paths are exercised through main itself — no in-process
// shortcut can prove the wiring. A fake model is never called: resume
// only opens the session, and the REPL is fed /quit.
func TestCLIEntrypointSessionScoping(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess build is slow")
	}
	root := t.TempDir()
	projectA := t.TempDir()
	projectB := t.TempDir()
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	nsA := projects.ProjectID(projectA)
	session, err := store.Create(nsA)
	if err != nil {
		t.Fatal(err)
	}
	// Materialize a header so the session exists on disk.
	if err := os.MkdirAll(filepath.Join(root, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	sessDir := filepath.Join(store.Root(), nsA, session.ID())
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	header := `{"item":"h` + strings.Repeat("0", 26) + `","t":"session.created","ts":"2026-01-02T03:04:05Z","provider":"fake","model":"fake-model"}` + "\n"
	if err := os.WriteFile(filepath.Join(sessDir, "log.jsonl"), []byte(header), 0o644); err != nil {
		t.Fatal(err)
	}
	id := session.ID()

	// A config with a fake provider pointing at a dead port: resume
	// never contacts it, but assembly must succeed. The provider lives
	// in provider_dir as its own YAML file (the loader's format).
	cfg := `default_provider: fake
default_model: fake-model
`
	if err := os.WriteFile(filepath.Join(root, "sai.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := `name: fake
base_url: http://127.0.0.1:1
api_key: test
models:
  fake-model:
    id: fake-model
    type: openai-chat
`
	if err := os.MkdirAll(filepath.Join(root, "providers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "providers", "fake.yaml"), []byte(provider), 0o644); err != nil {
		t.Fatal(err)
	}

	// Build the real binary once.
	bin := filepath.Join(t.TempDir(), "sai-test.exe")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."

	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build sai: %v\n%s", err, out)
	}

	run := func(dir string, stdin string, args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "SAI_SERVER_ROOT="+root)
		cmd.Stdin = strings.NewReader(stdin)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stderr.String(), err
	}

	// -session from the owning project: exit 0, the resumed id named on
	// the way out.
	stderr, err := run(projectA, "", "--server-root", root, "-session", id)
	if err != nil {
		t.Fatalf("same-project -session: %v\nstderr: [%s]", err, stderr)
	}
	if !strings.Contains(stderr, "sai: session "+id+"\n") {
		t.Fatalf("same-project exit output does not name the session: %s", stderr)
	}

	// -session from a foreign project: non-zero exit with the hint.
	stderr, err = run(projectB, "", "--server-root", root, "-session", id)
	if err == nil {
		t.Fatalf("cross-project -session succeeded\nstderr: %s", stderr)
	}
	if !strings.Contains(stderr, "can only be resumed from the project directory they belong to") {
		t.Fatalf("cross-project -session lacks the hint: %s", stderr)
	}

	// /resume (the command) from the owning project: switches to the
	// session, names it on exit.
	stderr, err = run(projectA, "/resume "+id+"\n/quit\n", "--server-root", root)
	if err != nil {
		t.Fatalf("/resume same-project: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stderr, "sai: session "+id+"\n") {
		t.Fatalf("/resume same-project did not end on the resumed session: %s", stderr)
	}

	// /resume a foreign id: the app keeps its fresh session (a
	// different id on exit) and prints the hint.
	stderr, err = run(projectB, "/resume "+id+"\n/quit\n", "--server-root", root)
	if err != nil {
		t.Fatalf("/resume cross-project: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stderr, "can only be resumed from the project directory they belong to") {
		t.Fatalf("/resume cross-project lacks the hint: %s", stderr)
	}
	// The foreign session was NOT adopted: the exit line that names the
	// persisted session must not name it (the error message mentions
	// the id, so pin the "sai: session <id>" exit-line form).
	if strings.Contains(stderr, "sai: session "+id+"\n") {
		t.Fatalf("/resume cross-project ended on the foreign session %s: %s", id, stderr)
	}
}
