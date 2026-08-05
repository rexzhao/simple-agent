package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/execution"
)

type sessionMarkReadArguments struct {
	SessionID string  `json:"session_id"`
	RunID     string  `json:"run_id"`
	ProjectID *string `json:"project_id,omitempty"`
}

func validateSessionMarkReadArguments(raw json.RawMessage) error {
	_, err := decodeSessionMarkReadArguments(raw)
	return err
}

func decodeSessionMarkReadArguments(raw json.RawMessage) (sessionMarkReadArguments, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return sessionMarkReadArguments{}, fmt.Errorf("arguments are required")
	}
	var arguments sessionMarkReadArguments
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&arguments); err != nil {
		return sessionMarkReadArguments{}, fmt.Errorf("invalid session.mark_read arguments")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return sessionMarkReadArguments{}, fmt.Errorf("invalid session.mark_read arguments")
	}
	if strings.TrimSpace(arguments.SessionID) == "" || strings.TrimSpace(arguments.RunID) == "" {
		return sessionMarkReadArguments{}, fmt.Errorf("session_id and run_id are required")
	}
	if arguments.ProjectID != nil && strings.TrimSpace(*arguments.ProjectID) == "" {
		return sessionMarkReadArguments{}, fmt.Errorf("project_id must not be empty")
	}
	arguments.SessionID = strings.TrimSpace(arguments.SessionID)
	arguments.RunID = strings.TrimSpace(arguments.RunID)
	if arguments.ProjectID != nil {
		value := strings.TrimSpace(*arguments.ProjectID)
		arguments.ProjectID = &value
	}
	return arguments, nil
}

func newSessionCommandRegistry(service *execution.Service) (*commands.Registry, error) {
	return commands.NewRegistry(commands.CommandDefinition{
		Name:                "session.mark_read",
		SchemaVersion:       1,
		CrossEpochRetrySafe: true,
		Validate:            validateSessionMarkReadArguments,
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
				return nil, err
			}
			return json.Marshal(result)
		},
	})
}
