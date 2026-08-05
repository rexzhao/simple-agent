package sessionindex

import (
	"context"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/blobstore"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

type gatedWriter struct {
	inner   BlobWriter
	started chan struct{}
	release chan struct{}
}

func (w *gatedWriter) Put(ctx context.Context, contentType string, content []byte) (protocol.BlobDescriptor, error) {
	close(w.started)
	select {
	case <-ctx.Done():
		return protocol.BlobDescriptor{}, ctx.Err()
	case <-w.release:
	}
	return w.inner.Put(ctx, contentType, content)
}

func TestOpenBarrierBuffersConcurrentMutationBehindSnapshot(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	state := testSession(t, store, "s", "p", "original")
	blobs, err := blobstore.New(blobstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()
	writer := &gatedWriter{inner: blobs, started: make(chan struct{}), release: make(chan struct{})}
	provider, err := NewProvider(store, ProviderOptions{BlobWriter: writer, InlineSnapshotBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	openedResult := make(chan struct {
		opened syncengine.OpenedResource
		err    error
	}, 1)
	go func() {
		opened, openErr := provider.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionIndex, ID: "p"}, nil)
		openedResult <- struct {
			opened syncengine.OpenedResource
			err    error
		}{opened, openErr}
	}()
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("snapshot did not reach blob barrier")
	}
	rename := SummaryFromSession(state, false)
	rename.DisplayName = "renamed"
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedSessionUpsert, ProjectID: "p", SessionID: "s", Summary: &rename}); err != nil {
		t.Fatal(err)
	}
	close(writer.release)
	result := <-openedResult
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.opened.Close()
	if result.opened.Sequence != 0 || result.opened.Snapshot.ResourceRevision != "0" {
		t.Fatalf("open barrier = sequence %d revision %s", result.opened.Sequence, result.opened.Snapshot.ResourceRevision)
	}
	if err := provider.Flush(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	select {
	case entry := <-result.opened.Changes:
		if entry.Sequence != 1 || entry.Change.ResourceRevision != "1" {
			t.Fatalf("buffered mutation = %#v", entry)
		}
	case <-time.After(time.Second):
		t.Fatal("mutation was lost across open barrier")
	}
}
