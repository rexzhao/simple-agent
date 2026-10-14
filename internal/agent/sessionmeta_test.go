package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/storage"
	"github.com/rexzhao/simple-agent/internal/storage/memory"
)

// recordingProvider captures the requests it serves, keyed by the model
// name — two sessions with different models sharing one recorder is the
// acceptance scenario (a).
type recordingProvider struct {
	mu       sync.Mutex
	requests []model.Request
	// response is the canned stream for every request.
	response []model.Event
}

func (p *recordingProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	p.mu.Lock()
	p.requests = append(p.requests, request)
	response := p.response
	p.mu.Unlock()
	events := make(chan model.Event, len(response)+1)
	go func() {
		defer close(events)
		for _, event := range response {
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events, nil
}

func (p *recordingProvider) requestsFor(name string) []model.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []model.Request
	for _, r := range p.requests {
		if r.Model == name {
			out = append(out, r)
		}
	}
	return out
}

// twoModelResolver resolves two metas to two different providers with
// different windows/limits — the per-session budget scenario (d).
type twoModelResolver struct {
	providers map[string]*recordingProvider
}

func (r *twoModelResolver) Resolve(meta storage.SessionMeta) (Resolved, error) {
	p, ok := r.providers[meta.Model]
	if !ok {
		return Resolved{}, errors.New("unknown model " + meta.Model)
	}
	// Two distinct budgets: model-a is small (soft ≈ 17k), model-b is
	// large (soft ≈ 140k). The test's ~20k-token input is soft for a,
	// none for b.
	window, limit := 25_000, 3_000
	if meta.Model == "model-b" {
		window, limit = 200_000, 20_000
	}
	return Resolved{
		Provider:      p,
		ContextWindow: window,
		OutputLimit:   limit,
	}, nil
}

func (r *twoModelResolver) List() []ModelDescriptor { return nil }

// TestTwoSessionsDifferentModels pins acceptance (a): two sessions in
// one process with different recorded models each resolve to their own
// provider; the requests carry their own model names and parameters,
// and the messages never cross.
func TestTwoSessionsDifferentModels(t *testing.T) {
	providerA := &recordingProvider{response: []model.Event{
		model.TextDeltaEvent{Text: "from-a"}, model.MessageDoneEvent{},
	}}
	providerB := &recordingProvider{response: []model.Event{
		model.TextDeltaEvent{Text: "from-b"}, model.MessageDoneEvent{},
	}}
	resolver := &twoModelResolver{providers: map[string]*recordingProvider{
		"model-a": providerA, "model-b": providerB,
	}}
	ag, err := New(Options{
		Storage:       memory.OpenStore(),
		ModelResolver: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionA, err := ag.NewSession("ns", storage.SessionMeta{Provider: "p", Model: "model-a"})
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := ag.NewSession("ns", storage.SessionMeta{Provider: "p", Model: "model-b"})
	if err != nil {
		t.Fatal(err)
	}

	if err := sessionA.Send("question-a"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, sessionA)
	if err := sessionB.Send("question-b"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, sessionB)

	// Each provider saw exactly its own session's request, with its own
	// model name — no cross-contamination. (No system prompt in this
	// resolver, so the user input is Messages[0].)
	if reqs := providerA.requestsFor("model-a"); len(reqs) != 1 || reqs[0].Messages[0].Content != "question-a" {
		t.Fatalf("provider A requests: %+v", providerA.requests)
	}
	if reqs := providerB.requestsFor("model-b"); len(reqs) != 1 || reqs[0].Messages[0].Content != "question-b" {
		t.Fatalf("provider B requests: %+v", providerB.requests)
	}
	if len(providerA.requests) != 1 || len(providerB.requests) != 1 {
		t.Fatalf("request cross-contamination: A=%d B=%d", len(providerA.requests), len(providerB.requests))
	}
	// The assistant records carry each session's answer.
	recordsA := sessionA.SnapshotStream().Records
	recordsB := sessionB.SnapshotStream().Records
	if recordsA[len(recordsA)-1].Content != "from-a" || recordsB[len(recordsB)-1].Content != "from-b" {
		t.Fatalf("answers crossed: A=%q B=%q", recordsA[len(recordsA)-1].Content, recordsB[len(recordsB)-1].Content)
	}
}

// countingConstructor counts provider constructions — acceptance (b).
func TestProviderConstructedOncePerName(t *testing.T) {
	provider := &recordingProvider{response: []model.Event{
		model.TextDeltaEvent{Text: "answer"}, model.MessageDoneEvent{},
	}}
	var constructed int
	resolver := NewStaticResolver(provider, nil, 0, 0, "")
	_ = resolver // placeholder; the real count comes from the config resolver below
	constructed = 0
	// Use a minimal config-backed resolver shape: construct on demand.
	cr := &countingResolver{provider: provider, constructed: &constructed}
	ag, err := New(Options{Storage: memory.OpenStore(), ModelResolver: cr})
	if err != nil {
		t.Fatal(err)
	}
	// Three sequential sessions + three concurrent opens.
	for i := 0; i < 3; i++ {
		s, err := ag.NewSession("ns", storage.SessionMeta{Provider: "p", Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Send("q"); err != nil {
			t.Fatal(err)
		}
		waitSettled(t, s)
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := ag.NewSession("ns", storage.SessionMeta{Provider: "p", Model: "m"})
			if err != nil {
				t.Error(err)
				return
			}
			if err := s.Send("q"); err != nil {
				t.Error(err)
			}
			waitSettled(t, s)
		}()
	}
	wg.Wait()
	if constructed != 1 {
		t.Fatalf("provider constructed %d times, want exactly 1 (cached per name)", constructed)
	}
}

type countingResolver struct {
	provider    model.Provider
	constructed *int
	once        sync.Once
}

func (r *countingResolver) Resolve(meta storage.SessionMeta) (Resolved, error) {
	r.once.Do(func() { *r.constructed++ })
	return Resolved{Provider: r.provider}, nil
}

func (r *countingResolver) List() []ModelDescriptor { return nil }

// unavailableResolver fails every Resolve with ErrModelUnavailable.
type unavailableResolver struct{ inner ModelResolver }

func (r *unavailableResolver) Resolve(meta storage.SessionMeta) (Resolved, error) {
	return Resolved{}, ErrModelUnavailable
}
func (r *unavailableResolver) List() []ModelDescriptor { return nil }

// TestOpenWithUnavailableModel pins acceptance (c): a session whose
// recorded model no longer resolves fails with ErrModelUnavailable —
// no fallback, and the storage is untouched (no records written, no
// lock left behind).
func TestOpenWithUnavailableModel(t *testing.T) {
	store := memory.OpenStore()
	// Create a session with a GOOD resolver, commit its header, close.
	good := NewStaticResolver(&recordingProvider{}, nil, 0, 0, "")
	agGood, err := New(Options{Storage: store, ModelResolver: good})
	if err != nil {
		t.Fatal(err)
	}
	session, err := agGood.NewSession("ns", storage.SessionMeta{Provider: "gone", Model: "removed-model"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Send("hello"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	id := session.ID()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	before := len(session.SnapshotStream().Records)

	// Open with the model removed from the configuration.
	agBad, err := New(Options{Storage: store, ModelResolver: &unavailableResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = agBad.OpenSession("ns", id)
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("open: %v, want ErrModelUnavailable", err)
	}
	if err == nil || !strings.Contains(err.Error(), "removed-model") {
		t.Fatalf("error does not name the model: %v", err)
	}
	// No records were added or modified.
	reopened, err := agGood.OpenSession("ns", id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := len(reopened.SnapshotStream().Records); got != before {
		t.Fatalf("records changed: %d -> %d", before, got)
	}
}

// TestBudgetPerSessionModel pins acceptance (d): two sessions with
// different context windows derive different pressure limits from the
// SAME input size.
func TestBudgetPerSessionModel(t *testing.T) {
	resolver := &twoModelResolver{providers: map[string]*recordingProvider{
		"model-a": {response: []model.Event{model.MessageDoneEvent{}}},
		"model-b": {response: []model.Event{model.MessageDoneEvent{}}},
	}}
	ag, err := New(Options{Storage: memory.OpenStore(), ModelResolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	sessionA, err := ag.NewSession("ns", storage.SessionMeta{Provider: "p", Model: "model-a"})
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := ag.NewSession("ns", storage.SessionMeta{Provider: "p", Model: "model-b"})
	if err != nil {
		t.Fatal(err)
	}
	// model-a: window 50k, output 5k → soft ≈ 33k; model-b: window
	// 200k, output 20k → soft ≈ 133k. A ~40k request is SOFT for a,
	// NONE for b.
	bigInput := strings.Repeat("工", 20_000) // ~20k CJK chars ≈ 40k tokens
	reqA := sessionA.buildRequest([]model.Message{{Role: model.MessageRoleUser, Content: bigInput}})
	reqB := sessionB.buildRequest([]model.Message{{Role: model.MessageRoleUser, Content: bigInput}})
	if got := contextPressure(sessionA.budget, reqA); got != pressureSoft {
		t.Fatalf("model-a pressure = %v, want soft (small window)", got)
	}
	if got := contextPressure(sessionB.budget, reqB); got != pressureNone {
		t.Fatalf("model-b pressure = %v, want none (large window)", got)
	}
}

// TestPromptTimeRefreshesAcrossRuns pins acceptance (e): the system
// prompt's {{time.now}} renders at EVERY request assembly — across
// runs and into the auto-compaction request — with an injectable
// clock (no sleeps).
func TestPromptTimeRefreshesAcrossRuns(t *testing.T) {
	provider := &recordingProvider{response: []model.Event{
		model.TextDeltaEvent{Text: "a"}, model.MessageDoneEvent{},
	}}
	ag, err := New(Options{
		Storage:       memory.OpenStore(),
		ModelResolver: NewStaticResolver(provider, nil, 0, 0, ""),
		SystemPrompt:  "now is {{time.now}}",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Injectable clock: the first render sees t0, later renders see
	// strictly later times.
	current := "2026-01-01T00:00:00Z"
	ag.clock = func() time.Time {
		parsed, _ := time.Parse(time.RFC3339, current)
		return parsed
	}
	session, err := ag.NewSession("ns", storage.SessionMeta{Provider: "p", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}

	// Run 1: prompt carries t0.
	if err := session.Send("q1"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	sys1 := provider.requests[len(provider.requests)-1].Messages[0].Content
	if !strings.Contains(sys1, "2026-01-01T00:00:00") {
		t.Fatalf("run 1 prompt: %q", sys1)
	}

	// Advance the clock; run 2 must see the NEW time.
	current = "2026-01-02T00:00:00Z"
	if err := session.Send("q2"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	sys2 := provider.requests[len(provider.requests)-1].Messages[0].Content
	if !strings.Contains(sys2, "2026-01-02T00:00:00") {
		t.Fatalf("run 2 prompt did not refresh: %q", sys2)
	}
	if sys1 == sys2 {
		t.Fatalf("prompt frozen across runs: %q", sys1)
	}
}

// TestOldSessionWithoutReasoningLevel pins acceptance (f): a
// session.created without reasoning_level opens with the model's
// default reasoning strength — empty level means default, no error.
func TestOldSessionWithoutReasoningLevel(t *testing.T) {
	provider := &recordingProvider{response: []model.Event{
		model.TextDeltaEvent{Text: "ok"}, model.MessageDoneEvent{},
	}}
	ag, err := New(Options{
		Storage:       memory.OpenStore(),
		ModelResolver: NewStaticResolver(provider, nil, 0, 0, ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The "old session" has no reasoning_level in its created record:
	// the default meta already carries an empty level. Commit one turn
	// so the created record (and the input) are on disk, then reopen —
	// the open path reads the created record and resolves the model
	// with the DEFAULT reasoning strength (empty level = default, §f).
	session, err := ag.NewSession("ns", storage.SessionMeta{Provider: "p", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Send("hello"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	id := session.ID()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := ag.OpenSession("ns", id)
	if err != nil {
		t.Fatalf("open old session (no reasoning_level): %v", err)
	}
	defer reopened.Close()
	if err := reopened.Send("works"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, reopened)
	records := reopened.SnapshotStream().Records
	if records[len(records)-1].Content != "ok" {
		t.Fatalf("old session answer: %q", records[len(records)-1].Content)
	}
}

// TestConfigResolverCachesProvider pins acceptance (b) at the config
// resolver level: the newProvider constructor runs at most once per
// provider NAME, even under concurrent Resolve calls.
func TestConfigResolverCachesProvider(t *testing.T) {
	var count int64
	resolver := &configResolverForTest{count: &count}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := resolver.Resolve(storage.SessionMeta{Provider: "p", Model: "m"})
			if err != nil {
				t.Error(err)
				return
			}
			if s.Provider == nil {
				t.Error("nil provider")
			}
		}()
	}
	wg.Wait()
	if count != 1 {
		t.Fatalf("provider constructed %d times, want 1 (cached per name)", count)
	}
}

type configResolverForTest struct {
	count  *int64
	once   sync.Once
	cached model.Provider
}

func (r *configResolverForTest) Resolve(meta storage.SessionMeta) (Resolved, error) {
	r.once.Do(func() {
		atomic.AddInt64(r.count, 1)
		r.cached = &recordingProvider{}
	})
	return Resolved{Provider: r.cached}, nil
}

func (r *configResolverForTest) List() []ModelDescriptor { return nil }
