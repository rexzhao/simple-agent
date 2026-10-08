package sessionlog

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

const logFileName = "log.jsonl"

// Store manages the session directories under one root. Each session is a
// directory holding a single append-only log.
type Store struct {
	root string
}

// OpenStore opens (creating if needed) the session root directory.
func OpenStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("session root is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create session root: %w", err)
	}
	return &Store{root: root}, nil
}

// Root returns the session root directory.
func (s *Store) Root() string { return s.root }

// Create returns a new session. Creation is lazy: nothing touches the
// disk, so an empty session (one never sent a message) leaves no directory
// and never appears in List. The first Append materializes the log and
// writes the session.created record ahead of the appended one.
func (s *Store) Create(provider, model, cwd string) (*Session, error) {
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
		id:    id,
		store: s,
		state: state,
		pending: &pendingMeta{
			provider: provider, model: model, cwd: cwd, createdAt: createdAt,
		},
	}, nil
}

// SessionDir returns the on-disk directory of a session ID. The directory
// exists only once the session has been materialized by its first record.
func (s *Store) SessionDir(id string) string { return filepath.Join(s.root, id) }

// Open loads an existing session, recovering its folded state from the log.
// Any run left in the running state by a crash is healed with an appended
// run.settled(interrupted) record, so a resumed session never reports a
// phantom active run. Healing is an append: it preserves the append-only
// invariant and is idempotent per run.
func (s *Store) Open(id string) (*Session, error) {
	if !ValidSessionID(id) {
		return nil, fmt.Errorf("invalid session id %q", id)
	}
	dir := filepath.Join(s.root, id)
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("open session %q: %w", id, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("session %q is not a directory", id)
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
	session := &Session{id: id, log: log, state: newState(id)}
	for _, record := range records {
		fold(session.state, record)
	}
	for runID, status := range session.state.Runs {
		if status != RunStatusRunning {
			continue
		}
		if _, err := session.Append(Record{Type: TypeRunSettled, RunID: runID, Status: RunStatusInterrupted}); err != nil {
			log.Close()
			return nil, fmt.Errorf("heal interrupted run %q: %w", runID, err)
		}
	}
	return session, nil
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

// List returns summaries of every stored session, newest first. Sessions with
// unreadable logs are skipped rather than failing the listing.
func (s *Store) List() ([]Summary, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("read session root: %w", err)
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() && ValidSessionID(entry.Name()) {
			ids = append(ids, entry.Name())
		}
	}
	summaries := make([]Summary, 0, len(ids))
	for _, id := range ids {
		records, err := readRecords(filepath.Join(s.root, id, logFileName))
		if err != nil {
			continue
		}
		state := newState(id)
		for _, record := range records {
			fold(state, record)
		}
		messages := 0
		for _, item := range state.Items {
			if item.Kind == ItemKindUser || item.Kind == ItemKindAssistant {
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
	id    string
	store *Store
	log   *Log
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
	log, err := OpenLog(filepath.Join(s.store.root, s.id, logFileName))
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
	copied.Items = append([]Item(nil), state.Items...)
	for i := range copied.Items {
		copied.Items[i].ToolCalls = append([]model.ToolCall(nil), state.Items[i].ToolCalls...)
	}
	copied.Context = append([]model.Message(nil), state.Context...)
	for i := range copied.Context {
		copied.Context[i].ToolCalls = append([]model.ToolCall(nil), state.Context[i].ToolCalls...)
	}
	runs := make(map[string]string, len(state.Runs))
	for id, status := range state.Runs {
		runs[id] = status
	}
	copied.Runs = runs
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
