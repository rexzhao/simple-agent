package main

// CLI session-management command tests (C7, §17.5): /sessions
// (numbering, --all, archived hiding), /resume (number, prefix
// unique/zero/multiple, archived refusal, out-of-range), /archive
// //unarchive (number bases, current-session refusal, round-trip),
// and exact command matching (/resumefoo and friends).

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/storage"
	"github.com/rexzhao/simple-agent/storage/jsonl"
	"github.com/rexzhao/simple-agent/storage/lockfile"
)

// newDeadAgent assembles the CLI's agent over the store (a dead-port
// static resolver: the session commands never contact a model).
func newDeadAgent(t *testing.T, store *jsonl.Store, cwd string) *agent.Agent {
	t.Helper()
	ag, err := agent.New(agent.Options{
		ModelResolver: agent.NewStaticResolver(&scriptedProvider{}, nil, 0, 0, ""),
		Storage:       store,
		CWD:           cwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

// sessionsWorld is a CLI over a jsonl store with N materialized live
// sessions plus one archived, returning the live ids (oldest first)
// and the archived id.
func sessionsWorld(t *testing.T, live int) (*cliApp, []string, string) {
	t.Helper()
	root := t.TempDir()
	projectDir := t.TempDir()
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	cli, err := setupCLI(newDeadAgent(t, store, projectDir), store, testSessionMeta, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cli.closeSession)

	var ids []string
	for i := 0; i < live+1; i++ {
		session, err := cli.agent.NewSession(cli.ns, testSessionMeta)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.MaterializeHeader(); err != nil {
			t.Fatal(err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, session.ID())
		// The listing sorts by mtime (ID as the tiebreak); spaced
		// creations keep the order deterministic instead of falling
		// to the ID tiebreak.
		time.Sleep(20 * time.Millisecond)
	}
	archived := ids[len(ids)-1]
	ids = ids[:len(ids)-1]
	if err := store.Archive(cli.ns, archived); err != nil {
		t.Fatal(err)
	}
	return cli, ids, archived
}

// captureOutput swaps the printer for a recorder; the CLI writes
// listing rows through it.
type recordingPrinter struct{ lines []string }

func (p *recordingPrinter) line(s string)  { p.lines = append(p.lines, s) }
func (p *recordingPrinter) write(s string) { p.lines = append(p.lines, s) }
func (p *recordingPrinter) frame(s string) {}
func (p *recordingPrinter) clear()         {}

func withRecorder(cli *cliApp) *recordingPrinter {
	rec := &recordingPrinter{}
	cli.printer = rec
	return rec
}

// TestSessionsNumbersAndHidesArchived: live rows numbered 1..N by
// mtime (newest first), the archived one absent by default.
func TestSessionsNumbersAndHidesArchived(t *testing.T) {
	cli, _, _ := sessionsWorld(t, 2)
	rec := withRecorder(cli)
	cli.dispatch("/sessions")
	if len(rec.lines) != 2 {
		t.Fatalf("rows = %v, want 2 live", rec.lines)
	}
	// Two numbered rows, no A row. WHICH id lands on which number is
	// pinned by TestResumeByNumberAndPrefix (it switches sessions by
	// number); here the short display prefixes of one batch's ids are
	// mutually indistinguishable, so id-matching would be meaningless.
	if !strings.HasPrefix(rec.lines[0], " 1  ") || !strings.HasPrefix(rec.lines[1], " 2  ") {
		t.Fatalf("numbering = %q", rec.lines)
	}
	for _, line := range rec.lines {
		if strings.HasPrefix(line, " A  ") {
			t.Fatalf("archived session in the default view: %q", line)
		}
	}
}

// TestSessionsAllShowsArchivedWithA: --all adds the archived row,
// marked A and unnumbered.
func TestSessionsAllShowsArchivedWithA(t *testing.T) {
	cli, _, _ := sessionsWorld(t, 2)
	rec := withRecorder(cli)
	cli.dispatch("/sessions --all")
	if len(rec.lines) != 3 {
		t.Fatalf("rows = %v, want 3", rec.lines)
	}
	aRows := 0
	for _, line := range rec.lines {
		if strings.HasPrefix(line, " A  ") {
			aRows++
		}
	}
	if aRows != 1 {
		t.Fatalf("--all rows with A = %d, want 1: %v", aRows, rec.lines)
	}
	// Live numbering is unaffected: the numbered rows are 1 and 2, in
	// order of appearance (the A row interleaves by mtime).
	var numbers []string
	for _, line := range rec.lines {
		if !strings.HasPrefix(line, " A  ") {
			head := strings.SplitN(strings.TrimSpace(line), "  ", 2)[0]
			numbers = append(numbers, head)
		}
	}
	if len(numbers) != 2 || numbers[0] != "1" || numbers[1] != "2" {
		t.Fatalf("live numbering = %v, want [1 2]: %v", numbers, rec.lines)
	}
	// Unknown flag.
	rec2 := withRecorder(cli)
	stderr := captureStderr(t, func() { cli.dispatch("/sessions --bogus") })
	if len(rec2.lines) != 0 || !strings.Contains(stderr, "unknown flag") {
		t.Fatalf("--bogus: rows %v stderr %q", rec2.lines, stderr)
	}
}

// TestResumeByNumberAndPrefix: a number takes the live listing's row;
// a unique prefix opens; the session actually switches.
func TestResumeByNumberAndPrefix(t *testing.T) {
	cli, ids, _ := sessionsWorld(t, 2)
	// Number 2 = the older live session (ids[0]).
	before := cli.session.ID()
	if cli.dispatch("/resume 2") {
		t.Fatal("/resume asked the REPL to exit")
	}
	if cli.session.ID() != ids[0] {
		t.Fatalf("/resume 2 → %s, want %s", cli.session.ID(), ids[0])
	}
	// Prefix: enough of ids[1] to be unique.
	prefix := ids[1][:20]
	if cli.dispatch("/resume " + prefix) {
		t.Fatal("/resume asked the REPL to exit")
	}
	if cli.session.ID() != ids[1] {
		t.Fatalf("/resume %s → %s, want %s", prefix, cli.session.ID(), ids[1])
	}
	_ = before
}

// TestResumeNumberOutOfRange: beyond the listing refuses with the
// range and does NOT switch the session.
func TestResumeNumberOutOfRange(t *testing.T) {
	cli, _, _ := sessionsWorld(t, 2)
	before := cli.session.ID()
	stderr := captureStderr(t, func() { cli.dispatch("/resume 3") })
	if !strings.Contains(stderr, "out of range") || !strings.Contains(stderr, "2 live session") {
		t.Fatalf("out-of-range stderr = %q", stderr)
	}
	if cli.session.ID() != before {
		t.Fatal("out-of-range /resume switched the session")
	}
}

// TestResumePrefixZeroAndMultiple.
func TestResumePrefixZeroAndMultiple(t *testing.T) {
	cli, ids, _ := sessionsWorld(t, 2)
	before := cli.session.ID()
	// Zero hits carry the project-scoping hint.
	stderr := captureStderr(t, func() { cli.dispatch("/resume sZZZZ") })
	if !strings.Contains(stderr, "no session in this project starts with") ||
		!strings.Contains(stderr, "project directory they belong to") {
		t.Fatalf("zero-hit stderr = %q", stderr)
	}
	// A common prefix of both ids (they share the "s" head): multiple.
	stderr = captureStderr(t, func() { cli.dispatch("/resume " + commonPrefix(ids[0], ids[1])) })
	if !strings.Contains(stderr, "multiple sessions start with") ||
		!strings.Contains(stderr, ids[0]) || !strings.Contains(stderr, ids[1]) {
		t.Fatalf("multi-hit stderr = %q", stderr)
	}
	if cli.session.ID() != before {
		t.Fatal("a failed prefix match switched the session")
	}
}

// TestResumeArchivedSuggestsUnarchive.
func TestResumeArchivedSuggestsUnarchive(t *testing.T) {
	cli, _, archived := sessionsWorld(t, 2)
	before := cli.session.ID()
	stderr := captureStderr(t, func() { cli.dispatch("/resume " + archived[:20]) })
	if !strings.Contains(stderr, "is archived") || !strings.Contains(stderr, "/unarchive") {
		t.Fatalf("archived stderr = %q", stderr)
	}
	if cli.session.ID() != before {
		t.Fatal("archived /resume switched the session")
	}
}

// TestArchiveByNumberAndUnarchiveRoundTrip: /archive <live #> parks
// the row; /unarchive <archived #> restores it (the two number bases).
func TestArchiveByNumberAndUnarchiveRoundTrip(t *testing.T) {
	cli, ids, archived := sessionsWorld(t, 2)
	// Archive live #1 (ids[1]).
	stderr := captureStderr(t, func() { cli.dispatch("/archive 1") })
	if !strings.Contains(stderr, "archived "+ids[1]) {
		t.Fatalf("/archive 1 stderr = %q", stderr)
	}
	if ok, _ := cli.store.IsArchived(cli.ns, ids[1]); !ok {
		t.Fatal("marker missing after /archive 1")
	}
	// Now TWO archived sessions exist. Archived numbering is mtime
	// desc and sessionsWorld created the PRE-EXISTING archived one
	// last — it is archived #1, ids[1] is #2.
	stderr = captureStderr(t, func() { cli.dispatch("/unarchive 1") })
	if !strings.Contains(stderr, "unarchived "+archived) {
		t.Fatalf("/unarchive 1 stderr = %q (want the pre-existing archived as #1)", stderr)
	}
	if ok, _ := cli.store.IsArchived(cli.ns, archived); ok {
		t.Fatal("marker present after /unarchive")
	}
	// ids[1] is the remaining archived #1.
	stderr = captureStderr(t, func() { cli.dispatch("/unarchive 1") })
	if !strings.Contains(stderr, "unarchived "+ids[1]) {
		t.Fatalf("/unarchive 1 stderr = %q (want ids[1] next)", stderr)
	}
	// /unarchive by prefix too.
	if err := cli.store.Archive(cli.ns, ids[0]); err != nil {
		t.Fatal(err)
	}
	stderr = captureStderr(t, func() { cli.dispatch("/unarchive " + ids[0][:20]) })
	if !strings.Contains(stderr, "unarchived "+ids[0]) {
		t.Fatalf("prefix /unarchive stderr = %q", stderr)
	}
}

// TestArchiveCurrentSessionRefused: the CLI holds the current
// session's lock; archiving it is refused, not auto-switched.
func TestArchiveCurrentSessionRefused(t *testing.T) {
	cli, _, _ := sessionsWorld(t, 2)
	// The refusal is reachable once the current session is on disk
	// (an empty fresh session never lists, so /archive cannot name it).
	if err := cli.session.MaterializeHeader(); err != nil {
		t.Fatal(err)
	}
	current := cli.session.ID()
	stderr := captureStderr(t, func() { cli.dispatch("/archive " + current) })
	if !strings.Contains(stderr, "this is the current session") {
		t.Fatalf("current-session stderr = %q", stderr)
	}
	if ok, _ := cli.store.IsArchived(cli.ns, current); ok {
		t.Fatal("the current session was archived")
	}
	if cli.session.ID() != current {
		t.Fatal("the refusal switched the session")
	}
}

// TestArchiveUnarchiveUnknownAndUsage.
func TestArchiveUnarchiveUnknownAndUsage(t *testing.T) {
	cli, _, _ := sessionsWorld(t, 2)
	for _, tc := range []struct{ cmd, want string }{
		{"/archive", "usage: /archive"},
		{"/unarchive", "usage: /unarchive"},
		{"/archive sZZZZ", "no session in this project starts with"},
		{"/unarchive 9", "out of range"},
	} {
		stderr := captureStderr(t, func() { cli.dispatch(tc.cmd) })
		if !strings.Contains(stderr, tc.want) {
			t.Fatalf("%s stderr = %q, want %q", tc.cmd, stderr, tc.want)
		}
	}
}

// TestExactCommandMatching: /resumefoo, /sessionsfoo, /archivefoo,
// /unarchivefoo all land in unknown-command (not in the command's
// argument parser).
func TestExactCommandMatching(t *testing.T) {
	cli, _, _ := sessionsWorld(t, 2)
	before := cli.session.ID()
	for _, text := range []string{"/resumefoo", "/sessionsfoo", "/archivefoo", "/unarchivefoo"} {
		stderr := captureStderr(t, func() { cli.dispatch(text) })
		if !strings.Contains(stderr, "unknown command "+text) {
			t.Fatalf("%s stderr = %q, want unknown-command", text, stderr)
		}
		// It must not have been routed into the command's own parser.
		if strings.Contains(stderr, "/resume:") || strings.Contains(stderr, "/sessions:") ||
			strings.Contains(stderr, "/archive:") || strings.Contains(stderr, "/unarchive:") {
			t.Fatalf("%s was routed into a command parser: %q", text, stderr)
		}
	}
	if cli.session.ID() != before {
		t.Fatal("an unknown command switched the session")
	}
}

// TestSessionsNoSessions: an empty project names the emptiness.
func TestSessionsNoSessions(t *testing.T) {
	root := t.TempDir()
	projectDir := t.TempDir()
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	cli, err := setupCLI(newDeadAgent(t, store, projectDir), store, testSessionMeta, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cli.closeSession)
	rec := withRecorder(cli)
	stderr := captureStderr(t, func() { cli.dispatch("/sessions") })
	if len(rec.lines) != 0 || !strings.Contains(stderr, "no sessions in this project") {
		t.Fatalf("empty listing: rows %v stderr %q", rec.lines, stderr)
	}
}

// commonPrefix of two strings.
func commonPrefix(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return a[:n]
}

// TestArchiveOpErrorMapping: the storage errors reach the user in CLI
// wording.
func TestArchiveOpErrorMapping(t *testing.T) {
	held := &lockfile.LockHeldError{Target: "session s000…"}
	if msg := archiveOpError(held).Error(); !strings.Contains(msg, "is locked by another process") ||
		!strings.Contains(msg, "is serve or another CLI running") {
		t.Fatalf("lock wording = %q", msg)
	}
	if msg := archiveOpError(storage.ErrSessionNotFound).Error(); !strings.Contains(msg, "no longer exists") {
		t.Fatalf("not-found wording = %q", msg)
	}
	sentinel := errors.New("disk on fire")
	if !errors.Is(archiveOpError(sentinel), sentinel) {
		t.Fatal("an unknown error must pass through unchanged")
	}
}

// TestSessionListTitleCJKSafe: a long CJK title truncates on a rune
// boundary — the column never splits a multi-byte character (titles
// are user input and often Chinese).
func TestSessionListTitleCJKSafe(t *testing.T) {
	long := strings.Repeat("中", 40)
	got := sessionListTitle(long)
	if !utf8.ValidString(got) {
		t.Fatalf("title column splits a rune: %q", got)
	}
	if runes := []rune(got); len(runes) > 24 {
		t.Fatalf("title column too wide: %d runes", len(runes))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated title lacks the ellipsis: %q", got)
	}
	// Short and empty titles unchanged.
	if got := sessionListTitle("short"); got != "short"+strings.Repeat(" ", 19) {
		t.Fatalf("short title padding = %q", got)
	}
	if got := sessionListTitle(""); !strings.Contains(got, "（无标题）") {
		t.Fatalf("empty title = %q", got)
	}
}
