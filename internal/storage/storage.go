// Package storage defines the session record schema, the state fold, and
// the Store/Session interfaces. Implementations (jsonl, memory) are dumb
// record stores: they append and return records and know nothing about
// conversation state — the state layer above them (agent.Session) is the
// only writer and the only folder.
package storage

import (
	"errors"
	"time"
)

// SessionInfo describes one stored session for listing (the serve-mode
// project/session browser). Title is best-effort: the first user input of
// the earliest segment — trusted only when the records structurally start
// at the session header; empty when the session start is no longer on
// disk (a leading session.created+context.compacted pair, or a headerless
// start). Modified is CONTENT activity — the jsonl layout takes the
// newest segment-file mtime (lock bookkeeping excluded), memory takes the
// last record's timestamp; both answer "when did this conversation last
// move", not "when was it last touched by a process".
type SessionInfo struct {
	ID       string
	Title    string
	Created  time.Time
	Modified time.Time
}

// Session is one stored session: an append-only sequence of records.
type Session interface {
	// ID returns the session identity, minted at Create and available
	// before the first record is persisted.
	ID() string
	// Append stores one record and returns the stored form (the
	// implementation may fill fields such as time).
	Append(record Record) (Record, error)
	// Records returns every stored record in order. A session which has
	// not been materialized yet has no records.
	Records() ([]Record, error)
	Close() error
}

// Store creates and opens sessions. The session.created record is not the
// store's business: the state layer above commits it as its first record.
type Store interface {
	// Create returns a new, lazily persisted session inside the project.
	// The project id groups sessions in the jsonl directory layout;
	// in-memory implementations ignore it.
	Create(projectID string) (Session, error)
	Open(id string) (Session, error)
	// OpenIn opens a session scoped to one project: the jsonl layout
	// resolves the id inside <root>/<projectID>/ only — the serve-mode
	// registration gate is structural (no scan across unregistered
	// projects) and the id→dir resolution cannot cross it.
	OpenIn(projectID, sessionID string) (Session, error)
	// HasSession probes whether a session exists under a project
	// WITHOUT taking its lock — the serve manager uses it to find the
	// owning project before assembling the agent (assembly precedes
	// the lock; a failed assembly must not leave a zombie lock).
	HasSession(projectID, sessionID string) bool
	// Delete removes a stored session and its records, permanently.
	// The session lock is taken first — a holder (a CLI mid-write, or
	// this process) refuses the deletion instead of silently unlinking
	// files under an active writer.
	Delete(projectID, sessionID string) error
	// ListSessions enumerates the stored sessions of one project,
	// newest activity first (ID as the tiebreak). Sessions never
	// materialized (no first record) are not listed: they hold nothing.
	// The listing is a VIEW: it tolerates concurrent appends (a writer
	// mid-line into a tail) by skipping unparseable lines, and a session
	// directory whose segment cannot be read at all is skipped rather
	// than failing the whole project — opening that session is where
	// real corruption errors.
	ListSessions(projectID string) ([]SessionInfo, error)
}

// ErrSessionNotFound is the typed sentinel for opening a session that
// does not exist (or lives outside the requested project). Callers
// classify with errors.Is — never by parsing the message.
var ErrSessionNotFound = errors.New("session not found")
