package webapp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

func TestBlobHTTPRouteReadsFullStoredBlob(t *testing.T) {
	server, _, app := newWebTestAppServerWithRunner(t, webTestRunner{})
	payload := []byte(`{"blob":"route-level"}`)
	descriptor, err := app.blobStore.Put(context.Background(), "application/json", payload)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, server.URL+descriptor.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != descriptor.ContentType || !bytes.Equal(body, payload) {
		t.Fatalf("blob response status=%d content-type=%q body=%q", response.StatusCode, response.Header.Get("Content-Type"), body)
	}
	if response.Header.Get("ETag") != descriptor.ETag || response.Header.Get("X-Content-SHA256") != descriptor.SHA256 || response.Header.Get("Accept-Ranges") != "bytes" || response.Header.Get("Expires") == "" {
		t.Fatalf("blob response safety headers=%v", response.Header)
	}

	headRequest, err := http.NewRequest(http.MethodHead, server.URL+descriptor.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	headRequest.Header.Set("Authorization", "Bearer "+testToken)
	headResponse, err := http.DefaultClient.Do(headRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer headResponse.Body.Close()
	headBody, _ := io.ReadAll(headResponse.Body)
	if headResponse.StatusCode != http.StatusOK || len(headBody) != 0 || headResponse.Header.Get("Content-Length") != strconv.Itoa(len(payload)) || headResponse.Header.Get("ETag") != descriptor.ETag {
		t.Fatalf("blob HEAD status=%d length=%q etag=%q body=%q", headResponse.StatusCode, headResponse.Header.Get("Content-Length"), headResponse.Header.Get("ETag"), headBody)
	}
}

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
