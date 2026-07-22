package execution

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/contextwindow"
)

// ConfiguredSessionOptions selects the working directory and root config used
// to create a durable session. Empty values use the project root and
// <cwd>/.agents/sai.yaml respectively.
type ConfiguredSessionOptions struct {
	CWD        string
	ConfigPath string
}

// CreateConfiguredSession creates a session from the project's resolved sai
// configuration. Presentation layers should use this method instead of
// resolving provider, model, tool, MCP, and skill metadata themselves.
func (s *Service) CreateConfiguredSession(projectID string, options ConfiguredSessionOptions) (SessionDetail, error) {
	project, err := s.loadActiveProject(projectID)
	if err != nil {
		return SessionDetail{}, err
	}

	cwd := strings.TrimSpace(options.CWD)
	if cwd == "" {
		cwd = project.Root
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return SessionDetail{}, fmt.Errorf("resolve session cwd %q: %w", options.CWD, err)
	}
	cwd = filepath.Clean(cwd)
	if !isSameOrAncestorProjectPath(project.Root, cwd) {
		return SessionDetail{}, fmt.Errorf("session cwd %q is outside project root %q", cwd, project.Root)
	}

	configPath := strings.TrimSpace(options.ConfigPath)
	if configPath == "" {
		configPath = filepath.Join(cwd, ".agents", "sai.yaml")
	} else if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(cwd, configPath)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return SessionDetail{}, err
	}
	resolved, err := cfg.ResolveModel("", "")
	if err != nil {
		return SessionDetail{}, err
	}
	selectedMCP, err := cfg.SelectedMCPServers(nil, false)
	if err != nil {
		return SessionDetail{}, err
	}
	selectedSkills, err := enabledSkillsForRun(cfg)
	if err != nil {
		return SessionDetail{}, err
	}
	window := contextwindow.ResolveWindow(resolved.ContextWindow)
	showReasoning := cfg.Agent.ShowReasoning
	saveToolResults := true
	contextMetadata := contextwindow.Metadata{
		ContextWindow:           window.Tokens,
		ContextWindowSource:     string(window.Source),
		WarningThresholdPercent: contextwindow.WarningThresholdPercent,
	}

	return s.CreateSession(project.ID, SessionCreateMetadata{
		CreatedCWD:      cwd,
		ConfigPath:      cfg.ConfigPath,
		Provider:        resolved.ProviderName,
		ModelProfile:    resolved.Profile,
		ModelID:         resolved.ModelID,
		ModelParameters: copyParameterMap(resolved.Parameters),
		EnabledTools:    copyStringSlice(cfg.Tools.Enabled),
		EnabledMCP:      mcpServerIDs(selectedMCP),
		EnabledSkills:   skillIDs(selectedSkills),
		ShowReasoning:   &showReasoning,
		Context:         &contextMetadata,
		SaveToolResults: &saveToolResults,
	})
}
