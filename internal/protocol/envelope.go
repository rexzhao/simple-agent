package protocol

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

// DefaultMaxMessageBytes is the single V1 frame-size contract. Transport
// implementations and resource producers must use this value rather than
// maintaining independent defaults.
const DefaultMaxMessageBytes = 256 * 1024

// MaxWireIdentifierBytes bounds IDs which are repeated in protocol envelopes
// and resource payloads. It leaves room for resource operations and envelope
// fields inside the frame limit.
const MaxWireIdentifierBytes = 4096

// MessageType is the discriminant carried by every V1 protocol envelope.
type MessageType string

const (
	MessageTypeHello             MessageType = "hello"
	MessageTypeWelcome           MessageType = "welcome"
	MessageTypePing              MessageType = "ping"
	MessageTypePong              MessageType = "pong"
	MessageTypeCommand           MessageType = "command"
	MessageTypeCommandAccepted   MessageType = "command_accepted"
	MessageTypeCommandResult     MessageType = "command_result"
	MessageTypeSubscribe         MessageType = "subscribe"
	MessageTypeSubscribed        MessageType = "subscribed"
	MessageTypeUnsubscribe       MessageType = "unsubscribe"
	MessageTypeUnsubscribed      MessageType = "unsubscribed"
	MessageTypeSnapshot          MessageType = "snapshot"
	MessageTypeChange            MessageType = "change"
	MessageTypeSubscriptionEvent MessageType = "subscription_event"
	MessageTypeAck               MessageType = "ack"
	MessageTypeResyncRequired    MessageType = "resync_required"
	MessageTypeError             MessageType = "error"
)

// Envelope is the wire envelope used by all V1 messages. Concrete message DTOs
// embed it and replace Payload with their message-specific payload type.
type Envelope struct {
	Version   int             `json:"version"`
	Type      MessageType     `json:"type"`
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp,omitempty"`
	TraceID   string          `json:"trace_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

// Message is the set of typed V1 wire messages accepted by DecodeMessage.
type Message interface {
	Kind() MessageType
	messageType() MessageType
	validate() error
}

// DecodeMessage validates and decodes one complete JSON protocol message.
// Unknown optional fields are ignored, but unknown message types and malformed
// required fields are rejected.
func DecodeMessage(data []byte) (Message, error) {
	if !json.Valid(data) {
		return nil, invalidJSON("message is not valid JSON")
	}

	var envelope Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, invalidMessage("envelope", err.Error())
	}
	if err := validateEnvelope(envelope); err != nil {
		return nil, err
	}
	if err := validateOptionalFieldNullability(data, envelope); err != nil {
		return nil, err
	}

	decode := func(target any) error {
		if err := json.Unmarshal(envelope.Payload, target); err != nil {
			return invalidField("payload", err.Error())
		}
		return nil
	}

	switch envelope.Type {
	case MessageTypeHello:
		var payload HelloPayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := HelloMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypeWelcome:
		var payload WelcomePayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := WelcomeMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypePing:
		var payload PingPayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := PingMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypePong:
		var payload PongPayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := PongMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypeCommand:
		var payload CommandPayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := CommandMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypeCommandAccepted:
		var payload CommandAcceptedPayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := CommandAcceptedMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypeCommandResult:
		var payload CommandResultPayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := CommandResultMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypeSubscribe:
		var payload SubscribePayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := SubscribeMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypeSubscribed:
		var payload SubscribedPayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := SubscribedMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypeUnsubscribe:
		var payload UnsubscribePayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := UnsubscribeMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypeUnsubscribed:
		var payload UnsubscribedPayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := UnsubscribedMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypeSnapshot:
		var payload SnapshotPayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := SnapshotMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypeChange:
		var payload ChangePayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := ChangeMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypeSubscriptionEvent:
		var payload SubscriptionEventPayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := SubscriptionEventMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypeAck:
		var payload AckPayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := AckMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypeResyncRequired:
		var payload ResyncRequiredPayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := ResyncRequiredMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	case MessageTypeError:
		var payload ErrorPayload
		if err := decode(&payload); err != nil {
			return nil, err
		}
		message := ErrorMessage{Envelope: envelope, Payload: payload}
		return message, message.validate()
	default:
		return nil, unknownType(envelope.Type)
	}
}

// EncodeMessage validates and encodes a typed V1 message.
func EncodeMessage(message Message) ([]byte, error) {
	if message == nil {
		return nil, invalidMessage("message", "message is nil")
	}
	if err := message.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(message)
}

func validateEnvelope(envelope Envelope) error {
	if envelope.Version != 1 {
		return unsupportedVersion(envelope.Version)
	}
	if !isNonEmpty(envelope.ID) {
		return invalidField("id", "must be a non-empty string")
	}
	if !isKnownMessageType(envelope.Type) {
		return unknownType(envelope.Type)
	}
	if envelope.Timestamp != "" {
		if err := validateRFC3339("timestamp", envelope.Timestamp); err != nil {
			return err
		}
	}
	if !isJSONObject(envelope.Payload) {
		return invalidField("payload", "must be a JSON object")
	}
	return nil
}

func validateOptionalFieldNullability(data []byte, envelope Envelope) error {
	if err := validateOptionalStringField(data, "timestamp", "timestamp", true); err != nil {
		return err
	}
	if err := validateOptionalStringField(data, "trace_id", "trace_id", false); err != nil {
		return err
	}

	var fields []string
	switch envelope.Type {
	case MessageTypeCommand:
		fields = []string{"expected_revision"}
	case MessageTypeSubscribe:
		fields = []string{"resume", "active_run_resume"}
	case MessageTypeCommandResult:
		fields = []string{"error"}
	case MessageTypeError:
		fields = []string{"request_id"}
	}
	for _, field := range fields {
		if err := rejectNullField(envelope.Payload, field, "payload."+field); err != nil {
			return err
		}
	}
	return nil
}

func validateOptionalStringField(object json.RawMessage, name, field string, timestamp bool) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(object, &fields); err != nil {
		return invalidField(field, "must be a JSON object")
	}
	value, ok := fields[name]
	if !ok {
		return nil
	}
	var stringValue string
	if err := json.Unmarshal(value, &stringValue); err != nil {
		return invalidField(field, "must be a string")
	}
	if timestamp {
		return validateRFC3339(field, stringValue)
	}
	return requiredString(field, stringValue)
}

func rejectNullField(object json.RawMessage, name, field string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(object, &fields); err != nil {
		return invalidField(field, "must be a JSON object")
	}
	value, ok := fields[name]
	if ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return invalidField(field, "cannot be null when provided")
	}
	return nil
}

const maxSafeJSONInteger uint64 = 9007199254740991

func validateSafeInteger(field string, value uint64) error {
	if value > maxSafeJSONInteger {
		return invalidField(field, "must be a safe JSON integer")
	}
	return nil
}

func isKnownMessageType(messageType MessageType) bool {
	switch messageType {
	case MessageTypeHello, MessageTypeWelcome, MessageTypePing, MessageTypePong,
		MessageTypeCommand, MessageTypeCommandAccepted, MessageTypeCommandResult,
		MessageTypeSubscribe, MessageTypeSubscribed, MessageTypeUnsubscribe,
		MessageTypeUnsubscribed, MessageTypeSnapshot, MessageTypeChange,
		MessageTypeSubscriptionEvent, MessageTypeAck, MessageTypeResyncRequired,
		MessageTypeError:
		return true
	default:
		return false
	}
}

func isJSONObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return false
	}
	var object struct{}
	return json.Unmarshal(trimmed, &object) == nil
}

func isNonEmpty(value string) bool {
	return len(bytes.TrimSpace([]byte(value))) > 0
}

func requiredString(field, value string) error {
	if !isNonEmpty(value) {
		return invalidField(field, "must be a non-empty string")
	}
	if !utf8.ValidString(value) {
		return invalidField(field, "must be valid UTF-8")
	}
	if len(value) > MaxWireIdentifierBytes {
		return invalidField(field, "exceeds the maximum wire identifier length")
	}
	return nil
}

func requiredRawObject(field string, value json.RawMessage) error {
	if len(value) == 0 {
		return invalidField(field, "is required")
	}
	if !isJSONObject(value) {
		return invalidField(field, "must be a JSON object")
	}
	return nil
}

func optionalRevision(field string, value *ResourceRevision) error {
	if value == nil {
		return nil
	}
	if err := ValidateResourceRevision(*value); err != nil {
		return invalidField(field, err.Error())
	}
	return nil
}

func requiredRevision(field string, value ResourceRevision) error {
	if err := ValidateResourceRevision(value); err != nil {
		return invalidField(field, err.Error())
	}
	return nil
}

func requiredDecimal[T ~string](field string, value T) error {
	if err := requiredString(field, string(value)); err != nil {
		return err
	}
	if err := ValidateDecimal(string(value)); err != nil {
		return invalidField(field, err.Error())
	}
	return nil
}
