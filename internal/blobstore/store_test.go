package blobstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStoreHTTPGetHeadRangeAndExpiry(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	store, err := New(Options{Now: func() time.Time { return now }, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	descriptor, err := store.Put(context.Background(), "application/json", []byte("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", descriptor.URL, nil)
	request.Header.Set("Range", "bytes=2-5")
	response := httptest.NewRecorder()
	store.ServeHTTP(response, request, descriptor.ID)
	if response.Code != 206 || response.Header().Get("Content-Range") != "bytes 2-5/10" || response.Body.String() != "2345" {
		t.Fatalf("range response code=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	if response.Header().Get("ETag") != descriptor.ETag || response.Header().Get("X-Content-SHA256") != descriptor.SHA256 {
		t.Fatal("hash metadata missing")
	}
	if response.Header().Get("Expires") != now.Add(time.Minute).UTC().Format(http.TimeFormat) {
		t.Fatalf("HTTP Expires = %q", response.Header().Get("Expires"))
	}
	head := httptest.NewRecorder()
	store.ServeHTTP(head, httptest.NewRequest("HEAD", descriptor.URL, nil), descriptor.ID)
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != "10" {
		t.Fatalf("head response code=%d body=%q headers=%v", head.Code, head.Body.String(), head.Header())
	}
	conditional := httptest.NewRecorder()
	conditionalRequest := httptest.NewRequest("GET", descriptor.URL, nil)
	conditionalRequest.Header.Set("If-None-Match", descriptor.ETag)
	store.ServeHTTP(conditional, conditionalRequest, descriptor.ID)
	if conditional.Code != 304 {
		t.Fatalf("conditional code=%d", conditional.Code)
	}
	now = now.Add(2 * time.Minute)
	expired := httptest.NewRecorder()
	store.ServeHTTP(expired, httptest.NewRequest("GET", descriptor.URL, nil), descriptor.ID)
	if expired.Code != 404 {
		t.Fatalf("expired code=%d", expired.Code)
	}
}

func TestStoreRejectsMalformedRange(t *testing.T) {
	store, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	descriptor, err := store.Put(context.Background(), "text/plain", []byte(strings.Repeat("x", 5)))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest("GET", descriptor.URL, nil)
	request.Header.Set("Range", "bytes=99-100")
	store.ServeHTTP(response, request, descriptor.ID)
	if response.Code != 416 {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("code=%d body=%q", response.Code, body)
	}
}

func TestStoreCapacityDedupJanitorAndReaderBounds(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	ticks := make(chan time.Time)
	acks := make(chan struct{})
	store, err := New(Options{
		Now: func() time.Time { return now }, TTL: time.Minute,
		MaxEntries: 2, MaxBytes: 6, MaxBlobBytes: 4,
		JanitorTick: ticks, JanitorAck: acks,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.Put(context.Background(), "text/plain", []byte("1234"))
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := store.Put(context.Background(), "text/plain", []byte("1234"))
	if err != nil || duplicate.ID != first.ID {
		t.Fatalf("dedup descriptor=%#v err=%v", duplicate, err)
	}
	if _, err := store.Put(context.Background(), "text/plain", []byte("5678")); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity error=%v", err)
	}
	if _, err := store.Put(context.Background(), "text/plain", []byte("12345")); !errors.Is(err, ErrCapacity) {
		t.Fatalf("single blob capacity error=%v", err)
	}
	reader, err := store.Open(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	view := make([]byte, 1)
	if n, err := reader.ReadAt(view, 1); err != nil || n != 1 || string(view) != "2" {
		t.Fatalf("reader view=%q n=%d err=%v", view, n, err)
	}
	view[0] = 'x'
	unchanged := make([]byte, 4)
	if n, err := reader.ReadAt(unchanged, 0); err != nil || n != len(unchanged) || string(unchanged) != "1234" {
		t.Fatalf("reader backing changed through read buffer: %q n=%d err=%v", unchanged, n, err)
	}
	if n, err := reader.ReadAt(nil, 0); n != 0 || err != nil {
		t.Fatalf("zero-length ReadAt=(%d,%v), want (0,nil)", n, err)
	}
	now = now.Add(2 * time.Minute)
	ticks <- now
	<-acks
	if stats := store.Stats(); stats.Entries != 0 || stats.Bytes != 0 {
		t.Fatalf("janitor did not reclaim expired blobs: %#v", stats)
	}
	if _, err := store.Open(first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired blob remained available: %v", err)
	}
}
