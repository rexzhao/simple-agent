package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDefaultRegistryPathUsesProjectSpecificFile(t *testing.T) {
	path, err := DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath() error = %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("DefaultRegistryPath() = %q, want absolute path", path)
	}
	if got, want := filepath.Base(path), defaultRegistryFileName; got != want {
		t.Fatalf("DefaultRegistryPath() base = %q, want %q", got, want)
	}
	if got, want := filepath.Base(filepath.Dir(path)), registrySubdirName; got != want {
		t.Fatalf("DefaultRegistryPath() registry dir = %q, want %q", got, want)
	}
	if got, want := filepath.Base(filepath.Dir(filepath.Dir(path))), defaultRegistryDirName; got != want {
		t.Fatalf("DefaultRegistryPath() home dir = %q, want %q", got, want)
	}
}

func TestHomeEnvVarNameFromProgramBasename(t *testing.T) {
	tests := []struct {
		program string
		want    string
	}{
		{program: "sai.exe", want: "SAI_HOME"},
		{program: filepath.Join("bin", "simple-agent.exe"), want: "SIMPLE_AGENT_HOME"},
		{program: "my.tool", want: "MY_TOOL_HOME"},
		{program: "my---tool", want: "MY_TOOL_HOME"},
		{program: "...", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.program, func(t *testing.T) {
			if got := HomeEnvVarName(tt.program); got != tt.want {
				t.Fatalf("HomeEnvVarName(%q) = %q, want %q", tt.program, got, tt.want)
			}
		})
	}
}

func TestResolveHomeDirPriority(t *testing.T) {
	root := t.TempDir()
	defaultRoot := filepath.Join(root, "default")
	envRoot := filepath.Join(root, "env")
	flagRoot := filepath.Join(root, "flag")
	t.Setenv("APPDATA", filepath.Join(defaultRoot, "appdata"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(defaultRoot, "xdg-config"))
	t.Setenv("HOME", filepath.Join(defaultRoot, "home"))
	t.Setenv("SIMPLE_AGENT_HOME", envRoot)

	got, err := ResolveHomeDir("simple-agent.exe", flagRoot)
	if err != nil {
		t.Fatalf("ResolveHomeDir(--home) error = %v", err)
	}
	if got != mustCanonicalPath(t, flagRoot) {
		t.Fatalf("ResolveHomeDir(--home) = %q, want %q", got, mustCanonicalPath(t, flagRoot))
	}

	got, err = ResolveHomeDir("simple-agent.exe", "")
	if err != nil {
		t.Fatalf("ResolveHomeDir(env) error = %v", err)
	}
	if got != mustCanonicalPath(t, envRoot) {
		t.Fatalf("ResolveHomeDir(env) = %q, want %q", got, mustCanonicalPath(t, envRoot))
	}

	got, err = ResolveHomeDir("...", "")
	if err != nil {
		t.Fatalf("ResolveHomeDir(default) error = %v", err)
	}
	wantDefault, err := DefaultHomeDir()
	if err != nil {
		t.Fatalf("DefaultHomeDir() error = %v", err)
	}
	if got != wantDefault {
		t.Fatalf("ResolveHomeDir(default) = %q, want %q", got, wantDefault)
	}
}

func TestRegistryStoreLoadMissingAndCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry", "servers.json")
	store := NewRegistryStore(path)
	gotPath, err := store.RegistryPath()
	if err != nil {
		t.Fatalf("RegistryPath() error = %v", err)
	}
	if gotPath != filepath.Clean(path) {
		t.Fatalf("RegistryPath() = %q, want %q", gotPath, filepath.Clean(path))
	}

	records, err := store.Load()
	if err != nil {
		t.Fatalf("Load() missing file error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("Load() missing file returned %d records, want 0", len(records))
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"not": "a registry"`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, err = store.Load()
	if err == nil {
		t.Fatal("Load() corrupt JSON error = nil, want error")
	}
	if !strings.Contains(err.Error(), "parse server registry") {
		t.Fatalf("Load() corrupt JSON error = %q, want parse server registry", err)
	}
}

func TestRegistryStoreUpsertReplacesSingleton(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	store := NewRegistryStore(path)
	root := t.TempDir()
	project := filepath.Join(root, "project")
	config := filepath.Join(project, ".agents", "sai.yaml")
	otherConfig := filepath.Join(project, ".agents", "other.yaml")

	first := testRegistryRecord(project, config, "127.0.0.1:1001", 1001, "token-one")
	if err := store.Upsert(first); err != nil {
		t.Fatalf("Upsert(first) error = %v", err)
	}
	if err := store.Upsert(testRegistryRecord(project, otherConfig, "127.0.0.1:1002", 1002, "token-other")); err != nil {
		t.Fatalf("Upsert(other) error = %v", err)
	}

	replacement := testRegistryRecord(project+string(filepath.Separator)+".", filepath.Dir(config)+string(filepath.Separator)+"."+string(filepath.Separator)+filepath.Base(config), "127.0.0.1:2001", 2001, "token-two")
	replacement.RequestedListen = "127.0.0.1:8787"
	same, err := SameRegistryIdentity(first, replacement)
	if err != nil {
		t.Fatalf("SameRegistryIdentity() error = %v", err)
	}
	if !same {
		t.Fatal("SameRegistryIdentity() = false, want true for canonical path variants")
	}
	if err := store.Upsert(replacement); err != nil {
		t.Fatalf("Upsert(replacement) error = %v", err)
	}

	records, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("List() returned %d records, want 1 singleton: %#v", len(records), records)
	}
	if records[0].BaseURL != replacement.BaseURL || records[0].PID != replacement.PID || records[0].Token != replacement.Token {
		t.Fatalf("first record = %#v, want replacement values", records[0])
	}
	if records[0].RequestedListen != "127.0.0.1:8787" {
		t.Fatalf("RequestedListen = %q, want replacement requested listen", records[0].RequestedListen)
	}
	wantIdentity, err := NewRegistryIdentity(project, config)
	if err != nil {
		t.Fatalf("NewRegistryIdentity() error = %v", err)
	}
	if !wantIdentity.Matches(records[0]) {
		t.Fatalf("record identity = %#v, want %#v", records[0].Identity(), wantIdentity)
	}
}

func TestRegistryStoreRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	store := NewRegistryStore(path)
	root := t.TempDir()
	project := filepath.Join(root, "project")
	config := filepath.Join(project, ".agents", "sai.yaml")
	otherConfig := filepath.Join(project, ".agents", "other.yaml")

	if err := store.Save([]RegistryRecord{
		testRegistryRecord(project, config, "127.0.0.1:1001", 1001, "token-one"),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	removed, err := store.Remove(project, config)
	if err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if !removed {
		t.Fatal("Remove() removed = false, want true")
	}
	records, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("records after remove = %#v, want empty", records)
	}

	if err := store.Save([]RegistryRecord{
		testRegistryRecord(project, otherConfig, "127.0.0.1:1002", 1002, "token-two"),
	}); err != nil {
		t.Fatalf("Save(other) error = %v", err)
	}
	removed, err = store.Remove(project, config)
	if err != nil {
		t.Fatalf("Remove() missing error = %v", err)
	}
	if removed {
		t.Fatal("Remove() missing removed = true, want false")
	}
}

func TestRegistryStoreSaveReplacesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	store := NewRegistryStore(path)
	root := t.TempDir()
	project := filepath.Join(root, "project")
	config := filepath.Join(project, ".agents", "sai.yaml")

	if err := store.Save([]RegistryRecord{
		testRegistryRecord(project, config, "127.0.0.1:1001", 1001, "token-one"),
	}); err != nil {
		t.Fatalf("Save(first) error = %v", err)
	}
	if err := store.Save([]RegistryRecord{
		testRegistryRecord(project, config, "127.0.0.1:2002", 2002, "token-two"),
	}); err != nil {
		t.Fatalf("Save(replacement) error = %v", err)
	}

	records, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("Load() returned %d records, want 1: %#v", len(records), records)
	}
	if records[0].BaseURL != "127.0.0.1:2002" || records[0].PID != 2002 || records[0].Token != "token-two" {
		t.Fatalf("record after replacement = %#v, want second Save contents", records[0])
	}
}

func TestRegistryStoreSerializesBaseURLField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	store := NewRegistryStore(path)
	root := t.TempDir()
	record := testRegistryRecord(root, filepath.Join(root, ".agents", "sai.yaml"), "127.0.0.1:2222", 2222, "token-base-url")

	if err := store.Save([]RegistryRecord{record}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v; raw=%s", err, raw)
	}
	if got := payload["base_url"]; got != "127.0.0.1:2222" {
		t.Fatalf("base_url = %#v, want host:port value in %s", got, raw)
	}
	if _, ok := payload["addr"]; ok {
		t.Fatalf("registry JSON contains old addr field: %s", raw)
	}
	for _, key := range []string{"pid", "base_url", "token", "version", "started_at"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("registry JSON missing %q field: %s", key, raw)
		}
	}

	records, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(records) != 1 || records[0].BaseURL != "127.0.0.1:2222" {
		t.Fatalf("Load() records = %#v, want base_url loaded", records)
	}

	fromJSON := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(fromJSON, raw, 0o600); err != nil {
		t.Fatalf("WriteFile(base_url registry) error = %v", err)
	}
	loaded, err := NewRegistryStore(fromJSON).Load()
	if err != nil {
		t.Fatalf("Load(base_url JSON) error = %v", err)
	}
	if len(loaded) != 1 || loaded[0].BaseURL != "127.0.0.1:2222" {
		t.Fatalf("Load(base_url JSON) = %#v, want base_url record", loaded)
	}
}

func TestRegistryPathComparisonUsesWindowsCaseInsensitivity(t *testing.T) {
	if !sameRegistryPath(filepath.Join("same", "path"), filepath.Join("same", "path")) {
		t.Fatal("sameRegistryPath() = false for identical paths")
	}

	got := sameRegistryPath(`C:\Work\Project`, `c:\work\project`)
	want := runtime.GOOS == "windows"
	if got != want {
		t.Fatalf("sameRegistryPath() = %v for case-only difference, want %v on %s", got, want, runtime.GOOS)
	}

	identity := RegistryIdentity{
		CWD:        `C:\Work\Project`,
		ConfigPath: `C:\Work\Project\.agents\sai.yaml`,
	}
	record := RegistryRecord{
		CWD:        `c:\work\project`,
		ConfigPath: `c:\work\project\.agents\sai.yaml`,
	}
	if got := identity.Matches(record); got != want {
		t.Fatalf("RegistryIdentity.Matches() = %v for case-only difference, want %v on %s", got, want, runtime.GOOS)
	}
}

func TestRegistryStoreSaveUsesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits for this check")
	}

	path := filepath.Join(t.TempDir(), "registry", "servers.json")
	store := NewRegistryStore(path)
	if err := store.Save([]RegistryRecord{}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(file) error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 0600", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat(dir) error = %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode = %o, want 0700", got)
	}
}

func TestGenerateRegistryTokenShapeAndRandomness(t *testing.T) {
	first, err := GenerateRegistryToken()
	if err != nil {
		t.Fatalf("GenerateRegistryToken() first error = %v", err)
	}
	second, err := GenerateRegistryToken()
	if err != nil {
		t.Fatalf("GenerateRegistryToken() second error = %v", err)
	}
	if first == second {
		t.Fatalf("GenerateRegistryToken() returned duplicate token %q", first)
	}
	tokenPattern := regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	for _, token := range []string{first, second} {
		if !tokenPattern.MatchString(token) {
			t.Fatalf("token %q does not match raw URL-safe 32-byte shape", token)
		}
	}
}

func TestNearestAncestorRecord(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	child := filepath.Join(project, "internal", "server")
	unrelated := filepath.Join(root, "other")

	rootRecord := testRegistryRecord(root, filepath.Join(root, ".agents", "sai.yaml"), "127.0.0.1:1001", 1001, "root")
	projectRecord := testRegistryRecord(project, filepath.Join(project, ".agents", "sai.yaml"), "127.0.0.1:1002", 1002, "project")
	unrelatedRecord := testRegistryRecord(unrelated, filepath.Join(unrelated, ".agents", "sai.yaml"), "127.0.0.1:1003", 1003, "other")

	records := []RegistryRecord{rootRecord, unrelatedRecord, projectRecord}
	nearest, ok, err := NearestAncestorRecord(child, records)
	if err != nil {
		t.Fatalf("NearestAncestorRecord() error = %v", err)
	}
	if !ok {
		t.Fatal("NearestAncestorRecord() ok = false, want true")
	}
	if nearest.Token != "project" {
		t.Fatalf("nearest record = %#v, want project record", nearest)
	}

	matches, err := AncestorRecords(child, records)
	if err != nil {
		t.Fatalf("AncestorRecords() error = %v", err)
	}
	if len(matches) != 2 || matches[0].Token != "project" || matches[1].Token != "root" {
		t.Fatalf("AncestorRecords() = %#v, want project then root", matches)
	}
}

func TestDiscoverHealthyChecksSingletonAndRemovesStale(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	child := filepath.Join(project, "internal", "cli")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("MkdirAll(child) error = %v", err)
	}

	process, err := Start(Options{
		CWD:        root,
		ConfigPath: filepath.Join(root, ".agents", "sai.yaml"),
		Listen:     "127.0.0.1:0",
		Version:    "test-version",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- process.Serve(context.Background())
	}()
	defer func() {
		_ = process.Shutdown(context.Background())
		select {
		case err := <-serveDone:
			if err != nil {
				t.Fatalf("Serve() error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Serve() did not stop")
		}
	}()
	waitForHealthyServer(t, process.Addr())

	store := NewRegistryStore(filepath.Join(t.TempDir(), "servers.json"))
	rootRecord := testRegistryRecord(root, filepath.Join(root, ".agents", "sai.yaml"), process.Addr(), os.Getpid(), "root")
	projectRecord := testRegistryRecord(project, filepath.Join(project, ".agents", "sai.yaml"), "127.0.0.1:0", 999999, "stale")
	if err := store.Save([]RegistryRecord{projectRecord}); err != nil {
		t.Fatalf("Save(stale) error = %v", err)
	}

	result, err := DiscoverHealthy(context.Background(), store, child, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("DiscoverHealthy(stale) error = %v", err)
	}
	if result.Found {
		t.Fatal("DiscoverHealthy(stale) found = true, want false")
	}
	if result.StaleRemoved != 1 {
		t.Fatalf("DiscoverHealthy(stale) StaleRemoved = %d, want 1", result.StaleRemoved)
	}
	records, err := store.List()
	if err != nil {
		t.Fatalf("List() after stale discovery error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("registry records after stale discovery = %#v, want empty", records)
	}

	if err := store.Save([]RegistryRecord{rootRecord}); err != nil {
		t.Fatalf("Save(root) error = %v", err)
	}

	result, err = DiscoverHealthy(context.Background(), store, child, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("DiscoverHealthy() error = %v", err)
	}
	if !result.Found {
		t.Fatal("DiscoverHealthy() found = false, want true")
	}
	if result.Record.Token != "root" {
		t.Fatalf("DiscoverHealthy() record = %#v, want root server", result.Record)
	}
	if result.StaleRemoved != 0 {
		t.Fatalf("DiscoverHealthy() StaleRemoved = %d, want 0", result.StaleRemoved)
	}

	records, err = store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(records) != 1 || records[0].Token != "root" {
		t.Fatalf("registry records after discovery = %#v, want root only", records)
	}
}

func testRegistryRecord(cwd, configPath, addr string, pid int, token string) RegistryRecord {
	return RegistryRecord{
		CWD:        cwd,
		ConfigPath: configPath,
		BaseURL:    addr,
		PID:        pid,
		Token:      token,
		StartedAt:  time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC),
		Version:    "test-version",
	}
}

func waitForHealthyServer(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := CheckHealth(context.Background(), addr, 100*time.Millisecond); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server %s did not become healthy", addr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func mustCanonicalPath(t *testing.T, path string) string {
	t.Helper()

	canonical, err := CanonicalPath(path)
	if err != nil {
		t.Fatalf("CanonicalPath(%q) error = %v", path, err)
	}
	return canonical
}
