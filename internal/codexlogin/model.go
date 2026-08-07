// Package codexlogin contains the safe, read-only sync projection for the
// Codex device-login lifecycle.  It deliberately does not contain the login
// state machine or token store; those remain owned by execution/webapp.
package codexlogin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

const (
	MaxLoginIDBytes         = 128
	MaxUserCodeBytes        = 128
	MaxVerificationURLBytes = 2048
)

const (
	StatusSignedOut = "signed_out"
	StatusPending   = "pending"
	StatusSignedIn  = "signed_in"
	StatusExpired   = "expired"
	StatusError     = "error"
)

const (
	OperationReplace = "replace"
	ErrorLoginFailed = "login_failed"
	ErrorLoginStart  = "login_start_failed"
)

var (
	ErrProviderInvalid     = errors.New("Codex provider is invalid")
	ErrProviderNotFound    = errors.New("Codex provider was not found")
	ErrProviderNotCodex    = errors.New("provider is not configured for Codex")
	ErrProviderUnavailable = errors.New("Codex provider is unavailable")
)

// Snapshot is the complete safe resource DTO.  In particular it has no
// account identifier, diagnostic text, token, auth-file content, or API key.
// The device-code fields are the only capabilities exposed and are bounded
// and filtered by SnapshotFromAuthStatus.
type Snapshot struct {
	Provider        string `json:"provider"`
	Status          string `json:"status"`
	LoginID         string `json:"login_id"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	Refreshable     bool   `json:"refreshable"`
	ErrorCode       string `json:"error_code"`
	ErrorMessage    string `json:"error_message"`
}

func (s Snapshot) Validate() error {
	if err := config.ValidateProviderName(s.Provider); err != nil {
		return fmt.Errorf("invalid provider: %w", err)
	}
	switch s.Status {
	case StatusSignedOut, StatusPending, StatusSignedIn, StatusExpired, StatusError:
	default:
		return fmt.Errorf("invalid Codex login status")
	}
	if !boundedText(s.LoginID, MaxLoginIDBytes, true) || !boundedText(s.UserCode, MaxUserCodeBytes, true) || !boundedText(s.ErrorCode, 128, true) || !boundedText(s.ErrorMessage, 256, true) {
		return fmt.Errorf("Codex login text exceeds its boundary")
	}
	if s.VerificationURL != "" && !safeVerificationURL(s.VerificationURL) {
		return fmt.Errorf("verification URL is not allowed")
	}
	if s.Status != StatusPending && (s.UserCode != "" || s.VerificationURL != "") {
		return fmt.Errorf("device-login fields require pending status")
	}
	if s.Status != StatusError && (s.ErrorCode != "" || s.ErrorMessage != "") {
		return fmt.Errorf("error fields require error status")
	}
	if s.Status == StatusError {
		if s.ErrorCode != ErrorLoginFailed && s.ErrorCode != ErrorLoginStart {
			return fmt.Errorf("invalid Codex login error code")
		}
		if s.ErrorMessage == "" {
			return fmt.Errorf("Codex login error message is required")
		}
	}
	if s.Status != StatusSignedIn && s.Status != StatusExpired && s.Refreshable {
		return fmt.Errorf("refreshable is only valid for an authenticated status")
	}
	return nil
}

func boundedText(value string, max int, allowEmpty bool) bool {
	if !utf8.ValidString(value) || (!allowEmpty && value == "") || len([]byte(value)) > max {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func safeVerificationURL(value string) bool {
	if !boundedText(value, MaxVerificationURLBytes, false) {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Host == "" || parsed.Path == "" || parsed.Path == "/" {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	// HTTP is accepted only for loopback fake device flows used by tests and
	// local development. Production device URLs remain HTTPS-only.
	if parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// SnapshotFromAuthStatus maps execution's status DTO into the intentionally
// smaller resource DTO.  The status.Message field is never copied because it
// may contain a filesystem path or provider diagnostic.
func SnapshotFromAuthStatus(provider string, status execution.CodexAuthStatus) Snapshot {
	result := Snapshot{Provider: provider, Status: status.Status}
	switch result.Status {
	case StatusSignedOut:
		// no fields
	case StatusPending:
		result.LoginID = safeText(status.LoginID, MaxLoginIDBytes)
		result.UserCode = safeText(status.UserCode, MaxUserCodeBytes)
		if safeVerificationURL(status.VerifyURL) {
			result.VerificationURL = status.VerifyURL
		}
	case StatusSignedIn, StatusExpired:
		result.Refreshable = status.Refreshable
	case StatusError:
		result.ErrorCode = ErrorLoginFailed
		result.ErrorMessage = "Codex login failed"
	default:
		result.Status = StatusError
		result.ErrorCode = ErrorLoginFailed
		result.ErrorMessage = "Codex login failed"
	}
	if result.Validate() != nil {
		return Snapshot{Provider: provider, Status: StatusError, ErrorCode: ErrorLoginFailed, ErrorMessage: "Codex login failed"}
	}
	return result
}

// ValidateDeviceCapabilities is used at the execution boundary immediately
// after the external device endpoint returns. Unlike SnapshotFromAuthStatus,
// which is a defensive projection for an already-owned status DTO, this
// function rejects an unsafe device response so callers cannot turn it into a
// misleading pending state with empty capabilities.
func ValidateDeviceCapabilities(provider string, status execution.CodexAuthStatus) error {
	if status.UserCode == "" || status.VerifyURL == "" {
		return fmt.Errorf("device-login capabilities are incomplete")
	}
	return (Snapshot{
		Provider:        provider,
		Status:          StatusPending,
		LoginID:         status.LoginID,
		UserCode:        status.UserCode,
		VerificationURL: status.VerifyURL,
	}).Validate()
}

func snapshotFromError(provider string) Snapshot {
	return Snapshot{Provider: provider, Status: StatusError, ErrorCode: ErrorLoginFailed, ErrorMessage: "Codex login failed"}
}

func safeText(value string, max int) string {
	if boundedText(value, max, true) {
		return value
	}
	return ""
}

type Operation struct {
	Provider string
	Value    Snapshot
}

func (o Operation) ToResourceChange(revision string) (syncengine.ResourceChange, error) {
	if _, err := protocol.ParseUint64Decimal(revision); err != nil {
		return syncengine.ResourceChange{}, fmt.Errorf("invalid resource revision")
	}
	if err := config.ValidateProviderName(o.Provider); err != nil || o.Value.Provider != o.Provider || o.Value.Validate() != nil {
		return syncengine.ResourceChange{}, fmt.Errorf("invalid Codex login operation")
	}
	raw, err := json.Marshal(struct {
		Op    string   `json:"op"`
		Key   string   `json:"key"`
		Value Snapshot `json:"value"`
	}{OperationReplace, o.Provider, o.Value})
	if err != nil {
		return syncengine.ResourceChange{}, fmt.Errorf("encode Codex login operation")
	}
	return syncengine.ResourceChange{
		ResourceRevision: protocol.ResourceRevision(revision),
		Operations:       []protocol.ChangeOperation{{Op: OperationReplace, Raw: raw}},
	}, nil
}

// CommittedChange is an identifier-only publication from the login domain.
type CommittedChange struct{ Provider string }

type ChangeSink interface {
	PublishCommitted(CommittedChange) error
}
