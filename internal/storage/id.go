package storage

import (
	"crypto/rand"
	"encoding/binary"
	"os"
	"time"
)

// Item ID kinds. One-letter prefixes keep grepped session files legible:
// the kind of a record is visible in its identity.
const (
	itemKindSession        = "s" // session id
	itemKindSessionCreated = "h" // session.created (the session header)
	itemKindUserInput      = "u"
	itemKindAssistant      = "a"
	itemKindToolResult     = "t"
	itemKindRunFailed      = "f"
	itemKindToolCall       = "c" // minted wire tool-call id fallback
	itemKindRun            = "r" // run grouping id ("run-" prefixed)
	itemKindCompaction     = "x" // context.compacted
)

// NewSessionID returns a fresh session identity.
func NewSessionID() string { return NewItemID(itemKindSession) }

// NewSessionCreatedID returns the item id of a session.created record.
func NewSessionCreatedID() string { return NewItemID(itemKindSessionCreated) }

// NewUserInputID returns the item id of a user.input record.
func NewUserInputID() string { return NewItemID(itemKindUserInput) }

// NewAssistantMessageID returns the item id of an assistant.message record.
func NewAssistantMessageID() string { return NewItemID(itemKindAssistant) }

// NewToolResultID returns the item id of a tool.result record.
func NewToolResultID() string { return NewItemID(itemKindToolResult) }

// NewRunFailedID returns the item id of a run.failed record.
func NewRunFailedID() string { return NewItemID(itemKindRunFailed) }

// NewContextCompactedID returns the item id of a context.compacted
// record, minted before the compaction model call begins.
func NewContextCompactedID() string { return NewItemID(itemKindCompaction) }

// NewToolCallID returns a minted wire tool-call id, used when a provider
// issues none. Providers validate the pairing between assistant tool calls
// and their results, not the id format, so a minted id is acceptable as
// long as both sides agree.
func NewToolCallID() string { return NewItemID(itemKindToolCall) }

// NewRunID returns a fresh run grouping id: "run-" followed by the same
// time-ordered base32 body as item ids, so runs are greppable and sorted.
func NewRunID() string { return "run-" + NewItemID(itemKindRun)[1:] }

// NewItemID returns a time-ordered, collision-resistant identifier: the
// kind prefix followed by base32 of 48 bits of millisecond time plus 80
// bits of crypto randomness.
func NewItemID(kind string) string {
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[:8], uint64(time.Now().UnixMilli()))
	if _, err := rand.Read(raw[8:]); err != nil {
		// Randomness failure degrades to time+pid uniqueness; acceptable
		// for a single-process local application.
		binary.BigEndian.PutUint64(raw[8:], uint64(os.Getpid())^uint64(time.Now().UnixNano()))
	}
	return kind + encodeBase32(raw[:])
}

const base32Alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

func encodeBase32(data []byte) string {
	var builder []byte
	var buffer uint32
	var bits uint
	for _, b := range data {
		buffer = buffer<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			builder = append(builder, base32Alphabet[(buffer>>bits)&0x1f])
		}
	}
	if bits > 0 {
		builder = append(builder, base32Alphabet[(buffer<<(5-bits))&0x1f])
	}
	return string(builder)
}
