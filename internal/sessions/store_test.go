package sessions

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/model"
)

func TestStoreSaveLoadFullMessages(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 2, 3, 4, 5, 0, time.UTC)}
	store := newStoreWithClock(root, clock.Now)

	session := Session{
		ID:           "session-1",
		Provider:     "paperhub",
		ModelProfile: "glm-5.2-fast",
		ModelID:      "glm-5.2",
		ModelParameters: map[string]any{
			"max_tokens":  2048,
			"temperature": 0.2,
		},
		CWD:           `F:\work\simple-agent`,
		ConfigPath:    filepath.Join(root, "..", "custom.yaml"),
		EnabledTools:  []string{"read_file"},
		EnabledMCP:    []string{"local"},
		EnabledSkills: []string{"review"},
		ShowReasoning: true,
		InstructionsSnapshot: []model.Message{
			{Role: model.MessageRoleSystem, Content: "Be concise."},
			{Role: model.MessageRoleDeveloper, Content: "Follow project rules."},
		},
		InstructionSources: []InstructionSource{
			{Role: model.MessageRoleSystem, Source: "sai_builtin"},
			{Role: model.MessageRoleDeveloper, Source: "agents_md", Path: filepath.Join(`F:\work\simple-agent`, "AGENTS.md")},
		},
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "Read docs/checklist.md"},
			{
				Role:    model.MessageRoleAssistant,
				Content: "I'll read it.",
				ToolCalls: []model.ToolCall{
					{ID: "call_read", Name: "read_file", Arguments: `{"path":"docs/checklist.md"}`},
				},
			},
			{Role: model.MessageRoleTool, ToolCallID: "call_read", Content: "M13 checklist body", IsError: false},
			{Role: model.MessageRoleAssistant, Content: "Done."},
		},
		Context: contextwindow.Metadata{
			ContextWindow:           128000,
			ContextWindowSource:     string(contextwindow.WindowSourceConfigured),
			WarningThresholdPercent: contextwindow.WarningThresholdPercent,
			LastRequestTokens:       1000,
			LastInputTokens:         900,
			LastOutputTokens:        50,
			LastTotalTokens:         950,
			LastUsageSource:         string(contextwindow.UsageSourceProvider),
		},
		SaveToolResults: true,
	}

	saved, err := store.Save(session)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if saved.Version != CurrentVersion {
		t.Fatalf("Version = %d, want %d", saved.Version, CurrentVersion)
	}
	if !saved.CreatedAt.Equal(clock.current) {
		t.Fatalf("CreatedAt = %s, want %s", saved.CreatedAt, clock.current)
	}
	if !saved.UpdatedAt.Equal(clock.current) {
		t.Fatalf("UpdatedAt = %s, want %s", saved.UpdatedAt, clock.current)
	}
	if _, err := os.Stat(filepath.Join(root, "session-1", "session.json")); err != nil {
		t.Fatalf("session.json stat error = %v", err)
	}

	loaded, err := store.Load("session-1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.ID != saved.ID || loaded.Provider != saved.Provider || loaded.ModelProfile != saved.ModelProfile || loaded.ModelID != saved.ModelID {
		t.Fatalf("loaded model metadata = %#v, want saved metadata %#v", loaded, saved)
	}
	if loaded.CWD != saved.CWD || loaded.ConfigPath != saved.ConfigPath {
		t.Fatalf("loaded paths = cwd %q config %q, want cwd %q config %q", loaded.CWD, loaded.ConfigPath, saved.CWD, saved.ConfigPath)
	}
	if loaded.RootConfigPath() != saved.ConfigPath {
		t.Fatalf("RootConfigPath() = %q, want %q", loaded.RootConfigPath(), saved.ConfigPath)
	}
	if !reflect.DeepEqual(loaded.EnabledTools, saved.EnabledTools) {
		t.Fatalf("EnabledTools = %#v, want %#v", loaded.EnabledTools, saved.EnabledTools)
	}
	if !reflect.DeepEqual(loaded.EnabledMCP, saved.EnabledMCP) {
		t.Fatalf("EnabledMCP = %#v, want %#v", loaded.EnabledMCP, saved.EnabledMCP)
	}
	if !reflect.DeepEqual(loaded.EnabledSkills, saved.EnabledSkills) {
		t.Fatalf("EnabledSkills = %#v, want %#v", loaded.EnabledSkills, saved.EnabledSkills)
	}
	if !loaded.ShowReasoning {
		t.Fatal("ShowReasoning = false, want true")
	}
	if !reflect.DeepEqual(loaded.InstructionsSnapshot, saved.InstructionsSnapshot) {
		t.Fatalf("InstructionsSnapshot = %#v, want %#v", loaded.InstructionsSnapshot, saved.InstructionsSnapshot)
	}
	if !reflect.DeepEqual(loaded.InstructionSources, saved.InstructionSources) {
		t.Fatalf("InstructionSources = %#v, want %#v", loaded.InstructionSources, saved.InstructionSources)
	}
	if !reflect.DeepEqual(loaded.Messages, saved.Messages) {
		t.Fatalf("Messages = %#v, want %#v", loaded.Messages, saved.Messages)
	}
	if !reflect.DeepEqual(loaded.Context, saved.Context) {
		t.Fatalf("Context = %#v, want %#v", loaded.Context, saved.Context)
	}
	if !loaded.SaveToolResults {
		t.Fatal("SaveToolResults = false, want true")
	}
	if got := loaded.ModelParameters["max_tokens"]; jsonNumberString(got) != "2048" {
		t.Fatalf("max_tokens = %#v, want 2048", got)
	}
}

func TestSessionRootConfigPathFallsBackToLegacyConfigDir(t *testing.T) {
	session := Session{ConfigDir: filepath.Join("project", ".agents")}

	want := filepath.Join("project", ".agents", "sai.yaml")
	if got := session.RootConfigPath(); got != want {
		t.Fatalf("RootConfigPath() = %q, want %q", got, want)
	}
}

func TestStoreListLatestAndDelete(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 2, 3, 4, 5, 0, time.UTC)}
	store := newStoreWithClock(root, clock.Now)

	if _, err := store.Save(Session{ID: "older", Provider: "paperhub", ModelProfile: "glm-5.2", ModelID: "glm-5.2"}); err != nil {
		t.Fatalf("Save(older) error = %v", err)
	}
	clock.current = clock.current.Add(time.Minute)
	if _, err := store.Save(Session{ID: "newer", Provider: "openai", ModelProfile: "default", ModelID: "gpt-5.1"}); err != nil {
		t.Fatalf("Save(newer) error = %v", err)
	}

	infos, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got, want := len(infos), 2; got != want {
		t.Fatalf("len(List()) = %d, want %d: %#v", got, want, infos)
	}
	if infos[0].ID != "newer" || infos[1].ID != "older" {
		t.Fatalf("List() order = %#v, want newest first", infos)
	}

	latest, err := store.Latest()
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if latest.ID != "newer" {
		t.Fatalf("Latest().ID = %q, want newer", latest.ID)
	}

	if err := store.Delete("newer"); err != nil {
		t.Fatalf("Delete(newer) error = %v", err)
	}
	if _, err := store.Load("newer"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load(deleted) error = %v, want ErrNotFound", err)
	}
	latest, err = store.Latest()
	if err != nil {
		t.Fatalf("Latest() after delete error = %v", err)
	}
	if latest.ID != "older" {
		t.Fatalf("Latest().ID after delete = %q, want older", latest.ID)
	}
}

func TestStoreDoesNotCreateDirectoryUntilSave(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewStore(root)

	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session root stat error = %v, want not exist", err)
	}

	infos, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("List() = %#v, want empty", infos)
	}
	if _, err := store.Latest(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Latest() error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session root stat after List/Latest = %v, want not exist", err)
	}
}

func TestStoreListUsesDirectoryIDWhenFileIDMissing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewStore(root)
	writeSessionJSON(t, root, "missing-id", `{
  "created_at": "2026-07-02T03:04:05Z",
  "updated_at": "2026-07-02T03:04:06Z",
  "version": 1,
  "provider": "paperhub",
  "model_profile": "glm-5.2",
  "model_id": "glm-5.2",
  "messages": []
}`)

	infos, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("len(List()) = %d, want 1: %#v", len(infos), infos)
	}
	if infos[0].ID != "missing-id" {
		t.Fatalf("List()[0].ID = %q, want missing-id", infos[0].ID)
	}
}

func TestStoreListRejectsMismatchedFileID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewStore(root)
	writeSessionJSON(t, root, "expected-id", `{
  "id": "other-id",
  "created_at": "2026-07-02T03:04:05Z",
  "updated_at": "2026-07-02T03:04:06Z",
  "version": 1,
  "provider": "paperhub",
  "model_profile": "glm-5.2",
  "model_id": "glm-5.2",
  "messages": []
}`)

	_, err := store.List()
	if err == nil {
		t.Fatal("List() error = nil, want mismatch error")
	}
	if got := err.Error(); !strings.Contains(got, `session file "expected-id" contains id "other-id"`) {
		t.Fatalf("List() error = %q, want mismatched id message", got)
	}
}

type fakeClock struct {
	current time.Time
}

func (c *fakeClock) Now() time.Time {
	return c.current
}

func jsonNumberString(value any) string {
	number, ok := value.(json.Number)
	if !ok {
		return ""
	}
	return number.String()
}

func writeSessionJSON(t *testing.T, root, id, content string) {
	t.Helper()

	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", dir, err)
	}
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
