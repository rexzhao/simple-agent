package webapp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

type uploadedImageResult struct {
	Hash      string `json:"hash"`
	MediaType string `json:"media_type"`
	SizeBytes int64  `json:"size_bytes"`
}

// handleUploadSessionImage is the bounded binary data plane for composer
// attachments. The resulting content-addressed reference is small enough for
// run.start; raw image bytes never enter a WebSocket command frame.
func (s *Server) handleUploadSessionImage(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("sessionID"))
	if s == nil || s.service == nil || s.service.SessionStore() == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "session_unavailable", "session storage is unavailable")
		return
	}
	mediaType, supported := model.NormalizeImageMediaType(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	if !supported {
		writeAPIError(w, http.StatusUnsupportedMediaType, "invalid_image_attachment", "unsupported image media type")
		return
	}
	if r.ContentLength > model.MaxImageInputBytes {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "invalid_image_attachment", "image exceeds the 4 MiB limit")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, model.MaxImageInputBytes+1))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_image_attachment", "could not read image attachment")
		return
	}
	if len(raw) == 0 || len(raw) > model.MaxImageInputBytes || !model.ImageBytesMatchMediaType(mediaType, raw) {
		writeAPIError(w, http.StatusBadRequest, "invalid_image_attachment", "image data does not match its media type or size limit")
		return
	}
	ref, err := s.service.StoreSessionImage(r.Context(), sessionID, mediaType, raw)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, uploadedImageResult{Hash: ref.Hash, MediaType: ref.MediaType, SizeBytes: ref.SizeBytes})
}

func (s *Server) handleSessionImage(w http.ResponseWriter, r *http.Request) {
	image, err := s.service.ReadSessionImage(r.PathValue("sessionID"), r.PathValue("hash"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	w.Header().Set("Content-Type", image.MediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(image.Data)))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(image.Data)
}

// writeServiceError is intentionally kept local to the remaining session image
// read boundary. Project, session, provider, and run mutations no longer have
// HTTP adapters; Blob store errors are mapped by blobstore.ServeHTTP.
func writeServiceError(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	code := "request_failed"
	switch {
	case errors.Is(err, projectstore.ErrNotFound), errors.Is(err, sessions.ErrNotFound):
		status = http.StatusNotFound
		code = "not_found"
	case errors.Is(err, context.Canceled):
		status = http.StatusRequestTimeout
		code = "cancelled"
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		message = "request failed"
	}
	writeAPIError(w, status, code, message)
}
