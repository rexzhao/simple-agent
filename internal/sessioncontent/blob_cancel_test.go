package sessioncontent

import (
	"context"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

type cancelAwareContentBlobWriter struct {
	started  chan struct{}
	canceled chan struct{}
}

func (w *cancelAwareContentBlobWriter) Put(ctx context.Context, _ string, _ []byte) (protocol.BlobDescriptor, error) {
	select {
	case <-w.started:
	default:
		close(w.started)
	}
	<-ctx.Done()
	select {
	case <-w.canceled:
	default:
		close(w.canceled)
	}
	return protocol.BlobDescriptor{}, ctx.Err()
}

func TestProviderShutdownCancelsSnapshotBlobWrite(t *testing.T) {
	store, session := newContentTestStore(t, "session-blob-shutdown")
	writer := &cancelAwareContentBlobWriter{started: make(chan struct{}), canceled: make(chan struct{})}
	p, err := NewProvider(store, ProviderOptions{InlineSnapshotBytes: 1, BlobWriter: writer})
	if err != nil {
		t.Fatal(err)
	}
	openResult := make(chan error, 1)
	go func() {
		_, openErr := p.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: session.ID}, nil)
		openResult <- openErr
	}()
	select {
	case <-writer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot BlobWriter was not reached")
	}
	p.Close()
	select {
	case <-writer.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("provider shutdown did not cancel snapshot BlobWriter")
	}
	select {
	case <-openResult:
	case <-time.After(2 * time.Second):
		t.Fatal("Open remained blocked after provider shutdown")
	}
}
