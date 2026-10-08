// Package storage defines the session record schema, the state fold, and
// the Store/Session interfaces. Implementations (jsonl, memory) are dumb
// record stores: they append and return records and know nothing about
// conversation state — the state layer above them (agent.Session) is the
// only writer and the only folder.
package storage

// Session is one stored session: an append-only sequence of records.
type Session interface {
	// ID returns the session identity, minted at Create and available
	// before the first record is persisted.
	ID() string
	// Append stores one record and returns the stored form (the
	// implementation may fill ordering fields such as seq and time).
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
}
