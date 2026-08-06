package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

// CommandHandler executes an application command. The request is copied by the
// registry/dispatcher boundary; a handler must not retain or mutate its
// Arguments buffer.
type CommandHandler func(context.Context, CommandRequest) (json.RawMessage, error)

// ResultCachePolicy controls what a dispatcher may replay after a command has
// completed. Durable results are replayed forever within the dispatcher
// epoch. Volatile results retain their request fingerprint tombstone, but the
// completed payload is regenerated for an exact retry. This is needed for
// read commands whose successful result contains an expiring data-plane
// capability (for example, a BlobDescriptor).
type ResultCachePolicy uint8

const (
	ResultCacheDurable ResultCachePolicy = iota
	ResultCacheVolatile
)

// CommandRequest is the transport-neutral command input passed to a command
// definition. Principal is an opaque authenticated identity; this package does
// not interpret it.
type CommandRequest struct {
	Name             string
	SchemaVersion    int
	RequestID        string
	ExpectedRevision *protocol.ResourceRevision
	Arguments        json.RawMessage
	Principal        string
}

func (r CommandRequest) clone() CommandRequest {
	clone := r
	clone.Arguments = append(json.RawMessage(nil), r.Arguments...)
	if r.ExpectedRevision != nil {
		revision := *r.ExpectedRevision
		clone.ExpectedRevision = &revision
	}
	return clone
}

// Clone returns an ownership-safe copy for an asynchronous executor.
func (r CommandRequest) Clone() CommandRequest { return r.clone() }

// CommandDefinition is the closed-registry contract for one command schema.
type CommandDefinition struct {
	Name                string
	SchemaVersion       int
	CrossEpochRetrySafe bool
	// CachePolicy is deliberately separate from CrossEpochRetrySafe. A
	// read-only command can be safe to retry across epochs while still needing
	// a fresh completed result because its descriptor or ticket expires.
	CachePolicy ResultCachePolicy
	// SupportsExpectedRevision is true only when Execute atomically checks the
	// request revision as part of the mutation.  A dispatcher must reject an
	// expected_revision for definitions which do not make that guarantee; it
	// must never silently turn an optimistic-concurrency request into an
	// unconditional write.
	SupportsExpectedRevision bool
	Validate                 func(json.RawMessage) error
	Execute                  CommandHandler
}

// Registry is immutable after construction. This prevents a running server
// from changing command meaning while request-id fingerprints are cached.
type Registry struct {
	definitions map[string]CommandDefinition
}

var (
	ErrInvalidDefinition  = errors.New("invalid command definition")
	ErrDuplicateCommand   = errors.New("command definition is already registered")
	ErrUnknownCommand     = errors.New("unknown command")
	ErrUnknownSchema      = errors.New("unknown command schema")
	ErrInvalidArguments   = errors.New("invalid command arguments")
	ErrInvalidFingerprint = errors.New("invalid command fingerprint")
)

// RegistryError is a stable, typed error suitable for conversion to a wire
// error. It deliberately contains no arguments or raw payload.
type RegistryError struct {
	Code    string
	Message string
	Err     error
}

// DomainError is a stable, transport-neutral application error.  The error
// text from an underlying service is deliberately not sent to the client;
// only Code and Message are exposed by the gateway.
type DomainError struct {
	Code    string
	Message string
	Err     error
}

func (e *DomainError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

func (e *DomainError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func NewDomainError(code, message string, err error) error {
	return &DomainError{Code: code, Message: message, Err: err}
}

func (e *RegistryError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

func (e *RegistryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// NewRegistry validates and closes a registry in one operation.
func NewRegistry(definitions ...CommandDefinition) (*Registry, error) {
	registry := &Registry{definitions: make(map[string]CommandDefinition, len(definitions))}
	for _, definition := range definitions {
		if err := registry.add(definition); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (r *Registry) add(definition CommandDefinition) error {
	if r == nil {
		return ErrInvalidDefinition
	}
	if strings.TrimSpace(definition.Name) == "" || definition.SchemaVersion < 1 || definition.Execute == nil || definition.CachePolicy > ResultCacheVolatile {
		return fmt.Errorf("%w: name, positive schema_version and execute are required", ErrInvalidDefinition)
	}
	key := definitionKey(definition.Name, definition.SchemaVersion)
	if _, exists := r.definitions[key]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateCommand, key)
	}
	r.definitions[key] = definition
	return nil
}

func definitionKey(name string, schemaVersion int) string {
	return fmt.Sprintf("%s\x00%d", name, schemaVersion)
}

// Definition returns a copy of a registered definition.
func (r *Registry) Definition(name string, schemaVersion int) (CommandDefinition, error) {
	if r == nil {
		return CommandDefinition{}, &RegistryError{Code: "unknown_command", Message: "unknown command", Err: ErrUnknownCommand}
	}
	definition, ok := r.definitions[definitionKey(name, schemaVersion)]
	if ok {
		return definition, nil
	}
	for key := range r.definitions {
		if strings.HasPrefix(key, name+"\x00") {
			return CommandDefinition{}, &RegistryError{Code: "unknown_command_schema", Message: "unknown command schema", Err: ErrUnknownSchema}
		}
	}
	return CommandDefinition{}, &RegistryError{Code: "unknown_command", Message: "unknown command", Err: ErrUnknownCommand}
}

// Names is intended for diagnostics and tests; it never exposes executable
// command internals.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	seen := make(map[string]struct{})
	for _, definition := range r.definitions {
		seen[definition.Name] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// Validate applies the registered schema validation without exposing a raw
// validation error to the protocol observer/logging layer.
func (r *Registry) Validate(definition CommandDefinition, arguments json.RawMessage) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &RegistryError{Code: "invalid_arguments", Message: "invalid command arguments", Err: ErrInvalidArguments}
		}
	}()
	if err := validateArgumentsJSON(arguments); err != nil {
		return &RegistryError{Code: "invalid_arguments", Message: "invalid command arguments", Err: err}
	}
	if definition.Validate == nil {
		return nil
	}
	if err := definition.Validate(arguments); err != nil {
		return &RegistryError{Code: "invalid_arguments", Message: "invalid command arguments", Err: err}
	}
	return nil
}

func validateArgumentsJSON(arguments json.RawMessage) error {
	trimmed := bytes.TrimSpace(arguments)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return ErrInvalidArguments
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return ErrInvalidArguments
	}
	return nil
}

// Fingerprint returns a stable structural fingerprint. Object keys are sorted
// and insignificant whitespace is discarded. Number lexemes are deliberately
// retained as json.Number values: 1, 1.0 and 1e0 are distinct by contract.
// This avoids float64 precision loss without attempting unbounded numeric
// normalization, which could turn attacker-controlled exponents into a CPU or
// memory amplification problem.
func Fingerprint(request CommandRequest) (string, error) {
	arguments, err := canonicalJSON(request.Arguments)
	if err != nil {
		return "", fmt.Errorf("%w: arguments: %v", ErrInvalidFingerprint, err)
	}
	value := map[string]any{
		"arguments":      json.RawMessage(arguments),
		"name":           request.Name,
		"schema_version": request.SchemaVersion,
	}
	if request.ExpectedRevision != nil {
		value["expected_revision"] = string(*request.ExpectedRevision)
	}
	canonical, err := canonicalValue(value)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidFingerprint, err)
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	return canonicalValue(value)
}

func canonicalValue(value any) ([]byte, error) {
	switch typed := value.(type) {
	case nil:
		return []byte("null"), nil
	case bool:
		if typed {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case string:
		return json.Marshal(typed)
	case json.Number:
		if typed.String() == "" {
			return nil, errors.New("empty number")
		}
		return []byte(typed.String()), nil
	case int:
		return []byte(strconv.FormatInt(int64(typed), 10)), nil
	case int64:
		return []byte(strconv.FormatInt(typed, 10)), nil
	case uint64:
		return []byte(strconv.FormatUint(typed, 10)), nil
	case json.RawMessage:
		return canonicalJSON(typed)
	case []any:
		parts := make([][]byte, len(typed))
		for i, item := range typed {
			encoded, err := canonicalValue(item)
			if err != nil {
				return nil, err
			}
			parts[i] = encoded
		}
		return []byte("[" + strings.Join(bytesToStrings(parts), ",") + "]"), nil
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			encodedKey, err := json.Marshal(key)
			if err != nil {
				return nil, err
			}
			encodedValue, err := canonicalValue(typed[key])
			if err != nil {
				return nil, err
			}
			parts = append(parts, string(encodedKey)+":"+string(encodedValue))
		}
		return []byte("{" + strings.Join(parts, ",") + "}"), nil
	default:
		return nil, fmt.Errorf("unsupported JSON value %T", value)
	}
}

func bytesToStrings(values [][]byte) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = string(value)
	}
	return result
}
