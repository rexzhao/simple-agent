package webapp

import (
	"path/filepath"
	"testing"
)

func TestCommandBasenameAndServerRootEnvironmentName(t *testing.T) {
	tests := []struct {
		argv0    string
		basename string
		envName  string
	}{
		{argv0: `C:\tools\sai.exe`, basename: "sai", envName: "SAI_SERVER_ROOT"},
		{argv0: `/opt/bin/simple-agent`, basename: "simple-agent", envName: "SIMPLE_AGENT_SERVER_ROOT"},
		{argv0: `C:\tools\my.tool.EXE`, basename: "my.tool", envName: "MY_TOOL_SERVER_ROOT"},
	}
	for _, tt := range tests {
		t.Run(tt.argv0, func(t *testing.T) {
			basename, err := commandBasename(tt.argv0)
			if err != nil {
				t.Fatalf("commandBasename() error = %v", err)
			}
			if basename != tt.basename {
				t.Fatalf("commandBasename() = %q, want %q", basename, tt.basename)
			}
			if got := serverRootEnvName(basename); got != tt.envName {
				t.Fatalf("serverRootEnvName() = %q, want %q", got, tt.envName)
			}
		})
	}
}

func TestResolveStorageRootUsesBasenameEnvironmentAndExplicitOverride(t *testing.T) {
	environmentRoot := filepath.Join(t.TempDir(), "from-env")
	explicitRoot := filepath.Join(t.TempDir(), "explicit")
	t.Setenv("CUSTOM_AGENT_SERVER_ROOT", environmentRoot)

	got, err := resolveStorageRoot("", "custom-agent")
	if err != nil {
		t.Fatalf("resolveStorageRoot(environment) error = %v", err)
	}
	want, _ := filepath.Abs(environmentRoot)
	if got != filepath.Clean(want) {
		t.Fatalf("resolveStorageRoot(environment) = %q, want %q", got, want)
	}

	got, err = resolveStorageRoot(explicitRoot, "custom-agent")
	if err != nil {
		t.Fatalf("resolveStorageRoot(explicit) error = %v", err)
	}
	want, _ = filepath.Abs(explicitRoot)
	if got != filepath.Clean(want) {
		t.Fatalf("resolveStorageRoot(explicit) = %q, want %q", got, want)
	}
}
