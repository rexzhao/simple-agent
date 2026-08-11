package execution

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/sessions"
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
	projectID, metadata, err := s.resolveConfiguredSessionMetadata(projectID, options)
	if err != nil {
		return SessionDetail{}, err
	}
	return s.CreateSession(projectID, metadata)
}

// CreateConfiguredSessionIdempotent resolves configuration only for a new
// identity. Once the store has committed the session, a retry returns the
// original frozen provider/model metadata even if server configuration has
// changed or is temporarily unavailable.
func (s *Service) CreateConfiguredSessionIdempotent(ctx context.Context, projectID, sessionID, fingerprint string, options ConfiguredSessionOptions) (SessionDetail, bool, error) {
	var parentLocks []*sessions.SessionWriteLock
	defer func() { releaseSessionMutationLocks(parentLocks) }()
	saved, created, err := s.createSessionIdempotent(ctx, sessionID, fingerprint, func(_ context.Context) (sessions.SessionV2, error) {
		resolvedProjectID, metadata, err := s.resolveConfiguredSessionMetadata(projectID, options)
		if err != nil {
			return sessions.SessionV2{}, err
		}
		if strings.TrimSpace(metadata.ParentSessionID) != "" {
			locks, lockErr := s.acquireSessionParentMutationLocks(metadata.ParentSessionID)
			if lockErr != nil {
				return sessions.SessionV2{}, lockErr
			}
			parentLocks = locks
		}
		return s.buildSession(resolvedProjectID, metadata, sessionID)
	})
	if err != nil {
		return SessionDetail{}, false, err
	}
	if created {
		s.publishSessionCreated(saved)
	}
	return sessionDetailFromStore(saved), created, nil
}

func (s *Service) resolveConfiguredSessionMetadata(projectID string, options ConfiguredSessionOptions) (string, SessionCreateMetadata, error) {
	project, err := s.loadActiveProject(projectID)
	if err != nil {
		return "", SessionCreateMetadata{}, err
	}

	cwd := strings.TrimSpace(options.CWD)
	if cwd == "" {
		cwd = project.Root
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return "", SessionCreateMetadata{}, fmt.Errorf("resolve session cwd %q: %w", options.CWD, err)
	}
	cwd = filepath.Clean(cwd)
	if !isSameOrAncestorProjectPath(project.Root, cwd) {
		return "", SessionCreateMetadata{}, fmt.Errorf("session cwd %q is outside project root %q", cwd, project.Root)
	}

	if suppliedConfigPath := strings.TrimSpace(options.ConfigPath); suppliedConfigPath != "" {
		resolvedConfigPath, err := filepath.Abs(suppliedConfigPath)
		if err != nil {
			return "", SessionCreateMetadata{}, fmt.Errorf("resolve session config path %q: %w", suppliedConfigPath, err)
		}
		if projectPathKey(resolvedConfigPath) != projectPathKey(s.ConfigPath()) {
			return "", SessionCreateMetadata{}, fmt.Errorf("session config path %q does not match server root config %q", suppliedConfigPath, s.ConfigPath())
		}
	}
	cfg, err := config.Load(s.ConfigPath())
	if err != nil {
		return "", SessionCreateMetadata{}, err
	}
	resolved, err := cfg.ResolveModel(options.Provider, options.ModelProfile)
	if err != nil {
		return "", SessionCreateMetadata{}, err
	}
	parameters, err := config.ApplyReasoningLevel(resolved.Parameters, resolved.ReasoningConfig, options.ReasoningLevel)
	if err != nil {
		return "", SessionCreateMetadata{}, err
	}
	selectedMCP, err := cfg.SelectedMCPServers(nil, false)
	if err != nil {
		return "", SessionCreateMetadata{}, err
	}
	selectedSkills, err := enabledSkillsForRun(cfg, cwd)
	if err != nil {
		return "", SessionCreateMetadata{}, err
	}
	window := contextwindow.ResolveWindow(resolved.ContextWindow)
	showReasoning := cfg.Agent.ShowReasoning
	saveToolResults := true
	contextMetadata := contextwindow.Metadata{
		ContextWindow:           window.Tokens,
		ContextWindowSource:     string(window.Source),
		WarningThresholdPercent: contextwindow.WarningThresholdPercent,
	}
	debugSettings := sessions.DebugSettings{RequestBodies: cfg.Logging.RequestBodies}

	return project.ID, SessionCreateMetadata{
		DisplayName:     strings.TrimSpace(options.DisplayName),
		ParentSessionID: strings.TrimSpace(options.ParentSessionID),
		CreatedCWD:      cwd,
		ConfigPath:      cfg.ConfigPath,
		Provider:        resolved.ProviderName,
		ModelProfile:    resolved.Profile,
		ModelID:         resolved.ModelID,
		Pricing:         copyModelPricing(resolved.Pricing),
		ReasoningLevel:  config.ResolveReasoningLevel(resolved.ReasoningConfig, options.ReasoningLevel),
		ModelParameters: copyParameterMap(parameters),
		EnabledTools:    copyStringSlice(cfg.Tools.Enabled),
		EnabledMCP:      mcpServerIDs(selectedMCP),
		EnabledSkills:   skillIDs(selectedSkills),
		ShowReasoning:   &showReasoning,
		FullAccess:      options.FullAccess,
		Debug:           &debugSettings,
		Context:         &contextMetadata,
		SaveToolResults: &saveToolResults,
	}, nil
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
		Pricing:         copyModelPricing(parent.Pricing),
		ReasoningLevel:  parent.ReasoningLevel,
		ModelParameters: copyParameterMap(parent.ModelParameters),
		EnabledTools:    copyStringSlice(parent.EnabledTools),
		EnabledMCP:      copyStringSlice(parent.EnabledMCP),
		EnabledSkills:   copyStringSlice(parent.EnabledSkills),
		ShowReasoning:   &showReasoning,
		FullAccess:      parent.FullAccess,
		Debug:           debugSettingsPointer(parent.Debug),
		Context:         &contextMetadata,
		SaveToolResults: &saveToolResults,
	})
}

// CreateInheritedSessionIdempotent is the durable variant used when a
// command explicitly requests an inherited child. The project id is checked
// against the parent before the first commit; a committed retry returns the
// frozen child without needing to rebuild the parent-derived metadata.
func (s *Service) CreateInheritedSessionIdempotent(ctx context.Context, projectID, parentID, displayName, sessionID, fingerprint string) (SessionDetail, bool, error) {
	var parentLocks []*sessions.SessionWriteLock
	defer func() { releaseSessionMutationLocks(parentLocks) }()
	saved, created, err := s.createSessionIdempotent(ctx, sessionID, fingerprint, func(_ context.Context) (sessions.SessionV2, error) {
		locks, lockErr := s.acquireSessionParentMutationLocks(parentID)
		if lockErr != nil {
			return sessions.SessionV2{}, lockErr
		}
		parentLocks = locks
		parent, err := s.GetSession(parentID)
		if err != nil {
			return sessions.SessionV2{}, err
		}
		if strings.TrimSpace(projectID) == "" || parent.ProjectID != strings.TrimSpace(projectID) {
			return sessions.SessionV2{}, fmt.Errorf("parent session belongs to a different project")
		}
		warningThreshold := parent.Context.WarningThresholdPercent
		if warningThreshold <= 0 {
			warningThreshold = contextwindow.WarningThresholdPercent
		}
		return s.buildSession(projectID, SessionCreateMetadata{
			DisplayName:     strings.TrimSpace(displayName),
			ParentSessionID: parent.ID,
			CreatedCWD:      inheritedSessionCWD(parent),
			ConfigPath:      s.ConfigPath(),
			Provider:        parent.Provider,
			ModelProfile:    parent.ModelProfile,
			ModelID:         parent.ModelID,
			Pricing:         copyModelPricing(parent.Pricing),
			ReasoningLevel:  parent.ReasoningLevel,
			ModelParameters: copyParameterMap(parent.ModelParameters),
			EnabledTools:    copyStringSlice(parent.EnabledTools),
			EnabledMCP:      copyStringSlice(parent.EnabledMCP),
			EnabledSkills:   copyStringSlice(parent.EnabledSkills),
			ShowReasoning:   boolPointer(parent.ShowReasoning),
			FullAccess:      parent.FullAccess,
			Debug:           debugSettingsPointer(parent.Debug),
			Context: &contextwindow.Metadata{
				ContextWindow:           parent.Context.ContextWindow,
				ContextWindowSource:     parent.Context.ContextWindowSource,
				WarningThresholdPercent: warningThreshold,
			},
			SaveToolResults: boolPointer(true),
		}, sessionID)
	})
	if err != nil {
		return SessionDetail{}, false, err
	}
	if created {
		s.publishSessionCreated(saved)
	}
	return sessionDetailFromStore(saved), created, nil
}

func inheritedSessionCWD(parent SessionDetail) string {
	if cwd := strings.TrimSpace(parent.CWD); cwd != "" {
		return cwd
	}
	return strings.TrimSpace(parent.CreatedCWD)
}

func boolPointer(value bool) *bool { return &value }

func debugSettingsPointer(settings sessions.DebugSettings) *sessions.DebugSettings {
	return &settings
}
