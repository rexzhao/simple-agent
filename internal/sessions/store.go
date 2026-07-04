package sessions

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/model"
)

const CurrentVersion = 1

var ErrNotFound = errors.New("session not found")

type Session struct {
	ID                   string                 `json:"id"`
	CreatedAt            time.Time              `json:"created_at"`
	UpdatedAt            time.Time              `json:"updated_at"`
	Version              int                    `json:"version"`
	Provider             string                 `json:"provider"`
	ModelProfile         string                 `json:"model_profile"`
	ModelID              string                 `json:"model_id"`
	ModelParameters      map[string]any         `json:"model_parameters,omitempty"`
	CWD                  string                 `json:"cwd"`
	ConfigPath           string                 `json:"config_path,omitempty"`
	ConfigDir            string                 `json:"config_dir,omitempty"`
	EnabledTools         []string               `json:"enabled_tools,omitempty"`
	EnabledMCP           []string               `json:"enabled_mcp,omitempty"`
	EnabledSkills        []string               `json:"enabled_skills,omitempty"`
	ShowReasoning        bool                   `json:"show_reasoning"`
	InstructionsSnapshot []model.Message        `json:"instructions_snapshot,omitempty"`
	InstructionSources   []InstructionSource    `json:"instruction_sources,omitempty"`
	Messages             []model.Message        `json:"messages"`
	Context              contextwindow.Metadata `json:"context,omitempty"`
	SaveToolResults      bool                   `json:"save_tool_results"`
}

type InstructionSource struct {
	Role   model.MessageRole `json:"role"`
	Source string            `json:"source"`
	Path   string            `json:"path,omitempty"`
}

func (s Session) RootConfigPath() string {
	if strings.TrimSpace(s.ConfigPath) != "" {
		return s.ConfigPath
	}
	if strings.TrimSpace(s.ConfigDir) != "" {
		return filepath.Join(s.ConfigDir, "sai.yaml")
	}
	return ""
}

type Info struct {
	ID              string    `json:"id"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Version         int       `json:"version"`
	Provider        string    `json:"provider"`
	ModelProfile    string    `json:"model_profile"`
	ModelID         string    `json:"model_id"`
	ProjectID       string    `json:"project_id,omitempty"`
	CreatedCWD      string    `json:"created_cwd,omitempty"`
	ContextWindow   int       `json:"context_window,omitempty"`
	ContextSource   string    `json:"context_window_source,omitempty"`
	SaveToolResults bool      `json:"save_tool_results"`
}

type Store struct {
	dir string
	now func() time.Time
}

func NewStore(dir string) *Store {
	return newStoreWithClock(dir, time.Now)
}

func newStoreWithClock(dir string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	dir = strings.TrimSpace(dir)
	if dir != "" {
		dir = filepath.Clean(dir)
	}
	return &Store{
		dir: dir,
		now: now,
	}
}

func (s *Store) Save(session Session) (Session, error) {
	if err := s.requireDir(); err != nil {
		return Session{}, err
	}

	now := s.now().UTC()
	if strings.TrimSpace(session.ID) == "" {
		id, err := newSessionID(now)
		if err != nil {
			return Session{}, err
		}
		session.ID = id
	}
	if err := validateSessionID(session.ID); err != nil {
		return Session{}, err
	}
	if session.Version == 0 {
		session.Version = CurrentVersion
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	session.UpdatedAt = now
	session.ModelParameters = copyMap(session.ModelParameters)
	session.EnabledTools = copyStrings(session.EnabledTools)
	session.EnabledMCP = copyStrings(session.EnabledMCP)
	session.EnabledSkills = copyStrings(session.EnabledSkills)
	session.InstructionsSnapshot = copyMessages(session.InstructionsSnapshot)
	session.InstructionSources = copyInstructionSources(session.InstructionSources)
	session.Messages = copyMessages(session.Messages)

	sessionDir := s.sessionDir(session.ID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return Session{}, fmt.Errorf("create session directory %q: %w", sessionDir, err)
	}

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return Session{}, fmt.Errorf("marshal session %q: %w", session.ID, err)
	}
	data = append(data, '\n')
	path := filepath.Join(sessionDir, "session.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return Session{}, fmt.Errorf("write session %q: %w", session.ID, err)
	}
	return session, nil
}

func (s *Store) Load(id string) (Session, error) {
	if err := s.requireDir(); err != nil {
		return Session{}, err
	}
	if err := validateSessionID(id); err != nil {
		return Session{}, err
	}

	session, err := readSessionFile(filepath.Join(s.sessionDir(id), "session.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Session{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return Session{}, err
	}
	if session.ID == "" {
		session.ID = id
	}
	if session.ID != id {
		return Session{}, mismatchedSessionIDError(id, session.ID)
	}
	return session, nil
}

func (s *Store) List() ([]Info, error) {
	if err := s.requireDir(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Info{}, nil
		}
		return nil, fmt.Errorf("read session store %q: %w", s.dir, err)
	}

	infos := make([]Info, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		if err := validateSessionID(id); err != nil {
			continue
		}
		session, err := readSessionFile(filepath.Join(s.sessionDir(id), "session.json"))
		if err != nil {
			return nil, err
		}
		if session.ID == "" {
			session.ID = id
		}
		if session.ID != id {
			return nil, mismatchedSessionIDError(id, session.ID)
		}
		infos = append(infos, session.info())
	}
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].UpdatedAt.Equal(infos[j].UpdatedAt) {
			return infos[i].ID < infos[j].ID
		}
		return infos[i].UpdatedAt.After(infos[j].UpdatedAt)
	})
	return infos, nil
}

func (s *Store) Latest() (Session, error) {
	infos, err := s.List()
	if err != nil {
		return Session{}, err
	}
	if len(infos) == 0 {
		return Session{}, ErrNotFound
	}
	return s.Load(infos[0].ID)
}

func (s *Store) Delete(id string) error {
	if err := s.requireDir(); err != nil {
		return err
	}
	if err := validateSessionID(id); err != nil {
		return err
	}

	sessionDir := s.sessionDir(id)
	if _, err := os.Stat(sessionDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return fmt.Errorf("stat session %q: %w", id, err)
	}
	if err := os.RemoveAll(sessionDir); err != nil {
		return fmt.Errorf("delete session %q: %w", id, err)
	}
	return nil
}

func (s *Store) sessionDir(id string) string {
	return filepath.Join(s.dir, id)
}

func (s *Store) requireDir() error {
	if s == nil || strings.TrimSpace(s.dir) == "" || s.dir == "." {
		return fmt.Errorf("session store directory is required")
	}
	return nil
}

func mismatchedSessionIDError(want, got string) error {
	return fmt.Errorf("session file %q contains id %q", want, got)
}

func (s Session) info() Info {
	return Info{
		ID:              s.ID,
		CreatedAt:       s.CreatedAt,
		UpdatedAt:       s.UpdatedAt,
		Version:         s.Version,
		Provider:        s.Provider,
		ModelProfile:    s.ModelProfile,
		ModelID:         s.ModelID,
		ContextWindow:   s.Context.ContextWindow,
		ContextSource:   s.Context.ContextWindowSource,
		SaveToolResults: s.SaveToolResults,
	}
}

func readSessionFile(path string) (Session, error) {
	file, err := os.Open(path)
	if err != nil {
		return Session{}, err
	}
	defer file.Close()

	var session Session
	decoder := json.NewDecoder(file)
	decoder.UseNumber()
	if err := decoder.Decode(&session); err != nil {
		return Session{}, fmt.Errorf("parse session file %q: %w", path, err)
	}
	return session, nil
}

func newSessionID(now time.Time) (string, error) {
	var randomBytes [4]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return now.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(randomBytes[:]), nil
}

func validateSessionID(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("session id is required")
	}
	if id != strings.TrimSpace(id) {
		return fmt.Errorf("session id %q has surrounding whitespace", id)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("invalid session id %q", id)
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("invalid session id %q", id)
	}
	return nil
}

func copyStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func copyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	copied := make(map[string]any, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func copyMessages(messages []model.Message) []model.Message {
	if messages == nil {
		return nil
	}
	copied := append([]model.Message(nil), messages...)
	for i := range copied {
		copied[i].ToolCalls = append([]model.ToolCall(nil), messages[i].ToolCalls...)
	}
	return copied
}

func copyInstructionSources(sources []InstructionSource) []InstructionSource {
	if sources == nil {
		return nil
	}
	return append([]InstructionSource(nil), sources...)
}
