package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ResourceType is the closed V1 resource catalog. Resource keys are not
// arbitrary JSON paths.
type ResourceType string

const (
	ResourceTypeProjectIndex     ResourceType = "project_index"
	ResourceTypeSessionIndex     ResourceType = "session_index"
	ResourceTypeSessionContent   ResourceType = "session_content"
	ResourceTypeProviderSettings ResourceType = "provider_settings"
	ResourceTypeModelCatalog     ResourceType = "model_catalog"
	ResourceTypeCodexLogin       ResourceType = "codex_login"
)

type ResourceKey struct {
	Type ResourceType `json:"type"`
	ID   string       `json:"id"`
}

type ResumeToken struct {
	StreamEpoch string   `json:"stream_epoch"`
	Sequence    Sequence `json:"sequence"`
}

// RunResumeToken is deliberately independent from ResumeToken. A run cursor
// is not a durable subscription sequence and is never acknowledged by the
// durable ACK message.
type RunResumeToken struct {
	RunEpoch  string    `json:"run_epoch"`
	RunID     string    `json:"run_id"`
	RunCursor RunCursor `json:"run_cursor"`
}

type HelloPayload struct {
	SupportedVersions []int  `json:"supported_versions"`
	ClientID          string `json:"client_id"`
}

type WelcomePayload struct {
	SelectedVersion     int    `json:"selected_version"`
	ConnectionID        string `json:"connection_id"`
	ServerEpoch         string `json:"server_epoch"`
	HeartbeatIntervalMS int    `json:"heartbeat_interval_ms"`
	MaxMessageBytes     int    `json:"max_message_bytes"`
}

type PingPayload struct{}
type PongPayload struct{}

type CommandPayload struct {
	Name             string            `json:"name"`
	SchemaVersion    int               `json:"schema_version"`
	RequestID        string            `json:"request_id"`
	ExpectedRevision *ResourceRevision `json:"expected_revision,omitempty"`
	Arguments        json.RawMessage   `json:"arguments"`
}

type CommandAcceptedPayload struct {
	RequestID string `json:"request_id"`
}

type CommandStatus string

const (
	CommandStatusSucceeded CommandStatus = "succeeded"
	CommandStatusFailed    CommandStatus = "failed"
)

type CommandError struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

type CommandResultPayload struct {
	RequestID string          `json:"request_id"`
	Status    CommandStatus   `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *CommandError   `json:"error,omitempty"`
}

type SubscribePayload struct {
	SubscriptionID  string          `json:"subscription_id"`
	Resource        ResourceKey     `json:"resource"`
	Resume          *ResumeToken    `json:"resume,omitempty"`
	ActiveRunResume *RunResumeToken `json:"active_run_resume,omitempty"`
}

type SubscribedPayload struct {
	SubscriptionID string      `json:"subscription_id"`
	Resource       ResourceKey `json:"resource"`
	StreamEpoch    string      `json:"stream_epoch"`
	Sequence       Sequence    `json:"sequence"`
}

type UnsubscribePayload struct {
	SubscriptionID string `json:"subscription_id"`
}

type UnsubscribedPayload struct {
	SubscriptionID string `json:"subscription_id"`
}

// BlobDescriptor identifies immutable HTTP data-plane content.
type BlobDescriptor struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Size        uint64 `json:"size"`
	SHA256      string `json:"sha256"`
	ETag        string `json:"etag"`
	ExpiresAt   string `json:"expires_at"`
}

// SnapshotContent is a strict one-of. Inline is intentionally a raw JSON
// boundary because each registered resource owns its content schema.
type SnapshotContent struct {
	Inline json.RawMessage `json:"inline,omitempty"`
	Blob   *BlobDescriptor `json:"blob,omitempty"`
}

type SnapshotPayload struct {
	SubscriptionID   string           `json:"subscription_id"`
	Resource         ResourceKey      `json:"resource"`
	StreamEpoch      string           `json:"stream_epoch"`
	Sequence         Sequence         `json:"sequence"`
	ResourceRevision ResourceRevision `json:"resource_revision"`
	Content          SnapshotContent  `json:"content"`
}

// ChangeOperation keeps the complete resource-specific operation at the
// protocol boundary. Raw is retained so DecodeMessage -> EncodeMessage does
// not discard fields such as metadata.replace or active_run.replace data.
type ChangeOperation struct {
	Op  string
	Raw json.RawMessage
}

func (operation *ChangeOperation) UnmarshalJSON(data []byte) error {
	if !isJSONObject(data) {
		return fmt.Errorf("change operation must be a JSON object")
	}
	var header struct {
		Op string `json:"op"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return err
	}
	operation.Op = header.Op
	operation.Raw = append(operation.Raw[:0], data...)
	return nil
}

func (operation ChangeOperation) MarshalJSON() ([]byte, error) {
	if len(operation.Raw) > 0 {
		return operation.Raw, nil
	}
	return json.Marshal(struct {
		Op string `json:"op"`
	}{Op: operation.Op})
}

type ChangePayload struct {
	SubscriptionID   string            `json:"subscription_id"`
	Resource         ResourceKey       `json:"resource"`
	StreamEpoch      string            `json:"stream_epoch"`
	Sequence         Sequence          `json:"sequence"`
	PreviousSequence Sequence          `json:"previous_sequence"`
	ResourceRevision ResourceRevision  `json:"resource_revision"`
	Operations       []ChangeOperation `json:"operations"`
}

type SubscriptionEventPayload struct {
	SubscriptionID string          `json:"subscription_id"`
	Resource       ResourceKey     `json:"resource"`
	Event          json.RawMessage `json:"event"`
}

type AckPayload struct {
	SubscriptionID string   `json:"subscription_id"`
	StreamEpoch    string   `json:"stream_epoch"`
	Sequence       Sequence `json:"sequence"`
}

type ResyncRequiredPayload struct {
	SubscriptionID string      `json:"subscription_id"`
	Resource       ResourceKey `json:"resource"`
	Reason         string      `json:"reason"`
}

type ErrorPayload struct {
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	RequestID *string         `json:"request_id,omitempty"`
	Details   json.RawMessage `json:"details,omitempty"`
}

// Concrete messages are discriminated by their embedded Envelope.Type.
type HelloMessage struct {
	Envelope
	Payload HelloPayload `json:"payload"`
}

type WelcomeMessage struct {
	Envelope
	Payload WelcomePayload `json:"payload"`
}

type PingMessage struct {
	Envelope
	Payload PingPayload `json:"payload"`
}

type PongMessage struct {
	Envelope
	Payload PongPayload `json:"payload"`
}

type CommandMessage struct {
	Envelope
	Payload CommandPayload `json:"payload"`
}

type CommandAcceptedMessage struct {
	Envelope
	Payload CommandAcceptedPayload `json:"payload"`
}

type CommandResultMessage struct {
	Envelope
	Payload CommandResultPayload `json:"payload"`
}

type SubscribeMessage struct {
	Envelope
	Payload SubscribePayload `json:"payload"`
}

type SubscribedMessage struct {
	Envelope
	Payload SubscribedPayload `json:"payload"`
}

type UnsubscribeMessage struct {
	Envelope
	Payload UnsubscribePayload `json:"payload"`
}

type UnsubscribedMessage struct {
	Envelope
	Payload UnsubscribedPayload `json:"payload"`
}

type SnapshotMessage struct {
	Envelope
	Payload SnapshotPayload `json:"payload"`
}

type ChangeMessage struct {
	Envelope
	Payload ChangePayload `json:"payload"`
}

type SubscriptionEventMessage struct {
	Envelope
	Payload SubscriptionEventPayload `json:"payload"`
}

type AckMessage struct {
	Envelope
	Payload AckPayload `json:"payload"`
}

type ResyncRequiredMessage struct {
	Envelope
	Payload ResyncRequiredPayload `json:"payload"`
}

type ErrorMessage struct {
	Envelope
	Payload ErrorPayload `json:"payload"`
}

// Short aliases keep the DTO names convenient without introducing a second
// representation for any message.
type Hello = HelloMessage
type Welcome = WelcomeMessage
type Ping = PingMessage
type Pong = PongMessage
type Command = CommandMessage
type CommandAccepted = CommandAcceptedMessage
type CommandResult = CommandResultMessage
type Subscribe = SubscribeMessage
type Subscribed = SubscribedMessage
type Unsubscribe = UnsubscribeMessage
type Unsubscribed = UnsubscribedMessage
type Snapshot = SnapshotMessage
type Change = ChangeMessage
type SubscriptionEvent = SubscriptionEventMessage
type Ack = AckMessage
type ResyncRequired = ResyncRequiredMessage
type Error = ErrorMessage

func (m HelloMessage) Kind() MessageType             { return MessageTypeHello }
func (m WelcomeMessage) Kind() MessageType           { return MessageTypeWelcome }
func (m PingMessage) Kind() MessageType              { return MessageTypePing }
func (m PongMessage) Kind() MessageType              { return MessageTypePong }
func (m CommandMessage) Kind() MessageType           { return MessageTypeCommand }
func (m CommandAcceptedMessage) Kind() MessageType   { return MessageTypeCommandAccepted }
func (m CommandResultMessage) Kind() MessageType     { return MessageTypeCommandResult }
func (m SubscribeMessage) Kind() MessageType         { return MessageTypeSubscribe }
func (m SubscribedMessage) Kind() MessageType        { return MessageTypeSubscribed }
func (m UnsubscribeMessage) Kind() MessageType       { return MessageTypeUnsubscribe }
func (m UnsubscribedMessage) Kind() MessageType      { return MessageTypeUnsubscribed }
func (m SnapshotMessage) Kind() MessageType          { return MessageTypeSnapshot }
func (m ChangeMessage) Kind() MessageType            { return MessageTypeChange }
func (m SubscriptionEventMessage) Kind() MessageType { return MessageTypeSubscriptionEvent }
func (m AckMessage) Kind() MessageType               { return MessageTypeAck }
func (m ResyncRequiredMessage) Kind() MessageType    { return MessageTypeResyncRequired }
func (m ErrorMessage) Kind() MessageType             { return MessageTypeError }

func (m HelloMessage) messageType() MessageType             { return MessageTypeHello }
func (m WelcomeMessage) messageType() MessageType           { return MessageTypeWelcome }
func (m PingMessage) messageType() MessageType              { return MessageTypePing }
func (m PongMessage) messageType() MessageType              { return MessageTypePong }
func (m CommandMessage) messageType() MessageType           { return MessageTypeCommand }
func (m CommandAcceptedMessage) messageType() MessageType   { return MessageTypeCommandAccepted }
func (m CommandResultMessage) messageType() MessageType     { return MessageTypeCommandResult }
func (m SubscribeMessage) messageType() MessageType         { return MessageTypeSubscribe }
func (m SubscribedMessage) messageType() MessageType        { return MessageTypeSubscribed }
func (m UnsubscribeMessage) messageType() MessageType       { return MessageTypeUnsubscribe }
func (m UnsubscribedMessage) messageType() MessageType      { return MessageTypeUnsubscribed }
func (m SnapshotMessage) messageType() MessageType          { return MessageTypeSnapshot }
func (m ChangeMessage) messageType() MessageType            { return MessageTypeChange }
func (m SubscriptionEventMessage) messageType() MessageType { return MessageTypeSubscriptionEvent }
func (m AckMessage) messageType() MessageType               { return MessageTypeAck }
func (m ResyncRequiredMessage) messageType() MessageType    { return MessageTypeResyncRequired }
func (m ErrorMessage) messageType() MessageType             { return MessageTypeError }

func validateTypedEnvelope(envelope Envelope, expected MessageType) error {
	if envelope.Version != 1 {
		return unsupportedVersion(envelope.Version)
	}
	if envelope.Type != expected {
		return invalidField("type", fmt.Sprintf("must be %q", expected))
	}
	if err := requiredString("id", envelope.ID); err != nil {
		return err
	}
	if envelope.Timestamp != "" {
		if err := validateRFC3339("timestamp", envelope.Timestamp); err != nil {
			return err
		}
	}
	if envelope.TraceID != "" {
		if err := requiredString("trace_id", envelope.TraceID); err != nil {
			return err
		}
	}
	return nil
}

func (m HelloMessage) validate() error {
	if err := validateTypedEnvelope(m.Envelope, m.messageType()); err != nil {
		return err
	}
	if len(m.Payload.SupportedVersions) == 0 {
		return invalidField("payload.supported_versions", "must contain at least one version")
	}
	hasV1 := false
	for index, version := range m.Payload.SupportedVersions {
		if version < 1 {
			return invalidField(fmt.Sprintf("payload.supported_versions[%d]", index), "must be positive")
		}
		if err := validateSafeInteger(fmt.Sprintf("payload.supported_versions[%d]", index), uint64(version)); err != nil {
			return err
		}
		if version == 1 {
			hasV1 = true
		}
	}
	if !hasV1 {
		return invalidField("payload.supported_versions", "must include version 1")
	}
	return requiredString("payload.client_id", m.Payload.ClientID)
}

func (m WelcomeMessage) validate() error {
	if err := validateTypedEnvelope(m.Envelope, m.messageType()); err != nil {
		return err
	}
	if m.Payload.SelectedVersion != 1 {
		return invalidField("payload.selected_version", "must be 1")
	}
	if err := requiredString("payload.connection_id", m.Payload.ConnectionID); err != nil {
		return err
	}
	if err := requiredString("payload.server_epoch", m.Payload.ServerEpoch); err != nil {
		return err
	}
	if m.Payload.HeartbeatIntervalMS <= 0 {
		return invalidField("payload.heartbeat_interval_ms", "must be positive")
	}
	if err := validateSafeInteger("payload.heartbeat_interval_ms", uint64(m.Payload.HeartbeatIntervalMS)); err != nil {
		return err
	}
	if m.Payload.MaxMessageBytes <= 0 {
		return invalidField("payload.max_message_bytes", "must be positive")
	}
	if err := validateSafeInteger("payload.max_message_bytes", uint64(m.Payload.MaxMessageBytes)); err != nil {
		return err
	}
	return nil
}

func (m PingMessage) validate() error { return validateTypedEnvelope(m.Envelope, m.messageType()) }
func (m PongMessage) validate() error { return validateTypedEnvelope(m.Envelope, m.messageType()) }

func (m CommandMessage) validate() error {
	if err := validateTypedEnvelope(m.Envelope, m.messageType()); err != nil {
		return err
	}
	if err := requiredString("payload.name", m.Payload.Name); err != nil {
		return err
	}
	if m.Payload.SchemaVersion < 1 {
		return invalidField("payload.schema_version", "must be positive")
	}
	if err := validateSafeInteger("payload.schema_version", uint64(m.Payload.SchemaVersion)); err != nil {
		return err
	}
	if err := requiredString("payload.request_id", m.Payload.RequestID); err != nil {
		return err
	}
	if err := optionalRevision("payload.expected_revision", m.Payload.ExpectedRevision); err != nil {
		return err
	}
	return requiredRawObject("payload.arguments", m.Payload.Arguments)
}

func (m CommandAcceptedMessage) validate() error {
	if err := validateTypedEnvelope(m.Envelope, m.messageType()); err != nil {
		return err
	}
	return requiredString("payload.request_id", m.Payload.RequestID)
}

func (m CommandResultMessage) validate() error {
	if err := validateTypedEnvelope(m.Envelope, m.messageType()); err != nil {
		return err
	}
	if err := requiredString("payload.request_id", m.Payload.RequestID); err != nil {
		return err
	}
	switch m.Payload.Status {
	case CommandStatusSucceeded:
		if m.Payload.Error != nil {
			return invalidField("payload.error", "must be omitted for a succeeded command")
		}
	case CommandStatusFailed:
		if m.Payload.Error == nil {
			return invalidField("payload.error", "is required for a failed command")
		}
		if err := m.Payload.Error.validate("payload.error"); err != nil {
			return err
		}
	default:
		return invalidField("payload.status", "must be succeeded or failed")
	}
	if len(m.Payload.Result) > 0 && bytes.Equal(bytes.TrimSpace(m.Payload.Result), []byte("null")) {
		return invalidField("payload.result", "cannot be null when provided")
	}
	if len(m.Payload.Result) > 0 && !json.Valid(m.Payload.Result) {
		return invalidField("payload.result", "must be valid JSON")
	}
	return nil
}

func (e CommandError) validate(field string) error {
	if err := requiredString(field+".code", e.Code); err != nil {
		return err
	}
	if err := requiredString(field+".message", e.Message); err != nil {
		return err
	}
	if len(e.Details) > 0 && !json.Valid(e.Details) {
		return invalidField(field+".details", "must be valid JSON")
	}
	return nil
}

func (m SubscribeMessage) validate() error {
	if err := validateTypedEnvelope(m.Envelope, m.messageType()); err != nil {
		return err
	}
	if err := requiredString("payload.subscription_id", m.Payload.SubscriptionID); err != nil {
		return err
	}
	if err := validateResourceKey("payload.resource", m.Payload.Resource); err != nil {
		return err
	}
	if err := validateResume("payload.resume", m.Payload.Resume); err != nil {
		return err
	}
	if m.Payload.ActiveRunResume != nil && m.Payload.Resource.Type != ResourceTypeSessionContent {
		return invalidField("payload.active_run_resume", "is only valid for session_content")
	}
	return validateRunResume("payload.active_run_resume", m.Payload.ActiveRunResume)
}

func (m SubscribedMessage) validate() error {
	if err := validateTypedEnvelope(m.Envelope, m.messageType()); err != nil {
		return err
	}
	if err := requiredString("payload.subscription_id", m.Payload.SubscriptionID); err != nil {
		return err
	}
	if err := validateResourceKey("payload.resource", m.Payload.Resource); err != nil {
		return err
	}
	if err := requiredString("payload.stream_epoch", m.Payload.StreamEpoch); err != nil {
		return err
	}
	return requiredDecimal("payload.sequence", m.Payload.Sequence)
}

func validateResume(field string, resume *ResumeToken) error {
	if resume == nil {
		return nil
	}
	if err := requiredString(field+".stream_epoch", resume.StreamEpoch); err != nil {
		return err
	}
	return requiredDecimal(field+".sequence", resume.Sequence)
}

// ValidateResumeToken validates the optional resume token shape used by the
// subscribe wire message. Nil is valid and means an initial snapshot.
func ValidateResumeToken(resume *ResumeToken) error {
	return validateResume("resume", resume)
}

func validateRunResume(field string, resume *RunResumeToken) error {
	if resume == nil {
		return nil
	}
	if err := requiredString(field+".run_epoch", resume.RunEpoch); err != nil {
		return err
	}
	if err := requiredString(field+".run_id", resume.RunID); err != nil {
		return err
	}
	if err := requiredString(field+".run_cursor", string(resume.RunCursor)); err != nil {
		return err
	}
	if err := ValidateRunCursor(resume.RunCursor); err != nil {
		return invalidField(field+".run_cursor", err.Error())
	}
	return nil
}

func ValidateRunResumeToken(resume *RunResumeToken) error {
	return validateRunResume("active_run_resume", resume)
}

func (m UnsubscribeMessage) validate() error {
	if err := validateTypedEnvelope(m.Envelope, m.messageType()); err != nil {
		return err
	}
	return requiredString("payload.subscription_id", m.Payload.SubscriptionID)
}

func (m UnsubscribedMessage) validate() error {
	if err := validateTypedEnvelope(m.Envelope, m.messageType()); err != nil {
		return err
	}
	return requiredString("payload.subscription_id", m.Payload.SubscriptionID)
}

func (m SnapshotMessage) validate() error {
	if err := validateTypedEnvelope(m.Envelope, m.messageType()); err != nil {
		return err
	}
	if err := requiredString("payload.subscription_id", m.Payload.SubscriptionID); err != nil {
		return err
	}
	if err := validateResourceKey("payload.resource", m.Payload.Resource); err != nil {
		return err
	}
	if err := requiredString("payload.stream_epoch", m.Payload.StreamEpoch); err != nil {
		return err
	}
	if err := requiredDecimal("payload.sequence", m.Payload.Sequence); err != nil {
		return err
	}
	if err := requiredRevision("payload.resource_revision", m.Payload.ResourceRevision); err != nil {
		return err
	}
	return validateSnapshotContent(m.Payload.Content)
}

func validateSnapshotContent(content SnapshotContent) error {
	hasInline := len(content.Inline) > 0
	hasBlob := content.Blob != nil
	if hasInline == hasBlob {
		return invalidField("payload.content", "must contain exactly one of inline or blob")
	}
	if hasInline {
		return requiredRawObject("payload.content.inline", content.Inline)
	}
	return validateBlobDescriptor(*content.Blob)
}

// ValidateSnapshotContent validates the same snapshot inline/blob union used
// by SnapshotMessage. It is exported for internal protocol producers so they
// cannot accept content that only fails later during wire encoding.
func ValidateSnapshotContent(content SnapshotContent) error {
	return validateSnapshotContent(content)
}

func validateBlobDescriptor(blob BlobDescriptor) error {
	if err := requiredString("payload.content.blob.id", blob.ID); err != nil {
		return err
	}
	if err := requiredString("payload.content.blob.url", blob.URL); err != nil {
		return err
	}
	if err := requiredString("payload.content.blob.content_type", blob.ContentType); err != nil {
		return err
	}
	if err := validateSafeInteger("payload.content.blob.size", blob.Size); err != nil {
		return err
	}
	if err := requiredString("payload.content.blob.sha256", blob.SHA256); err != nil {
		return err
	}
	if err := requiredString("payload.content.blob.etag", blob.ETag); err != nil {
		return err
	}
	return validateRFC3339("payload.content.blob.expires_at", blob.ExpiresAt)
}

// ValidateBlobDescriptor validates blob metadata, including the safe JSON
// integer size bound and the strict RFC3339 expiry format.
func ValidateBlobDescriptor(blob BlobDescriptor) error {
	return validateBlobDescriptor(blob)
}

func (m ChangeMessage) validate() error {
	if err := validateTypedEnvelope(m.Envelope, m.messageType()); err != nil {
		return err
	}
	if err := validateSubscriptionSequenceFields(m.Payload.SubscriptionID, m.Payload.Resource, m.Payload.StreamEpoch, m.Payload.Sequence, m.Payload.PreviousSequence, m.Payload.ResourceRevision); err != nil {
		return err
	}
	if len(m.Payload.Operations) == 0 {
		return invalidField("payload.operations", "must contain at least one operation")
	}
	for index, operation := range m.Payload.Operations {
		field := fmt.Sprintf("payload.operations[%d]", index)
		if err := operation.validate(field); err != nil {
			return err
		}
	}
	return nil
}

func (o ChangeOperation) validate(field string) error {
	if err := requiredString(field+".op", o.Op); err != nil {
		return err
	}
	if len(o.Raw) == 0 {
		return nil
	}
	if !isJSONObject(o.Raw) {
		return invalidField(field, "raw operation must be a JSON object")
	}
	var rawHeader struct {
		Op string `json:"op"`
	}
	if err := json.Unmarshal(o.Raw, &rawHeader); err != nil {
		return invalidField(field, err.Error())
	}
	if err := requiredString(field+".raw.op", rawHeader.Op); err != nil {
		return err
	}
	if rawHeader.Op != o.Op {
		return invalidField(field+".op", "does not match raw operation op")
	}
	return nil
}

// ValidateChangeOperation validates the operation form accepted by the V1
// change wire message. This keeps internal sync producers aligned with the
// protocol encoder.
func ValidateChangeOperation(operation ChangeOperation) error {
	return operation.validate("operation")
}

func validateSubscriptionSequenceFields(subscriptionID string, resource ResourceKey, streamEpoch string, sequence, previousSequence Sequence, revision ResourceRevision) error {
	if err := requiredString("payload.subscription_id", subscriptionID); err != nil {
		return err
	}
	if err := validateResourceKey("payload.resource", resource); err != nil {
		return err
	}
	if err := requiredString("payload.stream_epoch", streamEpoch); err != nil {
		return err
	}
	if err := requiredDecimal("payload.sequence", sequence); err != nil {
		return err
	}
	if err := requiredDecimal("payload.previous_sequence", previousSequence); err != nil {
		return err
	}
	return requiredRevision("payload.resource_revision", revision)
}

func (m SubscriptionEventMessage) validate() error {
	if err := validateTypedEnvelope(m.Envelope, m.messageType()); err != nil {
		return err
	}
	if err := requiredString("payload.subscription_id", m.Payload.SubscriptionID); err != nil {
		return err
	}
	if err := validateResourceKey("payload.resource", m.Payload.Resource); err != nil {
		return err
	}
	if err := validateSubscriptionEvent("payload.event", m.Payload.Event); err != nil {
		return err
	}
	if m.Payload.Resource.Type == ResourceTypeSessionContent {
		event, err := DecodeSubscriptionEvent(m.Payload.Event)
		if err != nil {
			return invalidField("payload.event", err.Error())
		}
		if event.SessionID != m.Payload.Resource.ID {
			return invalidField("payload.event.session_id", "does not match payload.resource.id")
		}
	}
	return nil
}

func validateSubscriptionEvent(field string, event json.RawMessage) error {
	if err := requiredRawObject(field, event); err != nil {
		return err
	}
	if err := ValidateSubscriptionEvent(event); err != nil {
		return invalidField(field, err.Error())
	}
	return nil
}

func (m AckMessage) validate() error {
	if err := validateTypedEnvelope(m.Envelope, m.messageType()); err != nil {
		return err
	}
	if err := requiredString("payload.subscription_id", m.Payload.SubscriptionID); err != nil {
		return err
	}
	if err := requiredString("payload.stream_epoch", m.Payload.StreamEpoch); err != nil {
		return err
	}
	return requiredDecimal("payload.sequence", m.Payload.Sequence)
}

func (m ResyncRequiredMessage) validate() error {
	if err := validateTypedEnvelope(m.Envelope, m.messageType()); err != nil {
		return err
	}
	if err := requiredString("payload.subscription_id", m.Payload.SubscriptionID); err != nil {
		return err
	}
	if err := validateResourceKey("payload.resource", m.Payload.Resource); err != nil {
		return err
	}
	return requiredString("payload.reason", m.Payload.Reason)
}

func (m ErrorMessage) validate() error {
	if err := validateTypedEnvelope(m.Envelope, m.messageType()); err != nil {
		return err
	}
	if err := requiredString("payload.code", m.Payload.Code); err != nil {
		return err
	}
	if err := requiredString("payload.message", m.Payload.Message); err != nil {
		return err
	}
	if m.Payload.RequestID != nil {
		if err := requiredString("payload.request_id", *m.Payload.RequestID); err != nil {
			return err
		}
	}
	if len(m.Payload.Details) > 0 && !json.Valid(m.Payload.Details) {
		return invalidField("payload.details", "must be valid JSON")
	}
	return nil
}

func validateResourceKey(field string, resource ResourceKey) error {
	if err := requiredString(field+".type", string(resource.Type)); err != nil {
		return err
	}
	switch resource.Type {
	case ResourceTypeProjectIndex, ResourceTypeSessionIndex, ResourceTypeSessionContent,
		ResourceTypeProviderSettings, ResourceTypeModelCatalog, ResourceTypeCodexLogin:
	default:
		return invalidField(field+".type", fmt.Sprintf("unknown resource type %q", resource.Type))
	}
	return requiredString(field+".id", resource.ID)
}

// ValidateResourceKey validates the closed resource key shape used by wire
// messages and internal sync providers.
func ValidateResourceKey(resource ResourceKey) error {
	return validateResourceKey("resource", resource)
}
