package agent_test

// Open-layer tests for empty-meta rejection against the PRODUCTION
// config resolver (internal/config), not the static test bridge: the
// agent package cannot import internal/config (import cycle), but this
// external test package can. Mutating the production guard in
// config.(*configResolver).Resolve must turn these red.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage"
	"github.com/rexzhao/simple-agent/storage/jsonl"
)

// productionResolver builds the production config-backed resolver.
// The empty-meta guard fires before any provider is built, so the
// constructor is only reached by the CONTROL case (a valid meta) —
// there it returns a stub provider (the reopen never streams).
func productionResolver(t *testing.T) agent.ModelResolver {
	t.Helper()
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
	resolver, err := config.NewModelResolver(cfg, func(config.ResolvedModel) (model.Provider, error) {
		return stubProvider{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

// stubProvider satisfies model.Provider for the control case; the
// reopened session is closed without streaming.
type stubProvider struct{}

func (stubProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	return nil, errors.New("stub: not used")
}

// TestOpenEmptyMetaRejectedByProductionResolver pins the open-layer
// behavior for both degenerate cases (§15.2) against the PRODUCTION
// resolver: a hand-written created record with empty provider/model,
// and a never-committed (record-less) session directory. Both must
// fail with ErrModelUnavailable, the log file must be byte-identical
// after the failed open (no records written), and a normal session
// must still reopen (the guard does not over-reject). The production
// guard lives in internal/config's configResolver.Resolve — deleting
// it makes the (i)/(ii) cases fail (they would silently resolve to
// the configured default instead of refusing).
func TestOpenEmptyMetaRejectedByProductionResolver(t *testing.T) {
	store, err := jsonl.OpenStore(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agent.Options{Storage: store, ModelResolver: productionResolver(t)})
	if err != nil {
		t.Fatal(err)
	}
	ns := "p" + strings.Repeat("0", 16)

	// (ii) Foreign log: a created record with empty provider/model,
	// hand-written into the store layout. The provider/model fields
	// are omitted entirely (omitempty) — the exact shape an old or
	// foreign writer produces.
	foreignID := "s" + strings.Repeat("a", 26)
	foreignDir := filepath.Join(store.Root(), ns, foreignID)
	if err := os.MkdirAll(foreignDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreignLog := `{"item":"h00000000000000000000000001","t":"session.created","ts":"2026-01-01T00:00:00Z"}` + "\n" +
		`{"item":"u00000000000000000000000002","t":"user.input","ts":"2026-01-01T00:01:00Z","run":"run-1","content":"hi"}` + "\n"
	logPath := filepath.Join(foreignDir, "log.jsonl")
	if err := os.WriteFile(logPath, []byte(foreignLog), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = ag.OpenSession(ns, foreignID)
	if !errors.Is(err, agent.ErrModelUnavailable) {
		t.Fatalf("(ii) empty meta open: %v, want ErrModelUnavailable", err)
	}
	after, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("(ii) the failed open modified the log file")
	}

	// (i) Never-committed session: a directory with no log at all
	// (a lazily-created session that was never written). Opening it
	// must refuse the same way — an empty record set is an empty meta.
	emptyID := "s" + strings.Repeat("b", 26)
	if err := os.MkdirAll(filepath.Join(store.Root(), ns, emptyID), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = ag.OpenSession(ns, emptyID)
	if !errors.Is(err, agent.ErrModelUnavailable) {
		t.Fatalf("(i) record-less open: %v, want ErrModelUnavailable", err)
	}

	// Control: a session whose recorded meta matches the configuration
	// reopens fine (the guard does not over-reject).
	good, err := ag.NewSession(ns, storage.SessionMeta{Provider: "fake", Model: "fake-model"})
	if err != nil {
		t.Fatal(err)
	}
	id := good.ID()
	// Commit the header so the reopen has a record to read, then close.
	if err := good.MaterializeHeader(); err != nil {
		t.Fatal(err)
	}
	if err := good.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := ag.OpenSession(ns, id)
	if err != nil {
		t.Fatalf("normal session reopen: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
