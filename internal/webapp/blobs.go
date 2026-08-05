package webapp

import "net/http"

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.blobStore == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.blobStore.ServeHTTP(w, r, r.PathValue("blobID"))
}
