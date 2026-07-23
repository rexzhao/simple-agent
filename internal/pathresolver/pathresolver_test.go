package pathresolver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveExpandsConfiguredRoots(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cwd := filepath.Join(root, "repo", "work")
	configDir := filepath.Join(root, "config")
	repo := filepath.Join(root, "repo")

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "home", value: "$HOME/skills", want: filepath.Join(home, "skills")},
		{name: "user alias", value: "$USER/skills", want: filepath.Join(home, "skills")},
		{name: "cwd", value: "$CWD/skills", want: filepath.Join(cwd, "skills")},
		{name: "config", value: "$CONFIG/skills", want: filepath.Join(configDir, "skills")},
		{name: "repo", value: "$REPO/skills", want: filepath.Join(repo, "skills")},
		{name: "relative", value: "skills", want: filepath.Join(configDir, "skills")},
	}
	variables := Variables{HomeDir: home, CWD: cwd, ConfigDir: configDir, RepoDir: repo}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Resolve(test.value, configDir, variables)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got != filepath.Clean(test.want) {
				t.Fatalf("Resolve() = %q, want %q", got, filepath.Clean(test.want))
			}
		})
	}
}

func TestExpandDiscoversRepoFromCWD(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	cwd := filepath.Join(repo, "one", "two")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(.git) error = %v", err)
	}

	got, err := Expand("$REPO/.agents/skills", Variables{CWD: cwd})
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	want := filepath.Join(repo, ".agents", "skills")
	if filepath.Clean(got) != want {
		t.Fatalf("Expand() = %q, want %q", got, want)
	}
}

func TestExpandReportsUnresolvedRepo(t *testing.T) {
	_, err := Expand("$REPO/skills", Variables{CWD: t.TempDir()})
	if err == nil {
		t.Fatal("Expand() error = nil, want unresolved repository error")
	}
}

func TestExpandDoesNotReplacePlaceholderPrefixes(t *testing.T) {
	got, err := Expand("$HOMELESS/skills", Variables{HomeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	if got != "$HOMELESS/skills" {
		t.Fatalf("Expand() = %q, want unchanged placeholder prefix", got)
	}
}

func TestResolveAllPreservesNilAndOrder(t *testing.T) {
	got, err := ResolveAll(nil, t.TempDir(), Variables{})
	if err != nil {
		t.Fatalf("ResolveAll(nil) error = %v", err)
	}
	if got != nil {
		t.Fatalf("ResolveAll(nil) = %#v, want nil", got)
	}

	base := t.TempDir()
	got, err = ResolveAll([]string{"first", "second"}, base, Variables{})
	if err != nil {
		t.Fatalf("ResolveAll() error = %v", err)
	}
	want := []string{filepath.Join(base, "first"), filepath.Join(base, "second")}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("ResolveAll()[%d] = %q, want %q", index, got[index], want[index])
		}
	}
}
