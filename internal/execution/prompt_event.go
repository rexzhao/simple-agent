package execution

import (
	"errors"
	"fmt"
	"strings"
)

// Stable exported errors for prompt event acceptance.
var (
	ErrPromptEventInvalid     = errors.New("prompt event is invalid")
	ErrPromptModeNotSupported = errors.New("prompt mode is not supported")
)

// PromptSource identifies the origin of a user prompt submitted to a session.
type PromptSource string

const (
	PromptSourceStdin         PromptSource = "stdin"
	PromptSourceMailbox       PromptSource = "mailbox"
	PromptSourceMCPTaskUpdate PromptSource = "mcp_task_update"
)

// PromptMode determines how a prompt is applied to a session run.
type PromptMode string

const (
	PromptModeEnqueueTurn  PromptMode = "enqueue_turn"
	PromptModeAppendActive PromptMode = "append_active"
)

// PromptEvent is a typed payload describing a user prompt to be applied to a
// session run. It is a pure data structure: it carries no queueing, controller,
// or persistence behavior. Metadata identifiers (MailboxTaskID, InputID) are
// optional and only carry meaning for specific sources.
type PromptEvent struct {
	Source        PromptSource
	Mode          PromptMode
	Content       string
	MailboxTaskID string
	InputID       string
}

// Validate reports whether the event has a known source, a known mode, and
// non-whitespace content. Content is compared verbatim: Validate never trims or
// rewrites Content. MailboxTaskID and InputID are optional and not validated.
func (e PromptEvent) Validate() error {
	switch e.Source {
	case PromptSourceStdin, PromptSourceMailbox, PromptSourceMCPTaskUpdate:
	default:
		return fmt.Errorf("prompt event source %q is unknown or empty", e.Source)
	}
	switch e.Mode {
	case PromptModeEnqueueTurn, PromptModeAppendActive:
	default:
		return fmt.Errorf("prompt event mode %q is unknown or empty", e.Mode)
	}
	if strings.TrimSpace(e.Content) == "" {
		return fmt.Errorf("prompt event content must be a non-empty string")
	}
	return nil
}
