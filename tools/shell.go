package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/model"
)

// BuiltinShell is the shell tool name.
const BuiltinShell = "shell"

// DefaultTimeoutMS bounds a shell command when the model does not pass
// timeout_ms: a hung command otherwise pins the run (and its
// single-flight slot) forever. Ten minutes — generous for builds and
// test suites, finite for `yes`.
const DefaultTimeoutMS = 10 * 60 * 1000

// DefaultMaxOutputBytes bounds in-memory output buffering when the
// model does not pass max_output_bytes. The final tool result is
// truncated further (50KB) on the way to the provider; this cap is
// about not exhausting process memory before that.
const DefaultMaxOutputBytes = 1 << 20

// Executor executes one tool call. The pre-interface shape, kept as
// the argument of Named (and for direct uses that predate the agent
// loop's Tool dispatch).
type Executor interface {
	Execute(ctx context.Context, arguments map[string]any) (model.ToolResult, error)
}

// ExecutorFunc adapts a function to Executor.
type ExecutorFunc func(ctx context.Context, arguments map[string]any) (model.ToolResult, error)

func (f ExecutorFunc) Execute(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
	return f(ctx, arguments)
}

// namedTool pairs a definition with its executor — the agent.Tool
// implementation Named returns.
type namedTool struct {
	def  model.ToolDefinition
	exec Executor
}

func (t namedTool) Definition() model.ToolDefinition { return t.def }

func (t namedTool) Execute(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
	return t.exec.Execute(ctx, arguments)
}

// Named pairs a provider-facing definition with an executor into an
// agent.Tool — the migration path for pre-interface call sites that
// built the definition and the execution separately.
func Named(def model.ToolDefinition, exec Executor) agent.Tool {
	return namedTool{def: def, exec: exec}
}

// Shell returns the shell tool. Every command runs with rootDir as
// its working directory; the shell itself is never path confined.
func Shell(rootDir string) (agent.Tool, error) {
	if strings.TrimSpace(rootDir) == "" {
		return nil, fmt.Errorf("rootDir must not be blank")
	}
	root, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve rootDir: %w", err)
	}
	return namedTool{def: shellDefinition(), exec: newShellExecutor(root)}, nil
}

func shellDefinition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        BuiltinShell,
		Description: "Execute a Bash command in the workspace. Commands use Bash/POSIX syntax (not PowerShell or cmd.exe syntax). On Windows this runs Git Bash or another bash.exe found on the machine; stdout and stderr are returned together.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Bash command to run.",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Optional positive command timeout in milliseconds.",
				},
				"max_output_bytes": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Optional positive cap for combined stdout/stderr bytes retained and returned.",
				},
			},
			"required":             []any{"command"},
			"additionalProperties": false,
		},
	}
}

func newShellExecutor(rootDir string) Executor {
	return ExecutorFunc(func(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
		command, err := requiredStringArgument(arguments, "command")
		if err != nil {
			return model.ToolResult{}, err
		}
		timeoutMS, timeoutSet, err := optionalPositiveIntArgument(arguments, "timeout_ms", 0)
		if err != nil {
			return model.ToolResult{}, err
		}
		maxOutputBytes, maxOutputSet, err := optionalPositiveIntArgument(arguments, "max_output_bytes", 0)
		if err != nil {
			return model.ToolResult{}, err
		}

		// A default timeout: without one, a hung command pins the run
		// (and its single-flight slot) forever.
		if !timeoutSet {
			timeoutMS = DefaultTimeoutMS
		}
		commandCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
		defer cancel()

		cmd, err := newShellCommand(commandCtx, command)
		if err != nil {
			return model.ToolResult{
				Name:    BuiltinShell,
				Content: err.Error(),
				IsError: true,
			}, nil
		}
		cmd.Dir = rootDir
		controller := newShellCommandController(cmd)
		defer controller.Close()

		// ALWAYS bounded output: an unbounded buffer meant `yes` or a
		// large `cat` could exhaust process memory before the
		// post-hoc 50KB tool-result truncation ever ran.
		if !maxOutputSet {
			maxOutputBytes = DefaultMaxOutputBytes
		}
		// Do not use CombinedOutput here: it buffers the complete command
		// output before we can truncate it. limitedOutput always reports a
		// successful full write to os/exec so both pipes keep draining, while
		// retaining at most maxOutputBytes in process memory.
		output := newLimitedOutput(maxOutputBytes)
		cmd.Stdout = output
		cmd.Stderr = output
		err = controller.Run(cmd)
		content := output.String()
		outputTruncated := output.Truncated()
		result := model.ToolResult{
			Name:    BuiltinShell,
			Content: content,
		}
		if err != nil {
			result.IsError = true
			result.Content = shellErrorContent(commandCtx, result.Content, err)
		}
		if outputTruncated {
			result.Content = strings.Join([]string{
				"truncated=true",
				fmt.Sprintf("max_output_bytes=%d", maxOutputBytes),
				"next_step=Increase max_output_bytes and rerun the command if more output is needed.",
				"",
				result.Content,
			}, "\n")
		}
		return result, nil
	})
}

type limitedOutput struct {
	mu        sync.Mutex
	limit     int
	data      []byte
	truncated bool
}

func newLimitedOutput(limit int) *limitedOutput {
	return &limitedOutput{limit: limit}
}

func (o *limitedOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	remaining := o.limit - len(o.data)
	if remaining <= 0 {
		if len(data) > 0 {
			o.truncated = true
		}
		return len(data), nil
	}
	if len(data) > remaining {
		o.data = append(o.data, data[:remaining]...)
		o.truncated = true
		return len(data), nil
	}
	o.data = append(o.data, data...)
	return len(data), nil
}

func (o *limitedOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.data)
}

func (o *limitedOutput) Truncated() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.truncated
}

func shellErrorContent(ctx context.Context, output string, err error) string {
	if ctxErr := ctx.Err(); ctxErr != nil {
		message := "shell command canceled"
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			message = "shell command timed out"
		}
		message = fmt.Sprintf("%s: %v", message, ctxErr)
		if strings.TrimSpace(output) == "" {
			return message
		}
		return message + "\n" + output
	}
	if output == "" {
		return err.Error()
	}
	return output
}

func optionalPositiveIntArgument(arguments map[string]any, name string, defaultValue int) (int, bool, error) {
	value, ok := arguments[name]
	if !ok {
		return defaultValue, false, nil
	}
	parsed, err := integerArgumentValue(value, name)
	if err != nil {
		return 0, true, err
	}
	if parsed <= 0 {
		return 0, true, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, true, nil
}

func requiredStringArgument(arguments map[string]any, name string) (string, error) {
	value, ok := arguments[name]
	if !ok {
		return "", fmt.Errorf("%s is required", name)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s must not be blank", name)
	}
	return text, nil
}

const shellCancelWaitDelay = 2 * time.Second

// shellCommandController owns the platform-specific process-group or process-
// tree lifecycle for one shell command. Run starts and waits for the command;
// Close is idempotent and removes any remaining descendants after completion.
type shellCommandController interface {
	Run(*exec.Cmd) error
	Close()
}

func integerArgumentValue(value any, name string) (int, error) {
	var parsed int64
	switch value := value.(type) {
	case int:
		parsed = int64(value)
	case int64:
		parsed = value
	case float64:
		if math.Trunc(value) != value {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		maxInt, minInt := intBounds()
		if value > float64(maxInt) || value < float64(minInt) {
			return 0, fmt.Errorf("%s is outside integer range", name)
		}
		parsed = int64(value)
	case json.Number:
		next, err := value.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		parsed = next
	default:
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	maxInt, minInt := intBounds()
	if parsed > maxInt || parsed < minInt {
		return 0, fmt.Errorf("%s is outside integer range", name)
	}
	return int(parsed), nil
}

func intBounds() (int64, int64) {
	maxInt := int64(^uint(0) >> 1)
	return maxInt, -maxInt - 1
}
