package execution

import (
	"strings"
	"testing"
)

func TestPromptEventValidate(t *testing.T) {
	validContent := "hello execution"
	preservedContent := "  leading and trailing spaces  \nsecond line\n"

	tests := []struct {
		name    string
		event   PromptEvent
		wantErr bool
		errSub  string
	}{
		{
			name:    "stdin enqueue_turn valid",
			event:   PromptEvent{Source: PromptSourceStdin, Mode: PromptModeEnqueueTurn, Content: validContent},
			wantErr: false,
		},
		{
			name:    "tui append_active valid",
			event:   PromptEvent{Source: PromptSourceTUI, Mode: PromptModeAppendActive, Content: validContent},
			wantErr: false,
		},
		{
			name:    "mailbox enqueue_turn valid with task id",
			event:   PromptEvent{Source: PromptSourceMailbox, Mode: PromptModeEnqueueTurn, Content: validContent, MailboxTaskID: "task-1"},
			wantErr: false,
		},
		{
			name:    "mcp_task_update append_active valid with input id",
			event:   PromptEvent{Source: PromptSourceMCPTaskUpdate, Mode: PromptModeAppendActive, Content: validContent, InputID: "input-1"},
			wantErr: false,
		},
		{
			name:    "metadata ids optional",
			event:   PromptEvent{Source: PromptSourceStdin, Mode: PromptModeEnqueueTurn, Content: validContent},
			wantErr: false,
		},
		{
			name:    "empty source rejected",
			event:   PromptEvent{Source: "", Mode: PromptModeEnqueueTurn, Content: validContent},
			wantErr: true,
			errSub:  "source",
		},
		{
			name:    "unknown source rejected",
			event:   PromptEvent{Source: PromptSource("cli"), Mode: PromptModeEnqueueTurn, Content: validContent},
			wantErr: true,
			errSub:  "source",
		},
		{
			name:    "empty mode rejected",
			event:   PromptEvent{Source: PromptSourceStdin, Mode: "", Content: validContent},
			wantErr: true,
			errSub:  "mode",
		},
		{
			name:    "unknown mode rejected",
			event:   PromptEvent{Source: PromptSourceStdin, Mode: PromptMode("prepend"), Content: validContent},
			wantErr: true,
			errSub:  "mode",
		},
		{
			name:    "empty content rejected",
			event:   PromptEvent{Source: PromptSourceStdin, Mode: PromptModeEnqueueTurn, Content: ""},
			wantErr: true,
			errSub:  "content",
		},
		{
			name:    "whitespace-only content rejected",
			event:   PromptEvent{Source: PromptSourceStdin, Mode: PromptModeEnqueueTurn, Content: "   \n\t  "},
			wantErr: true,
			errSub:  "content",
		},
		{
			name:    "content with surrounding whitespace and newlines valid and preserved",
			event:   PromptEvent{Source: PromptSourceStdin, Mode: PromptModeEnqueueTurn, Content: preservedContent},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := tt.event.Content
			err := tt.event.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Validate() error = nil, want error containing %q", tt.errSub)
				}
				if tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
					t.Fatalf("Validate() error = %q, want containing %q", err.Error(), tt.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
			if tt.event.Content != original {
				t.Fatalf("Validate() mutated Content: got %q, want %q", tt.event.Content, original)
			}
		})
	}
}

func TestPromptEventValidatePreservesContentExactly(t *testing.T) {
	content := "\t  keep\tme\n exactly \r\n "
	event := PromptEvent{Source: PromptSourceMailbox, Mode: PromptModeAppendActive, Content: content, MailboxTaskID: "task-9", InputID: "input-9"}
	if err := event.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
	if event.Content != content {
		t.Fatalf("Content = %q, want %q", event.Content, content)
	}
	if event.MailboxTaskID != "task-9" || event.InputID != "input-9" {
		t.Fatalf("metadata ids mutated = %q/%q", event.MailboxTaskID, event.InputID)
	}
}
