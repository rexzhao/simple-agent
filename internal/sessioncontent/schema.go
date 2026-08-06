package sessioncontent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

const SchemaVersion = 1

const (
	OpMetadataReplace          = "metadata.replace"
	OpItemUpsert               = "item.upsert"
	OpItemRemove               = "item.remove"
	OpHistoryWindowReplace     = "history.window.replace"
	OpActiveRunReplace         = "active_run.replace"
	OpActiveRunClear           = "active_run.clear"
	OpCompactionReplace        = "compaction.replace"
	OpHistoryDescriptorReplace = "history.window.descriptor.replace"
)

const (
	SessionStatusIdle        = "idle"
	SessionStatusRunning     = "running"
	SessionStatusFailed      = "failed"
	SessionStatusInterrupted = "interrupted"
)

var supportedCreatedBy = map[string]struct{}{
	sessions.SessionCreatedByUser: {}, sessions.SessionCreatedByAgent: {},
}

var supportedItemKinds = map[string]struct{}{
	sessions.ItemKindMessage: {}, sessions.ItemKindCompaction: {}, sessions.ItemKindRuntimeContext: {},
}

var supportedVisibilities = map[string]struct{}{
	sessions.ItemVisibilityVisible: {}, sessions.ItemVisibilityHidden: {}, sessions.ItemVisibilityDebug: {},
}

var supportedAudiences = map[string]struct{}{
	sessions.ItemAudienceUser: {}, sessions.ItemAudienceModel: {}, sessions.ItemAudienceInternal: {},
}

var supportedItemStatuses = map[string]struct{}{
	sessions.ItemStatusPending: {}, sessions.ItemStatusCompleted: {}, sessions.ItemStatusError: {}, sessions.ItemStatusInterrupted: {},
}

var supportedRoles = map[string]struct{}{
	string(model.MessageRoleSystem): {}, string(model.MessageRoleDeveloper): {}, string(model.MessageRoleUser): {},
	string(model.MessageRoleAssistant): {}, string(model.MessageRoleTool): {}, string(model.MessageRoleProvider): {},
}

// Snapshot is the durable, recoverable session-content baseline. The
// resource_revision is intentionally carried by the sync envelope; it is the
// session store's opaque LastSeq, never the WebSocket stream sequence.
type Snapshot struct {
	SchemaVersion int                  `json:"schema_version"`
	Session       SessionMetadata      `json:"session"`
	History       HistoryWindow        `json:"history"`
	ActiveRun     *ActiveRunDescriptor `json:"active_run"`
	Compaction    CompactionState      `json:"compaction"`
}

type SessionMetadata struct {
	ID                string    `json:"id"`
	Version           int       `json:"version"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	DisplayName       string    `json:"display_name,omitempty"`
	CreatedBy         string    `json:"created_by,omitempty"`
	ParentSessionID   string    `json:"parent_session_id,omitempty"`
	RootSessionID     string    `json:"root_session_id,omitempty"`
	SpawnDepth        int       `json:"spawn_depth,omitempty"`
	Archived          bool      `json:"archived"`
	ArchivedAt        time.Time `json:"archived_at,omitempty"`
	LastUsedAt        time.Time `json:"last_used_at"`
	CurrentRunID      string    `json:"current_run_id,omitempty"`
	RunningRunID      string    `json:"running_run_id,omitempty"`
	RunningTurnID     string    `json:"running_turn_id,omitempty"`
	InterruptedRunID  string    `json:"interrupted_run_id,omitempty"`
	InterruptedTurnID string    `json:"interrupted_turn_id,omitempty"`
	InterruptedAt     time.Time `json:"interrupted_at,omitempty"`
	LatestRunID       string    `json:"latest_run_id,omitempty"`
	LastRunID         string    `json:"last_run_id,omitempty"`
	LastRunStatus     string    `json:"last_run_status,omitempty"`
	HasUnreadResult   bool      `json:"has_unread_result"`
	// Provider/model fields are optional for old sessions created before the
	// execution metadata was populated. When present they are opaque
	// non-blank identifiers; the store remains authoritative for their exact
	// vocabulary.
	Provider        string                 `json:"provider,omitempty"`
	ModelProfile    string                 `json:"model_profile,omitempty"`
	ModelID         string                 `json:"model_id,omitempty"`
	Pricing         *config.ModelPricing   `json:"pricing,omitempty"`
	ReasoningLevel  string                 `json:"reasoning_level,omitempty"`
	ModelParameters map[string]any         `json:"model_parameters,omitempty"`
	Status          string                 `json:"status"`
	ProjectID       string                 `json:"project_id,omitempty"`
	CWD             string                 `json:"cwd,omitempty"`
	CreatedCWD      string                 `json:"created_cwd,omitempty"`
	ConfigPath      string                 `json:"config_path,omitempty"`
	ConfigDir       string                 `json:"config_dir,omitempty"`
	EnabledTools    []string               `json:"enabled_tools,omitempty"`
	EnabledMCP      []string               `json:"enabled_mcp,omitempty"`
	EnabledSkills   []string               `json:"enabled_skills,omitempty"`
	ShowReasoning   bool                   `json:"show_reasoning"`
	FullAccess      bool                   `json:"full_access"`
	Debug           sessions.DebugSettings `json:"debug"`
	Context         contextwindow.Metadata `json:"context"`
	SaveToolResults bool                   `json:"save_tool_results"`
	ActiveHistory   []string               `json:"active_history,omitempty"`
}

type HistoryWindow struct {
	Items      []Item                  `json:"items"`
	Descriptor HistoryWindowDescriptor `json:"descriptor"`
}

type HistoryWindowDescriptor struct {
	Limit         int    `json:"limit"`
	BeforeItemSeq string `json:"before_item_seq,omitempty"`
	AfterItemSeq  string `json:"after_item_seq,omitempty"`
	AlignTurn     bool   `json:"align_turn"`
	VisibleOnly   bool   `json:"visible_only"`
	OldestItemSeq string `json:"oldest_item_seq,omitempty"`
	NewestItemSeq string `json:"newest_item_seq,omitempty"`
	HasMoreBefore bool   `json:"has_more_before"`
	HasMoreAfter  bool   `json:"has_more_after"`
}

type ItemKey struct {
	// TurnID is intentionally required on the wire but may be the empty
	// string: legacy session-level/runtime items in the durable store have no
	// turn. AgentIteration remains zero-based, and ItemID is always required.
	TurnID         string `json:"turn_id"`
	AgentIteration int    `json:"agent_iteration"`
	ItemID         string `json:"item_id"`
}

type Item struct {
	Key        ItemKey   `json:"key"`
	Seq        int64     `json:"seq"`
	CreatedAt  time.Time `json:"created_at"`
	Kind       string    `json:"kind"`
	Visibility string    `json:"visibility"`
	Audience   string    `json:"audience"`
	// Status is optional for legacy items whose durable record predates the
	// item-status field; when present it is one of the supported item states.
	Status  string       `json:"status,omitempty"`
	Message *ItemMessage `json:"message,omitempty"`
}

type ItemMessage struct {
	Role       string            `json:"role"`
	Content    *TextContent      `json:"content,omitempty"`
	Reasoning  *TextContent      `json:"reasoning,omitempty"`
	Images     []ImageAttachment `json:"images,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall        `json:"tool_calls,omitempty"`
	IsError    bool              `json:"is_error,omitempty"`
}

type ItemContent struct {
	Inline  string                   `json:"inline,omitempty"`
	Preview string                   `json:"preview,omitempty"`
	Blob    *protocol.BlobDescriptor `json:"blob,omitempty"`
	// ContentType is optional only for legacy inline/preview records. D1
	// projections always populate it; when Blob is present it must equal the
	// descriptor content_type.
	ContentType string `json:"content_type,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
}

// TextContent is either complete inline text or a complete HTTP Blob. A
// truncated value is only legal when no Blob exists and explicitly means the
// public projection is bounded rather than silently clipped. D1 never emits
// Truncated=true for durable content; it fails/resyncs if a complete value
// cannot be placed inline or in the Blob data plane.
type TextContent = ItemContent

type ImageAttachment struct {
	Hash      string `json:"hash"`
	MediaType string `json:"media_type"`
	SizeBytes int64  `json:"size_bytes"`
}

type ToolCall struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Arguments *TextContent `json:"arguments,omitempty"`
}

type ActiveRunDescriptor struct {
	RunID               string                               `json:"run_id"`
	SessionID           string                               `json:"session_id"`
	TurnID              string                               `json:"turn_id,omitempty"`
	StartedAt           time.Time                            `json:"started_at"`
	Status              string                               `json:"status"`
	Recoverable         bool                                 `json:"recoverable"`
	RunEpoch            string                               `json:"run_epoch,omitempty"`
	RunCursor           protocol.RunCursor                   `json:"run_cursor,omitempty"`
	ReplayAvailable     bool                                 `json:"replay_available"`
	ReplayFromCursor    protocol.RunCursor                   `json:"replay_from_cursor,omitempty"`
	ReplayToCursor      protocol.RunCursor                   `json:"replay_to_cursor,omitempty"`
	RecoveryRequired    bool                                 `json:"recovery_required"`
	SettlementWatermark *protocol.DurableSettlementWatermark `json:"durable_settlement_watermark,omitempty"`
}

type CompactionState struct {
	Checkpoints []CompactionCheckpoint `json:"checkpoints"`
	Truncated   bool                   `json:"truncated"`
}

type CompactionCheckpoint struct {
	ID                    string    `json:"id"`
	CreatedAt             time.Time `json:"created_at"`
	Reason                string    `json:"reason"`
	Phase                 string    `json:"phase"`
	Trigger               string    `json:"trigger"`
	SummaryItemID         string    `json:"summary_item_id"`
	FromItemID            string    `json:"from_item_id,omitempty"`
	ToItemID              string    `json:"to_item_id,omitempty"`
	PreviousActiveHistory []string  `json:"previous_active_history,omitempty"`
	ReplacementHistory    []string  `json:"replacement_history"`
	SummaryProvider       string    `json:"summary_provider,omitempty"`
	SummaryModel          string    `json:"summary_model,omitempty"`
}

// UnmarshalJSON enforces required wire keys before Go's zero values can make
// a missing boolean, array, or nullable field indistinguishable from an
// intentionally supplied value. Optional fields are deliberately not listed
// here; this is the Go counterpart of the optional markers in web protocol
// types.ts.
func (s *Snapshot) UnmarshalJSON(data []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	if err := requireJSONFields(object, "snapshot", "schema_version", "session", "history", "compaction"); err != nil {
		return err
	}
	if _, ok := object["active_run"]; !ok {
		return fmt.Errorf("snapshot.active_run is required")
	}
	if err := validateSnapshotRequiredJSON(object); err != nil {
		return err
	}
	type plain Snapshot
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*s = Snapshot(decoded)
	return nil
}

func requireJSONFields(object map[string]json.RawMessage, path string, fields ...string) error {
	for _, field := range fields {
		raw, ok := object[field]
		if !ok || string(raw) == "null" {
			return fmt.Errorf("%s.%s is required", path, field)
		}
	}
	return nil
}

func requiredJSONObject(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		if err != nil {
			return nil, fmt.Errorf("%s must be an object: %w", path, err)
		}
		return nil, fmt.Errorf("%s must be an object", path)
	}
	return object, nil
}

func requiredJSONArray(raw json.RawMessage, path string) ([]json.RawMessage, error) {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		if err != nil {
			return nil, fmt.Errorf("%s must be an array: %w", path, err)
		}
		return nil, fmt.Errorf("%s must be an array", path)
	}
	return values, nil
}

func validateSnapshotRequiredJSON(root map[string]json.RawMessage) error {
	metadata, err := requiredJSONObject(root["session"], "snapshot.session")
	if err != nil {
		return err
	}
	if err := requireJSONFields(metadata, "snapshot.session", "id", "version", "created_at", "updated_at", "archived", "last_used_at", "has_unread_result", "status", "show_reasoning", "full_access", "debug", "context", "save_tool_results"); err != nil {
		return err
	}
	if _, err := requiredJSONObject(metadata["debug"], "snapshot.session.debug"); err != nil {
		return err
	}
	if _, err := requiredJSONObject(metadata["context"], "snapshot.session.context"); err != nil {
		return err
	}
	history, err := requiredJSONObject(root["history"], "snapshot.history")
	if err != nil {
		return err
	}
	if err := requireJSONFields(history, "snapshot.history", "items", "descriptor"); err != nil {
		return err
	}
	items, err := requiredJSONArray(history["items"], "snapshot.history.items")
	if err != nil {
		return err
	}
	descriptor, err := requiredJSONObject(history["descriptor"], "snapshot.history.descriptor")
	if err != nil {
		return err
	}
	if err := requireJSONFields(descriptor, "snapshot.history.descriptor", "limit", "align_turn", "visible_only", "has_more_before", "has_more_after"); err != nil {
		return err
	}
	for index, rawItem := range items {
		item, itemErr := requiredJSONObject(rawItem, fmt.Sprintf("snapshot.history.items[%d]", index))
		if itemErr != nil {
			return itemErr
		}
		if err := requireJSONFields(item, fmt.Sprintf("snapshot.history.items[%d]", index), "key", "seq", "created_at", "kind", "visibility", "audience"); err != nil {
			return err
		}
		key, keyErr := requiredJSONObject(item["key"], fmt.Sprintf("snapshot.history.items[%d].key", index))
		if keyErr != nil {
			return keyErr
		}
		if err := requireJSONFields(key, fmt.Sprintf("snapshot.history.items[%d].key", index), "turn_id", "agent_iteration", "item_id"); err != nil {
			return err
		}
		if rawMessage, ok := item["message"]; ok && string(rawMessage) != "null" {
			message, messageErr := requiredJSONObject(rawMessage, fmt.Sprintf("snapshot.history.items[%d].message", index))
			if messageErr != nil {
				return messageErr
			}
			if err := requireJSONFields(message, fmt.Sprintf("snapshot.history.items[%d].message", index), "role"); err != nil {
				return err
			}
			if rawCalls, ok := message["tool_calls"]; ok {
				calls, callsErr := requiredJSONArray(rawCalls, fmt.Sprintf("snapshot.history.items[%d].message.tool_calls", index))
				if callsErr != nil {
					return callsErr
				}
				for callIndex, rawCall := range calls {
					call, callErr := requiredJSONObject(rawCall, fmt.Sprintf("snapshot.history.items[%d].message.tool_calls[%d]", index, callIndex))
					if callErr != nil {
						return callErr
					}
					if err := requireJSONFields(call, fmt.Sprintf("snapshot.history.items[%d].message.tool_calls[%d]", index, callIndex), "id", "name"); err != nil {
						return err
					}
				}
			}
		}
	}
	if active := root["active_run"]; string(active) != "null" {
		run, runErr := requiredJSONObject(active, "snapshot.active_run")
		if runErr != nil {
			return runErr
		}
		if err := requireJSONFields(run, "snapshot.active_run", "run_id", "session_id", "started_at", "status", "recoverable"); err != nil {
			return err
		}
	}
	compaction, err := requiredJSONObject(root["compaction"], "snapshot.compaction")
	if err != nil {
		return err
	}
	if err := requireJSONFields(compaction, "snapshot.compaction", "checkpoints", "truncated"); err != nil {
		return err
	}
	checkpoints, err := requiredJSONArray(compaction["checkpoints"], "snapshot.compaction.checkpoints")
	if err != nil {
		return err
	}
	for index, rawCheckpoint := range checkpoints {
		checkpoint, checkpointErr := requiredJSONObject(rawCheckpoint, fmt.Sprintf("snapshot.compaction.checkpoints[%d]", index))
		if checkpointErr != nil {
			return checkpointErr
		}
		if err := requireJSONFields(checkpoint, fmt.Sprintf("snapshot.compaction.checkpoints[%d]", index), "id", "created_at", "reason", "phase", "trigger", "summary_item_id", "replacement_history"); err != nil {
			return err
		}
		if _, err := requiredJSONArray(checkpoint["replacement_history"], fmt.Sprintf("snapshot.compaction.checkpoints[%d].replacement_history", index)); err != nil {
			return err
		}
	}
	return nil
}

func (c CompactionState) Validate() error {
	if c.Checkpoints == nil {
		return fmt.Errorf("checkpoints must be an array")
	}
	seen := make(map[string]struct{}, len(c.Checkpoints))
	for index, checkpoint := range c.Checkpoints {
		if err := checkpoint.Validate(index); err != nil {
			return err
		}
		if _, ok := seen[checkpoint.ID]; ok {
			return fmt.Errorf("duplicate checkpoint id %q", checkpoint.ID)
		}
		seen[checkpoint.ID] = struct{}{}
	}
	if c.Truncated && len(c.Checkpoints) == 0 {
		return fmt.Errorf("truncated compaction state must retain a checkpoint")
	}
	return nil
}

func (c CompactionCheckpoint) Validate(index int) error {
	prefix := fmt.Sprintf("checkpoints[%d]", index)
	for field, value := range map[string]string{
		"id": c.ID, "summary_item_id": c.SummaryItemID,
	} {
		if err := validateID(value, prefix+"."+field); err != nil {
			return err
		}
	}
	for field, value := range map[string]string{"from_item_id": c.FromItemID, "to_item_id": c.ToItemID} {
		if value != "" {
			if err := validateID(value, prefix+"."+field); err != nil {
				return err
			}
		}
	}
	if err := validateTime(c.CreatedAt, prefix+".created_at"); err != nil {
		return err
	}
	if c.Reason == "" || c.Phase == "" || c.Trigger == "" {
		return fmt.Errorf("%s reason, phase and trigger are required", prefix)
	}
	for field, value := range map[string]string{
		"reason": c.Reason, "phase": c.Phase, "trigger": c.Trigger,
		"summary_provider": c.SummaryProvider, "summary_model": c.SummaryModel,
	} {
		if err := validateUTF8(value, prefix+"."+field); err != nil {
			return err
		}
	}
	if c.ReplacementHistory == nil {
		return fmt.Errorf("%s replacement_history must be an array", prefix)
	}
	for name, ids := range map[string][]string{
		"previous_active_history": c.PreviousActiveHistory,
		"replacement_history":     c.ReplacementHistory,
	} {
		for itemIndex, itemID := range ids {
			if err := validateID(itemID, fmt.Sprintf("%s.%s[%d]", prefix, name, itemIndex)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s Snapshot) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported session content schema version %d", s.SchemaVersion)
	}
	if err := s.Session.validate(); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if err := s.History.Validate(); err != nil {
		return fmt.Errorf("history: %w", err)
	}
	if err := s.Compaction.Validate(); err != nil {
		return fmt.Errorf("compaction: %w", err)
	}
	if s.ActiveRun != nil {
		if err := s.ActiveRun.Validate(s.Session); err != nil {
			return fmt.Errorf("active_run: %w", err)
		}
	}
	if s.Session.Status == SessionStatusRunning && s.ActiveRun == nil && s.Session.RunningTurnID == "" {
		return fmt.Errorf("running metadata requires active_run unless it is a legacy turn-only state")
	}
	if s.Session.Status != SessionStatusRunning && s.ActiveRun != nil {
		return fmt.Errorf("active_run requires running session metadata")
	}
	return nil
}

func (m SessionMetadata) validate() error {
	if err := validateSessionID(m.ID); err != nil {
		return err
	}
	for field, value := range map[string]string{
		"display_name": m.DisplayName, "provider": m.Provider, "model_profile": m.ModelProfile,
		"model_id": m.ModelID, "reasoning_level": m.ReasoningLevel, "cwd": m.CWD,
		"created_cwd": m.CreatedCWD, "config_path": m.ConfigPath, "config_dir": m.ConfigDir,
	} {
		if err := validateUTF8(value, field); err != nil {
			return err
		}
	}
	if m.Version != sessions.VersionV2 {
		return fmt.Errorf("version must be %d", sessions.VersionV2)
	}
	for field, value := range map[string]string{
		"parent_session_id":   m.ParentSessionID,
		"root_session_id":     m.RootSessionID,
		"current_run_id":      m.CurrentRunID,
		"running_run_id":      m.RunningRunID,
		"running_turn_id":     m.RunningTurnID,
		"interrupted_run_id":  m.InterruptedRunID,
		"interrupted_turn_id": m.InterruptedTurnID,
		"latest_run_id":       m.LatestRunID,
		"last_run_id":         m.LastRunID,
		"project_id":          m.ProjectID,
	} {
		if value == "" {
			continue
		}
		allowSession := strings.HasSuffix(field, "session_id") || field == "root_session_id"
		if allowSession {
			if err := validateSessionID(value); err != nil {
				return fmt.Errorf("%s: %w", field, err)
			}
		} else if err := validateID(value, field); err != nil {
			return err
		}
	}
	if m.CreatedBy != "" {
		if _, ok := supportedCreatedBy[m.CreatedBy]; !ok {
			return fmt.Errorf("unsupported created_by %q", m.CreatedBy)
		}
	}
	if _, ok := map[string]struct{}{SessionStatusIdle: {}, SessionStatusRunning: {}, SessionStatusFailed: {}, SessionStatusInterrupted: {}}[m.Status]; !ok {
		return fmt.Errorf("unsupported session status %q", m.Status)
	}
	for field, value := range map[string]string{"last_run_status": m.LastRunStatus} {
		if value == "" {
			continue
		}
		if !supportedRunStatus(value) {
			return fmt.Errorf("unsupported %s %q", field, value)
		}
	}
	for field, value := range map[string]time.Time{
		"created_at": m.CreatedAt, "updated_at": m.UpdatedAt, "last_used_at": m.LastUsedAt,
	} {
		if err := validateTime(value, field); err != nil {
			return err
		}
	}
	if m.Archived {
		if err := validateTime(m.ArchivedAt, "archived_at"); err != nil {
			return err
		}
	} else if !m.ArchivedAt.IsZero() {
		return fmt.Errorf("archived_at must be zero when archived is false")
	}
	if !m.InterruptedAt.IsZero() {
		if err := validateTime(m.InterruptedAt, "interrupted_at"); err != nil {
			return err
		}
	}
	if m.RunningRunID != "" && m.CurrentRunID != m.RunningRunID {
		return fmt.Errorf("current_run_id must match running_run_id")
	}
	if m.InterruptedRunID == "" && (m.InterruptedTurnID != "" || !m.InterruptedAt.IsZero()) {
		return fmt.Errorf("interrupted turn/time requires interrupted_run_id")
	}
	if m.InterruptedTurnID != "" && m.InterruptedRunID == "" {
		return fmt.Errorf("interrupted_turn_id requires interrupted_run_id")
	}
	for index, itemID := range m.ActiveHistory {
		if err := validateID(itemID, fmt.Sprintf("active_history[%d]", index)); err != nil {
			return err
		}
	}
	return nil
}

func (h HistoryWindow) Validate() error {
	if h.Descriptor.Limit <= 0 || h.Descriptor.Limit > 1000 {
		return fmt.Errorf("descriptor limit is outside 1..1000")
	}
	if h.Descriptor.BeforeItemSeq != "" && h.Descriptor.AfterItemSeq != "" {
		return fmt.Errorf("before_item_seq and after_item_seq cannot both be set")
	}
	if h.Descriptor.AlignTurn {
		return fmt.Errorf("D1 history windows must not use align_turn")
	}
	for field, value := range map[string]string{
		"before_item_seq": h.Descriptor.BeforeItemSeq,
		"after_item_seq":  h.Descriptor.AfterItemSeq,
		"oldest_item_seq": h.Descriptor.OldestItemSeq,
		"newest_item_seq": h.Descriptor.NewestItemSeq,
	} {
		if value == "" {
			continue
		}
		if value != "0" && strings.HasPrefix(value, "0") {
			return fmt.Errorf("%s is not a canonical decimal cursor", field)
		}
		if parsed, err := strconv.ParseUint(value, 10, 63); err != nil || parsed < 0 {
			return fmt.Errorf("%s is not a non-negative decimal cursor", field)
		}
	}
	if h.Descriptor.BeforeItemSeq != "" && len(h.Items) > 0 {
		before, _ := strconv.ParseInt(h.Descriptor.BeforeItemSeq, 10, 64)
		if h.Items[len(h.Items)-1].Seq >= before {
			return fmt.Errorf("history items must be strictly before before_item_seq")
		}
	}
	if h.Descriptor.AfterItemSeq != "" && len(h.Items) > 0 {
		after, _ := strconv.ParseInt(h.Descriptor.AfterItemSeq, 10, 64)
		if h.Items[0].Seq <= after {
			return fmt.Errorf("history items must be strictly after after_item_seq")
		}
	}
	seen := make(map[ItemKey]struct{}, len(h.Items))
	for i, item := range h.Items {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("item %d: %w", i, err)
		}
		if _, ok := seen[item.Key]; ok {
			return fmt.Errorf("duplicate item identity %s", item.Key)
		}
		seen[item.Key] = struct{}{}
		if i > 0 && h.Items[i-1].Seq >= item.Seq {
			return fmt.Errorf("items are not in strictly increasing durable order")
		}
	}
	if len(h.Items) == 0 {
		if h.Descriptor.OldestItemSeq != "" || h.Descriptor.NewestItemSeq != "" {
			return fmt.Errorf("empty history has non-empty sequence bounds")
		}
	} else {
		if h.Descriptor.OldestItemSeq != strconv.FormatInt(h.Items[0].Seq, 10) || h.Descriptor.NewestItemSeq != strconv.FormatInt(h.Items[len(h.Items)-1].Seq, 10) {
			return fmt.Errorf("history sequence bounds do not match items")
		}
	}
	if len(h.Items) > h.Descriptor.Limit {
		return fmt.Errorf("history item count exceeds descriptor limit")
	}
	if h.Descriptor.BeforeItemSeq == "" && h.Descriptor.AfterItemSeq == "" && h.Descriptor.HasMoreAfter {
		return fmt.Errorf("latest history window cannot have newer items")
	}
	if len(h.Items) == 0 && (h.Descriptor.HasMoreBefore || h.Descriptor.HasMoreAfter) && h.Descriptor.BeforeItemSeq == "" && h.Descriptor.AfterItemSeq == "" {
		return fmt.Errorf("empty latest history window cannot advertise more items")
	}
	return nil
}

func (i Item) Validate() error {
	if err := validateID(i.Key.ItemID, "item_id"); err != nil {
		return err
	}
	if i.Key.TurnID != "" {
		if err := validateID(i.Key.TurnID, "turn_id"); err != nil {
			return err
		}
	}
	if i.Key.AgentIteration < 0 || i.Seq <= 0 {
		return fmt.Errorf("agent_iteration and seq must be non-negative/positive")
	}
	if _, ok := supportedItemKinds[i.Kind]; !ok {
		return fmt.Errorf("unsupported item kind %q", i.Kind)
	}
	if _, ok := supportedVisibilities[i.Visibility]; !ok {
		return fmt.Errorf("unsupported item visibility %q", i.Visibility)
	}
	if _, ok := supportedAudiences[i.Audience]; !ok {
		return fmt.Errorf("unsupported item audience %q", i.Audience)
	}
	if i.Status != "" {
		if _, ok := supportedItemStatuses[i.Status]; !ok {
			return fmt.Errorf("unsupported item status %q", i.Status)
		}
	}
	if err := validateTime(i.CreatedAt, "created_at"); err != nil {
		return err
	}
	if i.Message != nil {
		if err := i.Message.Validate(); err != nil {
			return fmt.Errorf("message: %w", err)
		}
	}
	return nil
}

func (m ItemMessage) Validate() error {
	if _, ok := supportedRoles[m.Role]; !ok {
		return fmt.Errorf("unsupported role %q", m.Role)
	}
	if m.ToolCallID != "" {
		if err := validateID(m.ToolCallID, "tool_call_id"); err != nil {
			return err
		}
	}
	if m.Content != nil {
		if err := m.Content.Validate(); err != nil {
			return fmt.Errorf("content: %w", err)
		}
	}
	if m.Reasoning != nil {
		if err := m.Reasoning.Validate(); err != nil {
			return fmt.Errorf("reasoning: %w", err)
		}
	}
	for index, image := range m.Images {
		if err := validateID(image.Hash, fmt.Sprintf("images[%d].hash", index)); err != nil {
			return err
		}
		if _, supported := model.NormalizeImageMediaType(image.MediaType); !supported {
			return fmt.Errorf("images[%d] has unsupported media type %q", index, image.MediaType)
		}
		if image.SizeBytes <= 0 {
			return fmt.Errorf("images[%d].size_bytes must be positive", index)
		}
	}
	for index, call := range m.ToolCalls {
		if err := call.Validate(index); err != nil {
			return err
		}
	}
	return nil
}

func (c ItemContent) Validate() error {
	if c.ContentType != "" && c.ContentType != strings.TrimSpace(c.ContentType) {
		return fmt.Errorf("content_type is not canonical")
	}
	if c.ContentType != "" {
		switch c.ContentType {
		case "text/plain", "text/plain; charset=utf-8", "application/json":
		default:
			return fmt.Errorf("unsupported content_type %q", c.ContentType)
		}
	}
	if c.Blob != nil {
		if err := protocol.ValidateBlobDescriptor(*c.Blob); err != nil {
			return err
		}
		if c.Truncated {
			return fmt.Errorf("blob-backed text cannot be truncated")
		}
		if c.ContentType != "" && c.ContentType != c.Blob.ContentType {
			return fmt.Errorf("content_type does not match blob descriptor")
		}
	}
	if c.Inline != "" && c.Blob != nil {
		return fmt.Errorf("text cannot contain both inline and blob")
	}
	if c.Inline != "" && c.Preview != "" {
		return fmt.Errorf("inline text cannot also have a preview")
	}
	if !utf8.ValidString(c.Inline) || !utf8.ValidString(c.Preview) {
		return fmt.Errorf("text is not valid UTF-8")
	}
	if c.ContentType == "application/json" && c.Inline != "" && !json.Valid([]byte(c.Inline)) {
		return fmt.Errorf("application/json content is not valid JSON")
	}
	if c.Inline == "" && c.Preview == "" && c.Blob == nil && !c.Truncated {
		return fmt.Errorf("text content is empty")
	}
	return nil
}

func (c ToolCall) Validate(index int) error {
	if err := validateID(c.ID, fmt.Sprintf("tool_calls[%d].id", index)); err != nil {
		return err
	}
	if err := validateID(c.Name, fmt.Sprintf("tool_calls[%d].name", index)); err != nil {
		return err
	}
	if c.Arguments != nil {
		if err := c.Arguments.Validate(); err != nil {
			return fmt.Errorf("tool_calls[%d].arguments: %w", index, err)
		}
	}
	return nil
}

func (r ActiveRunDescriptor) Validate(metadata SessionMetadata) error {
	if err := validateID(r.RunID, "run_id"); err != nil {
		return err
	}
	if r.SessionID != metadata.ID {
		return fmt.Errorf("session_id does not match metadata")
	}
	if r.TurnID != "" {
		if err := validateID(r.TurnID, "turn_id"); err != nil {
			return err
		}
	}
	if r.Status != sessions.RunStatusRunning {
		return fmt.Errorf("status must be %q", sessions.RunStatusRunning)
	}
	if !r.Recoverable {
		return fmt.Errorf("active run must be recoverable")
	}
	if r.RunEpoch != "" {
		if err := validateID(r.RunEpoch, "run_epoch"); err != nil {
			return err
		}
	}
	if r.RunCursor != "" {
		if err := protocol.ValidateRunCursor(r.RunCursor); err != nil {
			return fmt.Errorf("run_cursor: %w", err)
		}
	}
	if r.ReplayFromCursor != "" {
		if err := protocol.ValidateRunCursor(r.ReplayFromCursor); err != nil {
			return fmt.Errorf("replay_from_cursor: %w", err)
		}
	}
	if r.ReplayToCursor != "" {
		if err := protocol.ValidateRunCursor(r.ReplayToCursor); err != nil {
			return fmt.Errorf("replay_to_cursor: %w", err)
		}
	}
	if r.ReplayAvailable && (r.RunEpoch == "" || r.RunCursor == "" || r.ReplayFromCursor == "" || r.ReplayToCursor == "") {
		return fmt.Errorf("replay availability requires run epoch and cursor range")
	}
	if r.SettlementWatermark != nil {
		if err := validateSettlementWatermark(*r.SettlementWatermark); err != nil {
			return err
		}
	}
	if err := validateTime(r.StartedAt, "started_at"); err != nil {
		return err
	}
	if metadata.RunningRunID != r.RunID {
		return fmt.Errorf("run_id does not match metadata running_run_id")
	}
	if metadata.RunningTurnID != r.TurnID {
		return fmt.Errorf("turn_id does not match metadata running_turn_id")
	}
	return nil
}

func validateSettlementWatermark(w protocol.DurableSettlementWatermark) error {
	if err := protocol.ValidateDurableSettlementWatermark(w); err != nil {
		return err
	}
	for index, item := range w.CoveredItems {
		if err := validateID(item.TurnID, fmt.Sprintf("settlement covered_items[%d].turn_id", index)); err != nil {
			return err
		}
		if item.AgentIteration <= 0 {
			return fmt.Errorf("settlement covered_items[%d].agent_iteration must be positive", index)
		}
		if err := validateID(item.ItemID, fmt.Sprintf("settlement covered_items[%d].item_id", index)); err != nil {
			return err
		}
	}
	return nil
}

func (k ItemKey) String() string {
	return fmt.Sprintf("(%q,%d,%q)", k.TurnID, k.AgentIteration, k.ItemID)
}

func validateID(value, field string) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", field)
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s is not canonical", field)
	}
	if len(value) > protocol.MaxWireIdentifierBytes {
		return fmt.Errorf("%s exceeds the maximum wire identifier length", field)
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return fmt.Errorf("%s contains whitespace/control characters", field)
		}
	}
	return nil
}

func validateUTF8(value, field string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", field)
	}
	return nil
}

func validateSessionID(value string) error {
	if err := validateID(value, "session_id"); err != nil {
		return err
	}
	if value == "." || value == ".." {
		return fmt.Errorf("session_id is not a canonical path segment")
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("session_id contains unsupported character")
	}
	if strings.EqualFold(strings.TrimRight(value, ". "), "blobs") {
		return fmt.Errorf("session_id is reserved")
	}
	return nil
}

func validateTime(value time.Time, field string) error {
	if value.IsZero() {
		return fmt.Errorf("%s is required and must be non-zero", field)
	}
	encoded, err := value.MarshalJSON()
	if err != nil || len(encoded) < 2 {
		return fmt.Errorf("%s is not RFC3339", field)
	}
	var parsed time.Time
	if err := parsed.UnmarshalJSON(encoded); err != nil {
		return fmt.Errorf("%s is not RFC3339: %w", field, err)
	}
	return nil
}

// MarshalJSON makes the Go wire optionality agree with the TypeScript
// protocol for optional time fields. time.Time is a struct, so encoding/json's
// omitempty does not omit its zero value by itself.
func (m SessionMetadata) MarshalJSON() ([]byte, error) {
	type plain SessionMetadata
	raw, err := json.Marshal(plain(m))
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	if m.ArchivedAt.IsZero() {
		delete(object, "archived_at")
	}
	if m.InterruptedAt.IsZero() {
		delete(object, "interrupted_at")
	}
	return json.Marshal(object)
}

func supportedRunStatus(value string) bool {
	switch value {
	case sessions.RunStatusRunning, sessions.RunStatusCommitted, sessions.RunStatusFailed, sessions.RunStatusInterrupted, sessions.RunStatusCancelled:
		return true
	default:
		return false
	}
}

func Operation(op string, payload any) (protocol.ChangeOperation, error) {
	if strings.TrimSpace(op) == "" {
		return protocol.ChangeOperation{}, fmt.Errorf("operation is required")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return protocol.ChangeOperation{}, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return protocol.ChangeOperation{}, fmt.Errorf("operation payload is not an object: %w", err)
	}
	withOp := make(map[string]json.RawMessage, len(object)+1)
	withOp["op"] = json.RawMessage(strconv.Quote(op))
	for key, value := range object {
		withOp[key] = value
	}
	encoded, err := json.Marshal(withOp)
	if err != nil {
		return protocol.ChangeOperation{}, err
	}
	return protocol.ChangeOperation{Op: op, Raw: encoded}, nil
}

func EqualSnapshot(a, b Snapshot) bool { return reflect.DeepEqual(a, b) }

func sessionMetadataFromState(state sessions.SessionV2) SessionMetadata {
	return SessionMetadata{
		ID: state.ID, Version: state.Version, CreatedAt: state.CreatedAt, UpdatedAt: state.UpdatedAt,
		DisplayName: state.DisplayName, CreatedBy: state.CreatedBy, ParentSessionID: state.ParentSessionID,
		RootSessionID: state.RootSessionID, SpawnDepth: state.SpawnDepth, Archived: state.Archived,
		ArchivedAt: state.ArchivedAt, LastUsedAt: state.LastUsedAt, CurrentRunID: state.CurrentRunID,
		RunningRunID: state.RunningRunID, RunningTurnID: state.RunningTurnID, InterruptedRunID: state.InterruptedRunID,
		InterruptedTurnID: state.InterruptedTurnID, InterruptedAt: state.InterruptedAt, LatestRunID: state.LatestRunID,
		LastRunID: state.LastRunID, LastRunStatus: state.LastRunStatus, HasUnreadResult: state.HasUnreadResult,
		Provider: state.Provider, ModelProfile: state.ModelProfile, ModelID: state.ModelID, Pricing: state.Pricing,
		ReasoningLevel: state.ReasoningLevel, ModelParameters: state.ModelParameters, Status: sessionStatus(state),
		ProjectID: state.ProjectID, CWD: state.CWD, CreatedCWD: state.CreatedCWD, ConfigPath: state.ConfigPath,
		ConfigDir: state.ConfigDir, EnabledTools: append([]string(nil), state.EnabledTools...),
		EnabledMCP: append([]string(nil), state.EnabledMCP...), EnabledSkills: append([]string(nil), state.EnabledSkills...),
		ShowReasoning: state.ShowReasoning, FullAccess: state.FullAccess, Debug: state.Debug, Context: state.Context,
		SaveToolResults: state.SaveToolResults, ActiveHistory: append([]string(nil), state.ActiveHistory...),
	}
}

func sessionStatus(state sessions.SessionV2) string {
	if strings.TrimSpace(state.RunningRunID) != "" || strings.TrimSpace(state.RunningTurnID) != "" {
		return SessionStatusRunning
	}
	if state.LastRunStatus == sessions.RunStatusFailed {
		return SessionStatusFailed
	}
	if state.LastRunStatus == sessions.RunStatusInterrupted {
		return SessionStatusInterrupted
	}
	if !state.InterruptedAt.IsZero() && (state.LastUsedAt.IsZero() || !state.LastUsedAt.After(state.InterruptedAt)) {
		return SessionStatusInterrupted
	}
	return SessionStatusIdle
}

func compactionStateFromSession(state sessions.SessionV2, max int) CompactionState {
	all := state.Compactions
	truncated := len(all) > max
	if truncated {
		all = all[len(all)-max:]
	}
	out := make([]CompactionCheckpoint, 0, len(all))
	for _, checkpoint := range all {
		out = append(out, CompactionCheckpoint{
			ID: checkpoint.ID, CreatedAt: checkpoint.CreatedAt, Reason: checkpoint.Reason, Phase: checkpoint.Phase,
			Trigger: checkpoint.Trigger, SummaryItemID: checkpoint.SummaryItemID, FromItemID: checkpoint.FromItemID,
			ToItemID: checkpoint.ToItemID, PreviousActiveHistory: append([]string(nil), checkpoint.PreviousActiveHistory...),
			ReplacementHistory: append([]string{}, checkpoint.ReplacementHistory...), SummaryProvider: checkpoint.SummaryProvider,
			SummaryModel: checkpoint.SummaryModel,
		})
	}
	return CompactionState{Checkpoints: out, Truncated: truncated}
}
