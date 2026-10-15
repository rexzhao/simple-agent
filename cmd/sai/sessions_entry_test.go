package main

// Real-binary entrypoint tests for the C7 session commands (§17.5):
// /sessions through the actual REPL, /resume <prefix>, and the C2
// carry-over — the startup's unknown-model error lists the configured
// choices.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/storage/jsonl"
)

// buildSaiBinary compiles the real CLI once for subprocess tests.
func buildSaiBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("subprocess build is slow")
	}
	bin := filepath.Join(t.TempDir(), "sai-test.exe")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build sai: %v\n%s", err, out)
	}
	return bin
}

// writeFakeProviderConfig lays out the config the entrypoint needs:
// sai.yaml plus one provider file (dead port — no network contact).
func writeFakeProviderConfig(t *testing.T, root string) {
	t.Helper()
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
}

// TestCLIEntrypointSessionsAndResume drives the real binary: the
// /sessions listing renders the seeded sessions, /resume <prefix>
// switches to one, and /resumefoo stays an unknown command.
func TestCLIEntrypointSessionsAndResume(t *testing.T) {
	bin := buildSaiBinary(t)
	root := t.TempDir()
	projectDir := t.TempDir()
	writeFakeProviderConfig(t, root)

	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	ns := projects.ProjectID(projectDir)
	var ids []string
	for i := 0; i < 2; i++ {
		session, err := store.Create(ns)
		if err != nil {
			t.Fatal(err)
		}
		header := `{"item":"h` + strings.Repeat("0", 26) + `","t":"session.created","ts":"2026-01-0` + string(rune('1'+i)) + `T03:04:05Z","provider":"fake","model":"fake-model"}` + "\n"
		sessDir := filepath.Join(store.Root(), ns, session.ID())
		if err := os.MkdirAll(sessDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sessDir, "log.jsonl"), []byte(header), 0o644); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, session.ID())
	}

	// A prefix unique to ids[1]: the two ids share their head, so use
	// enough characters — the full id minus one, plus verify.
	prefix := uniquePrefixOf(ids[1], ids[0])

	run := func(stdin string) (string, string, error) {
		cmd := exec.Command(bin)
		cmd.Dir = projectDir
		cmd.Env = append(os.Environ(), "SAI_SERVER_ROOT="+root)
		cmd.Stdin = strings.NewReader(stdin)
		var out, errBuf strings.Builder
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		err := cmd.Run()
		return out.String(), errBuf.String(), err
	}

	// /sessions lists both seeded sessions.
	stdout, _, err := run("/sessions\n/quit\n")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, id := range ids {
		if !strings.Contains(stdout, id[:12]) {
			t.Fatalf("/sessions output lacks %s: %q", id, stdout)
		}
	}

	// /resume <prefix> switches; the exit line names the session.
	_, stderr, err := run("/resume " + prefix + "\n/quit\n")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stderr, "sai: session "+ids[1]+"\n") {
		t.Fatalf("/resume %s did not end on the resumed session: %q", prefix, stderr)
	}

	// /resumefoo stays an unknown command at the real entrypoint.
	_, stderr, err = run("/resumefoo\n/quit\n")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stderr, "unknown command /resumefoo") {
		t.Fatalf("/resumefoo stderr = %q, want unknown-command", stderr)
	}
}

// TestCLIEntrypointUnknownModelListsChoices (the C2 carry-over): the
// startup's unknown-model error names the model AND lists the
// configured choices — the operator can fix the flag without opening
// the config.
func TestCLIEntrypointUnknownModelListsChoices(t *testing.T) {
	bin := buildSaiBinary(t)
	root := t.TempDir()
	projectDir := t.TempDir()
	writeFakeProviderConfig(t, root)

	cmd := exec.Command(bin, "-model", "no-such-model")
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(), "SAI_SERVER_ROOT="+root)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("startup with an unknown model succeeded")
	}
	msg := stderr.String()
	if !strings.Contains(msg, `unknown model "no-such-model" for provider "fake"`) {
		t.Fatalf("error lacks the model/provider: %q", msg)
	}
	if !strings.Contains(msg, "fake-model") || !strings.Contains(msg, "fake-reasoner") {
		t.Fatalf("error does not list the configured models: %q", msg)
	}
}

// uniquePrefixOf returns a prefix of id that does not prefix-match
// any of the others.
func uniquePrefixOf(id string, others ...string) string {
	for n := 1; n <= len(id); n++ {
		candidate := id[:n]
		unique := true
		for _, other := range others {
			if strings.HasPrefix(other, candidate) {
				unique = false
				break
			}
		}
		if unique {
			return candidate
		}
	}
	return id
}

// TestCLIEntrypointArchivedSession (C9a, the C7 carry-over): the
// startup id (-session / positional) hitting an ARCHIVED session gets
// the "unarchive it first" hint instead of a raw storage error.
func TestCLIEntrypointArchivedSession(t *testing.T) {
	bin := buildSaiBinary(t)
	root := t.TempDir()
	projectDir := t.TempDir()
	writeFakeProviderConfig(t, root)

	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	ns := projects.ProjectID(projectDir)
	session, err := store.Create(ns)
	if err != nil {
		t.Fatal(err)
	}
	header := `{"item":"h` + strings.Repeat("0", 26) + `","t":"session.created","ts":"2026-01-02T03:04:05Z","provider":"fake","model":"fake-model"}` + "\n"
	sessDir := filepath.Join(store.Root(), ns, session.ID())
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "log.jsonl"), []byte(header), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Archive(ns, session.ID()); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "-session", session.ID())
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(), "SAI_SERVER_ROOT="+root)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("startup on an archived session succeeded")
	}
	msg := stderr.String()
	if !strings.Contains(msg, "is archived") || !strings.Contains(msg, "/unarchive") {
		t.Fatalf("archived startup stderr = %q, want the unarchive hint", msg)
	}
}
