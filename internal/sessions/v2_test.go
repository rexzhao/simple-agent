package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/model"
)

func TestRootForHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "nested", "..", "home")
	root, err := RootForHome(home)
	if err != nil {
		t.Fatalf("RootForHome(%q) error = %v", home, err)
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		t.Fatalf("Abs(%q) error = %v", home, err)
	}
	if got, want := root, filepath.Join(filepath.Clean(abs), "data", "sessions"); got != want {
		t.Fatalf("RootForHome(%q) = %q, want %q", home, got, want)
	}

	if _, err := RootForHome(" "); err == nil {
		t.Fatal("RootForHome(blank) error = nil, want error")
	}
}

func TestV2StoreSessionWriteLockBlocksOtherProcesses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)
	if _, err := store.SaveMetadata(SessionV2{
		ID:              "session-1",
		Provider:        "test",
		ModelProfile:    "default",
		ModelID:         "model",
		SaveToolResults: true,
	}); err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}

	lock, err := store.AcquireSessionWriteLock(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("AcquireSessionWriteLock(parent) error = %v", err)
	}
	if lock.Path() == "" || filepath.Base(lock.Path()) != sessionWriteLockFileName {
		t.Fatalf("lock path = %q, want %s", lock.Path(), sessionWriteLockFileName)
	}

	runSessionWriteLockChild(t, root, "session-1", "blocked")

	if err := lock.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	runSessionWriteLockChild(t, root, "session-1", "acquire")

	if err := lock.Release(); err != nil {
		t.Fatalf("second Release() error = %v", err)
	}
}

func TestV2StoreSessionWriteLockChildProcess(t *testing.T) {
	mode := os.Getenv("SAI_SESSION_WRITE_LOCK_CHILD")
	if mode == "" {
		return
	}
	root := os.Getenv("SAI_SESSION_WRITE_LOCK_ROOT")
	sessionID := os.Getenv("SAI_SESSION_WRITE_LOCK_SESSION")
	store := NewV2Store(root)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	lock, err := store.AcquireSessionWriteLock(ctx, sessionID)
	switch mode {
	case "blocked":
		if !errors.Is(err, context.DeadlineExceeded) {
			if lock != nil {
				_ = lock.Release()
			}
			t.Fatalf("AcquireSessionWriteLock(blocked) error = %v, want deadline exceeded", err)
		}
	case "acquire":
		if err != nil {
			t.Fatalf("AcquireSessionWriteLock(acquire) error = %v", err)
		}
		if err := lock.Release(); err != nil {
			t.Fatalf("Release(child) error = %v", err)
		}
	default:
		t.Fatalf("unknown lock child mode %q", mode)
	}
}

func runSessionWriteLockChild(t *testing.T, root, sessionID, mode string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^TestV2StoreSessionWriteLockChildProcess$", "-test.v")
	cmd.Env = append(os.Environ(),
		"SAI_SESSION_WRITE_LOCK_CHILD="+mode,
		"SAI_SESSION_WRITE_LOCK_ROOT="+root,
		"SAI_SESSION_WRITE_LOCK_SESSION="+sessionID,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("session write lock child mode %s failed: %v\n%s", mode, err, output)
	}
}

func TestV2StoreSaveLoadMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 3, 1, 2, 3, 0, time.UTC)}
	store := newV2StoreWithClock(root, V2StoreOptions{}, clock.Now)

	session := SessionV2{
		ID:           "session-1",
		DisplayName:  "Planning Session",
		Archived:     true,
		LastUsedAt:   time.Date(2026, 7, 3, 0, 59, 0, 0, time.UTC),
		Provider:     "paperhub",
		ModelProfile: "glm-5.2-fast",
		ModelID:      "glm-5.2",
		ModelParameters: map[string]any{
			"max_tokens":  2048,
			"temperature": 0.2,
		},
		CWD:           `F:\work\simple-agent`,
		ProjectID:     "project-123",
		CreatedCWD:    `F:\work\simple-agent\created`,
		ConfigPath:    filepath.Join(root, "..", "custom.yaml"),
		EnabledTools:  []string{"read_file"},
		EnabledMCP:    []string{"local"},
		EnabledSkills: []string{"review"},
		ShowReasoning: true,
		InstructionsSnapshot: []model.Message{
			{
				Role:    model.MessageRoleDeveloper,
				Content: "Follow project rules.",
				ToolCalls: []model.ToolCall{
					{ID: "call-1", Name: "ignored", Arguments: "{}"},
				},
			},
		},
		InstructionSources: []InstructionSource{
			{Role: model.MessageRoleDeveloper, Source: "agents_md", Path: filepath.Join(`F:\work\simple-agent`, "AGENTS.md")},
		},
		Items: []SessionItem{
			{ID: "stale-item", Kind: ItemKindMessage, Message: &model.Message{Role: model.MessageRoleUser, Content: "do not write to meta"}},
		},
		ActiveHistory: []string{"stale-item"},
		Compactions: []CompactionCheckpoint{
			{ID: "stale-compaction", SummaryItemID: "summary-1", ReplacementHistory: []string{"summary-1"}},
		},
		LastSeq: 99,
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

	saved, err := store.SaveMetadata(session)
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	if saved.Version != VersionV2 {
		t.Fatalf("Version = %d, want %d", saved.Version, VersionV2)
	}
	if !saved.CreatedAt.Equal(clock.current) {
		t.Fatalf("CreatedAt = %s, want %s", saved.CreatedAt, clock.current)
	}
	if !saved.UpdatedAt.Equal(clock.current) {
		t.Fatalf("UpdatedAt = %s, want %s", saved.UpdatedAt, clock.current)
	}

	metadataPath := filepath.Join(root, "session-1", "meta.json")
	raw, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("ReadFile(meta.json) error = %v", err)
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("Unmarshal(meta.json) error = %v", err)
	}
	for _, key := range []string{"items", "active_history", "compactions", "last_seq"} {
		if _, ok := metadata[key]; ok {
			t.Fatalf("meta.json contains %q; want metadata only: %s", key, raw)
		}
	}
	for key, want := range map[string]string{
		"display_name": saved.DisplayName,
		"project_id":   saved.ProjectID,
		"created_cwd":  saved.CreatedCWD,
	} {
		var got string
		if err := json.Unmarshal(metadata[key], &got); err != nil {
			t.Fatalf("Unmarshal(meta.json[%s]) error = %v; raw=%s", key, err, raw)
		}
		if got != want {
			t.Fatalf("meta.json[%s] = %q, want %q", key, got, want)
		}
	}
	if _, ok := metadata["archived"]; ok {
		t.Fatalf("meta.json contains legacy archived boolean: %s", raw)
	}
	var archivedAt time.Time
	if err := json.Unmarshal(metadata["archived_at"], &archivedAt); err != nil {
		t.Fatalf("Unmarshal(meta.json[archived_at]) error = %v; raw=%s", err, raw)
	}
	if !archivedAt.Equal(saved.ArchivedAt) {
		t.Fatalf("meta.json[archived_at] = %s, want %s", archivedAt, saved.ArchivedAt)
	}
	var lastUsedAt time.Time
	if err := json.Unmarshal(metadata["last_used_at"], &lastUsedAt); err != nil {
		t.Fatalf("Unmarshal(meta.json[last_used_at]) error = %v; raw=%s", err, raw)
	}
	if !lastUsedAt.Equal(saved.LastUsedAt) {
		t.Fatalf("meta.json[last_used_at] = %s, want %s", lastUsedAt, saved.LastUsedAt)
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
	if loaded.ProjectID != saved.ProjectID || loaded.CreatedCWD != saved.CreatedCWD {
		t.Fatalf("loaded M21 identity = project_id %q created_cwd %q, want %q/%q", loaded.ProjectID, loaded.CreatedCWD, saved.ProjectID, saved.CreatedCWD)
	}
	if loaded.DisplayName != saved.DisplayName || loaded.Archived != saved.Archived || !loaded.LastUsedAt.Equal(saved.LastUsedAt) {
		t.Fatalf("loaded lifecycle metadata = display %q archived %t last_used %s, want %q/%t/%s", loaded.DisplayName, loaded.Archived, loaded.LastUsedAt, saved.DisplayName, saved.Archived, saved.LastUsedAt)
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
	if !reflect.DeepEqual(loaded.Context, saved.Context) {
		t.Fatalf("Context = %#v, want %#v", loaded.Context, saved.Context)
	}
	if !loaded.SaveToolResults {
		t.Fatal("SaveToolResults = false, want true")
	}
	if got := loaded.ModelParameters["max_tokens"]; jsonNumberString(got) != "2048" {
		t.Fatalf("max_tokens = %#v, want 2048", got)
	}
	if len(loaded.Items) != 0 || len(loaded.ActiveHistory) != 0 || len(loaded.Compactions) != 0 || loaded.LastSeq != 0 {
		t.Fatalf("loaded replay state = items %#v active %#v compactions %#v last_seq %d, want empty replay state", loaded.Items, loaded.ActiveHistory, loaded.Compactions, loaded.LastSeq)
	}
}

func TestV2StoreRunningTurnMarkersRecoverInterrupted(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 4, 1, 2, 3, 0, time.UTC)}
	store := newV2StoreWithClock(root, V2StoreOptions{}, clock.Now)
	if _, err := store.SaveMetadata(SessionV2{
		ID:           "session-running",
		Provider:     "codex",
		ModelProfile: "default",
		ModelID:      "gpt-5",
		CWD:          t.TempDir(),
	}); err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}

	running, err := store.MarkTurnRunning("session-running", "turn-000001")
	if err != nil {
		t.Fatalf("MarkTurnRunning() error = %v", err)
	}
	if running.RunningTurnID != "turn-000001" || !running.RunningStartedAt.Equal(clock.current) {
		t.Fatalf("running marker = turn %q at %s, want turn-000001 at %s", running.RunningTurnID, running.RunningStartedAt, clock.current)
	}
	infos, err := store.ListWithOptions(V2ListOptions{All: true})
	if err != nil {
		t.Fatalf("ListWithOptions() error = %v", err)
	}
	if len(infos) != 1 || infos[0].RunningTurnID != "turn-000001" {
		t.Fatalf("list running marker = %#v, want running turn", infos)
	}

	clock.current = clock.current.Add(time.Minute)
	marked, err := store.MarkRunningTurnsInterrupted()
	if err != nil {
		t.Fatalf("MarkRunningTurnsInterrupted() error = %v", err)
	}
	if len(marked) != 1 || marked[0].InterruptedTurnID != "turn-000001" || !marked[0].InterruptedAt.Equal(clock.current) {
		t.Fatalf("marked interrupted = %#v, want one interrupted turn at %s", marked, clock.current)
	}
	loaded, err := store.Load("session-running")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.RunningTurnID != "" || !loaded.RunningStartedAt.IsZero() {
		t.Fatalf("loaded running marker = turn %q at %s, want cleared", loaded.RunningTurnID, loaded.RunningStartedAt)
	}
	if loaded.InterruptedTurnID != "turn-000001" || !loaded.InterruptedAt.Equal(clock.current) {
		t.Fatalf("loaded interrupted marker = turn %q at %s, want turn-000001 at %s", loaded.InterruptedTurnID, loaded.InterruptedAt, clock.current)
	}
}

func TestV2StoreLoadCombinesMetadataWithReplayedState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 3, 1, 2, 3, 0, time.UTC)}
	store := newV2StoreWithClock(root, V2StoreOptions{}, clock.Now)

	if _, err := store.SaveMetadata(SessionV2{
		ID:            "session-1",
		Provider:      "paperhub",
		ModelProfile:  "glm-5.2-fast",
		ModelID:       "glm-5.2",
		Items:         []SessionItem{{ID: "stale-item"}},
		ActiveHistory: []string{"stale-item"},
		Compactions:   []CompactionCheckpoint{{ID: "stale-compaction"}},
		LastSeq:       99,
	}); err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}

	appendTestItem(t, store, "session-1", "item-1", "one")
	appendTestItem(t, store, "session-1", "item-2", "two")
	if _, err := store.ReplaceActiveHistory("session-1", []string{"item-2"}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}
	checkpoint, err := store.AppendCompaction("session-1", CompactionCheckpoint{
		ID:                 "compact-1",
		Reason:             "user_requested",
		Phase:              "manual",
		Trigger:            "manual",
		SummaryItemID:      "summary-1",
		ReplacementHistory: []string{"item-2"},
	})
	if err != nil {
		t.Fatalf("AppendCompaction() error = %v", err)
	}

	loaded, err := store.Load("session-1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Provider != "paperhub" || loaded.ModelID != "glm-5.2" {
		t.Fatalf("loaded metadata provider/model = %q/%q, want paperhub/glm-5.2", loaded.Provider, loaded.ModelID)
	}
	if got := itemIDs(loaded.Items); !reflect.DeepEqual(got, []string{"item-1", "item-2"}) {
		t.Fatalf("loaded item IDs = %#v, want replayed items", got)
	}
	if !reflect.DeepEqual(loaded.ActiveHistory, []string{"item-2"}) {
		t.Fatalf("ActiveHistory = %#v, want replayed active history", loaded.ActiveHistory)
	}
	if !reflect.DeepEqual(loaded.Compactions, []CompactionCheckpoint{checkpoint}) {
		t.Fatalf("Compactions = %#v, want %#v", loaded.Compactions, []CompactionCheckpoint{checkpoint})
	}
	if loaded.LastSeq != 4 {
		t.Fatalf("LastSeq = %d, want 4", loaded.LastSeq)
	}

	messages, err := loaded.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if got := messageContents(messages); !reflect.DeepEqual(got, []string{"two"}) {
		t.Fatalf("active messages = %#v, want item-2 content only", got)
	}
}

func TestV2StoreListLatestAndDeletePreservesBlobs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 3, 1, 2, 3, 0, time.UTC)}
	store := newV2StoreWithClock(root, V2StoreOptions{}, clock.Now)

	blob, err := store.WriteBlob([]byte("shared tool result"), "utf-8", "text/plain")
	if err != nil {
		t.Fatalf("WriteBlob() error = %v", err)
	}
	if _, err := store.SaveMetadata(SessionV2{ID: "older", Provider: "paperhub", ModelProfile: "glm-5.2", ModelID: "glm-5.2"}); err != nil {
		t.Fatalf("SaveMetadata(older) error = %v", err)
	}
	clock.current = clock.current.Add(time.Minute)
	if _, err := store.SaveMetadata(SessionV2{ID: "newer", Provider: "openai", ModelProfile: "default", ModelID: "gpt-5.1"}); err != nil {
		t.Fatalf("SaveMetadata(newer) error = %v", err)
	}
	appendTestItem(t, store, "segments-only", "item-1", "not listable without metadata")

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
	if _, err := store.ReadBlob(blob); err != nil {
		t.Fatalf("ReadBlob() after Delete(newer) error = %v, want blob preserved", err)
	}
	latest, err = store.Latest()
	if err != nil {
		t.Fatalf("Latest() after delete error = %v", err)
	}
	if latest.ID != "older" {
		t.Fatalf("Latest().ID after delete = %q, want older", latest.ID)
	}
}

func TestV2StoreListSkipsCorruptMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)
	if _, err := store.SaveMetadata(SessionV2{ID: "valid", Provider: "test", ModelID: "model"}); err != nil {
		t.Fatalf("SaveMetadata(valid) error = %v", err)
	}
	corruptDir := filepath.Join(root, "corrupt")
	if err := os.MkdirAll(corruptDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(corrupt) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "meta.json"), nil, 0o600); err != nil {
		t.Fatalf("WriteFile(corrupt metadata) error = %v", err)
	}

	infos, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got, want := sessionInfoIDs(infos), []string{"valid"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("List() IDs = %#v, want %#v", got, want)
	}
	if _, err := store.Load("corrupt"); !errors.Is(err, ErrCorruptedSession) {
		t.Fatalf("Load(corrupt) error = %v, want ErrCorruptedSession", err)
	}
}

func TestV2StoreSaveMetadataUsesCleanReplacement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)
	if _, err := store.SaveMetadata(SessionV2{ID: "session-1", DisplayName: "before"}); err != nil {
		t.Fatalf("SaveMetadata(before) error = %v", err)
	}
	if _, err := store.SaveMetadata(SessionV2{ID: "session-1", DisplayName: "after"}); err != nil {
		t.Fatalf("SaveMetadata(after) error = %v", err)
	}

	loaded, err := store.Load("session-1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.DisplayName != "after" {
		t.Fatalf("DisplayName = %q, want after", loaded.DisplayName)
	}
	temps, err := filepath.Glob(filepath.Join(root, "session-1", ".meta-*.tmp"))
	if err != nil {
		t.Fatalf("Glob(metadata temp files) error = %v", err)
	}
	if len(temps) != 0 {
		t.Fatalf("metadata temp files = %#v, want none", temps)
	}
}

func TestV2StoreListFiltersArchivedAndSortsByLastUsed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 3, 1, 2, 3, 0, time.UTC)}
	store := newV2StoreWithClock(root, V2StoreOptions{}, clock.Now)

	createdAt := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	writeLegacyV2Metadata(t, root, "legacy-fallback", createdAt, createdAt.Add(6*time.Minute))
	if _, err := store.SaveMetadata(SessionV2{
		ID:           "last-used-newest",
		CreatedAt:    createdAt,
		LastUsedAt:   createdAt.Add(5 * time.Minute),
		Provider:     "paperhub",
		ModelProfile: "glm-5.2",
		ModelID:      "glm-5.2",
	}); err != nil {
		t.Fatalf("SaveMetadata(last-used-newest) error = %v", err)
	}
	if _, err := store.SaveMetadata(SessionV2{
		ID:           "created-tie-newer",
		CreatedAt:    createdAt.Add(2 * time.Minute),
		LastUsedAt:   createdAt.Add(4 * time.Minute),
		Provider:     "paperhub",
		ModelProfile: "glm-5.2",
		ModelID:      "glm-5.2",
	}); err != nil {
		t.Fatalf("SaveMetadata(created-tie-newer) error = %v", err)
	}
	if _, err := store.SaveMetadata(SessionV2{
		ID:           "created-tie-older",
		CreatedAt:    createdAt.Add(time.Minute),
		LastUsedAt:   createdAt.Add(4 * time.Minute),
		Provider:     "paperhub",
		ModelProfile: "glm-5.2",
		ModelID:      "glm-5.2",
	}); err != nil {
		t.Fatalf("SaveMetadata(created-tie-older) error = %v", err)
	}
	if _, err := store.SaveMetadata(SessionV2{
		ID:           "archived-session",
		CreatedAt:    createdAt,
		LastUsedAt:   createdAt.Add(7 * time.Minute),
		Archived:     true,
		Provider:     "paperhub",
		ModelProfile: "glm-5.2",
		ModelID:      "glm-5.2",
	}); err != nil {
		t.Fatalf("SaveMetadata(archived-session) error = %v", err)
	}

	infos, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got, want := sessionInfoIDs(infos), []string{"legacy-fallback", "last-used-newest", "created-tie-newer", "created-tie-older"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("List() IDs = %#v, want %#v", got, want)
	}

	archived, err := store.ListWithOptions(V2ListOptions{Archived: true})
	if err != nil {
		t.Fatalf("ListWithOptions(archived) error = %v", err)
	}
	if got, want := sessionInfoIDs(archived), []string{"archived-session"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("archived List IDs = %#v, want %#v", got, want)
	}

	legacy, err := store.Load("legacy-fallback")
	if err != nil {
		t.Fatalf("Load(legacy-fallback) error = %v", err)
	}
	if want := createdAt.Add(6 * time.Minute); !legacy.LastUsedAt.Equal(want) {
		t.Fatalf("legacy LastUsedAt = %s, want fallback updated_at %s", legacy.LastUsedAt, want)
	}
}

func TestV2StoreSaveTurnUpdatesLastUsedAt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 3, 1, 2, 3, 0, time.UTC)}
	store := newV2StoreWithClock(root, V2StoreOptions{}, clock.Now)

	session, err := store.SaveMetadata(SessionV2{
		ID:           "turn-session",
		Provider:     "paperhub",
		ModelProfile: "glm-5.2",
		ModelID:      "glm-5.2",
	})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	if !session.LastUsedAt.Equal(clock.current) {
		t.Fatalf("initial LastUsedAt = %s, want %s", session.LastUsedAt, clock.current)
	}

	clock.current = clock.current.Add(5 * time.Minute)
	userItem := SessionItem{
		ID:         "msg-000001",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "hello"},
	}
	assistantItem := SessionItem{
		ID:         "msg-000002",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "hi"},
	}
	saved, err := store.SaveTurn(session, []SessionItem{userItem, assistantItem}, []string{userItem.ID, assistantItem.ID})
	if err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if !saved.LastUsedAt.Equal(clock.current) || !saved.UpdatedAt.Equal(clock.current) {
		t.Fatalf("turn timestamps = updated %s last_used %s, want %s", saved.UpdatedAt, saved.LastUsedAt, clock.current)
	}
}

func TestV2StoreLoadMissingMetadataReturnsNotFound(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)

	appendTestItem(t, store, "session-1", "item-1", "segment without metadata")

	if _, err := store.Load("session-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load() error = %v, want ErrNotFound", err)
	}
}

func TestV2StoreRejectsReservedBlobsSessionIDAndPreservesBlobStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)

	blob, err := store.WriteBlob([]byte("shared tool result"), "utf-8", "text/plain")
	if err != nil {
		t.Fatalf("WriteBlob() error = %v", err)
	}

	reservedIDs := []string{
		v2BlobsDirName,
		strings.ToUpper(v2BlobsDirName),
		v2BlobsDirName + ".",
		strings.ToUpper(v2BlobsDirName) + ".",
		v2BlobsDirName + "..",
	}
	operations := []struct {
		name string
		run  func(string) error
	}{
		{
			name: "SaveMetadata",
			run: func(id string) error {
				_, err := store.SaveMetadata(SessionV2{ID: id})
				return err
			},
		},
		{
			name: "Load",
			run: func(id string) error {
				_, err := store.Load(id)
				return err
			},
		},
		{
			name: "Replay",
			run: func(id string) error {
				_, err := store.Replay(id)
				return err
			},
		},
		{
			name: "AppendItem",
			run: func(id string) error {
				_, err := store.AppendItem(id, SessionItem{
					ID:         "item-1",
					Kind:       ItemKindMessage,
					Visibility: ItemVisibilityVisible,
					Audience:   ItemAudienceUser,
					Message:    &model.Message{Role: model.MessageRoleUser, Content: "blocked"},
				})
				return err
			},
		},
		{
			name: "ReplaceActiveHistory",
			run: func(id string) error {
				_, err := store.ReplaceActiveHistory(id, []string{"item-1"})
				return err
			},
		},
		{
			name: "AppendCompaction",
			run: func(id string) error {
				_, err := store.AppendCompaction(id, CompactionCheckpoint{
					ID:                 "compact-1",
					SummaryItemID:      "summary-1",
					ReplacementHistory: []string{"summary-1"},
				})
				return err
			},
		},
		{
			name: "AppendCompactionCheckpoint",
			run: func(id string) error {
				_, err := store.AppendCompactionCheckpoint(id, SessionItem{
					ID:         "summary-1",
					Kind:       ItemKindMessage,
					Visibility: ItemVisibilityHidden,
					Audience:   ItemAudienceModel,
					Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "summary"},
				}, CompactionCheckpoint{
					ID:                 "compact-1",
					SummaryItemID:      "summary-1",
					ReplacementHistory: []string{"summary-1"},
				})
				return err
			},
		},
		{
			name: "Delete",
			run: func(id string) error {
				return store.Delete(id)
			},
		},
	}
	for _, id := range reservedIDs {
		for _, operation := range operations {
			t.Run(operation.name+"/"+id, func(t *testing.T) {
				requireReservedV2SessionIDError(t, operation.run(id))
			})
		}
	}

	for _, id := range reservedIDs {
		if _, err := os.Stat(filepath.Join(root, id, "meta.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reserved namespace %q meta.json stat error = %v, want not exist", id, err)
		}
		if _, err := os.Stat(filepath.Join(root, id, "segments")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reserved namespace %q segments stat error = %v, want not exist", id, err)
		}
	}
	if _, err := store.ReadBlob(blob); err != nil {
		t.Fatalf("ReadBlob() after reserved id operations error = %v", err)
	}

	anotherBlob, err := store.WriteBlob([]byte("another shared tool result"), "utf-8", "text/plain")
	if err != nil {
		t.Fatalf("WriteBlob(second) error = %v", err)
	}
	if _, err := store.ReadBlob(anotherBlob); err != nil {
		t.Fatalf("ReadBlob(second) error = %v", err)
	}
}

func TestV2StoreAppendItemsReplayBySeq(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 3, 1, 2, 3, 0, time.UTC)}
	store := newV2StoreWithClock(root, V2StoreOptions{}, clock.Now)

	first, err := store.AppendItem("session-1", SessionItem{
		ID:         "item-1",
		TurnID:     "turn-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "hello"},
	})
	if err != nil {
		t.Fatalf("AppendItem(first) error = %v", err)
	}
	second, err := store.AppendItem("session-1", SessionItem{
		ID:         "item-2",
		TurnID:     "turn-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "hi"},
	})
	if err != nil {
		t.Fatalf("AppendItem(second) error = %v", err)
	}

	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("seqs = %d, %d; want 1, 2", first.Seq, second.Seq)
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if replayed.LastSeq != 2 {
		t.Fatalf("LastSeq = %d, want 2", replayed.LastSeq)
	}
	if got := []string{replayed.Items[0].ID, replayed.Items[1].ID}; !reflect.DeepEqual(got, []string{"item-1", "item-2"}) {
		t.Fatalf("replayed item order = %#v, want item-1,item-2", got)
	}
	if replayed.Items[0].Seq != 1 || replayed.Items[1].Seq != 2 {
		t.Fatalf("replayed seqs = %d, %d; want 1, 2", replayed.Items[0].Seq, replayed.Items[1].Seq)
	}
}

func TestV2StoreAppendItemsAndReplaceActiveHistoryCommitsTransaction(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 3, 1, 2, 3, 0, time.UTC)}
	store := newV2StoreWithClock(root, V2StoreOptions{}, clock.Now)

	replayed, err := store.AppendItemsAndReplaceActiveHistory("session-1", []SessionItem{
		{
			ID:         "item-1",
			Kind:       ItemKindMessage,
			Visibility: ItemVisibilityVisible,
			Audience:   ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "hello"},
		},
		{
			ID:         "item-2",
			Kind:       ItemKindMessage,
			Visibility: ItemVisibilityVisible,
			Audience:   ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "hi"},
		},
	}, []string{"item-1", "item-2"})
	if err != nil {
		t.Fatalf("AppendItemsAndReplaceActiveHistory() error = %v", err)
	}

	if got := itemIDs(replayed.Items); !reflect.DeepEqual(got, []string{"item-1", "item-2"}) {
		t.Fatalf("item IDs = %#v, want committed items", got)
	}
	if !reflect.DeepEqual(replayed.ActiveHistory, []string{"item-1", "item-2"}) {
		t.Fatalf("ActiveHistory = %#v, want committed replacement", replayed.ActiveHistory)
	}
	if replayed.Items[0].Seq != 2 || replayed.Items[1].Seq != 3 || replayed.LastSeq != 5 {
		t.Fatalf("seqs = items %d/%d last %d, want 2/3 last 5", replayed.Items[0].Seq, replayed.Items[1].Seq, replayed.LastSeq)
	}
}

func TestV2StoreAppendItemsAndReplaceActiveHistoryFromStateAdvancesCachedState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC)}
	store := newV2StoreWithClock(root, V2StoreOptions{}, clock.Now)

	state, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	state, err = store.AppendItemsAndReplaceActiveHistoryFromState("session-1", state, []SessionItem{
		{
			ID:         "user-1",
			Kind:       ItemKindMessage,
			Visibility: ItemVisibilityVisible,
			Audience:   ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "hello"},
		},
	}, []string{"user-1"})
	if err != nil {
		t.Fatalf("AppendItemsAndReplaceActiveHistoryFromState(first) error = %v", err)
	}
	state, err = store.AppendItemsAndReplaceActiveHistoryFromState("session-1", state, []SessionItem{
		{
			ID:         "assistant-1",
			Kind:       ItemKindMessage,
			Visibility: ItemVisibilityVisible,
			Audience:   ItemAudienceModel,
			Message: &model.Message{
				Role: model.MessageRoleAssistant,
				ToolCalls: []model.ToolCall{
					{ID: "call-1", Name: "read", Arguments: "{}"},
				},
			},
		},
		{
			ID:         "tool-1",
			Kind:       ItemKindMessage,
			Visibility: ItemVisibilityVisible,
			Audience:   ItemAudienceModel,
			Status:     ItemStatusPending,
			Message:    &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1"},
		},
	}, []string{"user-1", "assistant-1", "tool-1"})
	if err != nil {
		t.Fatalf("AppendItemsAndReplaceActiveHistoryFromState(second) error = %v", err)
	}

	if got := itemIDs(state.Items); !reflect.DeepEqual(got, []string{"user-1", "assistant-1", "tool-1"}) {
		t.Fatalf("cached item IDs = %#v, want appended items", got)
	}
	if !reflect.DeepEqual(state.ActiveHistory, []string{"user-1", "assistant-1", "tool-1"}) {
		t.Fatalf("cached ActiveHistory = %#v, want latest replacement", state.ActiveHistory)
	}
	if state.Items[0].Seq != 2 || state.Items[1].Seq != 6 || state.Items[2].Seq != 7 || state.LastSeq != 9 {
		t.Fatalf("cached seqs user/assistant/tool/last = %d/%d/%d/%d, want 2/6/7/9", state.Items[0].Seq, state.Items[1].Seq, state.Items[2].Seq, state.LastSeq)
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() after cached writes error = %v", err)
	}
	if !reflect.DeepEqual(replayed.Items, state.Items) {
		t.Fatalf("replayed items = %#v, want cached %#v", replayed.Items, state.Items)
	}
	if !reflect.DeepEqual(replayed.ActiveHistory, state.ActiveHistory) || replayed.LastSeq != state.LastSeq {
		t.Fatalf("replayed state = active %#v last %d, want active %#v last %d", replayed.ActiveHistory, replayed.LastSeq, state.ActiveHistory, state.LastSeq)
	}
}

func TestV2StoreUpdateItemReplaysAndPreservesBirthSeq(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 4, 1, 2, 3, 0, time.UTC)}
	store := newV2StoreWithClock(root, V2StoreOptions{}, clock.Now)

	toolItem, err := store.AppendItem("session-1", SessionItem{
		ID:         "tool-1",
		TurnID:     "turn-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceModel,
		Status:     ItemStatusPending,
		Message:    &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1"},
	})
	if err != nil {
		t.Fatalf("AppendItem(tool) error = %v", err)
	}
	if _, err := store.AppendItem("session-1", SessionItem{
		ID:         "user-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "next"},
	}); err != nil {
		t.Fatalf("AppendItem(user) error = %v", err)
	}

	updated, err := store.UpdateItem("session-1", SessionItem{
		ID:      "tool-1",
		Status:  ItemStatusCompleted,
		Message: &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1", Content: "tool output"},
	})
	if err != nil {
		t.Fatalf("UpdateItem() error = %v", err)
	}
	if updated.Seq != toolItem.Seq || updated.CreatedAt != toolItem.CreatedAt {
		t.Fatalf("updated seq/created_at = %d/%s, want %d/%s", updated.Seq, updated.CreatedAt, toolItem.Seq, toolItem.CreatedAt)
	}
	if updated.TurnID != "turn-1" || updated.Kind != ItemKindMessage || updated.Visibility != ItemVisibilityVisible || updated.Audience != ItemAudienceModel {
		t.Fatalf("updated immutable metadata = turn %q kind %q visibility %q audience %q", updated.TurnID, updated.Kind, updated.Visibility, updated.Audience)
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if got := itemIDs(replayed.Items); !reflect.DeepEqual(got, []string{"tool-1", "user-1"}) {
		t.Fatalf("item order = %#v, want unchanged order", got)
	}
	if replayed.Items[0].Seq != 1 || replayed.Items[1].Seq != 2 || replayed.LastSeq != 3 {
		t.Fatalf("seqs tool/user/last = %d/%d/%d, want 1/2/3", replayed.Items[0].Seq, replayed.Items[1].Seq, replayed.LastSeq)
	}
	if got := replayed.Items[0].Message.Content; got != "tool output" {
		t.Fatalf("updated content = %q, want tool output", got)
	}
	if got := replayed.Items[0].Status; got != ItemStatusCompleted {
		t.Fatalf("updated status = %q, want completed", got)
	}
}

func TestV2StoreUpdateItemFromStateAdvancesCachedState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)

	toolItem, err := store.AppendItem("session-1", SessionItem{
		ID:         "tool-1",
		TurnID:     "turn-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceModel,
		Status:     ItemStatusPending,
		Message:    &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1"},
	})
	if err != nil {
		t.Fatalf("AppendItem() error = %v", err)
	}
	state, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}

	updated, state, err := store.UpdateItemFromState("session-1", state, SessionItem{
		ID:      "tool-1",
		Status:  ItemStatusCompleted,
		Message: &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1", Content: "done"},
	})
	if err != nil {
		t.Fatalf("UpdateItemFromState() error = %v", err)
	}
	if updated.Seq != toolItem.Seq || updated.CreatedAt != toolItem.CreatedAt {
		t.Fatalf("updated seq/created_at = %d/%s, want %d/%s", updated.Seq, updated.CreatedAt, toolItem.Seq, toolItem.CreatedAt)
	}
	if state.LastSeq != 2 {
		t.Fatalf("cached LastSeq = %d, want update record seq 2", state.LastSeq)
	}
	if len(state.Items) != 1 || state.Items[0].Seq != toolItem.Seq || state.Items[0].Status != ItemStatusCompleted {
		t.Fatalf("cached items = %#v, want updated tool preserving birth seq", state.Items)
	}
	if state.Items[0].Message == nil || state.Items[0].Message.Content != "done" {
		t.Fatalf("cached message = %#v, want updated content", state.Items[0].Message)
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() after update error = %v", err)
	}
	if !reflect.DeepEqual(replayed.Items, state.Items) || replayed.LastSeq != state.LastSeq {
		t.Fatalf("replayed state = items %#v last %d, want cached %#v last %d", replayed.Items, replayed.LastSeq, state.Items, state.LastSeq)
	}
}

func TestV2StoreUpdateItemRejectsUnknownItem(t *testing.T) {
	store := NewV2Store(filepath.Join(t.TempDir(), "sessions"))

	_, err := store.UpdateItem("session-1", SessionItem{
		ID:      "missing",
		Message: &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1", Content: "ignored"},
	})
	if !errors.Is(err, ErrCorruptedSession) {
		t.Fatalf("UpdateItem(missing) error = %v, want ErrCorruptedSession", err)
	}
	if !strings.Contains(err.Error(), `item.updated references missing item "missing"`) {
		t.Fatalf("UpdateItem(missing) error = %q, want missing item detail", err)
	}
}

func TestV2StoreFromStateSessionMismatchDoesNotWrite(t *testing.T) {
	store := NewV2Store(filepath.Join(t.TempDir(), "sessions"))
	if _, err := store.AppendItem("session-1", SessionItem{
		ID:         "item-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "hello"},
	}); err != nil {
		t.Fatalf("AppendItem() error = %v", err)
	}

	state, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	state.ID = "other-session"

	if _, err := store.AppendItemsAndReplaceActiveHistoryFromState("session-1", state, []SessionItem{{
		ID:         "item-2",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "ignored"},
	}}, []string{"item-1", "item-2"}); err == nil {
		t.Fatal("AppendItemsAndReplaceActiveHistoryFromState(mismatch) error = nil, want error")
	}
	if _, _, err := store.UpdateItemFromState("session-1", state, SessionItem{
		ID:      "item-1",
		Message: &model.Message{Role: model.MessageRoleUser, Content: "ignored"},
	}); err == nil {
		t.Fatal("UpdateItemFromState(mismatch) error = nil, want error")
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() after mismatch errors = %v", err)
	}
	if got := itemIDs(replayed.Items); !reflect.DeepEqual(got, []string{"item-1"}) {
		t.Fatalf("items after mismatch errors = %#v, want only original item", got)
	}
	if replayed.LastSeq != 1 {
		t.Fatalf("LastSeq after mismatch errors = %d, want 1", replayed.LastSeq)
	}
}

func TestV2StoreReplayCommittedTransactionWithItemUpdated(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)

	original, err := store.AppendItem("session-1", SessionItem{
		ID:         "tool-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceModel,
		Status:     ItemStatusPending,
		Message:    &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1"},
	})
	if err != nil {
		t.Fatalf("AppendItem() error = %v", err)
	}

	segmentPath := filepath.Join(root, "session-1", "segments", "000001.jsonl")
	appendV2RecordsForTest(t, segmentPath,
		v2Record{Seq: 2, Type: RecordTypeTransactionBegin, TxID: "tx-update"},
		v2Record{
			Seq:  3,
			Type: RecordTypeItemUpdated,
			TxID: "tx-update",
			Item: &SessionItem{
				ID:        "tool-1",
				Seq:       99,
				CreatedAt: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
				Status:    ItemStatusError,
				Message:   &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1", Content: "failed", IsError: true},
			},
		},
		v2Record{Seq: 4, Type: RecordTypeTransactionCommit, TxID: "tx-update"},
	)

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if len(replayed.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1", len(replayed.Items))
	}
	item := replayed.Items[0]
	if item.Seq != original.Seq || item.CreatedAt != original.CreatedAt {
		t.Fatalf("updated seq/created_at = %d/%s, want original %d/%s", item.Seq, item.CreatedAt, original.Seq, original.CreatedAt)
	}
	if item.Kind != ItemKindMessage || item.Visibility != ItemVisibilityVisible || item.Audience != ItemAudienceModel {
		t.Fatalf("immutable metadata = kind %q visibility %q audience %q", item.Kind, item.Visibility, item.Audience)
	}
	if item.Status != ItemStatusError || item.Message == nil || item.Message.Content != "failed" || !item.Message.IsError {
		t.Fatalf("updated item = %#v, want error tool result", item)
	}
	if replayed.LastSeq != 4 {
		t.Fatalf("LastSeq = %d, want 4", replayed.LastSeq)
	}
}

func TestV2StoreReplayItemUpdatedUnknownIDIsCorrupted(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)
	appendTestItem(t, store, "session-1", "item-1", "one")

	segmentPath := filepath.Join(root, "session-1", "segments", "000001.jsonl")
	appendV2RecordsForTest(t, segmentPath, v2Record{
		Seq:  2,
		Type: RecordTypeItemUpdated,
		Item: &SessionItem{
			ID:      "missing",
			Message: &model.Message{Role: model.MessageRoleUser, Content: "ignored"},
		},
	})

	_, err := store.Replay("session-1")
	if !errors.Is(err, ErrCorruptedSession) {
		t.Fatalf("Replay() error = %v, want ErrCorruptedSession", err)
	}
	if !strings.Contains(err.Error(), `item.updated references missing item "missing"`) {
		t.Fatalf("Replay() error = %q, want missing item detail", err)
	}
}

func TestV2StoreUpdateItemBlobifiesLargeMessageContent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)

	if _, err := store.AppendItem("session-1", SessionItem{
		ID:         "tool-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceModel,
		Status:     ItemStatusPending,
		Message:    &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1"},
	}); err != nil {
		t.Fatalf("AppendItem() error = %v", err)
	}
	if _, err := store.ReplaceActiveHistory("session-1", []string{"tool-1"}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}

	largeContent := strings.Repeat("updated blob body ", 300) + "SECRET-UPDATED-BLOB"
	updated, err := store.UpdateItem("session-1", SessionItem{
		ID:      "tool-1",
		Status:  ItemStatusCompleted,
		Message: &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1", Content: largeContent},
	})
	if err != nil {
		t.Fatalf("UpdateItem() error = %v", err)
	}
	if updated.Message == nil || updated.Message.Content != "" {
		t.Fatalf("updated message content = %#v, want blobified empty content", updated.Message)
	}
	if updated.Content == nil || updated.Content.Blob == nil {
		t.Fatalf("updated item content = %#v, want blob ref", updated.Content)
	}

	segmentRaw, err := os.ReadFile(filepath.Join(root, "session-1", "segments", "000001.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile(segment) error = %v", err)
	}
	if bytes.Contains(segmentRaw, []byte("SECRET-UPDATED-BLOB")) || bytes.Contains(segmentRaw, []byte(largeContent)) {
		t.Fatalf("segment stored raw updated blob body: %s", segmentRaw)
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	messages, err := store.MaterializeActiveHistory(replayed)
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if got := messageContents(messages); !reflect.DeepEqual(got, []string{largeContent}) {
		t.Fatalf("materialized messages = %#v, want updated large content", got)
	}
}

func TestV2StorePersistedEventsAfterIncludesItemUpdated(t *testing.T) {
	store := NewV2Store(filepath.Join(t.TempDir(), "sessions"))

	if _, err := store.AppendItem("session-1", SessionItem{
		ID:         "tool-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceModel,
		Status:     ItemStatusPending,
		Message:    &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1"},
	}); err != nil {
		t.Fatalf("AppendItem() error = %v", err)
	}
	if _, err := store.ReplaceActiveHistory("session-1", []string{"tool-1"}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}
	if _, err := store.UpdateItem("session-1", SessionItem{
		ID:      "tool-1",
		Status:  ItemStatusCompleted,
		Message: &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1", Content: "done"},
	}); err != nil {
		t.Fatalf("UpdateItem() error = %v", err)
	}

	events, err := store.PersistedEventsAfter("session-1", 0)
	if err != nil {
		t.Fatalf("PersistedEventsAfter() error = %v", err)
	}
	want := []PersistedEvent{
		{Seq: 1, Type: RecordTypeItemAppended, ItemID: "tool-1"},
		{Seq: 3, Type: RecordTypeItemUpdated, ItemID: "tool-1"},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}

	events, err = store.PersistedEventsAfter("session-1", 1)
	if err != nil {
		t.Fatalf("PersistedEventsAfter(after append) error = %v", err)
	}
	if !reflect.DeepEqual(events, want[1:]) {
		t.Fatalf("events after append = %#v, want update only", events)
	}
}

func TestV2StoreAppendCompactionCheckpointCommitsTransaction(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 3, 4, 5, 6, 0, time.UTC)}
	store := newV2StoreWithClock(root, V2StoreOptions{}, clock.Now)

	appendTestItem(t, store, "session-1", "item-1", "one")
	if _, err := store.ReplaceActiveHistory("session-1", []string{"item-1"}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}

	summary := SessionItem{
		ID:         "summary-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityHidden,
		Audience:   ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "checkpoint"},
	}
	checkpoint := CompactionCheckpoint{
		ID:                    "compact-1",
		Reason:                "user_requested",
		Phase:                 "manual",
		Trigger:               "manual",
		SummaryItemID:         "summary-1",
		PreviousActiveHistory: []string{"item-1"},
		ReplacementHistory:    []string{"item-1", "summary-1"},
		SummaryProvider:       "paperhub",
		SummaryModel:          "glm-5.2",
	}

	replayed, err := store.AppendCompactionCheckpoint("session-1", summary, checkpoint)
	if err != nil {
		t.Fatalf("AppendCompactionCheckpoint() error = %v", err)
	}

	if got := itemIDs(replayed.Items); !reflect.DeepEqual(got, []string{"item-1", "summary-1"}) {
		t.Fatalf("item IDs = %#v, want visible item plus summary", got)
	}
	if replayed.Items[1].Seq != 4 || replayed.Items[1].CreatedAt != clock.current {
		t.Fatalf("summary seq/created_at = %d/%s, want 4/%s", replayed.Items[1].Seq, replayed.Items[1].CreatedAt, clock.current)
	}
	if replayed.Items[1].Visibility != ItemVisibilityHidden || replayed.Items[1].Audience != ItemAudienceModel {
		t.Fatalf("summary visibility/audience = %q/%q, want hidden/model", replayed.Items[1].Visibility, replayed.Items[1].Audience)
	}
	if !reflect.DeepEqual(replayed.ActiveHistory, []string{"item-1", "summary-1"}) {
		t.Fatalf("ActiveHistory = %#v, want replacement history", replayed.ActiveHistory)
	}

	checkpoint.CreatedAt = clock.current
	if !reflect.DeepEqual(replayed.Compactions, []CompactionCheckpoint{checkpoint}) {
		t.Fatalf("Compactions = %#v, want %#v", replayed.Compactions, []CompactionCheckpoint{checkpoint})
	}
	if replayed.LastSeq != 7 {
		t.Fatalf("LastSeq = %d, want 7", replayed.LastSeq)
	}

	messages, err := replayed.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if got := messageContents(messages); !reflect.DeepEqual(got, []string{"one", "checkpoint"}) {
		t.Fatalf("active messages = %#v, want original plus checkpoint", got)
	}
}

func TestV2StoreSaveCompactedTurnCommitsCompactionAndTurnTransaction(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 3, 4, 5, 6, 0, time.UTC)}
	store := newV2StoreWithClock(root, V2StoreOptions{}, clock.Now)

	session, err := store.SaveMetadata(SessionV2{
		ID:              "session-1",
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		SaveToolResults: true,
	})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	appendTestItem(t, store, "session-1", "item-1", "one")
	if _, err := store.ReplaceActiveHistory("session-1", []string{"item-1"}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}

	summary := SessionItem{
		ID:         "summary-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityHidden,
		Audience:   ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "checkpoint"},
	}
	checkpoint := CompactionCheckpoint{
		ID:                    "compact-1",
		Reason:                "context_limit",
		Phase:                 "pre_turn",
		Trigger:               "auto",
		SummaryItemID:         "summary-1",
		PreviousActiveHistory: []string{"item-1"},
		ReplacementHistory:    []string{"item-1", "summary-1"},
	}
	replayed, err := store.SaveCompactedTurn(session, summary, checkpoint, []SessionItem{
		{
			ID:         "item-2",
			Kind:       ItemKindMessage,
			Visibility: ItemVisibilityVisible,
			Audience:   ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "two"},
		},
		{
			ID:         "item-3",
			Kind:       ItemKindMessage,
			Visibility: ItemVisibilityVisible,
			Audience:   ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "three"},
		},
	}, []string{"item-1", "summary-1", "item-2", "item-3"})
	if err != nil {
		t.Fatalf("SaveCompactedTurn() error = %v", err)
	}

	if got := itemIDs(replayed.Items); !reflect.DeepEqual(got, []string{"item-1", "summary-1", "item-2", "item-3"}) {
		t.Fatalf("item IDs = %#v, want original summary and turn items", got)
	}
	if !reflect.DeepEqual(replayed.ActiveHistory, []string{"item-1", "summary-1", "item-2", "item-3"}) {
		t.Fatalf("ActiveHistory = %#v, want compacted history plus turn items", replayed.ActiveHistory)
	}
	if len(replayed.Compactions) != 1 || replayed.Compactions[0].ID != "compact-1" {
		t.Fatalf("Compactions = %#v, want compact-1", replayed.Compactions)
	}
	if replayed.Items[1].Seq != 4 || replayed.Items[2].Seq != 6 || replayed.Items[3].Seq != 7 || replayed.LastSeq != 9 {
		t.Fatalf("seqs summary/user/assistant/last = %d/%d/%d/%d, want 4/6/7/9", replayed.Items[1].Seq, replayed.Items[2].Seq, replayed.Items[3].Seq, replayed.LastSeq)
	}
	messages, err := replayed.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if got := messageContents(messages); !reflect.DeepEqual(got, []string{"one", "checkpoint", "two", "three"}) {
		t.Fatalf("active messages = %#v, want compacted history plus successful turn", got)
	}
}

func TestV2StoreLegacyNoStatusSessionLoadsMaterializesAndCompacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)

	session, err := store.SaveMetadata(SessionV2{
		ID:              "legacy-no-status",
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		SaveToolResults: true,
	})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	legacyIDs := []string{"legacy-user", "legacy-assistant-tool", "legacy-tool", "legacy-final"}
	if _, err := store.AppendItemsAndReplaceActiveHistory(session.ID, []SessionItem{
		{
			ID:         "legacy-user",
			Kind:       ItemKindMessage,
			Visibility: ItemVisibilityVisible,
			Audience:   ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "legacy prompt"},
		},
		{
			ID:         "legacy-assistant-tool",
			Kind:       ItemKindMessage,
			Visibility: ItemVisibilityVisible,
			Audience:   ItemAudienceModel,
			Message: &model.Message{
				Role:    model.MessageRoleAssistant,
				Content: "legacy assistant needs tool",
				ToolCalls: []model.ToolCall{{
					ID:        "call-legacy",
					Name:      "read_file",
					Arguments: `{"path":"legacy.txt"}`,
				}},
			},
		},
		{
			ID:         "legacy-tool",
			Kind:       ItemKindMessage,
			Visibility: ItemVisibilityVisible,
			Audience:   ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-legacy", Content: "legacy tool output"},
		},
		{
			ID:         "legacy-final",
			Kind:       ItemKindMessage,
			Visibility: ItemVisibilityVisible,
			Audience:   ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "legacy final"},
		},
	}, legacyIDs); err != nil {
		t.Fatalf("AppendItemsAndReplaceActiveHistory() error = %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(root, "legacy-no-status", "segments", "000001.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile(legacy segment) error = %v", err)
	}
	if bytes.Contains(raw, []byte(`"status"`)) || bytes.Contains(raw, []byte(RecordTypeItemUpdated)) {
		t.Fatalf("legacy segment contains new status/update records: %s", raw)
	}

	loaded, err := store.Load("legacy-no-status")
	if err != nil {
		t.Fatalf("Load(legacy-no-status) error = %v", err)
	}
	messages, err := loaded.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory(legacy) error = %v", err)
	}
	if got := messageContents(messages); !reflect.DeepEqual(got, []string{"legacy prompt", "legacy assistant needs tool", "legacy tool output", "legacy final"}) {
		t.Fatalf("legacy materialized messages = %#v, want original active history", got)
	}
	if len(messages) != 4 || messages[2].Role != model.MessageRoleTool || messages[2].ToolCallID != "call-legacy" || messages[2].IsError {
		t.Fatalf("legacy tool materialized as %#v, want completed non-error tool result", messages)
	}

	summary := SessionItem{
		ID:         "summary-legacy",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityHidden,
		Audience:   ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "legacy summary"},
	}
	checkpoint := CompactionCheckpoint{
		ID:                    "compact-legacy",
		Reason:                "context_limit",
		Phase:                 "pre_turn",
		Trigger:               "auto",
		SummaryItemID:         "summary-legacy",
		PreviousActiveHistory: legacyIDs,
		ReplacementHistory:    []string{"summary-legacy", "legacy-final"},
	}
	compacted, err := store.SaveCompactedTurn(loaded, summary, checkpoint, nil, []string{"summary-legacy", "legacy-final"})
	if err != nil {
		t.Fatalf("SaveCompactedTurn(legacy) error = %v", err)
	}
	if got := itemIDs(compacted.Items); !reflect.DeepEqual(got, []string{"legacy-user", "legacy-assistant-tool", "legacy-tool", "legacy-final", "summary-legacy"}) {
		t.Fatalf("compacted item IDs = %#v, want original legacy items plus summary", got)
	}
	var legacyTool SessionItem
	for _, item := range compacted.Items {
		if item.ID == "legacy-tool" {
			legacyTool = item
			break
		}
	}
	if legacyTool.ID == "" {
		t.Fatal("legacy tool item missing after compaction")
	}
	if legacyTool.Status != "" {
		t.Fatalf("legacy tool status = %q, want empty legacy status preserved", legacyTool.Status)
	}
	events, err := store.PersistedEventsAfter("legacy-no-status", 0)
	if err != nil {
		t.Fatalf("PersistedEventsAfter(legacy) error = %v", err)
	}
	for _, event := range events {
		if event.Type == RecordTypeItemUpdated {
			t.Fatalf("legacy compaction wrote unexpected item.updated event: %#v", events)
		}
	}
	compactedMessages, err := compacted.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory(compacted legacy) error = %v", err)
	}
	if got := messageContents(compactedMessages); !reflect.DeepEqual(got, []string{"legacy summary", "legacy final"}) {
		t.Fatalf("compacted legacy messages = %#v, want summary plus retained final", got)
	}
}

func TestV2StoreAppendCompactionCheckpointValidatesCheckpointWrite(t *testing.T) {
	baseSummary := SessionItem{
		ID:         "summary-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityHidden,
		Audience:   ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "checkpoint"},
	}
	baseCheckpoint := CompactionCheckpoint{
		ID:                 "compact-1",
		SummaryItemID:      "summary-1",
		ReplacementHistory: []string{"summary-1"},
	}

	tests := []struct {
		name        string
		edit        func(*SessionItem, *CompactionCheckpoint)
		wantMessage string
	}{
		{
			name: "missing summary item id",
			edit: func(summary *SessionItem, checkpoint *CompactionCheckpoint) {
				summary.ID = ""
			},
			wantMessage: "compaction summary item id is required",
		},
		{
			name: "visible summary item",
			edit: func(summary *SessionItem, checkpoint *CompactionCheckpoint) {
				summary.Visibility = ItemVisibilityVisible
			},
			wantMessage: `compaction summary item visibility must be "hidden"`,
		},
		{
			name: "non-model summary audience",
			edit: func(summary *SessionItem, checkpoint *CompactionCheckpoint) {
				summary.Audience = ItemAudienceUser
			},
			wantMessage: `compaction summary item audience must be "model"`,
		},
		{
			name: "non-message summary kind",
			edit: func(summary *SessionItem, checkpoint *CompactionCheckpoint) {
				summary.Kind = ItemKindCompaction
			},
			wantMessage: `compaction summary item kind must be "message"`,
		},
		{
			name: "nil summary message",
			edit: func(summary *SessionItem, checkpoint *CompactionCheckpoint) {
				summary.Message = nil
			},
			wantMessage: "compaction summary item message is required",
		},
		{
			name: "empty summary message content",
			edit: func(summary *SessionItem, checkpoint *CompactionCheckpoint) {
				summary.Message = &model.Message{Role: model.MessageRoleDeveloper, Content: "  "}
			},
			wantMessage: "compaction summary message content is required",
		},
		{
			name: "missing checkpoint id",
			edit: func(summary *SessionItem, checkpoint *CompactionCheckpoint) {
				checkpoint.ID = ""
			},
			wantMessage: "compaction checkpoint id is required",
		},
		{
			name: "missing checkpoint summary item id",
			edit: func(summary *SessionItem, checkpoint *CompactionCheckpoint) {
				checkpoint.SummaryItemID = ""
			},
			wantMessage: "compaction checkpoint summary item id is required",
		},
		{
			name: "mismatched checkpoint summary item id",
			edit: func(summary *SessionItem, checkpoint *CompactionCheckpoint) {
				checkpoint.SummaryItemID = "other-summary"
			},
			wantMessage: "does not match summary item id",
		},
		{
			name: "missing replacement history",
			edit: func(summary *SessionItem, checkpoint *CompactionCheckpoint) {
				checkpoint.ReplacementHistory = nil
			},
			wantMessage: "compaction replacement history is required",
		},
		{
			name: "empty replacement history item id",
			edit: func(summary *SessionItem, checkpoint *CompactionCheckpoint) {
				checkpoint.ReplacementHistory = []string{"summary-1", ""}
			},
			wantMessage: "compaction replacement history contains empty item id",
		},
		{
			name: "replacement history references missing item",
			edit: func(summary *SessionItem, checkpoint *CompactionCheckpoint) {
				checkpoint.ReplacementHistory = []string{"missing-item", "summary-1"}
			},
			wantMessage: `compaction replacement history references missing item id "missing-item"`,
		},
		{
			name: "replacement history missing summary",
			edit: func(summary *SessionItem, checkpoint *CompactionCheckpoint) {
				checkpoint.ReplacementHistory = []string{"item-1"}
			},
			wantMessage: "compaction replacement history must include summary item id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "sessions")
			store := NewV2Store(root)
			summary := baseSummary
			checkpoint := baseCheckpoint
			checkpoint.ReplacementHistory = copyStrings(baseCheckpoint.ReplacementHistory)
			tt.edit(&summary, &checkpoint)

			_, err := store.AppendCompactionCheckpoint("session-1", summary, checkpoint)
			if err == nil {
				t.Fatal("AppendCompactionCheckpoint() error = nil, want validation error")
			}
			if !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("AppendCompactionCheckpoint() error = %q, want %q", err, tt.wantMessage)
			}
			segmentsDir := filepath.Join(root, "session-1", "segments")
			if _, statErr := os.Stat(segmentsDir); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("segments dir stat error = %v, want not exist", statErr)
			}
		})
	}
}

func TestV2StoreAppendCompactionCheckpointRejectsDuplicateSummaryItemID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)
	appendTestItem(t, store, "session-1", "summary-1", "existing item")

	_, err := store.AppendCompactionCheckpoint("session-1", SessionItem{
		ID:         "summary-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityHidden,
		Audience:   ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "checkpoint"},
	}, CompactionCheckpoint{
		ID:                 "compact-1",
		SummaryItemID:      "summary-1",
		ReplacementHistory: []string{"summary-1"},
	})
	if err == nil {
		t.Fatal("AppendCompactionCheckpoint() error = nil, want duplicate summary item error")
	}
	if !strings.Contains(err.Error(), `compaction summary item id "summary-1" already exists`) {
		t.Fatalf("AppendCompactionCheckpoint() error = %q, want duplicate summary detail", err)
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if got := itemIDs(replayed.Items); !reflect.DeepEqual(got, []string{"summary-1"}) {
		t.Fatalf("item IDs after rejected duplicate summary = %#v, want existing only", got)
	}
	if replayed.LastSeq != 1 {
		t.Fatalf("LastSeq after rejected duplicate summary = %d, want 1", replayed.LastSeq)
	}
}

func TestV2StoreAppendCompactionCheckpointRejectsNonMessageReplacementRef(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)
	appendTestItem(t, store, "session-1", "item-1", "one")
	if _, err := store.AppendItem("session-1", SessionItem{
		ID:         "runtime-1",
		Kind:       ItemKindRuntimeContext,
		Visibility: ItemVisibilityHidden,
		Audience:   ItemAudienceInternal,
	}); err != nil {
		t.Fatalf("AppendItem(runtime-1) error = %v", err)
	}

	_, err := store.AppendCompactionCheckpoint("session-1", SessionItem{
		ID:         "summary-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityHidden,
		Audience:   ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "checkpoint"},
	}, CompactionCheckpoint{
		ID:                 "compact-1",
		SummaryItemID:      "summary-1",
		ReplacementHistory: []string{"item-1", "runtime-1", "summary-1"},
	})
	if err == nil {
		t.Fatal("AppendCompactionCheckpoint() error = nil, want non-message replacement ref error")
	}
	if !strings.Contains(err.Error(), `compaction replacement history references item id "runtime-1" without a message`) {
		t.Fatalf("AppendCompactionCheckpoint() error = %q, want non-message ref detail", err)
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if got := itemIDs(replayed.Items); !reflect.DeepEqual(got, []string{"item-1", "runtime-1"}) {
		t.Fatalf("item IDs after rejected non-message ref = %#v, want original items only", got)
	}
	if replayed.LastSeq != 2 {
		t.Fatalf("LastSeq after rejected non-message ref = %d, want 2", replayed.LastSeq)
	}
}

func TestV2StoreReplayIgnoresUncommittedTransaction(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)
	appendTestItem(t, store, "session-1", "item-1", "one")
	if _, err := store.ReplaceActiveHistory("session-1", []string{"item-1"}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}

	segmentPath := filepath.Join(root, "session-1", "segments", "000001.jsonl")
	appendV2RecordsForTest(t, segmentPath,
		v2Record{Seq: 3, Type: RecordTypeTransactionBegin, TxID: "tx-abandoned"},
		v2Record{
			Seq:  4,
			Type: RecordTypeItemAppended,
			TxID: "tx-abandoned",
			Item: &SessionItem{
				ID:         "item-2",
				Seq:        4,
				Kind:       ItemKindMessage,
				Visibility: ItemVisibilityVisible,
				Audience:   ItemAudienceUser,
				Message:    &model.Message{Role: model.MessageRoleUser, Content: "ignored"},
			},
		},
		v2Record{Seq: 5, Type: RecordTypeActiveHistoryReplaced, TxID: "tx-abandoned", ItemIDs: []string{"item-1", "item-2"}},
	)

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if got := itemIDs(replayed.Items); !reflect.DeepEqual(got, []string{"item-1"}) {
		t.Fatalf("item IDs after abandoned transaction = %#v, want original only", got)
	}
	if !reflect.DeepEqual(replayed.ActiveHistory, []string{"item-1"}) || replayed.LastSeq != 2 {
		t.Fatalf("state after abandoned transaction = active %#v last %d, want item-1/2", replayed.ActiveHistory, replayed.LastSeq)
	}

	replayed, err = store.AppendItemsAndReplaceActiveHistory("session-1", []SessionItem{
		{
			ID:         "item-3",
			Kind:       ItemKindMessage,
			Visibility: ItemVisibilityVisible,
			Audience:   ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "three"},
		},
	}, []string{"item-1", "item-3"})
	if err != nil {
		t.Fatalf("AppendItemsAndReplaceActiveHistory(after abandoned) error = %v", err)
	}
	if got := itemIDs(replayed.Items); !reflect.DeepEqual(got, []string{"item-1", "item-3"}) {
		t.Fatalf("item IDs after new transaction = %#v, want original plus new committed item", got)
	}
	if !reflect.DeepEqual(replayed.ActiveHistory, []string{"item-1", "item-3"}) {
		t.Fatalf("ActiveHistory after new transaction = %#v, want item-1,item-3", replayed.ActiveHistory)
	}
}

func TestV2StoreReplayIgnoresUncommittedCompactionCheckpointTransaction(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)
	appendTestItem(t, store, "session-1", "item-1", "one")
	if _, err := store.ReplaceActiveHistory("session-1", []string{"item-1"}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}

	segmentPath := filepath.Join(root, "session-1", "segments", "000001.jsonl")
	appendV2RecordsForTest(t, segmentPath,
		v2Record{Seq: 3, Type: RecordTypeTransactionBegin, TxID: "tx-abandoned-compact"},
		v2Record{
			Seq:  4,
			Type: RecordTypeItemAppended,
			TxID: "tx-abandoned-compact",
			Item: &SessionItem{
				ID:         "summary-1",
				Seq:        4,
				Kind:       ItemKindMessage,
				Visibility: ItemVisibilityHidden,
				Audience:   ItemAudienceModel,
				Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "ignored checkpoint"},
			},
		},
		v2Record{
			Seq:  5,
			Type: RecordTypeCompactionCreated,
			TxID: "tx-abandoned-compact",
			Compaction: &CompactionCheckpoint{
				ID:                 "compact-1",
				SummaryItemID:      "summary-1",
				ReplacementHistory: []string{"item-1", "summary-1"},
			},
		},
		v2Record{Seq: 6, Type: RecordTypeActiveHistoryReplaced, TxID: "tx-abandoned-compact", ItemIDs: []string{"item-1", "summary-1"}},
	)

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if got := itemIDs(replayed.Items); !reflect.DeepEqual(got, []string{"item-1"}) {
		t.Fatalf("item IDs after abandoned compaction = %#v, want original only", got)
	}
	if len(replayed.Compactions) != 0 {
		t.Fatalf("Compactions after abandoned compaction = %#v, want none", replayed.Compactions)
	}
	if !reflect.DeepEqual(replayed.ActiveHistory, []string{"item-1"}) || replayed.LastSeq != 2 {
		t.Fatalf("state after abandoned compaction = active %#v last %d, want item-1/2", replayed.ActiveHistory, replayed.LastSeq)
	}
}

func TestV2StoreSegmentRolloverByMaxLineCount(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2StoreWithOptions(root, V2StoreOptions{MaxSegmentLines: 2})

	for i := 1; i <= 3; i++ {
		if _, err := store.AppendItem("session-1", SessionItem{
			ID:         "item-" + string(rune('0'+i)),
			Kind:       ItemKindMessage,
			Visibility: ItemVisibilityVisible,
			Audience:   ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "message"},
		}); err != nil {
			t.Fatalf("AppendItem(%d) error = %v", i, err)
		}
	}

	segmentsDir := filepath.Join(root, "session-1", "segments")
	first := filepath.Join(segmentsDir, "000001.jsonl")
	second := filepath.Join(segmentsDir, "000002.jsonl")
	if got := mustCountLines(t, first); got != 2 {
		t.Fatalf("000001.jsonl lines = %d, want 2", got)
	}
	if got := mustCountLines(t, second); got != 1 {
		t.Fatalf("000002.jsonl lines = %d, want 1", got)
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if replayed.LastSeq != 3 {
		t.Fatalf("LastSeq = %d, want 3", replayed.LastSeq)
	}
}

func TestV2StoreRejectsRecordThatWouldReplayAsTooLarge(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)

	payload := recordPayloadForMarshaledSize(t, maxJSONLRecordBytes)
	_, err := store.appendRecord("session-1", v2Record{
		Type:    RecordTypeActiveHistoryReplaced,
		ItemIDs: []string{payload},
	})
	if err == nil {
		t.Fatal("appendRecord() error = nil, want record too large")
	}
	if !strings.Contains(err.Error(), "is too large") {
		t.Fatalf("appendRecord() error = %q, want too large detail", err)
	}

	segmentsDir := filepath.Join(root, "session-1", "segments")
	if _, statErr := os.Stat(segmentsDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("segments dir stat error = %v, want not exist", statErr)
	}
}

func TestV2StoreActiveHistoryReplacedReplayUsesLatest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)

	appendTestItem(t, store, "session-1", "item-1", "one")
	appendTestItem(t, store, "session-1", "item-2", "two")
	appendTestItem(t, store, "session-1", "item-3", "three")
	if _, err := store.ReplaceActiveHistory("session-1", []string{"item-1", "item-2"}); err != nil {
		t.Fatalf("ReplaceActiveHistory(first) error = %v", err)
	}
	if _, err := store.ReplaceActiveHistory("session-1", []string{"item-3"}); err != nil {
		t.Fatalf("ReplaceActiveHistory(second) error = %v", err)
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if !reflect.DeepEqual(replayed.ActiveHistory, []string{"item-3"}) {
		t.Fatalf("ActiveHistory = %#v, want latest replacement", replayed.ActiveHistory)
	}
	if replayed.LastSeq != 5 {
		t.Fatalf("LastSeq = %d, want 5", replayed.LastSeq)
	}
}

func TestV2StoreCompactionCreatedReplay(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 3, 4, 5, 6, 0, time.UTC)}
	store := newV2StoreWithClock(root, V2StoreOptions{}, clock.Now)

	checkpoint, err := store.AppendCompaction("session-1", CompactionCheckpoint{
		ID:                    "compact-1",
		Reason:                "user_requested",
		Phase:                 "manual",
		Trigger:               "manual",
		SummaryItemID:         "summary-1",
		PreviousActiveHistory: []string{"item-1", "item-2"},
		ReplacementHistory:    []string{"summary-1"},
		SummaryProvider:       "paperhub",
		SummaryModel:          "glm-5.2",
	})
	if err != nil {
		t.Fatalf("AppendCompaction() error = %v", err)
	}
	if checkpoint.CreatedAt.IsZero() {
		t.Fatal("checkpoint.CreatedAt is zero")
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if got, want := len(replayed.Compactions), 1; got != want {
		t.Fatalf("len(Compactions) = %d, want %d", got, want)
	}
	if !reflect.DeepEqual(replayed.Compactions[0], checkpoint) {
		t.Fatalf("Compactions[0] = %#v, want %#v", replayed.Compactions[0], checkpoint)
	}
}

func TestV2StoreBlobWriteDedupeMetadataAndReadByRef(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)
	raw := []byte("large tool result body")

	first, err := store.WriteBlob(raw, "utf-8", "text/plain")
	if err != nil {
		t.Fatalf("WriteBlob(first) error = %v", err)
	}
	second, err := store.WriteBlob(raw, "utf-8", "text/plain")
	if err != nil {
		t.Fatalf("WriteBlob(second) error = %v", err)
	}
	if first != second {
		t.Fatalf("second ref = %#v, want same as first %#v", second, first)
	}

	sum := sha256.Sum256(raw)
	wantHash := hex.EncodeToString(sum[:])
	if first.Hash != wantHash || first.SizeBytes != int64(len(raw)) || first.Encoding != "utf-8" || first.MediaType != "text/plain" {
		t.Fatalf("BlobRef = %#v, want hash/size/encoding/media metadata", first)
	}

	matches, err := filepath.Glob(filepath.Join(root, "blobs", "sha256", first.Hash[:2], "*.data"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("blob file count = %d, want 1: %#v", len(matches), matches)
	}

	read, err := store.ReadBlob(first)
	if err != nil {
		t.Fatalf("ReadBlob() error = %v", err)
	}
	if !bytes.Equal(read, raw) {
		t.Fatalf("ReadBlob() = %q, want %q", read, raw)
	}
}

func TestV2StoreSaveTurnBlobifiesLargeMessageContent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)
	largeContent := strings.Repeat("large blob-only body ", 300) + "SECRET-BLOB-BODY"

	session, err := store.SaveMetadata(SessionV2{
		ID:              "session-1",
		Provider:        "codex",
		ModelProfile:    "default",
		ModelID:         "gpt-5",
		SaveToolResults: true,
	})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	saved, err := store.SaveTurn(session, []SessionItem{{
		ID:         "blob-item",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: largeContent},
	}}, []string{"blob-item"})
	if err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}

	segmentRaw, err := os.ReadFile(filepath.Join(root, "session-1", "segments", "000001.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile(segment) error = %v", err)
	}
	if bytes.Contains(segmentRaw, []byte("SECRET-BLOB-BODY")) || bytes.Contains(segmentRaw, []byte(largeContent)) {
		t.Fatalf("segment stored raw blob body: %s", segmentRaw)
	}

	item := saved.Items[0]
	if item.Message == nil || item.Message.Content != "" {
		t.Fatalf("saved message content = %#v, want blobified empty message content", item.Message)
	}
	if item.Content == nil || item.Content.Blob == nil {
		t.Fatalf("saved item content = %#v, want blob ref", item.Content)
	}
	if !strings.HasPrefix(item.Content.Preview, "large blob-only body ") || strings.Contains(item.Content.Preview, "SECRET-BLOB-BODY") {
		t.Fatalf("saved preview = %q, want short non-secret prefix", item.Content.Preview)
	}
	if !bytes.Contains(segmentRaw, []byte(item.Content.Blob.Hash)) {
		t.Fatalf("segment does not contain blob hash %s: %s", item.Content.Blob.Hash, segmentRaw)
	}

	messages, err := store.MaterializeActiveHistory(saved)
	if err != nil {
		t.Fatalf("store.MaterializeActiveHistory() error = %v", err)
	}
	if got := messageContents(messages); !reflect.DeepEqual(got, []string{largeContent}) {
		t.Fatalf("materialized messages = %#v, want original large content", got)
	}
	if _, err := saved.MaterializeActiveHistory(); !errors.Is(err, ErrCorruptedSession) {
		t.Fatalf("session.MaterializeActiveHistory() error = %v, want ErrCorruptedSession without store", err)
	}

	other, err := store.SaveMetadata(SessionV2{
		ID:              "session-2",
		Provider:        "codex",
		ModelProfile:    "default",
		ModelID:         "gpt-5",
		SaveToolResults: true,
	})
	if err != nil {
		t.Fatalf("SaveMetadata(session-2) error = %v", err)
	}
	second, err := store.SaveTurn(other, []SessionItem{{
		ID:         "blob-item-2",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: largeContent},
	}}, []string{"blob-item-2"})
	if err != nil {
		t.Fatalf("SaveTurn(session-2) error = %v", err)
	}
	if got := second.Items[0].Content.Blob.Hash; got != item.Content.Blob.Hash {
		t.Fatalf("deduped blob hash = %s, want %s", got, item.Content.Blob.Hash)
	}
	matches, err := filepath.Glob(filepath.Join(root, "blobs", "sha256", item.Content.Blob.Hash[:2], "*.data"))
	if err != nil {
		t.Fatalf("Glob(blob files) error = %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("blob file count = %d, want 1: %#v", len(matches), matches)
	}
}

func TestV2StoreWriteBlobRejectsExistingCorruptBlob(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)
	raw := []byte("large tool result body")

	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	path := filepath.Join(root, "blobs", "sha256", hash[:2], hash+".data")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("truncated"), 0o600); err != nil {
		t.Fatalf("WriteFile(corrupt blob) error = %v", err)
	}

	_, err := store.WriteBlob(raw, "utf-8", "text/plain")
	if err == nil {
		t.Fatal("WriteBlob() error = nil, want corrupt existing blob error")
	}
	if !strings.Contains(err.Error(), "size mismatch") && !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("WriteBlob() error = %q, want integrity detail", err)
	}

	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(corrupt blob) error = %v", err)
	}
	if string(stored) != "truncated" {
		t.Fatalf("existing corrupt blob was overwritten: %q", stored)
	}
}

func TestV2SessionMaterializeActiveHistory(t *testing.T) {
	session := SessionV2{
		ID: "session-1",
		Items: []SessionItem{
			{
				ID:      "old-visible",
				Kind:    ItemKindMessage,
				Message: &model.Message{Role: model.MessageRoleUser, Content: "not active"},
			},
			{
				ID:   "active-user",
				Kind: ItemKindMessage,
				Message: &model.Message{
					Role:    model.MessageRoleUser,
					Content: "continue",
				},
			},
			{
				ID:   "active-assistant",
				Kind: ItemKindMessage,
				Message: &model.Message{
					Role:    model.MessageRoleAssistant,
					Content: "ok",
					ToolCalls: []model.ToolCall{
						{ID: "call-1", Name: "read_file", Arguments: `{"path":"README.md"}`},
					},
				},
			},
		},
		ActiveHistory: []string{"active-user", "active-assistant"},
	}

	messages, err := session.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	want := []model.Message{
		{Role: model.MessageRoleUser, Content: "continue"},
		{
			Role:    model.MessageRoleAssistant,
			Content: "ok",
			ToolCalls: []model.ToolCall{
				{ID: "call-1", Name: "read_file", Arguments: `{"path":"README.md"}`},
			},
		},
	}
	if !reflect.DeepEqual(messages, want) {
		t.Fatalf("messages = %#v, want %#v", messages, want)
	}

	messages[1].ToolCalls[0].Name = "mutated"
	if session.Items[2].Message.ToolCalls[0].Name != "read_file" {
		t.Fatalf("MaterializeActiveHistory returned aliased ToolCalls: %#v", session.Items[2].Message.ToolCalls)
	}
}

func TestV2SessionMaterializeActiveHistorySynthesizesPendingToolResults(t *testing.T) {
	session := SessionV2{
		ID: "session-1",
		Items: []SessionItem{
			{
				ID:     "pending-tool",
				Kind:   ItemKindMessage,
				Status: ItemStatusPending,
				Message: &model.Message{
					Role:       model.MessageRoleTool,
					ToolCallID: "call-pending",
				},
			},
			{
				ID:     "interrupted-tool",
				Kind:   ItemKindMessage,
				Status: ItemStatusInterrupted,
				Message: &model.Message{
					Role:       model.MessageRoleTool,
					ToolCallID: "call-interrupted",
					Content:    "persisted empty placeholder",
				},
			},
			{
				ID:     "completed-tool",
				Kind:   ItemKindMessage,
				Status: ItemStatusCompleted,
				Message: &model.Message{
					Role:       model.MessageRoleTool,
					ToolCallID: "call-completed",
					Content:    "done",
				},
			},
		},
		ActiveHistory: []string{"pending-tool", "interrupted-tool", "completed-tool"},
	}

	messages, err := session.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if got := messageContents(messages); !reflect.DeepEqual(got, []string{interruptedToolResultContent, interruptedToolResultContent, "done"}) {
		t.Fatalf("materialized messages = %#v, want synthesized pending/interrupted plus completed", got)
	}
	if !messages[0].IsError || !messages[1].IsError || messages[2].IsError {
		t.Fatalf("materialized IsError flags = %v/%v/%v, want true/true/false", messages[0].IsError, messages[1].IsError, messages[2].IsError)
	}
	if session.Items[0].Message.Content != "" || session.Items[0].Message.IsError {
		t.Fatalf("pending persisted message mutated: %#v", session.Items[0].Message)
	}

	storeMessages, err := NewV2Store(filepath.Join(t.TempDir(), "sessions")).MaterializeActiveHistory(session)
	if err != nil {
		t.Fatalf("store.MaterializeActiveHistory() error = %v", err)
	}
	if !reflect.DeepEqual(storeMessages, messages) {
		t.Fatalf("store materialized messages = %#v, want %#v", storeMessages, messages)
	}
}

func TestV2SessionMaterializeActiveHistoryTreatsEmptyStatusAsCompleted(t *testing.T) {
	session := SessionV2{
		ID: "session-1",
		Items: []SessionItem{
			{
				ID:   "legacy-tool",
				Kind: ItemKindMessage,
				Message: &model.Message{
					Role:       model.MessageRoleTool,
					ToolCallID: "call-legacy",
					Content:    "legacy result",
				},
			},
		},
		ActiveHistory: []string{"legacy-tool"},
	}

	messages, err := session.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if got := messageContents(messages); !reflect.DeepEqual(got, []string{"legacy result"}) {
		t.Fatalf("materialized legacy messages = %#v, want original content", got)
	}
	if messages[0].IsError {
		t.Fatalf("legacy tool result IsError = true, want false")
	}
}

func TestV2SessionMaterializeActiveHistoryCorruptedMissingRef(t *testing.T) {
	session := SessionV2{
		ID:            "session-1",
		ActiveHistory: []string{"missing"},
	}

	_, err := session.MaterializeActiveHistory()
	if !errors.Is(err, ErrCorruptedSession) {
		t.Fatalf("MaterializeActiveHistory() error = %v, want ErrCorruptedSession", err)
	}
	if !strings.Contains(err.Error(), `active history references missing item "missing"`) {
		t.Fatalf("MaterializeActiveHistory() error = %q, want missing ref detail", err)
	}
}

func TestV2SessionMaterializeActiveHistoryCorruptedNonMessageRef(t *testing.T) {
	session := SessionV2{
		ID: "session-1",
		Items: []SessionItem{
			{ID: "runtime-1", Kind: ItemKindRuntimeContext},
		},
		ActiveHistory: []string{"runtime-1"},
	}

	_, err := session.MaterializeActiveHistory()
	if !errors.Is(err, ErrCorruptedSession) {
		t.Fatalf("MaterializeActiveHistory() error = %v, want ErrCorruptedSession", err)
	}
	if !strings.Contains(err.Error(), `active history references item "runtime-1" without a message`) {
		t.Fatalf("MaterializeActiveHistory() error = %q, want no-message detail", err)
	}
}

func appendTestItem(t *testing.T, store *V2Store, sessionID, itemID, content string) {
	t.Helper()

	if _, err := store.AppendItem(sessionID, SessionItem{
		ID:         itemID,
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: content},
	}); err != nil {
		t.Fatalf("AppendItem(%q) error = %v", itemID, err)
	}
}

func writeLegacyV2Metadata(t *testing.T, root, id string, createdAt, updatedAt time.Time) {
	t.Helper()

	sessionDir := filepath.Join(root, id)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", sessionDir, err)
	}
	data, err := json.MarshalIndent(map[string]any{
		"id":            id,
		"version":       VersionV2,
		"created_at":    createdAt,
		"updated_at":    updatedAt,
		"provider":      "paperhub",
		"model_profile": "glm-5.2",
		"model_id":      "glm-5.2",
	}, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent(legacy metadata) error = %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(sessionDir, "meta.json"), data, 0o600); err != nil {
		t.Fatalf("WriteFile(legacy meta.json) error = %v", err)
	}
}

func sessionInfoIDs(infos []Info) []string {
	ids := make([]string, 0, len(infos))
	for _, info := range infos {
		ids = append(ids, info.ID)
	}
	return ids
}

func appendV2RecordsForTest(t *testing.T, path string, records ...v2Record) {
	t.Helper()

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("OpenFile(%q) error = %v", path, err)
	}
	for _, record := range records {
		line, err := marshalV2RecordLine(record)
		if err != nil {
			_ = file.Close()
			t.Fatalf("marshalV2RecordLine() error = %v", err)
		}
		if _, err := file.Write(line); err != nil {
			_ = file.Close()
			t.Fatalf("Write(%q) error = %v", path, err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close(%q) error = %v", path, err)
	}
}

func itemIDs(items []SessionItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func messageContents(messages []model.Message) []string {
	contents := make([]string, 0, len(messages))
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	return contents
}

func requireReservedV2SessionIDError(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("error = nil, want reserved v2 session id error")
	}
	if !strings.Contains(err.Error(), "reserved v2 session id") {
		t.Fatalf("error = %q, want reserved v2 session id detail", err)
	}
}

func mustCountLines(t *testing.T, path string) int {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return strings.Count(string(raw), "\n")
}

func recordPayloadForMarshaledSize(t *testing.T, target int) string {
	t.Helper()

	empty := v2Record{
		Seq:     1,
		Type:    RecordTypeActiveHistoryReplaced,
		ItemIDs: []string{""},
	}
	raw, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("Marshal(empty record) error = %v", err)
	}
	payloadLen := target - len(raw)
	if payloadLen < 0 {
		t.Fatalf("record overhead %d exceeds target %d", len(raw), target)
	}
	payload := strings.Repeat("a", payloadLen)
	withPayload := v2Record{
		Seq:     1,
		Type:    RecordTypeActiveHistoryReplaced,
		ItemIDs: []string{payload},
	}
	raw, err = json.Marshal(withPayload)
	if err != nil {
		t.Fatalf("Marshal(payload record) error = %v", err)
	}
	if len(raw) != target {
		t.Fatalf("payload record marshaled size = %d, want %d", len(raw), target)
	}
	return payload
}
