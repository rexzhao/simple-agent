package sessionindex

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/blobstore"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

type secondOpenBlockingWriter struct {
	inner   BlobWriter
	calls   atomic.Int32
	started chan struct{}
	cancel  chan struct{}
}

func (w *secondOpenBlockingWriter) Put(ctx context.Context, contentType string, content []byte) (protocol.BlobDescriptor, error) {
	if w.calls.Add(1) == 2 {
		close(w.started)
		select {
		case <-ctx.Done():
			close(w.cancel)
			return protocol.BlobDescriptor{}, ctx.Err()
		}
	}
	return w.inner.Put(ctx, contentType, content)
}

func TestCancelledBlobOpenDoesNotInvalidateHealthySubscription(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	state := testSession(t, store, "s", "p", "original")
	blobs, err := blobstore.New(blobstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()
	writer := &secondOpenBlockingWriter{inner: blobs, started: make(chan struct{}), cancel: make(chan struct{})}
	provider, err := NewProvider(store, ProviderOptions{BlobWriter: writer, InlineSnapshotBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()

	healthy := openIndex(t, provider, "p", nil)
	defer healthy.Close()
	secondContext, cancelSecond := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		_, openErr := provider.Open(secondContext, resourceKey("p"), nil)
		secondDone <- openErr
	}()
	<-writer.started
	cancelSecond()
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled blob Open=%v, want context.Canceled", err)
	}
	select {
	case <-writer.cancel:
	case <-time.After(time.Second):
		t.Fatal("blob writer did not observe request cancellation")
	}
	if err := provider.Flush(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}

	state.DisplayName = "after-cancel"
	state, err = store.SaveMetadata(state)
	if err != nil {
		t.Fatal(err)
	}
	summary := SummaryFromSession(state, false)
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedSessionUpsert, ProjectID: "p", SessionID: state.ID, Summary: &summary}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Flush(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	select {
	case terminal := <-healthy.Terminal:
		t.Fatalf("healthy subscription became terminal after local blob cancellation: %#v", terminal)
	case entry := <-healthy.Changes:
		if entry.Change.ResourceRevision != "1" || len(entry.Change.Operations) != 1 {
			t.Fatalf("healthy change=%#v", entry)
		}
	case <-time.After(time.Second):
		t.Fatal("healthy subscription did not receive subsequent mutation")
	}

	// The failed materialization did not damage the owner; a later open can
	// still materialize a fresh immutable descriptor from the same snapshot.
	later := openIndex(t, provider, "p", nil)
	later.Close()
}
