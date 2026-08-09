package webapp

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

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
