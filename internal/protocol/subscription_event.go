package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SubscriptionEventType is the closed set of transient session-content event
// types sent over WebSocket. The execution source is shared, while this is the
// typed subscription projection.
type SubscriptionEventType string

const (
	SubscriptionEventAssistantMessageStarted   SubscriptionEventType = "assistant.message.started"
	SubscriptionEventAssistantMessageUpdated   SubscriptionEventType = "assistant.message.updated"
	SubscriptionEventAssistantMessageCompleted SubscriptionEventType = "assistant.message.completed"
	SubscriptionEventAssistantMessageFailed    SubscriptionEventType = "assistant.message.failed"
	SubscriptionEventToolRequested             SubscriptionEventType = "tool.requested"
	SubscriptionEventToolRunning               SubscriptionEventType = "tool.running"
	SubscriptionEventToolProgress              SubscriptionEventType = "tool.progress"
	SubscriptionEventToolFinished              SubscriptionEventType = "tool.finished"
	SubscriptionEventPromptQueue               SubscriptionEventType = "run.prompt_queue"
	SubscriptionEventPromptAppended            SubscriptionEventType = "run.prompt_appended"
	SubscriptionEventRunStarted                SubscriptionEventType = "run.started"
	SubscriptionEventTurnFailed                SubscriptionEventType = "turn.failed"
	SubscriptionEventRunSettled                SubscriptionEventType = "run.settled"
)

const MaxTransientFailureMessageRunes = 600

type PromptQueueEntry struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Steer   bool   `json:"steer"`
}

type AssistantToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

type TransientItemWatermark struct {
	TurnID         string    `json:"turn_id"`
	AgentIteration int       `json:"agent_iteration"`
	ItemID         string    `json:"item_id"`
	RunCursor      RunCursor `json:"run_cursor"`
}

// DurableSettlementWatermark is the only relation a D3 adapter may use to
// remove transient overlay. Verified=false is an explicit conservative
// recovery instruction; clients must not infer coverage from a revision alone.
type DurableSettlementWatermark struct {
	ResourceRevision ResourceRevision         `json:"resource_revision"`
	RunCursor        RunCursor                `json:"run_cursor"`
	Verified         bool                     `json:"verified"`
	CoveredItems     []TransientItemWatermark `json:"covered_items"`
}

// TransientSubscriptionEvent is the strict, flat wire union carried by
// subscription_event. The zero value is invalid. MarshalJSON and
// UnmarshalJSON both validate the discriminated shape, so an event cannot
// silently acquire an unrecognized field or event type at either boundary.
type TransientSubscriptionEvent struct {
	Type      SubscriptionEventType `json:"type"`
	SessionID string                `json:"session_id"`
	RunID     string                `json:"run_id"`
	RunCursor RunCursor             `json:"run_cursor"`

	TurnID           string              `json:"turn_id,omitempty"`
	AgentIteration   int                 `json:"agent_iteration,omitempty"`
	ItemID           string              `json:"item_id,omitempty"`
	MessageRevision  string              `json:"message_revision,omitempty"`
	AssistantContent string              `json:"content,omitempty"`
	Reasoning        string              `json:"reasoning,omitempty"`
	ToolCalls        []AssistantToolCall `json:"tool_calls,omitempty"`
	SnapshotOmitted  bool                `json:"snapshot_omitted,omitempty"`

	ToolCallID     string `json:"tool_call_id,omitempty"`
	Name           string `json:"name,omitempty"`
	Arguments      string `json:"arguments,omitempty"`
	ArgumentsDelta string `json:"arguments_delta,omitempty"`
	ToolContent    string `json:"-"`
	IsError        *bool  `json:"is_error,omitempty"`

	Prompts         []PromptQueueEntry          `json:"-"`
	AppendedPrompts []string                    `json:"-"`
	Status          string                      `json:"status,omitempty"`
	Code            string                      `json:"code,omitempty"`
	Message         string                      `json:"message,omitempty"`
	Settlement      *DurableSettlementWatermark `json:"durable_settlement_watermark,omitempty"`
}

func (e TransientSubscriptionEvent) MarshalJSON() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	fields := map[string]any{
		"type": string(e.Type), "session_id": e.SessionID, "run_id": e.RunID,
		"run_cursor": string(e.RunCursor),
	}
	if e.TurnID != "" {
		fields["turn_id"] = e.TurnID
	}
	if e.AgentIteration > 0 {
		fields["agent_iteration"] = e.AgentIteration
	}
	if e.ItemID != "" {
		fields["item_id"] = e.ItemID
	}
	switch e.Type {
	case SubscriptionEventAssistantMessageStarted:
		fields["message_revision"] = e.MessageRevision
	case SubscriptionEventAssistantMessageUpdated, SubscriptionEventAssistantMessageCompleted, SubscriptionEventAssistantMessageFailed:
		fields["message_revision"] = e.MessageRevision
		if e.SnapshotOmitted {
			fields["snapshot_omitted"] = true
		} else {
			fields["content"] = e.AssistantContent
		}
		if e.Reasoning != "" {
			fields["reasoning"] = e.Reasoning
		}
		if e.ToolCalls != nil {
			fields["tool_calls"] = e.ToolCalls
		}
	case SubscriptionEventToolRequested, SubscriptionEventToolRunning:
		fields["tool_call_id"], fields["name"] = e.ToolCallID, e.Name
		if e.Arguments != "" {
			fields["arguments"] = e.Arguments
		}
	case SubscriptionEventToolProgress:
		fields["tool_call_id"], fields["name"], fields["arguments_delta"] = e.ToolCallID, e.Name, e.ArgumentsDelta
	case SubscriptionEventToolFinished:
		fields["tool_call_id"], fields["name"], fields["is_error"] = e.ToolCallID, e.Name, *e.IsError
		if e.ToolContent != "" {
			fields["content"] = e.ToolContent
		}
	case SubscriptionEventPromptQueue:
		fields["prompts"] = e.Prompts
	case SubscriptionEventPromptAppended:
		fields["prompts"] = e.AppendedPrompts
	case SubscriptionEventRunStarted:
		fields["status"] = e.Status
	case SubscriptionEventTurnFailed:
		fields["code"], fields["message"] = e.Code, e.Message
	case SubscriptionEventRunSettled:
		fields["status"], fields["durable_settlement_watermark"] = e.Status, e.Settlement
	}
	return json.Marshal(fields)
}

func (e *TransientSubscriptionEvent) UnmarshalJSON(data []byte) error {
	decoded, err := decodeSubscriptionEvent(data)
	if err != nil {
		return err
	}
	*e = decoded
	return nil
}

func (e TransientSubscriptionEvent) Validate() error {
	if !knownSubscriptionEventType(e.Type) {
		return fmt.Errorf("unknown subscription event type %q", e.Type)
	}
	if err := requiredEventID("session_id", e.SessionID); err != nil {
		return err
	}
	if err := requiredEventID("run_id", e.RunID); err != nil {
		return err
	}
	if err := ValidateRunCursor(e.RunCursor); err != nil {
		return fmt.Errorf("run_cursor: %w", err)
	}
	if e.TurnID != "" {
		if err := requiredEventID("turn_id", e.TurnID); err != nil {
			return err
		}
	}
	if e.ItemID != "" {
		if err := requiredEventID("item_id", e.ItemID); err != nil {
			return err
		}
	}
	if e.ToolCallID != "" {
		if err := requiredEventID("tool_call_id", e.ToolCallID); err != nil {
			return err
		}
	}
	if e.Name != "" {
		if err := requiredEventID("name", e.Name); err != nil {
			return err
		}
	}
	if e.AgentIteration < 0 {
		return fmt.Errorf("agent_iteration must be non-negative")
	}
	reject := func(field string, present bool) error {
		if present {
			return fmt.Errorf("%s is not valid for %q", field, e.Type)
		}
		return nil
	}
	if err := reject("item_id", e.ItemID != ""); err != nil && !isAssistantMessageEvent(e.Type) {
		return err
	}
	if err := reject("message_revision", e.MessageRevision != ""); err != nil && !isAssistantMessageEvent(e.Type) {
		return err
	}
	if err := reject("assistant content", e.AssistantContent != ""); err != nil && !isAssistantMessageSnapshotEvent(e.Type) {
		return err
	}
	if err := reject("reasoning", e.Reasoning != ""); err != nil && !isAssistantMessageSnapshotEvent(e.Type) {
		return err
	}
	if err := reject("tool_calls", e.ToolCalls != nil); err != nil && !isAssistantMessageSnapshotEvent(e.Type) {
		return err
	}
	if err := reject("snapshot_omitted", e.SnapshotOmitted); err != nil && e.Type != SubscriptionEventAssistantMessageCompleted && e.Type != SubscriptionEventAssistantMessageFailed {
		return err
	}
	if err := reject("tool_call_id", e.ToolCallID != ""); err != nil && e.Type != SubscriptionEventToolRequested && e.Type != SubscriptionEventToolRunning && e.Type != SubscriptionEventToolProgress && e.Type != SubscriptionEventToolFinished {
		return err
	}
	if err := reject("name", e.Name != ""); err != nil && e.Type != SubscriptionEventToolRequested && e.Type != SubscriptionEventToolRunning && e.Type != SubscriptionEventToolProgress && e.Type != SubscriptionEventToolFinished {
		return err
	}
	if err := reject("arguments", e.Arguments != ""); err != nil && e.Type != SubscriptionEventToolRequested && e.Type != SubscriptionEventToolRunning {
		return err
	}
	if err := reject("arguments_delta", e.ArgumentsDelta != ""); err != nil && e.Type != SubscriptionEventToolProgress {
		return err
	}
	if err := reject("content", e.ToolContent != ""); err != nil && e.Type != SubscriptionEventToolFinished {
		return err
	}
	if err := reject("is_error", e.IsError != nil); err != nil && e.Type != SubscriptionEventToolFinished {
		return err
	}
	if err := reject("prompts", e.Prompts != nil); err != nil && e.Type != SubscriptionEventPromptQueue {
		return err
	}
	if err := reject("prompts", e.AppendedPrompts != nil); err != nil && e.Type != SubscriptionEventPromptAppended {
		return err
	}
	if err := reject("status", e.Status != ""); err != nil && e.Type != SubscriptionEventRunStarted && e.Type != SubscriptionEventRunSettled {
		return err
	}
	if err := reject("code", e.Code != ""); err != nil && e.Type != SubscriptionEventTurnFailed {
		return err
	}
	if err := reject("message", e.Message != ""); err != nil && e.Type != SubscriptionEventTurnFailed {
		return err
	}
	if err := reject("durable_settlement_watermark", e.Settlement != nil); err != nil && e.Type != SubscriptionEventRunSettled {
		return err
	}
	if e.Type != SubscriptionEventRunStarted && e.Type != SubscriptionEventTurnFailed && e.Type != SubscriptionEventRunSettled && e.TurnID == "" && e.Type != SubscriptionEventPromptQueue && e.Type != SubscriptionEventPromptAppended {
		return fmt.Errorf("turn_id is required for %q", e.Type)
	}
	switch e.Type {
	case SubscriptionEventAssistantMessageStarted, SubscriptionEventAssistantMessageUpdated, SubscriptionEventAssistantMessageCompleted, SubscriptionEventAssistantMessageFailed:
		if e.ItemID == "" {
			return fmt.Errorf("item_id is required for %q", e.Type)
		}
		if e.AgentIteration <= 0 {
			return fmt.Errorf("agent_iteration is required for %q", e.Type)
		}
		if !isCanonicalUint64(e.MessageRevision) {
			return fmt.Errorf("message_revision must be an unsigned decimal integer")
		}
		if e.Type == SubscriptionEventAssistantMessageStarted && e.MessageRevision != "0" {
			return fmt.Errorf("assistant.message.started revision must be zero")
		}
		if e.Type != SubscriptionEventAssistantMessageStarted && e.MessageRevision == "0" {
			return fmt.Errorf("%s revision must be positive", e.Type)
		}
		if e.SnapshotOmitted && (e.AssistantContent != "" || e.Reasoning != "" || e.ToolCalls != nil) {
			return fmt.Errorf("omitted assistant snapshot cannot carry message fields")
		}
		for i, call := range e.ToolCalls {
			if err := requiredEventID("tool_calls["+strconv.Itoa(i)+"].id", call.ID); err != nil {
				return err
			}
			if err := requiredEventID("tool_calls["+strconv.Itoa(i)+"].name", call.Name); err != nil {
				return err
			}
			if call.Arguments != "" && !utf8.ValidString(call.Arguments) {
				return fmt.Errorf("tool_calls[%d].arguments is not valid UTF-8", i)
			}
		}
	case SubscriptionEventToolRequested, SubscriptionEventToolRunning:
		if e.ToolCallID == "" || e.Name == "" {
			return fmt.Errorf("tool_call_id and name are required for %q", e.Type)
		}
		if e.AgentIteration <= 0 {
			return fmt.Errorf("agent_iteration is required for %q", e.Type)
		}
		if e.Arguments != "" {
			if err := requiredEventText("arguments", e.Arguments); err != nil {
				return err
			}
		}
	case SubscriptionEventToolProgress:
		if e.ToolCallID == "" || e.Name == "" {
			return fmt.Errorf("tool progress identity and delta are required")
		}
		if e.AgentIteration <= 0 {
			return fmt.Errorf("agent_iteration is required for %q", e.Type)
		}
		if err := requiredEventText("arguments_delta", e.ArgumentsDelta); err != nil {
			return err
		}
	case SubscriptionEventToolFinished:
		if e.ToolCallID == "" || e.Name == "" || e.IsError == nil {
			return fmt.Errorf("tool finished identity and is_error are required")
		}
		if e.AgentIteration <= 0 {
			return fmt.Errorf("agent_iteration is required for %q", e.Type)
		}
		if e.ToolContent != "" {
			if err := requiredEventText("content", e.ToolContent); err != nil {
				return err
			}
		}
	case SubscriptionEventPromptQueue:
		if e.Prompts == nil {
			return fmt.Errorf("prompts is required for %q", e.Type)
		}
		for i, prompt := range e.Prompts {
			if err := requiredEventID("prompts["+strconv.Itoa(i)+"].id", prompt.ID); err != nil {
				return err
			}
			if err := requiredEventText("prompts["+strconv.Itoa(i)+"].content", prompt.Content); err != nil {
				return err
			}
		}
	case SubscriptionEventPromptAppended:
		if e.AppendedPrompts == nil {
			return fmt.Errorf("prompts is required for %q", e.Type)
		}
		for i, prompt := range e.AppendedPrompts {
			if err := requiredEventText("prompts["+strconv.Itoa(i)+"]", prompt); err != nil {
				return err
			}
		}
	case SubscriptionEventRunStarted:
		if e.Status != "running" {
			return fmt.Errorf("run.started status must be running")
		}
	case SubscriptionEventTurnFailed:
		if err := requiredEventID("turn_id", e.TurnID); err != nil {
			return err
		}
		if err := requiredEventID("code", e.Code); err != nil {
			return err
		}
		if err := requiredEventText("message", e.Message); err != nil {
			return err
		}
		if utf8.RuneCountInString(e.Message) > MaxTransientFailureMessageRunes {
			return fmt.Errorf("message exceeds %d runes", MaxTransientFailureMessageRunes)
		}
	case SubscriptionEventRunSettled:
		if e.Status != "committed" && e.Status != "failed" && e.Status != "interrupted" && e.Status != "cancelled" {
			return fmt.Errorf("invalid run.settled status %q", e.Status)
		}
		if e.Settlement == nil {
			return fmt.Errorf("durable_settlement_watermark is required for run.settled")
		}
		if err := ValidateDurableSettlementWatermark(*e.Settlement); err != nil {
			return err
		}
		if coveredCursor, err := CompareRunCursor(e.Settlement.RunCursor, e.RunCursor); err != nil {
			return fmt.Errorf("settlement run_cursor: %w", err)
		} else if coveredCursor > 0 {
			return fmt.Errorf("settlement run_cursor is after run.settled cursor")
		}
	}
	return nil
}

// ValidateDurableSettlementWatermark validates the explicit proof boundary
// between a transient run tail and durable session state. A false proof may
// not carry a tempting partial item list, and no covered tail may be newer
// than the durable watermark cursor. These rules keep adapters from having
// to infer a relationship from a resource revision alone.
func ValidateDurableSettlementWatermark(w DurableSettlementWatermark) error {
	if err := ValidateResourceRevision(w.ResourceRevision); err != nil {
		return fmt.Errorf("settlement resource_revision: %w", err)
	}
	if err := ValidateRunCursor(w.RunCursor); err != nil {
		return fmt.Errorf("settlement run_cursor: %w", err)
	}
	if w.CoveredItems == nil {
		return fmt.Errorf("settlement covered_items is required")
	}
	if !w.Verified && len(w.CoveredItems) != 0 {
		return fmt.Errorf("unverified settlement cannot contain covered_items")
	}
	for i, item := range w.CoveredItems {
		if item.TurnID == "" || item.ItemID == "" || item.AgentIteration <= 0 {
			return fmt.Errorf("settlement covered_items[%d] identity is invalid", i)
		}
		if err := ValidateRunCursor(item.RunCursor); err != nil {
			return fmt.Errorf("settlement covered_items[%d].run_cursor: %w", i, err)
		}
		if coveredCursor, err := CompareRunCursor(item.RunCursor, w.RunCursor); err != nil {
			return fmt.Errorf("settlement covered_items[%d].run_cursor: %w", i, err)
		} else if coveredCursor > 0 {
			return fmt.Errorf("settlement covered_items[%d].run_cursor is after settlement run_cursor", i)
		}
	}
	return nil
}

func ValidateSubscriptionEvent(data json.RawMessage) error {
	_, err := decodeSubscriptionEvent(data)
	return err
}

func DecodeSubscriptionEvent(data json.RawMessage) (TransientSubscriptionEvent, error) {
	return decodeSubscriptionEvent(data)
}

func decodeSubscriptionEvent(data []byte) (TransientSubscriptionEvent, error) {
	if !isJSONObject(data) {
		return TransientSubscriptionEvent{}, fmt.Errorf("subscription event must be a JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	var event TransientSubscriptionEvent
	getString := func(key string, dst *string, required bool) error {
		if raw, ok := fields[key]; ok {
			if err := json.Unmarshal(raw, dst); err != nil || strings.TrimSpace(*dst) == "" {
				return fmt.Errorf("%s must be a non-empty string", key)
			}
			return nil
		}
		if required {
			return fmt.Errorf("%s is required", key)
		}
		return nil
	}
	var typeString string
	if err := getString("type", &typeString, true); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	event.Type = SubscriptionEventType(typeString)
	if err := getString("session_id", &event.SessionID, true); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	if err := getString("run_id", &event.RunID, true); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	var cursor string
	if err := getString("run_cursor", &cursor, true); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	event.RunCursor = RunCursor(cursor)
	optionalString := func(key string, dst *string) error { return getString(key, dst, false) }
	if err := optionalString("turn_id", &event.TurnID); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	if raw, ok := fields["agent_iteration"]; ok {
		if isJSONNull(raw) {
			return TransientSubscriptionEvent{}, fmt.Errorf("agent_iteration must be an integer")
		}
		if err := json.Unmarshal(raw, &event.AgentIteration); err != nil {
			return TransientSubscriptionEvent{}, fmt.Errorf("agent_iteration must be an integer")
		}
	}
	if err := optionalString("item_id", &event.ItemID); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	if err := optionalString("message_revision", &event.MessageRevision); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	decodeOptionalText := func(key string, dst *string) error {
		raw, ok := fields[key]
		if !ok {
			return nil
		}
		if isJSONNull(raw) || json.Unmarshal(raw, dst) != nil || !utf8.ValidString(*dst) {
			return fmt.Errorf("%s must be a UTF-8 string", key)
		}
		return nil
	}
	if err := decodeOptionalText("reasoning", &event.Reasoning); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	if raw, ok := fields["tool_calls"]; ok {
		toolCalls, err := decodeAssistantToolCalls(raw)
		if err != nil {
			return TransientSubscriptionEvent{}, err
		}
		event.ToolCalls = toolCalls
	}
	if err := optionalString("tool_call_id", &event.ToolCallID); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	if err := optionalString("name", &event.Name); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	if err := optionalString("arguments", &event.Arguments); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	if err := optionalString("arguments_delta", &event.ArgumentsDelta); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	if isAssistantMessageSnapshotEvent(event.Type) {
		if raw, ok := fields["snapshot_omitted"]; ok {
			if isJSONNull(raw) || json.Unmarshal(raw, &event.SnapshotOmitted) != nil || !event.SnapshotOmitted {
				return TransientSubscriptionEvent{}, fmt.Errorf("snapshot_omitted must be true")
			}
		}
		if _, present := fields["content"]; !present && !event.SnapshotOmitted {
			return TransientSubscriptionEvent{}, fmt.Errorf("content is required for %q", event.Type)
		}
		if err := decodeOptionalText("content", &event.AssistantContent); err != nil {
			return TransientSubscriptionEvent{}, err
		}
	} else if err := decodeOptionalText("content", &event.ToolContent); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	if raw, ok := fields["is_error"]; ok {
		if isJSONNull(raw) {
			return TransientSubscriptionEvent{}, fmt.Errorf("is_error must be boolean")
		}
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return TransientSubscriptionEvent{}, fmt.Errorf("is_error must be boolean")
		}
		event.IsError = &value
	}
	if err := optionalString("status", &event.Status); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	if err := optionalString("code", &event.Code); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	if err := optionalString("message", &event.Message); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	if raw, ok := fields["prompts"]; ok {
		if event.Type == SubscriptionEventPromptAppended {
			if err := json.Unmarshal(raw, &event.AppendedPrompts); err != nil {
				return TransientSubscriptionEvent{}, fmt.Errorf("prompts must be an array of strings")
			}
		} else if prompts, err := decodePromptQueue(raw); err != nil {
			return TransientSubscriptionEvent{}, err
		} else {
			event.Prompts = prompts
		}
	}
	if raw, ok := fields["durable_settlement_watermark"]; ok {
		value, err := decodeSettlementWatermark(raw)
		if err != nil {
			return TransientSubscriptionEvent{}, err
		}
		event.Settlement = &value
	}
	if err := rejectUnknownEventFields(event.Type, fields); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	if err := event.Validate(); err != nil {
		return TransientSubscriptionEvent{}, err
	}
	return event, nil
}

func decodePromptQueue(raw []byte) ([]PromptQueueEntry, error) {
	if isJSONNull(raw) {
		return nil, fmt.Errorf("prompts must be an array of prompt objects")
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("prompts must be an array of prompt objects")
	}
	result := make([]PromptQueueEntry, len(values))
	for index, value := range values {
		var fields map[string]json.RawMessage
		if !isJSONObject(value) || json.Unmarshal(value, &fields) != nil {
			return nil, fmt.Errorf("prompts[%d] must be an object", index)
		}
		for key := range fields {
			if key != "id" && key != "content" && key != "steer" {
				return nil, fmt.Errorf("unknown prompt field %q", key)
			}
		}
		if err := json.Unmarshal(fields["id"], &result[index].ID); err != nil || result[index].ID == "" {
			return nil, fmt.Errorf("prompts[%d].id is required", index)
		}
		if err := json.Unmarshal(fields["content"], &result[index].Content); err != nil || result[index].Content == "" {
			return nil, fmt.Errorf("prompts[%d].content is required", index)
		}
		rawSteer, ok := fields["steer"]
		if !ok {
			return nil, fmt.Errorf("prompts[%d].steer is required", index)
		}
		if isJSONNull(rawSteer) || json.Unmarshal(rawSteer, &result[index].Steer) != nil {
			return nil, fmt.Errorf("prompts[%d].steer must be boolean", index)
		}
	}
	return result, nil
}

func decodeAssistantToolCalls(raw []byte) ([]AssistantToolCall, error) {
	if isJSONNull(raw) {
		return nil, fmt.Errorf("tool_calls must be an array")
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("tool_calls must be an array")
	}
	result := make([]AssistantToolCall, len(values))
	for index, value := range values {
		var fields map[string]json.RawMessage
		if !isJSONObject(value) || json.Unmarshal(value, &fields) != nil {
			return nil, fmt.Errorf("tool_calls[%d] must be an object", index)
		}
		for key := range fields {
			if key != "id" && key != "name" && key != "arguments" {
				return nil, fmt.Errorf("unknown tool_calls[%d] field %q", index, key)
			}
		}
		if err := json.Unmarshal(fields["id"], &result[index].ID); err != nil || strings.TrimSpace(result[index].ID) == "" {
			return nil, fmt.Errorf("tool_calls[%d].id is required", index)
		}
		if err := json.Unmarshal(fields["name"], &result[index].Name); err != nil || strings.TrimSpace(result[index].Name) == "" {
			return nil, fmt.Errorf("tool_calls[%d].name is required", index)
		}
		if rawArguments, ok := fields["arguments"]; ok {
			if isJSONNull(rawArguments) || json.Unmarshal(rawArguments, &result[index].Arguments) != nil || !utf8.ValidString(result[index].Arguments) {
				return nil, fmt.Errorf("tool_calls[%d].arguments must be a UTF-8 string", index)
			}
		}
	}
	return result, nil
}

func decodeSettlementWatermark(raw []byte) (DurableSettlementWatermark, error) {
	var fields map[string]json.RawMessage
	if !isJSONObject(raw) || json.Unmarshal(raw, &fields) != nil {
		return DurableSettlementWatermark{}, fmt.Errorf("durable_settlement_watermark must be an object")
	}
	for key := range fields {
		if key != "resource_revision" && key != "run_cursor" && key != "verified" && key != "covered_items" {
			return DurableSettlementWatermark{}, fmt.Errorf("unknown settlement field %q", key)
		}
	}
	var result DurableSettlementWatermark
	if err := json.Unmarshal(fields["resource_revision"], &result.ResourceRevision); err != nil || result.ResourceRevision == "" {
		return DurableSettlementWatermark{}, fmt.Errorf("settlement resource_revision is required")
	}
	if err := json.Unmarshal(fields["run_cursor"], &result.RunCursor); err != nil || result.RunCursor == "" {
		return DurableSettlementWatermark{}, fmt.Errorf("settlement run_cursor is required")
	}
	if isJSONNull(fields["verified"]) || json.Unmarshal(fields["verified"], &result.Verified) != nil {
		return DurableSettlementWatermark{}, fmt.Errorf("settlement verified must be boolean")
	}
	var items []json.RawMessage
	if isJSONNull(fields["covered_items"]) || json.Unmarshal(fields["covered_items"], &items) != nil {
		return DurableSettlementWatermark{}, fmt.Errorf("settlement covered_items must be an array")
	}
	result.CoveredItems = make([]TransientItemWatermark, len(items))
	for index, item := range items {
		var itemFields map[string]json.RawMessage
		if !isJSONObject(item) || json.Unmarshal(item, &itemFields) != nil {
			return DurableSettlementWatermark{}, fmt.Errorf("covered_items[%d] must be an object", index)
		}
		for key := range itemFields {
			if key != "turn_id" && key != "agent_iteration" && key != "item_id" && key != "run_cursor" {
				return DurableSettlementWatermark{}, fmt.Errorf("unknown covered item field %q", key)
			}
		}
		if err := json.Unmarshal(itemFields["turn_id"], &result.CoveredItems[index].TurnID); err != nil || result.CoveredItems[index].TurnID == "" {
			return DurableSettlementWatermark{}, fmt.Errorf("covered_items[%d].turn_id is required", index)
		}
		if err := json.Unmarshal(itemFields["agent_iteration"], &result.CoveredItems[index].AgentIteration); err != nil {
			return DurableSettlementWatermark{}, fmt.Errorf("covered_items[%d].agent_iteration is required", index)
		}
		if err := json.Unmarshal(itemFields["item_id"], &result.CoveredItems[index].ItemID); err != nil || result.CoveredItems[index].ItemID == "" {
			return DurableSettlementWatermark{}, fmt.Errorf("covered_items[%d].item_id is required", index)
		}
		if err := json.Unmarshal(itemFields["run_cursor"], &result.CoveredItems[index].RunCursor); err != nil || result.CoveredItems[index].RunCursor == "" {
			return DurableSettlementWatermark{}, fmt.Errorf("covered_items[%d].run_cursor is required", index)
		}
	}
	return result, nil
}

func isJSONNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func rejectUnknownEventFields(eventType SubscriptionEventType, fields map[string]json.RawMessage) error {
	allowed := map[string]struct{}{"type": {}, "session_id": {}, "run_id": {}, "run_cursor": {}, "turn_id": {}, "agent_iteration": {}, "item_id": {}}
	switch eventType {
	case SubscriptionEventAssistantMessageStarted:
		allowed["message_revision"] = struct{}{}
	case SubscriptionEventAssistantMessageUpdated, SubscriptionEventAssistantMessageCompleted, SubscriptionEventAssistantMessageFailed:
		allowed["message_revision"] = struct{}{}
		allowed["content"] = struct{}{}
		allowed["reasoning"] = struct{}{}
		allowed["tool_calls"] = struct{}{}
		allowed["snapshot_omitted"] = struct{}{}
	case SubscriptionEventToolRequested, SubscriptionEventToolRunning:
		allowed["tool_call_id"] = struct{}{}
		allowed["name"] = struct{}{}
		allowed["arguments"] = struct{}{}
	case SubscriptionEventToolProgress:
		allowed["tool_call_id"] = struct{}{}
		allowed["name"] = struct{}{}
		allowed["arguments_delta"] = struct{}{}
	case SubscriptionEventToolFinished:
		allowed["tool_call_id"] = struct{}{}
		allowed["name"] = struct{}{}
		allowed["is_error"] = struct{}{}
		allowed["content"] = struct{}{}
	case SubscriptionEventPromptQueue, SubscriptionEventPromptAppended:
		allowed["prompts"] = struct{}{}
	case SubscriptionEventRunStarted:
		allowed["status"] = struct{}{}
	case SubscriptionEventTurnFailed:
		allowed["code"] = struct{}{}
		allowed["message"] = struct{}{}
	case SubscriptionEventRunSettled:
		allowed["status"] = struct{}{}
		allowed["durable_settlement_watermark"] = struct{}{}
	default:
		return fmt.Errorf("unknown subscription event type %q", eventType)
	}
	for key := range fields {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unknown field %q for subscription event %q", key, eventType)
		}
	}
	return nil
}

func knownSubscriptionEventType(eventType SubscriptionEventType) bool {
	switch eventType {
	case SubscriptionEventAssistantMessageStarted, SubscriptionEventAssistantMessageUpdated, SubscriptionEventAssistantMessageCompleted, SubscriptionEventAssistantMessageFailed, SubscriptionEventToolRequested, SubscriptionEventToolRunning, SubscriptionEventToolProgress, SubscriptionEventToolFinished, SubscriptionEventPromptQueue, SubscriptionEventPromptAppended, SubscriptionEventRunStarted, SubscriptionEventTurnFailed, SubscriptionEventRunSettled:
		return true
	}
	return false
}

func isAssistantMessageEvent(eventType SubscriptionEventType) bool {
	return eventType == SubscriptionEventAssistantMessageStarted || isAssistantMessageSnapshotEvent(eventType)
}

func isAssistantMessageSnapshotEvent(eventType SubscriptionEventType) bool {
	return eventType == SubscriptionEventAssistantMessageUpdated || eventType == SubscriptionEventAssistantMessageCompleted || eventType == SubscriptionEventAssistantMessageFailed
}

func isCanonicalUint64(value string) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}

func requiredEventID(field, value string) error {
	return requiredString(field, value)
}

func requiredEventText(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", field)
	}
	return nil
}
