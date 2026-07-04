package projects

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestStoreCreateCanonicalizesPersistsAndDuplicatesByRootOnly(t *testing.T) {
	clock := fakeClock{current: time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)}
	storeRoot := filepath.Join(t.TempDir(), "home", "data", "projects")
	store := newStoreWithClock(storeRoot, clock.Now)
	projectRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(projectRoot) error = %v", err)
	}

	project, created, err := store.Create(filepath.Join(projectRoot, "."), "Display Name")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !created {
		t.Fatal("Create() created = false, want true")
	}
	canonicalRoot, err := CanonicalRoot(projectRoot)
	if err != nil {
		t.Fatalf("CanonicalRoot() error = %v", err)
	}
	if project.Root != canonicalRoot {
		t.Fatalf("project.Root = %q, want %q", project.Root, canonicalRoot)
	}
	if project.DisplayName != "Display Name" {
		t.Fatalf("DisplayName = %q, want Display Name", project.DisplayName)
	}
	if project.ID == "" || project.Version != Version || project.Archived {
		t.Fatalf("project metadata = %#v, want id version active", project)
	}
	if _, err := os.Stat(filepath.Join(storeRoot, project.ID, projectFileName)); err != nil {
		t.Fatalf("project metadata was not written under store root: %v", err)
	}
	if entries, err := os.ReadDir(projectRoot); err != nil || len(entries) != 0 {
		t.Fatalf("project repo entries = %#v, err = %v, want no marker files", entries, err)
	}

	duplicate, created, err := store.Create(projectRoot, "Different Name")
	if err != nil {
		t.Fatalf("Create(duplicate) error = %v", err)
	}
	if created {
		t.Fatal("Create(duplicate) created = true, want false")
	}
	if duplicate.ID != project.ID || duplicate.Root != project.Root {
		t.Fatalf("duplicate = %#v, want existing project %#v", duplicate, project)
	}
	if duplicate.DisplayName != "Display Name" {
		t.Fatalf("duplicate DisplayName = %q, want original display metadata", duplicate.DisplayName)
	}
}

func TestStoreCreateRequiresExistingDirectory(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "projects"))
	missing := filepath.Join(t.TempDir(), "missing")
	if _, _, err := store.Create(missing, "missing"); err == nil {
		t.Fatal("Create(missing) error = nil, want error")
	}

	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, _, err := store.Create(file, "file"); err == nil {
		t.Fatal("Create(file) error = nil, want error")
	}
}

func TestStoreListOmitsArchivedProjects(t *testing.T) {
	clock := fakeClock{current: time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)}
	store := newStoreWithClock(filepath.Join(t.TempDir(), "projects"), clock.Now)
	activeRoot := mkdirProjectDir(t, "active")
	archivedRoot := mkdirProjectDir(t, "archived")
	archivedChild := filepath.Join(archivedRoot, "child")
	if err := os.MkdirAll(archivedChild, 0o755); err != nil {
		t.Fatalf("MkdirAll(archivedChild) error = %v", err)
	}

	active, _, err := store.Create(activeRoot, "Active")
	if err != nil {
		t.Fatalf("Create(active) error = %v", err)
	}
	archived, _, err := store.Create(archivedRoot, "Archived")
	if err != nil {
		t.Fatalf("Create(archived) error = %v", err)
	}
	archived, err = store.Archive(archived.ID)
	if err != nil {
		t.Fatalf("Archive(archived) error = %v", err)
	}
	if !archived.Archived {
		t.Fatalf("Archive() returned archived = false: %#v", archived)
	}

	projects, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got, want := projectIDs(projects), []string{active.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("List() IDs = %#v, want %#v", got, want)
	}
	if _, err := store.Load(archived.ID); err != nil {
		t.Fatalf("Load(archived) error = %v; detail should still be available by id", err)
	}
	if _, ok, err := store.NearestAncestor(archivedChild); err != nil || ok {
		t.Fatalf("NearestAncestor(archived child) ok=%t err=%v, want no match", ok, err)
	}
}

func TestStoreDeleteRemovesProjectMetadataDirectory(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "projects")
	store := NewStore(storeRoot)
	projectRoot := mkdirProjectDir(t, "delete")
	project, _, err := store.Create(projectRoot, "Delete")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	extraPath := filepath.Join(storeRoot, project.ID, "data.bin")
	if err := os.WriteFile(extraPath, []byte("project data"), 0o600); err != nil {
		t.Fatalf("WriteFile(extra project data) error = %v", err)
	}

	if err := store.Delete(project.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(storeRoot, project.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("project directory stat error = %v, want not exist", err)
	}
	if _, err := store.Load(project.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load(deleted) error = %v, want ErrNotFound", err)
	}
	projects, err := store.List()
	if err != nil {
		t.Fatalf("List() after delete error = %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("List() after delete = %#v, want empty", projects)
	}
}

func TestStoreNearestAncestorAllowsNestedProjects(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "projects"))
	parentRoot := mkdirProjectDir(t, "parent")
	nestedRoot := filepath.Join(parentRoot, "nested")
	childDir := filepath.Join(nestedRoot, "child")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(childDir) error = %v", err)
	}

	parent, _, err := store.Create(parentRoot, "Parent")
	if err != nil {
		t.Fatalf("Create(parent) error = %v", err)
	}
	nested, _, err := store.Create(nestedRoot, "Nested")
	if err != nil {
		t.Fatalf("Create(nested) error = %v", err)
	}

	got, ok, err := store.NearestAncestor(childDir)
	if err != nil {
		t.Fatalf("NearestAncestor(child) error = %v", err)
	}
	if !ok || got.ID != nested.ID {
		t.Fatalf("NearestAncestor(child) = %#v/%t, want nested %#v", got, ok, nested)
	}
	got, ok, err = store.NearestAncestor(parentRoot)
	if err != nil {
		t.Fatalf("NearestAncestor(parent) error = %v", err)
	}
	if !ok || got.ID != parent.ID {
		t.Fatalf("NearestAncestor(parent) = %#v/%t, want parent %#v", got, ok, parent)
	}

	outside := mkdirProjectDir(t, "outside")
	if _, ok, err := store.NearestAncestor(outside); err != nil || ok {
		t.Fatalf("NearestAncestor(outside) ok=%t err=%v, want no match", ok, err)
	}
}

func TestStoreLoadMissing(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "projects"))
	_, err := store.Load("project-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load(missing) error = %v, want ErrNotFound", err)
	}
}

func mkdirProjectDir(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", path, err)
	}
	return path
}

func projectIDs(projects []Project) []string {
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		ids = append(ids, project.ID)
	}
	return ids
}

type fakeClock struct {
	current time.Time
}

func (c *fakeClock) Now() time.Time {
	current := c.current
	c.current = c.current.Add(time.Second)
	return current
}
