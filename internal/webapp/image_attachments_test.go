package webapp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/model"
)

type imageCaptureWebTestRunner struct {
	requests chan execution.SessionTurnRequest
}

func (r imageCaptureWebTestRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (r imageCaptureWebTestRunner) RunSessionTurn(_ context.Context, request execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	r.requests <- request
	if err := request.Publisher.Publish(eventbus.AssistantReady{
		TurnID:  request.TurnID,
		Message: model.Message{Role: model.MessageRoleAssistant, Content: "image received"},
	}); err != nil {
		return execution.SessionTurnResult{}, err
	}
	return execution.SessionTurnResult{Incremental: true}, nil
}

func TestStartRunAcceptsPersistsAndServesImageAttachment(t *testing.T) {
	runner := imageCaptureWebTestRunner{requests: make(chan execution.SessionTurnRequest, 1)}
	server, service := newWebTestServerWithRunner(t, runner)
	projectResponse := doJSONRequest(t, http.MethodPost, server.URL+"/api/projects", map[string]string{"root": t.TempDir()})
	var project execution.ProjectCreateResult
	decodeResponse(t, projectResponse, &project)
	sessionResponse := doJSONRequest(t, http.MethodPost, server.URL+"/api/projects/"+project.Project.ID+"/sessions", map[string]string{})
	var session execution.SessionDetail
	decodeResponse(t, sessionResponse, &session)

	raw := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	imageURL := model.ImageDataURL("image/png", raw)
	response := doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/"+session.ID+"/runs", startRunRequest{
		Content: "Describe this image",
		Images:  []startRunImageAttachment{{DataURL: imageURL}},
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("POST run status = %d body=%s", response.StatusCode, readBody(response))
	}
	response.Body.Close()

	select {
	case request := <-runner.requests:
		if request.Content != "Describe this image" || len(request.ContentBlocks) != 1 {
			t.Fatalf("runner request = %#v", request)
		}
		block := request.ContentBlocks[0]
		if block.Type != "input_image" || block.ImageURL != imageURL || block.Detail != "auto" {
			t.Fatalf("runner image block = %#v", block)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not receive image attachment")
	}

	var page execution.SessionItemsPage
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		page, err = service.GetSessionChatItems(session.ID)
		if err == nil && len(page.Items) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(page.Items) != 2 {
		t.Fatalf("persisted items = %#v, want user and assistant messages", page.Items)
	}
	images := page.Items[0].Message.Images
	if len(images) != 1 || images[0].MediaType != "image/png" || images[0].SizeBytes != int64(len(raw)) || images[0].Hash == "" {
		t.Fatalf("persisted image metadata = %#v", images)
	}

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/sessions/"+session.ID+"/images/"+images[0].Hash, nil)
	if err != nil {
		t.Fatalf("NewRequest(image) error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	imageResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET image error = %v", err)
	}
	defer imageResponse.Body.Close()
	if imageResponse.StatusCode != http.StatusOK || imageResponse.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("GET image status/content type = %d/%q", imageResponse.StatusCode, imageResponse.Header.Get("Content-Type"))
	}
	got, err := io.ReadAll(imageResponse.Body)
	if err != nil {
		t.Fatalf("ReadAll(image) error = %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("GET image body = %x, want %x", got, raw)
	}
}

func TestStartRunRequestRejectsInvalidImages(t *testing.T) {
	validPNG := model.ImageDataURL("image/png", []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	tests := []startRunRequest{
		{Images: []startRunImageAttachment{{DataURL: "https://example.com/image.png"}}},
		{Images: []startRunImageAttachment{{DataURL: model.ImageDataURL("image/bmp", []byte{'B', 'M'})}}},
		{Images: []startRunImageAttachment{{DataURL: model.ImageDataURL("image/png", []byte("not a png"))}}},
		{Images: []startRunImageAttachment{{DataURL: validPNG, Detail: "full"}}},
	}
	for _, request := range tests {
		if _, err := request.messageInput(); err == nil {
			t.Errorf("messageInput(%#v) error = nil, want error", request)
		}
	}
}

func TestStartRunRejectsImageWhenModelDoesNotDeclareImageInput(t *testing.T) {
	runner := imageCaptureWebTestRunner{requests: make(chan execution.SessionTurnRequest, 1)}
	server, _ := newWebTestServerWithRunner(t, runner)
	projectResponse := doJSONRequest(t, http.MethodPost, server.URL+"/api/projects", map[string]string{"root": t.TempDir()})
	var project execution.ProjectCreateResult
	decodeResponse(t, projectResponse, &project)
	sessionResponse := doJSONRequest(t, http.MethodPost, server.URL+"/api/projects/"+project.Project.ID+"/sessions", map[string]string{
		"provider":      "fake",
		"model_profile": "precise",
	})
	var session execution.SessionDetail
	decodeResponse(t, sessionResponse, &session)

	imageURL := model.ImageDataURL("image/png", []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	response := doJSONRequest(t, http.MethodPost, server.URL+"/api/sessions/"+session.ID+"/runs", startRunRequest{
		Images: []startRunImageAttachment{{DataURL: imageURL}},
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST run status = %d body=%s", response.StatusCode, readBody(response))
	}

	select {
	case <-runner.requests:
		t.Fatal("runner received unsupported image input")
	default:
	}
}

var _ execution.SessionIncrementalSupporter = imageCaptureWebTestRunner{}
var _ execution.SessionTurnRunner = imageCaptureWebTestRunner{}
