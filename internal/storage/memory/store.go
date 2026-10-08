// Package memory is the in-memory storage implementation: sessions live in
// a map and die with the process. It exists for tests and ephemeral
// sessions. Records outlive the handle: closing a session and reopening it
// by id mirrors the jsonl behavior of reopening a file.
package memory

import (
	"fmt"
	"sync"

	"github.com/rexzhao/simple-agent/internal/storage"
)

// Store manages sessions in memory.
type Store struct {
	mu       sync.Mutex
	sessions map[string]*records
}

// OpenStore returns an empty in-memory store.
func OpenStore() *Store {
	return &Store{sessions: make(map[string]*records)}
}

// Create returns a new session. The project id is ignored: memory sessions
// have no directory layout.
func (s *Store) Create(projectID string) (storage.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := &Session{id: storage.NewSessionID(), log: &records{}}
	s.sessions[session.id] = session.log
	return session, nil
}

// Open returns a fresh handle over an existing session's records.
func (s *Store) Open(id string) (storage.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("open session %q: not found", id)
	}
	return &Session{id: id, log: log}, nil
}

// records is the shared, append-only record list behind every handle.
type records struct {
	mu      sync.Mutex
	seq     int64
	entries []storage.Record
}

func (r *records) append(record storage.Record) (storage.Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	record.Seq = r.seq
	if record.Time == "" {
		record.Time = storage.NewTime()
	}
	if err := record.Validate(); err != nil {
		return storage.Record{}, err
	}
	r.entries = append(r.entries, record)
	return record, nil
}

func (r *records) snapshot() []storage.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]storage.Record(nil), r.entries...)
}

// Session is one handle over a record list.
type Session struct {
	mu     sync.Mutex
	id     string
	log    *records
	closed bool
}

// ID returns the session ID, available before any record exists.
func (s *Session) ID() string { return s.id }

// Append stores one record.
func (s *Session) Append(record storage.Record) (storage.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return storage.Record{}, fmt.Errorf("session is closed")
	}
	return s.log.append(record)
}

// Records returns every stored record in order.
func (s *Session) Records() ([]storage.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("session is closed")
	}
	return s.log.snapshot(), nil
}

// Close closes this handle. The records stay available for a later Open.
// It is idempotent.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
