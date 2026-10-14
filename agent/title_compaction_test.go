package agent_test

// C1's agent-layer A-form reproductions (§17.1): three REAL
// compaction paths — manual Compact, automatic pre-turn compaction,
// and a reopen-after-compaction append — each verified through the
// storage listing. If any of these goes red while the storage-layer
// test stays green, the title regression lives in the agent's record
// shapes or timing, not in the segment switch or head scan.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage"
	"github.com/rexzhao/simple-agent/storage/jsonl"
)

// titleProvider answers every request with a short text and (for
// compaction requests) a summary.
type titleProvider struct{}

func (p *titleProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	events := make(chan model.Event, 4)
	go func() {
		defer close(events)
		// A compaction request carries the summary prompt — answer
		// with a short summary either way; the regular request gets
		// plain text.
		events <- model.TextDeltaEvent{Text: "answer"}
		events <- model.MessageDoneEvent{}
	}()
	return events, nil
}

// titleEnv builds a real jsonl-backed agent + session over a temp
// root with one registered project.
func titleEnv(t *testing.T) (*agent.Agent, *agent.Session, *jsonl.Store, string) {
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
	resolver := agent.NewStaticResolver(&titleProvider{}, nil, 0, 0, "")
	ag, err := agent.New(agent.Options{
		ModelResolver: resolver,
		Storage:       store,
		CWD:           projectDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	ns := projects.ProjectID(projectDir)
	session, err := ag.NewSession(ns, storage.SessionMeta{Provider: "p", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return ag, session, store, ns
}

func titleOf(t *testing.T, store *jsonl.Store, ns, id string) string {
	t.Helper()
	infos, err := store.ListSessions(ns)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if info.ID == id {
			return info.Title
		}
	}
	t.Fatalf("session %s not found in listing", id)
	return ""
}

func waitRunSettled(t *testing.T, session *agent.Session) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for session.Running() || session.Compacting() {
		if time.Now().After(deadline) {
			t.Fatal("the run never settled")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestTitleSurvivesManualCompaction: the C1 path 1 — create, send,
// manual Compact (the agent writes the context.compacted through its
// own commit path, with pendingCreated materialization and the header
// copy), send again, and the listing still carries the FIRST input as
// the title.
func TestTitleSurvivesManualCompaction(t *testing.T) {
	_, session, store, ns := titleEnv(t)

	if err := session.Send("FIRST INPUT"); err != nil {
		t.Fatal(err)
	}
	waitRunSettled(t, session)
	if err := session.Compact(); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if err := session.Send("SECOND INPUT"); err != nil {
		t.Fatal(err)
	}
	waitRunSettled(t, session)

	if got := titleOf(t, store, ns, session.ID()); got != "FIRST INPUT" {
		t.Fatalf("title after manual compaction = %q, want %q", got, "FIRST INPUT")
	}
}

// TestTitleSurvivesAutoCompaction: the C1 path 2 — a small
// ContextWindow drives the automatic PRE-TURN compaction (the pending
// input is absorbed into the summary before the request is rebuilt);
// the title must still be the first input.
func TestTitleSurvivesAutoCompaction(t *testing.T) {
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
	// A small window so the second send crosses the pre-turn pressure.
	resolver := agent.NewStaticResolver(&titleProvider{}, nil, 2_000, 500, "")
	ag, err := agent.New(agent.Options{
		ModelResolver: resolver,
		Storage:       store,
		CWD:           projectDir,
		AutoCompact:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ns := projects.ProjectID(projectDir)
	session, err := ag.NewSession(ns, storage.SessionMeta{Provider: "p", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	if err := session.Send("THE AUTO FIRST"); err != nil {
		t.Fatal(err)
	}
	waitRunSettled(t, session)
	// A large second input crosses the small budget and triggers the
	// pre-turn auto compaction.
	if err := session.Send("SECOND " + strings.Repeat("wide ", 2000)); err != nil {
		t.Fatal(err)
	}
	waitRunSettled(t, session)

	// The auto compaction must have actually fired — otherwise this
	// test would be vacuously green (K7: AutoCompact disabled turns
	// THIS assertion red). The segment switch is covered by the title
	// assertion below (K2: the archive rename removed turns THAT red).
	hasCompacted := false
	for _, r := range session.SnapshotStream().Records {
		if r.Type == "context.compacted" {
			hasCompacted = true
		}
	}
	if !hasCompacted {
		t.Fatal("the auto compaction never fired — the test would be vacuous")
	}

	if got := titleOf(t, store, ns, session.ID()); got != "THE AUTO FIRST" {
		t.Fatalf("title after auto compaction = %q, want %q", got, "THE AUTO FIRST")
	}
}

// TestTitleSurvivesReopenAfterCompaction: the C1 path 3 — compact,
// close, reopen through a fresh agent, append another round; the
// title must not change (the earliest archive still holds the
// original start).
func TestTitleSurvivesReopenAfterCompaction(t *testing.T) {
	ag, session, store, ns := titleEnv(t)

	if err := session.Send("THE REOPEN FIRST"); err != nil {
		t.Fatal(err)
	}
	waitRunSettled(t, session)
	if err := session.Compact(); err != nil {
		t.Fatal(err)
	}
	id := session.ID()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen through a fresh agent on the same store.
	ag2, err := agent.New(agent.Options{
		ModelResolver: agent.NewStaticResolver(&titleProvider{}, nil, 0, 0, ""),
		Storage:       store,
		CWD:           ag.CWD(),
	})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := ag2.OpenSession(ns, id)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	if err := reopened.Send("after reopen"); err != nil {
		t.Fatal(err)
	}
	waitRunSettled(t, reopened)

	if got := titleOf(t, store, ns, id); got != "THE REOPEN FIRST" {
		t.Fatalf("title after reopen+append = %q, want %q", got, "THE REOPEN FIRST")
	}
}

// TestTitleProviderIsUsed is a compile guard: the provider must
// implement model.Provider.
var _ model.Provider = (*titleProvider)(nil)
