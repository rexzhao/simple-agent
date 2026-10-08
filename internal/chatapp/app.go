// Package chatapp is the minimal native application core: it loads the
// server-root configuration, builds a provider and tool registry, and drives
// the agent loop with the append-only sessionlog as the only durable sink.
//
// It deliberately does not use internal/execution: that layer exists for the
// Web UI's run coordination and will be rebuilt on sessionlog later. This
// package is the thin seam the native UI needs.
package chatapp

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessionlog"
	"github.com/rexzhao/simple-agent/internal/tools"
)

// Options configures App construction.
type Options struct {
	// ServerRoot is the configuration namespace root holding sai.yaml and
	// the sessions directory.
	ServerRoot string
	// CWD is the working directory tools operate in.
	CWD string
	// Provider and Model optionally override the config defaults. Model is
	// a model profile name within the provider.
	Provider string
	Model    string
}

// App is one running application instance bound to a server root.
type App struct {
	cfg            *config.Config
	resolved       config.ResolvedModel
	provider       model.Provider
	developerRole  model.MessageRole
	parameters     map[string]any
	outputLimit    int
	maxTurns       int
	cwd            string
	project        string
	systemPrompt   string
	toolRegistry *tools.Registry
	toolSchemas  []model.Tool
	store *sessionlog.Store
}

// New loads the server-root configuration and prepares the provider, tools,
// and session store. The config file must already exist with a usable default
// provider; provider credentials are resolved by config.Load.
func New(options Options) (*App, error) {
	if strings.TrimSpace(options.ServerRoot) == "" {
		return nil, fmt.Errorf("server root is required")
	}
	configPath := filepath.Join(options.ServerRoot, "sai.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", configPath, err)
	}
	resolved, err := cfg.ResolveModel(options.Provider, options.Model)
	if err != nil {
		return nil, err
	}
	provider, err := newProvider(resolved)
	if err != nil {
		return nil, err
	}
	cwd, err := resolveCWD(options.CWD)
	if err != nil {
		return nil, err
	}

	parameters, err := config.ApplyReasoningLevel(resolved.Parameters, resolved.ReasoningConfig, "")
	if err != nil {
		return nil, fmt.Errorf("apply reasoning level: %w", err)
	}

	developerRole := model.MessageRoleDeveloper
	if strings.TrimSpace(resolved.DeveloperRole) != "" {
		developerRole = model.MessageRole(resolved.DeveloperRole)
	}

	maxTurns := cfg.Agent.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 8
	}

	store, err := sessionlog.OpenStore(filepath.Join(options.ServerRoot, "data"))
	if err != nil {
		return nil, err
	}

	app := &App{
		cfg:            cfg,
		resolved:       resolved,
		provider:       provider,
		developerRole:  developerRole,
		parameters:     parameters,
		outputLimit:    resolved.OutputLimit,
		maxTurns:       maxTurns,
		cwd:            cwd,
		project:        sessionlog.ProjectID(cwd),
		toolRegistry: tools.NewRegistry(),
		store:        store,
	}
	if err := app.buildTools(); err != nil {
		return nil, err
	}
	if app.systemPrompt, err = app.buildSystemPrompt(); err != nil {
		return nil, err
	}
	return app, nil
}

// NewSession creates a fresh session bound to the app's cwd and model.
func (a *App) NewSession() (*Session, error) {
	log, err := a.store.Create(a.project, a.resolved.ProviderName, a.resolved.ModelID, a.cwd)
	if err != nil {
		return nil, err
	}
	return newSession(a, log), nil
}

// ResumeSession reopens a stored session by ID.
func (a *App) ResumeSession(id string) (*Session, error) {
	log, err := a.store.Open(id)
	if err != nil {
		return nil, err
	}
	return newSession(a, log), nil
}

// ListSessions returns the current project's session summaries, newest
// first.
func (a *App) ListSessions() ([]sessionlog.Summary, error) { return a.store.List(a.project) }

// SessionStoreRoot returns the data root directory (diagnostics and tests).
func (a *App) SessionStoreRoot() string { return a.store.Root() }

// Project returns the project ID sessions are grouped under.
func (a *App) Project() string { return a.project }

// Config returns the loaded configuration (read-only use).
func (a *App) Config() *config.Config { return a.cfg }

func (a *App) buildTools() error {
	if err := tools.RegisterShell(a.toolRegistry, a.cwd); err != nil {
		return fmt.Errorf("register tools: %w", err)
	}
	schemas, err := a.toolRegistry.EnabledSchemas([]string{tools.BuiltinShell})
	if err != nil {
		return fmt.Errorf("resolve enabled tools: %w", err)
	}
	a.toolSchemas = schemas
	return nil
}

func (a *App) buildSystemPrompt() (string, error) {
	sections := make([]string, 0, 2)
	if base := strings.TrimSpace(builtInInstructions); base != "" {
		sections = append(sections, base)
	}
	if raw := strings.TrimSpace(a.cfg.Prompt.SystemPrompt); raw != "" {
		rendered, err := renderPrompt(raw, a.cwd, filepath.Dir(a.cfg.ConfigPath))
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(rendered) != "" {
			sections = append(sections, rendered)
		}
	}
	return strings.Join(sections, "\n\n"), nil
}

func resolveCWD(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		cwd, err := filepath.Abs(".")
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		return cwd, nil
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve cwd %q: %w", value, err)
	}
	return abs, nil
}

const builtInInstructions = `You are a capable software agent working in the user's project directory.

- Your only tool is the shell: a Bash environment whose working directory is the project. Read, search, and edit files, run builds, tests, and git through shell commands.
- Inspect before you change: read the relevant code first, verify your edits afterwards.
- Keep answers concise; show file paths when you change them.`
