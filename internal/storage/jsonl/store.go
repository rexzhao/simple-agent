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
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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

// ValidSessionID accepts exactly the identifiers this package mints:
// "s" followed by the 26-character base32 body of NewItemID. Session
// ids are never user-chosen, so anything else is foreign — on disk, on
// the wire, or in a URL. (The previous loose check — "path-safe and
// not reserved" — accepted Windows hostilities like "..." or "CON":
// Win32 strips trailing dots, so HasSession("...") resolved to the
// project directory itself; that it failed only on the lock path was
// luck, not design.)
func ValidSessionID(id string) bool {
	if len(id) != 27 || id[0] != 's' {
		return false
	}
	for _, char := range id[1:] {
		if !strings.ContainsRune(base32Alphabet, char) {
			return false
		}
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
// directories of the data root. Session IDs are globally unique, so more
// than one hit means directories were copied around by hand — refuse
// instead of silently picking the first (the serve layer surfaces this as
// 409).
func (s *Store) sessionDir(id string) (string, error) {
	projects, err := os.ReadDir(s.root)
	if err != nil {
		return "", fmt.Errorf("read data root: %w", err)
	}
	var hit string
	for _, project := range projects {
		if !project.IsDir() || !ValidProjectID(project.Name()) {
			continue
		}
		dir := filepath.Join(s.root, project.Name(), id)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			if hit != "" {
				return "", fmt.Errorf("open session %q: multiple project directories hold this id: %s, %s", id, hit, dir)
			}
			hit = dir
		}
	}
	if hit == "" {
		return "", fmt.Errorf("open session %q: %w", id, storage.ErrSessionNotFound)
	}
	return hit, nil
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
	return s.openDir(id, dir)
}

// OpenIn loads a session scoped to one project: the id resolves inside
// <root>/<projectID>/ only. The serve-mode registration gate is structural
// — an unregistered project's sessions cannot be opened even though the
// store can see their directories.
func (s *Store) OpenIn(projectID, sessionID string) (storage.Session, error) {
	if !ValidProjectID(projectID) {
		return nil, fmt.Errorf("invalid project id %q", projectID)
	}
	if !ValidSessionID(sessionID) {
		return nil, fmt.Errorf("invalid session id %q", sessionID)
	}
	dir := filepath.Join(s.root, projectID, sessionID)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("open session %q: %w", sessionID, storage.ErrSessionNotFound)
	}
	return s.openDir(sessionID, dir)
}

// openDir takes the session lock, recovers an interrupted segment switch,
// and opens the active log — the shared sequence behind Open and OpenIn.
func (s *Store) openDir(id, dir string) (storage.Session, error) {
	// The lock precedes recovery and the log open: both touch files a
	// concurrent writer could be mid-switch on.
	lock, err := lockSession(dir)
	if err != nil {
		return nil, err
	}
	if err := recoverSegments(dir); err != nil {
		lock.Release()
		return nil, err
	}
	log, err := OpenLog(filepath.Join(dir, logFileName))
	if err != nil {
		lock.Release()
		return nil, err
	}
	return &Session{id: id, project: filepath.Base(filepath.Dir(dir)), store: s, log: log, lock: lock}, nil
}

// Session is one open session's log. It holds no conversation state and no
// creation metadata: the state layer above commits every record, including
// session.created.
type Session struct {
	id      string
	project string
	store   *Store
	log     *Log
	lock    *fileLock

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
	// The append boundary is where invalid UTF-8 dies: everything
	// downstream (the JSON line, the returned record, the fold, the
	// digest chain) sees the normalized form only.
	record = record.Normalized()
	if s.log == nil {
		// First append materializes the session: take the session lock
		// before creating anything on disk.
		dir := filepath.Join(s.store.root, s.project, s.id)
		lock, err := lockSession(dir)
		if err != nil {
			return storage.Record{}, err
		}
		log, err := OpenLog(filepath.Join(dir, logFileName))
		if err != nil {
			lock.Release()
			return storage.Record{}, err
		}
		s.log = log
		s.lock = lock
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
	// first line is read (a hand-edited blank first line means no header
	// copy, an accepted narrowing) — the switch cost stays independent
	// of the segment size.
	if s.log != nil {
		header, ok, err := firstRecord(filepath.Join(dir, logFileName))
		if err != nil {
			staging.Close()
			return storage.Record{}, err
		}
		if ok && header.Type == storage.TypeSessionCreated {
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

// firstRecord reads the first line of a log file as a record, without
// reading the rest. A blank or missing first line reports ok=false (no
// header copy is staged); an invalid one is an error.
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

// Close closes the underlying log and releases the session lock. A
// session which never materialized has nothing on disk and nothing to
// close. The session must not be appended to after.
func (s *Session) Close() error {
	s.mu.Lock()
	log := s.log
	lock := s.lock
	s.log = nil
	s.lock = nil
	s.mu.Unlock()
	var err error
	if log != nil {
		err = log.Close()
	}
	lock.Release()
	return err
}

// ListSessions enumerates the materialized sessions of one project, newest
// activity first. Reading is listing-grade best effort: unparseable lines
// are skipped (a CLI may hold the lock and be mid-append into the tail
// right now), and sessions without a log file hold nothing and stay
// unlisted.
func (s *Store) ListSessions(projectID string) ([]storage.SessionInfo, error) {
	if !ValidProjectID(projectID) {
		return nil, fmt.Errorf("invalid project id %q", projectID)
	}
	entries, err := os.ReadDir(filepath.Join(s.root, projectID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read project dir: %w", err)
	}
	var infos []storage.SessionInfo
	for _, entry := range entries {
		if !entry.IsDir() || !ValidSessionID(entry.Name()) {
			continue
		}
		dir := filepath.Join(s.root, projectID, entry.Name())
		info, ok, err := listSession(dir)
		if err != nil {
			// A listing is a view: one unreadable session directory
			// must not blank the whole project. The session errors
			// loudly when opened — that is where corruption belongs.
			continue
		}
		if ok {
			infos = append(infos, info)
		}
	}
	sort.Slice(infos, func(i, j int) bool {
		if !infos[i].Modified.Equal(infos[j].Modified) {
			return infos[i].Modified.After(infos[j].Modified)
		}
		return infos[i].ID < infos[j].ID // stable tiebreak
	})
	return infos, nil
}

// listSession derives one session's listing info from its directory. The
// second return is false for a session with no segment at all (never
// materialized, or a segment switch interrupted before the archive landed
// — either way there is nothing to list). Modified is CONTENT activity:
// the newest mtime among segment files only — the lock/holder bookkeeping
// (rewritten on every open) must not make an opened-but-idle session look
// recently active.
func listSession(dir string) (storage.SessionInfo, bool, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return storage.SessionInfo{}, false, fmt.Errorf("read session dir: %w", err)
	}
	var modified time.Time
	hasSegment := false
	for _, file := range files {
		if file.IsDir() || !isSegmentName(file.Name()) {
			continue
		}
		hasSegment = true
		if info, err := file.Info(); err == nil && info.ModTime().After(modified) {
			modified = info.ModTime()
		}
	}
	if !hasSegment {
		return storage.SessionInfo{}, false, nil
	}
	info, err := readSessionHead(dir)
	if err != nil {
		return storage.SessionInfo{}, false, err
	}
	info.ID = filepath.Base(dir)
	info.Modified = modified
	return info, true, nil
}

// isSegmentName reports whether a file is a log segment (active, archive,
// or staged next) — the only files whose mtimes count as content activity.
func isSegmentName(name string) bool {
	if name == logFileName || name == logSwitchStaging {
		return true
	}
	return strings.HasPrefix(name, logArchivePrefix) && strings.HasSuffix(name, logArchiveSuffix)
}

// headScanLimit bounds how many records scanHead COLLECTS from the
// segment head — the caller (readSessionHead) stops at the first user
// input after its structural checks, so the limit only bounds
// pathological files where no user input ever arrives. The structural
// guard makes a handful of records provably sufficient for real
// sessions; the generous value costs nothing when the hit comes early.
const headScanLimit = 1000

// readSessionHead reads Created and Title from the earliest segment — the
// lowest-numbered archive, else the active log. Archives are immutable, so
// the session start (header + first user input) survives compaction there;
// the active segment opens with a header copy that keeps the original
// timestamp. A leading session.created+context.compacted pair means the
// start is off-disk (archives were cleaned): the title stays empty rather
// than lying with a mid-conversation input.
//
// The guard is structural, not positional: a title is only trusted when
// the records genuinely start at the session header — records[0] must be
// the session.created (a headerless start stays untitled) and records[1]
// must not already be a compaction. With the guard, the scan terminates
// at the first user input and the head limit only bounds pathological
// files — a handful of records is provably sufficient.
func readSessionHead(dir string) (storage.SessionInfo, error) {
	var info storage.SessionInfo
	earliest := filepath.Join(dir, logFileName)
	if best, ok := earliestArchive(dir); ok {
		earliest = best
	}
	records, err := scanHead(earliest, headScanLimit)
	if err != nil {
		return info, err
	}
	if len(records) == 0 || records[0].Type != storage.TypeSessionCreated {
		return info, nil
	}
	info.Created = parseRecordTime(records[0].Time)
	if len(records) > 1 && records[1].Type == storage.TypeContextCompacted {
		return info, nil
	}
	for _, record := range records[1:] {
		if record.Type == storage.TypeUserInput {
			info.Title = record.Content
			break
		}
	}
	return info, nil
}

// earliestArchive returns the lowest-numbered archived segment.
func earliestArchive(dir string) (string, bool) {
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
		if bestNumber < 0 || number < bestNumber {
			best, bestNumber = name, number
		}
	}
	if bestNumber < 0 {
		return "", false
	}
	return filepath.Join(dir, best), true
}

// scanHead reads up to limit records from a segment, skipping unparseable
// lines — a concurrent writer may be mid-append into the tail (the session
// lock is not taken for a listing). Skipping is safe: the listing is a
// view, the session itself errors loudly when opened.
func scanHead(path string, limit int) ([]storage.Record, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open segment: %w", err)
	}
	defer file.Close()
	var records []storage.Record
	reader := bufio.NewReaderSize(file, 64*1024)
	for len(records) < limit {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var record storage.Record
			if decodeErr := json.Unmarshal(trimmed, &record); decodeErr == nil && record.Validate() == nil {
				records = append(records, record)
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("read segment: %w", err)
		}
	}
	return records, nil
}

// parseRecordTime is lenient by design: a listing must not fail on an
// odd timestamp, it just loses precision.
func parseRecordTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// HasSession probes whether a session directory exists under a project
// without taking its lock — the owning-project lookup for lazy opens.
func (s *Store) HasSession(projectID, sessionID string) bool {
	if !ValidProjectID(projectID) || !ValidSessionID(sessionID) {
		return false
	}
	info, err := os.Stat(filepath.Join(s.root, projectID, sessionID))
	return err == nil && info.IsDir()
}

// Delete removes a session permanently. The session lock is acquired
// first: an external holder (a CLI mid-write) refuses the deletion
// instead of letting files vanish under an active writer — on Unix the
// advisory flock would not stop the unlink, so the lock is the only
// guard. The manager additionally refuses sessions it has open.
//
// Windows cannot remove a directory while a handle inside it is open,
// so the deletion is staged: the CONTENT (log segments) dies while the
// lock is held — the data is gone under exclusive control — then the
// lock releases and the empty directory follows.
//
// The release→removal gap is honest about its limits: the directory
// still exists in it, so an external process racing in CAN open the
// dying session (a fresh empty log), and what it appends there is
// silently lost to the removal that follows. The window is the lock
// release to the RemoveAll of an already-contentless directory
// (microseconds), the lost data is only what that racer wrote inside
// it, and after the removal the racer converges to not-found on its
// next open. A tombstone checked by openDir could close even that —
// at the price of a marker whose failed removal permanently wedges
// the session — not worth it until a real workload hits the window.
func (s *Store) Delete(projectID, sessionID string) error {
	if !ValidProjectID(projectID) {
		return fmt.Errorf("invalid project id %q", projectID)
	}
	if !ValidSessionID(sessionID) {
		return fmt.Errorf("invalid session id %q", sessionID)
	}
	dir := filepath.Join(s.root, projectID, sessionID)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("open session %q: %w", sessionID, storage.ErrSessionNotFound)
	}
	lock, err := lockSession(dir)
	if err != nil {
		return err
	}
	// Remove the content under the lock; the lock file stays (its
	// handle is ours until Release).
	entries, err := os.ReadDir(dir)
	if err != nil {
		lock.Release()
		return fmt.Errorf("delete session %q: %w", sessionID, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "lock" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			lock.Release()
			return fmt.Errorf("delete session %q: %w", sessionID, err)
		}
	}
	// The holder note goes with the content; Release would remove it
	// anyway, but an empty-gap resurrection should not find stale
	// holder info.
	lock.Release()
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete session %q: %w", sessionID, err)
	}
	return nil
}
