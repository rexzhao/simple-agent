package projects

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	Version         = 1
	projectFileName = "project.json"
)

var ErrNotFound = errors.New("project not found")

type Project struct {
	ID          string    `json:"id"`
	Version     int       `json:"-"`
	Root        string    `json:"root"`
	DisplayName string    `json:"display_name,omitempty"`
	Archived    bool      `json:"-"`
	ArchivedAt  time.Time `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type projectFile struct {
	ID          string     `json:"id"`
	Root        string     `json:"root"`
	DisplayName string     `json:"display_name,omitempty"`
	ArchivedAt  *time.Time `json:"archived_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

type Store struct {
	root string
	now  func() time.Time
}

type ListOptions struct {
	Archived bool
}

func NewStore(root string) *Store {
	return newStoreWithClock(root, time.Now)
}

func newStoreWithClock(root string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	root = strings.TrimSpace(root)
	if root != "" {
		root = filepath.Clean(root)
	}
	return &Store{
		root: root,
		now:  now,
	}
}

func RootForHome(home string) (string, error) {
	home = strings.TrimSpace(home)
	if home == "" {
		return "", fmt.Errorf("home directory is required")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve home directory %q: %w", home, err)
	}
	return filepath.Join(filepath.Clean(abs), "data", "projects"), nil
}

func RootForServerRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", fmt.Errorf("server root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve server root %q: %w", root, err)
	}
	return filepath.Join(filepath.Clean(abs), "data", "projects"), nil
}

func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

func (s *Store) Create(root, displayName string) (Project, bool, error) {
	if err := s.requireRoot(); err != nil {
		return Project{}, false, err
	}
	canonicalRoot, err := CanonicalRoot(root)
	if err != nil {
		return Project{}, false, err
	}

	if existing, ok, err := s.findByRoot(canonicalRoot); err != nil {
		return Project{}, false, err
	} else if ok {
		return existing, false, nil
	}

	now := s.now().UTC()
	project := Project{
		ID:          projectIDForRoot(canonicalRoot),
		Root:        canonicalRoot,
		DisplayName: strings.TrimSpace(displayName),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	project = normalizeProjectLifecycle(project)
	if err := s.saveProject(project); err != nil {
		return Project{}, false, err
	}
	return project, true, nil
}

func (s *Store) Load(id string) (Project, error) {
	if err := s.requireRoot(); err != nil {
		return Project{}, err
	}
	if err := validateProjectID(id); err != nil {
		return Project{}, err
	}
	project, err := readProjectFile(s.projectPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Project{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return Project{}, err
	}
	if project.ID == "" {
		project.ID = id
	}
	if project.ID != id {
		return Project{}, fmt.Errorf("project file %q contains id %q", id, project.ID)
	}
	return copyProject(project), nil
}

func (s *Store) List() ([]Project, error) {
	return s.ListWithOptions(ListOptions{})
}

func (s *Store) ListWithOptions(options ListOptions) ([]Project, error) {
	return s.list(options)
}

func (s *Store) Rename(id, displayName string) (Project, error) {
	project, err := s.Load(id)
	if err != nil {
		return Project{}, err
	}
	project.DisplayName = strings.TrimSpace(displayName)
	project.UpdatedAt = s.now().UTC()
	if err := s.writeProject(project); err != nil {
		return Project{}, err
	}
	return copyProject(project), nil
}

func (s *Store) Archive(id string) (Project, error) {
	project, err := s.Load(id)
	if err != nil {
		return Project{}, err
	}
	now := s.now().UTC()
	project.Archived = true
	if project.ArchivedAt.IsZero() {
		project.ArchivedAt = now
	}
	project.UpdatedAt = now
	if err := s.writeProject(project); err != nil {
		return Project{}, err
	}
	return copyProject(project), nil
}

func (s *Store) Restore(id string) (Project, error) {
	project, err := s.Load(id)
	if err != nil {
		return Project{}, err
	}
	project.Archived = false
	project.ArchivedAt = time.Time{}
	project.UpdatedAt = s.now().UTC()
	if err := s.writeProject(project); err != nil {
		return Project{}, err
	}
	return copyProject(project), nil
}

func (s *Store) Delete(id string) error {
	if err := s.requireRoot(); err != nil {
		return err
	}
	if err := validateProjectID(id); err != nil {
		return err
	}
	dir := s.projectDir(id)
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return fmt.Errorf("stat project %q: %w", id, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("project %q is not a directory", id)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete project %q: %w", id, err)
	}
	return nil
}

func (s *Store) NearestAncestor(cwd string) (Project, bool, error) {
	if err := s.requireRoot(); err != nil {
		return Project{}, false, err
	}
	canonicalCWD, err := CanonicalRoot(cwd)
	if err != nil {
		return Project{}, false, err
	}
	projects, err := s.List()
	if err != nil {
		return Project{}, false, err
	}
	byRoot := make(map[string]Project, len(projects))
	for _, project := range projects {
		byRoot[projectRootKey(project.Root)] = project
	}
	for current := canonicalCWD; ; current = filepath.Dir(current) {
		if project, ok := byRoot[projectRootKey(current)]; ok {
			return project, true, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return Project{}, false, nil
		}
	}
}

func CanonicalRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", fmt.Errorf("project root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve project root %q: %w", root, err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalize project root %q: %w", root, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("stat project root %q: %w", canonical, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project root %q is not a directory", canonical)
	}
	return filepath.Clean(canonical), nil
}

func (s *Store) list(options ListOptions) ([]Project, error) {
	if err := s.requireRoot(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Project{}, nil
		}
		return nil, fmt.Errorf("read project store %q: %w", s.root, err)
	}

	projects := make([]Project, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		if err := validateProjectID(id); err != nil {
			continue
		}
		project, err := readProjectFile(s.projectPath(id))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if project.ID == "" {
			project.ID = id
		}
		if project.ID != id {
			return nil, fmt.Errorf("project file %q contains id %q", id, project.ID)
		}
		if project.Archived != options.Archived {
			continue
		}
		projects = append(projects, copyProject(project))
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].CreatedAt.Equal(projects[j].CreatedAt) {
			return projects[i].ID < projects[j].ID
		}
		return projects[i].CreatedAt.Before(projects[j].CreatedAt)
	})
	return projects, nil
}

func (s *Store) findByRoot(canonicalRoot string) (Project, bool, error) {
	activeProjects, err := s.ListWithOptions(ListOptions{})
	if err != nil {
		return Project{}, false, err
	}
	archivedProjects, err := s.ListWithOptions(ListOptions{Archived: true})
	if err != nil {
		return Project{}, false, err
	}
	projects := append(activeProjects, archivedProjects...)
	for _, project := range projects {
		if samePath(project.Root, canonicalRoot) {
			return project, true, nil
		}
	}
	return Project{}, false, nil
}

func (s *Store) saveProject(project Project) error {
	if err := s.requireRoot(); err != nil {
		return err
	}
	if err := validateProjectID(project.ID); err != nil {
		return err
	}
	canonicalRoot, err := CanonicalRoot(project.Root)
	if err != nil {
		return err
	}
	project.Root = canonicalRoot
	if project.Version == 0 {
		project.Version = Version
	}
	if project.CreatedAt.IsZero() {
		project.CreatedAt = s.now().UTC()
	}
	if project.UpdatedAt.IsZero() {
		project.UpdatedAt = project.CreatedAt
	}
	project = normalizeProjectLifecycle(project)
	return s.writeProject(project)
}

func (s *Store) writeProject(project Project) error {
	project = normalizeProjectLifecycle(project)
	dir := s.projectDir(project.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create project directory %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(projectFileFromProject(project), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal project %q: %w", project.ID, err)
	}
	data = append(data, '\n')
	if err := writeFileAtomicPrivate(s.projectPath(project.ID), data); err != nil {
		return fmt.Errorf("write project %q: %w", project.ID, err)
	}
	return nil
}

func (s *Store) projectDir(id string) string {
	return filepath.Join(s.root, id)
}

func (s *Store) projectPath(id string) string {
	return filepath.Join(s.projectDir(id), projectFileName)
}

func (s *Store) requireRoot() error {
	if s == nil || strings.TrimSpace(s.root) == "" || s.root == "." {
		return fmt.Errorf("project store directory is required")
	}
	return nil
}

func readProjectFile(path string) (Project, error) {
	file, err := os.Open(path)
	if err != nil {
		return Project{}, err
	}
	defer file.Close()

	var record projectFile
	decoder := json.NewDecoder(file)
	decoder.UseNumber()
	if err := decoder.Decode(&record); err != nil {
		return Project{}, fmt.Errorf("parse project file %q: %w", path, err)
	}
	return record.project(), nil
}

func projectFileFromProject(project Project) projectFile {
	project = normalizeProjectLifecycle(project)
	var archivedAt *time.Time
	if !project.ArchivedAt.IsZero() {
		value := project.ArchivedAt.UTC()
		archivedAt = &value
	}
	return projectFile{
		ID:          project.ID,
		Root:        project.Root,
		DisplayName: project.DisplayName,
		ArchivedAt:  archivedAt,
		CreatedAt:   project.CreatedAt,
		UpdatedAt:   project.UpdatedAt,
	}
}

func (r projectFile) project() Project {
	project := Project{
		ID:          r.ID,
		Version:     Version,
		Root:        r.Root,
		DisplayName: r.DisplayName,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
	if r.ArchivedAt != nil {
		project.ArchivedAt = r.ArchivedAt.UTC()
	}
	return normalizeProjectLifecycle(project)
}

func projectIDForRoot(root string) string {
	sum := sha256.Sum256([]byte(projectRootKey(root)))
	return "project-" + hex.EncodeToString(sum[:12])
}

func validateProjectID(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("project id is required")
	}
	if id != strings.TrimSpace(id) || id == "." || id == ".." {
		return fmt.Errorf("invalid project id %q", id)
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("invalid project id %q", id)
	}
	return nil
}

func projectRootKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func samePath(a, b string) bool {
	return projectRootKey(a) == projectRootKey(b)
}

func copyProject(project Project) Project {
	return normalizeProjectLifecycle(project)
}

func normalizeProjectLifecycle(project Project) Project {
	if !project.ArchivedAt.IsZero() {
		project.ArchivedAt = project.ArchivedAt.UTC()
		project.Archived = true
	} else if project.Archived {
		if !project.UpdatedAt.IsZero() {
			project.ArchivedAt = project.UpdatedAt.UTC()
		} else if !project.CreatedAt.IsZero() {
			project.ArchivedAt = project.CreatedAt.UTC()
		}
		project.Archived = !project.ArchivedAt.IsZero()
	} else {
		project.Archived = false
	}
	if project.Version == 0 {
		project.Version = Version
	}
	return project
}

func writeFileAtomicPrivate(path string, data []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".project-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()

	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("chmod temporary file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if runtime.GOOS == "windows" {
		_ = os.Remove(path)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	cleanup = false
	_ = os.Chmod(path, 0o600)
	return nil
}
