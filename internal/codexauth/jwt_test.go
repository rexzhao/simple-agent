package codexauth

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestDecodeClaimsExtractsChatGPTAccountID(t *testing.T) {
	payload := `{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-123"},"chatgpt_account_id":"acct-123","chatgpt_account_user_id":"user-1__acct-123","user_id":"user-1","poi":"org-abc","chatgpt_plan_type":"pro","session_id":"sess-1"}`
	token := fakeJWT(payload)
	claims, err := DecodeClaims(token)
	if err != nil {
		t.Fatalf("DecodeClaims() error = %v", err)
	}
	if claims.ChatGPTAccountID != "acct-123" {
		t.Fatalf("ChatGPTAccountID = %q, want acct-123", claims.ChatGPTAccountID)
	}
	if claims.ChatGPTAccountUserID != "user-1__acct-123" {
		t.Fatalf("ChatGPTAccountUserID = %q, want user-1__acct-123", claims.ChatGPTAccountUserID)
	}
	if claims.UserID != "user-1" || claims.POI != "org-abc" || claims.PlanType != "pro" || claims.SessionID != "sess-1" {
		t.Fatalf("claims = %#v, want remaining public claims", claims)
	}
}

func TestDecodeClaimsMissingFieldsReturnEmpty(t *testing.T) {
	claims, err := DecodeClaims(fakeJWT(`{"user_id":"only-user"}`))
	if err != nil {
		t.Fatalf("DecodeClaims() error = %v", err)
	}
	if claims.ChatGPTAccountID != "" {
		t.Fatalf("ChatGPTAccountID = %q, want empty when absent", claims.ChatGPTAccountID)
	}
	if claims.UserID != "only-user" {
		t.Fatalf("UserID = %q, want only-user", claims.UserID)
	}
}

func TestDecodeClaimsHandlesUnpaddedPayload(t *testing.T) {
	// "{}" encodes to "e30" (3 chars, needs one "=" pad).
	token := "header.e30.signature"
	claims, err := DecodeClaims(token)
	if err != nil {
		t.Fatalf("DecodeClaims(unpadded) error = %v", err)
	}
	if claims.UserID != "" {
		t.Fatalf("claims = %#v, want empty claims", claims)
	}
}

func TestDecodeClaimsRejectsMalformedTokens(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{name: "empty", token: ""},
		{name: "two segments", token: "a.b"},
		{name: "bad base64", token: "a.!!!.c"},
		{name: "not json", token: "a." + strings.TrimRight(base64.RawURLEncoding.EncodeToString([]byte("not-json")), "=") + ".c"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeClaims(test.token); err == nil {
				t.Fatalf("DecodeClaims(%q) error = nil, want error", test.token)
			}
		})
	}
}

// fakeJWT builds a three-segment token whose payload is the given JSON.
func fakeJWT(payload string) string {
	return "header." + strings.TrimRight(base64.RawURLEncoding.EncodeToString([]byte(payload)), "=") + ".signature"
}
