package subagents

import (
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

const (
	ToolSubagentStart  = "subagent_start"
	ToolSubagentSend   = "subagent_send"
	ToolSubagentStatus = "subagent_status"
	ToolSubagentWait   = "subagent_wait"
	ToolSubagentCancel = "subagent_cancel"
)

func Definitions() []model.Tool {
	return []model.Tool{
		StartDefinition(),
		SendDefinition(),
		StatusDefinition(),
		WaitDefinition(),
		CancelDefinition(),
	}
}

func StartDefinition() model.Tool {
	return model.Tool{
		Name:        ToolSubagentStart,
		Description: "Start a configured subagent job asynchronously.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_id": map[string]any{
					"type":        "string",
					"description": "Configured subagent id to run.",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "Initial instructions or task for the subagent.",
				},
				"display_name": map[string]any{
					"type":        "string",
					"description": "Optional display label for this job. This does not select the subagent config.",
				},
				"job_name": map[string]any{
					"type":        "string",
					"description": "Optional short job name for status displays.",
				},
			},
			"required":             []any{"agent_id"},
			"additionalProperties": false,
		},
	}
}

func SendDefinition() model.Tool {
	return model.Tool{
		Name:        ToolSubagentSend,
		Description: "Send a follow-up message to a running subagent job.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"job_id": map[string]any{
					"type":        "string",
					"description": "Subagent job id returned by subagent_start.",
				},
				"message": map[string]any{
					"type":        "string",
					"description": "Message to deliver to the running subagent job.",
				},
			},
			"required":             []any{"job_id", "message"},
			"additionalProperties": false,
		},
	}
}

func StatusDefinition() model.Tool {
	return model.Tool{
		Name:        ToolSubagentStatus,
		Description: "Return current metadata and final output for a subagent job.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"job_id": map[string]any{
					"type":        "string",
					"description": "Subagent job id returned by subagent_start.",
				},
			},
			"required":             []any{"job_id"},
			"additionalProperties": false,
		},
	}
}

func WaitDefinition() model.Tool {
	return model.Tool{
		Name:        ToolSubagentWait,
		Description: "Wait for a subagent job to finish until timeout_ms elapses.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"job_id": map[string]any{
					"type":        "string",
					"description": "Subagent job id returned by subagent_start.",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": "Maximum time to wait in milliseconds.",
					"minimum":     0,
					"maximum":     int(MaxWaitTimeout / time.Millisecond),
				},
			},
			"required":             []any{"job_id"},
			"additionalProperties": false,
		},
	}
}

func CancelDefinition() model.Tool {
	return model.Tool{
		Name:        ToolSubagentCancel,
		Description: "Cancel a running subagent job.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"job_id": map[string]any{
					"type":        "string",
					"description": "Subagent job id returned by subagent_start.",
				},
			},
			"required":             []any{"job_id"},
			"additionalProperties": false,
		},
	}
}
