package web

// Approval endpoint tests (B2 step 7a): the wire contract of
// POST /api/s/{id}/approval — guardAPI, session-level codes first,
// then the approval-level codes. The "pending approval exists" cases
// (200 / repeated 409) need the Ask state, which has no production
// trigger until 7b wires the policy hook; they land with 7b.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func approvalPost(base string, sessionID string, body string, headers map[string]string) (*http.Response, error) {
	req, _ := http.NewRequest("POST", base+"/api/s/"+sessionID+"/approval", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

// TestApprovalEndpointUnknownSession: an unregistered session gets
// the SESSION-level code first (unknown_session), never the
// approval-level ones.
func TestApprovalEndpointUnknownSession(t *testing.T) {
	_, _, base := newTestServer(t, nil)
	resp, err := approvalPost(base.URL, "s0000doesnotexist00", `{"item":"x","allow":true}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "unknown_session" {
		t.Fatalf("code = %q, want unknown_session (session-level first)", body["error"])
	}
}

// TestApprovalEndpointNoPending: a valid session with no pending
// approval → 404 approval_unknown.
func TestApprovalEndpointNoPending(t *testing.T) {
	session, _, base := newTestServer(t, nil)
	resp, err := approvalPost(base.URL, session.ID(), `{"item":"s0000000000000000000000001","allow":true}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "approval_unknown" {
		t.Fatalf("code = %q, want approval_unknown", body["error"])
	}
}

// TestApprovalEndpointBadRequests: malformed JSON and a missing item
// are plain-text 400s (the parse-layer boundary, §4).
func TestApprovalEndpointBadRequests(t *testing.T) {
	session, _, base := newTestServer(t, nil)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"bad json", `{not json`},
		{"missing item", `{"allow":true}`},
		{"empty item", `{"item":"  ","allow":true}`},
	} {
		resp, err := approvalPost(base.URL, session.ID(), tc.body, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", tc.name, resp.StatusCode)
		}
	}
}

// TestApprovalEndpointGuarded: the endpoint is a write — the CSRF
// guards apply (cross-origin simple POST is 403).
func TestApprovalEndpointGuarded(t *testing.T) {
	session, _, base := newTestServer(t, nil)
	resp, err := approvalPost(base.URL, session.ID(), `{"item":"x","allow":true}`, map[string]string{
		"Content-Type": "text/plain",
		"Origin":       "http://evil.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin simple POST: status = %d, want 403", resp.StatusCode)
	}
}
