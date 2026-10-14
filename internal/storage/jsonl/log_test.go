package jsonl

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/storage"
)

// TestCorruptionNoticeRidesInjectedLogger pins the §15.1 contract end to
// end: the mid-file corruption notice goes to the INJECTED logger, and
// the store never writes the process stderr directly. The sink side is
// what matters — a wired-but-unused logger field would pass everything
// else (the review probe that caught exactly that).
func TestCorruptionNoticeRidesInjectedLogger(t *testing.T) {
	original := maxTornTailBytes
	maxTornTailBytes = 16
	t.Cleanup(func() { maxTornTailBytes = original })

	// Capture the real stderr and swap in a pipe: any store write to it
	// fails the test.
	savedErr := os.Stderr
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = pipeW
	t.Cleanup(func() {
		os.Stderr = savedErr
		_ = pipeW.Close()
		_, _ = io.ReadAll(pipeR)
		_ = pipeR.Close()
	})

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	store, err := OpenStoreWithLogger(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	ns := testNS(t)
	session, err := store.Create(ns)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(storage.Record{ItemID: "h1", Type: storage.TypeSessionCreated}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	// Hand-corrupt: a valid record, then garbage past the torn-tail
	// threshold, terminated by a newline so it parses as a bad line.
	dir := session.(*Session).Dir()
	path := filepath.Join(dir, logFileName)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(content, []byte("{not json but longer than the torn-tail threshold}\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(ns, session.ID())
	if err != nil {
		t.Fatalf("reopen corrupted log: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	// The notice went to the injected logger...
	out := buf.String()
	if !strings.Contains(out, "mid-file corruption") {
		t.Fatalf("injected logger output = %q, want the corruption notice", out)
	}
	// ...and NOT to the process stderr.
	_ = pipeW.Close()
	stderrData, _ := io.ReadAll(pipeR)
	os.Stderr = savedErr
	if len(stderrData) != 0 {
		t.Fatalf("store wrote to the process stderr: %q", stderrData)
	}
}
