package execution

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

type Service struct {
	projectStore *projectstore.Store
	sessionStore *sessions.V2Store
}

type Project struct {
	ID          string
	Root        string
	DisplayName string
	Archived    bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type ProjectCreateResult struct {
	Project Project
	Created bool
}

type ProjectRemoveResult struct {
	Status          string
	ID              string
	RemovedSessions int
}

type ProjectListOptions struct {
	Archived bool
}

type NearestProjectOptions struct {
	IncludeArchived bool
}

func NewService(home string) (*Service, error) {
	projectRoot, err := projectstore.RootForHome(home)
	if err != nil {
		return nil, err
	}
	sessionRoot, err := sessions.RootForHome(home)
	if err != nil {
		return nil, err
	}
	return &Service{
		projectStore: projectstore.NewStore(projectRoot),
		sessionStore: sessions.NewV2Store(sessionRoot),
	}, nil
}

func (s *Service) CreateProject(root, displayName string) (ProjectCreateResult, error) {
	if s == nil || s.projectStore == nil {
		return ProjectCreateResult{}, fmt.Errorf("execution project store is not configured")
	}
	project, created, err := s.projectStore.Create(root, displayName)
	if err != nil {
		return ProjectCreateResult{}, err
	}
	return ProjectCreateResult{Project: projectFromStore(project), Created: created}, nil
}

func (s *Service) ListProjects(options ProjectListOptions) ([]Project, error) {
	if s == nil || s.projectStore == nil {
		return nil, fmt.Errorf("execution project store is not configured")
	}
	projects, err := s.projectStore.ListWithOptions(projectstore.ListOptions{Archived: options.Archived})
	if err != nil {
		return nil, err
	}
	return projectsFromStore(projects), nil
}

func (s *Service) GetProject(id string) (Project, error) {
	if s == nil || s.projectStore == nil {
		return Project{}, fmt.Errorf("execution project store is not configured")
	}
	project, err := s.projectStore.Load(id)
	if err != nil {
		return Project{}, err
	}
	return projectFromStore(project), nil
}

func (s *Service) NearestProject(cwd string, options NearestProjectOptions) (Project, bool, error) {
	if s == nil || s.projectStore == nil {
		return Project{}, false, fmt.Errorf("execution project store is not configured")
	}
	canonicalCWD, err := projectstore.CanonicalRoot(cwd)
	if err != nil {
		return Project{}, false, err
	}
	projects, err := s.projectStore.ListWithOptions(projectstore.ListOptions{})
	if err != nil {
		return Project{}, false, err
	}
	if options.IncludeArchived {
		archived, err := s.projectStore.ListWithOptions(projectstore.ListOptions{Archived: true})
		if err != nil {
			return Project{}, false, err
		}
		projects = append(projects, archived...)
	}
	var best projectstore.Project
	bestLen := -1
	for _, project := range projects {
		if strings.TrimSpace(project.Root) == "" || (!options.IncludeArchived && project.Archived) {
			continue
		}
		if !isSameOrAncestorProjectPath(project.Root, canonicalCWD) {
			continue
		}
		rootLen := len(projectPathKey(project.Root))
		if rootLen > bestLen {
			best = project
			bestLen = rootLen
		}
	}
	if bestLen < 0 {
		return Project{}, false, nil
	}
	return projectFromStore(best), true, nil
}

func (s *Service) RenameProject(id, displayName string) (Project, error) {
	if s == nil || s.projectStore == nil {
		return Project{}, fmt.Errorf("execution project store is not configured")
	}
	project, err := s.projectStore.Load(id)
	if err != nil {
		return Project{}, err
	}
	if project.Archived {
		return Project{}, fmt.Errorf("archived project cannot be renamed")
	}
	project, err = s.projectStore.Rename(project.ID, displayName)
	if err != nil {
		return Project{}, err
	}
	return projectFromStore(project), nil
}

func (s *Service) ArchiveProject(id string) (Project, error) {
	if s == nil || s.projectStore == nil {
		return Project{}, fmt.Errorf("execution project store is not configured")
	}
	project, err := s.projectStore.Load(id)
	if err != nil {
		return Project{}, err
	}
	if project.Archived {
		return projectFromStore(project), nil
	}
	project, err = s.projectStore.Archive(project.ID)
	if err != nil {
		return Project{}, err
	}
	return projectFromStore(project), nil
}

func (s *Service) RemoveProject(id string) (ProjectRemoveResult, error) {
	if s == nil || s.projectStore == nil {
		return ProjectRemoveResult{}, fmt.Errorf("execution project store is not configured")
	}
	project, err := s.projectStore.Load(id)
	if err != nil {
		return ProjectRemoveResult{}, err
	}
	if !project.Archived {
		return ProjectRemoveResult{}, fmt.Errorf("archive project before removing it")
	}
	removedSessions, err := s.removeProjectSessions(project.ID)
	if err != nil {
		return ProjectRemoveResult{}, err
	}
	if err := s.projectStore.Delete(project.ID); err != nil {
		if errors.Is(err, projectstore.ErrNotFound) {
			return ProjectRemoveResult{}, err
		}
		return ProjectRemoveResult{}, fmt.Errorf("remove project %s: %w", project.ID, err)
	}
	return ProjectRemoveResult{Status: "removed", ID: project.ID, RemovedSessions: removedSessions}, nil
}

func (s *Service) removeProjectSessions(projectID string) (int, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" || s == nil || s.sessionStore == nil {
		return 0, nil
	}
	infos, err := s.sessionStore.ListWithOptions(sessions.V2ListOptions{All: true})
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, info := range infos {
		if info.ProjectID != projectID {
			continue
		}
		if err := s.sessionStore.Delete(info.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func projectFromStore(project projectstore.Project) Project {
	return Project{
		ID:          project.ID,
		Root:        project.Root,
		DisplayName: project.DisplayName,
		Archived:    project.Archived,
		CreatedAt:   project.CreatedAt,
		UpdatedAt:   project.UpdatedAt,
	}
}

func projectsFromStore(projects []projectstore.Project) []Project {
	items := make([]Project, 0, len(projects))
	for _, project := range projects {
		items = append(items, projectFromStore(project))
	}
	return items
}

func isSameOrAncestorProjectPath(root, cwd string) bool {
	rootKey := projectPathKey(root)
	cwdKey := projectPathKey(cwd)
	if rootKey == cwdKey {
		return true
	}
	rel, err := filepath.Rel(rootKey, cwdKey)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func projectPathKey(path string) string {
	key := filepath.Clean(path)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key
}
