package protocol

import "fmt"

// ErrorCode identifies a stable protocol validation or decoding failure.
type ErrorCode string

const (
	ErrorCodeInvalidJSON        ErrorCode = "invalid_json"
	ErrorCodeInvalidMessage     ErrorCode = "invalid_message"
	ErrorCodeInvalidField       ErrorCode = "invalid_field"
	ErrorCodeUnsupportedVersion ErrorCode = "unsupported_version"
	ErrorCodeUnknownType        ErrorCode = "unknown_type"
)

// ProtocolError is returned when a wire message cannot be decoded or validated.
type ProtocolError struct {
	Code  ErrorCode
	Field string
	Msg   string
}

func (e *ProtocolError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("protocol %s: %s", e.Code, e.Msg)
	}
	return fmt.Sprintf("protocol %s (%s): %s", e.Code, e.Field, e.Msg)
}

func invalidMessage(field, message string) error {
	return &ProtocolError{Code: ErrorCodeInvalidMessage, Field: field, Msg: message}
}

func invalidField(field, message string) error {
	return &ProtocolError{Code: ErrorCodeInvalidField, Field: field, Msg: message}
}

func invalidJSON(message string) error {
	return &ProtocolError{Code: ErrorCodeInvalidJSON, Msg: message}
}

func unsupportedVersion(version int) error {
	return &ProtocolError{
		Code:  ErrorCodeUnsupportedVersion,
		Field: "version",
		Msg:   fmt.Sprintf("version %d is not supported", version),
	}
}

func unknownType(messageType MessageType) error {
	return &ProtocolError{
		Code:  ErrorCodeUnknownType,
		Field: "type",
		Msg:   fmt.Sprintf("unknown message type %q", messageType),
	}
}
