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
	// FullAccess lets file tools read and write outside the CWD.
	FullAccess bool
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
	fullAccess     bool
	toolRegistry   *tools.Registry
	toolSchemas    []model.Tool
	defaultEnabled []string
	// toolNotice reports config entries which were skipped (unknown tools).
	toolNotice string
	store      *sessionlog.Store
}

// ToolNotice returns a human-readable note about skipped configuration
// entries, or the empty string when nothing was skipped.
func (a *App) ToolNotice() string { return a.toolNotice }

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

	enabled := cfg.Tools.Enabled
	if len(enabled) == 0 {
		enabled = defaultTools
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
		fullAccess:     options.FullAccess,
		toolRegistry:   tools.NewRegistry(),
		defaultEnabled: enabled,
		store:          store,
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
	if err := tools.RegisterBuiltins(a.toolRegistry, a.cwd, a.fullAccess); err != nil {
		return fmt.Errorf("register tools: %w", err)
	}
	// The configured toolset may name tools from surfaces this app does not
	// provide (the retired Web stack's agent-session orchestration, MCP
	// servers). Intersect instead of failing: unknown names are skipped, and
	// an empty intersection falls back to every registered builtin so the
	// agent never runs toolless by accident.
	enabled := make([]string, 0, len(a.defaultEnabled))
	var unknown []string
	for _, name := range a.defaultEnabled {
		if _, ok := a.toolRegistry.Lookup(name); ok {
			enabled = append(enabled, name)
		} else {
			unknown = append(unknown, name)
		}
	}
	if len(enabled) == 0 {
		enabled = a.toolRegistry.Names()
	}
	if len(unknown) > 0 {
		a.toolNotice = fmt.Sprintf("ignored unregistered tools: %s", strings.Join(unknown, ", "))
	}
	schemas, err := a.toolRegistry.EnabledSchemas(enabled)
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

var defaultTools = []string{
	"list_files", "read_file", "glob_files", "grep_files",
	"write_file", "edit_file", "apply_patch", "shell",
}

const builtInInstructions = `You are a capable software agent working in the user's project directory.

- Prefer reading files and searching before making changes; verify your edits.
- Use the shell tool for builds, tests, and git operations.
- Keep answers concise; show file paths when you change them.`
