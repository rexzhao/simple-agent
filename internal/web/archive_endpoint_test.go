package web

// POST/DELETE /api/s/{id}/archive endpoint tests (C5): the
// archive/unarchive round-trip, the open refusal converging to 409
// session_archived, the listing hiding archived rows, the lock
// competition mapping to session_locked, and the shared refusals
// (running, watched).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/storage/jsonl"
)

func archiveCall(method, base, id string) (*http.Response, map[string]string) {
	req, _ := http.NewRequest(method, base+"/api/s/"+id+"/archive", nil) //nolint:noctx // test
	// guardAPI's content-type bar applies to every POST (no body here,
	// but the header is part of the POST contract).
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil
	}
	body := map[string]string{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	return resp, body
}

func TestArchiveEndpointRoundTrip(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, base, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)
	id := createJSONLSession(t, provider, store, projectDir)
	ns := projects.ProjectID(projectDir)

	// Archive → ok.
	resp, body := archiveCall("POST", base.URL, id)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("archive: %v %v", resp, body)
	}
	if archived, _ := store.IsArchived(ns, id); !archived {
		t.Fatal("marker missing after archive")
	}

	// The archived session refuses to open: 409 with the
	// session_archived code — the CODE, not just the status, is the
	// contract (session_locked is also 409; clients distinguish the
	// two only by this field).
	stateResp, err := http.Get(base.URL + "/api/s/" + id + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	stateBody := map[string]string{}
	_ = json.NewDecoder(stateResp.Body).Decode(&stateBody)
	stateResp.Body.Close()
	if stateResp.StatusCode != http.StatusConflict {
		t.Fatalf("state on archived: %d, want 409", stateResp.StatusCode)
	}
	if stateBody["error"] != "session_archived" {
		t.Fatalf("code on archived = %q, want session_archived", stateBody["error"])
	}

	// The project listing hides it (the default view).
	projectsResp, err := http.Get(base.URL + "/api/projects") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var projBody struct {
		Projects []struct {
			Sessions []struct {
				ID string `json:"id"`
			} `json:"sessions"`
		} `json:"projects"`
	}
	_ = json.NewDecoder(projectsResp.Body).Decode(&projBody)
	projectsResp.Body.Close()
	// The listing is cached (TTL); the archived row disappears once
	// the cache expires.
	waitFor(t, 2*time.Second, func() bool {
		projectsResp, err := http.Get(base.URL + "/api/projects") //nolint:noctx // test
		if err != nil {
			return false
		}
		var body struct {
			Projects []struct {
				Sessions []struct {
					ID string `json:"id"`
				} `json:"sessions"`
			} `json:"projects"`
		}
		_ = json.NewDecoder(projectsResp.Body).Decode(&body)
		projectsResp.Body.Close()
		for _, p := range body.Projects {
			for _, s := range p.Sessions {
				if s.ID == id {
					return false
				}
			}
		}
		return true
	})

	// Unarchive → ok, the session opens again.
	resp, body = archiveCall("DELETE", base.URL, id)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("unarchive: %v %v", resp, body)
	}
	stateResp, err = http.Get(base.URL + "/api/s/" + id + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	stateResp.Body.Close()
	if stateResp.StatusCode != http.StatusOK {
		t.Fatalf("state after unarchive: %d, want 200", stateResp.StatusCode)
	}
}

func TestArchiveEndpointUnknownSession(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	_, _, base, _ := newJSONLTestServer(t, provider)

	resp, body := archiveCall("POST", base.URL, "s00000000000000000000000000")
	if resp == nil || resp.StatusCode != http.StatusNotFound || body["error"] != "unknown_session" {
		t.Fatalf("archive unknown: %v %v", resp, body)
	}
	resp, body = archiveCall("DELETE", base.URL, "s00000000000000000000000000")
	if resp == nil || resp.StatusCode != http.StatusNotFound || body["error"] != "unknown_session" {
		t.Fatalf("unarchive unknown: %v %v", resp, body)
	}
}

// TestArchiveEndpointLocked: an external process holding the session
// lock (a CLI mid-write) turns the archive into 409 session_locked.
func TestArchiveEndpointLocked(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, base, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)
	id := createJSONLSession(t, provider, store, projectDir)

	// A second store instance plays the external process.
	external, err := jsonl.OpenStore(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	held, err := external.Open(projects.ProjectID(projectDir), id)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	resp, body := archiveCall("POST", base.URL, id)
	if resp == nil || resp.StatusCode != http.StatusConflict || body["error"] != "session_locked" {
		t.Fatalf("archive under lock: %v %v", resp, body)
	}
}

// TestArchiveEndpointRefusals: the shared Delete refusals carry over —
// watched (SSE subscriber, no refs) → session_open; running →
// run_active.
func TestArchiveEndpointRefusals(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, base, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)

	// Watched: session_open.
	watched := createJSONLSession(t, provider, store, projectDir)
	stream, err := http.Get(base.URL + "/api/s/" + watched + "/stream") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	waitFor(t, 2*time.Second, func() bool {
		hub := manager.hubOf(watched)
		return hub != nil && hub.Subscribers() > 0
	})
	resp, body := archiveCall("POST", base.URL, watched)
	if resp == nil || resp.StatusCode != http.StatusConflict || body["error"] != "session_open" {
		t.Fatalf("archive watched: %v %v", resp, body)
	}
	stream.Body.Close()

	// Running: run_active.
	running := createJSONLSession(t, provider, store, projectDir)
	if resp, err := http.Post(base.URL+"/api/s/"+running+"/send", "application/json", strings.NewReader(`{"text":"go"}`)); err != nil || resp.StatusCode != 200 { //nolint:noctx // test
		t.Fatalf("send: %v %v", err, resp)
	} else {
		resp.Body.Close()
	}
	resp, body = archiveCall("POST", base.URL, running)
	if resp == nil || resp.StatusCode != http.StatusConflict || body["error"] != "run_active" {
		t.Fatalf("archive running: %v %v", resp, body)
	}
	// End the run before cleanup: pool.Close tears the running session
	// down and waits for it (TestDeleteSession's pattern).
	provider.release()
}
