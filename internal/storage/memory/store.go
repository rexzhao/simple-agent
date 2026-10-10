// Package memory is the in-memory storage implementation: sessions live in
// a map and die with the process. It exists for tests and ephemeral
// sessions. Records outlive the handle: closing a session and reopening it
// by id mirrors the jsonl behavior of reopening a file.
package memory

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/storage"
)

// Store manages sessions in memory.
type Store struct {
	mu       sync.Mutex
	sessions map[string]*records
	// projectOf mirrors the jsonl layout: which project each session
	// belongs to. OpenIn's gate and ListSessions' scoping depend on it.
	projectOf map[string]string
}

// OpenStore returns an empty in-memory store.
func OpenStore() *Store {
	return &Store{sessions: make(map[string]*records), projectOf: make(map[string]string)}
}

// Create returns a new session inside the project.
func (s *Store) Create(projectID string) (storage.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := &Session{id: storage.NewSessionID(), log: &records{}}
	s.sessions[session.id] = session.log
	s.projectOf[session.id] = projectID
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
	entries []storage.Record
}

func (r *records) append(record storage.Record) (storage.Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	// Mirror the jsonl boundary: both implementations return the same
	// normalized form so no caller can observe a store-dependent digest.
	return s.log.append(record.Normalized())
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

// OpenIn opens a session scoped to one project: a session living under a
// different project is not found, mirroring the jsonl registration gate.
func (s *Store) OpenIn(projectID, sessionID string) (storage.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.projectOf[sessionID] != projectID {
		return nil, fmt.Errorf("open session %q: not found", sessionID)
	}
	log, ok := s.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("open session %q: not found", sessionID)
	}
	return &Session{id: sessionID, log: log}, nil
}

// ListSessions enumerates a project's sessions, newest activity first.
// Sessions with no records hold nothing and stay unlisted.
func (s *Store) ListSessions(projectID string) ([]storage.SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var infos []storage.SessionInfo
	for id, log := range s.sessions {
		if s.projectOf[id] != projectID {
			continue
		}
		info, ok := describeSession(id, log.snapshot())
		if ok {
			infos = append(infos, info)
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Modified.After(infos[j].Modified) })
	return infos, nil
}

// describeSession derives listing info from a record slice: Created is the
// session.created timestamp, Modified the last record's, Title the first
// user input unless the records start as a compaction product (the session
// start is off-store).
func describeSession(id string, records []storage.Record) (storage.SessionInfo, bool) {
	var info storage.SessionInfo
	if len(records) == 0 {
		return storage.SessionInfo{}, false
	}
	compacted := false
	for i, record := range records {
		switch record.Type {
		case storage.TypeSessionCreated:
			if i == 0 {
				if parsed, err := time.Parse(time.RFC3339Nano, record.Time); err == nil {
					info.Created = parsed
				}
			}
		case storage.TypeContextCompacted:
			if i == 1 {
				compacted = true
			}
		case storage.TypeUserInput:
			// The first input is the title — unless the records start
			// as a compaction product (the session start is off-store
			// and a mid-conversation input must not pose as one).
			if !compacted {
				info.Title = record.Content
			}
		}
	}
	info.ID = id
	info.Modified = parseLastTime(records)
	return info, true
}

func parseLastTime(records []storage.Record) time.Time {
	if len(records) == 0 {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, records[len(records)-1].Time)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
