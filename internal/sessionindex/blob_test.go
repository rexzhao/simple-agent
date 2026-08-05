package sessionindex

import (
	"context"
	"fmt"
	"testing"

	"github.com/rexzhao/simple-agent/internal/blobstore"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestLargeSnapshotUsesImmutableBlobDescriptor(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	for i := 0; i < 300; i++ {
		_ = testSession(t, store, fmt.Sprintf("session-%04d", i), "p", "a very long session name that makes the index snapshot large")
	}
	blobs, err := blobstore.New(blobstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()
	provider, err := NewProvider(store, ProviderOptions{BlobWriter: blobs})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	opened, err := provider.Open(context.Background(), resourceKey("p"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	descriptor, ok := opened.Snapshot.Content.BlobDescriptor()
	if !ok {
		t.Fatalf("large snapshot was inline: %d bytes", len(opened.Snapshot.Content.InlineBytes()))
	}
	if descriptor.Size <= DefaultInlineSnapshot || descriptor.ContentType != "application/json" {
		t.Fatalf("descriptor=%#v", descriptor)
	}
}

func resourceKey(project string) (key protocol.ResourceKey) {
	return protocol.ResourceKey{Type: protocol.ResourceTypeSessionIndex, ID: project}
}
