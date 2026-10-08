// Package jsonl is the file storage implementation: sessions under a data
// root laid out as <root>/<project>/<session>/log.jsonl. A project is a
// workspace directory identified by ProjectID; a session is a directory
// holding one append-only log.
package jsonl

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/rexzhao/simple-agent/internal/storage"
)

const logFileName = "log.jsonl"

// Store manages sessions under a data root.
type Store struct {
	root string
}

// OpenStore opens (creating if needed) the data root directory.
func OpenStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("data root is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create data root: %w", err)
	}
	return &Store{root: root}, nil
}

// Root returns the data root directory.
func (s *Store) Root() string { return s.root }

// ProjectID returns the stable project identifier for a workspace
// directory: "p" followed by base32 of the first ten bytes of the SHA-256
// of the normalized absolute path. The same directory always maps to the
// same project, with no registration step. Path case is folded on
// case-insensitive platforms.
func ProjectID(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	abs = filepath.Clean(abs)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		abs = strings.ToLower(abs)
	}
	digest := sha256.Sum256([]byte(abs))
	return "p" + encodeBase32(digest[:10])
}

// ValidProjectID accepts the identifiers ProjectID generates.
func ValidProjectID(id string) bool {
	if len(id) != 17 || id[0] != 'p' {
		return false
	}
	for _, char := range id[1:] {
		if !strings.ContainsRune(base32Alphabet, char) {
			return false
		}
	}
	return true
}

// ValidSessionID accepts the identifiers this package stores: path-safe
// and not a reserved directory name.
func ValidSessionID(id string) bool {
	if len(id) == 0 || len(id) > 128 || id != strings.TrimSpace(id) {
		return false
	}
	for _, r := range id {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	if id == "." || id == ".." || id == "blobs" {
		return false
	}
	return true
}

const base32Alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

func encodeBase32(data []byte) string {
	var builder []byte
	var buffer uint32
	var bits uint
	for _, b := range data {
		buffer = buffer<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			builder = append(builder, base32Alphabet[(buffer>>bits)&0x1f])
		}
	}
	if bits > 0 {
		builder = append(builder, base32Alphabet[(buffer<<(5-bits))&0x1f])
	}
	return string(builder)
}

// Create returns a new session inside the project. Creation is lazy:
// nothing touches the disk, so an empty session (one never committed to)
// leaves no directory and no file. The first Append opens the log; the
// state layer's session.created record is simply its first line.
func (s *Store) Create(projectID string) (storage.Session, error) {
	if !ValidProjectID(projectID) {
		return nil, fmt.Errorf("invalid project id %q", projectID)
	}
	return &Session{
		id:      storage.NewSessionID(),
		project: projectID,
		store:   s,
	}, nil
}

// Open loads an existing session. The session ID is globally unique, so the
// project directory it lives in is found by scanning the data root. A run
// which was interrupted by a crash needs no repair record: its user input
// simply stays unanswered, and the agent patches any dangling tool calls
// before the next request.
func (s *Store) Open(id string) (storage.Session, error) {
	if !ValidSessionID(id) {
		return nil, fmt.Errorf("invalid session id %q", id)
	}
	dir, err := s.sessionDir(id)
	if err != nil {
		return nil, err
	}
	log, err := OpenLog(filepath.Join(dir, logFileName))
	if err != nil {
		return nil, err
	}
	return &Session{id: id, project: filepath.Base(filepath.Dir(dir)), store: s, log: log}, nil
}

// sessionDir locates the directory of a session ID by scanning the project
// directories of the data root.
func (s *Store) sessionDir(id string) (string, error) {
	projects, err := os.ReadDir(s.root)
	if err != nil {
		return "", fmt.Errorf("read data root: %w", err)
	}
	for _, project := range projects {
		if !project.IsDir() || !ValidProjectID(project.Name()) {
			continue
		}
		dir := filepath.Join(s.root, project.Name(), id)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, nil
		}
	}
	return "", fmt.Errorf("open session %q: not found", id)
}

// Session is one open session's log. It holds no conversation state and no
// creation metadata: the state layer above commits every record, including
// session.created.
type Session struct {
	id      string
	project string
	store   *Store
	log     *Log

	mu sync.Mutex
}

// ID returns the session ID, available before the log is materialized.
func (s *Session) ID() string { return s.id }

// Dir returns the session's directory on disk. It exists only once the
// session has been materialized by its first record.
func (s *Session) Dir() string {
	return filepath.Join(s.store.root, s.project, s.id)
}

// Append stores one record, opening the log on the first call.
func (s *Session) Append(record storage.Record) (storage.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log == nil {
		log, err := OpenLog(filepath.Join(s.store.root, s.project, s.id, logFileName))
		if err != nil {
			return storage.Record{}, err
		}
		s.log = log
	}
	return s.log.Append(record)
}

// Records returns every stored record in order. A session which has not
// been materialized yet has no records.
func (s *Session) Records() ([]storage.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log == nil {
		return nil, nil
	}
	return s.log.Records()
}

// Close closes the underlying log. A session which never materialized has
// nothing on disk and nothing to close. The session must not be appended
// to after.
func (s *Session) Close() error {
	s.mu.Lock()
	log := s.log
	s.mu.Unlock()
	if log == nil {
		return nil
	}
	return log.Close()
}
