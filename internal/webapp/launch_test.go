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

func TestIsLoopbackAddress(t *testing.T) {
	loopback := []string{"127.0.0.1:0", "127.0.0.1:8080", "localhost:9000", "[::1]:8080"}
	for _, address := range loopback {
		if !isLoopbackAddress(address) {
			t.Fatalf("isLoopbackAddress(%q) = false, want true", address)
		}
	}
	nonLoopback := []string{"0.0.0.0:8080", "10.0.0.5:8080", "192.168.1.10:8080", "[::]:8080", "example.internal:8080", "not-an-address"}
	for _, address := range nonLoopback {
		if isLoopbackAddress(address) {
			t.Fatalf("isLoopbackAddress(%q) = true, want false", address)
		}
	}
}

func TestListenRejectsNonLoopbackWithoutOptIn(t *testing.T) {
	listener, err := listen("127.0.0.1:0", false)
	if err != nil {
		t.Fatalf("listen(loopback) error = %v", err)
	}
	_ = listener.Close()

	if _, err := listen("0.0.0.0:0", false); err == nil {
		t.Fatal("listen(0.0.0.0:0) without opt-in succeeded, want loopback error")
	}

	listener, err = listen("0.0.0.0:0", true)
	if err != nil {
		t.Fatalf("listen(0.0.0.0:0) with opt-in error = %v", err)
	}
	_ = listener.Close()
}
