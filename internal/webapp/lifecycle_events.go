package webapp

import (
	"fmt"
	"net/http"
	"time"

	"github.com/rexzhao/simple-agent/internal/execution"
)

const lifecycleSSEHeartbeat = 15 * time.Second

// handleLifecycleEvents serves the process-wide durable lifecycle stream. It
// deliberately only subscribes to execution lifecycle events; transient
// text/reasoning/tool deltas remain on the per-run stream.
func (s *Server) handleLifecycleEvents(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.service == nil || s.service.LifecycleHub() == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "events_unavailable", "lifecycle events are unavailable")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "stream_unsupported", "streaming is unavailable")
		return
	}

	subscription := s.service.LifecycleHub().Subscribe()
	defer subscription.Close()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, ": connected\nretry: 3000\n\n"); err != nil {
		return
	}
	flusher.Flush()

	heartbeat := time.NewTicker(lifecycleSSEHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case event, open := <-subscription.Events():
			if !open {
				// The hub closes a slow subscription. The client should reconnect
				// and bootstrap rather than receiving an incomplete stream.
				return
			}
			if err := writeLifecycleSSE(w, event); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func writeLifecycleSSE(w interface{ Write([]byte) (int, error) }, event execution.LifecycleEvent) error {
	if event.Type == "" || len(event.Payload) == 0 {
		return nil
	}
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, event.Payload)
	return err
}
