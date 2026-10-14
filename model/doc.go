// Package model is the provider abstraction: the unified request and
// message shapes, the streaming event protocol, and the adapters for
// the concrete provider APIs (openai_chat, openai_responses,
// anthropic_messages; httpstream is the shared transport with retry
// semantics).
//
// Compatibility: v0 — NO stability guarantees. Breaking changes land
// without notice until v1 is declared. If you embed this package, pin
// an exact commit.
package model
