package sessionlog

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

const logFileName = "log.jsonl"

// Store manages sessions under a data root laid out as
// <root>/<project>/<session>/log.jsonl. A project is a workspace directory
// identified by ProjectID; a session is a directory holding one append-only
// log.
type Store struct {
	root string
}

// OpenStore opens (creating if needed) the data root directory.
func OpenStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("data root is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create data root: %w", err)
	}
	return &Store{root: root}, nil
}

// Root returns the data root directory.
func (s *Store) Root() string { return s.root }

// ProjectID returns the stable project identifier for a workspace
// directory: "p" followed by base32 of the first ten bytes of the SHA-256
// of the normalized absolute path. The same directory always maps to the
// same project, on the CLI and in the future Web UI alike, with no
// registration step. Path case is folded on case-insensitive platforms.
func ProjectID(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	abs = filepath.Clean(abs)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		abs = strings.ToLower(abs)
	}
	digest := sha256.Sum256([]byte(abs))
	return "p" + encodeBase32(digest[:10])
}

// ValidProjectID accepts the identifiers ProjectID generates.
func ValidProjectID(id string) bool {
	if len(id) != 17 || id[0] != 'p' {
		return false
	}
	for _, char := range id[1:] {
		if !strings.ContainsRune(base32Alphabet, char) {
			return false
		}
	}
	return true
}

// Create returns a new session inside the project. Creation is lazy:
// nothing touches the disk, so an empty session (one never sent a message)
// leaves no directory and never appears in List. The first Append
// materializes the log and writes the session.created record ahead of the
// appended one.
func (s *Store) Create(projectID, provider, model, cwd string) (*Session, error) {
	if !ValidProjectID(projectID) {
		return nil, fmt.Errorf("invalid project id %q", projectID)
	}
	id := newSessionID()
	createdAt := newTime()
	state := newState(id)
	state.Provider = provider
	state.Model = model
	state.CWD = cwd
	if created, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		state.CreatedAt = created
		state.UpdatedAt = created
	}
	return &Session{
		id:      id,
		project: projectID,
		store:   s,
		state:   state,
		pending: &pendingMeta{
			provider: provider, model: model, cwd: cwd, createdAt: createdAt,
		},
	}, nil
}

// Open loads an existing session, recovering its folded state from the log.
// The session ID is globally unique, so the project directory it lives in
// is found by scanning the data root. A run which was interrupted by a
// crash needs no repair record: its user input simply stays unanswered, and
// chatapp patches any dangling tool calls before the next request.
func (s *Store) Open(id string) (*Session, error) {
	if !ValidSessionID(id) {
		return nil, fmt.Errorf("invalid session id %q", id)
	}
	dir, err := s.sessionDir(id)
	if err != nil {
		return nil, err
	}
	log, err := OpenLog(filepath.Join(dir, logFileName))
	if err != nil {
		return nil, err
	}
	records, err := log.Records()
	if err != nil {
		log.Close()
		return nil, err
	}
	project := filepath.Base(filepath.Dir(dir))
	session := &Session{id: id, project: project, log: log, state: newState(id)}
	for _, record := range records {
		fold(session.state, record)
	}
	return session, nil
}

// sessionDir locates the directory of a session ID by scanning the project
// directories of the data root.
func (s *Store) sessionDir(id string) (string, error) {
	projects, err := os.ReadDir(s.root)
	if err != nil {
		return "", fmt.Errorf("read data root: %w", err)
	}
	for _, project := range projects {
		if !project.IsDir() || !ValidProjectID(project.Name()) {
			continue
		}
		dir := filepath.Join(s.root, project.Name(), id)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, nil
		}
	}
	return "", fmt.Errorf("open session %q: not found", id)
}

// Summary describes one stored session for listing.
type Summary struct {
	ID           string
	Title        string
	Provider     string
	Model        string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	MessageCount int
	LastSeq      int64
}

// List returns summaries of the project's stored sessions, newest first.
// Sessions with unreadable logs are skipped rather than failing the listing.
func (s *Store) List(projectID string) ([]Summary, error) {
	if !ValidProjectID(projectID) {
		return nil, fmt.Errorf("invalid project id %q", projectID)
	}
	entries, err := os.ReadDir(filepath.Join(s.root, projectID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read project directory: %w", err)
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() && ValidSessionID(entry.Name()) {
			ids = append(ids, entry.Name())
		}
	}
	summaries := make([]Summary, 0, len(ids))
	for _, id := range ids {
		records, err := readRecords(filepath.Join(s.root, projectID, id, logFileName))
		if err != nil {
			continue
		}
		state := newState(id)
		for _, record := range records {
			fold(state, record)
		}
		messages := 0
		for _, message := range state.Context {
			if message.Role == model.MessageRoleUser || message.Role == model.MessageRoleAssistant {
				messages++
			}
		}
		summaries = append(summaries, Summary{
			ID: id, Title: state.Title, Provider: state.Provider, Model: state.Model,
			CreatedAt: state.CreatedAt, UpdatedAt: state.UpdatedAt,
			MessageCount: messages, LastSeq: state.LastSeq,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if !summaries[i].UpdatedAt.Equal(summaries[j].UpdatedAt) {
			return summaries[i].UpdatedAt.After(summaries[j].UpdatedAt)
		}
		return summaries[i].ID > summaries[j].ID
	})
	return summaries, nil
}

// Session is one open session: its append-only log plus the live folded state.
// Append writes a record and advances the fold atomically under the session
// lock; Snapshot returns a deep copy safe for any reader.
type Session struct {
	id      string
	project string
	store   *Store
	log     *Log
	// pending carries the creation metadata of a session which has not
	// touched the disk yet; it is consumed by the first Append.
	pending *pendingMeta

	mu              sync.RWMutex
	state           *State
	currentRunValue string
	subs            map[chan struct{}]struct{}
}

type pendingMeta struct {
	provider  string
	model     string
	cwd       string
	createdAt string
}

// ID returns the session ID.
func (s *Session) ID() string { return s.id }

// Project returns the project the session belongs to.
func (s *Session) Project() string { return s.project }

// Dir returns the session's directory on disk. It exists only once the
// session has been materialized by its first record.
func (s *Session) Dir() string { return filepath.Join(s.store.root, s.project, s.id) }

// Append stores one record and advances the folded state, then notifies
// subscribers. The first append on a lazily created session materializes the
// log and writes session.created first, so an empty session never exists on
// disk. Collection and delivery of the notification signal happen under the
// session lock so an unsubscribing reader can never race a concurrent send
// on a channel being closed.
func (s *Session) Append(record Record) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log == nil {
		if err := s.materializeLocked(); err != nil {
			return Record{}, err
		}
	}
	stored, err := s.log.Append(record)
	if err != nil {
		return Record{}, err
	}
	fold(s.state, stored)
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return stored, nil
}

// materializeLocked creates the session directory and log and writes the
// session.created record carrying the creation metadata. Callers hold s.mu.
func (s *Session) materializeLocked() error {
	log, err := OpenLog(filepath.Join(s.store.root, s.project, s.id, logFileName))
	if err != nil {
		return err
	}
	created, err := log.Append(Record{
		Type:     TypeSessionCreated,
		Provider: s.pending.provider,
		Model:    s.pending.model,
		CWD:      s.pending.cwd,
		Time:     s.pending.createdAt,
	})
	if err != nil {
		log.Close()
		return err
	}
	fold(s.state, created)
	s.log = log
	s.pending = nil
	return nil
}

// Snapshot returns a deep copy of the folded state.
func (s *Session) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyState(s.state)
}

// Subscribe registers a notification channel. One buffered signal is delivered
// per append (coalesced when the channel is full); the reader responds by
// re-reading Snapshot. The returned function unsubscribes and closes the
// channel, ending any `for range` loop draining it.
func (s *Session) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	if s.subs == nil {
		s.subs = make(map[chan struct{}]struct{})
	}
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	unsubscribe := func() {
		s.mu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
	return ch, unsubscribe
}

// Close closes the underlying log. A session which never materialized has
// nothing on disk and nothing to close. The session must not be appended to
// after.
func (s *Session) Close() error {
	s.mu.Lock()
	log := s.log
	s.mu.Unlock()
	if log == nil {
		return nil
	}
	return log.Close()
}

// RecordsAfter returns every stored record with a sequence greater than
// seq, in sequence order. A session which has not materialized yet has no
// records. It is the tailing primitive for consumers which replay the log
// (such as a CLI printing the JSONL stream).
func (s *Session) RecordsAfter(seq int64) ([]Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.log == nil {
		return nil, nil
	}
	records, err := s.log.Records()
	if err != nil {
		return nil, err
	}
	for index, record := range records {
		if record.Seq > seq {
			return records[index:], nil
		}
	}
	return nil, nil
}

// setCurrentRun sets the run ID the publisher stamps on records. It is
// publisher-private state kept on the session so a session has exactly one
// authoritative run scope even if publishers are recreated.
func (s *Session) setCurrentRun(runID string) {
	s.mu.Lock()
	s.currentRunValue = runID
	s.mu.Unlock()
}

func (s *Session) currentRun() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentRunValue
}

func copyState(state *State) State {
	copied := *state
	copied.Context = append([]model.Message(nil), state.Context...)
	for i := range copied.Context {
		copied.Context[i].ToolCalls = append([]model.ToolCall(nil), state.Context[i].ToolCalls...)
	}
	return copied
}

// ValidSessionID accepts the identifiers this package generates: path-safe
// and not a reserved directory name.
func ValidSessionID(id string) bool {
	if len(id) == 0 || len(id) > 128 || id != strings.TrimSpace(id) {
		return false
	}
	for _, r := range id {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	if id == "." || id == ".." || id == "blobs" {
		return false
	}
	return true
}

// newSessionID returns a time-ordered, collision-resistant identifier: 48 bits
// of millisecond time plus 80 bits of crypto randomness, base32-encoded. Time
// ordering makes the session listing a directory scan without stat calls.
func newSessionID() string {
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[:8], uint64(time.Now().UnixMilli()))
	if _, err := rand.Read(raw[8:]); err != nil {
		// Randomness failure degrades to time+pid uniqueness; acceptable for
		// a single-process local application.
		binary.BigEndian.PutUint64(raw[8:], uint64(os.Getpid())^uint64(time.Now().UnixNano()))
	}
	return "s" + encodeBase32(raw[:])
}

const base32Alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

func encodeBase32(data []byte) string {
	var builder strings.Builder
	var buffer uint32
	var bits uint
	for _, b := range data {
		buffer = buffer<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			builder.WriteByte(base32Alphabet[(buffer>>bits)&0x1f])
		}
	}
	if bits > 0 {
		builder.WriteByte(base32Alphabet[(buffer<<(5-bits))&0x1f])
	}
	return builder.String()
}
