package sessionindex

import (
	"context"
	"testing"

	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

type cancelAwareWriter struct {
	started chan struct{}
	cancel  chan struct{}
}

func (w *cancelAwareWriter) Put(ctx context.Context, _ string, _ []byte) (protocol.BlobDescriptor, error) {
	close(w.started)
	select {
	case <-ctx.Done():
		close(w.cancel)
		return protocol.BlobDescriptor{}, ctx.Err()
	}
}

func TestProviderCloseCancelsExternalBlobWriter(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	_ = testSession(t, store, "s", "p", "s")
	writer := &cancelAwareWriter{started: make(chan struct{}), cancel: make(chan struct{})}
	provider, err := NewProvider(store, ProviderOptions{BlobWriter: writer, InlineSnapshotBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	opened := make(chan error, 1)
	go func() {
		_, openErr := provider.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionIndex, ID: "p"}, nil)
		opened <- openErr
	}()
	<-writer.started
	closed := make(chan struct{})
	go func() {
		provider.Close()
		close(closed)
	}()
	select {
	case <-writer.cancel:
	case <-closed:
		t.Fatal("provider closed before external writer observed cancellation")
	}
	<-closed
	if err := <-opened; err == nil {
		t.Fatal("blocked blob open unexpectedly succeeded")
	}
}
