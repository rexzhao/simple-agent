package webapp

import (
	"context"
	"testing"

	"github.com/rexzhao/simple-agent/internal/blobstore"
	"github.com/rexzhao/simple-agent/internal/execution"
)

func TestInjectedBlobStoreIsNotOwnedByServer(t *testing.T) {
	home := t.TempDir()
	service, err := execution.NewService(home)
	if err != nil {
		t.Fatal(err)
	}
	writeWebTestConfig(t, home)
	external, err := blobstore.New(blobstore.Options{JanitorInterval: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer external.Close()
	app, err := NewServer(ServerOptions{Context: context.Background(), Service: service, Token: testToken, CWD: home, BlobStore: external})
	if err != nil {
		t.Fatal(err)
	}
	app.Close()
	if _, err := external.Put(context.Background(), "text/plain", []byte("still alive")); err != nil {
		t.Fatalf("injected blob store was closed by Server.Close: %v", err)
	}
}

func TestServerAssemblyFailureClosesInternalBlobWithoutReturningServer(t *testing.T) {
	// A service without its durable session store fails provider assembly after
	// NewServer has created its internal blob store. This is primarily a
	// regression guard for the cleanup path; no server is returned to leak it.
	app, err := NewServer(ServerOptions{Context: context.Background(), Service: &execution.Service{}, Token: testToken})
	if err == nil || app != nil {
		t.Fatalf("invalid service assembly app=%#v err=%v", app, err)
	}
}
