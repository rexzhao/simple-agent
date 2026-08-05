package blobstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

// Writer is the provider's cancellable immutable data-plane boundary.
type Writer interface {
	Put(ctx context.Context, contentType string, content []byte) (protocol.BlobDescriptor, error)
}

type Options struct {
	BaseURL string
	TTL     time.Duration
	Now     func() time.Time

	MaxEntries   int
	MaxBytes     int64
	MaxBlobBytes int64

	// JanitorInterval starts a bounded cleanup task. JanitorTick is intended
	// for deterministic tests; when supplied, cleanup waits for a value on it
	// instead of using a wall-clock ticker.
	JanitorInterval time.Duration
	JanitorTick     <-chan time.Time
	JanitorAck      chan<- struct{}
}

type Store struct {
	mu      sync.Mutex
	blobs   map[string]blob
	bytes   int64
	ttl     time.Duration
	baseURL string
	now     func() time.Time

	maxEntries   int
	maxBytes     int64
	maxBlobBytes int64

	janitorAck    chan<- struct{}
	closed        bool
	janitorCancel context.CancelFunc
	janitorDone   chan struct{}
}

type blob struct {
	descriptor protocol.BlobDescriptor
	content    []byte // immutable after insertion; readers may retain the slice
	expires    time.Time
}

// Reader is a read-only view over immutable blob bytes. Open never copies the
// blob; a reader remains safe after expiry reclamation or Store.Close because
// the backing immutable allocation is retained by this handle.
type Reader struct {
	descriptor protocol.BlobDescriptor
	content    []byte
}

func (r *Reader) ReadAt(p []byte, offset int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r == nil || offset < 0 {
		return 0, io.EOF
	}
	if offset >= int64(len(r.content)) {
		return 0, io.EOF
	}
	n := copy(p, r.content[offset:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *Reader) Descriptor() protocol.BlobDescriptor {
	if r == nil {
		return protocol.BlobDescriptor{}
	}
	return r.descriptor
}

var (
	ErrNotFound = errors.New("blob not found")
	ErrExpired  = errors.New("blob expired")
	ErrClosed   = errors.New("blob store is closed")
	ErrCapacity = errors.New("blob store capacity exceeded")
)

func New(options Options) (*Store, error) {
	if options.TTL == 0 {
		options.TTL = 5 * time.Minute
	}
	if options.TTL <= 0 {
		return nil, fmt.Errorf("blob ttl must be positive")
	}
	if options.JanitorInterval == 0 && options.JanitorTick == nil {
		options.JanitorInterval = time.Minute
	}
	if options.MaxEntries == 0 {
		options.MaxEntries = 256
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = 64 * 1024 * 1024
	}
	if options.MaxBlobBytes == 0 {
		options.MaxBlobBytes = 16 * 1024 * 1024
	}
	if options.MaxEntries <= 0 || options.MaxBytes <= 0 || options.MaxBlobBytes <= 0 || options.MaxBlobBytes > options.MaxBytes {
		return nil, fmt.Errorf("blob bounds are invalid")
	}
	if strings.TrimSpace(options.BaseURL) == "" {
		options.BaseURL = "/api/blobs/"
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	store := &Store{
		blobs: make(map[string]blob), ttl: options.TTL,
		baseURL: strings.TrimRight(options.BaseURL, "/") + "/", now: options.Now,
		maxEntries: options.MaxEntries, maxBytes: options.MaxBytes, maxBlobBytes: options.MaxBlobBytes,
		janitorAck: options.JanitorAck,
	}
	if options.JanitorTick != nil || options.JanitorInterval > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		store.janitorCancel = cancel
		store.janitorDone = make(chan struct{})
		go store.runJanitor(ctx, options.JanitorInterval, options.JanitorTick)
	}
	return store, nil
}

func (s *Store) Put(ctx context.Context, contentType string, content []byte) (protocol.BlobDescriptor, error) {
	return s.put(ctx, contentType, content, false)
}

// PutFresh stores a new descriptor even when the same immutable bytes already
// exist. It is used by consumers that intentionally refresh a descriptor
// before its expiry; ordinary Put remains content-addressed and deduplicating.
func (s *Store) PutFresh(ctx context.Context, contentType string, content []byte) (protocol.BlobDescriptor, error) {
	return s.put(ctx, contentType, content, true)
}

func (s *Store) put(ctx context.Context, contentType string, content []byte, forceFresh bool) (protocol.BlobDescriptor, error) {
	if s == nil {
		return protocol.BlobDescriptor{}, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return protocol.BlobDescriptor{}, ctx.Err()
	default:
	}
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		return protocol.BlobDescriptor{}, fmt.Errorf("blob content type is required")
	}
	if int64(len(content)) > s.maxBlobBytes {
		return protocol.BlobDescriptor{}, ErrCapacity
	}
	digest := sha256.Sum256(content)
	digestHex := hex.EncodeToString(digest[:])
	now := s.now().UTC()
	expires := now.Add(s.ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return protocol.BlobDescriptor{}, ErrClosed
	}
	s.sweepLocked(now)
	if !forceFresh {
		for _, item := range s.blobs {
			if item.descriptor.SHA256 == digestHex && item.descriptor.ContentType == contentType && now.Before(item.expires) {
				return item.descriptor, nil
			}
		}
	}
	if len(s.blobs) >= s.maxEntries || s.bytes+int64(len(content)) > s.maxBytes {
		return protocol.BlobDescriptor{}, ErrCapacity
	}
	select {
	case <-ctx.Done():
		return protocol.BlobDescriptor{}, ctx.Err()
	default:
	}
	idBytes := make([]byte, 18)
	if _, err := rand.Read(idBytes); err != nil {
		return protocol.BlobDescriptor{}, err
	}
	id := hex.EncodeToString(idBytes)
	descriptor := protocol.BlobDescriptor{
		ID: id, URL: s.baseURL + id, ContentType: contentType,
		Size: uint64(len(content)), SHA256: digestHex,
		ETag: `"` + digestHex + `"`, ExpiresAt: expires.Format(time.RFC3339Nano),
	}
	if err := protocol.ValidateBlobDescriptor(descriptor); err != nil {
		return protocol.BlobDescriptor{}, err
	}
	copyContent := append([]byte(nil), content...)
	s.blobs[id] = blob{descriptor: descriptor, content: copyContent, expires: expires}
	s.bytes += int64(len(copyContent))
	return descriptor, nil
}

func (s *Store) Open(id string) (*Reader, error) {
	if s == nil {
		return nil, ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	item, ok := s.blobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	if !s.now().Before(item.expires) {
		s.removeLocked(id, item)
		return nil, ErrExpired
	}
	return &Reader{descriptor: item.descriptor, content: item.content}, nil
}

func (s *Store) Descriptor(id string) (protocol.BlobDescriptor, error) {
	reader, err := s.Open(strings.TrimSpace(id))
	if err != nil {
		return protocol.BlobDescriptor{}, err
	}
	return reader.Descriptor(), nil
}

func (s *Store) runJanitor(ctx context.Context, interval time.Duration, tick <-chan time.Time) {
	defer close(s.janitorDone)
	if tick != nil {
		for {
			select {
			case <-ctx.Done():
				return
			case now, ok := <-tick:
				if !ok {
					return
				}
				s.SweepExpiredAt(now)
				if s.janitorAck != nil {
					select {
					case s.janitorAck <- struct{}{}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.SweepExpiredAt(now)
		}
	}
}

// SweepExpired is explicit and deterministic; the janitor uses the same
// operation when configured.
func (s *Store) SweepExpired() int {
	if s == nil {
		return 0
	}
	return s.SweepExpiredAt(s.now().UTC())
}

func (s *Store) SweepExpiredAt(now time.Time) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweepLocked(now.UTC())
}

func (s *Store) sweepLocked(now time.Time) int {
	removed := 0
	for id, item := range s.blobs {
		if !now.Before(item.expires) {
			s.removeLocked(id, item)
			removed++
		}
	}
	return removed
}

func (s *Store) removeLocked(id string, item blob) {
	delete(s.blobs, id)
	s.bytes -= int64(len(item.content))
	if s.bytes < 0 {
		s.bytes = 0
	}
}

func (s *Store) Close() {
	if s == nil {
		return
	}
	if s.janitorCancel != nil {
		s.janitorCancel()
		<-s.janitorDone
	}
	s.mu.Lock()
	s.closed = true
	s.blobs = make(map[string]blob)
	s.bytes = 0
	s.mu.Unlock()
}

// Stats exposes bounded accounting without exposing blob contents.
type Stats struct {
	Entries    int
	Bytes      int64
	MaxEntries int
	MaxBytes   int64
}

func (s *Store) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Entries: len(s.blobs), Bytes: s.bytes, MaxEntries: s.maxEntries, MaxBytes: s.maxBytes}
}

// ServeHTTP serves one immutable blob. Capability authentication is kept at
// the webapp boundary; this handler never logs request headers or content.
func (s *Store) ServeHTTP(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	reader, err := s.Open(id)
	if err != nil {
		if errors.Is(err, ErrClosed) {
			w.WriteHeader(http.StatusGone)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
		return
	}
	descriptor := reader.Descriptor()
	w.Header().Set("Content-Type", descriptor.ContentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", descriptor.ETag)
	w.Header().Set("X-Content-SHA256", descriptor.SHA256)
	if expiry, parseErr := time.Parse(time.RFC3339Nano, descriptor.ExpiresAt); parseErr == nil {
		w.Header().Set("Expires", expiry.UTC().Format(http.TimeFormat))
	}
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == descriptor.ETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	start, end, partial, ok := parseRange(r.Header.Get("Range"), int64(descriptor.Size))
	if !ok {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(int64(descriptor.Size), 10))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, descriptor.Size))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodGet {
		// Reader implements io.ReaderAt but never exposes its backing slice.
		// SectionReader bounds the read to the requested range and CopyN does
		// not materialize the complete immutable blob for a small range.
		section := io.NewSectionReader(reader, start, end-start+1)
		_, _ = io.CopyN(w, section, end-start+1)
	}
}

func parseRange(value string, size int64) (int64, int64, bool, bool) {
	if strings.TrimSpace(value) == "" {
		return 0, size - 1, false, size >= 0
	}
	if size <= 0 || !strings.HasPrefix(value, "bytes=") {
		return 0, 0, false, false
	}
	parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(value, "bytes=")), "-")
	if len(parts) != 2 {
		return 0, 0, false, false
	}
	if strings.TrimSpace(parts[0]) == "" {
		n, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, true
	}
	start, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, false
	}
	end := size - 1
	if strings.TrimSpace(parts[1]) != "" {
		end, err = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || end < start {
			return 0, 0, false, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true, true
}

var _ Writer = (*Store)(nil)
