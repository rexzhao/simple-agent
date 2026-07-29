package execution

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/contextwindow"
)

// ConfiguredSessionOptions selects the working directory and model used to
// create a durable session. Configuration always comes from the service's
// server-root config path.
type ConfiguredSessionOptions struct {
	CWD             string
	ConfigPath      string
	DisplayName     string
	ParentSessionID string
	Provider        string
	ModelProfile    string
	ReasoningLevel  string
	FullAccess      bool
}

type SessionModelOption struct {
	Provider              string   `json:"provider"`
	ModelProfile          string   `json:"model_profile"`
	ModelID               string   `json:"model_id"`
	ReasoningLevels       []string `json:"reasoning_levels,omitempty"`
	DefaultReasoningLevel string   `json:"default_reasoning_level,omitempty"`
}

type SessionModelOptions struct {
	Models          []SessionModelOption `json:"models"`
	DefaultProvider string               `json:"default_provider"`
	DefaultModel    string               `json:"default_model"`
}

// ConfiguredSessionModels returns the server-root models available to new
// sessions without exposing provider credentials or the rest of the config.
func (s *Service) ConfiguredSessionModels(projectID string) (SessionModelOptions, error) {
	if _, err := s.loadActiveProject(projectID); err != nil {
		return SessionModelOptions{}, err
	}
	cfg, err := config.Load(s.ConfigPath())
	if err != nil {
		return SessionModelOptions{}, err
	}
	models := cfg.ModelList()
	options := SessionModelOptions{
		Models:          make([]SessionModelOption, 0, len(models)),
		DefaultProvider: strings.TrimSpace(cfg.DefaultProvider),
		DefaultModel:    strings.TrimSpace(cfg.DefaultModel),
	}
	for _, model := range models {
		profile := cfg.Providers[model.Provider].Models[model.Profile]
		reasoningLevels := config.ReasoningLevelNames(profile.ReasoningConfig.Levels)
		options.Models = append(options.Models, SessionModelOption{
			Provider:              model.Provider,
			ModelProfile:          model.Profile,
			ModelID:               model.ID,
			ReasoningLevels:       reasoningLevels,
			DefaultReasoningLevel: profile.ReasoningConfig.Default,
		})
	}
	return options, nil
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

	if strings.TrimSpace(options.ConfigPath) != "" {
		return SessionDetail{}, fmt.Errorf("per-session config path is not supported; configuration comes from server root")
	}
	cfg, err := config.Load(s.ConfigPath())
	if err != nil {
		return SessionDetail{}, err
	}
	resolved, err := cfg.ResolveModel(options.Provider, options.ModelProfile)
	if err != nil {
		return SessionDetail{}, err
	}
	parameters, err := config.ApplyReasoningLevel(resolved.Parameters, resolved.ReasoningConfig, options.ReasoningLevel)
	if err != nil {
		return SessionDetail{}, err
	}
	selectedMCP, err := cfg.SelectedMCPServers(nil, false)
	if err != nil {
		return SessionDetail{}, err
	}
	selectedSkills, err := enabledSkillsForRun(cfg, cwd)
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
		DisplayName:     strings.TrimSpace(options.DisplayName),
		ParentSessionID: strings.TrimSpace(options.ParentSessionID),
		CreatedCWD:      cwd,
		ConfigPath:      cfg.ConfigPath,
		Provider:        resolved.ProviderName,
		ModelProfile:    resolved.Profile,
		ModelID:         resolved.ModelID,
		ReasoningLevel:  config.ResolveReasoningLevel(resolved.ReasoningConfig, options.ReasoningLevel),
		ModelParameters: copyParameterMap(parameters),
		EnabledTools:    copyStringSlice(cfg.Tools.Enabled),
		EnabledMCP:      mcpServerIDs(selectedMCP),
		EnabledSkills:   skillIDs(selectedSkills),
		ShowReasoning:   &showReasoning,
		FullAccess:      options.FullAccess,
		Context:         &contextMetadata,
		SaveToolResults: &saveToolResults,
	})
}

// CreateInheritedSession creates an agent child using the parent's frozen
// runtime model and capability snapshot. It starts with fresh history and
// context usage while retaining the same project and working directory.
func (s *Service) CreateInheritedSession(parentID, displayName string) (SessionDetail, error) {
	parent, err := s.GetSession(parentID)
	if err != nil {
		return SessionDetail{}, err
	}
	contextMetadata := contextwindow.Metadata{
		ContextWindow:           parent.Context.ContextWindow,
		ContextWindowSource:     parent.Context.ContextWindowSource,
		WarningThresholdPercent: parent.Context.WarningThresholdPercent,
	}
	if contextMetadata.WarningThresholdPercent <= 0 {
		contextMetadata.WarningThresholdPercent = contextwindow.WarningThresholdPercent
	}
	showReasoning := parent.ShowReasoning
	saveToolResults := true
	cwd := strings.TrimSpace(parent.CWD)
	if cwd == "" {
		cwd = parent.CreatedCWD
	}
	return s.CreateSession(parent.ProjectID, SessionCreateMetadata{
		DisplayName:     strings.TrimSpace(displayName),
		ParentSessionID: parent.ID,
		CreatedCWD:      cwd,
		ConfigPath:      s.ConfigPath(),
		Provider:        parent.Provider,
		ModelProfile:    parent.ModelProfile,
		ModelID:         parent.ModelID,
		ReasoningLevel:  parent.ReasoningLevel,
		ModelParameters: copyParameterMap(parent.ModelParameters),
		EnabledTools:    copyStringSlice(parent.EnabledTools),
		EnabledMCP:      copyStringSlice(parent.EnabledMCP),
		EnabledSkills:   copyStringSlice(parent.EnabledSkills),
		ShowReasoning:   &showReasoning,
		FullAccess:      parent.FullAccess,
		Context:         &contextMetadata,
		SaveToolResults: &saveToolResults,
	})
}
