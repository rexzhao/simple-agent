// Package sessionhost owns the serve-mode session lifecycle: lazy
// opens with concurrent dedup, idle eviction, deletion, and shutdown.
// It holds no HTTP and no hub — the host builds its companion objects
// through the Options hooks (web is the in-tree host).
//
// Compatibility: v0 — NO stability guarantees. Breaking changes land
// without notice until v1 is declared. If you embed this package, pin
// an exact commit.
package sessionhost
