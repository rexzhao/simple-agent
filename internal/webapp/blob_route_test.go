package webapp

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

func TestBlobHTTPRouteUsesCapabilityAndSupportsRange(t *testing.T) {
	server, _, app := newWebTestAppServerWithRunner(t, webTestRunner{})
	descriptor, err := app.blobStore.Put(context.Background(), "application/json", []byte(`{"large":true}`))
	if err != nil {
		t.Fatal(err)
	}
	unauthorizedRequest, err := http.NewRequest(http.MethodGet, server.URL+descriptor.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized, err := http.DefaultClient.Do(unauthorizedRequest)
	if err != nil {
		t.Fatal(err)
	}
	if unauthorized.StatusCode != http.StatusUnauthorized {
		unauthorized.Body.Close()
		t.Fatalf("unauthorized status=%d", unauthorized.StatusCode)
	}
	unauthorized.Body.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+descriptor.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Range", "bytes=0-7")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusPartialContent || string(body) != `{"large"` || response.Header.Get("Content-Length") != "8" {
		t.Fatalf("blob response status=%d body=%q headers=%v", response.StatusCode, body, response.Header)
	}
	if response.Header.Get("ETag") != descriptor.ETag || response.Header.Get("X-Content-SHA256") != descriptor.SHA256 {
		t.Fatal("blob hash headers missing")
	}
	if err := protocol.ValidateBlobDescriptor(descriptor); err != nil {
		t.Fatal(err)
	}
}
