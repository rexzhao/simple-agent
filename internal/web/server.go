package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/rexzhao/simple-agent/internal/agent"
)

//go:embed static
var staticFiles embed.FS

// chatLanding is the /chat page when no client is served: the chat
// client lives outside the binary (clients/chat, its own build chain)
// and is mounted with WithStaticDir / the -http-static flag.
const chatLanding = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>sai chat</title>
<style>
:root { color-scheme: dark; }
body { background: #16181d; color: #d7dae0; font: 14px/1.6 ui-monospace, monospace;
       display: grid; place-items: center; min-height: 100vh; margin: 0; }
main { max-width: 34em; padding: 2em; }
a { color: #8ab4f8; }
code { background: #22252c; padding: 0 2px; }
</style>
</head>
<body>
<main>
  <p>the chat client is not being served.</p>
  <p>build it and mount it:
     <code>cd clients/chat &amp;&amp; npm install &amp;&amp; npm run build</code>, then start with
     <code>-http-static clients/chat/dist</code>.</p>
  <p><a href="/log">open the log view</a> instead.</p>
</main>
</body>
</html>
`

// Server is the web UI: the manager (registered projects, lazily opened
// sessions) behind the session-scoped routes (design:
// docs/multi-session.md §4).
type Server struct {
	manager *Manager
	mux     *http.ServeMux
	opts    serverOptions
}

// serverOptions carries the Server configuration set through
// ServerOption values.
type serverOptions struct {
	staticDir string
}

// ServerOption configures a Server.
type ServerOption func(*serverOptions)

// WithStaticDir serves a chat client directory (the clients/chat build
// output, built with Vite base /chat/) at /chat, which is the default
// landing. The embedded log view stays at /log either way.
func WithStaticDir(dir string) ServerOption {
	return func(o *serverOptions) { o.staticDir = dir }
}

// New builds the server over a manager.
func New(manager *Manager, opts ...ServerOption) *Server {
	s := &Server{manager: manager}
	for _, opt := range opts {
		opt(&s.opts)
	}
	// Windows derives MIME types from the registry, where .js/.mjs map
	// to text/plain (or nothing) — module scripts enforce strict MIME
	// checking and refuse to load. Pin the types for the assets we ship.
	_ = mime.AddExtensionType(".js", "text/javascript")
	_ = mime.AddExtensionType(".mjs", "text/javascript")
	_ = mime.AddExtensionType(".css", "text/css")
	_ = mime.AddExtensionType(".html", "text/html; charset=utf-8")
	mux := http.NewServeMux()
	// Top-level routing: / is the chat client (default landing), /log is
	// the embedded zero-dependency log view, /api/... is the wire
	// contract. The chat client is an external build (clients/chat) and
	// is mounted with WithStaticDir; without it /chat explains how.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/chat", http.StatusFound)
	})
	// The embedded log tree lives under static/; strip the prefix so
	// /log serves index.html and the assets sit at /log/app.js etc.
	page, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic("web: embedded static files: " + err.Error())
	}
	// Exact + subtree patterns both point at the same handler: the
	// subtree form is what serves nested assets, the exact form avoids
	// a redirect hop for the bare path.
	mux.Handle("/log/", http.StripPrefix("/log", http.FileServer(http.FS(page))))
	mux.Handle("/log", http.StripPrefix("/log", http.FileServer(http.FS(page))))
	chat := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Landing doubles as the mount-point instructions.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(chatLanding))
	}))
	if s.opts.staticDir != "" {
		chat = http.StripPrefix("/chat", http.FileServer(http.Dir(s.opts.staticDir)))
	}
	mux.Handle("/chat/", chat)
	mux.Handle("/chat", chat)
	mux.HandleFunc("GET /api/projects", manager.Projects)
	mux.HandleFunc("GET /api/s/{id}/stream", s.handleStream)
	mux.HandleFunc("GET /api/s/{id}/state", s.handleState)
	mux.HandleFunc("GET /api/s/{id}/history", s.handleHistory)
	mux.HandleFunc("POST /api/s/{id}/send", s.handleSend)
	mux.HandleFunc("POST /api/s/{id}/steer", s.handleSteer)
	mux.HandleFunc("PUT /api/s/{id}/steer", s.handleSteerUpdate)
	mux.HandleFunc("DELETE /api/s/{id}/steer", s.handleSteerWithdraw)
	mux.HandleFunc("DELETE /api/s/{id}", manager.DeleteSession)
	mux.HandleFunc("POST /api/s/{id}/compact", s.handleCompact)
	mux.HandleFunc("POST /api/s/{id}/stop", s.handleStop)
	// Unmatched /api paths answer with a JSON 404, never the page
	// redirect: API consumers must not be sent wandering (a 3xx on an
	// unknown route reads as "route exists" to a naive client).
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "", "message": "unknown api route"})
	})
	s.mux = mux
	return s
}

// Handler returns the root handler. The mux is wrapped in CORS
// support for loopback http origins — the dev workflow of the chat
// client (a Vite server on another loopback port) needs to read our
// responses and to preflight its JSON POSTs. This is a deliberate
// trust extension, not a weakening: the server is loopback-only and
// unauthenticated, so any local process can already call it directly;
// admitting loopback-ORIGIN pages grants them no power an attacker
// with local code execution doesn't already have. Non-loopback
// origins get no CORS support and fail guardAPI exactly as before.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" &&
			strings.HasPrefix(r.URL.Path, "/api/") && loopbackOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				// The mux registers no OPTIONS patterns (its method
				// patterns would not match), so preflights must be
				// answered before routing.
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
				w.Header().Set("Access-Control-Max-Age", "86400")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		s.mux.ServeHTTP(w, r)
	})
}

// loopbackOrigin reports whether an Origin header value names a plain
// http:// loopback origin (dev servers are http; https origins stay
// rejected by guardAPI).
func loopbackOrigin(origin string) bool {
	u, ok := strings.CutPrefix(origin, "http://")
	if !ok {
		return false
	}
	return isLoopbackHost(u)
}

// Close stops the manager (sweeper, hubs, sessions, root lock).
func (s *Server) Close() { s.manager.Close() }

// isLoopbackHost reports whether a Host header value (host or host:port)
// names this machine only.
func isLoopbackHost(hostPort string) bool {
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		host = hostPort
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// guardAPI rejects cross-site requests. The write endpoints trigger
// shell execution: a browser page anywhere on the internet can fire a
// simple cross-site POST (text/plain needs no CORS preflight), so the
// checks are (1) mutations must be JSON content-type — a preflighted
// type that our zero-CORS policy never allows — and (2) any Origin, and
// the Host, must be loopback. GETs check Origin too when present.
func guardAPI(w http.ResponseWriter, r *http.Request, write bool) bool {
	if !isLoopbackHost(r.Host) {
		http.Error(w, "non-loopback host", http.StatusForbidden)
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, ok := strings.CutPrefix(origin, "http://")
		if !ok {
			http.Error(w, "bad origin", http.StatusForbidden)
			return false
		}
		if !isLoopbackHost(u) {
			http.Error(w, "cross-origin request", http.StatusForbidden)
			return false
		}
	}
	// Only POST needs the content-type bar: browsers treat PUT and DELETE
	// as preflighted methods regardless (a cross-site request never gets
	// past the OPTIONS preflight our zero-CORS policy refuses), but a
	// simple cross-site POST with text/plain does not preflight.
	if write && r.Method == http.MethodPost {
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if !guardAPI(w, r, false) {
		return
	}
	entry, release, ok := s.manager.resolve(w, r)
	if !ok {
		return
	}
	defer release()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	client, snapshot, err := entry.hub.register(r.URL.Query().Get("from"), windowParam(r))
	if err != nil {
		http.Error(w, "register: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer entry.hub.unregister(client)

	writeFrame := func(frame []byte) {
		fmt.Fprintf(w, "data: %s\n\n", frame)
		flusher.Flush()
	}
	writeFrame(snapshot)

	for {
		select {
		case frame := <-client.ch:
			writeFrame(frame)
		case <-client.done:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func windowParam(r *http.Request) int {
	if v := r.URL.Query().Get("window"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= DefaultWindow {
			return n
		}
	}
	return DefaultWindow
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if !guardAPI(w, r, false) {
		return
	}
	entry, release, ok := s.manager.resolve(w, r)
	if !ok {
		return
	}
	defer release()
	writeJSON(w, http.StatusOK, entry.hub.state())
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if !guardAPI(w, r, false) {
		return
	}
	entry, release, ok := s.manager.resolve(w, r)
	if !ok {
		return
	}
	defer release()

	before := r.URL.Query().Get("before")
	if before == "" {
		http.Error(w, "missing before", http.StatusBadRequest)
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	records, anchor, more, stale := entry.hub.history(before, limit)
	if stale {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "stale_anchor"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"records": records,
		"anchor":  anchor,
		"more":    more,
	})
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if !guardAPI(w, r, true) {
		return
	}
	entry, release, ok := s.manager.resolve(w, r)
	if !ok {
		return
	}
	defer release()

	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty_input"})
		return
	}
	if err := entry.session.Send(body.Text); err != nil {
		if errors.Is(err, agent.ErrRunActive) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "run_active"})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The events channel is republished under the session lock inside
	// Send; the wake is sent after Send returns, so the hub reattaches
	// with zero latency (the channel is small — late attachment could
	// stall the run goroutine).
	entry.hub.Wake()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSteer(w http.ResponseWriter, r *http.Request) {
	if !guardAPI(w, r, true) {
		return
	}
	entry, release, ok := s.manager.resolve(w, r)
	if !ok {
		return
	}
	defer release()

	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty_input"})
		return
	}
	item, err := entry.session.Steer(body.Text)
	if err != nil {
		if errors.Is(err, agent.ErrNoRun) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "no_run"})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"item": item})
}

func (s *Server) handleSteerUpdate(w http.ResponseWriter, r *http.Request) {
	if !guardAPI(w, r, true) {
		return
	}
	entry, release, ok := s.manager.resolve(w, r)
	if !ok {
		return
	}
	defer release()

	var body struct {
		Item string `json:"item"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := entry.session.UpdateSteer(body.Item, body.Text); err != nil {
		if errors.Is(err, agent.ErrNotQueued) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_queued"})
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSteerWithdraw(w http.ResponseWriter, r *http.Request) {
	if !guardAPI(w, r, true) {
		return
	}
	entry, release, ok := s.manager.resolve(w, r)
	if !ok {
		return
	}
	defer release()

	item := r.URL.Query().Get("item")
	if item == "" {
		http.Error(w, "missing item", http.StatusBadRequest)
		return
	}
	if err := entry.session.WithdrawSteer(item); err != nil {
		if errors.Is(err, agent.ErrNotQueued) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_queued"})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
	if !guardAPI(w, r, true) {
		return
	}
	entry, release, ok := s.manager.resolve(w, r)
	if !ok {
		return
	}
	defer release()

	// Synchronous pre-checks where the answer is decidable now. They are
	// advisory (TOCTOU): the authoritative single-flight check is inside
	// Compact; a race that slips past these surfaces through the
	// out-of-band notice instead of the response.
	snapshot := entry.session.Snapshot()
	if snapshot.Compacting != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "compacting"})
		return
	}
	if entry.session.Running() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run_active"})
		return
	}
	if len(snapshot.Context) == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "nothing_to_compact"})
		return
	}
	hub, session := entry.hub, entry.session
	// Compact holds the single-flight slot for the whole summary (tens
	// of seconds); the response must not.
	go func() {
		if err := session.Compact(); err != nil && !errors.Is(err, agent.ErrNothingToCompact) {
			hub.Notice("compact failed: " + err.Error())
			return
		}
		hub.Notice("")
	}()
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if !guardAPI(w, r, true) {
		return
	}
	entry, release, ok := s.manager.resolve(w, r)
	if !ok {
		return
	}
	defer release()

	// Idempotent: stopping with no active run is a no-op.
	entry.session.Stop()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
