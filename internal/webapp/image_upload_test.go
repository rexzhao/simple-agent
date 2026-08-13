package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/model"
)

func TestSessionImageUploadPersistsValidatedContentAddressedBlob(t *testing.T) {
	server, service := newWebTestServer(t)
	_, session := createWebProjectAndSession(t, service)
	raw := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/"+session.ID+"/images", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "image/png")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", response.StatusCode, readBody(response))
	}
	var result uploadedImageResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if result.MediaType != "image/png" || result.SizeBytes != int64(len(raw)) || len(result.Hash) != 64 {
		t.Fatalf("upload result=%#v", result)
	}
	got, err := service.SessionStore().ReadBlobForSession(session.ID, model.BlobRef{Hash: result.Hash, SizeBytes: result.SizeBytes, Encoding: "binary", MediaType: result.MediaType})
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("stored blob=%x err=%v", got, err)
	}
}

type imageRunCapture struct {
	requests chan execution.SessionTurnRequest
}

func (r imageRunCapture) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}
func (r imageRunCapture) RunSessionTurn(_ context.Context, request execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	r.requests <- request
	return execution.SessionTurnResult{Incremental: true}, nil
}

func TestRunStartConsumesUploadedImageReference(t *testing.T) {
	runner := imageRunCapture{requests: make(chan execution.SessionTurnRequest, 1)}
	_, service, app := newWebTestAppServerWithRunner(t, runner)
	_, session := createWebProjectAndSession(t, service)
	raw := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	ref, err := service.StoreSessionImage(context.Background(), session.ID, "image/png", raw)
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]any{
		"session_id": session.ID, "run_id": "run-image-reference", "content": "describe",
		"images": []map[string]any{{"hash": ref.Hash, "media_type": ref.MediaType, "size_bytes": ref.SizeBytes}},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := newSessionCommandRegistry(service, app.runs, sessionCommandRegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := registry.Definition("run.start", 1)
	if err != nil {
		t.Fatal(err)
	}
	request := commands.CommandRequest{Name: "run.start", SchemaVersion: 1, RequestID: "request-image", Arguments: arguments}
	if err := registry.Validate(definition, arguments); err != nil {
		t.Fatal(err)
	}
	if _, err := definition.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	select {
	case captured := <-runner.requests:
		if captured.Content != "describe" || len(captured.ContentBlocks) != 1 {
			t.Fatalf("captured request=%#v", captured)
		}
		mediaType, got, err := model.ParseSupportedImageDataURL(captured.ContentBlocks[0].ImageURL)
		if err != nil || mediaType != "image/png" || !bytes.Equal(got, raw) {
			t.Fatalf("captured image=%s %x err=%v", mediaType, got, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("image run did not reach runner")
	}
}

func TestSessionImageUploadRejectsInvalidTypeSignatureAndSession(t *testing.T) {
	server, service := newWebTestServer(t)
	_, session := createWebProjectAndSession(t, service)
	for _, test := range []struct {
		sessionID, mediaType string
		raw                  []byte
		status               int
	}{
		{session.ID, "image/bmp", []byte{'B', 'M'}, http.StatusUnsupportedMediaType},
		{session.ID, "image/png", []byte("not png"), http.StatusBadRequest},
		{"missing-session", "image/png", []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, http.StatusNotFound},
	} {
		request, err := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/"+test.sessionID+"/images", bytes.NewReader(test.raw))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+testToken)
		request.Header.Set("Content-Type", test.mediaType)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != test.status {
			t.Errorf("upload %s status=%d body=%s", test.mediaType, response.StatusCode, readBody(response))
		} else {
			response.Body.Close()
		}
	}
}
