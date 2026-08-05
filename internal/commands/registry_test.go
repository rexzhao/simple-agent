package commands

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

func TestFingerprintCanonicalizesObjectsWithoutLosingNumbers(t *testing.T) {
	left, err := Fingerprint(CommandRequest{Name: "fake", SchemaVersion: 1, ExpectedRevision: revision("42"), Arguments: json.RawMessage(`{"a":1,"nested":{"value":9007199254740993},"list":[2,3]}`)})
	if err != nil {
		t.Fatal(err)
	}
	right, err := Fingerprint(CommandRequest{Name: "fake", SchemaVersion: 1, ExpectedRevision: revision("42"), Arguments: json.RawMessage(` { "list": [2, 3], "nested": { "value": 9007199254740993 }, "a": 1 } `)})
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatal("semantically equivalent JSON produced different fingerprints")
	}
	other, err := Fingerprint(CommandRequest{Name: "fake", SchemaVersion: 1, ExpectedRevision: revision("42"), Arguments: json.RawMessage(`{"a":1,"nested":{"value":9007199254740992},"list":[2,3]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if left == other {
		t.Fatal("large JSON numbers were rounded into the same fingerprint")
	}
	fingerprints := make(map[string]string)
	for _, number := range []string{"1", "1.0", "1e0"} {
		fingerprint, err := Fingerprint(CommandRequest{Name: "number", SchemaVersion: 1, Arguments: json.RawMessage(`{"value":` + number + `}`)})
		if err != nil {
			t.Fatal(err)
		}
		fingerprints[number] = fingerprint
	}
	if fingerprints["1"] == fingerprints["1.0"] || fingerprints["1"] == fingerprints["1e0"] || fingerprints["1.0"] == fingerprints["1e0"] {
		t.Fatalf("number lexemes unexpectedly collapsed: %#v", fingerprints)
	}
}

func TestRegistryIsClosedAndReturnsStableErrors(t *testing.T) {
	registry, err := NewRegistry(CommandDefinition{Name: "fake", SchemaVersion: 1, Execute: func(context.Context, CommandRequest) (json.RawMessage, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Definition("missing", 1); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("unknown command=%v", err)
	}
	if _, err := registry.Definition("fake", 2); !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("unknown schema=%v", err)
	}
	definition, err := registry.Definition("fake", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Validate(definition, json.RawMessage(`[]`)); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("invalid arguments=%v", err)
	}
	if _, err := NewRegistry(CommandDefinition{Name: "fake", SchemaVersion: 1, Execute: definition.Execute}, CommandDefinition{Name: "fake", SchemaVersion: 1, Execute: definition.Execute}); !errors.Is(err, ErrDuplicateCommand) {
		t.Fatalf("duplicate=%v", err)
	}
}

func revision(value string) *protocol.ResourceRevision {
	revision := protocol.ResourceRevision(value)
	return &revision
}
