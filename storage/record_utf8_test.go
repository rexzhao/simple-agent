package storage

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNormalizedCoversInvalidUTF8 pins the exact behavior the web digest
// chain depends on: after the append boundary, the in-memory record, its
// JSON form, and a round-trip through json.Unmarshal are all
// byte-identical. Without normalization, json.Marshal replaces invalid
// bytes with U+FFFD only at persistence time, diverging the record from
// its stored form.
func TestNormalizedCoversInvalidUTF8(t *testing.T) {
	invalid := "a\xff\xfeb" // raw bytes, invalid UTF-8
	if strings.ToValidUTF8(invalid, "\uFFFD") == invalid {
		t.Fatal("test precondition: input must contain invalid UTF-8")
	}

	record := Record{
		ItemID:     "i1",
		Type:       TypeToolResult,
		Time:       "2026-10-10T00:00:00Z",
		Content:    invalid,
		Reasoning:  invalid,
		ToolName:   "shell",
		ToolCallID: invalid,
		ToolCalls: []WireToolCall{
			{ID: "c1", Name: invalid, Arguments: `{"cmd":"x"}`},
		},
	}

	normalized := record.Normalized()
	if strings.ContainsRune(normalized.Content, 0xFFFD) != true {
		t.Fatal("content was not normalized")
	}

	// The normalized record survives a JSON round-trip unchanged — the
	// property the un-normalized record lacks.
	encoded, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Record
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Content != normalized.Content || decoded.Reasoning != normalized.Reasoning {
		t.Fatal("normalized record diverged across a JSON round-trip")
	}
	if decoded.ToolCalls[0].Name != normalized.ToolCalls[0].Name {
		t.Fatal("tool call name diverged across a JSON round-trip")
	}

	// The un-normalized record does NOT have this property — the bug this
	// function exists to fix.
	encodedRaw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var decodedRaw Record
	if err := json.Unmarshal(encodedRaw, &decodedRaw); err != nil {
		t.Fatal(err)
	}
	if decodedRaw.Content == record.Content {
		t.Fatal("expected marshal to replace the invalid bytes (test premise broken)")
	}
}

// TestNormalizedPreservesValidText ensures normalization is the identity
// for valid input, including the HTML-escaped characters that differ
// between Go and JS serializers.
func TestNormalizedPreservesValidText(t *testing.T) {
	valid := "plain <html> & \u2028 unicode 中文 🎉 text"
	record := Record{ItemID: "i", Type: TypeUserInput, Content: valid, Reasoning: valid}
	if got := record.Normalized(); got.Content != valid || got.Reasoning != valid {
		t.Fatalf("normalization altered valid text: %q", got.Content)
	}
}
