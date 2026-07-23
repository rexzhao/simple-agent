package webapp

import (
	"net/http"
	"strconv"
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
