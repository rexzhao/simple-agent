package projectindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/blobstore"
	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

func testProject(t *testing.T, store *projectstore.Store, name string) projectstore.Project {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	project, created, err := store.Create(root, name)
	if err != nil || !created {
		t.Fatalf("create project: project=%#v created=%v err=%v", project, created, err)
	}
	return project
}

func openProjectIndex(t *testing.T, provider *Provider, resume *protocol.ResumeToken) syncengine.OpenedResource {
	t.Helper()
	opened, err := provider.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: ProjectIndexResourceID}, resume)
	if err != nil {
		t.Fatal(err)
	}
	return opened
}

func TestProjectSummaryIsStrictAndSnapshotIsStableSorted(t *testing.T) {
	project := ProjectSummary{ID: "project-a", Root: "/tmp/a", DisplayName: "A", CreatedAt: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), UpdatedAt: time.Date(2025, 1, 3, 3, 4, 5, 0, time.UTC)}
	wire, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ProjectSummary
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != project {
		t.Fatalf("summary round trip = %#v", decoded)
	}
	for _, invalid := range []string{
		`{"id":"project-a","root":"/tmp/a","display_name":"A","archived":false,"created_at":"2025-01-02T03:04:05Z","updated_at":"2025-01-03T03:04:05Z","extra":1}`,
		`{"id":"project a","root":"/tmp/a","display_name":"A","archived":false,"created_at":"2025-01-02T03:04:05Z","updated_at":"2025-01-03T03:04:05Z"}`,
		`{"id":"project-a","id":"project-b","root":"/tmp/a","display_name":"A","archived":false,"created_at":"2025-01-02T03:04:05Z","updated_at":"2025-01-03T03:04:05Z"}`,
		`{"id":"project-a","root":"/tmp/a","display_name":"A","archived":false,"created_at":"2025-01-02T03:04:05Z","updated_at":"2025-01-03T03:04:05Z"} {}`,
		`{"root":"/tmp/a","display_name":"A","archived":false,"created_at":"2025-01-02T03:04:05Z","updated_at":"2025-01-03T03:04:05Z"}`,
		`{"id":"project-a","root":null,"display_name":"A","archived":false,"created_at":"2025-01-02T03:04:05Z","updated_at":"2025-01-03T03:04:05Z"}`,
		`{"id":"project-a","root":"/tmp/a","display_name":"A","archived":"false","created_at":"2025-01-02T03:04:05Z","updated_at":"2025-01-03T03:04:05Z"}`,
		`{"id":"project-a","root":"/tmp/a","display_name":"A","archived":false,"created_at":1,"updated_at":"2025-01-03T03:04:05Z"}`,
	} {
		if err := json.Unmarshal([]byte(invalid), &decoded); err == nil {
			t.Fatalf("malformed project summary accepted: %s", invalid)
		}
	}
	var invalidSnapshot ProjectIndexSnapshot
	if err := json.Unmarshal([]byte(`{"projects":[],"extra":true}`), &invalidSnapshot); err == nil {
		t.Fatal("project snapshot accepted an unknown field")
	}
	if err := json.Unmarshal([]byte(`{"projects":[],"projects":[]}`), &invalidSnapshot); err == nil {
		t.Fatal("project snapshot accepted a duplicate projects field")
	}
	for _, invalid := range []string{`{}`, `{"projects":null}`, `{"projects":{}}`, `{"projects":[]} {}`} {
		if err := json.Unmarshal([]byte(invalid), &invalidSnapshot); err == nil {
			t.Fatalf("malformed project snapshot accepted: %s", invalid)
		}
	}
	invalidUTF8 := append([]byte(`{"id":"project-a","root":"/tmp/a","display_name":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`","archived":false,"created_at":"2025-01-02T03:04:05Z","updated_at":"2025-01-03T03:04:05Z"}`)...)
	if err := json.Unmarshal(invalidUTF8, &decoded); err == nil {
		t.Fatal("project summary accepted invalid UTF-8")
	}
	invalidSnapshotUTF8 := append([]byte(`{"projects":[{"id":"project-a","root":"/tmp/a","display_name":"`), 0xff)
	invalidSnapshotUTF8 = append(invalidSnapshotUTF8, []byte(`","archived":false,"created_at":"2025-01-02T03:04:05Z","updated_at":"2025-01-03T03:04:05Z"}]}`)...)
	if err := json.Unmarshal(invalidSnapshotUTF8, &invalidSnapshot); err == nil {
		t.Fatal("project snapshot accepted invalid UTF-8")
	}

	store := projectstore.NewStore(t.TempDir())
	a := testProject(t, store, "a")
	b := testProject(t, store, "b")
	inlineProvider, err := NewProvider(store, ProviderOptions{StreamEpoch: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := inlineProvider.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	inlineOpen := openProjectIndex(t, inlineProvider, nil)
	var snapshot ProjectIndexSnapshot
	if err := json.Unmarshal(inlineOpen.Snapshot.Content.InlineBytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Projects) != 2 || snapshot.Projects[0].Root != a.Root || snapshot.Projects[1].Root != b.Root {
		t.Fatalf("project snapshot order = %#v", snapshot.Projects)
	}
	inlineOpen.Close()
	inlineProvider.Close()

	provider, err := NewProvider(store, ProviderOptions{StreamEpoch: "test", InlineSnapshotBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if err := provider.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: ProjectIndexResourceID}, nil); err == nil {
		// This provider test uses no blob writer, so the open must fail rather
		// than silently putting a large list on the socket.
		t.Fatal("large project snapshot was sent inline without a blob writer")
	}
	provider.Close()

	blobStore, err := blobstore.New(blobstore.Options{BaseURL: "/api/blobs/", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer blobStore.Close()
	provider, err = NewProvider(store, ProviderOptions{StreamEpoch: "test", InlineSnapshotBytes: 1, BlobWriter: blobStore})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if err := provider.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	blobOpen := openProjectIndex(t, provider, nil)
	defer blobOpen.Close()
	if blobOpen.Snapshot.Content.Blob == nil || blobOpen.Snapshot.Content.Blob.ContentType != "application/json" {
		t.Fatalf("blob snapshot = %#v", blobOpen.Snapshot.Content)
	}
	descriptor := *blobOpen.Snapshot.Content.Blob
	reader, err := blobStore.Open(descriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	content := make([]byte, descriptor.Size)
	if n, err := reader.ReadAt(content, 0); err != nil || n != len(content) {
		t.Fatalf("blob content read n=%d err=%v", n, err)
	}
	var decodedSnapshot ProjectIndexSnapshot
	if err := json.Unmarshal(content, &decodedSnapshot); err != nil {
		t.Fatal(err)
	}
	_ = a
	_ = b
}

func TestProjectIndexLifecycleOperationsReplayEpochAndStrictKey(t *testing.T) {
	store := projectstore.NewStore(t.TempDir())
	project := testProject(t, store, "one")
	provider, err := NewProvider(store, ProviderOptions{StreamEpoch: "test", JournalEntries: 2, LiveCapacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if err := provider.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry := syncengine.NewProviderRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Open(context.Background(), syncengine.Principal{ID: "user"}, protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: "wrong"}, nil); err == nil {
		t.Fatal("wrong singleton key authorized")
	}
	if _, err := registry.Open(context.Background(), syncengine.Principal{}, protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: ProjectIndexResourceID}, nil); err == nil {
		t.Fatal("empty principal authorized")
	}
	opened := openProjectIndex(t, provider, nil)
	defer opened.Close()
	if opened.Sequence != 0 || opened.Snapshot.ResourceRevision != "0" {
		t.Fatalf("initial barrier = seq %d revision %s", opened.Sequence, opened.Snapshot.ResourceRevision)
	}

	publish := func(change CommittedChange) {
		t.Helper()
		if err := provider.PublishCommitted(change); err != nil {
			t.Fatal(err)
		}
		if err := provider.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	project, err = store.Rename(project.ID, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	publish(CommittedChange{Kind: CommittedProjectUpsert, ProjectID: project.ID, Project: &project})
	archived, err := store.Archive(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	publish(CommittedChange{Kind: CommittedProjectUpsert, ProjectID: project.ID, Project: &archived})
	restored, err := store.Restore(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	publish(CommittedChange{Kind: CommittedProjectUpsert, ProjectID: project.ID, Project: &restored})
	for want := uint64(1); want <= 3; want++ {
		entry := <-opened.Changes
		if entry.Sequence != want || entry.Change.ResourceRevision != protocol.ResourceRevision(strconv.FormatUint(want, 10)) {
			t.Fatalf("lifecycle entry = seq %d revision %s", entry.Sequence, entry.Change.ResourceRevision)
		}
	}
	resume := &protocol.ResumeToken{StreamEpoch: opened.StreamEpoch, Sequence: "1"}
	replayed := openProjectIndex(t, provider, resume)
	defer replayed.Close()
	if replayed.Decision.Classification != syncengine.ResumeReplayAvailable || len(replayed.Decision.Entries) != 2 {
		t.Fatalf("replay decision = %#v", replayed.Decision)
	}
	tooOld := openProjectIndex(t, provider, &protocol.ResumeToken{StreamEpoch: opened.StreamEpoch, Sequence: "0"})
	if tooOld.Decision.Classification != syncengine.ResumeTooOld || !tooOld.Decision.IsResync() {
		t.Fatalf("too old decision = %#v", tooOld.Decision)
	}
	tooOld.Close()
	oldEpoch := opened.StreamEpoch
	if err := provider.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	newOpen := openProjectIndex(t, provider, nil)
	defer newOpen.Close()
	if newOpen.StreamEpoch == oldEpoch {
		t.Fatal("warm did not create a new epoch")
	}
	mismatch := openProjectIndex(t, provider, &protocol.ResumeToken{StreamEpoch: oldEpoch, Sequence: "3"})
	if mismatch.Decision.Classification != syncengine.ResumeEpochMismatch || !mismatch.Decision.IsResync() {
		t.Fatalf("epoch mismatch decision = %#v", mismatch.Decision)
	}
	mismatch.Close()
	if err := store.Delete(project.ID); err != nil {
		t.Fatal(err)
	}
	// Delete after a rebuild is a typed remove, and is the only project-index
	// event involved in a project/session cascade.
	publish(CommittedChange{Kind: CommittedProjectRemove, ProjectID: project.ID})
	if newOpen.Snapshot.ResourceRevision != "0" {
		t.Fatalf("snapshot barrier revision unexpectedly changed: %s", newOpen.Snapshot.ResourceRevision)
	}
}

func TestProjectIndexOverflowSignalsResyncAndCloseCancelsBlob(t *testing.T) {
	store := projectstore.NewStore(t.TempDir())
	project := testProject(t, store, "one")
	provider, err := NewProvider(store, ProviderOptions{StreamEpoch: "test", LiveCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened := openProjectIndex(t, provider, nil)
	defer opened.Close()
	for i := 0; i < 2; i++ {
		value, renameErr := store.Rename(project.ID, string(rune('a'+i)))
		if renameErr != nil {
			t.Fatal(renameErr)
		}
		if err := provider.PublishCommitted(CommittedChange{Kind: CommittedProjectUpsert, ProjectID: value.ID, Project: &value}); err != nil {
			t.Fatal(err)
		}
	}
	if err := provider.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case terminal := <-opened.Terminal:
		if terminal.Reason != syncengine.LiveTerminalOverflow {
			t.Fatalf("terminal reason = %s", terminal.Reason)
		}
	case <-time.After(time.Second):
		t.Fatal("overflow did not terminate subscriber")
	}
	provider.Close()

	blocking := &blockingBlobWriter{started: make(chan struct{}), release: make(chan struct{})}
	provider, err = NewProvider(store, ProviderOptions{StreamEpoch: "test", InlineSnapshotBytes: 1, BlobWriter: blocking})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	type openOutcome struct {
		opened syncengine.OpenedResource
		err    error
	}
	result := make(chan openOutcome, 1)
	go func() {
		opened, openErr := provider.Open(ctx, protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: ProjectIndexResourceID}, nil)
		result <- openOutcome{opened: opened, err: openErr}
	}()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("blob writer was not called")
	}
	value, err := store.Rename(project.ID, "queued mutation")
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedProjectUpsert, ProjectID: value.ID, Project: &value}); err != nil {
		t.Fatal(err)
	}
	close(blocking.release)
	var barrierOpened syncengine.OpenedResource
	select {
	case outcome := <-result:
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		barrierOpened = outcome.opened
	case <-time.After(time.Second):
		t.Fatal("blob open did not finish")
	}
	cancel()
	if barrierOpened.Sequence != 0 {
		t.Fatalf("blob snapshot barrier sequence = %d, want 0", barrierOpened.Sequence)
	}
	if err := provider.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	entry := <-barrierOpened.Changes
	if entry.Sequence != 1 {
		t.Fatalf("queued mutation sequence = %d, want 1", entry.Sequence)
	}
	barrierOpened.Close()
	provider.Close()

	// A cancellable Blob open must not leave a registered subscriber behind.
	blocking = &blockingBlobWriter{started: make(chan struct{}), release: make(chan struct{})}
	provider, err = NewProvider(store, ProviderOptions{StreamEpoch: "test", InlineSnapshotBytes: 1, BlobWriter: blocking})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	result = make(chan openOutcome, 1)
	go func() {
		opened, openErr := provider.Open(ctx, protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: ProjectIndexResourceID}, nil)
		result <- openOutcome{opened: opened, err: openErr}
	}()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("cancellable blob writer was not called")
	}
	cancel()
	select {
	case outcome := <-result:
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("cancelled blob open error = %v", outcome.err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled blob open did not return")
	}
	provider.Close()
}

func TestInvalidationDropsQueuedChangesBeforeDurableRebuild(t *testing.T) {
	store := projectstore.NewStore(t.TempDir())
	project := testProject(t, store, "one")
	entered := make(chan struct{})
	release := make(chan struct{})
	var blockOnce sync.Once
	provider, err := NewProvider(store, ProviderOptions{
		StreamEpoch: "test", ProjectorQueueCapacity: 1,
		BeforeApply: func(CommittedChange) {
			blockOnce.Do(func() {
				close(entered)
				<-release
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if err := provider.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}

	old, err := store.Rename(project.ID, "old")
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedProjectUpsert, ProjectID: project.ID, Project: &old}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("projector did not enter blocking task")
	}
	queued, err := store.Rename(project.ID, "queued stale")
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedProjectUpsert, ProjectID: project.ID, Project: &queued}); err != nil {
		t.Fatal(err)
	}
	final, err := store.Rename(project.ID, "final durable value")
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedProjectUpsert, ProjectID: project.ID, Project: &final}); !errors.Is(err, ErrProjectorQueueFull) {
		t.Fatalf("overflow publish error = %v", err)
	}
	close(release)
	if err := provider.Flush(context.Background()); !errors.Is(err, ErrProviderInvalid) {
		t.Fatalf("flush after overflow error = %v", err)
	}

	opened := openProjectIndex(t, provider, nil)
	defer opened.Close()
	var snapshot ProjectIndexSnapshot
	if err := json.Unmarshal(opened.Snapshot.Content.InlineBytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Projects) != 1 || snapshot.Projects[0].DisplayName != "final durable value" {
		t.Fatalf("rebuild was rolled back by stale queued task: %#v", snapshot.Projects)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("rebuilt snapshot is invalid: %v", err)
	}
}

func TestRebuildReconcilesArchiveTransitionsWithoutDuplicateProjects(t *testing.T) {
	store := projectstore.NewStore(t.TempDir())
	project := testProject(t, store, "one")
	phase := 0
	provider, err := NewProvider(store, ProviderOptions{
		StreamEpoch: "test",
		BeforeWarm: func() {
			var transitionErr error
			if phase == 1 {
				_, transitionErr = store.Archive(project.ID)
			} else if phase == 2 {
				_, transitionErr = store.Restore(project.ID)
			}
			if transitionErr != nil {
				panic(transitionErr)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	for _, wantArchived := range []bool{true, false} {
		phase++
		if err := provider.Warm(context.Background()); err != nil {
			t.Fatal(err)
		}
		opened := openProjectIndex(t, provider, nil)
		var snapshot ProjectIndexSnapshot
		if err := json.Unmarshal(opened.Snapshot.Content.InlineBytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		opened.Close()
		if err := snapshot.Validate(); err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Projects) != 1 || snapshot.Projects[0].Archived != wantArchived {
			t.Fatalf("archive transition snapshot = %#v, want archived=%v", snapshot.Projects, wantArchived)
		}
	}
}

func TestProjectIndexRejectsMismatchedBlobDescriptor(t *testing.T) {
	store := projectstore.NewStore(t.TempDir())
	testProject(t, store, "one")
	digest := "0000000000000000000000000000000000000000000000000000000000000000"
	for _, descriptor := range []protocol.BlobDescriptor{
		{ID: "bad-type", URL: "/bad-type", ContentType: "text/plain", Size: 1, SHA256: digest, ETag: `"` + digest + `"`, ExpiresAt: "2999-01-01T00:00:00Z"},
		{ID: "bad-size", URL: "/bad-size", ContentType: "application/json", Size: 1, SHA256: digest, ETag: `"` + digest + `"`, ExpiresAt: "2999-01-01T00:00:00Z"},
	} {
		provider, err := NewProvider(store, ProviderOptions{StreamEpoch: "test", InlineSnapshotBytes: 1, BlobWriter: staticBlobWriter{descriptor: descriptor}})
		if err != nil {
			t.Fatal(err)
		}
		if err := provider.Warm(context.Background()); err != nil {
			provider.Close()
			t.Fatal(err)
		}
		if _, err := provider.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: ProjectIndexResourceID}, nil); err == nil {
			provider.Close()
			t.Fatalf("mismatched descriptor accepted: %#v", descriptor)
		}
		provider.Close()
	}
}

type blockingBlobWriter struct {
	started chan struct{}
	release chan struct{}
}

func (w *blockingBlobWriter) Put(ctx context.Context, contentType string, content []byte) (protocol.BlobDescriptor, error) {
	close(w.started)
	select {
	case <-w.release:
		digest := sha256.Sum256(content)
		hexDigest := hex.EncodeToString(digest[:])
		return protocol.BlobDescriptor{ID: "x", URL: "/x", ContentType: contentType, Size: uint64(len(content)), SHA256: hexDigest, ETag: `"` + hexDigest + `"`, ExpiresAt: "2999-01-01T00:00:00Z"}, nil
	case <-ctx.Done():
		return protocol.BlobDescriptor{}, ctx.Err()
	}
}

type staticBlobWriter struct {
	descriptor protocol.BlobDescriptor
}

func (w staticBlobWriter) Put(context.Context, string, []byte) (protocol.BlobDescriptor, error) {
	return w.descriptor, nil
}

var _ blobstore.Writer = (*blockingBlobWriter)(nil)
