// Package agent is the orchestration core: the Agent (construction
// and request assembly), the Session (the single ordered stream of
// records and events, compaction, steering), the Run, and the
// consumer-side Tool interface. Hosts build on it directly or through
// sessionhost (the session pool).
//
// Two package-level contracts worth knowing as a host: every durable
// fact goes through Session's commit path (the storage package only
// ever sees records), and all parameters are injected at construction
// — nothing reads configuration at runtime.
//
// Compatibility: v0 — NO stability guarantees. Breaking changes land
// without notice until v1 is declared. If you embed this package, pin
// an exact commit.
package agent
