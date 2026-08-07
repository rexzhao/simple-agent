package webapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/execution"
	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/providersettings"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

type sessionMarkReadArguments struct {
	SessionID string
	RunID     string
	ProjectID *string
}

type sessionRenameArguments struct {
	SessionID   string
	DisplayName string
}

type sessionIDArguments struct {
	SessionID string
}

type sessionHistoryReadArguments struct {
	SessionID string
	Cursor    *int64
	Direction string
	Limit     int
	AlignTurn bool
}

type sessionFullAccessArguments struct {
	SessionID  string
	FullAccess bool
}

type sessionDebugArguments struct {
	SessionID     string
	RequestBodies bool
}

type sessionCreateArguments struct {
	SessionID       string
	ProjectID       string
	DisplayName     *string
	ParentSessionID *string
	CWD             *string
	ConfigPath      *string
	Provider        *string
	ModelProfile    *string
	ReasoningLevel  *string
	FullAccess      *bool
}

type projectCreateArguments struct {
	OperationID string
	Root        string
	DisplayName string
}

type projectIDArguments struct {
	ProjectID string
}

type projectRenameArguments struct {
	ProjectID   string
	DisplayName string
}

type runCancelArguments struct {
	RunID string
}

type runStartArguments struct {
	SessionID string
	RunID     string
	Content   string
}

type runContinueArguments struct {
	SessionID string
	RunID     string
}

type runPromptAppendArguments struct {
	SessionID   string
	RunID       string
	OperationID string
	Content     string
}

type runPromptRemoveArguments struct {
	SessionID string
	RunID     string
	PromptID  string
}

type runPromptSteerArguments struct {
	SessionID string
	RunID     string
	PromptID  string
	Steer     bool
}

type runPromptMoveArguments struct {
	SessionID string
	RunID     string
	PromptID  string
	Delta     int
}

type runToolCancelArguments struct {
	SessionID  string
	RunID      string
	ToolCallID string
}

// Provider mutations deliberately use a flat, complete target document rather
// than a patch. Opaque fields are nevertheless write intents: *_mode=preserve
// tells execution to merge the durable value by provider/model identity, while
// replace is explicit. The create command adds a caller-owned operation
// identity to that same document. The target may contain credentials and
// arbitrary model parameters, so it is consumed synchronously by the execution
// service and is never included in a result or command-cache tombstone.
type providerCreateArguments struct {
	OperationID string
	Provider    string
	Input       execution.ProviderSettingsInput
}

type providerUpdateArguments struct {
	Provider string
	Input    execution.ProviderSettingsInput
}

type providerDefaultArguments struct {
	Provider string
	Model    string
}

type providerDiscoverArguments struct {
	Provider string
}

type codexLoginArguments struct {
	Provider string
}

type sessionRenameResult struct {
	SessionID   string `json:"session_id"`
	DisplayName string `json:"display_name"`
}

type sessionArchiveResult struct {
	SessionID string `json:"session_id"`
	Archived  bool   `json:"archived"`
}

type sessionFullAccessResult struct {
	SessionID  string `json:"session_id"`
	FullAccess bool   `json:"full_access"`
}

type sessionDebugResult struct {
	SessionID     string `json:"session_id"`
	RequestBodies bool   `json:"request_bodies"`
}

type sessionCreateResult struct {
	SessionID string `json:"session_id"`
	ProjectID string `json:"project_id"`
}

type projectCreateResult struct {
	OperationID string `json:"operation_id"`
	ProjectID   string `json:"project_id"`
	Created     bool   `json:"created"`
}

type projectRenameResult struct {
	ProjectID   string `json:"project_id"`
	DisplayName string `json:"display_name"`
}

type projectArchiveResult struct {
	ProjectID string `json:"project_id"`
	Archived  bool   `json:"archived"`
}

type projectDeleteResult struct {
	ProjectID       string `json:"project_id"`
	Status          string `json:"status"`
	RemovedSessions int    `json:"removed_sessions"`
}

type sessionDeleteResult struct {
	SessionID       string `json:"session_id"`
	Status          string `json:"status"`
	RemovedSessions int    `json:"removed_sessions"`
}

type sessionCompactCommandResult struct {
	SessionID     string `json:"session_id"`
	Status        string `json:"status"`
	CompactionID  string `json:"compaction_id"`
	SummaryItemID string `json:"summary_item_id"`
	Revision      string `json:"revision"`
}

type providerMutationResult struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Changed  bool   `json:"changed"`
}

type providerCreateResult struct {
	OperationID string `json:"operation_id"`
	Provider    string `json:"provider"`
	Status      string `json:"status"`
	Changed     bool   `json:"changed"`
}

type providerDefaultResult struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Status   string `json:"status"`
}

type providerDiscoverInlineResult struct {
	Provider string   `json:"provider"`
	Models   []string `json:"models"`
}

type codexLoginResult struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
}

type providerDiscoverBlobResult struct {
	Provider string                   `json:"provider"`
	Blob     *protocol.BlobDescriptor `json:"blob"`
}

// sessionHistoryReadResult is a descriptor boundary, not a second history
// model. Inline history and blob history both carry the exact SessionItemsPage
// DTO returned by the existing REST page endpoint.
type sessionHistoryReadResult struct {
	SessionID string                      `json:"session_id"`
	Cursor    int64                       `json:"cursor"`
	Direction string                      `json:"direction"`
	Limit     int                         `json:"limit"`
	AlignTurn bool                        `json:"align_turn"`
	History   *execution.SessionItemsPage `json:"history"`
	Blob      *protocol.BlobDescriptor    `json:"blob"`
}

func normalizedSessionCreateFingerprint(request commands.CommandRequest, arguments sessionCreateArguments) (string, error) {
	normalized := map[string]any{
		"session_id": arguments.SessionID,
		"project_id": arguments.ProjectID,
		// false is the business default, so omitted and explicit false are
		// one normalized operation rather than two claims for one entity.
		"full_access": false,
	}
	if arguments.DisplayName != nil {
		normalized["display_name"] = *arguments.DisplayName
	}
	if arguments.ParentSessionID != nil {
		normalized["parent_session_id"] = *arguments.ParentSessionID
	}
	if arguments.CWD != nil {
		normalized["cwd"] = *arguments.CWD
	}
	if arguments.ConfigPath != nil {
		normalized["config_path"] = *arguments.ConfigPath
	}
	if arguments.Provider != nil {
		normalized["provider"] = *arguments.Provider
	}
	if arguments.ModelProfile != nil {
		normalized["model_profile"] = *arguments.ModelProfile
	}
	if arguments.ReasoningLevel != nil {
		normalized["reasoning_level"] = *arguments.ReasoningLevel
	}
	if arguments.FullAccess != nil {
		normalized["full_access"] = *arguments.FullAccess
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	normalizedRequest := request
	normalizedRequest.Arguments = data
	return commands.Fingerprint(normalizedRequest)
}

type runCancelResult struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

type runStartResult struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	Status    string `json:"status"`
}

type runContinueResult struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	Status    string `json:"status"`
}

type runPromptAppendResult struct {
	OperationID string `json:"operation_id"`
	SessionID   string `json:"session_id"`
	RunID       string `json:"run_id"`
	Accepted    bool   `json:"accepted"`
}

type runPromptRemoveResult struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	PromptID  string `json:"prompt_id"`
	Removed   bool   `json:"removed"`
}

type runPromptSteerResult struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	PromptID  string `json:"prompt_id"`
	Steer     bool   `json:"steer"`
}

type runPromptMoveResult struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	PromptID  string `json:"prompt_id"`
	Moved     bool   `json:"moved"`
}

type runToolCancelResult struct {
	SessionID  string `json:"session_id"`
	RunID      string `json:"run_id"`
	ToolCallID string `json:"tool_call_id"`
	Cancelled  bool   `json:"cancelled"`
}

func runStartFingerprint(request commands.CommandRequest, arguments runStartArguments) (string, error) {
	fingerprintArgs, err := json.Marshal(map[string]string{
		"session_id": arguments.SessionID,
		"content":    arguments.Content,
	})
	if err != nil {
		return "", err
	}
	fingerprintRequest := request
	fingerprintRequest.Arguments = fingerprintArgs
	return commands.Fingerprint(fingerprintRequest)
}

// run.continue has no client-supplied target or content. The durable run row
// binds the new identity to the interrupted run selected while admission is
// locked (PreviousRunID); the wire fingerprint therefore contains only the
// normalized operation argument. The command name/schema remain part of the
// command fingerprint, so a run.start cannot collide with run.continue.
func runContinueFingerprint(request commands.CommandRequest, arguments runContinueArguments) (string, error) {
	fingerprintArgs, err := json.Marshal(map[string]string{
		"session_id": arguments.SessionID,
	})
	if err != nil {
		return "", err
	}
	fingerprintRequest := request
	fingerprintRequest.Arguments = fingerprintArgs
	return commands.Fingerprint(fingerprintRequest)
}

const (
	// These limits apply before a command-specific decoder runs.  They are
	// intentionally shared by all commands so a future command cannot forget
	// to put a bound around an extension map or nested array.
	maxCommandArgumentBytes     = 1 << 20
	maxCommandJSONDepth         = 32
	maxCommandJSONFields        = 16384
	maxCommandJSONCollectionLen = 4096
)

type commandJSONBounds struct {
	fields int
}

// validateBoundedJSON walks tokens instead of unmarshalling into map[string]any.
// Besides enforcing resource limits this catches duplicate keys at every
// nesting level, including arbitrary model parameters.
func validateBoundedJSON(raw []byte) error {
	if len(raw) == 0 || len(raw) > maxCommandArgumentBytes || !utf8.Valid(raw) {
		return fmt.Errorf("command JSON is outside the wire boundary")
	}
	if err := validateJSONStrings(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	bounds := commandJSONBounds{}
	if err := validateCommandJSONValue(decoder, 0, &bounds); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("command JSON contains trailing data")
	}
	return nil
}

func validateCommandJSONValue(decoder *json.Decoder, depth int, bounds *commandJSONBounds) error {
	if depth > maxCommandJSONDepth {
		return fmt.Errorf("command JSON nesting is too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid command JSON")
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			keys := make(map[string]struct{})
			count := 0
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return fmt.Errorf("invalid command JSON")
				}
				name, ok := key.(string)
				if !ok || !utf8.ValidString(name) {
					return fmt.Errorf("invalid command JSON object key")
				}
				if _, exists := keys[name]; exists {
					return fmt.Errorf("duplicate command JSON object key")
				}
				keys[name] = struct{}{}
				count++
				bounds.fields++
				if bounds.fields > maxCommandJSONFields || count > maxCommandJSONCollectionLen {
					return fmt.Errorf("command JSON object is too large")
				}
				if err := validateCommandJSONValue(decoder, depth+1, bounds); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return fmt.Errorf("invalid command JSON object")
			}
		case '[':
			count := 0
			for decoder.More() {
				count++
				if count > maxCommandJSONCollectionLen {
					return fmt.Errorf("command JSON array is too large")
				}
				if err := validateCommandJSONValue(decoder, depth+1, bounds); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return fmt.Errorf("invalid command JSON array")
			}
		default:
			return fmt.Errorf("invalid command JSON delimiter")
		}
	case string:
		if !utf8.ValidString(value) {
			return fmt.Errorf("command JSON string is not valid UTF-8")
		}
	case json.Number:
		if _, err := normalizeProviderJSONNumber(value); err != nil {
			return err
		}
	case bool, nil:
		// The token decoder has already validated the JSON scalar.
	default:
		return fmt.Errorf("invalid command JSON value")
	}
	return nil
}

// encoding/json replaces an escaped lone surrogate with U+FFFD. That is
// useful for permissive decoding, but unsafe for a target-state command: the
// submitted target would silently differ from the caller's JSON. Validate the
// string lexemes before Token/Unmarshal so every object key and nested value
// has either no surrogate escape or an immediately paired high/low escape.
func validateJSONStrings(raw []byte) error {
	for index := 0; index < len(raw); index++ {
		if raw[index] != '"' {
			continue
		}
		next, err := validateJSONString(raw, index)
		if err != nil {
			return err
		}
		index = next - 1
	}
	return nil
}

func validateJSONString(raw []byte, start int) (int, error) {
	for index := start + 1; index < len(raw); index++ {
		switch raw[index] {
		case '"':
			return index + 1, nil
		case '\\':
			index++
			if index >= len(raw) {
				return 0, fmt.Errorf("invalid command JSON string escape")
			}
			switch raw[index] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				continue
			case 'u':
				if index+4 >= len(raw) {
					return 0, fmt.Errorf("invalid command JSON unicode escape")
				}
				code, ok := parseJSONHex4(raw[index+1 : index+5])
				if !ok {
					return 0, fmt.Errorf("invalid command JSON unicode escape")
				}
				index += 4
				switch {
				case code >= 0xdc00 && code <= 0xdfff:
					return 0, fmt.Errorf("unpaired command JSON low surrogate")
				case code >= 0xd800 && code <= 0xdbff:
					if index+6 >= len(raw) || raw[index+1] != '\\' || raw[index+2] != 'u' {
						return 0, fmt.Errorf("unpaired command JSON high surrogate")
					}
					low, ok := parseJSONHex4(raw[index+3 : index+7])
					if !ok || low < 0xdc00 || low > 0xdfff {
						return 0, fmt.Errorf("invalid command JSON surrogate pair")
					}
					index += 6
				}
			default:
				return 0, fmt.Errorf("invalid command JSON string escape")
			}
		default:
			if raw[index] < 0x20 {
				return 0, fmt.Errorf("invalid command JSON control character")
			}
		}
	}
	return 0, fmt.Errorf("unterminated command JSON string")
}

func parseJSONHex4(value []byte) (int, bool) {
	if len(value) != 4 {
		return 0, false
	}
	result := 0
	for _, digit := range value {
		result <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			result += int(digit - '0')
		case digit >= 'a' && digit <= 'f':
			result += int(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			result += int(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return result, true
}

// strictCommandObject parses one JSON object, rejects duplicate keys and
// trailing values, and leaves field-level type checking to the command
// decoder. This is intentionally stricter than encoding/json's usual struct
// decoding, where duplicate fields are silently overwritten.
func strictCommandObject(raw json.RawMessage, command string) (map[string]json.RawMessage, error) {
	if err := validateBoundedJSON(raw); err != nil {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	delim, ok := start.(json.Delim)
	if !ok || delim != '{' {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid %s arguments", command)
		}
		name, ok := key.(string)
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid %s arguments", command)
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("invalid %s arguments", command)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("invalid %s arguments", command)
		}
		fields[name] = value
	}
	end, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	if end != json.Delim('}') {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	return fields, nil
}

func requireExactFields(fields map[string]json.RawMessage, command string, allowed ...string) error {
	set := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		set[name] = struct{}{}
	}
	for name := range fields {
		if _, ok := set[name]; !ok {
			return fmt.Errorf("invalid %s arguments", command)
		}
	}
	return nil
}

func requiredCommandString(fields map[string]json.RawMessage, name, command string) (string, error) {
	raw, ok := fields[name]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return "", fmt.Errorf("invalid %s arguments", command)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("invalid %s arguments", command)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("invalid %s arguments", command)
	}
	return value, nil
}

// requiredCommandStringAllowEmpty keeps the wire field required and applies
// the same trimming/UTF-8 boundary as requiredCommandString, but permits the
// empty value for domain fields whose empty value has an established meaning.
func requiredCommandStringAllowEmpty(fields map[string]json.RawMessage, name, command string) (string, error) {
	raw, ok := fields[name]
	if !ok || strings.TrimSpace(string(raw)) == "null" || !utf8.Valid(raw) {
		return "", fmt.Errorf("invalid %s arguments", command)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("invalid %s arguments", command)
	}
	return strings.TrimSpace(value), nil
}

// requiredRunStartContent is deliberately not implemented in terms of
// requiredCommandString. Content is user data whose exact string value is
// part of the durable run fingerprint; unlike IDs, leading/trailing
// whitespace must survive decoding unchanged.
func requiredRunStartContent(fields map[string]json.RawMessage, command string, maxBytes int) (string, error) {
	raw, ok := fields["content"]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return "", fmt.Errorf("invalid %s arguments", command)
	}
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("invalid %s arguments", command)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("invalid %s arguments", command)
	}
	if strings.TrimSpace(value) == "" || len(value) > maxBytes {
		return "", fmt.Errorf("invalid %s arguments", command)
	}
	return value, nil
}

func optionalCommandString(fields map[string]json.RawMessage, name, command string) (*string, error) {
	raw, ok := fields[name]
	if !ok {
		return nil, nil
	}
	value, err := requiredCommandString(map[string]json.RawMessage{name: raw}, name, command)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func requiredCommandBool(fields map[string]json.RawMessage, name, command string) (bool, error) {
	raw, ok := fields[name]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return false, fmt.Errorf("invalid %s arguments", command)
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, fmt.Errorf("invalid %s arguments", command)
	}
	return value, nil
}

func requiredCommandInt(fields map[string]json.RawMessage, name, command string, min, max int) (int, error) {
	raw, ok := fields[name]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return 0, fmt.Errorf("invalid %s arguments", command)
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil || value < min || value > max {
		return 0, fmt.Errorf("invalid %s arguments", command)
	}
	return value, nil
}

func requiredProviderIdentity(fields map[string]json.RawMessage, name, command string) (string, error) {
	raw, ok := fields[name]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return "", fmt.Errorf("invalid %s arguments", command)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || providersettings.ValidateProviderName(value) != nil {
		return "", fmt.Errorf("invalid %s arguments", command)
	}
	// Do not trim this identity. Internal spaces and Unicode are part of the
	// shared E10 provider-name contract; ValidateProviderName rejects only the
	// ambiguous edges and path/control characters.
	return value, nil
}

func decodeProviderJSONMap(raw json.RawMessage, command string) (map[string]any, error) {
	if err := validateBoundedJSON(raw); err != nil || string(bytes.TrimSpace(raw)) == "null" {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	normalized, err := normalizeProviderJSONNumbers(object)
	if err != nil {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	return normalized.(map[string]any), nil
}

const maxProviderJSONSafeInteger int64 = 9007199254740991

// yaml.v3 and the existing config loader use native integer values for YAML
// integer parameters. Preserve that distinction for safe integers, retain
// finite fractional/exponent values as float64, and reject values that JS or
// Go cannot represent without a silent type/precision change.
func normalizeProviderJSONNumbers(value any) (any, error) {
	switch typed := value.(type) {
	case json.Number:
		return normalizeProviderJSONNumber(typed)
	case map[string]any:
		for key, item := range typed {
			normalized, err := normalizeProviderJSONNumbers(item)
			if err != nil {
				return nil, err
			}
			typed[key] = normalized
		}
	case []any:
		for index, item := range typed {
			normalized, err := normalizeProviderJSONNumbers(item)
			if err != nil {
				return nil, err
			}
			typed[index] = normalized
		}
	}
	return value, nil
}

func normalizeProviderJSONNumber(value json.Number) (any, error) {
	text := value.String()
	if strings.ContainsAny(text, ".eE") {
		decimal, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(decimal) || math.IsInf(decimal, 0) {
			return nil, fmt.Errorf("provider JSON number is outside the finite float64 boundary")
		}
		// ParseFloat is allowed to silently round a decimal lexeme. In
		// particular, depending on the Go version, a non-zero value such as
		// 1e-400 can become zero without an error, and a value just below a
		// large integer can become that integer. Keep the original lexeme in
		// an exact rational long enough to distinguish those cases. This does
		// not require decimal values to be represented exactly in float64; it
		// only prevents a non-zero or mathematically fractional input from
		// changing its integer/zero semantics at the wire boundary.
		exact, ok := new(big.Rat).SetString(text)
		if !ok {
			return nil, fmt.Errorf("provider JSON number is not a valid decimal")
		}
		if exact.Sign() != 0 && decimal == 0 {
			return nil, fmt.Errorf("provider JSON number underflows float64")
		}
		if !exact.IsInt() && math.Trunc(decimal) == decimal {
			return nil, fmt.Errorf("provider JSON fractional number was rounded to an integer")
		}
		if math.Trunc(decimal) == decimal && math.Abs(decimal) > float64(maxProviderJSONSafeInteger) {
			return nil, fmt.Errorf("provider JSON integer-valued number exceeds the safe integer boundary")
		}
		return decimal, nil
	}
	integer, err := strconv.ParseInt(text, 10, 64)
	if err != nil || integer < -maxProviderJSONSafeInteger || integer > maxProviderJSONSafeInteger {
		return nil, fmt.Errorf("provider JSON integer exceeds the safe integer boundary")
	}
	return integer, nil
}

func requiredProviderString(fields map[string]json.RawMessage, name, command string) (string, error) {
	return requiredCommandString(fields, name, command)
}

func optionalProviderString(fields map[string]json.RawMessage, name, command string) (string, error) {
	raw, ok := fields[name]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		if ok {
			return "", fmt.Errorf("invalid %s arguments", command)
		}
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || !utf8.ValidString(value) {
		return "", fmt.Errorf("invalid %s arguments", command)
	}
	return value, nil
}

func decodeProviderStringArray(raw json.RawMessage, command string) ([]string, error) {
	if err := validateBoundedJSON(raw); err != nil {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var items []json.RawMessage
	if err := decoder.Decode(&items); err != nil || items == nil || len(items) > maxCommandJSONCollectionLen {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		var value string
		if err := json.Unmarshal(item, &value); err != nil || !utf8.ValidString(value) {
			return nil, fmt.Errorf("invalid %s arguments", command)
		}
		result = append(result, value)
	}
	return result, nil
}

func decodeProviderReasoning(raw json.RawMessage, command string) (config.ReasoningConfig, error) {
	fields, err := strictCommandObject(raw, command+" reasoning_config")
	if err != nil {
		return config.ReasoningConfig{}, err
	}
	if err := requireExactFields(fields, command+" reasoning_config", "parameter", "default", "levels"); err != nil {
		return config.ReasoningConfig{}, err
	}
	parameter, err := optionalProviderString(fields, "parameter", command)
	if err != nil {
		return config.ReasoningConfig{}, err
	}
	defaultLevel, err := optionalProviderString(fields, "default", command)
	if err != nil {
		return config.ReasoningConfig{}, err
	}
	levels := map[string]any(nil)
	if levelRaw, ok := fields["levels"]; ok {
		levels, err = decodeProviderJSONMap(levelRaw, command)
		if err != nil {
			return config.ReasoningConfig{}, err
		}
	}
	return config.ReasoningConfig{Parameter: parameter, Default: defaultLevel, Levels: levels}, nil
}

func decodeProviderPricing(raw json.RawMessage, command string) (*config.ModelPricing, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	fields, err := strictCommandObject(raw, command+" pricing")
	if err != nil {
		return nil, err
	}
	if err := requireExactFields(fields, command+" pricing", "input_cache_hit", "input_cache_miss", "cache_write", "output", "currency", "long_context_threshold", "long_context"); err != nil {
		return nil, err
	}
	var result config.ModelPricing
	for name, destination := range map[string]*float64{
		"input_cache_hit": &result.InputCacheHit, "input_cache_miss": &result.InputCacheMiss,
		"cache_write": &result.CacheWrite, "output": &result.Output,
	} {
		if value, ok := fields[name]; ok {
			if strings.TrimSpace(string(value)) == "null" {
				return nil, fmt.Errorf("invalid %s arguments", command)
			}
			if err := json.Unmarshal(value, destination); err != nil {
				return nil, fmt.Errorf("invalid %s arguments", command)
			}
		}
	}
	if currency, err := optionalProviderString(fields, "currency", command); err != nil {
		return nil, err
	} else {
		result.Currency = currency
	}
	if value, ok := fields["long_context_threshold"]; ok {
		if strings.TrimSpace(string(value)) == "null" {
			return nil, fmt.Errorf("invalid %s arguments", command)
		}
		var threshold int
		if err := json.Unmarshal(value, &threshold); err != nil || threshold < 0 || threshold > providersettings.MaxWireInteger {
			return nil, fmt.Errorf("invalid %s arguments", command)
		}
		result.LongContextThreshold = threshold
	}
	if value, ok := fields["long_context"]; ok && string(bytes.TrimSpace(value)) != "null" {
		tierFields, err := strictCommandObject(value, command+" long_context")
		if err != nil {
			return nil, err
		}
		if err := requireExactFields(tierFields, command+" long_context", "input_cache_hit", "input_cache_miss", "cache_write", "output"); err != nil {
			return nil, err
		}
		result.LongContext = &config.ModelPricingTier{}
		for name, destination := range map[string]*float64{
			"input_cache_hit": &result.LongContext.InputCacheHit, "input_cache_miss": &result.LongContext.InputCacheMiss,
			"cache_write": &result.LongContext.CacheWrite, "output": &result.LongContext.Output,
		} {
			if field, ok := tierFields[name]; ok {
				if err := json.Unmarshal(field, destination); err != nil {
					return nil, fmt.Errorf("invalid %s arguments", command)
				}
			}
		}
	}
	return &result, nil
}

func decodeProviderModel(raw json.RawMessage, command string) (execution.ProviderModelSettings, error) {
	fields, err := strictCommandObject(raw, command+" model")
	if err != nil {
		return execution.ProviderModelSettings{}, err
	}
	if err := requireExactFields(fields, command+" model", "profile", "id", "type", "compatibility", "input", "developer_role", "context_window", "input_limit", "output_limit", "parameters", "parameters_mode", "parameters_source_profile", "reasoning_config", "pricing"); err != nil {
		return execution.ProviderModelSettings{}, err
	}
	profile, err := requiredProviderString(fields, "profile", command)
	if err != nil {
		return execution.ProviderModelSettings{}, err
	}
	id, err := optionalProviderString(fields, "id", command)
	if err != nil {
		return execution.ProviderModelSettings{}, err
	}
	typeName, err := optionalProviderString(fields, "type", command)
	if err != nil {
		return execution.ProviderModelSettings{}, err
	}
	compatibility, err := optionalProviderString(fields, "compatibility", command)
	if err != nil {
		return execution.ProviderModelSettings{}, err
	}
	developerRole, err := optionalProviderString(fields, "developer_role", command)
	if err != nil {
		return execution.ProviderModelSettings{}, err
	}
	result := execution.ProviderModelSettings{Profile: profile, ID: id, Type: typeName, Compatibility: compatibility, DeveloperRole: developerRole, ParametersMode: execution.ProviderWriteReplace}
	parametersModeExplicit := false
	if _, ok := fields["parameters_mode"]; ok {
		parametersModeExplicit = true
		mode, modeErr := decodeProviderWriteMode(fields["parameters_mode"], command)
		if modeErr != nil {
			return execution.ProviderModelSettings{}, modeErr
		}
		result.ParametersMode = mode
	}
	if !parametersModeExplicit {
		return execution.ProviderModelSettings{}, fmt.Errorf("invalid %s arguments", command)
	}
	if _, ok := fields["parameters_source_profile"]; ok {
		source, sourceErr := optionalProviderString(fields, "parameters_source_profile", command)
		if sourceErr != nil || strings.TrimSpace(source) == "" {
			return execution.ProviderModelSettings{}, fmt.Errorf("invalid %s arguments", command)
		}
		result.ParametersSourceProfile = strings.TrimSpace(source)
	}
	if value, ok := fields["input"]; ok {
		result.Input, err = decodeProviderStringArray(value, command)
		if err != nil {
			return execution.ProviderModelSettings{}, err
		}
	}
	for name, destination := range map[string]*int{"context_window": &result.ContextWindow, "input_limit": &result.InputLimit, "output_limit": &result.OutputLimit} {
		if value, ok := fields[name]; ok {
			if strings.TrimSpace(string(value)) == "null" {
				return execution.ProviderModelSettings{}, fmt.Errorf("invalid %s arguments", command)
			}
			if err := json.Unmarshal(value, destination); err != nil || *destination < 0 || *destination > providersettings.MaxWireInteger {
				return execution.ProviderModelSettings{}, fmt.Errorf("invalid %s arguments", command)
			}
		}
	}
	if value, ok := fields["parameters"]; ok {
		if string(bytes.TrimSpace(value)) == "null" {
			return execution.ProviderModelSettings{}, fmt.Errorf("invalid %s arguments", command)
		}
		result.Parameters, err = decodeProviderJSONMap(value, command)
		if err != nil {
			return execution.ProviderModelSettings{}, err
		}
	}
	if result.ParametersMode == execution.ProviderWritePreserve {
		if !parametersModeExplicit || result.ParametersSourceProfile == "" {
			return execution.ProviderModelSettings{}, fmt.Errorf("invalid %s arguments", command)
		}
		if _, present := fields["parameters"]; present {
			return execution.ProviderModelSettings{}, fmt.Errorf("invalid %s arguments", command)
		}
	} else if parametersModeExplicit {
		if _, present := fields["parameters"]; !present || result.ParametersSourceProfile != "" {
			return execution.ProviderModelSettings{}, fmt.Errorf("invalid %s arguments", command)
		}
	}
	if value, ok := fields["reasoning_config"]; ok {
		if string(bytes.TrimSpace(value)) == "null" {
			return execution.ProviderModelSettings{}, fmt.Errorf("invalid %s arguments", command)
		}
		result.ReasoningConfig, err = decodeProviderReasoning(value, command)
		if err != nil {
			return execution.ProviderModelSettings{}, err
		}
	}
	if value, ok := fields["pricing"]; ok {
		result.Pricing, err = decodeProviderPricing(value, command)
		if err != nil {
			return execution.ProviderModelSettings{}, err
		}
	}
	return result, nil
}

var providerTargetFields = []string{
	"base_url", "api_key", "keep_api_key", "auth_file", "request_timeout",
	"http_proxy", "https_proxy", "max_concurrent_requests", "models",
}

var providerTargetWriteModeFields = []string{"base_url_mode", "auth_file_mode", "http_proxy_mode", "https_proxy_mode"}

func decodeProviderWriteMode(raw json.RawMessage, command string) (execution.ProviderWriteMode, error) {
	var value string
	if strings.TrimSpace(string(raw)) == "null" || json.Unmarshal(raw, &value) != nil || !utf8.ValidString(value) {
		return "", fmt.Errorf("invalid %s arguments", command)
	}
	mode := execution.ProviderWriteMode(strings.TrimSpace(value))
	if mode != execution.ProviderWritePreserve && mode != execution.ProviderWriteReplace {
		return "", fmt.Errorf("invalid %s arguments", command)
	}
	return mode, nil
}

func optionalProviderWriteMode(fields map[string]json.RawMessage, name, command string) (execution.ProviderWriteMode, error) {
	raw, ok := fields[name]
	if !ok {
		return execution.ProviderWriteReplace, nil
	}
	return decodeProviderWriteMode(raw, command)
}

func decodeProviderTargetFields(fields map[string]json.RawMessage, command string, requireComplete bool) (string, execution.ProviderSettingsInput, error) {
	if requireComplete {
		for _, name := range providerTargetFields {
			if _, ok := fields[name]; !ok {
				return "", execution.ProviderSettingsInput{}, fmt.Errorf("invalid %s arguments", command)
			}
		}
	}
	for _, name := range providerTargetWriteModeFields {
		if _, ok := fields[name]; !ok {
			return "", execution.ProviderSettingsInput{}, fmt.Errorf("invalid %s arguments", command)
		}
	}
	provider, err := requiredProviderIdentity(fields, "provider", command)
	if err != nil {
		return "", execution.ProviderSettingsInput{}, err
	}
	baseURLMode, err := optionalProviderWriteMode(fields, "base_url_mode", command)
	if err != nil {
		return "", execution.ProviderSettingsInput{}, err
	}
	baseURL, err := requiredProviderString(fields, "base_url", command)
	if baseURLMode == execution.ProviderWritePreserve && err != nil {
		baseURL, err = requiredCommandStringAllowEmpty(fields, "base_url", command)
	}
	if err != nil {
		return "", execution.ProviderSettingsInput{}, err
	}
	input := execution.ProviderSettingsInput{Name: provider, BaseURL: baseURL, BaseURLMode: baseURLMode}
	for name, destination := range map[string]*execution.ProviderWriteMode{
		"auth_file_mode":   &input.AuthFileMode,
		"http_proxy_mode":  &input.HTTPProxyMode,
		"https_proxy_mode": &input.HTTPSProxyMode,
	} {
		mode, modeErr := optionalProviderWriteMode(fields, name, command)
		if modeErr != nil {
			return "", execution.ProviderSettingsInput{}, modeErr
		}
		*destination = mode
	}
	for name, destination := range map[string]*string{"api_key": &input.APIKey, "auth_file": &input.AuthFile, "request_timeout": &input.RequestTimeout, "http_proxy": &input.HTTPProxy, "https_proxy": &input.HTTPSProxy} {
		if value, ok := fields[name]; ok {
			if strings.TrimSpace(string(value)) == "null" {
				return "", execution.ProviderSettingsInput{}, fmt.Errorf("invalid %s arguments", command)
			}
			if err := json.Unmarshal(value, destination); err != nil || !utf8.ValidString(*destination) {
				return "", execution.ProviderSettingsInput{}, fmt.Errorf("invalid %s arguments", command)
			}
		}
	}
	keepAPIKey, err := optionalCommandBool(fields, "keep_api_key", command)
	if err != nil {
		return "", execution.ProviderSettingsInput{}, err
	}
	if keepAPIKey != nil {
		input.KeepAPIKey = *keepAPIKey
	}
	if value, ok := fields["max_concurrent_requests"]; ok {
		if strings.TrimSpace(string(value)) == "null" {
			return "", execution.ProviderSettingsInput{}, fmt.Errorf("invalid %s arguments", command)
		}
		if err := json.Unmarshal(value, &input.MaxConcurrentRequests); err != nil || input.MaxConcurrentRequests < 0 || input.MaxConcurrentRequests > providersettings.MaxWireInteger {
			return "", execution.ProviderSettingsInput{}, fmt.Errorf("invalid %s arguments", command)
		}
	}
	modelsRaw, ok := fields["models"]
	if !ok {
		return "", execution.ProviderSettingsInput{}, fmt.Errorf("invalid %s arguments", command)
	}
	var modelItems []json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(modelsRaw))
	if err := decoder.Decode(&modelItems); err != nil || len(modelItems) == 0 || len(modelItems) > maxCommandJSONCollectionLen {
		return "", execution.ProviderSettingsInput{}, fmt.Errorf("invalid %s arguments", command)
	}
	input.Models = make([]execution.ProviderModelSettings, 0, len(modelItems))
	for _, item := range modelItems {
		model, err := decodeProviderModel(item, command)
		if err != nil {
			return "", execution.ProviderSettingsInput{}, err
		}
		input.Models = append(input.Models, model)
	}
	return provider, input, nil
}

func decodeProviderUpdateArguments(raw json.RawMessage) (providerUpdateArguments, error) {
	const command = "provider.update"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return providerUpdateArguments{}, err
	}
	if err := requireExactFields(fields, command, append(append([]string{"provider"}, providerTargetFields...), providerTargetWriteModeFields...)...); err != nil {
		return providerUpdateArguments{}, err
	}
	provider, input, err := decodeProviderTargetFields(fields, command, false)
	if err != nil {
		return providerUpdateArguments{}, err
	}
	return providerUpdateArguments{Provider: provider, Input: input}, nil
}

func decodeProviderCreateArguments(raw json.RawMessage) (providerCreateArguments, error) {
	const command = "provider.create"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return providerCreateArguments{}, err
	}
	if err := requireExactFields(fields, command, append(append([]string{"operation_id", "provider"}, providerTargetFields...), providerTargetWriteModeFields...)...); err != nil {
		return providerCreateArguments{}, err
	}
	operationID, err := requiredCommandString(fields, "operation_id", command)
	if err != nil || projectstore.ValidateOperationID(operationID) != nil {
		return providerCreateArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	provider, input, err := decodeProviderTargetFields(fields, command, true)
	if err != nil {
		return providerCreateArguments{}, err
	}
	return providerCreateArguments{OperationID: operationID, Provider: provider, Input: input}, nil
}

func decodeProviderDefaultArguments(raw json.RawMessage) (providerDefaultArguments, error) {
	const command = "provider.set_default"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return providerDefaultArguments{}, err
	}
	if err := requireExactFields(fields, command, "provider", "model"); err != nil {
		return providerDefaultArguments{}, err
	}
	provider, err := requiredProviderIdentity(fields, "provider", command)
	if err != nil {
		return providerDefaultArguments{}, err
	}
	model, err := requiredCommandString(fields, "model", command)
	if err != nil {
		return providerDefaultArguments{}, err
	}
	return providerDefaultArguments{Provider: provider, Model: model}, nil
}

func decodeProviderDiscoverArguments(raw json.RawMessage) (providerDiscoverArguments, error) {
	const command = "provider.discover_models"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return providerDiscoverArguments{}, err
	}
	if err := requireExactFields(fields, command, "provider"); err != nil {
		return providerDiscoverArguments{}, err
	}
	provider, err := requiredProviderIdentity(fields, "provider", command)
	return providerDiscoverArguments{Provider: provider}, err
}

func decodeCodexLoginArguments(raw json.RawMessage, command string) (codexLoginArguments, error) {
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return codexLoginArguments{}, err
	}
	if err := requireExactFields(fields, command, "provider"); err != nil {
		return codexLoginArguments{}, err
	}
	provider, err := requiredProviderIdentity(fields, "provider", command)
	if err != nil {
		return codexLoginArguments{}, err
	}
	return codexLoginArguments{Provider: provider}, nil
}

func validateProviderUpdateArguments(raw json.RawMessage) error {
	_, err := decodeProviderUpdateArguments(raw)
	return err
}

func validateProviderCreateArguments(raw json.RawMessage) error {
	_, err := decodeProviderCreateArguments(raw)
	return err
}

func validateProviderDefaultArguments(raw json.RawMessage) error {
	_, err := decodeProviderDefaultArguments(raw)
	return err
}

func validateProviderDiscoverArguments(raw json.RawMessage) error {
	_, err := decodeProviderDiscoverArguments(raw)
	return err
}

func validateCodexLoginStartArguments(raw json.RawMessage) error {
	_, err := decodeCodexLoginArguments(raw, "codex_login.start")
	return err
}

func validateCodexLoginClearArguments(raw json.RawMessage) error {
	_, err := decodeCodexLoginArguments(raw, "codex_login.clear")
	return err
}

func optionalCommandInt64(fields map[string]json.RawMessage, name, command string, min, max int64) (*int64, error) {
	raw, ok := fields[name]
	if !ok {
		return nil, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil || value < min || value > max {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	return &value, nil
}

func optionalCommandBool(fields map[string]json.RawMessage, name, command string) (*bool, error) {
	raw, ok := fields[name]
	if !ok {
		return nil, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	value, err := requiredCommandBool(fields, name, command)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

const maxSessionCreateArgumentBytes = 4096

// run.start is intentionally a bounded text-only clean-break contract. The
// existing REST endpoint retains its image/data-URL support; WebSocket
// command frames do not carry blob bytes until a separate blob upload
// contract is specified. Unknown image/blob/content-block fields are rejected
// rather than silently dropped.
const maxRunStartContentBytes = 256 * 1024
const maxRunPromptAppendContentBytes = sessions.MaxPromptAppendContentBytes

func boundedCommandString(fields map[string]json.RawMessage, name, command string, maxBytes int) (*string, error) {
	value, err := optionalCommandString(fields, name, command)
	if err != nil {
		return nil, err
	}
	if value != nil && len(*value) > maxBytes {
		return nil, fmt.Errorf("invalid %s arguments", command)
	}
	return value, nil
}

func decodeSessionCreateArguments(raw json.RawMessage) (sessionCreateArguments, error) {
	const command = "session.create"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return sessionCreateArguments{}, err
	}
	if err := requireExactFields(fields, command, "session_id", "project_id", "display_name", "parent_session_id", "cwd", "config_path", "provider", "model_profile", "reasoning_level", "full_access"); err != nil {
		return sessionCreateArguments{}, err
	}
	sessionID, err := requiredCommandString(fields, "session_id", command)
	if err != nil || sessions.ValidateSessionCreateID(sessionID) != nil {
		return sessionCreateArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	projectID, err := requiredCommandString(fields, "project_id", command)
	if err != nil || len(projectID) > 128 || projectstore.ValidateProjectID(projectID) != nil {
		return sessionCreateArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	displayName, err := boundedCommandString(fields, "display_name", command, maxSessionCreateArgumentBytes)
	if err != nil {
		return sessionCreateArguments{}, err
	}
	parentID, err := boundedCommandString(fields, "parent_session_id", command, maxSessionCreateArgumentBytes)
	if err != nil {
		return sessionCreateArguments{}, err
	}
	// parent_session_id references an existing entity; unlike the new entity
	// ID it may be a legacy path-safe ID longer than the client-create bound.
	if parentID != nil && sessions.ValidateSessionID(*parentID) != nil {
		return sessionCreateArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	cwd, err := boundedCommandString(fields, "cwd", command, maxSessionCreateArgumentBytes)
	if err != nil {
		return sessionCreateArguments{}, err
	}
	configPath, err := boundedCommandString(fields, "config_path", command, maxSessionCreateArgumentBytes)
	if err != nil {
		return sessionCreateArguments{}, err
	}
	provider, err := boundedCommandString(fields, "provider", command, maxSessionCreateArgumentBytes)
	if err != nil {
		return sessionCreateArguments{}, err
	}
	modelProfile, err := boundedCommandString(fields, "model_profile", command, maxSessionCreateArgumentBytes)
	if err != nil {
		return sessionCreateArguments{}, err
	}
	reasoningLevel, err := boundedCommandString(fields, "reasoning_level", command, maxSessionCreateArgumentBytes)
	if err != nil {
		return sessionCreateArguments{}, err
	}
	fullAccess, err := optionalCommandBool(fields, "full_access", command)
	if err != nil {
		return sessionCreateArguments{}, err
	}
	// Parent-only creates use the existing inherited-session semantics: the
	// child's provider/capability snapshot comes from the parent. Do not make
	// the same wire shape ambiguously mean either "inherit" or "resolve the
	// current server config" depending on which optional override happened to
	// be supplied. Configured root creates may use the other fields; inherited
	// overrides remain a later, separately specified command contract.
	if parentID != nil && (cwd != nil || configPath != nil || provider != nil || modelProfile != nil || reasoningLevel != nil || fullAccess != nil) {
		return sessionCreateArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	return sessionCreateArguments{
		SessionID: sessionID, ProjectID: projectID, DisplayName: displayName,
		ParentSessionID: parentID, CWD: cwd, ConfigPath: configPath,
		Provider: provider, ModelProfile: modelProfile, ReasoningLevel: reasoningLevel,
		FullAccess: fullAccess,
	}, nil
}

func decodeSessionMarkReadArguments(raw json.RawMessage) (sessionMarkReadArguments, error) {
	const command = "session.mark_read"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return sessionMarkReadArguments{}, err
	}
	if err := requireExactFields(fields, command, "session_id", "run_id", "project_id"); err != nil {
		return sessionMarkReadArguments{}, err
	}
	sessionID, err := requiredCommandString(fields, "session_id", command)
	if err != nil {
		return sessionMarkReadArguments{}, err
	}
	runID, err := requiredCommandString(fields, "run_id", command)
	if err != nil {
		return sessionMarkReadArguments{}, err
	}
	projectID, err := optionalCommandString(fields, "project_id", command)
	if err != nil {
		return sessionMarkReadArguments{}, err
	}
	return sessionMarkReadArguments{SessionID: sessionID, RunID: runID, ProjectID: projectID}, nil
}

func decodeSessionRenameArguments(raw json.RawMessage) (sessionRenameArguments, error) {
	const command = "session.rename"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return sessionRenameArguments{}, err
	}
	if err := requireExactFields(fields, command, "session_id", "display_name"); err != nil {
		return sessionRenameArguments{}, err
	}
	sessionID, err := requiredCommandString(fields, "session_id", command)
	if err != nil {
		return sessionRenameArguments{}, err
	}
	displayName, err := requiredCommandString(fields, "display_name", command)
	if err != nil {
		return sessionRenameArguments{}, err
	}
	return sessionRenameArguments{SessionID: sessionID, DisplayName: displayName}, nil
}

func decodeSessionIDArguments(raw json.RawMessage, command string) (sessionIDArguments, error) {
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return sessionIDArguments{}, err
	}
	if err := requireExactFields(fields, command, "session_id"); err != nil {
		return sessionIDArguments{}, err
	}
	sessionID, err := requiredCommandString(fields, "session_id", command)
	if err != nil || sessions.ValidateSessionID(sessionID) != nil {
		return sessionIDArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	return sessionIDArguments{SessionID: sessionID}, nil
}

const (
	maxProjectCommandPathBytes = 4096
	maxProjectCommandNameBytes = 4096
)

func decodeProjectCreateArguments(raw json.RawMessage) (projectCreateArguments, error) {
	const command = "project.create"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return projectCreateArguments{}, err
	}
	if err := requireExactFields(fields, command, "operation_id", "root", "display_name"); err != nil {
		return projectCreateArguments{}, err
	}
	operationID, err := requiredCommandString(fields, "operation_id", command)
	if err != nil || projectstore.ValidateOperationID(operationID) != nil {
		return projectCreateArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	root, err := requiredCommandString(fields, "root", command)
	if err != nil || len(root) > maxProjectCommandPathBytes || !utf8.ValidString(root) {
		return projectCreateArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	displayName, err := requiredCommandStringAllowEmpty(fields, "display_name", command)
	if err != nil || len(displayName) > maxProjectCommandNameBytes || !utf8.ValidString(displayName) {
		return projectCreateArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	return projectCreateArguments{OperationID: operationID, Root: root, DisplayName: displayName}, nil
}

func decodeProjectIDArguments(raw json.RawMessage, command string) (projectIDArguments, error) {
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return projectIDArguments{}, err
	}
	if err := requireExactFields(fields, command, "project_id"); err != nil {
		return projectIDArguments{}, err
	}
	projectID, err := requiredCommandString(fields, "project_id", command)
	if err != nil || len(projectID) > 128 || projectstore.ValidateProjectID(projectID) != nil {
		return projectIDArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	return projectIDArguments{ProjectID: projectID}, nil
}

func decodeProjectRenameArguments(raw json.RawMessage) (projectRenameArguments, error) {
	const command = "project.rename"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return projectRenameArguments{}, err
	}
	if err := requireExactFields(fields, command, "project_id", "display_name"); err != nil {
		return projectRenameArguments{}, err
	}
	projectID, err := requiredCommandString(fields, "project_id", command)
	if err != nil || len(projectID) > 128 || projectstore.ValidateProjectID(projectID) != nil {
		return projectRenameArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	displayName, err := requiredCommandString(fields, "display_name", command)
	if err != nil || len(displayName) > maxProjectCommandNameBytes || !utf8.ValidString(displayName) {
		return projectRenameArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	return projectRenameArguments{ProjectID: projectID, DisplayName: displayName}, nil
}

func validateProjectCreateArguments(raw json.RawMessage) error {
	_, err := decodeProjectCreateArguments(raw)
	return err
}

func validateProjectRenameArguments(raw json.RawMessage) error {
	_, err := decodeProjectRenameArguments(raw)
	return err
}

func validateProjectIDArguments(raw json.RawMessage, command string) error {
	_, err := decodeProjectIDArguments(raw, command)
	return err
}

func normalizedProjectCreateFingerprint(request commands.CommandRequest, arguments projectCreateArguments) (string, error) {
	canonicalRoot, err := projectstore.CanonicalRootKey(arguments.Root)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(map[string]string{
		"operation_id": arguments.OperationID,
		"root":         canonicalRoot,
		"display_name": strings.TrimSpace(arguments.DisplayName),
	})
	if err != nil {
		return "", err
	}
	normalizedRequest := request
	normalizedRequest.Arguments = data
	return commands.Fingerprint(normalizedRequest)
}

const (
	maxSessionHistoryReadLimit = 200
	// Command arguments cross the JSON/JavaScript boundary. Keep cursors in
	// the protocol's precision-safe integer range even though the durable
	// store represents sequence numbers as int64.
	maxSessionHistoryCursor int64 = 9007199254740991
)

func decodeSessionHistoryReadArguments(raw json.RawMessage) (sessionHistoryReadArguments, error) {
	const command = "session.history.read"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return sessionHistoryReadArguments{}, err
	}
	if err := requireExactFields(fields, command, "session_id", "cursor", "direction", "limit", "align_turn"); err != nil {
		return sessionHistoryReadArguments{}, err
	}
	sessionID, err := requiredCommandString(fields, "session_id", command)
	if err != nil || sessions.ValidateSessionID(sessionID) != nil {
		return sessionHistoryReadArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	cursor, err := optionalCommandInt64(fields, "cursor", command, 1, maxSessionHistoryCursor)
	if err != nil {
		return sessionHistoryReadArguments{}, err
	}
	var direction *string
	if rawDirection, ok := fields["direction"]; ok {
		if strings.TrimSpace(string(rawDirection)) == "null" {
			return sessionHistoryReadArguments{}, fmt.Errorf("invalid %s arguments", command)
		}
		var value string
		if err := json.Unmarshal(rawDirection, &value); err != nil || (value != "before" && value != "after") {
			return sessionHistoryReadArguments{}, fmt.Errorf("invalid %s arguments", command)
		}
		direction = &value
	}
	if cursor == nil {
		if direction != nil {
			return sessionHistoryReadArguments{}, fmt.Errorf("invalid %s arguments", command)
		}
	} else if direction == nil || (*direction != "before" && *direction != "after") {
		return sessionHistoryReadArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	limit := 50
	if _, ok := fields["limit"]; ok {
		limit, err = requiredCommandInt(fields, "limit", command, 1, maxSessionHistoryReadLimit)
		if err != nil {
			return sessionHistoryReadArguments{}, err
		}
	}
	alignTurn := false
	if _, ok := fields["align_turn"]; ok {
		alignTurn, err = requiredCommandBool(fields, "align_turn", command)
		if err != nil {
			return sessionHistoryReadArguments{}, err
		}
	}
	arguments := sessionHistoryReadArguments{SessionID: sessionID, Limit: limit, AlignTurn: alignTurn}
	if cursor != nil {
		arguments.Cursor = cursor
		arguments.Direction = *direction
	}
	return arguments, nil
}

func decodeSessionFullAccessArguments(raw json.RawMessage) (sessionFullAccessArguments, error) {
	const command = "session.set_full_access"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return sessionFullAccessArguments{}, err
	}
	if err := requireExactFields(fields, command, "session_id", "full_access"); err != nil {
		return sessionFullAccessArguments{}, err
	}
	sessionID, err := requiredCommandString(fields, "session_id", command)
	if err != nil {
		return sessionFullAccessArguments{}, err
	}
	fullAccess, err := requiredCommandBool(fields, "full_access", command)
	if err != nil {
		return sessionFullAccessArguments{}, err
	}
	return sessionFullAccessArguments{SessionID: sessionID, FullAccess: fullAccess}, nil
}

func decodeSessionDebugArguments(raw json.RawMessage) (sessionDebugArguments, error) {
	const command = "session.set_debug"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return sessionDebugArguments{}, err
	}
	if err := requireExactFields(fields, command, "session_id", "request_bodies"); err != nil {
		return sessionDebugArguments{}, err
	}
	sessionID, err := requiredCommandString(fields, "session_id", command)
	if err != nil {
		return sessionDebugArguments{}, err
	}
	requestBodies, err := requiredCommandBool(fields, "request_bodies", command)
	if err != nil {
		return sessionDebugArguments{}, err
	}
	return sessionDebugArguments{SessionID: sessionID, RequestBodies: requestBodies}, nil
}

func decodeRunCancelArguments(raw json.RawMessage) (runCancelArguments, error) {
	const command = "run.cancel"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return runCancelArguments{}, err
	}
	if err := requireExactFields(fields, command, "run_id"); err != nil {
		return runCancelArguments{}, err
	}
	runID, err := requiredCommandString(fields, "run_id", command)
	if err != nil {
		return runCancelArguments{}, err
	}
	return runCancelArguments{RunID: runID}, nil
}

func decodeRunStartArguments(raw json.RawMessage) (runStartArguments, error) {
	const command = "run.start"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return runStartArguments{}, err
	}
	if err := requireExactFields(fields, command, "session_id", "run_id", "content"); err != nil {
		return runStartArguments{}, err
	}
	sessionID, err := requiredCommandString(fields, "session_id", command)
	if err != nil || sessions.ValidateSessionID(sessionID) != nil {
		return runStartArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	runID, err := requiredCommandString(fields, "run_id", command)
	if err != nil || sessions.ValidateRunID(runID) != nil {
		return runStartArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	content, err := requiredRunStartContent(fields, command, maxRunStartContentBytes)
	if err != nil {
		return runStartArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	return runStartArguments{SessionID: sessionID, RunID: runID, Content: content}, nil
}

func decodeRunContinueArguments(raw json.RawMessage) (runContinueArguments, error) {
	const command = "run.continue"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return runContinueArguments{}, err
	}
	if err := requireExactFields(fields, command, "session_id", "run_id"); err != nil {
		return runContinueArguments{}, err
	}
	sessionID, err := requiredCommandString(fields, "session_id", command)
	if err != nil || sessions.ValidateSessionID(sessionID) != nil {
		return runContinueArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	runID, err := requiredCommandString(fields, "run_id", command)
	if err != nil || sessions.ValidateRunID(runID) != nil {
		return runContinueArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	return runContinueArguments{SessionID: sessionID, RunID: runID}, nil
}

func decodeRunPromptAppendArguments(raw json.RawMessage) (runPromptAppendArguments, error) {
	const command = "run.prompt.append"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return runPromptAppendArguments{}, err
	}
	if err := requireExactFields(fields, command, "session_id", "run_id", "operation_id", "content"); err != nil {
		return runPromptAppendArguments{}, err
	}
	sessionID, err := requiredCommandString(fields, "session_id", command)
	if err != nil || sessions.ValidateSessionID(sessionID) != nil {
		return runPromptAppendArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	runID, err := requiredCommandString(fields, "run_id", command)
	if err != nil || sessions.ValidateRunID(runID) != nil {
		return runPromptAppendArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	operationID, err := requiredCommandString(fields, "operation_id", command)
	if err != nil || sessions.ValidateOperationID(operationID) != nil {
		return runPromptAppendArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	content, err := requiredRunStartContent(fields, command, maxRunPromptAppendContentBytes)
	if err != nil {
		return runPromptAppendArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	return runPromptAppendArguments{SessionID: sessionID, RunID: runID, OperationID: operationID, Content: content}, nil
}

const maxActiveRunControlIDBytes = 256

func requiredActiveRunControlID(fields map[string]json.RawMessage, name, command string) (string, error) {
	value, err := requiredCommandString(fields, name, command)
	if err != nil || len(value) > maxActiveRunControlIDBytes {
		return "", fmt.Errorf("invalid %s arguments", command)
	}
	return value, nil
}

func decodeRunPromptRemoveArguments(raw json.RawMessage) (runPromptRemoveArguments, error) {
	const command = "run.prompt.remove"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return runPromptRemoveArguments{}, err
	}
	if err := requireExactFields(fields, command, "session_id", "run_id", "prompt_id"); err != nil {
		return runPromptRemoveArguments{}, err
	}
	sessionID, err := requiredCommandString(fields, "session_id", command)
	if err != nil || sessions.ValidateSessionID(sessionID) != nil {
		return runPromptRemoveArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	runID, err := requiredCommandString(fields, "run_id", command)
	if err != nil || sessions.ValidateRunID(runID) != nil {
		return runPromptRemoveArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	promptID, err := requiredActiveRunControlID(fields, "prompt_id", command)
	if err != nil {
		return runPromptRemoveArguments{}, err
	}
	return runPromptRemoveArguments{SessionID: sessionID, RunID: runID, PromptID: promptID}, nil
}

func decodeRunPromptSteerArguments(raw json.RawMessage) (runPromptSteerArguments, error) {
	const command = "run.prompt.steer"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return runPromptSteerArguments{}, err
	}
	if err := requireExactFields(fields, command, "session_id", "run_id", "prompt_id", "steer"); err != nil {
		return runPromptSteerArguments{}, err
	}
	sessionID, err := requiredCommandString(fields, "session_id", command)
	if err != nil || sessions.ValidateSessionID(sessionID) != nil {
		return runPromptSteerArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	runID, err := requiredCommandString(fields, "run_id", command)
	if err != nil || sessions.ValidateRunID(runID) != nil {
		return runPromptSteerArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	promptID, err := requiredActiveRunControlID(fields, "prompt_id", command)
	if err != nil {
		return runPromptSteerArguments{}, err
	}
	steer, err := requiredCommandBool(fields, "steer", command)
	if err != nil {
		return runPromptSteerArguments{}, err
	}
	return runPromptSteerArguments{SessionID: sessionID, RunID: runID, PromptID: promptID, Steer: steer}, nil
}

func decodeRunPromptMoveArguments(raw json.RawMessage) (runPromptMoveArguments, error) {
	const command = "run.prompt.move"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return runPromptMoveArguments{}, err
	}
	if err := requireExactFields(fields, command, "session_id", "run_id", "prompt_id", "delta"); err != nil {
		return runPromptMoveArguments{}, err
	}
	sessionID, err := requiredCommandString(fields, "session_id", command)
	if err != nil || sessions.ValidateSessionID(sessionID) != nil {
		return runPromptMoveArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	runID, err := requiredCommandString(fields, "run_id", command)
	if err != nil || sessions.ValidateRunID(runID) != nil {
		return runPromptMoveArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	promptID, err := requiredActiveRunControlID(fields, "prompt_id", command)
	if err != nil {
		return runPromptMoveArguments{}, err
	}
	delta, err := requiredCommandInt(fields, "delta", command, -maxActivePromptMoveDelta, maxActivePromptMoveDelta)
	if err != nil {
		return runPromptMoveArguments{}, err
	}
	return runPromptMoveArguments{SessionID: sessionID, RunID: runID, PromptID: promptID, Delta: delta}, nil
}

func decodeRunToolCancelArguments(raw json.RawMessage) (runToolCancelArguments, error) {
	const command = "run.tool.cancel"
	fields, err := strictCommandObject(raw, command)
	if err != nil {
		return runToolCancelArguments{}, err
	}
	if err := requireExactFields(fields, command, "session_id", "run_id", "tool_call_id"); err != nil {
		return runToolCancelArguments{}, err
	}
	sessionID, err := requiredCommandString(fields, "session_id", command)
	if err != nil || sessions.ValidateSessionID(sessionID) != nil {
		return runToolCancelArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	runID, err := requiredCommandString(fields, "run_id", command)
	if err != nil || sessions.ValidateRunID(runID) != nil {
		return runToolCancelArguments{}, fmt.Errorf("invalid %s arguments", command)
	}
	toolCallID, err := requiredActiveRunControlID(fields, "tool_call_id", command)
	if err != nil {
		return runToolCancelArguments{}, err
	}
	return runToolCancelArguments{SessionID: sessionID, RunID: runID, ToolCallID: toolCallID}, nil
}

func validateSessionMarkReadArguments(raw json.RawMessage) error {
	_, err := decodeSessionMarkReadArguments(raw)
	return err
}

func validateSessionRenameArguments(raw json.RawMessage) error {
	_, err := decodeSessionRenameArguments(raw)
	return err
}

func validateSessionIDArguments(raw json.RawMessage, command string) error {
	_, err := decodeSessionIDArguments(raw, command)
	return err
}

func validateSessionFullAccessArguments(raw json.RawMessage) error {
	_, err := decodeSessionFullAccessArguments(raw)
	return err
}

func validateSessionDebugArguments(raw json.RawMessage) error {
	_, err := decodeSessionDebugArguments(raw)
	return err
}

func validateSessionCreateArguments(raw json.RawMessage) error {
	_, err := decodeSessionCreateArguments(raw)
	return err
}

func validateRunCancelArguments(raw json.RawMessage) error {
	_, err := decodeRunCancelArguments(raw)
	return err
}

func validateRunStartArguments(raw json.RawMessage) error {
	_, err := decodeRunStartArguments(raw)
	return err
}

func validateRunContinueArguments(raw json.RawMessage) error {
	_, err := decodeRunContinueArguments(raw)
	return err
}

func validateRunPromptAppendArguments(raw json.RawMessage) error {
	_, err := decodeRunPromptAppendArguments(raw)
	return err
}

func validateRunPromptRemoveArguments(raw json.RawMessage) error {
	_, err := decodeRunPromptRemoveArguments(raw)
	return err
}

func validateRunPromptSteerArguments(raw json.RawMessage) error {
	_, err := decodeRunPromptSteerArguments(raw)
	return err
}

func validateRunPromptMoveArguments(raw json.RawMessage) error {
	_, err := decodeRunPromptMoveArguments(raw)
	return err
}

func validateRunToolCancelArguments(raw json.RawMessage) error {
	_, err := decodeRunToolCancelArguments(raw)
	return err
}

func validateSessionHistoryReadArguments(raw json.RawMessage) error {
	_, err := decodeSessionHistoryReadArguments(raw)
	return err
}

func sessionCommandError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		return commands.NewDomainError("not_found", "session not found", err)
	case errors.Is(err, execution.ErrSessionRunCoordinatorCapacity):
		return commands.NewDomainError("capacity", "too many runs are currently active", err)
	case errors.Is(err, execution.ErrSessionBusy):
		return commands.NewDomainError("session_busy", "session is busy", err)
	case errors.Is(err, context.Canceled):
		return commands.NewDomainError("cancelled", "command was cancelled", err)
	case errors.Is(err, execution.ErrSessionArchived):
		return commands.NewDomainError("session_archived", "session is archived", err)
	case errors.Is(err, execution.ErrSessionArchiveFirst):
		return commands.NewDomainError("archive_first", "session must be archived before removal", err)
	case errors.Is(err, execution.ErrPromptAppendRunNotFound):
		return commands.NewDomainError("run_not_found", "target run not found", err)
	case errors.Is(err, execution.ErrPromptAppendWrongSession):
		return commands.NewDomainError("run_wrong_session", "target run belongs to another session", err)
	case errors.Is(err, execution.ErrPromptAppendRunSettled):
		return commands.NewDomainError("run_settled", "target run is settled", err)
	case errors.Is(err, execution.ErrPromptAppendRunNotActive):
		return commands.NewDomainError("run_not_active", "target run is not the active run", err)
	case errors.Is(err, execution.ErrPromptAppendOutcomeUnknown):
		return commands.NewDomainError("operation_outcome_unknown", "prompt append may already have been applied; it will not be replayed automatically", err)
	case errors.Is(err, execution.ErrPromptAppendNotApplied):
		return commands.NewDomainError("operation_not_applied", "prompt append was not applied; retrying will not replay it", err)
	case errors.Is(err, execution.ErrRunControlNotFound):
		return commands.NewDomainError("run_not_found", "target run not found", err)
	case errors.Is(err, execution.ErrRunControlWrongSession):
		return commands.NewDomainError("run_wrong_session", "target run belongs to another session", err)
	case errors.Is(err, execution.ErrRunControlRunSettled):
		return commands.NewDomainError("run_settled", "target run is settled", err)
	case errors.Is(err, execution.ErrRunControlNotActive):
		return commands.NewDomainError("run_not_active", "target run is not active", err)
	case errors.Is(err, execution.ErrRunControlPromptNotFound):
		return commands.NewDomainError("prompt_not_found", "queued prompt not found", err)
	case errors.Is(err, execution.ErrRunControlToolNotActive):
		return commands.NewDomainError("tool_call_not_active", "tool call is not active", err)
	case errors.Is(err, sessions.ErrIdempotencyConflict):
		return commands.NewDomainError("idempotency_conflict", "client identity conflicts with an existing durable operation", err)
	default:
		return commands.NewDomainError("command_failed", "command execution failed", err)
	}
}

func projectCommandError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, projectstore.ErrNotFound):
		return commands.NewDomainError("project_not_found", "project not found", err)
	case errors.Is(err, projectstore.ErrIdempotencyConflict):
		return commands.NewDomainError("idempotency_conflict", "client identity conflicts with an existing durable operation", err)
	case errors.Is(err, projectstore.ErrOperationOutcomeUnknown):
		return commands.NewDomainError("operation_outcome_unknown", "project create outcome is unknown; it will not be replayed automatically", err)
	case errors.Is(err, projectstore.ErrOperationNotApplied):
		return commands.NewDomainError("operation_not_applied", "project create was not applied; retrying will not replay it", err)
	case errors.Is(err, execution.ErrSessionBusy):
		return commands.NewDomainError("session_busy", "project has a busy session", err)
	case errors.Is(err, execution.ErrProjectArchived):
		return commands.NewDomainError("project_archived", "project is archived", err)
	case errors.Is(err, execution.ErrProjectArchiveFirst):
		return commands.NewDomainError("archive_first", "project must be archived before removal", err)
	case errors.Is(err, context.Canceled):
		return commands.NewDomainError("cancelled", "command was cancelled", err)
	default:
		return commands.NewDomainError("command_failed", "project command failed", err)
	}
}

func providerCommandError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return commands.NewDomainError("cancelled", "command was cancelled", nil)
	}
	// Provider service errors can contain endpoint details, filesystem paths,
	// or an implementation-specific diagnostic. In particular, never expose
	// an authentication failure that might include a credential. The command
	// boundary therefore has one deliberately small public error vocabulary.
	return commands.NewDomainError("provider_command_failed", "provider command failed", nil)
}

func codexLoginCommandError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return commands.NewDomainError("cancelled", "command was cancelled", nil)
	}
	return commands.NewDomainError(codexLoginFailureCode(err), codexLoginFailureMessage(err), nil)
}

// The command cache is an idempotency tombstone, not a second copy of the
// target. Provider arguments can contain credentials in any field (including
// arbitrary model parameters), so retain no argument bytes at all. The
// provider.create operation ID remains in the cache key/fingerprint, not in
// cached arguments; the dispatcher retains only this minimal tombstone.
func redactProviderArguments(_ json.RawMessage) json.RawMessage {
	return json.RawMessage(`{}`)
}

func redactProviderCreateArguments(raw json.RawMessage) json.RawMessage {
	return redactProviderArguments(raw)
}

func redactProviderUpdateArguments(raw json.RawMessage) json.RawMessage {
	return redactProviderArguments(raw)
}

func redactCodexLoginArguments(_ json.RawMessage) json.RawMessage {
	return json.RawMessage(`{}`)
}

type sessionHistoryBlobWriter interface {
	Put(context.Context, string, []byte) (protocol.BlobDescriptor, error)
}

// sessionCommandRegistryOptions is deliberately typed. The command registry
// is a closed assembly boundary; silently accepting an arbitrary option (or
// silently letting a duplicate option win) would make a missing dependency a
// runtime behavior instead of a compile-time review point.
type sessionCommandRegistryOptions struct {
	HistoryWriter sessionHistoryBlobWriter
	CodexLogins   *codexLoginRegistry
}

const (
	maxSessionHistoryInlineBytes = 64 * 1024
	maxSessionHistoryBlobBytes   = 16 * 1024 * 1024
	maxProviderDiscoverModels    = 4096
	maxProviderDiscoverIDBytes   = 4096
	maxProviderDiscoverBytes     = 8 * 1024 * 1024
	maxProviderDiscoverInline    = 64 * 1024
)

func validateProviderDiscoverBlobDescriptor(descriptor protocol.BlobDescriptor, content []byte) error {
	if descriptor.ContentType != "application/json" || descriptor.Size != uint64(len(content)) || descriptor.Size > maxProviderDiscoverBytes {
		return errors.New("provider model blob descriptor size or content type is invalid")
	}
	if err := protocol.ValidateBlobDescriptor(descriptor); err != nil {
		return err
	}
	digest := sha256.Sum256(content)
	if !strings.EqualFold(descriptor.SHA256, hex.EncodeToString(digest[:])) {
		return errors.New("provider model blob descriptor hash is invalid")
	}
	return nil
}

func sessionDeleteCommandError(err error) error {
	if errors.Is(err, execution.ErrSessionArchiveFirst) {
		return commands.NewDomainError("archive_first", "session must be archived before removal", err)
	}
	return sessionCommandError(err)
}

func sessionCompactCommandError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled):
		return commands.NewDomainError("cancelled", "command was cancelled", err)
	case errors.Is(err, execution.ErrTurnFailed):
		return commands.NewDomainError("compact_failed", "session compaction failed", err)
	default:
		return sessionCommandError(err)
	}
}

func sessionHistoryReadCommandError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return commands.NewDomainError("cancelled", "command was cancelled", err)
	}
	return sessionCommandError(err)
}

func newSessionCommandRegistry(service *execution.Service, runs *runRegistry, options sessionCommandRegistryOptions) (*commands.Registry, error) {
	historyWriter := options.HistoryWriter
	codexLogins := options.CodexLogins
	return commands.NewRegistry(
		commands.CommandDefinition{
			// Starting device login performs an external operation without a
			// durable claim/outcome. It can join an existing pending login in
			// this epoch, but a reconnect in a new epoch must not replay it.
			Name: "codex_login.start", SchemaVersion: 1, CrossEpochRetrySafe: false,
			CachePolicy: commands.ResultCacheDurable, SupportsExpectedRevision: false,
			RedactArguments: redactCodexLoginArguments,
			Validate:        validateCodexLoginStartArguments,
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeCodexLoginArguments(request.Arguments, "codex_login.start")
				if err != nil {
					return nil, err
				}
				if codexLogins == nil {
					return nil, commands.NewDomainError("codex_provider_unavailable", "Codex provider is unavailable", nil)
				}
				if _, err := codexLogins.start(arguments.Provider); err != nil {
					return nil, codexLoginCommandError(err)
				}
				// The resource, not this promise, carries device UI capability
				// fields and all later auth outcomes.
				return json.Marshal(codexLoginResult{Provider: arguments.Provider, Status: "accepted"})
			},
		},
		commands.CommandDefinition{
			// Clear is a target-state operation: os.Remove is idempotent and
			// the resource owner suppresses unchanged signed_out publications.
			// It is therefore safe to repeat after an epoch change.
			Name: "codex_login.clear", SchemaVersion: 1, CrossEpochRetrySafe: true,
			CachePolicy: commands.ResultCacheDurable, SupportsExpectedRevision: false,
			RedactArguments: redactCodexLoginArguments,
			Validate:        validateCodexLoginClearArguments,
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeCodexLoginArguments(request.Arguments, "codex_login.clear")
				if err != nil {
					return nil, err
				}
				if codexLogins == nil {
					return nil, commands.NewDomainError("codex_provider_unavailable", "Codex provider is unavailable", nil)
				}
				if err := codexLogins.clear(arguments.Provider); err != nil {
					return nil, codexLoginCommandError(err)
				}
				return json.Marshal(codexLoginResult{Provider: arguments.Provider, Status: "cleared"})
			},
		},
		commands.CommandDefinition{
			// CreateProviderSettings has no durable operation claim/outcome
			// authority. The gateway cache deduplicates a request only within
			// this server epoch; a pending create must therefore not be replayed
			// automatically across an epoch. A later explicit create observes the
			// execution-owned provider file: an exact duplicate and a conflicting
			// target both fail with the same bounded public error.
			Name: "provider.create", SchemaVersion: 1, CrossEpochRetrySafe: false,
			CachePolicy: commands.ResultCacheDurable, SupportsExpectedRevision: false,
			RedactArguments: redactProviderCreateArguments,
			Validate:        validateProviderCreateArguments,
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeProviderCreateArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				if service == nil {
					return nil, commands.NewDomainError("provider_unavailable", "provider service is not configured", nil)
				}
				write, err := service.CreateProviderSettingsWithResult(arguments.Input)
				if err != nil {
					return nil, providerCommandError(err)
				}
				// This is a bounded acknowledgement only. ProviderSettings is the
				// authority and reaches the client through its resource stream.
				return json.Marshal(providerCreateResult{OperationID: arguments.OperationID, Provider: arguments.Provider, Status: "applied", Changed: write.Changed})
			},
		},
		commands.CommandDefinition{
			Name: "provider.update", SchemaVersion: 1, CrossEpochRetrySafe: true,
			CachePolicy: commands.ResultCacheDurable, SupportsExpectedRevision: false,
			RedactArguments: redactProviderUpdateArguments,
			Validate:        validateProviderUpdateArguments,
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeProviderUpdateArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				if service == nil {
					return nil, commands.NewDomainError("provider_unavailable", "provider service is not configured", nil)
				}
				write, err := service.UpdateProviderSettingsWithResult(arguments.Provider, arguments.Input)
				if err != nil {
					return nil, providerCommandError(err)
				}
				return json.Marshal(providerMutationResult{Provider: arguments.Provider, Status: "applied", Changed: write.Changed})
			},
		},
		commands.CommandDefinition{
			Name: "provider.set_default", SchemaVersion: 1, CrossEpochRetrySafe: true,
			CachePolicy: commands.ResultCacheDurable, SupportsExpectedRevision: false,
			Validate: validateProviderDefaultArguments,
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeProviderDefaultArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				if service == nil {
					return nil, commands.NewDomainError("provider_unavailable", "provider service is not configured", nil)
				}
				if _, err := service.UpdateDefaultProviderModel(arguments.Provider, arguments.Model); err != nil {
					return nil, providerCommandError(err)
				}
				// This is an acknowledgement only. ProviderSettings is the
				// authority and reaches the client through its resource stream.
				return json.Marshal(providerDefaultResult{Provider: arguments.Provider, Model: arguments.Model, Status: "applied"})
			},
		},
		commands.CommandDefinition{
			Name: "provider.discover_models", SchemaVersion: 1, CrossEpochRetrySafe: true,
			CachePolicy: commands.ResultCacheVolatile, SupportsExpectedRevision: false,
			Validate: validateProviderDiscoverArguments,
			Execute: func(ctx context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeProviderDiscoverArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				if service == nil {
					return nil, commands.NewDomainError("provider_unavailable", "provider service is not configured", nil)
				}
				models, err := service.DiscoverProviderModels(ctx, arguments.Provider)
				if err != nil {
					return nil, providerCommandError(err)
				}
				if len(models) > maxProviderDiscoverModels {
					return nil, commands.NewDomainError("provider_result_too_large", "provider model result is too large", nil)
				}
				seen := make(map[string]struct{}, len(models))
				normalized := make([]string, 0, len(models))
				totalBytes := 0
				for _, model := range models {
					if !utf8.ValidString(model) || len([]byte(model)) == 0 || len([]byte(model)) > maxProviderDiscoverIDBytes {
						return nil, commands.NewDomainError("provider_result_invalid", "provider model result is invalid", nil)
					}
					if _, duplicate := seen[model]; duplicate {
						continue
					}
					seen[model] = struct{}{}
					normalized = append(normalized, model)
					totalBytes += len([]byte(model))
					if totalBytes > maxProviderDiscoverBytes {
						return nil, commands.NewDomainError("provider_result_too_large", "provider model result is too large", nil)
					}
				}
				sort.Strings(normalized)
				modelBytes, err := json.Marshal(normalized)
				if err != nil {
					return nil, commands.NewDomainError("provider_result_invalid", "provider model result is invalid", nil)
				}
				if len(modelBytes) > maxProviderDiscoverBytes {
					return nil, commands.NewDomainError("provider_result_too_large", "provider model result is too large", nil)
				}
				inline, err := json.Marshal(providerDiscoverInlineResult{Provider: arguments.Provider, Models: normalized})
				if err != nil {
					return nil, commands.NewDomainError("provider_result_invalid", "provider model result is invalid", err)
				}
				if len(inline) <= maxProviderDiscoverInline || historyWriter == nil {
					if historyWriter == nil && len(inline) > maxProviderDiscoverInline {
						return nil, commands.NewDomainError("provider_result_too_large", "provider model result is too large", nil)
					}
					return inline, nil
				}
				descriptor, err := historyWriter.Put(ctx, "application/json", modelBytes)
				if err != nil {
					return nil, providerCommandError(err)
				}
				if err := validateProviderDiscoverBlobDescriptor(descriptor, modelBytes); err != nil {
					return nil, commands.NewDomainError("provider_result_invalid", "provider model result is invalid", nil)
				}
				return json.Marshal(providerDiscoverBlobResult{Provider: arguments.Provider, Blob: &descriptor})
			},
		},
		commands.CommandDefinition{
			Name: "project.create", SchemaVersion: 1, CrossEpochRetrySafe: true,
			SupportsExpectedRevision: false, Validate: validateProjectCreateArguments,
			Execute: func(ctx context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeProjectCreateArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				fingerprint, err := normalizedProjectCreateFingerprint(request, arguments)
				if err != nil {
					return nil, commands.NewDomainError("invalid", "project root is invalid", err)
				}
				if service == nil {
					return nil, commands.NewDomainError("project_unavailable", "project service is not configured", nil)
				}
				result, err := service.CreateProjectIdempotent(ctx, arguments.OperationID, fingerprint, arguments.Root, arguments.DisplayName)
				if err != nil {
					return nil, projectCommandError(err)
				}
				return json.Marshal(projectCreateResult{OperationID: arguments.OperationID, ProjectID: result.Project.ID, Created: result.Created})
			},
		},
		commands.CommandDefinition{
			Name: "project.rename", SchemaVersion: 1, CrossEpochRetrySafe: true,
			Validate: validateProjectRenameArguments,
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeProjectRenameArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				if service == nil {
					return nil, commands.NewDomainError("project_unavailable", "project service is not configured", nil)
				}
				result, err := service.RenameProject(arguments.ProjectID, arguments.DisplayName)
				if err != nil {
					return nil, projectCommandError(err)
				}
				return json.Marshal(projectRenameResult{ProjectID: result.ID, DisplayName: result.DisplayName})
			},
		},
		commands.CommandDefinition{
			Name: "project.archive", SchemaVersion: 1, CrossEpochRetrySafe: true,
			Validate: func(raw json.RawMessage) error { return validateProjectIDArguments(raw, "project.archive") },
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeProjectIDArguments(request.Arguments, "project.archive")
				if err != nil {
					return nil, err
				}
				if service == nil {
					return nil, commands.NewDomainError("project_unavailable", "project service is not configured", nil)
				}
				result, err := service.ArchiveProject(arguments.ProjectID)
				if err != nil {
					return nil, projectCommandError(err)
				}
				return json.Marshal(projectArchiveResult{ProjectID: result.ID, Archived: result.Archived})
			},
		},
		commands.CommandDefinition{
			Name: "project.restore", SchemaVersion: 1, CrossEpochRetrySafe: true,
			Validate: func(raw json.RawMessage) error { return validateProjectIDArguments(raw, "project.restore") },
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeProjectIDArguments(request.Arguments, "project.restore")
				if err != nil {
					return nil, err
				}
				if service == nil {
					return nil, commands.NewDomainError("project_unavailable", "project service is not configured", nil)
				}
				result, err := service.RestoreProject(arguments.ProjectID)
				if err != nil {
					return nil, projectCommandError(err)
				}
				return json.Marshal(projectArchiveResult{ProjectID: result.ID, Archived: result.Archived})
			},
		},
		commands.CommandDefinition{
			Name: "project.delete", SchemaVersion: 1, CrossEpochRetrySafe: false,
			Validate: func(raw json.RawMessage) error { return validateProjectIDArguments(raw, "project.delete") },
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeProjectIDArguments(request.Arguments, "project.delete")
				if err != nil {
					return nil, err
				}
				if service == nil {
					return nil, commands.NewDomainError("project_unavailable", "project service is not configured", nil)
				}
				result, err := service.RemoveProject(arguments.ProjectID)
				if err != nil {
					return nil, projectCommandError(err)
				}
				return json.Marshal(projectDeleteResult{ProjectID: result.ID, Status: result.Status, RemovedSessions: result.RemovedSessions})
			},
		},
		commands.CommandDefinition{
			Name: "run.prompt.remove", SchemaVersion: 1, CrossEpochRetrySafe: false,
			Validate: validateRunPromptRemoveArguments,
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeRunPromptRemoveArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				if runs == nil {
					return nil, sessionCommandError(execution.ErrRunControlNotFound)
				}
				if err := runs.removeActivePrompt(arguments.SessionID, arguments.RunID, arguments.PromptID); err != nil {
					return nil, sessionCommandError(err)
				}
				return json.Marshal(runPromptRemoveResult{SessionID: arguments.SessionID, RunID: arguments.RunID, PromptID: arguments.PromptID, Removed: true})
			},
		},
		commands.CommandDefinition{
			Name: "run.prompt.steer", SchemaVersion: 1, CrossEpochRetrySafe: false,
			Validate: validateRunPromptSteerArguments,
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeRunPromptSteerArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				if runs == nil {
					return nil, sessionCommandError(execution.ErrRunControlNotFound)
				}
				if err := runs.steerActivePrompt(arguments.SessionID, arguments.RunID, arguments.PromptID, arguments.Steer); err != nil {
					return nil, sessionCommandError(err)
				}
				return json.Marshal(runPromptSteerResult{SessionID: arguments.SessionID, RunID: arguments.RunID, PromptID: arguments.PromptID, Steer: arguments.Steer})
			},
		},
		commands.CommandDefinition{
			Name: "run.prompt.move", SchemaVersion: 1, CrossEpochRetrySafe: false,
			Validate: validateRunPromptMoveArguments,
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeRunPromptMoveArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				if runs == nil {
					return nil, sessionCommandError(execution.ErrRunControlNotFound)
				}
				moved, err := runs.moveActivePrompt(arguments.SessionID, arguments.RunID, arguments.PromptID, arguments.Delta)
				if err != nil {
					return nil, sessionCommandError(err)
				}
				return json.Marshal(runPromptMoveResult{SessionID: arguments.SessionID, RunID: arguments.RunID, PromptID: arguments.PromptID, Moved: moved})
			},
		},
		commands.CommandDefinition{
			Name: "run.tool.cancel", SchemaVersion: 1, CrossEpochRetrySafe: false,
			Validate: validateRunToolCancelArguments,
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeRunToolCancelArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				if runs == nil {
					return nil, sessionCommandError(execution.ErrRunControlNotFound)
				}
				if err := runs.cancelToolCall(arguments.SessionID, arguments.RunID, arguments.ToolCallID); err != nil {
					return nil, sessionCommandError(err)
				}
				return json.Marshal(runToolCancelResult{SessionID: arguments.SessionID, RunID: arguments.RunID, ToolCallID: arguments.ToolCallID, Cancelled: true})
			},
		},
		commands.CommandDefinition{
			Name: "run.prompt.append", SchemaVersion: 1, CrossEpochRetrySafe: true,
			Validate: validateRunPromptAppendArguments,
			Execute: func(ctx context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeRunPromptAppendArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				if service == nil {
					return nil, commands.NewDomainError("run_unavailable", "run execution is not configured", nil)
				}
				result, err := service.AppendPromptDurable(ctx, arguments.SessionID, arguments.RunID, arguments.OperationID, arguments.Content)
				if err != nil {
					return nil, sessionCommandError(err)
				}
				if !result.Accepted {
					return nil, commands.NewDomainError("operation_outcome_unknown", "prompt append was not durably accepted; it will not be replayed automatically", nil)
				}
				return json.Marshal(runPromptAppendResult{
					OperationID: result.OperationID,
					SessionID:   result.SessionID,
					RunID:       result.RunID,
					Accepted:    result.Accepted,
				})
			},
		},
		commands.CommandDefinition{
			Name: "run.start", SchemaVersion: 1, CrossEpochRetrySafe: true,
			Validate: validateRunStartArguments,
			Execute: func(ctx context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeRunStartArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				fingerprint, err := runStartFingerprint(request, arguments)
				if err != nil {
					return nil, commands.NewDomainError("invalid_fingerprint", "command fingerprint is invalid", err)
				}
				if runs == nil {
					return nil, commands.NewDomainError("run_unavailable", "run execution is not configured", nil)
				}
				status, err := runs.startDurable(arguments.SessionID, arguments.Content, arguments.RunID, fingerprint)
				if err != nil {
					return nil, sessionCommandError(err)
				}
				return json.Marshal(runStartResult{SessionID: arguments.SessionID, RunID: arguments.RunID, Status: status})
			},
		},
		commands.CommandDefinition{
			Name: "run.continue", SchemaVersion: 1, CrossEpochRetrySafe: true,
			Validate: validateRunContinueArguments,
			Execute: func(ctx context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeRunContinueArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				fingerprint, err := runContinueFingerprint(request, arguments)
				if err != nil {
					return nil, commands.NewDomainError("invalid_fingerprint", "command fingerprint is invalid", err)
				}
				if runs == nil {
					return nil, commands.NewDomainError("run_unavailable", "run execution is not configured", nil)
				}
				status, err := runs.continueDurable(arguments.SessionID, arguments.RunID, fingerprint)
				if err != nil {
					return nil, sessionCommandError(err)
				}
				return json.Marshal(runContinueResult{SessionID: arguments.SessionID, RunID: arguments.RunID, Status: status})
			},
		},
		commands.CommandDefinition{
			Name: "session.create", SchemaVersion: 1, CrossEpochRetrySafe: true,
			// The create primitive is durable at the session store. Expected
			// revisions are deliberately rejected by the gateway because this
			// command has no revision-conditional create semantics.
			SupportsExpectedRevision: false,
			Validate:                 validateSessionCreateArguments,
			Execute: func(ctx context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeSessionCreateArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				fingerprint, err := normalizedSessionCreateFingerprint(request, arguments)
				if err != nil {
					return nil, commands.NewDomainError("invalid_fingerprint", "command fingerprint is invalid", err)
				}
				displayName := ""
				if arguments.DisplayName != nil {
					displayName = *arguments.DisplayName
				}
				if arguments.ParentSessionID != nil {
					result, _, err := service.CreateInheritedSessionIdempotent(ctx, arguments.ProjectID, *arguments.ParentSessionID, displayName, arguments.SessionID, fingerprint)
					if err != nil {
						return nil, sessionCommandError(err)
					}
					return json.Marshal(sessionCreateResult{SessionID: result.ID, ProjectID: result.ProjectID})
				}
				options := execution.ConfiguredSessionOptions{DisplayName: displayName}
				if arguments.ParentSessionID != nil {
					options.ParentSessionID = *arguments.ParentSessionID
				}
				if arguments.CWD != nil {
					options.CWD = *arguments.CWD
				}
				if arguments.ConfigPath != nil {
					options.ConfigPath = *arguments.ConfigPath
				}
				if arguments.Provider != nil {
					options.Provider = *arguments.Provider
				}
				if arguments.ModelProfile != nil {
					options.ModelProfile = *arguments.ModelProfile
				}
				if arguments.ReasoningLevel != nil {
					options.ReasoningLevel = *arguments.ReasoningLevel
				}
				if arguments.FullAccess != nil {
					options.FullAccess = *arguments.FullAccess
				}
				result, _, err := service.CreateConfiguredSessionIdempotent(ctx, arguments.ProjectID, arguments.SessionID, fingerprint, options)
				if err != nil {
					return nil, sessionCommandError(err)
				}
				return json.Marshal(sessionCreateResult{SessionID: result.ID, ProjectID: result.ProjectID})
			},
		},
		commands.CommandDefinition{
			Name: "session.mark_read", SchemaVersion: 1, CrossEpochRetrySafe: true,
			Validate: validateSessionMarkReadArguments,
			Execute: func(ctx context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeSessionMarkReadArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				projectID := ""
				if arguments.ProjectID != nil {
					projectID = *arguments.ProjectID
				}
				result, err := service.MarkSessionReadContext(ctx, arguments.SessionID, arguments.RunID, projectID)
				if err != nil {
					return nil, sessionCommandError(err)
				}
				return json.Marshal(result)
			},
		},
		commands.CommandDefinition{
			Name: "session.rename", SchemaVersion: 1, CrossEpochRetrySafe: true,
			Validate: validateSessionRenameArguments,
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeSessionRenameArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				result, err := service.RenameSession(arguments.SessionID, arguments.DisplayName)
				if err != nil {
					return nil, sessionCommandError(err)
				}
				return json.Marshal(sessionRenameResult{SessionID: result.ID, DisplayName: result.DisplayName})
			},
		},
		commands.CommandDefinition{
			Name: "session.delete", SchemaVersion: 1, CrossEpochRetrySafe: false,
			Validate: func(raw json.RawMessage) error { return validateSessionIDArguments(raw, "session.delete") },
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeSessionIDArguments(request.Arguments, "session.delete")
				if err != nil {
					return nil, err
				}
				if service == nil {
					return nil, commands.NewDomainError("session_unavailable", "session service is not configured", nil)
				}
				result, err := service.RemoveSession(arguments.SessionID)
				if err != nil {
					return nil, sessionDeleteCommandError(err)
				}
				return json.Marshal(sessionDeleteResult{
					SessionID:       result.ID,
					Status:          result.Status,
					RemovedSessions: result.RemovedSessions,
				})
			},
		},
		commands.CommandDefinition{
			Name: "session.compact", SchemaVersion: 1, CrossEpochRetrySafe: false,
			Validate: func(raw json.RawMessage) error { return validateSessionIDArguments(raw, "session.compact") },
			Execute: func(ctx context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeSessionIDArguments(request.Arguments, "session.compact")
				if err != nil {
					return nil, err
				}
				if service == nil {
					return nil, commands.NewDomainError("session_unavailable", "session service is not configured", nil)
				}
				result, err := service.CompactSession(ctx, arguments.SessionID)
				if err != nil {
					return nil, sessionCompactCommandError(err)
				}
				return json.Marshal(sessionCompactCommandResult{
					SessionID:     arguments.SessionID,
					Status:        result.Status,
					CompactionID:  result.CompactionID,
					SummaryItemID: result.SummaryItemID,
					Revision:      strconv.FormatInt(result.LastSeq, 10),
				})
			},
		},
		commands.CommandDefinition{
			Name: "session.history.read", SchemaVersion: 1, CrossEpochRetrySafe: true,
			CachePolicy: commands.ResultCacheVolatile,
			Validate:    validateSessionHistoryReadArguments,
			Execute: func(ctx context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeSessionHistoryReadArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				if service == nil {
					return nil, commands.NewDomainError("session_unavailable", "session service is not configured", nil)
				}
				options := execution.SessionItemsOptions{Limit: arguments.Limit, AlignTurn: arguments.AlignTurn}
				if arguments.Cursor != nil {
					if arguments.Direction == "before" {
						options.BeforeSeq = *arguments.Cursor
					} else {
						options.AfterSeq = *arguments.Cursor
					}
				}
				page, err := service.GetSessionChatItemsPage(arguments.SessionID, options)
				if err != nil {
					return nil, sessionHistoryReadCommandError(err)
				}
				encoded, err := json.Marshal(page)
				if err != nil {
					return nil, commands.NewDomainError("history_read_failed", "session history could not be encoded", err)
				}
				result := sessionHistoryReadResult{
					SessionID: arguments.SessionID,
					Limit:     arguments.Limit,
					AlignTurn: arguments.AlignTurn,
					History:   nil,
					Blob:      nil,
				}
				if arguments.Cursor != nil {
					result.Cursor = *arguments.Cursor
					result.Direction = arguments.Direction
				}
				if len(encoded) <= maxSessionHistoryInlineBytes {
					result.History = &page
					return json.Marshal(result)
				}
				if len(encoded) > maxSessionHistoryBlobBytes {
					return nil, commands.NewDomainError("history_too_large", "session history page is too large", nil)
				}
				if historyWriter == nil {
					return nil, commands.NewDomainError("history_blob_unavailable", "session history blob delivery is unavailable", nil)
				}
				descriptor, err := historyWriter.Put(ctx, "application/json", encoded)
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return nil, sessionHistoryReadCommandError(err)
					}
					return nil, commands.NewDomainError("history_blob_failed", "session history blob could not be created", err)
				}
				if descriptor.ContentType != "application/json" || descriptor.Size > maxSessionHistoryBlobBytes || protocol.ValidateBlobDescriptor(descriptor) != nil {
					return nil, commands.NewDomainError("history_blob_failed", "session history blob descriptor is invalid", nil)
				}
				result.Blob = &descriptor
				return json.Marshal(result)
			},
		},
		commands.CommandDefinition{
			Name: "session.archive", SchemaVersion: 1, CrossEpochRetrySafe: true,
			Validate: func(raw json.RawMessage) error { return validateSessionIDArguments(raw, "session.archive") },
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeSessionIDArguments(request.Arguments, "session.archive")
				if err != nil {
					return nil, err
				}
				if _, err := service.ArchiveSession(arguments.SessionID); err != nil {
					return nil, sessionCommandError(err)
				}
				return json.Marshal(sessionArchiveResult{SessionID: arguments.SessionID, Archived: true})
			},
		},
		commands.CommandDefinition{
			Name: "session.restore", SchemaVersion: 1, CrossEpochRetrySafe: true,
			Validate: func(raw json.RawMessage) error { return validateSessionIDArguments(raw, "session.restore") },
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeSessionIDArguments(request.Arguments, "session.restore")
				if err != nil {
					return nil, err
				}
				if _, err := service.RestoreSession(arguments.SessionID); err != nil {
					return nil, sessionCommandError(err)
				}
				return json.Marshal(sessionArchiveResult{SessionID: arguments.SessionID, Archived: false})
			},
		},
		commands.CommandDefinition{
			Name: "session.set_full_access", SchemaVersion: 1, CrossEpochRetrySafe: true,
			Validate: validateSessionFullAccessArguments,
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeSessionFullAccessArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				if _, err := service.SetSessionFullAccess(arguments.SessionID, arguments.FullAccess); err != nil {
					return nil, sessionCommandError(err)
				}
				return json.Marshal(sessionFullAccessResult{SessionID: arguments.SessionID, FullAccess: arguments.FullAccess})
			},
		},
		commands.CommandDefinition{
			Name: "session.set_debug", SchemaVersion: 1, CrossEpochRetrySafe: true,
			Validate: validateSessionDebugArguments,
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeSessionDebugArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				if _, err := service.SetSessionDebug(arguments.SessionID, sessions.DebugSettings{RequestBodies: arguments.RequestBodies}); err != nil {
					return nil, sessionCommandError(err)
				}
				return json.Marshal(sessionDebugResult{SessionID: arguments.SessionID, RequestBodies: arguments.RequestBodies})
			},
		},
		commands.CommandDefinition{
			Name: "run.cancel", SchemaVersion: 1, CrossEpochRetrySafe: false,
			Validate: validateRunCancelArguments,
			Execute: func(_ context.Context, request commands.CommandRequest) (json.RawMessage, error) {
				arguments, err := decodeRunCancelArguments(request.Arguments)
				if err != nil {
					return nil, err
				}
				if runs == nil {
					return nil, commands.NewDomainError("run_not_found", "run not found", nil)
				}
				managed, ok := runs.cancel(arguments.RunID)
				if !ok || managed == nil || managed.run == nil {
					return nil, commands.NewDomainError("run_not_found", "run not found", nil)
				}
				return json.Marshal(runCancelResult{RunID: arguments.RunID, Status: string(managed.run.Status())})
			},
		},
	)
}
