package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Claims holds the public claims extracted from a Codex access token JWT.
// Signature is intentionally not verified: the token originates from our own
// auth file, and only claim extraction is needed here.
type Claims struct {
	ChatGPTAccountID     string `json:"chatgpt_account_id"`
	ChatGPTAccountUserID string `json:"chatgpt_account_user_id"`
	UserID               string `json:"user_id"`
	POI                  string `json:"poi"`
	PlanType             string `json:"chatgpt_plan_type"`
	SessionID            string `json:"session_id"`
}

// DecodeClaims decodes the payload segment of a Codex access token JWT.
// Missing claims are returned as empty strings without error; malformed
// tokens return an error.
func DecodeClaims(accessToken string) (Claims, error) {
	var claims Claims
	if strings.TrimSpace(accessToken) == "" {
		return claims, fmt.Errorf("cannot decode claims from empty access token")
	}
	segments := strings.Split(strings.TrimSpace(accessToken), ".")
	if len(segments) != 3 {
		return claims, fmt.Errorf("access token is not a JWT with three segments")
	}
	payload, err := base64.RawURLEncoding.DecodeString(stripBase64Padding(segments[1]))
	if err != nil {
		return claims, fmt.Errorf("decode access token payload: %w", err)
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, fmt.Errorf("parse access token claims: %w", err)
	}
	return claims, nil
}

// stripBase64Padding removes trailing "=" so RawURLEncoding.DecodeString
// accepts segments whether or not the emitter included padding.
func stripBase64Padding(value string) string {
	return strings.TrimRight(value, "=")
}
