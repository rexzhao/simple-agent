package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/execution"
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

type runCancelArguments struct {
	RunID string
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

type runCancelResult struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
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

func validateRunCancelArguments(raw json.RawMessage) error {
	_, err := decodeRunCancelArguments(raw)
	return err
}

func sessionCommandError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		return commands.NewDomainError("not_found", "session not found", err)
	case errors.Is(err, execution.ErrSessionBusy):
		return commands.NewDomainError("session_busy", "session is busy", err)
	case errors.Is(err, context.Canceled):
		return commands.NewDomainError("cancelled", "command was cancelled", err)
	case errors.Is(err, execution.ErrSessionArchived):
		return commands.NewDomainError("session_archived", "session is archived", err)
	default:
		return commands.NewDomainError("command_failed", "command execution failed", err)
	}
}

func newSessionCommandRegistry(service *execution.Service, runs *runRegistry) (*commands.Registry, error) {
	return commands.NewRegistry(
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
