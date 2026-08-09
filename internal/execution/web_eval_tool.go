package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/webdebug"
)

const (
	WebEvalToolName       = "web.eval"
	webEvalDefaultTimeout = webdebug.DefaultExecutionTimeoutMS
	// Keep structured JSON below Agent's existing 50 KiB tool-output limiter;
	// truncating this document would make it invalid JSON.
	webEvalMaxResultBytes = 48 * 1024
)

const webEvalToolDescription = "Evaluate arbitrary same-origin JavaScript in the live debug page (high risk; async is supported). An expression returns its completion value; a statement must explicitly return. There are no page, session, or connection selectors. This tool never retries or replays code. A synchronous infinite loop cannot be interrupted by a browser timer."

const webEvalPresentationRedacted = `{"arguments":"redacted"}`

type webEvalPresentationSummary struct {
	CodeBytes int  `json:"code_bytes"`
	TimeoutMS *int `json:"timeout_ms,omitempty"`
}

// webEvalPresentationArguments is the Go presentation boundary shared by
// live requested/started events and durable history DTOs. It accepts either
// original tool arguments or this same safe summary shape, and never returns
// code text.
func webEvalPresentationArguments(arguments string) string {
	if !utf8.ValidString(arguments) {
		return webEvalPresentationRedacted
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(arguments), &fields); err != nil || fields == nil {
		return webEvalPresentationRedacted
	}
	var codeBytes int
	if rawCode, ok := fields["code"]; ok {
		var code string
		if json.Unmarshal(rawCode, &code) != nil || code == "" || !utf8.ValidString(code) || len([]byte(code)) > protocol.DebugExecutionCodeMaxBytes {
			return webEvalPresentationRedacted
		}
		codeBytes = len([]byte(code))
	} else if rawCodeBytes, ok := fields["code_bytes"]; ok {
		parsed, ok := webEvalPresentationInteger(rawCodeBytes)
		if !ok || parsed <= 0 || parsed > protocol.DebugExecutionCodeMaxBytes {
			return webEvalPresentationRedacted
		}
		codeBytes = parsed
	} else {
		return webEvalPresentationRedacted
	}

	var timeout *int
	if rawTimeout, ok := fields["timeout_ms"]; ok {
		parsed, ok := webEvalPresentationInteger(rawTimeout)
		if !ok || parsed < protocol.DebugExecutionMinTimeoutMS || parsed > protocol.DebugExecutionMaxTimeoutMS {
			return webEvalPresentationRedacted
		}
		timeout = &parsed
	}
	encoded, err := json.Marshal(webEvalPresentationSummary{CodeBytes: codeBytes, TimeoutMS: timeout})
	if err != nil {
		return webEvalPresentationRedacted
	}
	return string(encoded)
}

func webEvalPresentationInteger(raw json.RawMessage) (int, bool) {
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0, false
	}
	return strictWebEvalInteger(number)
}

// WebEvalToolSchema is generated only for a prepared runtime that has a
// target-project session and a live Web server attachment. It is not a
// session/config enabled tool.
func WebEvalToolSchema() model.Tool {
	return model.Tool{
		Name:        WebEvalToolName,
		Description: webEvalToolDescription,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code": map[string]any{
					"type":        "string",
					"minLength":   1,
					"maxLength":   protocol.DebugExecutionCodeMaxBytes,
					"description": "JavaScript source; required, non-empty, valid UTF-8, and at most 64 KiB (65,536) UTF-8 bytes. The runtime enforces the byte limit.",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"minimum":     protocol.DebugExecutionMinTimeoutMS,
					"maximum":     protocol.DebugExecutionMaxTimeoutMS,
					"default":     webEvalDefaultTimeout,
					"description": "Optional execution timeout in milliseconds; defaults to 5000 and must be an integer from 100 through 30000.",
				},
			},
			"required":             []string{"code"},
			"additionalProperties": false,
		},
	}
}

type webEvalToolExecutor struct {
	store        *sessions.V2Store
	sessionID    string
	registration *WebEvalExecutorRegistration
}

// prepareWebEvalTool performs the first, visibility-only authority check. The
// registration token is captured so a later replacement cannot migrate this
// runtime to another owner.
func prepareWebEvalTool(session sessions.SessionV2, store *sessions.V2Store, service *Service) *webEvalToolExecutor {
	if service == nil || store == nil || service.SessionStore() != store || session.ProjectID != webdebug.TargetProjectID || session.ID == "" {
		return nil
	}
	authoritative, err := store.LoadState(session.ID)
	if err != nil || authoritative.ProjectID != webdebug.TargetProjectID {
		return nil
	}
	registration := service.CurrentWebEvalExecutor()
	if registration == nil || !registration.IsCurrent() {
		return nil
	}
	return &webEvalToolExecutor{
		store:        store,
		sessionID:    session.ID,
		registration: registration,
	}
}

func (e *webEvalToolExecutor) Execute(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
	started := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return model.ToolResult{}, err
	}
	code, timeoutMS, err := validateWebEvalArguments(arguments)
	if err != nil {
		return webEvalToolFailure(elapsedMilliseconds(started), webdebug.ErrorCodeInvalidExecution, "invalid web.eval arguments"), nil
	}

	// Runtime preparation is only a hint. A tool call always reloads the
	// caller session and checks the exact captured registration again.
	if e == nil || e.store == nil || e.registration == nil {
		return webEvalToolFailure(elapsedMilliseconds(started), webdebug.ErrorCodeNotConnected, "web debug executor is unavailable"), nil
	}
	session, loadErr := e.store.LoadState(e.sessionID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return model.ToolResult{}, ctxErr
	}
	if loadErr != nil {
		if errors.Is(loadErr, sessions.ErrNotFound) {
			return webEvalToolFailure(elapsedMilliseconds(started), webdebug.ErrorCodeSessionNotFound, "web debug session was not found"), nil
		}
		return webEvalToolFailure(elapsedMilliseconds(started), webdebug.ErrorCodeSessionUnavailable, "web debug session is unavailable"), nil
	}
	if session.ProjectID != webdebug.TargetProjectID {
		return webEvalToolFailure(elapsedMilliseconds(started), webdebug.ErrorCodeProjectMismatch, "web debug session is not eligible"), nil
	}
	if !e.registration.IsCurrent() {
		return webEvalToolFailure(elapsedMilliseconds(started), webdebug.ErrorCodeNotConnected, "web debug executor is unavailable"), nil
	}

	payload, executeErr := e.registration.Execute(ctx, code, timeoutMS)
	if ctxErr := ctx.Err(); ctxErr != nil {
		// Preserve the caller's cancellation/deadline identity. The Agent's
		// existing tool cancellation layer owns its presentation.
		return model.ToolResult{}, ctxErr
	}
	if executeErr != nil {
		return webEvalBrokerFailure(elapsedMilliseconds(started), executeErr), nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return model.ToolResult{}, ctxErr
	}
	return webEvalBrowserResult(started, payload), nil
}

func validateWebEvalArguments(arguments map[string]any) (string, int, error) {
	if arguments == nil {
		return "", 0, errors.New("arguments must be an object")
	}
	for key := range arguments {
		if key != "code" && key != "timeout_ms" {
			return "", 0, fmt.Errorf("unknown argument %q", key)
		}
	}
	rawCode, ok := arguments["code"]
	if !ok {
		return "", 0, errors.New("code is required")
	}
	code, ok := rawCode.(string)
	if !ok || code == "" || !utf8.ValidString(code) || len([]byte(code)) > protocol.DebugExecutionCodeMaxBytes {
		return "", 0, errors.New("code is invalid")
	}
	timeoutMS := webEvalDefaultTimeout
	if rawTimeout, ok := arguments["timeout_ms"]; ok {
		parsed, ok := strictWebEvalInteger(rawTimeout)
		if !ok || parsed < protocol.DebugExecutionMinTimeoutMS || parsed > protocol.DebugExecutionMaxTimeoutMS {
			return "", 0, errors.New("timeout_ms is invalid")
		}
		timeoutMS = parsed
	}
	return code, timeoutMS, nil
}

func strictWebEvalInteger(value any) (int, bool) {
	if number, ok := value.(json.Number); ok {
		return exactWebEvalNumber(string(number))
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return 0, false
	}
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		parsed := rv.Int()
		if int64(int(parsed)) != parsed {
			return 0, false
		}
		return int(parsed), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		parsed := rv.Uint()
		if uint64(int(parsed)) != parsed || int(parsed) < 0 {
			return 0, false
		}
		return int(parsed), true
	case reflect.Float32, reflect.Float64:
		value := rv.Float()
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
			return 0, false
		}
		bits := 64
		if rv.Kind() == reflect.Float32 {
			bits = 32
		}
		return exactWebEvalNumber(strconv.FormatFloat(value, 'g', -1, bits))
	}
	return 0, false
}

func exactWebEvalNumber(value string) (int, bool) {
	rational, ok := new(big.Rat).SetString(value)
	if !ok || !rational.IsInt() || !rational.Num().IsInt64() {
		return 0, false
	}
	parsed := rational.Num().Int64()
	if int64(int(parsed)) != parsed {
		return 0, false
	}
	return int(parsed), true
}

type webEvalOutputError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type webEvalOutput struct {
	Status      string                       `json:"status"`
	ElapsedMS   int64                        `json:"elapsed_ms"`
	ExecutionID string                       `json:"execution_id,omitempty"`
	PageID      string                       `json:"page_id,omitempty"`
	PageEpoch   string                       `json:"page_epoch,omitempty"`
	SessionID   string                       `json:"session_id,omitempty"`
	Value       json.RawMessage              `json:"value,omitempty"`
	Console     []protocol.DebugConsoleEntry `json:"console,omitempty"`
	Error       *webEvalOutputError          `json:"error,omitempty"`
}

func webEvalBrowserResult(started time.Time, payload protocol.DebugExecutionResultPayload) model.ToolResult {
	if err := protocol.ValidateDebugExecutionResultPayload(payload); err != nil {
		return webEvalToolFailure(elapsedMilliseconds(started), "web_debug_internal_error", "web evaluation failed")
	}
	result := webEvalOutput{
		Status:      string(payload.Status),
		ElapsedMS:   elapsedMilliseconds(started),
		ExecutionID: payload.ExecutionID,
		PageID:      payload.PageID,
		PageEpoch:   payload.PageEpoch,
		SessionID:   payload.SessionID,
		Console:     payload.Console,
	}
	if result.Status == "" {
		result.Status = string(protocol.DebugExecutionStatusFailed)
	}
	if result.Status == string(protocol.DebugExecutionStatusSucceeded) {
		result.Value = payload.Value
		if len(result.Value) == 0 {
			result.Value = json.RawMessage("null")
		}
	} else {
		result.Error = safeBrowserError(payload.Error)
	}
	content := marshalWebEvalOutput(result)
	return model.ToolResult{Content: content, IsError: result.Status != string(protocol.DebugExecutionStatusSucceeded)}
}
func safeBrowserError(err *protocol.DebugExecutionError) *webEvalOutputError {
	if err == nil || err.Code == "" {
		return &webEvalOutputError{Code: "web_debug_browser_error", Message: "browser execution failed"}
	}
	message := err.Message
	if message == "" {
		message = "browser execution failed"
	}
	return &webEvalOutputError{Code: err.Code, Message: message}
}

func webEvalToolFailure(elapsed int64, code, message string) model.ToolResult {
	return model.ToolResult{Content: marshalWebEvalOutput(webEvalOutput{
		Status:    string(protocol.DebugExecutionStatusFailed),
		ElapsedMS: elapsed,
		Error:     &webEvalOutputError{Code: code, Message: message},
	}), IsError: true}
}

func webEvalBrokerFailure(elapsed int64, err error) model.ToolResult {
	code, message := webEvalBrokerError(err)
	return webEvalToolFailure(elapsed, code, message)
}

type webEvalCodedError interface {
	WebEvalCode() string
}

func webEvalBrokerError(err error) (string, string) {
	var coded webEvalCodedError
	if errors.As(err, &coded) {
		code := coded.WebEvalCode()
		if isKnownWebEvalErrorCode(code) {
			return code, webEvalErrorMessage(code)
		}
		return "web_debug_internal_error", "web evaluation failed"
	}
	if errors.Is(err, ErrWebEvalExecutorUnavailable) {
		return webdebug.ErrorCodeNotConnected, "web debug executor is unavailable"
	}
	return "web_debug_internal_error", "web evaluation failed"
}

func isKnownWebEvalErrorCode(code string) bool {
	switch code {
	case webdebug.ErrorCodeDisabled,
		webdebug.ErrorCodeClosed,
		webdebug.ErrorCodeNotConnected,
		webdebug.ErrorCodeInvalidConnection,
		webdebug.ErrorCodeInvalidIdentity,
		webdebug.ErrorCodeNotEligible,
		webdebug.ErrorCodeSessionNotFound,
		webdebug.ErrorCodeProjectMismatch,
		webdebug.ErrorCodeSessionUnavailable,
		webdebug.ErrorCodePageNotRegistered,
		webdebug.ErrorCodeConnectionNotAllowed,
		webdebug.ErrorCodeInvalidExecution,
		webdebug.ErrorCodeExecutionBusy,
		webdebug.ErrorCodeExecutionTimeout,
		webdebug.ErrorCodeExecutionDisconnected:
		return true
	default:
		return false
	}
}

func webEvalErrorMessage(code string) string {
	switch code {
	case webdebug.ErrorCodeDisabled:
		return "web debug is disabled"
	case webdebug.ErrorCodeClosed:
		return "web debug server is closed"
	case webdebug.ErrorCodeNotConnected:
		return "no live web debug page is connected"
	case webdebug.ErrorCodeInvalidExecution:
		return "web evaluation arguments are invalid"
	case webdebug.ErrorCodeExecutionBusy:
		return "another web evaluation is already running"
	case webdebug.ErrorCodeExecutionTimeout:
		return "web evaluation timed out"
	case webdebug.ErrorCodeExecutionDisconnected:
		return "web debug page disconnected"
	case webdebug.ErrorCodeSessionNotFound:
		return "web debug session was not found"
	case webdebug.ErrorCodeProjectMismatch:
		return "web debug session is not eligible"
	case webdebug.ErrorCodeSessionUnavailable:
		return "web debug session is unavailable"
	default:
		return "web evaluation failed"
	}
}

func marshalWebEvalOutput(output webEvalOutput) string {
	content, err := json.Marshal(output)
	if err == nil && len(content) <= webEvalMaxResultBytes {
		return string(content)
	}
	// A broker result is bounded, but identity and console fields add framing.
	// Return a small valid JSON failure rather than truncating JSON or allowing
	// the Agent's limiter to receive an invalid partial document.
	fallback := webEvalOutput{
		Status:    string(protocol.DebugExecutionStatusFailed),
		ElapsedMS: output.ElapsedMS,
		Error:     &webEvalOutputError{Code: "web_debug_result_too_large", Message: "web evaluation result exceeded the output limit"},
	}
	content, _ = json.Marshal(fallback)
	return string(content)
}

func elapsedMilliseconds(started time.Time) int64 {
	return time.Since(started).Milliseconds()
}

var _ agent.ToolExecutor = (*runToolExecutor)(nil)
