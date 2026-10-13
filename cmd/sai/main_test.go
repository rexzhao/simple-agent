package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/model"
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
	session, err := ag.NewSession()
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
	session, err := ag.NewSession()
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
	// Every committed record is accounted for (printed, live-rendered,
	// or caught up from the re-subscription snapshot).
	if n := len(session.SnapshotStream().Records); cli.rendered != n {
		t.Fatalf("rendered %d records, session committed %d — the eviction catch-up lost some", cli.rendered, n)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = oldOut, oldErr
	<-drained
}
