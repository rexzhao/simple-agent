package webapp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/blobstore"
	"github.com/rexzhao/simple-agent/internal/codexlogin"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/projectindex"
	"github.com/rexzhao/simple-agent/internal/providersettings"
	"github.com/rexzhao/simple-agent/internal/sessioncontent"
	"github.com/rexzhao/simple-agent/internal/sessionindex"
	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/syncengine"
	"github.com/rexzhao/simple-agent/internal/webdebug"
	"github.com/rexzhao/simple-agent/internal/wsgateway"
)

var Version = "dev"

type ServerOptions struct {
	Context   context.Context
	Service   *execution.Service
	Token     string
	CWD       string
	LogWriter io.Writer

	// AllowNonLoopback relaxes the loopback-only Host header restriction. It
	// must only be set when the listener is intentionally bound to a
	// non-loopback interface; the bearer token and per-request Origin checks
	// remain enforced.
	AllowNonLoopback bool

	// The gateway and ticket store hooks keep B1 independently testable. The
	// production defaults use the secure bounded implementations.
	WebSocketGateway     *wsgateway.Gateway
	WSTicketStore        *wsgateway.TicketStore
	WSHandler            wsgateway.Handler
	WSObserver           wsgateway.Observer
	SessionIndexObserver sessionindex.Observer
	ProjectIndexObserver projectindex.Observer
	BlobStore            *blobstore.Store
}

type Server struct {
	service                      *execution.Service
	token                        string
	cwd                          string
	allowNonLoopback             bool
	ctx                          context.Context
	cancel                       context.CancelFunc
	mux                          *http.ServeMux
	runs                         *runRegistry
	codexLogins                  *codexLoginRegistry
	wsTickets                    *wsgateway.TicketStore
	wsGateway                    *wsgateway.Gateway
	wsDispatcher                 *wsgateway.Dispatcher
	webDebugBroker               *webdebug.Broker
	webEvalRegistration          *execution.WebEvalExecutorRegistration
	webEvalEnabled               bool
	projectIndex                 *projectindex.Provider
	providerSettings             *providersettings.Provider
	codexLogin                   *codexlogin.Provider
	sessionIndex                 *sessionindex.Provider
	sessionContent               *sessioncontent.Provider
	blobStore                    *blobstore.Store
	blobStoreOwned               bool
	sinkRegistration             *execution.SessionIndexSinkRegistration
	projectRegistration          *execution.ProjectIndexSinkRegistration
	providerSettingsRegistration *execution.ProviderSettingsSinkRegistration
	contentRegistration          *sessions.MutationSinkRegistration
	contentRunObserver           func()
}

func NewServer(options ServerOptions) (*Server, error) {
	if options.Service == nil {
		return nil, fmt.Errorf("web execution service is required")
	}
	if strings.TrimSpace(options.Token) == "" {
		return nil, fmt.Errorf("web capability token is required")
	}
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	ticketStore := options.WSTicketStore
	if ticketStore == nil {
		var err error
		ticketStore, err = wsgateway.NewTicketStore(wsgateway.TicketStoreOptions{})
		if err != nil {
			cancel()
			return nil, err
		}
	}
	blobStore := options.BlobStore
	blobStoreOwned := blobStore == nil
	if blobStore == nil {
		var err error
		blobStore, err = blobstore.New(blobstore.Options{BaseURL: "/api/blobs/"})
		if err != nil {
			cancel()
			return nil, err
		}
	}
	gateway := options.WebSocketGateway
	var dispatcher *wsgateway.Dispatcher
	var debugBroker *webdebug.Broker
	var webEvalRegistration *execution.WebEvalExecutorRegistration
	var projectIndexProvider *projectindex.Provider
	var sessionIndexProvider *sessionindex.Provider
	var projectRegistration *execution.ProjectIndexSinkRegistration
	var providerSettingsProvider *providersettings.Provider
	var providerSettingsRegistration *execution.ProviderSettingsSinkRegistration
	var codexLoginProvider *codexlogin.Provider
	var sessionContentProvider *sessioncontent.Provider
	var sinkRegistration *execution.SessionIndexSinkRegistration
	var contentRegistration *sessions.MutationSinkRegistration
	// The run registry is created before the command registry so run.cancel can
	// use the same process-local active-run owner as the typed command. It is
	// deliberately not cross-epoch safe.
	runs := newRunRegistry(ctx, options.Service, options.LogWriter)
	codexLogins := newCodexLoginRegistry(ctx, options.Service)
	cleanupAssembly := func() {
		if contentRegistration != nil {
			contentRegistration.Unregister()
		}
		if sinkRegistration != nil {
			sinkRegistration.Unregister()
		}
		if projectRegistration != nil {
			projectRegistration.Unregister()
		}
		if providerSettingsRegistration != nil {
			providerSettingsRegistration.Unregister()
		}
		if webEvalRegistration != nil {
			webEvalRegistration.Unregister()
		}
		if debugBroker != nil {
			debugBroker.Close()
		}
		if dispatcher != nil {
			dispatcher.Close()
		}
		if sessionIndexProvider != nil {
			sessionIndexProvider.Close()
		}
		if projectIndexProvider != nil {
			projectIndexProvider.Close()
		}
		if providerSettingsProvider != nil {
			providerSettingsProvider.Close()
		}
		if codexLoginProvider != nil {
			codexLoginProvider.Close()
		}
		if sessionContentProvider != nil {
			sessionContentProvider.Close()
		}
		if runs != nil {
			runs.Close()
		}
		if blobStoreOwned && blobStore != nil {
			blobStore.Close()
		}
		cancel()
	}
	baseConfig, err := loadWebDebugConfig(options.Service.ConfigPath())
	if err != nil {
		cleanupAssembly()
		return nil, fmt.Errorf("load web server config: %w", err)
	}
	debugBroker, err = webdebug.NewBroker(webdebug.Options{
		Enabled: baseConfig.WebEvalEnabled,
		Eligibility: func(_ context.Context, sessionID string) error {
			store := options.Service.SessionStore()
			if store == nil {
				return webdebug.ErrSessionUnavailable
			}
			session, err := store.LoadState(sessionID)
			if errors.Is(err, sessions.ErrNotFound) {
				return webdebug.ErrSessionNotFound
			}
			if err != nil {
				return webdebug.ErrSessionUnavailable
			}
			if session.ProjectID != webdebug.TargetProjectID {
				return webdebug.ErrProjectMismatch
			}
			return nil
		},
	})
	if err != nil {
		cleanupAssembly()
		return nil, err
	}
	if gateway == nil {
		if options.WSHandler != nil {
			var err error
			gateway, err = wsgateway.New(wsgateway.Options{
				Handler:  options.WSHandler,
				Observer: options.WSObserver,
			})
			if err != nil {
				cleanupAssembly()
				return nil, err
			}
		} else {
			serverEpoch, err := newServerEpoch()
			if err != nil {
				cleanupAssembly()
				return nil, err
			}
			sessionIndexProvider, err = sessionindex.NewProvider(options.Service.SessionStore(), sessionindex.ProviderOptions{
				StreamEpoch:  serverEpoch,
				OwnerContext: ctx,
				Observer:     options.SessionIndexObserver,
				BlobWriter:   blobStore,
			})
			if err != nil {
				cleanupAssembly()
				return nil, err
			}
			sinkRegistration = options.Service.RegisterSessionIndexChangeSink(sessionIndexProvider)
			if sinkRegistration == nil {
				cleanupAssembly()
				return nil, fmt.Errorf("session index sink registration failed")
			}
			if err := sessionIndexProvider.Warm(ctx); err != nil {
				cleanupAssembly()
				return nil, fmt.Errorf("warm session index provider: %w", err)
			}
			projectIndexProvider, err = projectindex.NewProvider(options.Service.ProjectStore(), projectindex.ProviderOptions{
				StreamEpoch:  serverEpoch,
				OwnerContext: ctx,
				Observer:     options.ProjectIndexObserver,
				BlobWriter:   blobStore,
			})
			if err != nil {
				cleanupAssembly()
				return nil, err
			}
			projectRegistration = options.Service.RegisterProjectIndexChangeSink(projectIndexProvider)
			if projectRegistration == nil {
				cleanupAssembly()
				return nil, fmt.Errorf("project index sink registration failed")
			}
			if err := projectIndexProvider.Warm(ctx); err != nil {
				cleanupAssembly()
				return nil, fmt.Errorf("warm project index provider: %w", err)
			}
			providerSettingsProvider, err = providersettings.NewProvider(providersettings.ProviderOptions{
				ConfigPath: options.Service.ConfigPath(), ServerRoot: options.Service.ServerRoot(),
				StreamEpoch: serverEpoch, OwnerContext: ctx, BlobWriter: blobStore,
				MaxChangeMessageBytes: wsgateway.DefaultMaxMessageBytes,
			})
			if err != nil {
				cleanupAssembly()
				return nil, err
			}
			providerSettingsRegistration = options.Service.RegisterProviderSettingsChangeSink(providerSettingsProvider)
			if providerSettingsRegistration == nil {
				cleanupAssembly()
				return nil, fmt.Errorf("provider settings sink registration failed")
			}
			if err := providerSettingsProvider.Warm(ctx); err != nil {
				cleanupAssembly()
				return nil, fmt.Errorf("warm provider settings provider: %w", err)
			}
			sessionContentProvider, err = sessioncontent.NewProvider(options.Service.SessionStore(), sessioncontent.ProviderOptions{
				StreamEpoch:           serverEpoch,
				OwnerContext:          ctx,
				BlobWriter:            blobStore,
				MaxChangeMessageBytes: wsgateway.DefaultMaxMessageBytes,
			})
			if err != nil {
				cleanupAssembly()
				return nil, err
			}
			contentRegistration = options.Service.SessionStore().RegisterMutationSink(sessionContentProvider)
			if contentRegistration == nil {
				cleanupAssembly()
				return nil, fmt.Errorf("session content mutation registration failed")
			}
			if err := sessionContentProvider.Warm(ctx); err != nil {
				cleanupAssembly()
				return nil, fmt.Errorf("warm session content provider: %w", err)
			}
			codexLoginProvider, err = codexlogin.NewProvider(codexlogin.ProviderOptions{
				StreamEpoch: serverEpoch, OwnerContext: ctx,
				ValidateProvider: func(providerName string) error {
					err := options.Service.ValidateCodexProvider(providerName)
					switch {
					case errors.Is(err, execution.ErrCodexProviderNotFound):
						return codexlogin.ErrProviderNotFound
					case errors.Is(err, execution.ErrCodexProviderNotCodex), errors.Is(err, execution.ErrCodexProviderNoAuthFile):
						return codexlogin.ErrProviderNotCodex
					case err != nil:
						return codexlogin.ErrProviderUnavailable
					default:
						return nil
					}
				},
				Status:                codexLogins.status,
				MaxChangeMessageBytes: wsgateway.DefaultMaxMessageBytes,
			})
			if err != nil {
				cleanupAssembly()
				return nil, err
			}
			codexLogins.setSink(codexLoginProvider)
			providers := syncengine.NewProviderRegistry()
			if err := providers.Register(sessionIndexProvider); err != nil {
				cleanupAssembly()
				return nil, err
			}
			if err := providers.Register(projectIndexProvider); err != nil {
				cleanupAssembly()
				return nil, err
			}
			if err := providers.Register(providerSettingsProvider); err != nil {
				cleanupAssembly()
				return nil, err
			}
			if err := providers.Register(sessionContentProvider); err != nil {
				cleanupAssembly()
				return nil, err
			}
			if err := providers.Register(codexLoginProvider); err != nil {
				cleanupAssembly()
				return nil, err
			}
			engine, err := syncengine.NewEngine(providers)
			if err != nil {
				cleanupAssembly()
				return nil, err
			}
			commandRegistry, err := newSessionCommandRegistry(options.Service, runs, sessionCommandRegistryOptions{
				HistoryWriter: blobStore,
				CodexLogins:   codexLogins,
			})
			if err != nil {
				cleanupAssembly()
				return nil, err
			}
			dispatcher, err = wsgateway.NewDispatcher(wsgateway.DispatcherOptions{
				Engine: engine, Commands: commandRegistry, OwnerContext: ctx,
				Observer: options.WSObserver,
			})
			if err != nil {
				cleanupAssembly()
				return nil, err
			}
			gateway, err = wsgateway.New(wsgateway.Options{
				Handler: dispatcher, Observer: options.WSObserver, ServerEpoch: serverEpoch,
			})
			if err != nil {
				cleanupAssembly()
				return nil, err
			}
		}
	}
	if err := gateway.DecorateHandler(func(delegate wsgateway.Handler) wsgateway.Handler {
		if delegate == nil {
			delegate = options.WSHandler
		}
		return webdebug.NewHandler(debugBroker, delegate)
	}); err != nil {
		cleanupAssembly()
		return nil, err
	}
	if debugBroker.Enabled() {
		webEvalRegistration = options.Service.RegisterWebEvalExecutor(webEvalBrokerAdapter{broker: debugBroker})
		if webEvalRegistration == nil {
			cleanupAssembly()
			return nil, fmt.Errorf("web eval executor registration failed")
		}
	}
	server := &Server{
		service:                      options.Service,
		token:                        options.Token,
		cwd:                          options.CWD,
		allowNonLoopback:             options.AllowNonLoopback,
		ctx:                          ctx,
		cancel:                       cancel,
		mux:                          http.NewServeMux(),
		wsTickets:                    ticketStore,
		wsGateway:                    gateway,
		wsDispatcher:                 dispatcher,
		webDebugBroker:               debugBroker,
		webEvalRegistration:          webEvalRegistration,
		webEvalEnabled:               baseConfig.WebEvalEnabled,
		projectIndex:                 projectIndexProvider,
		providerSettings:             providerSettingsProvider,
		sessionIndex:                 sessionIndexProvider,
		sessionContent:               sessionContentProvider,
		blobStore:                    blobStore,
		blobStoreOwned:               blobStoreOwned,
		sinkRegistration:             sinkRegistration,
		projectRegistration:          projectRegistration,
		providerSettingsRegistration: providerSettingsRegistration,
		contentRegistration:          contentRegistration,
	}
	server.runs = runs
	if sessionContentProvider != nil && server.runs != nil && server.runs.coordinator != nil {
		server.contentRunObserver = server.runs.coordinator.RegisterRunEventObserver(sessionContentProvider)
	}
	server.codexLogins = codexLogins
	server.codexLogin = codexLoginProvider
	server.routes()
	return server, nil
}

func loadWebDebugConfig(configPath string) (config.DebugConfig, error) {
	if strings.TrimSpace(configPath) == "" {
		return config.DebugConfig{}, nil
	}
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		// Some in-process server tests intentionally assemble a service without
		// a root config. Preserve that pre-existing startup behavior while
		// remaining fail-closed for this high-risk capability.
		return config.DebugConfig{}, nil
	} else if err != nil {
		return config.DebugConfig{}, err
	}
	cfg, err := config.LoadBase(configPath)
	if err != nil {
		return config.DebugConfig{}, err
	}
	return cfg.Debug, nil
}

func newServerEpoch() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "server_" + hex.EncodeToString(raw[:]), nil
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w, s.webEvalEnabled && r.URL.Path != "/api" && !strings.HasPrefix(r.URL.Path, "/api/"))
		if !s.allowNonLoopback && !validLoopbackHost(r.Host) {
			writeAPIError(w, http.StatusForbidden, "invalid_host", "request host is not allowed")
			return
		}
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			if r.URL.Path != "/api/ws" && !s.authorized(r) {
				writeAPIError(w, http.StatusUnauthorized, "unauthorized", "valid capability token required")
				return
			}
			if r.URL.Path == "/api/ws" {
				if !validWebSocketOrigin(r) {
					writeAPIError(w, http.StatusForbidden, "invalid_origin", "request origin is not allowed")
					return
				}
			} else if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && origin != "http://"+r.Host {
				writeAPIError(w, http.StatusForbidden, "invalid_origin", "request origin is not allowed")
				return
			}
		}
		s.mux.ServeHTTP(w, r)
	})
}

func (s *Server) Close() {
	if s == nil {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.contentRunObserver != nil {
		s.contentRunObserver()
	}
	if s.webDebugBroker != nil {
		if s.webEvalRegistration != nil {
			s.webEvalRegistration.Unregister()
		}
		s.webDebugBroker.Close()
	}
	if s.wsDispatcher != nil {
		s.wsDispatcher.Close()
	}
	if s.sinkRegistration != nil {
		s.sinkRegistration.Unregister()
	}
	if s.projectRegistration != nil {
		s.projectRegistration.Unregister()
	}
	if s.providerSettingsRegistration != nil {
		s.providerSettingsRegistration.Unregister()
	}
	if s.contentRegistration != nil {
		s.contentRegistration.Unregister()
	}
	if s.sessionIndex != nil {
		s.sessionIndex.Close()
	}
	if s.projectIndex != nil {
		s.projectIndex.Close()
	}
	if s.providerSettings != nil {
		s.providerSettings.Close()
	}
	if s.codexLogin != nil {
		s.codexLogin.Close()
	}
	if s.sessionContent != nil {
		s.sessionContent.Close()
	}
	if s.blobStore != nil && s.blobStoreOwned {
		s.blobStore.Close()
	}
	if s.runs != nil {
		s.runs.Close()
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/bootstrap", s.handleBootstrap)
	s.mux.HandleFunc("POST /api/ws-ticket", s.handleWSTicket)
	s.mux.HandleFunc("GET /api/ws", s.handleWebSocket)
	// Session images are a read boundary for blobs referenced by durable
	// content. All project/session/provider/run control is carried by typed WS
	// commands/resources; it deliberately has no parallel REST surface.
	s.mux.HandleFunc("GET /api/blobs/{blobID}", s.handleBlob)
	s.mux.HandleFunc("GET /api/sessions/{sessionID}/images/{hash}", s.handleSessionImage)
	s.mux.Handle("/", s.staticHandler())
}

func (s *Server) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) == 1
}

func (s *Server) staticHandler() http.Handler {
	assets, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// API paths never participate in the SPA fallback. In particular, a
		// removed or misspelled route must remain an explicit API 404 rather
		// than returning index.html with a successful status.
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			writeAPIError(w, http.StatusNotFound, "not_found", "route not found")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		assetPath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if assetPath == "." || assetPath == "" {
			assetPath = "index.html"
		}
		if _, err := fs.Stat(assets, assetPath); err != nil {
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}

func (s *Server) handleWSTicket(w http.ResponseWriter, r *http.Request) {
	ticket, expiresAt, err := s.wsTickets.Issue(wsgateway.TicketClaims{Principal: "capability"})
	if err != nil {
		if errors.Is(err, wsgateway.ErrTicketStoreFull) {
			writeAPIError(w, http.StatusServiceUnavailable, "ticket_unavailable", "websocket ticket service is temporarily unavailable")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "ticket_unavailable", "websocket ticket service is unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":     ticket,
		"expires_at": expiresAt.UTC().Format(time.RFC3339Nano),
	})
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.wsTickets.Consume(r.URL.Query().Get("ticket"))
	if !ok {
		// Keep this response identical for missing, malformed, expired, and
		// replayed tickets. Never echo or log the URL credential.
		writeAPIError(w, http.StatusUnauthorized, "invalid_ticket", "valid websocket ticket required")
		return
	}
	s.wsGateway.HTTPHandler(s.ctx, w, r, claims)
}

func validWebSocketOrigin(r *http.Request) bool {
	if r == nil || strings.TrimSpace(r.Host) == "" {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	return origin != "" && origin == "http://"+r.Host
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     Version,
		"cwd":         s.cwd,
		"server_root": s.service.ServerRoot(),
		"config_path": s.service.ConfigPath(),
	})
}

func setSecurityHeaders(w http.ResponseWriter, allowUnsafeEval bool) {
	scriptSource := "'self'"
	if allowUnsafeEval {
		scriptSource += " 'unsafe-eval'"
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src "+scriptSource+"; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func validLoopbackHost(hostport string) bool {
	host := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}
