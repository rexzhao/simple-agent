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
	// Meta is the session's model configuration, read from the
	// session.created record (empty when the head cannot be parsed).
	Meta SessionMeta
	// Archived reports the archive marker (§17.3): the session stays
	// on disk but refuses to open. It is false for every row of the
	// DEFAULT listing (archived sessions are filtered out); only the
	// include-archived view (AllLister) carries true rows.
	Archived bool
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
//
// The core is deliberately minimal (§15.1): ns is an OPAQUE grouping key
// — "project" and registration gating are host semantics. Enumerating,
// deleting, and probing are OPTIONAL capabilities the host discovers by
// type assertion; the core interface stays implementable by anything that
// can create and reopen append-only logs.
type Store interface {
	// Create returns a new, lazily persisted session inside the
	// namespace. The ns groups sessions in the jsonl directory layout;
	// in-memory implementations record it for ownership checks.
	Create(ns string) (Session, error)
	// Open reopens a stored session scoped to one namespace: the jsonl
	// layout resolves the id inside <root>/<ns>/ only. There is no
	// cross-namespace scan — a host that needs id→ns resolution keeps
	// its own registry (the serve mode's projects table).
	Open(ns, id string) (Session, error)
}

// Lister enumerates the stored sessions of one namespace, newest
// activity first (ID as the tiebreak). Sessions never materialized (no
// first record) are not listed: they hold nothing. The listing is a
// VIEW: it tolerates concurrent appends (a writer mid-line into a tail)
// by skipping unparseable lines, and a session directory whose segment
// cannot be read at all is skipped rather than failing the whole
// namespace — opening that session is where real corruption errors.
type Lister interface {
	Store
	ListSessions(ns string) ([]SessionInfo, error)
}

// Deleter removes a stored session and its records, permanently. The
// jsonl layer refuses an EXTERNAL lock holder (a CLI mid-write) instead
// of silently unlinking files under an active writer; a session held
// open by the CALLING process is the caller's business (the serve
// manager refuses its own open sessions before calling).
type Deleter interface {
	Store
	Delete(ns, id string) error
}

// Prober reports whether a session exists under a namespace WITHOUT
// taking its lock — the serve host uses it to find the owning project
// before assembling the agent (assembly precedes the lock; a failed
// assembly must not leave a zombie lock).
type Prober interface {
	Store
	HasSession(ns, id string) bool
}

// Archiver is the optional archive capability (§17.3): a session can
// be parked out of the working set without deleting it. Archive and
// Unarchive acquire the SESSION's own OS lock (the same one Open
// takes) and write/remove the marker inside it; an external holder
// (e.g. a CLI mid-write) refuses with the implementation's lock error
// (jsonl.LockHeldError) instead of mutating under an active writer.
// No root lock is required: the CLI never builds a Pool and cannot
// hold one. Both operations are IDEMPOTENT (an already-(un)archived
// session is a no-op success). An archived session refuses to open
// with ErrArchived; Delete ignores the marker (archive ≠ delete).
type Archiver interface {
	Store
	Archive(ns, id string) error
	Unarchive(ns, id string) error
	IsArchived(ns, id string) (bool, error)
}

// AllLister enumerates sessions INCLUDING archived ones (§17.3): the
// archive UI's "--all" view, with SessionInfo.Archived marking the
// parked rows. Hosts that only know Lister see the default view —
// archived sessions hidden from the working set.
type AllLister interface {
	Lister
	ListSessionsAll(ns string) ([]SessionInfo, error)
}

// ErrSessionNotFound is the typed sentinel for opening a session that
// does not exist (or lives outside the requested project). Callers
// classify with errors.Is — never by parsing the message.
var ErrSessionNotFound = errors.New("session not found")

// ErrArchived is the typed sentinel for opening an ARCHIVED session
// (§17.3): the session exists on disk but is parked out of the
// working set — it must be unarchived first. Callers classify with
// errors.Is.
var ErrArchived = errors.New("session archived")

// SessionMeta is the model configuration a session runs with: the payload
// of session.created (§15.2). Provider and Model name configured entries;
// ReasoningLevel selects a reasoning tier of that model ("" = the model's
// default — old sessions without the field replay exactly as they did).
type SessionMeta struct {
	Provider, Model, ReasoningLevel string
}
