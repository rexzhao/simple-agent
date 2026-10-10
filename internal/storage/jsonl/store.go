// Package jsonl is the file storage implementation: sessions under a data
// root laid out as <root>/<project>/<session>/log.jsonl. A project is a
// workspace directory identified by ProjectID; a session is a directory
// holding one append-only log.
package jsonl

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/rexzhao/simple-agent/internal/storage"
)

const (
	logFileName      = "log.jsonl"
	logSwitchStaging = "log.jsonl.next"
	logArchivePrefix = "log-"
	logArchiveSuffix = ".jsonl"
)

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
//
// A session's records live in segments: log.jsonl is the active segment,
// log-NNNN.jsonl files are immutable archives of the history before each
// compaction. An interrupted segment switch is recovered here.
func (s *Store) Open(id string) (storage.Session, error) {
	if !ValidSessionID(id) {
		return nil, fmt.Errorf("invalid session id %q", id)
	}
	dir, err := s.sessionDir(id)
	if err != nil {
		return nil, err
	}
	if err := recoverSegments(dir); err != nil {
		return nil, err
	}
	log, err := OpenLog(filepath.Join(dir, logFileName))
	if err != nil {
		return nil, err
	}
	return &Session{id: id, project: filepath.Base(filepath.Dir(dir)), store: s, log: log}, nil
}

// recoverSegments completes or discards an interrupted compaction switch.
// The switch stages the next segment in log.jsonl.next (fully written and
// synced), archives the active log, then activates the staged file; every
// interruption point converges to "the compaction never took effect" or
// "it fully did":
//
//   - staging exists, active exists: interrupted before the archive —
//     discard the staging (the compaction is lost, the context untouched);
//   - staging exists, active missing: interrupted between the renames —
//     complete the switch by activating the staging;
//   - active missing, no staging: a switch never staged (or the active
//     file was removed externally) — roll the newest archive back into
//     place, which is the pre-compaction history.
func recoverSegments(dir string) error {
	staging := filepath.Join(dir, logSwitchStaging)
	active := filepath.Join(dir, logFileName)
	stagingExists, err := fileExists(staging)
	if err != nil {
		return err
	}
	if stagingExists {
		activeExists, err := fileExists(active)
		if err != nil {
			return err
		}
		if activeExists {
			if err := os.Remove(staging); err != nil {
				return fmt.Errorf("discard interrupted segment staging: %w", err)
			}
		} else if err := os.Rename(staging, active); err != nil {
			return fmt.Errorf("complete interrupted segment switch: %w", err)
		}
		return nil
	}
	activeExists, err := fileExists(active)
	if err != nil {
		return err
	}
	if !activeExists {
		if newest, ok := newestArchive(dir); ok {
			if err := os.Rename(newest, active); err != nil {
				return fmt.Errorf("roll back newest archive: %w", err)
			}
		}
	}
	return nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// newestArchive returns the highest-numbered archived segment.
func newestArchive(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	best, bestNumber := "", -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, logArchivePrefix) || !strings.HasSuffix(name, logArchiveSuffix) {
			continue
		}
		number, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, logArchivePrefix), logArchiveSuffix))
		if err != nil || number < 0 {
			continue
		}
		if number > bestNumber {
			best, bestNumber = name, number
		}
	}
	if bestNumber < 0 {
		return "", false
	}
	return filepath.Join(dir, best), true
}

// nextArchiveName returns the archive path for the next switch.
func nextArchiveName(dir string) string {
	number := 0
	if newest, ok := newestArchive(dir); ok {
		name := filepath.Base(newest)
		if parsed, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, logArchivePrefix), logArchiveSuffix)); err == nil {
			number = parsed + 1
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%s%04d%s", logArchivePrefix, number, logArchiveSuffix))
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

// Append stores one record, opening the log on the first call. A
// context.compacted record switches the segment: the current log is
// archived immutably and a new active log opens with a copy of the
// session header (the session.created record) followed by the
// compaction record — so the active segment always folds into the
// current context on its own.
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
	if record.Type == storage.TypeContextCompacted {
		return s.appendCompactionLocked(record)
	}
	return s.log.Append(record)
}

// appendCompactionLocked performs the segment switch for one
// context.compacted record. The switch is staged first: the next segment
// (header + compaction record) is fully written and synced into
// log.jsonl.next; only then is the active log archived and the staging
// activated. Every interruption converges to "the compaction never took
// effect" (see recoverSegments). Callers hold s.mu.
func (s *Session) appendCompactionLocked(record storage.Record) (storage.Record, error) {
	dir := filepath.Join(s.store.root, s.project, s.id)
	stagingPath := filepath.Join(dir, logSwitchStaging)
	activePath := filepath.Join(dir, logFileName)

	// A stale staging from an interrupted switch must not leak into the
	// new one.
	if err := os.Remove(stagingPath); err != nil && !os.IsNotExist(err) {
		return storage.Record{}, fmt.Errorf("clear stale segment staging: %w", err)
	}
	staging, err := OpenLog(stagingPath)
	if err != nil {
		return storage.Record{}, err
	}
	// The new segment opens with a copy of the session header so it is
	// self-contained (metadata and creation time survive alone). Only the
	// first record is read — the switch cost stays independent of the
	// segment size.
	if s.log != nil {
		header, ok, err := firstRecord(filepath.Join(dir, logFileName))
		if err != nil {
			staging.Close()
			return storage.Record{}, err
		}
		if ok {
			if _, err := staging.Append(header); err != nil {
				staging.Close()
				return storage.Record{}, fmt.Errorf("stage segment header: %w", err)
			}
		}
	}
	stored, err := staging.Append(record)
	if err != nil {
		staging.Close()
		return storage.Record{}, fmt.Errorf("stage compaction record: %w", err)
	}
	if err := staging.Close(); err != nil {
		return storage.Record{}, fmt.Errorf("sync staged segment: %w", err)
	}

	// The staging is durable: archive the active log and activate it.
	// From here on the in-process log handle is closed; a failure in the
	// remaining steps leaves the session unusable in this process by
	// design. Do NOT lazily reopen here: in the window where the active
	// log was archived but the staging not yet activated, the active path
	// does not exist, and reopening would create an empty log.jsonl —
	// appending to it would make the next Open discard the staged
	// compaction seed. The on-disk state is convergent either way; the
	// session continues after a reopen (-session).
	if s.log != nil {
		if err := s.log.Close(); err != nil {
			return storage.Record{}, fmt.Errorf("segment switch failed; reopen the session to continue: %w", err)
		}
		if err := os.Rename(activePath, nextArchiveName(dir)); err != nil {
			return storage.Record{}, fmt.Errorf("segment switch failed; reopen the session to continue: %w", err)
		}
	}
	if err := os.Rename(stagingPath, activePath); err != nil {
		return storage.Record{}, fmt.Errorf("segment switch failed; reopen the session to continue: %w", err)
	}
	log, err := OpenLog(activePath)
	if err != nil {
		return storage.Record{}, fmt.Errorf("segment switch failed; reopen the session to continue: %w", err)
	}
	s.log = log
	return stored, nil
}

// firstRecord reads the first valid record of a log file without reading
// the rest.
func firstRecord(path string) (storage.Record, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return storage.Record{}, false, nil
		}
		return storage.Record{}, false, fmt.Errorf("open session log: %w", err)
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	line, err := reader.ReadBytes('\n')
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return storage.Record{}, false, nil
	}
	var record storage.Record
	if err := json.Unmarshal(trimmed, &record); err != nil || record.Validate() != nil {
		return storage.Record{}, false, fmt.Errorf("session log %s has an invalid header line", path)
	}
	return record, true, nil
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
