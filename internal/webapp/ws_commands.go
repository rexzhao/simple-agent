package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/projects"
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

// strictCommandObject parses one JSON object, rejects duplicate keys and
// trailing values, and leaves field-level type checking to the command
// decoder. This is intentionally stricter than encoding/json's usual struct
// decoding, where duplicate fields are silently overwritten.
func strictCommandObject(raw json.RawMessage, command string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
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
	if err != nil || len(projectID) > 128 || projects.ValidateProjectID(projectID) != nil {
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
	if err != nil {
		return sessionIDArguments{}, err
	}
	return sessionIDArguments{SessionID: sessionID}, nil
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

func newSessionCommandRegistry(service *execution.Service, runs *runRegistry) (*commands.Registry, error) {
	return commands.NewRegistry(
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
